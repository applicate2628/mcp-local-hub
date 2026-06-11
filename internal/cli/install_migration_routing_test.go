package cli

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"mcp-local-hub/internal/api"
)

// resetUpgradeRoutingSeams clears the test-only upgrade-routing seam so each
// test installs its own fake. t.Cleanup restores the production nil-default at
// the end of the test scope.
//
// v0.6 Phase F NOTE: the migration/rollback routing seams
// (migrationDispatcher, migrationRollbackDispatcher, schedulerEnumeratorFn,
// forwardMigrationFn, rollbackMigrationFn, enumerateAllMcphubTasksFn) were
// deleted with the internal/migration package. Only the cold-restart upgrade
// dispatcher and the v5UpgradeFn wiring seam remain.
func resetUpgradeRoutingSeams(t *testing.T) {
	t.Helper()
	origDispatch := upgradeDispatcher
	origV5 := v5UpgradeFn
	t.Cleanup(func() {
		upgradeDispatcher = origDispatch
		v5UpgradeFn = origV5
	})
	upgradeDispatcher = nil
	v5UpgradeFn = nil
}

// TestUpgrade_DispatchesThroughUpgradeDispatcher pins that
// `mcphub install --upgrade` flows through the upgradeDispatcher seam.
func TestUpgrade_DispatchesThroughUpgradeDispatcher(t *testing.T) {
	resetUpgradeRoutingSeams(t)
	resetUpgradeSeams(t)

	var dispatched bool
	upgradeDispatcher = func(cmd *cobra.Command) error {
		dispatched = true
		return nil
	}

	c := newInstallCmdReal()
	c.SetArgs([]string{"--upgrade"})
	c.SetOut(&bytes.Buffer{})
	c.SetErr(&bytes.Buffer{})
	c.SilenceUsage = true
	c.SilenceErrors = true
	if err := c.Execute(); err != nil {
		t.Fatalf("--upgrade: %v", err)
	}
	if !dispatched {
		t.Fatal("upgradeDispatcher was not invoked")
	}
}

// TestDispatchUpgradeReal_RoutesToV5UpgradeWhenIntentPresent pins the
// machine-state decision tree: supervisor-intent.json present → cold-restart
// upgrade path (runV5UpgradeReal → v5UpgradeFn seam).
func TestDispatchUpgradeReal_RoutesToV5UpgradeWhenIntentPresent(t *testing.T) {
	resetUpgradeRoutingSeams(t)
	resetUpgradeSeams(t)

	root := t.TempDir()
	t.Cleanup(api.SetDaemonStateRootForTest(root))
	stateDir, err := api.DaemonStateDir()
	if err != nil {
		t.Fatalf("DaemonStateDir: %v", err)
	}
	intentPath := filepath.Join(stateDir, "supervisor-intent.json")
	if err := os.WriteFile(intentPath, []byte("{}"), 0o600); err != nil {
		t.Fatalf("write intent: %v", err)
	}

	upgradeExecutableFn = func() (string, error) { return `C:\dev\mcphub.exe`, nil }
	upgradeTargetPathFn = func() (string, error) { return `C:\Users\u\.local\bin\mcphub.exe`, nil }
	var v5Invoked bool
	v5UpgradeFn = func(cmd *cobra.Command) error {
		v5Invoked = true
		return nil
	}

	cmd := &cobra.Command{}
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	if err := dispatchUpgradeReal(cmd); err != nil {
		t.Fatalf("dispatchUpgradeReal: %v", err)
	}
	if !v5Invoked {
		t.Fatal("supervisor-intent.json present → expected the v5 cold-restart upgrade path, but v5UpgradeFn was not invoked")
	}
}

