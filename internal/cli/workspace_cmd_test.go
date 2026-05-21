// Package cli — tests for the `mcphub workspace ...` subcommand group
// (Phases B.2 + B.3 of the v0.5.x serena-supervisor unified plan).
package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"mcp-local-hub/internal/api"
	"mcp-local-hub/internal/config"
)

// ------------------------------------------------------------------
// Shared test helpers
// ------------------------------------------------------------------

// withSerenaManifest installs a test-only serena manifest loader that
// returns a manifest with a daemon_template.port_pool block. Restores
// the original loader on test cleanup.
func withSerenaManifest(t *testing.T, start, end int) {
	t.Helper()
	prev := loadSerenaManifestForCLI
	t.Cleanup(func() { loadSerenaManifestForCLI = prev })
	loadSerenaManifestForCLI = func() (*config.ServerManifest, error) {
		return &config.ServerManifest{
			Name:      "serena",
			Kind:      config.KindWorkspaceScoped,
			Transport: "native-http",
			Command:   "uvx",
			DaemonTemplate: &config.DaemonTemplate{
				PortPool:          &config.PortPool{Start: start, End: end},
				ExtraArgsTemplate: []string{"--project", "${workspace.path}"},
			},
		}, nil
	}
}

// withStateDir redirects the registry path to a fresh temp dir via the
// same env-var seam DefaultRegistryPath honors.
func withStateDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("LOCALAPPDATA", dir)
	t.Setenv("XDG_STATE_HOME", dir)
	// Verify the regPath actually lands inside dir (sanity check that the
	// env seam is wired the way these tests assume).
	regPath, err := api.DefaultRegistryPath()
	if err != nil {
		t.Fatalf("DefaultRegistryPath: %v", err)
	}
	if !strings.HasPrefix(regPath, dir) {
		t.Fatalf("regPath %s not under tempDir %s — env seam broken", regPath, dir)
	}
	return dir
}

// makeWorkspaceDir creates an existing on-disk workspace directory and
// optionally seeds a `.serena/project.yml` with the given languages.
// Returns the canonical workspace path (after EvalSymlinks + drive-case
// normalization).
func makeWorkspaceDir(t *testing.T, root string, seedLanguages []string) string {
	t.Helper()
	// Use a deterministic name so the workspace_key in different tests
	// in the same temp dir parent does not collide.
	ws := filepath.Join(root, "workspace")
	if err := os.MkdirAll(ws, 0o700); err != nil {
		t.Fatalf("mkdir workspace: %v", err)
	}
	if seedLanguages != nil {
		if err := os.MkdirAll(filepath.Join(ws, ".serena"), 0o700); err != nil {
			t.Fatalf("mkdir .serena: %v", err)
		}
		// Hand-write the YAML so we don't depend on the implementation's
		// marshal shape.
		buf := &bytes.Buffer{}
		buf.WriteString("languages:\n")
		for _, l := range seedLanguages {
			buf.WriteString("  - " + l + "\n")
		}
		buf.WriteString("read_only: false\n")
		if err := os.WriteFile(filepath.Join(ws, ".serena", "project.yml"), buf.Bytes(), 0o600); err != nil {
			t.Fatalf("write project.yml: %v", err)
		}
	}
	canon, err := api.CanonicalWorkspacePath(ws)
	if err != nil {
		t.Fatalf("canonicalize %s: %v", ws, err)
	}
	return canon
}

// runWorkspaceCmd executes the workspace cobra command with args and
// returns combined stdout+stderr plus any error.
func runWorkspaceCmd(t *testing.T, args ...string) (string, error) {
	t.Helper()
	c := newWorkspaceCmd()
	buf := &bytes.Buffer{}
	c.SetOut(buf)
	c.SetErr(buf)
	c.SilenceUsage = true
	c.SetArgs(args)
	err := c.Execute()
	return buf.String(), err
}

// ------------------------------------------------------------------
// B.2: register
// ------------------------------------------------------------------

