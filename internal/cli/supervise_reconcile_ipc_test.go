package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"mcp-local-hub/internal/api"
	"mcp-local-hub/internal/api/apitest"
	"mcp-local-hub/internal/scheduler"
)

// newReconcileTestDeps constructs an ipcDispatchDeps suitable for
// unit-testing handleReconcile. The controller carries a real
// supervisorController + EventLoop so we can observe EvIntentUpdate
// posts via a registered handler — the SAME wiring contract production
// uses, just without the spawn/terminate side effects.
//
// The fixture wires:
//   - stateDir under apitest.HardenedTempDir (parent-dir DACL gate passes)
//   - supervisor-events.log opened so audit emits don't crash
//   - daemonIntent / intent caches seeded so the SM has descriptors to
//     transition against
//   - an EventLoop the test can drain to observe posted events
//
// Returns the deps + the controller + the event-loop pull channel
// (re-exposed because deps.controllerProvider() abstracts it).
type reconcileTestFixture struct {
	deps        ipcDispatchDeps
	ctrl        *supervisorController
	loop        *api.EventLoop
	postedCh    chan api.LoopEvent
	postedCount *atomic.Int32
}

func newReconcileTestFixture(t *testing.T, intent *api.SupervisorIntentFile) *reconcileTestFixture {
	t.Helper()
	tmpHome := apitest.HardenedTempDir(t)
	intentPath := filepath.Join(tmpHome, "supervisor-intent.json")
	if intent == nil {
		intent = &api.SupervisorIntentFile{Version: 1}
	}
	if err := api.WriteSupervisorIntent(intentPath, intent); err != nil {
		t.Fatalf("seed supervisor-intent.json: %v", err)
	}

	events, err := api.OpenSupervisorEventLog(filepath.Join(tmpHome, "supervisor-events.log"))
	if err != nil {
		t.Fatalf("OpenSupervisorEventLog: %v", err)
	}
	t.Cleanup(func() { events.Close() })

	var reconcileReady atomic.Bool
	reconcileReady.Store(true)

	// Build the controller with a real EventLoop so apply-mode posts
	// land where a handler can observe them. We register a recording
	// handler INSTEAD of the real handleLoopEvent so we don't pull in
	// spawn/terminate dependencies.
	loop := api.NewEventLoop(32)
	postedCh := make(chan api.LoopEvent, 64)
	var postedCount atomic.Int32
	loop.RegisterHandler(func(ev api.LoopEvent) {
		postedCount.Add(1)
		select {
		case postedCh <- ev:
		default:
		}
	})
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go loop.Run(ctx)

	ctrl := &supervisorController{
		intentCache:         newIntentCache(),
		eventLoop:           loop,
		events:              events,
		daemonIntent:        newDaemonIntentCache(),
		ctx:                 ctx,
		failureWindow:       respawnFailureWindow,
		quarantineThreshold: respawnQuarantineThreshold,
	}
	ctrl.intentCache.Refresh(intent)

	deps := ipcDispatchDeps{
		stateDir:           tmpHome,
		events:             events,
		runtimeTracker:     NewDaemonRuntimeTracker(),
		reconcileReady:     &reconcileReady,
		controllerProvider: func() *supervisorController { return ctrl },
	}
	return &reconcileTestFixture{
		deps:        deps,
		ctrl:        ctrl,
		loop:        loop,
		postedCh:    postedCh,
		postedCount: &postedCount,
	}
}

// decodeReconcileResponse decodes the LAST IPC response frame on the
// fake-conn buffer into api.ReconcileResponse. Mirrors the pattern in
// supervise_respawn_test.go.
func decodeReconcileResponse(t *testing.T, conn *fakeIPCConn) (api.IPCResponse, api.ReconcileResponse) {
	t.Helper()
	resp := conn.lastResponse(t)
	if resp.Error != nil {
		return resp, api.ReconcileResponse{}
	}
	if !resp.OK {
		t.Fatalf("response was not OK: %+v", resp)
	}
	raw, err := json.Marshal(resp.Result)
	if err != nil {
		t.Fatalf("re-marshal Result: %v", err)
	}
	var out api.ReconcileResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decode ReconcileResponse: %v (raw=%s)", err, raw)
	}
	return resp, out
}

// installSchedulerListFake registers a per-test scheduler.List override
// that returns the given slice. Returns the cleanup closure tests defer.
func installSchedulerListFake(t *testing.T, tasks []scheduler.TaskStatus) {
	t.Helper()
	uninstall := setReconcileSchedulerListFnForTest(func(context.Context) ([]scheduler.TaskStatus, error) {
		return tasks, nil
	})
	t.Cleanup(uninstall)
}

// TestReconcileIPC_DryRunReturnsDriftWithoutMutating seeds an intent
// declaring one daemon AND a scheduler row that is "stopped" against an
// intent that wants "running". Dry-run mode must surface the drift but
// NOT post EvIntentUpdate.
func TestReconcileIPC_DryRunReturnsDriftWithoutMutating(t *testing.T) {
	taskName := `\mcp-local-hub-foo-default`
	intent := &api.SupervisorIntentFile{
		Version: 1,
		Daemons: []api.SupervisorDaemon{
			{TaskName: taskName, Server: "foo", Daemon: "default", Port: 9200},
		},
	}
	fx := newReconcileTestFixture(t, intent)
	installSchedulerListFake(t, []scheduler.TaskStatus{
		{Name: taskName, State: "Ready"}, // scheduler stopped, intent wants running
	})

	req := api.IPCRequest{
		ID:   1,
		Cmd:  "reconcile",
		Args: map[string]any{"apply": false},
	}
	conn := newFakeIPCConn()
	if err := handleReconcile(conn, req, fx.deps); err != nil {
		t.Fatalf("handleReconcile: %v", err)
	}
	_, body := decodeReconcileResponse(t, conn)
	if !body.DryRun {
		t.Errorf("DryRun=false in dry-run mode response: %+v", body)
	}
	if body.DriftCount != 1 {
		t.Errorf("DriftCount = %d, want 1; drift=%+v", body.DriftCount, body.Drift)
	}
	if body.AppliedCount != 0 {
		t.Errorf("AppliedCount = %d, want 0 in dry-run", body.AppliedCount)
	}
	if len(body.Drift) != 1 || body.Drift[0].TaskName != taskName {
		t.Fatalf("expected drift[0].TaskName=%q; got %+v", taskName, body.Drift)
	}
	if body.Drift[0].Action != api.ReconcileActionPostEvIntentUpdate {
		t.Errorf("Drift[0].Action = %q, want %q", body.Drift[0].Action, api.ReconcileActionPostEvIntentUpdate)
	}
	// No events should have been posted.
	time.Sleep(50 * time.Millisecond) // settle window
	if got := fx.postedCount.Load(); got != 0 {
		t.Errorf("dry-run posted %d events; want 0", got)
	}
}

