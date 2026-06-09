//go:build windows

// install_migration_wiring_windows_test.go — Windows-tagged unit tests
// for the production migration wiring at install_migration_wiring_windows.go.
//
// The tests target two convergent codex-r2 P0/P1 findings:
//
//   - Lane C P0 / codex-r2-a/b/c-p0: the ForwardOptions built by
//     runForwardMigrationWindows must wire RollbackOnFailure so a step-14
//     reconcile-ready timeout drives auto-rollback in-process. Without
//     it, journal.go falls back to the manual-rollback error message
//     AFTER legacy scheduler tasks are already deleted, leaving the host
//     in a broken state. TestRunForwardMigrationWindows_WiresRollbackOnFailure
//     pins the contract.
//
//   - Lane F P0 #2 / codex-r2-b/f-p1: lookupMigrationProcessIdentity
//     must map process.ErrProcessNotFound onto migration.ErrProcessNotFound
//     so journal.go:1142's `errors.Is(idErr, migration.ErrProcessNotFound)`
//     genuine-unbound cross-check fires. Without the mapping, the two
//     sentinels live in different packages and every "PID gone" surfaces
//     as a transient-retry-exhaustion abort. TestRunForwardMigrationWindows_ErrProcessNotFoundMappedToMigrationSentinel
//     pins both the mapping and the negative path (other errors pass
//     through unchanged).

package cli

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"mcp-local-hub/internal/api"
	"mcp-local-hub/internal/api/apitest"
	"mcp-local-hub/internal/migration"
	"mcp-local-hub/internal/process"
)

// withNoopSchedulerEnv pins MCPHUB_E2E_SCHEDULER=none for the test
// scope so scheduler.New() returns a noopScheduler instead of dialing
// the real Windows Task Scheduler. The host CI scheduler is shared
// with developer-installed mcp-local-hub-* tasks; a real call here
// would surface those rows and risk side-effecting them.
func withNoopSchedulerEnv(t *testing.T) {
	t.Helper()
	prev, hadPrev := os.LookupEnv("MCPHUB_E2E_SCHEDULER")
	if err := os.Setenv("MCPHUB_E2E_SCHEDULER", "none"); err != nil {
		t.Fatalf("set MCPHUB_E2E_SCHEDULER: %v", err)
	}
	t.Cleanup(func() {
		if hadPrev {
			_ = os.Setenv("MCPHUB_E2E_SCHEDULER", prev)
		} else {
			_ = os.Unsetenv("MCPHUB_E2E_SCHEDULER")
		}
	})
}

// withTempStateDir routes api.DaemonStateDir() through a per-test
// tmp dir so buildForwardMigrationOptions / buildRollbackMigrationOptions
// do not write into the developer's real %LOCALAPPDATA%\mcp-local-hub.
// Returns the absolute tmp path so callers can seed supervisor-intent.json
// when the rollback closure path is being exercised.
//
// Uses apitest.HardenedTempDir so the parent-directory DACL gate in
// api.WriteStateFileAtomic accepts the path. The default %TEMP%
// under R:\Temp grants Authenticated Users (S-1-5-11) write/delete
// rights inherited from the workstation's TEMP DACL, which trips the
// state-file allowlist; the hardened helper installs a PROTECTED
// single-user DACL that matches the allowlist.
func withTempStateDir(t *testing.T) string {
	t.Helper()
	root := apitest.HardenedTempDir(t)
	restore := api.SetDaemonStateRootForTest(root)
	t.Cleanup(restore)
	return root
}

// TestRunForwardMigrationWindows_WiresRollbackOnFailure pins the
// codex-r2-a/b/c P0 contract: runForwardMigrationWindows (via its
// extracted helper buildForwardMigrationOptions) MUST wire
// migration.ForwardOptions.RollbackOnFailure so journal.go's step-14
// reconcile-ready timeout drives RunRollback in-process instead of
// the historical "consider --rollback-to-legacy" fall-through.
//
// The test asserts only that the field is non-nil — the journal's
// own test suite (internal/migration/journal_test.go:724,
// TestForwardMigration_ReconcileReadyTimeoutAutoRollbacks) already
// covers the auto-rollback execution semantics, so duplicating that
// here would be redundant. The wiring assertion is what kept being
// missed in production.
func TestRunForwardMigrationWindows_WiresRollbackOnFailure(t *testing.T) {
	withNoopSchedulerEnv(t)
	withTempStateDir(t)

	_, mopts, err := buildForwardMigrationOptions(dispatchUpgradeOpts{})
	if err != nil {
		t.Fatalf("buildForwardMigrationOptions: %v", err)
	}
	if mopts.RollbackOnFailure == nil {
		t.Fatal("ForwardOptions.RollbackOnFailure is nil — a step-14 reconcile-ready timeout would fall back to the manual-rollback error AFTER legacy tasks have already been deleted, leaving the host half-migrated. Production must wire this callback.")
	}
}

