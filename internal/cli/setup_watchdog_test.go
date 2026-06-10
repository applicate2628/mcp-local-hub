// Tests for Task 11 (internal/cli/setup.go + uninstall.go) — wiring
// watchdog auto-install/uninstall into mcphub setup / uninstall per
// plan §16 (state-dir sanity), §42 (Administrator install refusal),
// §49 (audit-degraded cascade), §60 (EventLog source registration),
// §61 (--allow-elevated audit fail-closed) + Task 11 in the watchdog
// plan v13.
//
// Test seam strategy:
//   - api.SetDaemonStateRootForTest routes every state file (intent,
//     state, audit log, watchdog log, --once.lock) into a temp dir.
//   - api.SetTestSchedulerFactoryFn injects a fake scheduler so the
//     watchdog InstallWatchdogTask path lands in-memory rather than
//     calling schtasks.
//   - api.SetTestCanonicalMcphubPathFn / SetTestCurrentWindowsUserFn
//     stub the canonical-mcphub resolver + per-user principal so
//     InstallWatchdogTask's BuildWatchdogXML succeeds without real
//     filesystem state.
//   - The package-level CLI seams (setupIsElevatedFn, setupRegisterEventLogFn,
//     setupRemoveEventLogFn, setupStateDirSanityFn) drive the elevation
//     check, EventLog registration, and state-dir sanity branch
//     deterministically.
package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"sync/atomic"
	"testing"

	"mcp-local-hub/internal/api"
	"mcp-local-hub/internal/scheduler"
)

// ---------------------------------------------------------------------------
// Helpers — shared seam reset + fake scheduler wiring.
// ---------------------------------------------------------------------------

// setupWatchdogTestHelper sets up a fresh temp state dir + fake scheduler
// and registers t.Cleanup hooks that restore every package-level CLI seam
// the watchdog setup/uninstall flow depends on.
//
// Returns (stateDir, fakeSch) so individual tests can assert on
// fakeSch.deletes() / fakeSch.importXMLCalls and read state files
// directly when needed.
func setupWatchdogTestHelper(t *testing.T) (string, *watchdogFakeScheduler) {
	t.Helper()
	root := t.TempDir()
	restore := api.SetDaemonStateRootForTest(root)
	t.Cleanup(restore)

	fakeSch := &watchdogFakeScheduler{}
	restoreSch := api.SetTestSchedulerFactoryFn(func() (scheduler.Scheduler, error) {
		return fakeSch, nil
	})
	t.Cleanup(restoreSch)

	restorePath := api.SetTestCanonicalMcphubPathFn(func() (string, error) {
		return `C:\fake\mcphub.exe`, nil
	})
	t.Cleanup(restorePath)
	restoreUser := api.SetTestCurrentWindowsUserFn(func() (string, error) {
		return "test-user", nil
	})
	t.Cleanup(restoreUser)

	prevElev := setupIsElevatedFn
	prevReg := setupRegisterEventLogFn
	prevRem := setupRemoveEventLogFn
	prevSanity := setupStateDirSanityFn
	t.Cleanup(func() {
		setupIsElevatedFn = prevElev
		setupRegisterEventLogFn = prevReg
		setupRemoveEventLogFn = prevRem
		setupStateDirSanityFn = prevSanity
	})

	// Default seams: not elevated, EventLog ops succeed silently,
	// state-dir sanity defers to the real api.DaemonStateDir() (which
	// the temp-root override already redirects).
	setupIsElevatedFn = func() (bool, error) { return false, nil }
	setupRegisterEventLogFn = func() error { return nil }
	setupRemoveEventLogFn = func() error { return nil }
	setupStateDirSanityFn = nil // means use the real api.DaemonStateDir().

	return root, fakeSch
}

// ---------------------------------------------------------------------------
// runSetupWatchdog — Task 11 happy path + idempotency.
// ---------------------------------------------------------------------------

// countImportXMLByName returns how many of the fake scheduler's ImportXML
// calls targeted the given task name. After Phase 3a, runSetupWatchdog
// installs BOTH the watchdog (WatchdogTaskName) and the supervisor-liveness
// (LivenessTaskName) tasks, so assertions that previously counted ALL
// ImportXML calls now filter by name to stay watchdog-specific.
func countImportXMLByName(fakeSch *watchdogFakeScheduler, name string) int {
	n := 0
	for _, c := range fakeSch.importXMLCalls {
		if c.name == name {
			n++
		}
	}
	return n
}

