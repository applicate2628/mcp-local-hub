package api

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Bug 2026-07-11 P1-2 (Option B). The GC classifier had ONE committed signal — a
// live hub binding at the captured port. Routine drift ops (gate-ON reconcile,
// port-edit+reinstall, uninstall, demigrate) erase it, so a committed-but-unflipped
// `adopting` row fell to CRASH_REAP and its secret snapshots were destroyed ≥24h
// later (silent, unrecoverable P1 data loss). The fix adds a drift-proof
// manifest-exists KEEP signal (Signal 2b) AND, for the residual case-5 (a committed
// adopt whose manifest was also deleted), a GC-lane positive-crash-evidence gate:
// REAP demands POSITIVE proof the adopt mutated nothing, never mere absence.
//
// These tests live at the GC lane. The classifier is shared with the capture-UPSERT
// lane, which is exercised (non-regression) by
// TestAdoptUpsertReapsPriorOrphanNotGatedByUnmutatedCheck below +
// TestCaptureAdoptProvenanceUpsertReapsOrphan.

// Part 1 — a COMMITTED-but-`adopting` row whose live hub binding has DRIFTED away is
// KEPT because its manifest still exists on disk (no routine drift op deletes it).
// Three drift shapes, all with the manifest present => reaped 0, row + snapshot survive.
func TestAdoptGcCommittedDriftManifestPresentKeeps(t *testing.T) {
	cases := []struct {
		name      string
		manifest  string
		port      int
		codexBody string // codex config with NO matching live hub binding at `port`
	}{
		{
			name:     "gate-on-aggregate",
			manifest: "gcdriftgate",
			port:     9431,
			// gate-ON reconcile replaced the per-server entry with the mcphub-hub aggregate.
			codexBody: "[mcp_servers.mcphub-hub]\nurl = \"http://127.0.0.1:9500/clients/codex-cli/mcp\"\n",
		},
		{
			name:     "port-mismatch",
			manifest: "gcdriftport",
			port:     9432,
			// a manifest port-edit + reinstall rewrote the entry to a DIFFERENT port.
			codexBody: "[mcp_servers.gcdriftport]\nurl = \"http://127.0.0.1:9999/mcp\"\n",
		},
		{
			name:     "entry-removed",
			manifest: "gcdriftgone",
			port:     9433,
			// uninstall / demigrate removed the per-server entry entirely.
			codexBody: "[mcp_servers]\n",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			setupAdoptTestEnv(t, tc.manifest, tc.codexBody)
			// The committed adopt's manifest is still on disk (Signal 2b keep signal).
			writeAdoptManifestForClassifierTest(t, tc.manifest, tc.port, "codex-cli")
			seedAgedAdoptingRow(t, tc.manifest, withAdoptRowPort(tc.port))

			reaped, err := gcOrphanedAdoptingProvenance(1 * time.Hour)
			if err != nil {
				t.Fatalf("gc: %v", err)
			}
			if reaped != 0 {
				t.Errorf("reaped = %d, want 0 (manifest present => Signal 2b committed KEEP)", reaped)
			}
			if _, found, _ := ReadAdoptProvenance(tc.manifest); !found {
				t.Errorf("committed-but-drifted row (manifest present) was reaped; Signal 2b must keep it")
			}
			d, _ := adoptSnapshotDir(tc.manifest)
			if _, statErr := os.Stat(d); statErr != nil {
				t.Errorf("committed row's secret snapshot dir was destroyed: %v", statErr)
			}
		})
	}
}

