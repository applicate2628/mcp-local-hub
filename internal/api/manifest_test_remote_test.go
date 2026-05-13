package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestManifestTestRemote_HappyPath(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("expected POST, got %s", r.Method)
		}
		if ct := r.Header.Get("Content-Type"); ct != "application/json" {
			t.Errorf("Content-Type=%q, want application/json", ct)
		}
		if mpv := r.Header.Get("MCP-Protocol-Version"); mpv != testRemoteProtocolVersion {
			t.Errorf("MCP-Protocol-Version=%q, want %q", mpv, testRemoteProtocolVersion)
		}
		var req map[string]any
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		if req["method"] != "initialize" {
			t.Errorf("method=%v, want initialize", req["method"])
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"jsonrpc": "2.0",
			"id":      1,
			"result": map[string]any{
				"protocolVersion": testRemoteProtocolVersion,
				"capabilities":    map[string]any{"tools": map[string]any{}},
				"serverInfo": map[string]any{
					"name":    "test-server",
					"version": "0.1.0",
				},
			},
		})
	}))
	defer srv.Close()
	dir := t.TempDir()
	writeManifest(t, dir, "remote", "name: remote\nkind: global\ntransport: remote-http\nurl: "+srv.URL+"\nclient_bindings:\n  - client: claude-code\n")
	a := NewAPI()
	res, err := a.manifestTestRemoteWithClient(context.Background(), dir, "remote", buildTLSTrustingClient(srv))
	if err != nil {
		t.Fatalf("test-remote: %v", err)
	}
	if res.ProtocolVersion != testRemoteProtocolVersion {
		t.Errorf("ProtocolVersion=%q", res.ProtocolVersion)
	}
	if res.ServerName != "test-server" || res.ServerVersion != "0.1.0" {
		t.Errorf("serverInfo wrong: %+v", res)
	}
	if _, ok := res.Capabilities["tools"]; !ok {
		t.Errorf("capabilities missing tools: %v", res.Capabilities)
	}
}

func TestManifestTestRemote_SendsExpandedHeaders(t *testing.T) {
	var gotAuth string
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":"2025-11-25","capabilities":{},"serverInfo":{"name":"x","version":"y"}}}`))
	}))
	defer srv.Close()
	dir := t.TempDir()
	writeManifest(t, dir, "remote", "name: remote\nkind: global\ntransport: remote-http\nurl: "+srv.URL+"\nheaders:\n  Authorization: \"Bearer literal-token\"\nclient_bindings:\n  - client: claude-code\n")
	a := NewAPI()
	if _, err := a.manifestTestRemoteWithClient(context.Background(), dir, "remote", buildTLSTrustingClient(srv)); err != nil {
		t.Fatalf("test-remote: %v", err)
	}
	if gotAuth != "Bearer literal-token" {
		t.Errorf("Authorization=%q, want %q", gotAuth, "Bearer literal-token")
	}
}

func TestManifestTestRemote_TransportGate(t *testing.T) {
	dir := t.TempDir()
	writeManifest(t, dir, "local", "name: local\nkind: global\ntransport: stdio-bridge\ncommand: echo\nbase_args: [\"hi\"]\ndaemons:\n  - name: default\n    port: 9999\nclient_bindings:\n  - client: claude-code\n    daemon: default\n")
	a := NewAPI()
	_, err := a.manifestTestRemoteWithClient(context.Background(), dir, "local", &http.Client{})
	if err == nil {
		t.Fatal("expected transport-gate rejection")
	}
	if !strings.Contains(err.Error(), "transport=") || !strings.Contains(err.Error(), "remote-http") {
		t.Errorf("error should name expected transport: %v", err)
	}
}

func TestManifestTestRemote_HTTPSOnly(t *testing.T) {
	// Plain HTTP httptest server — ParseManifest itself would refuse
	// http:// urls (validation gate), so we have to bypass parser via
	// the sendRemoteInitialize entrypoint to prove the wire-level
	// belt-and-suspenders check is also live.
	plain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("nope"))
	}))
	defer plain.Close()
	if !strings.HasPrefix(plain.URL, "http://") {
		t.Skipf("httptest.NewServer is not plain http; got %q", plain.URL)
	}
	_, err := sendRemoteInitialize(context.Background(), &http.Client{}, plain.URL, nil)
	if err == nil || !strings.Contains(err.Error(), "https") {
		t.Errorf("expected https-only rejection, got %v", err)
	}
}

