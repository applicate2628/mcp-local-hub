package gui

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"mcp-local-hub/internal/api"
	"mcp-local-hub/internal/api/lsp_routing"
)

type stubLSPResolver struct {
	mu            sync.Mutex
	results       map[string]*lsp_routing.ResolveResult
	errs          map[string]error
	markers       map[string]bool
	registrations map[string]*api.WorkspaceEntry
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

func (s *stubLSPResolver) HasProjectMarker(root, language string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.markers[language+"|"+root]
}

func (s *stubLSPResolver) RegisteredWorkspace(workspaceKey, language string) (*api.WorkspaceEntry, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.registrations == nil {
		return nil, false
	}
	entry := s.registrations[language+"|"+workspaceKey]
	if entry == nil {
		return nil, false
	}
	cp := *entry
	return &cp, true
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

func lspToolCallParamsJSON(t *testing.T, name string, arguments map[string]any) string {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"name":      name,
		"arguments": arguments,
	})
	if err != nil {
		t.Fatalf("marshal tools/call params: %v", err)
	}
	return string(body)
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

func TestLSPRouter_ResourcesAndPromptsListAreSyntheticEmpty(t *testing.T) {
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
	if !strings.Contains(initRR.Body.String(), `"resources"`) ||
		!strings.Contains(initRR.Body.String(), `"prompts"`) {
		t.Fatalf("initialize did not advertise resources/prompts capabilities: %s", initRR.Body.String())
	}

	for _, tc := range []struct {
		method string
		field  string
		id     string
	}{
		{method: "resources/list", field: "resources", id: "2"},
		{method: "prompts/list", field: "prompts", id: "3"},
	} {
		t.Run(tc.method, func(t *testing.T) {
			rr := postLSP(t, s, "go", rpcBody(tc.method, tc.id, `{}`), nil)
			if rr.Code != http.StatusOK {
				t.Fatalf("%s status = %d body=%s", tc.method, rr.Code, rr.Body.String())
			}
			var resp struct {
				JSONRPC string                       `json:"jsonrpc"`
				ID      int                          `json:"id"`
				Result  map[string][]json.RawMessage `json:"result"`
				Error   json.RawMessage              `json:"error"`
			}
			if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
				t.Fatalf("decode %s response: %v; raw=%s", tc.method, err, rr.Body.String())
			}
			if len(resp.Error) != 0 {
				t.Fatalf("%s returned error: %s", tc.method, string(resp.Error))
			}
			if resp.JSONRPC != "2.0" {
				t.Fatalf("%s jsonrpc = %q, want 2.0", tc.method, resp.JSONRPC)
			}
			if got := resp.ID; got != mustAtoiForLSPTest(t, tc.id) {
				t.Fatalf("%s id = %d, want %s", tc.method, got, tc.id)
			}
			if items, ok := resp.Result[tc.field]; !ok {
				t.Fatalf("%s result missing %q: %+v", tc.method, tc.field, resp.Result)
			} else if len(items) != 0 {
				t.Fatalf("%s result.%s length = %d, want 0; items=%+v", tc.method, tc.field, len(items), items)
			}
		})
	}
}

