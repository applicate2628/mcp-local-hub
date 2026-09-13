package api

import (
	"os"
	"path/filepath"
	"testing"

	"mcp-local-hub/internal/clients"
)

func TestProviderRestoreGateRejectsReinstalledHubBinding(t *testing.T) {
	name := "provider-restore-reinstall-race"
	manifestRoot, _, rec, _ := setupProviderPreInstallRecoveryFixture(t, name, AdoptOperationStateDeAdopting)
	if err := os.Remove(filepath.Join(manifestRoot, name, "manifest.yaml")); err != nil {
		t.Fatal(err)
	}
	rec.ProviderSource.DeAdoptPhase = "managed_removed"
	writeDeAdoptExecutorRecord(t, *rec)
	if err := writeProviderInstallPhase(name, providerInstallPhaseManagedSettled); err != nil {
		t.Fatal(err)
	}
	adapter := clients.AllClients()["codex-cli"]
	if adapter == nil {
		t.Fatal("codex-cli adapter unavailable")
	}
	if err := os.WriteFile(adapter.ConfigPath(), []byte(deAdoptHubConfig(name)), 0o600); err != nil {
		t.Fatalf("inject reinstalled hub binding: %v", err)
	}

	if providerInstallPhaseAllowsRestore(name) {
		t.Fatal("provider restore accepted a hub binding recreated after E3")
	}
}
