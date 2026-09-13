package api

import (
	"context"
	"io"
	"path/filepath"
	"testing"
)

func TestProviderGenerationAuditPreexistingStopDoesNotAuthenticateRewrite(t *testing.T) {
	name := "provider-audit-generation-rewrite"
	_, stateRoot, rec, _ := setupProviderPreInstallRecoveryFixture(t, name, AdoptOperationStateDeAdopting)
	intentPath := filepath.Join(stateRoot, supervisorIntentFileLeaf)
	task := "\\mcp-local-hub-" + name + "-" + adoptDefaultDaemonName
	intent := &SupervisorIntentFile{Version: 1,
		Daemons: []SupervisorDaemon{{TaskName: task, Server: name, Daemon: adoptDefaultDaemonName, Port: rec.Port, ManifestHash: rec.ExpectedManifestHash}},
		Stops:   map[string]DaemonIntent{task: {Desired: IntentDesiredStopped, Reason: IntentReasonUserStop}},
	}
	if err := WriteSupervisorIntent(intentPath, intent); err != nil {
		t.Fatal(err)
	}
	fence, err := frozenProviderAdoptDaemonFence(rec)
	if err != nil {
		t.Fatal(err)
	}
	current, err := ReadSupervisorIntent(intentPath)
	if err != nil {
		t.Fatal(err)
	}
	// An equal-content write, not this operation's stop, consumes generation + 1.
	if err := WriteSupervisorIntent(intentPath, current); err != nil {
		t.Fatal(err)
	}
	if err := removeSettledProviderAdoptDaemonFence(rec, fence); err == nil {
		t.Fatal("preexisting stop authenticated a replacement generation")
	}
	preserved, err := ReadSupervisorIntent(intentPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(preserved.Daemons) != 1 {
		t.Fatalf("replacement descriptor was deleted: %+v", preserved.Daemons)
	}
}

func TestProviderGenerationAuditOrdinaryRemovalPreservesReplacement(t *testing.T) {
	name := "provider-audit-ordinary-replacement"
	_, stateRoot, rec, provider := setupProviderPreInstallRecoveryFixture(t, name, AdoptOperationStateAdopting)
	task := "\\mcp-local-hub-" + name + "-" + adoptDefaultDaemonName
	if err := markProviderInstallStartedForTask(task); err != nil {
		t.Fatal(err)
	}
	intentPath := filepath.Join(stateRoot, supervisorIntentFileLeaf)
	original := SupervisorDaemon{TaskName: task, Server: name, Daemon: adoptDefaultDaemonName, Port: rec.Port, ManifestHash: rec.ExpectedManifestHash}
	if err := WriteSupervisorIntent(intentPath, &SupervisorIntentFile{Version: 1, Daemons: []SupervisorDaemon{original}}); err != nil {
		t.Fatal(err)
	}
	plan, err := NewAPI().BuildDeAdoptPlan(name)
	if err != nil {
		t.Fatal(err)
	}
	previous := deAdoptAfterManifestDeleteHook
	t.Cleanup(func() { deAdoptAfterManifestDeleteHook = previous })
	replacement := original
	replacement.Port++
	deAdoptAfterManifestDeleteHook = func() {
		if err := WriteSupervisorIntent(intentPath, &SupervisorIntentFile{Version: 1, Daemons: []SupervisorDaemon{replacement}}); err != nil {
			t.Fatal(err)
		}
	}
	stops := 0
	_, err = NewAPI().executeDeAdoptPlanWithOpts(plan, io.Discard, ExecuteDeAdoptOpts{providerDeps: providerTransactionDeps{
		source: provider,
		stop: func(_ context.Context, frozen SupervisorDaemon) (StoppedSettlement, error) {
			stops++
			if frozen.Port != original.Port {
				t.Fatal("test unexpectedly stopped replacement")
			}
			return StoppedSettlement{TaskName: frozen.TaskName, State: StoppedSettlementStopped, Reason: StoppedSettlementReasonStopped}, nil
		},
	}})
	if err == nil {
		t.Fatal("ordinary teardown accepted ownership replaced after settlement")
	}
	if stops != 1 || provider.calls != 0 || provider.entry.Enabled {
		t.Fatalf("stops=%d restore calls=%d enabled=%t", stops, provider.calls, provider.entry.Enabled)
	}
	preserved, err := ReadSupervisorIntent(intentPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(preserved.Daemons) != 1 || preserved.Daemons[0].Port != replacement.Port {
		t.Fatalf("unsettled replacement was erased: %+v", preserved.Daemons)
	}
}
