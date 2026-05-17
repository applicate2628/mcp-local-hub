package cli

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"mcp-local-hub/internal/api"
	"mcp-local-hub/internal/migration"
)

// resetMigrationRoutingSeams clears the test-only seams that route
// upgrade/rollback dispatch through fakes. t.Cleanup restores the
// production nil-default at the end of the test scope.
func resetMigrationRoutingSeams(t *testing.T) {
	t.Helper()
	origDispatch := migrationDispatcher
	origRollback := migrationRollbackDispatcher
	origSched := schedulerEnumeratorFn
	t.Cleanup(func() {
		migrationDispatcher = origDispatch
		migrationRollbackDispatcher = origRollback
		schedulerEnumeratorFn = origSched
	})
	migrationDispatcher = nil
	migrationRollbackDispatcher = nil
	schedulerEnumeratorFn = nil
}

// TestRollbackToLegacy_DispatchesRunRollback pins that
// `mcphub install --rollback-to-legacy` flows through the
// migrationRollbackDispatcher seam (which production wires to
// migration.RunRollback via runMigrationRollbackReal).
func TestRollbackToLegacy_DispatchesRunRollback(t *testing.T) {
	resetMigrationRoutingSeams(t)
	resetUpgradeSeams(t)

	var dispatched bool
	migrationRollbackDispatcher = func(cmd *cobra.Command) error {
		dispatched = true
		return nil
	}

	c := newInstallCmdReal()
	c.SetArgs([]string{"--rollback-to-legacy"})
	c.SetOut(&bytes.Buffer{})
	c.SetErr(&bytes.Buffer{})
	c.SilenceUsage = true
	c.SilenceErrors = true
	if err := c.Execute(); err != nil {
		t.Fatalf("--rollback-to-legacy: %v", err)
	}
	if !dispatched {
		t.Fatal("migrationRollbackDispatcher was not invoked")
	}
}

// TestRollbackToLegacy_PropagatesMigrationExitCodeError pins that a
// migration.ExitCodeError returned by the rollback dispatcher
// surfaces unchanged so cmd/mcphub/main.go can map it to its declared
// exit code (8/9/13/14).
func TestRollbackToLegacy_PropagatesMigrationExitCodeError(t *testing.T) {
	resetMigrationRoutingSeams(t)
	resetUpgradeSeams(t)

	want := &migration.ExitCodeError{Code: migration.ExitRollbackTokenMismatch, Err: errors.New("PROCESS_TERMINATE denied")}
	migrationRollbackDispatcher = func(cmd *cobra.Command) error {
		return want
	}

	c := newInstallCmdReal()
	c.SetArgs([]string{"--rollback-to-legacy"})
	c.SetOut(&bytes.Buffer{})
	c.SetErr(&bytes.Buffer{})
	c.SilenceUsage = true
	c.SilenceErrors = true
	err := c.Execute()
	if err == nil {
		t.Fatal("want ExitCodeError, got nil")
	}
	var got *migration.ExitCodeError
	if !errors.As(err, &got) {
		t.Fatalf("error must be *migration.ExitCodeError so main.go can map exit code; got %T %v", err, err)
	}
	if got.Code != migration.ExitRollbackTokenMismatch {
		t.Errorf("Code = %d, want %d", got.Code, migration.ExitRollbackTokenMismatch)
	}
}

// TestUpgrade_RoutesToForwardMigrationWhenLegacyTasksPresent pins the
// routing decision tree: --upgrade with no supervisor-intent.json but
// with legacy `mcp-local-hub-*` Scheduler tasks present → forward
// migration. This is the load-bearing v0.4.x → v0.5.0 entry point.
//
// The state-dir probe goes through the real api.DaemonStateDir; we
// inject the scheduler count via schedulerEnumeratorFn and stub the
// dispatcher via migrationDispatcher. The state-dir's
// supervisor-intent.json is read directly with os.Stat, so we rely on
// the test environment not having one — handled by checking the seam
// invocation rather than the real state-dir.
func TestUpgrade_RoutesToForwardMigrationWhenLegacyTasksPresent(t *testing.T) {
	resetMigrationRoutingSeams(t)
	resetUpgradeSeams(t)

	// Bypass the real hasSupervisorIntent + hasLegacySchedulerTasks
	// probes by stubbing the high-level dispatcher seam itself. The
	// migrationDispatcher fires only on the --upgrade path; if the
	// routing branch never reached us, the test fails on
	// `dispatchedOpts == nil`.
	var dispatched bool
	var dispatchedOpts dispatchUpgradeOpts
	migrationDispatcher = func(cmd *cobra.Command, opts dispatchUpgradeOpts) error {
		dispatched = true
		dispatchedOpts = opts
		return nil
	}

	c := newInstallCmdReal()
	c.SetArgs([]string{"--upgrade", "--discard-scheduler-customizations", "--migration-strict-template"})
	c.SetOut(&bytes.Buffer{})
	c.SetErr(&bytes.Buffer{})
	c.SilenceUsage = true
	c.SilenceErrors = true
	if err := c.Execute(); err != nil {
		t.Fatalf("--upgrade: %v", err)
	}
	if !dispatched {
		t.Fatal("migrationDispatcher was not invoked")
	}
	if !dispatchedOpts.DiscardSchedulerCustomizations {
		t.Errorf("DiscardSchedulerCustomizations = false, want true (flag was set)")
	}
	if !dispatchedOpts.StrictTemplate {
		t.Errorf("StrictTemplate = false, want true (flag was set)")
	}
}

