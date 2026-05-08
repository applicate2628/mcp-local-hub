// Tests for Task 10 (install_intent.go + Install/Stop/Restart/Uninstall/
// Register call-site wiring) — verify intent + audit writes per the
// fail-handling table from plan v13 §65 + Task 10:
//
//	mcphub stop --server X            BEFORE kill   fail-closed both ways
//	mcphub stop --server X --force    skip intent   fail-closed if audit
//	                                                fails (incl.
//	                                                ErrIdentityOversize)
//	mcphub install <s>                AUDIT-FIRST   fail-closed
//	mcphub register <ws> <lang>       AFTER PASS    log warning + cont.
//	mcphub restart                    AFTER /Run    log warning + cont.
//	mcphub uninstall                  BEFORE delete log + proceed
//
// Tests use the same daemonStateRootOverride seam (state_paths_test.go)
// + appendIntentAuditFn seam (api_surfaces_test.go) the daemon-intent
// and audit tests use, so each case runs hermetically against a per-
// test temp dir.
package api

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"mcp-local-hub/internal/config"
)

// recordingAuditWriter mirrors installTestAuditFn but additionally
// supports per-Action fail-injection for tests that exercise the
// fail-closed boundaries. Captures every entry passed through
// appendIntentAuditFn so assertions can inspect the recorded shape.
type recordingAuditWriter struct {
	entries     []IntentAuditEntry
	failActions map[string]error // Action → error to return; "*" matches any
	calls       int32
}

// installRecordingAudit installs r as the appendIntentAuditFn seam for
// the duration of t. Returns the previous function so callers can
// re-bind in the middle of a test if needed.
func installRecordingAudit(t *testing.T, r *recordingAuditWriter) {
	t.Helper()
	orig := appendIntentAuditFn
	appendIntentAuditFn = func(e IntentAuditEntry) error {
		atomic.AddInt32(&r.calls, 1)
		// Snapshot the entry struct (it's a value type; pointer
		// fields like Before/After are still aliased but tests don't
		// mutate them).
		r.entries = append(r.entries, e)
		if r.failActions != nil {
			if err, ok := r.failActions[e.Action]; ok && err != nil {
				return err
			}
			if err, ok := r.failActions["*"]; ok && err != nil {
				return err
			}
		}
		return nil
	}
	t.Cleanup(func() { appendIntentAuditFn = orig })
}

// stopFakeKillCounter installs a no-op replacement for lookupProcess so
// the kill path becomes a counted no-op. Returns a pointer to the
// counter so tests can assert on it. Restoration is automatic.
func stopFakeKillCounter(t *testing.T) *int32 {
	t.Helper()
	var counter int32
	orig := lookupProcess
	lookupProcess = func(port int) (int, uint64, int64, bool) {
		atomic.AddInt32(&counter, 1)
		return 0, 0, 0, false // ok=false → killDaemonByPort returns nil
	}
	t.Cleanup(func() { lookupProcess = orig })
	return &counter
}

// ---------------------------------------------------------------------------
// recordStopIntent — direct unit tests on the helper.
// ---------------------------------------------------------------------------

func TestRecordStopIntent_NoForce_HappyPath(t *testing.T) {
	a := NewAPI()
	dir := daemonIntentTestHelper(t)
	r := &recordingAuditWriter{}
	installRecordingAudit(t, r)

	taskNames := []string{"mcp-local-hub-time-default"}
	if err := a.recordStopIntent(taskNames, false); err != nil {
		t.Fatalf("recordStopIntent: %v", err)
	}
	// Two audit entries per task: set-intent (auto from
	// WriteDaemonIntent) + user-stop (explicit). Codex deep-sec PR #135
	// Finding 1: every audit entry now carries the canonical leading-
	// backslash task identity so log filters pivot on one shape.
	wantActions := map[string]bool{
		"set-intent":          false,
		AuditActionUserStop:   false,
	}
	canonicalTask := "\\" + taskNames[0]
	for _, e := range r.entries {
		if _, ok := wantActions[e.Action]; ok {
			wantActions[e.Action] = true
		}
		if e.Task != canonicalTask {
			t.Errorf("audit Task = %q, want %q (canonical leading-backslash form)", e.Task, canonicalTask)
		}
	}
	for action, seen := range wantActions {
		if !seen {
			t.Errorf("expected audit Action=%q in entries: %+v", action, r.entries)
		}
	}
	// Intent file written with Desired=stopped + Reason=user-stop.
	res := a.ReadDaemonIntent()
	if res.State != IntentStateValid {
		t.Fatalf("intent state = %q, want valid (dir=%s)", res.State, dir)
	}
	got, ok := res.File.Tasks[canonicalTask]
	if !ok {
		t.Fatalf("intent file missing entry for %s; tasks=%+v", canonicalTask, res.File.Tasks)
	}
	if got.Desired != IntentDesiredStopped {
		t.Errorf("Desired = %q, want %q", got.Desired, IntentDesiredStopped)
	}
	if got.Reason != IntentReasonUserStop {
		t.Errorf("Reason = %q, want %q", got.Reason, IntentReasonUserStop)
	}
}

