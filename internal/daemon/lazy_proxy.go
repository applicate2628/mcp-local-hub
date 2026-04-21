package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"mcp-local-hub/internal/api"
)

// LazyProxyConfig describes one lazy-proxy instance. One proxy per
// (workspace, language) tuple; the scheduler task launches one proxy per
// registered tuple at user login.
type LazyProxyConfig struct {
	// WorkspaceKey is the 8-char deterministic hash of the canonical workspace
	// path (see api.WorkspaceKey). Used as the registry primary-key component
	// and as the key used by the inflight gate (`<WorkspaceKey>|<Language>`).
	WorkspaceKey string
	// WorkspacePath is the full canonical absolute path of the workspace.
	// Retained for diagnostics; not used for routing.
	WorkspacePath string
	// Language is the manifest-declared language slug ("python", "go", ...).
	Language string
	// BackendKind is "mcp-language-server" | "gopls-mcp". Drives synthetic
	// catalog selection via api.ToolCatalogForBackend.
	BackendKind string
	// Port is the TCP port to bind on 127.0.0.1. Assigned by the registry's
	// port allocator at register time.
	Port int
	// Lifecycle materializes the heavy backend on first tools/call.
	Lifecycle BackendLifecycle
	// RegistryPath is the workspace registry YAML file. Lifecycle transitions
	// are written here under flock.
	RegistryPath string

	// InflightMinRetryGap defaults to 2 seconds when zero. Applied to the
	// inflight gate's retry-throttle on materialization failures.
	InflightMinRetryGap time.Duration
	// ToolsCallDebounce defaults to 5 seconds when zero. Only successful-forward
	// LastToolsCallAt registry writes are debounced; lifecycle transitions
	// and LastError are written immediately.
	ToolsCallDebounce time.Duration
}

// LazyProxy is the per-port HTTP proxy that answers synthetic handshake
// traffic (initialize, tools/list, notifications/*) from the embedded tool
// catalog and lazily materializes the heavy backend on first tools/call.
//
// Concurrency invariants:
//   - First tools/call: N concurrent callers collapse to 1 Materialize via
//     the inflight gate (singleflight).
//   - After a successful Materialize, the endpoint is cached in p.endpoint
//     under p.mu; subsequent tools/calls hit the cache without touching the
//     gate.
//   - On send-failure mid-stream, the proxy evicts the cached endpoint and
//     calls gate.Forget so the NEXT tools/call re-materializes (subject to
//     the retry-throttle gap).
//   - LastToolsCallAt registry writes are coalesced to the debounce interval
//     to keep the YAML file churn-free under sustained traffic.
type LazyProxy struct {
	cfg    LazyProxyConfig
	gate   *InflightGate
	server *http.Server

	mu       sync.Mutex
	endpoint MCPEndpoint
	closed   atomic.Bool

	debounceMu         sync.Mutex
	lastToolsCallWrite time.Time
}

// NewLazyProxy constructs a proxy with defaulted InflightMinRetryGap (2s)
// and ToolsCallDebounce (5s) when zero. Returned proxy is not yet listening;
// call ListenAndServe or attach Handler() to an httptest server.
func NewLazyProxy(cfg LazyProxyConfig) *LazyProxy {
	if cfg.InflightMinRetryGap == 0 {
		cfg.InflightMinRetryGap = 2 * time.Second
	}
	if cfg.ToolsCallDebounce == 0 {
		cfg.ToolsCallDebounce = 5 * time.Second
	}
	return &LazyProxy{
		cfg:  cfg,
		gate: NewInflightGate(cfg.InflightMinRetryGap),
	}
}

// Handler returns the http.Handler for the proxy. Exposed for tests so they
// can fire requests via httptest.NewRecorder without real port binding.
// Registered routes:
//   - POST /mcp | /: JSON-RPC over HTTP (primary client path)
//   - GET  /mcp | /: SSE keepalive stream (reserved for future bridge use)
func (p *LazyProxy) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/mcp", p.handleMCP)
	mux.HandleFunc("/", p.handleMCP) // accept both /mcp and / for compatibility
	return mux
}

