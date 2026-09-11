package api

import (
	"io"
	"os"
	"path/filepath"
	"testing"

	"mcp-local-hub/internal/clients"
)

func setupProviderPreInstallRecoveryFixture(t *testing.T, name string, state AdoptOperationState) (string, string, *AdoptProvenanceRecord, *providerLifecycleFake) {
	t.Helper()
	_, manifestRoot, stateRoot, rec := setupDeAdoptPlannerFixture(t, name, deAdoptPlannerFixture{
		state: state, originalState: AdoptOriginalStateAbsent,
		liveConfig: "[mcp_servers]\n", manifestPresent: true,
	})
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cwd := t.TempDir()
	rec.ProviderSource = &ProviderSourceProvenanceV1{
		ProviderClient: "codex-cli", PluginRef: "fixture@catalog", ServerName: name,
		Scope: "user", ReceiptFingerprint: "receipt", ActivationFingerprint: "activation",
		PolicyFingerprint: "policy", PriorEnabledPresent: true, PriorEnabled: true,
		ExpectedDisabledFingerprint: "disabled", DisablePhase: "disable_applied",
	}
	writeDeAdoptExecutorRecord(t, rec)
	provider := &providerLifecycleFake{entry: clients.ProviderMCPEntryV1{
		ProviderClient: "codex-cli", PluginRef: "fixture@catalog", ServerName: name,
		Transport: clients.ProviderMCPTransportStdio, Command: exe, WorkingDir: &cwd,
		Scope: clients.ProviderMCPScopeUser, Enabled: false, ReceiptFingerprint: "receipt",
		ActivationFingerprint: "disabled", ActivationEnabledPresent: true,
		ActivationEnabled: false, DisabledActivationFingerprint: "disabled",
		PolicyState: clients.ProviderMCPPolicyNone, PolicyFingerprint: "policy",
	}}
	return manifestRoot, stateRoot, rec, provider
}

func TestExecuteDeAdoptProviderPreInstallMissingSupervisorIntent(t *testing.T) {
	name := "provider-preinstall-intent-absent"
	manifestRoot, stateRoot, rec, provider := setupProviderPreInstallRecoveryFixture(t, name, AdoptOperationStateAdopting)
	if _, err := os.Stat(filepath.Join(stateRoot, supervisorIntentFileLeaf)); !os.IsNotExist(err) {
		t.Fatalf("supervisor intent must be genuinely absent, stat err=%v", err)
	}

	plan, err := NewAPI().BuildDeAdoptPlan(name)
	if err != nil || plan.Routing != DeAdoptRoutingFresh || plan.providerRecovery {
		t.Fatalf("pre-install plan=%+v err=%v, want ordinary fresh teardown", plan, err)
	}
	if _, err := NewAPI().executeDeAdoptPlanWithOpts(plan, io.Discard, ExecuteDeAdoptOpts{
		providerDeps: providerTransactionDeps{source: provider},
	}); err != nil {
		t.Fatalf("pre-install recovery apply: %v", err)
	}
	if provider.calls != 1 || !provider.entry.Enabled {
		t.Fatalf("pre-install recovery provider=%+v calls=%d", provider.entry, provider.calls)
	}
	assertDeAdoptClosed(t, manifestRoot, stateRoot, rec)
}

func TestExecuteDeAdoptProviderPreInstallAbsenceIsReprovedAfterCrashAndRetry(t *testing.T) {
	name := "provider-preinstall-crash-retry"
	manifestRoot, stateRoot, rec, provider := setupProviderPreInstallRecoveryFixture(t, name, AdoptOperationStateAdopting)
	if _, err := os.Stat(filepath.Join(stateRoot, supervisorIntentFileLeaf)); !os.IsNotExist(err) {
		t.Fatalf("supervisor intent must be genuinely absent, stat err=%v", err)
	}

	plan, err := NewAPI().BuildDeAdoptPlan(name)
	if err != nil {
		t.Fatal(err)
	}
	const crash = "injected pre-E4 crash"
	deAdoptBeforeManifestDeleteHook = func() { panic(crash) }
	func() {
		defer func() {
			if got := recover(); got != crash {
				t.Fatalf("recovered panic=%v, want %q", got, crash)
			}
		}()
		_, _ = NewAPI().executeDeAdoptPlanWithOpts(plan, io.Discard, ExecuteDeAdoptOpts{
			providerDeps: providerTransactionDeps{source: provider},
		})
	}()
	deAdoptBeforeManifestDeleteHook = nil

	persisted, found, err := ReadAdoptProvenance(name)
	if err != nil || !found {
		t.Fatalf("ReadAdoptProvenance: found=%t err=%v", found, err)
	}
	if persisted.OperationState != AdoptOperationStateDeAdopting || persisted.ProviderSource == nil || persisted.ProviderSource.DeAdoptPhase != "" {
		t.Fatalf("crash state=%+v, want de_adopting with empty phase so retry must re-prove absence", persisted)
	}

	retry, err := NewAPI().BuildDeAdoptPlan(name)
	if err != nil || retry.Routing != DeAdoptRoutingResume {
		t.Fatalf("retry plan=%+v err=%v", retry, err)
	}
	if _, err := NewAPI().executeDeAdoptPlanWithOpts(retry, io.Discard, ExecuteDeAdoptOpts{
		providerDeps: providerTransactionDeps{source: provider},
	}); err != nil {
		t.Fatalf("retry apply: %v", err)
	}
	if provider.calls != 1 || !provider.entry.Enabled {
		t.Fatalf("retry provider=%+v calls=%d", provider.entry, provider.calls)
	}
	assertDeAdoptClosed(t, manifestRoot, stateRoot, rec)
}

func TestExecuteDeAdoptProviderLegacyDeAdoptingPreInstallRecovers(t *testing.T) {
	name := "provider-preinstall-legacy-deadopting"
	manifestRoot, stateRoot, rec, provider := setupProviderPreInstallRecoveryFixture(t, name, AdoptOperationStateDeAdopting)
	if rec.ProviderSource == nil || rec.ProviderSource.DeAdoptPhase != "" {
		t.Fatalf("fixture must model legacy crash-after-E2 state: %+v", rec)
	}
	if _, err := os.Stat(filepath.Join(stateRoot, supervisorIntentFileLeaf)); !os.IsNotExist(err) {
		t.Fatalf("supervisor intent must be genuinely absent, stat err=%v", err)
	}

	plan, err := NewAPI().BuildDeAdoptPlan(name)
	if err != nil || plan.Routing != DeAdoptRoutingResume || plan.providerRecovery {
		t.Fatalf("legacy pre-install plan=%+v err=%v, want ordinary resume", plan, err)
	}
	if _, err := NewAPI().executeDeAdoptPlanWithOpts(plan, io.Discard, ExecuteDeAdoptOpts{
		providerDeps: providerTransactionDeps{source: provider},
	}); err != nil {
		t.Fatalf("legacy pre-install recovery apply: %v", err)
	}
	if provider.calls != 1 || !provider.entry.Enabled {
		t.Fatalf("legacy recovery provider=%+v calls=%d", provider.entry, provider.calls)
	}
	assertDeAdoptClosed(t, manifestRoot, stateRoot, rec)
}
