package api

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// B2 — both manifest hashes are populated AT CAPTURE from plan.ManifestYAML,
// BEFORE any promote (arch F1).
func TestCaptureAdoptProvenanceHashesAtCapture(t *testing.T) {
	entry := "mui-adopt-prov-hash"
	setupAdoptTestEnv(t, entry, `[mcp_servers.mui-adopt-prov-hash]
command = "go"
args = ["version"]
`)
	port := nextBindableAdoptPortForTest(t, collectUsedAdoptPorts())
	api := NewAPI()
	plan, err := api.BuildAdoptPlan(AdoptOpts{EntryName: entry, Client: "codex-cli", ManifestName: entry, Port: port})
	if err != nil {
		t.Fatalf("BuildAdoptPlan: %v", err)
	}
	rec, err := api.captureAdoptProvenance(plan)
	if err != nil {
		t.Fatalf("captureAdoptProvenance: %v", err)
	}
	wantHash := ManifestHashContent([]byte(plan.ManifestYAML))
	if wantHash == "" {
		t.Fatal("manifest hash empty")
	}
	if rec.AdoptManifestHash != wantHash {
		t.Errorf("returned rec.AdoptManifestHash = %q, want %q", rec.AdoptManifestHash, wantHash)
	}

	// Read from disk (not the in-memory rec) to prove durability.
	got, found, err := ReadAdoptProvenance(entry)
	if err != nil || !found {
		t.Fatalf("ReadAdoptProvenance found=%v err=%v", found, err)
	}
	if got.OperationState != AdoptOperationStateAdopting {
		t.Errorf("operation_state = %q, want adopting (no promote yet)", got.OperationState)
	}
	if got.AdoptManifestHash != wantHash || got.ExpectedManifestHash != wantHash {
		t.Errorf("hashes = (%q,%q), want both == %q (F1)", got.AdoptManifestHash, got.ExpectedManifestHash, wantHash)
	}
}

