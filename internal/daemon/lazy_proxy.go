package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"strconv"
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
	// MaterializedHardCap is the maximum number of LSP backend rows that may
	// be concurrently lifecycle=starting|active across the registry. Zero
	// disables the cap.
	MaterializedHardCap int
	// IdleBackendTTL stops a materialized backend after this much idle time.
	// The proxy process stays up and the next tools/call rematerializes.
	// Zero disables idle reaping.
	IdleBackendTTL time.Duration
	// IdleBackendCheckEvery defaults to DefaultLSPIdleBackendCheckEvery when
	// IdleBackendTTL is enabled and this value is zero.
	IdleBackendCheckEvery time.Duration

	// MaterializeWaitBudget bounds how long a single cold forwarded request
	// (first tools/call / forward per (workspace, language) after the backend
	// is cold) is held before the proxy fast-fails with a 503 "cold start in
	// progress; retry" while the materialize continues in the background. It
	// also bounds the first-request probation window (the gopls-style indexing
	// pause after the MCP handshake). Defaults to DefaultLSPMaterializeWaitBudget
	// (15s) when zero.
	MaterializeWaitBudget time.Duration
	// ColdStartConcurrency caps how many OTHER LSP backends may be concurrently
	// in the expensive cold-start window (registry Lifecycle == Starting AND
	// port-live) before this proxy refuses to enter the materialize path and
	// returns a 503 "cold-start slots busy; retry" without spawning. Because
	// the row stays Starting through indexing (see the probation warmed flag),
	// this bounds the indexing window, not just spawn+handshake. Defaults to
	// DefaultLSPColdStartConcurrency (2) when zero; a negative value disables
	// the cold-start gate.
	ColdStartConcurrency int
	// ColdStartMaxProbation bounds how long a materialized-but-never-warmed
	// backend may hold its cold-start slot before the probation watchdog
	// (piggybacked on the idle-reaper tick) tears it down and frees the slot.
	// Defaults to DefaultLSPColdStartMaxProbation (5m) when zero; a negative
	// value disables the watchdog.
	ColdStartMaxProbation time.Duration
}

const (
	DefaultLSPMaterializedHardCap   = api.DefaultLSPMaterializedHardCap
	DefaultLSPIdleBackendTTL        = 30 * time.Minute
	DefaultLSPIdleBackendCheckEvery = time.Minute

	// DefaultLSPMaterializeWaitBudget is the default LazyProxyConfig
	// MaterializeWaitBudget: the per-request cold-start hold + probation bound.
	DefaultLSPMaterializeWaitBudget = 15 * time.Second
	// DefaultLSPColdStartConcurrency is the default cap on OTHER Starting +
	// port-live LSP backends before this proxy refuses to cold-start.
	DefaultLSPColdStartConcurrency = 2
	// DefaultLSPColdStartMaxProbation is the default probation-watchdog window
	// after which a materialized-but-never-warmed backend is torn down.
	DefaultLSPColdStartMaxProbation = 5 * time.Minute

	// Bounds attacker-controlled document lifecycle state retained by the
	// long-lived lazy proxy. Normal editors keep far fewer concurrently-open
	// documents; this cap prevents unbounded unique URI growth from local
	// clients while preserving duplicate-open refcounting for tracked URIs.
	maxTrackedDocRefs = 4096
	maxDocURIBytes    = 4096
)

var materializedSlotPortLiveFn = lazyProxyPortLive

// errColdStartSlotsBusy is the sentinel a cold-start-slot refusal satisfies via
// errors.Is; the concrete coldStartSlotsBusyError carries the observed warming
// count for the operator-facing 503 message.
var errColdStartSlotsBusy = errors.New("cold-start slots busy")

// errLazyProxyUnpublishable is returned by publishMaterializedEndpoint (and
// surfaced by the materialize fn) when a freshly materialized endpoint cannot be
// installed because the proxy is closing or mid-teardown. The endpoint is torn
// down before it is returned; the caller treats it like any other materialize
// failure but does NOT stamp the row Failed (see ensureMaterialized).
var errLazyProxyUnpublishable = errors.New("lazy proxy closed before endpoint publish")

type coldStartSlotsBusyError struct{ warming int }

func (e *coldStartSlotsBusyError) Error() string {
	return fmt.Sprintf("LSP cold-start slots busy (%d backends warming); retry in ~30s", e.warming)
}

func (e *coldStartSlotsBusyError) Is(target error) bool { return target == errColdStartSlotsBusy }

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
	cfg      LazyProxyConfig
	gate     *InflightGate
	server   *http.Server
	listener net.Listener // populated by Bind; consumed by Serve

	mu       sync.Mutex
	endpoint MCPEndpoint
	reaping  bool
	closed   atomic.Bool

	// warmed reports whether the materialized backend has served at least one
	// SUCCESSFUL forwarded request since it was published. gopls-style backends
	// index for tens of seconds AFTER the MCP handshake, so materialize-success
	// is not usable-success; while !warmed the request handlers bound the
	// upstream call with MaterializeWaitBudget and 503-retry on our deadline.
	// The first successful response flips warmed true and performs the
	// LifecycleActive registry write (the row stays Starting through indexing).
	// Reset false at every endpoint publish and every teardown.
	warmed bool
	// endpointPublishedAt is when the current endpoint was published (cold
	// materialize succeeded). The probation watchdog tears down a backend that
	// stays !warmed past ColdStartMaxProbation from this instant. Zero when no
	// endpoint is published.
	endpointPublishedAt time.Time
	// endpointGeneration is bumped on every endpoint publish AND every teardown.
	// A request captures the generation when it obtains the endpoint and passes
	// it to markWarmedOnFirstSuccess, which no-ops if the generation changed in
	// the meantime — so a late first-success cannot stamp LifecycleActive over a
	// Failed that a concurrent teardown just wrote (F6a).
	endpointGeneration uint64
	// startingSince is when reserveMaterializedSlot last set this row Starting
	// without an endpoint yet published. Cleared on publish and on every
	// teardown. The probation watchdog's belt-and-braces branch uses it to reap
	// an orphan Starting row that never got an endpoint published (e.g. if the
	// publish-from-fn path were ever skipped), so the cold-start slot frees (F1).
	startingSince time.Time

	inflightBackendRequests int
	lastBackendActivity     time.Time
	idleStartOnce           sync.Once
	idleStopOnce            sync.Once
	idleStop                chan struct{}

	debounceMu         sync.Mutex
	lastToolsCallWrite time.Time

	// docRefs counts, per textDocument URI, how many client-side opens are
	// outstanding against the shared upstream backend. One proxy serves
	// every client/agent for its (workspace, language) tuple, so multiple
	// agents can open the SAME document concurrently. The LSP server tracks
	// a document once; a second didOpen for an already-open URI is a protocol
	// violation, and a didClose from one agent while another still has the
	// document open would drop it for everyone. This refcount forwards
	// didOpen to upstream only on the first open of a URI (0->1) and didClose
	// only on the last close (1->0); intermediate opens/closes are absorbed.
	//
	// Guarded by docRefsMu (a dedicated mutex, NOT p.mu, so document
	// bookkeeping never contends with the materialization/reaper critical
	// section). The map is reset to empty whenever the backend is torn down
	// (Stop / onSendFailure / reapIdleBackend), because a fresh backend has
	// no open documents — keeping stale counts would absorb the first didOpen
	// after a restart and silently desynchronize the proxy from upstream.
	docRefsMu sync.Mutex
	docRefs   map[string]int
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
	if cfg.MaterializeWaitBudget == 0 {
		cfg.MaterializeWaitBudget = DefaultLSPMaterializeWaitBudget
	}
	if cfg.ColdStartConcurrency == 0 {
		cfg.ColdStartConcurrency = DefaultLSPColdStartConcurrency
	}
	if cfg.ColdStartMaxProbation == 0 {
		cfg.ColdStartMaxProbation = DefaultLSPColdStartMaxProbation
	}
	if cfg.IdleBackendTTL > 0 && cfg.IdleBackendCheckEvery == 0 {
		cfg.IdleBackendCheckEvery = DefaultLSPIdleBackendCheckEvery
	}
	p := &LazyProxy{
		cfg:      cfg,
		gate:     NewInflightGate(cfg.InflightMinRetryGap),
		idleStop: make(chan struct{}),
		docRefs:  make(map[string]int),
	}
	p.startIdleReaper()
	return p
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

