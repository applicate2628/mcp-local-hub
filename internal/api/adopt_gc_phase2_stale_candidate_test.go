package api

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Bug 2026-07-11 (P1) — GC Phase 2 must NEVER reap from its STALE Phase-1 candidate
// copy. A same-manifest re-adopt that COMMITS inside the Phase-1->Phase-2 gap
// replaces the aged `adopting` orphan with a fresh committed `adopted` row + new
// secret snapshots; reaping the stale copy (name-only, classified against the OLD
// captured port) would silently destroy exactly the provenance this feature exists
// to preserve. The under-lease re-read + identity gate must skip it.
//
// Deterministic interleave: adoptGCBeforePhase2Hook fires ONCE after Phase 1
// snapshots the candidate and before Phase 2 re-reads it, standing in for the
// concurrent re-adopt's completed UPSERT. WITHOUT the fix, Phase 2 classifies the
// stale `adopting`/oldPort copy (codex holds no entry for the manifest => CRASH_REAP)
// and reapAdoptProvenanceRow drops every name-matching row + its snapshot dir,
// destroying the fresh committed row — so this test FAILS on the unpatched code.
func TestGcPhase2SkipsStaleCandidateAfterConcurrentReadopt(t *testing.T) {
	manifest := "gcstalereadopt"
	oldPort, newPort := 9451, 9452
	// codex config present but WITHOUT an entry for the manifest, so classifyDead-
	// AdoptingRow returns CRASH_REAP on the stale row (arms the destructive path).
	setupAdoptTestEnv(t, manifest, "[mcp_servers]\n")

	// The aged `adopting` orphan Phase 1 selects (R_old, port oldPort) + its snapshot.
	seedAgedAdoptingRow(t, manifest, withAdoptRowPort(oldPort))

	// Simulate the concurrent re-adopt COMMITTING inside the Phase-1->Phase-2 gap:
	// replace R_old with a fresh committed `adopted` row (new UpdatedAt + new port)
	// and pin a NEW secret snapshot. Runs synchronously in the test goroutine.
	adoptGCBeforePhase2Hook = func() {
		adoptGCBeforePhase2Hook = nil // fire once
		store, err := readAdoptedEntries()
		if err != nil {
			t.Fatalf("hook read store: %v", err)
		}
		found := false
		for i := range store.Records {
			if store.Records[i].ManifestName == manifest {
				store.Records[i].OperationState = AdoptOperationStateAdopted
				store.Records[i].UpdatedAt = time.Now().UTC()
				store.Records[i].Port = newPort
				found = true
			}
		}
		if !found {
			t.Fatalf("hook: seeded row for %q vanished", manifest)
		}
		if err := writeAdoptedEntries(store); err != nil {
			t.Fatalf("hook write store: %v", err)
		}
		d, _ := adoptSnapshotDir(manifest)
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatalf("hook mkdir snapshot: %v", err)
		}
		if err := os.WriteFile(filepath.Join(d, "codex-cli.snapshot"), []byte("NEW-SECRET"), 0o600); err != nil {
			t.Fatalf("hook write snapshot: %v", err)
		}
	}
	t.Cleanup(func() { adoptGCBeforePhase2Hook = nil })

	reaped, err := gcOrphanedAdoptingProvenance(1 * time.Hour)
	if err != nil {
		t.Fatalf("gcOrphanedAdoptingProvenance: %v", err)
	}
	if reaped != 0 {
		t.Errorf("reaped = %d, want 0 (Phase 2 must skip the STALE candidate after the row was replaced by a fresh re-adopt)", reaped)
	}
	rec, found, rerr := ReadAdoptProvenance(manifest)
	if rerr != nil {
		t.Fatalf("ReadAdoptProvenance: %v", rerr)
	}
	if !found {
		t.Fatal("the fresh re-adopt's committed row was DESTROYED by the GC reaping from the stale Phase-1 candidate (the P1 data-destruction bug)")
	}
	if rec.OperationState != AdoptOperationStateAdopted {
		t.Errorf("row state = %q, want adopted (the fresh re-adopt's committed row)", rec.OperationState)
	}
	if rec.Port != newPort {
		t.Errorf("row port = %d, want %d (the fresh re-adopt's row)", rec.Port, newPort)
	}
	d, _ := adoptSnapshotDir(manifest)
	b, rdErr := os.ReadFile(filepath.Join(d, "codex-cli.snapshot"))
	if rdErr != nil {
		t.Errorf("fresh re-adopt's secret snapshot destroyed: %v", rdErr)
	} else if string(b) != "NEW-SECRET" {
		t.Errorf("fresh re-adopt's secret snapshot changed: got %q, want NEW-SECRET", b)
	}
}