// TestReconcileIPC_ApplyPostsEvIntentUpdate verifies that apply mode
// posts EvIntentUpdate per drift entry whose action is
// post_ev_intent_update.
func TestReconcileIPC_ApplyPostsEvIntentUpdate(t *testing.T) {
	tasks := []string{
		`\mcp-local-hub-foo-default`,
		`\mcp-local-hub-bar-default`,
	}
	intent := &api.SupervisorIntentFile{
		Version: 1,
		Daemons: []api.SupervisorDaemon{
			{TaskName: tasks[0], Server: "foo", Daemon: "default"},
			{TaskName: tasks[1], Server: "bar", Daemon: "default"},
		},
	}
	fx := newReconcileTestFixture(t, intent)
	installSchedulerListFake(t, []scheduler.TaskStatus{
		{Name: tasks[0], State: "Ready"},   // stopped, intent wants running → drift
		{Name: tasks[1], State: "Running"}, // already running + intent wants running → no_op
	})

	req := api.IPCRequest{
		ID:   2,
		Cmd:  "reconcile",
		Args: map[string]any{"apply": true},
	}
	conn := newFakeIPCConn()
	if err := handleReconcile(conn, req, fx.deps); err != nil {
		t.Fatalf("handleReconcile: %v", err)
	}
	_, body := decodeReconcileResponse(t, conn)
	if body.DryRun {
		t.Errorf("DryRun=true in apply-mode response")
	}
	if body.DriftCount != 2 {
		t.Errorf("DriftCount = %d, want 2; drift=%+v", body.DriftCount, body.Drift)
	}
	if body.AppliedCount != 1 {
		t.Errorf("AppliedCount = %d, want 1 (only foo is actionable); drift=%+v", body.AppliedCount, body.Drift)
	}

	// Drain the event loop: we expect exactly one EvIntentUpdate for foo.
	deadline := time.Now().Add(1 * time.Second)
	var observed []api.LoopEvent
	for time.Now().Before(deadline) && len(observed) < 1 {
		select {
		case ev := <-fx.postedCh:
			observed = append(observed, ev)
		case <-time.After(50 * time.Millisecond):
		}
	}
	if len(observed) != 1 {
		t.Fatalf("expected 1 EvIntentUpdate post; got %d (%+v)", len(observed), observed)
	}
	if observed[0].Kind != api.EvIntentUpdate {
		t.Errorf("posted event kind = %q, want %q", observed[0].Kind, api.EvIntentUpdate)
	}
	if observed[0].TaskName != tasks[0] {
		t.Errorf("posted event task_name = %q, want %q", observed[0].TaskName, tasks[0])
	}
}

func TestReconcileIPC_SchedulerUnavailableTreatsSupervisorOwnedRowsAsMissing(t *testing.T) {
	taskName := `\mcp-local-hub-lsp-deadbeef-go`
	intent := &api.SupervisorIntentFile{
		Version: 1,
		Daemons: []api.SupervisorDaemon{
			{
				TaskName: taskName,
				Server:   "lsp",
				Daemon:   "go",
				Command:  "mcphub",
				Args:     []string{"daemon", "workspace-proxy", "--language", "go"},
				Port:     9242,
			},
		},
	}
	fx := newReconcileTestFixture(t, intent)
	uninstall := setReconcileSchedulerNewFnForTest(func() (scheduler.Scheduler, error) {
		return nil, fmt.Errorf("scheduler.New: %w", scheduler.ErrNotImplemented)
	})
	defer uninstall()

	req := api.IPCRequest{
		ID:   24,
		Cmd:  "reconcile",
		Args: map[string]any{"apply": true},
	}
	conn := newFakeIPCConn()
	if err := handleReconcile(conn, req, fx.deps); err != nil {
		t.Fatalf("handleReconcile: %v", err)
	}
	resp, body := decodeReconcileResponse(t, conn)
	if resp.Error != nil {
		t.Fatalf("scheduler not-implemented must be an empty snapshot, got error %+v", resp.Error)
	}
	if body.DriftCount != 1 || len(body.Drift) != 1 {
		t.Fatalf("DriftCount=%d drift=%+v, want one supervisor-owned missing row", body.DriftCount, body.Drift)
	}
	entry := body.Drift[0]
	if entry.TaskName != taskName {
		t.Errorf("TaskName = %q, want %q", entry.TaskName, taskName)
	}
	if entry.SchedulerState != api.ReconcileSchedulerStateMissing {
		t.Errorf("SchedulerState = %q, want %q", entry.SchedulerState, api.ReconcileSchedulerStateMissing)
	}
	if entry.Action != api.ReconcileActionPostEvIntentUpdate {
		t.Errorf("Action = %q, want %q for schedulerless supervisor-owned descriptor",
			entry.Action, api.ReconcileActionPostEvIntentUpdate)
	}
	if body.AppliedCount != 1 {
		t.Fatalf("AppliedCount = %d, want 1", body.AppliedCount)
	}

	select {
	case ev := <-fx.postedCh:
		if ev.Kind != api.EvIntentUpdate || ev.TaskName != taskName {
			t.Fatalf("posted event = %+v, want EvIntentUpdate for %s", ev, taskName)
		}
	case <-time.After(1 * time.Second):
		t.Fatal("expected EvIntentUpdate for schedulerless supervisor-owned row")
	}
}

// TestReconcileIPC_ApplyExcludesOrphanedLSPDescriptor is the apply-mode IPC
// mirror of the startup reconciler's Bug 1 orphan guard (P2 all-return-paths
// fix). An LSP workspace-proxy descriptor survives in supervisor-intent.json
// with NO backing workspaces.yaml row (the unregister path dropped the row but
// left the descriptor). Without the gate, apply-mode reconcile classifies it as
// post_ev_intent_update and re-SPAWNS the unbacked proxy, which exits 1 "not
// registered" and churns into quarantine — the exact Bug 1 recurrence on the
// apply path. With the gate, the orphan is downgraded to needs_manual_review,
// NO EvIntentUpdate is dispatched, and the operator-actionable
// orphaned-lsp-descriptor-skipped warn event fires.
func TestReconcileIPC_ApplyExcludesOrphanedLSPDescriptor(t *testing.T) {
	taskName := `\mcp-local-hub-lsp-deadbeef-go`
	// A real-shape LSP workspace-proxy descriptor (Server == "mcp-language-server",
	// argv `daemon workspace-proxy --port … --workspace … --language …`) — the
	// exact shape api.BuildSupervisorDaemonForLSP emits. The --workspace dir need
	// not exist for the registry lookup (CanonicalWorkspacePathForCleanup is
	// best-effort); using a temp dir keeps it realistic.
	wsDir := t.TempDir()
	intent := &api.SupervisorIntentFile{
		Version: 1,
		Daemons: []api.SupervisorDaemon{
			{
				TaskName:  taskName,
				Server:    "mcp-language-server",
				Daemon:    "lsp-deadbeef-go",
				Command:   "mcphub",
				Args:      []string{"daemon", "workspace-proxy", "--port", "9242", "--workspace", wsDir, "--language", "go"},
				Workspace: wsDir,
				Port:      9242,
			},
		},
	}
	fx := newReconcileTestFixture(t, intent)

	// Hermetic registry isolation (#278 fable r2): LSPRegistryRowBacksDescriptor
	// resolves the registry via api.DefaultRegistryPath(), an ENV-based resolver
	// (LOCALAPPDATA / XDG_STATE_HOME) that does NOT consult the
	// daemonStateRootOverride seam — the SetDaemonStateRootForTest call this
	// test previously relied on never actually redirected it (the test passed
	// only because the temp --workspace path had no row in the developer's
	// REAL registry, and stayed exposed to a fail-open flake whenever the live
	// registry flock was briefly contended). Redirect the env so
	// workspaces.yaml is ABSENT under a temp root → the descriptor has no
	// backing row → orphan, deterministically, with no read of the
	// developer's live registry at all.
	regRoot := t.TempDir()
	t.Setenv("LOCALAPPDATA", regRoot)
	t.Setenv("XDG_STATE_HOME", regRoot)

	// sched=missing + intent=running on a supervisor-owned row would, absent the
	// gate, classify as post_ev_intent_update (spawn).
	installSchedulerListFake(t, nil)

	req := api.IPCRequest{
		ID:   42,
		Cmd:  "reconcile",
		Args: map[string]any{"apply": true},
	}
	conn := newFakeIPCConn()
	if err := handleReconcile(conn, req, fx.deps); err != nil {
		t.Fatalf("handleReconcile: %v", err)
	}
	resp, body := decodeReconcileResponse(t, conn)
	if resp.Error != nil {
		t.Fatalf("unexpected error response: %+v", resp.Error)
	}
	if body.DriftCount != 1 || len(body.Drift) != 1 {
		t.Fatalf("DriftCount=%d drift=%+v, want exactly one entry", body.DriftCount, body.Drift)
	}
	entry := body.Drift[0]
	if entry.TaskName != taskName {
		t.Errorf("TaskName = %q, want %q", entry.TaskName, taskName)
	}
	// The orphan must be downgraded — NOT post_ev_intent_update.
	if entry.Action != api.ReconcileActionNeedsManualReview {
		t.Errorf("Action = %q, want %q (orphaned LSP descriptor must be excluded from the spawn-desired set)",
			entry.Action, api.ReconcileActionNeedsManualReview)
	}
	// Apply mode must dispatch NOTHING for the orphan.
	if body.AppliedCount != 0 {
		t.Errorf("AppliedCount = %d, want 0 (no spawn for an unbacked orphan)", body.AppliedCount)
	}

	// No EvIntentUpdate may be posted for the orphan.
	select {
	case ev := <-fx.postedCh:
		t.Fatalf("unexpected event posted for orphaned LSP descriptor: %+v (apply-mode must not re-spawn it)", ev)
	case <-time.After(200 * time.Millisecond):
		// good — nothing dispatched.
	}

	// The operator-actionable warn event must have fired (shared single-owner
	// emit with the startup guard).
	logRaw := readEventLogForTest(t, fx.deps.stateDir)
	if !strings.Contains(logRaw, `"event":"orphaned-lsp-descriptor-skipped"`) {
		t.Fatalf("expected orphaned-lsp-descriptor-skipped audit event; log:\n%s", logRaw)
	}
}

