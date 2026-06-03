package gui

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"mcp-local-hub/internal/api"
	"mcp-local-hub/internal/api/lsp_routing"
)

type stubLSPResolver struct {
	mu      sync.Mutex
	results map[string]*lsp_routing.ResolveResult
	errs    map[string]error
}

func (s *stubLSPResolver) ResolveByPath(path, language string) (*lsp_routing.ResolveResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := language + "|" + path
	if err := s.errs[key]; err != nil {
		return nil, err
	}
	if res := s.results[key]; res != nil {
		cp := *res
		if res.Entry != nil {
			entry := *res.Entry
			cp.Entry = &entry
		}
		return &cp, nil
	}
	return nil, lsp_routing.ErrWorkspaceNotFound
}

func postLSP(t *testing.T, s *Server, language string, body []byte, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/lsp/"+language+"/mcp", bytes.NewReader(body))
	if _, hasCT := headers["Content-Type"]; !hasCT {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rr := httptest.NewRecorder()
	s.mux.ServeHTTP(rr, req)
	return rr
}

func rpcBody(method string, id string, params string) []byte {
	if params == "" {
		params = "{}"
	}
	return []byte(`{"jsonrpc":"2.0","id":` + id + `,"method":"` + method + `","params":` + params + `}`)
}

func TestLSPRouter_InitializeAndToolsListUseCatalogWithoutProxy(t *testing.T) {
	s := NewServer(Config{Port: 9125})
	s.SetLSPRouterDeps(&lspRouterDeps{
		Resolver: &stubLSPResolver{},
		Sessions: lsp_routing.NewSessionRouter(),
		BackendKindForLanguage: func(lang string) (string, bool) {
			if lang == "go" {
				return "gopls-mcp", true
			}
			return "", false
		},
	})

	initRR := postLSP(t, s, "go", rpcBody("initialize", "1", `{}`), nil)
	if initRR.Code != http.StatusOK {
		t.Fatalf("initialize status = %d body=%s", initRR.Code, initRR.Body.String())
	}
	if sid := initRR.Header().Get("Mcp-Session-Id"); sid == "" {
		t.Fatal("initialize did not mint a client-side router session id")
	}
	if !strings.Contains(initRR.Body.String(), `"name":"gopls"`) {
		t.Fatalf("initialize did not use gopls catalog: %s", initRR.Body.String())
	}

	listRR := postLSP(t, s, "go", rpcBody("tools/list", "2", `{}`), nil)
	if listRR.Code != http.StatusOK {
		t.Fatalf("tools/list status = %d body=%s", listRR.Code, listRR.Body.String())
	}
	if !strings.Contains(listRR.Body.String(), `"name":"go_workspace"`) {
		t.Fatalf("tools/list did not come from the gopls catalog: %s", listRR.Body.String())
	}
}

func TestLSPRouter_RegisteredWorkspaceForwardsSessionless(t *testing.T) {
	var upstreamSession string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamSession = r.Header.Get("Mcp-Session-Id")
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Mcp-Session-Id", "upstream-session-must-not-leak")
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"ok":true}}`))
	}))
	t.Cleanup(upstream.Close)

	entry := &api.WorkspaceEntry{WorkspaceKey: "alpha", WorkspacePath: "/repo/alpha", Language: "python", Backend: "mcp-language-server", Port: 9201}
	resolver := &stubLSPResolver{results: map[string]*lsp_routing.ResolveResult{
		"python|/repo/alpha/main.py": {
			WorkspaceRoot: "/repo/alpha",
			WorkspaceKey:  "alpha",
			Registered:    true,
			Entry:         entry,
			ProjectMarker: true,
		},
	}}
	s := NewServer(Config{Port: 9125})
	s.SetLSPRouterDeps(&lspRouterDeps{
		Resolver:               resolver,
		Sessions:               lsp_routing.NewSessionRouter(),
		BackendKindForLanguage: func(lang string) (string, bool) { return "mcp-language-server", true },
		UpstreamURLFn:          func(ws *api.WorkspaceEntry) string { return upstream.URL },
	})

	body := rpcBody("tools/call", "1", `{"name":"diagnostics","arguments":{"filePath":"/repo/alpha/main.py"}}`)
	rr := postLSP(t, s, "python", body, map[string]string{"Mcp-Session-Id": "client-session"})
	if rr.Code != http.StatusOK {
		t.Fatalf("tools/call status = %d body=%s", rr.Code, rr.Body.String())
	}
	if upstreamSession != "" {
		t.Fatalf("router forwarded Mcp-Session-Id to sessionless LSP proxy: %q", upstreamSession)
	}
	if rr.Header().Get("Mcp-Session-Id") == "upstream-session-must-not-leak" {
		t.Fatal("router leaked upstream Mcp-Session-Id back to client")
	}
}

func TestLSPRouter_UnregisteredMarkerWorkspaceAutoRegistersThenForwards(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"registered":true}}`))
	}))
	t.Cleanup(upstream.Close)

	var autoCalls atomic.Int32
	entry := api.WorkspaceEntry{WorkspaceKey: "alpha", WorkspacePath: "/repo/alpha", Language: "python", Backend: "mcp-language-server", Port: 9201}
	resolver := &stubLSPResolver{results: map[string]*lsp_routing.ResolveResult{
		"python|/repo/alpha/main.py": {
			WorkspaceRoot: "/repo/alpha",
			WorkspaceKey:  "alpha",
			Registered:    false,
			ProjectMarker: true,
		},
	}}
	s := NewServer(Config{Port: 9125})
	s.SetLSPRouterDeps(&lspRouterDeps{
		Resolver:               resolver,
		Sessions:               lsp_routing.NewSessionRouter(),
		BackendKindForLanguage: func(lang string) (string, bool) { return "mcp-language-server", true },
		AutoRegisterFn: func(ctx context.Context, wsKey, workspacePath, language string) (*api.WorkspaceEntry, error) {
			autoCalls.Add(1)
			if wsKey != "alpha" || workspacePath != "/repo/alpha" || language != "python" {
				t.Fatalf("auto-register args = (%q, %q, %q)", wsKey, workspacePath, language)
			}
			return &entry, nil
		},
		UpstreamURLFn: func(ws *api.WorkspaceEntry) string { return upstream.URL },
	})

	body := rpcBody("tools/call", "1", `{"name":"diagnostics","arguments":{"filePath":"/repo/alpha/main.py"}}`)
	rr := postLSP(t, s, "python", body, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("tools/call status = %d body=%s", rr.Code, rr.Body.String())
	}
	if autoCalls.Load() != 1 {
		t.Fatalf("auto-register calls = %d, want 1", autoCalls.Load())
	}
}