func mustAtoiForLSPTest(t *testing.T, raw string) int {
	t.Helper()
	switch raw {
	case "1":
		return 1
	case "2":
		return 2
	case "3":
		return 3
	default:
		t.Fatalf("test helper only supports small literal ids, got %q", raw)
		return 0
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

func TestLSPRouter_RegisteredWorkspaceWaitsForEnsureBeforeForward(t *testing.T) {
	upstreamHit := make(chan struct{}, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamHit <- struct{}{}
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"ready":true}}`))
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
	ensureEntered := make(chan struct{})
	releaseEnsure := make(chan struct{})
	s := NewServer(Config{Port: 9125})
	s.SetLSPRouterDeps(&lspRouterDeps{
		Resolver:               resolver,
		Sessions:               lsp_routing.NewSessionRouter(),
		BackendKindForLanguage: func(lang string) (string, bool) { return "mcp-language-server", true },
		AutoRegisterFn: func(ctx context.Context, wsKey, workspacePath, language string) (*api.WorkspaceEntry, error) {
			if wsKey != "alpha" || workspacePath != "/repo/alpha" || language != "python" {
				t.Fatalf("ensure args = (%q, %q, %q)", wsKey, workspacePath, language)
			}
			close(ensureEntered)
			<-releaseEnsure
			return entry, nil
		},
		UpstreamURLFn: func(ws *api.WorkspaceEntry) string { return upstream.URL },
	})

	done := make(chan *httptest.ResponseRecorder, 1)
	body := rpcBody("tools/call", "1", `{"name":"diagnostics","arguments":{"filePath":"/repo/alpha/main.py"}}`)
	go func() {
		done <- postLSP(t, s, "python", body, nil)
	}()

	select {
	case <-ensureEntered:
	case rr := <-done:
		t.Fatalf("router forwarded registered row before EnsureLSPRegistered joined readiness: status=%d body=%s", rr.Code, rr.Body.String())
	case <-time.After(2 * time.Second):
		t.Fatal("router neither entered EnsureLSPRegistered nor completed request")
	}
	select {
	case <-upstreamHit:
		t.Fatal("router forwarded upstream before EnsureLSPRegistered was released")
	case <-done:
		t.Fatal("router completed request before EnsureLSPRegistered was released")
	case <-time.After(100 * time.Millisecond):
	}

	close(releaseEnsure)
	select {
	case rr := <-done:
		if rr.Code != http.StatusOK {
			t.Fatalf("tools/call status = %d body=%s", rr.Code, rr.Body.String())
		}
	case <-time.After(2 * time.Second):
		t.Fatal("router did not complete after EnsureLSPRegistered was released")
	}
}

func TestLSPRouter_UntrustedMarkerWorkspaceRefusedAndNotAutoRegistered(t *testing.T) {
	// SECURITY (trusted-root gate): an unregistered, project-marker-bearing
	// workspace whose root is NOT contained by any trusted root must be
	// REFUSED with the #269 "not registered" error, and AutoRegisterFn must
	// NOT be called — the untrusted MCP tool-argument path must not be able
	// to authorize a brand-new local LSP workspace.
	var autoCalls atomic.Int32
	resolver := &stubLSPResolver{results: map[string]*lsp_routing.ResolveResult{
		"python|/repo/alpha/main.py": {
			WorkspaceRoot: "/repo/alpha",
			WorkspaceKey:  "alpha",
			Registered:    false,
			ProjectMarker: true, // marker present, but markers are NOT authorization
		},
	}}
	s := NewServer(Config{Port: 9125})
	s.SetLSPRouterDeps(&lspRouterDeps{
		Resolver:               resolver,
		Sessions:               lsp_routing.NewSessionRouter(),
		BackendKindForLanguage: func(lang string) (string, bool) { return "mcp-language-server", true },
		AutoRegisterFn: func(ctx context.Context, wsKey, workspacePath, language string) (*api.WorkspaceEntry, error) {
			autoCalls.Add(1)
			return nil, errors.New("auto-register must not run for an untrusted root")
		},
		// Untrusted: no root contains /repo/alpha.
		TrustedRootCheckFn: func(workspaceRoot string) (bool, error) { return false, nil },
	})

	body := rpcBody("tools/call", "1", `{"name":"diagnostics","arguments":{"filePath":"/repo/alpha/main.py"}}`)
	rr := postLSP(t, s, "python", body, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("tools/call status = %d body=%s", rr.Code, rr.Body.String())
	}
	if autoCalls.Load() != 0 {
		t.Fatalf("auto-register calls = %d, want 0 (untrusted root must not auto-register)", autoCalls.Load())
	}
	if !strings.Contains(rr.Body.String(), "is not registered") ||
		!strings.Contains(rr.Body.String(), "mcphub register") {
		t.Fatalf("untrusted workspace error should require explicit registration, got: %s", rr.Body.String())
	}
}

// TestLSPRouter_TrustedRootWorkspaceAutoRegistersThenForwards is the
// other half of the gate: a path UNDER a trusted root (operator-config
// allowed root OR a root blessed by a prior explicit register) DOES
// auto-register and forward — the convenience PR #266 shipped is kept.
func TestLSPRouter_TrustedRootWorkspaceAutoRegistersThenForwards(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"registered":true}}`))
	}))
	t.Cleanup(upstream.Close)

	var autoCalls atomic.Int32
	var checkedRoot string
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
		// Trusted: /repo/alpha is contained by a trusted root. The gate is
		// the only thing that lets first-touch auto-register proceed.
		TrustedRootCheckFn: func(workspaceRoot string) (bool, error) {
			checkedRoot = workspaceRoot
			return true, nil
		},
		UpstreamURLFn: func(ws *api.WorkspaceEntry) string { return upstream.URL },
	})

	body := rpcBody("tools/call", "1", `{"name":"diagnostics","arguments":{"filePath":"/repo/alpha/main.py"}}`)
	rr := postLSP(t, s, "python", body, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("tools/call status = %d body=%s", rr.Code, rr.Body.String())
	}
	if autoCalls.Load() != 1 {
		t.Fatalf("auto-register calls = %d, want 1 (trusted root must auto-register)", autoCalls.Load())
	}
	if checkedRoot != "/repo/alpha" {
		t.Fatalf("trusted-root gate checked %q, want the resolved WorkspaceRoot /repo/alpha", checkedRoot)
	}
}

