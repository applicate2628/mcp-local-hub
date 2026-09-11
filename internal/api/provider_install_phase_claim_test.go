package api

import (
	"testing"
)

func TestProviderInstallRecoveryClaimBlocksInstallStart(t *testing.T) {
	name := "provider-install-recovery-claim"
	_, _, rec, _ := setupProviderPreInstallRecoveryFixture(t, name, AdoptOperationStateAdopting)

	claimed, err := providerInstallNeverStarted(rec)
	if err != nil || !claimed {
		t.Fatalf("providerInstallNeverStarted: claimed=%t err=%v", claimed, err)
	}
	phase, err := readProviderInstallPhase(name)
	if err != nil || phase != providerInstallPhaseRecoveryClaimed {
		t.Fatalf("phase=%q err=%v, want %q", phase, err, providerInstallPhaseRecoveryClaimed)
	}

	task := "mcp-local-hub-" + name + "-" + adoptDefaultDaemonName
	if err := markProviderInstallStartedForTask(task); err == nil {
		t.Fatal("Install start unexpectedly crossed a claimed pre-install recovery")
	}
	phase, err = readProviderInstallPhase(name)
	if err != nil || phase != providerInstallPhaseRecoveryClaimed {
		t.Fatalf("phase after refused Install=%q err=%v", phase, err)
	}

	// Recovery retries are idempotent: the durable claim remains the same proof.
	claimed, err = providerInstallNeverStarted(rec)
	if err != nil || !claimed {
		t.Fatalf("replayed recovery claim: claimed=%t err=%v", claimed, err)
	}
}

func TestProviderInstallStartedBlocksRecoveryClaim(t *testing.T) {
	name := "provider-install-started-wins"
	_, _, rec, _ := setupProviderPreInstallRecoveryFixture(t, name, AdoptOperationStateAdopting)
	task := "mcp-local-hub-" + name + "-" + adoptDefaultDaemonName

	if err := markProviderInstallStartedForTask(task); err != nil {
		t.Fatalf("markProviderInstallStartedForTask: %v", err)
	}
	claimed, err := providerInstallNeverStarted(rec)
	if err != nil {
		t.Fatalf("providerInstallNeverStarted: %v", err)
	}
	if claimed {
		t.Fatal("started Install was reclassified as pre-install recovery")
	}
	phase, err := readProviderInstallPhase(name)
	if err != nil || phase != providerInstallPhaseStarted {
		t.Fatalf("phase=%q err=%v, want %q", phase, err, providerInstallPhaseStarted)
	}
}

func TestProviderInstallStartRefusesDeAdoptingProvider(t *testing.T) {
	name := "provider-install-during-deadopt"
	_, _, _, _ = setupProviderPreInstallRecoveryFixture(t, name, AdoptOperationStateDeAdopting)
	task := "mcp-local-hub-" + name + "-" + adoptDefaultDaemonName

	if err := markProviderInstallStartedForTask(task); err == nil {
		t.Fatal("Install start unexpectedly admitted while provider de-adopt is active")
	}
	phase, err := readProviderInstallPhase(name)
	if err != nil || phase != providerInstallPhaseNotStarted {
		t.Fatalf("phase=%q err=%v, want untouched %q", phase, err, providerInstallPhaseNotStarted)
	}
}
