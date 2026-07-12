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

// Dual committed signals (bug 2026-07-11 P1-2, supersedes the former r3
// "manifest-independent" contract) — the classifier KEEPs on EITHER a live hub
// binding OR a manifest on disk (Signal 2b), and REAPs only when BOTH are absent:
//   - keepBindingM: manifest DELETED (never written), live hub binding at the ROW's
//     immutable captured port -> KEEP via the binding signal (still recognized from
//     the row's port, NOT the manifest file's edited contents).
//   - keepManifestM: NO live binding, manifest PRESENT -> KEEP via Signal 2b. This is
//     the fix: the former contract REAPED this "manifest present, no live binding"
//     row and destroyed a committed adopt's provenance after routine binding drift.
//   - reapM: NO live binding AND manifest ABSENT, no finalized client provenance
//     (anchor orphan) -> REAP (a true pre-install crash).
func TestGcClassifierManifestSignals(t *testing.T) {
	keepBindingM, keepManifestM, reapM := "gcsigbind", "gcsigmanifest", "gcsigreap"
	keepBindingPort, keepManifestPort, reapPort := 9411, 9412, 9413
	// codex holds a live hub binding ONLY for keepBindingM (at its port); it has NO
	// entry for keepManifestM or reapM (their bindings drifted / never installed).
	codexBody := fmt.Sprintf("[mcp_servers.%s]\nurl = \"http://127.0.0.1:%d/mcp\"\n",
		keepBindingM, keepBindingPort)
	setupAdoptTestEnv(t, keepBindingM, codexBody)
	// keepBindingM: manifest DELETED (never written) — kept purely by the live binding.
	seedAgedAdoptingRow(t, keepBindingM, withAdoptRowPort(keepBindingPort))
	// keepManifestM: NO live binding, manifest PRESENT (edited to an unrelated port —
	// contents are irrelevant, only existence matters). Signal 2b keeps it.
	writeAdoptManifestForClassifierTest(t, keepManifestM, 9999, "codex-cli")
	seedAgedAdoptingRow(t, keepManifestM, withAdoptRowPort(keepManifestPort))
	// reapM: NO live binding, manifest ABSENT, and no finalized client provenance.
	seedAgedAdoptingRow(t, reapM, withAdoptRowPort(reapPort), func(r *AdoptProvenanceRecord) { r.Clients = nil })

	reaped, err := gcOrphanedAdoptingProvenance(1 * time.Hour)
	if err != nil {
		t.Fatalf("gcOrphanedAdoptingProvenance: %v", err)
	}
	if _, found, _ := ReadAdoptProvenance(keepBindingM); !found {
		t.Errorf("keepBindingM (live binding at the ROW's port, manifest DELETED) was reaped; the binding signal must keep it")
	}
	if _, found, _ := ReadAdoptProvenance(keepManifestM); !found {
		t.Errorf("keepManifestM (NO live binding, manifest PRESENT) was reaped; Signal 2b must keep a committed-then-drifted row (this is the P1-2 fix)")
	}
	if _, found, _ := ReadAdoptProvenance(reapM); found {
		t.Errorf("reapM (no live binding AND no manifest) was NOT reaped")
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
