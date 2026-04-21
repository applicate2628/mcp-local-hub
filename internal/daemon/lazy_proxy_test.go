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
	e.parent.sendCount.Add(1)
	if e.parent.sendRequestErr != nil {
		return nil, e.parent.sendRequestErr
	}
	res := e.parent.sendResultRaw
	if len(res) == 0 {
		res = json.RawMessage(`{"ok":true}`)
	}
	return &JSONRPCResponse{Jsonrpc: "2.0", ID: req.ID, Result: res}, nil
}

func (e *fakeEndpoint) Close() error { e.closed.Store(true); return nil }

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
	req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
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
	req := httptest.NewRequestWithContext(ctx, http.MethodPost, "/mcp", bytes.NewReader(body))
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