// Part 2 (positive control) — a TRUE pre-install crash orphan still reaps: manifest
// ABSENT, no live binding, and the present client's live config is byte-frozen since
// capture (its whole-file sha256 == the recorded SnapshotSHA256) => Install never ran
// => reaped 1, row + snapshot gone.
func TestAdoptGcCrashOrphanUnmutatedReaps(t *testing.T) {
	entry := "gcunmutated"
	// Pre-adopt config: a plain stdio entry (NOT a hub binding). Install never ran, so
	// the live config is still exactly this.
	codexBody := "[mcp_servers." + entry + "]\ncommand = \"go\"\nargs = [\"version\"]\n"
	codexPath, _, _ := setupAdoptTestEnv(t, entry, codexBody)
	liveBytes, err := os.ReadFile(codexPath)
	if err != nil {
		t.Fatal(err)
	}
	sha := ManifestHashContent(liveBytes)

	seed := &AdoptedEntries{Version: 1, Records: []AdoptProvenanceRecord{{
		ManifestName:    entry,
		SourceEntryName: entry,
		AdoptClients:    []string{"codex-cli"},
		OperationState:  AdoptOperationStateAdopting,
		UpdatedAt:       time.Now().Add(-2 * time.Hour).UTC(),
		Clients: []AdoptClientProvenance{{
			Client: "codex-cli", OriginalState: AdoptOriginalStatePresent,
			SnapshotRef: "adopt-provenance/" + entry + "/codex-cli.snapshot", SnapshotSHA256: sha,
		}},
	}}}
	if err := writeAdoptedEntries(seed); err != nil {
		t.Fatal(err)
	}
	d, _ := adoptSnapshotDir(entry)
	if err := os.MkdirAll(d, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(d, "codex-cli.snapshot"), liveBytes, 0o600); err != nil {
		t.Fatal(err)
	}

	reaped, err := gcOrphanedAdoptingProvenance(1 * time.Hour)
	if err != nil {
		t.Fatalf("gc: %v", err)
	}
	if reaped != 1 {
		t.Errorf("reaped = %d, want 1 (manifest absent + config byte-frozen since capture => pre-install crash)", reaped)
	}
	if _, found, _ := ReadAdoptProvenance(entry); found {
		t.Errorf("pre-install-crash orphan was NOT reaped")
	}
	if _, statErr := os.Stat(d); !os.IsNotExist(statErr) {
		t.Errorf("snapshot dir survived the reap: %v", statErr)
	}
}