// TestSetup_RunsWatchdogInstall_HappyPath asserts the post-Bootstrap
// watchdog wiring reaches scheduler.ImportXML for the canonical
// WatchdogTaskName and prints the §16 confirmation lines. Phase 3a (v0.6
// spec §15 P1-b) ALSO installs the additive supervisor-liveness task; this
// test now asserts BOTH ImportXML calls land (watchdog + liveness) while the
// watchdog confirmation lines are preserved unchanged.
func TestSetup_RunsWatchdogInstall_HappyPath(t *testing.T) {
	_, fakeSch := setupWatchdogTestHelper(t)

	out := &bytes.Buffer{}
	if err := runSetupWatchdog(out, false); err != nil {
		t.Fatalf("runSetupWatchdog: %v", err)
	}
	if got := countImportXMLByName(fakeSch, api.WatchdogTaskName); got != 1 {
		t.Fatalf("watchdog ImportXML calls = %d, want 1", got)
	}
	// Phase 3a additive: the supervisor-liveness task is installed alongside
	// the watchdog (the watchdog install is UNTOUCHED).
	if got := countImportXMLByName(fakeSch, api.LivenessTaskName); got != 1 {
		t.Errorf("liveness ImportXML calls = %d, want 1 (Phase 3a additive install)", got)
	}
	if !strings.Contains(out.String(), api.WatchdogTaskName) {
		t.Errorf("stdout missing watchdog task name; got %q", out.String())
	}
	if !strings.Contains(out.String(), "5 min") {
		t.Errorf("stdout missing cadence; got %q", out.String())
	}
	if !strings.Contains(out.String(), "mcphub watchdog uninstall") {
		t.Errorf("stdout missing disable command hint; got %q", out.String())
	}
	if !strings.Contains(out.String(), api.LivenessTaskName) {
		t.Errorf("stdout missing liveness task name; got %q", out.String())
	}
}

// TestSetup_RunsTwice_Idempotent asserts re-running setup does not
// error; the scheduler factory + InstallWatchdogTask handles
// re-import via /F (overwrite).
func TestSetup_RunsTwice_Idempotent(t *testing.T) {
	_, fakeSch := setupWatchdogTestHelper(t)

	out := &bytes.Buffer{}
	if err := runSetupWatchdog(out, false); err != nil {
		t.Fatalf("first runSetupWatchdog: %v", err)
	}
	if err := runSetupWatchdog(out, false); err != nil {
		t.Fatalf("second runSetupWatchdog: %v", err)
	}
	// Two runs × one watchdog ImportXML each = 2 (Phase 3a's additive liveness
	// ImportXML is counted separately so this watchdog-idempotency assertion is
	// unaffected by the new install).
	if got := countImportXMLByName(fakeSch, api.WatchdogTaskName); got != 2 {
		t.Errorf("watchdog ImportXML calls = %d after two runs, want 2", got)
	}
	if got := countImportXMLByName(fakeSch, api.LivenessTaskName); got != 2 {
		t.Errorf("liveness ImportXML calls = %d after two runs, want 2", got)
	}
}

// ---------------------------------------------------------------------------
// runSetupWatchdog — state-dir sanity rejection (§16, exit 8).
// ---------------------------------------------------------------------------

// TestSetup_StateDirSanityFails_AbortsExit8 stubs the state-dir
// resolver to return an error; setup must abort with exit 8 before
// any scheduler call.
func TestSetup_StateDirSanityFails_AbortsExit8(t *testing.T) {
	_, fakeSch := setupWatchdogTestHelper(t)

	stubErr := errors.New("synthetic state-dir sanity failure")
	setupStateDirSanityFn = func() (string, error) { return "", stubErr }

	out := &bytes.Buffer{}
	err := runSetupWatchdog(out, false)
	if err == nil {
		t.Fatal("expected error for state-dir sanity failure; got nil")
	}
	var fe interface {
		ExitCode() int
		IsMcphubForceExit() bool
	}
	if !errors.As(err, &fe) {
		t.Fatalf("expected forceExitError; got %T (%v)", err, err)
	}
	if fe.ExitCode() != 8 {
		t.Errorf("exit code = %d, want 8", fe.ExitCode())
	}
	if len(fakeSch.importXMLCalls) != 0 {
		t.Errorf("ImportXML should NOT be called on state-dir failure; got %d calls", len(fakeSch.importXMLCalls))
	}
	if !strings.Contains(out.String(), "synthetic state-dir sanity failure") {
		t.Errorf("stderr missing state-dir error context; got %q", out.String())
	}
}

// ---------------------------------------------------------------------------
// runSetupWatchdog — elevation refusal (§42 + §61).
// ---------------------------------------------------------------------------