func TestWorkspaceRegister_AllocatesPortFromPool(t *testing.T) {
	withSerenaManifest(t, 9121, 9123)
	stateDir := withStateDir(t)
	tmp := t.TempDir()
	ws := makeWorkspaceDir(t, tmp, []string{"python", "typescript"})

	out, err := runWorkspaceCmd(t, "register", ws)
	if err != nil {
		t.Fatalf("register: %v\noutput: %s", err, out)
	}
	regPath, _ := api.DefaultRegistryPath()
	reg := api.NewRegistry(regPath)
	if err := reg.Load(); err != nil {
		t.Fatalf("load registry: %v", err)
	}
	got := reg.SerenaEntries()
	if len(got) != 1 {
		t.Fatalf("want 1 serena entry, got %d", len(got))
	}
	if got[0].Port != 9121 {
		t.Errorf("port = %d, want first-in-pool 9121", got[0].Port)
	}
	if got[0].Language != api.SerenaLanguageSentinel {
		t.Errorf("language = %q, want %q", got[0].Language, api.SerenaLanguageSentinel)
	}
	if got[0].Backend != "serena" {
		t.Errorf("backend = %q, want %q", got[0].Backend, "serena")
	}
	wantLangs := []string{"python", "typescript"}
	if !equalStrings(got[0].Languages, wantLangs) {
		t.Errorf("languages = %v, want %v", got[0].Languages, wantLangs)
	}
	if got[0].RegisteredVia != "manual" {
		t.Errorf("registered_via = %q, want %q", got[0].RegisteredVia, "manual")
	}
	if got[0].RegisteredAt.IsZero() {
		t.Errorf("registered_at unset")
	}

	// Ensure the second registration in this batch (different ws) would
	// pick the next free port — exercises AllocateSerenaPort iteration
	// over the registry's AllocatedPorts set.
	tmp2 := t.TempDir()
	ws2 := makeWorkspaceDir(t, tmp2, []string{"go"})
	if _, err := runWorkspaceCmd(t, "register", ws2); err != nil {
		t.Fatalf("register second: %v", err)
	}
	reg2 := api.NewRegistry(regPath)
	if err := reg2.Load(); err != nil {
		t.Fatalf("reload: %v", err)
	}
	entries := reg2.SerenaEntries()
	if len(entries) != 2 {
		t.Fatalf("want 2 entries, got %d", len(entries))
	}
	seen := map[int]bool{}
	for _, e := range entries {
		seen[e.Port] = true
	}
	if !seen[9121] || !seen[9122] {
		t.Errorf("expected ports {9121, 9122}; got %v", seen)
	}
	_ = stateDir
}

func TestWorkspaceRegister_RejectsExistingPath(t *testing.T) {
	withSerenaManifest(t, 9121, 9123)
	withStateDir(t)
	tmp := t.TempDir()
	ws := makeWorkspaceDir(t, tmp, []string{"python"})

	if _, err := runWorkspaceCmd(t, "register", ws); err != nil {
		t.Fatalf("first register: %v", err)
	}
	out, err := runWorkspaceCmd(t, "register", ws)
	if err == nil {
		t.Fatalf("second register should fail; output: %s", out)
	}
	if !strings.Contains(err.Error(), "already registered") {
		t.Errorf("error message should mention 'already registered'; got %q", err.Error())
	}
}

func TestWorkspaceRegister_MissingProjectYmlNoLanguages_ErrorsWithBootstrapHint(t *testing.T) {
	withSerenaManifest(t, 9121, 9123)
	withStateDir(t)
	tmp := t.TempDir()
	// Note: no seedLanguages → no .serena/project.yml on disk.
	ws := makeWorkspaceDir(t, tmp, nil)

	out, err := runWorkspaceCmd(t, "register", ws)
	if err == nil {
		t.Fatalf("expected error for missing project.yml; output: %s", out)
	}
	msg := err.Error()
	if !strings.Contains(msg, "bootstrap") {
		t.Errorf("error message should mention bootstrap; got %q", msg)
	}
	if !strings.Contains(msg, ".serena/project.yml") {
		t.Errorf("error message should name project.yml; got %q", msg)
	}
}

