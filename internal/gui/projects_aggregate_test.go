// internal/gui/projects_aggregate_test.go
//
// Per-project-GUI Phase 3a: GET /api/projects (the A+B+C aggregate). Tests:
//   - composition: one DTO per canonical project key, joining A (workspace LSP
//     entries), B (project-scoped scan incl. both claude scopes), and C (groups,
//     project-unbound in P3a).
//   - golden global-scan-unchanged: building the aggregate (which runs N project
//     scans) does NOT perturb the global DefaultScanConfigPaths scan — the
//     PROJECT resolver is used throughout.
//
// STATE-SAFETY: isolateHome(t) fences HOME/LOCALAPPDATA/XDG; workspacesTestSeam
// injects a synthetic registry (no live workspaces.yaml read); groups use a
// sandboxed state dir. No live config is touched.
package gui

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"mcp-local-hub/internal/api"
	"mcp-local-hub/internal/clients"
)

// getProjectsAggregate issues a same-origin GET /api/projects.
func getProjectsAggregate(t *testing.T, s *Server) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/projects", nil)
	req.Host = "127.0.0.1:9081"
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	rec := httptest.NewRecorder()
	s.port.Store(9081)
	s.mux.ServeHTTP(rec, req)
	return rec
}

// injectWorkspaceRegistry points workspacesTestSeam at a synthetic registry for
// the duration of the test.
func injectWorkspaceRegistry(t *testing.T, entries []api.WorkspaceEntry) {
	t.Helper()
	orig := workspacesTestSeam
	workspacesTestSeam = func() (*api.Registry, error) {
		reg := api.NewRegistry("")
		reg.Workspaces = entries
		return reg, nil
	}
	t.Cleanup(func() { workspacesTestSeam = orig })
}

// TestProjectsAggregate_ComposesABC seeds one project (workspace entry + project
// config files + ~/.claude.json local) and a group, then asserts the aggregate
// DTO joins all three.
func TestProjectsAggregate_ComposesABC(t *testing.T) {
	isolateHome(t)
	stateDir := t.TempDir()
	t.Cleanup(api.SetDaemonStateRootForTest(stateDir))
	t.Cleanup(api.SetClientWriteFallbackForTest())

	root := t.TempDir()
	seedProject(t, root) // .mcp.json(alpha) + .cursor(beta) + .vscode(gamma)
	// claude-local: approve alpha (.mcp.json Project-scope server).
	key := fwdSlashKey(root)
	writeSyntheticClaudeJSON(t, `{"projects":{"`+jsonEscPath(key)+`":{"enabledMcpjsonServers":["alpha"]}}}`)

	// A — a workspace LSP entry for this project.
	injectWorkspaceRegistry(t, []api.WorkspaceEntry{
		{WorkspaceKey: "k1", WorkspacePath: root, Language: "go", Backend: "mcp-language-server", Port: 9300},
	})

	// C — a group.
	if err := api.WriteGroups(api.GroupsConfig{Version: 1, Groups: []api.Group{{Name: "frontend", Servers: []string{"memory"}}}}); err != nil {
		t.Fatalf("seed group: %v", err)
	}

	s := NewServer(Config{})
	rec := getProjectsAggregate(t, s)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var out projectsAggregateResponse
	if err := json.NewDecoder(rec.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v (body=%s)", err, rec.Body.String())
	}

	// One project keyed by the canonical key.
	wantKey := clients.CanonicalProjectKey(root)
	if len(out.Projects) != 1 {
		t.Fatalf("projects len=%d, want 1: %+v", len(out.Projects), out.Projects)
	}
	p := out.Projects[0]
	if p.Key != wantKey {
		t.Errorf("project key=%q, want %q", p.Key, wantKey)
	}
	// A: the workspace LSP entry is present.
	if len(p.Entries) != 1 || p.Entries[0].Language != "go" {
		t.Errorf("workspace entries (A) wrong: %+v", p.Entries)
	}
	// B: the project scan is present with the per-client servers.
	if p.Scan == nil {
		t.Fatalf("scan (B) nil; scan_error=%q", p.ScanError)
	}
	byName := map[string]api.ScanEntry{}
	for _, e := range p.Scan.Entries {
		byName[e.Name] = e
	}
	if _, ok := byName["alpha"].ClientPresence["claude-code"]; !ok {
		t.Errorf("alpha has no claude-code presence in aggregate scan")
	}
	if _, ok := byName["beta"].ClientPresence["cursor"]; !ok {
		t.Errorf("beta has no cursor presence")
	}
	// B (claude-local enrichment): alpha is approved → ProjectEnabled true.
	if pe := byName["alpha"].ProjectEnabled; pe == nil || !*pe {
		t.Errorf("alpha ProjectEnabled = %v, want true (enabledMcpjsonServers)", pe)
	}
	// C: the group is surfaced.
	if len(out.Groups) != 1 || out.Groups[0].Name != "frontend" {
		t.Errorf("groups (C) wrong: %+v", out.Groups)
	}
	if out.GroupsError != "" {
		t.Errorf("unexpected GroupsError=%q", out.GroupsError)
	}
}

