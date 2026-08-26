// Tests for the maintenance-task wiring in `mcphub setup` / `mcphub
// uninstall` (internal/cli/setup.go + uninstall.go) per plan §16
// (state-dir sanity), §42 (Administrator install refusal), §60 (EventLog
// source registration), §61 (--allow-elevated audit fail-closed).
//
// v0.6 Phase C/D (spec §5): the legacy `\mcp-local-hub-watchdog` task is
// no longer INSTALLED here — the supervisor owns daemon revival and the
// supervisor-liveness task owns owner-death recovery. These tests INVERT
// the prior watchdog-install assertions: setup must NOT ImportXML the
// watchdog, MUST ImportXML the liveness task, and MUST best-effort delete
// any leftover legacy watchdog task on existing hosts.
//
// Test seam strategy:
//   - api.SetDaemonStateRootForTest routes every state file (intent,
//     state, audit log, --once.lock) into a temp dir.
//   - api.SetTestSchedulerFactoryFn injects a recording fake scheduler so
//     the liveness install (ImportXML) + legacy-watchdog removal (Delete)
//     land in-memory rather than calling schtasks.
//   - api.SetTestCanonicalMcphubPathFn / SetTestCurrentWindowsUserFn stub
//     the canonical-mcphub resolver + per-user principal so the liveness
//     install's BuildLivenessXML succeeds without real filesystem state.
//   - The package-level CLI seams (setupIsElevatedFn, setupRegisterEventLogFn,
//     setupRemoveEventLogFn, setupStateDirSanityFn) drive the elevation
//     check, EventLog registration, and state-dir sanity branch
//     deterministically.
package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"mcp-local-hub/internal/api"
	"mcp-local-hub/internal/scheduler"
)

// ---------------------------------------------------------------------------
// Recording fake scheduler (self-contained; the prior watchdogFakeScheduler
// lived in the deleted internal/cli/watchdog_test.go).
// ---------------------------------------------------------------------------

var errNotImplementedForSetupTest = errors.New("setupFakeScheduler: not implemented")

type setupImportXMLCall struct {
	name string
	xml  []byte
}

type setupFakeScheduler struct {
	mu             sync.Mutex
	tasks          map[string][]byte
	deleteCalls    []string
	importXMLCalls []setupImportXMLCall
	order          []string
	importXMLErr   error
	listResult     []scheduler.TaskStatus
	listErr        error
}

func (f *setupFakeScheduler) Create(scheduler.TaskSpec) error { return errNotImplementedForSetupTest }
func (f *setupFakeScheduler) Delete(name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.tasks, name)
	f.deleteCalls = append(f.deleteCalls, name)
	f.order = append(f.order, "delete:"+name)
	return nil
}
func (f *setupFakeScheduler) Run(string) error  { return errNotImplementedForSetupTest }
func (f *setupFakeScheduler) Stop(string) error { return errNotImplementedForSetupTest }
func (f *setupFakeScheduler) Status(string) (scheduler.TaskStatus, error) {
	return scheduler.TaskStatus{}, errNotImplementedForSetupTest
}
func (f *setupFakeScheduler) List(prefix string) ([]scheduler.TaskStatus, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.listErr != nil {
		return nil, f.listErr
	}
	out := make([]scheduler.TaskStatus, len(f.listResult))
	copy(out, f.listResult)
	return out, nil
}
func (f *setupFakeScheduler) ExportXML(name string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	xml, ok := f.tasks[name]
	if !ok {
		return nil, scheduler.ErrTaskNotFound
	}
	return append([]byte(nil), xml...), nil
}
func (f *setupFakeScheduler) ImportXML(name string, xml []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	cp := make([]byte, len(xml))
	copy(cp, xml)
	f.importXMLCalls = append(f.importXMLCalls, setupImportXMLCall{name: name, xml: cp})
	f.order = append(f.order, "import:"+name)
	if f.importXMLErr != nil {
		return f.importXMLErr
	}
	f.tasks[name] = cp
	return nil
}
func (f *setupFakeScheduler) deletes() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, len(f.deleteCalls))
	copy(out, f.deleteCalls)
	return out
}