// Bind binds the TCP listener on 127.0.0.1:<port>. It returns once the
// port is bound but before accepting any requests. Call Serve afterward
// to run the request loop.
//
// IMPORTANT: Bind is lock-free with respect to the registry file lock so
// a caller already holding reg.Lock() (to make the "check registry +
// bind port" check atomic vs concurrent unregister) does not deadlock
// on Windows LockFileEx when Bind tries to re-acquire the same flock.
// Writing the initial LifecycleConfigured state is the caller's
// responsibility — see ListenAndServe for the convenient all-in-one
// path, or WriteConfiguredState for the explicit pre-Bind write that
// uses the caller's already-loaded Registry instance.
func (p *LazyProxy) Bind() error {
	addr := fmt.Sprintf("127.0.0.1:%d", p.cfg.Port)
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("bind %s: %w", addr, err)
	}
	p.listener = ln
	p.server = &http.Server{
		Addr:              addr,
		Handler:           p.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		// WriteTimeout 0: handlers own cancellation via r.Context().
	}
	return nil
}

// Serve runs the request loop on the listener populated by Bind. Returns
// http.ErrServerClosed after a clean Stop.
func (p *LazyProxy) Serve() error {
	if p.listener == nil || p.server == nil {
		return errors.New("proxy not bound — call Bind() first")
	}
	return p.server.Serve(p.listener)
}

// ListenAndServe writes the initial Configured state, binds, and serves
// in one call. Retained for callers that don't need the lock-aware
// Bind/Serve split (tests using httptest, production callers without
// registry-flock concerns). Production CLI uses the split form so the
// flock wraps check-plus-bind atomically.
func (p *LazyProxy) ListenAndServe() error {
	_ = api.NewRegistry(p.cfg.RegistryPath).PutLifecycle(
		p.cfg.WorkspaceKey, p.cfg.Language, api.LifecycleConfigured, "")
	if err := p.Bind(); err != nil {
		return err
	}
	return p.Serve()
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
	p.stopIdleReaper()
	p.mu.Lock()
	if p.endpoint != nil {
		_ = p.endpoint.Close()
		p.endpoint = nil
	}
	p.endpointGeneration++
	p.warmed = false
	p.endpointPublishedAt = time.Time{}
	p.startingSince = time.Time{}
	p.lastBackendActivity = time.Time{}
	p.mu.Unlock()
	p.resetDocRefs()
	stopErr := p.stopLifecycleBounded(ctx)
	if stopErr != nil {
		fmt.Fprintf(daemonDiagWriter(), "warn: lazy_proxy: lifecycle stop: %v\n", stopErr)
	}
	p.gate.Forget(p.inflightKey())
	if p.server != nil {
		shutdownErr := p.server.Shutdown(ctx)
		// Codex CLI xhigh re-review on 479cbc3 (P2): embed lifecycle
		// stop error in the returned err so callers get durable
		// visibility, not just stderr (which scheduled paths drop).
		if shutdownErr != nil && stopErr != nil {
			return fmt.Errorf("%w; lifecycle stop: %v", shutdownErr, stopErr)
		}
		if shutdownErr != nil {
			return shutdownErr
		}
		return stopErr
	}
	return stopErr
}