// Part 2 (case-5 closure, Option B) — a COMMITTED adopt whose manifest was later
// DELETED and whose hub bindings drifted looks byte-for-byte like a pre-install crash
// to the classifier (manifest absent + no live binding => CRASH_REAP). The positive
// crash-evidence gate distinguishes it: its client config was MUTATED by Install
// (live sha != recorded snapshot sha) => KEEP, preserving the secret snapshots for
// de-adopt. Includes a NON-VACUOUS proof (neutralize the gate => it reaps => the gate
// is what averted the data loss).
func TestAdoptGcCase5MutatedConfigKeeps(t *testing.T) {
	entry := "gccase5"
	// LIVE codex config = POST-adopt state; it holds NO hub binding for `entry` (the
	// binding drifted away and the operator deleted the manifest).
	codexBody := "[mcp_servers.someother]\ncommand = \"go\"\n"
	codexPath, _, _ := setupAdoptTestEnv(t, entry, codexBody)

	// The recorded snapshot sha is of the PRE-adopt bytes, which DIFFER from the live
	// (Install-mutated) config => this row is a committed adopt, not a pre-install crash.
	preAdoptSha := ManifestHashContent([]byte("[mcp_servers." + entry + "]\ncommand = \"go\"\nargs = [\"version\"]\n"))
	if live, _ := os.ReadFile(codexPath); ManifestHashContent(live) == preAdoptSha {
		t.Fatal("precondition: live config must DIFFER from the recorded snapshot sha")
	}

	seedCase5 := func() {
		seed := &AdoptedEntries{Version: 1, Records: []AdoptProvenanceRecord{{
			ManifestName:    entry,
			SourceEntryName: entry,
			AdoptClients:    []string{"codex-cli"},
			OperationState:  AdoptOperationStateAdopting,
			UpdatedAt:       time.Now().Add(-2 * time.Hour).UTC(),
			Clients: []AdoptClientProvenance{{
				Client: "codex-cli", OriginalState: AdoptOriginalStatePresent,
				SnapshotRef: "adopt-provenance/" + entry + "/codex-cli.snapshot", SnapshotSHA256: preAdoptSha,
			}},
		}}}
		if err := writeAdoptedEntries(seed); err != nil {
			t.Fatal(err)
		}
		d, _ := adoptSnapshotDir(entry)
		_ = os.MkdirAll(d, 0o700)
		_ = os.WriteFile(filepath.Join(d, "codex-cli.snapshot"), []byte("PRE-ADOPT-SECRET"), 0o600)
	}

	// (a) The real gate KEEPS the case-5 row.
	seedCase5()
	reaped, err := gcOrphanedAdoptingProvenance(1 * time.Hour)
	if err != nil {
		t.Fatalf("gc: %v", err)
	}
	if reaped != 0 {
		t.Errorf("reaped = %d, want 0 (case-5: committed adopt with a mutated config must be KEPT)", reaped)
	}
	if _, found, _ := ReadAdoptProvenance(entry); !found {
		t.Fatal("case-5 committed row was DESTROYED (the P1 data loss): the positive-unmutated gate must keep it")
	}
	d, _ := adoptSnapshotDir(entry)
	if _, statErr := os.Stat(d); statErr != nil {
		t.Errorf("case-5 committed row's secret snapshot was destroyed: %v", statErr)
	}

	// (b) NON-VACUOUS proof: neutralize the positive-unmutated gate and confirm the
	// SAME row now reaps => the gate (not some other guard) is what prevented the loss.
	orig := adoptRowProvablyUnmutatedFn
	adoptRowProvablyUnmutatedFn = func(AdoptProvenanceRecord) bool { return true }
	t.Cleanup(func() { adoptRowProvablyUnmutatedFn = orig })
	reaped2, err := gcOrphanedAdoptingProvenance(1 * time.Hour)
	if err != nil {
		t.Fatalf("gc (neutralized): %v", err)
	}
	if reaped2 != 1 {
		t.Errorf("neutralized reaped = %d, want 1 (proves the unmutated gate is the load-bearing keep)", reaped2)
	}
	if _, found, _ := ReadAdoptProvenance(entry); found {
		t.Error("neutralized gate did not reap the row; the case-5 test would be vacuous")
	}
}

// Part 1 fail-closed — a manifest STAT ERROR is treated as KEEP (REAP demands
// positive absence, never an unprovable one; destructive-default polarity).
func TestAdoptGcFailClosedManifestStatErrorKeeps(t *testing.T) {
	manifest := "gcstaterr"
	setupAdoptTestEnv(t, manifest, "[mcp_servers]\n")
	seedAgedAdoptingRow(t, manifest, withAdoptRowPort(9441))

	orig := adoptManifestExistsFn
	adoptManifestExistsFn = func(string) (bool, error) { return false, fmt.Errorf("induced stat error") }
	t.Cleanup(func() { adoptManifestExistsFn = orig })

	reaped, err := gcOrphanedAdoptingProvenance(1 * time.Hour)
	if err != nil {
		t.Fatalf("gc: %v", err)
	}
	if reaped != 0 {
		t.Errorf("reaped = %d, want 0 (a manifest stat error must fail closed to KEEP)", reaped)
	}
	if _, found, _ := ReadAdoptProvenance(manifest); !found {
		t.Errorf("row reaped despite an unprovable manifest stat (fail-closed => KEEP)")
	}
}

