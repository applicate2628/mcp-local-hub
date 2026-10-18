package cli

import (
	"bytes"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"mcp-local-hub/internal/api"
	"mcp-local-hub/internal/scheduler"
)

// TestUpgradeCmd_Registered pins that the top-level `mcphub upgrade`
// alias is wired into the root command tree (Available Commands).
// Before this PR, the only entry point was `mcphub install --upgrade`,
// which wasn't discoverable via `mcphub --help`.
func TestUpgradeCmd_Registered(t *testing.T) {
	root := NewRootCmd()
	var got *string
	for _, sub := range root.Commands() {
		if sub.Name() == "upgrade" {
			s := sub.Short
			got = &s
			break
		}
	}
	if got == nil {
		t.Fatal("upgrade subcommand not registered on root")
	}
	if !strings.Contains(*got, "canonical mcphub.exe") {
		t.Errorf("Short description should mention canonical mcphub.exe; got %q", *got)
	}
	if !strings.Contains(*got, "install --upgrade") {
		t.Errorf("Short description should reference install --upgrade alias; got %q", *got)
	}
}

// The top-level alias and install flag share the one upgrade dispatcher.
func TestUpgradeCmd_RoutesThroughDispatchUpgrade(t *testing.T) {
	resetUpgradeRoutingSeams(t)
	resetUpgradeSeams(t)

	var dispatched bool
	upgradeDispatcher = func(cmd *cobra.Command) error {
		dispatched = true
		return nil
	}
	// The alias must not bypass the dispatcher and enter preflight directly.
	upgradeExecutableFn = func() (string, error) {
		t.Fatal("alias bypassed dispatchUpgrade and attempted upgrade preflight directly")
		return "", nil
	}

	root := NewRootCmd()
	root.SetArgs([]string{"upgrade"})
	root.SetOut(&bytes.Buffer{})
	root.SetErr(&bytes.Buffer{})
	root.SilenceUsage = true
	root.SilenceErrors = true

	if err := root.Execute(); err != nil {
		t.Fatalf("upgrade alias: %v", err)
	}
	if !dispatched {
		t.Fatal("upgrade alias did not route through dispatchUpgrade (upgradeDispatcher seam never fired)")
	}
}

// A committed-only recovery request must never enter ordinary upgrade routing,
// which refuses a running canonical GUI before it can inspect the journal.
func TestUpgradeCmd_CommittedRecoveryBypassesOrdinaryDispatcher(t *testing.T) {
	resetUpgradeRoutingSeams(t)
	resetUpgradeSeams(t)
	priorRecovery := committedRecoveryFn
	t.Cleanup(func() { committedRecoveryFn = priorRecovery })
	recovered := false
	committedRecoveryFn = func(*cobra.Command) error {
		recovered = true
		return nil
	}
	upgradeDispatcher = func(*cobra.Command) error {
		t.Fatal("committed recovery entered ordinary upgrade dispatcher")
		return nil
	}
	root := NewRootCmd()
	root.SetArgs([]string{"upgrade", "--complete-committed-recovery"})
	root.SetOut(&bytes.Buffer{})
	root.SetErr(&bytes.Buffer{})
	root.SilenceUsage = true
	root.SilenceErrors = true
	if err := root.Execute(); err != nil || !recovered {
		t.Fatalf("committed recovery routing error = %v", err)
	}
}

func TestUpgradeCmd_PendingRecoveryExclusiveAndEarly(t *testing.T) {
	resetUpgradeRoutingSeams(t)
	resetUpgradeSeams(t)
	prior := pendingRecoveryFn
	t.Cleanup(func() { pendingRecoveryFn = prior })
	called := 0
	pendingRecoveryFn = func(*cobra.Command) error { called++; return nil }
	upgradeDispatcher = func(*cobra.Command) error {
		t.Fatal("pending recovery entered normal promotion dispatcher")
		return nil
	}
	upgradeExecutableFn = func() (string, error) { t.Fatal("pending recovery entered self/promotion preflight"); return "", nil }
	root := NewRootCmd()
	root.SetArgs([]string{"upgrade", "--recover-pending-transaction"})
	root.SetOut(&bytes.Buffer{})
	root.SetErr(&bytes.Buffer{})
	if err := root.Execute(); err != nil || called != 1 {
		t.Fatalf("early pending route: %v calls=%d", err, called)
	}
}

func TestUpgradeCmd_PendingRecoveryRejectsOtherModesArgumentsAndGUIFlags(t *testing.T) {
	priorPending, priorCommitted := pendingRecoveryFn, committedRecoveryFn
	t.Cleanup(func() { pendingRecoveryFn = priorPending; committedRecoveryFn = priorCommitted })
	pendingRecoveryFn = func(*cobra.Command) error { t.Fatal("invalid request reached pending recovery"); return nil }
	committedRecoveryFn = func(*cobra.Command) error { t.Fatal("invalid request reached committed recovery"); return nil }
	for _, suffix := range [][]string{{"--complete-committed-recovery"}, {"--terminate-gui-for-upgrade"}, {"--stop-gui-owned-fleet-for-upgrade"}, {"unexpected"}} {
		root := NewRootCmd()
		root.SetArgs(append([]string{"upgrade", "--recover-pending-transaction"}, suffix...))
		root.SetOut(&bytes.Buffer{})
		root.SetErr(&bytes.Buffer{})
		if err := root.Execute(); err == nil || !strings.Contains(err.Error(), "UPGRADE_PENDING_RECOVERY_NOT_ADMITTED") {
			t.Fatalf("suffix=%v error=%v", suffix, err)
		}
	}
}

