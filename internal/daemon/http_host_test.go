package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

// mockUpstream is a test helper: wraps an httptest.Server that impersonates
// an MCP server and records the requests it sees. responseMode controls
// whether it replies as application/json or text/event-stream.
type mockUpstream struct {
	t            *testing.T
	server       *httptest.Server
	mu           struct{}
	responseMode string // "json" or "sse"
	lastBody     []byte
}

func newMockUpstream(t *testing.T, mode string) *mockUpstream {
	t.Helper()
	m := &mockUpstream{t: t, responseMode: mode}
	m.server = httptest.NewServer(http.HandlerFunc(m.handle))
	return m
}

func (m *mockUpstream) URL() string      { return m.server.URL }
func (m *mockUpstream) LastBody() []byte { return m.lastBody }

func (m *mockUpstream) Close() { m.server.Close() }

// handle replies to the three MCP methods we care about for bridge tests:
// initialize, tools/list, resources/read. Any other method returns
// a -32601. The reply shape depends on m.responseMode.
func (m *mockUpstream) handle(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	m.lastBody = body
	var msg map[string]json.RawMessage
	_ = json.Unmarshal(body, &msg)
	var method string
	_ = json.Unmarshal(msg["method"], &method)
	id := msg["id"]

	var result json.RawMessage
	switch method {
	case "initialize":
		result = json.RawMessage(`{"protocolVersion":"2025-03-26","capabilities":{"resources":{},"tools":{}},"serverInfo":{"name":"mock","version":"1"}}`)
	case "tools/list":
		result = json.RawMessage(`{"tools":[{"name":"native_tool","description":"a native tool"}]}`)
	case "resources/read":
		var params struct {
			URI string `json:"uri"`
		}
		_ = json.Unmarshal(msg["params"], &params)
		escaped, _ := json.Marshal("resource body for " + params.URI)
		result = json.RawMessage(fmt.Sprintf(`{"contents":[{"uri":%q,"mimeType":"text/plain","text":%s}]}`, params.URI, string(escaped)))
	default:
		w.WriteHeader(http.StatusOK)
		errResp, _ := json.Marshal(map[string]any{
			"jsonrpc": "2.0",
			"id":      json.RawMessage(id),
			"error":   map[string]any{"code": -32601, "message": "method not found: " + method},
		})
		_, _ = w.Write(errResp)
		return
	}

	respBody, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      json.RawMessage(id),
		"result":  result,
	})

	switch m.responseMode {
	case "sse":
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Mcp-Session-Id", "mock-session")
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprintf(w, "event: message\ndata: %s\n\n", respBody)
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
	default:
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Mcp-Session-Id", "mock-session")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(respBody)
	}
}

// newHTTPHostAgainstMock stands up an HTTPHost wired directly to the
// mock upstream. Skips the real subprocess — we inject httpClient and
// upstreamURL after construction.
func newHTTPHostAgainstMock(t *testing.T, upstream *mockUpstream) *HTTPHost {
	t.Helper()
	h := &HTTPHost{
		httpClient:  upstream.server.Client(),
		upstreamURL: upstream.URL(),
		bridge:      NewProtocolBridge(),
		done:        make(chan struct{}),
		childExited: make(chan struct{}),
		started:     true, // skip Start() — no subprocess to spawn
	}
	return h
}

func TestHTTPHost_JSON_InjectsReadResourceOnToolsList(t *testing.T) {
	upstream := newMockUpstream(t, "json")
	defer upstream.Close()

	h := newHTTPHostAgainstMock(t, upstream)
	ts := httptest.NewServer(h.HTTPHandler())
	defer ts.Close()

	// initialize first so capabilities are cached.
	post(t, ts.URL, "application/json",
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"t","version":"1"}}}`)

	// tools/list
	body := post(t, ts.URL, "application/json", `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`)
	var out struct {
		Result struct {
			Tools []struct {
				Name string `json:"name"`
			} `json:"tools"`
		} `json:"result"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("unmarshal: %v (body=%s)", err, body)
	}
	names := map[string]bool{}
	for _, tl := range out.Result.Tools {
		names[tl.Name] = true
	}
	if !names["native_tool"] || !names["__read_resource__"] {
		t.Errorf("tools/list missing expected entries: %+v", names)
	}
}

