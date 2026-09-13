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

func TestExecuteDeAdoptProviderPreInstallRowAppearingBeforeE4IsStopped(t *testing.T) {
	name := "provider-preinstall-e4-row"
	manifestRoot, stateRoot, rec, provider := setupProviderPreInstallRecoveryFixture(t, name, AdoptOperationStateAdopting)
	// Model an Install admitted before E2, not a daemon with never-started history.
	if err := markProviderInstallStartedForTask("mcp-local-hub-" + name + "-" + adoptDefaultDaemonName); err != nil {
		t.Fatal(err)
	}
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
		}}}
		if writeErr := WriteSupervisorIntent(filepath.Join(stateRoot, supervisorIntentFileLeaf), intent); writeErr != nil {
			t.Fatalf("inject supervisor row: %v", writeErr)
		}
	}
	t.Cleanup(func() { deAdoptBeforeManifestDeleteHook = previous })

	stops := 0
	_, err = NewAPI().executeDeAdoptPlanWithOpts(plan, io.Discard, ExecuteDeAdoptOpts{
		providerDeps: providerTransactionDeps{
			source: provider,
			stop: func(_ context.Context, frozen SupervisorDaemon) (StoppedSettlement, error) {
				stops++
				if frozen.Server != name || frozen.Daemon != adoptDefaultDaemonName || frozen.Port != rec.Port {
					t.Fatalf("unexpected frozen row: %+v", frozen)
				}
				return StoppedSettlement{TaskName: frozen.TaskName, State: StoppedSettlementStopped, Reason: StoppedSettlementReasonStopped}, nil
			},
		},
	})
	if err != nil {
		t.Fatalf("late pre-E4 row should be settled, not mistaken for historical absence: %v", err)
	}
	if stops != 1 || provider.calls != 1 || !provider.entry.Enabled {
		t.Fatalf("stops=%d provider=%+v calls=%d", stops, provider.entry, provider.calls)
	}
	assertDeAdoptClosed(t, manifestRoot, stateRoot, rec)
}

func TestExecuteDeAdoptProviderPreInstallRowAfterManifestDeleteIsPreserved(t *testing.T) {
	name := "provider-preinstall-post-delete-row"
	manifestRoot, stateRoot, rec, provider := setupProviderPreInstallRecoveryFixture(t, name, AdoptOperationStateAdopting)
	plan, err := NewAPI().BuildDeAdoptPlan(name)
	if err != nil {
		t.Fatal(err)
	}

	previous := deAdoptAfterManifestDeleteHook
	deAdoptAfterManifestDeleteHook = func() {
		intent := &SupervisorIntentFile{Version: 1, Daemons: []SupervisorDaemon{{
			TaskName:     "\\mcp-local-hub-" + name + "-" + adoptDefaultDaemonName,
			Server:       name,
			Daemon:       adoptDefaultDaemonName,
			Port:         rec.Port,
			ManifestHash: rec.ExpectedManifestHash,
		}}}
		if writeErr := WriteSupervisorIntent(filepath.Join(stateRoot, supervisorIntentFileLeaf), intent); writeErr != nil {
			t.Fatalf("inject post-delete supervisor row: %v", writeErr)
		}
	}
	t.Cleanup(func() { deAdoptAfterManifestDeleteHook = previous })

	_, err = NewAPI().executeDeAdoptPlanWithOpts(plan, io.Discard, ExecuteDeAdoptOpts{
		providerDeps: providerTransactionDeps{
			source: provider,
			stop: func(context.Context, SupervisorDaemon) (StoppedSettlement, error) {
				t.Fatal("post-delete row must be preserved for a later retry, not deleted without settlement")
				return StoppedSettlement{}, nil
			},
		},
	})
	if err == nil || !strings.Contains(err.Error(), "E_PROVIDER_LIFECYCLE_UNSUPPORTED") {
		t.Fatalf("error=%v, want fail-closed lifecycle refusal", err)
	}
	if provider.calls != 0 || provider.entry.Enabled {
		t.Fatalf("provider restored after post-delete ownership race: provider=%+v calls=%d", provider.entry, provider.calls)
	}
	if _, statErr := os.Stat(filepath.Join(manifestRoot, name, "manifest.yaml")); !os.IsNotExist(statErr) {
		t.Fatalf("manifest should already be hash-deleted before the injected late row, stat err=%v", statErr)
	}
	intent, readErr := ReadSupervisorIntent(filepath.Join(stateRoot, supervisorIntentFileLeaf))
	if readErr != nil {
		t.Fatalf("ReadSupervisorIntent: %v", readErr)
	}
	if len(intent.Daemons) != 1 || intent.Daemons[0].Server != name {
		t.Fatalf("late supervisor row was not preserved: %+v", intent.Daemons)
	}
	persisted, found, readErr := ReadAdoptProvenance(name)
	if readErr != nil || !found || persisted.ProviderSource == nil || persisted.ProviderSource.DeAdoptPhase != "" {
		t.Fatalf("pre-install recovery phase advanced despite late ownership: row=%+v found=%t err=%v", persisted, found, readErr)
	}
}