func TestWorkspaceRegister_LanguagesFlagOverridesProjectYml(t *testing.T) {
	withSerenaManifest(t, 9121, 9123)
	withStateDir(t)
	tmp := t.TempDir()
	ws := makeWorkspaceDir(t, tmp, []string{"python"})

	if _, err := runWorkspaceCmd(t, "register", ws, "--languages", "cpp,typescript,markdown"); err != nil {
		t.Fatalf("register: %v", err)
	}
	regPath, _ := api.DefaultRegistryPath()
	reg := api.NewRegistry(regPath)
	if err := reg.Load(); err != nil {
		t.Fatalf("load: %v", err)
	}
	got := reg.SerenaEntries()
	if len(got) != 1 {
		t.Fatalf("want 1 entry, got %d", len(got))
	}
	want := []string{"cpp", "markdown", "typescript"} // sorted on write
	if !equalStrings(got[0].Languages, want) {
		t.Errorf("languages = %v, want %v", got[0].Languages, want)
	}
}

// ------------------------------------------------------------------
// B.2: unregister
// ------------------------------------------------------------------

// seedTwoBackends registers one serena row + one LSP row under the same
// workspace_key for unregister-semantic tests.
func seedTwoBackends(t *testing.T) (regPath, wsCanon, wsKey string) {
	t.Helper()
	withSerenaManifest(t, 9121, 9123)
	withStateDir(t)
	tmp := t.TempDir()
	wsCanon = makeWorkspaceDir(t, tmp, []string{"python"})
	wsKey = api.WorkspaceKey(wsCanon)

	if _, err := runWorkspaceCmd(t, "register", wsCanon); err != nil {
		t.Fatalf("register serena: %v", err)
	}
	regPath, _ = api.DefaultRegistryPath()
	reg := api.NewRegistry(regPath)
	if err := reg.Load(); err != nil {
		t.Fatalf("load: %v", err)
	}
	// Inject a fake LSP row via PutLSP — same path the production
	// registerOneLanguage uses (Phase 3 lazy-mode register).
	if err := reg.PutLSP(api.WorkspaceEntry{
		WorkspaceKey:  wsKey,
		WorkspacePath: wsCanon,
		Language:      "python",
		Backend:       "mcp-language-server",
		Port:          9200,
		TaskName:      "mcp-local-hub-lsp-" + wsKey + "-python",
	}); err != nil {
		t.Fatalf("PutLSP: %v", err)
	}
	if err := reg.Save(); err != nil {
		t.Fatalf("save: %v", err)
	}
	return regPath, wsCanon, wsKey
}

func TestWorkspaceUnregister_RemovesLSPOnlyByDefault(t *testing.T) {
	regPath, ws, wsKey := seedTwoBackends(t)

	if _, err := runWorkspaceCmd(t, "unregister", ws); err != nil {
		t.Fatalf("unregister: %v", err)
	}
	reg := api.NewRegistry(regPath)
	if err := reg.Load(); err != nil {
		t.Fatalf("load: %v", err)
	}
	// Serena row must remain.
	if _, ok := reg.GetSerena(wsKey); !ok {
		t.Errorf("serena row was removed; default should be LSP-only")
	}
	// LSP rows must be gone.
	lsp := reg.ListByWorkspaceLSP(wsKey)
	if len(lsp) != 0 {
		t.Errorf("LSP rows survived; want 0, got %d", len(lsp))
	}
}

func TestWorkspaceUnregister_BackendSerenaRemovesOnlySerena(t *testing.T) {
	regPath, ws, wsKey := seedTwoBackends(t)

	if _, err := runWorkspaceCmd(t, "unregister", ws, "--backend", "serena"); err != nil {
		t.Fatalf("unregister: %v", err)
	}
	reg := api.NewRegistry(regPath)
	if err := reg.Load(); err != nil {
		t.Fatalf("load: %v", err)
	}
	if _, ok := reg.GetSerena(wsKey); ok {
		t.Errorf("serena row should be removed")
	}
	lsp := reg.ListByWorkspaceLSP(wsKey)
	if len(lsp) != 1 {
		t.Errorf("LSP rows want 1, got %d", len(lsp))
	}
}

