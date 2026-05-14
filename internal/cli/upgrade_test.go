package cli

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"

	"mcp-local-hub/internal/api"
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

// TestUpgradeCmd_DelegatesToInstallUpgrade pins that running
// `mcphub upgrade` invokes the same code path as `mcphub install
// --upgrade` — verifies the self-replace guard fires identically
// when the runtime conditions match, so we know the alias isn't
// silently diverging from the install-flag variant.
func TestUpgradeCmd_DelegatesToInstallUpgrade(t *testing.T) {
	resetUpgradeSeams(t)

	canonical := "C:\\Users\\u\\.local\\bin\\mcphub.exe"
	upgradeExecutableFn = func() (string, error) { return canonical, nil }
	upgradeTargetPathFn = func() (string, error) { return canonical, nil }
	stopCalled := false
	upgradeStopAllFn = func() ([]api.RestartResult, error) {
		stopCalled = true
		return nil, nil
	}

	root := NewRootCmd()
	root.SetArgs([]string{"upgrade"})
	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	root.SetOut(stdout)
	root.SetErr(stderr)
	root.SilenceUsage = true
	root.SilenceErrors = true

	err := root.Execute()
	if err == nil {
		t.Fatal("want self-replace guard error (same as `install --upgrade` from canonical path), got nil")
	}
	if !strings.Contains(err.Error(), "refusing to --upgrade from the canonical binary") {
		t.Errorf("expected self-replace guard error from runInstallUpgrade; got %q", err.Error())
	}
	if stopCalled {
		t.Errorf("StopAll must NOT fire when self-replace guard catches; same invariant as install --upgrade")
	}
}

// TestUpgradeCmd_HappyPath_AliasReachesSameWorkflow pins that on the
// non-canonical-path (good) case, the alias runs through StopAll →
// Bootstrap → RestartAll just like `install --upgrade`. Uses fakes
// so no real daemons are touched.
func TestUpgradeCmd_HappyPath_AliasReachesSameWorkflow(t *testing.T) {
	resetUpgradeSeams(t)

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

	root := NewRootCmd()
	root.SetArgs([]string{"upgrade"})
	root.SetOut(&bytes.Buffer{})
	root.SetErr(&bytes.Buffer{})
	root.SilenceUsage = true
	root.SilenceErrors = true

	if err := root.Execute(); err != nil {
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