func TestRecordStopIntent_NoForce_AuditFailsErrIdentityOversize(t *testing.T) {
	a := NewAPI()
	daemonIntentTestHelper(t)
	// Audit returns ErrIdentityOversize specifically for the
	// user-stop action. set-intent (emitted by WriteDaemonIntent)
	// passes through so the intent file is still written, but the
	// caller MUST fail closed on the user-stop audit failure.
	r := &recordingAuditWriter{
		failActions: map[string]error{AuditActionUserStop: ErrIdentityOversize},
	}
	installRecordingAudit(t, r)

	err := a.recordStopIntent([]string{"mcp-local-hub-time-default"}, false)
	if err == nil {
		t.Fatal("recordStopIntent: want error on ErrIdentityOversize, got nil")
	}
	if !errors.Is(err, ErrIdentityOversize) {
		t.Errorf("error chain: want ErrIdentityOversize, got %v", err)
	}
}

func TestRecordStopIntent_NoForce_AuditFailsGenericError(t *testing.T) {
	a := NewAPI()
	daemonIntentTestHelper(t)
	stubErr := errors.New("synthetic audit disk-full")
	r := &recordingAuditWriter{
		failActions: map[string]error{AuditActionUserStop: stubErr},
	}
	installRecordingAudit(t, r)

	err := a.recordStopIntent([]string{"mcp-local-hub-time-default"}, false)
	if err == nil {
		t.Fatal("recordStopIntent: want error on generic audit failure, got nil")
	}
	if !errors.Is(err, stubErr) {
		t.Errorf("error chain: want stubErr, got %v", err)
	}
}

func TestRecordStopIntent_Force_HappyPath(t *testing.T) {
	a := NewAPI()
	daemonIntentTestHelper(t)
	r := &recordingAuditWriter{}
	installRecordingAudit(t, r)

	taskNames := []string{"mcp-local-hub-time-default"}
	if err := a.recordStopIntent(taskNames, true); err != nil {
		t.Fatalf("recordStopIntent force: %v", err)
	}
	// Force path emits ONLY the forced-stop-without-intent audit
	// entry — no WriteDaemonIntent → no set-intent + no user-stop.
	if len(r.entries) != 1 {
		t.Fatalf("audit entries = %d, want 1: %+v", len(r.entries), r.entries)
	}
	got := r.entries[0]
	if got.Action != AuditActionForcedStopWithoutIntent {
		t.Errorf("Action = %q, want %q", got.Action, AuditActionForcedStopWithoutIntent)
	}
	if got.Priority != "high" {
		t.Errorf("Priority = %q, want %q", got.Priority, "high")
	}
	// Codex deep-sec PR #135 Finding 1: audit Task field is canonical.
	canonicalTask := "\\" + taskNames[0]
	if got.Task != canonicalTask {
		t.Errorf("Task = %q, want %q", got.Task, canonicalTask)
	}
	// Intent file MUST NOT contain an entry for this task: --force
	// explicitly skips the intent write so the watchdog auto-revives.
	res := a.ReadDaemonIntent()
	if _, ok := res.File.Tasks[canonicalTask]; ok {
		t.Errorf("intent file unexpectedly contains entry for %s after --force stop: %+v",
			canonicalTask, res.File.Tasks[canonicalTask])
	}
}

