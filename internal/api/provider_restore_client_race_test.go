package api

import (
	"io"
	"os"
	"path/filepath"
	"testing"

	"mcp-local-hub/internal/clients"
)

func TestProviderRestoreGateRejectsReinstalledHubBinding(t *testing.T) {
	for _, tc := range []struct {
		name      string
		alias     bool
		recreated bool
	}{
		{name: "legacy source", recreated: true},
		{name: "recreated alias", alias: true, recreated: true},
		{name: "removed alias", alias: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			name := "provider-restore-target-key"
			manifestRoot, _, rec, _ := setupProviderPreInstallRecoveryFixture(t, name, AdoptOperationStateDeAdopting)
			if err := os.Remove(filepath.Join(manifestRoot, name, "manifest.yaml")); err != nil {
				t.Fatal(err)
			}
			if len(rec.Clients) != 1 || rec.Clients[0].Client != "codex-cli" {
				t.Fatalf("unexpected fixture client provenance: %+v", rec.Clients)
			}
			target := rec.SourceEntryName
			if tc.alias {
				target += "-http"
				rec.Clients[0].TargetEntryName = target
			}
			rec.Clients[0].ToolTimeoutSec = 30
			rec.ProviderSource.DeAdoptPhase = "managed_removed"
			writeDeAdoptExecutorRecord(t, *rec)
			if err := writeProviderInstallPhase(name, providerInstallPhaseManagedSettled); err != nil {
				t.Fatal(err)
			}
			config := deAdoptNativeConfig(rec.SourceEntryName, "native-command")
			if tc.recreated {
				config = deAdoptHubConfigWithToolTimeout(target, 30)
				if tc.alias {
					config += deAdoptNativeConfig(rec.SourceEntryName, "native-command")
				}
			}
			adapter := clients.AllClients()["codex-cli"]
			if adapter == nil {
				t.Fatal("codex-cli adapter unavailable")
			}
			if err := os.WriteFile(adapter.ConfigPath(), []byte(config), 0o600); err != nil {
				t.Fatal(err)
			}
			if allowed := providerInstallPhaseAllowsRestore(name); allowed == tc.recreated {
				t.Fatalf("restore allowed=%t for target %q, recreated=%t", allowed, target, tc.recreated)
			}
			if got := mustReadFileForAdoptTest(t, adapter.ConfigPath()); string(got) != config {
				t.Fatal("read-only restore guard changed the client configuration")
			}
		})
	}
}

func TestProviderPreInstallRecoveryCompletesWithoutClientAdapter(t *testing.T) {
	for _, manifestPresent := range []bool{false, true} {
		label := "before-manifest"
		if manifestPresent {
			label = "after-manifest"
		}
		t.Run(label, func(t *testing.T) {
			name := "provider-recovery-unavailable-client"
			manifestRoot, stateRoot, rec, provider := setupProviderPreInstallRecoveryFixture(t, name, AdoptOperationStateAdopting)
			if !manifestPresent {
				if err := os.Remove(filepath.Join(manifestRoot, name, "manifest.yaml")); err != nil {
					t.Fatal(err)
				}
			}
			// Install never crossed its mutation barrier, so this recorded
			// client's availability must not block provider-only recovery.
			rec.AdoptClients = []string{"provider-client-probe-unavailable"}
			rec.Clients = nil
			writeDeAdoptExecutorRecord(t, *rec)
			plan, err := NewAPI().BuildDeAdoptPlan(name)
			if err != nil {
				t.Fatal(err)
			}
			if !plan.providerRecovery {
				t.Fatalf("durable pre-Install recovery was not selected: %+v", plan)
			}
			if _, err := NewAPI().executeDeAdoptPlanWithOpts(plan, io.Discard, ExecuteDeAdoptOpts{providerDeps: providerTransactionDeps{source: provider}}); err != nil {
				t.Fatalf("provider recovery depended on an untouched client: %v", err)
			}
			if provider.calls != 1 || !provider.entry.Enabled {
				t.Fatalf("restore calls=%d enabled=%t", provider.calls, provider.entry.Enabled)
			}
			assertDeAdoptClosed(t, manifestRoot, stateRoot, *rec)
		})
	}
}

func TestProviderManagedSettlementStillRequiresReadableClient(t *testing.T) {
	name := "provider-settled-unavailable-client"
	manifestRoot, _, rec, _ := setupProviderPreInstallRecoveryFixture(t, name, AdoptOperationStateDeAdopting)
	if err := os.Remove(filepath.Join(manifestRoot, name, "manifest.yaml")); err != nil {
		t.Fatal(err)
	}
	rec.AdoptClients = []string{"provider-client-probe-unavailable"}
	rec.Clients = nil
	rec.ProviderSource.DeAdoptPhase = "managed_removed"
	writeDeAdoptExecutorRecord(t, *rec)
	if err := writeProviderInstallPhase(name, providerInstallPhaseManagedSettled); err != nil {
		t.Fatal(err)
	}
	if providerInstallPhaseAllowsRestore(name) {
		t.Fatal("managed settlement bypassed the unreadable-client guard")
	}
}

func TestProviderRecoveryExecutableRequiresPositiveManifestProof(t *testing.T) {
	for _, tc := range []struct {
		name      string
		readiness DeAdoptManifestReadiness
		want      bool
	}{
		{name: "unknown", readiness: DeAdoptManifestReadiness{}},
		{name: "unreadable", readiness: DeAdoptManifestReadiness{Reason: "manifest could not be read"}},
		{name: "hash mismatch", readiness: DeAdoptManifestReadiness{Present: true}},
		{name: "absent", readiness: DeAdoptManifestReadiness{AlreadyAbsent: true}, want: true},
		{name: "verified", readiness: DeAdoptManifestReadiness{Present: true, HashReady: true}, want: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			plan := &DeAdoptPlan{Routing: DeAdoptRoutingFresh, providerRecovery: true, Manifest: tc.readiness}
			if got := plan.ProviderRecoveryExecutable(); got != tc.want {
				t.Fatalf("executable=%t, want %t for readiness %+v", got, tc.want, tc.readiness)
			}
		})
	}
}

func TestProviderClaimedRestoreRequiresAbsentManifest(t *testing.T) {
	for _, tc := range []struct {
		name    string
		exists  bool
		err     error
		allowed bool
	}{
		{name: "absent", allowed: true},
		{name: "recreated", exists: true},
		{name: "unreadable", err: os.ErrPermission},
	} {
		t.Run(tc.name, func(t *testing.T) {
			name := "provider-claimed-restore-manifest"
			_, _, rec, _ := setupProviderPreInstallRecoveryFixture(t, name, AdoptOperationStateDeAdopting)
			rec.ProviderSource.DeAdoptPhase = "managed_removed"
			rec.AdoptClients = []string{"provider-client-probe-unavailable"}
			rec.Clients = nil
			writeDeAdoptExecutorRecord(t, *rec)
			if err := writeProviderInstallPhase(name, providerInstallPhaseRecoveryClaimed); err != nil {
				t.Fatal(err)
			}
			previous := adoptManifestExistsFn
			adoptManifestExistsFn = func(string) (bool, error) { return tc.exists, tc.err }
			t.Cleanup(func() { adoptManifestExistsFn = previous })
			if allowed := providerRestoreBindingsClear(name); allowed != tc.allowed {
				t.Fatalf("claimed restore allowed=%t, want %t", allowed, tc.allowed)
			}
		})
	}
}
