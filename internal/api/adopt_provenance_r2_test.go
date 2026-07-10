package api

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// writeAdoptManifestForClassifierTest writes a VALID adopt-shaped manifest to the
// on-disk manifest dir so classifyDeadAdoptingRow can parse it and find a binding.
func writeAdoptManifestForClassifierTest(t *testing.T, manifest string, port int, client string) {
	t.Helper()
	yaml := renderStdioBridgeManifestYAML(manifest, "go", []string{"version"}, nil, port, adoptClientBindings([]string{client}))
	dir := filepath.Join(defaultManifestDir(), manifest)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	if err := os.WriteFile(filepath.Join(dir, "manifest.yaml"), []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
}

func seedAgedAdoptingRow(t *testing.T, manifest string, extra ...func(*AdoptProvenanceRecord)) {
	t.Helper()
	store, err := readAdoptedEntries()
	if err != nil {
		t.Fatalf("read store: %v", err)
	}
	rec := AdoptProvenanceRecord{
		ManifestName:    manifest,
		SourceEntryName: manifest,
		AdoptClients:    []string{"codex-cli"},
		OperationState:  AdoptOperationStateAdopting,
		UpdatedAt:       time.Now().Add(-2 * time.Hour).UTC(),
	}
	for _, f := range extra {
		f(&rec)
	}
	store.Records = append(store.Records, rec)
	if err := writeAdoptedEntries(store); err != nil {
		t.Fatalf("write store: %v", err)
	}
	d, _ := adoptSnapshotDir(manifest)
	_ = os.MkdirAll(d, 0o700)
	_ = os.WriteFile(filepath.Join(d, "codex-cli.snapshot"), []byte("SECRET"), 0o600)
}

func withAdoptRowPort(p int) func(*AdoptProvenanceRecord) {
	return func(r *AdoptProvenanceRecord) { r.Port = p }
}

// Claim 17 (finding 2) — the classifier KEEPS a committed row (a live hub binding
// exists, derived from the ROW's immutable port) and REAPS a pre-install orphan (NO
// live binding). SUPERSEDES the round-1 "manifest EXISTS => keep" rule; no manifest
// file is consulted (see TestGcClassifierIsManifestIndependent).
func TestGcClassifierLiveBindingKeepsValidNoBindingReaps(t *testing.T) {
	keepM, reapM := "gckeepbind", "gcreapnobind"
	keepPort, reapPort := 9401, 9402
	// codex config carries ONLY the keepM hub-URL entry => a LIVE binding for keepM.
	codexBody := "[mcp_servers." + keepM + "]\nurl = \"http://127.0.0.1:" + strconv.Itoa(keepPort) + "/mcp\"\n"
	setupAdoptTestEnv(t, keepM, codexBody)
	seedAgedAdoptingRow(t, keepM, withAdoptRowPort(keepPort))
	seedAgedAdoptingRow(t, reapM, withAdoptRowPort(reapPort))

	reaped, err := gcOrphanedAdoptingProvenance(1 * time.Hour)
	if err != nil {
		t.Fatalf("gc: %v", err)
	}
	if _, found, _ := ReadAdoptProvenance(keepM); !found {
		t.Errorf("keepM (live hub binding) was reaped; COMMITTED_KEEP must preserve it")
	}
	if _, found, _ := ReadAdoptProvenance(reapM); found {
		t.Errorf("reapM (valid manifest, NO live binding) was NOT reaped; CRASH_REAP must remove it")
	}
	if reaped != 1 {
		t.Errorf("reaped = %d, want 1 (only reapM)", reaped)
	}
}

// Claim 16 (finding 1) — a second same-manifest adopt FAILs CLOSED while the first
// holds the lease, and the first's row + snapshot are byte-intact (never reaped).
func TestConcurrentSameManifestAdoptFailsClosed(t *testing.T) {
	entry := "leasecc"
	setupAdoptTestEnv(t, entry, `[mcp_servers.leasecc]
command = "go"
args = ["version"]
`)
	// Simulate a live in-flight adopt for `entry` by holding its lease.
	lk, ok, err := tryAcquireAdoptManifestLease(entry)
	if err != nil || !ok {
		t.Fatalf("hold lease: ok=%v err=%v", ok, err)
	}
	defer func() { _ = lk.Unlock() }()

	// The first adopt's LIVE (fresh) row + snapshot.
	seed := &AdoptedEntries{Version: 1, Records: []AdoptProvenanceRecord{{
		ManifestName: entry, SourceEntryName: entry, AdoptClients: []string{"codex-cli"},
		OperationState: AdoptOperationStateAdopting, UpdatedAt: time.Now().UTC(),
		Clients: []AdoptClientProvenance{{Client: "codex-cli", OriginalState: AdoptOriginalStatePresent, SnapshotRef: "adopt-provenance/leasecc/codex-cli.snapshot", SnapshotSHA256: "sha"}},
	}}}
	if err := writeAdoptedEntries(seed); err != nil {
		t.Fatal(err)
	}
	snapDir, _ := adoptSnapshotDir(entry)
	_ = os.MkdirAll(snapDir, 0o700)
	snapFile := filepath.Join(snapDir, "codex-cli.snapshot")
	_ = os.WriteFile(snapFile, []byte("FIRST-SECRET"), 0o600)

	port := nextBindableAdoptPortForTest(t, collectUsedAdoptPorts())
	plan, err := NewAPI().BuildAdoptPlan(AdoptOpts{EntryName: entry, Client: "codex-cli", ManifestName: entry, Port: port})
	if err != nil {
		t.Fatalf("BuildAdoptPlan: %v", err)
	}
	if err := NewAPI().ExecuteAdopt(plan, ioDiscardForAdoptTest{}); err == nil {
		t.Fatal("second same-manifest adopt succeeded; want fail-closed on the lease")
	} else if !strings.Contains(err.Error(), "concurrent adopt") {
		t.Errorf("error should name the concurrent-adopt refusal: %v", err)
	}
	if _, found, _ := ReadAdoptProvenance(entry); !found {
		t.Errorf("first adopt's LIVE row was reaped by the second (lease must protect it)")
	}
	if b, _ := os.ReadFile(snapFile); string(b) != "FIRST-SECRET" {
		t.Errorf("first adopt's snapshot was destroyed: %q", b)
	}
}

// Claim 16 (GC side) — the GC SKIPs an aged `adopting` row whose lease is held
// (a live but slow adopt); its row + snapshot survive.
func TestGcSkipsLeasedManifest(t *testing.T) {
	isolateStateDir(t)
	manifest := "gcleased"
	seedAgedAdoptingRow(t, manifest)
	lk, ok, err := tryAcquireAdoptManifestLease(manifest)
	if err != nil || !ok {
		t.Fatalf("hold lease: ok=%v err=%v", ok, err)
	}
	defer func() { _ = lk.Unlock() }()

	reaped, err := gcOrphanedAdoptingProvenance(1 * time.Hour)
	if err != nil {
		t.Fatalf("gc: %v", err)
	}
	if reaped != 0 {
		t.Errorf("reaped = %d, want 0 (a leased manifest must be skipped)", reaped)
	}
	if _, found, _ := ReadAdoptProvenance(manifest); !found {
		t.Errorf("leased manifest's row was reaped")
	}
	d, _ := adoptSnapshotDir(manifest)
	if _, statErr := os.Stat(d); statErr != nil {
		t.Errorf("leased manifest's snapshot dir was removed: %v", statErr)
	}
}

// Claim 18 (finding 3 backstop) — the GC's snapshot-dir scan reaps a ROWLESS
// <manifest>/ dir (no store row) under a free lease, and SKIPs a rowless dir whose
// lease is held.
func TestGcReapsRowlessSnapshotDir(t *testing.T) {
	isolateStateDir(t)
	// Rowless dir with a FREE lease -> reaped.
	freeM := "rowlessfree"
	freeDir, _ := adoptSnapshotDir(freeM)
	_ = os.MkdirAll(freeDir, 0o700)
	_ = os.WriteFile(filepath.Join(freeDir, "codex-cli.snapshot"), []byte("SECRET"), 0o600)
	// Rowless dir whose lease is HELD -> skipped.
	heldM := "rowlessheld"
	heldDir, _ := adoptSnapshotDir(heldM)
	_ = os.MkdirAll(heldDir, 0o700)
	_ = os.WriteFile(filepath.Join(heldDir, "codex-cli.snapshot"), []byte("SECRET"), 0o600)
	lk, ok, err := tryAcquireAdoptManifestLease(heldM)
	if err != nil || !ok {
		t.Fatalf("hold lease: ok=%v err=%v", ok, err)
	}
	defer func() { _ = lk.Unlock() }()

	reaped, err := gcOrphanedAdoptingProvenance(1 * time.Hour)
	if err != nil {
		t.Fatalf("gc: %v", err)
	}
	if reaped != 1 {
		t.Errorf("reaped = %d, want 1 (only the free rowless dir)", reaped)
	}
	if _, statErr := os.Stat(freeDir); !os.IsNotExist(statErr) {
		t.Errorf("free rowless snapshot dir survived the GC: %v", statErr)
	}
	if _, statErr := os.Stat(heldDir); statErr != nil {
		t.Errorf("held rowless snapshot dir was removed despite the lease: %v", statErr)
	}
}

// Claim 19 (finding 4) + row-first anchor — a partial-cleanup failure during capture
// is SURFACED (not swallowed), and the ANCHOR row survives (proving it was written
// BEFORE the snapshots, which the abort removed), remaining GC-reclaimable.
func TestCaptureCleanupErrorSurfacedRowFirstAnchorRemains(t *testing.T) {
	entry := "rowfirst"
	codexPath, _, _ := setupAdoptTestEnv(t, entry, `[mcp_servers.rowfirst]
command = "go"
args = ["version"]
`)
	home := filepath.Dir(filepath.Dir(codexPath))
	cursorPath := filepath.Join(home, ".cursor", "mcp.json")
	if err := os.MkdirAll(filepath.Dir(cursorPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cursorPath, []byte(`{"mcpServers": {`), 0o600); err != nil { // malformed -> capture fails at c2
		t.Fatal(err)
	}
	// Inject a store-write failure so the abort's row-drop fails AFTER it removed the
	// snapshots — the anchor row then remains.
	orig := writeAdoptedEntriesFn
	writeAdoptedEntriesFn = func(*AdoptedEntries) error { return fmt.Errorf("induced abort write failure") }
	t.Cleanup(func() { writeAdoptedEntriesFn = orig })

	plan := &AdoptPlan{
		EntryName: entry, SourceClient: "codex-cli", ManifestName: entry,
		AdoptClients: []string{"codex-cli", "cursor"},
		ManifestYAML: "name: " + entry + "\n",
	}
	_, err := NewAPI().captureAdoptProvenance(plan)
	if err == nil {
		t.Fatal("capture succeeded despite a corrupt client; want a fail-closed error")
	}
	if !strings.Contains(err.Error(), "cleanup failed") {
		t.Errorf("capture error must SURFACE the cleanup failure (finding 4): %v", err)
	}
	// Row-first: the ANCHOR row exists (c1 wrote it before c2's snapshots); the abort
	// removed the snapshots FIRST (so no rowless secret dir survives).
	rec, found, rErr := ReadAdoptProvenance(entry)
	if rErr != nil || !found {
		t.Fatalf("anchor row missing (row-first requires it to survive a failed cleanup): found=%v err=%v", found, rErr)
	}
	if rec.OperationState != AdoptOperationStateAdopting {
		t.Errorf("anchor row state = %q, want adopting", rec.OperationState)
	}
	d, _ := adoptSnapshotDir(entry)
	if _, statErr := os.Stat(d); !os.IsNotExist(statErr) {
		t.Errorf("snapshot dir survived (abort must remove snapshots before failing on the row write): %v", statErr)
	}
	// The anchor row is GC-reclaimable once the store write recovers AND the clients
	// are cleanly readable with NO live hub binding (the manifest-independent
	// classifier reaps only when every adopt_client is readable and none holds the
	// hub entry). Repair the corrupt cursor to a clean empty config — a read error
	// would fail-safe KEEP (finding B). codex still holds only the pre-adopt STDIO
	// entry (no hub URL), so neither client has a live hub binding.
	writeAdoptedEntriesFn = orig
	if err := os.WriteFile(cursorPath, []byte(`{"mcpServers":{}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	// Age the anchor so the GC's Phase-2 age gate considers it.
	ageAdoptRow(t, entry)
	if reaped, gErr := gcOrphanedAdoptingProvenance(1 * time.Hour); gErr != nil {
		t.Fatalf("gc: %v", gErr)
	} else if reaped != 1 {
		t.Errorf("GC reaped = %d, want 1 (the reclaimable anchor)", reaped)
	}
	if _, found, _ := ReadAdoptProvenance(entry); found {
		t.Errorf("anchor row was not reclaimed by the GC")
	}
}

// Claim 20 (finding 5) — a MiMoCode client whose entry resolves from a LOWER read
// layer (config.json) while the hub WRITE TARGET (mimocode.json) is ABSENT captures
// as `present-merged-lower` with NO snapshot, and the adopt does NOT fail.
func TestCaptureMimoCodePresentMergedLower(t *testing.T) {
	entry := "mimomerged"
	codexPath, _, _ := setupAdoptTestEnv(t, entry, `[mcp_servers.mimomerged]
command = "go"
args = ["version"]
`)
	t.Setenv("MIMOCODE_HOME", "")
	t.Setenv("XDG_CONFIG_HOME", "")
	home := filepath.Dir(filepath.Dir(codexPath))
	mimoDir := filepath.Join(home, ".config", "mimocode")
	if err := os.MkdirAll(mimoDir, 0o700); err != nil {
		t.Fatal(err)
	}
	// config.json (a LOWER read layer) carries the entry; mimocode.json (the WRITE
	// target = ConfigPath) is ABSENT.
	if err := os.WriteFile(filepath.Join(mimoDir, "config.json"), []byte(`{"mcp":{"mimomerged":{"type":"local","command":["go","version"]}}}`), 0o600); err != nil {
		t.Fatal(err)
	}

	plan := &AdoptPlan{
		EntryName: entry, SourceClient: "mimocode", ManifestName: entry,
		AdoptClients: []string{"mimocode"},
		ManifestYAML: "name: " + entry + "\n",
	}
	rec, err := NewAPI().captureAdoptProvenance(plan)
	if err != nil {
		t.Fatalf("capture must SUCCEED for a present-merged-lower client: %v", err)
	}
	var mimo *AdoptClientProvenance
	for i := range rec.Clients {
		if rec.Clients[i].Client == "mimocode" {
			mimo = &rec.Clients[i]
		}
	}
	if mimo == nil {
		t.Fatalf("mimocode client not recorded; clients=%+v", rec.Clients)
	}
	if mimo.OriginalState != AdoptOriginalStatePresentMergedLower {
		t.Errorf("mimocode original_state = %q, want present-merged-lower", mimo.OriginalState)
	}
	if mimo.SnapshotRef != "" || mimo.SnapshotSHA256 != "" {
		t.Errorf("present-merged-lower must have NO snapshot: ref=%q sha=%q", mimo.SnapshotRef, mimo.SnapshotSHA256)
	}
}

func ageAdoptRow(t *testing.T, manifest string) {
	t.Helper()
	store, err := readAdoptedEntries()
	if err != nil {
		t.Fatal(err)
	}
	for i := range store.Records {
		if store.Records[i].ManifestName == manifest {
			store.Records[i].UpdatedAt = time.Now().Add(-2 * time.Hour).UTC()
		}
	}
	if err := writeAdoptedEntries(store); err != nil {
		t.Fatal(err)
	}
}

// Finding B (r3) — an `adopting` row whose adopt_client is UNREADABLE at classify
// time is KEPT (a read error cannot DISPROVE a live binding), never reaped.
func TestGcClassifierUnreadableClientKeeps(t *testing.T) {
	entry := "gcunreadable"
	codexPath, _, _ := setupAdoptTestEnv(t, entry, "[mcp_servers.gcunreadable]\ncommand = \"go\"\n")
	// Corrupt codex so GetEntry ERRORS (not a clean no-entry) at classify time.
	if err := os.WriteFile(codexPath, []byte("this is not valid toml {{{"), 0o600); err != nil {
		t.Fatal(err)
	}
	seedAgedAdoptingRow(t, entry, withAdoptRowPort(9421))

	reaped, err := gcOrphanedAdoptingProvenance(1 * time.Hour)
	if err != nil {
		t.Fatalf("gc: %v", err)
	}
	if reaped != 0 {
		t.Errorf("reaped = %d, want 0 (an unreadable adopt_client => fail-safe KEEP)", reaped)
	}
	if _, found, _ := ReadAdoptProvenance(entry); !found {
		t.Errorf("row with an unreadable adopt_client was reaped (finding B: read error => KEEP)")
	}
}

// Finding C (r3) / finding 3 (r4) — a config edited to REMOVE the entry between
// GetEntry and the snapshot ReadFile makes capture FAIL CLOSED. As of r4 the guard
// is adapter.EntryPresentInBytes over the EXACT snapshot bytes (not a fresh GetEntry
// re-read), so a delete-then-recreate race cannot slip an entry-less snapshot past
// it: the bytes that get validated are the same bytes that get pinned. No snapshot
// inconsistent with the recorded `present` state, no committed row.
func TestCaptureFailsClosedOnConfigEditDuringSnapshot(t *testing.T) {
	entry := "captoctou"
	codexPath, _, _ := setupAdoptTestEnv(t, entry, `[mcp_servers.captoctou]
command = "go"
args = ["version"]
`)
	// Simulate a concurrent editor removing the entry AFTER GetEntry (present) but
	// BEFORE the snapshot ReadFile.
	orig := adoptCaptureBeforeSnapshotReadHook
	adoptCaptureBeforeSnapshotReadHook = func(client string) {
		if client == "codex-cli" {
			_ = os.WriteFile(codexPath, []byte("[mcp_servers]\n"), 0o600) // entry removed
			adoptCaptureBeforeSnapshotReadHook = nil                      // fire once
		}
	}
	t.Cleanup(func() { adoptCaptureBeforeSnapshotReadHook = orig })

	plan := &AdoptPlan{
		EntryName: entry, SourceClient: "codex-cli", ManifestName: entry,
		AdoptClients: []string{"codex-cli"},
		ManifestYAML: "name: " + entry + "\n",
	}
	if _, err := NewAPI().captureAdoptProvenance(plan); err == nil {
		t.Fatal("capture succeeded despite the entry being removed during the snapshot read; want fail-closed")
	} else if !strings.Contains(err.Error(), "changed during") {
		t.Errorf("error should name the config-changed-during-snapshot failure: %v", err)
	}
	// No committed row, no snapshot (the abort cleaned the anchor + partial dir).
	if rec, found, _ := ReadAdoptProvenance(entry); found {
		t.Errorf("capture-TOCTOU-failed adopt left a committed row: %+v", rec)
	}
	d, _ := adoptSnapshotDir(entry)
	if _, statErr := os.Stat(d); !os.IsNotExist(statErr) {
		t.Errorf("capture-TOCTOU-failed adopt left a snapshot dir: %v", statErr)
	}
}

// Finding 1 (r4) — present-merged-lower is keyed on SourceBelowWriteTarget, NOT on a
// missing ConfigPath. A MiMoCode entry that resolves from the LOWER config.json layer
// while the write target (mimocode.json = ConfigPath) EXISTS (holding an unrelated
// entry) is still present-merged-lower with NO snapshot.
func TestCaptureMimoCodeMergedLowerWithWriteTargetPresent(t *testing.T) {
	entry := "mimomergedlive"
	codexPath, _, _ := setupAdoptTestEnv(t, entry, `[mcp_servers.mimomergedlive]
command = "go"
args = ["version"]
`)
	t.Setenv("MIMOCODE_HOME", "")
	t.Setenv("XDG_CONFIG_HOME", "")
	home := filepath.Dir(filepath.Dir(codexPath))
	mimoDir := filepath.Join(home, ".config", "mimocode")
	if err := os.MkdirAll(mimoDir, 0o700); err != nil {
		t.Fatal(err)
	}
	// config.json (a LOWER read layer) carries the ADOPTED entry.
	if err := os.WriteFile(filepath.Join(mimoDir, "config.json"), []byte(`{"mcp":{"mimomergedlive":{"type":"local","command":["go","version"]}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	// mimocode.json (the WRITE TARGET = ConfigPath) EXISTS but holds a DIFFERENT
	// entry — the adopted entry is BELOW an existing write target. Old ConfigPath-ENOENT
	// keying would have mis-routed this to `present` + snapshot; SourceBelowWriteTarget
	// keying keeps it present-merged-lower.
	if err := os.WriteFile(filepath.Join(mimoDir, "mimocode.json"), []byte(`{"mcp":{"someotherserver":{"type":"local","command":["true"]}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	plan := &AdoptPlan{
		EntryName: entry, SourceClient: "mimocode", ManifestName: entry,
		AdoptClients: []string{"mimocode"},
		ManifestYAML: "name: " + entry + "\n",
	}
	rec, err := NewAPI().captureAdoptProvenance(plan)
	if err != nil {
		t.Fatalf("capture must SUCCEED for a present-merged-lower client with an EXISTING write target: %v", err)
	}
	var mimo *AdoptClientProvenance
	for i := range rec.Clients {
		if rec.Clients[i].Client == "mimocode" {
			mimo = &rec.Clients[i]
		}
	}
	if mimo == nil {
		t.Fatalf("mimocode client not recorded; clients=%+v", rec.Clients)
	}
	if mimo.OriginalState != AdoptOriginalStatePresentMergedLower {
		t.Errorf("mimocode original_state = %q, want present-merged-lower (entry below an EXISTING write target)", mimo.OriginalState)
	}
	if mimo.SnapshotRef != "" || mimo.SnapshotSHA256 != "" {
		t.Errorf("present-merged-lower must have NO snapshot: ref=%q sha=%q", mimo.SnapshotRef, mimo.SnapshotSHA256)
	}
}

// Finding 2 (r4) — a SourceBelowWriteTarget==false entry (single-file client: the
// entry lives IN the write target) whose ConfigPath vanishes between GetEntry and the
// snapshot ReadFile must FAIL CLOSED, never be guessed present-merged-lower (which is
// now reserved exclusively for SourceBelowWriteTarget==true).
func TestCaptureFailsClosedWhenWriteTargetConfigVanishes(t *testing.T) {
	entry := "capvanish"
	codexPath, _, _ := setupAdoptTestEnv(t, entry, `[mcp_servers.capvanish]
command = "go"
args = ["version"]
`)
	orig := adoptCaptureBeforeSnapshotReadHook
	adoptCaptureBeforeSnapshotReadHook = func(client string) {
		if client == "codex-cli" {
			_ = os.Remove(codexPath) // whole config gone -> ConfigPath ENOENT
			adoptCaptureBeforeSnapshotReadHook = nil
		}
	}
	t.Cleanup(func() { adoptCaptureBeforeSnapshotReadHook = orig })

	plan := &AdoptPlan{
		EntryName: entry, SourceClient: "codex-cli", ManifestName: entry,
		AdoptClients: []string{"codex-cli"},
		ManifestYAML: "name: " + entry + "\n",
	}
	if _, err := NewAPI().captureAdoptProvenance(plan); err == nil {
		t.Fatal("capture succeeded despite the write-target config vanishing; want fail-closed")
	} else if !strings.Contains(err.Error(), "disappeared during capture") {
		t.Errorf("error should name the vanished-write-target fail-closed: %v", err)
	}
	if rec, found, _ := ReadAdoptProvenance(entry); found {
		t.Errorf("vanished-config capture left a committed row: %+v", rec)
	}
}
