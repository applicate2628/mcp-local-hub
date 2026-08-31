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
	if !strings.Contains(*got, "canonical mcphub binary") {
		t.Errorf("Short description should mention canonical binary; got %q", *got)
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