func (f *setupFakeScheduler) operationOrder() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, len(f.order))
	copy(out, f.order)
	return out
}

// importXMLByName returns how many ImportXML calls targeted the given name.
func (f *setupFakeScheduler) importXMLByName(name string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.importXMLCalls {
		if c.name == name {
			n++
		}
	}
	return n
}

// containsTask reports whether name appears in the recorded Delete calls.
func containsTask(deletes []string, name string) bool {
	for _, d := range deletes {
		if d == name {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// Helpers — shared seam reset + fake scheduler wiring.
// ---------------------------------------------------------------------------

// setupWatchdogTestHelper sets up a fresh temp state dir + fake scheduler
// and registers t.Cleanup hooks that restore every package-level CLI seam
// the maintenance-task setup/uninstall flow depends on.
func setupWatchdogTestHelper(t *testing.T) (string, *setupFakeScheduler) {
	t.Helper()
	root := t.TempDir()
	restore := api.SetDaemonStateRootForTest(root)
	t.Cleanup(restore)

	fakeSch := &setupFakeScheduler{tasks: make(map[string][]byte)}
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

	// Default seams: not elevated, EventLog ops succeed silently, state-dir
	// sanity defers to the real api.DaemonStateDir() (the temp-root override
	// redirects it).
	setupIsElevatedFn = func() (bool, error) { return false, nil }
	setupRegisterEventLogFn = func() error { return nil }
	setupRemoveEventLogFn = func() error { return nil }
	setupStateDirSanityFn = nil

	return root, fakeSch
}

// ---------------------------------------------------------------------------
// runSetupWatchdog — Phase C: NO watchdog install, liveness install + legacy
// watchdog cleanup.
// ---------------------------------------------------------------------------

// TestSetup_DoesNotInstallWatchdog_InstallsLiveness asserts the C inversion:
// the watchdog is NEVER ImportXML'd, the liveness task IS, and a best-effort
// Delete of the legacy watchdog task fires.
func TestSetup_DoesNotInstallWatchdog_InstallsLiveness(t *testing.T) {
	_, fakeSch := setupWatchdogTestHelper(t)

	out := &bytes.Buffer{}
	if err := runSetupWatchdog(out, false); err != nil {
		t.Fatalf("runSetupWatchdog: %v", err)
	}
	// The legacy watchdog must NEVER be installed.
	if got := fakeSch.importXMLByName(api.LegacyWatchdogTaskName); got != 0 {
		t.Errorf("watchdog ImportXML calls = %d, want 0 (Phase C dropped the install)", got)
	}
	// The liveness task IS installed (the sole maintenance-task install).
	if got := fakeSch.importXMLByName(api.LivenessTaskName); got != 1 {
		t.Errorf("liveness ImportXML calls = %d, want 1", got)
	}
	// A best-effort legacy-watchdog removal fires for existing-host cleanup.
	if !containsTask(fakeSch.deletes(), api.LegacyWatchdogTaskName) {
		t.Errorf("expected Delete(%q) for existing-host cleanup; got %v", api.LegacyWatchdogTaskName, fakeSch.deletes())
	}
	if !strings.Contains(out.String(), api.LivenessTaskName) {
		t.Errorf("stdout missing liveness task name; got %q", out.String())
	}
}

func TestSetup_LivenessInstallFailureFailsClosedAndKeepsLegacyWatchdog(t *testing.T) {
	_, fakeSch := setupWatchdogTestHelper(t)
	fakeSch.importXMLErr = errors.New("schtasks /Create /XML rejected liveness")

	out := &bytes.Buffer{}
	err := runSetupWatchdog(out, false)
	if err == nil {
		t.Fatal("runSetupWatchdog: want liveness install failure, got nil")
	}
	var fe interface {
		ExitCode() int
		IsMcphubForceExit() bool
	}
	if !errors.As(err, &fe) {
		t.Fatalf("expected forceExitError; got %T (%v)", err, err)
	}
	if fe.ExitCode() != 12 {
		t.Errorf("exit code = %d, want 12", fe.ExitCode())
	}
	if containsTask(fakeSch.deletes(), api.LegacyWatchdogTaskName) {
		t.Errorf("legacy watchdog must NOT be deleted when liveness install fails; deletes=%v", fakeSch.deletes())
	}
	got := out.String()
	for _, want := range []string{"supervisor-liveness task install failed", "mcphub setup", "schtasks /Create /XML"} {
		if !strings.Contains(got, want) {
			t.Errorf("stdout missing %q; got %q", want, got)
		}
	}
}

func TestSetup_NonWindowsLivenessNotImplementedContinues(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("ErrNotImplemented liveness installs must fail closed on Windows")
	}
	_, fakeSch := setupWatchdogTestHelper(t)
	fakeSch.importXMLErr = fmt.Errorf("linux scheduler backend: %w", scheduler.ErrNotImplemented)

	out := &bytes.Buffer{}
	if err := runSetupWatchdog(out, false); err != nil {
		t.Fatalf("runSetupWatchdog: %v", err)
	}
	if got := fakeSch.importXMLByName(api.LivenessTaskName); got != 1 {
		t.Errorf("liveness ImportXML calls = %d, want 1", got)
	}
	if !containsTask(fakeSch.deletes(), api.LegacyWatchdogTaskName) {
		t.Errorf("expected legacy watchdog cleanup after POSIX liveness skip; got %v", fakeSch.deletes())
	}
	got := out.String()
	for _, want := range []string{"supervisor-liveness task skipped", "Windows-only capability"} {
		if !strings.Contains(got, want) {
			t.Errorf("stdout missing %q; got %q", want, got)
		}
	}
}

func TestSetup_InstallsLivenessBeforeRemovingLegacyWatchdog(t *testing.T) {
	_, fakeSch := setupWatchdogTestHelper(t)

	out := &bytes.Buffer{}
	if err := runSetupWatchdog(out, false); err != nil {
		t.Fatalf("runSetupWatchdog: %v", err)
	}
	want := []string{
		"import:" + api.LivenessTaskName,
		"delete:" + api.LegacyWatchdogTaskName,
	}
	got := fakeSch.operationOrder()
	if len(got) < len(want) {
		t.Fatalf("operation order = %v, want prefix %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("operation order = %v, want prefix %v", got, want)
		}
	}
}

// TestSetup_RunsTwice_Idempotent asserts re-running setup does not error and
// the settled liveness task is verified without a second ImportXML call.
func TestSetup_RunsTwice_Idempotent(t *testing.T) {
	_, fakeSch := setupWatchdogTestHelper(t)

	out := &bytes.Buffer{}
	if err := runSetupWatchdog(out, false); err != nil {
		t.Fatalf("first runSetupWatchdog: %v", err)
	}
	if err := runSetupWatchdog(out, false); err != nil {
		t.Fatalf("second runSetupWatchdog: %v", err)
	}
	if got := fakeSch.importXMLByName(api.LivenessTaskName); got != 1 {
		t.Errorf("liveness ImportXML calls = %d after two runs, want 1", got)
	}
	if got := fakeSch.importXMLByName(api.LegacyWatchdogTaskName); got != 0 {
		t.Errorf("watchdog ImportXML calls = %d, want 0 across both runs", got)
	}
}

// ---------------------------------------------------------------------------
// runSetupWatchdog — state-dir sanity rejection (§16, exit 8).
// ---------------------------------------------------------------------------

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
		t.Errorf("ImportXML should NOT be called when refusing elevation; got %d calls", len(fakeSch.importXMLCalls))
	}
}