// TestLSPRouter_TrustedRootGateErrorFailsClosed asserts the gate fails
// CLOSED: a TrustedRootCheckFn error (corrupt store, insecure-parent
// rejection) refuses auto-register exactly like an untrusted path,
// rather than silently authorizing.
func TestLSPRouter_TrustedRootGateErrorFailsClosed(t *testing.T) {
	var autoCalls atomic.Int32
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
			return nil, errors.New("auto-register must not run when the gate errors")
		},
		TrustedRootCheckFn: func(workspaceRoot string) (bool, error) {
			return false, errors.New("corrupt trusted-roots store")
		},
	})

	body := rpcBody("tools/call", "1", `{"name":"diagnostics","arguments":{"filePath":"/repo/alpha/main.py"}}`)
	rr := postLSP(t, s, "python", body, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("tools/call status = %d body=%s", rr.Code, rr.Body.String())
	}
	if autoCalls.Load() != 0 {
		t.Fatalf("auto-register calls = %d, want 0 (gate error must fail closed)", autoCalls.Load())
	}
	if !strings.Contains(rr.Body.String(), "is not registered") {
		t.Fatalf("gate-error refusal should use the not-registered error, got: %s", rr.Body.String())
	}
}

// TestLSPRouter_NilTrustedRootGateFailsClosed asserts that legacy deps
// with no TrustedRootCheckFn wired refuse first-touch auto-register — an
// unset gate must never silently authorize.
func TestLSPRouter_NilTrustedRootGateFailsClosed(t *testing.T) {
	var autoCalls atomic.Int32
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
			return nil, errors.New("auto-register must not run with no gate wired")
		},
		// TrustedRootCheckFn intentionally nil.
	})

	body := rpcBody("tools/call", "1", `{"name":"diagnostics","arguments":{"filePath":"/repo/alpha/main.py"}}`)
	rr := postLSP(t, s, "python", body, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("tools/call status = %d body=%s", rr.Code, rr.Body.String())
	}
	if autoCalls.Load() != 0 {
		t.Fatalf("auto-register calls = %d, want 0 (nil gate must fail closed)", autoCalls.Load())
	}
}

