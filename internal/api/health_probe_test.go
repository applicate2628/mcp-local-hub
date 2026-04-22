package api

import (
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestSingleHealthProbe_OK verifies the probe reports OK + correct
// tool count when the MCP server responds to initialize + tools/list.
func TestSingleHealthProbe_OK(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := ""
		buf := make([]byte, 4096)
		n, _ := r.Body.Read(buf)
		body = string(buf[:n])
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Mcp-Session-Id", "test-session")
		switch {
		case strings.Contains(body, `"initialize"`):
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":"2025-03-26","capabilities":{}}}`))
		case strings.Contains(body, `"tools/list"`):
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":2,"result":{"tools":[{"name":"a"},{"name":"b"},{"name":"c"}]}}`))
		default:
			http.Error(w, "unexpected", http.StatusInternalServerError)
		}
	}))
	defer srv.Close()
	port := parsePort(t, srv.URL)

	h := singleHealthProbe(port)
	if h == nil {
		t.Fatal("singleHealthProbe returned nil")
	}
	if !h.OK {
		t.Errorf("expected OK=true, got %+v", h)
	}
	if h.ToolCount != 3 {
		t.Errorf("ToolCount = %d, want 3", h.ToolCount)
	}
}

// TestSingleHealthProbe_ErrorFromServer verifies the probe reports
// the MCP server's error verbatim when tools/list returns a JSON-RPC
// error. This is the scenario the audit flagged: daemon alive, MCP
// server up, but backend (e.g. gdb binary) missing — the server
// responds to tools/list with an error and we want that visible.
func TestSingleHealthProbe_ErrorFromServer(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		buf := make([]byte, 4096)
		n, _ := r.Body.Read(buf)
		body := string(buf[:n])
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Mcp-Session-Id", "test-session")
		if strings.Contains(body, `"initialize"`) {
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"capabilities":{}}}`))
			return
		}
		// tools/list returns an error.
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":2,"error":{"code":-32603,"message":"backend unavailable: gdb not on PATH"}}`))
	}))
	defer srv.Close()
	port := parsePort(t, srv.URL)

	h := singleHealthProbe(port)
	if h == nil {
		t.Fatal("nil probe")
	}
	if h.OK {
		t.Errorf("expected OK=false, got %+v", h)
	}
	if !strings.Contains(h.Err, "gdb not on PATH") {
		t.Errorf("expected err to include upstream message: %q", h.Err)
	}
}

// TestSingleHealthProbe_Unreachable verifies the probe reports an
// error (not nil, not OK) for a port nothing is listening on.
func TestSingleHealthProbe_Unreachable(t *testing.T) {
	// A port that almost certainly isn't bound.
	h := singleHealthProbe(65535)
	if h == nil {
		t.Fatal("nil probe")
	}
	if h.OK {
		t.Errorf("expected OK=false for unreachable port: %+v", h)
	}
	if h.Err == "" {
		t.Error("expected non-empty Err")
	}
}

// TestSingleHealthProbe_OversizedResponse verifies tools/list responses
// larger than the safety cap are rejected without full buffering.
func TestSingleHealthProbe_OversizedResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		buf := make([]byte, 4096)
		n, _ := r.Body.Read(buf)
		body := string(buf[:n])
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Mcp-Session-Id", "test-session")
		if strings.Contains(body, `"initialize"`) {
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"capabilities":{}}}`))
			return
		}
		tooLarge := strings.Repeat("x", maxHealthProbeResponseBytes+1)
		_, _ = w.Write([]byte(tooLarge))
	}))
	defer srv.Close()
	port := parsePort(t, srv.URL)

	h := singleHealthProbe(port)
	if h == nil {
		t.Fatal("nil probe")
	}
	if h.OK {
		t.Fatalf("expected OK=false for oversized response, got %+v", h)
	}
	if !strings.Contains(h.Err, "response too large") {
		t.Fatalf("expected oversized-response error, got %q", h.Err)
	}
}

// parsePort is a small helper: extract the port part from an
// httptest.Server URL ("http://127.0.0.1:PORT").
func parsePort(t *testing.T, url string) int {
	t.Helper()
	_, portStr, err := net.SplitHostPort(strings.TrimPrefix(url, "http://"))
	if err != nil {
		t.Fatalf("split host:port from %q: %v", url, err)
	}
	var p int
	if _, err := fmt.Sscanf(portStr, "%d", &p); err != nil {
		t.Fatalf("parse port %q: %v", portStr, err)
	}
	return p
}
