package api

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"mcp-local-hub/internal/secrets"
)

// assertNoAdoptProvenanceResidue asserts a failed/aborted adopt left NO
// provenance row and NO snapshot dir for the manifest.
func assertNoAdoptProvenanceResidue(t *testing.T, manifestName string) {
	t.Helper()
	if rec, found, err := ReadAdoptProvenance(manifestName); err != nil {
		t.Fatalf("ReadAdoptProvenance: %v", err)
	} else if found {
		t.Errorf("provenance row survived the aborted adopt: %+v", rec)
	}
	dir, err := adoptSnapshotDir(manifestName)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("provenance snapshot dir survived the aborted adopt: stat err = %v", err)
	}
}

// C1 — a real adopt persists an `adopted` provenance record readable from disk
// alone (no shared in-memory state); the pinned snapshot exists and its hash
// matches its bytes.
func TestExecuteAdoptPersistsProvenanceAcrossFreshAPI(t *testing.T) {
	entry := "mui-adopt-seam-c1"
	setupAdoptTestEnv(t, entry, `[mcp_servers.mui-adopt-seam-c1]
command = "go"
args = ["version"]
`)
	port := nextBindableAdoptPortForTest(t, collectUsedAdoptPorts())
	plan, err := NewAPI().BuildAdoptPlan(AdoptOpts{EntryName: entry, Client: "codex-cli", ManifestName: entry, Port: port})
	if err != nil {
		t.Fatalf("BuildAdoptPlan: %v", err)
	}
	// Execute on one API; read back on a fresh package-level accessor (reads the
	// on-disk store, no shared in-memory state).
	if err := NewAPI().ExecuteAdopt(plan, ioDiscardForAdoptTest{}); err != nil {
		t.Fatalf("ExecuteAdopt: %v", err)
	}
	rec, found, err := ReadAdoptProvenance(entry)
	if err != nil || !found {
		t.Fatalf("ReadAdoptProvenance found=%v err=%v", found, err)
	}
	if rec.OperationState != AdoptOperationStateAdopted {
		t.Errorf("operation_state = %q, want adopted", rec.OperationState)
	}
	var src *AdoptClientProvenance
	for i := range rec.Clients {
		if rec.Clients[i].Client == "codex-cli" {
			src = &rec.Clients[i]
		}
	}
	if src == nil || src.OriginalState != AdoptOriginalStatePresent || src.SnapshotRef == "" || src.SnapshotSHA256 == "" {
		t.Fatalf("codex-cli provenance = %+v, want present + snapshot", src)
	}
	stateDir, err := DaemonStateDir()
	if err != nil {
		t.Fatal(err)
	}
	snap, err := os.ReadFile(filepath.Join(stateDir, filepath.FromSlash(src.SnapshotRef)))
	if err != nil {
		t.Fatalf("read pinned snapshot: %v", err)
	}
	if got := ManifestHashContent(snap); got != src.SnapshotSHA256 {
		t.Errorf("snapshot_sha256 = %q, want %q (hash of on-disk snapshot bytes)", src.SnapshotSHA256, got)
	}
}