func TestLSPRouter_GitOnlyResolvedWorkspaceDoesNotAutoSpawn(t *testing.T) {
	resolver := &stubLSPResolver{results: map[string]*lsp_routing.ResolveResult{
		"python|/repo/gitonly/main.py": {
			WorkspaceRoot: "/repo/gitonly",
			WorkspaceKey:  "gitonly",
			Registered:    false,
			ProjectMarker: false,
		},
	}}
	s := NewServer(Config{Port: 9125})
	s.SetLSPRouterDeps(&lspRouterDeps{
		Resolver:               resolver,
		Sessions:               lsp_routing.NewSessionRouter(),
		BackendKindForLanguage: func(lang string) (string, bool) { return "mcp-language-server", true },
		AutoRegisterFn: func(ctx context.Context, wsKey, workspacePath, language string) (*api.WorkspaceEntry, error) {
			t.Fatal("git-only fallback must not auto-register")
			return nil, errors.New("unexpected auto-register")
		},
	})

	body := rpcBody("tools/call", "1", `{"name":"diagnostics","arguments":{"filePath":"/repo/gitonly/main.py"}}`)
	rr := postLSP(t, s, "python", body, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("git-only status = %d body=%s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "language project marker") {
		t.Fatalf("git-only error should name marker gate, got: %s", rr.Body.String())
	}
}

func TestLSPRouter_WorkspaceNotFoundReturnsJSONRPCError(t *testing.T) {
	s := NewServer(Config{Port: 9125})
	s.SetLSPRouterDeps(&lspRouterDeps{
		Resolver: &stubLSPResolver{errs: map[string]error{
			"python|/outside/file.py": lsp_routing.ErrWorkspaceNotFound,
		}},
		Sessions:               lsp_routing.NewSessionRouter(),
		BackendKindForLanguage: func(lang string) (string, bool) { return "mcp-language-server", true },
	})

	body := rpcBody("tools/call", "1", `{"name":"diagnostics","arguments":{"filePath":"/outside/file.py"}}`)
	rr := postLSP(t, s, "python", body, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("not-found status = %d body=%s", rr.Code, rr.Body.String())
	}
	var env map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode JSON-RPC envelope: %v body=%s", err, rr.Body.String())
	}
	if env["error"] == nil {
		t.Fatalf("expected JSON-RPC error envelope, got: %s", rr.Body.String())
	}
}

