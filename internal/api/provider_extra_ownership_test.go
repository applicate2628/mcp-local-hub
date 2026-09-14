package api

import (
	"io"
	"path/filepath"
	"strings"
	"testing"
)

func TestExecuteDeAdoptProviderRejectsExtraOwnedSupervisorRow(t *testing.T) {
	name := "provider-extra-owned-row"
	manifestRoot, stateRoot, rec, provider := setupProviderPreInstallRecoveryFixture(t, name, AdoptOperationStateAdopting)
	intent := &SupervisorIntentFile{Version: 1, Daemons: []SupervisorDaemon{
		{
			TaskName:     "\\mcp-local-hub-" + name + "-" + adoptDefaultDaemonName,
			Server:       name,
			Daemon:       adoptDefaultDaemonName,
			Port:         rec.Port,
			ManifestHash: rec.ExpectedManifestHash,
		},
		{
			TaskName:     "\\mcp-local-hub-" + name + "-stale",
			Port:         rec.Port + 1,
			ManifestHash: rec.ExpectedManifestHash,
		},
	}}
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
		t.Fatalf("extra owned row error=%v, want fail-closed lifecycle refusal", err)
	}
	if provider.calls != 0 || provider.entry.Enabled {
		t.Fatalf("provider was restored despite extra owned row: provider=%+v calls=%d", provider.entry, provider.calls)
	}
	if _, _, statErr := NewAPI().ManifestGetInWithHash(manifestRoot, name); statErr != nil {
		t.Fatalf("manifest was removed despite unresolved extra ownership: %v", statErr)
	}
	persisted, found, readErr := ReadAdoptProvenance(name)
	if readErr != nil || !found || persisted.ProviderSource == nil || persisted.ProviderSource.DeAdoptPhase != "" {
		t.Fatalf("extra ownership mutated provider phase before refusal: row=%+v found=%t err=%v", persisted, found, readErr)
	}
}