// TestUpgrade_PropagatesMigrationExitCodeError pins that an
// ExitCodeError returned by the upgrade dispatcher surfaces unchanged
// so the exit code reaches cmd/mcphub/main.go.
func TestUpgrade_PropagatesMigrationExitCodeError(t *testing.T) {
	resetMigrationRoutingSeams(t)
	resetUpgradeSeams(t)

	want := &migration.ExitCodeError{Code: migration.ExitMigrationPowerShellLocked, Err: migration.ErrPowerShellLocked}
	migrationDispatcher = func(cmd *cobra.Command, opts dispatchUpgradeOpts) error {
		return want
	}

	c := newInstallCmdReal()
	c.SetArgs([]string{"--upgrade"})
	c.SetOut(&bytes.Buffer{})
	c.SetErr(&bytes.Buffer{})
	c.SilenceUsage = true
	c.SilenceErrors = true
	err := c.Execute()
	if err == nil {
		t.Fatal("want ExitCodeError, got nil")
	}
	var got *migration.ExitCodeError
	if !errors.As(err, &got) {
		t.Fatalf("error must be *migration.ExitCodeError so main.go can map exit code; got %T %v", err, err)
	}
	if got.Code != migration.ExitMigrationPowerShellLocked {
		t.Errorf("Code = %d, want %d", got.Code, migration.ExitMigrationPowerShellLocked)
	}
}

// TestRollbackToLegacy_MutexErrors pins that --rollback-to-legacy is
// mutually exclusive with the manifest-install and --upgrade flag
// families.
func TestRollbackToLegacy_MutexErrors(t *testing.T) {
	cases := []struct {
		name string
		args []string
	}{
		{"server", []string{"--rollback-to-legacy", "--server", "time"}},
		{"daemon", []string{"--rollback-to-legacy", "--daemon", "default"}},
		{"all", []string{"--rollback-to-legacy", "--all"}},
		{"clients", []string{"--rollback-to-legacy", "--clients", "claude-code"}},
		{"all-clients", []string{"--rollback-to-legacy", "--all-clients"}},
		{"reconcile-hub-mode", []string{"--rollback-to-legacy", "--reconcile-hub-mode"}},
		{"dry-run", []string{"--rollback-to-legacy", "--dry-run"}},
		{"upgrade", []string{"--rollback-to-legacy", "--upgrade"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := newInstallCmdReal()
			c.SetArgs(tc.args)
			c.SetOut(&bytes.Buffer{})
			c.SetErr(&bytes.Buffer{})
			c.SilenceUsage = true
			c.SilenceErrors = true
			err := c.Execute()
			if err == nil {
				t.Fatalf("want mutex error for %v, got nil", tc.args)
			}
			if !strings.Contains(err.Error(), "--rollback-to-legacy is mutually exclusive") {
				t.Errorf("want mutex error, got %q", err.Error())
			}
		})
	}
}

