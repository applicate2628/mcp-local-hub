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
	"os"
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
	return &fakeEndpoint{parent: f, closeCh: make(chan struct{})}, nil
}

func (f *fakeLifecycle) Stop() error { f.stopCount.Add(1); return nil }

type fakeEndpoint struct {
	parent *fakeLifecycle
	closed atomic.Bool
	// closeCh (when non-nil) is closed by Close() and unblocks a SendRequest that
	// is gated on firstSendGate with a "backend host stopped" error — modeling a
	// probation reap (ep.Close + Lifecycle.Stop) severing an in-flight delivered
	// request, so the F1 reap-sever path can be exercised deterministically. Nil for
	// endpoints constructed directly (never Close()d mid-send).
	closeCh   chan struct{}
	closeOnce sync.Once
}

func (e *fakeEndpoint) SendRequest(ctx context.Context, req *JSONRPCRequest) (*JSONRPCResponse, error) {
	if e.closed.Load() {
		return nil, errors.New("endpoint closed")
	}
	n := e.parent.sendCount.Add(1)
	if e.parent.firstSendGate != nil && n == 1 {
		select {
		case <-e.parent.firstSendGate:
		case <-e.closeCh: // nil channel never fires; closed by Close() to sever the send
			return nil, errors.New("backend host stopped")
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

func (e *fakeEndpoint) Close() error {
	e.closed.Store(true)
	if e.closeCh != nil {
		e.closeOnce.Do(func() { close(e.closeCh) })
	}
	return nil
}

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
	// Stop the proxy at cleanup (idempotent — tests that Stop themselves are
	// unaffected). NewLazyProxy starts the probation-watchdog ticker by default;
	// without this, every test leaked a live 1-minute ticker whose later fire
	// (the -race package run exceeds 60s) did registry IO against deleted
	// TempDirs — the recurring "TempDir RemoveAll: directory not empty /
	// r.yaml.lock in use" flake class.
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = p.Stop(ctx)
	})
	return p, regPath
}

// stopProxyOnCleanup registers a bounded idempotent Stop for an ad-hoc test
// proxy — same leaked-watchdog-ticker rationale as the newTestProxyWithCfg
// cleanup (see comment there).
func stopProxyOnCleanup(t *testing.T, p *LazyProxy) {
	t.Helper()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = p.Stop(ctx)
	})
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

