package api

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
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
	if err := initializeProviderInstallPhase(name); err != nil {
		t.Fatalf("initialize provider install phase: %v", err)
	}
	provider := &providerLifecycleFake{entry: clients.ProviderMCPEntryV1{
		ProviderClient: "codex-cli", PluginRef: "fixture@catalog", ServerName: name,
		Transport: clients.ProviderMCPTransportStdio, Command: exe, WorkingDir: &cwd,
		Scope: clients.ProviderMCPScopeUser, Enabled: false, ReceiptFingerprint: "receipt",
		ActivationFingerprint: "disabled", ActivationEnabledPresent: true,
		ActivationEnabled: false, DisabledActivationFingerprint: "disabled",
		PolicyState: clients.ProviderMCPPolicyNone, PolicyFingerprint: "policy",
	}}
	return manifestRoot, stateRoot, &rec, provider
}

func TestExecuteDeAdoptProviderPreInstallMissingSupervisorIntent(t *testing.T) {
	name := "provider-preinstall-intent-absent"
	manifestRoot, stateRoot, rec, provider := setupProviderPreInstallRecoveryFixture(t, name, AdoptOperationStateAdopting)
	if _, err := os.Stat(filepath.Join(stateRoot, supervisorIntentFileLeaf)); !os.IsNotExist(err) {
		t.Fatalf("supervisor intent must be genuinely absent, stat err=%v", err)
	}
	plan, err := NewAPI().BuildDeAdoptPlan(name)
	if err != nil || plan.Routing != DeAdoptRoutingFresh || !plan.providerRecovery {
		t.Fatalf("pre-install plan=%+v err=%v, want explicit durable recovery", plan, err)
	}
	wire, err := json.Marshal(plan)
	if err != nil {
		t.Fatalf("marshal recovery plan: %v", err)
	}
	var public map[string]any
	if err := json.Unmarshal(wire, &public); err != nil {
		t.Fatalf("decode recovery plan: %v", err)
	}
	if public["ProviderRecoveryReady"] != true {
		t.Fatalf("recovery plan wire ProviderRecoveryReady=%#v, want true; wire=%s", public["ProviderRecoveryReady"], wire)
	}
	if _, err := NewAPI().executeDeAdoptPlanWithOpts(plan, io.Discard, ExecuteDeAdoptOpts{providerDeps: providerTransactionDeps{source: provider}}); err != nil {
		t.Fatalf("pre-install recovery apply: %v", err)
	}
	if provider.calls != 1 || !provider.entry.Enabled {
		t.Fatalf("pre-install recovery provider=%+v calls=%d", provider.entry, provider.calls)
	}
	assertDeAdoptClosed(t, manifestRoot, stateRoot, *rec)
}