// Bug 2026-07-11 (defense-in-depth) — reapAdoptProvenanceRow is identity-gated at the
// mutation point: it NO-OPs unless the LIVE row still matches the caller's expected
// (state, UpdatedAt). A state or UpdatedAt mismatch leaves the row + snapshot intact;
// only an exact identity match reaps (so the guard is not vacuous).
func TestReapAdoptProvenanceRowNoOpsOnIdentityMismatch(t *testing.T) {
	isolateStateDir(t)
	manifest := "reapident"
	ts := time.Now().Add(-2 * time.Hour).UTC()
	seed := &AdoptedEntries{Version: 1, Records: []AdoptProvenanceRecord{{
		ManifestName: manifest, OperationState: AdoptOperationStateAdopted, UpdatedAt: ts,
	}}}
	if err := writeAdoptedEntries(seed); err != nil {
		t.Fatal(err)
	}
	d, _ := adoptSnapshotDir(manifest)
	if err := os.MkdirAll(d, 0o700); err != nil {
		t.Fatal(err)
	}
	snapFile := filepath.Join(d, "codex-cli.snapshot")
	if err := os.WriteFile(snapFile, []byte("SECRET"), 0o600); err != nil {
		t.Fatal(err)
	}

	// (a) STATE mismatch: expected adopting, live is adopted => NO-OP.
	if err := reapAdoptProvenanceRow(manifest, AdoptOperationStateAdopting, ts); err != nil {
		t.Fatalf("reap (state mismatch) returned error: %v", err)
	}
	if _, found, _ := ReadAdoptProvenance(manifest); !found {
		t.Errorf("reap destroyed the row on a STATE mismatch (must no-op)")
	}
	if _, statErr := os.Stat(snapFile); statErr != nil {
		t.Errorf("reap destroyed the snapshot on a STATE mismatch: %v", statErr)
	}

	// (b) UpdatedAt mismatch: right state, wrong timestamp => NO-OP.
	if err := reapAdoptProvenanceRow(manifest, AdoptOperationStateAdopted, ts.Add(-time.Hour)); err != nil {
		t.Fatalf("reap (updatedat mismatch) returned error: %v", err)
	}
	if _, found, _ := ReadAdoptProvenance(manifest); !found {
		t.Errorf("reap destroyed the row on an UpdatedAt mismatch (must no-op)")
	}
	if _, statErr := os.Stat(snapFile); statErr != nil {
		t.Errorf("reap destroyed the snapshot on an UpdatedAt mismatch: %v", statErr)
	}

	// (c) Positive control — EXACT identity match reaps (guard isn't vacuous). Use the
	// round-tripped persisted UpdatedAt so the .Equal match is exact.
	persisted, found, _ := ReadAdoptProvenance(manifest)
	if !found {
		t.Fatal("row missing before the positive-control reap")
	}
	if err := reapAdoptProvenanceRow(manifest, AdoptOperationStateAdopted, persisted.UpdatedAt); err != nil {
		t.Fatalf("reap (exact identity match) returned error: %v", err)
	}
	if _, found, _ := ReadAdoptProvenance(manifest); found {
		t.Errorf("reap did NOT remove the row on an exact identity match")
	}
	if _, statErr := os.Stat(d); !os.IsNotExist(statErr) {
		t.Errorf("reap did NOT remove the snapshot dir on an exact identity match: %v", statErr)
	}
}