// currentGen reads the proxy's current endpoint generation under p.mu — used by
// tests that call the now-gen-guarded onSendFailure(gen, err) directly (F4).
func currentGen(p *LazyProxy) uint64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.endpointGeneration
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

	ep, _, err := p.ensureMaterialized(context.Background())
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

	first, _, err := p.ensureMaterialized(context.Background())
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
		ep, _, err := p.ensureMaterialized(context.Background())
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

	first, _, err := p.ensureMaterialized(context.Background())
	if err != nil {
		t.Fatalf("initial ensureMaterialized: %v", err)
	}
	p.endBackendRequest()

	failureDone := make(chan struct{})
	go func() {
		p.onSendFailure(currentGen(p), errors.New("backend subprocess exited"))
		close(failureDone)
	}()
	<-f.stopStarted

	type materializedResult struct {
		ep  MCPEndpoint
		err error
	}
	result := make(chan materializedResult, 1)
	go func() {
		ep, _, err := p.ensureMaterialized(context.Background())
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
		ep, _, err := p.ensureMaterialized(context.Background())
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
	// Settle: the abandoned caller left a DETACHED materialize running; wait for
	// its publish (whose reconcile registry write completes before p.endpoint
	// becomes observable under p.mu) so no registry file handle is open in the
	// TempDir when the test's cleanup removes it (Windows: "directory not empty").
	waitForCond(t, "detached materialize published", func() bool {
		p.mu.Lock()
		defer p.mu.Unlock()
		return p.endpoint != nil
	})
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
	stopProxyOnCleanup(t, p)

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
	stopProxyOnCleanup(t, p)

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
		stopProxyOnCleanup(t, proxies[len(proxies)-1])
	}

	errs := make(chan error, len(proxies))
	var wg sync.WaitGroup
	for _, proxy := range proxies {
		wg.Add(1)
		go func(p *LazyProxy) {
			defer wg.Done()
			_, _, err := p.ensureMaterialized(context.Background())
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
	// The idle config must be set on the LazyProxyConfig BEFORE NewLazyProxy so
	// the background ticker (which NewLazyProxy now starts unconditionally when
	// the probation watchdog is configured, F4) starts with the fast test
	// interval. Reconfiguring p.cfg after construction no longer works because
	// idleStartOnce is already consumed by the watchdog-only ticker.
	f := &fakeLifecycle{kind: "mcp-language-server"}
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
		WorkspaceKey:          "abcd1234",
		WorkspacePath:         "D:/test/ws",
		Language:              "python",
		BackendKind:           "mcp-language-server",
		Port:                  0,
		Lifecycle:             f,
		RegistryPath:          regPath,
		InflightMinRetryGap:   10 * time.Millisecond,
		ToolsCallDebounce:     100 * time.Millisecond,
		IdleBackendTTL:        30 * time.Millisecond,
		IdleBackendCheckEvery: 10 * time.Millisecond,
	})
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = p.Stop(ctx)
	})

	if rr := postRPC(t, p.Handler(), "tools/call", 1); rr.Code != http.StatusOK {
		t.Fatalf("tools/call code=%d body=%s", rr.Code, rr.Body.String())
	}

	// Poll for BOTH the lifecycle stop AND the registry write. The reaper's
	// order is Stop() THEN PutLifecycle(Configured) (correct: don't claim
	// Configured until the backend is actually stopped), so observing
	// stopCount>=1 does NOT yet imply the registry row flipped — reading it
	// immediately raced the reaper's write and flaked under -race load.
	deadline := time.Now().Add(2 * time.Second)
	lastLifecycle := "<never-read>"
	for time.Now().Before(deadline) {
		if f.stopCount.Load() >= 1 {
			lastLifecycle = readEntry(t, regPath).Lifecycle
			if lastLifecycle == api.LifecycleConfigured {
				return
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	if f.stopCount.Load() >= 1 {
		t.Fatalf("lifecycle after idle reap = %q, want %q (registry write never landed)", lastLifecycle, api.LifecycleConfigured)
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
	stopProxyOnCleanup(t, p)
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
	p.onSendFailure(currentGen(p), errors.New("backend subprocess exited"))

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
// reaching a known state) instead of sleeping a fixed duration. The deadline
// only bounds FAILURE reporting — a passing condition returns as soon as it
// holds — so it is deliberately generous: a 2s deadline was observed starving
// under loaded -race package runs (a 50ms AfterFunc took >2s to get scheduled),
// flaking tests that are otherwise deterministic.
func waitForCond(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
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
	// Settle: wait for the detached materialize (500ms) to publish so its registry
	// write is not racing the TempDir cleanup (Windows: "directory not empty").
	waitForCond(t, "detached materialize published", func() bool {
		p.mu.Lock()
		defer p.mu.Unlock()
		return p.endpoint != nil
	})
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
	// Settle: wait for the detached materialize (800ms) to publish so its registry
	// write is not racing the TempDir cleanup (Windows: "directory not empty").
	waitForCond(t, "detached materialize published", func() bool {
		p.mu.Lock()
		defer p.mu.Unlock()
		return p.endpoint != nil
	})
}

func TestLazyProxy_ColdToolsCall_AwaitsDeliveredResponse_ThenWarmActive(t *testing.T) {
	// Predicate 1: a cold tools/call is a REQUEST — it is AWAITED (not 503-ed after
	// delivery) under the request-hold ceiling. While the delivered first send is in
	// flight the row is Starting + !warmed; when the backend finishes indexing the
	// SAME call returns 200 with the real body (sendCount==1, NOT re-delivered), and
	// warms → Active. No 503, no duplicate delivery, no teardown.
	f := &fakeLifecycle{kind: "mcp-language-server", firstSendGate: make(chan struct{})}
	p, regPath := newTestProxy(t, "mcp-language-server", f)
	h := p.Handler()

	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- postRPC(t, h, "tools/call", 1) }()

	// The delivered first send is in flight (blocked on the indexing gate).
	waitForCond(t, "first send delivered + in flight", func() bool { return f.sendCount.Load() == 1 })
	if e := readEntry(t, regPath); e.Lifecycle != api.LifecycleStarting {
		t.Fatalf("lifecycle while awaiting = %q, want Starting", e.Lifecycle)
	}
	if p.isWarmed() {
		t.Fatal("warmed before any successful response")
	}

	close(f.firstSendGate) // backend finishes indexing → the awaited real response
	rr := <-done
	if rr.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200 (awaited real result, NO 503); body=%s", rr.Code, rr.Body.String())
	}
	if got := parseRPC(t, rr.Body.Bytes()); got["result"] == nil {
		t.Fatalf("no real result body: %+v", got)
	}
	if got := f.sendCount.Load(); got != 1 {
		t.Fatalf("sendCount = %d, want 1 (delivered exactly once — no drop-and-retry)", got)
	}
	if !p.isWarmed() {
		t.Fatal("not warmed after the awaited success")
	}
	if e := readEntry(t, regPath); e.Lifecycle != api.LifecycleActive {
		t.Fatalf("lifecycle after success = %q, want Active", e.Lifecycle)
	}
	if sc := f.stopCount.Load(); sc != 0 {
		t.Fatalf("await tore down backend: stopCount=%d", sc)
	}
	if mc := f.materializeCount.Load(); mc != 1 {
		t.Fatalf("materializeCount = %d, want 1", mc)
	}
}

func TestLazyProxy_ColdToolsCall_CeilingExceeded_NonRetryableControlledError(t *testing.T) {
	// Predicate 1 (reliability #2): a delivered request that exceeds the request-hold
	// ceiling returns a NON-retryable controlled error — HTTP 500, NO Retry-After, NO
	// "retry in" wording, sendCount==1 (delivered once, NOT re-sent), no teardown, row
	// stays Starting (the backend is healthy, just slow).
	f := &fakeLifecycle{kind: "mcp-language-server", firstSendGate: make(chan struct{})}
	p, regPath := newTestProxy(t, "mcp-language-server", f)
	// Short ceiling so it fires fast. Set post-construction so NewLazyProxy's
	// ordering clamp (which ran with the defaults) does not bump it back up.
	p.cfg.ColdRequestHoldCeiling = 40 * time.Millisecond
	h := p.Handler()

	rr := postRPC(t, h, "tools/call", 1) // gate never closes → ceiling fires at 40ms
	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("code = %d, want 500 (controlled non-retryable); body=%s", rr.Code, rr.Body.String())
	}
	if ra := rr.Header().Get("Retry-After"); ra != "" {
		t.Fatalf("Retry-After = %q, want empty (must NOT be auto-retryable)", ra)
	}
	body := rr.Body.String()
	if strings.Contains(body, "retry in") {
		t.Fatalf("controlled error contains a retry hint (would trigger auto-retry): %s", body)
	}
	if !strings.Contains(body, "do not auto-retry") {
		t.Fatalf("controlled error missing the do-not-retry contract: %s", body)
	}
	if got := f.sendCount.Load(); got != 1 {
		t.Fatalf("sendCount = %d, want 1 (delivered once, not re-sent)", got)
	}
	if sc := f.stopCount.Load(); sc != 0 {
		t.Fatalf("ceiling expiry tore down backend: stopCount=%d", sc)
	}
	if e := readEntry(t, regPath); e.Lifecycle != api.LifecycleStarting {
		t.Fatalf("lifecycle after ceiling = %q, want Starting (delivered, not a failure)", e.Lifecycle)
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

func TestLazyProxy_ColdToolsCall_CeilingExceeded_EndpointStaysCachedForRetry(t *testing.T) {
	// The request-hold-ceiling controlled error must NOT tear the backend down: the
	// endpoint stays cached, the row stays Starting, and a retry (once the backend is
	// warm) hits the cache rather than re-materializing.
	f := &fakeLifecycle{kind: "mcp-language-server", firstSendGate: make(chan struct{})}
	p, regPath := newTestProxy(t, "mcp-language-server", f)
	p.cfg.ColdRequestHoldCeiling = 40 * time.Millisecond
	h := p.Handler()

	rr := postRPC(t, h, "tools/call", 1) // ceiling fires at 40ms → 500 controlled error
	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("code = %d, want 500 controlled error; body=%s", rr.Code, rr.Body.String())
	}
	if sc := f.stopCount.Load(); sc != 0 {
		t.Fatalf("ceiling deadline tore down backend: stopCount=%d", sc)
	}
	p.mu.Lock()
	cached := p.endpoint
	p.mu.Unlock()
	if cached == nil {
		t.Fatal("endpoint evicted on the ceiling (should stay cached for the retry)")
	}
	if e := readEntry(t, regPath); e.Lifecycle != api.LifecycleStarting {
		t.Fatalf("lifecycle = %q, want Starting (ceiling is not a failure)", e.Lifecycle)
	}

	// A retry hits the cached endpoint (the gate only blocks the FIRST send, now
	// consumed) and warms → 200 + Active, exactly one materialize.
	rr = postRPC(t, h, "tools/call", 2)
	if rr.Code != http.StatusOK {
		t.Fatalf("warm retry code = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	if mc := f.materializeCount.Load(); mc != 1 {
		t.Fatalf("materializeCount = %d, want 1 (endpoint cached across the ceiling)", mc)
	}
	if e := readEntry(t, regPath); e.Lifecycle != api.LifecycleActive {
		t.Fatalf("lifecycle after warm retry = %q, want Active", e.Lifecycle)
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
	stopProxyOnCleanup(t, p)

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
	stopProxyOnCleanup(t, p)

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
	p.onSendFailure(currentGen(p), errors.New("backend subprocess exited"))
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

// --- P2c commission fixes: F1 abandon-publish, F2 doc-lifecycle probation, ----
// --- F3 own-flight join, F5 bounded Stop, F6a generation guard ----------------

// TestLazyProxy_AbandonedMaterialize_StillPublishesAndLeavesStarting is the
// primary F1 guard: the only cold caller abandons on budget expiry (503), yet
// the detached materialize must STILL publish the endpoint (no leak), so a later
// call hits the cache, warms, and the row can leave Starting → Active.
func TestLazyProxy_AbandonedMaterialize_StillPublishesAndLeavesStarting(t *testing.T) {
	f := &fakeLifecycle{kind: "mcp-language-server", materializeDelay: 150 * time.Millisecond}
	p, regPath := newTestProxy(t, "mcp-language-server", f)
	p.cfg.MaterializeWaitBudget = 20 * time.Millisecond
	h := p.Handler()

	// Caller abandons at 20ms while the 150ms materialize is still running.
	rr := postRPC(t, h, "tools/call", 1)
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("cold code = %d, want 503; body=%s", rr.Code, rr.Body.String())
	}
	if e := readEntry(t, regPath); e.Lifecycle != api.LifecycleStarting {
		t.Fatalf("lifecycle at abandonment = %q, want Starting", e.Lifecycle)
	}

	// F1: the detached materialize must publish the endpoint even though no
	// caller was left waiting.
	waitForCond(t, "endpoint published by detached materialize", func() bool {
		p.mu.Lock()
		defer p.mu.Unlock()
		return p.endpoint != nil
	})
	if got := f.materializeCount.Load(); got != 1 {
		t.Fatalf("materializeCount = %d, want 1", got)
	}

	// A later call hits the published endpoint (no re-materialize), warms, and
	// the row leaves Starting → Active.
	rr = postRPC(t, h, "tools/call", 2)
	if rr.Code != http.StatusOK {
		t.Fatalf("warm code = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	if got := f.materializeCount.Load(); got != 1 {
		t.Fatalf("materializeCount = %d after warm call, want 1 (endpoint was published, not leaked)", got)
	}
	if e := readEntry(t, regPath); e.Lifecycle != api.LifecycleActive {
		t.Fatalf("lifecycle after warm = %q, want Active", e.Lifecycle)
	}
}

// TestLazyProxy_AbandonedMaterialize_FailureStampsFailed is the F1 failure twin:
// the only caller abandons on budget expiry, then the detached materialize FAILS
// — the fn must stamp Failed so the row does not stay Starting forever.
func TestLazyProxy_AbandonedMaterialize_FailureStampsFailed(t *testing.T) {
	boom := errors.New("handshake timeout while indexing")
	f := &fakeLifecycle{kind: "mcp-language-server", materializeErr: boom, materializeDelay: 150 * time.Millisecond}
	p, regPath := newTestProxy(t, "mcp-language-server", f)
	p.cfg.MaterializeWaitBudget = 20 * time.Millisecond

	rr := postRPC(t, p.Handler(), "tools/call", 1)
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("cold code = %d, want 503; body=%s", rr.Code, rr.Body.String())
	}
	// The detached fn fails at ~150ms and MUST stamp Failed with no waiter present.
	waitForCond(t, "detached failure stamps Failed", func() bool {
		return readEntry(t, regPath).Lifecycle == api.LifecycleFailed
	})
	if le := readEntry(t, regPath).LastError; !strings.Contains(le, "handshake timeout") {
		t.Fatalf("LastError = %q, want the materialize error", le)
	}
	// Settle: the Failed observation proves the fn RAN; wait for its flight to
	// fully exit (deferred activeFlights decrement runs after the fn's registry
	// write + flock unlock returned) so no handle races the TempDir cleanup.
	waitForCond(t, "detached flight exited", func() bool {
		return !p.gate.HasActiveFlight(p.inflightKey())
	})
}

// TestLazyProxy_ProbationWatchdog_ReapsOrphanStartingRowWithNoEndpoint is the F1
// belt-and-braces branch: a Starting row with NO endpoint published and no flight
// running is torn down + stamped Failed so its cold-start slot frees.
func TestLazyProxy_ProbationWatchdog_ReapsOrphanStartingRowWithNoEndpoint(t *testing.T) {
	f := &fakeLifecycle{kind: "mcp-language-server"}
	p, regPath := newTestProxy(t, "mcp-language-server", f)
	p.cfg.ColdStartMaxProbation = 50 * time.Millisecond

	// Orphan: row Starting, no endpoint, startingSince long ago, no flight.
	_ = api.NewRegistry(regPath).PutLifecycle("abcd1234", "python", api.LifecycleStarting, "")
	p.mu.Lock()
	p.endpoint = nil
	p.startingSince = time.Now().Add(-time.Minute)
	p.mu.Unlock()

	p.reapWedgedProbation(time.Now().UTC())

	e := readEntry(t, regPath)
	if e.Lifecycle != api.LifecycleFailed {
		t.Fatalf("orphan Starting row not reaped: lifecycle = %q, want Failed", e.Lifecycle)
	}
	if got := f.stopCount.Load(); got != 1 {
		t.Fatalf("Lifecycle.Stop count = %d, want 1 (best-effort teardown)", got)
	}
}

// TestLazyProxy_DocLifecycle_ProbationDeadlineDeliversOnceNoReplay is the
// Finding 1 guard: a didOpen whose (first) send blocks past our probation budget
// was ALREADY written to the backend before the deadline fired (writeStdin
// precedes the response wait). Treating it as delivered (202, refcount KEPT)
// rather than 503+rollback prevents the client from re-forwarding a duplicate
// didOpen on retry. No teardown, warmed stays false (no response yet).
func TestLazyProxy_DocLifecycle_ProbationDeadlineDeliversOnceNoReplay(t *testing.T) {
	f := &fakeLifecycle{kind: "mcp-language-server", firstSendGate: make(chan struct{})}
	p, _ := newTestProxy(t, "mcp-language-server", f)
	p.cfg.MaterializeWaitBudget = 40 * time.Millisecond
	h := p.Handler()
	const uri = "file:///ws/foo.go"

	// Cold didOpen: the send is written to the backend, then blocks on the gate
	// until our budget fires. The notification is delivered → 202, NOT 503.
	rr := postDocNotification(t, h, "textDocument/didOpen", uri)
	if rr.Code != http.StatusAccepted {
		t.Fatalf("cold didOpen code = %d, want 202 (delivered once, no replay); body=%s", rr.Code, rr.Body.String())
	}
	if got := f.sendCount.Load(); got != 1 {
		t.Fatalf("sendCount = %d, want 1 (notification written exactly once)", got)
	}
	if p.isWarmed() {
		t.Fatal("warmed set despite no backend response (only a deadline)")
	}
	if sc := f.stopCount.Load(); sc != 0 {
		t.Fatalf("probation deadline tore down backend: stopCount=%d", sc)
	}

	// The refcount was NOT rolled back, so a genuine second open of the same URI
	// is ABSORBED (1->2) and never re-forwarded — proving no duplicate replay. If
	// the first open had been rolled back (old bug), this would be a 0->1 forward
	// and reach the backend (sendCount would climb).
	rr = postDocNotification(t, h, "textDocument/didOpen", uri)
	if rr.Code != http.StatusAccepted {
		t.Fatalf("duplicate didOpen code = %d, want 202", rr.Code)
	}
	if got := f.sendCount.Load(); got != 1 {
		t.Fatalf("sendCount = %d after duplicate didOpen, want 1 (first open's refcount must be retained — no replay)", got)
	}
}

// TestLazyProxy_DocLifecycle_WarmForwardsOnceAndSetsWarmed confirms a genuinely
// fast (non-probation) doc-lifecycle forward still works: it reaches the backend
// exactly once and warms the proxy.
func TestLazyProxy_DocLifecycle_WarmForwardsOnceAndSetsWarmed(t *testing.T) {
	f := &fakeLifecycle{kind: "mcp-language-server"} // no gate → instant response
	p, _ := newTestProxy(t, "mcp-language-server", f)
	h := p.Handler()
	const uri = "file:///ws/foo.go"

	rr := postDocNotification(t, h, "textDocument/didOpen", uri)
	if rr.Code != http.StatusAccepted {
		t.Fatalf("didOpen code = %d, want 202; body=%s", rr.Code, rr.Body.String())
	}
	if got := f.sendCount.Load(); got != 1 {
		t.Fatalf("sendCount = %d, want 1 (forwarded exactly once)", got)
	}
	if !p.isWarmed() {
		t.Fatal("fast didOpen forward did not set warmed")
	}
}

// TestLazyProxy_ColdStartSlots_OwnInFlightJoinNotRefused is the F3 guard: with
// ColdStartConcurrency other backends already warming, a RETRY that merely joins
// this proxy's OWN in-flight materialize must be allowed through (it spawns
// nothing), not refused as slots-busy.
func TestLazyProxy_ColdStartSlots_OwnInFlightJoinNotRefused(t *testing.T) {
	// Two foreign Starting rows become port-live only AFTER this proxy's own
	// flight has started, modeling: cold start begins (0 foreign live) → 503 on
	// budget → two other backends start → retry must JOIN, not be refused.
	var foreignLive atomic.Bool
	orig := materializedSlotPortLiveFn
	materializedSlotPortLiveFn = func(port int) bool {
		if port == 9301 || port == 9302 {
			return foreignLive.Load()
		}
		return true
	}
	t.Cleanup(func() { materializedSlotPortLiveFn = orig })

	f := &fakeLifecycle{kind: "mcp-language-server", materializeDelay: 400 * time.Millisecond}
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
		// Generous join budget: it only bounds how long the retry WAITS (the pass
		// path returns as soon as the 400ms materialize completes), but a tight
		// value flakes to a 503 when host load stretches the fake's timer — the
		// full-package run has observed the 400ms delay starved past 5s.
		MaterializeWaitBudget: 60 * time.Second,
	})
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = p.Stop(ctx)
	})
	h := p.Handler()

	firstCode := make(chan int, 1)
	go func() {
		rr := postRPC(t, h, "tools/call", 1)
		firstCode <- rr.Code
	}()

	// Wait until this proxy's own materialize flight is active (endpoint not yet
	// published — still in the 400ms materialize delay).
	waitForCond(t, "own materialize flight active", func() bool {
		return p.gate.HasActiveFlight(p.inflightKey())
	})
	// Two foreign Starting rows now go live — the cold-start gate WOULD refuse a
	// fresh cold start, but this retry joins the running flight (F3).
	foreignLive.Store(true)

	rr := postRPC(t, h, "tools/call", 2)
	if strings.Contains(rr.Body.String(), "cold-start slots busy") {
		t.Fatalf("own in-flight join refused by cold-start gate (F3 regression): %s", rr.Body.String())
	}
	if rr.Code != http.StatusOK {
		t.Fatalf("join code = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	if got := f.materializeCount.Load(); got != 1 {
		t.Fatalf("materializeCount = %d, want 1 (retry joined, did not spawn)", got)
	}
	if code := <-firstCode; code != http.StatusOK {
		t.Fatalf("first call code = %d, want 200", code)
	}
}

// slowStopLifecycle models the real BackendLifecycle mutex contract: Materialize
// holds the lifecycle mutex for its whole (blockable) duration, and Stop() must
// acquire the same mutex — so a slow Materialize makes Stop() block. Used by the
// F5 bounded-Stop test.
type slowStopLifecycle struct {
	mu                 sync.Mutex
	materializeStarted chan struct{}
	releaseMaterialize chan struct{}
	startedOnce        sync.Once
	stopCount          atomic.Int32
}

func newSlowStopLifecycle() *slowStopLifecycle {
	return &slowStopLifecycle{
		materializeStarted: make(chan struct{}),
		releaseMaterialize: make(chan struct{}),
	}
}

func (f *slowStopLifecycle) Kind() string { return "mcp-language-server" }

func (f *slowStopLifecycle) Materialize(ctx context.Context) (MCPEndpoint, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.startedOnce.Do(func() { close(f.materializeStarted) })
	select {
	case <-f.releaseMaterialize:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return &fakeEndpoint{parent: &fakeLifecycle{kind: "mcp-language-server"}}, nil
}

func (f *slowStopLifecycle) Stop() error {
	f.stopCount.Add(1)
	f.mu.Lock() // blocks until Materialize releases the mutex, like the real impl
	defer f.mu.Unlock()
	return nil
}

// TestLazyProxy_Stop_BoundedByCtxWhileMaterializeHoldsLifecycleMutex is the F5
// guard: Lifecycle.Stop() blocks behind a slow Materialize holding the lifecycle
// mutex, but LazyProxy.Stop must return within its ctx budget rather than wait.
func TestLazyProxy_Stop_BoundedByCtxWhileMaterializeHoldsLifecycleMutex(t *testing.T) {
	f := newSlowStopLifecycle()
	p, _ := newTestProxy(t, "mcp-language-server", nil)
	p.cfg.Lifecycle = f

	go func() { _, _, _ = p.ensureMaterialized(context.Background()) }()
	<-f.materializeStarted

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	err := p.Stop(ctx)
	elapsed := time.Since(start)

	if elapsed > time.Second {
		t.Fatalf("Stop blocked %v behind the lifecycle mutex; want bounded by ctx (F5)", elapsed)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Stop err = %v, want context.DeadlineExceeded (bounded return)", err)
	}
	// Release the materialize so the detached Stop goroutine can finish.
	close(f.releaseMaterialize)
}

// TestLazyProxy_MarkWarmed_GenerationGuardSkipsAfterTeardown is the F6a guard: a
// stale-generation first-success must NOT stamp Active over a Failed that a
// concurrent teardown wrote.
func TestLazyProxy_MarkWarmed_GenerationGuardSkipsAfterTeardown(t *testing.T) {
	f := &fakeLifecycle{kind: "mcp-language-server"}
	p, regPath := newTestProxy(t, "mcp-language-server", f)

	_, gen, err := p.ensureMaterialized(context.Background())
	if err != nil {
		t.Fatalf("ensureMaterialized: %v", err)
	}
	p.endBackendRequest()

	// A concurrent teardown stamps Failed and bumps the endpoint generation.
	p.onSendFailure(gen, errors.New("backend subprocess exited"))
	if e := readEntry(t, regPath); e.Lifecycle != api.LifecycleFailed {
		t.Fatalf("pre-condition: lifecycle = %q, want Failed after teardown", e.Lifecycle)
	}

	// A late first-success carrying the STALE generation must be a no-op.
	p.markWarmedOnFirstSuccess(gen)
	if e := readEntry(t, regPath); e.Lifecycle != api.LifecycleFailed {
		t.Fatalf("stale-generation markWarmed overwrote Failed with %q (F6a regression)", e.Lifecycle)
	}
}

// TestLazyProxy_ProbationDwellRowRendersAsStartingContract is the F6b
// consumer-contract sweep: while a backend dwells in probation the registry row
// stays Starting, and the lifecycle string is exactly "starting" — the literal
// downstream consumers (`mcphub status` renderHealthCell, GUI LspMatrix badge)
// switch on. A dwell that reported anything else would silently break them.
func TestLazyProxy_ProbationDwellRowRendersAsStartingContract(t *testing.T) {
	if api.LifecycleStarting != "starting" {
		t.Fatalf("LifecycleStarting constant = %q, want %q (consumer string contract)", api.LifecycleStarting, "starting")
	}
	f := &fakeLifecycle{kind: "mcp-language-server", firstSendGate: make(chan struct{})}
	p, regPath := newTestProxy(t, "mcp-language-server", f)
	h := p.Handler()

	// Predicate 1: a cold tools/call is AWAITED (not 503-ed); during the await the
	// row dwells at the literal "starting" that downstream consumers switch on.
	done := make(chan struct{})
	go func() { _ = postRPC(t, h, "tools/call", 1); close(done) }()
	waitForCond(t, "first send in flight", func() bool { return f.sendCount.Load() == 1 })
	e := readEntry(t, regPath)
	if e.Lifecycle != api.LifecycleStarting {
		t.Fatalf("probation-dwell lifecycle = %q, want the literal %q", e.Lifecycle, api.LifecycleStarting)
	}
	close(f.firstSendGate)
	<-done
}

// --- PR #489 commission round: Findings 2 & 3 concurrency guards --------------

// TestLazyProxy_PublishMaterialized_DoesNotRepublishOverConcurrentWarm is the
// Finding 2 guard: publishMaterializedEndpoint must NOT overwrite an endpoint a
// concurrent caller already published (and warmed). Materialize returns a fresh
// wrapper around the SAME running host; republishing would reset warmed=false /
// bump the generation and throw an ACTIVE backend back under probation. The
// redundant wrapper is closed but the shared host is NOT stopped.
func TestLazyProxy_PublishMaterialized_DoesNotRepublishOverConcurrentWarm(t *testing.T) {
	f := &fakeLifecycle{kind: "mcp-language-server"}
	p, _ := newTestProxy(t, "mcp-language-server", f)

	// Simulate caller 1 having published + warmed an endpoint.
	existing := &fakeEndpoint{parent: f}
	p.mu.Lock()
	p.endpoint = existing
	p.warmed = true
	p.endpointGeneration = 7
	p.endpointPublishedAt = time.Now().Add(-time.Hour)
	p.mu.Unlock()

	// Caller 2's fn publishes a redundant fresh wrapper — must be a no-op vs the
	// live warm endpoint.
	redundant := &fakeEndpoint{parent: f}
	if err := p.publishMaterializedEndpoint(redundant); err != nil {
		t.Fatalf("publishMaterializedEndpoint returned err: %v", err)
	}

	p.mu.Lock()
	gotEp := p.endpoint
	gotWarmed := p.warmed
	gotGen := p.endpointGeneration
	p.mu.Unlock()

	if gotEp != MCPEndpoint(existing) {
		t.Fatal("warm endpoint was overwritten by a redundant publish (Finding 2)")
	}
	if !gotWarmed {
		t.Fatal("warmed reset by redundant publish (Finding 2)")
	}
	if gotGen != 7 {
		t.Fatalf("generation bumped 7 -> %d by redundant publish (Finding 2)", gotGen)
	}
	if !redundant.closed.Load() {
		t.Fatal("redundant wrapper was not closed")
	}
	if sc := f.stopCount.Load(); sc != 0 {
		t.Fatalf("Lifecycle.Stop called on redundant publish (would kill the shared host): stopCount=%d", sc)
	}
}

// TestLazyProxy_MarkWarmed_ConcurrentTeardownDoesNotOverwriteFailed is the
// Finding 3 guard: the LifecycleActive write must not overwrite a Failed that a
// concurrent teardown stamped. markWarmed and onSendFailure run concurrently
// (under -race); onSendFailure always bumps the generation + stamps Failed, so
// the registry must ALWAYS end Failed — markWarmed either skips on the generation
// mismatch or writes Active first (superseded by Failed), never a stale Active
// last. Repeated to widen the interleaving window.
func TestLazyProxy_MarkWarmed_ConcurrentTeardownDoesNotOverwriteFailed(t *testing.T) {
	for i := 0; i < 100; i++ {
		f := &fakeLifecycle{kind: "mcp-language-server"}
		p, regPath := newTestProxy(t, "mcp-language-server", f)

		_, gen, err := p.ensureMaterialized(context.Background())
		if err != nil {
			t.Fatalf("iter %d: ensureMaterialized: %v", i, err)
		}
		p.endBackendRequest()

		var wg sync.WaitGroup
		wg.Add(2)
		go func() { defer wg.Done(); p.markWarmedOnFirstSuccess(gen) }()
		go func() { defer wg.Done(); p.onSendFailure(gen, errors.New("backend died")) }()
		wg.Wait()

		if e := readEntry(t, regPath); e.Lifecycle != api.LifecycleFailed {
			t.Fatalf("iter %d: lifecycle = %q, want Failed (stale Active overwrote a real failure — Finding 3)", i, e.Lifecycle)
		}

		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		_ = p.Stop(ctx)
		cancel()
	}
}

// --- PR #489 round-3: unified single-owner lifecycle (P2), slot-scan (P3),
// --- await-after-delivery + reliability constraints (P1) ---------------------

// TestLazyProxy_Reconcile_RestoresActiveAfterConcurrentStartingDowngrade is the
// Predicate 2 stuck-Starting fix: a warmed (Active) row that a concurrent
// reserveMaterializedSlot downgraded to Starting out-of-band (registry write +
// shadow invalidation) is restored to Active by the next acquisition reconcile —
// not left stuck Starting forever eating a cold-start slot.
func TestLazyProxy_Reconcile_RestoresActiveAfterConcurrentStartingDowngrade(t *testing.T) {
	f := &fakeLifecycle{kind: "mcp-language-server"}
	p, regPath := newTestProxy(t, "mcp-language-server", f)
	h := p.Handler()

	if rr := postRPC(t, h, "tools/call", 1); rr.Code != http.StatusOK {
		t.Fatalf("warm code=%d", rr.Code)
	}
	if e := readEntry(t, regPath); e.Lifecycle != api.LifecycleActive {
		t.Fatalf("setup lifecycle=%q, want Active", e.Lifecycle)
	}

	// Simulate the concurrent-reserve race: the registry is downgraded to Starting
	// out-of-band and the reconcile shadow is invalidated — exactly what
	// reserveMaterializedSlot's flock write + shadow reset leave when its snapshot
	// missed the publish.
	_ = api.NewRegistry(regPath).PutLifecycle("abcd1234", "python", api.LifecycleStarting, "")
	p.mu.Lock()
	p.lastWrittenLifecycle = ""
	p.mu.Unlock()

	// The next tools/call is a warm cache hit -> acquisition-return reconcile derives
	// Active (endpoint!=nil AND warmed) and RESTORES it.
	if rr := postRPC(t, h, "tools/call", 2); rr.Code != http.StatusOK {
		t.Fatalf("second code=%d", rr.Code)
	}
	if e := readEntry(t, regPath); e.Lifecycle != api.LifecycleActive {
		t.Fatalf("lifecycle after reconcile = %q, want Active (stuck-Starting fix)", e.Lifecycle)
	}
}

// TestLazyProxy_Reconcile_ConcurrentDowngradeSettlesActive is the -race stress
// variant: concurrent warm tools/calls race concurrent out-of-band Starting
// downgrades; a final warm call must converge the row to Active.
func TestLazyProxy_Reconcile_ConcurrentDowngradeSettlesActive(t *testing.T) {
	f := &fakeLifecycle{kind: "mcp-language-server"}
	p, regPath := newTestProxy(t, "mcp-language-server", f)
	h := p.Handler()
	if rr := postRPC(t, h, "tools/call", 0); rr.Code != http.StatusOK {
		t.Fatalf("warm code=%d", rr.Code)
	}

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(id int) { defer wg.Done(); postRPC(t, h, "tools/call", id) }(i)
	}
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = api.NewRegistry(regPath).PutLifecycle("abcd1234", "python", api.LifecycleStarting, "")
			p.mu.Lock()
			p.lastWrittenLifecycle = ""
			p.mu.Unlock()
		}()
	}
	wg.Wait()

	if rr := postRPC(t, h, "tools/call", 999); rr.Code != http.StatusOK {
		t.Fatalf("final code=%d", rr.Code)
	}
	if e := readEntry(t, regPath); e.Lifecycle != api.LifecycleActive {
		t.Fatalf("final lifecycle = %q, want Active (converged)", e.Lifecycle)
	}
}