// C2 — a capture failure (unreadable selected client) fails the adopt CLOSED:
// NO vault key, NO manifest, NO client-config change, NO provenance row.
func TestExecuteAdoptCaptureFailClosed(t *testing.T) {
	entry := "mui-adopt-seam-c2"
	literalSecret := "literal-" + "secret-value"
	codexPath, manifestRoot, _ := setupAdoptTestEnv(t, entry, `[mcp_servers.mui-adopt-seam-c2]
command = "go"
args = ["version"]

[mcp_servers.mui-adopt-seam-c2.env]
API_KEY = "`+literalSecret+`"
`)
	if _, err := NewAPI().SecretsInit(); err != nil {
		t.Fatalf("SecretsInit: %v", err)
	}
	home := filepath.Dir(filepath.Dir(codexPath))
	cursorPath := filepath.Join(home, ".cursor", "mcp.json")
	writeJSONForAdoptTest(t, cursorPath, map[string]any{
		"mcpServers": map[string]any{"someOther": map[string]any{"command": "x"}},
	})
	port := nextBindableAdoptPortForTest(t, collectUsedAdoptPorts())
	plan, err := NewAPI().BuildAdoptPlan(AdoptOpts{
		EntryName: entry, Client: "codex-cli", ManifestName: entry, Port: port,
		Clients:  []string{"codex-cli", "cursor"},
		ScanOpts: ScanOpts{CodexConfigPath: codexPath, CursorConfigPath: cursorPath},
	})
	if err != nil {
		t.Fatalf("BuildAdoptPlan: %v", err)
	}
	if len(plan.SecretRoutedKeys) == 0 {
		t.Fatal("precondition: need a routed secret so 'no vault key' is a meaningful assertion")
	}
	codexBefore := mustReadFileForAdoptTest(t, codexPath)

	// Corrupt cursor AFTER BuildAdoptPlan so capture (during Execute) fails on it.
	if err := os.WriteFile(cursorPath, []byte(`{"mcpServers": {`), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := NewAPI().ExecuteAdopt(plan, ioDiscardForAdoptTest{}); err == nil {
		t.Fatal("ExecuteAdopt succeeded despite an unreadable selected client at capture; want fail-closed error")
	}

	vault, err := secrets.OpenVault(secrets.DefaultKeyPath(), secrets.DefaultVaultPath())
	if err != nil {
		t.Fatalf("OpenVault: %v", err)
	}
	if keys := vault.List(); len(keys) != 0 {
		t.Errorf("capture-failed adopt wrote vault keys %v, want none", keys)
	}
	if _, err := os.Stat(filepath.Join(manifestRoot, entry, "manifest.yaml")); !os.IsNotExist(err) {
		t.Errorf("capture-failed adopt created a manifest: stat err = %v", err)
	}
	if after := mustReadFileForAdoptTest(t, codexPath); !bytes.Equal(codexBefore, after) {
		t.Errorf("capture-failed adopt changed the source client config")
	}
	assertNoAdoptProvenanceResidue(t, entry)
}

// C3 — abort fires on EACH of the three post-capture failure branches
// (persist-secrets, manifest-create, install), leaving no provenance residue.
func TestExecuteAdoptAbortOnEachFailureBranch(t *testing.T) {
	t.Run("persist-secrets-failure", func(t *testing.T) {
		entry := "mui-adopt-seam-c3-persist"
		literalSecret := "literal-" + "secret-value"
		setupAdoptTestEnv(t, entry, `[mcp_servers.mui-adopt-seam-c3-persist]
command = "go"
args = ["version"]

[mcp_servers.mui-adopt-seam-c3-persist.env]
API_KEY = "`+literalSecret+`"
`)
		// NO SecretsInit -> persistAdoptRoutedSecrets fails opening a nonexistent
		// vault, AFTER capture already wrote provenance. Abort site 1.
		port := nextBindableAdoptPortForTest(t, collectUsedAdoptPorts())
		plan, err := NewAPI().BuildAdoptPlan(AdoptOpts{EntryName: entry, Client: "codex-cli", ManifestName: entry, Port: port})
		if err != nil {
			t.Fatalf("BuildAdoptPlan: %v", err)
		}
		if len(plan.SecretRoutedKeys) == 0 {
			t.Fatal("precondition: a routed secret is required to reach persistAdoptRoutedSecrets")
		}
		if err := NewAPI().ExecuteAdopt(plan, ioDiscardForAdoptTest{}); err == nil {
			t.Fatal("ExecuteAdopt succeeded; want a persist-secrets failure (no vault)")
		}
		assertNoAdoptProvenanceResidue(t, entry)
	})

	t.Run("manifest-create-failure", func(t *testing.T) {
		entry := "mui-adopt-seam-c3-manifest"
		_, manifestRoot, _ := setupAdoptTestEnv(t, entry, `[mcp_servers.mui-adopt-seam-c3-manifest]
command = "go"
args = ["version"]
`)
		port := nextBindableAdoptPortForTest(t, collectUsedAdoptPorts())
		plan, err := NewAPI().BuildAdoptPlan(AdoptOpts{EntryName: entry, Client: "codex-cli", ManifestName: entry, Port: port})
		if err != nil {
			t.Fatalf("BuildAdoptPlan: %v", err)
		}
		// Pre-create the manifest AFTER BuildAdoptPlan so ManifestCreate fails
		// ("already exists"), AFTER capture. Abort site 2.
		mdir := filepath.Join(manifestRoot, entry)
		if err := os.MkdirAll(mdir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(mdir, "manifest.yaml"), []byte("name: "+entry+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := NewAPI().ExecuteAdopt(plan, ioDiscardForAdoptTest{}); err == nil {
			t.Fatal("ExecuteAdopt succeeded; want a manifest-create failure")
		}
		assertNoAdoptProvenanceResidue(t, entry)
	})

	t.Run("install-failure", func(t *testing.T) {
		entry := "mui-adopt-seam-c3-install"
		codexPath, _, _ := setupAdoptTestEnv(t, entry, `[mcp_servers.mui-adopt-seam-c3-install]
command = "go"
args = ["version"]
`)
		port := nextBindableAdoptPortForTest(t, collectUsedAdoptPorts())
		plan, err := NewAPI().BuildAdoptPlan(AdoptOpts{EntryName: entry, Client: "codex-cli", ManifestName: entry, Port: port})
		if err != nil {
			t.Fatalf("BuildAdoptPlan: %v", err)
		}
		// Induce a client-config write failure so Install fails AFTER capture +
		// manifest-create. Abort site 3. (The fail seam only affects
		// clients.WriteConfigFile, NOT the state-file pipeline capture/abort use.)
		failClientConfigWritesForAdoptTest(t, codexPath)
		if err := NewAPI().ExecuteAdopt(plan, ioDiscardForAdoptTest{}); err == nil {
			t.Fatal("ExecuteAdopt succeeded; want an install failure")
		}
		assertNoAdoptProvenanceResidue(t, entry)
	})
}

// C5 — a promote-flip failure after Install leaves a recoverable `adopting`
// row with both hashes but returns the typed provenance-receipt failure.
func TestExecuteAdoptPromoteRecoverable(t *testing.T) {
	entry := "mui-adopt-seam-c5"
	setupAdoptTestEnv(t, entry, `[mcp_servers.mui-adopt-seam-c5]
command = "go"
args = ["version"]
`)
	orig := promoteAdoptProvenanceFn
	promoteAdoptProvenanceFn = func(string) error { return fmt.Errorf("induced promote flip failure") }
	t.Cleanup(func() { promoteAdoptProvenanceFn = orig })

	port := nextBindableAdoptPortForTest(t, collectUsedAdoptPorts())
	plan, err := NewAPI().BuildAdoptPlan(AdoptOpts{EntryName: entry, Client: "codex-cli", ManifestName: entry, Port: port})
	if err != nil {
		t.Fatalf("BuildAdoptPlan: %v", err)
	}
	err = NewAPI().ExecuteAdopt(plan, ioDiscardForAdoptTest{})
	if err == nil {
		t.Fatal("ExecuteAdopt succeeded despite a failed provenance receipt")
	}
	var stageErr *AdoptStageError
	if !errors.As(err, &stageErr) || stageErr.Stage != "provenance-receipt" {
		t.Fatalf("ExecuteAdopt error = %v, want typed provenance-receipt stage", err)
	}
	rec, found, err := ReadAdoptProvenance(entry)
	if err != nil || !found {
		t.Fatalf("ReadAdoptProvenance found=%v err=%v", found, err)
	}
	if rec.OperationState != AdoptOperationStateAdopting {
		t.Errorf("operation_state = %q, want adopting (recoverable) after a promote failure", rec.OperationState)
	}
	if rec.AdoptManifestHash == "" || rec.ExpectedManifestHash == "" {
		t.Errorf("recoverable row missing hashes: adopt=%q expected=%q (de-adopt's hash-gate needs both)", rec.AdoptManifestHash, rec.ExpectedManifestHash)
	}
}

// C6 — the persisted adopt_manifest_hash equals ManifestHashContent of the
// manifest ManifestCreate actually wrote to disk.
func TestExecuteAdoptManifestHashMatchesRow(t *testing.T) {
	entry := "mui-adopt-seam-c6"
	_, manifestRoot, _ := setupAdoptTestEnv(t, entry, `[mcp_servers.mui-adopt-seam-c6]
command = "go"
args = ["version"]
`)
	port := nextBindableAdoptPortForTest(t, collectUsedAdoptPorts())
	plan, err := NewAPI().BuildAdoptPlan(AdoptOpts{EntryName: entry, Client: "codex-cli", ManifestName: entry, Port: port})
	if err != nil {
		t.Fatalf("BuildAdoptPlan: %v", err)
	}
	if err := NewAPI().ExecuteAdopt(plan, ioDiscardForAdoptTest{}); err != nil {
		t.Fatalf("ExecuteAdopt: %v", err)
	}
	rec, found, err := ReadAdoptProvenance(entry)
	if err != nil || !found {
		t.Fatalf("ReadAdoptProvenance found=%v err=%v", found, err)
	}
	onDisk := mustReadFileForAdoptTest(t, filepath.Join(manifestRoot, entry, "manifest.yaml"))
	if want := ManifestHashContent(onDisk); rec.AdoptManifestHash != want {
		t.Errorf("adopt_manifest_hash = %q, want hash of on-disk manifest %q", rec.AdoptManifestHash, want)
	}
}

// P2 — a client PRESENT at Build whose entry vanishes before capture fails the
// adopt CLOSED (a Build->capture TOCTOU on the adopted entry): no vault key, no
// manifest, no source-client write, no provenance row. Uses a NON-source
// present-at-Build client (cursor) to prove the general-case fix, not just the
// source special case.
func TestCaptureFailsClosedWhenPresentClientVanishes(t *testing.T) {
	entry := "mui-adopt-vanish"
	literalSecret := "literal-" + "secret-value"
	codexPath, manifestRoot, _ := setupAdoptTestEnv(t, entry, `[mcp_servers.mui-adopt-vanish]
command = "go"
args = ["version"]

[mcp_servers.mui-adopt-vanish.env]
API_KEY = "`+literalSecret+`"
`)
	if _, err := NewAPI().SecretsInit(); err != nil {
		t.Fatalf("SecretsInit: %v", err)
	}
	home := filepath.Dir(filepath.Dir(codexPath))
	cursorPath := filepath.Join(home, ".cursor", "mcp.json")
	// cursor has the SAME-signature entry at Build (present + matching -> Matching).
	writeJSONForAdoptTest(t, cursorPath, map[string]any{
		"mcpServers": map[string]any{entry: map[string]any{
			"command": "go", "args": []any{"version"},
			"env": map[string]any{"API_KEY": literalSecret},
		}},
	})
	port := nextBindableAdoptPortForTest(t, collectUsedAdoptPorts())
	plan, err := NewAPI().BuildAdoptPlan(AdoptOpts{
		EntryName: entry, Client: "codex-cli", ManifestName: entry, Port: port,
		Clients:  []string{"codex-cli", "cursor"},
		ScanOpts: ScanOpts{CodexConfigPath: codexPath, CursorConfigPath: cursorPath},
	})
	if err != nil {
		t.Fatalf("BuildAdoptPlan: %v", err)
	}
	if !containsAdoptString(plan.presentAtBuild, "cursor") || !containsAdoptString(plan.AdoptClients, "cursor") {
		t.Fatalf("precondition: cursor must be present-at-Build AND selected; present=%#v selected=%#v", plan.presentAtBuild, plan.AdoptClients)
	}
	codexBefore := mustReadFileForAdoptTest(t, codexPath)

	// TOCTOU: cursor's entry vanishes between Build and capture (config parses
	// cleanly, same-name entry gone).
	writeJSONForAdoptTest(t, cursorPath, map[string]any{"mcpServers": map[string]any{}})

	if err := NewAPI().ExecuteAdopt(plan, ioDiscardForAdoptTest{}); err == nil {
		t.Fatal("ExecuteAdopt succeeded despite a present-at-Build client vanishing; want fail-closed error")
	}
	vault, err := secrets.OpenVault(secrets.DefaultKeyPath(), secrets.DefaultVaultPath())
	if err != nil {
		t.Fatalf("OpenVault: %v", err)
	}
	if keys := vault.List(); len(keys) != 0 {
		t.Errorf("vanish-capture adopt wrote vault keys %v, want none", keys)
	}
	if _, err := os.Stat(filepath.Join(manifestRoot, entry, "manifest.yaml")); !os.IsNotExist(err) {
		t.Errorf("vanish-capture adopt created a manifest: stat err = %v", err)
	}
	if after := mustReadFileForAdoptTest(t, codexPath); !bytes.Equal(codexBefore, after) {
		t.Errorf("vanish-capture adopt changed the source client config")
	}
	assertNoAdoptProvenanceResidue(t, entry)
}

// P2 control — a genuine entryless-fanout target (absent at Build, NOT in
// PresentAtBuild) still classifies `absent` and the adopt SUCCEEDS. The fix must
// not regress the fan-out path.
func TestCaptureFanoutAbsentStillSucceeds(t *testing.T) {
	entry := "mui-adopt-fanout-ok"
	codexPath, manifestRoot, _ := setupAdoptTestEnv(t, entry, `[mcp_servers.mui-adopt-fanout-ok]
command = "go"
args = ["version"]
`)
	home := filepath.Dir(filepath.Dir(codexPath))
	cursorPath := filepath.Join(home, ".cursor", "mcp.json")
	// Valid cursor config LACKING the entry -> entryless fanout (absent at Build).
	writeJSONForAdoptTest(t, cursorPath, map[string]any{
		"mcpServers": map[string]any{"someOther": map[string]any{"command": "x"}},
	})
	port := nextBindableAdoptPortForTest(t, collectUsedAdoptPorts())
	plan, err := NewAPI().BuildAdoptPlan(AdoptOpts{
		EntryName: entry, Client: "codex-cli", ManifestName: entry, Port: port,
		Clients:  []string{"codex-cli", "cursor"},
		ScanOpts: ScanOpts{CodexConfigPath: codexPath, CursorConfigPath: cursorPath},
	})
	if err != nil {
		t.Fatalf("BuildAdoptPlan: %v", err)
	}
	if containsAdoptString(plan.presentAtBuild, "cursor") {
		t.Fatalf("precondition: cursor must NOT be present-at-Build: %#v", plan.presentAtBuild)
	}
	if !containsAdoptString(plan.AdoptClients, "cursor") {
		t.Fatalf("precondition: cursor must be a selected fanout target: %#v", plan.AdoptClients)
	}
	if err := NewAPI().ExecuteAdopt(plan, ioDiscardForAdoptTest{}); err != nil {
		t.Fatalf("ExecuteAdopt on a legitimate entryless-fanout target must succeed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(manifestRoot, entry, "manifest.yaml")); err != nil {
		t.Errorf("manifest missing after a successful fanout adopt: %v", err)
	}
	rec, found, err := ReadAdoptProvenance(entry)
	if err != nil || !found {
		t.Fatalf("ReadAdoptProvenance found=%v err=%v", found, err)
	}
	byClient := map[string]AdoptClientProvenance{}
	for _, c := range rec.Clients {
		byClient[c.Client] = c
	}
	if byClient["cursor"].OriginalState != AdoptOriginalStateAbsent {
		t.Errorf("cursor should classify absent (fanout), got %q", byClient["cursor"].OriginalState)
	}
	if byClient["codex-cli"].OriginalState != AdoptOriginalStatePresent {
		t.Errorf("codex-cli should classify present, got %q", byClient["codex-cli"].OriginalState)
	}
}