// TestLSPRouter_AutoRegisterPathDoesNotBless is the load-bearing
// security invariant: the ROUTER's first-touch auto-register path must
// NEVER bless a trusted root. Blessing on the router path would let an
// untrusted MCP tool-argument path self-trust and pass the gate on the
// very next request, re-opening the vulnerability.
//
// The router exposes no bless seam by construction (lspRouterDeps has
// AutoRegisterFn + a read-only TrustedRootCheckFn, no bless function),
// so this test redirects the state dir to a fresh temp tree, drives a
// trusted first-touch auto-register through the real router handler, and
// asserts the on-disk lsp-trusted-roots.json store is NOT created or
// modified by the router path.
func TestLSPRouter_AutoRegisterPathDoesNotBless(t *testing.T) {
	stateDir := t.TempDir()
	restore := api.SetDaemonStateRootForTest(stateDir)
	t.Cleanup(restore)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"registered":true}}`))
	}))
	t.Cleanup(upstream.Close)

	storePath, err := api.DefaultLSPTrustedRootsPath()
	if err != nil {
		t.Fatalf("resolve trusted-roots store path: %v", err)
	}
	if _, statErr := os.Stat(storePath); statErr == nil {
		t.Fatalf("trusted-roots store unexpectedly exists before the router runs: %s", storePath)
	}

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
			// Production wires this to api.NewAPI().EnsureLSPRegistered,
			// which performs NO bless. The stub mirrors that contract: it
			// registers but must not write the trusted-roots store.
			return &entry, nil
		},
		// Trusted so the first-touch auto-register PROCEEDS — that is the
		// path that must not bless.
		TrustedRootCheckFn: func(workspaceRoot string) (bool, error) { return true, nil },
		UpstreamURLFn:      func(ws *api.WorkspaceEntry) string { return upstream.URL },
	})

	body := rpcBody("tools/call", "1", `{"name":"diagnostics","arguments":{"filePath":"/repo/alpha/main.py"}}`)
	rr := postLSP(t, s, "python", body, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("tools/call status = %d body=%s", rr.Code, rr.Body.String())
	}

	// The router auto-registered (trusted), but it must NOT have blessed:
	// the on-disk store must still not exist.
	if _, statErr := os.Stat(storePath); statErr == nil {
		t.Fatalf("ROUTER auto-register path blessed a trusted root (store created at %s) — this re-opens the vulnerability", storePath)
	} else if !os.IsNotExist(statErr) {
		t.Fatalf("unexpected stat error on trusted-roots store: %v", statErr)
	}
	// And the live gate still reports the store empty (no root recorded).
	f, err := api.LoadDefaultLSPTrustedRoots()
	if err != nil {
		t.Fatalf("load trusted-roots after router run: %v", err)
	}
	if len(f.Roots) != 0 {
		t.Fatalf("router path must record zero trusted roots, got %v", f.Roots)
	}
}

func TestLSPRouter_GitOnlyResolvedWorkspaceDoesNotAutoSpawn(t *testing.T) {
	// Under the trusted-root gate, an unregistered git-only workspace that
	// is NOT under any trusted root is refused at the authorization
	// boundary (not the marker gate, which PR #269 retired). AutoRegisterFn
	// must not be called.
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
		TrustedRootCheckFn: func(workspaceRoot string) (bool, error) { return false, nil },
	})

	body := rpcBody("tools/call", "1", `{"name":"diagnostics","arguments":{"filePath":"/repo/gitonly/main.py"}}`)
	rr := postLSP(t, s, "python", body, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("git-only status = %d body=%s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "is not registered") ||
		!strings.Contains(rr.Body.String(), "mcphub register") {
		t.Fatalf("git-only untrusted error should require explicit registration, got: %s", rr.Body.String())
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

func TestLSPRouter_PathlessSingleCandidateReEnsuresWorkspace(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"workspace":"alpha"}}`))
	}))
	t.Cleanup(upstream.Close)

	sessions := lsp_routing.NewSessionRouter()
	stale := api.WorkspaceEntry{WorkspaceKey: "alpha", WorkspacePath: "/repo/alpha", Language: "go", Backend: "gopls-mcp", Port: 9201}
	refreshed := stale
	refreshed.Port = 9209
	sessions.TouchWorkspace("client-session", &stale)

	var autoCalls atomic.Int32
	s := NewServer(Config{Port: 9125})
	s.SetLSPRouterDeps(&lspRouterDeps{
		Resolver: &stubLSPResolver{markers: map[string]bool{
			"go|/repo/alpha": true,
		}},
		Sessions:               sessions,
		BackendKindForLanguage: func(lang string) (string, bool) { return "gopls-mcp", true },
		AutoRegisterFn: func(ctx context.Context, wsKey, workspacePath, language string) (*api.WorkspaceEntry, error) {
			autoCalls.Add(1)
			if wsKey != "alpha" || workspacePath != "/repo/alpha" || language != "go" {
				t.Fatalf("pathless re-ensure args = (%q, %q, %q)", wsKey, workspacePath, language)
			}
			return &refreshed, nil
		},
		UpstreamURLFn: func(ws *api.WorkspaceEntry) string {
			if ws.Port != refreshed.Port {
				t.Fatalf("pathless forwarded stale port %d, want refreshed port %d", ws.Port, refreshed.Port)
			}
			return upstream.URL
		},
	})

	body := rpcBody("tools/call", "1", `{"name":"go_workspace","arguments":{}}`)
	rr := postLSP(t, s, "go", body, map[string]string{"Mcp-Session-Id": "client-session"})
	if rr.Code != http.StatusOK {
		t.Fatalf("pathless call status = %d body=%s", rr.Code, rr.Body.String())
	}
	if calls := autoCalls.Load(); calls != 1 {
		t.Fatalf("AutoRegisterFn calls = %d, want 1 for pathless single candidate", calls)
	}
}

