package api

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBuildDeAdoptPlanProviderPreManifestRecoveryPrecedesClientProbe(t *testing.T) {
	name := "provider-pre-manifest-before-client-probe"
	manifestRoot, _, rec, _ := setupProviderPreInstallRecoveryFixture(t, name, AdoptOperationStateAdopting)
	if err := os.Remove(filepath.Join(manifestRoot, name, "manifest.yaml")); err != nil {
		t.Fatalf("remove manifest: %v", err)
	}
	// A pre-ManifestCreate provider crash has no client-side Install commit to
	// classify. Make the recorded adopt client intentionally unavailable so this
	// regression proves provider recovery is selected before fallible client probes.
	rec.AdoptClients = []string{"provider-client-probe-unavailable"}
	rec.Clients = nil
	writeDeAdoptExecutorRecord(t, rec)

	plan, err := NewAPI().BuildDeAdoptPlan(name)
	if err != nil {
		t.Fatalf("BuildDeAdoptPlan: %v", err)
	}
	if plan.Routing != DeAdoptRoutingFresh || !plan.providerRecovery {
		t.Fatalf("plan=%+v, want explicit provider recovery before client probes", plan)
	}
}

func TestExecuteDeAdoptProviderSettledAbsenceRejectsRowAppearingBeforeE4(t *testing.T) {
	name := "provider-settled-absence-e4-race"
	manifestRoot, stateRoot, rec, provider := setupProviderPreInstallRecoveryFixture(t, name, AdoptOperationStateAdopting)
	plan, err := NewAPI().BuildDeAdoptPlan(name)
	if err != nil {
		t.Fatal(err)
	}

	previous := deAdoptBeforeManifestDeleteHook
	deAdoptBeforeManifestDeleteHook = func() {
		intent := &SupervisorIntentFile{Version: 1, Daemons: []SupervisorDaemon{{
			TaskName:     "\\mcp-local-hub-" + name + "-" + adoptDefaultDaemonName,
			Server:       name,
			Daemon:       adoptDefaultDaemonName,
			Port:         rec.Port,
			ManifestHash: rec.ExpectedManifestHash,
		}}
		if writeErr := WriteSupervisorIntent(filepath.Join(stateRoot, supervisorIntentFileLeaf), intent); writeErr != nil {
			t.Fatalf("inject supervisor row: %v", writeErr)
		}
	}
	t.Cleanup(func() { deAdoptBeforeManifestDeleteHook = previous })

	_, err = NewAPI().executeDeAdoptPlanWithOpts(plan, io.Discard, ExecuteDeAdoptOpts{
		providerDeps: providerTransactionDeps{
			source: provider,
			stop: func(context.Context, SupervisorDaemon) (StoppedSettlement, error) {
				t.Fatal("settled-absence path must reject a later row rather than silently treating it as already stopped")
				return StoppedSettlement{}, nil
			},
		},
	})
	if err == nil || !strings.Contains(err.Error(), "E_PROVIDER_LIFECYCLE_UNSUPPORTED") {
		t.Fatalf("error=%v, want fail-closed lifecycle refusal", err)
	}
	if provider.calls != 0 || provider.entry.Enabled {
		t.Fatalf("provider restored after E4 ownership race: provider=%+v calls=%d", provider.entry, provider.calls)
	}
	if _, statErr := os.Stat(filepath.Join(manifestRoot, name, "manifest.yaml")); statErr != nil {
		t.Fatalf("manifest removed after E4 ownership race: %v", statErr)
	}
}
