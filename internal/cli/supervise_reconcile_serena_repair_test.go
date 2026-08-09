// Package cli — composition test for the P1 fix tracked under
// work-items/active (mcphub-register-intent): `mcphub workspace register`
// used to commit a workspaces.yaml row and print an unqualified success
// WITHOUT ever touching supervisor-intent.json — no daemon row, no
// reconcile, no spawn. The fix wires the running supervisor's apply-mode
// `reconcile` IPC handler to self-heal that registry/intent split
// (api.RepairSerenaIntentFromRegistry) BEFORE computing drift, so a
// just-registered workspace's daemon row is appended and reconciled in the
// SAME round trip `mcphub workspace register` triggers via
// DialSupervisorIPCReconcile(apply=true).
//
// This file tests the SERVER side of that wiring (handleReconcile itself,
// supervise_reconcile_ipc.go). The CLIENT side (runWorkspaceRegister's
// gated success message) is tested separately in workspace_cmd_test.go —
// each of the pre-existing "Registry allocation", "full auto-register
// transaction", "auto-register idempotency", "append-only intent repair",
// and "router-miss-only auto-register" tests covers ONE seam in isolation;
// none of them composed "explicit register" with "live-supervisor
// convergence", which is exactly the gap this fixed defect lived in.
package cli

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gofrs/flock"

	"mcp-local-hub/internal/api"
	"mcp-local-hub/internal/scheduler"
)

// seedOrphanSerenaRegistryRow writes one serena sentinel row (no matching
// supervisor-intent daemon — an "orphan", the exact shape a bare `mcphub
// workspace register` used to leave behind pre-fix) into the registry at
// regPath. Returns the derived workspace key.
func seedOrphanSerenaRegistryRow(t *testing.T, regPath, workspacePath string, port int) string {
	t.Helper()
	key := api.WorkspaceKey(workspacePath)
	reg := api.NewRegistry(regPath)
	unlock, err := reg.Lock()
	if err != nil {
		t.Fatalf("lock registry: %v", err)
	}
	defer assertRegistryReleased(t, unlock)
	if err := reg.Load(); err != nil {
		t.Fatalf("load registry: %v", err)
	}
	if err := reg.PutSerena(api.WorkspaceEntry{
		WorkspaceKey:  key,
		WorkspacePath: workspacePath,
		Language:      api.SerenaLanguageSentinel,
		Backend:       api.SerenaServerName,
		Port:          port,
		TaskName:      "mcp-local-hub-serena-" + key,
		Languages:     []string{"python"},
	}); err != nil {
		t.Fatalf("put serena row: %v", err)
	}
	if err := reg.Save(); err != nil {
		t.Fatalf("save registry: %v", err)
	}
	return key
}

