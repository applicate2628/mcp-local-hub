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