func TestWorkspaceUnregister_BackendAllRemovesEverything(t *testing.T) {
	regPath, ws, wsKey := seedTwoBackends(t)

	if _, err := runWorkspaceCmd(t, "unregister", ws, "--backend", "all"); err != nil {
		t.Fatalf("unregister: %v", err)
	}
	reg := api.NewRegistry(regPath)
	if err := reg.Load(); err != nil {
		t.Fatalf("load: %v", err)
	}
	if got := reg.ListByWorkspace(wsKey); len(got) != 0 {
		t.Errorf("want 0 rows, got %d", len(got))
	}
}

func TestWorkspaceUnregister_RemovesEntryButLeavesDisk(t *testing.T) {
	_, ws, _ := seedTwoBackends(t)

	// `.serena/project.yml` was created by makeWorkspaceDir; confirm
	// unregister --backend all does NOT touch it.
	projectYml := filepath.Join(ws, ".serena", "project.yml")
	if _, err := os.Stat(projectYml); err != nil {
		t.Fatalf("project.yml missing before unregister: %v", err)
	}
	if _, err := runWorkspaceCmd(t, "unregister", ws, "--backend", "all"); err != nil {
		t.Fatalf("unregister: %v", err)
	}
	if _, err := os.Stat(projectYml); err != nil {
		t.Errorf("project.yml deleted by unregister: %v (must survive)", err)
	}
}

// ------------------------------------------------------------------
// B.2: list
// ------------------------------------------------------------------

// seedSerenaListFixture registers 2 workspaces and marks the first as
// default. Returns the canonical paths in registration order. Uses
// short distinguishable workspace dir names so the table-truncation
// helper does not collapse the two paths into a shared visible prefix.
func seedSerenaListFixture(t *testing.T) (string, string) {
	t.Helper()
	withSerenaManifest(t, 9121, 9123)
	withStateDir(t)

	tmpA := t.TempDir()
	wsA := makeWorkspaceDirNamed(t, tmpA, "alpha-ws", []string{"python"})
	if _, err := runWorkspaceCmd(t, "register", wsA, "--default"); err != nil {
		t.Fatalf("register A: %v", err)
	}
	tmpB := t.TempDir()
	wsB := makeWorkspaceDirNamed(t, tmpB, "bravo-ws", []string{"go", "typescript"})
	if _, err := runWorkspaceCmd(t, "register", wsB); err != nil {
		t.Fatalf("register B: %v", err)
	}
	return wsA, wsB
}

// makeWorkspaceDirNamed is makeWorkspaceDir with an explicit dirname so
// the trailing component differs between fixture workspaces even when
// their temp-dir parents share a long prefix (Go's testing temp dirs
// on Windows can be 60+ chars wide).
func makeWorkspaceDirNamed(t *testing.T, root, name string, seedLanguages []string) string {
	t.Helper()
	ws := filepath.Join(root, name)
	if err := os.MkdirAll(ws, 0o700); err != nil {
		t.Fatalf("mkdir workspace: %v", err)
	}
	if seedLanguages != nil {
		if err := os.MkdirAll(filepath.Join(ws, ".serena"), 0o700); err != nil {
			t.Fatalf("mkdir .serena: %v", err)
		}
		buf := &bytes.Buffer{}
		buf.WriteString("languages:\n")
		for _, l := range seedLanguages {
			buf.WriteString("  - " + l + "\n")
		}
		buf.WriteString("read_only: false\n")
		if err := os.WriteFile(filepath.Join(ws, ".serena", "project.yml"), buf.Bytes(), 0o600); err != nil {
			t.Fatalf("write project.yml: %v", err)
		}
	}
	canon, err := api.CanonicalWorkspacePath(ws)
	if err != nil {
		t.Fatalf("canonicalize %s: %v", ws, err)
	}
	return canon
}