// Part 2 fail-closed — a present-at-capture client whose live config is UNREADABLE
// (its config file absent under the redirected HOME) cannot be sha-proven unmutated
// => KEEP.
func TestAdoptGcFailClosedSnapshotReadErrorKeeps(t *testing.T) {
	entry := "gcreaderr"
	setupAdoptTestEnv(t, entry, "[mcp_servers]\n") // codex present but no entry for `entry`
	// Row classifies CRASH_REAP (manifest absent, codex has no entry), but the recorded
	// present client is claude-code, whose config does NOT exist under the redirected
	// HOME => its live-config read errors => cannot prove unmutated => KEEP.
	seed := &AdoptedEntries{Version: 1, Records: []AdoptProvenanceRecord{{
		ManifestName:    entry,
		SourceEntryName: entry,
		AdoptClients:    []string{"codex-cli"},
		OperationState:  AdoptOperationStateAdopting,
		UpdatedAt:       time.Now().Add(-2 * time.Hour).UTC(),
		Clients: []AdoptClientProvenance{{
			Client: "claude-code", OriginalState: AdoptOriginalStatePresent,
			SnapshotRef: "adopt-provenance/" + entry + "/claude-code.snapshot", SnapshotSHA256: "deadbeef",
		}},
	}}}
	if err := writeAdoptedEntries(seed); err != nil {
		t.Fatal(err)
	}
	d, _ := adoptSnapshotDir(entry)
	_ = os.MkdirAll(d, 0o700)
	_ = os.WriteFile(filepath.Join(d, "claude-code.snapshot"), []byte("SECRET"), 0o600)

	reaped, err := gcOrphanedAdoptingProvenance(1 * time.Hour)
	if err != nil {
		t.Fatalf("gc: %v", err)
	}
	if reaped != 0 {
		t.Errorf("reaped = %d, want 0 (an unprovable present-client config must fail closed to KEEP)", reaped)
	}
	if _, found, _ := ReadAdoptProvenance(entry); !found {
		t.Errorf("row reaped despite an unprovable client config (fail-closed => KEEP)")
	}
}

// Part 3 — the mutation-point manifest guard REFUSES the reap and emits a DISTINCT
// audit event when a manifest re-appears inside the classify->reap window (a
// classifier regression or a concurrently re-created manifest). Uses the
// adoptGCBeforeReapHook seam to write the manifest deterministically after the
// candidate has already classified CRASH_REAP.
func TestAdoptGcMutationPointManifestGuardSkipsAndEmits(t *testing.T) {
	entry := "gcguard"
	_, _, stateRoot := setupAdoptTestEnv(t, entry, "[mcp_servers]\n")
	// Manifest ABSENT at classify time => the row classifies CRASH_REAP.
	seedAgedAdoptingRow(t, entry, withAdoptRowPort(9451))
	adoptGCBeforeReapHook = func() {
		adoptGCBeforeReapHook = nil // fire once
		writeAdoptManifestForClassifierTest(t, entry, 9451, "codex-cli")
	}
	t.Cleanup(func() { adoptGCBeforeReapHook = nil })

	reaped, err := gcOrphanedAdoptingProvenance(1 * time.Hour)
	if err != nil {
		t.Fatalf("gc: %v", err)
	}
	if reaped != 0 {
		t.Errorf("reaped = %d, want 0 (mutation-point guard must refuse once a manifest appears in the window)", reaped)
	}
	if _, found, _ := ReadAdoptProvenance(entry); !found {
		t.Errorf("row reaped despite the mutation-point manifest guard")
	}
	ev, _ := findSupervisorEventByName(t, filepath.Join(stateRoot, SupervisorEventLogFileLeaf), "adopt-provenance-reap-skipped-manifest-present")
	if ev == nil {
		t.Fatal("no adopt-provenance-reap-skipped-manifest-present event")
	}
	if ev["severity"] != SupervisorEventSeverityWarn {
		t.Errorf("skip event severity = %v, want warn", ev["severity"])
	}
	if ev["source"] != adoptProvenanceEventSource {
		t.Errorf("skip event source = %v, want %q", ev["source"], adoptProvenanceEventSource)
	}
	body, _ := ev["body"].(map[string]any)
	if body == nil || body["manifest"] != entry {
		t.Errorf("skip event body manifest = %v, want %q", body["manifest"], entry)
	}
	if _, ok := body["age_seconds"].(float64); !ok {
		t.Errorf("skip event missing numeric age_seconds: %v", body)
	}
}