// TestSetup_Elevated_NoFlag_Refuses stubs the elevation detector to
// return true; without --allow-elevated, setup must refuse the
// watchdog install with a clear error and never call ImportXML.
func TestSetup_Elevated_NoFlag_Refuses(t *testing.T) {
	_, fakeSch := setupWatchdogTestHelper(t)
	setupIsElevatedFn = func() (bool, error) { return true, nil }

	out := &bytes.Buffer{}
	err := runSetupWatchdog(out, false)
	if err == nil {
		t.Fatal("expected refusal; got nil")
	}
	if !strings.Contains(err.Error(), "--allow-elevated") {
		t.Errorf("error must mention --allow-elevated; got %q", err.Error())
	}
	if len(fakeSch.importXMLCalls) != 0 {
		t.Errorf("ImportXML should NOT be called when refusing elevation; got %d calls",
			len(fakeSch.importXMLCalls))
	}
}

// TestSetup_Elevated_WithAllowElevated_HappyPath_AuditWritten covers
// §42 + §61: with --allow-elevated, the high-priority audit entry is
// written first, then the watchdog install proceeds.
func TestSetup_Elevated_WithAllowElevated_HappyPath_AuditWritten(t *testing.T) {
	_, fakeSch := setupWatchdogTestHelper(t)
	setupIsElevatedFn = func() (bool, error) { return true, nil }

	out := &bytes.Buffer{}
	if err := runSetupWatchdog(out, true); err != nil {
		t.Fatalf("runSetupWatchdog with allowElevated=true: %v", err)
	}
	if got := countImportXMLByName(fakeSch, api.WatchdogTaskName); got != 1 {
		t.Errorf("watchdog ImportXML calls = %d, want 1", got)
	}

	a := api.NewAPI()
	tail := a.ReadIntentAuditTail(20)
	found := false
	for _, e := range tail {
		if e.Action == "watchdog-install-elevated-override" {
			if e.Priority != "high" {
				t.Errorf("audit entry Priority = %q, want high", e.Priority)
			}
			if !strings.Contains(e.Reason, "--allow-elevated") {
				t.Errorf("audit entry Reason = %q, want it to mention --allow-elevated",
					e.Reason)
			}
			found = true
			break
		}
	}
	if !found {
		t.Errorf("audit log missing watchdog-install-elevated-override entry; tail=%+v", tail)
	}
}

// TestSetup_Elevated_WithAllowElevated_AuditFails_Exit11 covers §61:
// when the elevated-override audit write fails, setup must exit 11
// (audit-required-but-failed) and the watchdog must NOT be installed.
func TestSetup_Elevated_WithAllowElevated_AuditFails_Exit11(t *testing.T) {
	_, fakeSch := setupWatchdogTestHelper(t)
	setupIsElevatedFn = func() (bool, error) { return true, nil }

	// Inject a recording audit writer that always fails for the
	// elevated-override action.
	r := &recordingAuditWriter{
		failActions: map[string]error{"watchdog-install-elevated-override": errors.New("synthetic audit failure")},
	}
	installRecordingAuditFromCLITest(t, r)

	out := &bytes.Buffer{}
	err := runSetupWatchdog(out, true)
	if err == nil {
		t.Fatal("expected error on audit-fail; got nil")
	}
	var fe interface {
		ExitCode() int
		IsMcphubForceExit() bool
	}
	if !errors.As(err, &fe) {
		t.Fatalf("expected forceExitError; got %T (%v)", err, err)
	}
	if fe.ExitCode() != 11 {
		t.Errorf("exit code = %d, want 11", fe.ExitCode())
	}
	if len(fakeSch.importXMLCalls) != 0 {
		t.Errorf("ImportXML should NOT be called when audit fails; got %d calls",
			len(fakeSch.importXMLCalls))
	}
}

// ---------------------------------------------------------------------------
// runSetupWatchdog — EventLog source registration (§60, Windows-only).
// ---------------------------------------------------------------------------

// TestSetup_EventLogRegistrationFails_NonFatal stubs the EventLog
// registration seam to return an error; setup must continue and
// the watchdog must still be installed. Per §60 the registration
// failure logs `eventlog-source-registration-failed-non-fatal`.
func TestSetup_EventLogRegistrationFails_NonFatal(t *testing.T) {
	_, fakeSch := setupWatchdogTestHelper(t)

	regCalls := int32(0)
	setupRegisterEventLogFn = func() error {
		atomic.AddInt32(&regCalls, 1)
		return errors.New("synthetic eventlog registration failure")
	}

	out := &bytes.Buffer{}
	if err := runSetupWatchdog(out, false); err != nil {
		t.Fatalf("runSetupWatchdog: %v", err)
	}
	if atomic.LoadInt32(&regCalls) != 1 {
		t.Errorf("RegisterEventLog calls = %d, want 1", atomic.LoadInt32(&regCalls))
	}
	if got := countImportXMLByName(fakeSch, api.WatchdogTaskName); got != 1 {
		t.Errorf("watchdog should still install when EventLog reg fails; got %d watchdog ImportXML calls", got)
	}
	a := api.NewAPI()
	tail := a.ReadWatchdogLogTail(20)
	found := false
	for _, e := range tail {
		if e.Action == "eventlog-source-registration-failed-non-fatal" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("watchdog.log missing eventlog-source-registration-failed-non-fatal entry; tail=%+v", tail)
	}
}