// B3 — capture UPSERT reaps a prior orphan row + stale snapshot dir; exactly one
// row for the manifest remains.
func TestCaptureAdoptProvenanceUpsertReapsOrphan(t *testing.T) {
	entry := "mui-adopt-prov-upsert"
	setupAdoptTestEnv(t, entry, `[mcp_servers.mui-adopt-prov-upsert]
command = "go"
args = ["version"]
`)
	// Pre-seed a stale orphan: an adopting row + a stale snapshot dir with a file.
	stale := &AdoptedEntries{Version: 1, Records: []AdoptProvenanceRecord{{
		ManifestName:   entry,
		OperationState: AdoptOperationStateAdopting,
		UpdatedAt:      time.Now().Add(-2 * time.Hour).UTC(),
		Clients:        []AdoptClientProvenance{{Client: "old-client", OriginalState: AdoptOriginalStatePresent}},
	}}}
	if err := writeAdoptedEntries(stale); err != nil {
		t.Fatalf("seed stale store: %v", err)
	}
	dir, err := adoptSnapshotDir(entry)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	staleFile := filepath.Join(dir, "old-client.snapshot")
	if err := os.WriteFile(staleFile, []byte("STALE"), 0o600); err != nil {
		t.Fatal(err)
	}

	port := nextBindableAdoptPortForTest(t, collectUsedAdoptPorts())
	api := NewAPI()
	plan, err := api.BuildAdoptPlan(AdoptOpts{EntryName: entry, Client: "codex-cli", ManifestName: entry, Port: port})
	if err != nil {
		t.Fatalf("BuildAdoptPlan: %v", err)
	}
	if _, err := api.captureAdoptProvenance(plan); err != nil {
		t.Fatalf("captureAdoptProvenance: %v", err)
	}

	m, err := readAdoptedEntries()
	if err != nil {
		t.Fatalf("readAdoptedEntries: %v", err)
	}
	count := 0
	for _, r := range m.Records {
		if r.ManifestName == entry {
			count++
		}
	}
	if count != 1 {
		t.Errorf("rows for manifest = %d, want 1 (upsert reaps prior)", count)
	}
	if _, err := os.Stat(staleFile); !os.IsNotExist(err) {
		t.Errorf("stale snapshot file survived the upsert: stat err = %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "codex-cli.snapshot")); err != nil {
		t.Errorf("fresh codex-cli snapshot missing after capture: %v", err)
	}
}

// B4 — a corrupted client config at capture is a fail-closed CAPTURE FAILURE
// (arch F4): capture errors, the client is never classified absent, and no row
// or snapshot is left behind.
func TestCaptureAdoptProvenanceReadErrorFailClosed(t *testing.T) {
	entry := "mui-adopt-prov-failclosed"
	codexPath, _, _ := setupAdoptTestEnv(t, entry, `[mcp_servers.mui-adopt-prov-failclosed]
command = "go"
args = ["version"]
`)
	home := filepath.Dir(filepath.Dir(codexPath))
	cursorPath := filepath.Join(home, ".cursor", "mcp.json")
	if err := os.MkdirAll(filepath.Dir(cursorPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cursorPath, []byte(`{"mcpServers": {`), 0o600); err != nil { // malformed JSON
		t.Fatal(err)
	}

	// Construct the plan DIRECTLY: a corrupted client (which BuildAdoptPlan would
	// refuse/exclude) must reach capture in AdoptClients.
	plan := &AdoptPlan{
		EntryName:    entry,
		SourceClient: "codex-cli",
		ManifestName: entry,
		Port:         nextBindableAdoptPortForTest(t, collectUsedAdoptPorts()),
		AdoptClients: []string{"codex-cli", "cursor"},
		ManifestYAML: "name: " + entry + "\n",
	}
	if _, err := NewAPI().captureAdoptProvenance(plan); err == nil {
		t.Fatal("expected a fail-closed capture error for a corrupt client config; got nil")
	}

	if rec, found, err := ReadAdoptProvenance(entry); err != nil {
		t.Fatalf("ReadAdoptProvenance after failed capture: %v", err)
	} else if found {
		t.Errorf("capture failure left a committed row: %+v", rec)
	}
	dir, err := adoptSnapshotDir(entry)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("capture failure left a snapshot dir: stat err = %v", err)
	}
}

// B5 — the pinned snapshot preserves the ORIGINAL secret literal (the manifest
// routes it to a vault ref; only the snapshot keeps the pre-adopt spelling).
func TestCaptureAdoptProvenancePreservesSecretLiteral(t *testing.T) {
	entry := "mui-adopt-prov-secret"
	setupAdoptTestEnv(t, entry, `[mcp_servers.mui-adopt-prov-secret]
command = "go"
args = ["version"]

[mcp_servers.mui-adopt-prov-secret.env]
API_KEY = "literal-secret-value"
`)
	if _, err := NewAPI().SecretsInit(); err != nil {
		t.Fatalf("SecretsInit: %v", err)
	}
	port := nextBindableAdoptPortForTest(t, collectUsedAdoptPorts())
	api := NewAPI()
	plan, err := api.BuildAdoptPlan(AdoptOpts{EntryName: entry, Client: "codex-cli", ManifestName: entry, Port: port})
	if err != nil {
		t.Fatalf("BuildAdoptPlan: %v", err)
	}
	if len(plan.SecretRoutedKeys) == 0 {
		t.Fatal("precondition: expected the manifest to route the secret (so the literal survives only via the snapshot)")
	}
	if _, err := api.captureAdoptProvenance(plan); err != nil {
		t.Fatalf("captureAdoptProvenance: %v", err)
	}

	rec, found, err := ReadAdoptProvenance(entry)
	if err != nil || !found {
		t.Fatalf("ReadAdoptProvenance found=%v err=%v", found, err)
	}
	var ref string
	for _, c := range rec.Clients {
		if c.Client == "codex-cli" {
			if c.OriginalState != AdoptOriginalStatePresent {
				t.Fatalf("codex-cli original_state = %q, want present", c.OriginalState)
			}
			ref = c.SnapshotRef
		}
	}
	if ref == "" {
		t.Fatal("codex-cli snapshot ref empty")
	}
	stateDir, err := DaemonStateDir()
	if err != nil {
		t.Fatal(err)
	}
	snap, err := os.ReadFile(filepath.Join(stateDir, filepath.FromSlash(ref)))
	if err != nil {
		t.Fatalf("read snapshot: %v", err)
	}
	if !bytes.Contains(snap, []byte("literal-secret-value")) {
		t.Errorf("snapshot did not preserve the ORIGINAL secret literal:\n%s", snap)
	}
	if bytes.Contains(snap, []byte("secret:")) {
		t.Errorf("snapshot leaked a routed secret ref (want the pre-adopt literal):\n%s", snap)
	}
}

// B6 — present/absent classify: source client present (+snapshot); entryless
// fanout client absent (empty snapshot).
func TestCaptureAdoptProvenancePresentAbsentClassify(t *testing.T) {
	entry := "mui-adopt-prov-classify"
	codexPath, _, _ := setupAdoptTestEnv(t, entry, `[mcp_servers.mui-adopt-prov-classify]
command = "go"
args = ["version"]
`)
	home := filepath.Dir(filepath.Dir(codexPath))
	cursorPath := filepath.Join(home, ".cursor", "mcp.json")
	writeJSONForAdoptTest(t, cursorPath, map[string]any{
		"mcpServers": map[string]any{"someOther": map[string]any{"command": "x"}},
	})
	port := nextBindableAdoptPortForTest(t, collectUsedAdoptPorts())
	api := NewAPI()
	plan, err := api.BuildAdoptPlan(AdoptOpts{
		EntryName: entry, Client: "codex-cli", ManifestName: entry, Port: port,
		Clients:  []string{"codex-cli", "cursor"},
		ScanOpts: ScanOpts{CodexConfigPath: codexPath, CursorConfigPath: cursorPath},
	})
	if err != nil {
		t.Fatalf("BuildAdoptPlan: %v", err)
	}
	if !containsAdoptString(plan.AdoptClients, "cursor") {
		t.Fatalf("precondition: cursor not fanned out: %#v", plan.AdoptClients)
	}
	if _, err := api.captureAdoptProvenance(plan); err != nil {
		t.Fatalf("captureAdoptProvenance: %v", err)
	}

	rec, _, err := ReadAdoptProvenance(entry)
	if err != nil {
		t.Fatalf("ReadAdoptProvenance: %v", err)
	}
	byClient := map[string]AdoptClientProvenance{}
	for _, c := range rec.Clients {
		byClient[c.Client] = c
	}
	if src := byClient["codex-cli"]; src.OriginalState != AdoptOriginalStatePresent || src.SnapshotRef == "" || src.SnapshotSHA256 == "" {
		t.Errorf("codex-cli = %+v, want present + non-empty snapshot", src)
	}
	if fan := byClient["cursor"]; fan.OriginalState != AdoptOriginalStateAbsent || fan.SnapshotRef != "" || fan.SnapshotSHA256 != "" {
		t.Errorf("cursor = %+v, want absent + empty snapshot", fan)
	}
}

// B7 — promote flips state (no hash write), idempotent; abort removes row+dir,
// idempotent.
func TestPromoteAndAbortIdempotent(t *testing.T) {
	entry := "mui-adopt-prov-promabort"
	setupAdoptTestEnv(t, entry, `[mcp_servers.mui-adopt-prov-promabort]
command = "go"
args = ["version"]
`)
	port := nextBindableAdoptPortForTest(t, collectUsedAdoptPorts())
	api := NewAPI()
	plan, err := api.BuildAdoptPlan(AdoptOpts{EntryName: entry, Client: "codex-cli", ManifestName: entry, Port: port})
	if err != nil {
		t.Fatalf("BuildAdoptPlan: %v", err)
	}
	rec, err := api.captureAdoptProvenance(plan)
	if err != nil {
		t.Fatalf("captureAdoptProvenance: %v", err)
	}
	hashBefore := rec.AdoptManifestHash

	if err := promoteAdoptProvenanceToAdopted(entry); err != nil {
		t.Fatalf("promote: %v", err)
	}
	if err := promoteAdoptProvenanceToAdopted(entry); err != nil {
		t.Fatalf("promote (idempotent): %v", err)
	}
	got, found, err := ReadAdoptProvenance(entry)
	if err != nil || !found {
		t.Fatalf("read after promote found=%v err=%v", found, err)
	}
	if got.OperationState != AdoptOperationStateAdopted {
		t.Errorf("state = %q, want adopted", got.OperationState)
	}
	if got.AdoptManifestHash != hashBefore || got.ExpectedManifestHash != hashBefore {
		t.Errorf("promote mutated hashes: got (%q,%q), want both %q", got.AdoptManifestHash, got.ExpectedManifestHash, hashBefore)
	}

	if err := abortAdoptProvenance(rec); err != nil {
		t.Fatalf("abort: %v", err)
	}
	if err := abortAdoptProvenance(rec); err != nil {
		t.Fatalf("abort (idempotent): %v", err)
	}
	if _, found, err := ReadAdoptProvenance(entry); err != nil {
		t.Fatalf("read after abort: %v", err)
	} else if found {
		t.Errorf("abort left a row for the manifest")
	}
	dir, err := adoptSnapshotDir(entry)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("abort left a snapshot dir: stat err = %v", err)
	}
}