func TestLSPRouter_PathlessSingleCandidateRefusesWhenRequestedLanguageMarkerAbsent(t *testing.T) {
	sessions := lsp_routing.NewSessionRouter()
	cached := api.WorkspaceEntry{WorkspaceKey: "alpha", WorkspacePath: "/repo/alpha", Language: "python", Backend: "mcp-language-server", Port: 9201}
	sessions.TouchWorkspace("client-session", &cached)

	var autoCalls atomic.Int32
	s := NewServer(Config{Port: 9125})
	s.SetLSPRouterDeps(&lspRouterDeps{
		Resolver: &stubLSPResolver{markers: map[string]bool{
			"go|/repo/alpha": false,
		}},
		Sessions:               sessions,
		BackendKindForLanguage: func(lang string) (string, bool) { return "gopls-mcp", true },
		AutoRegisterFn: func(ctx context.Context, wsKey, workspacePath, language string) (*api.WorkspaceEntry, error) {
			autoCalls.Add(1)
			return nil, errors.New("unexpected auto-register")
		},
	})

	body := rpcBody("tools/call", "1", `{"name":"go_workspace","arguments":{}}`)
	rr := postLSP(t, s, "go", body, map[string]string{"Mcp-Session-Id": "client-session"})
	if rr.Code != http.StatusOK {
		t.Fatalf("pathless marker-refusal status = %d body=%s", rr.Code, rr.Body.String())
	}
	if calls := autoCalls.Load(); calls != 0 {
		t.Fatalf("AutoRegisterFn calls = %d, want 0 when marker is absent", calls)
	}
	if !strings.Contains(rr.Body.String(), "no language project marker for go under /repo/alpha; refusing .git-only LSP auto-register") {
		t.Fatalf("pathless marker-refusal error mismatch: %s", rr.Body.String())
	}
}

