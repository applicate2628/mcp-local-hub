package api

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gofrs/flock"
	"mcp-local-hub/internal/clients"
)

func setupProviderInstallFailureFixture(t *testing.T, name string) (string, string, string, *AdoptPlan, *providerLifecycleFake, providerTransactionDeps) {
	t.Helper()
	clientPath, dir, state := setupAdoptTestEnv(t, name, "[mcp_servers]\n")
	preparePreflightBinaryChecks(t)
	installFakeScheduler(t, newInstallFakeScheduler())
	installFakeAutostartBackend(t, &fakeInstallAutostartBackend{})
	t.Cleanup(setSupervisorReconcileApplyHookForTest(func(context.Context, bool) (ReconcileResponse, error) { return ReconcileResponse{}, nil }))
	oldAdmission := frozenStdioBridgeAdmissionFn
	t.Cleanup(func() { frozenStdioBridgeAdmissionFn = oldAdmission })
	frozenStdioBridgeAdmissionFn = func(context.Context, []FrozenStdioBridgeAdmissionRequest) error { return nil }
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cwd := t.TempDir()
	provider := &providerLifecycleFake{entry: clients.ProviderMCPEntryV1{
		ProviderClient: "codex-cli", PluginRef: "fixture@catalog", ServerName: name,
		Transport: clients.ProviderMCPTransportStdio, Command: exe, WorkingDir: &cwd,
		Scope: clients.ProviderMCPScopeUser, Enabled: true,
		ReceiptFingerprint: "receipt", ActivationFingerprint: "activation",
		ActivationEnabledPresent: true, ActivationEnabled: true,
		DisabledActivationFingerprint: "disabled", PolicyState: clients.ProviderMCPPolicyNone, PolicyFingerprint: "policy",
	}}
	plan, err := NewAPI().buildProviderAdoptPlan(AdoptOpts{EntryName: name, Client: "codex-cli", ManifestName: name, ProviderPluginRef: "fixture@catalog", Port: nextBindableAdoptPortForTest(t, collectUsedAdoptPorts())}, nil, provider)
	if err != nil {
		t.Fatal(err)
	}
	deps := providerTransactionDeps{source: provider, observer: func(context.Context, providerDirectProcessIdentityV1, []providerProcessGenerationV1) providerDirectProcessObservationV1 {
		return providerDirectProcessObservationV1{State: providerProcessObservationComplete}
	}}
	return clientPath, dir, state, plan, provider, deps
}

func TestProviderInstallRolledBackFailureRemainsRecoverable(t *testing.T) {
	const name = "provider-rollback-restore-retry"
	clientPath, dir, state, plan, provider, deps := setupProviderInstallFailureFixture(t, name)
	seam := seedInstallWriteSeam(t, map[string]clientWriteSpec{clientPath: {addEntry: addEntryFailMutated, restore: restoreSucceed}})
	provider.failRestore = true
	if err := NewAPI().ExecuteAdoptWithOpts(plan, io.Discard, ExecuteAdoptOpts{providerDeps: deps}); err == nil {
		t.Fatal("injected install failure was lost")
	}
	if seam.writeN[filepath.Clean(clientPath)] != 2 {
		t.Fatalf("test did not execute actual client rollback: writes=%v", seam.writeN)
	}
	entry, err := clients.AllClients()["codex-cli"].GetEntry(name)
	if err != nil || entry != nil {
		t.Fatalf("client not rolled back: entry=%+v err=%v", entry, err)
	}
	rec, found, err := ReadAdoptProvenance(name)
	if err != nil || !found {
		t.Fatalf("recovery provenance missing: found=%v err=%v", found, err)
	}
	phase, err := readProviderInstallPhase(name)
	if err != nil || phase != providerInstallPhaseManagedSettled {
		t.Errorf("successful unpublished rollback lost its durable proof: phase=%q err=%v", phase, err)
	}
	if _, err := os.Stat(filepath.Join(dir, name, "manifest.yaml")); !os.IsNotExist(err) {
		t.Fatalf("rolled-back manifest remains: %v", err)
	}
	if provider.entry.Enabled {
		t.Fatal("failed restore unexpectedly enabled provider")
	}
	provider.failRestore = false
	recovery, err := NewAPI().BuildDeAdoptPlan(name)
	if err != nil {
		t.Fatalf("real rollback recovery cannot be planned: %v", err)
	}
	if _, err := NewAPI().executeDeAdoptPlanWithOpts(recovery, io.Discard, ExecuteDeAdoptOpts{providerDeps: deps}); err != nil {
		t.Fatalf("real rollback recovery failed: %v", err)
	}
	if !provider.entry.Enabled {
		t.Fatal("recovery did not restore provider")
	}
	assertDeAdoptClosed(t, dir, state, *rec)
}

