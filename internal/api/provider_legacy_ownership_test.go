package api

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestExecuteDeAdoptProviderPreInstallLegacyBlankServerRowFailsClosed(t *testing.T) {
	name := "provider-preinstall-legacy-blank-server"
	manifestRoot, stateRoot, rec, provider := setupProviderPreInstallRecoveryFixture(t, name, AdoptOperationStateAdopting)
	intent := &SupervisorIntentFile{Version: 1, Daemons: []SupervisorDaemon{{
		TaskName:     "\\mcp-local-hub-" + name + "-" + adoptDefaultDaemonName,
		Port:         rec.Port,
		ManifestHash: rec.ExpectedManifestHash,
	}}}
	if err := WriteSupervisorIntent(filepath.Join(stateRoot, supervisorIntentFileLeaf), intent); err != nil {
		t.Fatal(err)
	}

	plan, err := NewAPI().BuildDeAdoptPlan(name)
	if err != nil {
		t.Fatal(err)
	}
	_, err = NewAPI().executeDeAdoptPlanWithOpts(plan, io.Discard, ExecuteDeAdoptOpts{
		providerDeps: providerTransactionDeps{source: provider},
	})
	if err == nil || !strings.Contains(err.Error(), "E_PROVIDER_LIFECYCLE_UNSUPPORTED") {
		t.Fatalf("legacy blank-server ownership error=%v, want fail-closed lifecycle refusal", err)
	}
	if provider.calls != 0 || provider.entry.Enabled {
		t.Fatalf("provider was restored despite legacy owned daemon: provider=%+v calls=%d", provider.entry, provider.calls)
	}
	if _, statErr := os.Stat(filepath.Join(manifestRoot, name, "manifest.yaml")); statErr != nil {
		t.Fatalf("manifest was removed despite unresolved legacy daemon ownership: %v", statErr)
	}
	persisted, found, readErr := ReadAdoptProvenance(name)
	if readErr != nil || !found || persisted.OperationState != AdoptOperationStateAdopting || persisted.ProviderSource == nil || persisted.ProviderSource.DeAdoptPhase != "" {
		t.Fatalf("legacy ownership mutated provenance before refusal: row=%+v found=%t err=%v", persisted, found, readErr)
	}
}