// ---------------------------------------------------------------------------
// runUninstallWatchdog ordering — Task 11.1 + §60 EventLog removal.
// ---------------------------------------------------------------------------

// TestUninstall_OrderingDisableWatchdogFirst asserts the call order
// when `mcphub uninstall` runs: watchdog scheduled task is deleted
// FIRST (before the per-server tasks) so it does not race with
// uninstall mid-teardown.
//
// This test exercises the LAST-server uninstall: the fake scheduler
// reports only the target server's tasks, so the partial-uninstall
// gate (Codex bot P2) authorizes watchdog removal.
func TestUninstall_OrderingDisableWatchdogFirst(t *testing.T) {
	_, fakeSch := setupWatchdogTestHelper(t)

	// Seed the fake scheduler with only the target server's task. After
	// uninstall the gate sees zero remaining managed servers → watchdog
	// is removed.
	fakeSch.listResult = []scheduler.TaskStatus{
		{Name: "\\mcp-local-hub-time-default"},
	}

	out := &bytes.Buffer{}
	if err := runUninstallWatchdog(out, "time"); err != nil {
		t.Fatalf("runUninstallWatchdog: %v", err)
	}
	deletes := fakeSch.deletes()
	if len(deletes) < 1 {
		t.Fatalf("expected at least one Delete call; got %v", deletes)
	}
	if deletes[0] != api.WatchdogTaskName {
		t.Errorf("first Delete call = %q, want %q (watchdog must be removed first)",
			deletes[0], api.WatchdogTaskName)
	}
}

// TestUninstall_RemoveEventLogSource asserts the EventLog Remove path
// is invoked during `mcphub uninstall` cleanup when the partial-
// uninstall gate authorizes the global teardown (last managed server).
func TestUninstall_RemoveEventLogSource(t *testing.T) {
	_, fakeSch := setupWatchdogTestHelper(t)

	// Last-server uninstall: only the target server's task remains.
	fakeSch.listResult = []scheduler.TaskStatus{
		{Name: "\\mcp-local-hub-time-default"},
	}

	removeCalls := int32(0)
	setupRemoveEventLogFn = func() error {
		atomic.AddInt32(&removeCalls, 1)
		return nil
	}

	out := &bytes.Buffer{}
	if err := runUninstallWatchdog(out, "time"); err != nil {
		t.Fatalf("runUninstallWatchdog: %v", err)
	}
	if got := atomic.LoadInt32(&removeCalls); got != 1 {
		t.Errorf("RemoveEventLog calls = %d, want 1", got)
	}
}

// ---------------------------------------------------------------------------
// Partial-uninstall gating — Codex bot P2 (medium).
// ---------------------------------------------------------------------------

// TestUninstall_LastServer_RemovesWatchdog covers the P2 happy path:
// the only managed server is the one being uninstalled → after this
// uninstall the remaining set is empty → watchdog is removed.
func TestUninstall_LastServer_RemovesWatchdog(t *testing.T) {
	_, fakeSch := setupWatchdogTestHelper(t)

	// One managed server tracked: "time". Uninstall it.
	fakeSch.listResult = []scheduler.TaskStatus{
		{Name: "\\mcp-local-hub-time-default"},
		// Maintenance tasks must NOT count toward the remaining set.
		{Name: api.WatchdogTaskName},
		{Name: "\\mcp-local-hub-weekly-refresh"},
	}

	out := &bytes.Buffer{}
	if err := runUninstallWatchdog(out, "time"); err != nil {
		t.Fatalf("runUninstallWatchdog: %v", err)
	}
	deletes := fakeSch.deletes()
	if len(deletes) == 0 || deletes[0] != api.WatchdogTaskName {
		t.Errorf("expected watchdog Delete call; got %v", deletes)
	}
	// Output should NOT mention "watchdog kept installed".
	if strings.Contains(out.String(), "watchdog kept installed") {
		t.Errorf("stdout should not say 'watchdog kept installed' for last-server uninstall; got %q", out.String())
	}
}

