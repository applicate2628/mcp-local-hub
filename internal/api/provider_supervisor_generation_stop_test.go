package api

import (
	"path/filepath"
	"testing"
)

func TestRemoveSettledProviderAdoptDaemonGenerationAcceptsOwnStopGeneration(t *testing.T) {
	name := "provider-settled-own-stop-generation"
	_, stateRoot, rec, _ := setupProviderPreInstallRecoveryFixture(t, name, AdoptOperationStateDeAdopting)
	intentPath := filepath.Join(stateRoot, supervisorIntentFileLeaf)
	intent := &SupervisorIntentFile{Version: 1, Daemons: []SupervisorDaemon{{
		TaskName:     "\\mcp-local-hub-" + name + "-" + adoptDefaultDaemonName,
		Server:       name,
		Daemon:       adoptDefaultDaemonName,
		Port:         rec.Port,
		ManifestHash: rec.ExpectedManifestHash,
	}}
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

	if err := removeSettledProviderAdoptDaemonGeneration(rec, frozen, generation); err != nil {
		t.Fatalf("cleanup rejected the generation written by its own managed stop: %v", err)
	}
	settled, err := ReadSupervisorIntent(intentPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(settled.Daemons) != 0 {
		t.Fatalf("settled provider row remained: %+v", settled.Daemons)
	}
}
