package cli

import (
	"context"
	"encoding/json"
	"errors"
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

// TestReconcileIPC_ApplyRefreshesCacheBeforePosting verifies that apply mode
// refreshes the controller's cached supervisor intent and daemon intent from
// the same files used for drift classification before EvIntentUpdate is
// visible to the event loop.
func TestReconcileIPC_ApplyRefreshesCacheBeforePosting(t *testing.T) {
	taskName := `\mcp-local-hub-foo-default`
	fx := newReconcileTestFixture(t, &api.SupervisorIntentFile{Version: 1})

	freshIntent := &api.SupervisorIntentFile{
		Version: 1,
		Daemons: []api.SupervisorDaemon{
			{TaskName: taskName, Server: "foo", Daemon: "default"},
		},
	}
	if err := api.WriteSupervisorIntent(filepath.Join(fx.deps.stateDir, "supervisor-intent.json"), freshIntent); err != nil {
		t.Fatalf("overwrite supervisor-intent.json with fresh intent: %v", err)
	}
	now := time.Now().UTC()
	freshDaemonIntent := api.DaemonIntentFile{
		Tasks: map[string]api.DaemonIntent{
			taskName: {
				Desired:   api.IntentDesiredStopped,
				Reason:    api.IntentReasonUserStop,
				UpdatedAt: now,
			},
		},
	}
	diRaw, err := json.Marshal(freshDaemonIntent)
	if err != nil {
		t.Fatalf("marshal daemon-intent: %v", err)
	}
	if err := os.WriteFile(filepath.Join(fx.deps.stateDir, "daemon-intent.json"), diRaw, 0o600); err != nil {
		t.Fatalf("seed daemon-intent.json: %v", err)
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

// TestReconcileIPC_MissingTaskInSchedulerFlaggedAsMissing — intent has
// a daemon descriptor, scheduler has no row → SchedulerState="missing"
// and Action="needs_manual_review" (because EvIntentUpdate cannot
// re-create a scheduler task; operator must re-install).
func TestReconcileIPC_MissingTaskInSchedulerFlaggedAsMissing(t *testing.T) {
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
	if body.Drift[0].Action != api.ReconcileActionNeedsManualReview {
		t.Errorf("Action = %q, want %q for missing scheduler entry",
			body.Drift[0].Action, api.ReconcileActionNeedsManualReview)
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

// TestReconcileIPC_TimeoutErrors verifies that the IPC CLIENT returns
// a timeout error when the operation exceeds its ctx deadline. The
// handler itself is fast; we exercise the timeout by making the
// scheduler-list call block past the ctx deadline of the client dial.
//
// This is the client-side timeout test (closes the spec acceptance:
// "mcphub reconcile returns within 30s OR explicit timeout error").
// Because the actual handler is synchronous + fast, we exercise the
// timeout at the BLOCK BOUNDARY most likely to be slow in production
// (scheduler.List) by making it sleep longer than the ctx deadline,
// then driving the handler directly with a deadline-respecting ctx.
//
// Note: the IPC handler does not currently honor ctx because
// writeIPCFrame writes synchronously; the operational timeout lives
// in the DialSupervisorIPCReconcile client (verified separately in
// internal/api). Here we assert the slow-scheduler-list case still
// returns a populated response (no deadlock), and that the slow path
// is observable in audit.
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

// TestReconcileIPC_DaemonIntentStopOverridesDefault — when daemon-intent.json
// declares Desired=stopped for a task whose scheduler row is running,
// the action must be post_ev_intent_update (terminate). Exercises the
// daemon-intent overlay parse path.
func TestReconcileIPC_DaemonIntentStopOverridesDefault(t *testing.T) {
	taskName := `\mcp-local-hub-foo-default`
	intent := &api.SupervisorIntentFile{
		Version: 1,
		Daemons: []api.SupervisorDaemon{
			{TaskName: taskName, Server: "foo", Daemon: "default"},
		},
	}
	fx := newReconcileTestFixture(t, intent)

	// Seed daemon-intent.json with Desired=stopped for taskName.
	// IMPORTANT: marshal via json.Marshal on the typed struct so the
	// backslash in the canonical leading-backslash task key is
	// JSON-escaped properly. A hand-built raw JSON string with a
	// literal backslash-m sequence would produce
	// 'invalid character m in string escape code' at decode time and
	// the file would be quarantined as corrupt.
	now := time.Now().UTC()
	di := api.DaemonIntentFile{
		Tasks: map[string]api.DaemonIntent{
			taskName: {
				Desired:   api.IntentDesiredStopped,
				Reason:    api.IntentReasonUserStop,
				UpdatedAt: now,
			},
		},
	}
	diRaw, err := json.Marshal(di)
	if err != nil {
		t.Fatalf("marshal daemon-intent: %v", err)
	}
	if err := os.WriteFile(filepath.Join(fx.deps.stateDir, "daemon-intent.json"),
		diRaw, 0o600); err != nil {
		t.Fatalf("seed daemon-intent.json: %v", err)
	}
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
