package api

import (
	"context"
	"errors"
	"os"
	"path/filepath"
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
	if !providerInstallPhaseAllowsRestore(name) {
		t.Fatal("claimed pre-install recovery must be restore-eligible after ownership is absent")
	}

	task := "mcp-local-hub-" + name + "-" + adoptDefaultDaemonName
	if err := markProviderInstallStartedForTask(name, task); err == nil {
		t.Fatal("Install start unexpectedly crossed a claimed pre-install recovery")
	}
	phase, err = readProviderInstallPhase(name)
	if err != nil || phase != providerInstallPhaseRecoveryClaimed {
		t.Fatalf("phase after refused Install=%q err=%v", phase, err)
	}

	claimed, err = providerInstallNeverStarted(rec)
	if err != nil || !claimed {
		t.Fatalf("replayed recovery claim: claimed=%t err=%v", claimed, err)
	}
}

func TestProviderInstallStartedBlocksRecoveryClaimAndRestore(t *testing.T) {
	name := "provider-install-started-wins"
	_, _, rec, _ := setupProviderPreInstallRecoveryFixture(t, name, AdoptOperationStateAdopting)
	task := "mcp-local-hub-" + name + "-" + adoptDefaultDaemonName

	if err := markProviderInstallStartedForTask(name, task); err != nil {
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
	if providerInstallPhaseAllowsRestore(name) {
		t.Fatal("started Install without managed settlement unexpectedly became restore-eligible")
	}
}

func TestProviderManagedStopMakesStartedInstallRestoreEligible(t *testing.T) {
	name := "provider-install-managed-settled"
	_, _, _, _ = setupProviderPreInstallRecoveryFixture(t, name, AdoptOperationStateAdopting)
	task := "mcp-local-hub-" + name + "-" + adoptDefaultDaemonName
	if err := markProviderInstallStartedForTask(name, task); err != nil {
		t.Fatalf("markProviderInstallStartedForTask: %v", err)
	}

	deps := providerTransactionDeps{stop: func(context.Context, SupervisorDaemon) (StoppedSettlement, error) {
		return StoppedSettlement{State: StoppedSettlementStopped, Reason: StoppedSettlementReasonStopped}, nil
	}}
	if _, err := deps.stopManaged(context.Background(), NewAPI(), SupervisorDaemon{Server: name}); err != nil {
		t.Fatalf("stopManaged: %v", err)
	}
	phase, err := readProviderInstallPhase(name)
	if err != nil || phase != providerInstallPhaseManagedSettled {
		t.Fatalf("phase=%q err=%v, want %q", phase, err, providerInstallPhaseManagedSettled)
	}
	if !providerInstallPhaseAllowsRestore(name) {
		t.Fatal("terminal managed settlement did not become restore-eligible")
	}
}

func TestProviderManagedStopBootstrapsLegacyMissingMarker(t *testing.T) {
	name := "provider-install-legacy-managed-settled"
	_, _, _, _ = setupProviderPreInstallRecoveryFixture(t, name, AdoptOperationStateAdopting)
	marker, err := providerInstallPhasePath(name)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(marker); err != nil {
		t.Fatalf("remove provider-install phase marker: %v", err)
	}
	deps := providerTransactionDeps{stop: func(context.Context, SupervisorDaemon) (StoppedSettlement, error) {
		return StoppedSettlement{State: StoppedSettlementStopped, Reason: StoppedSettlementReasonStopped}, nil
	}}
	if _, err := deps.stopManaged(context.Background(), NewAPI(), SupervisorDaemon{Server: name}); err != nil {
		t.Fatalf("legacy stopManaged: %v", err)
	}
	phase, err := readProviderInstallPhase(name)
	if err != nil || phase != providerInstallPhaseManagedSettled {
		t.Fatalf("legacy phase=%q err=%v, want %q", phase, err, providerInstallPhaseManagedSettled)
	}
	if !providerInstallPhaseAllowsRestore(name) {
		t.Fatal("legacy exact managed settlement did not become restore-eligible")
	}
}

func TestProviderRestoreGateClaimsNotStartedMarker(t *testing.T) {
	name := "provider-install-restore-gate-claim"
	_, _, _, _ = setupProviderPreInstallRecoveryFixture(t, name, AdoptOperationStateAdopting)
	phase, err := readProviderInstallPhase(name)
	if err != nil || phase != providerInstallPhaseNotStarted {
		t.Fatalf("phase=%q err=%v, want %q", phase, err, providerInstallPhaseNotStarted)
	}
	if !providerInstallPhaseAllowsRestore(name) {
		t.Fatal("restore gate failed to claim a genuine not-started provider adoption")
	}
	phase, err = readProviderInstallPhase(name)
	if err != nil || phase != providerInstallPhaseRecoveryClaimed {
		t.Fatalf("phase after restore gate=%q err=%v", phase, err)
	}
	task := "mcp-local-hub-" + name + "-" + adoptDefaultDaemonName
	if err := markProviderInstallStartedForTask(name, task); err == nil {
		t.Fatal("Install start unexpectedly crossed the restore-gate recovery claim")
	}
}

func TestProviderInstallStartRefusesDeAdoptingProvider(t *testing.T) {
	name := "provider-install-during-deadopt"
	_, _, _, _ = setupProviderPreInstallRecoveryFixture(t, name, AdoptOperationStateDeAdopting)
	task := "mcp-local-hub-" + name + "-" + adoptDefaultDaemonName
	if err := markProviderInstallStartedForTask(name, task); err == nil {
		t.Fatal("Install start unexpectedly admitted while provider de-adopt is active")
	}
}

func TestProviderInstallPhaseReadOnlyNotStartedProofRejectsStartedAndMissing(t *testing.T) {
	name := "provider-install-readonly-proof"
	_, _, _, _ = setupProviderPreInstallRecoveryFixture(t, name, AdoptOperationStateAdopting)
	if !providerInstallPhaseIsNotStarted(name) {
		t.Fatal("fresh durable not_started marker was not recognized")
	}
	task := "mcp-local-hub-" + name + "-" + adoptDefaultDaemonName
	if err := markProviderInstallStartedForTask(name, task); err != nil {
		t.Fatal(err)
	}
	if providerInstallPhaseIsNotStarted(name) {
		t.Fatal("started marker was misclassified as pre-install")
	}
	marker, err := providerInstallPhasePath(name)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(marker); err != nil {
		t.Fatal(err)
	}
	if providerInstallPhaseIsNotStarted(name) {
		t.Fatal("missing legacy marker was misclassified as pre-install")
	}
}

func TestProviderRestoreGateBootstrapsLegacySettledRemovedReceipt(t *testing.T) {
	name := "provider-install-legacy-removed-restore"
	manifestRoot, _, rec, _ := setupProviderPreInstallRecoveryFixture(t, name, AdoptOperationStateDeAdopting)
	// managed_removed follows manifest deletion in the real de-adopt sequence.
	if err := os.Remove(filepath.Join(manifestRoot, name, "manifest.yaml")); err != nil {
		t.Fatal(err)
	}
	rec.ProviderSource.DeAdoptPhase = "managed_removed"
	writeDeAdoptExecutorRecord(t, *rec)
	marker, err := providerInstallPhasePath(name)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(marker); err != nil {
		t.Fatalf("remove legacy provider-install marker: %v", err)
	}
	if !providerInstallPhaseAllowsRestore(name) {
		t.Fatal("durable legacy managed_removed receipt did not bootstrap restore authority")
	}
	phase, err := readProviderInstallPhase(name)
	if err != nil || phase != providerInstallPhaseManagedSettled {
		t.Fatalf("bootstrapped phase=%q err=%v, want %q", phase, err, providerInstallPhaseManagedSettled)
	}
}

func TestProviderRestoreGateDoesNotInventLegacySettlementFromAdoptingReceipt(t *testing.T) {
	name := "provider-install-missing-marker-adopting"
	_, _, _, _ = setupProviderPreInstallRecoveryFixture(t, name, AdoptOperationStateAdopting)
	marker, err := providerInstallPhasePath(name)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(marker); err != nil {
		t.Fatal(err)
	}
	if providerInstallPhaseAllowsRestore(name) {
		t.Fatal("missing marker on an adopting receipt invented managed settlement")
	}
}

func TestRecordInstallAuditMarksProviderInstallStartedAfterAuditBarrier(t *testing.T) {
	name := "provider-install-audit-barrier"
	_, _, _, _ = setupProviderPreInstallRecoveryFixture(t, name, AdoptOperationStateAdopting)
	task := "mcp-local-hub-" + name + "-" + adoptDefaultDaemonName
	previous := appendIntentAuditFn
	appendIntentAuditFn = func(IntentAuditEntry) error { return nil }
	t.Cleanup(func() { appendIntentAuditFn = previous })
	batch := installAuditTaskBatch{ManifestName: name, TaskNames: []string{task}}
	if err := NewAPI().recordInstallAuditForTasks(batch); err != nil {
		t.Fatalf("recordInstallAuditForTasks: %v", err)
	}
	phase, err := readProviderInstallPhase(name)
	if err != nil || phase != providerInstallPhaseStarted {
		t.Fatalf("phase after successful audit=%q err=%v, want %q", phase, err, providerInstallPhaseStarted)
	}
}

func TestRecordInstallAuditFailureLeavesProviderInstallNotStarted(t *testing.T) {
	name := "provider-install-audit-refusal"
	_, _, _, _ = setupProviderPreInstallRecoveryFixture(t, name, AdoptOperationStateAdopting)
	task := "mcp-local-hub-" + name + "-" + adoptDefaultDaemonName
	previous := appendIntentAuditFn
	appendIntentAuditFn = func(IntentAuditEntry) error { return errors.New("injected audit failure") }
	t.Cleanup(func() { appendIntentAuditFn = previous })
	batch := installAuditTaskBatch{ManifestName: name, TaskNames: []string{task}}
	if err := NewAPI().recordInstallAuditForTasks(batch); err == nil {
		t.Fatal("recordInstallAuditForTasks unexpectedly crossed failed audit")
	}
	phase, err := readProviderInstallPhase(name)
	if err != nil || phase != providerInstallPhaseNotStarted {
		t.Fatalf("phase after failed audit=%q err=%v, want %q", phase, err, providerInstallPhaseNotStarted)
	}
}

func TestRecordInstallAuditTaskCollisionDoesNotClaimOtherManifest(t *testing.T) {
	providerName := "foo-bar"
	_, _, _, _ = setupProviderPreInstallRecoveryFixture(t, providerName, AdoptOperationStateAdopting)
	collidingTask := "mcp-local-hub-foo-bar-default"
	previous := appendIntentAuditFn
	appendIntentAuditFn = func(IntentAuditEntry) error { return nil }
	t.Cleanup(func() { appendIntentAuditFn = previous })
	ordinary := installAuditTaskBatch{ManifestName: "foo", TaskNames: []string{collidingTask}}
	if err := NewAPI().recordInstallAuditForTasks(ordinary); err != nil {
		t.Fatalf("unrelated colliding install was rejected: %v", err)
	}
	phase, err := readProviderInstallPhase(providerName)
	if err != nil || phase != providerInstallPhaseNotStarted {
		t.Fatalf("colliding ordinary install changed provider phase=%q err=%v", phase, err)
	}
	provider := installAuditTaskBatch{ManifestName: providerName, TaskNames: []string{collidingTask}}
	if err := NewAPI().recordInstallAuditForTasks(provider); err != nil {
		t.Fatalf("provider install admission failed: %v", err)
	}
	phase, err = readProviderInstallPhase(providerName)
	if err != nil || phase != providerInstallPhaseStarted {
		t.Fatalf("provider install phase=%q err=%v, want %q", phase, err, providerInstallPhaseStarted)
	}
}
