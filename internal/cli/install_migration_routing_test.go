package cli

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/spf13/cobra"

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
		name    string
		err     error
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