// TestRunForwardMigrationWindows_RollbackOnFailureClosureReadsSupervisorIntent
// extends the above wiring assertion: when the closure fires AND
// supervisor-intent.json is on disk (journal.go step 7 writes it at
// line 1027 BEFORE step 14 can time out), the closure must return a
// non-nil RollbackOptions whose ExpectedDaemons mirrors the on-disk
// intent. This is what gives journal.go's RunRollback the daemon
// list it needs for port-unbound verification at rollback step 3.
func TestRunForwardMigrationWindows_RollbackOnFailureClosureReadsSupervisorIntent(t *testing.T) {
	withNoopSchedulerEnv(t)
	stateDir := withTempStateDir(t)

	// Seed supervisor-intent.json the way journal.go step 7 would.
	intent := &api.SupervisorIntentFile{
		Version:   1,
		UpdatedAt: time.Now().UTC().Format(time.RFC3339Nano),
		Daemons: []api.SupervisorDaemon{
			{
				TaskName: `\mcp-local-hub-time-default`,
				Server:   "time",
				Daemon:   "default",
				Port:     9130,
			},
		},
		StrictMode: false,
	}
	if err := api.WriteSupervisorIntent(filepath.Join(stateDir, "supervisor-intent.json"), intent); err != nil {
		t.Fatalf("seed supervisor-intent.json: %v", err)
	}

	_, mopts, err := buildForwardMigrationOptions(dispatchUpgradeOpts{})
	if err != nil {
		t.Fatalf("buildForwardMigrationOptions: %v", err)
	}
	if mopts.RollbackOnFailure == nil {
		t.Fatal("ForwardOptions.RollbackOnFailure is nil — see TestRunForwardMigrationWindows_WiresRollbackOnFailure")
	}

	rbOpts := mopts.RollbackOnFailure()
	if rbOpts == nil {
		t.Fatal("RollbackOnFailure() returned nil even with supervisor-intent.json on disk — the auto-rollback path is unreachable")
	}
	if len(rbOpts.ExpectedDaemons) != 1 {
		t.Fatalf("RollbackOptions.ExpectedDaemons len = %d, want 1 (read from seeded supervisor-intent.json)", len(rbOpts.ExpectedDaemons))
	}
	if rbOpts.ExpectedDaemons[0].TaskName != `\mcp-local-hub-time-default` {
		t.Errorf("RollbackOptions.ExpectedDaemons[0].TaskName = %q, want %q", rbOpts.ExpectedDaemons[0].TaskName, `\mcp-local-hub-time-default`)
	}
	if rbOpts.Scheduler == nil {
		t.Error("RollbackOptions.Scheduler is nil — rollback would panic")
	}
	if rbOpts.LookupProcessIdentity == nil {
		t.Error("RollbackOptions.LookupProcessIdentity is nil — rollback step 3 port-unbound verification would skip")
	}
	if rbOpts.ShimUninstaller == nil {
		t.Error("RollbackOptions.ShimUninstaller is nil — rollback would leak the autostart shim")
	}
}

// TestRunForwardMigrationWindows_RollbackOnFailureClosureFallsBackOnMissingIntent
// pins the graceful-degradation path: when supervisor-intent.json is
// NOT on disk (e.g. forward migration aborted before step 7), the
// closure returns nil so journal.go falls back to the manual-rollback
// error message. The operator still sees actionable guidance.
func TestRunForwardMigrationWindows_RollbackOnFailureClosureFallsBackOnMissingIntent(t *testing.T) {
	withNoopSchedulerEnv(t)
	withTempStateDir(t) // empty — no supervisor-intent.json seeded

	_, mopts, err := buildForwardMigrationOptions(dispatchUpgradeOpts{})
	if err != nil {
		t.Fatalf("buildForwardMigrationOptions: %v", err)
	}
	if mopts.RollbackOnFailure == nil {
		t.Fatal("ForwardOptions.RollbackOnFailure is nil — see TestRunForwardMigrationWindows_WiresRollbackOnFailure")
	}

	rbOpts := mopts.RollbackOnFailure()
	if rbOpts != nil {
		t.Fatalf("RollbackOnFailure() returned non-nil RollbackOptions when supervisor-intent.json is absent; want nil so journal.go falls back to manual-rollback error. Got %+v", rbOpts)
	}
}