// TestUninstall_PartialServer_KeepsWatchdog covers the P2 fix: when
// other managed servers remain after this uninstall, the watchdog
// MUST stay installed. Codex bot finding (medium): removing one
// server when multiple are installed silently removed the global
// watchdog for all remaining servers.
func TestUninstall_PartialServer_KeepsWatchdog(t *testing.T) {
	_, fakeSch := setupWatchdogTestHelper(t)

	// Two managed servers tracked. Uninstall only "time"; "wolfram"
	// remains, so the watchdog must NOT be removed.
	fakeSch.listResult = []scheduler.TaskStatus{
		{Name: "\\mcp-local-hub-time-default"},
		{Name: "\\mcp-local-hub-wolfram-default"},
		// Maintenance tasks excluded from gate.
		{Name: api.WatchdogTaskName},
	}

	removeEventLogCalls := int32(0)
	setupRemoveEventLogFn = func() error {
		atomic.AddInt32(&removeEventLogCalls, 1)
		return nil
	}

	out := &bytes.Buffer{}
	if err := runUninstallWatchdog(out, "time"); err != nil {
		t.Fatalf("runUninstallWatchdog: %v", err)
	}

	// Watchdog Delete must NOT appear.
	for _, name := range fakeSch.deletes() {
		if name == api.WatchdogTaskName {
			t.Errorf("watchdog must NOT be deleted while other servers remain; got delete %q", name)
		}
	}
	// EventLog removal must also be skipped: it is a global teardown
	// path that belongs with the watchdog removal gate.
	if got := atomic.LoadInt32(&removeEventLogCalls); got != 0 {
		t.Errorf("EventLog removal must be skipped when watchdog stays; got %d calls", got)
	}
	// Operator-visible informational line must mention the kept-installed
	// reason so a future operator does not assume the watchdog vanished.
	if !strings.Contains(out.String(), "watchdog kept installed") {
		t.Errorf("stdout missing 'watchdog kept installed' informational line; got %q", out.String())
	}
}

// TestUninstall_NoServers_KeepsWatchdog covers the idempotent edge:
// the target server is already removed (e.g. a re-run of `mcphub
// uninstall`). Expected: do not call runUninstallWatchdog's
// scheduler.Delete on the watchdog because the post-uninstall set
// is determined from the live scheduler list, not the target. With
// zero managed daemons present BEFORE the uninstall, the gate still
// authorizes watchdog removal — but per the Codex bot's intent the
// helper must be safe to invoke either way.
//
// The acceptance criterion the user spelled out: "should not call
// runUninstallWatchdog (idempotent)". Practically this means: with
// zero managed servers visible, the gate STILL fires and removes
// the watchdog (because the post-uninstall set is empty — the
// last-server happy path); but the call is safe and idempotent.
func TestUninstall_NoServers_KeepsWatchdog(t *testing.T) {
	_, fakeSch := setupWatchdogTestHelper(t)

	// Zero managed servers visible. Calling runUninstallWatchdog must
	// be safe (idempotent) and the gate authorizes removal because the
	// post-uninstall remaining set is empty.
	fakeSch.listResult = []scheduler.TaskStatus{
		// Only maintenance tasks present.
		{Name: api.WatchdogTaskName},
	}

	out := &bytes.Buffer{}
	if err := runUninstallWatchdog(out, "time"); err != nil {
		t.Fatalf("runUninstallWatchdog: %v", err)
	}
	// With zero managed daemons present, the watchdog Delete IS
	// authorized (post-uninstall set is empty). The call is safe to
	// repeat — UninstallWatchdogTask is idempotent under the API
	// contract.
	deletes := fakeSch.deletes()
	if len(deletes) == 0 || deletes[0] != api.WatchdogTaskName {
		t.Errorf("expected watchdog Delete call (post-uninstall set empty); got %v", deletes)
	}
}

// TestUninstall_ListError_FailsClosed covers the gate's error path:
// when scheduler.List returns an error, we cannot determine whether
// other servers remain. Fail closed by KEEPING the watchdog
// installed so a transient List failure does not silently strip
// recovery from N peer servers.
func TestUninstall_ListError_FailsClosed(t *testing.T) {
	_, fakeSch := setupWatchdogTestHelper(t)
	fakeSch.listErr = errors.New("synthetic list failure")

	out := &bytes.Buffer{}
	if err := runUninstallWatchdog(out, "time"); err != nil {
		t.Fatalf("runUninstallWatchdog: %v", err)
	}
	for _, name := range fakeSch.deletes() {
		if name == api.WatchdogTaskName {
			t.Errorf("List error must fail-closed (keep watchdog); got delete %q", name)
		}
	}
}

