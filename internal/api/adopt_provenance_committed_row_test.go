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

// Finding A (r3) — the classifier is MANIFEST-INDEPENDENT: it reconstructs the
// expected hub binding from the ROW's IMMUTABLE captured port, so a committed row (a
// live hub binding) is KEPT even after its manifest is DELETED or EDITED (port
// changed). A row with NO live binding is REAPED regardless of manifest state. This
// SUPERSEDES the round-1/r2 manifest-existence keep/reap rule (the classifier no
// longer reads the manifest file). Rewritten from the former
// TestGcOrphanedPreservesCommittedButAdoptingRow, whose manifest-existence premise
// no longer applies.
func TestGcClassifierIsManifestIndependent(t *testing.T) {
	keepM, editM, reapM := "gcindepkeep", "gcindepedit", "gcindepreap"
	keepPort, editPort, reapPort := 9411, 9412, 9413
	// codex holds live hub bindings for keepM (port 9411) and editM (port 9412), and
	// NO entry for reapM.
	codexBody := fmt.Sprintf("[mcp_servers.%s]\nurl = \"http://127.0.0.1:%d/mcp\"\n\n[mcp_servers.%s]\nurl = \"http://127.0.0.1:%d/mcp\"\n",
		keepM, keepPort, editM, editPort)
	setupAdoptTestEnv(t, keepM, codexBody)
	// keepM: manifest DELETED (never written) — the classifier must not need it.
	seedAgedAdoptingRow(t, keepM, withAdoptRowPort(keepPort))
	// editM: manifest EDITED to a DIFFERENT port; the row keeps the captured port, so
	// the classifier still recognizes the live binding at the row's port.
	writeAdoptManifestForClassifierTest(t, editM, 9999, "codex-cli")
	seedAgedAdoptingRow(t, editM, withAdoptRowPort(editPort))
	// reapM: manifest present, but NO live hub binding -> true pre-install orphan.
	writeAdoptManifestForClassifierTest(t, reapM, reapPort, "codex-cli")
	seedAgedAdoptingRow(t, reapM, withAdoptRowPort(reapPort))

	reaped, err := gcOrphanedAdoptingProvenance(1 * time.Hour)
	if err != nil {
		t.Fatalf("gcOrphanedAdoptingProvenance: %v", err)
	}
	if _, found, _ := ReadAdoptProvenance(keepM); !found {
		t.Errorf("keepM (live binding, manifest DELETED) was reaped; the classifier must not read the manifest")
	}
	if _, found, _ := ReadAdoptProvenance(editM); !found {
		t.Errorf("editM (live binding at the ROW's port, manifest EDITED to a different port) was reaped; the classifier must use the row's immutable port")
	}
	if _, found, _ := ReadAdoptProvenance(reapM); found {
		t.Errorf("reapM (no live binding) was NOT reaped")
	}
	if reaped != 1 {
		t.Errorf("reaped = %d, want 1 (only reapM)", reaped)
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