// TestRunForwardMigrationWindows_ErrProcessNotFoundMappedToMigrationSentinel
// pins the codex-r2-b/f-p1 contract: lookupMigrationProcessIdentity
// must wrap process.ErrProcessNotFound as migration.ErrProcessNotFound
// so journal.go:1142's `errors.Is(idErr, migration.ErrProcessNotFound)`
// genuine-unbound cross-check fires. Without the mapping the two
// sentinels live in different packages and the journal treats every
// "PID gone" as a transient-retry-exhaustion abort, breaking the
// Lane F P0 #2 contract.
func TestRunForwardMigrationWindows_ErrProcessNotFoundMappedToMigrationSentinel(t *testing.T) {
	// Stub the process-lookup seam to return the production
	// ErrProcessNotFound sentinel. The adapter MUST translate it.
	origFn := processLookupIdentityFn
	t.Cleanup(func() { processLookupIdentityFn = origFn })
	processLookupIdentityFn = func(pid int) (process.ProcessIdentity, error) {
		return process.ProcessIdentity{}, process.ErrProcessNotFound
	}

	_, err := lookupMigrationProcessIdentity(12345)
	if err == nil {
		t.Fatal("lookupMigrationProcessIdentity returned nil error; want migration.ErrProcessNotFound")
	}
	if !errors.Is(err, migration.ErrProcessNotFound) {
		t.Fatalf("err must wrap migration.ErrProcessNotFound so journal.go:1142 cross-check fires; got %v (process.ErrProcessNotFound match: %v)", err, errors.Is(err, process.ErrProcessNotFound))
	}
}

// TestRunForwardMigrationWindows_OtherLookupErrorsPassThrough pins
// the negative-path contract for the same sentinel mapping: an error
// that is NOT process.ErrProcessNotFound MUST NOT be reshaped into
// migration.ErrProcessNotFound. Without this guarantee the journal
// would treat transient WMI / PowerShell stalls as "PID genuinely
// unbound" and skip the retry-exhaustion abort branch, which is the
// load-bearing fail-closed for the Lane F P0 #2 contract.
func TestRunForwardMigrationWindows_OtherLookupErrorsPassThrough(t *testing.T) {
	origFn := processLookupIdentityFn
	t.Cleanup(func() { processLookupIdentityFn = origFn })

	sentinelOther := errors.New("simulated transient WMI stall")
	processLookupIdentityFn = func(pid int) (process.ProcessIdentity, error) {
		return process.ProcessIdentity{}, sentinelOther
	}

	_, err := lookupMigrationProcessIdentity(12345)
	if err == nil {
		t.Fatal("lookupMigrationProcessIdentity returned nil error; want the simulated transient error")
	}
	if errors.Is(err, migration.ErrProcessNotFound) {
		t.Fatalf("a transient lookup error must NOT collapse onto migration.ErrProcessNotFound (would skip retry-exhaustion abort); got %v", err)
	}
	if !errors.Is(err, sentinelOther) {
		t.Fatalf("err must wrap the underlying transient cause; got %v", err)
	}
}

// TestRunForwardMigrationWindows_SuccessPathCopiesIdentityFields pins
// that on the success path the field-for-field struct copy from
// process.ProcessIdentity to migration.ProcessIdentity preserves every
// field. The two types are parallel by design (see migration/journal.go
// ProcessIdentity docstring); a silent drift between them would
// surface as a phantom 4-gate ownership failure during forward
// migration step 9.
func TestRunForwardMigrationWindows_SuccessPathCopiesIdentityFields(t *testing.T) {
	origFn := processLookupIdentityFn
	t.Cleanup(func() { processLookupIdentityFn = origFn })

	want := process.ProcessIdentity{
		PID:              4242,
		Basename:         "mcphub.exe",
		CommandLine:      `mcphub.exe daemon --server time --daemon default`,
		ExecutablePath:   `C:\Program Files\mcp-local-hub\mcphub.exe`,
		CreationDateUnix: 1714912345,
	}
	processLookupIdentityFn = func(pid int) (process.ProcessIdentity, error) {
		return want, nil
	}

	got, err := lookupMigrationProcessIdentity(4242)
	if err != nil {
		t.Fatalf("lookupMigrationProcessIdentity: %v", err)
	}
	if got.PID != want.PID || got.Basename != want.Basename ||
		got.CommandLine != want.CommandLine || got.ExecutablePath != want.ExecutablePath ||
		got.CreationDateUnix != want.CreationDateUnix {
		t.Errorf("identity field mismatch:\n got %+v\nwant %+v", got, want)
	}
}

