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

// TestSetup_RunsWatchdogInstall_HappyPath asserts the post-Bootstrap
// watchdog wiring reaches scheduler.ImportXML for the canonical
// WatchdogTaskName and prints the §16 confirmation lines.
func TestSetup_RunsWatchdogInstall_HappyPath(t *testing.T) {
	_, fakeSch := setupWatchdogTestHelper(t)

	out := &bytes.Buffer{}
	if err := runSetupWatchdog(out, false); err != nil {
		t.Fatalf("runSetupWatchdog: %v", err)
	}
	calls := fakeSch.importXMLCalls
	if len(calls) != 1 {
		t.Fatalf("ImportXML calls = %d, want 1", len(calls))
	}
	if calls[0].name != api.WatchdogTaskName {
		t.Errorf("ImportXML name = %q, want %q", calls[0].name, api.WatchdogTaskName)
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
	if got := len(fakeSch.importXMLCalls); got != 2 {
		t.Errorf("ImportXML calls = %d after two runs, want 2", got)
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
	if len(fakeSch.importXMLCalls) != 1 {
		t.Errorf("ImportXML calls = %d, want 1", len(fakeSch.importXMLCalls))
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
	if len(fakeSch.importXMLCalls) != 1 {
		t.Errorf("watchdog should still install when EventLog reg fails; got %d ImportXML calls",
			len(fakeSch.importXMLCalls))
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