func TestExecuteDeAdoptProviderRestoreRepairsLateExactRowAfterManagedRemoved(t *testing.T) {
	name := "provider-restore-late-exact-row"
	manifestRoot, stateRoot, rec, provider := setupProviderPreInstallRecoveryFixture(t, name, AdoptOperationStateDeAdopting)
	if err := os.Remove(filepath.Join(manifestRoot, name, "manifest.yaml")); err != nil {
		t.Fatalf("remove manifest: %v", err)
	}
	rec.ProviderSource.DeAdoptPhase = "managed_removed"
	writeDeAdoptExecutorRecord(t, rec)
	if err := writeProviderInstallPhase(name, providerInstallPhaseRecoveryClaimed); err != nil {
		t.Fatalf("set reachable recovery phase: %v", err)
	}
	intent := &SupervisorIntentFile{Version: 1, Daemons: []SupervisorDaemon{{
		TaskName:     "\\mcp-local-hub-" + name + "-" + adoptDefaultDaemonName,
		Server:       name,
		Daemon:       adoptDefaultDaemonName,
		Port:         rec.Port,
		ManifestHash: rec.ExpectedManifestHash,
	}}}
	if err := WriteSupervisorIntent(filepath.Join(stateRoot, supervisorIntentFileLeaf), intent); err != nil {
		t.Fatal(err)
	}

	previousStop := providerRecoveryStopManagedFn
	stops := 0
	providerRecoveryStopManagedFn = func(_ context.Context, _ *API, frozen SupervisorDaemon) (StoppedSettlement, error) {
		stops++
		if frozen.Server != name || frozen.Daemon != adoptDefaultDaemonName || frozen.Port != rec.Port || frozen.ManifestHash != rec.ExpectedManifestHash {
			t.Fatalf("unexpected late frozen row: %+v", frozen)
		}
		return StoppedSettlement{TaskName: frozen.TaskName, State: StoppedSettlementStopped, Reason: StoppedSettlementReasonStopped}, nil
	}
	t.Cleanup(func() { providerRecoveryStopManagedFn = previousStop })

	plan, err := NewAPI().BuildDeAdoptPlan(name)
	if err != nil {
		t.Fatal(err)
	}
	_, err = NewAPI().executeDeAdoptPlanWithOpts(plan, io.Discard, ExecuteDeAdoptOpts{
		providerDeps: providerTransactionDeps{source: provider},
	})
	if err != nil {
		t.Fatalf("late exact managed row retry should repair and complete: %v", err)
	}
	if stops != 1 || provider.calls != 1 || !provider.entry.Enabled {
		t.Fatalf("stops=%d provider=%+v calls=%d", stops, provider.entry, provider.calls)
	}
	intent, readErr := ReadSupervisorIntent(filepath.Join(stateRoot, supervisorIntentFileLeaf))
	if readErr != nil {
		t.Fatalf("ReadSupervisorIntent: %v", readErr)
	}
	if len(intent.Daemons) != 0 {
		t.Fatalf("late exact supervisor row remained after repair: %+v", intent.Daemons)
	}
	assertDeAdoptClosed(t, manifestRoot, stateRoot, rec)
}

func TestExecuteDeAdoptProviderRestoreRejectsLateLegacyOwnedRow(t *testing.T) {
	name := "provider-restore-late-legacy-row"
	manifestRoot, stateRoot, rec, provider := setupProviderPreInstallRecoveryFixture(t, name, AdoptOperationStateDeAdopting)
	if err := os.Remove(filepath.Join(manifestRoot, name, "manifest.yaml")); err != nil {
		t.Fatalf("remove manifest: %v", err)
	}
	rec.ProviderSource.DeAdoptPhase = "managed_removed"
	writeDeAdoptExecutorRecord(t, rec)
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
		t.Fatalf("error=%v, want provider restore ownership refusal", err)
	}
	if provider.calls != 0 || provider.entry.Enabled {
		t.Fatalf("provider restored while a legacy owned row remained: provider=%+v calls=%d", provider.entry, provider.calls)
	}
}

func TestRemoveSettledProviderAdoptDaemonGenerationRejectsSameContentRewrite(t *testing.T) {
	name := "provider-settled-generation-rewrite"
	_, stateRoot, rec, _ := setupProviderPreInstallRecoveryFixture(t, name, AdoptOperationStateDeAdopting)
	intentPath := filepath.Join(stateRoot, supervisorIntentFileLeaf)
	intent := &SupervisorIntentFile{Version: 1, Daemons: []SupervisorDaemon{{
		TaskName:     "\\mcp-local-hub-" + name + "-" + adoptDefaultDaemonName,
		Server:       name,
		Daemon:       adoptDefaultDaemonName,
		Port:         rec.Port,
		ManifestHash: rec.ExpectedManifestHash,
	}}}
	if err := WriteSupervisorIntent(intentPath, intent); err != nil {
		t.Fatal(err)
	}
	frozen, generation, err := frozenProviderAdoptDaemonWithGeneration(rec)
	if err != nil {
		t.Fatalf("freeze provider row: %v", err)
	}

	// Recommit the identical descriptor. Descriptor equality alone cannot
	// distinguish this replacement from the generation that was settled.
	current, err := ReadSupervisorIntent(intentPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := WriteSupervisorIntent(intentPath, current); err != nil {
		t.Fatalf("rewrite identical provider row: %v", err)
	}
	if err := removeSettledProviderAdoptDaemonGeneration(rec, frozen, generation); err == nil || !strings.Contains(err.Error(), "E_PROVIDER_LIFECYCLE_UNSUPPORTED") {
		t.Fatalf("generation cleanup error=%v, want fail-closed replacement refusal", err)
	}
	preserved, err := ReadSupervisorIntent(intentPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(preserved.Daemons) != 1 || preserved.Daemons[0].TaskName != frozen.TaskName {
		t.Fatalf("rewritten provider row was deleted: %+v", preserved.Daemons)
	}
	if preserved.IntentGeneration == generation {
		t.Fatalf("rewrite did not advance intent generation: still %d", generation)
	}
}