// TestLazyProxy_Reconcile_ShadowResetOnTeardownReenablesWrite covers the highest-
// risk shadow-reset site: a generation bump (teardown) MUST reset the shadow so a
// re-materialize's reconcile is not suppressed by a stale Active shadow (the gen
// guard prevents a WRONG write; the shadow reset prevents a MISSING write).
func TestLazyProxy_Reconcile_ShadowResetOnTeardownReenablesWrite(t *testing.T) {
	f := &fakeLifecycle{kind: "mcp-language-server"}
	p, regPath := newTestProxyWithCfg(t, "mcp-language-server", f, 10*time.Millisecond, 100*time.Millisecond)
	h := p.Handler()

	if rr := postRPC(t, h, "tools/call", 1); rr.Code != http.StatusOK {
		t.Fatalf("warm code=%d", rr.Code)
	}
	if e := readEntry(t, regPath); e.Lifecycle != api.LifecycleActive {
		t.Fatalf("setup=%q, want Active", e.Lifecycle)
	}

	p.onSendFailure(currentGen(p), errors.New("backend died"))
	if e := readEntry(t, regPath); e.Lifecycle != api.LifecycleFailed {
		t.Fatalf("post-teardown=%q, want Failed", e.Lifecycle)
	}
	p.mu.Lock()
	shadow := p.lastWrittenLifecycle
	p.mu.Unlock()
	if shadow != "" {
		t.Fatalf("shadow = %q after teardown, want reset to empty", shadow)
	}

	time.Sleep(20 * time.Millisecond) // past the retry throttle
	if rr := postRPC(t, h, "tools/call", 2); rr.Code != http.StatusOK {
		t.Fatalf("re-materialize code=%d", rr.Code)
	}
	if e := readEntry(t, regPath); e.Lifecycle != api.LifecycleActive {
		t.Fatalf("lifecycle after re-materialize = %q, want Active (shadow reset re-enabled the write)", e.Lifecycle)
	}
}