func TestUpgradeCmd_FalsePendingFlagRetainsNormalDispatcher(t *testing.T) {
	resetUpgradeRoutingSeams(t)
	prior := pendingRecoveryFn
	t.Cleanup(func() { pendingRecoveryFn = prior })
	pendingRecoveryFn = func(*cobra.Command) error { t.Fatal("false recovery flag entered recovery"); return nil }
	called := 0
	upgradeDispatcher = func(*cobra.Command) error { called++; return nil }
	root := NewRootCmd()
	root.SetArgs([]string{"upgrade", "--recover-pending-transaction=false"})
	root.SetOut(&bytes.Buffer{})
	root.SetErr(&bytes.Buffer{})
	if err := root.Execute(); err != nil || called != 1 {
		t.Fatalf("normal route: %v calls=%d", err, called)
	}
}

func TestUpgradeCmd_CommittedRecoveryRejectsConflictingFlagsAndArguments(t *testing.T) {
	priorRecovery := committedRecoveryFn
	t.Cleanup(func() { committedRecoveryFn = priorRecovery })
	committedRecoveryFn = func(*cobra.Command) error {
		t.Fatal("recovery adapter called for invalid flags or arguments")
		return nil
	}
	for _, args := range [][]string{
		{"upgrade", "--complete-committed-recovery", "--terminate-gui-for-upgrade"},
		{"upgrade", "--complete-committed-recovery", "--stop-gui-owned-fleet-for-upgrade"},
		{"upgrade", "--complete-committed-recovery", "unexpected"},
	} {
		root := NewRootCmd()
		root.SetArgs(args)
		root.SetOut(&bytes.Buffer{})
		root.SetErr(&bytes.Buffer{})
		root.SilenceUsage = true
		root.SilenceErrors = true
		if err := root.Execute(); err == nil || !strings.Contains(err.Error(), "UPGRADE_COMMITTED_RECOVERY_NOT_ADMITTED") {
			t.Fatalf("args %q: error = %v", args, err)
		}
	}
}

// TestUpgradeCmd_AliasAndFlagShareDispatcher pins that BOTH documented entry
// points — the `mcphub upgrade` alias and the `mcphub install --upgrade` flag —
// route through the SAME dispatchUpgrade seam, so they cannot silently diverge
// (FIX 1, bot r33 P2 on PR #288).
func TestUpgradeCmd_AliasAndFlagShareDispatcher(t *testing.T) {
	for _, tc := range []struct {
		name string
		exec func(root *cobra.Command) error
	}{
		{
			name: "alias",
			exec: func(root *cobra.Command) error {
				root.SetArgs([]string{"upgrade"})
				return root.Execute()
			},
		},
		{
			name: "install-flag",
			exec: func(root *cobra.Command) error {
				root.SetArgs([]string{"install", "--upgrade"})
				return root.Execute()
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resetUpgradeRoutingSeams(t)
			resetUpgradeSeams(t)

			var dispatched int
			upgradeDispatcher = func(cmd *cobra.Command) error {
				dispatched++
				return nil
			}

			root := NewRootCmd()
			root.SetOut(&bytes.Buffer{})
			root.SetErr(&bytes.Buffer{})
			root.SilenceUsage = true
			root.SilenceErrors = true

			if err := tc.exec(root); err != nil {
				t.Fatalf("%s: %v", tc.name, err)
			}
			if dispatched != 1 {
				t.Fatalf("%s: dispatchUpgrade fired %d time(s), want exactly 1", tc.name, dispatched)
			}
		})
	}
}

func TestUpgradeCmd_FreshHostAliasFailsClosedWithoutMutation(t *testing.T) {
	resetUpgradeRoutingSeams(t)
	resetUpgradeSeams(t)

	// Fresh-install state has no managed daemon fleet, so the shared dispatcher
	// must return its stable pre-mutation refusal.
	root := t.TempDir()
	t.Cleanup(api.SetDaemonStateRootForTest(root))
	restoreScheduler := api.SetTestSchedulerFactoryFn(func() (scheduler.Scheduler, error) {
		return &upgradeRoutingFakeScheduler{}, nil
	})
	t.Cleanup(restoreScheduler)

	upgradeExecutableFn = func() (string, error) { return windowsFixturePath("X", "fixture", "candidate.exe"), nil }
	upgradeTargetPathFn = func() (string, error) { return windowsFixturePath("X", "fixture", "canonical.exe"), nil }
	rootCmd := NewRootCmd()
	rootCmd.SetArgs([]string{"upgrade"})
	rootCmd.SetOut(&bytes.Buffer{})
	rootCmd.SetErr(&bytes.Buffer{})
	rootCmd.SilenceUsage = true
	rootCmd.SilenceErrors = true

	err := rootCmd.Execute()
	if err == nil || !strings.Contains(err.Error(), upgradeRequiresManagedSupervisorID) {
		t.Fatalf("upgrade alias error = %v", err)
	}
}