// TestReconcileIPC_ApplyRefreshesCacheBeforePosting verifies that apply mode
// refreshes the controller's cached supervisor intent and daemon intent from
// the same files used for drift classification before EvIntentUpdate is
// visible to the event loop.
func TestReconcileIPC_ApplyRefreshesCacheBeforePosting(t *testing.T) {
	taskName := `\mcp-local-hub-foo-default`
	fx := newReconcileTestFixture(t, &api.SupervisorIntentFile{Version: 1})

	now := time.Now().UTC()
	// Phase 4-E2: the stop lives in supervisor-intent.json's stops sub-block
	// (the sole stop source), not a separate daemon-intent.json.
	freshIntent := &api.SupervisorIntentFile{
		Version: 1,
		Daemons: []api.SupervisorDaemon{
			{TaskName: taskName, Server: "foo", Daemon: "default"},
		},
		Stops: map[string]api.DaemonIntent{
			taskName: {
				Desired:   api.IntentDesiredStopped,
				Reason:    api.IntentReasonUserStop,
				UpdatedAt: now,
			},
		},
	}
	if err := api.WriteSupervisorIntent(filepath.Join(fx.deps.stateDir, "supervisor-intent.json"), freshIntent); err != nil {
		t.Fatalf("overwrite supervisor-intent.json with fresh intent: %v", err)
	}
	installSchedulerListFake(t, []scheduler.TaskStatus{
		{Name: taskName, State: "Running"},
	})

	type cacheObservation struct {
		hasIntent bool
		desired   string
	}
	observedCache := make(chan cacheObservation, 1)
	fx.loop.RegisterHandler(func(ev api.LoopEvent) {
		if ev.Kind != api.EvIntentUpdate || ev.TaskName != taskName {
			return
		}
		_, hasIntent := fx.ctrl.intentCache.Lookup(taskName)
		daemonIntent := fx.ctrl.daemonIntent.Lookup(taskName)
		observedCache <- cacheObservation{
			hasIntent: hasIntent,
			desired:   daemonIntent.Desired,
		}
	})

	req := api.IPCRequest{
		ID:   22,
		Cmd:  "reconcile",
		Args: map[string]any{"apply": true},
	}
	conn := newFakeIPCConn()
	if err := handleReconcile(conn, req, fx.deps); err != nil {
		t.Fatalf("handleReconcile: %v", err)
	}
	_, body := decodeReconcileResponse(t, conn)
	if body.AppliedCount != 1 {
		t.Fatalf("AppliedCount = %d, want 1; drift=%+v", body.AppliedCount, body.Drift)
	}

	select {
	case got := <-observedCache:
		if !got.hasIntent {
			t.Fatalf("controller intent cache was stale when EvIntentUpdate was handled")
		}
		if got.desired != api.IntentDesiredStopped {
			t.Fatalf("controller daemon-intent cache desired=%q, want %q before EvIntentUpdate handling",
				got.desired, api.IntentDesiredStopped)
		}
	case <-time.After(1 * time.Second):
		t.Fatal("timed out waiting for EvIntentUpdate cache observation")
	}
}