// ---------------------------------------------------------------------------
// codex round-3 Lane B P1 #2: PIDForServerDaemon error return contract.
// ---------------------------------------------------------------------------

// TestPIDForServerDaemon_NoMatchReturnsErrProcessNotFound pins the
// confirmed-no-match contract: an empty server/daemon pair must return
// migration.ErrProcessNotFound so the journal's no-running-daemon
// audit branch fires rather than the phantom-abort branch.
func TestPIDForServerDaemon_NoMatchReturnsErrProcessNotFound(t *testing.T) {
	// Empty inputs are the deterministic no-match shortcut — they
	// don't shell out, so the test runs regardless of whether
	// powershell.exe / wmic.exe are available on the CI runner.
	pid, err := pidForServerDaemonViaTasklist("", "")
	if pid != 0 {
		t.Errorf("pid for empty inputs: want 0, got %d", pid)
	}
	if !errors.Is(err, migration.ErrProcessNotFound) {
		t.Fatalf("empty inputs must return migration.ErrProcessNotFound so the journal's no-running-daemon branch fires; got %v", err)
	}
}

// TestPIDForServerDaemon_ProbeFailureReturnsError pins the
// codex round-3 Lane B P1 #2 contract: when the CLM probe transport
// fails, the helper MUST return a non-nil error that is NOT
// ErrProcessNotFound — the journal will then map it to
// MIGRATION_PORT_LOOKUP_INCONSISTENT and abort rather than silently
// classifying as "genuine unbound".
//
// Uses the probePowerShellCLMFn seam to drive the exact decision
// matrix without shelling out to powershell.exe.
func TestPIDForServerDaemon_ProbeFailureReturnsError(t *testing.T) {
	// Case 1: CLM probe transport failure → wrapped error.
	origProbe := probePowerShellCLMFn
	t.Cleanup(func() { probePowerShellCLMFn = origProbe })
	probeErr := errors.New("simulated powershell transport hang")
	probePowerShellCLMFn = func() (bool, error) { return false, probeErr }

	pid, err := pidForServerDaemonViaTasklist("memory", "default")
	if pid != 0 {
		t.Errorf("probe failure: want pid=0, got %d", pid)
	}
	if err == nil {
		t.Fatal("CLM probe transport failure must return non-nil error so journal aborts; got nil")
	}
	if errors.Is(err, migration.ErrProcessNotFound) {
		t.Fatalf("probe failure must NOT collapse onto ErrProcessNotFound (would let journal classify as genuine unbound); got %v", err)
	}
	if !errors.Is(err, probeErr) {
		t.Fatalf("err must wrap the underlying probe cause; got %v", err)
	}

	// Case 2: CLM-locked AND wmic absent → wrapped error.
	probePowerShellCLMFn = func() (bool, error) { return false, nil } // CLM locked but probe itself OK
	origWmic := wmicPresentFn
	t.Cleanup(func() { wmicPresentFn = origWmic })
	wmicPresentFn = func() bool { return false }

	pid, err = pidForServerDaemonViaTasklist("memory", "default")
	if pid != 0 {
		t.Errorf("CLM-locked + wmic absent: want pid=0, got %d", pid)
	}
	if err == nil {
		t.Fatal("CLM-locked + wmic absent must return non-nil error; got nil")
	}
	if errors.Is(err, migration.ErrProcessNotFound) {
		t.Fatalf("CLM-locked + wmic absent must NOT collapse onto ErrProcessNotFound; got %v", err)
	}
	if !strings.Contains(err.Error(), "CLM-locked") {
		t.Fatalf("error wording should name CLM-locked condition; got %v", err)
	}
}

// ---------------------------------------------------------------------------
// codex round-3 Lane C P1 #3: runV5UpgradeWindows fails closed on
// unreadable intent.
// ---------------------------------------------------------------------------

