package gui

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
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

func TestLSPRegister_POST_OK(t *testing.T) {
	var gotWorkspace string
	var gotLanguages []string
	s := NewServer(Config{Port: 9125, Version: "test", PID: 1})
	s.lspRegistrar = fakeLSPRegistrar{
		RegisterFn: func(workspacePath string, languages []string) (*lspRegisterReport, error) {
			gotWorkspace = workspacePath
			gotLanguages = append([]string(nil), languages...)
			return &lspRegisterReport{
				Workspace:    workspacePath,
				WorkspaceKey: "project",
				Entries: []api.WorkspaceEntry{
					{
						WorkspaceKey:  "project",
						WorkspacePath: workspacePath,
						Language:      "go",
						Backend:       "gopls-mcp",
						Port:          9201,
						TaskName:      `\mcp-local-hub-lsp-project-go`,
					},
				},
				Results: []lspRegisterLanguageResult{{Language: "go", Status: "ok"}},
			}, nil
		},
	}

	body := bytes.NewBufferString(`{"workspace_path":"D:/dev/project","language":"go"}`)
	req := httptest.NewRequest("POST", "/api/lsp/register", body)
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	s.mux.ServeHTTP(rr, req)

	if rr.Code != 200 {
		t.Fatalf("got %d: %s", rr.Code, rr.Body.String())
	}
	if gotWorkspace != "D:/dev/project" {
		t.Errorf("workspace_path = %q, want D:/dev/project", gotWorkspace)
	}
	if len(gotLanguages) != 1 || gotLanguages[0] != "go" {
		t.Errorf("languages = %+v, want [go]", gotLanguages)
	}
	var resp map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	entries, ok := resp["entries"].([]any)
	if !ok || len(entries) != 1 {
		t.Fatalf("response entries = %#v, want one entry array", resp["entries"])
	}
	entry, ok := entries[0].(map[string]any)
	if !ok {
		t.Fatalf("response entry type = %T", entries[0])
	}
	if entry["task_name"] != `\mcp-local-hub-lsp-project-go` {
		t.Errorf("task_name = %#v, want snake_case task_name", entry["task_name"])
	}
	if _, exists := entry["TaskName"]; exists {
		t.Fatalf("response entry leaked PascalCase TaskName key: %#v", entry)
	}
}

func TestRealLSPRegistrar_UsesEnsureLSPRegistered(t *testing.T) {
	prev := ensureLSPRegisteredForGUI
	t.Cleanup(func() { ensureLSPRegisteredForGUI = prev })

	// Stub the explicit-register bless seam so the test does NOT touch the
	// real %LOCALAPPDATA% trusted-roots store, and so we can assert the
	// EXPLICIT register blesses the workspace's canonical root exactly once.
	prevBless := blessLSPTrustedRootForGUI
	t.Cleanup(func() { blessLSPTrustedRootForGUI = prevBless })
	var blessed []string
	blessLSPTrustedRootForGUI = func(workspaceRoot string) error {
		blessed = append(blessed, workspaceRoot)
		return nil
	}

	var calls []string
	ensureLSPRegisteredForGUI = func(ctx context.Context, workspaceKey, workspacePath, language string) (api.WorkspaceEntry, error) {
		if ctx == nil {
			t.Fatal("EnsureLSPRegistered context is nil")
		}
		if workspaceKey != "" {
			t.Fatalf("workspaceKey = %q, want empty so API computes the canonical key", workspaceKey)
		}
		calls = append(calls, language)
		return api.WorkspaceEntry{
			WorkspaceKey:  "project",
			WorkspacePath: "/canonical/dev/project",
			Language:      language,
			Backend:       "mcp-language-server",
			Port:          9200 + len(calls),
			TaskName:      api.LSPTaskNameForWorkspaceLanguage("project", language),
			ClientEntries: map[string]string{},
		}, nil
	}

	report, err := (realLSPRegistrar{}).RegisterLSP("D:/dev/project", []string{"go", "python"})
	if err != nil {
		t.Fatalf("RegisterLSP: %v", err)
	}
	if len(calls) != 2 || calls[0] != "go" || calls[1] != "python" {
		t.Fatalf("EnsureLSPRegistered calls = %v, want [go python]", calls)
	}
	if report.WorkspaceKey != "project" {
		t.Fatalf("report workspace key = %q, want project", report.WorkspaceKey)
	}
	if len(report.Entries) != 2 {
		t.Fatalf("report entries = %d, want 2", len(report.Entries))
	}
	if len(report.Results) != 2 || report.Results[0].Status != "ok" || report.Results[1].Status != "ok" {
		t.Fatalf("report results = %+v, want two ok language results", report.Results)
	}
	for _, entry := range report.Entries {
		if len(entry.ClientEntries) != 0 {
			t.Fatalf("GUI LSP enable must not write client entries; got %+v", entry.ClientEntries)
		}
	}
	// Bless once per workspace (not once per language), using the canonical
	// WorkspacePath EnsureLSPRegistered returned — NOT the raw request path.
	if len(blessed) != 1 {
		t.Fatalf("explicit GUI register should bless the trusted root exactly once, got %d: %v", len(blessed), blessed)
	}
	if blessed[0] != "/canonical/dev/project" {
		t.Fatalf("blessed root = %q, want the canonical WorkspacePath /canonical/dev/project", blessed[0])
	}
}