// TestLazyProxy_SlotScan_SkipsPortDialForNonStartingActiveRows is the Predicate 3
// guard: reserveMaterializedSlot dials ONLY Starting/Active rows; Configured /
// Failed / Missing rows are skipped before the 300ms dial.
func TestLazyProxy_SlotScan_SkipsPortDialForNonStartingActiveRows(t *testing.T) {
	var mu sync.Mutex
	var dialed []int
	orig := materializedSlotPortLiveFn
	materializedSlotPortLiveFn = func(port int) bool {
		mu.Lock()
		dialed = append(dialed, port)
		mu.Unlock()
		return false
	}
	t.Cleanup(func() { materializedSlotPortLiveFn = orig })

	f := &fakeLifecycle{kind: "mcp-language-server"}
	regPath := filepath.Join(t.TempDir(), "r.yaml")
	seed := api.NewRegistry(regPath)
	seed.Put(api.WorkspaceEntry{WorkspaceKey: "abcd1234", WorkspacePath: "D:/test/ws", Language: "python", Backend: "mcp-language-server", TaskName: "mcp-local-hub-lsp-abcd1234-python", Port: 9200, Lifecycle: api.LifecycleConfigured})
	seed.Put(api.WorkspaceEntry{WorkspaceKey: "conf0001", WorkspacePath: "D:/test/c", Language: "go", Backend: "gopls-mcp", TaskName: "mcp-local-hub-lsp-conf0001-go", Port: 9301, Lifecycle: api.LifecycleConfigured})
	seed.Put(api.WorkspaceEntry{WorkspaceKey: "fail0001", WorkspacePath: "D:/test/f", Language: "rust", Backend: "mcp-language-server", TaskName: "mcp-local-hub-lsp-fail0001-rust", Port: 9302, Lifecycle: api.LifecycleFailed})
	seed.Put(api.WorkspaceEntry{WorkspaceKey: "miss0001", WorkspacePath: "D:/test/m", Language: "java", Backend: "mcp-language-server", TaskName: "mcp-local-hub-lsp-miss0001-java", Port: 9303, Lifecycle: api.LifecycleMissing})
	seed.Put(api.WorkspaceEntry{WorkspaceKey: "star0001", WorkspacePath: "D:/test/s", Language: "kotlin", Backend: "mcp-language-server", TaskName: "mcp-local-hub-lsp-star0001-kotlin", Port: 9304, Lifecycle: api.LifecycleStarting})
	if err := seed.Save(); err != nil {
		t.Fatalf("seed registry: %v", err)
	}
	p := NewLazyProxy(LazyProxyConfig{
		WorkspaceKey: "abcd1234", WorkspacePath: "D:/test/ws", Language: "python",
		BackendKind: "mcp-language-server", Port: 9200, Lifecycle: f, RegistryPath: regPath,
		InflightMinRetryGap: 20 * time.Millisecond, ToolsCallDebounce: 100 * time.Millisecond,
		ColdStartConcurrency: 2,
	})
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = p.Stop(ctx)
	})

	if rr := postRPC(t, p.Handler(), "tools/call", 1); rr.Code != http.StatusOK {
		t.Fatalf("tools/call code=%d body=%s", rr.Code, rr.Body.String())
	}

	mu.Lock()
	defer mu.Unlock()
	for _, port := range dialed {
		if port == 9301 || port == 9302 || port == 9303 {
			t.Fatalf("dialed a non-Starting/Active row port %d (Predicate 3 regression); dialed=%v", port, dialed)
		}
	}
	sawStarting := false
	for _, port := range dialed {
		if port == 9304 {
			sawStarting = true
		}
	}
	if !sawStarting {
		t.Fatalf("Starting row port 9304 was not dialed; dialed=%v", dialed)
	}
}

// TestLazyProxy_ColdRequestHoldCeilingInvariantClamped is reliability #4: a
// misordered config is clamped so ColdStartMaxProbation > ColdRequestHoldCeiling >
// MaterializeWaitBudget holds.
func TestLazyProxy_ColdRequestHoldCeilingInvariantClamped(t *testing.T) {
	f := &fakeLifecycle{kind: "mcp-language-server"}
	regPath := filepath.Join(t.TempDir(), "r.yaml")
	seed := api.NewRegistry(regPath)
	seed.Put(api.WorkspaceEntry{WorkspaceKey: "abcd1234", WorkspacePath: "D:/test/ws", Language: "python", Backend: "mcp-language-server", TaskName: "mcp-local-hub-lsp-abcd1234-python", Lifecycle: ""})
	if err := seed.Save(); err != nil {
		t.Fatalf("seed: %v", err)
	}
	p := NewLazyProxy(LazyProxyConfig{
		WorkspaceKey: "abcd1234", WorkspacePath: "D:/test/ws", Language: "python",
		BackendKind: "mcp-language-server", Lifecycle: f, RegistryPath: regPath,
		MaterializeWaitBudget:  30 * time.Second, // deliberately > default ceiling
		ColdRequestHoldCeiling: 10 * time.Second, // < budget -> clamp up
		ColdStartMaxProbation:  5 * time.Second,  // < ceiling -> clamp up
	})
	stopProxyOnCleanup(t, p)
	if !(p.cfg.MaterializeWaitBudget < p.cfg.ColdRequestHoldCeiling) {
		t.Fatalf("after clamp: ceiling %s not > budget %s", p.cfg.ColdRequestHoldCeiling, p.cfg.MaterializeWaitBudget)
	}
	if !(p.cfg.ColdRequestHoldCeiling < p.cfg.ColdStartMaxProbation) {
		t.Fatalf("after clamp: probation %s not > ceiling %s", p.cfg.ColdStartMaxProbation, p.cfg.ColdRequestHoldCeiling)
	}
}

// TestLazyProxy_ColdForwardHeldEvent_FiresBeyondBudget is reliability #5: the
// lsp-cold-forward-held observability event fires when a request is held past
// ~MaterializeWaitBudget, and a fast/warm forward does NOT fire it.
func TestLazyProxy_ColdForwardHeldEvent_FiresBeyondBudget(t *testing.T) {
	var fired atomic.Int32
	var gotMethod atomic.Value
	override := func(backendKind, workspacePath, method, heldBeyond, ceiling string) {
		gotMethod.Store(method)
		fired.Add(1)
	}
	// Atomic seam: a detached timer callback can outlive this test, so the
	// override + restore go through the atomic pointer (plain-var swap raced).
	coldForwardHeldEventFn.Store(&override)
	t.Cleanup(func() { coldForwardHeldEventFn.Store(nil) })

	f := &fakeLifecycle{kind: "mcp-language-server", firstSendGate: make(chan struct{})}
	p, _ := newTestProxy(t, "mcp-language-server", f)
	p.cfg.MaterializeWaitBudget = 50 * time.Millisecond
	p.cfg.ColdRequestHoldCeiling = 5 * time.Second // long: event fires before the ceiling
	h := p.Handler()

	done := make(chan struct{})
	go func() { _ = postRPC(t, h, "tools/call", 1); close(done) }()
	waitForCond(t, "cold-forward-held event fired", func() bool { return fired.Load() >= 1 })
	if m, _ := gotMethod.Load().(string); m != "tools/call" {
		t.Fatalf("event method = %q, want tools/call", m)
	}
	close(f.firstSendGate)
	<-done

	// A fast/warm forward must NOT fire the event (timer stopped before firing).
	before := fired.Load()
	if rr := postRPC(t, h, "tools/call", 2); rr.Code != http.StatusOK {
		t.Fatalf("warm code=%d", rr.Code)
	}
	if fired.Load() != before {
		t.Fatalf("fast warm forward fired the held event (%d -> %d)", before, fired.Load())
	}
}

// --- PR #489 round-4: commission findings F1-F5 ------------------------------

// TestLazyProxy_ReapSeveredDeliveredRequest_ControlledError is F1: the probation
// watchdog (Branch A) severs a DELIVERED in-flight request (tools/call AND generic
// forward). The severed request must get the NON-retryable controlled 500 (delivered,
// do-not-retry), NOT a retryable-looking -32603 that would let an agent duplicate a
// partially-executed mutating tool.
func TestLazyProxy_ReapSeveredDeliveredRequest_ControlledError(t *testing.T) {
	for _, method := range []string{"tools/call", "textDocument/definition"} {
		t.Run(method, func(t *testing.T) {
			f := &fakeLifecycle{kind: "mcp-language-server", firstSendGate: make(chan struct{})}
			p, regPath := newTestProxy(t, "mcp-language-server", f)
			h := p.Handler()

			done := make(chan *httptest.ResponseRecorder, 1)
			go func() { done <- postRPC(t, h, method, 1) }()
			waitForCond(t, "delivered send in flight", func() bool { return f.sendCount.Load() == 1 })

			// Age the publish so Branch A fires; the in-flight request is still
			// awaiting over a never-warmed backend, so reapWedgedProbation severs it.
			p.mu.Lock()
			p.endpointPublishedAt = time.Now().Add(-time.Hour)
			p.mu.Unlock()
			p.reapWedgedProbation(time.Now().UTC())

			rr := <-done
			if rr.Code != http.StatusInternalServerError {
				t.Fatalf("severed %s code = %d, want 500 controlled error; body=%s", method, rr.Code, rr.Body.String())
			}
			if ra := rr.Header().Get("Retry-After"); ra != "" {
				t.Fatalf("Retry-After = %q, want empty (non-retryable)", ra)
			}
			body := rr.Body.String()
			if strings.Contains(body, "retry in") {
				t.Fatalf("severed error carries a retry hint (would auto-retry): %s", body)
			}
			if !strings.Contains(body, "do not auto-retry") {
				t.Fatalf("severed error missing the do-not-retry contract: %s", body)
			}
			if e := readEntry(t, regPath); e.Lifecycle != api.LifecycleFailed {
				t.Fatalf("lifecycle after reap = %q, want Failed (slot freed)", e.Lifecycle)
			}
		})
	}
}

// TestLazyProxy_ColdForward_AwaitsDeliveredResponse_ThenWarmActive is F2: the generic
// REQUEST path (handleForward) awaits a delivered cold response then warms → Active,
// mirroring the tools/call contract.
func TestLazyProxy_ColdForward_AwaitsDeliveredResponse_ThenWarmActive(t *testing.T) {
	f := &fakeLifecycle{kind: "mcp-language-server", firstSendGate: make(chan struct{})}
	p, regPath := newTestProxy(t, "mcp-language-server", f)
	h := p.Handler()

	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- postRPC(t, h, "textDocument/definition", 1) }()
	waitForCond(t, "forward send delivered + in flight", func() bool { return f.sendCount.Load() == 1 })
	if e := readEntry(t, regPath); e.Lifecycle != api.LifecycleStarting {
		t.Fatalf("lifecycle while awaiting forward = %q, want Starting", e.Lifecycle)
	}
	if p.isWarmed() {
		t.Fatal("warmed before any successful forward response")
	}

	close(f.firstSendGate)
	rr := <-done
	if rr.Code != http.StatusOK {
		t.Fatalf("forward code = %d, want 200 (awaited real result); body=%s", rr.Code, rr.Body.String())
	}
	if got := parseRPC(t, rr.Body.Bytes()); got["result"] == nil {
		t.Fatalf("no real forward result body: %+v", got)
	}
	if got := f.sendCount.Load(); got != 1 {
		t.Fatalf("sendCount = %d, want 1 (delivered exactly once)", got)
	}
	if !p.isWarmed() {
		t.Fatal("forward success did not set warmed")
	}
	if e := readEntry(t, regPath); e.Lifecycle != api.LifecycleActive {
		t.Fatalf("lifecycle after forward success = %q, want Active", e.Lifecycle)
	}
}

// TestLazyProxy_ColdForward_CeilingExceeded_NonRetryableControlledError is F2: the
// generic REQUEST path returns the same non-retryable 500 on a hold-ceiling expiry.
func TestLazyProxy_ColdForward_CeilingExceeded_NonRetryableControlledError(t *testing.T) {
	f := &fakeLifecycle{kind: "mcp-language-server", firstSendGate: make(chan struct{})}
	p, regPath := newTestProxy(t, "mcp-language-server", f)
	p.cfg.ColdRequestHoldCeiling = 40 * time.Millisecond
	h := p.Handler()

	rr := postRPC(t, h, "textDocument/references", 1) // gate never closes → ceiling fires
	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("code = %d, want 500 controlled; body=%s", rr.Code, rr.Body.String())
	}
	if ra := rr.Header().Get("Retry-After"); ra != "" {
		t.Fatalf("Retry-After = %q, want empty", ra)
	}
	body := rr.Body.String()
	if strings.Contains(body, "retry in") {
		t.Fatalf("controlled forward error carries a retry hint: %s", body)
	}
	if !strings.Contains(body, "do not auto-retry") {
		t.Fatalf("controlled forward error missing do-not-retry: %s", body)
	}
	if got := f.sendCount.Load(); got != 1 {
		t.Fatalf("sendCount = %d, want 1 (delivered once)", got)
	}
	if e := readEntry(t, regPath); e.Lifecycle != api.LifecycleStarting {
		t.Fatalf("lifecycle after forward ceiling = %q, want Starting", e.Lifecycle)
	}
}

// TestLazyProxy_ColdForward_HeldEventFires is F2: the lsp-cold-forward-held event
// fires for a held generic forward too.
func TestLazyProxy_ColdForward_HeldEventFires(t *testing.T) {
	var fired atomic.Int32
	var gotMethod atomic.Value
	override := func(backendKind, workspacePath, method, heldBeyond, ceiling string) {
		gotMethod.Store(method)
		fired.Add(1)
	}
	// Atomic seam: a detached timer callback can outlive this test, so the
	// override + restore go through the atomic pointer (plain-var swap raced).
	coldForwardHeldEventFn.Store(&override)
	t.Cleanup(func() { coldForwardHeldEventFn.Store(nil) })

	f := &fakeLifecycle{kind: "mcp-language-server", firstSendGate: make(chan struct{})}
	p, _ := newTestProxy(t, "mcp-language-server", f)
	p.cfg.MaterializeWaitBudget = 50 * time.Millisecond
	p.cfg.ColdRequestHoldCeiling = 5 * time.Second
	h := p.Handler()

	done := make(chan struct{})
	go func() { _ = postRPC(t, h, "workspace/symbol", 1); close(done) }()
	waitForCond(t, "held event fired for forward", func() bool { return fired.Load() >= 1 })
	if m, _ := gotMethod.Load().(string); m != "workspace/symbol" {
		t.Fatalf("event method = %q, want workspace/symbol", m)
	}
	close(f.firstSendGate)
	<-done
}