func TestWorkspaceList_TabularOutput(t *testing.T) {
	wsA, wsB := seedSerenaListFixture(t)
	out, err := runWorkspaceCmd(t, "list")
	if err != nil {
		t.Fatalf("list: %v\noutput: %s", err, out)
	}
	for _, want := range []string{"WORKSPACE", "LANGUAGES", "DEFAULT", "PORT", "LAST_SPAWN"} {
		if !strings.Contains(out, want) {
			t.Errorf("header missing column %q; output:\n%s", want, out)
		}
	}
	// Paths may be truncated by the column-width helper; assert on the
	// observable prefix instead of the full path. CI temp dirs can be
	// 60+ chars wide on Windows.
	prefixA := workspacePrefixForTable(wsA)
	prefixB := workspacePrefixForTable(wsB)
	if !strings.Contains(out, prefixA) {
		t.Errorf("output missing workspace A prefix %q; out:\n%s", prefixA, out)
	}
	if !strings.Contains(out, prefixB) {
		t.Errorf("output missing workspace B prefix %q; out:\n%s", prefixB, out)
	}
	// Ordering: alphabetic by WorkspacePath. Find both lines and compare
	// their indexes.
	idxA := strings.Index(out, prefixA)
	idxB := strings.Index(out, prefixB)
	if idxA < 0 || idxB < 0 {
		t.Fatalf("paths not found; out:\n%s", out)
	}
	expectedFirstIdx := idxA
	expectedSecondIdx := idxB
	if wsB < wsA {
		expectedFirstIdx, expectedSecondIdx = idxB, idxA
	}
	if expectedFirstIdx > expectedSecondIdx {
		t.Errorf("ordering not alphabetic by path; idxA=%d idxB=%d", idxA, idxB)
	}
	// Default marker should appear on the wsA row (the one we set --default for).
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, prefixA) && !strings.Contains(line, "*") {
			t.Errorf("default marker '*' missing on wsA row: %q", line)
		}
	}
	// Languages render in the LANGUAGES column.
	for _, lang := range []string{"python", "go", "typescript"} {
		if !strings.Contains(out, lang) {
			t.Errorf("language %q missing from output", lang)
		}
	}
	// Ports render in the PORT column.
	for _, port := range []string{"9121", "9122"} {
		if !strings.Contains(out, port) {
			t.Errorf("port %q missing from output", port)
		}
	}
}

// workspacePrefixForTable returns the observable string for path p as
// it would appear under the list-table truncation helper. If p is short
// enough, returns it verbatim; otherwise the first (width-3) chars
// (the truncate helper appends "..." after position n-3).
func workspacePrefixForTable(p string) string {
	width := workspaceTablePathWidth
	if len(p) <= width {
		return p
	}
	return p[:width-3]
}

func TestWorkspaceList_JSONOutput(t *testing.T) {
	wsA, wsB := seedSerenaListFixture(t)
	out, err := runWorkspaceCmd(t, "list", "--json")
	if err != nil {
		t.Fatalf("list --json: %v\noutput: %s", err, out)
	}
	got := strings.TrimSpace(out)
	if !strings.HasPrefix(got, "[") || !strings.HasSuffix(got, "]") {
		t.Fatalf("not a JSON array:\n%s", got)
	}
	var rows []workspaceListJSONRow
	if err := json.Unmarshal([]byte(got), &rows); err != nil {
		t.Fatalf("json decode: %v\nbody: %s", err, got)
	}
	if len(rows) != 2 {
		t.Fatalf("want 2 rows, got %d", len(rows))
	}
	// Build a map by path so order independence is explicit.
	byPath := map[string]workspaceListJSONRow{}
	for _, r := range rows {
		byPath[r.WorkspacePath] = r
	}
	rA, okA := byPath[wsA]
	rB, okB := byPath[wsB]
	if !okA || !okB {
		t.Fatalf("missing rows; got map keys %v", mapKeys(byPath))
	}
	if !rA.Default {
		t.Errorf("wsA should be default=true")
	}
	if rB.Default {
		t.Errorf("wsB should be default=false")
	}
	if rA.Language != api.SerenaLanguageSentinel || rB.Language != api.SerenaLanguageSentinel {
		t.Errorf("language sentinel missing: A=%q B=%q", rA.Language, rB.Language)
	}
	if rA.Backend != "serena" || rB.Backend != "serena" {
		t.Errorf("backend not serena: A=%q B=%q", rA.Backend, rB.Backend)
	}
	if rA.Port == 0 || rB.Port == 0 || rA.Port == rB.Port {
		t.Errorf("ports should be distinct non-zero; A=%d B=%d", rA.Port, rB.Port)
	}
}

// ------------------------------------------------------------------
// B.2: set-default
// ------------------------------------------------------------------