func TestHTTPHost_JSON_RewritesReadResourceCall(t *testing.T) {
	upstream := newMockUpstream(t, "json")
	defer upstream.Close()
	h := newHTTPHostAgainstMock(t, upstream)
	ts := httptest.NewServer(h.HTTPHandler())
	defer ts.Close()

	post(t, ts.URL, "application/json",
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"t","version":"1"}}}`)

	// tools/call __read_resource__ → upstream sees resources/read, client sees
	// tools/call-shaped content.
	body := post(t, ts.URL, "application/json",
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"__read_resource__","arguments":{"uri":"resource://workflow"}}}`)

	// Verify upstream saw resources/read with correct uri.
	var upstreamMsg map[string]json.RawMessage
	_ = json.Unmarshal(upstream.LastBody(), &upstreamMsg)
	var upstreamMethod string
	_ = json.Unmarshal(upstreamMsg["method"], &upstreamMethod)
	if upstreamMethod != "resources/read" {
		t.Errorf("upstream method = %q, want resources/read", upstreamMethod)
	}

	// Verify client saw content[] with reshaped body.
	var out struct {
		Result struct {
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"result"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(out.Result.Content) != 1 {
		t.Fatalf("expected 1 content, got %d: %s", len(out.Result.Content), body)
	}
	if !strings.Contains(out.Result.Content[0].Text, "resource body for resource://workflow") {
		t.Errorf("content missing resource body: %s", out.Result.Content[0].Text)
	}
}

func TestHTTPHost_SSE_InjectsAndReshapes(t *testing.T) {
	upstream := newMockUpstream(t, "sse")
	defer upstream.Close()
	h := newHTTPHostAgainstMock(t, upstream)
	ts := httptest.NewServer(h.HTTPHandler())
	defer ts.Close()

	// initialize over SSE — client accepts only SSE.
	initBody := postAcceptSSE(t, ts.URL,
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"t","version":"1"}}}`)
	// Extract data: payload.
	initPayload := extractSSEDataPayload(t, initBody)
	if !strings.Contains(string(initPayload), `"resources"`) {
		t.Fatalf("initialize response missing resources capability: %s", initPayload)
	}

	// tools/list over SSE — expect __read_resource__ in the data payload.
	listBody := postAcceptSSE(t, ts.URL, `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`)
	listPayload := extractSSEDataPayload(t, listBody)
	if !strings.Contains(string(listPayload), "__read_resource__") {
		t.Errorf("SSE tools/list did not have __read_resource__ injected: %s", listPayload)
	}

	// tools/call __read_resource__ over SSE.
	callBody := postAcceptSSE(t, ts.URL,
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"__read_resource__","arguments":{"uri":"resource://x"}}}`)
	callPayload := extractSSEDataPayload(t, callBody)
	if !strings.Contains(string(callPayload), `"content"`) {
		t.Errorf("SSE tools/call response missing reshaped content: %s", callPayload)
	}
	if !strings.Contains(string(callPayload), "resource body for resource://x") {
		t.Errorf("SSE tools/call content missing upstream body: %s", callPayload)
	}
}

// TestHTTPHost_InitializeForwardsAndPreservesSessionID verifies that
// EVERY initialize reaches upstream (no cache-based short-circuit) and
// that upstream's Mcp-Session-Id header is copied to the client's
// response. Caching + replaying initialize in HTTPHost would strip the
// session-id, breaking every subsequent POST/GET from the client.
//
// This is the regression guard for a real bug caught in empirical
// testing (Phase 5): the old implementation short-circuited and
// returned "Bad Request: Missing session ID" on the second client call.
func TestHTTPHost_InitializeForwardsAndPreservesSessionID(t *testing.T) {
	upstream := newMockUpstream(t, "json")
	defer upstream.Close()
	h := newHTTPHostAgainstMock(t, upstream)
	ts := httptest.NewServer(h.HTTPHandler())
	defer ts.Close()

	// Client uses net/http directly so we can inspect response headers.
	doInit := func(id int) (body []byte, sessionID string) {
		body = post(t, ts.URL, "application/json",
			`{"jsonrpc":"2.0","id":`+strconv.Itoa(id)+`,"method":"initialize","params":{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"t","version":"1"}}}`)
		// Re-issue to capture headers (post() discards them).
		req, _ := http.NewRequest("POST", ts.URL+"/mcp", strings.NewReader(
			`{"jsonrpc":"2.0","id":`+strconv.Itoa(id+100)+`,"method":"initialize","params":{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"t","version":"1"}}}`))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("POST: %v", err)
		}
		defer resp.Body.Close()
		return body, resp.Header.Get("Mcp-Session-Id")
	}

	body1, sid1 := doInit(1)
	upstream1 := append([]byte(nil), upstream.LastBody()...)
	body2, sid2 := doInit(2)
	upstream2 := upstream.LastBody()

	// Every initialize must reach upstream (both upstream bodies must be set).
	if len(upstream1) == 0 || len(upstream2) == 0 {
		t.Fatalf("upstream did not see both initializes: u1=%q u2=%q", upstream1, upstream2)
	}
	// mock writes the same Mcp-Session-Id ("mock-session") for every
	// response. What we're guarding is that the proxy COPIED that header
	// to the client — not that each session is distinct.
	if sid1 != "mock-session" || sid2 != "mock-session" {
		t.Errorf("Mcp-Session-Id not propagated: sid1=%q sid2=%q", sid1, sid2)
	}
	// And the response bodies must have the client-sent ids (1 and 2).
	var r1, r2 map[string]any
	_ = json.Unmarshal(body1, &r1)
	_ = json.Unmarshal(body2, &r2)
	if r1["id"].(float64) != 1 || r2["id"].(float64) != 2 {
		t.Errorf("id not preserved on forwarded initialize: r1.id=%v r2.id=%v", r1["id"], r2["id"])
	}
}

