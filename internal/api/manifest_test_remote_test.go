package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
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

// TestManifestTestRemote_ManifestHeadersCannotOverrideProtocolHeaders
// pins bot r3 P2 closure (PR #171): a manifest attempting to set
// MCP-Protocol-Version, Accept, or Content-Type must NOT win against
// the protocol headers we own. Apply user headers first, then force
// the protocol headers — any conflicting manifest entry is silently
// overwritten by the protocol baseline.
func TestManifestTestRemote_ManifestHeadersCannotOverrideProtocolHeaders(t *testing.T) {
	var gotMPV, gotAccept, gotContentType, gotAuth string
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMPV = r.Header.Get("MCP-Protocol-Version")
		gotAccept = r.Header.Get("Accept")
		gotContentType = r.Header.Get("Content-Type")
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":"2025-11-25","serverInfo":{"name":"x","version":"y"}}}`))
	}))
	defer srv.Close()
	dir := t.TempDir()
	writeManifest(t, dir, "remote", "name: remote\nkind: global\ntransport: remote-http\nurl: "+srv.URL+"\nheaders:\n  Authorization: \"Bearer real-token\"\n  MCP-Protocol-Version: \"1.0-bad\"\n  Accept: \"text/html\"\n  Content-Type: \"text/plain\"\nclient_bindings:\n  - client: claude-code\n")
	a := NewAPI()
	if _, err := a.manifestTestRemoteWithClient(context.Background(), dir, "remote", buildTLSTrustingClient(srv)); err != nil {
		t.Fatalf("test-remote: %v", err)
	}
	if gotMPV != testRemoteProtocolVersion {
		t.Errorf("MCP-Protocol-Version=%q, want %q (manifest must not override)", gotMPV, testRemoteProtocolVersion)
	}
	if gotAccept != "application/json, text/event-stream" {
		t.Errorf("Accept=%q, want default (manifest must not override)", gotAccept)
	}
	if gotContentType != "application/json" {
		t.Errorf("Content-Type=%q, want application/json (manifest must not override)", gotContentType)
	}
	if gotAuth != "Bearer real-token" {
		t.Errorf("Authorization=%q (non-protocol header should pass through unchanged)", gotAuth)
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

func TestParseSSEEvents_MultilineData(t *testing.T) {
	in := []byte(": comment\ndata: {\"jsonrpc\":\"2.0\",\ndata: \"id\":1}\n\n")
	got := parseSSEEvents(in)
	if len(got) != 1 {
		t.Fatalf("got %d events, want 1: %q", len(got), got)
	}
	joined := string(got[0])
	if !strings.Contains(joined, `"jsonrpc":"2.0"`) || !strings.Contains(joined, `"id":1`) {
		t.Errorf("got %q, want both jsonrpc and id pieces", joined)
	}
}

func TestParseSSEEvents_NoData(t *testing.T) {
	got := parseSSEEvents([]byte(": comment only\n\n"))
	if len(got) != 0 {
		t.Errorf("got %d events, want 0", len(got))
	}
}

// TestParseSSEEvents_MultipleEvents pins the bot r1 P1 closure: a
// streaming server can emit progress/notification events before the
// initialize reply. The parser must split them, and findMatchingRPCReply
// must pick the envelope whose id matches the one we sent.
func TestParseSSEEvents_MultipleEvents(t *testing.T) {
	in := []byte("event: progress\ndata: {\"jsonrpc\":\"2.0\",\"method\":\"progress\",\"params\":{\"step\":1}}\n\nevent: response\ndata: {\"jsonrpc\":\"2.0\",\"id\":1,\"result\":{\"protocolVersion\":\"2025-11-25\"}}\n\n")
	events := parseSSEEvents(in)
	if len(events) != 2 {
		t.Fatalf("got %d events, want 2: %v", len(events), events)
	}
	rpc, err := findMatchingRPCReply(events, 1)
	if err != nil {
		t.Fatalf("findMatchingRPCReply: %v", err)
	}
	if rpc.Result["protocolVersion"] != "2025-11-25" {
		t.Errorf("picked wrong envelope: %+v", rpc)
	}
}

// TestFindMatchingRPCReply_RejectsNonJSONRPCBody pins bot r1 P2
// closure: a non-MCP endpoint returning `{"result":{...}}` without
// the JSON-RPC envelope must fail the smoke. Likewise an envelope
// with a wrong id (or no id) must not satisfy the smoke against the
// id we sent.
func TestFindMatchingRPCReply_RejectsNonJSONRPCBody(t *testing.T) {
	cases := map[string][]byte{
		"missing jsonrpc field":       []byte(`{"result":{"protocolVersion":"x"}}`),
		"wrong jsonrpc version":       []byte(`{"jsonrpc":"1.0","id":1,"result":{}}`),
		"id mismatch":                 []byte(`{"jsonrpc":"2.0","id":42,"result":{}}`),
		"string id when number sent":  []byte(`{"jsonrpc":"2.0","id":"1","result":{}}`),
		"no id":                       []byte(`{"jsonrpc":"2.0","result":{}}`),
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := findMatchingRPCReply([][]byte{body}, 1)
			if err == nil {
				t.Fatalf("%s: expected mismatch error, got nil", name)
			}
		})
	}
}

// TestFindMatchingRPCReply_SurfacesDecodeError pins the parse-error
// path: when the only event has invalid JSON, the operator should
// see the decode error so they can debug rather than a generic
// "no envelope matched".
func TestFindMatchingRPCReply_SurfacesDecodeError(t *testing.T) {
	_, err := findMatchingRPCReply([][]byte{[]byte("not json {{")}, 1)
	if err == nil || !strings.Contains(err.Error(), "decode response") {
		t.Errorf("expected decode-response diagnostic, got %v", err)
	}
}

// TestNewTestRemoteClient_NoHardcodedTimeout pins bot r2 P2 closure
// (PR #171): the production client must not carry a Timeout that
// would silently override the operator's --timeout flag. Cancellation
// goes through ctx only.
func TestNewTestRemoteClient_NoHardcodedTimeout(t *testing.T) {
	c := newTestRemoteClient()
	if c.Timeout != 0 {
		t.Errorf("newTestRemoteClient Timeout=%v, want 0 (ctx-only cancellation)", c.Timeout)
	}
}

// TestSendRemoteInitialize_RespectsContextDeadline pins that a
// caller-supplied ctx deadline cancels a slow upstream, regardless
// of any client-level timeout (which we don't set). A 200 ms ctx
// against a server that sleeps 2 s must fail fast.
func TestSendRemoteInitialize_RespectsContextDeadline(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(2 * time.Second)
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{}}`))
	}))
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := sendRemoteInitialize(ctx, buildTLSTrustingClient(srv), srv.URL, nil)
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("expected ctx deadline error")
	}
	if elapsed > 1*time.Second {
		t.Errorf("elapsed=%v — ctx should have cancelled well before server sleep finished", elapsed)
	}
}

// TestRPCIDEquals pins the id-comparison contract: only numeric ids
// equal to the int we sent are accepted.
func TestRPCIDEquals(t *testing.T) {
	if !rpcIDEquals(float64(1), 1) {
		t.Error("float64(1) should match int 1 (JSON numbers decode to float64)")
	}
	if rpcIDEquals(float64(1.5), 1) {
		t.Error("float64(1.5) should not match int 1")
	}
	if rpcIDEquals("1", 1) {
		t.Error("string ids should not match numeric id we sent")
	}
	if rpcIDEquals(nil, 1) {
		t.Error("nil id should not match")
	}
}