func TestProviderInstallAppliedIntentReleaseFailurePreservesAdoption(t *testing.T) {
	const name = "provider-intent-release-failure"
	clientPath, dir, state, plan, provider, deps := setupProviderInstallFailureFixture(t, name)
	intentPath := filepath.Join(state, supervisorIntentFileLeaf)
	lockPath := supervisorIntentLockPath(intentPath)
	oldUnlock := flockUnlockFn
	cause := errors.New("injected applied supervisor-intent release failure")
	var stranded *flock.Flock
	flockUnlockFn = func(fl *flock.Flock) error {
		if fl.Path() == lockPath {
			if _, err := os.Stat(intentPath); err == nil {
				stranded = fl
				return cause
			}
		}
		return oldUnlock(fl)
	}
	t.Cleanup(func() {
		flockUnlockFn = oldUnlock
		if stranded != nil {
			_ = stranded.Unlock()
		}
		unconfirmedLockReleasesMu.Lock()
		delete(unconfirmedLockReleases, lockPath)
		unconfirmedLockReleasesMu.Unlock()
	})
	err := NewAPI().ExecuteAdoptWithOpts(plan, io.Discard, ExecuteAdoptOpts{providerDeps: deps})
	if !errors.Is(err, cause) || !errors.Is(err, ErrAppliedLockReleaseUnconfirmed) {
		t.Fatalf("expected applied intent release failure: %v", err)
	}
	if stranded == nil {
		t.Fatal("release injection did not hit committed intent")
	}
	if provider.entry.Enabled || provider.calls != 1 {
		t.Errorf("provider restored while committed managed owner remained: enabled=%v calls=%d", provider.entry.Enabled, provider.calls)
	}
	if _, err := os.Stat(filepath.Join(dir, name, "manifest.yaml")); err != nil {
		t.Errorf("committed install manifest was removed: %v", err)
	}
	if _, found, err := ReadAdoptProvenance(name); err != nil || !found {
		t.Errorf("committed install lost recovery provenance: found=%v err=%v", found, err)
	}
	if phase, err := readProviderInstallPhase(name); err != nil || phase != providerInstallPhaseStarted {
		t.Errorf("uncertain install incorrectly settled: phase=%q err=%v", phase, err)
	}
	if !strings.Contains(string(mustReadFileForAdoptTest(t, clientPath)), "http://127.0.0.1") {
		t.Fatal("fixture did not commit the managed binding")
	}
}

func TestProviderInstallPublishedWriteFailurePreservesAdoption(t *testing.T) {
	const name = "provider-published-write-failure"
	_, dir, state, plan, provider, deps := setupProviderInstallFailureFixture(t, name)
	intentPath := filepath.Join(state, supervisorIntentFileLeaf)
	old := postRenameOpenFailHook
	cause := errors.New("injected post-publication reopen failure")
	fired := false
	postRenameOpenFailHook = func() error {
		// Preflight and client/snapshot writes pass. Refuse readback only once
		// the actual supervisor-intent file has been published.
		if _, err := os.Stat(intentPath); err == nil && !fired {
			fired = true
			return cause
		}
		return nil
	}
	t.Cleanup(func() { postRenameOpenFailHook = old })
	err := NewAPI().ExecuteAdoptWithOpts(plan, io.Discard, ExecuteAdoptOpts{providerDeps: deps})
	postRenameOpenFailHook = old
	if !fired || err == nil {
		t.Fatalf("post-publication fault was not exercised: fired=%v err=%v", fired, err)
	}
	if provider.entry.Enabled || provider.calls != 1 {
		t.Errorf("provider restored after uncertain intent publication: enabled=%v calls=%d", provider.entry.Enabled, provider.calls)
	}
	if _, err := os.Stat(filepath.Join(dir, name, "manifest.yaml")); err != nil {
		t.Errorf("uncertain intent lost its manifest: %v", err)
	}
	if _, found, err := ReadAdoptProvenance(name); err != nil || !found {
		t.Errorf("uncertain intent lost provenance: found=%v err=%v", found, err)
	}
	if phase, err := readProviderInstallPhase(name); err != nil || phase != providerInstallPhaseStarted {
		t.Errorf("uncertain publication was called settled: phase=%q err=%v", phase, err)
	}
}

func TestProviderInstallSettledHistoryWithRetainedDescriptorUsesManagedRecovery(t *testing.T) {
	const name = "provider-settled-interrupted-phase"
	dir, state, rec, provider := setupProviderPreInstallRecoveryFixture(t, name, AdoptOperationStateDeAdopting)
	// Persisted stop succeeded, but the process exited before recording the
	// separate managed_stop_settled phase. The exact descriptor still exists.
	if err := writeProviderInstallPhase(name, providerInstallPhaseManagedSettled); err != nil {
		t.Fatal(err)
	}
	task := "\\mcp-local-hub-" + name + "-" + adoptDefaultDaemonName
	intent := &SupervisorIntentFile{Version: 1, Daemons: []SupervisorDaemon{{TaskName: task, Server: name, Daemon: adoptDefaultDaemonName, Port: rec.Port, ManifestHash: rec.ExpectedManifestHash}}}
	if err := WriteSupervisorIntent(filepath.Join(state, supervisorIntentFileLeaf), intent); err != nil {
		t.Fatal(err)
	}
	plan, err := NewAPI().BuildDeAdoptPlan(name)
	if err != nil {
		t.Fatal(err)
	}
	if plan.providerRecovery {
		t.Error("settled history with a retained descriptor was misrouted to provider-only recovery")
	}
	stops := 0
	_, err = NewAPI().executeDeAdoptPlanWithOpts(plan, io.Discard, ExecuteDeAdoptOpts{providerDeps: providerTransactionDeps{source: provider, stop: func(_ context.Context, frozen SupervisorDaemon) (StoppedSettlement, error) {
		stops++
		return StoppedSettlement{TaskName: frozen.TaskName, State: StoppedSettlementStopped, Reason: StoppedSettlementReasonStopped}, nil
	}}})
	if err != nil {
		t.Fatalf("interrupted managed phase did not resume: %v", err)
	}
	if stops != 1 || !provider.entry.Enabled {
		t.Fatalf("managed retry: stops=%d provider enabled=%v", stops, provider.entry.Enabled)
	}
	assertDeAdoptClosed(t, dir, state, *rec)
}

