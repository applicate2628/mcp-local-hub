package api

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Finding 1 (P1) — a second capture for a manifest that already has a COMMITTED
// (adopted) provenance row must FAIL CLOSED, never dropping the committed row or
// its snapshot (GUI double-submit: two plans built before the first commits).
func TestCaptureFailsClosedOnCommittedPriorRow(t *testing.T) {
	isolateStateDir(t)
	manifest := "committedm"
	// Seed a COMMITTED (adopted) prior row + its snapshot dir.
	seed := &AdoptedEntries{Version: 1, Records: []AdoptProvenanceRecord{{
		ManifestName:         manifest,
		OperationState:       AdoptOperationStateAdopted,
		AdoptManifestHash:    "hash1",
		ExpectedManifestHash: "hash1",
		UpdatedAt:            time.Now().UTC(),
		Clients: []AdoptClientProvenance{{
			Client: "codex-cli", OriginalState: AdoptOriginalStatePresent,
			SnapshotRef: "adopt-provenance/committedm/codex-cli.snapshot", SnapshotSHA256: "sha1",
		}},
	}}}
	if err := writeAdoptedEntries(seed); err != nil {
		t.Fatalf("seed store: %v", err)
	}
	snapDir, err := adoptSnapshotDir(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(snapDir, 0o700); err != nil {
		t.Fatal(err)
	}
	snapFile := filepath.Join(snapDir, "codex-cli.snapshot")
	if err := os.WriteFile(snapFile, []byte("PRIOR-SECRET"), 0o600); err != nil {
		t.Fatal(err)
	}

	// A second capture for the SAME manifest (plan built before the first committed).
	plan := &AdoptPlan{
		EntryName: manifest, SourceClient: "codex-cli", ManifestName: manifest,
		AdoptClients: []string{"codex-cli"},
		ManifestYAML: "name: " + manifest + "\n",
	}
	rec, err := NewAPI().captureAdoptProvenance(plan)
	if err == nil {
		t.Fatal("capture succeeded despite a committed prior row; want a fail-closed error")
	}
	if rec != nil {
		t.Errorf("capture returned a non-nil rec on the fail-closed path: %+v", rec)
	}
	if !strings.Contains(err.Error(), "committed") {
		t.Errorf("error should name the committed-provenance refusal: %v", err)
	}

	// The committed row + its snapshot SURVIVE (zero side effects).
	got, found, rerr := ReadAdoptProvenance(manifest)
	if rerr != nil || !found {
		t.Fatalf("committed row destroyed by the refused capture: found=%v err=%v", found, rerr)
	}
	if got.OperationState != AdoptOperationStateAdopted {
		t.Errorf("committed row state changed to %q, want adopted", got.OperationState)
	}
	if b, err := os.ReadFile(snapFile); err != nil || string(b) != "PRIOR-SECRET" {
		t.Errorf("committed snapshot destroyed/changed: got %q err=%v", b, err)
	}
	// Exactly one row for the manifest (no partial-replacement residue).
	m, err := readAdoptedEntries()
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, r := range m.Records {
		if r.ManifestName == manifest {
			count++
		}
	}
	if count != 1 {
		t.Errorf("rows for manifest = %d, want exactly 1", count)
	}
}

// Finding 2 (r2 model) — GC reaps a TRUE orphan (aged `adopting`, manifest ABSENT)
// but PRESERVES a row it cannot prove is a crash orphan. NOTE (design r2 supersede):
// the round-1 "manifest EXISTS => keep" rule is REPLACED by the hub-binding-live
// classifier (classifyDeadAdoptingRow). Here `existsM` carries an UNPARSEABLE
// manifest, so the classifier fail-safes to KEEP (never reap on uncertainty) — the
// genuine committed-KEEP-via-live-binding vs valid-no-binding-REAP distinction is
// exercised by TestGcClassifierLiveBindingKeepsValidNoBindingReaps (r2). This test
// pins the fail-safe-keep + absent-reap legs only.
func TestGcOrphanedPreservesCommittedButAdoptingRow(t *testing.T) {
	isolateStateDir(t)
	mdir := defaultManifestDir()

	existsM := "gcexists"
	existsMDir := filepath.Join(mdir, existsM)
	if err := os.MkdirAll(existsMDir, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(existsMDir) })
	if err := os.WriteFile(filepath.Join(existsMDir, "manifest.yaml"), []byte("name: "+existsM+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	absentM := "gcabsent"
	_ = os.RemoveAll(filepath.Join(mdir, absentM)) // ensure the true-orphan's manifest is absent

	aged := time.Now().Add(-2 * time.Hour).UTC()
	seed := &AdoptedEntries{Version: 1, Records: []AdoptProvenanceRecord{
		{ManifestName: existsM, OperationState: AdoptOperationStateAdopting, UpdatedAt: aged,
			Clients: []AdoptClientProvenance{{Client: "codex-cli", OriginalState: AdoptOriginalStatePresent, SnapshotRef: "adopt-provenance/gcexists/codex-cli.snapshot"}}},
		{ManifestName: absentM, OperationState: AdoptOperationStateAdopting, UpdatedAt: aged},
	}}
	if err := writeAdoptedEntries(seed); err != nil {
		t.Fatal(err)
	}
	existsSnap, _ := adoptSnapshotDir(existsM)
	if err := os.MkdirAll(existsSnap, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(existsSnap, "codex-cli.snapshot"), []byte("SECRET"), 0o600); err != nil {
		t.Fatal(err)
	}

	reaped, err := gcOrphanedAdoptingProvenance(1 * time.Hour)
	if err != nil {
		t.Fatalf("gcOrphanedAdoptingProvenance: %v", err)
	}
	if reaped != 1 {
		t.Errorf("reaped = %d, want 1 (only the true orphan whose manifest is absent)", reaped)
	}
	// existsM: manifest is UNPARSEABLE -> classifier fail-safes to KEEP (never reap
	// on uncertainty) -> PRESERVED.
	if _, found, _ := ReadAdoptProvenance(existsM); !found {
		t.Errorf("aged `adopting` row with an unparseable manifest was reaped (classifier must fail-safe KEEP)")
	}
	if _, statErr := os.Stat(existsSnap); statErr != nil {
		t.Errorf("fail-safe-kept row's snapshot dir was removed: %v", statErr)
	}
	// absentM: manifest ABSENT -> true orphan -> reaped.
	if _, found, _ := ReadAdoptProvenance(absentM); found {
		t.Errorf("true orphan (manifest absent) was not reaped")
	}
}

// Finding 3 (P2) — abort removes the snapshot dir BEFORE dropping the row. On an
// injected store-write failure the snapshot is already gone (proving order) and
// the row REMAINS (the safe row->missing-snapshot state, reclaimable by GC/UPSERT
// — never a leaked snapshot->no-row).
func TestAbortRemovesSnapshotsBeforeRowOnWriteFailure(t *testing.T) {
	isolateStateDir(t)
	manifest := "abortorder"
	seed := &AdoptedEntries{Version: 1, Records: []AdoptProvenanceRecord{{
		ManifestName: manifest, OperationState: AdoptOperationStateAdopting, UpdatedAt: time.Now().UTC(),
	}}}
	if err := writeAdoptedEntries(seed); err != nil {
		t.Fatal(err)
	}
	snapDir, _ := adoptSnapshotDir(manifest)
	if err := os.MkdirAll(snapDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(snapDir, "codex-cli.snapshot"), []byte("SECRET"), 0o600); err != nil {
		t.Fatal(err)
	}

	orig := writeAdoptedEntriesFn
	writeAdoptedEntriesFn = func(*AdoptedEntries) error { return fmt.Errorf("induced store-write failure") }
	t.Cleanup(func() { writeAdoptedEntriesFn = orig })

	if err := abortAdoptProvenance(&AdoptProvenanceRecord{ManifestName: manifest}); err == nil {
		t.Fatal("abort with an injected write failure returned nil; want the write error surfaced")
	}
	// Snapshots-first: the dir is GONE even though the row write failed.
	if _, statErr := os.Stat(snapDir); !os.IsNotExist(statErr) {
		t.Errorf("snapshot dir survived a write-failed abort (ordering bug: snapshot removal must precede the row write): stat err = %v", statErr)
	}
	// Restore the real writer, then confirm the row REMAINS (write failed) — the
	// safe reclaimable state, not a leaked snapshot-without-row.
	writeAdoptedEntriesFn = orig
	if _, found, _ := ReadAdoptProvenance(manifest); !found {
		t.Errorf("row was dropped despite the write failure (should remain for GC/UPSERT to reclaim)")
	}
}

// Finding 3 companion — the success path removes BOTH the snapshot and the row,
// and a second abort is an idempotent no-op.
func TestAbortRemovesBothSnapshotAndRow(t *testing.T) {
	isolateStateDir(t)
	manifest := "abortboth"
	seed := &AdoptedEntries{Version: 1, Records: []AdoptProvenanceRecord{{
		ManifestName: manifest, OperationState: AdoptOperationStateAdopting, UpdatedAt: time.Now().UTC(),
	}}}
	if err := writeAdoptedEntries(seed); err != nil {
		t.Fatal(err)
	}
	snapDir, _ := adoptSnapshotDir(manifest)
	if err := os.MkdirAll(snapDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(snapDir, "codex-cli.snapshot"), []byte("SECRET"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := abortAdoptProvenance(&AdoptProvenanceRecord{ManifestName: manifest}); err != nil {
		t.Fatalf("abort: %v", err)
	}
	if _, found, _ := ReadAdoptProvenance(manifest); found {
		t.Errorf("abort left the row")
	}
	if _, statErr := os.Stat(snapDir); !os.IsNotExist(statErr) {
		t.Errorf("abort left the snapshot dir: stat err = %v", statErr)
	}
	if err := abortAdoptProvenance(&AdoptProvenanceRecord{ManifestName: manifest}); err != nil {
		t.Errorf("second abort not idempotent: %v", err)
	}
}