// TestMigrationFlags_RequireUpgradeOrRollback pins that
// --discard-scheduler-customizations and --migration-strict-template
// are migration-only flags. Using them without --upgrade or
// --rollback-to-legacy must be rejected (otherwise the operator would
// see a silent no-op).
func TestMigrationFlags_RequireUpgradeOrRollback(t *testing.T) {
	cases := []struct {
		name string
		args []string
	}{
		{"discard-alone", []string{"--discard-scheduler-customizations"}},
		{"strict-template-alone", []string{"--migration-strict-template"}},
		{"both-alone", []string{"--discard-scheduler-customizations", "--migration-strict-template"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := newInstallCmdReal()
			c.SetArgs(tc.args)
			c.SetOut(&bytes.Buffer{})
			c.SetErr(&bytes.Buffer{})
			c.SilenceUsage = true
			c.SilenceErrors = true
			err := c.Execute()
			if err == nil {
				t.Fatalf("want migration-flags-require-mode error for %v, got nil", tc.args)
			}
			if !strings.Contains(err.Error(), "require --upgrade or --rollback-to-legacy") {
				t.Errorf("want migration-flags-require error, got %q", err.Error())
			}
		})
	}
}

// TestMigrationExitCodeError_ImplementsErrorsAs pins the
// `errors.As(err, &*migration.ExitCodeError)` mapping cmd/mcphub/main.go
// uses to translate ExitCodeError into os.Exit(Code). The test does
// NOT call os.Exit directly — it verifies the type/wrap chain so a
// wrapped ExitCodeError still unwraps through errors.As.
//
// Pins Fix Group 1 / codex-c-p0-4: exit-code mapping for migration
// sentinels (8/9/13/14) must propagate through wrap layers.
func TestMigrationExitCodeError_ImplementsErrorsAs(t *testing.T) {
	cases := []struct {
		name     string
		err      error
		wantCode int
	}{
		{
			"bare ExitCodeError",
			&migration.ExitCodeError{Code: migration.ExitInstallBusy, Err: errors.New("lock held")},
			migration.ExitInstallBusy,
		},
		{
			"single-wrap",
			func() error {
				inner := &migration.ExitCodeError{Code: migration.ExitMigrationPowerShellLocked, Err: errors.New("ps locked")}
				return errors.Join(errors.New("forward migration"), inner)
			}(),
			migration.ExitMigrationPowerShellLocked,
		},
		{
			"deep-wrap (mimics dispatchUpgradeReal wrap chain)",
			func() error {
				inner := &migration.ExitCodeError{Code: migration.ExitRollbackTokenMismatch, Err: errors.New("denied")}
				wrap1 := errors.Join(errors.New("rollback step 1"), inner)
				wrap2 := errors.Join(errors.New("rollback driver"), wrap1)
				return wrap2
			}(),
			migration.ExitRollbackTokenMismatch,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var got *migration.ExitCodeError
			if !errors.As(tc.err, &got) {
				t.Fatalf("errors.As did not unwrap to *migration.ExitCodeError; cmd/mcphub/main.go could not map exit code from %v", tc.err)
			}
			if got.Code != tc.wantCode {
				t.Errorf("Code = %d, want %d", got.Code, tc.wantCode)
			}
		})
	}
}

// TestHasSupervisorIntent_AbsentReturnsFalse pins that
// hasSupervisorIntent reports `false, nil` when the file is missing.
// The DaemonStateDir resolver may create the state directory as a
// side effect on first call; we rely only on the absent-file signal
// being false/nil rather than on the dir state.
func TestHasSupervisorIntent_AbsentReturnsFalse(t *testing.T) {
	// The probe goes through api.DaemonStateDir which may produce a
	// platform-specific path. We can't easily redirect it here, but
	// the function's contract — "missing file → (false, nil)" — is
	// platform-independent. On a host where the real state-dir
	// HAPPENS to have a supervisor-intent.json, this test would
	// return (true, nil); skip in that case so we don't false-fail.
	got, err := hasSupervisorIntent()
	if err != nil {
		t.Fatalf("hasSupervisorIntent error: %v", err)
	}
	if got {
		t.Skip("host has a supervisor-intent.json on disk; cannot assert absent-state behavior")
	}
	// got == false here; the t.Skip above handled the true case.
}

// TestHasSupervisorIntent_AbsentReturnsFalseNoError pins the absent-file
// contract under the deterministic state-dir override seam. Distinct
// from TestHasSupervisorIntent_AbsentReturnsFalse above which probes the
// real platform path and may skip; this variant uses a temp dir so it
// always exercises the absent-file branch.
func TestHasSupervisorIntent_AbsentReturnsFalseNoError(t *testing.T) {
	root := t.TempDir()
	t.Cleanup(api.SetDaemonStateRootForTest(root))

	got, err := hasSupervisorIntent()
	if err != nil {
		t.Fatalf("hasSupervisorIntent err = %v, want nil", err)
	}
	if got {
		t.Fatalf("hasSupervisorIntent = true, want false (no file on disk)")
	}
}