func TestDispatchUpgradeReal_SupervisorPathRunsUpgradeGuardsBeforeV5(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("dev-build upgrade guard is Windows-only")
	}
	resetUpgradeRoutingSeams(t)
	resetUpgradeSeams(t)

	root := t.TempDir()
	t.Cleanup(api.SetDaemonStateRootForTest(root))
	stateDir, err := api.DaemonStateDir()
	if err != nil {
		t.Fatalf("DaemonStateDir: %v", err)
	}
	intentPath := filepath.Join(stateDir, "supervisor-intent.json")
	if err := os.WriteFile(intentPath, []byte("{}"), 0o600); err != nil {
		t.Fatalf("write intent: %v", err)
	}

	upgradeExecutableFn = func() (string, error) { return `C:\dev\mcphub.exe`, nil }
	upgradeTargetPathFn = func() (string, error) { return `C:\Users\u\.local\bin\mcphub.exe`, nil }
	upgradeBuildVersionFn = func() string { return "dev" }
	var v5Invoked bool
	v5UpgradeFn = func(cmd *cobra.Command) error {
		v5Invoked = true
		return nil
	}

	cmd := &cobra.Command{}
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	err = dispatchUpgradeReal(cmd)
	if err == nil {
		t.Fatal("dispatchUpgradeReal: want dev-build refusal before v5 upgrade, got nil")
	}
	if !strings.Contains(err.Error(), "refusing to --upgrade from a dev-build binary") {
		t.Fatalf("dispatchUpgradeReal error = %v, want dev-build guard refusal", err)
	}
	if v5Invoked {
		t.Fatal("v5 upgrade path was invoked despite dev-build guard failure")
	}
}

// TestDispatchUpgradeReal_FreshInstallFallsBackToLegacyUpgrade pins the
// fresh-install branch: no supervisor-intent.json on disk → the legacy
// runInstallUpgrade body (binary-copy fallback), NOT the v5 cold-restart path.
//
// The assertion routes through the runInstallUpgrade entry seam
// (upgradeExecutableFn): a sentinel error returned there proves the legacy body
// was entered (it resolves the current executable first). The full
// runInstallUpgrade happy-path is covered by install_upgrade_test.go; here we
// only pin the ROUTING decision.
func TestDispatchUpgradeReal_FreshInstallFallsBackToLegacyUpgrade(t *testing.T) {
	resetUpgradeRoutingSeams(t)
	resetUpgradeSeams(t)

	root := t.TempDir()
	t.Cleanup(api.SetDaemonStateRootForTest(root))
	// No supervisor-intent.json seeded → fresh-install branch.

	var v5Invoked bool
	v5UpgradeFn = func(cmd *cobra.Command) error {
		v5Invoked = true
		return nil
	}
	// Sentinel marks that the legacy runInstallUpgrade body was entered
	// (it calls upgradeExecutableFn first via resolveUpgradeSelfPaths).
	sentinel := errors.New("legacy-upgrade-body-reached")
	upgradeExecutableFn = func() (string, error) { return "", sentinel }

	cmd := &cobra.Command{}
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	err := dispatchUpgradeReal(cmd)
	if v5Invoked {
		t.Fatal("fresh install (no supervisor-intent.json) must NOT route to the v5 cold-restart path")
	}
	if !errors.Is(err, sentinel) {
		t.Fatalf("fresh install must fall through to the legacy runInstallUpgrade body (sentinel from its first seam); got %v", err)
	}
}

// TestHasSupervisorIntent_AbsentReturnsFalseNoError pins the absent-file
// contract under the deterministic state-dir override seam.
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
// state-dir → (true, nil), regardless of file contents.
func TestHasSupervisorIntent_RegularFileReturnsTrue(t *testing.T) {
	root := t.TempDir()
	t.Cleanup(api.SetDaemonStateRootForTest(root))

	stateDir, err := api.DaemonStateDir()
	if err != nil {
		t.Fatalf("DaemonStateDir: %v", err)
	}
	intentPath := filepath.Join(stateDir, "supervisor-intent.json")
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

// TestHasSupervisorIntent_DirectoryReturnsError pins the round-4 fix:
// a directory named supervisor-intent.json under the state-dir is a
// corrupt-state-dir condition that must surface a non-nil error so the routing
// dispatcher fails closed instead of silently falling through to the legacy
// runInstallUpgrade path.
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