// TestRealLSPRegistrar_DoesNotBlessWhenEveryLanguageFails asserts the
// bless seam is NOT invoked when no language registered successfully —
// there is no canonical root to seed trust from.
func TestRealLSPRegistrar_DoesNotBlessWhenEveryLanguageFails(t *testing.T) {
	prev := ensureLSPRegisteredForGUI
	t.Cleanup(func() { ensureLSPRegisteredForGUI = prev })
	ensureLSPRegisteredForGUI = func(ctx context.Context, workspaceKey, workspacePath, language string) (api.WorkspaceEntry, error) {
		return api.WorkspaceEntry{}, errors.New("register failed")
	}

	prevBless := blessLSPTrustedRootForGUI
	t.Cleanup(func() { blessLSPTrustedRootForGUI = prevBless })
	blessCalls := 0
	blessLSPTrustedRootForGUI = func(workspaceRoot string) error {
		blessCalls++
		return nil
	}

	report, err := (realLSPRegistrar{}).RegisterLSP("D:/dev/project", []string{"go"})
	if err != nil {
		t.Fatalf("RegisterLSP: %v", err)
	}
	if len(report.Entries) != 0 {
		t.Fatalf("expected zero entries on total failure, got %+v", report.Entries)
	}
	if blessCalls != 0 {
		t.Fatalf("bless must not fire when no language registered, got %d calls", blessCalls)
	}
}

func TestRealLSPRegistrar_ReportsPartialBatchFailures(t *testing.T) {
	prev := ensureLSPRegisteredForGUI
	t.Cleanup(func() { ensureLSPRegisteredForGUI = prev })

	// Stub the bless seam so the test does not write the real
	// %LOCALAPPDATA% trusted-roots store (the "go" language registers ok,
	// which would otherwise bless).
	prevBless := blessLSPTrustedRootForGUI
	t.Cleanup(func() { blessLSPTrustedRootForGUI = prevBless })
	blessLSPTrustedRootForGUI = func(string) error { return nil }

	ensureLSPRegisteredForGUI = func(ctx context.Context, workspaceKey, workspacePath, language string) (api.WorkspaceEntry, error) {
		if language == "not-a-language" {
			return api.WorkspaceEntry{}, errors.New("unknown LSP language not-a-language")
		}
		return api.WorkspaceEntry{
			WorkspaceKey:  "project",
			WorkspacePath: workspacePath,
			Language:      language,
			Backend:       "gopls-mcp",
			Port:          9201,
			TaskName:      api.LSPTaskNameForWorkspaceLanguage("project", language),
		}, nil
	}

	report, err := (realLSPRegistrar{}).RegisterLSP("D:/dev/project", []string{"go", "not-a-language"})
	if err != nil {
		t.Fatalf("RegisterLSP partial batch returned error: %v", err)
	}
	if len(report.Entries) != 1 || report.Entries[0].Language != "go" {
		t.Fatalf("entries = %+v, want only successful go entry", report.Entries)
	}
	if len(report.Results) != 2 {
		t.Fatalf("results = %+v, want per-language result for both inputs", report.Results)
	}
	if report.Results[0].Language != "go" || report.Results[0].Status != "ok" || report.Results[0].Error != "" {
		t.Fatalf("result[0] = %+v, want go ok", report.Results[0])
	}
	if report.Results[1].Language != "not-a-language" || report.Results[1].Status != "error" ||
		!strings.Contains(report.Results[1].Error, "unknown LSP language") {
		t.Fatalf("result[1] = %+v, want not-a-language error", report.Results[1])
	}
}