// TestRunV5UpgradeWindows_UnreadableIntentAbortsUpgrade pins the
// fail-closed contract: routing chose this path because
// supervisor-intent.json existed; if ReadSupervisorIntent fails the
// upgrade MUST abort rather than fall through to a no-verify upgrade
// (which would let the new supervisor start without proving the old
// daemon ports unbound — exactly the regression codex-r2-c-p1-8
// already paid for).
//
// We exercise the contract by seeding a corrupt intent file (invalid
// JSON) under the test state-dir, then calling runV5UpgradeWindows
// directly. The expected behavior is a non-nil error wrapping
// "supervisor-intent.json present but unreadable".
//
// We do NOT install a fake UpgradeDeps because the error must fire
// BEFORE RunInstallUpgrade is reached; a fake there would be
// uncovered.
func TestRunV5UpgradeWindows_UnreadableIntentAbortsUpgrade(t *testing.T) {
	withNoopSchedulerEnv(t)
	stateDir := withTempStateDir(t)

	// Seed a corrupt supervisor-intent.json (not valid JSON).
	intentPath := filepath.Join(stateDir, "supervisor-intent.json")
	if err := os.WriteFile(intentPath, []byte("not-valid-json{{{"), 0600); err != nil {
		t.Fatalf("seed corrupt intent: %v", err)
	}

	cmd := &cobra.Command{}
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)

	err := runV5UpgradeWindows(cmd)
	if err == nil {
		t.Fatal("runV5UpgradeWindows must return non-nil error when supervisor-intent.json is unreadable; got nil")
	}
	if !strings.Contains(err.Error(), "supervisor-intent.json present but unreadable") {
		t.Fatalf("error must name the unreadable-intent cause so operators can diagnose; got %v", err)
	}
}

// ---------------------------------------------------------------------------
// codex round-4 Lane C P1: v5UpgradeDeps.ForceKillSupervisor error
// propagation. The historical implementation swallowed every
// ReadSupervisorLockOwner error including permission denied / corrupt
// sidecar / invalid PID and returned nil — hiding the failure from
// the now-strict RunInstallUpgrade path which relies on a non-nil
// return to escalate to verifyPortsUnbound / abort. Only a proven
// "already exited / no owner" condition (file not present) should
// map to benign.
// ---------------------------------------------------------------------------

// TestV5UpgradeDeps_ForceKillSupervisor_NotExistIsBenign pins the
// genuine-no-supervisor-running path: ReadSupervisorLockOwner returns
// os.IsNotExist (the sidecar is absent because no supervisor is
// running). ForceKillSupervisor MUST return nil so the upgrade
// proceeds without trying to kill nothing.
func TestV5UpgradeDeps_ForceKillSupervisor_NotExistIsBenign(t *testing.T) {
	// supervisorLockDir points at a path whose .owner.json does NOT
	// exist. ReadSupervisorLockOwner will return an os.IsNotExist
	// error and the helper must treat that as benign.
	tmpDir := t.TempDir()
	lockDir := filepath.Join(tmpDir, "supervisor.lock")
	// Do NOT create supervisor.lock.owner.json — absence is the test.

	d := &v5UpgradeDeps{supervisorLockDir: lockDir}
	if err := d.ForceKillSupervisor(""); err != nil {
		t.Fatalf("absent .owner.json must map to benign nil (no supervisor running); got %v", err)
	}
}

// TestV5UpgradeDeps_ForceKillSupervisor_PermissionDeniedPropagates pins
// the round-4 fix: a ReadSupervisorLockOwner error that is NOT
// os.IsNotExist must propagate as a non-nil error from
// ForceKillSupervisor. The historical implementation collapsed every
// read failure (permission denied, ENOENT for parent, corrupt JSON)
// onto benign nil, which hid the failure from the upgrade orchestrator.
//
// We simulate the non-IsNotExist failure by pointing the lock dir at
// a path whose .owner.json IS present but contains invalid JSON
// (unmarshal failure). On POSIX a chmod 0 would suffice for
// permission denied; on Windows the symmetric path is harder to set
// up portably, so the corrupt-JSON variant covers the same control
// flow (non-IsNotExist error from ReadSupervisorLockOwner) and is
// the load-bearing test for the propagation contract.
func TestV5UpgradeDeps_ForceKillSupervisor_PermissionDeniedPropagates(t *testing.T) {
	tmpDir := t.TempDir()
	lockDir := filepath.Join(tmpDir, "supervisor.lock")
	ownerPath := lockDir + ".owner.json"

	// Write a corrupt sidecar — json.Unmarshal will fail, which is a
	// non-IsNotExist error path through ReadSupervisorLockOwner.
	if err := os.WriteFile(ownerPath, []byte("not-valid-json{{{"), 0o600); err != nil {
		t.Fatalf("seed corrupt sidecar: %v", err)
	}

	d := &v5UpgradeDeps{supervisorLockDir: lockDir}
	err := d.ForceKillSupervisor("")
	if err == nil {
		t.Fatal("non-IsNotExist ReadSupervisorLockOwner failure must propagate from ForceKillSupervisor; got nil")
	}
	// IsNotExist must NOT be true — we wrote the file deliberately,
	// so the failure must be on the unmarshal path, not the read path.
	if os.IsNotExist(err) {
		t.Fatalf("error should NOT be IsNotExist (file was written); got %v", err)
	}
	if !strings.Contains(err.Error(), "force-kill") && !strings.Contains(err.Error(), "owner") {
		t.Errorf("error should describe the force-kill / lock-owner read failure; got %v", err)
	}
}