func TestLSPRouter_PathlessDisambiguation(t *testing.T) {
	var forwarded []string
	upstreamAlpha := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		forwarded = append(forwarded, "alpha")
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"workspace":"alpha"}}`))
	}))
	t.Cleanup(upstreamAlpha.Close)
	upstreamBeta := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		forwarded = append(forwarded, "beta")
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"workspace":"beta"}}`))
	}))
	t.Cleanup(upstreamBeta.Close)

	alpha := &api.WorkspaceEntry{WorkspaceKey: "alpha", WorkspacePath: "/repo/alpha", Language: "go", Backend: "gopls-mcp", Port: 9201}
	beta := &api.WorkspaceEntry{WorkspaceKey: "beta", WorkspacePath: "/repo/beta", Language: "go", Backend: "gopls-mcp", Port: 9202}
	resolver := &stubLSPResolver{results: map[string]*lsp_routing.ResolveResult{
		"go|/repo/alpha/main.go": {WorkspaceRoot: "/repo/alpha", WorkspaceKey: "alpha", Registered: true, Entry: alpha, ProjectMarker: true},
		"go|/repo/beta/main.go":  {WorkspaceRoot: "/repo/beta", WorkspaceKey: "beta", Registered: true, Entry: beta, ProjectMarker: true},
	}}
	s := NewServer(Config{Port: 9125})
	s.SetLSPRouterDeps(&lspRouterDeps{
		Resolver:               resolver,
		Sessions:               lsp_routing.NewSessionRouter(),
		BackendKindForLanguage: func(lang string) (string, bool) { return "gopls-mcp", true },
		UpstreamURLFn: func(ws *api.WorkspaceEntry) string {
			if ws.WorkspaceKey == "alpha" {
				return upstreamAlpha.URL
			}
			return upstreamBeta.URL
		},
	})

	initRR := postLSP(t, s, "go", rpcBody("initialize", "1", `{}`), nil)
	sessionID := initRR.Header().Get("Mcp-Session-Id")
	if sessionID == "" {
		t.Fatal("initialize did not mint session id")
	}
	pathless := rpcBody("tools/call", "2", `{"name":"go_workspace","arguments":{}}`)
	zeroRR := postLSP(t, s, "go", pathless, map[string]string{"Mcp-Session-Id": sessionID})
	if !strings.Contains(zeroRR.Body.String(), "make a file-scoped call first") {
		t.Fatalf("0-workspace pathless error mismatch: %s", zeroRR.Body.String())
	}

	alphaBody := rpcBody("tools/call", "3", `{"name":"go_file_context","arguments":{"file":"/repo/alpha/main.go"}}`)
	alphaRR := postLSP(t, s, "go", alphaBody, map[string]string{"Mcp-Session-Id": sessionID})
	if alphaRR.Code != http.StatusOK {
		t.Fatalf("alpha path call status = %d body=%s", alphaRR.Code, alphaRR.Body.String())
	}
	oneRR := postLSP(t, s, "go", pathless, map[string]string{"Mcp-Session-Id": sessionID})
	if !strings.Contains(oneRR.Body.String(), `"workspace":"alpha"`) {
		t.Fatalf("1-workspace pathless did not route to alpha: %s", oneRR.Body.String())
	}

	betaBody := rpcBody("tools/call", "4", `{"name":"go_file_context","arguments":{"file":"/repo/beta/main.go"}}`)
	betaRR := postLSP(t, s, "go", betaBody, map[string]string{"Mcp-Session-Id": sessionID})
	if betaRR.Code != http.StatusOK {
		t.Fatalf("beta path call status = %d body=%s", betaRR.Code, betaRR.Body.String())
	}
	ambiguousRR := postLSP(t, s, "go", pathless, map[string]string{"Mcp-Session-Id": sessionID})
	if !strings.Contains(ambiguousRR.Body.String(), "ambiguous") ||
		!strings.Contains(ambiguousRR.Body.String(), "/repo/alpha") ||
		!strings.Contains(ambiguousRR.Body.String(), "/repo/beta") {
		t.Fatalf("N-workspace ambiguous error mismatch: %s", ambiguousRR.Body.String())
	}
}

func TestLSPRouter_ForwardsSSEPassthrough(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("event: message\ndata: {\"ok\":true}\n\n"))
	}))
	t.Cleanup(upstream.Close)

	entry := &api.WorkspaceEntry{WorkspaceKey: "alpha", WorkspacePath: "/repo/alpha", Language: "python", Backend: "mcp-language-server", Port: 9201}
	resolver := &stubLSPResolver{results: map[string]*lsp_routing.ResolveResult{
		"python|/repo/alpha/main.py": {WorkspaceRoot: "/repo/alpha", WorkspaceKey: "alpha", Registered: true, Entry: entry, ProjectMarker: true},
	}}
	s := NewServer(Config{Port: 9125})
	s.SetLSPRouterDeps(&lspRouterDeps{
		Resolver:               resolver,
		Sessions:               lsp_routing.NewSessionRouter(),
		BackendKindForLanguage: func(lang string) (string, bool) { return "mcp-language-server", true },
		UpstreamURLFn:          func(ws *api.WorkspaceEntry) string { return upstream.URL },
	})

	body := rpcBody("tools/call", "1", `{"name":"diagnostics","arguments":{"filePath":"/repo/alpha/main.py"}}`)
	rr := postLSP(t, s, "python", body, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("SSE forward status = %d body=%s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Header().Get("Content-Type"), "text/event-stream") {
		t.Fatalf("Content-Type = %q, want text/event-stream", rr.Header().Get("Content-Type"))
	}
	if rr.Body.String() != "event: message\ndata: {\"ok\":true}\n\n" {
		t.Fatalf("SSE body mismatch: %q", rr.Body.String())
	}
}