func TestLSPRegister_POSTLanguagesReturnsPartialSuccessReport(t *testing.T) {
	s := NewServer(Config{Port: 9125, Version: "test", PID: 1})
	s.lspRegistrar = fakeLSPRegistrar{
		RegisterFn: func(workspacePath string, languages []string) (*lspRegisterReport, error) {
			if len(languages) != 2 || languages[0] != "go" || languages[1] != "not-a-language" {
				t.Fatalf("languages = %+v, want [go not-a-language]", languages)
			}
			return &lspRegisterReport{
				Workspace:    workspacePath,
				WorkspaceKey: "project",
				Entries: []api.WorkspaceEntry{{
					WorkspaceKey:  "project",
					WorkspacePath: workspacePath,
					Language:      "go",
					Backend:       "gopls-mcp",
					Port:          9201,
					TaskName:      `\mcp-local-hub-lsp-project-go`,
				}},
				Results: []lspRegisterLanguageResult{
					{Language: "go", Status: "ok"},
					{Language: "not-a-language", Status: "error", Error: "unknown LSP language not-a-language"},
				},
			}, nil
		},
	}

	body := bytes.NewBufferString(`{"workspace_path":"D:/dev/project","languages":["go","not-a-language"]}`)
	req := httptest.NewRequest("POST", "/api/lsp/register", body)
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	s.mux.ServeHTTP(rr, req)

	if rr.Code != http.StatusMultiStatus {
		t.Fatalf("status = %d, want 207 partial success; body=%s", rr.Code, rr.Body.String())
	}
	var resp struct {
		Entries []workspaceEntryDTO         `json:"entries"`
		Results []lspRegisterLanguageResult `json:"results"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.Entries) != 1 || resp.Entries[0].TaskName != `\mcp-local-hub-lsp-project-go` {
		t.Fatalf("entries = %+v, want successful go DTO", resp.Entries)
	}
	if len(resp.Results) != 2 || resp.Results[1].Status != "error" {
		t.Fatalf("results = %+v, want per-language error", resp.Results)
	}
}

func TestLSPRegister_POST_RejectsCrossOrigin(t *testing.T) {
	s := NewServer(Config{Port: 9125, Version: "test", PID: 1})
	s.lspRegistrar = fakeLSPRegistrar{
		RegisterFn: func(string, []string) (*lspRegisterReport, error) {
			t.Fatal("registrar must not be called for cross-origin requests")
			return nil, nil
		},
	}

	req := httptest.NewRequest("POST", "/api/lsp/register", bytes.NewBufferString(`{"workspace_path":"D:/dev/project","language":"go"}`))
	req.Header.Set("Origin", "https://example.test")
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	s.mux.ServeHTTP(rr, req)

	if rr.Code != 403 {
		t.Errorf("expected 403, got %d: %s", rr.Code, rr.Body.String())
	}
}

type fakeLSPRegistrar struct {
	RegisterFn func(workspacePath string, languages []string) (*lspRegisterReport, error)
}

func (f fakeLSPRegistrar) RegisterLSP(workspacePath string, languages []string) (*lspRegisterReport, error) {
	if f.RegisterFn == nil {
		return nil, fmt.Errorf("RegisterFn not configured")
	}
	return f.RegisterFn(workspacePath, languages)
}