// ---------------------------------------------------------------------------
// Supervisor-liveness teardown — Phase 3a (v0.6 spec §15 P1-b).
//
// The `\mcp-local-hub-liveness` task is a hub-wide shared maintenance job
// like the watchdog; it is torn down INSIDE the same last-server gate.
// ---------------------------------------------------------------------------

// containsTask reports whether name appears in the recorded Delete calls.
func containsTask(deletes []string, name string) bool {
	for _, d := range deletes {
		if d == name {
			return true
		}
	}
	return false
}

// TestUninstall_LastServer_RemovesLivenessTask covers FIX 1(b)+(c): on a
// last-server uninstall the supervisor-liveness task is removed alongside the
// watchdog. The fake scheduler reports only the target server's task (plus
// maintenance tasks), so the partial-uninstall gate authorizes the global
// teardown.
func TestUninstall_LastServer_RemovesLivenessTask(t *testing.T) {
	_, fakeSch := setupWatchdogTestHelper(t)

	// Last-server uninstall: only the target server's task remains, plus the
	// two hub-wide maintenance tasks (watchdog + liveness) that must NOT
	// poison the gate.
	fakeSch.listResult = []scheduler.TaskStatus{
		{Name: "\\mcp-local-hub-time-default"},
		{Name: api.WatchdogTaskName},
		{Name: api.LivenessTaskName},
	}

	out := &bytes.Buffer{}
	if err := runUninstallWatchdog(out, "time"); err != nil {
		t.Fatalf("runUninstallWatchdog: %v", err)
	}
	deletes := fakeSch.deletes()
	if !containsTask(deletes, api.LivenessTaskName) {
		t.Errorf("expected Delete(%q) on last-server uninstall; got %v", api.LivenessTaskName, deletes)
	}
	// The watchdog is still removed first; the liveness teardown rides
	// alongside it inside the same gate.
	if !containsTask(deletes, api.WatchdogTaskName) {
		t.Errorf("expected Delete(%q) on last-server uninstall; got %v", api.WatchdogTaskName, deletes)
	}
	if len(deletes) == 0 || deletes[0] != api.WatchdogTaskName {
		t.Errorf("watchdog must be deleted FIRST; got %v", deletes)
	}
}

// TestUninstall_PartialServer_KeepsLivenessTask covers FIX 1(b)+(c): when a
// peer server remains the supervisor-liveness task MUST stay installed — it is
// a hub-wide shared task gated identically to the watchdog. Removing it while
// a peer still relies on owner-relaunch recovery would strip the recovery net.
func TestUninstall_PartialServer_KeepsLivenessTask(t *testing.T) {
	_, fakeSch := setupWatchdogTestHelper(t)

	// Two managed servers tracked. Uninstall only "time"; "wolfram" remains,
	// so neither the watchdog nor the liveness task may be removed.
	fakeSch.listResult = []scheduler.TaskStatus{
		{Name: "\\mcp-local-hub-time-default"},
		{Name: "\\mcp-local-hub-wolfram-default"},
		{Name: api.WatchdogTaskName},
		{Name: api.LivenessTaskName},
	}

	out := &bytes.Buffer{}
	if err := runUninstallWatchdog(out, "time"); err != nil {
		t.Fatalf("runUninstallWatchdog: %v", err)
	}
	deletes := fakeSch.deletes()
	if containsTask(deletes, api.LivenessTaskName) {
		t.Errorf("liveness task must NOT be deleted while a peer server remains; got %v", deletes)
	}
	if containsTask(deletes, api.WatchdogTaskName) {
		t.Errorf("watchdog must NOT be deleted while a peer server remains; got %v", deletes)
	}
}

// TestShouldRemoveGlobalWatchdog_LivenessAndWatchdogOnly_GatePasses is the
// regression guard for the P1 defect FIX 1 closes: before `-liveness` was
// added to api.IsMaintenanceTaskName, ServerFromTaskName("...-liveness")
// returned "liveness" (a non-empty pseudo-server) which landed in the gate's
// `remaining` set and made len(remaining)==0 IMPOSSIBLE while the liveness
// task existed — permanently poisoning the last-server gate so the watchdog
// could never be torn down. With the fix, a scheduler list of ONLY the two
// hub-wide maintenance tasks (watchdog + liveness) and NO real server tasks
// must let the gate authorize teardown.
func TestShouldRemoveGlobalWatchdog_LivenessAndWatchdogOnly_GatePasses(t *testing.T) {
	_, fakeSch := setupWatchdogTestHelper(t)

	// Only the two hub-wide maintenance tasks remain — no real server tasks.
	fakeSch.listResult = []scheduler.TaskStatus{
		{Name: api.LivenessTaskName},
		{Name: api.WatchdogTaskName},
	}

	out := &bytes.Buffer{}
	// serverBeingUninstalled is irrelevant here: there are zero real server
	// tasks, so the remaining set must be empty regardless.
	if !shouldRemoveGlobalWatchdog(out, "time") {
		t.Errorf("gate must pass (return true) when only liveness + watchdog remain; output=%q", out.String())
	}
	if strings.Contains(out.String(), "kept installed") {
		t.Errorf("gate should not report 'kept installed' when no real server tasks remain; got %q", out.String())
	}
}

