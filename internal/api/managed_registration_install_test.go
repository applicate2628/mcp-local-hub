package api

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"mcp-local-hub/internal/clients"
	"mcp-local-hub/internal/config"
)

// TestW10_FrozenCodexAliasAndTimeoutReconcilesWithoutReresolve catches the
// managed runtime dropping the planner's frozen Codex relocation descriptor and
// falling back to a no-op/direct AddEntry path.
func TestW10_FrozenCodexAliasAndTimeoutReconcilesWithoutReresolve(t *testing.T) {
	root := t.TempDir()
	codexHome := filepath.Join(root, "codex-home")
	stateDir := filepath.Join(root, "state")
	if err := os.MkdirAll(codexHome, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CODEX_HOME", codexHome)
	configPath := filepath.Join(codexHome, "config.toml")
	if err := os.WriteFile(configPath, []byte("[mcp_servers.serena-mcphub]\nurl = \"http://127.0.0.1:9121/mcp\"\ntool_timeout_sec = 45\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	client, err := clients.NewCodexCLI()
	if err != nil {
		t.Fatal(err)
	}
	update := ClientUpdatePlan{Client: "codex-cli", Action: ClientUpdateAddReplace, EntryName: "serena-mcphub", URL: "http://127.0.0.1:9121/mcp", ToolTimeoutSec: 45, DaemonName: "default"}
	input := managedRegistrationFrozenInstallInput{
		Server: "serena", ReleaseIdentity: managedRegistrationReleaseIdentity{Version: "0.4.37-test", Commit: "install-fixture"},
		CapturedAtUTC: time.Date(2026, 9, 10, 2, 0, 0, 0, time.UTC), StateRoot: stateDir, JournalRoot: stateDir,
		BackupIO: managedRegistrationBackupIO{}, ClientPopulation: map[string]clients.Client{"codex-cli": client},
		Targets: []managedRegistrationFrozenInstallTarget{{
			ClientID: "codex-cli", LogicalEntryName: "serena", PhysicalEntryName: update.EntryName, Action: update.Action,
			DaemonName: "default", TaskName: "mcp-local-hub-serena-default", Port: 9121, URLPath: "/mcp",
			Desired: installClientEntryV1("serena", update, ""),
			Codex:   &managedRegistrationFrozenCodexRelocation{SourceEntryName: "serena", TargetEntryName: update.EntryName, ExpectedSource: clients.CodexTransportNone, AlreadyConfigured: true},
		}},
	}
	writerCalls := guardW10ReconcilerClientWriter(t)
	report, err := (managedRegistrationInstallCoreV1{api: &API{}}).ReconcileInstall(context.Background(), input)
	if err != nil {
		t.Fatalf("ReconcileInstall: %v", err)
	}
	if len(report.Rows) != 1 || report.Rows[0].Settlement != clients.ManagedRegistrationSettlementSettled {
		t.Fatalf("managed report = %#v", report)
	}
	if *writerCalls != 0 {
		t.Fatalf("post-install managed settlement called client writer %d time(s)", *writerCalls)
	}
	logical, err := client.GetEntry("serena")
	if err != nil || logical != nil {
		t.Fatalf("logical source after reconcile = %#v, %v", logical, err)
	}
	target, err := client.GetEntry("serena-mcphub")
	if err != nil || target == nil || target.URL != update.URL || target.ToolTimeoutSec != 45 {
		t.Fatalf("frozen target after reconcile = %#v, %v", target, err)
	}
	journal, err := newManagedRegistrationJournalStore(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	stored, err := journal.read(context.Background())
	if err != nil || !managedRegistrationJournalSettled(stored) {
		t.Fatalf("managed journal = %#v, %v", stored, err)
	}
}

func TestW10_InstallCompositionFreezesPhysicalAliasAndTimeout(t *testing.T) {
	fixture := newW10InstallCompositionFixture(t)
	core := &w10ManagedInstallCoreFake{}
	materializeCalls := 0
	clientRowsAtMaterialize := -1
	var order []string
	core.onCall = func() { order = append(order, "managed-settlement") }
	deps := fixture.deps(core, func(_ context.Context, _ *API, _ *config.ServerManifest, plan *Plan, _ InstallOpts, _ ClientRoutingAuthorityRequest, _ io.Writer) error {
		materializeCalls++
		order = append(order, "pr-core")
		clientRowsAtMaterialize = len(plan.ClientUpdates)
		return nil
	})
	var receipt InstallMutationReceiptV1
	err := fixture.api.installFrozenPlanCoreWithManagedDeps(context.Background(), fixture.manifest, fixture.plan, fixture.opts, CanonicalAtAdmission(), io.Discard, func(got InstallMutationReceiptV1) { receipt = got }, deps)
	if err != nil {
		t.Fatalf("installFrozenPlanCoreWithManagedDeps: %v", err)
	}
	if materializeCalls != 1 || clientRowsAtMaterialize != 1 || core.calls != 1 || !receipt.Committed || strings.Join(order, ",") != "pr-core,managed-settlement" {
		t.Fatalf("materialize/clientRows/core/committed = %d/%d/%d/%t", materializeCalls, clientRowsAtMaterialize, core.calls, receipt.Committed)
	}
	if len(core.inputs) != 1 || len(core.inputs[0].Targets) != 1 {
		t.Fatalf("managed inputs = %#v", core.inputs)
	}
	input, target := core.inputs[0], core.inputs[0].Targets[0]
	if input.Server != "serena" || input.StateRoot != fixture.stateDir || input.JournalRoot != fixture.stateDir || input.ReleaseIdentity.Version != "0.4.37-test" || input.ReleaseIdentity.Commit != "install-test-commit" || !input.CapturedAtUTC.Equal(fixture.now) {
		t.Fatalf("frozen input = %#v", input)
	}
	if target.ClientID != "codex-cli" || target.LogicalEntryName != "serena" || target.PhysicalEntryName != "serena-mcphub" || target.Desired.Name != "serena-mcphub" || target.Desired.ToolTimeoutSec != 45 || target.Codex == nil || target.Codex.ExpectedSource != clients.CodexTransportStdio || target.Codex.AlreadyConfigured {
		t.Fatalf("frozen target = %#v", target)
	}
	if len(input.ClientPopulation) != 1 || input.ClientPopulation["codex-cli"] == nil || len(input.ClientConstructionFailures) != 0 {
		t.Fatalf("frozen client population = %#v failures=%#v", input.ClientPopulation, input.ClientConstructionFailures)
	}
}

func TestW10_CommandCoordinatorMutationThenReceiverOnce(t *testing.T) {
	for _, tc := range []struct {
		name          string
		managedErr    error
		wantCommit    string
		wantReceivers int
	}{
		{name: "managed_success", wantCommit: "settled", wantReceivers: 1},
		{name: "committed_managed_failure", managedErr: errW10InstallManagedFailure, wantCommit: "committed_unverified", wantReceivers: 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fixture := newW10InstallCompositionFixture(t)
			core := &w10ManagedInstallCoreFake{err: tc.managedErr}
			materializeCalls := 0
			deps := fixture.deps(core, func(_ context.Context, _ *API, _ *config.ServerManifest, plan *Plan, _ InstallOpts, _ ClientRoutingAuthorityRequest, _ io.Writer) error {
				materializeCalls++
				if len(plan.ClientUpdates) != 1 {
					t.Fatalf("PR core did not receive the frozen client update: %#v", plan.ClientUpdates)
				}
				return nil
			})
			mutation := &w10InstallCommandMutation{fixture: fixture, deps: deps}
			receiver := &commandReceiverFake{settlement: CommandSettlementV1{CommitState: "settled"}}
			settlement, err := (CommandCoordinator{Mutation: mutation, Receiver: receiver}).Install(context.Background(), fixture.opts)
			if tc.managedErr == nil && err != nil {
				t.Fatalf("CommandCoordinator.Install: %v", err)
			}
			if tc.managedErr != nil && !errors.Is(err, tc.managedErr) {
				t.Fatalf("CommandCoordinator.Install error = %v, want %v", err, tc.managedErr)
			}
			if tc.managedErr != nil {
				var committed *InstallCommittedUnverifiedError
				if !errors.As(err, &committed) {
					t.Fatalf("CommandCoordinator.Install error = %T %v, want InstallCommittedUnverifiedError", err, err)
				}
			}
			if mutation.calls != 1 || materializeCalls != 1 || core.calls != 1 || receiver.calls != tc.wantReceivers {
				t.Fatalf("mutation/materialize/core/receiver calls = %d/%d/%d/%d", mutation.calls, materializeCalls, core.calls, receiver.calls)
			}
			if settlement.CommitState != tc.wantCommit {
				t.Fatalf("commit state = %q, want %q", settlement.CommitState, tc.wantCommit)
			}
		})
	}
}

func TestW10_InstallCompositionSkipsManagedForEmptyDryRunOrNoClientConfig(t *testing.T) {
	for _, tc := range []struct {
		name              string
		dryRun            bool
		skipClientConfig  bool
		emptyTargets      bool
		wantMaterialRows  int
		wantReceiptCommit bool
	}{
		{name: "empty_targets", emptyTargets: true, wantMaterialRows: 0, wantReceiptCommit: true},
		{name: "dry_run", dryRun: true, wantMaterialRows: 1, wantReceiptCommit: false},
		{name: "skip_client_config", skipClientConfig: true, wantMaterialRows: 1, wantReceiptCommit: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fixture := newW10InstallCompositionFixture(t)
			fixture.opts.DryRun = tc.dryRun
			fixture.opts.SkipClientConfigWrites = tc.skipClientConfig
			if tc.emptyTargets {
				fixture.plan.ClientUpdates = nil
			}
			core := &w10ManagedInstallCoreFake{}
			materializeCalls := 0
			materialRows := -1
			deps := fixture.deps(core, func(_ context.Context, _ *API, _ *config.ServerManifest, plan *Plan, _ InstallOpts, _ ClientRoutingAuthorityRequest, _ io.Writer) error {
				materializeCalls++
				materialRows = len(plan.ClientUpdates)
				return nil
			})
			var receipt InstallMutationReceiptV1
			err := fixture.api.installFrozenPlanCoreWithManagedDeps(context.Background(), fixture.manifest, fixture.plan, fixture.opts, CanonicalAtAdmission(), io.Discard, func(got InstallMutationReceiptV1) { receipt = got }, deps)
			if err != nil {
				t.Fatalf("installFrozenPlanCoreWithManagedDeps: %v", err)
			}
			if materializeCalls != 1 || materialRows != tc.wantMaterialRows || core.calls != 0 || receipt.Committed != tc.wantReceiptCommit || receipt.DryRun != tc.dryRun {
				t.Fatalf("materialize/rows/core/committed/dry = %d/%d/%d/%t/%t", materializeCalls, materialRows, core.calls, receipt.Committed, receipt.DryRun)
			}
			if (tc.emptyTargets || tc.skipClientConfig) && (receipt.RequireBindings || len(receipt.bindingExpectations) != 0) {
				t.Fatalf("no-client receipt carries binding requirements: %#v", receipt)
			}
			entries, readErr := os.ReadDir(fixture.stateDir)
			if readErr != nil || len(entries) != 0 {
				t.Fatalf("non-managed case state entries = %#v, %v", entries, readErr)
			}
		})
	}
}

func TestW10_PRCoreFailureSkipsManagedSettlement(t *testing.T) {
	fixture := newW10InstallCompositionFixture(t)
	core := &w10ManagedInstallCoreFake{}
	coreFailure := errors.New("injected PR core failure")
	deps := fixture.deps(core, func(context.Context, *API, *config.ServerManifest, *Plan, InstallOpts, ClientRoutingAuthorityRequest, io.Writer) error {
		return coreFailure
	})
	var receipt InstallMutationReceiptV1
	err := fixture.api.installFrozenPlanCoreWithManagedDeps(context.Background(), fixture.manifest, fixture.plan, fixture.opts, CanonicalAtAdmission(), io.Discard, func(got InstallMutationReceiptV1) { receipt = got }, deps)
	if !errors.Is(err, coreFailure) || core.calls != 0 || receipt.Committed {
		t.Fatalf("core failure err=%v managed calls=%d committed=%t", err, core.calls, receipt.Committed)
	}
}

func TestW10_AdoptCommittedUnverifiedPreservesDurableState(t *testing.T) {
	entry := "managed-settlement-unverified"
	codexPath, manifestRoot, _ := setupAdoptTestEnv(t, entry, "[mcp_servers."+entry+"]\ncommand = \"go\"\nargs = [\"version\"]\n\n[mcp_servers."+entry+".env]\nAPI_KEY = \"literal-secret-value\"\n")
	if _, err := NewAPI().SecretsInit(); err != nil {
		t.Fatalf("SecretsInit: %v", err)
	}
	preAdopt := mustReadFileForAdoptTest(t, codexPath)
	plan, err := NewAPI().BuildAdoptPlan(AdoptOpts{EntryName: entry, Client: "codex-cli", ManifestName: entry, Port: nextBindableAdoptPortForTest(t, collectUsedAdoptPorts())})
	if err != nil || len(plan.SecretRoutedKeys) == 0 {
		t.Fatalf("BuildAdoptPlan: plan=%+v err=%v", plan, err)
	}
	cause := newManagedRegistrationFailure(ManagedRegistrationFailurePreconditionConflict, nil)
	err = NewAPI().ExecuteAdoptWithOpts(plan, io.Discard, ExecuteAdoptOpts{
		installFn: func(context.Context, *API, InstallOpts) error { return &InstallCommittedUnverifiedError{cause: cause} },
	})
	var committed *InstallCommittedUnverifiedError
	if !errors.As(err, &committed) || !strings.Contains(err.Error(), "PRESERVED") || strings.Contains(err.Error(), "can be re-run") {
		t.Fatalf("committed-unverified adopt error = %T %v", err, err)
	}
	assertPreservedProvenance(t, entry, plan, manifestRoot, map[string][]byte{filepath.Clean(codexPath): preAdopt})
}

var errW10InstallManagedFailure = errors.New("injected committed managed-registration failure")

type w10ManagedInstallCoreFake struct {
	calls  int
	inputs []managedRegistrationFrozenInstallInput
	err    error
	onCall func()
}

func (f *w10ManagedInstallCoreFake) ReconcileInstall(_ context.Context, input managedRegistrationFrozenInstallInput) (*ManagedRegistrationReconcileReport, error) {
	f.calls++
	if f.onCall != nil {
		f.onCall()
	}
	f.inputs = append(f.inputs, input)
	return &ManagedRegistrationReconcileReport{}, f.err
}

type w10InstallCompositionFixture struct {
	api      *API
	manifest *config.ServerManifest
	plan     *Plan
	opts     InstallOpts
	client   clients.Client
	stateDir string
	now      time.Time
}

func newW10InstallCompositionFixture(t *testing.T) w10InstallCompositionFixture {
	t.Helper()
	root := t.TempDir()
	codexHome := filepath.Join(root, "codex-home")
	stateDir := filepath.Join(root, "state")
	if err := os.MkdirAll(codexHome, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CODEX_HOME", codexHome)
	if err := os.WriteFile(filepath.Join(codexHome, "config.toml"), []byte("[mcp_servers.serena]\ncommand = \"uvx\"\nargs = [\"serena\"]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	client, err := clients.NewCodexCLI()
	if err != nil {
		t.Fatal(err)
	}
	update := ClientUpdatePlan{Client: "codex-cli", Action: ClientUpdateAddReplace, EntryName: "serena-mcphub", URL: "http://127.0.0.1:9121/mcp", ToolTimeoutSec: 45, DaemonName: "default"}
	return w10InstallCompositionFixture{
		api: NewAPI(), client: client, stateDir: stateDir, now: time.Date(2026, 9, 10, 2, 30, 0, 0, time.UTC),
		manifest: &config.ServerManifest{
			Name: "serena", Kind: config.KindGlobal, Transport: config.TransportNativeHTTP,
			Daemons:        []config.DaemonSpec{{Name: "default", Port: 9121}},
			ClientBindings: []config.ClientBinding{{Client: "codex-cli", Daemon: "default", URLPath: "/mcp", ToolTimeoutSec: 45}},
		},
		plan: &Plan{
			Server: "serena", SupervisorIntent: []SupervisorIntentEntry{{Name: "mcp-local-hub-serena-default", Args: []string{"daemon", "--server", "serena", "--daemon", "default"}}},
			ClientUpdates: []ClientUpdatePlan{update}, installRelayExePath: filepath.Join(root, "mcphub.exe"),
		},
		opts: InstallOpts{Server: "serena", Writer: io.Discard},
	}
}

func (f w10InstallCompositionFixture) deps(core managedRegistrationInstallCore, materialize managedRegistrationInstallMaterializer) managedRegistrationInstallCompositionDeps {
	return managedRegistrationInstallCompositionDeps{
		Core: core,
		Clients: func() (map[string]clients.Client, []string) {
			return map[string]clients.Client{"codex-cli": f.client}, nil
		},
		StateDir:    func() (string, error) { return f.stateDir, nil },
		BuildInfo:   func() (string, string, string) { return "0.4.37-test", "install-test-commit", "ignored" },
		Now:         func() time.Time { return f.now },
		Materialize: materialize,
	}
}

type w10InstallCommandMutation struct {
	fixture w10InstallCompositionFixture
	deps    managedRegistrationInstallCompositionDeps
	calls   int
}

func (m *w10InstallCommandMutation) Install(ctx context.Context, opts InstallOpts) (InstallMutationReceiptV1, error) {
	m.calls++
	var receipt InstallMutationReceiptV1
	err := m.fixture.api.installFrozenPlanCoreWithManagedDeps(ctx, m.fixture.manifest, m.fixture.plan, opts, CanonicalAtAdmission(), io.Discard, func(got InstallMutationReceiptV1) { receipt = got }, m.deps)
	return receipt, err
}

func (m *w10InstallCommandMutation) Restart(context.Context, RestartCommandRequestV1) (RestartMutationReceiptV1, error) {
	return RestartMutationReceiptV1{}, errors.New("unexpected restart")
}