// TestReconcileIPC_ApplyRepairsSerenaIntentFromRegistryBeforeDrift seeds an
// orphan serena registry row (registered, but with NO matching
// supervisor-intent daemon) alongside an EXISTING spec-bearing serena daemon
// for a DIFFERENT workspace — satisfying the §7.1 "dynamic pool already
// introduced" precondition so RepairSerenaIntentFromRegistry may APPEND
// rather than defer to `mcphub migrate serena legacy-to-dynamic-pool`. It
// then asserts that ONE apply-mode `reconcile` IPC call:
//
//  1. appends the missing daemon row to supervisor-intent.json (observable
//     both in the handler's own response body's drift AND, durably, on disk
//     via api.ReadSupervisorIntent + HasSpecBearingSerenaDaemonForWorkspaceKey), and
//  2. computes and APPLIES drift against that now-complete intent in the
//     SAME round trip — an EvIntentUpdate is posted for the newly-appended
//     row (the "observable reconcile/start request" this composition proves,
//     i.e. the fix is not merely a file write with no live effect).
func TestReconcileIPC_ApplyRepairsSerenaIntentFromRegistryBeforeDrift(t *testing.T) {
	manifestDir := t.TempDir()
	seedSerenaManifest(t, manifestDir, alreadyMigratedManifestYAML)
	t.Setenv("MCPHUB_MANIFEST_DIR_OVERRIDE", manifestDir)

	healthyWS := t.TempDir()
	healthyKey := api.WorkspaceKey(healthyWS)
	healthyTaskName := `\mcp-local-hub-serena-` + healthyKey
	intent := &api.SupervisorIntentFile{
		Version: 1,
		Daemons: []api.SupervisorDaemon{
			{
				TaskName:  healthyTaskName,
				Server:    api.SerenaServerName,
				Daemon:    "serena-" + healthyKey,
				Command:   "mcphub",
				Args:      []string{"daemon", "serena-proxy", "--task-name", healthyTaskName},
				Workspace: healthyWS,
				Port:      9150,
				// Non-nil RuntimeSpec is the ONLY thing HasRuntimeSpecRow cares
				// about; its contents are otherwise unused by this test.
				RuntimeSpec: &api.DaemonRuntimeSpec{
					SpecVersion:   1,
					ChildCommand:  "uvx",
					UpstreamPort:  19150,
					ExternalPort:  9150,
					WorkspacePath: healthyWS,
				},
			},
		},
	}
	fx := newReconcileTestFixture(t, intent)

	// Seed the orphan AFTER constructing the fixture — newReconcileTestFixture
	// redirects LOCALAPPDATA/XDG_STATE_HOME to its OWN temp root (see its doc
	// comment), so api.DefaultRegistryPath() must be resolved (and the row
	// written) after that redirect is in effect.
	regPath, err := api.DefaultRegistryPath()
	if err != nil {
		t.Fatalf("registry path: %v", err)
	}
	orphanWS := t.TempDir()
	orphanKey := seedOrphanSerenaRegistryRow(t, regPath, orphanWS, 9151)
	orphanTaskName := `\mcp-local-hub-serena-` + orphanKey

	// Neither daemon has a scheduler row — both read as "missing" from the
	// scheduler's perspective; only the orphan's post-repair presence in
	// intent is under test here.
	installSchedulerListFake(t, []scheduler.TaskStatus{})

	req := api.IPCRequest{
		ID:   1001,
		Cmd:  "reconcile",
		Args: map[string]any{"apply": true},
	}
	conn := newFakeIPCConn()
	if err := handleReconcile(conn, req, fx.deps); err != nil {
		t.Fatalf("handleReconcile: %v", err)
	}
	_, body := decodeReconcileResponse(t, conn)

	// The orphan must show up in THIS SAME reconcile pass's drift — proving
	// the repair ran BEFORE the intent read/drift computation, not after.
	var orphanDrift *api.DriftEntry
	for i := range body.Drift {
		if body.Drift[i].TaskName == orphanTaskName {
			orphanDrift = &body.Drift[i]
		}
	}
	if orphanDrift == nil {
		t.Fatalf("no drift entry for the repaired orphan task %s; drift=%+v", orphanTaskName, body.Drift)
	}
	if orphanDrift.IntentDesired != api.ReconcileIntentDesiredRunning {
		t.Errorf("orphan IntentDesired = %q, want %q (the repair-appended row wants running)",
			orphanDrift.IntentDesired, api.ReconcileIntentDesiredRunning)
	}
	if orphanDrift.Action != api.ReconcileActionPostEvIntentUpdate {
		t.Errorf("orphan Action = %q, want %q (missing scheduler + intent-wants-running must spawn)",
			orphanDrift.Action, api.ReconcileActionPostEvIntentUpdate)
	}
	if body.AppliedCount < 1 {
		t.Errorf("AppliedCount = %d, want >= 1 (the orphan's spawn must be applied)", body.AppliedCount)
	}
	if body.SerenaOrphansRepaired != 1 {
		t.Errorf("SerenaOrphansRepaired = %d, want 1 (matches the dry-run preview count — same classification, both modes)", body.SerenaOrphansRepaired)
	}
	if len(body.SerenaOrphansDeferred) != 0 {
		t.Errorf("SerenaOrphansDeferred = %v, want none", body.SerenaOrphansDeferred)
	}
	if body.SerenaRepairOutcome != api.SerenaIntentRepairOutcomeCompleted {
		t.Errorf("SerenaRepairOutcome = %q, want %q", body.SerenaRepairOutcome, api.SerenaIntentRepairOutcomeCompleted)
	}

	// An EvIntentUpdate for the orphan's task name must actually be posted —
	// proving this is a live reconcile effect, not just a durable file write.
	deadline := time.After(2 * time.Second)
	found := false
	for !found {
		select {
		case ev := <-fx.postedCh:
			if ev.Kind == api.EvIntentUpdate && ev.TaskName == orphanTaskName {
				found = true
			}
		case <-deadline:
			t.Fatal("timed out waiting for EvIntentUpdate for the repaired orphan task")
		}
	}

	// Durable proof: re-read supervisor-intent.json from disk (independent of
	// the in-memory response body) and confirm the spec-bearing row for the
	// orphan workspace key is now actually there.
	intentPath := filepath.Join(fx.deps.stateDir, "supervisor-intent.json")
	onDisk, rerr := api.ReadSupervisorIntent(intentPath)
	if rerr != nil {
		t.Fatalf("re-read supervisor-intent.json: %v", rerr)
	}
	if !onDisk.HasSpecBearingSerenaDaemonForWorkspaceKey(orphanKey) {
		t.Fatalf("supervisor-intent.json has no spec-bearing serena daemon for the repaired workspace key %s after apply-mode reconcile", orphanKey)
	}
	// The pre-existing healthy row must be UNTOUCHED (append-only, never
	// replace-all — the core invariant RepairSerenaIntentFromRegistry exists
	// to preserve).
	if !onDisk.HasSpecBearingSerenaDaemonForWorkspaceKey(healthyKey) {
		t.Fatalf("pre-existing healthy serena daemon for workspace key %s was lost — repair must be append-only", healthyKey)
	}
}

