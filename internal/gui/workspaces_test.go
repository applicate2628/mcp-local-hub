package gui

import (
	"encoding/json"
	"net/http/httptest"
	"testing"

	"mcp-local-hub/internal/api"
)

// Restore the test seam after each subtest so test ordering can't leak
// the synthetic registry into other handlers that touch the registry.
func resetWorkspacesTestSeam(t *testing.T) {
	t.Helper()
	prev := workspacesTestSeam
	t.Cleanup(func() { workspacesTestSeam = prev })
}

func TestWorkspaces_GET_EmptyRegistry(t *testing.T) {
	resetWorkspacesTestSeam(t)
	workspacesTestSeam = func() (*api.Registry, error) {
		return api.NewRegistry("/synthetic/empty/path"), nil
	}

	s := NewServer(Config{Port: 9125, Version: "test", PID: 1})
	req := httptest.NewRequest("GET", "/api/workspaces", nil)
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	rr := httptest.NewRecorder()
	s.mux.ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("got %d: %s", rr.Code, rr.Body.String())
	}
	var resp workspacesResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.Workspaces) != 0 || len(resp.Entries) != 0 {
		t.Errorf("expected empty arrays, got Workspaces=%d Entries=%d", len(resp.Workspaces), len(resp.Entries))
	}
}

func TestWorkspaces_GET_DedupesByKey_SortedByKeyThenLanguage(t *testing.T) {
	resetWorkspacesTestSeam(t)
	workspacesTestSeam = func() (*api.Registry, error) {
		reg := api.NewRegistry("/synthetic/path")
		// Intentionally out-of-order to exercise the sort.
		reg.Workspaces = []api.WorkspaceEntry{
			{WorkspaceKey: "beta", WorkspacePath: "/proj/beta", Language: "rust", Backend: "mcp-language-server", Port: 9202, TaskName: `\mcp-local-hub-lsp-beta-rust`},
			{WorkspaceKey: "alpha", WorkspacePath: "/proj/alpha", Language: "go", Backend: "gopls-mcp", Port: 9201, TaskName: `\mcp-local-hub-lsp-alpha-go`},
			{WorkspaceKey: "alpha", WorkspacePath: "/proj/alpha", Language: "rust", Backend: "mcp-language-server", Port: 9203, TaskName: `\mcp-local-hub-lsp-alpha-rust`},
			{WorkspaceKey: "alpha", WorkspacePath: "/proj/alpha", Language: "clangd", Backend: "mcp-language-server", Port: 9200, TaskName: `\mcp-local-hub-lsp-alpha-clangd`},
		}
		return reg, nil
	}

	s := NewServer(Config{Port: 9125, Version: "test", PID: 1})
	req := httptest.NewRequest("GET", "/api/workspaces", nil)
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	rr := httptest.NewRecorder()
	s.mux.ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("got %d: %s", rr.Code, rr.Body.String())
	}
	var resp workspacesResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.Workspaces) != 2 {
		t.Fatalf("expected 2 deduped pairs, got %d: %+v", len(resp.Workspaces), resp.Workspaces)
	}
	if resp.Workspaces[0].WorkspaceKey != "alpha" || resp.Workspaces[1].WorkspaceKey != "beta" {
		t.Errorf("expected sorted [alpha,beta], got %+v", resp.Workspaces)
	}
	if len(resp.Entries) != 4 {
		t.Fatalf("expected 4 entries, got %d", len(resp.Entries))
	}
	// Sort invariant: (key, language) ascending. alpha:{clangd,go,rust}, then beta:{rust}.
	want := []struct{ key, lang string }{
		{"alpha", "clangd"},
		{"alpha", "go"},
		{"alpha", "rust"},
		{"beta", "rust"},
	}
	for i, w := range want {
		if resp.Entries[i].WorkspaceKey != w.key || resp.Entries[i].Language != w.lang {
			t.Errorf("entries[%d] = (%s, %s), want (%s, %s)", i,
				resp.Entries[i].WorkspaceKey, resp.Entries[i].Language, w.key, w.lang)
		}
	}
	// TaskName carried through so the frontend can match LSP-row taskName
	// → registry entry without re-deriving from manifest+key.
	if resp.Entries[0].TaskName != `\mcp-local-hub-lsp-alpha-clangd` {
		t.Errorf("alpha-clangd TaskName = %q", resp.Entries[0].TaskName)
	}
}

func TestWorkspaces_NonGET_405(t *testing.T) {
	resetWorkspacesTestSeam(t)
	workspacesTestSeam = func() (*api.Registry, error) {
		return api.NewRegistry("/synthetic"), nil
	}
	s := NewServer(Config{Port: 9125, Version: "test", PID: 1})
	req := httptest.NewRequest("POST", "/api/workspaces", nil)
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	req.Header.Set("Origin", "http://127.0.0.1:9125")
	rr := httptest.NewRecorder()
	s.mux.ServeHTTP(rr, req)
	if rr.Code != 405 {
		t.Errorf("expected 405, got %d: %s", rr.Code, rr.Body.String())
	}
}