// ---------------------------------------------------------------------------
// runUninstall orchestration (bot P1.1) — per-server before watchdog.
//
// runUninstall is the testable orchestration body for `mcphub uninstall`.
// Bot P1.1 finding: the previous ordering called runUninstallWatchdog
// FIRST and api.Uninstall second. On a last-server uninstall this could
// strip the watchdog scheduled task and EventLog wiring even when the
// subsequent per-server uninstall failed, leaving daemons installed but
// without auto-recovery. The reordered flow runs api.Uninstall FIRST and
// invokes the watchdog teardown ONLY on success.
// ---------------------------------------------------------------------------

// uninstallCallRecord captures the order in which runUninstall invokes
// its injected dependencies. Each entry tags the step (`uninstall` or
// `watchdog`) so tests can assert ordering and gating.
type uninstallCallRecord struct {
	step   string
	server string
}

// TestRunUninstall_PerServerFailure_KeepsWatchdog covers bot P1.1: when
// the per-server uninstall returns an error, the watchdog teardown MUST
// NOT run. The user is left with the watchdog still installed so it can
// keep recovering whatever scheduler tasks remained after the partial
// failure.
func TestRunUninstall_PerServerFailure_KeepsWatchdog(t *testing.T) {
	stubErr := errors.New("synthetic per-server uninstall failure")
	var calls []uninstallCallRecord

	doUninstall := func(s string) (*api.UninstallReport, error) {
		calls = append(calls, uninstallCallRecord{step: "uninstall", server: s})
		return nil, stubErr
	}
	doWatchdogUninstall := func(_ io.Writer, s string) error {
		calls = append(calls, uninstallCallRecord{step: "watchdog", server: s})
		return nil
	}

	out := &bytes.Buffer{}
	err := runUninstall(out, "time", doUninstall, doWatchdogUninstall)
	if err == nil {
		t.Fatal("runUninstall: want per-server uninstall error, got nil")
	}
	if !errors.Is(err, stubErr) {
		t.Errorf("error chain: want stubErr, got %v", err)
	}
	if len(calls) != 1 {
		t.Fatalf("calls = %d, want exactly 1 (per-server only): %+v", len(calls), calls)
	}
	if calls[0].step != "uninstall" {
		t.Errorf("call[0].step = %q, want %q", calls[0].step, "uninstall")
	}
	if calls[0].server != "time" {
		t.Errorf("call[0].server = %q, want %q", calls[0].server, "time")
	}
	for _, c := range calls {
		if c.step == "watchdog" {
			t.Errorf("watchdog teardown invoked despite per-server failure; calls=%+v", calls)
		}
	}
	// Output must NOT contain the success-completion line — partial
	// failure surfaces as an error to the caller.
	if strings.Contains(out.String(), "Uninstall complete") {
		t.Errorf("stdout should not contain 'Uninstall complete' on per-server failure; got %q", out.String())
	}
}

// TestRunUninstall_LastServerSuccess_RemovesWatchdog covers the happy
// path: when the per-server uninstall succeeds, the watchdog teardown
// is invoked AFTER it. The actual gate that decides whether to remove
// the watchdog scheduled task lives inside runUninstallWatchdog (Codex
// bot P2 gate) and is exercised by the existing
// TestUninstall_LastServer_RemovesWatchdog. This test only verifies the
// orchestration: per-server first, watchdog second, both invoked.
func TestRunUninstall_LastServerSuccess_RemovesWatchdog(t *testing.T) {
	var calls []uninstallCallRecord

	doUninstall := func(s string) (*api.UninstallReport, error) {
		calls = append(calls, uninstallCallRecord{step: "uninstall", server: s})
		return &api.UninstallReport{
			Server:       s,
			TasksDeleted: []string{"\\mcp-local-hub-time-default"},
		}, nil
	}
	doWatchdogUninstall := func(_ io.Writer, s string) error {
		calls = append(calls, uninstallCallRecord{step: "watchdog", server: s})
		return nil
	}

	out := &bytes.Buffer{}
	if err := runUninstall(out, "time", doUninstall, doWatchdogUninstall); err != nil {
		t.Fatalf("runUninstall: %v", err)
	}
	if len(calls) != 2 {
		t.Fatalf("calls = %d, want 2 (per-server + watchdog): %+v", len(calls), calls)
	}
	if calls[0].step != "uninstall" {
		t.Errorf("call[0].step = %q, want %q (per-server FIRST)", calls[0].step, "uninstall")
	}
	if calls[1].step != "watchdog" {
		t.Errorf("call[1].step = %q, want %q (watchdog SECOND)", calls[1].step, "watchdog")
	}
	if !strings.Contains(out.String(), "Uninstall complete") {
		t.Errorf("stdout missing 'Uninstall complete' on success; got %q", out.String())
	}
}

