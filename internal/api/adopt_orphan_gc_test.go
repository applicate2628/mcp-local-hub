package api

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// D1 — a STALE `adopting` row (aged updated_at) + its snapshot dir is reaped:
// row gone, snapshot dir gone, reaped == 1.
//
// This is a capture-ANCHOR orphan: no live hub binding, no manifest on disk, and no
// finalized per-client provenance (capture crashed before it wrote the client rows).
// Under the P1-2 positive-evidence contract that is a definitive pre-install crash —
// capture runs entirely before Install, so a Clients-less row has nothing Install
// could have mutated and is vacuously provably-unmutated => REAP. (A row WITH a
// finalized `present` client requires its live config to byte-match its snapshot
// before it reaps — see TestGcCase* / TestGcClassifierManifestSignals.)
func TestGcOrphanedReapsStaleAdoptingOrphan(t *testing.T) {
	isolateStateDir(t)
	manifest := "gcstale"
	seed := &AdoptedEntries{Version: 1, Records: []AdoptProvenanceRecord{{
		ManifestName:   manifest,
		OperationState: AdoptOperationStateAdopting,
		UpdatedAt:      time.Now().Add(-2 * time.Hour).UTC(),
	}}}
	if err := writeAdoptedEntries(seed); err != nil {
		t.Fatalf("seed store: %v", err)
	}
	dir, err := adoptSnapshotDir(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "codex-cli.snapshot"), []byte("SECRET"), 0o600); err != nil {
		t.Fatal(err)
	}

	reaped, err := gcOrphanedAdoptingProvenance(1 * time.Hour)
	if err != nil {
		t.Fatalf("gcOrphanedAdoptingProvenance: %v", err)
	}
	if reaped != 1 {
		t.Errorf("reaped = %d, want 1", reaped)
	}
	if _, found, err := ReadAdoptProvenance(manifest); err != nil {
		t.Fatalf("ReadAdoptProvenance: %v", err)
	} else if found {
		t.Errorf("stale `adopting` row survived GC")
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("stale snapshot dir survived GC: stat err = %v", err)
	}
}

// D2 — a FRESH `adopting` row and an (old) `adopted` row are BOTH preserved:
// reaped == 0 (GC never reaps a fresh adopting row nor any adopted row).
func TestGcOrphanedPreservesFreshAndAdopted(t *testing.T) {
	isolateStateDir(t)
	fresh := "gcfresh"
	done := "gcdone"
	seed := &AdoptedEntries{Version: 1, Records: []AdoptProvenanceRecord{
		{ManifestName: fresh, OperationState: AdoptOperationStateAdopting, UpdatedAt: time.Now().UTC()},
		// Deliberately OLD but `adopted` — GC must never touch an adopted row,
		// regardless of age.
		{ManifestName: done, OperationState: AdoptOperationStateAdopted, UpdatedAt: time.Now().Add(-48 * time.Hour).UTC()},
	}}
	if err := writeAdoptedEntries(seed); err != nil {
		t.Fatalf("seed store: %v", err)
	}
	reaped, err := gcOrphanedAdoptingProvenance(1 * time.Hour)
	if err != nil {
		t.Fatalf("gcOrphanedAdoptingProvenance: %v", err)
	}
	if reaped != 0 {
		t.Errorf("reaped = %d, want 0 (fresh adopting + old adopted are both preserved)", reaped)
	}
	if _, found, _ := ReadAdoptProvenance(fresh); !found {
		t.Errorf("fresh `adopting` row was reaped (must be preserved)")
	}
	if _, found, _ := ReadAdoptProvenance(done); !found {
		t.Errorf("`adopted` row was reaped (GC must never touch adopted, even when old)")
	}
}

// D3 — a step-0a GC failure is NON-FATAL: a real adopt still succeeds and still
// commits provenance.
func TestExecuteAdoptGCFailureNonFatal(t *testing.T) {
	entry := "mui-adopt-gc-nonfatal"
	setupAdoptTestEnv(t, entry, `[mcp_servers.mui-adopt-gc-nonfatal]
command = "go"
args = ["version"]
`)
	orig := gcOrphanedAdoptingProvenanceFn
	gcOrphanedAdoptingProvenanceFn = func(time.Duration) (int, error) { return 0, fmt.Errorf("induced gc failure") }
	t.Cleanup(func() { gcOrphanedAdoptingProvenanceFn = orig })

	port := nextBindableAdoptPortForTest(t, collectUsedAdoptPorts())
	plan, err := NewAPI().BuildAdoptPlan(AdoptOpts{EntryName: entry, Client: "codex-cli", ManifestName: entry, Port: port})
	if err != nil {
		t.Fatalf("BuildAdoptPlan: %v", err)
	}
	if err := NewAPI().ExecuteAdopt(plan, ioDiscardForAdoptTest{}); err != nil {
		t.Fatalf("ExecuteAdopt must succeed despite a non-fatal step-0a GC failure: %v", err)
	}
	if rec, found, err := ReadAdoptProvenance(entry); err != nil || !found {
		t.Fatalf("ReadAdoptProvenance found=%v err=%v", found, err)
	} else if rec.OperationState != AdoptOperationStateAdopted {
		t.Errorf("operation_state = %q, want adopted (adopt committed despite GC failure)", rec.OperationState)
	}
}

// D4 — the GC-reaped event carries trigger:"gc" (distinct from the upsert path's
// trigger:"upsert").
func TestGcOrphanedReapedEventCarriesGCTrigger(t *testing.T) {
	stateDir := isolateStateDir(t)
	manifest := "gctrigger"
	seed := &AdoptedEntries{Version: 1, Records: []AdoptProvenanceRecord{{
		ManifestName:   manifest,
		OperationState: AdoptOperationStateAdopting,
		UpdatedAt:      time.Now().Add(-3 * time.Hour).UTC(),
	}}}
	if err := writeAdoptedEntries(seed); err != nil {
		t.Fatalf("seed store: %v", err)
	}
	if _, err := gcOrphanedAdoptingProvenance(1 * time.Hour); err != nil {
		t.Fatalf("gcOrphanedAdoptingProvenance: %v", err)
	}

	ev, _ := findSupervisorEventByName(t, filepath.Join(stateDir, SupervisorEventLogFileLeaf), "adopt-provenance-orphan-reaped")
	if ev == nil {
		t.Fatal("no adopt-provenance-orphan-reaped event")
	}
	body, _ := ev["body"].(map[string]any)
	if body == nil {
		t.Fatalf("event body not an object: %v", ev["body"])
	}
	if body["trigger"] != "gc" {
		t.Errorf("orphan-reaped trigger = %v, want gc (distinct from the upsert path)", body["trigger"])
	}
	if body["manifest"] != manifest {
		t.Errorf("orphan-reaped manifest = %v, want %q", body["manifest"], manifest)
	}
}