func TestRecordStopIntent_Force_AuditFailsErrIdentityOversize(t *testing.T) {
	a := NewAPI()
	daemonIntentTestHelper(t)
	r := &recordingAuditWriter{
		failActions: map[string]error{AuditActionForcedStopWithoutIntent: ErrIdentityOversize},
	}
	installRecordingAudit(t, r)

	err := a.recordStopIntent([]string{"mcp-local-hub-time-default"}, true)
	if err == nil {
		t.Fatal("recordStopIntent force: want error on ErrIdentityOversize, got nil")
	}
	if !errors.Is(err, ErrIdentityOversize) {
		t.Errorf("error chain: want ErrIdentityOversize, got %v", err)
	}
	// No intent file ever existed (force path skips it). After the
	// fail-closed audit we still expect IntentStateMissing.
	res := a.ReadDaemonIntent()
	if res.State != IntentStateMissing {
		t.Errorf("intent state = %q, want missing on force-only fail-closed", res.State)
	}
}

// ---------------------------------------------------------------------------
// recordInstallAuditPreMutation — install audit-first per §62.
// ---------------------------------------------------------------------------

func installAuditPreMutationTestManifest() *config.ServerManifest {
	return &config.ServerManifest{
		Name:      "task10test",
		Kind:      config.KindGlobal,
		Transport: config.TransportNativeHTTP,
		Command:   "uvx",
		Daemons: []config.DaemonSpec{
			{Name: "alpha", Port: 9601},
			{Name: "beta", Port: 9602},
		},
	}
}

func TestRecordInstallAuditPreMutation_HappyPath(t *testing.T) {
	a := NewAPI()
	daemonIntentTestHelper(t)
	r := &recordingAuditWriter{}
	installRecordingAudit(t, r)
	m := installAuditPreMutationTestManifest()

	if err := a.recordInstallAuditPreMutation(m, ""); err != nil {
		t.Fatalf("recordInstallAuditPreMutation: %v", err)
	}
	if len(r.entries) != 2 {
		t.Fatalf("audit entries = %d, want 2 (one per daemon): %+v", len(r.entries), r.entries)
	}
	// Codex deep-sec PR #135 Finding 1: audit Task field is the canonical
	// leading-backslash form so log filters pivot on one shape.
	wantTasks := []string{"\\mcp-local-hub-task10test-alpha", "\\mcp-local-hub-task10test-beta"}
	for i, e := range r.entries {
		if e.Action != AuditActionServerInstall {
			t.Errorf("entries[%d].Action = %q, want %q", i, e.Action, AuditActionServerInstall)
		}
		if e.Task != wantTasks[i] {
			t.Errorf("entries[%d].Task = %q, want %q", i, e.Task, wantTasks[i])
		}
		if e.Reason != IntentReasonInstall {
			t.Errorf("entries[%d].Reason = %q, want %q", i, e.Reason, IntentReasonInstall)
		}
	}
}

func TestRecordInstallAuditPreMutation_AuditFailsErrIdentityOversize(t *testing.T) {
	a := NewAPI()
	daemonIntentTestHelper(t)
	r := &recordingAuditWriter{
		failActions: map[string]error{AuditActionServerInstall: ErrIdentityOversize},
	}
	installRecordingAudit(t, r)
	m := installAuditPreMutationTestManifest()

	err := a.recordInstallAuditPreMutation(m, "")
	if err == nil {
		t.Fatal("recordInstallAuditPreMutation: want error, got nil")
	}
	if !errors.Is(err, ErrIdentityOversize) {
		t.Errorf("error chain: want ErrIdentityOversize, got %v", err)
	}
	// On the FIRST failure we abort: only one entry should have been
	// recorded (the second daemon never reaches the audit append).
	if len(r.entries) != 1 {
		t.Errorf("audit entries on failure = %d, want 1 (early abort): %+v", len(r.entries), r.entries)
	}
}

func TestRecordInstallAuditPreMutation_DaemonFilter_OnlyOneTask(t *testing.T) {
	a := NewAPI()
	daemonIntentTestHelper(t)
	r := &recordingAuditWriter{}
	installRecordingAudit(t, r)
	m := installAuditPreMutationTestManifest()

	if err := a.recordInstallAuditPreMutation(m, "beta"); err != nil {
		t.Fatalf("recordInstallAuditPreMutation: %v", err)
	}
	if len(r.entries) != 1 {
		t.Fatalf("audit entries = %d, want 1 (filter=beta): %+v", len(r.entries), r.entries)
	}
	// Codex deep-sec PR #135 Finding 1: canonical leading-backslash form.
	if got := r.entries[0].Task; got != "\\mcp-local-hub-task10test-beta" {
		t.Errorf("entries[0].Task = %q, want \\mcp-local-hub-task10test-beta", got)
	}
}