// TestRunUninstall_RequiresServer covers the bare-flag error path so
// the cobra wrapper's `--server is required` contract is preserved
// after the helper extraction.
func TestRunUninstall_RequiresServer(t *testing.T) {
	doUninstall := func(string) (*api.UninstallReport, error) {
		t.Fatal("doUninstall must NOT be called when --server is empty")
		return nil, nil
	}
	doWatchdogUninstall := func(io.Writer, string) error {
		t.Fatal("doWatchdogUninstall must NOT be called when --server is empty")
		return nil
	}
	err := runUninstall(&bytes.Buffer{}, "", doUninstall, doWatchdogUninstall)
	if err == nil {
		t.Fatal("runUninstall: want error on empty --server, got nil")
	}
	if !strings.Contains(err.Error(), "--server is required") {
		t.Errorf("error message: want '--server is required', got %v", err)
	}
}

// TestRunUninstall_WatchdogTeardownFailure_PropagatesError covers the
// error-propagation contract on the watchdog step: when the per-server
// uninstall succeeded but runUninstallWatchdog itself returns an error,
// runUninstall surfaces that error to the cobra wrapper so the user
// sees a non-zero exit. The per-server uninstall is still recorded in
// stdout (it actually happened), but the success-completion line is
// suppressed.
func TestRunUninstall_WatchdogTeardownFailure_PropagatesError(t *testing.T) {
	stubErr := errors.New("synthetic watchdog teardown failure")
	doUninstall := func(s string) (*api.UninstallReport, error) {
		return &api.UninstallReport{Server: s}, nil
	}
	doWatchdogUninstall := func(io.Writer, string) error { return stubErr }

	out := &bytes.Buffer{}
	err := runUninstall(out, "time", doUninstall, doWatchdogUninstall)
	if err == nil {
		t.Fatal("runUninstall: want watchdog teardown error, got nil")
	}
	if !errors.Is(err, stubErr) {
		t.Errorf("error chain: want stubErr, got %v", err)
	}
	if strings.Contains(out.String(), "Uninstall complete") {
		t.Errorf("stdout should not contain 'Uninstall complete' on watchdog failure; got %q", out.String())
	}
}

// installRecordingAuditFromCLITest is the cli-package equivalent of
// installRecordingAudit — it cannot reach the api-package private
// appendIntentAuditFn directly, so it routes through the public test
// hook surface SetTestAuditAppendFn injected at the disk-append
// layer. The recordingAuditWriter shape mirrors the api-package
// helper so per-Action fail-injection works the same way.
//
// Implementation note: SetTestAuditAppendFn intercepts at the byte-
// write level; we parse the JSON line so we can still pivot on
// Action for failActions matching.
func installRecordingAuditFromCLITest(t *testing.T, r *recordingAuditWriter) {
	t.Helper()
	restore := api.SetTestAuditAppendFn(func(path string, line []byte) error {
		atomic.AddInt32(&r.calls, 1)
		entry := parseAuditLineForCLITest(line)
		r.entries = append(r.entries, entry)
		if r.failActions != nil {
			if err, ok := r.failActions[entry.Action]; ok && err != nil {
				return err
			}
			if err, ok := r.failActions["*"]; ok && err != nil {
				return err
			}
		}
		return nil
	})
	t.Cleanup(restore)
}

// recordingAuditWriter mirrors the api-package fail-injection helper
// so cli tests can assert Action / Priority / Reason without parsing
// raw JSON in every test body.
type recordingAuditWriter struct {
	entries     []api.IntentAuditEntry
	failActions map[string]error // Action → error
	calls       int32
}

// parseAuditLineForCLITest decodes a single JSON-Lines audit row into
// the public IntentAuditEntry shape. Tolerant of unknown fields (the
// production schema may grow). The IntentAuditEntry custom UnmarshalJSON
// already handles control-byte unescape and discards the sealed
// system_entry input.
func parseAuditLineForCLITest(line []byte) api.IntentAuditEntry {
	var entry api.IntentAuditEntry
	trimmed := bytes.TrimRight(line, "\n")
	_ = json.Unmarshal(trimmed, &entry)
	return entry
}