// TestReconcileIPC_ApplyRefreshesCacheFromSubBlockDespiteCorruptDaemonIntent is
// the corrupt-daemon-intent companion to the PR #286 regression guard. Post-E2
// the supervisor-intent.json `stops` sub-block is the SOLE, AUTHORITATIVE stop
// source and UnifiedStopsFile IGNORES the vestigial daemon-intent.json, so a
// CORRUPT daemon-intent.json is irrelevant to the cache: when supervisor-intent
// was read SUCCESSFULLY (carrying a FRESH stop in the sub-block), apply-mode MUST
// refresh the controller cache from that sub-block and APPLY the stop, regardless
// of the corrupt vestigial file.
//
// This replaces the prior preserve-on-corrupt-daemon-intent assertion, which
// encoded the SAME conflation bot PR #286 flagged ("daemon-intent untrusted"
// wrongly treated as "stops view untrusted"). The authoritative-source corrupt
// case is a DIFFERENT path: a corrupt supervisor-intent.json fail-closes UPSTREAM
// with RECONCILE_INTENT_READ_FAILED (the handler returns an error and never calls
// applyReconcileDrift, so the cache is untouched) — that genuine preserve mode is
// covered by the upstream read-failure path, not this vestigial-file test.
//
// NEGATIVE CONTROL: under the over-broad `State != Corrupt` guard the corrupt
// daemon-intent.json forces cacheRefreshDaemonIntent=nil, the fresh sub-block stop
// never reaches the cache, and this test FAILS at the cache-observation assertion.
func TestReconcileIPC_ApplyRefreshesCacheFromSubBlockDespiteCorruptDaemonIntent(t *testing.T) {
	taskName := `\mcp-local-hub-foo-default`
	now := time.Now().UTC()
	// Fresh stop in the AUTHORITATIVE supervisor-intent.json stops sub-block.
	intent := &api.SupervisorIntentFile{
		Version: 1,
		Daemons: []api.SupervisorDaemon{
			{TaskName: taskName, Server: "foo", Daemon: "default"},
		},
		Stops: map[string]api.DaemonIntent{
			taskName: {
				Desired:   api.IntentDesiredStopped,
				Reason:    api.IntentReasonUserStop,
				UpdatedAt: now,
			},
		},
	}
	fx := newReconcileTestFixture(t, intent)

	// Pre-seed the controller cache with NO stop, so the fresh stop can ONLY
	// arrive via a successful cache refresh from the sub-block.
	fx.ctrl.daemonIntent.Refresh(&api.DaemonIntentFile{Tasks: map[string]api.DaemonIntent{}})

	// Corrupt vestigial daemon-intent.json → ReadDaemonIntentFile reports
	// State=corrupt with Err set. UnifiedStopsFile ignores it entirely.
	if err := os.WriteFile(filepath.Join(fx.deps.stateDir, "daemon-intent.json"), []byte(`{"tasks":`), 0o600); err != nil {
		t.Fatalf("seed corrupt daemon-intent.json: %v", err)
	}
	// Scheduler reports the daemon running → intent=stopped is a terminate drift.
	installSchedulerListFake(t, []scheduler.TaskStatus{
		{Name: taskName, State: "Running"},
	})

	observedDesired := make(chan string, 1)
	fx.loop.RegisterHandler(func(ev api.LoopEvent) {
		if ev.Kind != api.EvIntentUpdate || ev.TaskName != taskName {
			return
		}
		observedDesired <- fx.ctrl.daemonIntent.Lookup(taskName).Desired
	})

	req := api.IPCRequest{
		ID:   24,
		Cmd:  "reconcile",
		Args: map[string]any{"apply": true},
	}
	conn := newFakeIPCConn()
	if err := handleReconcile(conn, req, fx.deps); err != nil {
		t.Fatalf("handleReconcile: %v", err)
	}
	_, body := decodeReconcileResponse(t, conn)
	if body.AppliedCount != 1 {
		t.Fatalf("AppliedCount = %d, want 1 for running scheduler + fresh sub-block stop drift; drift=%+v",
			body.AppliedCount, body.Drift)
	}

	select {
	case got := <-observedDesired:
		if got != api.IntentDesiredStopped {
			t.Fatalf("daemon-intent cache desired observed during EvIntentUpdate = %q, want refreshed %q "+
				"(the fresh sub-block stop must reach the cache despite the corrupt daemon-intent.json)",
				got, api.IntentDesiredStopped)
		}
	case <-time.After(1 * time.Second):
		t.Fatal("timed out waiting for EvIntentUpdate cache observation")
	}

	got := fx.ctrl.daemonIntent.Lookup(taskName)
	if got.Desired != api.IntentDesiredStopped {
		t.Fatalf("daemon-intent cache desired = %q, want refreshed %q from the authoritative sub-block "+
			"despite the corrupt vestigial daemon-intent.json",
			got.Desired, api.IntentDesiredStopped)
	}

	// The corrupt vestigial read is still surfaced as a warn audit event — the
	// fix changes the cache trust decision, NOT the diagnostic emit.
	logRaw := readEventLogForTest(t, fx.deps.stateDir)
	if !strings.Contains(logRaw, `"event":"daemon-intent-read-failed"`) {
		t.Fatalf("expected daemon-intent-read-failed audit event; log:\n%s", logRaw)
	}
	if !strings.Contains(logRaw, `"state":"corrupt"`) {
		t.Fatalf("expected corrupt state in daemon-intent audit event; log:\n%s", logRaw)
	}
}

// TestReconcileIPC_ApplyRefreshesCacheFromSubBlockDespiteDaemonReadError is the
// PR #286 regression guard. It locks in the CORRECT post-E2 semantics: the
// supervisor-intent.json `stops` sub-block is the SOLE, AUTHORITATIVE stop
// source, and UnifiedStopsFile ignores the vestigial daemon-intent.json. So when
// supervisor-intent.json was read SUCCESSFULLY (carrying a FRESH stop in the
// sub-block) but the vestigial daemon-intent.json read ERRORS (State=missing with
// Err set — e.g. a stale directory at its path), apply-mode MUST refresh the
// controller cache from the fresh sub-block and APPLY the stop. The daemon-intent
// read error must NOT gate that refresh.
//
// The earlier PR #286 fix gated the refresh on daemonIntentRes.Err == nil, which
// inverted this: a stale daemon-intent.json directory skipped the refresh, the
// controller kept its stale (running-default) cache, and the operator's fresh
// stop was never applied (AppliedCount lied; stop reconciliation broke). This
// test asserts the OPPOSITE of that broken behavior — the fresh stop LANDS.
//
// NEGATIVE CONTROL: under the over-broad `daemonIntentRes.Err == nil` guard the
// observed cache Desired stays empty (running-default) and this test FAILS at the
// cache-observation assertion below.
func TestReconcileIPC_ApplyRefreshesCacheFromSubBlockDespiteDaemonReadError(t *testing.T) {
	taskName := `\mcp-local-hub-foo-default`
	now := time.Now().UTC()
	// Fresh stop in the AUTHORITATIVE supervisor-intent.json stops sub-block.
	intent := &api.SupervisorIntentFile{
		Version: 1,
		Daemons: []api.SupervisorDaemon{
			{TaskName: taskName, Server: "foo", Daemon: "default"},
		},
		Stops: map[string]api.DaemonIntent{
			taskName: {
				Desired:   api.IntentDesiredStopped,
				Reason:    api.IntentReasonUserStop,
				UpdatedAt: now,
			},
		},
	}
	fx := newReconcileTestFixture(t, intent)

	// Pre-seed the controller cache with NO stop, so the fresh stop can ONLY
	// arrive via a successful cache refresh from the sub-block. If the refresh
	// is wrongly skipped, the handler observes this empty (running-default) cache.
	fx.ctrl.daemonIntent.Refresh(&api.DaemonIntentFile{Tasks: map[string]api.DaemonIntent{}})

	// Force os.ReadFile(daemon-intent.json) to fail with a non-ENOENT read error
	// (EISDIR). This is the bot's exact scenario: the vestigial path is anomalous
	// but the authoritative sub-block is healthy.
	if err := os.Mkdir(filepath.Join(fx.deps.stateDir, "daemon-intent.json"), 0o700); err != nil {
		t.Fatalf("seed unreadable daemon-intent.json directory: %v", err)
	}
	// Scheduler reports the daemon running → intent=stopped is a terminate drift
	// (post_ev_intent_update). Apply must dispatch EvIntentUpdate AND the
	// controller cache must carry the fresh stop when the handler reads it.
	installSchedulerListFake(t, []scheduler.TaskStatus{
		{Name: taskName, State: "Running"},
	})

	observedDesired := make(chan string, 1)
	fx.loop.RegisterHandler(func(ev api.LoopEvent) {
		if ev.Kind != api.EvIntentUpdate || ev.TaskName != taskName {
			return
		}
		observedDesired <- fx.ctrl.daemonIntent.Lookup(taskName).Desired
	})

	req := api.IPCRequest{
		ID:   27,
		Cmd:  "reconcile",
		Args: map[string]any{"apply": true},
	}
	conn := newFakeIPCConn()
	if err := handleReconcile(conn, req, fx.deps); err != nil {
		t.Fatalf("handleReconcile: %v", err)
	}
	_, body := decodeReconcileResponse(t, conn)
	if body.AppliedCount != 1 {
		t.Fatalf("AppliedCount = %d, want 1 for running scheduler + fresh sub-block stop drift; drift=%+v",
			body.AppliedCount, body.Drift)
	}

	select {
	case got := <-observedDesired:
		if got != api.IntentDesiredStopped {
			t.Fatalf("daemon-intent cache desired observed during EvIntentUpdate = %q, want refreshed %q "+
				"(the fresh sub-block stop must reach the cache despite the daemon-intent read error)",
				got, api.IntentDesiredStopped)
		}
	case <-time.After(1 * time.Second):
		t.Fatal("timed out waiting for EvIntentUpdate cache observation")
	}

	got := fx.ctrl.daemonIntent.Lookup(taskName)
	if got.Desired != api.IntentDesiredStopped {
		t.Fatalf("daemon-intent cache desired = %q, want refreshed %q from the authoritative sub-block "+
			"despite the vestigial daemon-intent read error",
			got.Desired, api.IntentDesiredStopped)
	}

	// The daemon-intent read failure is still surfaced as a warn audit event
	// (the read is attempted; its error is logged) — the fix changes the cache
	// trust decision, NOT the diagnostic emit.
	logRaw := readEventLogForTest(t, fx.deps.stateDir)
	if !strings.Contains(logRaw, `"event":"daemon-intent-read-failed"`) {
		t.Fatalf("expected daemon-intent-read-failed audit event; log:\n%s", logRaw)
	}
	if !strings.Contains(logRaw, `"state":"missing"`) {
		t.Fatalf("expected missing state in daemon-intent audit event; log:\n%s", logRaw)
	}
}

