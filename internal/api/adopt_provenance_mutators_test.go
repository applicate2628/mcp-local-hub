package api

import (
	"os"
	"path/filepath"
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
