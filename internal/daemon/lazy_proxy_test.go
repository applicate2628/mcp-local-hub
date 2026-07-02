package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"mcp-local-hub/internal/api"
)

// --- test doubles -----------------------------------------------------------

// fakeLifecycle is a BackendLifecycle stand-in that records Materialize /
// Stop call counts and returns caller-configured results. Every test in
// this file uses a fakeLifecycle so no real LSP subprocess is spawned.
type fakeLifecycle struct {
	kind string

	// materializeErr is returned from Materialize when non-nil. If nil, a
	// fakeEndpoint is returned.
	materializeErr   error
	materializeDelay time.Duration

	// sendRequestErr, if set, is returned from the fakeEndpoint's SendRequest.
	sendRequestErr error
	// sendResultRaw is the raw result bytes the fake endpoint returns (default
	// `{"ok":true}`).
	sendResultRaw json.RawMessage
	// sendRPCError, if set, is returned as a JSON-RPC error response with a
	// nil Go error to model backends rejecting a delivered request.
	sendRPCError *JSONRPCError

	// firstSendGate, when non-nil, makes the FIRST SendRequest block until the
	// gate closes OR the request context is done — modeling a gopls-style
	// indexing pause after the MCP handshake. A never-closed gate lets the
	// caller's probation budget fire (SendRequest returns ctx.Err()); a
	// test-closed gate lets the first response land, exercising the
	// Starting -> warmed -> Active transition.
	firstSendGate chan struct{}

	materializeCount atomic.Int32
	stopCount        atomic.Int32
	sendCount        atomic.Int32
}

func (f *fakeLifecycle) Kind() string { return f.kind }