// ---------------------------------------------------------------------------
// recordInstallIntentPostSuccess — best-effort post-install intent
// (audit-fail tolerated; intent-fail is logged through the writer).
// ---------------------------------------------------------------------------

func TestRecordInstallIntentPostSuccess_HappyPath(t *testing.T) {
	a := NewAPI()
	daemonIntentTestHelper(t)
	r := &recordingAuditWriter{}
	installRecordingAudit(t, r)
	m := installAuditPreMutationTestManifest()
	var buf bytes.Buffer

	a.recordInstallIntentPostSuccess(m, "", &buf)

	res := a.ReadDaemonIntent()
	if res.State != IntentStateValid {
		t.Fatalf("intent state = %q, want valid", res.State)
	}
	// Codex deep-sec PR #135 Finding 1: canonical leading-backslash key.
	for _, name := range []string{"\\mcp-local-hub-task10test-alpha", "\\mcp-local-hub-task10test-beta"} {
		got, ok := res.File.Tasks[name]
		if !ok {
			t.Errorf("intent file missing entry for %s; tasks=%+v", name, res.File.Tasks)
			continue
		}
		if got.Desired != IntentDesiredRunning {
			t.Errorf("%s.Desired = %q, want %q", name, got.Desired, IntentDesiredRunning)
		}
		if got.Reason != IntentReasonInstall {
			t.Errorf("%s.Reason = %q, want %q", name, got.Reason, IntentReasonInstall)
		}
	}
	if buf.Len() != 0 {
		t.Errorf("expected no warnings on happy path, got: %s", buf.String())
	}
}

func TestRecordInstallIntentPostSuccess_AuditFails_LoggedNotPropagated(t *testing.T) {
	a := NewAPI()
	daemonIntentTestHelper(t)
	// All set-intent audit appends fail. WriteDaemonIntent itself
	// still succeeds (the on-disk file is updated atomically before
	// the audit is dispatched), so the intent file ends up populated.
	// The function MUST NOT propagate the audit error.
	r := &recordingAuditWriter{
		failActions: map[string]error{"set-intent": errors.New("synthetic disk-full")},
	}
	installRecordingAudit(t, r)
	m := installAuditPreMutationTestManifest()
	var buf bytes.Buffer

	a.recordInstallIntentPostSuccess(m, "", &buf)

	// Even though audit failed, intent file should still be written
	// because WriteDaemonIntent's audit path is fail-open.
	res := a.ReadDaemonIntent()
	if res.State != IntentStateValid {
		t.Fatalf("intent state = %q, want valid (intent write succeeded; audit-fail is best-effort)", res.State)
	}
	if len(res.File.Tasks) != 2 {
		t.Errorf("intent entries = %d, want 2", len(res.File.Tasks))
	}
}

// ---------------------------------------------------------------------------
// stopTaskNamesForServer — manifest-derived task name resolution.
// ---------------------------------------------------------------------------

func TestStopTaskNamesForServer_RequiresServer(t *testing.T) {
	if _, err := stopTaskNamesForServer("", ""); err == nil {
		t.Fatal("stopTaskNamesForServer: want error on empty server")
	}
}

func TestStopTaskNamesForServer_UnknownServer_Errors(t *testing.T) {
	_, err := stopTaskNamesForServer("does-not-exist", "")
	if err == nil {
		t.Fatal("stopTaskNamesForServer: want error on unknown server")
	}
	if !strings.Contains(err.Error(), "load manifest") {
		t.Errorf("error message: want 'load manifest', got %v", err)
	}
}

// ---------------------------------------------------------------------------
// stopTaskNamesForServer — workspace registry fail-closed (bot P1.2).
//
// For workspace-scoped servers, registry path/load failures must NOT
// silently return an empty task-name set: the caller (Stop / StopWithOpts)
// would proceed to stopKillCore with no intent/audit recording, killing
// daemons that the watchdog could immediately revive. Plan §8 "Stop
// fail-closed both ways" requires that if intent paths can't be
// determined, the kill is skipped.
// ---------------------------------------------------------------------------