func TestWorkspaceSetDefault_UpdatesRegistry(t *testing.T) {
	withSerenaManifest(t, 9121, 9123)
	withStateDir(t)
	tmpA := t.TempDir()
	tmpB := t.TempDir()
	wsA := makeWorkspaceDir(t, tmpA, []string{"python"})
	wsB := makeWorkspaceDir(t, tmpB, []string{"go"})
	if _, err := runWorkspaceCmd(t, "register", wsA, "--default"); err != nil {
		t.Fatalf("register A: %v", err)
	}
	if _, err := runWorkspaceCmd(t, "register", wsB); err != nil {
		t.Fatalf("register B: %v", err)
	}

	regPath, _ := api.DefaultRegistryPath()
	stateDir := filepath.Dir(regPath)
	got, err := readDefaultWorkspace(stateDir)
	if err != nil {
		t.Fatalf("read default: %v", err)
	}
	if got != wsA {
		t.Errorf("initial default = %q, want %q", got, wsA)
	}

	if _, err := runWorkspaceCmd(t, "set-default", wsB); err != nil {
		t.Fatalf("set-default B: %v", err)
	}
	got, err = readDefaultWorkspace(stateDir)
	if err != nil {
		t.Fatalf("read default: %v", err)
	}
	if got != wsB {
		t.Errorf("after set-default B: default = %q, want %q", got, wsB)
	}

	// set-default on an unregistered workspace must fail.
	tmpC := t.TempDir()
	wsC := makeWorkspaceDir(t, tmpC, []string{"rust"})
	if _, err := runWorkspaceCmd(t, "set-default", wsC); err == nil {
		t.Errorf("set-default on unregistered workspace should fail")
	}
}

// ------------------------------------------------------------------
// B.3: bootstrap
// ------------------------------------------------------------------

// touch creates an empty file at path, creating parent dirs as needed.
func touch(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
}

// readBootstrappedLanguages reads .serena/project.yml under root and
// returns the languages list (sorted).
func readBootstrappedLanguages(t *testing.T, root string) []string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(root, ".serena", "project.yml"))
	if err != nil {
		t.Fatalf("read project.yml: %v", err)
	}
	var doc serenaProjectYml
	if err := yamlUnmarshalForTest(data, &doc); err != nil {
		t.Fatalf("parse: %v", err)
	}
	out := append([]string(nil), doc.Languages...)
	sort.Strings(out)
	return out
}

func TestWorkspaceBootstrap_DetectsLanguagesFromExtensions(t *testing.T) {
	tmp := t.TempDir()
	// Seed a multi-language repo.
	touch(t, filepath.Join(tmp, "main.cpp"))
	touch(t, filepath.Join(tmp, "header.h"))
	touch(t, filepath.Join(tmp, "extra.hpp"))
	touch(t, filepath.Join(tmp, "tool.go"))
	touch(t, filepath.Join(tmp, "ui.tsx"))
	touch(t, filepath.Join(tmp, "util.ts"))
	touch(t, filepath.Join(tmp, "page.js"))
	touch(t, filepath.Join(tmp, "script.py"))
	touch(t, filepath.Join(tmp, "lib.rs"))
	touch(t, filepath.Join(tmp, "README.md"))
	touch(t, filepath.Join(tmp, "style.css"))
	touch(t, filepath.Join(tmp, "index.html"))
	touch(t, filepath.Join(tmp, "sim.f90"))

	if _, err := runWorkspaceCmd(t, "bootstrap", tmp); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}

	got := readBootstrappedLanguages(t, tmp)
	want := []string{
		"cpp", "fortran", "go", "javascript", "markdown",
		"python", "rust", "typescript", "vscode-css", "vscode-html",
	}
	sort.Strings(want)
	if !equalStrings(got, want) {
		t.Errorf("languages = %v, want %v", got, want)
	}
}