func TestManifestTestRemote_UpstreamRPCError(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"error":{"code":-32600,"message":"unsupported protocolVersion"}}`))
	}))
	defer srv.Close()
	dir := t.TempDir()
	writeManifest(t, dir, "remote", "name: remote\nkind: global\ntransport: remote-http\nurl: "+srv.URL+"\nclient_bindings:\n  - client: claude-code\n")
	a := NewAPI()
	_, err := a.manifestTestRemoteWithClient(context.Background(), dir, "remote", buildTLSTrustingClient(srv))
	if err == nil || !strings.Contains(err.Error(), "-32600") {
		t.Errorf("expected upstream rpc error with code -32600, got %v", err)
	}
}

func TestManifestTestRemote_UpstreamHTTPStatus(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte("bad token"))
	}))
	defer srv.Close()
	dir := t.TempDir()
	writeManifest(t, dir, "remote", "name: remote\nkind: global\ntransport: remote-http\nurl: "+srv.URL+"\nclient_bindings:\n  - client: claude-code\n")
	a := NewAPI()
	_, err := a.manifestTestRemoteWithClient(context.Background(), dir, "remote", buildTLSTrustingClient(srv))
	if err == nil || !strings.Contains(err.Error(), "401") {
		t.Errorf("expected HTTP 401 surfacing, got %v", err)
	}
}

func TestManifestTestRemote_SSEResponse(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		// One-event SSE response — Streamable HTTP allows initialize
		// to come back as a single SSE data line.
		_, _ = w.Write([]byte(": ping\ndata: {\"jsonrpc\":\"2.0\",\"id\":1,\"result\":{\"protocolVersion\":\"2025-11-25\",\"serverInfo\":{\"name\":\"sse\",\"version\":\"1\"}}}\n\n"))
	}))
	defer srv.Close()
	dir := t.TempDir()
	writeManifest(t, dir, "remote", "name: remote\nkind: global\ntransport: remote-http\nurl: "+srv.URL+"\nclient_bindings:\n  - client: claude-code\n")
	a := NewAPI()
	res, err := a.manifestTestRemoteWithClient(context.Background(), dir, "remote", buildTLSTrustingClient(srv))
	if err != nil {
		t.Fatalf("test-remote: %v", err)
	}
	if res.ServerName != "sse" {
		t.Errorf("ServerName=%q, want sse", res.ServerName)
	}
}

func TestManifestTestRemote_MissingSecret(t *testing.T) {
	dir := t.TempDir()
	writeManifest(t, dir, "remote", "name: remote\nkind: global\ntransport: remote-http\nurl: https://example.com/mcp\nheaders:\n  Authorization: \"Bearer ${secret:NONEXISTENT_KEY_FOR_TEST}\"\nclient_bindings:\n  - client: claude-code\n")
	a := NewAPI()
	_, err := a.manifestTestRemoteWithClient(context.Background(), dir, "remote", &http.Client{})
	if err == nil || !strings.Contains(err.Error(), "expand headers") {
		t.Errorf("expected expand-headers failure, got %v", err)
	}
}

func TestExtractSingleSSEMessage_MultilineData(t *testing.T) {
	in := []byte(": comment\ndata: {\"jsonrpc\":\"2.0\",\ndata: \"id\":1}\n\n")
	got, err := extractSingleSSEMessage(in)
	if err != nil {
		t.Fatalf("extractSingleSSEMessage: %v", err)
	}
	if !strings.Contains(string(got), `"jsonrpc":"2.0"`) || !strings.Contains(string(got), `"id":1`) {
		t.Errorf("got %q, want both jsonrpc and id pieces", got)
	}
}

func TestExtractSingleSSEMessage_NoData(t *testing.T) {
	_, err := extractSingleSSEMessage([]byte(": comment only\n\n"))
	if err == nil {
		t.Fatal("expected error for SSE response with no data: lines")
	}
}
