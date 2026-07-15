package api

import (
	"os"
	"testing"
	"time"
)

// TestAdoptGcGateOnCommittedManifestAbsentKeeps is the HEAD verification for bug
// 2026-07-11-classify-dead-adopting-row-gate-on-blind (open, high, data-loss). It sets up
// the bug's exact worst case: a gate-ON host where a committed-but-`adopting` provenance row
// has its manifest ALSO deleted (so classifyDeadAdoptingRow's Signal-2b manifest-present KEEP
// cannot fire), the per-server SourceEntryName has been replaced by the `mcphub-hub` aggregate
// (so the per-server committed signal reads false), and a PRESENT client holds a secret snapshot.
//
// The classifier misclassifies CRASH_REAP (the documented blind spot). The question this test
// answers: does the reap actually DESTROY the snapshots (bug open), or does the #551 write-target
// entry-shape predicate KEEP the row (data-loss closed by #551)? The predicate classifies
// SourceEntryName in the live config — under gate-ON that entry is ABSENT (only mcphub-hub is
// present), and for a PRESENT client an absent live entry with a non-nil snapshot is
// GenuineConflict, NOT RestoreDone, so adoptRowProvablyUnmutated must return false => KEEP.
func TestAdoptGcGateOnCommittedManifestAbsentKeeps(t *testing.T) {
	entry := "gateoncommitted"
	// gate-ON: gate-ON reconcile replaced the per-server entry with the mcphub-hub aggregate;
	// the per-server SourceEntryName is ABSENT from the live config.
	liveBytes := []byte("[mcp_servers.mcphub-hub]\nurl = \"http://127.0.0.1:9500/clients/codex-cli/mcp\"\n")
	setupAdoptTestEnv(t, entry, string(liveBytes))

	// The pre-adopt native snapshot for the source entry (the secret-bearing artifact de-adopt needs).
	snapshotBytes := []byte("[mcp_servers." + entry + "]\ncommand = \"go\"\nargs = [\"version\"]\n")
	ref, sha, err := writeAdoptClientSnapshot(entry, "codex-cli", snapshotBytes)
	if err != nil {
		t.Fatal(err)
	}

	// Manifest ABSENT (setupAdoptTestEnv creates the manifest ROOT but writes no manifest for
	// `entry`), so classifyDeadAdoptingRow's Signal-2b manifest-present KEEP cannot fire.
	rec := AdoptProvenanceRecord{
		ManifestName:    entry,
		SourceEntryName: entry,
		AdoptClients:    []string{"codex-cli"},
		OperationState:  AdoptOperationStateAdopting,
		UpdatedAt:       time.Now().Add(-2 * time.Hour).UTC(),
		Clients: []AdoptClientProvenance{{
			Client: "codex-cli", OriginalState: AdoptOriginalStatePresent,
			SnapshotRef: ref, SnapshotSHA256: sha,
		}},
	}
	if err := writeAdoptedEntries(&AdoptedEntries{Version: 1, Records: []AdoptProvenanceRecord{rec}}); err != nil {
		t.Fatal(err)
	}

	// Precondition — reproduce the classifier blind spot: no per-server binding + no manifest
	// => the classifier calls CRASH_REAP even though Install committed (gate-ON hid the binding).
	if classifyDeadAdoptingRow(rec) != adoptRowCrashReap {
		t.Fatalf("precondition: gate-ON committed row (manifest absent) should classify CRASH_REAP (the documented blind spot)")
	}

	// The load-bearing check: the #551 entry-shape predicate is the last gate before snapshot
	// destruction. It must KEEP (return false) because SourceEntryName is absent under gate-ON
	// (GenuineConflict for a present client), so the secret snapshots survive.
	if adoptRowProvablyUnmutated(rec) {
		t.Errorf("DATA-LOSS: the entry-shape predicate reports the gate-ON committed row provably-unmutated; " +
			"its secret snapshots would be reaped (bug 2026-07-11-classify-dead-adopting-row-gate-on-blind still open)")
	}

	reaped, err := gcOrphanedAdoptingProvenance(1 * time.Hour)
	if err != nil {
		t.Fatalf("gc: %v", err)
	}
	if reaped != 0 {
		t.Errorf("reaped = %d, want 0 (the gate-ON committed row's snapshots must be kept)", reaped)
	}
	if _, found, _ := ReadAdoptProvenance(entry); !found {
		t.Errorf("the gate-ON committed row was reaped; its secret snapshots are lost")
	}
	d, _ := adoptSnapshotDir(entry)
	if _, statErr := os.Stat(d); statErr != nil {
		t.Errorf("the gate-ON committed row's secret snapshot dir was destroyed: %v", statErr)
	}
}