func (f *fakeLifecycle) Materialize(ctx context.Context) (MCPEndpoint, error) {
	f.materializeCount.Add(1)
	if f.materializeDelay > 0 {
		select {
		case <-time.After(f.materializeDelay):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if f.materializeErr != nil {
		return nil, f.materializeErr
	}
	return &fakeEndpoint{parent: f}, nil
}

func (f *fakeLifecycle) Stop() error { f.stopCount.Add(1); return nil }

type fakeEndpoint struct {
	parent *fakeLifecycle
	closed atomic.Bool
}

func (e *fakeEndpoint) SendRequest(ctx context.Context, req *JSONRPCRequest) (*JSONRPCResponse, error) {
	if e.closed.Load() {
		return nil, errors.New("endpoint closed")
	}
	n := e.parent.sendCount.Add(1)
	if e.parent.firstSendGate != nil && n == 1 {
		select {
		case <-e.parent.firstSendGate:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if e.parent.sendRequestErr != nil {
		return nil, e.parent.sendRequestErr
	}
	if e.parent.sendRPCError != nil {
		return &JSONRPCResponse{Jsonrpc: "2.0", ID: req.ID, Error: e.parent.sendRPCError}, nil
	}
	res := e.parent.sendResultRaw
	if len(res) == 0 {
		res = json.RawMessage(`{"ok":true}`)
	}
	return &JSONRPCResponse{Jsonrpc: "2.0", ID: req.ID, Result: res}, nil
}

func (e *fakeEndpoint) Close() error { e.closed.Store(true); return nil }

type reapingRaceLifecycle struct {
	mu sync.Mutex

	host   *reapingRaceEndpoint
	nextID int

	stopStarted           chan struct{}
	allowStop             chan struct{}
	materializeDuringStop chan struct{}
	stopStartedOnce       sync.Once
	stopReleased          atomic.Bool

	materializeCount atomic.Int32
	stopCount        atomic.Int32
}

func newReapingRaceLifecycle() *reapingRaceLifecycle {
	return &reapingRaceLifecycle{
		stopStarted:           make(chan struct{}),
		allowStop:             make(chan struct{}),
		materializeDuringStop: make(chan struct{}, 1),
	}
}

func (f *reapingRaceLifecycle) Kind() string { return "mcp-language-server" }

func (f *reapingRaceLifecycle) Materialize(ctx context.Context) (MCPEndpoint, error) {
	f.materializeCount.Add(1)
	select {
	case <-f.stopStarted:
		if !f.stopReleased.Load() {
			select {
			case f.materializeDuringStop <- struct{}{}:
			default:
			}
		}
	default:
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.host == nil {
		f.nextID++
		f.host = &reapingRaceEndpoint{id: f.nextID}
	}
	return f.host, nil
}

func (f *reapingRaceLifecycle) Stop() error {
	f.stopCount.Add(1)
	f.stopStartedOnce.Do(func() { close(f.stopStarted) })
	<-f.allowStop
	f.stopReleased.Store(true)
	f.mu.Lock()
	f.host = nil
	f.mu.Unlock()
	return nil
}

type reapingRaceEndpoint struct {
	id     int
	closed atomic.Bool
}

func (e *reapingRaceEndpoint) SendRequest(ctx context.Context, req *JSONRPCRequest) (*JSONRPCResponse, error) {
	if e.closed.Load() {
		return nil, errors.New("endpoint closed")
	}
	return &JSONRPCResponse{Jsonrpc: "2.0", ID: req.ID, Result: json.RawMessage(`{"ok":true}`)}, nil
}

func (e *reapingRaceEndpoint) Close() error {
	e.closed.Store(true)
	return nil
}

type stopAfterMaterializeLifecycle struct {
	materializeStarted chan struct{}
	releaseMaterialize chan struct{}
	ep                 *fakeEndpoint
	stopCount          atomic.Int32
	startedOnce        sync.Once
}

func newStopAfterMaterializeLifecycle() *stopAfterMaterializeLifecycle {
	return &stopAfterMaterializeLifecycle{
		materializeStarted: make(chan struct{}),
		releaseMaterialize: make(chan struct{}),
	}
}

func (f *stopAfterMaterializeLifecycle) Kind() string { return "mcp-language-server" }

func (f *stopAfterMaterializeLifecycle) Materialize(ctx context.Context) (MCPEndpoint, error) {
	f.startedOnce.Do(func() { close(f.materializeStarted) })
	select {
	case <-f.releaseMaterialize:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	parent := &fakeLifecycle{kind: "mcp-language-server"}
	f.ep = &fakeEndpoint{parent: parent}
	return f.ep, nil
}

func (f *stopAfterMaterializeLifecycle) Stop() error {
	f.stopCount.Add(1)
	return nil
}

// recordingLifecycle / recordingEndpoint capture every (method, uri) tuple
// that actually reaches the upstream backend via SendRequest. Used by the
// didOpen/didClose refcount tests to assert exactly which notifications were
// forwarded vs absorbed by the per-URI gate. Materialize/Stop counts are
// tracked so reset-on-teardown behavior can be verified too.
type recordingLifecycle struct {
	mu               sync.Mutex
	forwarded        []forwardedDoc
	materializeCount atomic.Int32
	stopCount        atomic.Int32
	ep               *recordingEndpoint
}

type forwardedDoc struct {
	method string
	uri    string
}

func (f *recordingLifecycle) Kind() string { return "mcp-language-server" }

func (f *recordingLifecycle) Materialize(ctx context.Context) (MCPEndpoint, error) {
	f.materializeCount.Add(1)
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.ep == nil {
		f.ep = &recordingEndpoint{parent: f}
	}
	return f.ep, nil
}

func (f *recordingLifecycle) Stop() error {
	f.stopCount.Add(1)
	f.mu.Lock()
	f.ep = nil
	f.mu.Unlock()
	return nil
}

func (f *recordingLifecycle) forwardedDocs() []forwardedDoc {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]forwardedDoc, len(f.forwarded))
	copy(out, f.forwarded)
	return out
}

type recordingEndpoint struct {
	parent *recordingLifecycle
	closed atomic.Bool
}

func (e *recordingEndpoint) SendRequest(ctx context.Context, req *JSONRPCRequest) (*JSONRPCResponse, error) {
	if e.closed.Load() {
		return nil, errors.New("endpoint closed")
	}
	if req.Method == "textDocument/didOpen" || req.Method == "textDocument/didClose" {
		e.parent.mu.Lock()
		e.parent.forwarded = append(e.parent.forwarded, forwardedDoc{
			method: req.Method,
			uri:    docURIFromParams(req.Params),
		})
		e.parent.mu.Unlock()
	}
	return &JSONRPCResponse{Jsonrpc: "2.0", ID: req.ID, Result: json.RawMessage(`{"ok":true}`)}, nil
}

func (e *recordingEndpoint) Close() error { e.closed.Store(true); return nil }

// --- helpers ---------------------------------------------------------------

func newTestProxy(t *testing.T, kind string, f *fakeLifecycle) (*LazyProxy, string) {
	t.Helper()
	return newTestProxyWithCfg(t, kind, f, 50*time.Millisecond, 100*time.Millisecond)
}

func newTestProxyWithCfg(t *testing.T, kind string, f *fakeLifecycle, retryGap, toolsDebounce time.Duration) (*LazyProxy, string) {
	t.Helper()
	regPath := filepath.Join(t.TempDir(), "r.yaml")
	// Seed the (workspace_key, language) entry — mirrors production flow
	// where api.Register creates the entry before the proxy process runs.
	// PutLifecycle silently no-ops if the entry is missing (to prevent
	// ghost-row resurrection after unregister), so tests that assert
	// proxy lifecycle writes must seed first.
	seed := api.NewRegistry(regPath)
	seed.Put(api.WorkspaceEntry{
		WorkspaceKey:  "abcd1234",
		WorkspacePath: "D:/test/ws",
		Language:      "python",
		Backend:       kind,
		TaskName:      "mcp-local-hub-lsp-abcd1234-python",
		Lifecycle:     "", // proxy's ListenAndServe will stamp Configured
	})
	if err := seed.Save(); err != nil {
		t.Fatalf("seed registry: %v", err)
	}
	p := NewLazyProxy(LazyProxyConfig{
		WorkspaceKey:        "abcd1234",
		WorkspacePath:       "D:/test/ws",
		Language:            "python",
		BackendKind:         kind,
		Port:                0,
		Lifecycle:           f,
		RegistryPath:        regPath,
		InflightMinRetryGap: retryGap,
		ToolsCallDebounce:   toolsDebounce,
	})
	return p, regPath
}

func postRPC(t *testing.T, h http.Handler, method string, id int) *httptest.ResponseRecorder {
	t.Helper()
	body := fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":%q,"params":{}}`, id, method)
	req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1/mcp", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	return rr
}

// postDocNotification fires a textDocument/didOpen|didClose notification (no
// id) carrying params.textDocument.uri = uri, mirroring the LSP wire shape a
// client/agent sends.
func postDocNotification(t *testing.T, h http.Handler, method, uri string) *httptest.ResponseRecorder {
	t.Helper()
	body := fmt.Sprintf(`{"jsonrpc":"2.0","method":%q,"params":{"textDocument":{"uri":%q}}}`, method, uri)
	req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1/mcp", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	return rr
}

func postRPCWithHeaders(t *testing.T, h http.Handler, method string, id int, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	body := fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":%q,"params":{}}`, id, method)
	req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(body))
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	return rr
}

func parseRPC(t *testing.T, body []byte) map[string]any {
	t.Helper()
	var got map[string]any
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("unmarshal response: %v body=%s", err, string(body))
	}
	return got
}

func readEntry(t *testing.T, regPath string) api.WorkspaceEntry {
	t.Helper()
	r := api.NewRegistry(regPath)
	if err := r.Load(); err != nil {
		t.Fatalf("registry load: %v", err)
	}
	e, ok := r.Get("abcd1234", "python")
	if !ok {
		t.Fatalf("no registry entry for (abcd1234, python)")
	}
	return e
}

// wrapMissing reconstructs the exact error shape mcpLanguageServerStdio
// uses so tests share the same production classifier path without exposing
// the unexported errMissingBinary sentinel.
func wrapMissing(cmdName string) error {
	lc := NewMcpLanguageServerStdio(McpLanguageServerStdioConfig{
		WrapperCommand: cmdName,
		WrapperArgs:    []string{"-workspace", "ignored"},
		Workspace:      ".",
		Language:       "python",
	})
	_, err := lc.Materialize(context.Background())
	if err == nil {
		return errors.New("expected LookPath failure for bogus cmd")
	}
	return err
}

// --- tests -----------------------------------------------------------------

func TestLazyProxyLoopbackGuardRejectsHostilePOSTAndSSE(t *testing.T) {
	f := &fakeLifecycle{kind: "mcp-language-server"}
	p, _ := newTestProxy(t, "mcp-language-server", f)
	handler := p.Handler()

	for _, tc := range []struct {
		name         string
		method       string
		host         string
		origin       string
		secFetchSite string
	}{
		{
			name:         "post hostile host same-origin fetch",
			method:       http.MethodPost,
			host:         "evil.example",
			secFetchSite: "same-origin",
		},
		{
			name:         "post hostile origin cross-site fetch",
			method:       http.MethodPost,
			origin:       "https://evil.example",
			secFetchSite: "cross-site",
		},
		{
			name:         "sse hostile host same-origin fetch",
			method:       http.MethodGet,
			host:         "evil.example",
			secFetchSite: "same-origin",
		},
		{
			name:         "sse hostile origin cross-site fetch",
			method:       http.MethodGet,
			origin:       "https://evil.example",
			secFetchSite: "cross-site",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reqCtx, cancel := context.WithCancel(context.Background())
			cancel()
			var body io.Reader
			if tc.method == http.MethodPost {
				body = strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{}}`)
			}
			req := httptest.NewRequestWithContext(reqCtx, tc.method, "http://127.0.0.1/mcp", body)
			req.Header.Set("Content-Type", "application/json")
			if tc.host != "" {
				req.Host = tc.host
			}
			if tc.origin != "" {
				req.Header.Set("Origin", tc.origin)
			}
			if tc.secFetchSite != "" {
				req.Header.Set("Sec-Fetch-Site", tc.secFetchSite)
			}

			rr := httptest.NewRecorder()
			handler.ServeHTTP(rr, req)
			if rr.Code != http.StatusForbidden {
				t.Fatalf("status = %d, want %d; body=%s", rr.Code, http.StatusForbidden, rr.Body.String())
			}
		})
	}
	if got := f.materializeCount.Load(); got != 0 {
		t.Fatalf("rejected requests materialized backend %d time(s), want 0", got)
	}
}

func TestLazyProxyLoopbackGuardAllowsNoOriginPOST(t *testing.T) {
	f := &fakeLifecycle{kind: "mcp-language-server"}
	p, _ := newTestProxy(t, "mcp-language-server", f)
	rr := postRPC(t, p.Handler(), "initialize", 1)
	if rr.Code != http.StatusOK {
		t.Fatalf("POST without Origin code=%d want %d body=%s", rr.Code, http.StatusOK, rr.Body.String())
	}
	got := parseRPC(t, rr.Body.Bytes())
	if got["result"] == nil {
		t.Fatalf("POST without Origin returned no result: %+v", got)
	}
}

func TestLazyProxy_InitializeSyntheticNoMaterialize(t *testing.T) {
	f := &fakeLifecycle{kind: "mcp-language-server"}
	p, _ := newTestProxy(t, "mcp-language-server", f)
	rr := postRPC(t, p.Handler(), "initialize", 1)
	if rr.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	got := parseRPC(t, rr.Body.Bytes())
	if got["result"] == nil {
		t.Fatalf("no result in synthetic initialize: %v", got)
	}
	result := got["result"].(map[string]any)
	if result["serverInfo"] == nil {
		t.Errorf("serverInfo missing: %+v", result)
	}
	if f.materializeCount.Load() != 0 {
		t.Errorf("initialize triggered materialize: count=%d", f.materializeCount.Load())
	}
}

func TestLazyProxy_RejectsNonJSONContentType(t *testing.T) {
	f := &fakeLifecycle{kind: "mcp-language-server"}
	p, _ := newTestProxy(t, "mcp-language-server", f)
	rr := postRPCWithHeaders(t, p.Handler(), "tools/list", 1, map[string]string{
		"Content-Type": "text/plain",
	})
	if rr.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("code = %d, want 415; body=%s", rr.Code, rr.Body.String())
	}
	if f.materializeCount.Load() != 0 {
		t.Errorf("request triggered materialize: count=%d", f.materializeCount.Load())
	}
}

func TestLazyProxy_RejectsCrossSiteOrigin(t *testing.T) {
	f := &fakeLifecycle{kind: "mcp-language-server"}
	p, _ := newTestProxy(t, "mcp-language-server", f)
	rr := postRPCWithHeaders(t, p.Handler(), "tools/list", 1, map[string]string{
		"Content-Type": "application/json",
		"Origin":       "https://attacker.example",
	})
	if rr.Code != http.StatusForbidden {
		t.Fatalf("code = %d, want 403; body=%s", rr.Code, rr.Body.String())
	}
	if f.materializeCount.Load() != 0 {
		t.Errorf("request triggered materialize: count=%d", f.materializeCount.Load())
	}
}

// TestLazyProxy_PingEchoesClientID guards the JSON-RPC request/response
// correlation contract: when a client sends `ping` with a real id, the
// proxy must echo that same id in its reply. A hard-coded null (or any
// other value) breaks heartbeat/probe logic that matches id-to-response
// via strict equality.
func TestLazyProxy_PingEchoesClientID(t *testing.T) {
	f := &fakeLifecycle{kind: "mcp-language-server"}
	p, _ := newTestProxy(t, "mcp-language-server", f)

	rr := postRPC(t, p.Handler(), "ping", 4242)
	if rr.Code != http.StatusOK {
		t.Fatalf("ping code = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	got := parseRPC(t, rr.Body.Bytes())
	// JSON numbers decode as float64 via encoding/json into an interface{}.
	id, ok := got["id"].(float64)
	if !ok {
		t.Fatalf("ping reply id type %T want float64 (echoed request id): %v", got["id"], got)
	}
	if int(id) != 4242 {
		t.Errorf("ping id = %v, want 4242 (client id must be echoed)", id)
	}
	if f.materializeCount.Load() != 0 {
		t.Errorf("ping triggered materialize: count=%d", f.materializeCount.Load())
	}
}

// TestLazyProxy_UnknownNotificationReturns202NoForward guards the
// dispatch default-branch fix: ANY `notifications/*` method (known or
// unknown) must resolve as 202 Accepted without forwarding to the
// backend, because JSON-RPC 2.0 forbids responses to notifications and
// handleForward would block waiting for one that the backend is spec-
// bound not to emit. Regression scenario: client sends
// notifications/progress or a custom notification — proxy must not
// materialize or forward.
func TestLazyProxy_UnknownNotificationReturns202NoForward(t *testing.T) {
	f := &fakeLifecycle{kind: "mcp-language-server"}
	p, _ := newTestProxy(t, "mcp-language-server", f)

	for _, method := range []string{"notifications/progress", "notifications/roots/list_changed", "notifications/custom/app-specific"} {
		rr := postRPC(t, p.Handler(), method, 0) // notifications have no id
		if rr.Code != http.StatusAccepted {
			t.Errorf("%s: code = %d, want 202 Accepted (notifications must not forward)", method, rr.Code)
		}
		if got := rr.Body.Len(); got != 0 {
			t.Errorf("%s: body length = %d, want 0", method, got)
		}
	}
	if f.materializeCount.Load() != 0 {
		t.Errorf("unknown notifications triggered materialize: count=%d", f.materializeCount.Load())
	}
}

// TestLazyProxy_NotificationsReturn202NoBody verifies that true JSON-RPC
// notifications (no id expected per spec) receive 202 Accepted with an
// empty body — not a synthetic JSON envelope with null id, which could
// confuse strict JSON-RPC clients.
func TestLazyProxy_NotificationsReturn202NoBody(t *testing.T) {
	f := &fakeLifecycle{kind: "mcp-language-server"}
	p, _ := newTestProxy(t, "mcp-language-server", f)

	for _, method := range []string{"notifications/initialized", "notifications/cancelled"} {
		rr := postRPC(t, p.Handler(), method, 99)
		if rr.Code != http.StatusAccepted {
			t.Errorf("%s: code = %d, want 202 Accepted", method, rr.Code)
		}
		if got := rr.Body.Len(); got != 0 {
			t.Errorf("%s: body length = %d, want 0 (notifications have no response)", method, got)
		}
	}
	if f.materializeCount.Load() != 0 {
		t.Errorf("notifications triggered materialize: count=%d", f.materializeCount.Load())
	}
}

func TestLazyProxy_ToolsListSyntheticNoMaterialize(t *testing.T) {
	f := &fakeLifecycle{kind: "mcp-language-server"}
	p, _ := newTestProxy(t, "mcp-language-server", f)
	rr := postRPC(t, p.Handler(), "tools/list", 2)
	if rr.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200", rr.Code)
	}
	got := parseRPC(t, rr.Body.Bytes())
	result := got["result"].(map[string]any)
	tools, ok := result["tools"].([]any)
	if !ok {
		t.Fatalf("tools missing or wrong type: %+v", result)
	}
	if len(tools) == 0 {
		t.Error("tools/list returned empty tool set from synthetic catalog")
	}
	if f.materializeCount.Load() != 0 {
		t.Errorf("tools/list triggered materialize: count=%d", f.materializeCount.Load())
	}
}

func TestLazyProxy_ResourcesAndPromptsListSyntheticNoMaterialize(t *testing.T) {
	f := &fakeLifecycle{kind: "mcp-language-server"}
	p, _ := newTestProxy(t, "mcp-language-server", f)
	h := p.Handler()

	cases := []struct {
		method string
		field  string
	}{
		{method: "resources/list", field: "resources"},
		{method: "prompts/list", field: "prompts"},
	}
	for _, tc := range cases {
		rr := postRPC(t, h, tc.method, 2)
		if rr.Code != http.StatusOK {
			t.Fatalf("%s code = %d, want 200; body=%s", tc.method, rr.Code, rr.Body.String())
		}
		got := parseRPC(t, rr.Body.Bytes())
		result, ok := got["result"].(map[string]any)
		if !ok {
			t.Fatalf("%s result missing or wrong type: %+v", tc.method, got)
		}
		items, ok := result[tc.field].([]any)
		if !ok {
			t.Fatalf("%s field %q missing or wrong type: %+v", tc.method, tc.field, result)
		}
		if len(items) != 0 {
			t.Fatalf("%s returned %d %s, want empty synthetic list", tc.method, len(items), tc.field)
		}
	}
	if f.materializeCount.Load() != 0 {
		t.Errorf("resources/prompts list triggered materialize: count=%d", f.materializeCount.Load())
	}
}

// TestLazyProxy_BackendKindSelectsCatalog verifies the proxy passes its
// configured BackendKind to the synthetic-catalog factory. Using "gopls-mcp"
// must surface gopls tool names (not mcp-language-server's).
func TestLazyProxy_BackendKindSelectsCatalog(t *testing.T) {
	f := &fakeLifecycle{kind: "gopls-mcp"}
	p, _ := newTestProxy(t, "gopls-mcp", f)
	rr := postRPC(t, p.Handler(), "tools/list", 1)
	if rr.Code != http.StatusOK {
		t.Fatalf("code = %d", rr.Code)
	}
	got := parseRPC(t, rr.Body.Bytes())
	result := got["result"].(map[string]any)
	tools := result["tools"].([]any)
	foundGoplsTool := false
	for _, t := range tools {
		name, _ := t.(map[string]any)["name"].(string)
		if strings.HasPrefix(name, "go_") {
			foundGoplsTool = true
			break
		}
	}
	if !foundGoplsTool {
		t.Errorf("gopls-mcp catalog did not surface any go_* tool; tools=%+v", tools)
	}
}

// TestLazyProxy_UnknownBackendKindRejectsSynthetic verifies initialize with
// an unknown backend kind returns a JSON-RPC error envelope (driven by
// api.ToolCatalogForBackend's miss path).
func TestLazyProxy_UnknownBackendKindRejectsSynthetic(t *testing.T) {
	f := &fakeLifecycle{kind: "unknown-kind"}
	p, _ := newTestProxy(t, "unknown-kind", f)
	rr := postRPC(t, p.Handler(), "initialize", 1)
	if rr.Code != http.StatusOK {
		t.Fatalf("code = %d", rr.Code)
	}
	got := parseRPC(t, rr.Body.Bytes())
	if got["error"] == nil {
		t.Fatalf("expected JSON-RPC error for unknown backend kind: %+v", got)
	}
}

func TestLazyProxy_RepeatedInitializeStillSynthetic(t *testing.T) {
	f := &fakeLifecycle{kind: "mcp-language-server"}
	p, _ := newTestProxy(t, "mcp-language-server", f)
	h := p.Handler()
	if rr := postRPC(t, h, "initialize", 1); rr.Code != http.StatusOK {
		t.Fatalf("init code=%d", rr.Code)
	}
	if rr := postRPC(t, h, "tools/call", 2); rr.Code != http.StatusOK {
		t.Fatalf("tools/call code=%d body=%s", rr.Code, rr.Body.String())
	}
	startCount := f.materializeCount.Load()
	if rr := postRPC(t, h, "initialize", 3); rr.Code != http.StatusOK {
		t.Fatalf("init2 code=%d", rr.Code)
	}
	if f.materializeCount.Load() != startCount {
		t.Errorf("second initialize triggered extra materialize: before=%d after=%d",
			startCount, f.materializeCount.Load())
	}
	if f.sendCount.Load() != 1 {
		t.Errorf("initialize should not forward to backend: sendCount=%d", f.sendCount.Load())
	}
}

func TestLazyProxy_ToolsCallMaterializesOnce(t *testing.T) {
	f := &fakeLifecycle{kind: "mcp-language-server"}
	p, regPath := newTestProxy(t, "mcp-language-server", f)
	h := p.Handler()
	if rr := postRPC(t, h, "tools/call", 10); rr.Code != http.StatusOK {
		t.Fatalf("first tools/call: %d body=%s", rr.Code, rr.Body.String())
	}
	if rr := postRPC(t, h, "tools/call", 11); rr.Code != http.StatusOK {
		t.Fatalf("second tools/call: %d body=%s", rr.Code, rr.Body.String())
	}
	if got := f.materializeCount.Load(); got != 1 {
		t.Errorf("materializeCount = %d, want 1", got)
	}
	if got := f.sendCount.Load(); got != 2 {
		t.Errorf("sendCount = %d, want 2 (one per tools/call)", got)
	}
	e := readEntry(t, regPath)
	if e.Lifecycle != api.LifecycleActive {
		t.Errorf("lifecycle = %q, want %q", e.Lifecycle, api.LifecycleActive)
	}
	if e.LastMaterializedAt.IsZero() {
		t.Errorf("LastMaterializedAt not stamped after successful materialize")
	}
}

func TestLazyProxy_EnsureMaterializedReservesCachedEndpointAgainstIdleReaper(t *testing.T) {
	f := &fakeLifecycle{kind: "mcp-language-server"}
	p, _ := newTestProxy(t, "mcp-language-server", f)
	p.cfg.IdleBackendTTL = 10 * time.Millisecond
	endpoint := &fakeEndpoint{parent: f}
	p.mu.Lock()
	p.endpoint = endpoint
	p.lastBackendActivity = time.Now().Add(-time.Minute)
	p.mu.Unlock()

	ep, err := p.ensureMaterialized(context.Background())
	if err != nil {
		t.Fatalf("ensureMaterialized: %v", err)
	}
	p.reapIdleBackend(time.Now().UTC())
	_, err = ep.SendRequest(context.Background(), &JSONRPCRequest{
		Jsonrpc: "2.0",
		ID:      json.RawMessage("1"),
		Method:  "tools/call",
	})
	p.endBackendRequest()
	if err != nil {
		t.Fatalf("idle reaper closed cached endpoint before request was reserved: %v", err)
	}
	if got := f.stopCount.Load(); got != 0 {
		t.Fatalf("idle reaper stopped backend while cached endpoint was in use; stopCount=%d", got)
	}
}

func TestLazyProxy_EnsureMaterializedWaitsForIdleReapStopBeforeLifecycleReuse(t *testing.T) {
	f := newReapingRaceLifecycle()
	p, _ := newTestProxy(t, "mcp-language-server", nil)
	p.cfg.Lifecycle = f
	p.cfg.IdleBackendTTL = 10 * time.Millisecond

	first, err := p.ensureMaterialized(context.Background())
	if err != nil {
		t.Fatalf("initial ensureMaterialized: %v", err)
	}
	p.endBackendRequest()
	p.mu.Lock()
	p.lastBackendActivity = time.Now().Add(-time.Minute)
	p.mu.Unlock()

	reapDone := make(chan struct{})
	go func() {
		p.reapIdleBackend(time.Now().UTC())
		close(reapDone)
	}()
	<-f.stopStarted

	type materializedResult struct {
		ep  MCPEndpoint
		err error
	}
	result := make(chan materializedResult, 1)
	go func() {
		ep, err := p.ensureMaterialized(context.Background())
		result <- materializedResult{ep: ep, err: err}
	}()

	select {
	case <-f.materializeDuringStop:
		t.Fatal("Materialize ran while the idle reaper was still stopping the cached lifecycle")
	case got := <-result:
		t.Fatalf("ensureMaterialized returned before lifecycle Stop completed: ep=%T err=%v", got.ep, got.err)
	case <-time.After(100 * time.Millisecond):
	}

	close(f.allowStop)
	select {
	case got := <-result:
		if got.err != nil {
			t.Fatalf("ensureMaterialized after reaping: %v", got.err)
		}
		if got.ep == first {
			t.Fatal("ensureMaterialized reused the endpoint being reaped; want a fresh endpoint after Stop")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("ensureMaterialized did not finish after lifecycle Stop completed")
	}
	select {
	case <-reapDone:
	case <-time.After(2 * time.Second):
		t.Fatal("idle reaper did not finish after lifecycle Stop was released")
	}
}

func TestLazyProxy_OnSendFailureWaitsForStopBeforeLifecycleReuse(t *testing.T) {
	f := newReapingRaceLifecycle()
	p, _ := newTestProxy(t, "mcp-language-server", nil)
	p.cfg.Lifecycle = f

	first, err := p.ensureMaterialized(context.Background())
	if err != nil {
		t.Fatalf("initial ensureMaterialized: %v", err)
	}
	p.endBackendRequest()

	failureDone := make(chan struct{})
	go func() {
		p.onSendFailure(errors.New("backend subprocess exited"))
		close(failureDone)
	}()
	<-f.stopStarted

	type materializedResult struct {
		ep  MCPEndpoint
		err error
	}
	result := make(chan materializedResult, 1)
	go func() {
		ep, err := p.ensureMaterialized(context.Background())
		result <- materializedResult{ep: ep, err: err}
	}()

	select {
	case <-f.materializeDuringStop:
		t.Fatal("Materialize ran while onSendFailure was still stopping the cached lifecycle")
	case got := <-result:
		t.Fatalf("ensureMaterialized returned before onSendFailure Stop completed: ep=%T err=%v", got.ep, got.err)
	case <-time.After(100 * time.Millisecond):
	}

	close(f.allowStop)
	select {
	case <-failureDone:
	case <-time.After(2 * time.Second):
		t.Fatal("onSendFailure did not finish after lifecycle Stop was released")
	}
	select {
	case got := <-result:
		if got.err != nil {
			t.Fatalf("ensureMaterialized after onSendFailure: %v", got.err)
		}
		if got.ep == first {
			t.Fatal("ensureMaterialized reused the endpoint being reaped after send failure")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("ensureMaterialized did not finish after onSendFailure completed")
	}
}

func TestLazyProxy_StopDuringMaterializeFailsClosed(t *testing.T) {
	f := newStopAfterMaterializeLifecycle()
	p, _ := newTestProxy(t, "mcp-language-server", nil)
	p.cfg.Lifecycle = f

	type materializedResult struct {
		ep  MCPEndpoint
		err error
	}
	result := make(chan materializedResult, 1)
	go func() {
		ep, err := p.ensureMaterialized(context.Background())
		result <- materializedResult{ep: ep, err: err}
	}()
	<-f.materializeStarted

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := p.Stop(ctx); err != nil {
		t.Fatalf("Stop during materialize: %v", err)
	}
	close(f.releaseMaterialize)

	var got materializedResult
	select {
	case got = <-result:
	case <-time.After(2 * time.Second):
		t.Fatal("ensureMaterialized did not finish after materialize was released")
	}
	if got.err == nil {
		t.Fatalf("ensureMaterialized returned nil error after Stop won; ep=%T", got.ep)
	}
	if f.ep == nil {
		t.Fatal("test lifecycle did not return an endpoint")
	}
	if !f.ep.closed.Load() {
		t.Fatal("endpoint returned after Stop was not closed")
	}
	if got := f.stopCount.Load(); got < 2 {
		t.Fatalf("Lifecycle.Stop count = %d, want at least 2 (Stop plus closed-won materialize cleanup)", got)
	}
	p.mu.Lock()
	cached := p.endpoint
	p.mu.Unlock()
	if cached != nil {
		t.Fatalf("endpoint cached after Stop won materialization race: %T", cached)
	}
}

func TestLazyProxy_ConcurrentFirstCall(t *testing.T) {
	// 200ms delay (not 30ms) gives enough headroom for all 10 goroutines to
	// enter gate.Do before the first materialize completes, even under
	// parallel-test-suite load. Shorter delays produce observed flakes
	// where goroutines serialize through the gate, each starting a new flight.
	f := &fakeLifecycle{kind: "mcp-language-server", materializeDelay: 200 * time.Millisecond}
	p, _ := newTestProxy(t, "mcp-language-server", f)
	h := p.Handler()
	var wg sync.WaitGroup
	codes := make([]int, 10)
	for i := range 10 {
		wg.Go(func() {
			rr := postRPC(t, h, "tools/call", i)
			codes[i] = rr.Code
		})
	}
	wg.Wait()
	if got := f.materializeCount.Load(); got != 1 {
		t.Errorf("materializeCount = %d under 10 concurrent tools/call, want 1", got)
	}
	for i, c := range codes {
		if c != http.StatusOK {
			t.Errorf("request[%d] code = %d, want 200", i, c)
		}
	}
}

func TestLazyProxy_MissingBinaryYieldsMissingState(t *testing.T) {
	missing := wrapMissing("not-a-real-binary-xyz-" + fmt.Sprint(time.Now().UnixNano()))
	if !IsMissingBinaryErr(missing) {
		t.Fatalf("sanity: wrapMissing must satisfy IsMissingBinaryErr (got err=%v)", missing)
	}
	f := &fakeLifecycle{kind: "mcp-language-server", materializeErr: missing}
	p, regPath := newTestProxy(t, "mcp-language-server", f)
	rr := postRPC(t, p.Handler(), "tools/call", 1)
	if rr.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200", rr.Code)
	}
	got := parseRPC(t, rr.Body.Bytes())
	if got["error"] == nil {
		t.Fatalf("expected JSON-RPC error envelope: %+v", got)
	}
	e := readEntry(t, regPath)
	if e.Lifecycle != api.LifecycleMissing {
		t.Errorf("lifecycle = %q, want %q", e.Lifecycle, api.LifecycleMissing)
	}
	if e.LastError == "" {
		t.Errorf("LastError should be stamped on missing")
	}
}

func TestLazyProxy_OtherFailureYieldsFailedState(t *testing.T) {
	boom := errors.New("handshake timeout")
	f := &fakeLifecycle{kind: "mcp-language-server", materializeErr: boom}
	p, regPath := newTestProxy(t, "mcp-language-server", f)
	rr := postRPC(t, p.Handler(), "tools/call", 1)
	if rr.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200", rr.Code)
	}
	got := parseRPC(t, rr.Body.Bytes())
	if got["error"] == nil {
		t.Fatalf("expected JSON-RPC error: %+v", got)
	}
	e := readEntry(t, regPath)
	if e.Lifecycle != api.LifecycleFailed {
		t.Errorf("lifecycle = %q, want %q", e.Lifecycle, api.LifecycleFailed)
	}
	if !strings.Contains(e.LastError, "handshake") {
		t.Errorf("LastError = %q, expected to contain 'handshake'", e.LastError)
	}
}

func TestLazyProxy_ThrottledRetryReturnsCachedError(t *testing.T) {
	f := &fakeLifecycle{kind: "mcp-language-server", materializeErr: errors.New("boom")}
	p, _ := newTestProxyWithCfg(t, "mcp-language-server", f, 2*time.Second, 100*time.Millisecond)
	h := p.Handler()
	if rr := postRPC(t, h, "tools/call", 1); rr.Code != http.StatusOK {
		t.Fatalf("first code=%d", rr.Code)
	}
	if rr := postRPC(t, h, "tools/call", 2); rr.Code != http.StatusOK {
		t.Fatalf("second code=%d", rr.Code)
	}
	if got := f.materializeCount.Load(); got != 1 {
		t.Errorf("materializeCount = %d after throttled retry, want 1", got)
	}
}

// TestLazyProxy_ClientCancelDoesNotTearDownBackend guards the narrow
// case where a client disconnects mid-request: SendRequest returns
// context.Canceled (driven by r.Context()), but the backend subprocess
// is still alive and healthy. The proxy must NOT call onSendFailure
// in that case — tearing down the backend over a client-side issue
// forces every other caller into an avoidable rematerialization and
// briefly marks lifecycle as Failed in the registry.
func TestLazyProxy_ClientCancelDoesNotTearDownBackend(t *testing.T) {
	// Fake returns context.Canceled (as if the downstream SendRequest
	// observed the client-side cancel propagating through its ctx).
	f := &fakeLifecycle{
		kind:           "mcp-language-server",
		sendRequestErr: context.Canceled,
	}
	p, regPath := newTestProxy(t, "mcp-language-server", f)
	h := p.Handler()

	// Use a pre-canceled context so the proxy sees ctx.Err() != nil on
	// the failed SendRequest — matches the isClientCancelErr branch.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	body := []byte(`{"jsonrpc":"2.0","id":1,"method":"tools/call"}`)
	req := httptest.NewRequestWithContext(ctx, http.MethodPost, "http://127.0.0.1/mcp", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	// The backend must NOT be stopped — client-cancel is not a crash.
	if got := f.stopCount.Load(); got != 0 {
		t.Errorf("client-cancel triggered backend Stop: count=%d (regression)", got)
	}
	// And the registry Lifecycle must still be Active (or whatever
	// materialization set), NOT Failed.
	e := readEntry(t, regPath)
	if e.Lifecycle == api.LifecycleFailed {
		t.Errorf("client-cancel flipped lifecycle to Failed (regression): %+v", e)
	}
}

// TestLazyProxy_ServeBeforeBindErrors guards the Bind/Serve split contract:
// callers that use the lock-aware two-step flow (acquire registry lock →
// Bind → release lock → Serve) must see a clear error if they try to Serve
// without a prior Bind. Prevents silent misuse where a caller forgets
// to Bind and a subsequent Serve no-ops or panics.
func TestLazyProxy_ServeBeforeBindErrors(t *testing.T) {
	f := &fakeLifecycle{kind: "mcp-language-server"}
	p, _ := newTestProxy(t, "mcp-language-server", f)
	err := p.Serve()
	if err == nil {
		t.Fatal("Serve without prior Bind must error")
	}
	if !strings.Contains(err.Error(), "not bound") && !strings.Contains(err.Error(), "Bind()") {
		t.Errorf("error should mention the missing Bind: %v", err)
	}
}

func TestLazyProxy_BackendCrashReMaterializes(t *testing.T) {
	// First tools/call materializes successfully but SendRequest errors out
	// mid-stream. Second call must see the evicted cache and re-materialize
	// once past the retry throttle.
	f := &fakeLifecycle{
		kind:           "mcp-language-server",
		sendRequestErr: errors.New("backend subprocess exited"),
	}
	p, regPath := newTestProxyWithCfg(t, "mcp-language-server", f, 10*time.Millisecond, 100*time.Millisecond)
	h := p.Handler()
	rr := postRPC(t, h, "tools/call", 1)
	if rr.Code != http.StatusOK {
		t.Fatalf("first code=%d body=%s", rr.Code, rr.Body.String())
	}
	got := parseRPC(t, rr.Body.Bytes())
	if got["error"] == nil {
		t.Fatalf("expected error envelope on send failure: %+v", got)
	}
	// Post-crash, pre-remat assertions. These guard the onSendFailure
	// ordering contract (Lifecycle.Stop MUST run before gate.Forget so
	// concurrent callers don't reuse the dead host). If Stop is skipped
	// or reordered, stopCount stays at zero and the host-tear-down race
	// silently regresses.
	if sc := f.stopCount.Load(); sc < 1 {
		t.Errorf("expected Lifecycle.Stop to be called after crash, stopCount=%d", sc)
	}
	crashEntry := readEntry(t, regPath)
	if crashEntry.Lifecycle != api.LifecycleFailed {
		t.Errorf("expected LifecycleFailed between crash and remat, got %q", crashEntry.Lifecycle)
	}
	// Flip fake so next SendRequest succeeds.
	f.sendRequestErr = nil
	// Wait past throttle so the gate re-runs Materialize.
	time.Sleep(30 * time.Millisecond)
	rr = postRPC(t, h, "tools/call", 2)
	if rr.Code != http.StatusOK {
		t.Fatalf("second code=%d body=%s", rr.Code, rr.Body.String())
	}
	got = parseRPC(t, rr.Body.Bytes())
	if got["error"] != nil {
		t.Fatalf("second call should succeed, got error: %+v", got)
	}
	if mc := f.materializeCount.Load(); mc != 2 {
		t.Errorf("materializeCount = %d, want 2 (first + remat after crash)", mc)
	}
}

func TestLazyProxy_LastToolsCallAtDebounced(t *testing.T) {
	// 5s debounce: 20 rapid successful tools/calls should advance
	// LastToolsCallAt exactly once (set on the first call).
	f := &fakeLifecycle{kind: "mcp-language-server"}
	p, regPath := newTestProxyWithCfg(t, "mcp-language-server", f, 10*time.Millisecond, 5*time.Second)
	h := p.Handler()
	if rr := postRPC(t, h, "tools/call", 0); rr.Code != http.StatusOK {
		t.Fatalf("first code=%d", rr.Code)
	}
	firstEntry := readEntry(t, regPath)
	firstStamp := firstEntry.LastToolsCallAt
	if firstStamp.IsZero() {
		t.Fatal("first tools/call did not stamp LastToolsCallAt")
	}
	for i := range 19 {
		if rr := postRPC(t, h, "tools/call", i+1); rr.Code != http.StatusOK {
			t.Fatalf("call[%d] code=%d", i+1, rr.Code)
		}
	}
	lastEntry := readEntry(t, regPath)
	if !lastEntry.LastToolsCallAt.Equal(firstStamp) {
		t.Errorf("LastToolsCallAt advanced despite debounce: first=%v last=%v",
			firstStamp, lastEntry.LastToolsCallAt)
	}
}

func TestLazyProxy_MaterializedHardCapRejectsWhenOtherBackendActive(t *testing.T) {
	f := &fakeLifecycle{kind: "mcp-language-server"}
	regPath := filepath.Join(t.TempDir(), "r.yaml")
	otherProxy, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen other proxy: %v", err)
	}
	t.Cleanup(func() { _ = otherProxy.Close() })
	otherPort := otherProxy.Addr().(*net.TCPAddr).Port
	seed := api.NewRegistry(regPath)
	seed.Put(api.WorkspaceEntry{
		WorkspaceKey:  "abcd1234",
		WorkspacePath: "D:/test/ws",
		Language:      "python",
		Backend:       "mcp-language-server",
		TaskName:      "mcp-local-hub-lsp-abcd1234-python",
		Port:          9200,
		Lifecycle:     api.LifecycleConfigured,
	})
	seed.Put(api.WorkspaceEntry{
		WorkspaceKey:  "efgh5678",
		WorkspacePath: "D:/test/other",
		Language:      "python",
		Backend:       "mcp-language-server",
		TaskName:      "mcp-local-hub-lsp-efgh5678-python",
		Port:          otherPort,
		Lifecycle:     api.LifecycleActive,
	})
	if err := seed.Save(); err != nil {
		t.Fatalf("seed registry: %v", err)
	}
	p := NewLazyProxy(LazyProxyConfig{
		WorkspaceKey:          "abcd1234",
		WorkspacePath:         "D:/test/ws",
		Language:              "python",
		BackendKind:           "mcp-language-server",
		Port:                  9200,
		Lifecycle:             f,
		RegistryPath:          regPath,
		InflightMinRetryGap:   20 * time.Millisecond,
		ToolsCallDebounce:     100 * time.Millisecond,
		MaterializedHardCap:   1,
		IdleBackendTTL:        0,
		IdleBackendCheckEvery: 0,
	})

	rr := postRPC(t, p.Handler(), "tools/call", 1)
	if rr.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "materialized LSP backend cap reached") {
		t.Fatalf("cap error mismatch: %s", rr.Body.String())
	}
	if got := f.materializeCount.Load(); got != 0 {
		t.Fatalf("materializeCount = %d, want 0 when cap is reached", got)
	}
}

func TestLazyProxy_MaterializedHardCapIgnoresStaleDeadRows(t *testing.T) {
	f := &fakeLifecycle{kind: "mcp-language-server"}
	regPath := filepath.Join(t.TempDir(), "r.yaml")
	seed := api.NewRegistry(regPath)
	seed.Put(api.WorkspaceEntry{
		WorkspaceKey:  "abcd1234",
		WorkspacePath: "D:/test/ws",
		Language:      "python",
		Backend:       "mcp-language-server",
		TaskName:      "mcp-local-hub-lsp-abcd1234-python",
		Port:          9200,
		Lifecycle:     api.LifecycleConfigured,
	})
	seed.Put(api.WorkspaceEntry{
		WorkspaceKey:  "dead0001",
		WorkspacePath: "D:/test/dead-active",
		Language:      "go",
		Backend:       "gopls-mcp",
		TaskName:      "mcp-local-hub-lsp-dead0001-go",
		Port:          1,
		Lifecycle:     api.LifecycleActive,
	})
	seed.Put(api.WorkspaceEntry{
		WorkspaceKey:  "dead0002",
		WorkspacePath: "D:/test/dead-starting",
		Language:      "rust",
		Backend:       "mcp-language-server",
		TaskName:      "mcp-local-hub-lsp-dead0002-rust",
		Port:          2,
		Lifecycle:     api.LifecycleStarting,
	})
	if err := seed.Save(); err != nil {
		t.Fatalf("seed registry: %v", err)
	}
	p := NewLazyProxy(LazyProxyConfig{
		WorkspaceKey:          "abcd1234",
		WorkspacePath:         "D:/test/ws",
		Language:              "python",
		BackendKind:           "mcp-language-server",
		Port:                  9200,
		Lifecycle:             f,
		RegistryPath:          regPath,
		InflightMinRetryGap:   20 * time.Millisecond,
		ToolsCallDebounce:     100 * time.Millisecond,
		MaterializedHardCap:   1,
		IdleBackendTTL:        0,
		IdleBackendCheckEvery: 0,
	})

	rr := postRPC(t, p.Handler(), "tools/call", 1)
	if rr.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200", rr.Code)
	}
	if strings.Contains(rr.Body.String(), "materialized LSP backend cap reached") {
		t.Fatalf("stale dead lifecycle rows exhausted cap: %s", rr.Body.String())
	}
	if got := f.materializeCount.Load(); got != 1 {
		t.Fatalf("materializeCount = %d, want 1 when only stale dead rows exist", got)
	}
}

func TestLazyProxy_DefaultMaterializedHardCapAllowsNineConcurrentWorkspaceProbes(t *testing.T) {
	// This test isolates the hard-cap (16) behavior. Force the port-live seam to
	// false so neither the hard cap nor the new cold-start-concurrency gate
	// counts any of the seeded rows as live — the seeded ports (9200+) are also
	// unbound in principle, but on a host running real mcp-local-hub LSP daemons
	// the real dialer would otherwise see them and the cold-start gate would
	// (correctly) throttle. Isolating via the seam keeps this hard-cap assertion
	// deterministic.
	origPortLive := materializedSlotPortLiveFn
	materializedSlotPortLiveFn = func(int) bool { return false }
	t.Cleanup(func() { materializedSlotPortLiveFn = origPortLive })

	regPath := filepath.Join(t.TempDir(), "r.yaml")
	seed := api.NewRegistry(regPath)
	languages := []string{
		"python",
		"go",
		"rust",
		"typescript",
		"javascript",
		"csharp",
		"java",
		"kotlin",
		"fortran",
	}
	for i, lang := range languages {
		key := fmt.Sprintf("ws%06d", i)
		seed.Put(api.WorkspaceEntry{
			WorkspaceKey:  key,
			WorkspacePath: "D:/test/ws",
			Language:      lang,
			Backend:       "mcp-language-server",
			TaskName:      "mcp-local-hub-lsp-" + key + "-" + lang,
			Port:          9200 + i,
			Lifecycle:     api.LifecycleConfigured,
		})
	}
	if err := seed.Save(); err != nil {
		t.Fatalf("seed registry: %v", err)
	}

	proxies := make([]*LazyProxy, 0, len(languages))
	for i, lang := range languages {
		key := fmt.Sprintf("ws%06d", i)
		proxies = append(proxies, NewLazyProxy(LazyProxyConfig{
			WorkspaceKey:        key,
			WorkspacePath:       "D:/test/ws",
			Language:            lang,
			BackendKind:         "mcp-language-server",
			Port:                9200 + i,
			Lifecycle:           &fakeLifecycle{kind: "mcp-language-server"},
			RegistryPath:        regPath,
			InflightMinRetryGap: 20 * time.Millisecond,
			ToolsCallDebounce:   100 * time.Millisecond,
			MaterializedHardCap: DefaultLSPMaterializedHardCap,
			IdleBackendTTL:      0,
		}))
	}

	errs := make(chan error, len(proxies))
	var wg sync.WaitGroup
	for _, proxy := range proxies {
		wg.Add(1)
		go func(p *LazyProxy) {
			defer wg.Done()
			_, err := p.ensureMaterialized(context.Background())
			errs <- err
		}(proxy)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("nine default-cap materializations should all succeed; got %v", err)
		}
	}
}

func TestLazyProxy_IdleTTLStopsMaterializedBackend(t *testing.T) {
	f := &fakeLifecycle{kind: "mcp-language-server"}
	p, regPath := newTestProxyWithCfg(t, "mcp-language-server", f, 10*time.Millisecond, 100*time.Millisecond)
	p.cfg.IdleBackendTTL = 30 * time.Millisecond
	p.cfg.IdleBackendCheckEvery = 10 * time.Millisecond
	p.startIdleReaper()

	if rr := postRPC(t, p.Handler(), "tools/call", 1); rr.Code != http.StatusOK {
		t.Fatalf("tools/call code=%d body=%s", rr.Code, rr.Body.String())
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if f.stopCount.Load() >= 1 {
			e := readEntry(t, regPath)
			if e.Lifecycle != api.LifecycleConfigured {
				t.Fatalf("lifecycle after idle reap = %q, want %q", e.Lifecycle, api.LifecycleConfigured)
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("idle reaper did not stop backend; stopCount=%d", f.stopCount.Load())
}

func TestLazyProxy_ShutdownStopsEndpoint(t *testing.T) {
	f := &fakeLifecycle{kind: "mcp-language-server"}
	p, _ := newTestProxy(t, "mcp-language-server", f)
	h := p.Handler()
	if rr := postRPC(t, h, "tools/call", 1); rr.Code != http.StatusOK {
		t.Fatalf("tools/call code=%d", rr.Code)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := p.Stop(ctx); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if got := f.stopCount.Load(); got != 1 {
		t.Errorf("Lifecycle.Stop called %d times, want 1", got)
	}
}

func TestLazyProxy_ConfiguredStateOnStartup(t *testing.T) {
	// Verifies the 5th reachable state: LifecycleConfigured written on
	// ListenAndServe startup before any client request arrives.
	f := &fakeLifecycle{kind: "mcp-language-server"}
	port, err := pickFreePort()
	if err != nil {
		t.Fatalf("pickFreePort: %v", err)
	}
	regPath := filepath.Join(t.TempDir(), "r.yaml")
	// Seed entry with empty Lifecycle; the test asserts the proxy writes
	// LifecycleConfigured on startup, upgrading the stored state.
	seed := api.NewRegistry(regPath)
	seed.Put(api.WorkspaceEntry{
		WorkspaceKey: "abcd1234", WorkspacePath: "D:/test/ws",
		Language: "python", Backend: "mcp-language-server",
		TaskName:  "mcp-local-hub-lsp-abcd1234-python",
		Lifecycle: "",
	})
	if err := seed.Save(); err != nil {
		t.Fatalf("seed registry: %v", err)
	}
	p := NewLazyProxy(LazyProxyConfig{
		WorkspaceKey:        "abcd1234",
		WorkspacePath:       "D:/test/ws",
		Language:            "python",
		BackendKind:         "mcp-language-server",
		Port:                port,
		Lifecycle:           f,
		RegistryPath:        regPath,
		InflightMinRetryGap: 20 * time.Millisecond,
		ToolsCallDebounce:   100 * time.Millisecond,
	})
	errCh := make(chan error, 1)
	go func() { errCh <- p.ListenAndServe() }()
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = p.Stop(ctx)
	}()
	// Wait for the registry write by polling.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		r := api.NewRegistry(regPath)
		if err := r.Load(); err == nil {
			if e, ok := r.Get("abcd1234", "python"); ok && e.Lifecycle == api.LifecycleConfigured {
				return // pass
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("registry never observed LifecycleConfigured")
}

// TestLazyProxy_ListenAndServeBindsLoopback verifies the proxy binds on
// 127.0.0.1:<port> and services real HTTP round-trips.
func TestLazyProxy_ListenAndServeBindsLoopback(t *testing.T) {
	f := &fakeLifecycle{kind: "mcp-language-server"}
	port, err := pickFreePort()
	if err != nil {
		t.Fatalf("pickFreePort: %v", err)
	}
	p := NewLazyProxy(LazyProxyConfig{
		WorkspaceKey:        "abcd1234",
		WorkspacePath:       "D:/test/ws",
		Language:            "python",
		BackendKind:         "mcp-language-server",
		Port:                port,
		Lifecycle:           f,
		RegistryPath:        filepath.Join(t.TempDir(), "r.yaml"),
		InflightMinRetryGap: 20 * time.Millisecond,
		ToolsCallDebounce:   100 * time.Millisecond,
	})
	go func() { _ = p.ListenAndServe() }()
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = p.Stop(ctx)
	}()
	client := &http.Client{Timeout: 500 * time.Millisecond}
	url := fmt.Sprintf("http://127.0.0.1:%d/mcp", port)
	deadline := time.Now().Add(2 * time.Second)
	var lastErr error
	for time.Now().Before(deadline) {
		body := `{"jsonrpc":"2.0","id":1,"method":"initialize"}`
		resp, err := client.Post(url, "application/json", strings.NewReader(body))
		if err == nil {
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			return
		}
		lastErr = err
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("proxy never accepted on port %d; last err=%v", port, lastErr)
}

// pickFreePort returns an ephemeral port that was open at the moment of
// asking — a minor race exists between release and the proxy's own Listen
// call, but 127.0.0.1:0 binding reuse on modern OS kernels is negligible
// for a short-lived test suite.
func pickFreePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	port := l.Addr().(*net.TCPAddr).Port
	_ = l.Close()
	return port, nil
}

// newRecordingProxy builds a proxy wired to a recordingLifecycle so tests can
// assert exactly which didOpen/didClose notifications reached upstream.
func newRecordingProxy(t *testing.T) (*LazyProxy, *recordingLifecycle) {
	t.Helper()
	f := &recordingLifecycle{}
	regPath := filepath.Join(t.TempDir(), "r.yaml")
	seed := api.NewRegistry(regPath)
	seed.Put(api.WorkspaceEntry{
		WorkspaceKey:  "abcd1234",
		WorkspacePath: "D:/test/ws",
		Language:      "python",
		Backend:       "mcp-language-server",
		TaskName:      "mcp-local-hub-lsp-abcd1234-python",
		Lifecycle:     "",
	})
	if err := seed.Save(); err != nil {
		t.Fatalf("seed registry: %v", err)
	}
	p := NewLazyProxy(LazyProxyConfig{
		WorkspaceKey:        "abcd1234",
		WorkspacePath:       "D:/test/ws",
		Language:            "python",
		BackendKind:         "mcp-language-server",
		Port:                0,
		Lifecycle:           f,
		RegistryPath:        regPath,
		InflightMinRetryGap: 10 * time.Millisecond,
		ToolsCallDebounce:   100 * time.Millisecond,
	})
	return p, f
}

// TestLazyProxy_DidOpenMultiOpenSingleClose is the core multi-agent guard:
// two agents open the SAME document, then both close it. Upstream must see
// exactly ONE didOpen (first open) and ONE didClose (last close) — never a
// duplicate open (LSP protocol violation) or a premature close that drops the
// document while the second agent still has it open.
func TestLazyProxy_DidOpenMultiOpenSingleClose(t *testing.T) {
	p, f := newRecordingProxy(t)
	h := p.Handler()
	const uri = "file:///ws/foo.go"

	// Agent A opens, then Agent B opens the same URI.
	if rr := postDocNotification(t, h, "textDocument/didOpen", uri); rr.Code != http.StatusAccepted {
		t.Fatalf("first didOpen code=%d, want 202; body=%s", rr.Code, rr.Body.String())
	}
	if rr := postDocNotification(t, h, "textDocument/didOpen", uri); rr.Code != http.StatusAccepted {
		t.Fatalf("second didOpen code=%d, want 202; body=%s", rr.Code, rr.Body.String())
	}
	// Both agents close.
	if rr := postDocNotification(t, h, "textDocument/didClose", uri); rr.Code != http.StatusAccepted {
		t.Fatalf("first didClose code=%d, want 202; body=%s", rr.Code, rr.Body.String())
	}
	if rr := postDocNotification(t, h, "textDocument/didClose", uri); rr.Code != http.StatusAccepted {
		t.Fatalf("second didClose code=%d, want 202; body=%s", rr.Code, rr.Body.String())
	}

	got := f.forwardedDocs()
	want := []forwardedDoc{
		{method: "textDocument/didOpen", uri: uri},
		{method: "textDocument/didClose", uri: uri},
	}
	if len(got) != len(want) {
		t.Fatalf("forwarded %d notifications, want %d: %+v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("forwarded[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}
}

// TestLazyProxy_DidCloseAfterLastCloseReopensCleanly verifies that once the
// refcount returns to zero, a subsequent didOpen for the same URI forwards
// again (the document was genuinely closed upstream, so the reopen is a real
// first-open).
func TestLazyProxy_DidCloseAfterLastCloseReopensCleanly(t *testing.T) {
	p, f := newRecordingProxy(t)
	h := p.Handler()
	const uri = "file:///ws/foo.go"

	postDocNotification(t, h, "textDocument/didOpen", uri)  // 0->1 forward
	postDocNotification(t, h, "textDocument/didClose", uri) // 1->0 forward
	postDocNotification(t, h, "textDocument/didOpen", uri)  // 0->1 forward again

	got := f.forwardedDocs()
	want := []forwardedDoc{
		{method: "textDocument/didOpen", uri: uri},
		{method: "textDocument/didClose", uri: uri},
		{method: "textDocument/didOpen", uri: uri},
	}
	if len(got) != len(want) {
		t.Fatalf("forwarded %d, want %d: %+v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("forwarded[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}
}

// TestLazyProxy_DidCloseSingleOpenMultiClose guards the inverse: ONE open
// followed by MULTIPLE closes (a buggy or duplicate-closing client) must
// forward exactly one didOpen and one didClose. The extra close must be
// absorbed and must NOT drive the refcount negative, because that would later
// suppress a legitimate didClose for a real second open.
func TestLazyProxy_DidCloseSingleOpenMultiClose(t *testing.T) {
	p, f := newRecordingProxy(t)
	h := p.Handler()
	const uri = "file:///ws/bar.py"

	postDocNotification(t, h, "textDocument/didOpen", uri)  // 0->1 forward
	postDocNotification(t, h, "textDocument/didClose", uri) // 1->0 forward
	// Spurious extra close — absorbed, count stays at 0 (not -1).
	if rr := postDocNotification(t, h, "textDocument/didClose", uri); rr.Code != http.StatusAccepted {
		t.Fatalf("extra didClose code=%d, want 202", rr.Code)
	}

	got := f.forwardedDocs()
	want := []forwardedDoc{
		{method: "textDocument/didOpen", uri: uri},
		{method: "textDocument/didClose", uri: uri},
	}
	if len(got) != len(want) {
		t.Fatalf("forwarded %d, want %d: %+v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("forwarded[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}

	// Prove the count did not go negative: a fresh open+close pair after the
	// spurious close must still forward both halves.
	postDocNotification(t, h, "textDocument/didOpen", uri)  // 0->1 forward
	postDocNotification(t, h, "textDocument/didClose", uri) // 1->0 forward
	got = f.forwardedDocs()
	if len(got) != 4 {
		t.Fatalf("after reopen/close, forwarded %d, want 4: %+v", len(got), got)
	}
	if got[2].method != "textDocument/didOpen" || got[3].method != "textDocument/didClose" {
		t.Errorf("reopen pair not forwarded after spurious close: %+v", got[2:])
	}
}

// TestLazyProxy_DidOpenDistinctURIsForwardIndependently verifies the gate is
// per-URI: opens for different documents each forward, and closing one does
// not affect the other's refcount.
func TestLazyProxy_DidOpenDistinctURIsForwardIndependently(t *testing.T) {
	p, f := newRecordingProxy(t)
	h := p.Handler()
	const uriA = "file:///ws/a.go"
	const uriB = "file:///ws/b.go"

	postDocNotification(t, h, "textDocument/didOpen", uriA)  // A 0->1 forward
	postDocNotification(t, h, "textDocument/didOpen", uriB)  // B 0->1 forward
	postDocNotification(t, h, "textDocument/didOpen", uriA)  // A 1->2 absorb
	postDocNotification(t, h, "textDocument/didClose", uriB) // B 1->0 forward

	got := f.forwardedDocs()
	want := []forwardedDoc{
		{method: "textDocument/didOpen", uri: uriA},
		{method: "textDocument/didOpen", uri: uriB},
		{method: "textDocument/didClose", uri: uriB},
	}
	if len(got) != len(want) {
		t.Fatalf("forwarded %d, want %d: %+v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("forwarded[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}
}

// TestLazyProxy_DidOpenMalformedURIAbsorbed verifies a didOpen/didClose with
// no params.textDocument.uri is absorbed (202, no forward) rather than sent
// upstream as an uncorrelatable request.
func TestLazyProxy_DidOpenMalformedURIAbsorbed(t *testing.T) {
	p, f := newRecordingProxy(t)
	h := p.Handler()

	// Empty params object — no textDocument.uri.
	body := `{"jsonrpc":"2.0","method":"textDocument/didOpen","params":{}}`
	req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1/mcp", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusAccepted {
		t.Fatalf("malformed didOpen code=%d, want 202; body=%s", rr.Code, rr.Body.String())
	}
	if got := f.forwardedDocs(); len(got) != 0 {
		t.Errorf("malformed didOpen forwarded upstream: %+v", got)
	}
	if mc := f.materializeCount.Load(); mc != 0 {
		t.Errorf("malformed didOpen materialized backend: count=%d", mc)
	}
}

func TestLazyProxy_DocRefsRejectUniqueURIsOverCap(t *testing.T) {
	p, f := newRecordingProxy(t)
	h := p.Handler()

	for i := 0; i < maxTrackedDocRefs; i++ {
		uri := fmt.Sprintf("file:///ws/%04d.go", i)
		if rr := postDocNotification(t, h, "textDocument/didOpen", uri); rr.Code != http.StatusAccepted {
			t.Fatalf("didOpen %d code=%d, want 202; body=%s", i, rr.Code, rr.Body.String())
		}
	}
	if got := len(f.forwardedDocs()); got != maxTrackedDocRefs {
		t.Fatalf("forwarded %d didOpen notifications, want %d", got, maxTrackedDocRefs)
	}

	if rr := postDocNotification(t, h, "textDocument/didOpen", "file:///ws/overflow.go"); rr.Code != http.StatusTooManyRequests {
		t.Fatalf("overflow didOpen code=%d, want 429; body=%s", rr.Code, rr.Body.String())
	}
	if got := len(f.forwardedDocs()); got != maxTrackedDocRefs {
		t.Fatalf("overflow didOpen forwarded unexpectedly; forwarded=%d want %d", got, maxTrackedDocRefs)
	}
}

func TestLazyProxy_DocRefRollsBackOnBackendJSONRPCError(t *testing.T) {
	f := &fakeLifecycle{
		kind:         "mcp-language-server",
		sendRPCError: &JSONRPCError{Code: -32602, Message: "invalid params"},
	}
	p, _ := newTestProxy(t, "mcp-language-server", f)
	h := p.Handler()
	const uri = "file:///ws/rejected.go"

	if rr := postDocNotification(t, h, "textDocument/didOpen", uri); rr.Code != http.StatusAccepted {
		t.Fatalf("first rejected didOpen code=%d, want 202; body=%s", rr.Code, rr.Body.String())
	}
	if rr := postDocNotification(t, h, "textDocument/didOpen", uri); rr.Code != http.StatusAccepted {
		t.Fatalf("second rejected didOpen code=%d, want 202; body=%s", rr.Code, rr.Body.String())
	}
	if got := f.sendCount.Load(); got != 2 {
		t.Fatalf("backend sends=%d, want 2 (JSON-RPC errors must not leave URI refcounted)", got)
	}
}

// TestLazyProxy_DocRefsResetOnBackendTeardown verifies the refcount map is
// cleared when the backend is torn down. After a backend crash (onSendFailure
// path) the next didOpen for a previously-open URI must forward again — the
// fresh backend has no open documents, so a retained count would silently
// suppress the reopen.
func TestLazyProxy_DocRefsResetOnBackendTeardown(t *testing.T) {
	p, f := newRecordingProxy(t)
	h := p.Handler()
	const uri = "file:///ws/foo.go"

	// Open the document (0->1 forward).
	postDocNotification(t, h, "textDocument/didOpen", uri)
	if got := f.forwardedDocs(); len(got) != 1 {
		t.Fatalf("setup didOpen not forwarded: %+v", got)
	}

	// Simulate a backend crash/teardown directly through the same path the
	// proxy uses on a mid-stream failure. This resets docRefs.
	p.onSendFailure(errors.New("backend subprocess exited"))

	// Wait past the retry throttle so the next call re-materializes.
	time.Sleep(20 * time.Millisecond)

	// The same URI opens again. Because the refcount was reset, this is a
	// genuine first-open against the fresh backend and MUST forward.
	if rr := postDocNotification(t, h, "textDocument/didOpen", uri); rr.Code != http.StatusAccepted {
		t.Fatalf("post-teardown didOpen code=%d, want 202; body=%s", rr.Code, rr.Body.String())
	}
	got := f.forwardedDocs()
	if len(got) != 2 {
		t.Fatalf("post-teardown didOpen not forwarded (stale refcount?): %+v", got)
	}
	if got[1].method != "textDocument/didOpen" || got[1].uri != uri {
		t.Errorf("post-teardown forward = %+v, want didOpen %s", got[1], uri)
	}
	if mc := f.materializeCount.Load(); mc != 2 {
		t.Errorf("materializeCount = %d, want 2 (initial + post-crash remat)", mc)
	}
}

// --- P2c cold-materialize: bounded wait, probation, cold-start slots ---------

// waitForCond polls a deterministic condition (e.g. a background goroutine
// reaching a known state) instead of sleeping a fixed duration.
func waitForCond(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("condition never met within deadline: %s", what)
}

func TestLazyProxy_ColdToolsCall_BoundedWaitReturns503RetryAfter(t *testing.T) {
	// Materialize itself is slower than the caller's budget → the bounded join
	// returns ErrMaterializeInFlight → the handler emits 503 + Retry-After: 15
	// while the materialize keeps running in the background.
	f := &fakeLifecycle{kind: "mcp-language-server", materializeDelay: 500 * time.Millisecond}
	p, regPath := newTestProxy(t, "mcp-language-server", f)
	p.cfg.MaterializeWaitBudget = 40 * time.Millisecond

	rr := postRPC(t, p.Handler(), "tools/call", 1)
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("code = %d, want 503; body=%s", rr.Code, rr.Body.String())
	}
	if ra := rr.Header().Get("Retry-After"); ra != "15" {
		t.Fatalf("Retry-After = %q, want 15", ra)
	}
	if !strings.Contains(rr.Body.String(), "cold start in progress") {
		t.Fatalf("body = %s, want cold-start-in-progress message", rr.Body.String())
	}
	// The row must stay Starting (NOT Failed) — the materialize is still running.
	e := readEntry(t, regPath)
	if e.Lifecycle != api.LifecycleStarting {
		t.Fatalf("lifecycle = %q, want Starting (in-flight is not a failure)", e.Lifecycle)
	}
	if e.LastError != "" {
		t.Fatalf("LastError = %q, want empty", e.LastError)
	}
}

func TestLazyProxy_ColdToolsCall_FastMaterializeAnswersInline(t *testing.T) {
	// Fast materialize + fast send within budget → answered inline (200),
	// warmed, LifecycleActive stamped on the first success.
	f := &fakeLifecycle{kind: "mcp-language-server"}
	p, regPath := newTestProxy(t, "mcp-language-server", f)
	p.cfg.MaterializeWaitBudget = 5 * time.Second

	rr := postRPC(t, p.Handler(), "tools/call", 1)
	if rr.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	if got := parseRPC(t, rr.Body.Bytes()); got["result"] == nil {
		t.Fatalf("no result: %+v", got)
	}
	if !p.isWarmed() {
		t.Fatal("proxy not warmed after first successful response")
	}
	e := readEntry(t, regPath)
	if e.Lifecycle != api.LifecycleActive {
		t.Fatalf("lifecycle = %q, want Active", e.Lifecycle)
	}
	if e.LastMaterializedAt.IsZero() {
		t.Fatal("LastMaterializedAt not stamped on first success")
	}
}

func TestLazyProxy_ConcurrentColdCallers_OneMaterialize_AllBoundedAtBudget(t *testing.T) {
	// N concurrent cold callers collapse to ONE materialize (singleflight) and,
	// because the materialize outlasts every caller's budget, ALL fast-fail 503.
	f := &fakeLifecycle{kind: "mcp-language-server", materializeDelay: 800 * time.Millisecond}
	p, _ := newTestProxy(t, "mcp-language-server", f)
	p.cfg.MaterializeWaitBudget = 50 * time.Millisecond
	h := p.Handler()

	const n = 8
	codes := make([]int, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Go(func() {
			rr := postRPC(t, h, "tools/call", i)
			codes[i] = rr.Code
		})
	}
	wg.Wait()
	if got := f.materializeCount.Load(); got != 1 {
		t.Errorf("materializeCount = %d under %d concurrent cold callers, want 1 (singleflight)", got, n)
	}
	for i, c := range codes {
		if c != http.StatusServiceUnavailable {
			t.Errorf("caller[%d] code = %d, want 503 (all bounded at budget)", i, c)
		}
	}
}

func TestLazyProxy_Probation_FirstRequestSlow_503Warming_ThenWarmServes200(t *testing.T) {
	// The FIRST send blocks (gopls indexing) until our probation budget fires →
	// 503 warming; the SECOND send (endpoint already cached) is fast → 200,
	// warmed, Active. The endpoint is materialized exactly once.
	f := &fakeLifecycle{kind: "mcp-language-server", firstSendGate: make(chan struct{})}
	p, regPath := newTestProxy(t, "mcp-language-server", f)
	p.cfg.MaterializeWaitBudget = 60 * time.Millisecond
	h := p.Handler()

	rr := postRPC(t, h, "tools/call", 1)
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("first (cold) code = %d, want 503; body=%s", rr.Code, rr.Body.String())
	}
	if ra := rr.Header().Get("Retry-After"); ra != "15" {
		t.Fatalf("Retry-After = %q, want 15", ra)
	}
	if p.isWarmed() {
		t.Fatal("warmed despite probation deadline (no successful response yet)")
	}
	if e := readEntry(t, regPath); e.Lifecycle != api.LifecycleStarting {
		t.Fatalf("lifecycle after probation 503 = %q, want Starting", e.Lifecycle)
	}
	if sc := f.stopCount.Load(); sc != 0 {
		t.Fatalf("probation 503 tore down backend: stopCount=%d", sc)
	}

	rr = postRPC(t, h, "tools/call", 2)
	if rr.Code != http.StatusOK {
		t.Fatalf("second (warm) code = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	if !p.isWarmed() {
		t.Fatal("not warmed after first successful response")
	}
	if e := readEntry(t, regPath); e.Lifecycle != api.LifecycleActive {
		t.Fatalf("lifecycle after warm success = %q, want Active", e.Lifecycle)
	}
	if mc := f.materializeCount.Load(); mc != 1 {
		t.Fatalf("materializeCount = %d, want 1 (endpoint cached across the retry)", mc)
	}
}

func TestLazyProxy_Probation_LifecycleActiveWrittenOnlyOnFirstSuccess(t *testing.T) {
	// While the first (blocked) send is in flight the row stays Starting; the
	// LifecycleActive write is deferred to the first SUCCESS.
	gate := make(chan struct{})
	f := &fakeLifecycle{kind: "mcp-language-server", firstSendGate: gate}
	p, regPath := newTestProxy(t, "mcp-language-server", f)
	p.cfg.MaterializeWaitBudget = 5 * time.Second // large: probation must not fire
	h := p.Handler()

	done := make(chan int, 1)
	go func() {
		rr := postRPC(t, h, "tools/call", 1)
		done <- rr.Code
	}()

	// Materialize succeeded and the (blocked) first send is in flight.
	waitForCond(t, "first send in flight", func() bool { return f.sendCount.Load() == 1 })
	if e := readEntry(t, regPath); e.Lifecycle != api.LifecycleStarting {
		t.Fatalf("lifecycle during probation = %q, want Starting (Active is deferred)", e.Lifecycle)
	}
	if p.isWarmed() {
		t.Fatal("warmed before any successful response")
	}

	close(gate) // let the first send complete
	if code := <-done; code != http.StatusOK {
		t.Fatalf("first success code = %d, want 200", code)
	}
	if !p.isWarmed() {
		t.Fatal("not warmed after first success")
	}
	if e := readEntry(t, regPath); e.Lifecycle != api.LifecycleActive {
		t.Fatalf("lifecycle after first success = %q, want Active", e.Lifecycle)
	}
}

func TestLazyProxy_Probation_DeadlineExceeded_NoBackendTeardown(t *testing.T) {
	// Our probation deadline must NOT tear the backend down: the endpoint stays
	// cached (for the retry), the row stays Starting, and Stop is not called.
	f := &fakeLifecycle{kind: "mcp-language-server", firstSendGate: make(chan struct{})}
	p, regPath := newTestProxy(t, "mcp-language-server", f)
	p.cfg.MaterializeWaitBudget = 40 * time.Millisecond

	rr := postRPC(t, p.Handler(), "tools/call", 1)
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("code = %d, want 503", rr.Code)
	}
	if sc := f.stopCount.Load(); sc != 0 {
		t.Fatalf("probation deadline tore down backend: stopCount=%d", sc)
	}
	p.mu.Lock()
	cached := p.endpoint
	p.mu.Unlock()
	if cached == nil {
		t.Fatal("endpoint evicted on probation deadline (should stay for the retry)")
	}
	if e := readEntry(t, regPath); e.Lifecycle != api.LifecycleStarting {
		t.Fatalf("lifecycle = %q, want Starting (probation deadline is not a failure)", e.Lifecycle)
	}
}

func TestLazyProxy_ProbationWatchdog_WedgedBackendTornDownAndSlotFreed(t *testing.T) {
	// A backend published long ago that never warmed is torn down by the
	// watchdog: endpoint closed, Lifecycle.Stop called, gate forgotten, and the
	// registry row leaves Starting (→ Failed) so its cold-start slot is freed.
	f := &fakeLifecycle{kind: "mcp-language-server"}
	p, regPath := newTestProxy(t, "mcp-language-server", f)
	p.cfg.ColdStartMaxProbation = 50 * time.Millisecond

	// Seed the row as Starting (the state a materialized-but-unwarmed backend
	// holds) so "slot freed" == "row no longer Starting" is observable.
	_ = api.NewRegistry(regPath).PutLifecycle("abcd1234", "python", api.LifecycleStarting, "")

	ep := &fakeEndpoint{parent: f}
	p.mu.Lock()
	p.endpoint = ep
	p.warmed = false
	p.endpointPublishedAt = time.Now().Add(-time.Minute)
	p.mu.Unlock()

	p.reapWedgedProbation(time.Now().UTC())

	if !ep.closed.Load() {
		t.Fatal("wedged endpoint not closed by probation watchdog")
	}
	if sc := f.stopCount.Load(); sc != 1 {
		t.Fatalf("Lifecycle.Stop count = %d, want 1", sc)
	}
	p.mu.Lock()
	stillCached := p.endpoint
	p.mu.Unlock()
	if stillCached != nil {
		t.Fatal("endpoint still cached after probation teardown")
	}
	e := readEntry(t, regPath)
	if e.Lifecycle != api.LifecycleFailed {
		t.Fatalf("lifecycle = %q, want Failed (slot freed)", e.Lifecycle)
	}
	if !strings.Contains(e.LastError, "never served a first response") {
		t.Fatalf("LastError = %q, want probation message", e.LastError)
	}
}

func TestLazyProxy_ColdStartSlots_BusyReturns503WithoutMaterialize(t *testing.T) {
	// Two OTHER Starting + port-live backends == ColdStartConcurrency → the proxy
	// refuses to enter the materialize path and emits 503 + Retry-After: 30.
	orig := materializedSlotPortLiveFn
	materializedSlotPortLiveFn = func(int) bool { return true }
	t.Cleanup(func() { materializedSlotPortLiveFn = orig })

	f := &fakeLifecycle{kind: "mcp-language-server"}
	regPath := filepath.Join(t.TempDir(), "r.yaml")
	seed := api.NewRegistry(regPath)
	seed.Put(api.WorkspaceEntry{WorkspaceKey: "abcd1234", WorkspacePath: "D:/test/ws", Language: "python", Backend: "mcp-language-server", TaskName: "mcp-local-hub-lsp-abcd1234-python", Port: 9200, Lifecycle: api.LifecycleConfigured})
	seed.Put(api.WorkspaceEntry{WorkspaceKey: "warm0001", WorkspacePath: "D:/test/w1", Language: "go", Backend: "gopls-mcp", TaskName: "mcp-local-hub-lsp-warm0001-go", Port: 9301, Lifecycle: api.LifecycleStarting})
	seed.Put(api.WorkspaceEntry{WorkspaceKey: "warm0002", WorkspacePath: "D:/test/w2", Language: "rust", Backend: "mcp-language-server", TaskName: "mcp-local-hub-lsp-warm0002-rust", Port: 9302, Lifecycle: api.LifecycleStarting})
	if err := seed.Save(); err != nil {
		t.Fatalf("seed registry: %v", err)
	}
	p := NewLazyProxy(LazyProxyConfig{
		WorkspaceKey: "abcd1234", WorkspacePath: "D:/test/ws", Language: "python",
		BackendKind: "mcp-language-server", Port: 9200, Lifecycle: f, RegistryPath: regPath,
		InflightMinRetryGap: 20 * time.Millisecond, ToolsCallDebounce: 100 * time.Millisecond,
		ColdStartConcurrency: 2,
	})

	rr := postRPC(t, p.Handler(), "tools/call", 1)
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("code = %d, want 503; body=%s", rr.Code, rr.Body.String())
	}
	if ra := rr.Header().Get("Retry-After"); ra != "30" {
		t.Fatalf("Retry-After = %q, want 30", ra)
	}
	if !strings.Contains(rr.Body.String(), "cold-start slots busy") {
		t.Fatalf("body = %s, want slots-busy message", rr.Body.String())
	}
	if got := f.materializeCount.Load(); got != 0 {
		t.Fatalf("materializeCount = %d, want 0 (refused before materialize)", got)
	}
}

func TestLazyProxy_ColdStartSlots_StalePortDeadStartingRowIgnored(t *testing.T) {
	// Two OTHER Starting rows whose ports are DEAD (crashed daemons) must NOT
	// count against the cold-start budget → the proxy materializes normally.
	orig := materializedSlotPortLiveFn
	materializedSlotPortLiveFn = func(int) bool { return false }
	t.Cleanup(func() { materializedSlotPortLiveFn = orig })

	f := &fakeLifecycle{kind: "mcp-language-server"}
	regPath := filepath.Join(t.TempDir(), "r.yaml")
	seed := api.NewRegistry(regPath)
	seed.Put(api.WorkspaceEntry{WorkspaceKey: "abcd1234", WorkspacePath: "D:/test/ws", Language: "python", Backend: "mcp-language-server", TaskName: "mcp-local-hub-lsp-abcd1234-python", Port: 9200, Lifecycle: api.LifecycleConfigured})
	seed.Put(api.WorkspaceEntry{WorkspaceKey: "dead0001", WorkspacePath: "D:/test/d1", Language: "go", Backend: "gopls-mcp", TaskName: "mcp-local-hub-lsp-dead0001-go", Port: 9301, Lifecycle: api.LifecycleStarting})
	seed.Put(api.WorkspaceEntry{WorkspaceKey: "dead0002", WorkspacePath: "D:/test/d2", Language: "rust", Backend: "mcp-language-server", TaskName: "mcp-local-hub-lsp-dead0002-rust", Port: 9302, Lifecycle: api.LifecycleStarting})
	if err := seed.Save(); err != nil {
		t.Fatalf("seed registry: %v", err)
	}
	p := NewLazyProxy(LazyProxyConfig{
		WorkspaceKey: "abcd1234", WorkspacePath: "D:/test/ws", Language: "python",
		BackendKind: "mcp-language-server", Port: 9200, Lifecycle: f, RegistryPath: regPath,
		InflightMinRetryGap: 20 * time.Millisecond, ToolsCallDebounce: 100 * time.Millisecond,
		ColdStartConcurrency: 2,
	})

	rr := postRPC(t, p.Handler(), "tools/call", 1)
	if rr.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	if got := f.materializeCount.Load(); got != 1 {
		t.Fatalf("materializeCount = %d, want 1 (dead Starting rows ignored)", got)
	}
}

func TestLazyProxy_MissingLSPBinary_InstantMissingError(t *testing.T) {
	// A missing binary is classified instantly by the LookPath preflight — the
	// bounded-wait budget must NOT delay or mask it.
	missing := wrapMissing("not-a-real-binary-xyz-" + fmt.Sprint(time.Now().UnixNano()))
	if !IsMissingBinaryErr(missing) {
		t.Fatalf("sanity: wrapMissing must satisfy IsMissingBinaryErr (got err=%v)", missing)
	}
	f := &fakeLifecycle{kind: "mcp-language-server", materializeErr: missing}
	p, regPath := newTestProxy(t, "mcp-language-server", f)
	p.cfg.MaterializeWaitBudget = 5 * time.Second

	start := time.Now()
	rr := postRPC(t, p.Handler(), "tools/call", 1)
	elapsed := time.Since(start)
	if rr.Code != http.StatusOK {
		t.Fatalf("missing-binary code = %d, want 200 JSON-RPC error", rr.Code)
	}
	if got := parseRPC(t, rr.Body.Bytes()); got["error"] == nil {
		t.Fatalf("expected JSON-RPC error: %+v", parseRPC(t, rr.Body.Bytes()))
	}
	if elapsed > time.Second {
		t.Fatalf("missing-binary path took %v, want near-instant (not budget-bound)", elapsed)
	}
	e := readEntry(t, regPath)
	if e.Lifecycle != api.LifecycleMissing {
		t.Fatalf("lifecycle = %q, want Missing", e.Lifecycle)
	}
}

func TestLazyProxy_WarmGoesCold_ReentersBoundedColdPath(t *testing.T) {
	// After warming, a mid-session backend crash resets warmed and the next call
	// re-enters the cold path (re-materialize + re-warm).
	f := &fakeLifecycle{kind: "mcp-language-server"}
	p, regPath := newTestProxyWithCfg(t, "mcp-language-server", f, 10*time.Millisecond, 100*time.Millisecond)
	p.cfg.MaterializeWaitBudget = 5 * time.Second
	h := p.Handler()

	if rr := postRPC(t, h, "tools/call", 1); rr.Code != http.StatusOK {
		t.Fatalf("first code=%d", rr.Code)
	}
	if !p.isWarmed() {
		t.Fatal("not warmed after first success")
	}

	// Backend goes cold mid-session (crash teardown) — warmed must reset.
	p.onSendFailure(errors.New("backend subprocess exited"))
	if p.isWarmed() {
		t.Fatal("warmed flag not reset on teardown")
	}
	if e := readEntry(t, regPath); e.Lifecycle != api.LifecycleFailed {
		t.Fatalf("lifecycle after teardown = %q, want Failed", e.Lifecycle)
	}

	// Next call re-enters the cold path and re-materializes (past the throttle).
	time.Sleep(20 * time.Millisecond)
	if rr := postRPC(t, h, "tools/call", 2); rr.Code != http.StatusOK {
		t.Fatalf("re-entered code=%d body=%s", rr.Code, rr.Body.String())
	}
	if !p.isWarmed() {
		t.Fatal("not warmed after re-materialize success")
	}
	if mc := f.materializeCount.Load(); mc != 2 {
		t.Fatalf("materializeCount = %d, want 2 (initial + re-materialize)", mc)
	}
}
