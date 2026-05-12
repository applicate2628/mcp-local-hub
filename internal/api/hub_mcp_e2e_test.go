// hub_mcp_e2e_test.go — Phase 4 Task 4.5 (G4 unified hub MCP).
//
// End-to-end integration test that drives the hub listener with
// real on-disk state, then exercises the 7-check auth gate +
// JSON-RPC handshake + DELETE termination + /internal/reload-tokens
// against a live httptest server.
//
// Uses the api-package-only hubMcpStateTestHelper + hardenedTempDir
// so the SecureWriteClientConfig parent-dir DACL gate passes on
// Windows. The httptest.Server is bound on a per-test ephemeral
// port; no real socket touches the developer's port range.
//
// This is the "happy-path everything works together" test. Each
// individual gate is covered by hub_mcp_handler_test.go +
// hub_mcp_internal_reload_test.go; this file drives the live mux
// end-to-end so a regression in the wiring of those pieces surfaces
// here.
//
// Spec: docs/superpowers/specs/2026-05-12-g4-unified-hub-mcp-design-v3.md
// §"Bind ordering" + §"Cross-client invariant" + §"Control endpoint
// contract".
// Plan: docs/superpowers/plans/2026-05-12-g4-unified-hub-mcp.md Task 4.5.

package api

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestHubMcpE2EAuthGateAcceptsValidRequest exercises the full
// success path: live tokens + endpoint state + handler mux → the
// auth gate accepts a properly-formed initialize.
func TestHubMcpE2EAuthGateAcceptsValidRequest(t *testing.T) {
	hubMcpStateTestHelper(t)

	// Seed tokens + endpoint state on disk.
	tbl, err := EnsureHubTokens([]string{"claude-code"})
	if err != nil {
		t.Fatalf("EnsureHubTokens: %v", err)
	}
	clientTok, ok := tbl.Tokens["claude-code"]
	if !ok {
		t.Fatal("EnsureHubTokens did not produce claude-code entry")
	}
	ep, err := EnsureHubEndpoint(0, 1234) // port=0 placeholder; real port comes from httptest
	if err != nil {
		t.Fatalf("EnsureHubEndpoint: %v", err)
	}
	if ep.InstanceID == "" {
		t.Fatal("EnsureHubEndpoint produced empty InstanceID")
	}

	// Build the mux exactly the way internal/gui/hub_listener.go does.
	store := NewHubSessionStore(SessionStoreOpts{})
	t.Cleanup(store.Close)
	handler := NewHubMcpHandler(store)
	handler.SetEndpoint(ep)

	mux := http.NewServeMux()
	mux.Handle("/clients/", handler)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	// Initialize the session. The handler's auth gate runs first;
	// we expect 200 OK + Mcp-Session-Id header back.
	initBody := []byte(`{"jsonrpc":"2.0","id":"init-1","method":"initialize","params":{"protocolVersion":"2025-11-25"}}`)
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/clients/claude-code/mcp", bytes.NewReader(initBody))
	req.Header.Set("X-Mcphub-Hub-Token", clientTok)
	req.Header.Set("X-Mcphub-Instance-Id", ep.InstanceID)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("initialize: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("initialize: got %d, want 200; body=%s", resp.StatusCode, string(raw))
	}
	sid := resp.Header.Get("Mcp-Session-Id")
	if sid == "" {
		t.Fatal("initialize: no Mcp-Session-Id header in response")
	}
	body, _ := io.ReadAll(resp.Body)
	var env struct {
		ID     json.RawMessage `json:"id"`
		Result struct {
			ProtocolVersion string `json:"protocolVersion"`
		} `json:"result"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("initialize body parse: %v / body=%s", err, body)
	}
	if string(env.ID) != `"init-1"` {
		t.Errorf("initialize id: got %s, want \"init-1\"", env.ID)
	}
	if env.Result.ProtocolVersion != "2025-11-25" {
		t.Errorf("initialize protocolVersion: got %q, want %q", env.Result.ProtocolVersion, "2025-11-25")
	}

	// Ping the hub-local echo with the freshly-allocated session.
	pingBody := []byte(`{"jsonrpc":"2.0","id":42,"method":"ping"}`)
	req2, _ := http.NewRequest(http.MethodPost, srv.URL+"/clients/claude-code/mcp", bytes.NewReader(pingBody))
	req2.Header.Set("X-Mcphub-Hub-Token", clientTok)
	req2.Header.Set("X-Mcphub-Instance-Id", ep.InstanceID)
	req2.Header.Set("Mcp-Session-Id", sid)
	req2.Header.Set("MCP-Protocol-Version", "2025-11-25")
	resp2, err := http.DefaultClient.Do(req2)
	if err != nil {
		t.Fatalf("ping: %v", err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp2.Body)
		t.Fatalf("ping: got %d, want 200; body=%s", resp2.StatusCode, string(raw))
	}

	// DELETE the session.
	delReq, _ := http.NewRequest(http.MethodDelete, srv.URL+"/clients/claude-code/mcp", nil)
	delReq.Header.Set("X-Mcphub-Hub-Token", clientTok)
	delReq.Header.Set("X-Mcphub-Instance-Id", ep.InstanceID)
	delReq.Header.Set("Mcp-Session-Id", sid)
	delResp, err := http.DefaultClient.Do(delReq)
	if err != nil {
		t.Fatalf("DELETE: %v", err)
	}
	delResp.Body.Close()
	if delResp.StatusCode != http.StatusNoContent {
		t.Errorf("DELETE: got %d, want 204", delResp.StatusCode)
	}

	// Subsequent request on the deleted session → 404.
	postReq, _ := http.NewRequest(http.MethodPost, srv.URL+"/clients/claude-code/mcp", bytes.NewReader(pingBody))
	postReq.Header.Set("X-Mcphub-Hub-Token", clientTok)
	postReq.Header.Set("X-Mcphub-Instance-Id", ep.InstanceID)
	postReq.Header.Set("Mcp-Session-Id", sid)
	postReq.Header.Set("MCP-Protocol-Version", "2025-11-25")
	postResp, err := http.DefaultClient.Do(postReq)
	if err != nil {
		t.Fatalf("post-DELETE POST: %v", err)
	}
	postResp.Body.Close()
	if postResp.StatusCode != http.StatusNotFound {
		t.Errorf("post-DELETE POST: got %d, want 404", postResp.StatusCode)
	}
}

// TestHubMcpE2EAuthGateRejectsCrossClientReuse exercises the spec
// §"Cross-client invariant" tuple-fence: a session created on the
// claude-code path cannot be reused on the codex-cli path.
func TestHubMcpE2EAuthGateRejectsCrossClientReuse(t *testing.T) {
	hubMcpStateTestHelper(t)

	tbl, err := EnsureHubTokens([]string{"claude-code", "codex-cli"})
	if err != nil {
		t.Fatalf("EnsureHubTokens: %v", err)
	}
	claudeTok := tbl.Tokens["claude-code"]
	codexTok := tbl.Tokens["codex-cli"]
	ep, err := EnsureHubEndpoint(0, 1234)
	if err != nil {
		t.Fatalf("EnsureHubEndpoint: %v", err)
	}

	store := NewHubSessionStore(SessionStoreOpts{})
	t.Cleanup(store.Close)
	handler := NewHubMcpHandler(store)
	handler.SetEndpoint(ep)

	mux := http.NewServeMux()
	mux.Handle("/clients/", handler)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	// Initialize via claude-code.
	initBody := []byte(`{"jsonrpc":"2.0","id":"init-1","method":"initialize","params":{"protocolVersion":"2025-11-25"}}`)
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/clients/claude-code/mcp", bytes.NewReader(initBody))
	req.Header.Set("X-Mcphub-Hub-Token", claudeTok)
	req.Header.Set("X-Mcphub-Instance-Id", ep.InstanceID)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("initialize: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("initialize: got %d, want 200", resp.StatusCode)
	}
	sid := resp.Header.Get("Mcp-Session-Id")
	if sid == "" {
		t.Fatal("initialize: no Mcp-Session-Id header")
	}

	// Replay the same Mcp-Session-Id but on /clients/codex-cli/mcp
	// with the codex-cli token: rejected by gate 6 (session-client
	// binding mismatch) → 401 empty body.
	pingBody := []byte(`{"jsonrpc":"2.0","id":1,"method":"ping"}`)
	req2, _ := http.NewRequest(http.MethodPost, srv.URL+"/clients/codex-cli/mcp", bytes.NewReader(pingBody))
	req2.Header.Set("X-Mcphub-Hub-Token", codexTok)
	req2.Header.Set("X-Mcphub-Instance-Id", ep.InstanceID)
	req2.Header.Set("Mcp-Session-Id", sid)
	req2.Header.Set("MCP-Protocol-Version", "2025-11-25")
	resp2, err := http.DefaultClient.Do(req2)
	if err != nil {
		t.Fatalf("cross-client POST: %v", err)
	}
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusUnauthorized {
		t.Errorf("cross-client session reuse: got %d, want 401", resp2.StatusCode)
	}
}

// TestHubMcpE2EAuthGateGETReturns405 exercises spec §"Client-origin
// lifecycle methods" — GET /clients/{id}/mcp returns 405 with
// `Allow: POST, DELETE`. Auth gates 1-5 still run; a properly-authed
// GET hits the 405 fallback.
func TestHubMcpE2EAuthGateGETReturns405(t *testing.T) {
	hubMcpStateTestHelper(t)

	tbl, err := EnsureHubTokens([]string{"claude-code"})
	if err != nil {
		t.Fatalf("EnsureHubTokens: %v", err)
	}
	tok := tbl.Tokens["claude-code"]
	ep, err := EnsureHubEndpoint(0, 1234)
	if err != nil {
		t.Fatalf("EnsureHubEndpoint: %v", err)
	}

	store := NewHubSessionStore(SessionStoreOpts{})
	t.Cleanup(store.Close)
	handler := NewHubMcpHandler(store)
	handler.SetEndpoint(ep)

	mux := http.NewServeMux()
	mux.Handle("/clients/", handler)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/clients/claude-code/mcp", nil)
	req.Header.Set("X-Mcphub-Hub-Token", tok)
	req.Header.Set("X-Mcphub-Instance-Id", ep.InstanceID)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("GET: got %d, want 405", resp.StatusCode)
	}
	if allow := resp.Header.Get("Allow"); allow != "POST, DELETE" {
		t.Errorf("GET Allow header: got %q, want %q", allow, "POST, DELETE")
	}
}

// TestHubMcpE2EInternalReloadCycle exercises the live-reload pipeline
// end-to-end: mount both the per-client handler AND the
// /internal/reload-tokens handler on one mux, rotate a token via
// RotateHubToken, POST to /internal/reload-tokens with the control
// header, and verify a subsequent per-client request with the NEW
// token passes the auth gate.
func TestHubMcpE2EInternalReloadCycle(t *testing.T) {
	hubMcpStateTestHelper(t)

	if _, err := EnsureHubTokens([]string{"claude-code"}); err != nil {
		t.Fatalf("EnsureHubTokens: %v", err)
	}
	ep, err := EnsureHubEndpoint(0, 1234)
	if err != nil {
		t.Fatalf("EnsureHubEndpoint: %v", err)
	}

	store := NewHubSessionStore(SessionStoreOpts{})
	t.Cleanup(store.Close)
	handler := NewHubMcpHandler(store)
	handler.SetEndpoint(ep)
	reload := NewInternalReloadHandler()

	mux := http.NewServeMux()
	mux.Handle("/clients/", handler)
	mux.Handle("/internal/reload-tokens", reload)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	// Rotate to a fresh claude-code token on disk.
	tbl, err := RotateHubToken("claude-code")
	if err != nil {
		t.Fatalf("RotateHubToken: %v", err)
	}
	newTok := tbl.Tokens["claude-code"]

	// Clobber the live snapshot so subsequent gate-4 compare would
	// reject — proves the reload actually re-reads from disk.
	publishTokenTable(HubTokenTable{Tokens: map[string]string{}})

	// Reload via the control endpoint.
	ctrlTok := func() string {
		p := reload.controlTok.Load()
		if p == nil {
			return ""
		}
		return *p
	}()
	if ctrlTok == "" {
		t.Fatal("control token empty")
	}
	reloadReq, _ := http.NewRequest(http.MethodPost, srv.URL+"/internal/reload-tokens", nil)
	reloadReq.Header.Set("X-Mcphub-Control-Token", ctrlTok)
	reloadResp, err := http.DefaultClient.Do(reloadReq)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	reloadResp.Body.Close()
	if reloadResp.StatusCode != http.StatusNoContent {
		t.Fatalf("reload: got %d, want 204", reloadResp.StatusCode)
	}

	// Per-client gate accepts the NEW token (was just published by
	// the reload).
	initBody := []byte(`{"jsonrpc":"2.0","id":"init-1","method":"initialize","params":{"protocolVersion":"2025-11-25"}}`)
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/clients/claude-code/mcp", bytes.NewReader(initBody))
	req.Header.Set("X-Mcphub-Hub-Token", newTok)
	req.Header.Set("X-Mcphub-Instance-Id", ep.InstanceID)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("post-reload init: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("post-reload init with new token: got %d, want 200", resp.StatusCode)
	}
}

// TestHubMcpE2ENonLoopbackHostReturns403 — the loopback-guard
// middleware runs before any other check, so a request with a
// non-loopback Host returns 403 regardless of valid auth headers.
//
// httptest.NewServer binds 127.0.0.1 by default; we have to mint a
// raw Request and feed it through the handler directly to simulate
// the DNS-rebind scenario (Host header carrying a non-loopback name).
func TestHubMcpE2ENonLoopbackHostReturns403(t *testing.T) {
	hubMcpStateTestHelper(t)

	tbl, err := EnsureHubTokens([]string{"claude-code"})
	if err != nil {
		t.Fatalf("EnsureHubTokens: %v", err)
	}
	tok := tbl.Tokens["claude-code"]
	ep, err := EnsureHubEndpoint(0, 1234)
	if err != nil {
		t.Fatalf("EnsureHubEndpoint: %v", err)
	}

	store := NewHubSessionStore(SessionStoreOpts{})
	t.Cleanup(store.Close)
	handler := NewHubMcpHandler(store)
	handler.SetEndpoint(ep)

	req := httptest.NewRequest(http.MethodPost, "/clients/claude-code/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25"}}`))
	req.Host = "evil.example.com" // simulated DNS-rebind
	req.Header.Set("X-Mcphub-Hub-Token", tok)
	req.Header.Set("X-Mcphub-Instance-Id", ep.InstanceID)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusForbidden {
		t.Errorf("non-loopback Host: got %d, want 403", w.Code)
	}
}
