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
	"time"

	"github.com/gofrs/flock"

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

// stopFakeKillCounter installs a no-op replacement for the kill-by-port
// path so the kill operation becomes a counted no-op. Returns a pointer
// to the counter so tests can assert on it. Restoration is automatic.
//
// Counts killByPortFn invocations specifically — NOT lookupProcess, which
// is shared between the kill path and other read-only ownership checks
// (portHeldByOurDaemon, etc.). Counting lookupProcess would conflate the
// two and inflate the counter on hosts where portInUse trips during
// Preflight's own-port detection (bug-bash A6 #6 r2 P1 closure).
func stopFakeKillCounter(t *testing.T) *int32 {
	t.Helper()
	var counter int32
	orig := killByPortFn
	killByPortFn = func(port int, timeout time.Duration) error {
		atomic.AddInt32(&counter, 1)
		return nil // no-op
	}
	t.Cleanup(func() { killByPortFn = orig })
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
		"set-intent":        false,
		AuditActionUserStop: false,
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
	// Phase 4-E2: the stop now lands in supervisor-intent.json's `stops`
	// sub-block, NOT daemon-intent.json.
	got, ok := readSubBlockStopForTest(t, dir)[canonicalTask]
	if !ok {
		t.Fatalf("stops sub-block missing entry for %s", canonicalTask)
	}
	if got.Desired != IntentDesiredStopped {
		t.Errorf("Desired = %q, want %q", got.Desired, IntentDesiredStopped)
	}
	if got.Reason != IntentReasonUserStop {
		t.Errorf("Reason = %q, want %q", got.Reason, IntentReasonUserStop)
	}
	// daemon-intent.json must NOT be written by the E2 stop path.
	if a.ReadDaemonIntent().State == IntentStateValid {
		t.Errorf("daemon-intent.json should not be written by the E2 stop path")
	}
}

// readSubBlockStopForTest reads the supervisor-intent.json `stops` sub-block
// under the test state dir (Phase 4-E2: the sole stop source). Returns an
// empty map when the file does not exist (a no-op stop never created it).
func readSubBlockStopForTest(t *testing.T, stateDir string) map[string]DaemonIntent {
	t.Helper()
	got, err := ReadSupervisorIntent(filepath.Join(stateDir, supervisorIntentFileLeaf))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return map[string]DaemonIntent{}
		}
		t.Fatalf("read supervisor-intent.json stops: %v", err)
	}
	if got.Stops == nil {
		return map[string]DaemonIntent{}
	}
	return got.Stops
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

// Phase 4-E2: recordInstallIntentPostSuccess writes Desired=running, which on
// the stops sub-block CLEARS any prior stop (re-enable) and is a NO-OP when no
// prior stop exists (a never-stopped daemon needs no running record). The E1
// behavior — an explicit Desired=running entry in daemon-intent.json — is gone.
func TestRecordInstallIntentPostSuccess_E2_ClearsPriorStop(t *testing.T) {
	a := NewAPI()
	dir := daemonIntentTestHelper(t)
	r := &recordingAuditWriter{}
	installRecordingAudit(t, r)
	m := installAuditPreMutationTestManifest()

	// Seed a prior stop for one of the install tasks in the sub-block.
	alpha := "\\mcp-local-hub-task10test-alpha"
	if err := a.WriteStopIntent(alpha, DaemonIntent{
		Desired: IntentDesiredStopped, Reason: IntentReasonUserStop, UpdatedAt: time.Now().UTC(),
	}, "tester"); err != nil {
		t.Fatalf("seed prior stop: %v", err)
	}
	if _, ok := readSubBlockStopForTest(t, dir)[alpha]; !ok {
		t.Fatalf("precondition: prior stop should be set")
	}

	var buf bytes.Buffer
	a.recordInstallIntentPostSuccess(m, "", &buf)

	// The install (Desired=running) cleared the prior stop → re-enabled.
	if _, ok := readSubBlockStopForTest(t, dir)[alpha]; ok {
		t.Errorf("install Desired=running should have cleared the prior stop")
	}
	// daemon-intent.json must not be written by the E2 path.
	if a.ReadDaemonIntent().State == IntentStateValid {
		t.Errorf("daemon-intent.json should not be written by the E2 install path")
	}
	if buf.Len() != 0 {
		t.Errorf("expected no warnings on happy path, got: %s", buf.String())
	}
}