// TestV5UpgradeDeps_ForceKillSupervisor_InvalidPIDPropagates pins the
// round-4 fix: a valid sidecar JSON whose PID is non-positive
// (0, negative) is a corrupt-sidecar condition. ForceKillSupervisor
// must propagate a non-nil error so the upgrade aborts rather than
// silently no-op (which would let a stale supervisor process keep
// holding the listening ports).
func TestV5UpgradeDeps_ForceKillSupervisor_InvalidPIDPropagates(t *testing.T) {
	tmpDir := t.TempDir()
	lockDir := filepath.Join(tmpDir, "supervisor.lock")
	ownerPath := lockDir + ".owner.json"

	// Valid JSON, PID=0 (the canonical "unset" sentinel that
	// SupervisorLockOwner uses).
	body := `{"pid":0,"started_at":"2026-05-17T00:00:00Z"}`
	if err := os.WriteFile(ownerPath, []byte(body), 0o600); err != nil {
		t.Fatalf("seed PID-0 sidecar: %v", err)
	}

	d := &v5UpgradeDeps{supervisorLockDir: lockDir}
	err := d.ForceKillSupervisor("")
	if err == nil {
		t.Fatal("invalid PID (<=0) in .owner.json must propagate from ForceKillSupervisor; got nil")
	}
	if !strings.Contains(err.Error(), "PID") {
		t.Errorf("error should name the invalid-PID cause; got %v", err)
	}
}

// ---------------------------------------------------------------------------
// bot PR #276 r3 P2: kill-target identity gate. The supervisor reapers
// (forceKillSupervisor / v5UpgradeDeps.ForceKillSupervisor) read the PID
// from supervisor.lock.owner.json and force-kill it. The owner sidecar
// SURVIVES a supervisor crash (a quiet interlock holder's Release() leaves
// it; an OS-killed supervisor never tidies it), so its recorded PID can be
// REUSED by an unrelated OS process. The pre-fix reapers blindly
// taskkill'd that PID — killing an unrelated live process during the
// migrate's held-lock window. The fix validates the PID is a live mcphub
// supervisor (basename mcphub(.exe) + `supervise` command-line) BEFORE the
// kill; a stale/reused/unrelated PID is treated as "no supervisor to reap"
// (no-op). These tests drive the gate through the processLookupIdentityFn
// seam and observe the kill through the killPIDViaTaskkillFn seam so NOTHING
// is ever actually killed.
// ---------------------------------------------------------------------------

// swapKillPIDViaTaskkillForTest replaces killPIDViaTaskkillFn for the test
// scope and returns a pointer to a slice that records every PID the reaper
// would have force-killed. The recorder returns nil (success) so a reaper
// that DOES decide to kill proceeds through its normal success path.
func swapKillPIDViaTaskkillForTest(t *testing.T) *[]int {
	t.Helper()
	var killed []int
	orig := killPIDViaTaskkillFn
	t.Cleanup(func() { killPIDViaTaskkillFn = orig })
	killPIDViaTaskkillFn = func(pid int) error {
		killed = append(killed, pid)
		return nil
	}
	return &killed
}

// swapProcessLookupForTest installs a fixed lookup result for the
// processLookupIdentityFn seam for the test scope.
func swapProcessLookupForTest(t *testing.T, ident process.ProcessIdentity, err error) {
	t.Helper()
	orig := processLookupIdentityFn
	t.Cleanup(func() { processLookupIdentityFn = orig })
	processLookupIdentityFn = func(pid int) (process.ProcessIdentity, error) {
		// Echo the requested PID so callers that read ident.PID see a
		// faithful value (the real backend does the same).
		out := ident
		out.PID = pid
		return out, err
	}
}