// TestReconcileIPC_DryRunDoesNotRepairSerenaIntentFromRegistry pins the
// write-scoping half of the BLOCKING 3 fix (mcphub-register-intent REVISE
// round 2): a dry-run reconcile (apply=false) must NEVER mutate state, so
// commit must not run when args.Apply is false. It ALSO pins the new
// visibility half: the response's SerenaOrphansRepaired/SerenaOrphansDeferred
// fields must report the SAME count a real --apply would materialize — a
// dry-run reconcile can no longer hide that the very next `--apply` would
// silently append this orphan.
func TestReconcileIPC_DryRunDoesNotRepairSerenaIntentFromRegistry(t *testing.T) {
	manifestDir := t.TempDir()
	seedSerenaManifest(t, manifestDir, alreadyMigratedManifestYAML)
	t.Setenv("MCPHUB_MANIFEST_DIR_OVERRIDE", manifestDir)

	healthyWS := t.TempDir()
	healthyKey := api.WorkspaceKey(healthyWS)
	healthyTaskName := `\mcp-local-hub-serena-` + healthyKey
	intent := &api.SupervisorIntentFile{
		Version: 1,
		Daemons: []api.SupervisorDaemon{
			{
				TaskName:  healthyTaskName,
				Server:    api.SerenaServerName,
				Daemon:    "serena-" + healthyKey,
				Command:   "mcphub",
				Workspace: healthyWS,
				Port:      9150,
				RuntimeSpec: &api.DaemonRuntimeSpec{
					SpecVersion:   1,
					ChildCommand:  "uvx",
					UpstreamPort:  19150,
					ExternalPort:  9150,
					WorkspacePath: healthyWS,
				},
			},
		},
	}
	fx := newReconcileTestFixture(t, intent)

	regPath, err := api.DefaultRegistryPath()
	if err != nil {
		t.Fatalf("registry path: %v", err)
	}
	orphanWS := t.TempDir()
	orphanKey := seedOrphanSerenaRegistryRow(t, regPath, orphanWS, 9151)

	installSchedulerListFake(t, []scheduler.TaskStatus{})

	req := api.IPCRequest{
		ID:   1002,
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
	for _, d := range body.Drift {
		if d.TaskName == `\mcp-local-hub-serena-`+orphanKey {
			t.Fatalf("dry-run must not surface the orphan in the drift table (it is not in intent.Daemons — repair only PREVIEWS, it never appends): %+v", d)
		}
	}
	// BLOCKING 3 fix: the preview count/deferred-keys must be visible in the
	// response EVEN THOUGH nothing was written — this is what a dry-run
	// reconcile could never show before this fix.
	if body.SerenaOrphansRepaired != 1 {
		t.Errorf("SerenaOrphansRepaired = %d, want 1 (the orphan a real --apply WOULD materialize)", body.SerenaOrphansRepaired)
	}
	if len(body.SerenaOrphansDeferred) != 0 {
		t.Errorf("SerenaOrphansDeferred = %v, want none", body.SerenaOrphansDeferred)
	}
	if body.SerenaRepairOutcome != api.SerenaIntentRepairOutcomeCompleted {
		t.Errorf("SerenaRepairOutcome = %q, want %q", body.SerenaRepairOutcome, api.SerenaIntentRepairOutcomeCompleted)
	}

	intentPath := filepath.Join(fx.deps.stateDir, "supervisor-intent.json")
	onDisk, rerr := api.ReadSupervisorIntent(intentPath)
	if rerr != nil {
		t.Fatalf("re-read supervisor-intent.json: %v", rerr)
	}
	if onDisk.HasSpecBearingSerenaDaemonForWorkspaceKey(orphanKey) {
		t.Fatalf("dry-run reconcile must NEVER mutate supervisor-intent.json, but the orphan key %s is now present", orphanKey)
	}
}

// TestReconcileIPC_SerenaRepairLockSkipsRemainOK proves the typed repair
// outcome does not turn a lock-contended Serena pass into an IPC transport
// failure. Apply continues to dispatch ordinary drift safely; preview remains
// read-only. Both surfaces report the exact lock that prevented a Serena
// classification so callers cannot mistake a zero repair count for completion.
func TestReconcileIPC_SerenaRepairLockSkipsRemainOK(t *testing.T) {
	type lockKind string
	const (
		registryLock lockKind = "registry"
		intentLock   lockKind = "intent"
	)
	tests := []struct {
		name        string
		apply       bool
		wantOutcome api.SerenaIntentRepairOutcome
		lock        lockKind
	}{
		{
			name:        "apply registry lock",
			apply:       true,
			wantOutcome: api.SerenaIntentRepairOutcomeSkippedRegistryLock,
			lock:        registryLock,
		},
		{
			name:        "preview registry lock",
			apply:       false,
			wantOutcome: api.SerenaIntentRepairOutcomeSkippedRegistryLock,
			lock:        registryLock,
		},
		{
			name:        "apply intent lock",
			apply:       true,
			wantOutcome: api.SerenaIntentRepairOutcomeSkippedIntentLock,
			lock:        intentLock,
		},
		{
			name:        "preview intent lock",
			apply:       false,
			wantOutcome: api.SerenaIntentRepairOutcomeSkippedIntentLock,
			lock:        intentLock,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fx := newReconcileTestFixture(t, &api.SupervisorIntentFile{Version: 1})
			installSchedulerListFake(t, []scheduler.TaskStatus{})
			registryPath, err := api.DefaultRegistryPath()
			if err != nil {
				t.Fatalf("registry path: %v", err)
			}
			// A real row forces the repair past the empty-registry early return,
			// allowing the intent-lock cases to reach their TryLock gate.
			seedOrphanSerenaRegistryRow(t, registryPath, t.TempDir(), 9151)
			intentPath := filepath.Join(fx.deps.stateDir, "supervisor-intent.json")
			intentBefore, err := os.ReadFile(intentPath)
			if err != nil {
				t.Fatalf("read intent before: %v", err)
			}
			registryBefore, err := os.ReadFile(registryPath)
			if err != nil {
				t.Fatalf("read registry before: %v", err)
			}

			var release func() error
			switch tt.lock {
			case registryLock:
				reg := api.NewRegistry(registryPath)
				unlock, err := reg.Lock()
				if err != nil {
					t.Fatalf("hold registry lock: %v", err)
				}
				release = unlock
			case intentLock:
				lock := flock.New(filepath.Join(fx.deps.stateDir, "supervisor-intent.json.lock"))
				locked, err := lock.TryLock()
				if err != nil {
					t.Fatalf("hold intent lock: %v", err)
				}
				if !locked {
					t.Fatal("could not acquire intent lock to simulate contention")
				}
				release = lock.Unlock
			default:
				t.Fatalf("unknown lock kind %q", tt.lock)
			}
			defer assertRegistryReleased(t, release)
			conn := newFakeIPCConn()
			req := api.IPCRequest{ID: 1100, Cmd: "reconcile", Args: map[string]any{"apply": tt.apply}}
			if err := handleReconcile(conn, req, fx.deps); err != nil {
				t.Fatalf("handleReconcile: %v", err)
			}
			frame, body := decodeReconcileResponse(t, conn)
			if !frame.OK || frame.Error != nil {
				t.Fatalf("lock skip must keep an OK IPC frame; got OK=%v error=%+v", frame.OK, frame.Error)
			}
			if body.SerenaRepairOutcome != tt.wantOutcome {
				t.Errorf("SerenaRepairOutcome = %q, want %q", body.SerenaRepairOutcome, tt.wantOutcome)
			}
			if body.SerenaRepairError != "" {
				t.Errorf("SerenaRepairError = %q, want empty for a lock skip", body.SerenaRepairError)
			}
			if body.SerenaOrphansRepaired != 0 || len(body.SerenaOrphansDeferred) != 0 {
				t.Errorf("repair result = repaired=%d deferred=%v, want zero result on a lock skip", body.SerenaOrphansRepaired, body.SerenaOrphansDeferred)
			}
			intentAfter, err := os.ReadFile(intentPath)
			if err != nil {
				t.Fatalf("read intent after: %v", err)
			}
			registryAfter, err := os.ReadFile(registryPath)
			if err != nil {
				t.Fatalf("read registry after: %v", err)
			}
			if string(intentAfter) != string(intentBefore) || string(registryAfter) != string(registryBefore) {
				t.Fatalf("lock skip mutated repair inputs: intentChanged=%v registryChanged=%v", string(intentAfter) != string(intentBefore), string(registryAfter) != string(registryBefore))
			}
			eventRaw, err := os.ReadFile(filepath.Join(fx.deps.stateDir, "supervisor-events.log"))
			if err != nil {
				t.Fatalf("read supervisor event log: %v", err)
			}
			if tt.apply {
				if !strings.Contains(string(eventRaw), `"event":"serena-intent-repair-skipped"`) {
					t.Errorf("apply-mode lock skip did not emit serena-intent-repair-skipped; log=%s", eventRaw)
				}
				if !strings.Contains(string(eventRaw), `"outcome":"`+string(tt.wantOutcome)+`"`) || !strings.Contains(string(eventRaw), `"retryable":true`) {
					t.Errorf("apply-mode skip event did not carry exact outcome/retryable fields; log=%s", eventRaw)
				}
			} else if strings.Contains(string(eventRaw), `"event":"serena-intent-repair-skipped"`) {
				t.Errorf("dry-run preview emitted a repair-mutation skip event; log=%s", eventRaw)
			}
		})
	}
}