func TestExecuteDeAdoptProviderPreInstallAbsenceIsReprovedAfterCrashAndRetry(t *testing.T) {
	name := "provider-preinstall-crash-retry"
	manifestRoot, stateRoot, rec, provider := setupProviderPreInstallRecoveryFixture(t, name, AdoptOperationStateAdopting)
	if _, err := os.Stat(filepath.Join(stateRoot, supervisorIntentFileLeaf)); !os.IsNotExist(err) {
		t.Fatalf("supervisor intent must be genuinely absent, stat err=%v", err)
	}
	plan, err := NewAPI().BuildDeAdoptPlan(name)
	if err != nil || !plan.providerRecovery {
		t.Fatalf("pre-install recovery plan=%+v err=%v", plan, err)
	}
	previous := deAdoptAfterManifestDeleteHook
	t.Cleanup(func() { deAdoptAfterManifestDeleteHook = previous })
	const crash = "injected post-delete pre-E2 crash"
	deAdoptAfterManifestDeleteHook = func() { panic(crash) }
	func() {
		defer func() {
			if got := recover(); got != crash {
				t.Fatalf("recovered panic=%v, want %q", got, crash)
			}
		}()
		_, _ = NewAPI().executeDeAdoptPlanWithOpts(plan, io.Discard, ExecuteDeAdoptOpts{providerDeps: providerTransactionDeps{source: provider}})
	}()
	deAdoptAfterManifestDeleteHook = previous
	persisted, found, err := ReadAdoptProvenance(name)
	if err != nil || !found {
		t.Fatalf("ReadAdoptProvenance: found=%t err=%v", found, err)
	}
	if persisted.OperationState != AdoptOperationStateAdopting || persisted.ProviderSource == nil || persisted.ProviderSource.DeAdoptPhase != "" {
		t.Fatalf("crash state=%+v, want adopting with empty de-adopt phase", persisted)
	}
	phase, err := readProviderInstallPhase(name)
	if err != nil || phase != providerInstallPhaseRecoveryClaimed {
		t.Fatalf("crash lost durable recovery claim: phase=%q err=%v", phase, err)
	}
	if _, statErr := os.Stat(filepath.Join(manifestRoot, name, "manifest.yaml")); !os.IsNotExist(statErr) {
		t.Fatalf("exact manifest should already be deleted at crash boundary: %v", statErr)
	}
	retry, err := NewAPI().BuildDeAdoptPlan(name)
	if err != nil || retry.Routing != DeAdoptRoutingFresh || !retry.providerRecovery {
		t.Fatalf("retry plan=%+v err=%v", retry, err)
	}
	if _, err := NewAPI().executeDeAdoptPlanWithOpts(retry, io.Discard, ExecuteDeAdoptOpts{providerDeps: providerTransactionDeps{source: provider}}); err != nil {
		t.Fatalf("retry apply: %v", err)
	}
	if provider.calls != 1 || !provider.entry.Enabled {
		t.Fatalf("retry provider=%+v calls=%d", provider.entry, provider.calls)
	}
	assertDeAdoptClosed(t, manifestRoot, stateRoot, *rec)
}

func TestExecuteDeAdoptProviderDeAdoptingNotStartedRecovers(t *testing.T) {
	name := "provider-preinstall-deadopting"
	manifestRoot, stateRoot, rec, provider := setupProviderPreInstallRecoveryFixture(t, name, AdoptOperationStateDeAdopting)
	if rec.ProviderSource == nil || rec.ProviderSource.DeAdoptPhase != "" {
		t.Fatalf("fixture must model crash-after-E2 state: %+v", rec)
	}
	if _, err := os.Stat(filepath.Join(stateRoot, supervisorIntentFileLeaf)); !os.IsNotExist(err) {
		t.Fatalf("supervisor intent must be genuinely absent, stat err=%v", err)
	}
	plan, err := NewAPI().BuildDeAdoptPlan(name)
	if err != nil || plan.Routing != DeAdoptRoutingResume || !plan.providerRecovery {
		t.Fatalf("pre-install plan=%+v err=%v, want durable recovery resume", plan, err)
	}
	if _, err := NewAPI().executeDeAdoptPlanWithOpts(plan, io.Discard, ExecuteDeAdoptOpts{providerDeps: providerTransactionDeps{source: provider}}); err != nil {
		t.Fatalf("pre-install recovery apply: %v", err)
	}
	if provider.calls != 1 || !provider.entry.Enabled {
		t.Fatalf("recovery provider=%+v calls=%d", provider.entry, provider.calls)
	}
	assertDeAdoptClosed(t, manifestRoot, stateRoot, *rec)
}