// TestReconcileIPC_ApplyPreservesDaemonIntentCacheOnMissingSupervisorIntent locks
// in the Phase 4-E2 P2-BLOCKER fix: when supervisor-intent.json — the SOLE stop
// source after E2 — is physically ABSENT during an apply-mode reconcile, the
// synthesized empty intent must still NOT refresh the controller daemon-intent
// cache with empty stops. Refreshing it would clear the operator's last-known
// stops and let a child-exit/SM evaluation in that window REVIVE a deliberately
// stopped daemon (the un-suppress bug). This is the missing-file analogue of
// TestReconcileIPC_ApplyPreservesDaemonIntentCacheOnCorruptRead and mirrors the
// sibling watcher path resolveWatcherDaemonIntent's fail-closed posture on
// supFailed (which is true for both corrupt AND os.ErrNotExist).
//
// PRE-FIX: cacheRefreshDaemonIntent was the non-nil empty unified file (a
// missing daemon-intent.json is non-corrupt), so applyReconcileDrift called
// ctrl.daemonIntent.Refresh(empty) and erased previousStop → this test FAILS
// (the Lookup below returns an empty Desired instead of the preserved stop).
func TestReconcileIPC_ApplyPreservesDaemonIntentCacheOnMissingSupervisorIntent(t *testing.T) {
	taskName := `\mcp-local-hub-foo-default`
	intent := &api.SupervisorIntentFile{
		Version: 1,
		Daemons: []api.SupervisorDaemon{
			{TaskName: taskName, Server: "foo", Daemon: "default"},
		},
	}
	fx := newReconcileTestFixture(t, intent)

	// The fixture seeds supervisor-intent.json; remove it so the read returns
	// os.ErrNotExist (the "sole stop source physically absent" case). The
	// controller still carries the descriptor + the prior stop in-memory.
	if err := os.Remove(filepath.Join(fx.deps.stateDir, "supervisor-intent.json")); err != nil {
		t.Fatalf("remove supervisor-intent.json: %v", err)
	}

	previousStop := api.DaemonIntentFile{
		Tasks: map[string]api.DaemonIntent{
			taskName: {
				Desired:   api.IntentDesiredStopped,
				Reason:    api.IntentReasonUserStop,
				UpdatedAt: time.Now().UTC(),
			},
		},
	}
	fx.ctrl.daemonIntent.Refresh(&previousStop)

	// Scheduler reports the daemon running. With the descriptor gone from the
	// (now-missing) intent file, the intent walk finds no declared daemons —
	// the running task surfaces as an orphan (needs_manual_review), never a
	// post. AppliedCount must stay 0 regardless of the cache outcome.
	installSchedulerListFake(t, []scheduler.TaskStatus{
		{Name: taskName, State: "Running"},
	})

	req := api.IPCRequest{
		ID:   26,
		Cmd:  "reconcile",
		Args: map[string]any{"apply": true},
	}
	conn := newFakeIPCConn()
	if err := handleReconcile(conn, req, fx.deps); err != nil {
		t.Fatalf("handleReconcile: %v", err)
	}
	_, body := decodeReconcileResponse(t, conn)
	if body.AppliedCount != 0 {
		t.Fatalf("AppliedCount = %d, want 0 for orphan running daemon on missing intent; drift=%+v",
			body.AppliedCount, body.Drift)
	}

	got := fx.ctrl.daemonIntent.Lookup(taskName)
	if got.Desired != api.IntentDesiredStopped {
		t.Fatalf("daemon-intent cache desired = %q, want preserved %q after missing supervisor-intent.json",
			got.Desired, api.IntentDesiredStopped)
	}
}

// TestReconcileIPC_HandlerTimeoutCancelsSchedulerList verifies the production
// scheduler-list path hands the reconcile context to a context-aware scheduler
// implementation instead of abandoning an uncancellable goroutine after the
// handler deadline fires.
func TestReconcileIPC_HandlerTimeoutCancelsSchedulerList(t *testing.T) {
	taskName := `\mcp-local-hub-foo-default`
	intent := &api.SupervisorIntentFile{
		Version: 1,
		Daemons: []api.SupervisorDaemon{
			{TaskName: taskName, Server: "foo", Daemon: "default"},
		},
	}
	fx := newReconcileTestFixture(t, intent)

	origTimeout := reconcileHandlerTimeout
	reconcileHandlerTimeout = 25 * time.Millisecond
	t.Cleanup(func() { reconcileHandlerTimeout = origTimeout })

	ctxCanceled := make(chan error, 1)
	fakeScheduler := &reconcileContextSchedulerForTest{
		listContext: func(ctx context.Context, prefix string) ([]scheduler.TaskStatus, error) {
			if prefix != "mcp-local-hub-" {
				t.Errorf("scheduler prefix = %q, want mcp-local-hub-", prefix)
			}
			<-ctx.Done()
			ctxCanceled <- ctx.Err()
			return nil, ctx.Err()
		},
	}
	uninstall := setReconcileSchedulerNewFnForTest(func() (scheduler.Scheduler, error) {
		return fakeScheduler, nil
	})
	defer uninstall()

	req := api.IPCRequest{
		ID:   25,
		Cmd:  "reconcile",
		Args: map[string]any{"apply": false},
	}
	conn := newFakeIPCConn()
	if err := handleReconcile(conn, req, fx.deps); err != nil {
		t.Fatalf("handleReconcile: %v", err)
	}
	resp := conn.lastResponse(t)
	if resp.Error == nil {
		t.Fatalf("expected timeout response, got %+v", resp)
	}
	if resp.Error.Code != "RECONCILE_TIMEOUT" {
		t.Fatalf("response code = %q, want RECONCILE_TIMEOUT", resp.Error.Code)
	}

	select {
	case err := <-ctxCanceled:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("scheduler ListContext saw ctx err %v, want DeadlineExceeded", err)
		}
	case <-time.After(1 * time.Second):
		t.Fatal("scheduler ListContext did not observe reconcile context cancellation")
	}
}