func TestLSPRouter_PathlessRegisteredGitFallbackWorkspaceSkipsMarkerGate(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"workspace":"alpha"}}`))
	}))
	t.Cleanup(upstream.Close)

	sessions := lsp_routing.NewSessionRouter()
	registered := api.WorkspaceEntry{
		WorkspaceKey:  "alpha",
		WorkspacePath: "/repo/alpha",
		Language:      "go",
		Backend:       "gopls-mcp",
		Port:          9201,
	}
	refreshed := registered
	refreshed.Port = 9209
	sessions.TouchWorkspace("client-session", &registered)

	var autoCalls atomic.Int32
	s := NewServer(Config{Port: 9125})
	s.SetLSPRouterDeps(&lspRouterDeps{
		Resolver: &stubLSPResolver{
			markers: map[string]bool{
				"go|/repo/alpha": false,
			},
			registrations: map[string]*api.WorkspaceEntry{
				"go|alpha": &registered,
			},
		},
		Sessions:               sessions,
		BackendKindForLanguage: func(lang string) (string, bool) { return "gopls-mcp", true },
		AutoRegisterFn: func(ctx context.Context, wsKey, workspacePath, language string) (*api.WorkspaceEntry, error) {
			autoCalls.Add(1)
			if wsKey != "alpha" || workspacePath != "/repo/alpha" || language != "go" {
				t.Fatalf("pathless re-ensure args = (%q, %q, %q)", wsKey, workspacePath, language)
			}
			return &refreshed, nil
		},
		UpstreamURLFn: func(ws *api.WorkspaceEntry) string {
			if ws.Port != refreshed.Port {
				t.Fatalf("pathless forwarded stale port %d, want refreshed port %d", ws.Port, refreshed.Port)
			}
			return upstream.URL
		},
	})

	body := rpcBody("tools/call", "1", `{"name":"go_workspace","arguments":{}}`)
	rr := postLSP(t, s, "go", body, map[string]string{"Mcp-Session-Id": "client-session"})
	if rr.Code != http.StatusOK {
		t.Fatalf("registered .git-fallback pathless call status = %d body=%s", rr.Code, rr.Body.String())
	}
	if calls := autoCalls.Load(); calls != 1 {
		t.Fatalf("AutoRegisterFn calls = %d, want 1 registered re-ensure", calls)
	}
}

func TestLSPRouter_PathlessUnregisteredGitFallbackWorkspaceStillRequiresMarker(t *testing.T) {
	sessions := lsp_routing.NewSessionRouter()
	cached := api.WorkspaceEntry{WorkspaceKey: "alpha", WorkspacePath: "/repo/alpha", Language: "go", Backend: "gopls-mcp", Port: 9201}
	sessions.TouchWorkspace("client-session", &cached)

	var autoCalls atomic.Int32
	s := NewServer(Config{Port: 9125})
	s.SetLSPRouterDeps(&lspRouterDeps{
		Resolver: &stubLSPResolver{markers: map[string]bool{
			"go|/repo/alpha": false,
		}},
		Sessions:               sessions,
		BackendKindForLanguage: func(lang string) (string, bool) { return "gopls-mcp", true },
		AutoRegisterFn: func(ctx context.Context, wsKey, workspacePath, language string) (*api.WorkspaceEntry, error) {
			autoCalls.Add(1)
			return nil, errors.New("unexpected auto-register")
		},
	})

	body := rpcBody("tools/call", "1", `{"name":"go_workspace","arguments":{}}`)
	rr := postLSP(t, s, "go", body, map[string]string{"Mcp-Session-Id": "client-session"})
	if rr.Code != http.StatusOK {
		t.Fatalf("pathless marker-refusal status = %d body=%s", rr.Code, rr.Body.String())
	}
	if calls := autoCalls.Load(); calls != 0 {
		t.Fatalf("AutoRegisterFn calls = %d, want 0 for unregistered .git fallback", calls)
	}
	if !strings.Contains(rr.Body.String(), "no language project marker for go under /repo/alpha; refusing .git-only LSP auto-register") {
		t.Fatalf("pathless marker-refusal error mismatch: %s", rr.Body.String())
	}
}

func TestLSPRouter_PathlessSingleCandidateReEnsuresWhenRequestedLanguageMarkerPresent(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"workspace":"alpha"}}`))
	}))
	t.Cleanup(upstream.Close)

	sessions := lsp_routing.NewSessionRouter()
	stale := api.WorkspaceEntry{WorkspaceKey: "alpha", WorkspacePath: "/repo/alpha", Language: "python", Backend: "mcp-language-server", Port: 9201}
	refreshed := stale
	refreshed.Language = "go"
	refreshed.Backend = "gopls-mcp"
	refreshed.Port = 9209
	sessions.TouchWorkspace("client-session", &stale)

	var autoCalls atomic.Int32
	s := NewServer(Config{Port: 9125})
	s.SetLSPRouterDeps(&lspRouterDeps{
		Resolver: &stubLSPResolver{markers: map[string]bool{
			"go|/repo/alpha": true,
		}},
		Sessions:               sessions,
		BackendKindForLanguage: func(lang string) (string, bool) { return "gopls-mcp", true },
		AutoRegisterFn: func(ctx context.Context, wsKey, workspacePath, language string) (*api.WorkspaceEntry, error) {
			autoCalls.Add(1)
			if wsKey != "alpha" || workspacePath != "/repo/alpha" || language != "go" {
				t.Fatalf("pathless re-ensure args = (%q, %q, %q)", wsKey, workspacePath, language)
			}
			return &refreshed, nil
		},
		UpstreamURLFn: func(ws *api.WorkspaceEntry) string {
			if ws.Port != refreshed.Port {
				t.Fatalf("pathless forwarded stale port %d, want refreshed port %d", ws.Port, refreshed.Port)
			}
			return upstream.URL
		},
	})

	body := rpcBody("tools/call", "1", `{"name":"go_workspace","arguments":{}}`)
	rr := postLSP(t, s, "go", body, map[string]string{"Mcp-Session-Id": "client-session"})
	if rr.Code != http.StatusOK {
		t.Fatalf("pathless call status = %d body=%s", rr.Code, rr.Body.String())
	}
	if calls := autoCalls.Load(); calls != 1 {
		t.Fatalf("AutoRegisterFn calls = %d, want 1 for marker-backed pathless candidate", calls)
	}
}