// TestSetup_Elevated_WithAllowElevated_HappyPath_AuditWritten covers §42 +
// §61: with --allow-elevated, the high-priority audit entry is written first
// (now tagged with the LIVENESS task), then the liveness install proceeds.
//
// Verification routes through the recording-audit seam rather than
// ReadIntentAuditTail (deleted with the watchdog log reader in Phase D).
func TestSetup_Elevated_WithAllowElevated_HappyPath_AuditWritten(t *testing.T) {
	_, fakeSch := setupWatchdogTestHelper(t)
	setupIsElevatedFn = func() (bool, error) { return true, nil }

	r := &recordingAuditWriter{}
	installRecordingAuditFromCLITest(t, r)

	out := &bytes.Buffer{}
	if err := runSetupWatchdog(out, true); err != nil {
		t.Fatalf("runSetupWatchdog with allowElevated=true: %v", err)
	}
	if got := fakeSch.importXMLByName(api.LivenessTaskName); got != 1 {
		t.Errorf("liveness ImportXML calls = %d, want 1", got)
	}

	found := false
	for _, e := range r.entries {
		if e.Action == api.AuditActionWatchdogInstallElevatedOverride {
			if e.Priority != "high" {
				t.Errorf("audit entry Priority = %q, want high", e.Priority)
			}
			if !strings.Contains(e.Reason, "--allow-elevated") {
				t.Errorf("audit entry Reason = %q, want it to mention --allow-elevated", e.Reason)
			}
			found = true
			break
		}
	}
	if !found {
		t.Errorf("audit log missing elevated-override entry; entries=%+v", r.entries)
	}
}