// TestLazyProxy_NegativeColdStartMaxProbation_NotClamped is F3: a negative
// ColdStartMaxProbation (documented "disable the watchdog") must NOT be silently
// re-armed by the ordering clamp, and the ceiling clamp is skipped too (skip BOTH).
func TestLazyProxy_NegativeColdStartMaxProbation_NotClamped(t *testing.T) {
	f := &fakeLifecycle{kind: "mcp-language-server"}
	regPath := filepath.Join(t.TempDir(), "r.yaml")
	seed := api.NewRegistry(regPath)
	seed.Put(api.WorkspaceEntry{WorkspaceKey: "abcd1234", WorkspacePath: "D:/test/ws", Language: "python", Backend: "mcp-language-server", TaskName: "mcp-local-hub-lsp-abcd1234-python", Lifecycle: ""})
	if err := seed.Save(); err != nil {
		t.Fatalf("seed: %v", err)
	}
	p := NewLazyProxy(LazyProxyConfig{
		WorkspaceKey: "abcd1234", WorkspacePath: "D:/test/ws", Language: "python",
		BackendKind: "mcp-language-server", Lifecycle: f, RegistryPath: regPath,
		MaterializeWaitBudget:  30 * time.Second, // > ceiling below → ceiling clamp fires
		ColdRequestHoldCeiling: 10 * time.Second, // < budget → clamped to 2×budget
		ColdStartMaxProbation:  -1 * time.Second, // disabled → skip ONLY the probation clamp
	})
	stopProxyOnCleanup(t, p)
	// r3-F3: the documented negative-probation disable is preserved (the probation
	// clamp must not silently re-arm the watchdog).
	if p.cfg.ColdStartMaxProbation != -1*time.Second {
		t.Fatalf("negative probation clamped to %s (r3-F3 regression); want preserved -1s", p.cfg.ColdStartMaxProbation)
	}
	// r4-F5: the UNRELATED ceiling>budget clamp still applies with the watchdog
	// disabled — the request-hold/materialize tiers govern their phases regardless.
	if want := 60 * time.Second; p.cfg.ColdRequestHoldCeiling != want {
		t.Fatalf("ceiling = %s with disabled probation, want clamped %s (r4-F5: ceiling>budget clamp is unconditional)", p.cfg.ColdRequestHoldCeiling, want)
	}
}

// TestLazyProxy_OnSendFailure_StaleGenerationNoOp is F4: a late failure carrying an
// OLD generation must NOT evict a freshly-republished newer-generation healthy
// endpoint nor stamp Failed over it (symmetric with markWarmedOnFirstSuccess).
func TestLazyProxy_OnSendFailure_StaleGenerationNoOp(t *testing.T) {
	f := &fakeLifecycle{kind: "mcp-language-server"}
	p, regPath := newTestProxy(t, "mcp-language-server", f)

	if rr := postRPC(t, p.Handler(), "tools/call", 1); rr.Code != http.StatusOK {
		t.Fatalf("warm code=%d", rr.Code)
	}
	staleGen := currentGen(p) - 1 // one generation behind the current live endpoint

	p.onSendFailure(staleGen, errors.New("late failure from an already-torn-down endpoint"))

	p.mu.Lock()
	ep := p.endpoint
	p.mu.Unlock()
	if ep == nil {
		t.Fatal("stale-generation onSendFailure evicted the live endpoint (F4 regression)")
	}
	if e := readEntry(t, regPath); e.Lifecycle == api.LifecycleFailed {
		t.Fatalf("stale-generation onSendFailure stamped Failed over a healthy endpoint (F4 regression): %q", e.Lifecycle)
	}
}

// TestLazyProxy_ColdRequestHeldError_MessageDistinguishesWarmVsCold is F5: the
// controlled-error message reports "still indexing" for a COLD backend and a
// "slow query" for a WARM backend (the ceiling is now the universal request bound),
// both keeping the 500 + do-not-retry semantics and no retry hint.
func TestLazyProxy_ColdRequestHeldError_MessageDistinguishesWarmVsCold(t *testing.T) {
	f := &fakeLifecycle{kind: "mcp-language-server"}
	p, _ := newTestProxy(t, "mcp-language-server", f)

	rrCold := httptest.NewRecorder()
	p.writeColdRequestHeldError(rrCold, json.RawMessage("1"), false)
	coldBody := rrCold.Body.String()
	if rrCold.Code != http.StatusInternalServerError {
		t.Fatalf("cold code = %d, want 500", rrCold.Code)
	}
	if !strings.Contains(coldBody, "still indexing") {
		t.Fatalf("cold message missing 'still indexing': %s", coldBody)
	}
	if !strings.Contains(coldBody, "do not auto-retry") || strings.Contains(coldBody, "retry in") {
		t.Fatalf("cold message broke the non-retry contract: %s", coldBody)
	}

	rrWarm := httptest.NewRecorder()
	p.writeColdRequestHeldError(rrWarm, json.RawMessage("2"), true)
	warmBody := rrWarm.Body.String()
	if rrWarm.Code != http.StatusInternalServerError {
		t.Fatalf("warm code = %d, want 500", rrWarm.Code)
	}
	if strings.Contains(warmBody, "still indexing") {
		t.Fatalf("warm message wrongly reports 'still indexing' (F5 misdirection): %s", warmBody)
	}
	if !strings.Contains(warmBody, "slow query") {
		t.Fatalf("warm message missing 'slow query': %s", warmBody)
	}
	if !strings.Contains(warmBody, "do not auto-retry") || strings.Contains(warmBody, "retry in") {
		t.Fatalf("warm message broke the non-retry contract: %s", warmBody)
	}
}

// --- PR #489 bot round-3: F1 shadow-on-failed-write, F2 abort-reconcile --------

// TestLazyProxy_Reconcile_FailedWriteDoesNotRecordShadow_RetriesNext is F1 (bot
// r3): a FAILED registry write must NOT record the lastWrittenLifecycle shadow —
// otherwise later warm requests hit the shadow fast-path and never retry the
// failed Starting->Active move, leaving the row stuck while actually warm. The
// registry write is failed deterministically by making the registry path's parent
// a FILE (Registry.Lock's MkdirAll then errors), then healed.
func TestLazyProxy_Reconcile_FailedWriteDoesNotRecordShadow_RetriesNext(t *testing.T) {
	tmp := t.TempDir()
	blocker := filepath.Join(tmp, "blocked")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatalf("blocker: %v", err)
	}
	regPath := filepath.Join(blocker, "r.yaml") // parent is a FILE -> registry writes fail
	f := &fakeLifecycle{kind: "mcp-language-server"}
	p := NewLazyProxy(LazyProxyConfig{
		WorkspaceKey: "abcd1234", WorkspacePath: "D:/test/ws", Language: "python",
		BackendKind: "mcp-language-server", Lifecycle: f, RegistryPath: regPath,
	})
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = p.Stop(ctx)
	})

	// Manufacture a live not-yet-warmed endpoint at a known generation.
	p.mu.Lock()
	p.endpoint = &fakeEndpoint{parent: f}
	p.endpointGeneration = 5
	p.warmed = false
	p.mu.Unlock()

	// First success: the Active reconcile write FAILS (unwritable registry path).
	p.markWarmedOnFirstSuccess(5)
	p.mu.Lock()
	shadow := p.lastWrittenLifecycle
	warmed := p.warmed
	p.mu.Unlock()
	if !warmed {
		t.Fatal("warmed flag not set on first success")
	}
	if shadow != "" {
		t.Fatalf("shadow = %q recorded despite FAILED registry write (F1 regression: write never retried)", shadow)
	}

	// Heal the path: replace the blocker file with a real directory + seeded row.
	if err := os.Remove(blocker); err != nil {
		t.Fatalf("remove blocker: %v", err)
	}
	seed := api.NewRegistry(regPath)
	seed.Put(api.WorkspaceEntry{
		WorkspaceKey: "abcd1234", WorkspacePath: "D:/test/ws", Language: "python",
		Backend: "mcp-language-server", TaskName: "mcp-local-hub-lsp-abcd1234-python",
		Lifecycle: api.LifecycleStarting,
	})
	if err := seed.Save(); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// Next gen-matching success retries the write (shadow still empty -> miss)
	// and succeeds -> row Active + shadow recorded.
	p.markWarmedOnFirstSuccess(5)
	if e := readEntry(t, regPath); e.Lifecycle != api.LifecycleActive {
		t.Fatalf("lifecycle after retry = %q, want Active (failed write must be retried)", e.Lifecycle)
	}
	p.mu.Lock()
	shadow = p.lastWrittenLifecycle
	p.mu.Unlock()
	if shadow != api.LifecycleActive {
		t.Fatalf("shadow after successful retry = %q, want Active", shadow)
	}
}

// TestLazyProxy_CallerAbortAfterConcurrentWarm_ReconcilesActive is F2 (bot r3):
// a caller that wrote Starting (reserve) and then aborts on its own canceled ctx
// must NOT leave the registry downgraded when a concurrent publisher made the
// endpoint live+warmed in the race window — the abort path reconciles to Active.
func TestLazyProxy_CallerAbortAfterConcurrentWarm_ReconcilesActive(t *testing.T) {
	f := newStopAfterMaterializeLifecycle()
	p, regPath := newTestProxy(t, "mcp-language-server", nil)
	p.cfg.Lifecycle = f
	p.cfg.MaterializeWaitBudget = 5 * time.Second // budget must not fire; ctx-cancel drives the abort

	ctx, cancel := context.WithCancel(context.Background())
	res := make(chan error, 1)
	go func() {
		_, _, err := p.ensureMaterialized(ctx)
		res <- err
	}()
	// B reserved (row Starting) and its flight is blocked inside Materialize.
	<-f.materializeStarted
	if e := readEntry(t, regPath); e.Lifecycle != api.LifecycleStarting {
		t.Fatalf("precondition: row = %q, want Starting (B's reserve)", e.Lifecycle)
	}

	// A concurrent publisher (modeled directly under p.mu, as the race window
	// leaves it) installs a live WARMED endpoint while the registry still carries
	// B's Starting downgrade and the shadow is reset.
	parent := &fakeLifecycle{kind: "mcp-language-server"}
	p.mu.Lock()
	p.endpoint = &fakeEndpoint{parent: parent}
	p.endpointGeneration++
	p.lastWrittenLifecycle = ""
	p.warmed = true
	p.startingSince = time.Time{}
	p.mu.Unlock()

	cancel() // B aborts while joining its (still-blocked) flight
	if err := <-res; !errors.Is(err, context.Canceled) {
		t.Fatalf("aborted caller err = %v, want context.Canceled", err)
	}
	// The abort path must have reconciled the live+warmed endpoint -> Active.
	if e := readEntry(t, regPath); e.Lifecycle != api.LifecycleActive {
		t.Fatalf("row after caller abort = %q, want Active (F2 regression: stuck Starting until a later request)", e.Lifecycle)
	}

	// Cleanup: unblock the detached flight (its publish sees the live endpoint,
	// closes the redundant wrapper, and keeps state intact), then stop the proxy.
	close(f.releaseMaterialize)
	sctx, scancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer scancel()
	_ = p.Stop(sctx)
}

// --- pre-bot round (r4): F1 rollback-vs-reset epoch, F2 watchdog gating,
// --- F4 Branch-B load-error retention -----------------------------------------

// TestLazyProxy_DocRefRollback_AfterTeardownReset_NoPhantomRefcount is r4-F1:
// a doc-lifecycle rollback racing a teardown's resetDocRefs must NOT inject a
// phantom refcount into the fresh map (which would permanently absorb the next
// legitimate didOpen). Covers the deterministic reset-then-rollback order at the
// primitive level, then the full Branch-A interleave through the handler.
func TestLazyProxy_DocRefRollback_AfterTeardownReset_NoPhantomRefcount(t *testing.T) {
	const uri = "file:///ws/foo.go"

	// Primitive-level deterministic order: apply (1->0 close, forward) ->
	// teardown reset -> rollback. The rollback must no-op on the epoch mismatch.
	{
		f := &fakeLifecycle{kind: "mcp-language-server"}
		p, _ := newTestProxy(t, "mcp-language-server", f)
		p.docRefsMu.Lock()
		p.docRefs[uri] = 1
		p.docRefsMu.Unlock()

		forward, epoch, err := p.applyDocRef(uri, false) // didClose 1->0
		if err != nil || !forward {
			t.Fatalf("applyDocRef close: forward=%v err=%v, want forward=true", forward, err)
		}
		p.resetDocRefs()                    // teardown clears the map + bumps the epoch
		p.rollbackDocRef(uri, false, epoch) // late rollback for the PRE-reset transition

		p.docRefsMu.Lock()
		n := len(p.docRefs)
		p.docRefsMu.Unlock()
		if n != 0 {
			t.Fatalf("rollback injected a phantom refcount into the fresh map (r4-F1 regression): %d entries", n)
		}

		// The next didOpen must be a genuine first-open (0->1 forward).
		forward, _, err = p.applyDocRef(uri, true)
		if err != nil || !forward {
			t.Fatalf("post-reset didOpen absorbed by phantom refcount (r4-F1 regression): forward=%v err=%v", forward, err)
		}
	}

	// Full interleave through the handler: didClose delivered + in flight over a
	// never-warmed backend; Branch A reaps (resets docRefs, severs the send); the
	// handler's failure-path rollback must leave the map EMPTY and a subsequent
	// didOpen must be FORWARDED (sendCount increments).
	f := &fakeLifecycle{kind: "mcp-language-server", firstSendGate: make(chan struct{})}
	p, regPath := newTestProxy(t, "mcp-language-server", f)
	h := p.Handler()

	// A prior open is outstanding so the didClose is the forwarded 1->0 close.
	p.docRefsMu.Lock()
	p.docRefs[uri] = 1
	p.docRefsMu.Unlock()

	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- postDocNotification(t, h, "textDocument/didClose", uri) }()
	waitForCond(t, "didClose delivered + in flight", func() bool { return f.sendCount.Load() == 1 })

	// Branch A: age the publish and reap — severs the in-flight send
	// ("backend host stopped", NOT DeadlineExceeded, so the probation-202 branch
	// is skipped and the failure-path rollback runs), resets docRefs, bumps gen.
	p.mu.Lock()
	p.endpointPublishedAt = time.Now().Add(-time.Hour)
	p.mu.Unlock()
	p.reapWedgedProbation(time.Now().UTC())
	<-done

	p.docRefsMu.Lock()
	n := len(p.docRefs)
	p.docRefsMu.Unlock()
	if n != 0 {
		t.Fatalf("docRefs = %d entries after severed didClose rollback, want 0 (phantom refcount, r4-F1)", n)
	}
	if e := readEntry(t, regPath); e.Lifecycle != api.LifecycleFailed {
		t.Fatalf("lifecycle after reap = %q, want Failed", e.Lifecycle)
	}

	// A subsequent didOpen is a genuine first-open on the fresh backend and MUST
	// be forwarded (send #2; the fake gates only send #1).
	rr := postDocNotification(t, h, "textDocument/didOpen", uri)
	if rr.Code != http.StatusAccepted {
		t.Fatalf("post-reap didOpen code = %d, want 202; body=%s", rr.Code, rr.Body.String())
	}
	if got := f.sendCount.Load(); got != 2 {
		t.Fatalf("sendCount = %d, want 2 (didOpen absorbed by phantom refcount — never forwarded, r4-F1)", got)
	}
}