func TestLSPRouter_RelativeFilePathUsesSingleBoundSessionWorkspace(t *testing.T) {
	var upstreamHit atomic.Bool
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamHit.Store(true)
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"workspace":"alpha"}}`))
	}))
	t.Cleanup(upstream.Close)

	workspaceRoot := t.TempDir()
	relPath := "src/main.py"
	joinedPath := filepath.Clean(filepath.Join(workspaceRoot, relPath))
	entry := &api.WorkspaceEntry{WorkspaceKey: "alpha", WorkspacePath: workspaceRoot, Language: "python", Backend: "mcp-language-server", Port: 9201}

	sessions := lsp_routing.NewSessionRouter()
	sessions.TouchWorkspace("client-session", entry)

	s := NewServer(Config{Port: 9125})
	s.SetLSPRouterDeps(&lspRouterDeps{
		Resolver: &stubLSPResolver{
			errs: map[string]error{
				"python|" + relPath: lsp_routing.ErrInvalidPath,
			},
			results: map[string]*lsp_routing.ResolveResult{
				"python|" + joinedPath: {WorkspaceRoot: workspaceRoot, WorkspaceKey: "alpha", Registered: true, Entry: entry, ProjectMarker: true},
			},
		},
		Sessions:               sessions,
		BackendKindForLanguage: func(lang string) (string, bool) { return "mcp-language-server", true },
		UpstreamURLFn:          func(ws *api.WorkspaceEntry) string { return upstream.URL },
	})

	body := rpcBody("tools/call", "1", lspToolCallParamsJSON(t, "diagnostics", map[string]any{"filePath": relPath}))
	rr := postLSP(t, s, "python", body, map[string]string{"Mcp-Session-Id": "client-session"})
	if rr.Code != http.StatusOK {
		t.Fatalf("relative filePath status = %d body=%s", rr.Code, rr.Body.String())
	}
	if !upstreamHit.Load() {
		t.Fatal("relative filePath did not route to the bound session workspace")
	}
}

func TestLSPRouter_RelativeFilePathWithoutSessionStillErrors(t *testing.T) {
	relPath := "src/main.py"
	s := NewServer(Config{Port: 9125})
	s.SetLSPRouterDeps(&lspRouterDeps{
		Resolver: &stubLSPResolver{errs: map[string]error{
			"python|" + relPath: lsp_routing.ErrInvalidPath,
		}},
		Sessions:               lsp_routing.NewSessionRouter(),
		BackendKindForLanguage: func(lang string) (string, bool) { return "mcp-language-server", true },
	})

	body := rpcBody("tools/call", "1", lspToolCallParamsJSON(t, "diagnostics", map[string]any{"filePath": relPath}))
	rr := postLSP(t, s, "python", body, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("relative filePath error status = %d body=%s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "no LSP workspace for path "+relPath) {
		t.Fatalf("relative filePath error mismatch: %s", rr.Body.String())
	}
}

func TestLSPRouter_RelativeFilePathWithMultipleCandidatesIsAmbiguous(t *testing.T) {
	relPath := "src/main.py"
	sessions := lsp_routing.NewSessionRouter()
	sessions.TouchWorkspace("client-session", &api.WorkspaceEntry{WorkspaceKey: "alpha", WorkspacePath: "/repo/alpha", Language: "python", Backend: "mcp-language-server", Port: 9201})
	sessions.TouchWorkspace("client-session", &api.WorkspaceEntry{WorkspaceKey: "beta", WorkspacePath: "/repo/beta", Language: "python", Backend: "mcp-language-server", Port: 9202})

	s := NewServer(Config{Port: 9125})
	s.SetLSPRouterDeps(&lspRouterDeps{
		Resolver: &stubLSPResolver{errs: map[string]error{
			"python|" + relPath: lsp_routing.ErrInvalidPath,
		}},
		Sessions:               sessions,
		BackendKindForLanguage: func(lang string) (string, bool) { return "mcp-language-server", true },
	})

	body := rpcBody("tools/call", "1", lspToolCallParamsJSON(t, "diagnostics", map[string]any{"filePath": relPath}))
	rr := postLSP(t, s, "python", body, map[string]string{"Mcp-Session-Id": "client-session"})
	if rr.Code != http.StatusOK {
		t.Fatalf("relative filePath ambiguous status = %d body=%s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "ambiguous LSP workspace") ||
		!strings.Contains(rr.Body.String(), "/repo/alpha") ||
		!strings.Contains(rr.Body.String(), "/repo/beta") {
		t.Fatalf("relative filePath ambiguous error mismatch: %s", rr.Body.String())
	}
}

func TestLSPRouter_FilesBatchAcrossWorkspacesRejected(t *testing.T) {
	var upstreamHit atomic.Bool
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamHit.Store(true)
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"ok":true}}`))
	}))
	t.Cleanup(upstream.Close)

	alphaRoot := t.TempDir()
	betaRoot := t.TempDir()
	alphaFile := filepath.Join(alphaRoot, "main.go")
	betaFile := filepath.Join(betaRoot, "main.go")
	alphaEntry := &api.WorkspaceEntry{WorkspaceKey: "alpha", WorkspacePath: alphaRoot, Language: "go", Backend: "gopls-mcp", Port: 9201}
	betaEntry := &api.WorkspaceEntry{WorkspaceKey: "beta", WorkspacePath: betaRoot, Language: "go", Backend: "gopls-mcp", Port: 9202}

	s := NewServer(Config{Port: 9125})
	s.SetLSPRouterDeps(&lspRouterDeps{
		Resolver: &stubLSPResolver{results: map[string]*lsp_routing.ResolveResult{
			"go|" + alphaFile: {WorkspaceRoot: alphaRoot, WorkspaceKey: "alpha", Registered: true, Entry: alphaEntry, ProjectMarker: true},
			"go|" + betaFile:  {WorkspaceRoot: betaRoot, WorkspaceKey: "beta", Registered: true, Entry: betaEntry, ProjectMarker: true},
		}},
		Sessions:               lsp_routing.NewSessionRouter(),
		BackendKindForLanguage: func(lang string) (string, bool) { return "gopls-mcp", true },
		UpstreamURLFn:          func(ws *api.WorkspaceEntry) string { return upstream.URL },
	})

	body := rpcBody("tools/call", "1", lspToolCallParamsJSON(t, "go_diagnostics", map[string]any{"files": []string{alphaFile, betaFile}}))
	rr := postLSP(t, s, "go", body, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("cross-workspace files batch status = %d body=%s", rr.Code, rr.Body.String())
	}
	if upstreamHit.Load() {
		t.Fatal("cross-workspace files batch was forwarded upstream")
	}
	if !strings.Contains(rr.Body.String(), "files span multiple LSP workspaces") ||
		!strings.Contains(rr.Body.String(), "alpha") ||
		!strings.Contains(rr.Body.String(), "beta") ||
		!strings.Contains(rr.Body.String(), "split the call per workspace") {
		t.Fatalf("cross-workspace files batch error mismatch: %s", rr.Body.String())
	}
}