// TestProjectsAggregate_DedupesByCanonicalKey: two registry rows for the SAME
// project (different language) collapse into ONE DTO with two entries.
func TestProjectsAggregate_DedupesByCanonicalKey(t *testing.T) {
	isolateHome(t)
	stateDir := t.TempDir()
	t.Cleanup(api.SetDaemonStateRootForTest(stateDir))
	root := t.TempDir()

	injectWorkspaceRegistry(t, []api.WorkspaceEntry{
		{WorkspaceKey: "k1", WorkspacePath: root, Language: "go", Backend: "mcp-language-server"},
		{WorkspaceKey: "k1", WorkspacePath: root, Language: "python", Backend: "mcp-language-server"},
	})

	s := NewServer(Config{})
	rec := getProjectsAggregate(t, s)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var out projectsAggregateResponse
	if err := json.NewDecoder(rec.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(out.Projects) != 1 {
		t.Fatalf("projects len=%d, want 1 (deduped by canonical key)", len(out.Projects))
	}
	if len(out.Projects[0].Entries) != 2 {
		t.Errorf("entries len=%d, want 2 (go+python under one project)", len(out.Projects[0].Entries))
	}
	// Deterministic order (sorted by language).
	if out.Projects[0].Entries[0].Language != "go" || out.Projects[0].Entries[1].Language != "python" {
		t.Errorf("entries not sorted by language: %+v", out.Projects[0].Entries)
	}
}

// TestProjectsAggregate_GlobalScanUnchanged: building the aggregate (N project
// scans) does NOT perturb the global DefaultScanConfigPaths scan output.
func TestProjectsAggregate_GlobalScanUnchanged(t *testing.T) {
	isolateHome(t)
	stateDir := t.TempDir()
	t.Cleanup(api.SetDaemonStateRootForTest(stateDir))

	before, err := api.NewAPI().ScanFrom(api.ScanOpts{ConfigPaths: api.DefaultScanConfigPaths()})
	if err != nil {
		t.Fatalf("baseline global scan: %v", err)
	}

	root := t.TempDir()
	seedProject(t, root)
	injectWorkspaceRegistry(t, []api.WorkspaceEntry{
		{WorkspaceKey: "k1", WorkspacePath: root, Language: "go", Backend: "mcp-language-server"},
	})

	s := NewServer(Config{})
	rec := getProjectsAggregate(t, s)
	if rec.Code != http.StatusOK {
		t.Fatalf("aggregate status=%d body=%s", rec.Code, rec.Body.String())
	}

	after, err := api.NewAPI().ScanFrom(api.ScanOpts{ConfigPaths: api.DefaultScanConfigPaths()})
	if err != nil {
		t.Fatalf("post global scan: %v", err)
	}
	before.At = after.At
	sortEntriesByName(before.Entries)
	sortEntriesByName(after.Entries)
	if !reflect.DeepEqual(before, after) {
		t.Errorf("global scan changed across an aggregate build:\nbefore=%+v\nafter=%+v", before, after)
	}
}

// TestProjectsAggregate_DeletedRootCarriesScanError: a registered project whose
// dir no longer exists yields a per-project ScanError (not a whole-aggregate
// failure) and still surfaces the A data.
func TestProjectsAggregate_DeletedRootCarriesScanError(t *testing.T) {
	isolateHome(t)
	stateDir := t.TempDir()
	t.Cleanup(api.SetDaemonStateRootForTest(stateDir))

	gone := t.TempDir() + "-removed-xyz" // never created
	injectWorkspaceRegistry(t, []api.WorkspaceEntry{
		{WorkspaceKey: "k1", WorkspacePath: gone, Language: "go", Backend: "mcp-language-server"},
	})

	s := NewServer(Config{})
	rec := getProjectsAggregate(t, s)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var out projectsAggregateResponse
	if err := json.NewDecoder(rec.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(out.Projects) != 1 {
		t.Fatalf("projects len=%d, want 1", len(out.Projects))
	}
	if out.Projects[0].Scan != nil {
		t.Errorf("expected nil Scan for a missing root")
	}
	if out.Projects[0].ScanError != "PROJECT_ROOT_INVALID" {
		t.Errorf("ScanError=%q, want PROJECT_ROOT_INVALID", out.Projects[0].ScanError)
	}
	// A data still present.
	if len(out.Projects[0].Entries) != 1 {
		t.Errorf("A entries lost for a missing-root project")
	}
}