// TestLazyProxy_Watchdog_RunsWithColdStartGateDisabled is r4-F2: the probation
// watchdog TICKER must run whenever probation is configured, INDEPENDENT of the
// cold-start gate — ColdStartConcurrency<0 (documented gate-disable) + idle-ttl=0
// previously left watchdogOn false, so a wedged never-warmed backend held its
// Starting row + live port forever against the MaterializedHardCap.
func TestLazyProxy_Watchdog_RunsWithColdStartGateDisabled(t *testing.T) {
	f := &fakeLifecycle{kind: "mcp-language-server"}
	regPath := filepath.Join(t.TempDir(), "r.yaml")
	seed := api.NewRegistry(regPath)
	seed.Put(api.WorkspaceEntry{WorkspaceKey: "abcd1234", WorkspacePath: "D:/test/ws", Language: "python", Backend: "mcp-language-server", TaskName: "mcp-local-hub-lsp-abcd1234-python", Lifecycle: api.LifecycleStarting})
	if err := seed.Save(); err != nil {
		t.Fatalf("seed: %v", err)
	}
	// Explicit tiny tiers keep the ordering clamps quiet (20ms>10ms, 50ms>20ms)
	// so the constructor-time watchdog config is exactly what the test sets.
	p := NewLazyProxy(LazyProxyConfig{
		WorkspaceKey: "abcd1234", WorkspacePath: "D:/test/ws", Language: "python",
		BackendKind: "mcp-language-server", Lifecycle: f, RegistryPath: regPath,
		MaterializeWaitBudget:  10 * time.Millisecond,
		ColdRequestHoldCeiling: 20 * time.Millisecond,
		ColdStartMaxProbation:  50 * time.Millisecond,
		ColdStartConcurrency:   -1, // gate DISABLED — watchdog must still run
		IdleBackendTTL:         0,  // idle reaping off — ticker is watchdog-only
		IdleBackendCheckEvery:  10 * time.Millisecond,
	})
	stopProxyOnCleanup(t, p)

	// Wedged never-warmed backend published long ago.
	p.mu.Lock()
	p.endpoint = &fakeEndpoint{parent: f}
	p.endpointGeneration++
	p.warmed = false
	p.endpointPublishedAt = time.Now().Add(-time.Minute)
	p.mu.Unlock()

	// The watchdog TICKER (not a direct reap call) must tear it down.
	waitForCond(t, "watchdog reaped the wedged backend with the gate disabled", func() bool {
		return readEntry(t, regPath).Lifecycle == api.LifecycleFailed
	})
	if sc := f.stopCount.Load(); sc < 1 {
		t.Fatalf("Lifecycle.Stop count = %d, want >= 1 (watchdog teardown)", sc)
	}
}

// TestLazyProxy_WatchdogBranchB_TransientLoadErrorRetainsOrphanTimer is r4-F4:
// Branch B must NOT clear startingSince when the row verification FAILED to load
// (indistinguishable-from-advanced was the bug) — the orphan timer stays armed and
// the next tick reaps once the registry is readable again.
func TestLazyProxy_WatchdogBranchB_TransientLoadErrorRetainsOrphanTimer(t *testing.T) {
	f := &fakeLifecycle{kind: "mcp-language-server"}
	p, regPath := newTestProxy(t, "mcp-language-server", f)
	p.cfg.ColdStartMaxProbation = 50 * time.Millisecond

	// Orphan: Starting reserved long ago, no endpoint, no flight.
	p.mu.Lock()
	p.endpoint = nil
	p.startingSince = time.Now().Add(-time.Minute)
	p.mu.Unlock()

	// Tick 1: the registry is transiently unreadable (garbage YAML -> Load error).
	if err := os.WriteFile(regPath, []byte("{{{ not yaml"), 0o600); err != nil {
		t.Fatalf("corrupt registry: %v", err)
	}
	p.reapWedgedProbation(time.Now().UTC())
	p.mu.Lock()
	retained := !p.startingSince.IsZero()
	reaping := p.reaping
	p.mu.Unlock()
	if !retained {
		t.Fatal("transient registry load error cleared startingSince — Branch B permanently disarmed (r4-F4 regression)")
	}
	if reaping {
		t.Fatal("reaping flag not restored after load-error early return")
	}
	if sc := f.stopCount.Load(); sc != 0 {
		t.Fatalf("load-error tick tore down the lifecycle: stopCount=%d", sc)
	}

	// Heal the registry with the genuine orphan Starting row.
	seed := api.NewRegistry(regPath)
	seed.Put(api.WorkspaceEntry{WorkspaceKey: "abcd1234", WorkspacePath: "D:/test/ws", Language: "python", Backend: "mcp-language-server", TaskName: "mcp-local-hub-lsp-abcd1234-python", Lifecycle: api.LifecycleStarting})
	if err := seed.Save(); err != nil {
		t.Fatalf("heal registry: %v", err)
	}

	// Tick 2: the retained timer lets the reap proceed -> row Failed, timer cleared.
	p.reapWedgedProbation(time.Now().UTC())
	if e := readEntry(t, regPath); e.Lifecycle != api.LifecycleFailed {
		t.Fatalf("lifecycle after healed tick = %q, want Failed (orphan reaped on retry)", e.Lifecycle)
	}
	p.mu.Lock()
	cleared := p.startingSince.IsZero()
	p.mu.Unlock()
	if !cleared {
		t.Fatal("startingSince not cleared after the reap committed")
	}
	if sc := f.stopCount.Load(); sc < 1 {
		t.Fatalf("Lifecycle.Stop count = %d, want >= 1 (orphan teardown)", sc)
	}
}

// --- Edge 1 (idle-reap Configured-stomp) + Edge 2 (warm doc-lifecycle
// --- delivered-then-cancel refcount desync) test doubles + tests -------------

// gatedStopLifecycle lets a test pause an idle reap mid-teardown: Materialize
// returns instantly (so a tools/call can warm the backend), but Stop() blocks on
// releaseStop after signaling stopEntered — so the reap is parked AFTER its
// generation bump + endpoint-nil but BEFORE its terminal Configured write.
type gatedStopLifecycle struct {
	stopEntered chan struct{}
	releaseStop chan struct{}
	enteredOnce sync.Once
	stopCount   atomic.Int32
}

func (f *gatedStopLifecycle) Kind() string { return "mcp-language-server" }

func (f *gatedStopLifecycle) Materialize(ctx context.Context) (MCPEndpoint, error) {
	return &fakeEndpoint{parent: &fakeLifecycle{kind: "mcp-language-server"}}, nil
}

func (f *gatedStopLifecycle) Stop() error {
	f.stopCount.Add(1)
	f.enteredOnce.Do(func() { close(f.stopEntered) })
	<-f.releaseStop
	return nil
}

// warmDocLifecycle models a WARM backend for the doc-lifecycle refcount tests.
// Non-doc sends (the warming tools/call) return success instantly. The FIRST
// didOpen/didClose send signals docReached (delivered) and then either blocks on
// the request ctx and returns ctx.Err() (docBlockFirstOnCtx: DELIVERED-then-
// await-cut; first-only so a post-teardown re-forward completes normally) or
// returns docErr immediately (a pre-delivery send failure). docSendCount counts
// only doc sends that actually reached the backend — the refcount-retained
// assertion checks it does not climb on an absorbed duplicate open. backendDead
// drives the BackendAlive probe: true models the child having exited at the same
// instant the ctx arm won SendRPC's select (PR #492 r2 P2).
type warmDocLifecycle struct {
	kind               string
	docBlockFirstOnCtx bool
	docErr             error

	docReached   chan struct{}
	reachedOnce  sync.Once
	sendCount    atomic.Int32
	docSendCount atomic.Int32
	stopCount    atomic.Int32
	backendDead  atomic.Bool
}

func (f *warmDocLifecycle) Kind() string { return f.kind }

func (f *warmDocLifecycle) Materialize(ctx context.Context) (MCPEndpoint, error) {
	return &warmDocEndpoint{parent: f}, nil
}

func (f *warmDocLifecycle) Stop() error { f.stopCount.Add(1); return nil }

type warmDocEndpoint struct {
	parent *warmDocLifecycle
	closed atomic.Bool
}

func (e *warmDocEndpoint) SendRequest(ctx context.Context, req *JSONRPCRequest) (*JSONRPCResponse, error) {
	if e.closed.Load() {
		return nil, errors.New("endpoint closed")
	}
	e.parent.sendCount.Add(1)
	if req.Method == "textDocument/didOpen" || req.Method == "textDocument/didClose" {
		n := e.parent.docSendCount.Add(1)
		e.parent.reachedOnce.Do(func() {
			if e.parent.docReached != nil {
				close(e.parent.docReached)
			}
		})
		if e.parent.docBlockFirstOnCtx && n == 1 {
			<-ctx.Done() // delivered; the await outlives the client → ctx.Err()
			return nil, ctx.Err()
		}
		if e.parent.docErr != nil {
			return nil, e.parent.docErr
		}
	}
	return &JSONRPCResponse{Jsonrpc: "2.0", ID: req.ID, Result: json.RawMessage(`{"ok":true}`)}, nil
}

func (e *warmDocEndpoint) Close() error { e.closed.Store(true); return nil }

// BackendAlive implements the optional backendAliveProber facet: the probe the
// delivered⇒keep classification consults on a ctx-shaped error (PR #492 r2 P2).
func (e *warmDocEndpoint) BackendAlive() bool { return !e.parent.backendDead.Load() }

// postDocNotificationCtx is postDocNotification with a caller-supplied context so
// a test can cancel the client request AFTER the notification is delivered.
func postDocNotificationCtx(t *testing.T, h http.Handler, method, uri string, ctx context.Context) *httptest.ResponseRecorder {
	t.Helper()
	body := fmt.Sprintf(`{"jsonrpc":"2.0","method":%q,"params":{"textDocument":{"uri":%q}}}`, method, uri)
	req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1/mcp", strings.NewReader(body)).WithContext(ctx)
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	return rr
}

// postRPCCtx is postRPC with a caller-supplied context (e.g. pre-canceled, to
// model a client that disconnected while the send failure surfaced).
func postRPCCtx(t *testing.T, h http.Handler, method string, id int, ctx context.Context) *httptest.ResponseRecorder {
	t.Helper()
	body := fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":%q,"params":{}}`, id, method)
	req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1/mcp", strings.NewReader(body)).WithContext(ctx)
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	return rr
}