// Part 1 non-regression — the capture-UPSERT lane is NOT gated by the GC-lane
// positive-unmutated check, and Signal 2b does not spuriously refuse an operator
// re-adopt (the manifest is absent at capture time). A prior aged `adopting` orphan
// whose present-client config is byte-identical to its snapshot (a genuine pre-install
// crash the GC lane would REAP via the positive-unmutated gate) is reaped-and-replaced
// by the classifier alone here — the UPSERT path never invokes that GC-only gate.
func TestAdoptUpsertReapsPriorOrphanNotGatedByUnmutatedCheck(t *testing.T) {
	entry := "gcupsertnoreg"
	codexBody := "[mcp_servers." + entry + "]\ncommand = \"go\"\nargs = [\"version\"]\n"
	codexPath, _, _ := setupAdoptTestEnv(t, entry, codexBody)
	liveBytes, _ := os.ReadFile(codexPath)
	sha := ManifestHashContent(liveBytes)

	prior := &AdoptedEntries{Version: 1, Records: []AdoptProvenanceRecord{{
		ManifestName:    entry,
		SourceEntryName: entry,
		AdoptClients:    []string{"codex-cli"},
		OperationState:  AdoptOperationStateAdopting,
		UpdatedAt:       time.Now().Add(-2 * time.Hour).UTC(),
		Clients: []AdoptClientProvenance{{
			Client: "codex-cli", OriginalState: AdoptOriginalStatePresent,
			SnapshotRef: "adopt-provenance/" + entry + "/codex-cli.snapshot", SnapshotSHA256: sha,
		}},
	}}}
	if err := writeAdoptedEntries(prior); err != nil {
		t.Fatal(err)
	}
	d, _ := adoptSnapshotDir(entry)
	_ = os.MkdirAll(d, 0o700)
	_ = os.WriteFile(filepath.Join(d, "codex-cli.snapshot"), []byte("PRIOR"), 0o600)

	port := nextBindableAdoptPortForTest(t, collectUsedAdoptPorts())
	api := NewAPI()
	plan, err := api.BuildAdoptPlan(AdoptOpts{EntryName: entry, Client: "codex-cli", ManifestName: entry, Port: port})
	if err != nil {
		t.Fatalf("BuildAdoptPlan: %v", err)
	}
	if _, err := api.captureAdoptProvenance(plan); err != nil {
		t.Fatalf("captureAdoptProvenance (UPSERT) must NOT be refused by Signal 2b: %v", err)
	}
	m, err := readAdoptedEntries()
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, r := range m.Records {
		if r.ManifestName == entry {
			count++
		}
	}
	if count != 1 {
		t.Errorf("rows for manifest = %d, want 1 (UPSERT reaps prior + writes fresh)", count)
	}
	if _, err := os.Stat(filepath.Join(d, "codex-cli.snapshot")); err != nil {
		t.Errorf("fresh codex-cli snapshot missing after UPSERT re-capture: %v", err)
	}
}

// P2-2 — an adopt <client>.snapshot path is classified secret-bearing (the pre-adopt
// config it pins may embed literal secret env values), so a read-broadened snapshot
// hard-fails like secrets.age instead of warn-and-proceeding.
func TestIsSecretBearingStateFilePathAdoptSnapshot(t *testing.T) {
	cases := []struct {
		path string
		want bool
	}{
		{filepath.Join("state", "adopt-provenance", "context7", "codex-cli.snapshot"), true},
		{"/var/state/adopt-provenance/mymanifest/claude-code.snapshot", true},
		{"codex-cli.snapshot", true},
		{"CODEX-CLI.SNAPSHOT", true}, // case-insensitive
		{"secrets.age", true},        // pre-existing behavior preserved
		{"supervisor-intent.json", false},
		{"managed-entries.json", false},
	}
	for _, tc := range cases {
		if got := isSecretBearingStateFilePath(tc.path); got != tc.want {
			t.Errorf("isSecretBearingStateFilePath(%q) = %v, want %v", tc.path, got, tc.want)
		}
	}
}
