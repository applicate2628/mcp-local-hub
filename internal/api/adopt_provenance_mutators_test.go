package api

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func seedAdoptProvenanceMutatorRecord(t *testing.T, rec AdoptProvenanceRecord) string {
	t.Helper()
	if err := writeAdoptedEntries(&AdoptedEntries{
		Version: adoptedEntriesSchemaVersion,
		Records: []AdoptProvenanceRecord{rec},
	}); err != nil {
		t.Fatalf("seed adopted provenance: %v", err)
	}
	snapshotDir, err := adoptSnapshotDir(rec.ManifestName)
	if err != nil {
		t.Fatalf("resolve snapshot dir: %v", err)
	}
	if err := os.MkdirAll(snapshotDir, 0o700); err != nil {
		t.Fatalf("create snapshot dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(snapshotDir, "codex-cli.snapshot"), []byte("SECRET"), 0o600); err != nil {
		t.Fatalf("seed snapshot: %v", err)
	}
	return snapshotDir
}

func TestAdoptProvenanceMutatorsT6MarkAndClose(t *testing.T) {
	isolateStateDir(t)
	manifest := "deadoptt6"
	rec := sampleAdoptRecord()
	rec.ManifestName = manifest
	rec.SourceEntryName = manifest
	rec.OperationState = AdoptOperationStateAdopted
	rec.UpdatedAt = time.Now().Add(-time.Hour).UTC()
	snapshotDir := seedAdoptProvenanceMutatorRecord(t, rec)

	if err := MarkAdoptProvenanceDeAdopting(manifest); err != nil {
		t.Fatalf("MarkAdoptProvenanceDeAdopting: %v", err)
	}
	marked, found, err := ReadAdoptProvenance(manifest)
	if err != nil || !found {
		t.Fatalf("read marked provenance: found=%v err=%v", found, err)
	}
	if marked.OperationState != AdoptOperationStateDeAdopting {
		t.Fatalf("state after mark = %q, want %q", marked.OperationState, AdoptOperationStateDeAdopting)
	}
	firstMarkedAt := marked.UpdatedAt

	if err := MarkAdoptProvenanceDeAdopting(manifest); err != nil {
		t.Fatalf("idempotent MarkAdoptProvenanceDeAdopting: %v", err)
	}
	remarked, found, err := ReadAdoptProvenance(manifest)
	if err != nil || !found {
		t.Fatalf("read re-marked provenance: found=%v err=%v", found, err)
	}
	if remarked.OperationState != AdoptOperationStateDeAdopting || !remarked.UpdatedAt.Equal(firstMarkedAt) {
		t.Fatalf("idempotent re-mark changed row: state=%q updated_at=%v, want state=%q updated_at=%v", remarked.OperationState, remarked.UpdatedAt, AdoptOperationStateDeAdopting, firstMarkedAt)
	}

	if err := CloseAdoptProvenance(manifest); err != nil {
		t.Fatalf("CloseAdoptProvenance: %v", err)
	}
	if _, found, err := ReadAdoptProvenance(manifest); err != nil || found {
		t.Fatalf("row after close: found=%v err=%v, want absent", found, err)
	}
	if _, err := os.Stat(snapshotDir); !os.IsNotExist(err) {
		t.Fatalf("snapshot dir after close: stat err=%v, want absent", err)
	}
	// A successful close must never leave the forbidden snapshot-without-row
	// state: once the row is absent, the snapshot directory is absent too.
	if _, found, err := ReadAdoptProvenance(manifest); err != nil || found {
		t.Fatalf("post-close row state: found=%v err=%v", found, err)
	} else if _, statErr := os.Stat(snapshotDir); !os.IsNotExist(statErr) {
		t.Fatalf("snapshot-without-row residue exists: %v", statErr)
	}

	if err := CloseAdoptProvenance(manifest); err != nil {
		t.Fatalf("idempotent CloseAdoptProvenance: %v", err)
	}
}

func TestMarkAdoptProvenanceDeAdoptingRejectsMissingAndClosed(t *testing.T) {
	t.Run("missing", func(t *testing.T) {
		isolateStateDir(t)
		err := MarkAdoptProvenanceDeAdopting("missingmark")
		if err == nil || !strings.Contains(err.Error(), "no provenance row") {
			t.Fatalf("missing-row error = %v, want clear no-row refusal", err)
		}
	})

	t.Run("closed", func(t *testing.T) {
		isolateStateDir(t)
		rec := sampleAdoptRecord()
		rec.ManifestName = "closedmark"
		rec.OperationState = AdoptOperationStateClosed
		seedAdoptProvenanceMutatorRecord(t, rec)
		err := MarkAdoptProvenanceDeAdopting(rec.ManifestName)
		if err == nil || !strings.Contains(err.Error(), "already closed") {
			t.Fatalf("closed-row error = %v, want already-closed refusal", err)
		}
		got, found, readErr := ReadAdoptProvenance(rec.ManifestName)
		if readErr != nil || !found || got.OperationState != AdoptOperationStateClosed {
			t.Fatalf("closed row changed: found=%v state=%v err=%v", found, got, readErr)
		}
	})
}

func TestCloseAdoptProvenanceRejectsNonDeAdoptingRow(t *testing.T) {
	isolateStateDir(t)
	rec := sampleAdoptRecord()
	rec.ManifestName = "closeadopted"
	rec.OperationState = AdoptOperationStateAdopted
	snapshotDir := seedAdoptProvenanceMutatorRecord(t, rec)

	err := CloseAdoptProvenance(rec.ManifestName)
	if err == nil || !strings.Contains(err.Error(), "state") {
		t.Fatalf("CloseAdoptProvenance(adopted) error = %v, want state refusal", err)
	}
	if _, found, readErr := ReadAdoptProvenance(rec.ManifestName); readErr != nil || !found {
		t.Fatalf("row after refused close: found=%v err=%v", found, readErr)
	}
	if _, statErr := os.Stat(snapshotDir); statErr != nil {
		t.Fatalf("snapshot after refused close: %v", statErr)
	}
}

func TestMarkAdoptProvenanceDeAdoptingT12B4ReclassifiesAdoptingRow(t *testing.T) {
	t.Run("committed-adopting succeeds", func(t *testing.T) {
		isolateStateDir(t)
		origManifestExists := adoptManifestExistsFn
		adoptManifestExistsFn = func(string) (bool, error) { return true, nil }
		t.Cleanup(func() { adoptManifestExistsFn = origManifestExists })

		rec := sampleAdoptRecord()
		rec.ManifestName = "b4committed"
		rec.SourceEntryName = rec.ManifestName
		rec.AdoptClients = nil
		rec.Clients = nil
		rec.OperationState = AdoptOperationStateAdopting
		seedAdoptProvenanceMutatorRecord(t, rec)
		if got := classifyDeadAdoptingRow(rec); got != adoptRowCommittedKeep {
			t.Fatalf("precondition classifier = %v, want adoptRowCommittedKeep", got)
		}

		if err := MarkAdoptProvenanceDeAdopting(rec.ManifestName); err != nil {
			t.Fatalf("mark committed-adopting row: %v", err)
		}
		got, found, err := ReadAdoptProvenance(rec.ManifestName)
		if err != nil || !found || got.OperationState != AdoptOperationStateDeAdopting {
			t.Fatalf("committed-adopting mark: found=%v row=%+v err=%v", found, got, err)
		}
	})

	t.Run("uncommitted orphan refuses and remains reapable", func(t *testing.T) {
		isolateStateDir(t)
		origManifestExists := adoptManifestExistsFn
		adoptManifestExistsFn = func(string) (bool, error) { return false, nil }
		t.Cleanup(func() { adoptManifestExistsFn = origManifestExists })

		rec := sampleAdoptRecord()
		rec.ManifestName = "b4orphan"
		rec.SourceEntryName = rec.ManifestName
		rec.AdoptClients = nil
		rec.Clients = nil
		rec.OperationState = AdoptOperationStateAdopting
		rec.UpdatedAt = time.Now().Add(-2 * time.Hour).UTC()
		snapshotDir := seedAdoptProvenanceMutatorRecord(t, rec)
		if got := classifyDeadAdoptingRow(rec); got == adoptRowCommittedKeep {
			t.Fatalf("precondition classifier = adoptRowCommittedKeep, want reapable orphan")
		}

		err := MarkAdoptProvenanceDeAdopting(rec.ManifestName)
		if err == nil || !strings.Contains(err.Error(), "not committed") {
			t.Fatalf("orphan mark error = %v, want not-committed refusal", err)
		}
		got, found, readErr := ReadAdoptProvenance(rec.ManifestName)
		if readErr != nil || !found || got.OperationState != AdoptOperationStateAdopting {
			t.Fatalf("orphan was wedged/changed: found=%v row=%+v err=%v", found, got, readErr)
		}
		if verdict := classifyDeadAdoptingRow(*got); verdict == adoptRowCommittedKeep {
			t.Fatalf("refused orphan is no longer reapable: classifier=%v", verdict)
		}
		if _, statErr := os.Stat(snapshotDir); statErr != nil {
			t.Fatalf("refused orphan snapshot changed: %v", statErr)
		}

		reaped, gcErr := gcOrphanedAdoptingProvenance(time.Hour)
		if gcErr != nil || reaped != 1 {
			t.Fatalf("GC of refused orphan: reaped=%d err=%v, want one reap", reaped, gcErr)
		}
		if _, found, readErr := ReadAdoptProvenance(rec.ManifestName); readErr != nil || found {
			t.Fatalf("refused orphan was not reapable: found=%v err=%v", found, readErr)
		}
	})
}

func TestBuildDeAdoptPlanProviderRecoveryKeepsAmbiguousAdoptingRow(t *testing.T) {
	isolateStateDir(t)
	rec := sampleAdoptRecord()
	rec.ManifestName = "provider-recovery-keep"
	rec.SourceEntryName = rec.ManifestName
	rec.AdoptClients = nil
	rec.Clients = nil
	rec.OperationState = AdoptOperationStateAdopting
	rec.UpdatedAt = time.Now().Add(-2 * time.Hour).UTC()
	rec.ProviderSource = &ProviderSourceProvenanceV1{
		ProviderClient: "codex-cli", PluginRef: "fixture@catalog", ServerName: rec.ManifestName,
		Scope: "user", ReceiptFingerprint: "receipt", ActivationFingerprint: "prior",
		PolicyFingerprint: "policy", PriorEnabledPresent: true, PriorEnabled: true,
		ExpectedDisabledFingerprint: "disabled", DisablePhase: "disable_planned",
	}
	seedAdoptProvenanceMutatorRecord(t, rec)

	if reaped, err := gcOrphanedAdoptingProvenance(time.Hour); err != nil || reaped != 0 {
		t.Fatalf("ambiguous provider recovery GC = reaped=%d err=%v, want retained", reaped, err)
	}
	plan, err := NewAPI().BuildDeAdoptPlan(rec.ManifestName)
	if err != nil {
		t.Fatalf("BuildDeAdoptPlan provider recovery: %v", err)
	}
	if plan.Routing != DeAdoptRoutingFresh || plan.RefusalReason != "" {
		t.Fatalf("provider recovery plan = %+v, want executable explicit recovery", plan)
	}
}

func TestBuildAdoptPlanRefusesProviderRecoveryReceipt(t *testing.T) {
	isolateStateDir(t)
	rec := sampleAdoptRecord()
	rec.ManifestName = "provider-recovery-refusal"
	rec.SourceEntryName = rec.ManifestName
	rec.AdoptClients = nil
	rec.Clients = nil
	rec.OperationState = AdoptOperationStateAdopting
	rec.ProviderSource = &ProviderSourceProvenanceV1{
		ProviderClient: "codex-cli", PluginRef: "fixture@catalog", ServerName: rec.ManifestName,
		Scope: "user", ReceiptFingerprint: "receipt", ActivationFingerprint: "prior",
		PolicyFingerprint: "policy", PriorEnabledPresent: true, PriorEnabled: true,
		ExpectedDisabledFingerprint: "disabled", DisablePhase: "disable_planned",
	}
	seedAdoptProvenanceMutatorRecord(t, rec)
	_, err := NewAPI().BuildAdoptPlan(AdoptOpts{EntryName: rec.ManifestName, Client: "codex-cli", ManifestName: rec.ManifestName, ProviderPluginRef: rec.ProviderSource.PluginRef})
	if err == nil || !strings.Contains(err.Error(), "E_PROVIDER_RECOVERY_REQUIRED") {
		t.Fatalf("fresh provider adopt error=%v, want recovery refusal", err)
	}
}

func TestCaptureAdoptProvenanceGenericUpsertPreservesProviderRecoveryReceipt(t *testing.T) {
	entry := "provider-recovery-generic-capture"
	setupAdoptTestEnv(t, entry, `[mcp_servers.provider-recovery-generic-capture]
command = "go"
args = ["version"]
`)

	prior := sampleAdoptRecord()
	prior.ManifestName = entry
	prior.SourceEntryName = entry
	prior.AdoptClients = []string{"codex-cli"}
	prior.Clients = prior.Clients[:1]
	prior.Clients[0].Client = "codex-cli"
	prior.Clients[0].SnapshotRef = "adopt-provenance/" + entry + "/codex-cli.snapshot"
	prior.OperationState = AdoptOperationStateAdopting
	prior.ProviderSource = sampleProviderSource()
	prior.ProviderSource.ServerName = entry
	prior.ProviderSource.DisablePhase = "disable_planned"
	snapshotDir := seedAdoptProvenanceMutatorRecord(t, prior)
	before, found, err := ReadAdoptProvenance(entry)
	if err != nil || !found {
		t.Fatalf("read seeded recovery receipt: found=%v err=%v", found, err)
	}
	snapshotPath := filepath.Join(snapshotDir, "codex-cli.snapshot")
	snapshotBefore, err := os.ReadFile(snapshotPath)
	if err != nil {
		t.Fatalf("read seeded recovery snapshot: %v", err)
	}

	plan := &AdoptPlan{
		EntryName:        entry,
		SourceClient:     "codex-cli",
		ManifestName:     entry,
		Port:             nextBindableAdoptPortForTest(t, collectUsedAdoptPorts()),
		AdoptClients:     []string{"codex-cli"},
		ManifestYAML:     "name: " + entry + "\n",
		presentAtBuild:   []string{"codex-cli"},
		TargetEntryNames: map[string]string{"codex-cli": entry},
	}
	previousUnmutated := adoptRowProvablyUnmutatedFn
	adoptRowProvablyUnmutatedFn = func(AdoptProvenanceRecord) bool { return true }
	t.Cleanup(func() { adoptRowProvablyUnmutatedFn = previousUnmutated })
	if _, err := NewAPI().captureAdoptProvenance(plan); err == nil || !strings.Contains(err.Error(), "E_PROVIDER_RECOVERY_REQUIRED") {
		t.Fatalf("generic capture error=%v, want E_PROVIDER_RECOVERY_REQUIRED", err)
	}

	after, found, err := ReadAdoptProvenance(entry)
	if err != nil || !found {
		t.Fatalf("read recovery receipt after generic capture: found=%v err=%v", found, err)
	}
	if !reflect.DeepEqual(after, before) {
		t.Fatalf("generic capture changed provider recovery receipt\nbefore=%#v\nafter=%#v", before, after)
	}
	snapshotAfter, err := os.ReadFile(snapshotPath)
	if err != nil {
		t.Fatalf("read recovery snapshot after generic capture: %v", err)
	}
	if !reflect.DeepEqual(snapshotAfter, snapshotBefore) {
		t.Fatalf("generic capture changed provider recovery snapshot: before=%q after=%q", snapshotBefore, snapshotAfter)
	}
}

func TestAdvanceProviderDeAdoptPhaseRequiresExactLinearTransition(t *testing.T) {
	isolateStateDir(t)
	rec := sampleAdoptRecord()
	rec.ManifestName = "provider-phase"
	rec.OperationState = AdoptOperationStateDeAdopting
	rec.ProviderSource = &ProviderSourceProvenanceV1{
		ProviderClient: "codex-cli", PluginRef: "fixture@catalog", ServerName: rec.ManifestName,
		Scope: "user", ReceiptFingerprint: "receipt", ActivationFingerprint: "prior",
		PolicyFingerprint: "policy", PriorEnabledPresent: true, PriorEnabled: true,
		ExpectedDisabledFingerprint: "disabled", DisablePhase: "disable_applied",
	}
	seedAdoptProvenanceMutatorRecord(t, rec)

	if _, err := AdvanceProviderDeAdoptPhase(rec.ManifestName, "", "managed_removed"); err == nil {
		t.Fatal("skipped phase transition was accepted")
	}
	updated, err := AdvanceProviderDeAdoptPhase(rec.ManifestName, "", "managed_stop_settled")
	if err != nil || updated.ProviderSource.DeAdoptPhase != "managed_stop_settled" {
		t.Fatalf("first phase update = %+v err=%v", updated, err)
	}
	if _, err := AdvanceProviderDeAdoptPhase(rec.ManifestName, "", "managed_stop_settled"); err == nil {
		t.Fatal("stale expected phase was accepted")
	}
}