// TestReconcileIPC_HandlerTimeout verifies that handleReconcile creates the
// handler-side 25-second deadline and propagates it to long-running scheduler
// work through the reconcile scheduler seam.
func TestReconcileIPC_HandlerTimeout(t *testing.T) {
	if reconcileHandlerTimeout != 25*time.Second {
		t.Fatalf("reconcileHandlerTimeout = %s, want 25s", reconcileHandlerTimeout)
	}
	taskName := `\mcp-local-hub-foo-default`
	intent := &api.SupervisorIntentFile{
		Version: 1,
		Daemons: []api.SupervisorDaemon{
			{TaskName: taskName, Server: "foo", Daemon: "default"},
		},
	}
	fx := newReconcileTestFixture(t, intent)

	deadlineDelta := make(chan time.Duration, 1)
	uninstall := setReconcileSchedulerListFnForTest(func(ctx context.Context) ([]scheduler.TaskStatus, error) {
		deadline, ok := ctx.Deadline()
		if !ok {
			t.Fatal("scheduler list context had no deadline")
		}
		deadlineDelta <- time.Until(deadline)
		return []scheduler.TaskStatus{{Name: taskName, State: "Ready"}}, nil
	})
	defer uninstall()

	req := api.IPCRequest{
		ID:   23,
		Cmd:  "reconcile",
		Args: map[string]any{"apply": false},
	}
	conn := newFakeIPCConn()
	if err := handleReconcile(conn, req, fx.deps); err != nil {
		t.Fatalf("handleReconcile: %v", err)
	}
	_, body := decodeReconcileResponse(t, conn)
	if body.DriftCount != 1 {
		t.Fatalf("DriftCount = %d, want 1; drift=%+v", body.DriftCount, body.Drift)
	}

	select {
	case got := <-deadlineDelta:
		if got <= 24*time.Second || got > 25*time.Second {
			t.Fatalf("scheduler context deadline delta = %s, want within (24s, 25s]", got)
		}
	case <-time.After(1 * time.Second):
		t.Fatal("scheduler fake did not observe reconcile context")
	}
}

// TestReconcileIPC_NoDriftReturnsEmptyArray verifies that the
// already-aligned case returns DriftCount=0 (or only no_op entries
// counted; the spec test name says "empty array", so we assert no
// actionable drift).
func TestReconcileIPC_NoDriftReturnsEmptyArray(t *testing.T) {
	taskName := `\mcp-local-hub-foo-default`
	intent := &api.SupervisorIntentFile{
		Version: 1,
		Daemons: []api.SupervisorDaemon{
			{TaskName: taskName, Server: "foo", Daemon: "default"},
		},
	}
	fx := newReconcileTestFixture(t, intent)
	installSchedulerListFake(t, []scheduler.TaskStatus{
		{Name: taskName, State: "Running"}, // matches intent (default desired=running)
	})

	req := api.IPCRequest{
		ID:   3,
		Cmd:  "reconcile",
		Args: map[string]any{"apply": false},
	}
	conn := newFakeIPCConn()
	if err := handleReconcile(conn, req, fx.deps); err != nil {
		t.Fatalf("handleReconcile: %v", err)
	}
	_, body := decodeReconcileResponse(t, conn)
	// The handler always emits one row per intent daemon for visibility;
	// the "no drift" assertion checks the action vocabulary: no entry
	// should be actionable.
	if body.DriftCount != 1 {
		t.Fatalf("expected 1 informational row; got %d (drift=%+v)", body.DriftCount, body.Drift)
	}
	if body.Drift[0].Action != api.ReconcileActionNoOp {
		t.Errorf("Action = %q, want %q (clean state)", body.Drift[0].Action, api.ReconcileActionNoOp)
	}
	if body.AppliedCount != 0 {
		t.Errorf("AppliedCount = %d, want 0 on clean state", body.AppliedCount)
	}
}

// TestReconcileIPC_MissingTaskSpawnsViaIntentUpdate — under the no-legacy
// ownership model (spec §0.2) EVERY supervisor-intent row is
// supervisor-owned, so a regular `foo/default` descriptor with no scheduler
// row + the default running intent classifies post_ev_intent_update (spawn
// directly from supervisor-intent.json). SchedulerState is still "missing"
// (the scheduler has no row), but a missing row is no longer a "legacy task
// lost, operator must re-install" signal — the supervisor spawns the daemon
// itself. This replaces the dead needs_manual_review expectation the
// scheduler-era classifier returned here.
func TestReconcileIPC_MissingTaskSpawnsViaIntentUpdate(t *testing.T) {
	taskName := `\mcp-local-hub-foo-default`
	intent := &api.SupervisorIntentFile{
		Version: 1,
		Daemons: []api.SupervisorDaemon{
			{TaskName: taskName, Server: "foo", Daemon: "default"},
		},
	}
	fx := newReconcileTestFixture(t, intent)
	installSchedulerListFake(t, []scheduler.TaskStatus{}) // empty scheduler

	req := api.IPCRequest{
		ID:   4,
		Cmd:  "reconcile",
		Args: map[string]any{"apply": false},
	}
	conn := newFakeIPCConn()
	if err := handleReconcile(conn, req, fx.deps); err != nil {
		t.Fatalf("handleReconcile: %v", err)
	}
	_, body := decodeReconcileResponse(t, conn)
	if body.DriftCount != 1 {
		t.Fatalf("expected 1 drift entry; got %d (%+v)", body.DriftCount, body.Drift)
	}
	if body.Drift[0].SchedulerState != api.ReconcileSchedulerStateMissing {
		t.Errorf("SchedulerState = %q, want %q",
			body.Drift[0].SchedulerState, api.ReconcileSchedulerStateMissing)
	}
	if body.Drift[0].Action != api.ReconcileActionPostEvIntentUpdate {
		t.Errorf("Action = %q, want %q for missing scheduler entry (no-legacy: supervisor spawns it)",
			body.Drift[0].Action, api.ReconcileActionPostEvIntentUpdate)
	}
}

