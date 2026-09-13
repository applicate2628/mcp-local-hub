package api

import (
	"path/filepath"
	"testing"
)

func TestRemoveSettledProviderAdoptDaemonGenerationAcceptsOwnStopGeneration(t *testing.T) {
	name := "provider-settled-own-stop-generation"
	_, stateRoot, rec, _ := setupProviderPreInstallRecoveryFixture(t, name, AdoptOperationStateDeAdopting)
	intentPath := filepath.Join(stateRoot, supervisorIntentFileLeaf)
	otherTask := "\\mcp-local-hub-unrelated-default"
	intent := &SupervisorIntentFile{
		Version: 1,
		Daemons: []SupervisorDaemon{{
			TaskName:     "\\mcp-local-hub-" + name + "-" + adoptDefaultDaemonName,
			Server:       name,
			Daemon:       adoptDefaultDaemonName,
			Port:         rec.Port,
			ManifestHash: rec.ExpectedManifestHash,
		}},
		Stops: map[string]DaemonIntent{
			otherTask: {Desired: IntentDesiredStopped, Reason: IntentReasonUserStop},
		},
	}
	if err := WriteSupervisorIntent(intentPath, intent); err != nil {
		t.Fatal(err)
	}
	frozen, generation, err := frozenProviderAdoptDaemonWithGeneration(rec)
	if err != nil {
		t.Fatalf("freeze provider row: %v", err)
	}

	if err := MutateSupervisorIntentIfChanged(intentPath, func(current *SupervisorIntentFile) (bool, error) {
		if current.Stops == nil {
			current.Stops = make(map[string]DaemonIntent)
		}
		current.Stops[frozen.TaskName] = DaemonIntent{Desired: IntentDesiredStopped, Reason: IntentReasonUserStop}
		return true, nil
	}); err != nil {
		t.Fatalf("record managed stop: %v", err)
	}

	settledIntent, err := ReadSupervisorIntent(intentPath)
	if err != nil {
		t.Fatal(err)
	}
	if settledIntent.IntentGeneration != generation+1 {
		t.Fatalf("managed stop generation=%d, want %d", settledIntent.IntentGeneration, generation+1)
	}
	if err := removeSettledProviderAdoptDaemonGeneration(rec, frozen, settledIntent.IntentGeneration); err != nil {
		t.Fatalf("cleanup rejected the exact generation written by its own managed stop: %v", err)
	}
	settled, err := ReadSupervisorIntent(intentPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(settled.Daemons) != 0 {
		t.Fatalf("settled provider row remained: %+v", settled.Daemons)
	}
	if _, ok := settled.Stops[canonicalIntentTaskKey(frozen.TaskName)]; ok {
		t.Fatalf("settled provider stop survived descriptor removal: %+v", settled.Stops)
	}
	if _, ok := settled.Stops[canonicalIntentTaskKey(otherTask)]; !ok {
		t.Fatalf("unrelated stop was removed: %+v", settled.Stops)
	}
}

func TestRemoveSettledProviderAdoptDaemonFencePrunesLegacyWatermark(t *testing.T) {
	name := "provider-settled-watermark-cleanup"
	_, stateRoot, rec, _ := setupProviderPreInstallRecoveryFixture(t, name, AdoptOperationStateDeAdopting)
	intentPath := filepath.Join(stateRoot, supervisorIntentFileLeaf)
	task := "\\mcp-local-hub-" + name + "-" + adoptDefaultDaemonName
	otherTask := "\\mcp-local-hub-unrelated-default"
	intent := &SupervisorIntentFile{
		Version: 1,
		Daemons: []SupervisorDaemon{{
			TaskName:     task,
			Server:       name,
			Daemon:       adoptDefaultDaemonName,
			Port:         rec.Port,
			ManifestHash: rec.ExpectedManifestHash,
		}},
		LegacyStopWatermarks: map[string]DaemonIntent{
			task:      {Desired: IntentDesiredStopped, Reason: IntentReasonUserStop},
			otherTask: {Desired: IntentDesiredStopped, Reason: IntentReasonUserStop},
		},
	}
	if err := WriteSupervisorIntent(intentPath, intent); err != nil {
		t.Fatal(err)
	}
	fence, err := frozenProviderAdoptDaemonFence(rec)
	if err != nil {
		t.Fatalf("freeze provider row: %v", err)
	}
	if err := removeSettledProviderAdoptDaemonFence(rec, fence); err != nil {
		t.Fatalf("cleanup rejected exact settled fence: %v", err)
	}
	settled, err := ReadSupervisorIntent(intentPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(settled.Daemons) != 0 {
		t.Fatalf("settled provider row remained: %+v", settled.Daemons)
	}
	if _, ok := settled.LegacyStopWatermarks[canonicalIntentTaskKey(task)]; ok {
		t.Fatalf("settled provider watermark survived descriptor removal: %+v", settled.LegacyStopWatermarks)
	}
	if _, ok := settled.LegacyStopWatermarks[canonicalIntentTaskKey(otherTask)]; !ok {
		t.Fatalf("unrelated watermark was removed: %+v", settled.LegacyStopWatermarks)
	}
}