func TestLSPRouter_FilesBatchSameWorkspaceRoutes(t *testing.T) {
	var upstreamHit atomic.Bool
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamHit.Store(true)
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"ok":true}}`))
	}))
	t.Cleanup(upstream.Close)

	workspaceRoot := t.TempDir()
	fileA := filepath.Join(workspaceRoot, "a.go")
	fileB := filepath.Join(workspaceRoot, "b.go")
	entry := &api.WorkspaceEntry{WorkspaceKey: "alpha", WorkspacePath: workspaceRoot, Language: "go", Backend: "gopls-mcp", Port: 9201}

	s := NewServer(Config{Port: 9125})
	s.SetLSPRouterDeps(&lspRouterDeps{
		Resolver: &stubLSPResolver{results: map[string]*lsp_routing.ResolveResult{
			"go|" + fileA: {WorkspaceRoot: workspaceRoot, WorkspaceKey: "alpha", Registered: true, Entry: entry, ProjectMarker: true},
			"go|" + fileB: {WorkspaceRoot: workspaceRoot, WorkspaceKey: "alpha", Registered: true, Entry: entry, ProjectMarker: true},
		}},
		Sessions:               lsp_routing.NewSessionRouter(),
		BackendKindForLanguage: func(lang string) (string, bool) { return "gopls-mcp", true },
		UpstreamURLFn:          func(ws *api.WorkspaceEntry) string { return upstream.URL },
	})

	body := rpcBody("tools/call", "1", lspToolCallParamsJSON(t, "go_diagnostics", map[string]any{"files": []string{fileA, fileB}}))
	rr := postLSP(t, s, "go", body, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("same-workspace files batch status = %d body=%s", rr.Code, rr.Body.String())
	}
	if !upstreamHit.Load() {
		t.Fatal("same-workspace files batch was not forwarded upstream")
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
