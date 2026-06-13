package cli

import (
	"bytes"
	"errors"
	"io"
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

// TestUpgradeCmd_RoutesThroughDispatchUpgrade is the FIX 1 (bot r33 P2,
// PR #288) falsifying regression. The top-level `mcphub upgrade` alias MUST
// route through the SAME machine-state dispatcher (dispatchUpgrade) the
// `install --upgrade` flag uses — NOT call the legacy runInstallUpgrade body
// directly.
//
// Pre-fix the alias's RunE was `return runInstallUpgrade(cmd)`, which bypasses
// the dispatcher: on a v0.5+ host with daemon rows it ran the legacy
// stop/copy/restart path instead of the supervisor rename-aside + IPC handoff.
// With the seam injected, a pre-fix alias would never fire upgradeDispatcher;
// this assertion catches that divergence.
func TestUpgradeCmd_RoutesThroughDispatchUpgrade(t *testing.T) {
	resetUpgradeRoutingSeams(t)
	resetUpgradeSeams(t)

	var dispatched bool
	upgradeDispatcher = func(cmd *cobra.Command) error {
		dispatched = true
		return nil
	}
	// If the alias regressed to calling runInstallUpgrade directly, it would
	// resolve the current executable first via this seam. Fail loud if so.
	upgradeExecutableFn = func() (string, error) {
		t.Fatal("alias reached the legacy runInstallUpgrade body (resolved current executable) instead of routing through dispatchUpgrade")
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

// TestUpgradeCmd_HappyPath_AliasReachesSameWorkflow pins that on a
// fresh-install host (no supervisor-intent.json, no legacy scheduler tasks),
// the alias's dispatchUpgrade routing falls through to the legacy
// runInstallUpgrade body and runs StopAll → Bootstrap → RestartAll, identical
// to `install --upgrade`. Uses fakes so no real daemons are touched.
func TestUpgradeCmd_HappyPath_AliasReachesSameWorkflow(t *testing.T) {
	resetUpgradeRoutingSeams(t)
	resetUpgradeSeams(t)

	// Fresh-install state: empty state-dir (no supervisor-intent.json) and an
	// empty fake scheduler (no legacy daemon tasks) → dispatchUpgradeReal falls
	// through to runInstallUpgrade.
	root := t.TempDir()
	t.Cleanup(api.SetDaemonStateRootForTest(root))
	restoreScheduler := api.SetTestSchedulerFactoryFn(func() (scheduler.Scheduler, error) {
		return &upgradeRoutingFakeScheduler{}, nil
	})
	t.Cleanup(restoreScheduler)

	upgradeExecutableFn = func() (string, error) { return "C:\\dev\\mcphub.exe", nil }
	upgradeTargetPathFn = func() (string, error) { return "C:\\Users\\u\\.local\\bin\\mcphub.exe", nil }
	var order []string
	upgradeStopAllFn = func() ([]api.RestartResult, error) {
		order = append(order, "stop")
		return []api.RestartResult{{TaskName: "demo"}}, nil
	}
	upgradeBootstrapFn = func(w io.Writer) error {
		order = append(order, "bootstrap")
		return nil
	}
	upgradeRestartAllFn = func() ([]api.RestartResult, error) {
		order = append(order, "restart")
		return []api.RestartResult{{TaskName: "demo"}}, nil
	}

	rootCmd := NewRootCmd()
	rootCmd.SetArgs([]string{"upgrade"})
	rootCmd.SetOut(&bytes.Buffer{})
	rootCmd.SetErr(&bytes.Buffer{})
	rootCmd.SilenceUsage = true
	rootCmd.SilenceErrors = true

	if err := rootCmd.Execute(); err != nil {
		t.Fatalf("upgrade alias: %v", err)
	}
	want := []string{"stop", "bootstrap", "restart"}
	if len(order) != len(want) {
		t.Fatalf("order = %v, want %v", order, want)
	}
	for i, step := range want {
		if order[i] != step {
			t.Errorf("order[%d] = %q, want %q", i, order[i], step)
		}
	}
}

// Suppress unused-import linter when running narrow subset.
var _ = errors.New