// TestSupervisorPIDIsLiveMcphubSupervisor_Gate is the direct unit test of
// the identity gate.
func TestSupervisorPIDIsLiveMcphubSupervisor_Gate(t *testing.T) {
	const pid = 4242
	cases := []struct {
		name  string
		ident process.ProcessIdentity
		err   error
		want  bool
	}{
		{
			name:  "live mcphub supervisor (exe)",
			ident: process.ProcessIdentity{Basename: "mcphub.exe", CommandLine: `C:\Users\dev\.local\bin\mcphub.exe supervise --strict-mode`},
			want:  true,
		},
		{
			name:  "live mcphub supervisor (no .exe basename)",
			ident: process.ProcessIdentity{Basename: "mcphub", CommandLine: `/usr/local/bin/mcphub supervise`},
			want:  true,
		},
		{
			name:  "reused PID belongs to unrelated process",
			ident: process.ProcessIdentity{Basename: "notepad.exe", CommandLine: `C:\Windows\System32\notepad.exe`},
			want:  false,
		},
		{
			name:  "mcphub process but NOT a supervisor (gui)",
			ident: process.ProcessIdentity{Basename: "mcphub.exe", CommandLine: `mcphub.exe gui --no-browser`},
			want:  false,
		},
		{
			name:  "mcphub process but NOT a supervisor (daemon child)",
			ident: process.ProcessIdentity{Basename: "mcphub.exe", CommandLine: `mcphub.exe daemon --server time --daemon default`},
			want:  false,
		},
		{
			name:  "process gone (ErrProcessNotFound)",
			ident: process.ProcessIdentity{},
			err:   process.ErrProcessNotFound,
			want:  false,
		},
		{
			name:  "transient lookup error",
			ident: process.ProcessIdentity{},
			err:   errors.New("simulated transient WMI stall"),
			want:  false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			swapProcessLookupForTest(t, tc.ident, tc.err)
			if got := supervisorPIDIsLiveMcphubSupervisor(pid); got != tc.want {
				t.Fatalf("supervisorPIDIsLiveMcphubSupervisor(%d) = %v; want %v", pid, got, tc.want)
			}
		})
	}
	// PID <= 0 is rejected up front (no lookup needed).
	if supervisorPIDIsLiveMcphubSupervisor(0) {
		t.Fatal("PID 0 must be rejected by the gate")
	}
	if supervisorPIDIsLiveMcphubSupervisor(-1) {
		t.Fatal("negative PID must be rejected by the gate")
	}
}

// TestForceKillSupervisor_Rollback_SkipsReusedNonSupervisorPID is the
// load-bearing regression for the rollback reaper closure: a sidecar PID
// that has been REUSED by an unrelated process must NOT be force-killed.
//
// Falsification on the unfixed code: without the supervisorPIDIsLiveMcphubSupervisor
// gate, forceKillSupervisor reads owner.PID and calls killPIDViaTaskkillFn
// unconditionally, so `killed` would contain the reused PID — the exact
// kill-an-unrelated-process bug the finding names.
func TestForceKillSupervisor_Rollback_SkipsReusedNonSupervisorPID(t *testing.T) {
	tmpDir := t.TempDir()
	lockPath := filepath.Join(tmpDir, "supervisor.lock")
	ownerPath := lockPath + ".owner.json"

	// Seed a sidecar naming a PID that (per the lookup stub) is now an
	// unrelated process — the post-crash PID-reuse scenario.
	const reusedPID = 31337
	body := `{"pid":31337,"started_at":"2026-05-17T00:00:00Z"}`
	if err := os.WriteFile(ownerPath, []byte(body), 0o600); err != nil {
		t.Fatalf("seed reused-PID sidecar: %v", err)
	}
	swapProcessLookupForTest(t, process.ProcessIdentity{
		Basename:    "python.exe",
		CommandLine: `C:\Python\python.exe some-unrelated-script.py`,
	}, nil)
	killed := swapKillPIDViaTaskkillForTest(t)

	if err := forceKillSupervisor(lockPath)(); err != nil {
		t.Fatalf("forceKillSupervisor on a reused non-supervisor PID must be a benign no-op; got err %v", err)
	}
	if len(*killed) != 0 {
		t.Fatalf("reused non-supervisor PID %d must NOT be force-killed; killed=%v", reusedPID, *killed)
	}
}