// ListenAndServe writes the initial LifecycleConfigured state, binds
// 127.0.0.1:<port>, and serves until Stop is called. Returns
// http.ErrServerClosed after a clean Stop.
func (p *LazyProxy) ListenAndServe() error {
	// Initial registry state: proxy up, backend not spawned.
	// Writing Configured is a no-op if the entry is already at that state,
	// and it re-asserts the invariant if the registry was hand-edited.
	_ = api.NewRegistry(p.cfg.RegistryPath).PutLifecycle(
		p.cfg.WorkspaceKey, p.cfg.Language, api.LifecycleConfigured, "")
	p.server = &http.Server{
		Addr:              fmt.Sprintf("127.0.0.1:%d", p.cfg.Port),
		Handler:           p.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		// WriteTimeout 0: handlers own cancellation via r.Context().
	}
	return p.server.ListenAndServe()
}

// Stop closes the materialized endpoint (if any), invokes Lifecycle.Stop to
// tree-kill the backend subprocess, clears gate state, and shuts down the
// HTTP listener within ctx's deadline.
//
// Safe to call multiple times: subsequent calls return nil immediately.
func (p *LazyProxy) Stop(ctx context.Context) error {
	if !p.closed.CompareAndSwap(false, true) {
		return nil
	}
	p.mu.Lock()
	if p.endpoint != nil {
		_ = p.endpoint.Close()
		p.endpoint = nil
	}
	p.mu.Unlock()
	_ = p.cfg.Lifecycle.Stop()
	p.gate.Forget(p.inflightKey())
	if p.server != nil {
		return p.server.Shutdown(ctx)
	}
	return nil
}

func (p *LazyProxy) inflightKey() string {
	return p.cfg.WorkspaceKey + "|" + p.cfg.Language
}

// --- JSON-RPC dispatch -----------------------------------------------------

// handleMCP is the per-request dispatch.
func (p *LazyProxy) handleMCP(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		p.handleSSE(w, r)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 4<<20)) // 4 MiB cap
	if err != nil {
		writeRPCError(w, nil, rpcErrParseError, "read body: "+err.Error())
		return
	}
	var req JSONRPCRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeRPCError(w, nil, rpcErrParseError, "parse error: "+err.Error())
		return
	}
	switch req.Method {
	case "initialize":
		resp, err := api.SyntheticInitializeResponse(req.ID, p.cfg.BackendKind)
		if err != nil {
			writeRPCError(w, req.ID, rpcErrInternalError, err.Error())
			return
		}
		writeJSON(w, resp)
	case "tools/list":
		resp, err := api.SyntheticToolsListResponse(req.ID, p.cfg.BackendKind)
		if err != nil {
			writeRPCError(w, req.ID, rpcErrInternalError, err.Error())
			return
		}
		writeJSON(w, resp)
	case "notifications/initialized", "notifications/cancelled":
		// True JSON-RPC 2.0 notifications: no response expected. Answer
		// with 202 Accepted and empty body — emitting a response with
		// null id confuses strict clients that match id-based envelopes.
		w.WriteHeader(http.StatusAccepted)
	case "ping":
		// ping is a REQUEST per MCP spec: client sends id, expects the
		// same id echoed in the reply. A hard-coded null breaks request
		// correlation in clients' heartbeat/probe logic.
		id := req.ID
		if len(id) == 0 {
			id = json.RawMessage("null")
		}
		writeJSON(w, fmt.Appendf(nil, `{"jsonrpc":"2.0","id":%s,"result":{}}`, string(id)))
	case "tools/call":
		p.handleToolsCall(w, r, &req)
	default:
		// JSON-RPC 2.0 forbids responses to notifications (requests with
		// no id, method prefix "notifications/"). Forwarding one through
		// handleForward would block waiting for a response the backend
		// is spec-bound not to send. Accept with 202 + empty body instead.
		// The two well-known notifications are matched explicitly above;
		// this guard catches future/custom notifications like
		// notifications/progress, notifications/roots/list_changed, etc.
		if strings.HasPrefix(req.Method, "notifications/") {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		// Resources, prompts, and any other non-initialize/list method
		// requires a materialized backend. Treat it like tools/call for
		// materialization semantics but do NOT debounce-write the
		// LastToolsCallAt timestamp.
		p.handleForward(w, r, &req)
	}
}