// TestLazyProxy_IdleReap_StaleConfiguredWriteNoStompsNewerStarting is the Edge 1
// guard: the idle reaper's terminal Configured write is routed through the
// single-owner gen-guarded reconcile, so a reap whose generation was superseded by
// a concurrent republish/reserve NO-OPs instead of stomping the newer Starting row
// back to Configured for the cold-materialize window (which would make other
// proxies' cold-start gate / hard cap undercount by one).
func TestLazyProxy_IdleReap_StaleConfiguredWriteNoStompsNewerStarting(t *testing.T) {
	f := &gatedStopLifecycle{stopEntered: make(chan struct{}), releaseStop: make(chan struct{})}
	regPath := filepath.Join(t.TempDir(), "r.yaml")
	seed := api.NewRegistry(regPath)
	seed.Put(api.WorkspaceEntry{
		WorkspaceKey: "abcd1234", WorkspacePath: "D:/test/ws", Language: "python",
		Backend: "mcp-language-server", TaskName: "mcp-local-hub-lsp-abcd1234-python", Lifecycle: "",
	})
	if err := seed.Save(); err != nil {
		t.Fatalf("seed: %v", err)
	}
	p := NewLazyProxy(LazyProxyConfig{
		WorkspaceKey: "abcd1234", WorkspacePath: "D:/test/ws", Language: "python",
		BackendKind: "mcp-language-server", Lifecycle: f, RegistryPath: regPath,
		InflightMinRetryGap:   10 * time.Millisecond,
		ToolsCallDebounce:     100 * time.Millisecond,
		IdleBackendTTL:        50 * time.Millisecond,
		IdleBackendCheckEvery: time.Hour, // no auto-tick; the reap is driven manually
	})
	stopProxyOnCleanup(t, p)
	h := p.Handler()

	// Warm the backend so the idle reaper has an Active row to reap.
	if rr := postRPC(t, h, "tools/call", 1); rr.Code != http.StatusOK {
		t.Fatalf("warm code=%d body=%s", rr.Code, rr.Body.String())
	}
	if e := readEntry(t, regPath); e.Lifecycle != api.LifecycleActive {
		t.Fatalf("setup lifecycle=%q, want Active", e.Lifecycle)
	}

	// Drive the reap with a far-future `now` so the idle-TTL check passes. It bumps
	// the generation, nils the endpoint, then blocks in the gated Stop() BEFORE its
	// terminal Configured write.
	reapDone := make(chan struct{})
	go func() { p.reapIdleBackend(time.Now().Add(time.Hour)); close(reapDone) }()
	<-f.stopEntered

	// While the reap is parked mid-teardown, a materialize republishes an endpoint
	// and claims the row Starting under a NEWER generation — exactly what
	// publishMaterializedEndpoint does. In production the reaping flag serializes
	// this; here the superseded-generation state is constructed directly to prove
	// the gen guard. The reap's captured generation is now stale.
	p.mu.Lock()
	p.endpoint = &fakeEndpoint{parent: &fakeLifecycle{kind: "mcp-language-server"}}
	p.endpointGeneration++
	p.lastWrittenLifecycle = ""
	p.warmed = false
	p.endpointPublishedAt = time.Now()
	p.startingSince = time.Time{}
	newGen := p.endpointGeneration
	p.reconcileRegistryLifecycleLocked(newGen) // sanctioned Starting write (endpoint!=nil, !warmed)
	p.mu.Unlock()
	if e := readEntry(t, regPath); e.Lifecycle != api.LifecycleStarting {
		t.Fatalf("post-republish lifecycle=%q, want Starting", e.Lifecycle)
	}

	// Let the stale reap finish; its gen-guarded Configured write must no-op.
	close(f.releaseStop)
	<-reapDone

	if e := readEntry(t, regPath); e.Lifecycle != api.LifecycleStarting {
		t.Fatalf("lifecycle after stale reap = %q, want Starting (stale idle-reap Configured write must not stomp the newer row — Edge 1)", e.Lifecycle)
	}
}

// TestLazyProxy_RejectedPublish_ClearsStaleStartingSince_ReapSettlesConfigured is
// the bot PR #492 r1 P2 guard: a flight that RESERVED (flock Starting write +
// markStartingReserved) and then died at the publish-rejection gate
// (errLazyProxyUnpublishable, mid-reap) must NOT leave its startingSince marker
// armed — the reap's final single-owner reconcile would otherwise derive a phantom
// Starting row (no endpoint, no active flight) that counts against every other
// proxy's cold-start gate / hard cap until Branch B's ≤5m probation timer. The
// rejection branch zeroes the marker at the flight's terminal moment, so the reap
// settles the row to Configured and a fresh cold request finds a clean slate.
func TestLazyProxy_RejectedPublish_ClearsStaleStartingSince_ReapSettlesConfigured(t *testing.T) {
	f := &gatedStopLifecycle{stopEntered: make(chan struct{}), releaseStop: make(chan struct{})}
	regPath := filepath.Join(t.TempDir(), "r.yaml")
	seed := api.NewRegistry(regPath)
	seed.Put(api.WorkspaceEntry{
		WorkspaceKey: "abcd1234", WorkspacePath: "D:/test/ws", Language: "python",
		Backend: "mcp-language-server", TaskName: "mcp-local-hub-lsp-abcd1234-python", Lifecycle: "",
	})
	if err := seed.Save(); err != nil {
		t.Fatalf("seed: %v", err)
	}
	p := NewLazyProxy(LazyProxyConfig{
		WorkspaceKey: "abcd1234", WorkspacePath: "D:/test/ws", Language: "python",
		BackendKind: "mcp-language-server", Lifecycle: f, RegistryPath: regPath,
		InflightMinRetryGap:   10 * time.Millisecond,
		ToolsCallDebounce:     100 * time.Millisecond,
		IdleBackendTTL:        50 * time.Millisecond,
		IdleBackendCheckEvery: time.Hour, // no auto-tick; the reap is driven manually
	})
	stopProxyOnCleanup(t, p)
	h := p.Handler()

	// Warm the backend so the idle reaper has an Active row to reap.
	if rr := postRPC(t, h, "tools/call", 1); rr.Code != http.StatusOK {
		t.Fatalf("warm code=%d body=%s", rr.Code, rr.Body.String())
	}
	if e := readEntry(t, regPath); e.Lifecycle != api.LifecycleActive {
		t.Fatalf("setup lifecycle=%q, want Active", e.Lifecycle)
	}

	// Park the reap mid-teardown: it bumps the generation, nils the endpoint, then
	// blocks in the gated Stop() BEFORE its final reconcile.
	reapDone := make(chan struct{})
	go func() { p.reapIdleBackend(time.Now().Add(time.Hour)); close(reapDone) }()
	<-f.stopEntered

	// Model the pre-reap-cohort caller mid-flow during the reap window: its
	// reserve writes the flock Starting row and markStartingReserved arms
	// startingSince (endpoint is nil while the reap is in flight).
	if _, err := p.reserveMaterializedSlot(); err != nil {
		t.Fatalf("reserve during reap window: %v", err)
	}
	p.markStartingReserved(time.Now().UTC())
	if e := readEntry(t, regPath); e.Lifecycle != api.LifecycleStarting {
		t.Fatalf("post-reserve lifecycle=%q, want Starting", e.Lifecycle)
	}

	// The cohort flight's materialize reaches publishMaterializedEndpoint while
	// p.reaping is still held → rejection (errLazyProxyUnpublishable). The FIX
	// zeroes startingSince under the same p.mu hold that observed reaping, BEFORE
	// the rejection's own (gated, blocking) Lifecycle.Stop call.
	pubErr := make(chan error, 1)
	go func() {
		pubErr <- p.publishMaterializedEndpoint(&fakeEndpoint{parent: &fakeLifecycle{kind: "mcp-language-server"}})
	}()
	// stopCount 2 = the rejection path entered its Stop → its marker-clear already ran.
	waitForCond(t, "publish rejection reached its lifecycle Stop", func() bool {
		return f.stopCount.Load() >= 2
	})

	// Release both parked Stops; the reap's final reconcile now derives the row
	// state with the dead flight's marker GONE.
	close(f.releaseStop)
	<-reapDone
	if err := <-pubErr; !errors.Is(err, errLazyProxyUnpublishable) {
		t.Fatalf("publish during reap returned %v, want errLazyProxyUnpublishable", err)
	}

	if e := readEntry(t, regPath); e.Lifecycle != api.LifecycleConfigured {
		t.Fatalf("lifecycle after reap = %q, want Configured (dead flight's stale startingSince must not persist a phantom Starting — PR #492 r1 P2)", e.Lifecycle)
	}
	p.mu.Lock()
	marker := p.startingSince
	p.mu.Unlock()
	if !marker.IsZero() {
		t.Fatalf("startingSince = %v after rejected publish, want zero (flight-terminal clear)", marker)
	}

	// A fresh cold request must find a clean slate (Configured row — the state the
	// cold-start gate / hard cap scans — not a phantom Starting) and warm normally,
	// proving the normal publish path still reserves, publishes, and reconciles
	// through to Active.
	if rr := postRPC(t, h, "tools/call", 2); rr.Code != http.StatusOK {
		t.Fatalf("fresh cold request after reap: code=%d body=%s", rr.Code, rr.Body.String())
	}
	if e := readEntry(t, regPath); e.Lifecycle != api.LifecycleActive {
		t.Fatalf("lifecycle after fresh warm = %q, want Active", e.Lifecycle)
	}
}

// TestLazyProxy_DocLifecycle_WarmDeliveredThenClientCancel_KeepsRefcount is the
// Edge 2 guard: on the WARM notification path (raw client ctx, no probation budget),
// a didOpen that is DELIVERED to the backend and then outlives the client deadline
// must answer 202 with the refcount KEPT — not roll it back (which desynced the
// count and duplicated the next upstream open). The backend stays ALIVE here
// (backendDead false), so this also pins the probe-positive side of the PR #492
// r2 P2 guard: a pure post-delivery cancel with a live backend keeps exactly as
// before.
func TestLazyProxy_DocLifecycle_WarmDeliveredThenClientCancel_KeepsRefcount(t *testing.T) {
	f := &warmDocLifecycle{kind: "mcp-language-server", docBlockFirstOnCtx: true, docReached: make(chan struct{})}
	p, _ := newTestProxy(t, "mcp-language-server", nil)
	p.cfg.Lifecycle = f
	h := p.Handler()
	const uri = "file:///ws/foo.go"

	// Warm so the doc forward runs on the WARM path.
	if rr := postRPC(t, h, "tools/call", 1); rr.Code != http.StatusOK {
		t.Fatalf("warm tools/call code=%d body=%s", rr.Code, rr.Body.String())
	}
	if !p.isWarmed() {
		t.Fatal("backend not warmed after tools/call")
	}

	// First didOpen: delivered, then the client cancels while the backend is still
	// (slowly) processing. Delivered → 202 with the refcount kept.
	ctx, cancel := context.WithCancel(context.Background())
	rrCh := make(chan *httptest.ResponseRecorder, 1)
	go func() { rrCh <- postDocNotificationCtx(t, h, "textDocument/didOpen", uri, ctx) }()
	<-f.docReached // delivered to the backend
	cancel()       // client gives up AFTER delivery
	rr := <-rrCh
	if rr.Code != http.StatusAccepted {
		t.Fatalf("delivered-then-cancel didOpen code = %d, want 202 (delivered, refcount kept); body=%s", rr.Code, rr.Body.String())
	}
	if sc := f.stopCount.Load(); sc != 0 {
		t.Fatalf("client-cancel after delivery tore down the backend: stopCount=%d", sc)
	}
	if got := f.docSendCount.Load(); got != 1 {
		t.Fatalf("docSendCount = %d after first didOpen, want 1", got)
	}

	// The refcount was KEPT (0->1 retained), so a duplicate didOpen for the same URI
	// is ABSORBED (1->2) and never re-forwarded. Under the desync bug the first open
	// was rolled back, so this would be a fresh 0->1 forward reaching the backend
	// again (docSendCount climbs to 2). A bounded ctx keeps the assertion from
	// hanging on the bug path (where the second open forwards and blocks on ctx).
	ctx2, cancel2 := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel2()
	rr2 := postDocNotificationCtx(t, h, "textDocument/didOpen", uri, ctx2)
	if rr2.Code != http.StatusAccepted {
		t.Fatalf("duplicate didOpen code = %d, want 202 (absorbed); body=%s", rr2.Code, rr2.Body.String())
	}
	if got := f.docSendCount.Load(); got != 1 {
		t.Fatalf("docSendCount = %d after duplicate didOpen, want 1 (first open's refcount must be retained — no duplicate upstream open)", got)
	}
}