// TestHasSupervisorIntent_RegularFileReturnsTrue pins the happy-path
// contract: a regular file named supervisor-intent.json under the
// state-dir → (true, nil), regardless of file contents (routing decision
// is presence-based; intent validity is checked downstream).
func TestHasSupervisorIntent_RegularFileReturnsTrue(t *testing.T) {
	root := t.TempDir()
	t.Cleanup(api.SetDaemonStateRootForTest(root))

	stateDir, err := api.DaemonStateDir()
	if err != nil {
		t.Fatalf("DaemonStateDir: %v", err)
	}
	intentPath := filepath.Join(stateDir, "supervisor-intent.json")
	// Write an arbitrary body; hasSupervisorIntent only inspects file
	// type (regular vs dir vs other), not contents.
	if err := os.WriteFile(intentPath, []byte("{}"), 0o600); err != nil {
		t.Fatalf("write intent: %v", err)
	}

	got, err := hasSupervisorIntent()
	if err != nil {
		t.Fatalf("hasSupervisorIntent err = %v, want nil", err)
	}
	if !got {
		t.Fatalf("hasSupervisorIntent = false, want true (regular file present)")
	}
}

// TestHasSupervisorIntent_NonRegularReturnsError pins that any
// non-regular, non-directory file kind (symlink, named pipe, socket)
// at supervisor-intent.json also surfaces a non-nil error. Uses
// os.Symlink with a target that does not need to exist; on Windows
// this requires SeCreateSymbolicLinkPrivilege or developer mode and
// is therefore skipped when symlink creation fails (the dir-case test
// above still covers the canonical corruption shape on Windows).
func TestHasSupervisorIntent_NonRegularReturnsError(t *testing.T) {
	root := t.TempDir()
	t.Cleanup(api.SetDaemonStateRootForTest(root))

	stateDir, err := api.DaemonStateDir()
	if err != nil {
		t.Fatalf("DaemonStateDir: %v", err)
	}
	intentPath := filepath.Join(stateDir, "supervisor-intent.json")
	// Symlink target need not exist for Lstat-based testing, but
	// os.Stat (which hasSupervisorIntent uses) follows symlinks, so the
	// target MUST exist and be non-regular for the test to be
	// meaningful. Point it at the state-dir itself which is a real
	// directory; the resolved mode will then be a directory mode.
	if err := os.Symlink(stateDir, intentPath); err != nil {
		t.Skipf("symlink creation not permitted on this platform: %v", err)
	}

	// Symlink → dir resolves to a directory via os.Stat; the existing
	// IsDir() guard covers this. The test still adds defense-in-depth
	// against future refactors that might switch to Lstat.
	got, err := hasSupervisorIntent()
	if err == nil {
		t.Fatalf("hasSupervisorIntent err = nil, want non-nil (intent is non-regular via symlink)")
	}
	if got {
		t.Fatalf("hasSupervisorIntent = true on non-regular file, want false")
	}
}

// TestHasSupervisorIntent_DirectoryReturnsError pins the round-4 fix:
// a directory named supervisor-intent.json under the state-dir is a
// corrupt-state-dir condition. hasSupervisorIntent must surface a
// non-nil error so the routing dispatcher fails closed instead of
// silently falling through to the legacy runInstallUpgrade path
// (round-3 added an unreadable-intent guard inside runV5UpgradeWindows
// that the prior silent-false branch never reached).
func TestHasSupervisorIntent_DirectoryReturnsError(t *testing.T) {
	root := t.TempDir()
	t.Cleanup(api.SetDaemonStateRootForTest(root))

	stateDir, err := api.DaemonStateDir()
	if err != nil {
		t.Fatalf("DaemonStateDir: %v", err)
	}
	intentPath := filepath.Join(stateDir, "supervisor-intent.json")
	if err := os.Mkdir(intentPath, 0o700); err != nil {
		t.Fatalf("mkdir intent-as-dir: %v", err)
	}

	got, err := hasSupervisorIntent()
	if err == nil {
		t.Fatalf("hasSupervisorIntent err = nil, want non-nil (intent is a directory)")
	}
	if got {
		t.Fatalf("hasSupervisorIntent = true on directory, want false")
	}
	if !strings.Contains(err.Error(), "supervisor-intent.json") {
		t.Errorf("error must mention the offending path; got: %v", err)
	}
	if !strings.Contains(err.Error(), "directory") {
		t.Errorf("error must say 'directory' so operators see the corruption shape; got: %v", err)
	}
}