// A pending provider receipt can also be resumed by ordinary Install, whose
// explicit project authority can select a Codex alias. An event append failure
// retains that real client transaction, so it must not become rollback evidence.
func TestProviderInstallRetainedCodexSettlementIsNotRollback(t *testing.T) {
	const name = "provider-retained-codex-settlement"
	clientPath, dir, state, adoptPlan, _, _ := setupProviderInstallFailureFixture(t, name)
	original := []byte("[mcp_servers." + name + "]\nurl = \"http://127.0.0.1:9292/mcp\"\nenabled = false\n")
	if err := os.WriteFile(clientPath, original, 0o600); err != nil {
		t.Fatal(err)
	}
	api := NewAPI()
	if _, err := api.captureAdoptProvenance(adoptPlan); err != nil {
		t.Fatal(err)
	}
	if err := api.ManifestCreate(name, adoptPlan.ManifestYAML); err != nil {
		t.Fatal(err)
	}
	projectRoot := t.TempDir()
	projectPath := filepath.Join(projectRoot, ".codex", "config.toml")
	if err := os.MkdirAll(filepath.Dir(projectPath), 0o700); err != nil {
		t.Fatal(err)
	}
	projectBytes := []byte("[mcp_servers." + name + "]\ncommand = \"go\"\nargs = [\"version\"]\n")
	if err := os.WriteFile(projectPath, projectBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	// Fail only the settlement-event sink; the separate admission audit and
	// the actual Codex transaction continue to use their production owners.
	eventPath := filepath.Join(state, SupervisorEventLogFileLeaf)
	if err := os.Remove(eventPath); err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	if err := os.Mkdir(eventPath, 0o700); err != nil {
		t.Fatal(err)
	}
	err := api.Install(InstallOpts{
		Server: name, ClientsInclude: []string{"codex-cli"}, Writer: io.Discard,
		CodexProjectRoot: projectRoot, CodexWorkingDir: projectRoot,
	})
	if !errors.Is(err, ErrClientConfigSettlementEventFailed) {
		t.Fatalf("expected retained settlement-event failure, got %v", err)
	}
	var rolledBack *providerInstallUnpublishedRollbackError
	if errors.As(err, &rolledBack) {
		t.Error("retained Codex client mutation was certified as unpublished rollback")
	}
	client := clients.AllClients()["codex-cli"]
	alias, readErr := client.GetEntry(name + "-mcphub")
	if readErr != nil || alias == nil || alias.URL == "" {
		t.Fatalf("real retained alias missing: entry=%+v err=%v", alias, readErr)
	}
	if source, readErr := client.GetEntry(name); readErr != nil || source != nil {
		t.Fatalf("logical source was not relocated: entry=%+v err=%v", source, readErr)
	}
	if got := string(mustReadFileForAdoptTest(t, projectPath)); got != string(projectBytes) {
		t.Error("read-only project layer was changed")
	}
	if phase, readErr := readProviderInstallPhase(name); readErr != nil || phase != providerInstallPhaseStarted {
		t.Errorf("retained client mutation lost started history: phase=%q err=%v", phase, readErr)
	}
	if _, statErr := os.Stat(filepath.Join(state, supervisorIntentFileLeaf)); !os.IsNotExist(statErr) {
		t.Errorf("test did not isolate the pre-intent error: %v", statErr)
	}
	if _, statErr := os.Stat(filepath.Join(dir, name, "manifest.yaml")); statErr != nil {
		t.Errorf("retained mutation lost manifest: %v", statErr)
	}
	if _, found, readErr := ReadAdoptProvenance(name); readErr != nil || !found {
		t.Errorf("retained mutation lost provenance: found=%v err=%v", found, readErr)
	}
	snapshotPath := filepath.Join(state, adoptProvenanceSnapshotSubdir, name, "codex-cli"+adoptSnapshotFileSuffix)
	if got := string(mustReadFileForAdoptTest(t, snapshotPath)); got != string(original) {
		t.Error("retained mutation lost its exact pre-install recovery snapshot")
	}
}