// TestEmitSerenaIntentRepairOutcomePreservesAuditVocabulary pins the one audit
// owner shared by startup and apply-mode reconcile. Lock skips gain their own
// retryable event without renaming the established failed/result events.
func TestEmitSerenaIntentRepairOutcomePreservesAuditVocabulary(t *testing.T) {
	tests := []struct {
		name         string
		result       api.SerenaIntentRepairResult
		err          error
		wantEvent    string
		wantContains []string
	}{
		{
			name:      "registry lock skip",
			result:    api.SerenaIntentRepairResult{Outcome: api.SerenaIntentRepairOutcomeSkippedRegistryLock},
			wantEvent: "serena-intent-repair-skipped",
			wantContains: []string{
				`"outcome":"skipped_registry_lock"`,
				`"retryable":true`,
			},
		},
		{
			name:      "intent lock skip",
			result:    api.SerenaIntentRepairResult{Outcome: api.SerenaIntentRepairOutcomeSkippedIntentLock},
			wantEvent: "serena-intent-repair-skipped",
			wantContains: []string{
				`"outcome":"skipped_intent_lock"`,
				`"retryable":true`,
			},
		},
		{
			name: "pending removal incomplete",
			result: api.SerenaIntentRepairResult{
				Outcome:    api.SerenaIntentRepairOutcomeIncompleteRemovalFence,
				Incomplete: []api.SerenaIntentRepairIncomplete{{WorkspaceKey: "abcd1234", Reason: api.SerenaIntentRepairIncompleteGenerationMismatch}},
				Recovered:  []api.SerenaIntentRepairRecovery{{WorkspaceKey: "efgh5678", Reason: api.SerenaIntentRepairRecoveryGenerationReclaimed}},
			},
			wantEvent: "serena-intent-repair-skipped",
			wantContains: []string{
				`"outcome":"incomplete_removal_fence"`,
				`"reason":"generation_mismatch"`,
				`"reason":"generation_reclaimed"`,
				`"retryable":true`,
			},
		},
		{
			name:      "actual failure",
			result:    api.SerenaIntentRepairResult{Outcome: api.SerenaIntentRepairOutcomeError},
			err:       errors.New("injected catalog failure"),
			wantEvent: "serena-intent-repair-failed",
			wantContains: []string{
				`"outcome":"error"`,
				"injected catalog failure",
			},
		},
		{
			name:      "completed materialization",
			result:    api.SerenaIntentRepairResult{Outcome: api.SerenaIntentRepairOutcomeCompleted, Repaired: 1},
			wantEvent: "serena-intent-repair-result",
			wantContains: []string{
				`"outcome":"completed"`,
				`"repaired_count":1`,
			},
		},
		{
			name: "completed pending-removal recoveries",
			result: api.SerenaIntentRepairResult{
				Outcome: api.SerenaIntentRepairOutcomeCompleted,
				Recovered: []api.SerenaIntentRepairRecovery{
					{WorkspaceKey: "abcd1234", Reason: api.SerenaIntentRepairRecoveryGenerationReclaimed},
					{WorkspaceKey: "efgh5678", Reason: api.SerenaIntentRepairRecoveryLegacyLeaseExpired},
				},
			},
			wantEvent: "serena-intent-repair-result",
			wantContains: []string{
				`"reason":"generation_reclaimed"`,
				`"reason":"legacy_lease_expired"`,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "supervisor-events.log")
			events, err := api.OpenSupervisorEventLog(path)
			if err != nil {
				t.Fatalf("open supervisor event log: %v", err)
			}
			emitSerenaIntentRepairOutcome(events, tt.result, tt.err)
			events.Close()
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read supervisor event log: %v", err)
			}
			if !strings.Contains(string(raw), `"event":"`+tt.wantEvent+`"`) {
				t.Fatalf("missing event %q; log=%s", tt.wantEvent, raw)
			}
			for _, want := range tt.wantContains {
				if !strings.Contains(string(raw), want) {
					t.Errorf("event log missing %q; log=%s", want, raw)
				}
			}
		})
	}
}