// TestForceKillSupervisor_Rollback_KillsLiveSupervisor pins the positive
// path: a sidecar PID that IS a live mcphub supervisor is still reaped
// (the gate does not over-block the legitimate case).
func TestForceKillSupervisor_Rollback_KillsLiveSupervisor(t *testing.T) {
	tmpDir := t.TempDir()
	lockPath := filepath.Join(tmpDir, "supervisor.lock")
	ownerPath := lockPath + ".owner.json"

	const supPID = 4242
	body := `{"pid":4242,"started_at":"2026-05-17T00:00:00Z"}`
	if err := os.WriteFile(ownerPath, []byte(body), 0o600); err != nil {
		t.Fatalf("seed supervisor sidecar: %v", err)
	}
	swapProcessLookupForTest(t, process.ProcessIdentity{
		Basename:    "mcphub.exe",
		CommandLine: `C:\Users\dev\.local\bin\mcphub.exe supervise`,
	}, nil)
	killed := swapKillPIDViaTaskkillForTest(t)

	if err := forceKillSupervisor(lockPath)(); err != nil {
		t.Fatalf("forceKillSupervisor on a live supervisor PID: %v", err)
	}
	if len(*killed) != 1 || (*killed)[0] != supPID {
		t.Fatalf("live supervisor PID %d must be force-killed exactly once; killed=%v", supPID, *killed)
	}
}

// TestV5UpgradeDeps_ForceKillSupervisor_SkipsReusedNonSupervisorPID is the
// same regression for the v0.5 upgrade reaper. A reused non-supervisor PID
// must be a benign no-op (return nil, no kill), NOT a propagated error —
// it proves there is no supervisor to reap, so the upgrade/migrate proceeds
// to its port-unbound verify exactly as on the "no supervisor running"
// path.
//
// Falsification on the unfixed code: ForceKillSupervisor would
// killPIDViaTaskkillFn(owner.PID) unconditionally → `killed` contains the
// reused PID.
func TestV5UpgradeDeps_ForceKillSupervisor_SkipsReusedNonSupervisorPID(t *testing.T) {
	tmpDir := t.TempDir()
	lockDir := filepath.Join(tmpDir, "supervisor.lock")
	ownerPath := lockDir + ".owner.json"

	const reusedPID = 31337
	body := `{"pid":31337,"started_at":"2026-05-17T00:00:00Z"}`
	if err := os.WriteFile(ownerPath, []byte(body), 0o600); err != nil {
		t.Fatalf("seed reused-PID sidecar: %v", err)
	}
	swapProcessLookupForTest(t, process.ProcessIdentity{
		Basename:    "node.exe",
		CommandLine: `C:\Program Files\nodejs\node.exe server.js`,
	}, nil)
	killed := swapKillPIDViaTaskkillForTest(t)

	d := &v5UpgradeDeps{supervisorLockDir: lockDir}
	if err := d.ForceKillSupervisor(""); err != nil {
		t.Fatalf("ForceKillSupervisor on a reused non-supervisor PID must be benign nil; got %v", err)
	}
	if len(*killed) != 0 {
		t.Fatalf("reused non-supervisor PID %d must NOT be force-killed; killed=%v", reusedPID, *killed)
	}
}

// TestV5UpgradeDeps_ForceKillSupervisor_KillsLiveSupervisor pins the
// positive path for the upgrade reaper.
func TestV5UpgradeDeps_ForceKillSupervisor_KillsLiveSupervisor(t *testing.T) {
	tmpDir := t.TempDir()
	lockDir := filepath.Join(tmpDir, "supervisor.lock")
	ownerPath := lockDir + ".owner.json"

	const supPID = 4242
	body := `{"pid":4242,"started_at":"2026-05-17T00:00:00Z"}`
	if err := os.WriteFile(ownerPath, []byte(body), 0o600); err != nil {
		t.Fatalf("seed supervisor sidecar: %v", err)
	}
	swapProcessLookupForTest(t, process.ProcessIdentity{
		Basename:    "mcphub.exe",
		CommandLine: `mcphub.exe supervise --strict-mode`,
	}, nil)
	killed := swapKillPIDViaTaskkillForTest(t)

	d := &v5UpgradeDeps{supervisorLockDir: lockDir}
	if err := d.ForceKillSupervisor(""); err != nil {
		t.Fatalf("ForceKillSupervisor on a live supervisor PID: %v", err)
	}
	if len(*killed) != 1 || (*killed)[0] != supPID {
		t.Fatalf("live supervisor PID %d must be force-killed exactly once; killed=%v", supPID, *killed)
	}
}