func (p *LazyProxy) handleToolsCall(w http.ResponseWriter, r *http.Request, req *JSONRPCRequest) {
	ep, err := p.ensureMaterialized(r.Context())
	if err != nil {
		code := rpcErrInternalError
		if IsMissingBinaryErr(err) {
			code = rpcErrMissingBinary
		}
		writeRPCError(w, req.ID, code, err.Error())
		return
	}
	resp, err := ep.SendRequest(r.Context(), req)
	if err != nil {
		// Backend died mid-stream or stdio channel failed. Evict the cached
		// endpoint, clear the inflight gate (so the next call re-materializes),
		// and mark the registry as Failed so `status` surfaces the incident.
		p.onSendFailure(err)
		writeRPCError(w, req.ID, rpcErrInternalError, err.Error())
		return
	}
	// Only record the tools-call timestamp on successful forward.
	p.debounceWriteToolsCallTimestamp()
	out, err := json.Marshal(resp)
	if err != nil {
		writeRPCError(w, req.ID, rpcErrInternalError, "marshal response: "+err.Error())
		return
	}
	writeJSON(w, out)
}

func (p *LazyProxy) handleForward(w http.ResponseWriter, r *http.Request, req *JSONRPCRequest) {
	ep, err := p.ensureMaterialized(r.Context())
	if err != nil {
		code := rpcErrInternalError
		if IsMissingBinaryErr(err) {
			code = rpcErrMissingBinary
		}
		writeRPCError(w, req.ID, code, err.Error())
		return
	}
	resp, err := ep.SendRequest(r.Context(), req)
	if err != nil {
		p.onSendFailure(err)
		writeRPCError(w, req.ID, rpcErrInternalError, err.Error())
		return
	}
	out, err := json.Marshal(resp)
	if err != nil {
		writeRPCError(w, req.ID, rpcErrInternalError, "marshal response: "+err.Error())
		return
	}
	writeJSON(w, out)
}

// handleSSE answers GET /mcp. v1 minimal behavior: before materialization,
// write an empty event stream with a keepalive comment and hold the
// connection open until the client cancels. Post-materialization a future
// revision will bridge upstream SSE frames through the endpoint adapter.
func (p *LazyProxy) handleSSE(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	flusher, _ := w.(http.Flusher)
	_, _ = fmt.Fprint(w, ": keepalive\n\n")
	if flusher != nil {
		flusher.Flush()
	}
	<-r.Context().Done()
}

// --- materialization -------------------------------------------------------

// ensureMaterialized returns the cached endpoint or materializes a new one
// via the inflight gate. Classifies failures to pick Missing vs Failed in
// the registry.
func (p *LazyProxy) ensureMaterialized(ctx context.Context) (MCPEndpoint, error) {
	// Fast path: cache hit.
	p.mu.Lock()
	if p.endpoint != nil {
		ep := p.endpoint
		p.mu.Unlock()
		return ep, nil
	}
	p.mu.Unlock()

	// Slow path: go through the gate. Mark Starting BEFORE entering the gate
	// so `status` shows "starting" while the singleflight is in-flight.
	reg := api.NewRegistry(p.cfg.RegistryPath)
	_ = reg.PutLifecycle(p.cfg.WorkspaceKey, p.cfg.Language, api.LifecycleStarting, "")

	key := p.inflightKey()
	v, err := p.gate.Do(ctx, key, func(ctx context.Context) (any, error) {
		return p.cfg.Lifecycle.Materialize(ctx)
	})
	if err != nil {
		state := api.LifecycleFailed
		if IsMissingBinaryErr(err) {
			state = api.LifecycleMissing
		}
		_ = reg.PutLifecycle(p.cfg.WorkspaceKey, p.cfg.Language, state, err.Error())
		return nil, err
	}
	ep, ok := v.(MCPEndpoint)
	if !ok {
		_ = reg.PutLifecycle(p.cfg.WorkspaceKey, p.cfg.Language, api.LifecycleFailed,
			fmt.Sprintf("gate returned non-endpoint type %T", v))
		return nil, fmt.Errorf("gate returned non-endpoint type %T", v)
	}
	// Publish the cached endpoint. Race note: two goroutines can exit the
	// gate's singleflight simultaneously (one winner + N losers) and each
	// observes the same ep. Storing ep twice is harmless since both point
	// at the same underlying object.
	p.mu.Lock()
	p.endpoint = ep
	p.mu.Unlock()
	_ = reg.PutLifecycleWithTimestamps(
		p.cfg.WorkspaceKey, p.cfg.Language, api.LifecycleActive, "",
		time.Now().UTC(), time.Time{},
	)
	return ep, nil
}