// B8 — event bodies are redacted (names/counts/paths only); orphan-reaped
// upsert carries trigger:"upsert"; all events land under the `adopt` source.
func TestAdoptProvenanceEventBodiesRedacted(t *testing.T) {
	stateDir := isolateStateDir(t)
	logPath := filepath.Join(stateDir, SupervisorEventLogFileLeaf)

	emitAdoptProvenanceCaptured(&AdoptProvenanceRecord{
		ManifestName:     "provm",
		RoutedSecretKeys: []string{"PROVM_API_KEY"},
		Clients: []AdoptClientProvenance{
			{Client: "codex-cli", OriginalState: AdoptOriginalStatePresent, SnapshotRef: "adopt-provenance/provm/codex-cli.snapshot"},
			{Client: "cursor", OriginalState: AdoptOriginalStateAbsent},
		},
	})
	emitAdoptProvenanceCommitted("provm", "deadbeef")
	emitAdoptProvenanceCaptureFailed("provm", "cursor", "config could not be read or parsed")
	emitAdoptProvenanceAbort("provm", "adopt failure cleanup")
	emitAdoptProvenanceCommitFailed("provm")
	emitAdoptProvenanceOrphanReaped("provm", 7200, adoptOrphanReapTriggerUpsert)

	captured, _ := findSupervisorEventByName(t, logPath, "adopt-provenance-captured")
	if captured == nil {
		t.Fatal("no adopt-provenance-captured event")
	}
	cb, _ := captured["body"].(map[string]any)
	if cb == nil {
		t.Fatalf("captured body not an object: %v", captured["body"])
	}
	if cb["manifest"] != "provm" {
		t.Errorf("captured manifest = %v", cb["manifest"])
	}
	if pc, _ := cb["present_count"].(float64); pc != 1 {
		t.Errorf("present_count = %v, want 1", cb["present_count"])
	}
	if ac, _ := cb["absent_count"].(float64); ac != 1 {
		t.Errorf("absent_count = %v, want 1", cb["absent_count"])
	}

	orphan, _ := findSupervisorEventByName(t, logPath, "adopt-provenance-orphan-reaped")
	if orphan == nil {
		t.Fatal("no adopt-provenance-orphan-reaped event")
	}
	if ob, _ := orphan["body"].(map[string]any); ob == nil || ob["trigger"] != "upsert" {
		t.Errorf("orphan-reaped trigger = %v, want upsert", orphan["body"])
	}

	// Every provenance event lands under the `adopt` source.
	for _, ev := range []string{
		"adopt-provenance-captured", "adopt-provenance-committed",
		"adopt-provenance-capture-failed", "adopt-provenance-abort",
		"adopt-provenance-commit-failed", "adopt-provenance-orphan-reaped",
	} {
		found, _ := findSupervisorEventByName(t, logPath, ev)
		if found == nil {
			t.Errorf("missing event %q", ev)
			continue
		}
		if found["source"] != "adopt" {
			t.Errorf("event %q source = %v, want adopt", ev, found["source"])
		}
	}

	// Redaction: no secret VALUE ever reaches the log (the helpers accept only
	// names/counts/paths). Assert canary values are absent.
	raw, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	for _, forbidden := range []string{"literal-secret-value", "BEGIN PRIVATE KEY"} {
		if bytes.Contains(raw, []byte(forbidden)) {
			t.Errorf("event log leaked a secret value %q", forbidden)
		}
	}
}