// TestSetup_Elevated_WithAllowElevated_AuditFails_Exit11 covers §61: when the
// elevated-override audit write fails, setup must exit 11 and the liveness
// task must NOT be installed.
func TestSetup_Elevated_WithAllowElevated_AuditFails_Exit11(t *testing.T) {
	_, fakeSch := setupWatchdogTestHelper(t)
	setupIsElevatedFn = func() (bool, error) { return true, nil }

	r := &recordingAuditWriter{
		failActions: map[string]error{api.AuditActionWatchdogInstallElevatedOverride: errors.New("synthetic audit failure")},
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
		t.Errorf("ImportXML should NOT be called when audit fails; got %d calls", len(fakeSch.importXMLCalls))
	}
}

// ---------------------------------------------------------------------------
// runSetupWatchdog — EventLog source registration (§60, non-fatal).
// ---------------------------------------------------------------------------

// TestSetup_EventLogRegistrationFails_NonFatal stubs the EventLog
// registration seam to return an error; setup must continue and the
// liveness task must still be installed.
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
	if got := fakeSch.importXMLByName(api.LivenessTaskName); got != 1 {
		t.Errorf("liveness should still install when EventLog reg fails; got %d liveness ImportXML calls", got)
	}
	if !strings.Contains(out.String(), "EventLog source registration failed") {
		t.Errorf("stdout missing non-fatal EventLog warning; got %q", out.String())
	}
}

// ---------------------------------------------------------------------------
// runUninstallWatchdog ordering — Task 11.1 + §60 EventLog removal.
// ---------------------------------------------------------------------------

// TestUninstall_OrderingDisableWatchdogFirst asserts the legacy watchdog
// task is deleted FIRST (before peer EventLog cleanup) on a last-server
// uninstall.
func TestUninstall_OrderingDisableWatchdogFirst(t *testing.T) {
	_, fakeSch := setupWatchdogTestHelper(t)

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
	if deletes[0] != api.LegacyWatchdogTaskName {
		t.Errorf("first Delete call = %q, want %q (legacy watchdog removed first)", deletes[0], api.LegacyWatchdogTaskName)
	}
}

