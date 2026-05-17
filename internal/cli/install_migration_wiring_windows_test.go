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
	"os"
	"path/filepath"
	"testing"
	"time"

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
