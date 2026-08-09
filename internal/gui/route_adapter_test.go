// internal/gui/route_adapter_test.go
package gui

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestRouteHandler_MountsOnlySerenaAndLSP proves the front-daemon adapter
// (RouteHandler) exposes /serena/mcp and /lsp/<language>/mcp and NOTHING
// else of the GUI's API surface — the isolation claim 'mcphub route' Phase
// 1b relies on (it must not accidentally serve /api/*, dashboard, settings,
// secrets, migration, etc.).
func TestRouteHandler_MountsOnlySerenaAndLSP(t *testing.T) {
	s := NewServer(Config{Port: 9200, Version: "test", PID: 1})
	h := s.RouteHandler()

	// /serena/mcp is mounted: routing layer not wired -> 503 (not 404), same
	// canonical body as the full GUI mux's /serena/mcp route.
	req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:9200/serena/mcp", bytes.NewReader([]byte(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("/serena/mcp status = %d, want 503 (routing unwired placeholder)", rec.Code)
	}

	// /lsp/<lang>/mcp is mounted: routing layer not wired -> a JSON-RPC
	// "lsp router is not configured" internal error, not a 404.
	req2 := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:9200/lsp/go/mcp", bytes.NewReader([]byte(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)))
	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, req2)
	if rec2.Code == http.StatusNotFound {
		t.Errorf("/lsp/go/mcp status = 404, want mounted (any non-404 status)")
	}

	// The rest of the GUI API surface must NOT be reachable through this
	// narrower mux — /api/ping is registered on the FULL s.mux but RouteHandler
	// builds its own fresh mux with only the two MCP routes.
	req3 := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:9200/api/ping", nil)
	rec3 := httptest.NewRecorder()
	h.ServeHTTP(rec3, req3)
	if rec3.Code != http.StatusNotFound {
		t.Errorf("/api/ping status = %d, want 404 (RouteHandler must not expose the GUI API surface)", rec3.Code)
	}

	req4 := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:9200/dashboard", nil)
	rec4 := httptest.NewRecorder()
	h.ServeHTTP(rec4, req4)
	if rec4.Code != http.StatusNotFound {
		t.Errorf("/dashboard status = %d, want 404 (RouteHandler must not expose the GUI API surface)", rec4.Code)
	}
}

// TestRouteHandler_DNSRebindGuardStillApplies proves RouteHandler keeps the
// Host-header DNS-rebind guard (claim 4's negative case) on the front
// daemon's OWN port, independent of any other Server instance's port.
func TestRouteHandler_DNSRebindGuardStillApplies(t *testing.T) {
	s := NewServer(Config{Port: 9200, Version: "test", PID: 1})
	h := s.RouteHandler()

	req := httptest.NewRequest(http.MethodPost, "http://evil.example.com:9200/serena/mcp", bytes.NewReader([]byte(`{}`)))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403 (DNS-rebind Host must be rejected)", rec.Code)
	}
}

// TestRouteHandler_AdmitsNoOriginPOST proves claim 4: an MCP client POST with
// no Origin / Sec-Fetch-Site header (Claude Code's shape) is admitted on the
// front daemon's own port.
func TestRouteHandler_AdmitsNoOriginPOST(t *testing.T) {
	s := NewServer(Config{Port: 9200, Version: "test", PID: 1})
	h := s.RouteHandler()

	req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:9200/serena/mcp", bytes.NewReader([]byte(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	// No Origin, no Sec-Fetch-Site -> passes requireSameOrigin; falls through
	// to the (unwired) serena router deps check -> 503, NOT 403.
	if rec.Code == http.StatusForbidden {
		t.Errorf("status = 403, want admitted (no-Origin POST must pass the same-origin guard)")
	}
}