// TestLazyProxy_DocLifecycle_WarmPreDeliveryFailure_RollsBackAndTearsDown proves
// the Edge 2 delivered⇒keep classification does NOT swallow a genuine send failure:
// a didOpen whose send fails with a NON-context error before the write commits is
// NOT delivered, so the handler still rolls back the refcount and tears the backend
// down (row Failed, 200 JSON-RPC error — never a 202-delivered).
func TestLazyProxy_DocLifecycle_WarmPreDeliveryFailure_RollsBackAndTearsDown(t *testing.T) {
	f := &warmDocLifecycle{kind: "mcp-language-server", docErr: errors.New("backend host stopped"), docReached: make(chan struct{})}
	p, regPath := newTestProxy(t, "mcp-language-server", nil)
	p.cfg.Lifecycle = f
	h := p.Handler()
	const uri = "file:///ws/foo.go"

	if rr := postRPC(t, h, "tools/call", 1); rr.Code != http.StatusOK {
		t.Fatalf("warm tools/call code=%d body=%s", rr.Code, rr.Body.String())
	}
	if !p.isWarmed() {
		t.Fatal("backend not warmed after tools/call")
	}

	rr := postDocNotificationCtx(t, h, "textDocument/didOpen", uri, context.Background())
	if rr.Code != http.StatusOK {
		t.Fatalf("pre-delivery-failed didOpen code = %d, want 200 (JSON-RPC error, NOT 202-delivered); body=%s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "backend host stopped") {
		t.Fatalf("error body = %s, want it to carry the backend failure", rr.Body.String())
	}
	if sc := f.stopCount.Load(); sc != 1 {
		t.Fatalf("stopCount = %d after pre-delivery send failure, want 1 (teardown must fire)", sc)
	}
	if e := readEntry(t, regPath); e.Lifecycle != api.LifecycleFailed {
		t.Fatalf("lifecycle after send failure = %q, want Failed", e.Lifecycle)
	}
	p.docRefsMu.Lock()
	n := len(p.docRefs)
	p.docRefsMu.Unlock()
	if n != 0 {
		t.Fatalf("docRefs = %d entries after failed didOpen, want 0 (rolled back + teardown reset)", n)
	}
}

// TestLazyProxy_DocLifecycle_CancelWithDeadBackend_TearsDownNotKeep is the bot
// PR #492 r2 P2 guard: Go's select picks pseudo-randomly among READY cases, so a
// doc notification whose client cancel fires at the same instant the subprocess
// dies can surface as a ctx error from SendRPC even though done/childExited was
// ALSO ready. The delivered⇒keep classification must NOT keep on that shape —
// keeping would cache a DEAD endpoint (onSendFailure skipped) and retain a
// refcount the dead backend no longer honors, so the next didOpen for the uri
// would be absorbed (never forwarded) until some unrelated forward tripped
// teardown. Select-arm forcing is impractical, so the test engineers the
// observable state the race produces: err = ctx.Canceled AND the backend's death
// probe reads dead when the classification runs → assert the FAILURE path
// (rollback + teardown + row Failed + error response, never 202), and a fresh
// didOpen afterwards re-materializes and forwards. The probe-positive twin
// (ctx.Canceled + live backend → 202 + keep) is pinned by
// TestLazyProxy_DocLifecycle_WarmDeliveredThenClientCancel_KeepsRefcount.
func TestLazyProxy_DocLifecycle_CancelWithDeadBackend_TearsDownNotKeep(t *testing.T) {
	f := &warmDocLifecycle{kind: "mcp-language-server", docBlockFirstOnCtx: true, docReached: make(chan struct{})}
	p, regPath := newTestProxy(t, "mcp-language-server", nil)
	p.cfg.Lifecycle = f
	h := p.Handler()
	const uri = "file:///ws/foo.go"

	// Warm so the doc forward runs on the WARM path (raw client ctx).
	if rr := postRPC(t, h, "tools/call", 1); rr.Code != http.StatusOK {
		t.Fatalf("warm tools/call code=%d body=%s", rr.Code, rr.Body.String())
	}
	if !p.isWarmed() {
		t.Fatal("backend not warmed after tools/call")
	}

	// didOpen delivered, then the backend dies AND the client cancels — SendRPC
	// returns the ctx arm (the fake models select picking ctx over childExited).
	ctx, cancel := context.WithCancel(context.Background())
	rrCh := make(chan *httptest.ResponseRecorder, 1)
	go func() { rrCh <- postDocNotificationCtx(t, h, "textDocument/didOpen", uri, ctx) }()
	<-f.docReached            // delivered to the backend
	f.backendDead.Store(true) // child exits...
	cancel()                  // ...as the client cancel fires (both arms "ready")
	rr := <-rrCh

	// FAILURE path, not delivered⇒keep: error response (JSON-RPC 200 envelope),
	// teardown fired, row Failed, refcount not retained.
	if rr.Code == http.StatusAccepted {
		t.Fatalf("cancel-with-dead-backend didOpen answered 202 (delivered⇒keep) — dead endpoint cached + refcount desync (PR #492 r2 P2); body=%s", rr.Body.String())
	}
	if rr.Code != http.StatusOK {
		t.Fatalf("cancel-with-dead-backend didOpen code = %d, want 200 (JSON-RPC error envelope); body=%s", rr.Code, rr.Body.String())
	}
	if body := rr.Body.String(); !strings.Contains(body, "backend dead at doc-notification cancel/deadline") {
		t.Fatalf("error body = %s, want the dead-backend classification message", body)
	}
	if sc := f.stopCount.Load(); sc != 1 {
		t.Fatalf("stopCount = %d, want 1 (onSendFailure teardown must fire — dead endpoint must not stay cached)", sc)
	}
	if e := readEntry(t, regPath); e.Lifecycle != api.LifecycleFailed {
		t.Fatalf("lifecycle = %q, want Failed (teardown stamps the registry)", e.Lifecycle)
	}
	p.docRefsMu.Lock()
	n := len(p.docRefs)
	p.docRefsMu.Unlock()
	if n != 0 {
		t.Fatalf("docRefs = %d entries, want 0 (rollback + teardown reset — no phantom refcount against the dead backend)", n)
	}

	// The next didOpen must re-materialize a FRESH backend and forward (0->1) —
	// under the keep bug it would be absorbed by the retained refcount and the
	// dead endpoint would still be cached. docBlockFirstOnCtx blocks only the
	// first doc send, so this re-forward completes; the fake's backend is healthy
	// again for the fresh endpoint.
	f.backendDead.Store(false)
	time.Sleep(20 * time.Millisecond) // past the inflight retry-throttle gap
	rr2 := postDocNotificationCtx(t, h, "textDocument/didOpen", uri, context.Background())
	if rr2.Code != http.StatusAccepted {
		t.Fatalf("fresh didOpen after teardown code = %d, want 202; body=%s", rr2.Code, rr2.Body.String())
	}
	if got := f.docSendCount.Load(); got != 2 {
		t.Fatalf("docSendCount = %d, want 2 (fresh didOpen must FORWARD to the re-materialized backend, not be absorbed)", got)
	}
	if e := readEntry(t, regPath); e.Lifecycle != api.LifecycleActive {
		t.Fatalf("lifecycle after fresh forward = %q, want Active", e.Lifecycle)
	}
}

// TestLazyProxy_DocLifecycle_BrokenPipeUnderCanceledCtx_RollsBackAndTearsDown is
// the bot PR #492 r3 P2 guard (the ROOT defect): the delivered classification
// must key on the ERROR IDENTITY, never on ambient ctx state. A broken-pipe
// send failure (the notification was NEVER written) arriving while the client
// ctx happens to be already canceled classified as delivered⇒keep under the old
// ctx-state isClientCancelErr — composed with an alive-reading probe (the
// procExited drain window), that kept a refcount for an undelivered
// notification PERMANENTLY (the retained count absorbs the retry, so no
// subsequent send ever fires onSendFailure/resetDocRefs). With the
// identity-keyed predicate the io error classifies NOT-delivered regardless of
// ctx state → rollback + teardown.
func TestLazyProxy_DocLifecycle_BrokenPipeUnderCanceledCtx_RollsBackAndTearsDown(t *testing.T) {
	f := &warmDocLifecycle{kind: "mcp-language-server", docErr: errors.New("write stdin: broken pipe"), docReached: make(chan struct{})}
	// backendDead stays FALSE: the probe reads ALIVE (models the drain window the
	// bot composed with — the classification must not even reach the probe).
	p, regPath := newTestProxy(t, "mcp-language-server", nil)
	p.cfg.Lifecycle = f
	h := p.Handler()
	const uri = "file:///ws/foo.go"

	// Warm so the doc forward runs on the WARM path (raw client ctx).
	if rr := postRPC(t, h, "tools/call", 1); rr.Code != http.StatusOK {
		t.Fatalf("warm tools/call code=%d body=%s", rr.Code, rr.Body.String())
	}
	if !p.isWarmed() {
		t.Fatal("backend not warmed after tools/call")
	}

	// didOpen under a PRE-CANCELED client ctx; the send fails with a non-context
	// io error (never delivered). Old predicate: ctx.Err()!=nil → "client cancel"
	// → 202 + keep (phantom). New predicate: identity fails → failure path.
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	rr := postDocNotificationCtx(t, h, "textDocument/didOpen", uri, canceled)
	if rr.Code == http.StatusAccepted {
		t.Fatalf("broken-pipe under canceled ctx answered 202 (delivered⇒keep for an UNDELIVERED notification — PR #492 r3 P2); body=%s", rr.Body.String())
	}
	if rr.Code != http.StatusOK {
		t.Fatalf("broken-pipe under canceled ctx code = %d, want 200 (JSON-RPC error envelope); body=%s", rr.Code, rr.Body.String())
	}
	if body := rr.Body.String(); !strings.Contains(body, "broken pipe") {
		t.Fatalf("error body = %s, want it to carry the io failure", body)
	}
	if sc := f.stopCount.Load(); sc != 1 {
		t.Fatalf("stopCount = %d, want 1 (undelivered send failure must tear down)", sc)
	}
	if e := readEntry(t, regPath); e.Lifecycle != api.LifecycleFailed {
		t.Fatalf("lifecycle = %q, want Failed", e.Lifecycle)
	}
	p.docRefsMu.Lock()
	n := len(p.docRefs)
	p.docRefsMu.Unlock()
	if n != 0 {
		t.Fatalf("docRefs = %d entries, want 0 (refcount for an undelivered didOpen must be rolled back)", n)
	}

	// A later didOpen must be a genuine first-open forward on a fresh backend —
	// under the phantom bug it would be absorbed forever.
	f.docErr = nil
	time.Sleep(20 * time.Millisecond) // past the inflight retry-throttle gap
	rr2 := postDocNotificationCtx(t, h, "textDocument/didOpen", uri, context.Background())
	if rr2.Code != http.StatusAccepted {
		t.Fatalf("fresh didOpen after teardown code = %d, want 202; body=%s", rr2.Code, rr2.Body.String())
	}
	if got := f.docSendCount.Load(); got != 2 {
		t.Fatalf("docSendCount = %d, want 2 (fresh didOpen must FORWARD, not be absorbed by a phantom refcount)", got)
	}
}

// TestLazyProxy_ToolsCall_BrokenPipeUnderCanceledCtx_TearsDownGenericError is
// the request-path arm of the PR #492 r3 P2 root fix: the same identity-keyed
// isClientCancelErr serves handleToolsCall/handleForward, so a broken-pipe send
// failure under an already-canceled client ctx must take the BACKEND-FAILURE
// path (teardown + generic -32603) — never the client-cancel skip-teardown
// branch (which left a dead endpoint cached), and never the non-retryable
// "delivered" 500 (isColdHoldCeilingDeadline requires a DeadlineExceeded
// identity + live client, so an io error cannot reach it).
func TestLazyProxy_ToolsCall_BrokenPipeUnderCanceledCtx_TearsDownGenericError(t *testing.T) {
	f := &fakeLifecycle{kind: "mcp-language-server"}
	p, regPath := newTestProxy(t, "mcp-language-server", f)
	h := p.Handler()

	// Warm first (success path), then arm the broken-pipe send failure.
	if rr := postRPC(t, h, "tools/call", 1); rr.Code != http.StatusOK {
		t.Fatalf("warm tools/call code=%d body=%s", rr.Code, rr.Body.String())
	}
	if e := readEntry(t, regPath); e.Lifecycle != api.LifecycleActive {
		t.Fatalf("setup lifecycle=%q, want Active", e.Lifecycle)
	}
	f.sendRequestErr = errors.New("write stdin: broken pipe")

	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	rr := postRPCCtx(t, h, "tools/call", 2, canceled)
	if rr.Code == http.StatusInternalServerError {
		t.Fatalf("broken-pipe under canceled ctx returned the non-retryable delivered 500 (duplicate-safety inverted); body=%s", rr.Body.String())
	}
	if rr.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200 (generic JSON-RPC error envelope); body=%s", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	if !strings.Contains(body, "broken pipe") {
		t.Fatalf("error body = %s, want the io failure", body)
	}
	if strings.Contains(body, "delivered") {
		t.Fatalf("error body claims delivery for an undelivered request: %s", body)
	}
	if sc := f.stopCount.Load(); sc != 1 {
		t.Fatalf("stopCount = %d, want 1 (backend send failure must tear down even under a canceled client ctx)", sc)
	}
	if e := readEntry(t, regPath); e.Lifecycle != api.LifecycleFailed {
		t.Fatalf("lifecycle = %q, want Failed (dead endpoint must be evicted + stamped, not left cached)", e.Lifecycle)
	}
}

// TestLazyProxy_ToolsCall_PendingCapSaturation_Retryable503NoTeardown covers
// the caller layer of work-items/bugs/2026-07-02-sendrpc-pending-uncapped.md
// (fable pre-bot P1): a SendRPC pending-cap refusal (ErrTooManyPending) is a
// THIRD identity class — the backend is HEALTHY and nothing was written
// (pre-delivery), so the handler must answer a retryable 503 (Retry-After)
// and must NOT run onSendFailure. Tearing down a merely-saturated backend
// would kill every delivered in-flight request (up to maxPendingRequests of
// them, each possibly partially executed) and re-enter the same fan-out
// cold — a self-inflicted outage loop.
func TestLazyProxy_ToolsCall_PendingCapSaturation_Retryable503NoTeardown(t *testing.T) {
	f := &fakeLifecycle{kind: "mcp-language-server"}
	p, regPath := newTestProxy(t, "mcp-language-server", f)
	h := p.Handler()

	// Warm first (success path), then arm the saturation refusal.
	if rr := postRPC(t, h, "tools/call", 1); rr.Code != http.StatusOK {
		t.Fatalf("warm tools/call code=%d body=%s", rr.Code, rr.Body.String())
	}
	f.sendRequestErr = fmt.Errorf("%w (128 in flight)", ErrTooManyPending)

	rr := postRPC(t, h, "tools/call", 2)
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("code = %d, want 503 (retryable saturation refusal); body=%s", rr.Code, rr.Body.String())
	}
	if ra := rr.Header().Get("Retry-After"); ra == "" {
		t.Fatal("saturation 503 must carry Retry-After (the refusal is pre-delivery, retry-safe)")
	}
	if sc := f.stopCount.Load(); sc != 0 {
		t.Fatalf("stopCount = %d, want 0 (a saturated backend is HEALTHY — teardown here is the self-inflicted-outage class)", sc)
	}
	if e := readEntry(t, regPath); e.Lifecycle != api.LifecycleActive {
		t.Fatalf("lifecycle = %q, want Active retained (no Failed stamp for saturation)", e.Lifecycle)
	}

	// Saturation clears → the SAME cached endpoint serves the retry (no
	// re-materialization happened during the refusal).
	f.sendRequestErr = nil
	if rr := postRPC(t, h, "tools/call", 3); rr.Code != http.StatusOK {
		t.Fatalf("retry after saturation cleared: code=%d body=%s", rr.Code, rr.Body.String())
	}
}