// pointRegistryAtTempDir routes DefaultRegistryPath()'s env-driven lookup
// at a fresh temp directory. Returns the path that the registry helper
// would resolve to so the caller can plant a corrupt file there. Both
// LOCALAPPDATA (Windows) and XDG_STATE_HOME (POSIX) are set so the test
// is platform-agnostic — DefaultRegistryPath consults LOCALAPPDATA first
// on Windows and XDG_STATE_HOME on Linux/macOS.
func pointRegistryAtTempDir(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	t.Setenv("LOCALAPPDATA", root)
	t.Setenv("XDG_STATE_HOME", root)
	regPath, err := DefaultRegistryPath()
	if err != nil {
		t.Fatalf("DefaultRegistryPath: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(regPath), 0o700); err != nil {
		t.Fatalf("mkdir registry dir: %v", err)
	}
	return regPath
}

// TestStopTaskNamesForServer_Workspace_RegistryLoadFails_ReturnsError
// covers bot P1.2: a corrupt workspaces.yaml causes reg.Load() to error.
// The previous behavior returned (nil, nil) and let Stop proceed; now it
// must propagate the error so the caller refuses to kill.
func TestStopTaskNamesForServer_Workspace_RegistryLoadFails_ReturnsError(t *testing.T) {
	regPath := pointRegistryAtTempDir(t)
	// Corrupt YAML — reg.Load wraps the parse error.
	if err := os.WriteFile(regPath, []byte("this: is: not\n  - valid: ["), 0o600); err != nil {
		t.Fatalf("seed corrupt registry: %v", err)
	}
	// mcp-language-server is the shipped workspace-scoped manifest; it
	// reaches the workspace registry branch in stopTaskNamesForServer.
	_, err := stopTaskNamesForServer("mcp-language-server", "")
	if err == nil {
		t.Fatal("stopTaskNamesForServer: want error on registry load failure, got nil")
	}
	if !strings.Contains(err.Error(), "registry") {
		t.Errorf("error message: want mention of registry, got %v", err)
	}
}

// TestStopWithOpts_Workspace_RegistryLoadFails_NoKill is the end-to-end
// equivalent of bot P1.2: when registry load fails for a workspace-scoped
// server, StopWithOpts must return the error WITHOUT calling stopKillCore.
// The kill counter (stopFakeKillCounter) confirms that the kill path
// never runs — daemons stay alive instead of being killed without any
// intent/audit record.
func TestStopWithOpts_Workspace_RegistryLoadFails_NoKill(t *testing.T) {
	a := NewAPI()
	daemonIntentTestHelper(t)
	regPath := pointRegistryAtTempDir(t)
	if err := os.WriteFile(regPath, []byte("this: is: not\n  - valid: ["), 0o600); err != nil {
		t.Fatalf("seed corrupt registry: %v", err)
	}
	r := &recordingAuditWriter{}
	installRecordingAudit(t, r)
	killCounter := stopFakeKillCounter(t)

	_, err := a.StopWithOpts(StopOpts{Server: "mcp-language-server", Force: false})
	if err == nil {
		t.Fatal("StopWithOpts: want error on registry load failure, got nil")
	}
	if got := atomic.LoadInt32(killCounter); got != 0 {
		t.Errorf("kill path invoked %d times on registry-load fail-closed; want 0 (plan §8)", got)
	}
	// Codex deep-sec PR #135 Finding 3: forensic-trail audit entry recorded
	// on the early-exit fail-closed path. Exactly one stop-failed-no-kill
	// entry; no set-intent / user-stop because the kill path never ran.
	if len(r.entries) != 1 {
		t.Fatalf("audit entries on fail-closed = %d, want 1 (stop-failed-no-kill): %+v", len(r.entries), r.entries)
	}
	if r.entries[0].Action != AuditActionStopFailedNoKill {
		t.Errorf("audit Action = %q, want %q", r.entries[0].Action, AuditActionStopFailedNoKill)
	}
}

// TestStop_Workspace_RegistryLoadFails_NoKill mirrors the StopWithOpts
// case for the back-compat Stop entry point so both surfaces share the
// fail-closed contract.
func TestStop_Workspace_RegistryLoadFails_NoKill(t *testing.T) {
	a := NewAPI()
	daemonIntentTestHelper(t)
	regPath := pointRegistryAtTempDir(t)
	if err := os.WriteFile(regPath, []byte("this: is: not\n  - valid: ["), 0o600); err != nil {
		t.Fatalf("seed corrupt registry: %v", err)
	}
	r := &recordingAuditWriter{}
	installRecordingAudit(t, r)
	killCounter := stopFakeKillCounter(t)

	_, err := a.Stop("mcp-language-server", "")
	if err == nil {
		t.Fatal("Stop: want error on registry load failure, got nil")
	}
	if got := atomic.LoadInt32(killCounter); got != 0 {
		t.Errorf("kill path invoked %d times on registry-load fail-closed; want 0 (plan §8)", got)
	}
	// Codex deep-sec PR #135 Finding 3: forensic-trail audit entry recorded
	// on the early-exit fail-closed path.
	if len(r.entries) != 1 {
		t.Fatalf("audit entries on fail-closed = %d, want 1 (stop-failed-no-kill): %+v", len(r.entries), r.entries)
	}
	if r.entries[0].Action != AuditActionStopFailedNoKill {
		t.Errorf("audit Action = %q, want %q", r.entries[0].Action, AuditActionStopFailedNoKill)
	}
}

// ---------------------------------------------------------------------------
// recordRestartIntentForTask / recordUninstallIntentForTasks /
// recordRegisterIntentForTask — best-effort behavior on audit failure.
// ---------------------------------------------------------------------------

func TestRecordRestartIntentForTask_AuditFails_LoggedNotPropagated(t *testing.T) {
	a := NewAPI()
	daemonIntentTestHelper(t)
	r := &recordingAuditWriter{
		failActions: map[string]error{"*": errors.New("synthetic audit failure")},
	}
	installRecordingAudit(t, r)
	var buf bytes.Buffer

	a.recordRestartIntentForTask("\\mcp-local-hub-time-default", &buf)
	// Restart audit fail must NOT propagate. We write to buf so a
	// non-empty buffer is the success signal here.
	if buf.Len() == 0 {
		t.Errorf("expected warning written on audit fail, buf empty")
	}
}

func TestRecordRestartIntentForTask_HappyPath(t *testing.T) {
	a := NewAPI()
	daemonIntentTestHelper(t)
	r := &recordingAuditWriter{}
	installRecordingAudit(t, r)
	var buf bytes.Buffer

	a.recordRestartIntentForTask("\\mcp-local-hub-time-default", &buf)

	res := a.ReadDaemonIntent()
	if res.State != IntentStateValid {
		t.Fatalf("intent state = %q, want valid", res.State)
	}
	got, ok := res.File.Tasks["\\mcp-local-hub-time-default"]
	if !ok {
		t.Fatalf("intent file missing entry")
	}
	if got.Desired != IntentDesiredRunning {
		t.Errorf("Desired = %q, want %q", got.Desired, IntentDesiredRunning)
	}
	if got.Reason != IntentReasonInstall {
		t.Errorf("Reason = %q, want %q (restart re-asserts install intent)", got.Reason, IntentReasonInstall)
	}
	// Audit captured the server-restarted Action.
	sawServerRestarted := false
	for _, e := range r.entries {
		if e.Action == AuditActionServerRestarted {
			sawServerRestarted = true
			break
		}
	}
	if !sawServerRestarted {
		t.Errorf("expected Action=%q in audit entries: %+v", AuditActionServerRestarted, r.entries)
	}
}

func TestRecordUninstallIntentForTasks_HappyPath(t *testing.T) {
	a := NewAPI()
	daemonIntentTestHelper(t)
	r := &recordingAuditWriter{}
	installRecordingAudit(t, r)
	var buf bytes.Buffer

	tasks := []string{"mcp-local-hub-task10test-alpha", "mcp-local-hub-task10test-beta"}
	a.recordUninstallIntentForTasks(tasks, &buf)

	res := a.ReadDaemonIntent()
	if res.State != IntentStateValid {
		t.Fatalf("intent state = %q, want valid", res.State)
	}
	// Codex deep-sec PR #135 Finding 1: storage uses canonical leading-
	// backslash form regardless of caller-supplied shape.
	canonicalTasks := []string{"\\" + tasks[0], "\\" + tasks[1]}
	for _, name := range canonicalTasks {
		got, ok := res.File.Tasks[name]
		if !ok {
			t.Errorf("intent file missing entry for %s; tasks=%+v", name, res.File.Tasks)
			continue
		}
		if got.Desired != IntentDesiredStopped {
			t.Errorf("%s.Desired = %q, want %q", name, got.Desired, IntentDesiredStopped)
		}
		if got.Reason != IntentReasonUninstalled {
			t.Errorf("%s.Reason = %q, want %q", name, got.Reason, IntentReasonUninstalled)
		}
	}
	// Audit emitted server-uninstalled per task.
	count := 0
	for _, e := range r.entries {
		if e.Action == AuditActionServerUninstalled {
			count++
		}
	}
	if count != len(tasks) {
		t.Errorf("server-uninstalled audit entries = %d, want %d", count, len(tasks))
	}
}

func TestRecordUninstallIntentForTasks_AuditFails_LoggedNotPropagated(t *testing.T) {
	a := NewAPI()
	daemonIntentTestHelper(t)
	r := &recordingAuditWriter{
		failActions: map[string]error{
			"*": errors.New("synthetic audit failure"),
		},
	}
	installRecordingAudit(t, r)
	var buf bytes.Buffer

	a.recordUninstallIntentForTasks([]string{"mcp-local-hub-task10test-alpha"}, &buf)
	// Uninstall MUST NOT propagate audit errors. The writer collected
	// the warning text — function returned without panicking.
	if buf.Len() == 0 {
		t.Error("expected warnings written on uninstall audit fail")
	}
}

func TestRecordRegisterIntentForTask_HappyPath(t *testing.T) {
	a := NewAPI()
	daemonIntentTestHelper(t)
	r := &recordingAuditWriter{}
	installRecordingAudit(t, r)
	var buf bytes.Buffer

	taskName := "mcp-local-hub-lsp-deadbeef-python"
	a.recordRegisterIntentForTask(taskName, &buf)

	res := a.ReadDaemonIntent()
	// Codex deep-sec PR #135 Finding 1: storage uses canonical leading-
	// backslash form regardless of caller-supplied shape.
	canonicalTask := "\\" + taskName
	got, ok := res.File.Tasks[canonicalTask]
	if !ok {
		t.Fatalf("intent file missing entry for %s; res.State=%q tasks=%+v", canonicalTask, res.State, res.File.Tasks)
	}
	if got.Desired != IntentDesiredRunning {
		t.Errorf("Desired = %q, want %q", got.Desired, IntentDesiredRunning)
	}
	if got.Reason != IntentReasonRegister {
		t.Errorf("Reason = %q, want %q", got.Reason, IntentReasonRegister)
	}
	// Audit emitted workspace-registered.
	saw := false
	for _, e := range r.entries {
		if e.Action == AuditActionWorkspaceRegistered {
			saw = true
		}
	}
	if !saw {
		t.Errorf("expected Action=%q in audit entries: %+v", AuditActionWorkspaceRegistered, r.entries)
	}
}

func TestRecordRegisterIntentForTask_AuditFails_LoggedNotPropagated(t *testing.T) {
	a := NewAPI()
	daemonIntentTestHelper(t)
	r := &recordingAuditWriter{
		failActions: map[string]error{"*": errors.New("synthetic")},
	}
	installRecordingAudit(t, r)
	var buf bytes.Buffer

	a.recordRegisterIntentForTask("mcp-local-hub-lsp-deadbeef-python", &buf)
	if buf.Len() == 0 {
		t.Error("expected warnings written on register audit fail")
	}
}

// ---------------------------------------------------------------------------
// installAuditTaskNames — pure helper.
// ---------------------------------------------------------------------------

func TestInstallAuditTaskNames_AllDaemons(t *testing.T) {
	m := installAuditPreMutationTestManifest()
	got := installAuditTaskNames(m, "")
	want := []string{"mcp-local-hub-task10test-alpha", "mcp-local-hub-task10test-beta"}
	if len(got) != len(want) {
		t.Fatalf("got %d names, want %d: %+v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("got[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestInstallAuditTaskNames_DaemonFilter(t *testing.T) {
	m := installAuditPreMutationTestManifest()
	got := installAuditTaskNames(m, "alpha")
	if len(got) != 1 {
		t.Fatalf("got %d names, want 1: %+v", len(got), got)
	}
	if got[0] != "mcp-local-hub-task10test-alpha" {
		t.Errorf("got[0] = %q, want mcp-local-hub-task10test-alpha", got[0])
	}
}

// ---------------------------------------------------------------------------
// Stop end-to-end — verifies the back-compat Stop entry runs the
// fail-closed intent-write before invoking the kill core. Uses
// stopFakeKillCounter to confirm kill-by-port is NOT called when the
// audit fails (plan §51).
// ---------------------------------------------------------------------------

func TestStopWithOpts_NoForce_AuditFails_NoKill(t *testing.T) {
	a := NewAPI()
	daemonIntentTestHelper(t)
	r := &recordingAuditWriter{
		failActions: map[string]error{AuditActionUserStop: ErrIdentityOversize},
	}
	installRecordingAudit(t, r)
	killCounter := stopFakeKillCounter(t)

	// Use the manifest-shipped server "time" so the manifest load
	// succeeds. The fake kill counter increments only if the kill
	// path runs; on fail-closed it must remain zero.
	_, err := a.StopWithOpts(StopOpts{Server: "time", Force: false})
	if err == nil {
		t.Fatal("StopWithOpts: want error on audit ErrIdentityOversize, got nil")
	}
	if !errors.Is(err, ErrIdentityOversize) {
		t.Errorf("error chain: want ErrIdentityOversize, got %v", err)
	}
	if got := atomic.LoadInt32(killCounter); got != 0 {
		t.Errorf("kill path invoked %d times on fail-closed; want 0 (plan §51)", got)
	}
}

func TestStopWithOpts_Force_AuditFails_NoKill(t *testing.T) {
	a := NewAPI()
	daemonIntentTestHelper(t)
	r := &recordingAuditWriter{
		failActions: map[string]error{AuditActionForcedStopWithoutIntent: ErrIdentityOversize},
	}
	installRecordingAudit(t, r)
	killCounter := stopFakeKillCounter(t)

	_, err := a.StopWithOpts(StopOpts{Server: "time", Force: true})
	if err == nil {
		t.Fatal("StopWithOpts force: want error on audit ErrIdentityOversize, got nil")
	}
	if !errors.Is(err, ErrIdentityOversize) {
		t.Errorf("error chain: want ErrIdentityOversize, got %v", err)
	}
	if got := atomic.LoadInt32(killCounter); got != 0 {
		t.Errorf("kill path invoked %d times on force fail-closed; want 0", got)
	}
}

func TestStopWithOpts_NoForce_GenericAuditError_NoKill(t *testing.T) {
	a := NewAPI()
	daemonIntentTestHelper(t)
	stubErr := errors.New("synthetic generic audit failure")
	r := &recordingAuditWriter{
		failActions: map[string]error{AuditActionUserStop: stubErr},
	}
	installRecordingAudit(t, r)
	killCounter := stopFakeKillCounter(t)

	_, err := a.StopWithOpts(StopOpts{Server: "time", Force: false})
	if err == nil {
		t.Fatal("StopWithOpts: want error on generic audit failure, got nil")
	}
	if !errors.Is(err, stubErr) {
		t.Errorf("error chain: want stubErr, got %v", err)
	}
	if got := atomic.LoadInt32(killCounter); got != 0 {
		t.Errorf("kill path invoked %d times on fail-closed; want 0", got)
	}
}

// ---------------------------------------------------------------------------
// Install end-to-end fail-closed — Install must NOT proceed past the
// audit-first step on ErrIdentityOversize. End state: identical to
// never-attempted install (plan §62).
// ---------------------------------------------------------------------------

func TestInstall_AuditFailsErrIdentityOversize_NoSchedulerMutation(t *testing.T) {
	a := NewAPI()
	daemonIntentTestHelper(t)
	preparePreflightBinaryChecks(t)

	r := &recordingAuditWriter{
		failActions: map[string]error{AuditActionServerInstall: ErrIdentityOversize},
	}
	installRecordingAudit(t, r)
	killCounter := stopFakeKillCounter(t)

	// Use a real shipped manifest "time" — Install loads it via embed
	// FS. On audit fail we expect the install to error out BEFORE
	// touching scheduler / clients / intent file.
	err := a.Install(InstallOpts{Server: "time", Writer: &bytes.Buffer{}})
	if err == nil {
		t.Fatal("Install: want error on audit ErrIdentityOversize, got nil")
	}
	if !errors.Is(err, ErrIdentityOversize) {
		t.Errorf("error chain: want ErrIdentityOversize, got %v", err)
	}
	// Intent file should be untouched: no install record AND no
	// pre-existing entries (per the per-test temp dir helper).
	res := a.ReadDaemonIntent()
	if res.State == IntentStateValid && len(res.File.Tasks) > 0 {
		t.Errorf("intent file unexpectedly populated after fail-closed install: %+v",
			res.File.Tasks)
	}
	// kill path must not have been touched (Install never calls it,
	// but assert anyway as a regression guard).
	if got := atomic.LoadInt32(killCounter); got != 0 {
		t.Errorf("kill path invoked %d times during install fail-closed; want 0", got)
	}
}