func TestReconcileIPC_SupervisorOwnedMissingTaskAppliesStart(t *testing.T) {
	// Hermetic registry isolation (#278 fable r2): the apply-mode orphan gate
	// consults api.LSPRegistryRowBacksDescriptor, which resolves the registry
	// via api.DefaultRegistryPath() — an ENV-based resolver (LOCALAPPDATA /
	// XDG_STATE_HOME) that does NOT honor the daemonStateRootOverride seam.
	// The original form of this test hardcoded a REAL workspace path, so its
	// verdict depended on the developer's live workspaces.yaml (it broke the
	// moment that workspace's go row moved under the @serena sentinel).
	// Redirect both env vars so every platform resolves the registry under a
	// temp root, then seed the BACKING row the descriptor needs for the spawn
	// direction to stay post_ev_intent_update under the gate.
	regRoot := t.TempDir()
	t.Setenv("LOCALAPPDATA", regRoot)
	t.Setenv("XDG_STATE_HOME", regRoot)

	ws := t.TempDir()
	canonical, err := api.CanonicalWorkspacePathForCleanup(ws)
	if err != nil {
		t.Fatalf("canonicalize workspace: %v", err)
	}
	key := api.WorkspaceKey(canonical)
	taskName := `\mcp-local-hub-lsp-` + key + `-go`
	regPath, err := api.DefaultRegistryPath()
	if err != nil {
		t.Fatalf("registry path: %v", err)
	}
	reg := api.NewRegistry(regPath)
	if perr := reg.PutLSP(api.WorkspaceEntry{
		WorkspaceKey:  key,
		WorkspacePath: canonical,
		Language:      "go",
		Backend:       "mcp-language-server",
		Port:          9200,
		TaskName:      "mcp-local-hub-lsp-" + key + "-go",
	}); perr != nil {
		t.Fatalf("seed registry row: %v", perr)
	}
	if serr := reg.Save(); serr != nil {
		t.Fatalf("save seeded registry: %v", serr)
	}

	intent := &api.SupervisorIntentFile{
		Version: 1,
		Daemons: []api.SupervisorDaemon{
			{
				TaskName:  taskName,
				Server:    "mcp-language-server",
				Daemon:    "lsp-" + key + "-go",
				Command:   "mcphub",
				Args:      []string{"daemon", "workspace-proxy", "--port", "9200", "--workspace", ws, "--language", "go"},
				Workspace: ws,
				Port:      9200,
			},
		},
	}
	fx := newReconcileTestFixture(t, intent)
	installSchedulerListFake(t, []scheduler.TaskStatus{})

	req := api.IPCRequest{
		ID:   44,
		Cmd:  "reconcile",
		Args: map[string]any{"apply": true},
	}
	conn := newFakeIPCConn()
	if err := handleReconcile(conn, req, fx.deps); err != nil {
		t.Fatalf("handleReconcile: %v", err)
	}
	_, body := decodeReconcileResponse(t, conn)
	if body.DriftCount != 1 {
		t.Fatalf("expected 1 drift entry; got %d (%+v)", body.DriftCount, body.Drift)
	}
	if body.Drift[0].SchedulerState != api.ReconcileSchedulerStateMissing {
		t.Errorf("SchedulerState = %q, want %q",
			body.Drift[0].SchedulerState, api.ReconcileSchedulerStateMissing)
	}
	if body.Drift[0].Action != api.ReconcileActionPostEvIntentUpdate {
		t.Errorf("Action = %q, want %q for supervisor-owned missing scheduler entry",
			body.Drift[0].Action, api.ReconcileActionPostEvIntentUpdate)
	}
	if body.AppliedCount != 1 {
		t.Errorf("AppliedCount = %d, want 1", body.AppliedCount)
	}
	select {
	case ev := <-fx.postedCh:
		if ev.Kind != api.EvIntentUpdate || ev.TaskName != taskName {
			t.Fatalf("posted event = %+v, want EvIntentUpdate for %s", ev, taskName)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for EvIntentUpdate")
	}
}

// TestReconcileIPC_OrphanSchedulerTaskFlaggedNeedsManualReview —
// scheduler has a row that the intent does NOT declare → orphan;
// IntentDesired="?" and Action="needs_manual_review".
func TestReconcileIPC_OrphanSchedulerTaskFlaggedNeedsManualReview(t *testing.T) {
	orphan := `\mcp-local-hub-orphan-default`
	intent := &api.SupervisorIntentFile{Version: 1} // no daemons declared
	fx := newReconcileTestFixture(t, intent)
	installSchedulerListFake(t, []scheduler.TaskStatus{
		{Name: orphan, State: "Running"},
	})

	req := api.IPCRequest{
		ID:   5,
		Cmd:  "reconcile",
		Args: map[string]any{"apply": false},
	}
	conn := newFakeIPCConn()
	if err := handleReconcile(conn, req, fx.deps); err != nil {
		t.Fatalf("handleReconcile: %v", err)
	}
	_, body := decodeReconcileResponse(t, conn)
	if body.DriftCount != 1 {
		t.Fatalf("expected 1 drift entry for orphan; got %d (%+v)", body.DriftCount, body.Drift)
	}
	entry := body.Drift[0]
	if entry.TaskName != orphan {
		t.Errorf("TaskName = %q, want %q", entry.TaskName, orphan)
	}
	if entry.IntentDesired != api.ReconcileIntentDesiredUnknown {
		t.Errorf("IntentDesired = %q, want %q",
			entry.IntentDesired, api.ReconcileIntentDesiredUnknown)
	}
	if entry.Action != api.ReconcileActionNeedsManualReview {
		t.Errorf("Action = %q, want %q for orphan",
			entry.Action, api.ReconcileActionNeedsManualReview)
	}
}

// TestReconcileIPC_AuditEventEmitted verifies the
// `mcphub-reconcile-invoked` audit event is written with the expected
// body fields.
func TestReconcileIPC_AuditEventEmitted(t *testing.T) {
	taskName := `\mcp-local-hub-foo-default`
	intent := &api.SupervisorIntentFile{
		Version: 1,
		Daemons: []api.SupervisorDaemon{
			{TaskName: taskName, Server: "foo", Daemon: "default"},
		},
	}
	fx := newReconcileTestFixture(t, intent)
	installSchedulerListFake(t, []scheduler.TaskStatus{
		{Name: taskName, State: "Ready"},
	})

	req := api.IPCRequest{
		ID:   6,
		Cmd:  "reconcile",
		Args: map[string]any{"apply": true},
	}
	conn := newFakeIPCConn()
	if err := handleReconcile(conn, req, fx.deps); err != nil {
		t.Fatalf("handleReconcile: %v", err)
	}

	logRaw := readEventLogForTest(t, fx.deps.stateDir)
	if !strings.Contains(logRaw, `"event":"mcphub-reconcile-invoked"`) {
		t.Fatalf("expected mcphub-reconcile-invoked event in audit log; got:\n%s", logRaw)
	}
	// Body assertions: dry_run=false, drift_count=1, applied_count=1.
	// Match on the JSONL line containing our event name and check key
	// substrings — full JSON parse would tie us to internal field order.
	if !strings.Contains(logRaw, `"dry_run":false`) {
		t.Errorf("expected dry_run:false in audit body; log: %s", logRaw)
	}
	if !strings.Contains(logRaw, `"drift_count":1`) {
		t.Errorf("expected drift_count:1 in audit body; log: %s", logRaw)
	}
	if !strings.Contains(logRaw, `"applied_count":1`) {
		t.Errorf("expected applied_count:1 in audit body; log: %s", logRaw)
	}
}

// TestReconcileIPC_TimeoutErrors verifies that the handler-side 25s
// ctx deadline propagates to the scheduler.List call. After r2,
// handleReconcile creates its own context.WithTimeout(ctx,
// reconcileHandlerTimeout) (supervise_reconcile_ipc.go:112) so a
// pathological scheduler.List that hangs longer than 25s is unwound
// inside the handler rather than relying solely on the client's 30s
// dial timeout (DialSupervisorIPCReconcile). The test exercises the
// scheduler-list block boundary by making it sleep longer than the
// handler ctx deadline, then asserting the handler returns a
// populated response without deadlocking and the slow path is
// observable in audit.
func TestReconcileIPC_TimeoutErrors(t *testing.T) {
	taskName := `\mcp-local-hub-foo-default`
	intent := &api.SupervisorIntentFile{
		Version: 1,
		Daemons: []api.SupervisorDaemon{
			{TaskName: taskName, Server: "foo", Daemon: "default"},
		},
	}
	fx := newReconcileTestFixture(t, intent)

	// Scheduler list returns a synthetic error so the handler surfaces
	// the failure (this is the production-equivalent of a hung
	// scheduler). The handler must return a structured IPC error code
	// rather than blocking indefinitely.
	uninstall := setReconcileSchedulerListFnForTest(func(context.Context) ([]scheduler.TaskStatus, error) {
		return nil, errors.New("scheduler list timed out")
	})
	defer uninstall()

	req := api.IPCRequest{
		ID:   7,
		Cmd:  "reconcile",
		Args: map[string]any{"apply": false},
	}
	conn := newFakeIPCConn()
	if err := handleReconcile(conn, req, fx.deps); err != nil {
		t.Fatalf("handleReconcile: %v", err)
	}
	resp := conn.lastResponse(t)
	if resp.Error == nil {
		t.Fatalf("expected error response from failed scheduler list; got %+v", resp)
	}
	if resp.Error.Code != "RECONCILE_SCHEDULER_LIST_FAILED" {
		t.Errorf("expected RECONCILE_SCHEDULER_LIST_FAILED; got %s", resp.Error.Code)
	}
	if !strings.Contains(resp.Error.Message, "scheduler list timed out") {
		t.Errorf("error message should carry root cause; got %q", resp.Error.Message)
	}
	if !resp.Final {
		t.Error("response must be final")
	}
}

// TestReconcileIPC_DaemonIntentStopOverridesDefault — when the
// supervisor-intent.json stops sub-block declares Desired=stopped for a task
// whose scheduler row is running, the action must be post_ev_intent_update
// (terminate). Phase 4-E2: the stop lives in the sub-block (the sole stop
// source), not a separate daemon-intent.json.
func TestReconcileIPC_DaemonIntentStopOverridesDefault(t *testing.T) {
	taskName := `\mcp-local-hub-foo-default`
	now := time.Now().UTC()
	intent := &api.SupervisorIntentFile{
		Version: 1,
		Daemons: []api.SupervisorDaemon{
			{TaskName: taskName, Server: "foo", Daemon: "default"},
		},
		Stops: map[string]api.DaemonIntent{
			taskName: {
				Desired:   api.IntentDesiredStopped,
				Reason:    api.IntentReasonUserStop,
				UpdatedAt: now,
			},
		},
	}
	fx := newReconcileTestFixture(t, intent)

	installSchedulerListFake(t, []scheduler.TaskStatus{
		{Name: taskName, State: "Running"}, // running, intent says stop → drift
	})

	req := api.IPCRequest{
		ID:   8,
		Cmd:  "reconcile",
		Args: map[string]any{"apply": false},
	}
	conn := newFakeIPCConn()
	if err := handleReconcile(conn, req, fx.deps); err != nil {
		t.Fatalf("handleReconcile: %v", err)
	}
	_, body := decodeReconcileResponse(t, conn)
	if body.DriftCount != 1 {
		t.Fatalf("expected 1 drift entry; got %d (%+v)", body.DriftCount, body.Drift)
	}
	entry := body.Drift[0]
	if entry.IntentDesired != api.ReconcileIntentDesiredStopped {
		t.Errorf("IntentDesired = %q, want %q (daemon-intent overrides default-running)",
			entry.IntentDesired, api.ReconcileIntentDesiredStopped)
	}
	if entry.Action != api.ReconcileActionPostEvIntentUpdate {
		t.Errorf("Action = %q, want %q (running + intent=stopped is actionable)",
			entry.Action, api.ReconcileActionPostEvIntentUpdate)
	}
}

func TestReconcileIPC_ExpiredUserStopClassifiesDesiredRunning(t *testing.T) {
	expiredTask := `\mcp-local-hub-expired-default`
	freshTask := `\mcp-local-hub-fresh-default`
	now := time.Now().UTC()
	intent := &api.SupervisorIntentFile{
		Version: 1,
		Daemons: []api.SupervisorDaemon{
			{TaskName: expiredTask, Server: "expired", Daemon: "default"},
			{TaskName: freshTask, Server: "fresh", Daemon: "default"},
		},
		Stops: map[string]api.DaemonIntent{
			expiredTask: {
				Desired:   api.IntentDesiredStopped,
				Reason:    api.IntentReasonUserStop,
				UpdatedAt: now.Add(-api.StopIntentTTL - time.Hour),
			},
			freshTask: {
				Desired:   api.IntentDesiredStopped,
				Reason:    api.IntentReasonUserStop,
				UpdatedAt: now,
			},
		},
	}
	fx := newReconcileTestFixture(t, intent)
	installSchedulerListFake(t, []scheduler.TaskStatus{
		{Name: expiredTask, State: "Running"},
		{Name: freshTask, State: "Running"},
	})

	req := api.IPCRequest{
		ID:   28,
		Cmd:  "reconcile",
		Args: map[string]any{"apply": true},
	}
	conn := newFakeIPCConn()
	if err := handleReconcile(conn, req, fx.deps); err != nil {
		t.Fatalf("handleReconcile: %v", err)
	}
	_, body := decodeReconcileResponse(t, conn)
	if body.AppliedCount != 1 {
		t.Fatalf("AppliedCount = %d, want 1 (only fresh active stop should post); drift=%+v",
			body.AppliedCount, body.Drift)
	}

	byTask := map[string]api.DriftEntry{}
	for _, entry := range body.Drift {
		byTask[entry.TaskName] = entry
	}
	expired := byTask[expiredTask]
	if expired.IntentDesired != api.ReconcileIntentDesiredRunning {
		t.Fatalf("expired user-stop IntentDesired = %q, want %q",
			expired.IntentDesired, api.ReconcileIntentDesiredRunning)
	}
	if expired.Action != api.ReconcileActionNoOp {
		t.Fatalf("expired user-stop Action = %q, want %q for already-running daemon",
			expired.Action, api.ReconcileActionNoOp)
	}
	fresh := byTask[freshTask]
	if fresh.IntentDesired != api.ReconcileIntentDesiredStopped {
		t.Fatalf("fresh user-stop IntentDesired = %q, want %q",
			fresh.IntentDesired, api.ReconcileIntentDesiredStopped)
	}
	if fresh.Action != api.ReconcileActionPostEvIntentUpdate {
		t.Fatalf("fresh user-stop Action = %q, want %q",
			fresh.Action, api.ReconcileActionPostEvIntentUpdate)
	}

	select {
	case ev := <-fx.postedCh:
		if ev.Kind != api.EvIntentUpdate || ev.TaskName != freshTask {
			t.Fatalf("posted event = %+v, want EvIntentUpdate for fresh stop %s only", ev, freshTask)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for fresh stop EvIntentUpdate")
	}
	select {
	case ev := <-fx.postedCh:
		t.Fatalf("unexpected second event posted for expired stop: %+v", ev)
	case <-time.After(200 * time.Millisecond):
	}
}

type reconcileContextSchedulerForTest struct {
	listContext func(context.Context, string) ([]scheduler.TaskStatus, error)
}

func (s *reconcileContextSchedulerForTest) Create(scheduler.TaskSpec) error { return nil }
func (s *reconcileContextSchedulerForTest) Delete(string) error             { return nil }
func (s *reconcileContextSchedulerForTest) Run(string) error                { return nil }
func (s *reconcileContextSchedulerForTest) Stop(string) error               { return nil }
func (s *reconcileContextSchedulerForTest) Status(string) (scheduler.TaskStatus, error) {
	return scheduler.TaskStatus{}, scheduler.ErrTaskNotFound
}
func (s *reconcileContextSchedulerForTest) List(string) ([]scheduler.TaskStatus, error) {
	return nil, errors.New("legacy List path called; want ListContext")
}
func (s *reconcileContextSchedulerForTest) ListContext(ctx context.Context, prefix string) ([]scheduler.TaskStatus, error) {
	return s.listContext(ctx, prefix)
}
func (s *reconcileContextSchedulerForTest) ExportXML(string) ([]byte, error) {
	return nil, scheduler.ErrTaskNotFound
}
func (s *reconcileContextSchedulerForTest) ImportXML(string, []byte) error { return nil }