// onSendFailure handles mid-stream backend death: evict the cached endpoint,
// tear down the dead subprocess, clear gate state so the next call
// re-materializes, stamp Failed in the registry. Preserves the underlying
// error chain (errors.Is works upstream) because PutLifecycle records
// err.Error() verbatim after truncation.
//
// Ordering is load-bearing: Lifecycle.Stop() MUST precede gate.Forget().
// Stop invalidates the lifecycle impl's cached host (b.host = nil inside
// mcpLanguageServerStdio / goplsMCPStdio), so the next Materialize spawns
// fresh. If we Forget first, a concurrent ensureMaterialized caller can
// enter the cleared gate, call Materialize on the not-yet-stopped
// lifecycle, observe b.host != nil, and receive an endpoint wrapping the
// dying host — producing an extra dead-endpoint round-trip before
// self-correction. "Disable-then-publish": kill the shared resource, THEN
// signal that new callers may enter.
func (p *LazyProxy) onSendFailure(err error) {
	p.mu.Lock()
	if p.endpoint != nil {
		_ = p.endpoint.Close()
		p.endpoint = nil
	}
	p.mu.Unlock()
	// Tell the lifecycle impl to tear its subprocess down first — safe even
	// if the child already exited on its own. This invalidates the impl's
	// cached host so any concurrent Materialize that slips in after the
	// next Forget() will re-spawn rather than reuse the dead host.
	_ = p.cfg.Lifecycle.Stop()
	p.gate.Forget(p.inflightKey())
	_ = api.NewRegistry(p.cfg.RegistryPath).PutLifecycle(
		p.cfg.WorkspaceKey, p.cfg.Language, api.LifecycleFailed, err.Error())
}

// debounceWriteToolsCallTimestamp coalesces LastToolsCallAt writes to the
// configured debounce interval. Called only on successful tools/call forward.
// The debounce state is intentionally process-local: a proxy restart resets
// lastToolsCallWrite, so the first call after restart always touches the
// registry (the correct behavior for `status --last-used`).
func (p *LazyProxy) debounceWriteToolsCallTimestamp() {
	p.debounceMu.Lock()
	now := time.Now()
	due := now.Sub(p.lastToolsCallWrite) >= p.cfg.ToolsCallDebounce
	if due {
		p.lastToolsCallWrite = now
	}
	p.debounceMu.Unlock()
	if !due {
		return
	}
	_ = api.NewRegistry(p.cfg.RegistryPath).PutLifecycleWithTimestamps(
		p.cfg.WorkspaceKey, p.cfg.Language, api.LifecycleActive, "",
		time.Time{}, now.UTC(),
	)
}

// --- helpers ---------------------------------------------------------------

// JSON-RPC 2.0 error codes used by the proxy. -32700..-32603 are the spec
// constants; -32000..-32099 is the application-defined range. We use -32010
// for missing-binary so status / CLI can distinguish it from generic
// internal errors without parsing message text.
const (
	rpcErrParseError    = -32700
	rpcErrInternalError = -32603
	rpcErrMissingBinary = -32010
)

func writeJSON(w http.ResponseWriter, body []byte) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(body)
}

func writeRPCError(w http.ResponseWriter, id json.RawMessage, code int, msg string) {
	envelope := map[string]any{
		"jsonrpc": "2.0",
		"error":   map[string]any{"code": code, "message": msg},
	}
	if len(id) > 0 {
		envelope["id"] = id
	} else {
		envelope["id"] = nil
	}
	b, _ := json.Marshal(envelope)
	w.Header().Set("Content-Type", "application/json")
	// JSON-RPC errors ride a 200 OK body per convention; status-level codes
	// are reserved for transport errors (4xx/5xx) the proxy does not emit.
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(b)
}