func TestExecuteDeAdoptProviderStartedInstallCannotBecomePreInstallByDeletingIntent(t *testing.T) {
	name := "provider-started-intent-lost"
	_, stateRoot, _, provider := setupProviderPreInstallRecoveryFixture(t, name, AdoptOperationStateAdopting)
	if err := markProviderInstallStartedForTask(name, "mcp-local-hub-"+name+"-"+adoptDefaultDaemonName); err != nil {
		t.Fatalf("mark provider install started: %v", err)
	}
	if _, err := os.Stat(filepath.Join(stateRoot, supervisorIntentFileLeaf)); !os.IsNotExist(err) {
		t.Fatalf("supervisor intent must be genuinely absent, stat err=%v", err)
	}
	plan, err := NewAPI().BuildDeAdoptPlan(name)
	if err != nil {
		t.Fatal(err)
	}
	_, err = NewAPI().executeDeAdoptPlanWithOpts(plan, io.Discard, ExecuteDeAdoptOpts{providerDeps: providerTransactionDeps{source: provider}})
	if err == nil || !strings.Contains(err.Error(), "E_PROVIDER_LIFECYCLE_UNSUPPORTED") {
		t.Fatalf("error=%v, want durable started-install refusal", err)
	}
	if provider.calls != 0 || provider.entry.Enabled {
		t.Fatalf("provider restored after started Install lost its descriptor: provider=%+v calls=%d", provider.entry, provider.calls)
	}
}

func TestExecuteDeAdoptProviderLegacyMissingInstallMarkerFailsClosed(t *testing.T) {
	name := "provider-legacy-marker-missing"
	_, stateRoot, rec, provider := setupProviderPreInstallRecoveryFixture(t, name, AdoptOperationStateAdopting)
	marker, err := providerInstallPhasePath(name)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(marker); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(stateRoot, supervisorIntentFileLeaf)); !os.IsNotExist(err) {
		t.Fatalf("supervisor intent must be genuinely absent, stat err=%v", err)
	}
	plan, err := NewAPI().BuildDeAdoptPlan(name)
	if err != nil {
		t.Fatal(err)
	}
	_, err = NewAPI().executeDeAdoptPlanWithOpts(plan, io.Discard, ExecuteDeAdoptOpts{providerDeps: providerTransactionDeps{source: provider}})
	if err == nil || !strings.Contains(err.Error(), "E_PROVIDER_LIFECYCLE_UNSUPPORTED") {
		t.Fatalf("error=%v, want missing-marker fail-closed refusal; rec=%+v", err, rec)
	}
	if provider.calls != 0 || provider.entry.Enabled {
		t.Fatalf("provider restored from synthetic legacy history: provider=%+v calls=%d", provider.entry, provider.calls)
	}
}

func TestProviderRecoveryPlanWireReadiness(t *testing.T) {
	for _, state := range []string{"absent", "verified", "changed", "unreadable"} {
		t.Run(state, func(t *testing.T) {
			name := "provider-plan-wire"
			root, _, _, _ := setupProviderPreInstallRecoveryFixture(t, name, AdoptOperationStateAdopting)
			path := filepath.Join(root, name, "manifest.yaml")
			switch state {
			case "absent", "unreadable":
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
				if state == "unreadable" {
					if err := os.Mkdir(path, 0o700); err != nil {
						t.Fatal(err)
					}
				}
			case "changed":
				raw, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, append(raw, []byte("\n# external edit\n")...), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			plan, err := NewAPI().BuildDeAdoptPlan(name)
			if err != nil {
				t.Fatal(err)
			}
			raw, err := json.Marshal(plan)
			if err != nil {
				t.Fatal(err)
			}
			var public map[string]any
			if err := json.Unmarshal(raw, &public); err != nil {
				t.Fatal(err)
			}
			if _, ok := public["Clients"].([]any); !ok {
				t.Fatalf("wire client dispositions must be an array: %s", raw)
			}
			want := state == "absent" || state == "verified"
			if public["ProviderRecoveryReady"] != want {
				t.Fatalf("wire recovery readiness=%v, want %v; plan=%s", public["ProviderRecoveryReady"], want, raw)
			}
			if state == "absent" && (!plan.Manifest.AlreadyAbsent || plan.Manifest.HashReady) {
				t.Fatalf("pre-manifest readiness=%+v", plan.Manifest)
			}
			phase, err := readProviderInstallPhase(name)
			if err != nil || phase != providerInstallPhaseNotStarted {
				t.Fatalf("planning changed durable phase: %q, %v", phase, err)
			}
		})
	}
}