func TestRecordInstallIntentPostSuccess_AuditFails_LoggedNotPropagated(t *testing.T) {
	a := NewAPI()
	dir := daemonIntentTestHelper(t)
	// The clear-intent audit (emitted when re-enabling a prior stop) fails.
	// WriteStopIntent still commits the sub-block write before dispatching the
	// audit, so the stop is cleared; the function MUST NOT propagate the error.
	r := &recordingAuditWriter{
		failActions: map[string]error{"clear-intent": errors.New("synthetic disk-full")},
	}
	installRecordingAudit(t, r)
	m := installAuditPreMutationTestManifest()

	alpha := "\\mcp-local-hub-task10test-alpha"
	if err := a.WriteStopIntent(alpha, DaemonIntent{
		Desired: IntentDesiredStopped, Reason: IntentReasonUserStop, UpdatedAt: time.Now().UTC(),
	}, "tester"); err != nil {
		t.Fatalf("seed prior stop: %v", err)
	}

	var buf bytes.Buffer
	a.recordInstallIntentPostSuccess(m, "", &buf)

	// Even though the clear-intent audit failed, the sub-block clear committed.
	if _, ok := readSubBlockStopForTest(t, dir)[alpha]; ok {
		t.Errorf("E2: prior stop should be cleared despite audit failure (fail-open)")
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
	root := hardenedTempDir(t)
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
// writeRegisterRunningIntentForTask — best-effort behavior on audit failure.
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

// Phase 4-E2: recordRestartIntentForTask writes Desired=running, which CLEARS
// any prior stop from the sub-block (re-enable) and is a no-op on a clean
// state. The server-restarted audit still fires.
func TestRecordRestartIntentForTask_E2_ClearsPriorStop(t *testing.T) {
	a := NewAPI()
	dir := daemonIntentTestHelper(t)
	r := &recordingAuditWriter{}
	installRecordingAudit(t, r)
	var buf bytes.Buffer

	task := "\\mcp-local-hub-time-default"
	// Seed a prior stop, then restart re-enables (clears) it.
	if err := a.WriteStopIntent(task, DaemonIntent{
		Desired: IntentDesiredStopped, Reason: IntentReasonUserStop, UpdatedAt: time.Now().UTC(),
	}, "tester"); err != nil {
		t.Fatalf("seed prior stop: %v", err)
	}
	a.recordRestartIntentForTask(task, &buf)

	if _, ok := readSubBlockStopForTest(t, dir)[task]; ok {
		t.Errorf("restart Desired=running should have cleared the prior stop")
	}
	if a.ReadDaemonIntent().State == IntentStateValid {
		t.Errorf("daemon-intent.json should not be written by the E2 restart path")
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
	dir := daemonIntentTestHelper(t)
	r := &recordingAuditWriter{}
	installRecordingAudit(t, r)
	var buf bytes.Buffer

	tasks := []string{"mcp-local-hub-task10test-alpha", "mcp-local-hub-task10test-beta"}
	a.recordUninstallIntentForTasks(tasks, &buf)

	// Phase 4-E2: the uninstall tombstone (Desired=stopped, uninstalled) lands
	// in the supervisor-intent.json stops sub-block.
	stops := readSubBlockStopForTest(t, dir)
	// Codex deep-sec PR #135 Finding 1: storage uses canonical leading-
	// backslash form regardless of caller-supplied shape.
	canonicalTasks := []string{"\\" + tasks[0], "\\" + tasks[1]}
	for _, name := range canonicalTasks {
		got, ok := stops[name]
		if !ok {
			t.Errorf("stops sub-block missing entry for %s; stops=%+v", name, stops)
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

// Phase 4-E2: the transactional Register intent writer clears the target
// task's prior stop, and the separate workspace-registered observation fires
// only after the transaction commits.
func TestRegisterIntentTransaction_E2_ClearsPriorStopAndAuditsAfterCommit(t *testing.T) {
	a := NewAPI()
	dir := daemonIntentTestHelper(t)
	r := &recordingAuditWriter{}
	installRecordingAudit(t, r)

	taskName := "mcp-local-hub-lsp-deadbeef-python"
	canonicalTask := "\\" + taskName
	// Seed a prior stop, then register re-enables (clears) it.
	if err := a.WriteStopIntent(canonicalTask, DaemonIntent{
		Desired: IntentDesiredStopped, Reason: IntentReasonUserStop, UpdatedAt: time.Now().UTC(),
	}, "tester"); err != nil {
		t.Fatalf("seed prior stop: %v", err)
	}
	transaction := newRegistrationTransaction()
	if _, err := a.writeRegisterRunningIntentForTask(taskName, transaction.AddCompensation); err != nil {
		t.Fatalf("write running intent: %v", err)
	}
	enrollRegisterWorkspaceAudit(transaction, taskName)
	for _, entry := range r.entries {
		if entry.Action == AuditActionWorkspaceRegistered {
			t.Fatalf("workspace-registered audit fired before commit: %+v", r.entries)
		}
	}
	outcome := transaction.Commit()
	if !outcome.Committed() || outcome.ObserverErr != nil {
		t.Fatalf("commit outcome = %+v", outcome)
	}

	if _, ok := readSubBlockStopForTest(t, dir)[canonicalTask]; ok {
		t.Errorf("register Desired=running should have cleared the prior stop")
	}
	if a.ReadDaemonIntent().State == IntentStateValid {
		t.Errorf("daemon-intent.json should not be written by the E2 register path")
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

func TestRegisterWorkspaceAuditFailure_IsPostCommitObserverError(t *testing.T) {
	daemonIntentTestHelper(t)
	r := &recordingAuditWriter{
		failActions: map[string]error{"*": errors.New("synthetic")},
	}
	installRecordingAudit(t, r)

	transaction := newRegistrationTransaction()
	enrollRegisterWorkspaceAudit(transaction, "mcp-local-hub-lsp-deadbeef-python")
	outcome := transaction.Commit()
	if !outcome.Committed() {
		t.Fatalf("observer failure changed committed state: %+v", outcome)
	}
	if outcome.ObserverErr == nil || !strings.Contains(outcome.ObserverErr.Error(), "synthetic") {
		t.Fatalf("observer error = %v, want synthetic audit failure", outcome.ObserverErr)
	}
}

func TestRegisterWorkspaceRegisteredAudit_OnlyAfterOuterCommit(t *testing.T) {
	for _, failurePoint := range []string{
		"same-language",
		"later-language",
		"cleanup",
		"finalizer",
	} {
		t.Run(failurePoint, func(t *testing.T) {
			daemonIntentTestHelper(t)
			recorder := &recordingAuditWriter{}
			installRecordingAudit(t, recorder)
			tx := newRegistrationTransaction()
			enrollRegisterWorkspaceAudit(tx, "mcp-local-hub-lsp-deadbeef-python")
			outcome := tx.Fail(errors.New("injected " + failurePoint + " failure"))
			if outcome.State != registrationTransactionRolledBack {
				t.Fatalf("outcome = %+v, want rolled-back", outcome)
			}
			for _, entry := range recorder.entries {
				if entry.Action == AuditActionWorkspaceRegistered {
					t.Fatalf("workspace-registered fired before outer commit at %s: %+v", failurePoint, recorder.entries)
				}
			}
		})
	}

	t.Run("commit", func(t *testing.T) {
		daemonIntentTestHelper(t)
		recorder := &recordingAuditWriter{}
		installRecordingAudit(t, recorder)
		tx := newRegistrationTransaction()
		enrollRegisterWorkspaceAudit(tx, "mcp-local-hub-lsp-deadbeef-python")
		outcome := tx.Commit()
		if !outcome.Committed() || outcome.ObserverErr != nil {
			t.Fatalf("commit outcome = %+v", outcome)
		}
		count := 0
		for _, entry := range recorder.entries {
			if entry.Action == AuditActionWorkspaceRegistered {
				count++
			}
		}
		if count != 1 {
			t.Fatalf("workspace-registered count = %d, want 1; entries=%+v", count, recorder.entries)
		}
	})
}

func TestRegisterRunningIntentRollbackMatrix(t *testing.T) {
	for _, priorPresent := range []bool{false, true} {
		name := "prior-absent"
		if priorPresent {
			name = "prior-present"
		}
		t.Run(name, func(t *testing.T) {
			a := NewAPI()
			dir := daemonIntentTestHelper(t)
			target := "\\mcp-local-hub-lsp-deadbeef-python"
			sibling := "\\mcp-local-hub-lsp-feedface-go"
			now := time.Now().UTC().Truncate(time.Second)
			siblingStop := DaemonIntent{
				Desired:   IntentDesiredStopped,
				Reason:    IntentReasonUserStop,
				UpdatedAt: now,
			}
			if err := a.WriteStopIntent(sibling, siblingStop, "tester"); err != nil {
				t.Fatalf("seed sibling stop: %v", err)
			}
			var priorStop DaemonIntent
			if priorPresent {
				priorStop = DaemonIntent{
					Desired:   IntentDesiredStopped,
					Reason:    IntentReasonInstall,
					UpdatedAt: now.Add(time.Second),
				}
				if err := a.WriteStopIntent(target, priorStop, "tester"); err != nil {
					t.Fatalf("seed target stop: %v", err)
				}
			}

			tx := newRegistrationTransaction()
			if _, err := a.writeRegisterRunningIntentForTask(target, tx.AddCompensation); err != nil {
				t.Fatalf("write running intent: %v", err)
			}
			if _, present := readSubBlockStopForTest(t, dir)[target]; present {
				t.Fatal("running intent did not clear target stop")
			}
			outcome := tx.Fail(errors.New("injected post-write failure"))
			if outcome.State != registrationTransactionRolledBack {
				t.Fatalf("outcome = %+v, want rolled-back", outcome)
			}
			stops := readSubBlockStopForTest(t, dir)
			gotSibling, siblingPresent := stops[sibling]
			if !siblingPresent ||
				gotSibling.Desired != siblingStop.Desired ||
				gotSibling.Reason != siblingStop.Reason ||
				!gotSibling.UpdatedAt.Equal(siblingStop.UpdatedAt) {
				t.Fatalf("sibling stop changed: got=%+v present=%v want=%+v all=%+v", gotSibling, siblingPresent, siblingStop, stops)
			}
			gotTarget, targetPresent := stops[target]
			if targetPresent != priorPresent {
				t.Fatalf("target presence = %v, want %v; stop=%+v", targetPresent, priorPresent, gotTarget)
			}
			if priorPresent &&
				(gotTarget.Desired != priorStop.Desired ||
					gotTarget.Reason != priorStop.Reason ||
					!gotTarget.UpdatedAt.Equal(priorStop.UpdatedAt)) {
				t.Fatalf("target stop = %+v, want %+v", gotTarget, priorStop)
			}
		})
	}
}

func TestRegisterRunningIntentReleaseUnconfirmedCommitsForwardWithoutRestore(t *testing.T) {
	a := NewAPI()
	dir := daemonIntentTestHelper(t)
	task := `\mcp-local-hub-lsp-release-unconfirmed-go`
	prior := DaemonIntent{Desired: IntentDesiredStopped, Reason: IntentReasonUserStop, UpdatedAt: time.Now().UTC()}
	if err := a.WriteStopIntent(task, prior, "tester"); err != nil {
		t.Fatalf("seed stop: %v", err)
	}
	path, err := DefaultSupervisorIntentPath()
	if err != nil {
		t.Fatalf("supervisor intent path: %v", err)
	}
	lockPath := path + supervisorIntentLockSuffix
	releaseCause := errors.New("injected stop-intent unlock failure")
	previousUnlock := flockUnlockFn
	var stranded *flock.Flock
	flockUnlockFn = func(fl *flock.Flock) error {
		if fl.Path() == lockPath {
			stranded = fl
			return releaseCause
		}
		return previousUnlock(fl)
	}
	t.Cleanup(func() {
		flockUnlockFn = previousUnlock
		if stranded != nil {
			_ = stranded.Unlock()
		}
		unconfirmedLockReleasesMu.Lock()
		delete(unconfirmedLockReleases, lockPath)
		unconfirmedLockReleasesMu.Unlock()
	})

	tx := newRegistrationTransaction()
	if _, err := a.writeRegisterRunningIntentForTask(task, tx.AddCompensation); !isAppliedLockReleaseUnconfirmed(err) || !errors.Is(err, releaseCause) {
		t.Fatalf("running intent error = %v, want durable applied release-unconfirmed", err)
	}
	if _, present := readSubBlockStopForTest(t, dir)[task]; present {
		t.Fatal("durably cleared stop was restored before forward settlement")
	}
	if outcome := tx.CommitForward(); outcome.State != registrationTransactionCommitted {
		t.Fatalf("forward settlement outcome = %+v, want committed", outcome)
	}
	if _, present := readSubBlockStopForTest(t, dir)[task]; present {
		t.Fatal("forward settlement invoked the pre-armed stop restore")
	}
}

func TestRegisterRunningIntentRollbackPreservesNewerOperatorStop(t *testing.T) {
	a := NewAPI()
	dir := daemonIntentTestHelper(t)
	target := "\\mcp-local-hub-lsp-deadbeef-python"
	now := time.Now().UTC().Truncate(time.Second)
	prior := DaemonIntent{
		Desired:   IntentDesiredStopped,
		Reason:    IntentReasonInstall,
		UpdatedAt: now,
	}
	if err := a.WriteStopIntent(target, prior, "tester"); err != nil {
		t.Fatalf("seed prior stop: %v", err)
	}

	tx := newRegistrationTransaction()
	if _, err := a.writeRegisterRunningIntentForTask(target, tx.AddCompensation); err != nil {
		t.Fatalf("write running intent: %v", err)
	}
	newer := DaemonIntent{
		Desired:   IntentDesiredStopped,
		Reason:    IntentReasonUserDisabled,
		UpdatedAt: prior.UpdatedAt.Add(time.Minute),
	}
	if err := a.WriteStopIntent(target, newer, "operator"); err != nil {
		t.Fatalf("write newer operator stop: %v", err)
	}

	outcome := tx.Fail(errors.New("injected registration failure"))
	if !errors.Is(outcome.Err, ErrStopIntentRollbackConflict) {
		t.Fatalf("rollback error = %v, want compare-and-swap conflict", outcome.Err)
	}
	stops := readSubBlockStopForTest(t, dir)
	got, present := stops[target]
	if !present || got.Desired != newer.Desired || got.Reason != newer.Reason || !got.UpdatedAt.Equal(newer.UpdatedAt) {
		t.Fatalf("rollback overwrote newer operator stop: got=%+v present=%t want=%+v", got, present, newer)
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
	origPreflightPortInUse := preflightPortInUse
	preflightPortInUse = func(int) bool { return false }
	t.Cleanup(func() { preflightPortInUse = origPreflightPortInUse })

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