// TestUninstall_RemoveEventLogSource asserts the EventLog Remove path runs
// when the partial-uninstall gate authorizes the global teardown.
func TestUninstall_RemoveEventLogSource(t *testing.T) {
	_, fakeSch := setupWatchdogTestHelper(t)

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

// TestUninstall_LastServer_RemovesWatchdog covers the P2 happy path: the only
// managed server is the one being uninstalled → the legacy watchdog is removed.
func TestUninstall_LastServer_RemovesWatchdog(t *testing.T) {
	_, fakeSch := setupWatchdogTestHelper(t)

	fakeSch.listResult = []scheduler.TaskStatus{
		{Name: "\\mcp-local-hub-time-default"},
		// Maintenance tasks must NOT count toward the remaining set.
		{Name: api.LegacyWatchdogTaskName},
		{Name: api.LivenessTaskName},
		{Name: "\\mcp-local-hub-weekly-refresh"},
	}

	out := &bytes.Buffer{}
	if err := runUninstallWatchdog(out, "time"); err != nil {
		t.Fatalf("runUninstallWatchdog: %v", err)
	}
	deletes := fakeSch.deletes()
	if len(deletes) == 0 || deletes[0] != api.LegacyWatchdogTaskName {
		t.Errorf("expected legacy watchdog Delete call first; got %v", deletes)
	}
	if strings.Contains(out.String(), "watchdog kept installed") {
		t.Errorf("stdout should not say 'watchdog kept installed' for last-server uninstall; got %q", out.String())
	}
}

// TestUninstall_PartialServer_KeepsWatchdog covers the P2 fix: when other
// managed servers remain, the legacy watchdog + liveness tasks must stay.
func TestUninstall_PartialServer_KeepsWatchdog(t *testing.T) {
	_, fakeSch := setupWatchdogTestHelper(t)

	fakeSch.listResult = []scheduler.TaskStatus{
		{Name: "\\mcp-local-hub-time-default"},
		{Name: "\\mcp-local-hub-wolfram-default"},
		{Name: api.LegacyWatchdogTaskName},
		{Name: api.LivenessTaskName},
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
	if containsTask(fakeSch.deletes(), api.LegacyWatchdogTaskName) {
		t.Errorf("legacy watchdog must NOT be deleted while other servers remain; got %v", fakeSch.deletes())
	}
	if containsTask(fakeSch.deletes(), api.LivenessTaskName) {
		t.Errorf("liveness must NOT be deleted while other servers remain; got %v", fakeSch.deletes())
	}
	if got := atomic.LoadInt32(&removeEventLogCalls); got != 0 {
		t.Errorf("EventLog removal must be skipped when maintenance tasks stay; got %d calls", got)
	}
	if !strings.Contains(out.String(), "watchdog kept installed") {
		t.Errorf("stdout missing 'watchdog kept installed' informational line; got %q", out.String())
	}
}

func TestUninstall_SupervisorIntentPeerOnly_KeepsWatchdogAndLiveness(t *testing.T) {
	stateDir, fakeSch := setupWatchdogTestHelper(t)

	fakeSch.listResult = []scheduler.TaskStatus{
		{Name: "\\mcp-local-hub-time-default"},
		{Name: api.LegacyWatchdogTaskName},
		{Name: api.LivenessTaskName},
	}
	if err := api.WriteSupervisorIntent(filepath.Join(stateDir, "supervisor-intent.json"), &api.SupervisorIntentFile{
		Version: 1,
		Daemons: []api.SupervisorDaemon{{
			TaskName: `\mcp-local-hub-wolfram-default`,
			Server:   "wolfram",
			Daemon:   "default",
			Port:     9126,
		}},
	}); err != nil {
		t.Fatalf("seed supervisor-intent.json: %v", err)
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
	if containsTask(fakeSch.deletes(), api.LegacyWatchdogTaskName) {
		t.Errorf("legacy watchdog must NOT be deleted while supervisor-intent peer remains; got %v", fakeSch.deletes())
	}
	if containsTask(fakeSch.deletes(), api.LivenessTaskName) {
		t.Errorf("liveness must NOT be deleted while supervisor-intent peer remains; got %v", fakeSch.deletes())
	}
	if got := atomic.LoadInt32(&removeEventLogCalls); got != 0 {
		t.Errorf("EventLog removal must be skipped while supervisor-intent peer remains; got %d calls", got)
	}
	if !strings.Contains(out.String(), "wolfram") {
		t.Errorf("stdout should identify the supervisor-intent peer retaining maintenance tasks; got %q", out.String())
	}
}

// TestUninstall_LastServer_RemovesLivenessTask covers the liveness teardown
// alongside the legacy watchdog on a last-server uninstall.
func TestUninstall_LastServer_RemovesLivenessTask(t *testing.T) {
	_, fakeSch := setupWatchdogTestHelper(t)

	fakeSch.listResult = []scheduler.TaskStatus{
		{Name: "\\mcp-local-hub-time-default"},
		{Name: api.LegacyWatchdogTaskName},
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
	if !containsTask(deletes, api.LegacyWatchdogTaskName) {
		t.Errorf("expected Delete(%q) on last-server uninstall; got %v", api.LegacyWatchdogTaskName, deletes)
	}
	if len(deletes) == 0 || deletes[0] != api.LegacyWatchdogTaskName {
		t.Errorf("legacy watchdog must be deleted FIRST; got %v", deletes)
	}
}

// TestShouldRemoveGlobalWatchdog_LivenessAndWatchdogOnly_GatePasses is the
// regression guard for the gate-poison defect: api.IsMaintenanceTaskName must
// classify BOTH `-watchdog` and `-liveness` so a scheduler list of only those
// two hub-wide maintenance tasks lets the last-server gate authorize teardown.
func TestShouldRemoveGlobalWatchdog_LivenessAndWatchdogOnly_GatePasses(t *testing.T) {
	_, fakeSch := setupWatchdogTestHelper(t)

	fakeSch.listResult = []scheduler.TaskStatus{
		{Name: api.LivenessTaskName},
		{Name: api.LegacyWatchdogTaskName},
	}

	out := &bytes.Buffer{}
	if !shouldRemoveGlobalWatchdog(out, "time") {
		t.Errorf("gate must pass (return true) when only liveness + watchdog remain; output=%q", out.String())
	}
	if strings.Contains(out.String(), "kept installed") {
		t.Errorf("gate should not report 'kept installed' when no real server tasks remain; got %q", out.String())
	}
}

func TestUninstall_ListError_FailsClosed(t *testing.T) {
	_, fakeSch := setupWatchdogTestHelper(t)
	fakeSch.listErr = errors.New("synthetic list failure")

	out := &bytes.Buffer{}
	if err := runUninstallWatchdog(out, "time"); err != nil {
		t.Fatalf("runUninstallWatchdog: %v", err)
	}
	if containsTask(fakeSch.deletes(), api.LegacyWatchdogTaskName) {
		t.Errorf("List error must fail-closed (keep watchdog); got %v", fakeSch.deletes())
	}
}

// ---------------------------------------------------------------------------
// runUninstall orchestration (bot P1.1) — per-server before watchdog.
// ---------------------------------------------------------------------------

type uninstallCallRecord struct {
	step   string
	server string
}

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
	for _, c := range calls {
		if c.step == "watchdog" {
			t.Errorf("watchdog teardown invoked despite per-server failure; calls=%+v", calls)
		}
	}
	if strings.Contains(out.String(), "Uninstall complete") {
		t.Errorf("stdout should not contain 'Uninstall complete' on per-server failure; got %q", out.String())
	}
}

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

// ---------------------------------------------------------------------------
// Audit fail-injection helper (cli-package equivalent of installRecordingAudit).
// ---------------------------------------------------------------------------

// installRecordingAuditFromCLITest routes the api-package audit disk-append
// seam through a recording writer so per-Action fail-injection works in cli
// tests without reaching the private appendIntentAuditFn directly.
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

// recordingAuditWriter mirrors the api-package fail-injection helper so cli
// tests can assert Action / Priority / Reason without parsing raw JSON.
type recordingAuditWriter struct {
	entries     []api.IntentAuditEntry
	failActions map[string]error // Action → error
	calls       int32
}

// parseAuditLineForCLITest decodes a single JSON-Lines audit row into the
// public IntentAuditEntry shape. Tolerant of unknown fields.
func parseAuditLineForCLITest(line []byte) api.IntentAuditEntry {
	var entry api.IntentAuditEntry
	trimmed := bytes.TrimRight(line, "\n")
	_ = json.Unmarshal(trimmed, &entry)
	return entry
}