// stopLifecycleBounded runs Lifecycle.Stop() on a detached goroutine and waits
// for it bounded by ctx. BackendLifecycle.Stop takes no context and serializes
// on the lifecycle impl's own mutex, which a slow Materialize can hold for up to
// the detached materializeHardCeiling (120s). Blocking the proxy's graceful
// Stop() on that would make it miss its (typically 5s) shutdown budget (F5).
// On ctx expiry we return ctx.Err() and let the process-exit / supervisor
// Job-Object reaper collect the still-materializing backend; the detached
// goroutine still completes Lifecycle.Stop() once Materialize releases the mutex.
func (p *LazyProxy) stopLifecycleBounded(ctx context.Context) error {
	done := make(chan error, 1)
	go func() { done <- p.cfg.Lifecycle.Stop() }()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (p *LazyProxy) inflightKey() string {
	return p.cfg.WorkspaceKey + "|" + p.cfg.Language
}

// --- JSON-RPC dispatch -----------------------------------------------------

// handleMCP is the per-request dispatch.
func (p *LazyProxy) handleMCP(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		// SSE path — let the SSE handler apply its own admission
		// (loopback + Accept) so headers commit cleanly.
		p.handleSSE(w, r)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	// Content-Type is a syntactic gate (415 Unsupported Media Type) and
	// fires before any auth/origin check (403). Browsers issuing
	// non-JSON cross-origin posts get a clear 415 rather than a
	// generic 403 that obscures the real reason.
	if !isJSONContentType(r.Header.Get("Content-Type")) {
		http.Error(w, "unsupported media type", http.StatusUnsupportedMediaType)
		return
	}
	if rejectUnsafeLoopbackRequest(w, r) {
		return
	}
	if !isAllowedOrigin(r.Header.Get("Origin")) {
		http.Error(w, "forbidden", http.StatusForbidden)
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
	case "resources/list":
		resp, err := api.SyntheticResourcesListResponse(req.ID, p.cfg.BackendKind)
		if err != nil {
			writeRPCError(w, req.ID, rpcErrInternalError, err.Error())
			return
		}
		writeJSON(w, resp)
	case "prompts/list":
		resp, err := api.SyntheticPromptsListResponse(req.ID, p.cfg.BackendKind)
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
	case "textDocument/didOpen", "textDocument/didClose":
		// Per-URI refcount gate: with one shared backend per (workspace,
		// language) tuple, N agents can open the same document. Forward
		// didOpen upstream only on the first open of a URI and didClose
		// only on the last close so the LSP server never sees a duplicate
		// open or a premature close. Notifications get 202 + empty body.
		p.handleDocLifecycle(w, r, &req)
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
		// Other non-initialize/list methods require a materialized
		// backend. Treat them like tools/call for materialization
		// semantics but do NOT debounce-write the LastToolsCallAt
		// timestamp.
		p.handleForward(w, r, &req)
	}
}

func isJSONContentType(contentType string) bool {
	if contentType == "" {
		return false
	}
	mt, _, err := mime.ParseMediaType(contentType)
	return err == nil && mt == "application/json"
}

func isAllowedOrigin(origin string) bool {
	if origin == "" || origin == "null" {
		return true
	}
	u, err := url.Parse(origin)
	if err != nil {
		return false
	}
	host := strings.ToLower(u.Hostname())
	return host == "localhost" || host == "127.0.0.1" || host == "::1"
}

func (p *LazyProxy) handleToolsCall(w http.ResponseWriter, r *http.Request, req *JSONRPCRequest) {
	ep, gen, err := p.ensureMaterialized(r.Context())
	if err != nil {
		if p.writeColdStartRefusal(w, req.ID, err) {
			return
		}
		code := rpcErrInternalError
		if IsMissingBinaryErr(err) {
			code = rpcErrMissingBinary
		}
		writeRPCError(w, req.ID, code, err.Error())
		return
	}
	defer p.endBackendRequest()
	warmed := p.isWarmed()
	callCtx, cancel := p.boundedCallCtx(r.Context(), warmed)
	defer cancel()
	resp, err := ep.SendRequest(callCtx, req)
	if err != nil {
		// Probation deadline: our MaterializeWaitBudget elapsed while the
		// backend was still indexing, and the CLIENT context is still live.
		// Emit the same 503 "cold start in progress" (never a bare "context
		// deadline exceeded") and leave the backend up for the retry.
		if p.isProbationDeadline(r.Context(), warmed, err) {
			p.writeColdStartInProgress(w, req.ID)
			return
		}
		// Differentiate client-cancel from backend failure. SendRequest
		// is driven by r.Context(); a client disconnect or timeout
		// returns context.Canceled / context.DeadlineExceeded even
		// when the backend is healthy. Tearing down on client cancel
		// would force avoidable rematerialization for every other
		// caller of the same proxy.
		if isClientCancelErr(r.Context(), err) {
			writeRPCError(w, req.ID, rpcErrInternalError, err.Error())
			return
		}
		// Backend died mid-stream or stdio channel failed. Evict the cached
		// endpoint, clear the inflight gate (so the next call re-materializes),
		// and mark the registry as Failed so `status` surfaces the incident.
		p.onSendFailure(err)
		writeRPCError(w, req.ID, rpcErrInternalError, err.Error())
		return
	}
	// First successful response ends probation and stamps LifecycleActive.
	p.markWarmedOnFirstSuccess(gen)
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
	ep, gen, err := p.ensureMaterialized(r.Context())
	if err != nil {
		if p.writeColdStartRefusal(w, req.ID, err) {
			return
		}
		code := rpcErrInternalError
		if IsMissingBinaryErr(err) {
			code = rpcErrMissingBinary
		}
		writeRPCError(w, req.ID, code, err.Error())
		return
	}
	defer p.endBackendRequest()
	warmed := p.isWarmed()
	callCtx, cancel := p.boundedCallCtx(r.Context(), warmed)
	defer cancel()
	resp, err := ep.SendRequest(callCtx, req)
	if err != nil {
		// Probation deadline — see handleToolsCall.
		if p.isProbationDeadline(r.Context(), warmed, err) {
			p.writeColdStartInProgress(w, req.ID)
			return
		}
		// Client-cancel is not a backend failure — see handleToolsCall.
		if isClientCancelErr(r.Context(), err) {
			writeRPCError(w, req.ID, rpcErrInternalError, err.Error())
			return
		}
		p.onSendFailure(err)
		writeRPCError(w, req.ID, rpcErrInternalError, err.Error())
		return
	}
	p.markWarmedOnFirstSuccess(gen)
	out, err := json.Marshal(resp)
	if err != nil {
		writeRPCError(w, req.ID, rpcErrInternalError, "marshal response: "+err.Error())
		return
	}
	writeJSON(w, out)
}

// boundedCallCtx wraps the client request context in a MaterializeWaitBudget
// timeout while the backend is in first-request probation (!warmed), so a
// cold gopls-style indexing pause fast-fails to a 503 retry instead of holding
// the connection. Once warmed the client context passes through unmodified.
func (p *LazyProxy) boundedCallCtx(ctx context.Context, warmed bool) (context.Context, context.CancelFunc) {
	if warmed {
		return ctx, func() {}
	}
	return context.WithTimeout(ctx, p.cfg.MaterializeWaitBudget)
}

// isProbationDeadline reports whether err is our own probation-budget deadline
// (not the client's own cancel/deadline). It fires only while !warmed, only
// when the CLIENT context is still live (so a client that set a shorter
// deadline is still classified as a client cancel), and only for a
// DeadlineExceeded error.
func (p *LazyProxy) isProbationDeadline(clientCtx context.Context, warmed bool, err error) bool {
	return !warmed && clientCtx.Err() == nil && errors.Is(err, context.DeadlineExceeded)
}

// coldStartInProgressMessage is the operator/agent-facing body for a
// bounded-wait cold-start 503 (materialize-in-flight OR first-request
// probation deadline).
func (p *LazyProxy) coldStartInProgressMessage() string {
	return fmt.Sprintf("language backend cold start in progress (%s, %s); retry in ~15s",
		p.cfg.BackendKind, p.cfg.WorkspacePath)
}

// writeColdStartInProgress emits the 503 + JSON-RPC -32603 + Retry-After: 15
// cold-start-in-progress response.
func (p *LazyProxy) writeColdStartInProgress(w http.ResponseWriter, id json.RawMessage) {
	writeRPCErrorStatus(w, id, http.StatusServiceUnavailable, rpcErrInternalError,
		p.coldStartInProgressMessage(), 15)
}

// writeColdStartRefusal maps the two cold-start throttle sentinels returned by
// ensureMaterialized to their 503 responses and reports whether it handled err.
// Any other error is left for the caller's generic mapping.
func (p *LazyProxy) writeColdStartRefusal(w http.ResponseWriter, id json.RawMessage, err error) bool {
	switch {
	case errors.Is(err, ErrMaterializeInFlight):
		p.writeColdStartInProgress(w, id)
		return true
	case errors.Is(err, errColdStartSlotsBusy):
		writeRPCErrorStatus(w, id, http.StatusServiceUnavailable, rpcErrInternalError, err.Error(), 30)
		return true
	}
	return false
}

// handleDocLifecycle gates textDocument/didOpen and textDocument/didClose
// behind a per-URI refcount so a single shared backend never sees a duplicate
// open (protocol violation) or a premature close (drops the document while
// another agent still has it open). didOpen forwards to upstream only on the
// first open of a URI (count 0->1); didClose forwards only on the last close
// (count 1->0). Intermediate opens/closes are absorbed: the proxy answers 202
// Accepted with an empty body and does not touch the backend.
//
// These are JSON-RPC notifications (no id, no response expected). On the
// forwarding transition we still drive the request through the materialized
// endpoint's SendRequest — the same channel handleForward already used for
// these methods — so the wire contract with the backend is unchanged; only
// WHEN we forward is gated by the refcount. SendRequest's response (if any)
// is discarded: notifications carry no client-visible reply.
func (p *LazyProxy) handleDocLifecycle(w http.ResponseWriter, r *http.Request, req *JSONRPCRequest) {
	uri := docURIFromParams(req.Params)
	if uri == "" {
		// Malformed notification (no textDocument.uri). Without a URI there
		// is nothing to refcount; absorb it rather than forward a request
		// the backend cannot correlate. 202 keeps notification semantics.
		w.WriteHeader(http.StatusAccepted)
		return
	}

	isOpen := req.Method == "textDocument/didOpen"
	forward, err := p.applyDocRef(uri, isOpen)
	if err != nil {
		http.Error(w, err.Error(), http.StatusTooManyRequests)
		return
	}
	if !forward {
		// Refcount absorbed this open/close — upstream already has the
		// correct view of the document. No materialize, no forward.
		w.WriteHeader(http.StatusAccepted)
		return
	}

	ep, gen, err := p.ensureMaterialized(r.Context())
	if err != nil {
		// Materialization did not yield an endpoint: the upstream forward will
		// not happen, so roll back the refcount transition we optimistically
		// applied to keep the count consistent with what the backend saw.
		p.rollbackDocRef(uri, isOpen)
		if p.writeColdStartRefusal(w, req.ID, err) {
			return
		}
		code := rpcErrInternalError
		if IsMissingBinaryErr(err) {
			code = rpcErrMissingBinary
		}
		writeRPCError(w, req.ID, code, err.Error())
		return
	}
	defer p.endBackendRequest()
	// Probation-bound the forward while !warmed, exactly like handleToolsCall/
	// handleForward. Without this a didOpen/didClose that arrives during the
	// backend's post-handshake indexing window would hold the connection until
	// the client's own deadline and surface a bare "context deadline exceeded"
	// (HTTP 200) to a client on a direct per-daemon port (F2).
	warmed := p.isWarmed()
	callCtx, cancel := p.boundedCallCtx(r.Context(), warmed)
	defer cancel()
	resp, err := ep.SendRequest(callCtx, req)
	if err != nil {
		// The notification was not delivered, so roll back the transition.
		p.rollbackDocRef(uri, isOpen)
		// Probation deadline: our budget elapsed while the backend was indexing
		// and the client ctx is still live → emit the same 503 "cold start in
		// progress" (never a bare deadline-exceeded), no teardown (F2).
		if p.isProbationDeadline(r.Context(), warmed, err) {
			p.writeColdStartInProgress(w, req.ID)
			return
		}
		// Client-cancel is not a backend failure — see handleToolsCall.
		if isClientCancelErr(r.Context(), err) {
			writeRPCError(w, req.ID, rpcErrInternalError, err.Error())
			return
		}
		p.onSendFailure(err)
		writeRPCError(w, req.ID, rpcErrInternalError, err.Error())
		return
	}
	// The backend served a request → first success ends probation (F2).
	p.markWarmedOnFirstSuccess(gen)
	if resp != nil && resp.Error != nil {
		// Backend rejected the lifecycle notification, so do not retain a
		// refcount that would suppress a future legitimate open/close. The
		// client-visible HTTP contract remains notification-like (202/no body).
		p.rollbackDocRef(uri, isOpen)
	}
	// Notification delivered. JSON-RPC notifications take no response body.
	w.WriteHeader(http.StatusAccepted)
}

// applyDocRef records an open/close against uri and reports whether the
// operation should be forwarded to the upstream backend. didOpen forwards on
// the 0->1 transition; didClose forwards on the 1->0 transition. A didClose
// against an already-zero count is absorbed and never drives the count
// negative (a spurious or duplicate close from one agent must not affect the
// document state another agent still depends on).
func (p *LazyProxy) applyDocRef(uri string, open bool) (bool, error) {
	if len(uri) > maxDocURIBytes {
		return false, fmt.Errorf("textDocument.uri exceeds %d bytes", maxDocURIBytes)
	}
	p.docRefsMu.Lock()
	defer p.docRefsMu.Unlock()
	if open {
		prev := p.docRefs[uri]
		if prev == 0 && len(p.docRefs) >= maxTrackedDocRefs {
			return false, fmt.Errorf("too many tracked text documents (max %d)", maxTrackedDocRefs)
		}
		p.docRefs[uri] = prev + 1
		return prev == 0, nil
	}
	prev := p.docRefs[uri]
	if prev <= 0 {
		// Close with no recorded open — nothing upstream to release.
		return false, nil
	}
	if prev == 1 {
		delete(p.docRefs, uri)
		return true, nil
	}
	p.docRefs[uri] = prev - 1
	return false, nil
}

// rollbackDocRef undoes the most recent applyDocRef transition for uri when
// the corresponding upstream forward could not be delivered. It is the exact
// inverse: a failed didOpen decrements (back toward 0), a failed didClose
// re-increments (restoring the open the close was about to release).
func (p *LazyProxy) rollbackDocRef(uri string, open bool) {
	p.docRefsMu.Lock()
	defer p.docRefsMu.Unlock()
	if open {
		if cur := p.docRefs[uri]; cur <= 1 {
			delete(p.docRefs, uri)
		} else {
			p.docRefs[uri] = cur - 1
		}
		return
	}
	p.docRefs[uri] = p.docRefs[uri] + 1
}

// resetDocRefs clears all per-URI open counts. Called whenever the backend is
// torn down (Stop / onSendFailure / reapIdleBackend): the fresh backend that
// the next request materializes has no open documents, so any retained count
// would wrongly absorb the first didOpen after the restart.
func (p *LazyProxy) resetDocRefs() {
	p.docRefsMu.Lock()
	p.docRefs = make(map[string]int)
	p.docRefsMu.Unlock()
}

// docURIFromParams extracts params.textDocument.uri from a didOpen/didClose
// notification. Both DidOpenTextDocumentParams (textDocument is a
// TextDocumentItem) and DidCloseTextDocumentParams (textDocument is a
// TextDocumentIdentifier) carry the uri under textDocument.uri per the LSP
// spec. Returns "" when params are absent or malformed.
func docURIFromParams(params json.RawMessage) string {
	if len(params) == 0 {
		return ""
	}
	var p struct {
		TextDocument struct {
			URI string `json:"uri"`
		} `json:"textDocument"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return ""
	}
	return p.TextDocument.URI
}

// isClientCancelErr reports whether err is the consequence of the client's
// request context being canceled or timing out, rather than a backend
// failure. Treats any error with the request context already canceled as
// client-cancel — SendRequest commonly wraps context.Canceled inside a
// "write stdin:" or "send rpc:" wrapper, and errors.Is would miss the
// unwrapped case. If the context is still Live we fall back to
// errors.Is(err, context.Canceled|DeadlineExceeded) for belt-and-braces.
func isClientCancelErr(ctx context.Context, err error) bool {
	if err == nil {
		return false
	}
	if ctx.Err() != nil {
		return true
	}
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

// handleSSE answers GET /mcp. v1 minimal behavior: before materialization,
// write an empty event stream with a keepalive comment and hold the
// connection open until the client cancels. Post-materialization a future
// revision will bridge upstream SSE frames through the endpoint adapter.
func (p *LazyProxy) handleSSE(w http.ResponseWriter, r *http.Request) {
	if rejectUnsafeLoopbackRequest(w, r) {
		return
	}
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
// via the inflight gate, and returns the endpoint generation the caller must
// pass to markWarmedOnFirstSuccess. Classifies failures to pick Missing vs
// Failed in the registry.
//
// Endpoint publication and lifecycle bookkeeping happen INSIDE the singleflight
// fn (the detached winner goroutine), not in the caller after DoBounded returns.
// This is load-bearing for F1: if every bounded waiter abandons the join
// (budget-expiry 503 or ctx-cancel) before the materialize finishes, a
// caller-side publish would never run — leaking the spawned backend and leaving
// a permanent Starting row that counts against every other proxy's cold-start
// slot. Publishing from the fn makes publication independent of any waiter.
func (p *LazyProxy) ensureMaterialized(ctx context.Context) (MCPEndpoint, uint64, error) {
	// Fast path: cache hit.
	for {
		if p.closed.Load() {
			return nil, 0, errors.New("lazy proxy is closed")
		}
		p.mu.Lock()
		if p.endpoint != nil {
			ep := p.endpoint
			gen := p.endpointGeneration
			p.beginBackendRequestLocked(time.Now().UTC())
			p.mu.Unlock()
			return ep, gen, nil
		}
		if !p.reaping {
			p.mu.Unlock()
			break
		}
		p.mu.Unlock()

		select {
		case <-ctx.Done():
			return nil, 0, ctx.Err()
		case <-time.After(10 * time.Millisecond):
		}
	}
	if p.closed.Load() {
		return nil, 0, errors.New("lazy proxy is closed")
	}

	// Slow path: reserve a cold-start slot (marks Starting BEFORE entering the
	// gate so `status` shows "starting" while the singleflight is in flight),
	// then go through the gate.
	if err := p.reserveMaterializedSlot(); err != nil {
		return nil, 0, err
	}
	p.markStartingReserved(time.Now().UTC())

	key := p.inflightKey()
	_, err := p.gate.DoBounded(ctx, key, p.cfg.MaterializeWaitBudget, func(fnCtx context.Context) (any, error) {
		ep, mErr := p.cfg.Lifecycle.Materialize(fnCtx)
		if mErr != nil {
			// Stamp the failure from INSIDE the fn so lifecycle bookkeeping does
			// not depend on a bounded waiter still being present. An
			// abandoned-but-FAILED materialize must still leave the row
			// Failed/Missing, never stuck Starting (F1).
			state := api.LifecycleFailed
			if IsMissingBinaryErr(mErr) {
				state = api.LifecycleMissing
			}
			_ = api.NewRegistry(p.cfg.RegistryPath).PutLifecycle(
				p.cfg.WorkspaceKey, p.cfg.Language, state, mErr.Error())
			return nil, mErr
		}
		// Publish from INSIDE the fn too, so an abandoned-but-SUCCESSFUL
		// materialize (every bounded waiter left on budget-expiry / ctx-cancel)
		// still installs the endpoint instead of leaking the spawned backend and
		// a permanent Starting row (F1). The row stays Starting here; the
		// LifecycleActive write is DEFERRED to the first successful forwarded
		// response (markWarmedOnFirstSuccess), because a gopls-style backend
		// indexes for tens of seconds after the MCP handshake and is not usable
		// until then.
		if pErr := p.publishMaterializedEndpoint(ep); pErr != nil {
			return nil, pErr
		}
		return ep, nil
	})
	if err != nil {
		// ErrMaterializeInFlight: the materialize is still running (and will
		// publish or stamp Failed from the fn); the handler maps it to a 503
		// "cold start in progress". A caller-ctx cancel while joining is likewise
		// not a failure this caller must record — the detached materialize
		// continues and its fn owns the outcome. A genuine materialize error was
		// already stamped inside the fn; a THROTTLE-cached error skipped the fn,
		// so re-stamp here so the optimistic Starting row does not linger. Skip
		// the stamp when closing (errLazyProxyUnpublishable) so a clean shutdown
		// is not recorded as a failure.
		if errors.Is(err, ErrMaterializeInFlight) || ctx.Err() != nil || p.closed.Load() {
			return nil, 0, err
		}
		if errors.Is(err, errLazyProxyUnpublishable) {
			return nil, 0, err
		}
		state := api.LifecycleFailed
		if IsMissingBinaryErr(err) {
			state = api.LifecycleMissing
		}
		_ = api.NewRegistry(p.cfg.RegistryPath).PutLifecycle(
			p.cfg.WorkspaceKey, p.cfg.Language, state, err.Error())
		return nil, 0, err
	}
	// The fn published the endpoint. Re-read it under the lock together with the
	// current generation and register this caller's in-flight request. Losers of
	// the singleflight all arrive here and re-read the same published endpoint,
	// so publication is idempotent. If a teardown niled it between publish and
	// now, surface that as an error rather than hand back a stale object.
	p.mu.Lock()
	if p.closed.Load() {
		p.mu.Unlock()
		return nil, 0, errors.New("lazy proxy is closed")
	}
	ep := p.endpoint
	if ep == nil {
		p.mu.Unlock()
		return nil, 0, errors.New("materialized endpoint torn down before use")
	}
	gen := p.endpointGeneration
	p.beginBackendRequestLocked(time.Now().UTC())
	p.mu.Unlock()
	return ep, gen, nil
}

// publishMaterializedEndpoint installs a freshly materialized endpoint as the
// live backend. It runs INSIDE the singleflight fn (the detached winner
// goroutine) so the endpoint is published even when every bounded waiter has
// already abandoned the join (F1). Bumps the endpoint generation and clears the
// probation flags (warmed false, freshly published, startingSince cleared).
// Returns errLazyProxyUnpublishable — after tearing the endpoint down — when the
// proxy is closing or mid-teardown, so the fn reports failure rather than
// publishing into a dead proxy.
func (p *LazyProxy) publishMaterializedEndpoint(ep MCPEndpoint) error {
	now := time.Now().UTC()
	p.mu.Lock()
	if p.closed.Load() || p.reaping {
		p.mu.Unlock()
		_ = ep.Close()
		if stopErr := p.cfg.Lifecycle.Stop(); stopErr != nil {
			fmt.Fprintf(daemonDiagWriter(), "warn: lazy_proxy: lifecycle stop after unpublishable materialize: %v\n", stopErr)
		}
		p.gate.Forget(p.inflightKey())
		return errLazyProxyUnpublishable
	}
	p.endpoint = ep
	p.endpointGeneration++
	p.warmed = false
	p.endpointPublishedAt = now
	p.startingSince = time.Time{}
	p.mu.Unlock()
	return nil
}

// markStartingReserved records when reserveMaterializedSlot set this row Starting
// without an endpoint yet published, so the probation watchdog's belt-and-braces
// branch can reap an orphan Starting row (F1).
func (p *LazyProxy) markStartingReserved(now time.Time) {
	p.mu.Lock()
	// Only arm the orphan timer while no endpoint is published; a concurrent
	// publish/warm must win over a late reserve stamp.
	if p.endpoint == nil {
		p.startingSince = now
	}
	p.mu.Unlock()
}

// markWarmedOnFirstSuccess flips warmed true on the FIRST successful forwarded
// response and, on that transition only, performs the deferred LifecycleActive
// registry write (with LastMaterializedAt) that ensureMaterialized no longer
// does at materialize-success. gen is the endpoint generation the caller
// captured when it obtained the endpoint: if the generation has since changed (a
// teardown replaced or removed the endpoint and likely stamped Failed), this
// no-ops so a late success cannot overwrite that Failed with Active (F6a).
// Idempotent: subsequent calls for the same generation are a no-op too.
func (p *LazyProxy) markWarmedOnFirstSuccess(gen uint64) {
	p.mu.Lock()
	if p.warmed || p.endpoint == nil || p.endpointGeneration != gen {
		p.mu.Unlock()
		return
	}
	p.warmed = true
	p.mu.Unlock()
	_ = api.NewRegistry(p.cfg.RegistryPath).PutLifecycleWithTimestamps(
		p.cfg.WorkspaceKey, p.cfg.Language, api.LifecycleActive, "",
		time.Now().UTC(), time.Time{},
	)
}

// isWarmed reports whether the current backend has already served a first
// successful response (so the request handlers can skip the probation bound).
func (p *LazyProxy) isWarmed() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.warmed
}

func (p *LazyProxy) reserveMaterializedSlot() error {
	reg := api.NewRegistry(p.cfg.RegistryPath)
	capOn := p.cfg.MaterializedHardCap > 0
	coldGateOn := p.cfg.ColdStartConcurrency > 0
	if !capOn && !coldGateOn {
		return reg.PutLifecycle(p.cfg.WorkspaceKey, p.cfg.Language, api.LifecycleStarting, "")
	}
	unlock, err := reg.Lock()
	if err != nil {
		return fmt.Errorf("reserve materialized LSP backend slot: %w", err)
	}
	defer unlock()
	if err := reg.Load(); err != nil {
		return fmt.Errorf("reserve materialized LSP backend slot: %w", err)
	}
	entry, ok := reg.Get(p.cfg.WorkspaceKey, p.cfg.Language)
	if !ok {
		return fmt.Errorf("reserve materialized LSP backend slot: registry entry missing for %s/%s",
			p.cfg.WorkspaceKey, p.cfg.Language)
	}
	// Count OTHER live LSP rows under the same flock: `active` (Starting|Active)
	// for the total-materialized hard cap, `startingLive` (Starting only) for the
	// cold-start-concurrency gate. Both exclude stale rows from crashed daemons
	// via the port-live self-heal. Because the row stays Starting through indexing
	// (probation), the cold-start gate bounds the expensive index window.
	active := 0
	startingLive := 0
	for _, e := range reg.LSPEntries() {
		if e.WorkspaceKey == p.cfg.WorkspaceKey && e.Language == p.cfg.Language {
			continue
		}
		live := e.Port > 0 && materializedSlotPortLiveFn != nil && materializedSlotPortLiveFn(e.Port)
		if !live {
			continue
		}
		if e.Lifecycle == api.LifecycleStarting || e.Lifecycle == api.LifecycleActive {
			active++
		}
		if e.Lifecycle == api.LifecycleStarting {
			startingLive++
		}
	}
	if capOn && active >= p.cfg.MaterializedHardCap {
		return fmt.Errorf("materialized LSP backend cap reached: %d active/starting backends, cap %d",
			active, p.cfg.MaterializedHardCap)
	}
	// Cold-start-concurrency gate: refuse to ENTER the materialize path (no
	// spawn, no gate) when too many other backends are already warming. The
	// client retry IS the queue — no in-process waiting, no held connection.
	// EXCEPTION (F3): skip the gate when this proxy already has its OWN
	// materialize in flight. A retry after a bounded-wait 503 merely JOINS the
	// running singleflight (spawns nothing), so refusing it here would starve the
	// very retry the 503 asked for and keep this backend from ever warming.
	if coldGateOn && startingLive >= p.cfg.ColdStartConcurrency &&
		!p.gate.HasActiveFlight(p.inflightKey()) {
		return &coldStartSlotsBusyError{warming: startingLive}
	}
	entry.Lifecycle = api.LifecycleStarting
	entry.LastError = ""
	reg.Put(entry)
	if err := reg.Save(); err != nil {
		return fmt.Errorf("reserve materialized LSP backend slot: %w", err)
	}
	return nil
}

func (p *LazyProxy) beginBackendRequest() {
	now := time.Now().UTC()
	p.mu.Lock()
	p.beginBackendRequestLocked(now)
	p.mu.Unlock()
}

func (p *LazyProxy) beginBackendRequestLocked(now time.Time) {
	p.inflightBackendRequests++
	p.lastBackendActivity = now
}

func (p *LazyProxy) endBackendRequest() {
	now := time.Now().UTC()
	p.mu.Lock()
	if p.inflightBackendRequests > 0 {
		p.inflightBackendRequests--
	}
	p.lastBackendActivity = now
	p.mu.Unlock()
}

func lazyProxyPortLive(port int) bool {
	conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 300*time.Millisecond)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

func (p *LazyProxy) startIdleReaper() {
	// The single background ticker drives BOTH the idle reaper AND the cold-start
	// probation watchdog. Start it when EITHER is configured (F4): gating it on
	// IdleBackendTTL alone meant `--idle-backend-ttl=0` silently disabled the
	// probation watchdog while the cold-start gate stayed on, so a wedged Starting
	// backend would hold its slot forever. Each per-tick reaper self-skips when
	// its own knob is off (reapIdleBackend on IdleBackendTTL<=0; reapWedgedProbation
	// on ColdStartMaxProbation<=0), so a watchdog-only ticker never idle-reaps.
	idleOn := p.cfg.IdleBackendTTL > 0
	watchdogOn := p.cfg.ColdStartMaxProbation > 0 && p.cfg.ColdStartConcurrency > 0
	if !idleOn && !watchdogOn {
		return
	}
	interval := p.cfg.IdleBackendCheckEvery
	if interval <= 0 {
		interval = DefaultLSPIdleBackendCheckEvery
	}
	p.idleStartOnce.Do(func() {
		go func() {
			ticker := time.NewTicker(interval)
			defer ticker.Stop()
			for {
				select {
				case <-ticker.C:
					now := time.Now().UTC()
					p.reapIdleBackend(now)
					p.reapWedgedProbation(now)
				case <-p.idleStop:
					return
				}
			}
		}()
	})
}

func (p *LazyProxy) stopIdleReaper() {
	p.idleStopOnce.Do(func() {
		if p.idleStop != nil {
			close(p.idleStop)
		}
	})
}

func (p *LazyProxy) reapIdleBackend(now time.Time) {
	if p.closed.Load() || p.cfg.IdleBackendTTL <= 0 {
		return
	}
	var ep MCPEndpoint
	p.mu.Lock()
	if p.endpoint == nil ||
		p.reaping ||
		p.inflightBackendRequests > 0 ||
		p.lastBackendActivity.IsZero() ||
		now.Sub(p.lastBackendActivity) < p.cfg.IdleBackendTTL {
		p.mu.Unlock()
		return
	}
	ep = p.endpoint
	p.reaping = true
	p.endpoint = nil
	p.endpointGeneration++
	p.warmed = false
	p.endpointPublishedAt = time.Time{}
	p.startingSince = time.Time{}
	p.lastBackendActivity = time.Time{}
	p.mu.Unlock()

	_ = ep.Close()
	p.resetDocRefs()
	if err := p.cfg.Lifecycle.Stop(); err != nil {
		fmt.Fprintf(daemonDiagWriter(), "warn: lazy_proxy: idle lifecycle stop: %v\n", err)
		_ = api.NewRegistry(p.cfg.RegistryPath).PutLifecycle(
			p.cfg.WorkspaceKey, p.cfg.Language, api.LifecycleFailed,
			fmt.Sprintf("idle reaper lifecycle stop: %v", err))
		p.mu.Lock()
		p.reaping = false
		p.mu.Unlock()
		return
	}
	p.gate.Forget(p.inflightKey())
	_ = api.NewRegistry(p.cfg.RegistryPath).PutLifecycle(
		p.cfg.WorkspaceKey, p.cfg.Language, api.LifecycleConfigured, "")
	p.mu.Lock()
	p.reaping = false
	p.mu.Unlock()
}

// reapWedgedProbation frees a cold-start slot held by a backend wedged in the
// probation window. It has two branches:
//
//	Branch A (primary): a materialized backend that has been published for longer
//	than ColdStartMaxProbation without ever serving a first successful response
//	(still !warmed). It holds a cold-start slot and a Starting registry row that
//	would otherwise suppress a healthy retry. Unlike the idle reaper it
//	deliberately does NOT skip on inflightBackendRequests > 0 — the in-flight
//	request IS the wedged one.
//
//	Branch B (belt-and-braces, F1): NO endpoint was ever published, but
//	reserveMaterializedSlot set a Starting row that neither a publish nor a
//	teardown cleared, and no materialize flight is currently running. Left alone
//	this orphan Starting row would count against every other proxy's cold-start
//	slots forever. This branch should be unreachable because publication now
//	happens inside the singleflight fn, but it backstops any path that leaves a
//	Starting row without an endpoint.
//
// Both stamp LifecycleFailed and best-effort Stop the lifecycle so the slot frees
// and the next request re-enters the bounded cold path cleanly.
func (p *LazyProxy) reapWedgedProbation(now time.Time) {
	if p.closed.Load() || p.cfg.ColdStartMaxProbation <= 0 {
		return
	}
	p.mu.Lock()
	if p.reaping {
		p.mu.Unlock()
		return
	}
	// Branch A — published-but-never-warmed backend.
	if p.endpoint != nil {
		if p.warmed ||
			p.endpointPublishedAt.IsZero() ||
			now.Sub(p.endpointPublishedAt) < p.cfg.ColdStartMaxProbation {
			p.mu.Unlock()
			return
		}
		ep := p.endpoint
		p.reaping = true
		p.endpoint = nil
		p.endpointGeneration++
		p.warmed = false
		p.endpointPublishedAt = time.Time{}
		p.startingSince = time.Time{}
		p.lastBackendActivity = time.Time{}
		p.mu.Unlock()

		_ = ep.Close()
		p.resetDocRefs()
		p.teardownWedgedLifecycle(fmt.Sprintf("backend never served a first response within %s", p.cfg.ColdStartMaxProbation))
		return
	}
	// Branch B — orphan Starting row with no endpoint.
	if p.startingSince.IsZero() ||
		now.Sub(p.startingSince) < p.cfg.ColdStartMaxProbation ||
		p.gate.HasActiveFlight(p.inflightKey()) {
		p.mu.Unlock()
		return
	}
	p.reaping = true
	p.startingSince = time.Time{}
	p.mu.Unlock()

	// Only reap if the registry row really is still Starting; if it has already
	// advanced (a teardown stamped Failed/Configured, or a warm stamped Active
	// elsewhere) there is no orphan slot to free.
	if !p.registryRowIsStarting() {
		p.mu.Lock()
		p.reaping = false
		p.mu.Unlock()
		return
	}
	p.resetDocRefs()
	p.teardownWedgedLifecycle("backend never published an endpoint within " + p.cfg.ColdStartMaxProbation.String())
}

// teardownWedgedLifecycle is the shared tail of reapWedgedProbation's two
// branches: best-effort Stop the lifecycle, forget the gate, stamp Failed, and
// clear the reaping flag.
func (p *LazyProxy) teardownWedgedLifecycle(reason string) {
	if err := p.cfg.Lifecycle.Stop(); err != nil {
		fmt.Fprintf(daemonDiagWriter(), "warn: lazy_proxy: probation watchdog lifecycle stop: %v\n", err)
	}
	p.gate.Forget(p.inflightKey())
	_ = api.NewRegistry(p.cfg.RegistryPath).PutLifecycle(
		p.cfg.WorkspaceKey, p.cfg.Language, api.LifecycleFailed, reason)
	p.mu.Lock()
	p.reaping = false
	p.mu.Unlock()
}

// registryRowIsStarting reports whether this proxy's registry row is currently
// Lifecycle==Starting. Used by reapWedgedProbation's orphan branch to avoid
// re-stamping a row that already advanced away from Starting.
func (p *LazyProxy) registryRowIsStarting() bool {
	reg := api.NewRegistry(p.cfg.RegistryPath)
	if err := reg.Load(); err != nil {
		return false
	}
	e, ok := reg.Get(p.cfg.WorkspaceKey, p.cfg.Language)
	return ok && e.Lifecycle == api.LifecycleStarting
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
	if p.reaping {
		p.mu.Unlock()
		return
	}
	p.reaping = true
	if p.endpoint != nil {
		_ = p.endpoint.Close()
		p.endpoint = nil
	}
	p.endpointGeneration++
	p.warmed = false
	p.endpointPublishedAt = time.Time{}
	p.startingSince = time.Time{}
	p.lastBackendActivity = time.Time{}
	p.mu.Unlock()
	p.resetDocRefs()
	// Tell the lifecycle impl to tear its subprocess down first — safe even
	// if the child already exited on its own. This invalidates the impl's
	// cached host so any concurrent Materialize that slips in after the
	// next Forget() will re-spawn rather than reuse the dead host.
	//
	// Codex CLI xhigh re-review on 479cbc3 (P2): embed any lifecycle
	// stop error into the registry message below so it survives where
	// stderr might not — the LifecycleFailed registry write is the
	// canonical durable surface for lazy-proxy failure attribution.
	if stopErr := p.cfg.Lifecycle.Stop(); stopErr != nil {
		fmt.Fprintf(daemonDiagWriter(), "warn: lazy_proxy: lifecycle stop after failure: %v\n", stopErr)
		err = fmt.Errorf("%w; lifecycle stop: %v", err, stopErr)
	}
	p.gate.Forget(p.inflightKey())
	_ = api.NewRegistry(p.cfg.RegistryPath).PutLifecycle(
		p.cfg.WorkspaceKey, p.cfg.Language, api.LifecycleFailed, err.Error())
	p.mu.Lock()
	p.reaping = false
	p.mu.Unlock()
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

// writeRPCErrorStatus is the transport-status sibling of writeRPCError: it
// emits a JSON-RPC error envelope under an explicit non-200 HTTP status (used
// for the cold-start 503 retry contract) and, when retryAfterSecs > 0, a
// standard Retry-After header. The LSP router forwards this status + header
// verbatim; the message text is the load-bearing agent-level retry signal, the
// header is decoration for clients that honor it.
func writeRPCErrorStatus(w http.ResponseWriter, id json.RawMessage, httpStatus, code int, msg string, retryAfterSecs int) {
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
	if retryAfterSecs > 0 {
		w.Header().Set("Retry-After", strconv.Itoa(retryAfterSecs))
	}
	w.WriteHeader(httpStatus)
	_, _ = w.Write(b)
}