func TestWorkspaceBootstrap_SkipsGitignored(t *testing.T) {
	tmp := t.TempDir()
	// Root-level .gitignore that should suppress "custom_ignored" subdir.
	if err := os.WriteFile(filepath.Join(tmp, ".gitignore"),
		[]byte("custom_ignored\n# comment\n\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	touch(t, filepath.Join(tmp, "main.go"))
	// File in the gitignored dir would otherwise flag rust as detected.
	touch(t, filepath.Join(tmp, "custom_ignored", "deep.rs"))

	if _, err := runWorkspaceCmd(t, "bootstrap", tmp); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	got := readBootstrappedLanguages(t, tmp)
	if containsString(got, "rust") {
		t.Errorf("rust should have been gitignored; got %v", got)
	}
	if !containsString(got, "go") {
		t.Errorf("go should still be detected; got %v", got)
	}
}

func TestWorkspaceBootstrap_AlwaysSkipsNodeModulesEtc(t *testing.T) {
	tmp := t.TempDir()
	// NO .gitignore: rely on the hardcoded skip list only.
	touch(t, filepath.Join(tmp, "main.go"))
	for _, skipDir := range []string{"node_modules", "target", "dist", ".git"} {
		touch(t, filepath.Join(tmp, skipDir, "should_be_invisible.py"))
	}
	if _, err := runWorkspaceCmd(t, "bootstrap", tmp); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	got := readBootstrappedLanguages(t, tmp)
	if containsString(got, "python") {
		t.Errorf("python under hardcoded-skip dir should be invisible; got %v", got)
	}
	if !containsString(got, "go") {
		t.Errorf("go should still be detected; got %v", got)
	}
}

func TestWorkspaceBootstrap_RefusesOverwriteWithoutForce(t *testing.T) {
	tmp := t.TempDir()
	touch(t, filepath.Join(tmp, "main.go"))
	// Pre-create a project.yml.
	if err := os.MkdirAll(filepath.Join(tmp, ".serena"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tmp, ".serena", "project.yml"),
		[]byte("preserved: true\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	out, err := runWorkspaceCmd(t, "bootstrap", tmp)
	if err == nil {
		t.Fatalf("expected error; output: %s", out)
	}
	if !strings.Contains(err.Error(), "--force") {
		t.Errorf("error should mention --force; got %q", err.Error())
	}

	// And the file must NOT have been overwritten.
	data, _ := os.ReadFile(filepath.Join(tmp, ".serena", "project.yml"))
	if !strings.Contains(string(data), "preserved: true") {
		t.Errorf("project.yml was modified despite refusal: %s", data)
	}
}

func TestWorkspaceBootstrap_ForceOverwrites(t *testing.T) {
	tmp := t.TempDir()
	touch(t, filepath.Join(tmp, "main.go"))
	if err := os.MkdirAll(filepath.Join(tmp, ".serena"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tmp, ".serena", "project.yml"),
		[]byte("preserved: true\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := runWorkspaceCmd(t, "bootstrap", tmp, "--force"); err != nil {
		t.Fatalf("bootstrap --force: %v", err)
	}
	got := readBootstrappedLanguages(t, tmp)
	if !containsString(got, "go") {
		t.Errorf("force-overwrite should write the survey result; got %v", got)
	}
}

func TestWorkspaceBootstrap_DepthBoundedAt5(t *testing.T) {
	tmp := t.TempDir()
	// Depth 1: tmp/lvl1 (file: tmp/lvl1/should_be_seen.go) — depth count from
	// root: the file is one separator deeper than tmp itself.
	touch(t, filepath.Join(tmp, "lvl1", "should_be_seen.go"))
	// Depth 6: tmp/a/b/c/d/e/f/deep.py — 6 directory levels deep.
	touch(t, filepath.Join(tmp, "a", "b", "c", "d", "e", "f", "deep.py"))

	if _, err := runWorkspaceCmd(t, "bootstrap", tmp); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	got := readBootstrappedLanguages(t, tmp)
	if !containsString(got, "go") {
		t.Errorf("shallow .go must be detected; got %v", got)
	}
	if containsString(got, "python") {
		t.Errorf("python past depth-5 should not be detected; got %v", got)
	}
}

// ------------------------------------------------------------------
// Small helpers (test-only)
// ------------------------------------------------------------------

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func containsString(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}

func mapKeys(m map[string]workspaceListJSONRow) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// yamlUnmarshalForTest is a tiny indirection so we can swap implementations
// without dragging the yaml import into every test file.
func yamlUnmarshalForTest(data []byte, v any) error {
	return yaml.Unmarshal(data, v)
}
