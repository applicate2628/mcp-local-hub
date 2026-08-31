package cli

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"mcp-local-hub/internal/api"
	"mcp-local-hub/internal/scheduler"
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
	// A daemon-bearing intent (≥1 descriptor row) is what makes the routing
	// discriminator classify the host as a real v0.5 supervisor (bot r32 P2).
	// A bare `{}` / stops-only file routes as "no v0.5 supervisor" now.
	seedDaemonBearingIntent(t, stateDir)

	upgradeExecutableFn = func() (string, error) { return windowsFixturePath("X", "fixture", "candidate.exe"), nil }
	upgradeTargetPathFn = func() (string, error) { return windowsFixturePath("X", "fixture", "canonical.exe"), nil }
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
		t.Fatal("supervisor-intent.json with ≥1 daemon row present → expected the v5 cold-restart upgrade path, but v5UpgradeFn was not invoked")
	}
}

// seedDaemonBearingIntent writes a supervisor-intent.json carrying exactly one
// long-lived daemon descriptor under the given state dir. The routing
// discriminator (hasSupervisorIntent) classifies a host as a v0.5 supervisor
// ONLY when at least one such row survives api.ReadSupervisorIntent's
// one-shot/maintenance filtering (bot r32 P2), so tests that pin the v5 route
// must seed a real descriptor rather than a bare `{}`.
func seedDaemonBearingIntent(t *testing.T, stateDir string) {
	t.Helper()
	intentPath := filepath.Join(stateDir, "supervisor-intent.json")
	if err := api.WriteSupervisorIntent(intentPath, &api.SupervisorIntentFile{
		Version: 1,
		Daemons: []api.SupervisorDaemon{{
			TaskName: `\mcp-local-hub-memory-default`,
			Server:   "memory",
			Daemon:   "default",
			Command:  "mcphub",
			Args:     []string{"daemon", "--server", "memory", "--daemon", "default"},
			Port:     9128,
		}},
	}); err != nil {
		t.Fatalf("seed daemon-bearing intent: %v", err)
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
	// Daemon-bearing intent so the routing discriminator classifies this host
	// as a v0.5 supervisor (bot r32 P2); the test then asserts the dev-build
	// guard fires before the v5 upgrade body.
	seedDaemonBearingIntent(t, stateDir)

	upgradeExecutableFn = func() (string, error) { return windowsFixturePath("X", "fixture", "candidate.exe"), nil }
	upgradeTargetPathFn = func() (string, error) { return windowsFixturePath("X", "fixture", "canonical.exe"), nil }
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

func TestDispatchUpgradeReal_FreshInstallFailsClosedBeforeMutation(t *testing.T) {
	resetUpgradeRoutingSeams(t)
	resetUpgradeSeams(t)

	root := t.TempDir()
	t.Cleanup(api.SetDaemonStateRootForTest(root))
	restoreScheduler := api.SetTestSchedulerFactoryFn(func() (scheduler.Scheduler, error) {
		return &upgradeRoutingFakeScheduler{}, nil
	})
	t.Cleanup(restoreScheduler)
	// No supervisor-intent.json seeded → fresh-install branch.

	var v5Invoked bool
	v5UpgradeFn = func(cmd *cobra.Command) error {
		v5Invoked = true
		return nil
	}
	mutated := false
	upgradeStopAllFn = func() ([]api.RestartResult, error) { mutated = true; return nil, nil }
	upgradeBootstrapFn = func(io.Writer) error { mutated = true; return nil }

	cmd := &cobra.Command{}
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	err := dispatchUpgradeReal(cmd)
	if v5Invoked {
		t.Fatal("fresh install (no supervisor-intent.json) must NOT route to the v5 cold-restart path")
	}
	if err == nil || !strings.Contains(err.Error(), "UPGRADE_TRANSACTION_REQUIRES_MANAGED_SUPERVISOR") {
		t.Fatalf("fresh install error = %v", err)
	}
	if mutated {
		t.Fatal("fresh-install upgrade mutated before fail-closed result")
	}
}

func TestRunV5UpgradeReal_UnwiredPlatformFailsClosedBeforeMutation(t *testing.T) {
	resetUpgradeRoutingSeams(t)
	resetUpgradeSeams(t)
	mutated := false
	upgradeStopAllFn = func() ([]api.RestartResult, error) { mutated = true; return nil, nil }
	upgradeBootstrapFn = func(io.Writer) error { mutated = true; return nil }
	v5UpgradeFn = nil
	err := runV5UpgradeReal(&cobra.Command{})
	if err == nil || !strings.Contains(err.Error(), upgradePlatformUnsupportedID) {
		t.Fatalf("error = %v", err)
	}
	if mutated {
		t.Fatal("unwired platform upgrade mutated before fail-closed result")
	}
}

func TestDispatchUpgradeReal_NoIntentLegacySchedulerTasksFailClosedBeforeMutation(t *testing.T) {
	resetUpgradeRoutingSeams(t)
	resetUpgradeSeams(t)

	root := t.TempDir()
	t.Cleanup(api.SetDaemonStateRootForTest(root))
	restoreScheduler := api.SetTestSchedulerFactoryFn(func() (scheduler.Scheduler, error) {
		return &upgradeRoutingFakeScheduler{
			tasks: []scheduler.TaskStatus{{Name: `\mcp-local-hub-memory-default`, State: "Running"}},
		}, nil
	})
	t.Cleanup(restoreScheduler)
	// No supervisor-intent.json seeded, but legacy daemon-shaped scheduler task exists.

	upgradeExecutableFn = func() (string, error) { return windowsFixturePath("X", "fixture", "candidate.exe"), nil }
	upgradeTargetPathFn = func() (string, error) { return windowsFixturePath("X", "fixture", "canonical.exe"), nil }
	var stopped bool
	upgradeStopAllFn = func() ([]api.RestartResult, error) {
		stopped = true
		return []api.RestartResult{{TaskName: `\mcp-local-hub-memory-default`}}, nil
	}
	var bootstrapped bool
	upgradeBootstrapFn = func(io.Writer) error {
		bootstrapped = true
		return nil
	}
	var restarted bool
	upgradeRestartAllFn = func() ([]api.RestartResult, error) {
		restarted = true
		return nil, nil
	}
	var installed []string
	upgradeInstallServerFn = func(server string, w io.Writer) error {
		installed = append(installed, server)
		return nil
	}

	cmd := &cobra.Command{}
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	err := dispatchUpgradeReal(cmd)
	if err == nil || !strings.Contains(err.Error(), "UPGRADE_TRANSACTION_LEGACY_SCHEDULER_UNSUPPORTED") {
		t.Fatalf("dispatchUpgradeReal error = %v", err)
	}
	if stopped || bootstrapped || restarted || len(installed) != 0 {
		t.Fatalf("legacy fail-closed path mutated: stopped=%v bootstrapped=%v restarted=%v installed=%v", stopped, bootstrapped, restarted, installed)
	}
}

func TestRunLegacySchedulerUpgradeMigration_RestartsUnmatchedLegacyTasksAfterBinaryCopy(t *testing.T) {
	resetUpgradeSeams(t)

	upgradeExecutableFn = func() (string, error) { return windowsFixturePath("X", "fixture", "candidate.exe"), nil }
	upgradeTargetPathFn = func() (string, error) { return windowsFixturePath("X", "fixture", "canonical.exe"), nil }

	var order []string
	upgradeStopAllFn = func() ([]api.RestartResult, error) {
		order = append(order, "stop-all")
		return []api.RestartResult{
			{TaskName: `\mcp-local-hub-memory-default`},
			{TaskName: `\mcp-local-hub-lsp-abcd1234-python`},
		}, nil
	}
	upgradeBootstrapFn = func(io.Writer) error {
		order = append(order, "bootstrap")
		return nil
	}
	upgradeRestartAllFn = func() ([]api.RestartResult, error) {
		t.Fatal("legacy migration must not call RestartAll; matched legacy tasks are migrated into supervisor intent")
		return nil, nil
	}
	var restarted []string
	upgradeRestartTasksFn = func(taskNames []string) ([]api.RestartResult, error) {
		order = append(order, "restart-unmatched")
		restarted = append(restarted, taskNames...)
		return []api.RestartResult{{TaskName: taskNames[0]}}, nil
	}
	var installed []string
	upgradeInstallServerFn = func(server string, w io.Writer) error {
		order = append(order, "install:"+server)
		installed = append(installed, server)
		return nil
	}

	cmd := &cobra.Command{}
	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	cmd.SetOut(stdout)
	cmd.SetErr(stderr)
	err := runLegacySchedulerUpgradeMigration(cmd, legacyUpgradeProbe{
		servers:     []string{"memory"},
		legacyTasks: []string{`mcp-local-hub-lsp-abcd1234-python`, `mcp-local-hub-memory-default`},
		unmatched:   []string{`mcp-local-hub-lsp-abcd1234-python`},
	})
	if err != nil {
		t.Fatalf("runLegacySchedulerUpgradeMigration: %v", err)
	}
	wantOrder := []string{"stop-all", "bootstrap", "restart-unmatched", "install:memory"}
	if len(order) != len(wantOrder) {
		t.Fatalf("order = %v, want %v", order, wantOrder)
	}
	for i := range wantOrder {
		if order[i] != wantOrder[i] {
			t.Fatalf("order = %v, want %v", order, wantOrder)
		}
	}
	if len(restarted) != 1 || restarted[0] != `mcp-local-hub-lsp-abcd1234-python` {
		t.Fatalf("restarted unmatched tasks = %v, want [mcp-local-hub-lsp-abcd1234-python]", restarted)
	}
	if len(installed) != 1 || installed[0] != "memory" {
		t.Fatalf("installed servers = %v, want [memory]", installed)
	}
	if !strings.Contains(stderr.String(), "legacy scheduler tasks without matching shipped manifests were left for manual review") {
		t.Fatalf("stderr missing unmatched warning; got %q", stderr.String())
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

// TestHasSupervisorIntent_DaemonBearingReturnsTrue pins the v0.5-supervisor
// contract (bot r32 P2): a regular supervisor-intent.json carrying ≥1 daemon
// descriptor row → (true, nil). Mere file presence is no longer sufficient;
// the file must name an actual supervisor-owned daemon.
func TestHasSupervisorIntent_DaemonBearingReturnsTrue(t *testing.T) {
	root := t.TempDir()
	t.Cleanup(api.SetDaemonStateRootForTest(root))

	stateDir, err := api.DaemonStateDir()
	if err != nil {
		t.Fatalf("DaemonStateDir: %v", err)
	}
	seedDaemonBearingIntent(t, stateDir)

	got, err := hasSupervisorIntent()
	if err != nil {
		t.Fatalf("hasSupervisorIntent err = %v, want nil", err)
	}
	if !got {
		t.Fatalf("hasSupervisorIntent = false, want true (≥1 daemon row present)")
	}
}

// TestHasSupervisorIntent_StopsOnlyReturnsFalse is the FIX 1 (bot r32 P2)
// falsifying regression. A stops-only / descriptor-less supervisor-intent.json
// (the shape a `mcphub ... stop` mints on a v0.4 scheduler-only host: a Stops
// map, ZERO Daemons) must route as "no v0.5 supervisor" so the dispatcher runs
// the legacy-scheduler migration instead of taking runV5UpgradeReal and
// silently dropping the existing legacy scheduler tasks.
//
// Pre-fix this returned true (mere regular-file presence) → the dispatcher
// took the v5 path and skipped probeLegacySchedulerUpgradeServers.
func TestHasSupervisorIntent_StopsOnlyReturnsFalse(t *testing.T) {
	root := t.TempDir()
	t.Cleanup(api.SetDaemonStateRootForTest(root))

	stateDir, err := api.DaemonStateDir()
	if err != nil {
		t.Fatalf("DaemonStateDir: %v", err)
	}
	intentPath := filepath.Join(stateDir, "supervisor-intent.json")
	if err := api.WriteSupervisorIntent(intentPath, &api.SupervisorIntentFile{
		Version: 1,
		// No Daemons. Only a per-task operator-stop override, exactly what a
		// stop writer mints on a scheduler-only host.
		Stops: map[string]api.DaemonIntent{
			`\mcp-local-hub-memory-default`: {
				Desired: api.IntentDesiredStopped,
				Reason:  api.IntentReasonUserStop,
			},
		},
	}); err != nil {
		t.Fatalf("seed stops-only intent: %v", err)
	}

	got, err := hasSupervisorIntent()
	if err != nil {
		t.Fatalf("hasSupervisorIntent err = %v, want nil (stops-only file is readable)", err)
	}
	if got {
		t.Fatalf("hasSupervisorIntent = true on a stops-only/descriptor-less intent; want false so the legacy-scheduler migration path runs (bot r32 P2)")
	}
}

// TestHasSupervisorIntent_EmptyObjectReturnsFalse pins that a bare `{}`
// (zero daemons, zero stops) also routes as "no v0.5 supervisor".
func TestHasSupervisorIntent_EmptyObjectReturnsFalse(t *testing.T) {
	root := t.TempDir()
	t.Cleanup(api.SetDaemonStateRootForTest(root))

	stateDir, err := api.DaemonStateDir()
	if err != nil {
		t.Fatalf("DaemonStateDir: %v", err)
	}
	intentPath := filepath.Join(stateDir, "supervisor-intent.json")
	if err := api.WriteStateFileBytesAtomic(intentPath, []byte("{}")); err != nil {
		t.Fatalf("write intent: %v", err)
	}

	got, err := hasSupervisorIntent()
	if err != nil {
		t.Fatalf("hasSupervisorIntent err = %v, want nil", err)
	}
	if got {
		t.Fatalf("hasSupervisorIntent = true on `{}`; want false (zero daemon rows)")
	}
}

// TestHasSupervisorIntent_UnreadableReturnsError pins the fail-closed posture:
// a corrupt (unparseable) supervisor-intent.json must surface a wrapped hard
// error, NOT a silent false — the routing dispatcher then fails closed instead
// of mis-routing on a file it could not read.
func TestHasSupervisorIntent_UnreadableReturnsError(t *testing.T) {
	root := t.TempDir()
	t.Cleanup(api.SetDaemonStateRootForTest(root))

	stateDir, err := api.DaemonStateDir()
	if err != nil {
		t.Fatalf("DaemonStateDir: %v", err)
	}
	intentPath := filepath.Join(stateDir, "supervisor-intent.json")
	if err := api.WriteStateFileBytesAtomic(intentPath, []byte("not-valid-json{{{")); err != nil {
		t.Fatalf("write corrupt intent: %v", err)
	}

	got, err := hasSupervisorIntent()
	if err == nil {
		t.Fatalf("hasSupervisorIntent err = nil, want non-nil (corrupt JSON must fail closed)")
	}
	if got {
		t.Fatalf("hasSupervisorIntent = true on corrupt JSON, want false")
	}
	if !strings.Contains(err.Error(), "supervisor-intent.json") {
		t.Errorf("error must mention the offending path; got: %v", err)
	}
}

type upgradeRoutingFakeScheduler struct {
	tasks []scheduler.TaskStatus
}

func (f *upgradeRoutingFakeScheduler) Create(scheduler.TaskSpec) error { return nil }
func (f *upgradeRoutingFakeScheduler) Delete(string) error             { return nil }
func (f *upgradeRoutingFakeScheduler) Run(string) error                { return nil }
func (f *upgradeRoutingFakeScheduler) Stop(string) error               { return nil }
func (f *upgradeRoutingFakeScheduler) Status(string) (scheduler.TaskStatus, error) {
	return scheduler.TaskStatus{}, scheduler.ErrTaskNotFound
}
func (f *upgradeRoutingFakeScheduler) List(prefix string) ([]scheduler.TaskStatus, error) {
	out := make([]scheduler.TaskStatus, 0, len(f.tasks))
	for _, task := range f.tasks {
		name := strings.TrimPrefix(task.Name, `\`)
		if prefix == "" || strings.HasPrefix(name, prefix) {
			out = append(out, task)
		}
	}
	return out, nil
}
func (f *upgradeRoutingFakeScheduler) ExportXML(string) ([]byte, error) {
	return nil, scheduler.ErrTaskNotFound
}
func (f *upgradeRoutingFakeScheduler) ImportXML(string, []byte) error { return nil }

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