// TestHTTPHost_InitializeCapabilitiesCachedForInjection verifies that
// even though initialize itself is NOT short-circuited, the response
// capabilities ARE cached — so the next tools/list correctly decides
// whether to inject synthetic tools.
func TestHTTPHost_InitializeCapabilitiesCachedForInjection(t *testing.T) {
	upstream := newMockUpstream(t, "json")
	defer upstream.Close()
	h := newHTTPHostAgainstMock(t, upstream)
	ts := httptest.NewServer(h.HTTPHandler())
	defer ts.Close()

	// initialize: upstream advertises resources capability.
	post(t, ts.URL, "application/json",
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"t","version":"1"}}}`)

	// tools/list: should inject __read_resource__ because the bridge
	// cached capabilities.resources from initialize.
	body := post(t, ts.URL, "application/json", `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`)
	if !strings.Contains(string(body), "__read_resource__") {
		t.Errorf("capability cache not populated — tools/list did not get __read_resource__: %s", body)
	}
}

// --- SSE parser edge cases ---

func TestStreamSSE_PassesThroughNonJSONDataLines(t *testing.T) {
	upstream := newMockUpstream(t, "json")
	defer upstream.Close()
	h := newHTTPHostAgainstMock(t, upstream)

	// Simulated SSE stream with mix of JSON data, non-JSON comments,
	// and event fields. Must preserve non-data lines as-is.
	in := strings.NewReader(
		"event: keepalive\n" +
			": server-sent comment\n" +
			"data: not json here\n" +
			"\n" +
			"event: message\n" +
			"data: {\"jsonrpc\":\"2.0\",\"id\":5,\"result\":{\"tools\":[]}}\n" +
			"\n",
	)
	w := &fakeFlushResponseWriter{header: http.Header{}}

	h.bridge.CacheInitialize(json.RawMessage(`{"result":{"capabilities":{"resources":{}}}}`))
	h.streamSSEResponse(w, in, "tools/list", nil)

	out := w.buf.String()
	if !strings.Contains(out, "event: keepalive") {
		t.Errorf("keepalive event not preserved: %q", out)
	}
	if !strings.Contains(out, ": server-sent comment") {
		t.Errorf("SSE comment not preserved: %q", out)
	}
	if !strings.Contains(out, "data: not json here") {
		t.Errorf("non-JSON data line not preserved: %q", out)
	}
	// The JSON data line should have been transformed (tools/list → __read_resource__ injected).
	if !strings.Contains(out, "__read_resource__") {
		t.Errorf("JSON data line not transformed: %q", out)
	}
}

// --- test helpers ---

func post(t *testing.T, url, contentType, body string) []byte {
	t.Helper()
	req, _ := http.NewRequest("POST", url+"/mcp", strings.NewReader(body))
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("Accept", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		t.Logf("POST %s returned HTTP %d: %s", url, resp.StatusCode, raw)
	}
	return raw
}

func postAcceptSSE(t *testing.T, url, body string) []byte {
	t.Helper()
	req, _ := http.NewRequest("POST", url+"/mcp", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return raw
}

func extractSSEDataPayload(t *testing.T, sseBody []byte) []byte {
	t.Helper()
	for _, line := range strings.Split(string(sseBody), "\n") {
		if strings.HasPrefix(line, "data: ") {
			return []byte(strings.TrimPrefix(line, "data: "))
		}
	}
	t.Fatalf("no data: line in SSE body: %s", sseBody)
	return nil
}

// fakeFlushResponseWriter is a minimal http.ResponseWriter + http.Flusher
// impl used by streamSSEResponse tests so we can inspect written bytes.
type fakeFlushResponseWriter struct {
	header http.Header
	buf    strings.Builder
	status int
}

func (f *fakeFlushResponseWriter) Header() http.Header         { return f.header }
func (f *fakeFlushResponseWriter) WriteHeader(statusCode int)  { f.status = statusCode }
func (f *fakeFlushResponseWriter) Write(b []byte) (int, error) { return f.buf.Write(b) }
func (f *fakeFlushResponseWriter) Flush()                      {}

// --- waitForReady / Start guardrail ---

func TestHTTPHost_WaitForReady_ReturnsImmediatelyWhenUpstreamReady(t *testing.T) {
	// httptest.Server is up before Start() is called, so waitForReady
	// should return on the very first probe.
	upstream := newMockUpstream(t, "json")
	defer upstream.Close()

	h, err := NewHTTPHost(HTTPHostConfig{
		Command:       "true", // no-op command
		UpstreamPort:  parsePort(t, upstream.URL()),
		HealthTimeout: 2 * time.Second,
	})
	if err != nil {
		t.Fatalf("NewHTTPHost: %v", err)
	}
	h.httpClient = upstream.server.Client()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	if err := h.waitForReady(ctx); err != nil {
		t.Fatalf("waitForReady failed when upstream already up: %v", err)
	}
}

func parsePort(t *testing.T, urlStr string) int {
	t.Helper()
	_, port, err := net.SplitHostPort(strings.TrimPrefix(urlStr, "http://"))
	if err != nil {
		t.Fatalf("parse port from %q: %v", urlStr, err)
	}
	p := 0
	fmt.Sscanf(port, "%d", &p)
	return p
}
