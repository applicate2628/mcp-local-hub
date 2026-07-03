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

	// MaterializeWaitBudget bounds the MATERIALIZE phase only: how long a caller
	// waits in gate.DoBounded for the spawn + MCP handshake + singleflight-dedup
	// before the proxy fast-fails with a 503 "cold start in progress; retry"
	// (ErrMaterializeInFlight) while the materialize continues in the background.
	// That 503 is PRE-delivery (nothing has been written to the backend yet) so it
	// is retry-safe. It ALSO bounds the NOTIFICATION first-request probation window
	// (didOpen/didClose → 202-delivered). It does NOT bound a REQUEST's post-
	// handshake SendRequest — see ColdRequestHoldCeiling. Defaults to
	// DefaultLSPMaterializeWaitBudget (15s) when zero.
	MaterializeWaitBudget time.Duration
	// ColdRequestHoldCeiling bounds a REQUEST-path (tools/call, generic forward)
	// post-handshake SendRequest — the send is awaited under min(client-ctx,
	// ColdRequestHoldCeiling). A tools/call is a REQUEST whose response the client
	// needs and whose bytes SendRPC writes to the backend BEFORE awaiting, so a
	// probation-style fast-fail after delivery would make the client retry a fresh
	// (duplicate) side-effecting call. On ceiling expiry (the call WAS delivered)
	// the proxy returns a NON-retryable controlled error, never a retry 503. Must
	// be sized >= the slowest fronted backend's realistic worst-case cold index
	// (rust-analyzer / clangd / large-TS routinely exceed 60s) and STRICTLY between
	// MaterializeWaitBudget and ColdStartMaxProbation. Defaults to
	// DefaultLSPColdRequestHoldCeiling (120s) when zero; clamped up on a misordered
	// config (see NewLazyProxy).
	ColdRequestHoldCeiling time.Duration
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
	// MaterializeWaitBudget: the materialize-phase 503 hold + notification probation.
	DefaultLSPMaterializeWaitBudget = 15 * time.Second
	// DefaultLSPColdRequestHoldCeiling is the default request-path SendRequest hold
	// ceiling. Sized well above a slow cold LSP index (rust-analyzer / clangd /
	// large-TS can exceed 60s) and strictly between MaterializeWaitBudget and
	// DefaultLSPColdStartMaxProbation, so the proxy returns a controlled non-retryable
	// error before either the router upstream timeout or the probation watchdog fires.
	//
	// REQUIRED 4-tier ordering (F6):
	//   DefaultLSPColdStartMaxProbation > gui.lspForwardUpstreamTimeout >
	//   DefaultLSPColdRequestHoldCeiling > DefaultLSPMaterializeWaitBudget
	// NewLazyProxy clamps the 3 daemon-side tiers; the router tier lives in another
	// package and is guarded only by the compile-time
	// gui.TestLSPRouter_ForwardTimeoutOrderingInvariant. LANDMINE: exposing
	// ColdRequestHoldCeiling or ColdStartMaxProbation as a runtime flag REQUIRES
	// extending the NewLazyProxy clamp to also bound against the router tier (or
	// wiring lspForwardUpstreamTimeout into the clamp as a passed-in value), or a
	// misconfigured knob could invert the ordering and let the client see a raw
	// router 504 instead of the controlled non-retryable error.
	DefaultLSPColdRequestHoldCeiling = 120 * time.Second
	// DefaultLSPColdStartConcurrency is the default cap on OTHER Starting +
	// port-live LSP backends before this proxy refuses to cold-start.
	DefaultLSPColdStartConcurrency = 2
	// DefaultLSPColdStartMaxProbation is the default probation-watchdog window
	// after which a materialized-but-never-warmed backend is torn down. It MUST
	// stay strictly greater than the LSP-forward upstream timeout (and thus the
	// request hold ceiling); that ordering bounds a SINGLE request's own hold — a
	// request started at publish is severed by its own ceiling well before the
	// watchdog fires. It does NOT prevent Branch A from severing a LATE-ARRIVING
	// in-flight request (one that started near the probation boundary, e.g. at
	// publish+4:30, is only partway into its ceiling when the reap fires at 5:00).
	// That sever is BY DESIGN — the never-warmed backend is presumed wedged — and
	// the severed request receives the controlled non-retryable 500 via
	// reapSeveredGeneration, never a retryable-looking error.
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
	// lastWrittenLifecycle is the running-state lifecycle value this proxy last
	// wrote to the registry via reconcileRegistryLifecycleLocked. It makes the
	// reconcile idempotent — YAML is written only when the derived state differs,
	// so warm request traffic does not churn the registry (Predicate 2). It is a
	// SHADOW of the registry, so it MUST be reset ("") whenever an out-of-band
	// writer changes the row: every endpointGeneration bump (publish + all four
	// teardowns) and reserveMaterializedSlot's flock Starting write. The gen guard
	// prevents a WRONG write; the shadow reset prevents a MISSING write.
	lastWrittenLifecycle string
	// reapSeveredGeneration records the endpoint generation of an in-flight request
	// that the probation watchdog (reapWedgedProbation Branch A) severed by tearing
	// the backend down while inflightBackendRequests > 0. The request handler
	// compares its captured generation against this: on a match, the DELIVERED
	// in-flight request gets the same NON-retryable controlled error as a hold-
	// ceiling expiry (HTTP 500 + do-not-retry) instead of a generic retryable-looking
	// -32603 (which would let an agent duplicate a partially-executed mutating tool).
	// Generations are strictly monotonic, so a recorded value matches only the exact
	// severed flight and is never reused (F1).
	reapSeveredGeneration uint64

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
	// docRefsEpoch identifies the CURRENT docRefs map instance; resetDocRefs bumps
	// it under docRefsMu. applyDocRef returns the epoch its transition landed in,
	// and rollbackDocRef no-ops when the epoch has moved on (r4-F1): a rollback for
	// a transition applied to a map that a teardown has since RESET must not inject
	// a phantom refcount into the fresh map (which would permanently absorb the
	// next legitimate didOpen). Epoch-under-docRefsMu (not endpointGeneration under
	// p.mu) is deliberate: the epoch is captured atomically WITH the apply, so
	// there is no capture-vs-apply window, and no cross-mutex ordering to reason
	// about.
	docRefsEpoch uint64
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
	if cfg.ColdRequestHoldCeiling == 0 {
		cfg.ColdRequestHoldCeiling = DefaultLSPColdRequestHoldCeiling
	}
	if cfg.ColdStartConcurrency == 0 {
		cfg.ColdStartConcurrency = DefaultLSPColdStartConcurrency
	}
	if cfg.ColdStartMaxProbation == 0 {
		cfg.ColdStartMaxProbation = DefaultLSPColdStartMaxProbation
	}
	// Enforce the timeout-ordering invariant that keeps the request-hold, the
	// materialize budget, and the probation watchdog from inverting (reliability
	// constraint): ColdStartMaxProbation > ColdRequestHoldCeiling > MaterializeWaitBudget.
	// Clamp + warn on a misordered config so a misconfiguration can never let the
	// request ceiling fire before the materialize budget. NOTE (r4-F3): the
	// probation>ceiling ordering bounds a SINGLE request's own hold only — Branch A
	// MAY still sever a late-arriving in-flight request near the probation
	// boundary (by design; the severed request gets the controlled non-retryable
	// 500 via reapSeveredGeneration). (The cross-component upper bound
	// ColdStartMaxProbation > LSP-forward upstream timeout > ColdRequestHoldCeiling
	// is asserted against the router constant by a gui-package test.)
	//
	// The ceiling>budget clamp applies UNCONDITIONALLY: the ceiling/budget tiers
	// govern the request-hold and materialize phases regardless of whether the
	// probation watchdog is enabled, so a disabled watchdog must not silently
	// permit a ceiling that fires before the materialize budget (r4-F5).
	if cfg.ColdRequestHoldCeiling <= cfg.MaterializeWaitBudget {
		clamped := 2 * cfg.MaterializeWaitBudget
		fmt.Fprintf(daemonDiagWriter(),
			"warn: lazy_proxy: ColdRequestHoldCeiling (%s) <= MaterializeWaitBudget (%s); clamping ceiling to %s to preserve the timeout ordering\n",
			cfg.ColdRequestHoldCeiling, cfg.MaterializeWaitBudget, clamped)
		cfg.ColdRequestHoldCeiling = clamped
	}
	// SKIP ONLY the probation clamp when the watchdog is DISABLED via a negative
	// ColdStartMaxProbation sentinel (documented disable, mirrors ColdStartConcurrency<0):
	// the operator has taken manual control for a backend whose cold index legitimately
	// exceeds 5m, and the probation clamp would otherwise silently re-arm the watchdog
	// at 2×ceiling, reaping every cold start in a churn loop (r3-F3).
	if cfg.ColdStartMaxProbation > 0 && cfg.ColdStartMaxProbation <= cfg.ColdRequestHoldCeiling {
		clamped := 2 * cfg.ColdRequestHoldCeiling
		fmt.Fprintf(daemonDiagWriter(),
			"warn: lazy_proxy: ColdStartMaxProbation (%s) <= ColdRequestHoldCeiling (%s); clamping probation to %s to preserve the timeout ordering\n",
			cfg.ColdStartMaxProbation, cfg.ColdRequestHoldCeiling, clamped)
		cfg.ColdStartMaxProbation = clamped
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
	srv := &http.Server{
		Addr:              addr,
		Handler:           p.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		// WriteTimeout 0: handlers own cancellation via r.Context().
	}
	// Publish listener + server under p.mu: Stop reads p.server on a different
	// goroutine (the ListenAndServe-in-a-goroutine + concurrent Stop pattern), so
	// an unsynchronized write here races that read.
	p.mu.Lock()
	p.listener = ln
	p.server = srv
	p.mu.Unlock()
	return nil
}

// Serve runs the request loop on the listener populated by Bind. Returns
// http.ErrServerClosed after a clean Stop.
func (p *LazyProxy) Serve() error {
	p.mu.Lock()
	ln, srv := p.listener, p.server
	p.mu.Unlock()
	if ln == nil || srv == nil {
		return errors.New("proxy not bound — call Bind() first")
	}
	return srv.Serve(ln)
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
	p.lastWrittenLifecycle = "" // Predicate 2: reset the shadow on the gen bump
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
	// Read p.server under p.mu — Bind writes it on a different goroutine under the
	// same lock (the ListenAndServe-in-a-goroutine + concurrent Stop pattern).
	p.mu.Lock()
	srv := p.server
	p.mu.Unlock()
	if srv != nil {
		shutdownErr := srv.Shutdown(ctx)
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
	// Predicate 1 (await-after-delivery): a tools/call is a REQUEST whose response
	// the client needs and whose bytes SendRPC writes to the backend BEFORE it
	// awaits, so a probation-style fast-fail after delivery would make the client
	// retry a FRESH, duplicate side-effecting call. Await it under
	// min(client-ctx, ColdRequestHoldCeiling) — NO probation 503 on the send path.
	// (The materialize-phase ErrMaterializeInFlight 503 handled above is pre-
	// delivery and stays retryable.) On ceiling expiry the call was delivered →
	// a NON-retryable controlled error, never a retry hint. NOTE: this ceiling is
	// the UNIVERSAL request bound — it applies to WARM requests too (a behavior
	// change from the old !warmed-only probation), deliberately, to protect the
	// direct-per-daemon-port path from a no-client-timeout hang up to the 5m watchdog.
	callCtx, cancel := context.WithTimeout(r.Context(), p.cfg.ColdRequestHoldCeiling)
	defer cancel()
	stopHeld := p.armColdForwardHeldEvent(req.Method)
	resp, err := ep.SendRequest(callCtx, req)
	stopHeld()
	if err != nil {
		// Saturation refusal (SendRPC pending cap) FIRST: the error's identity
		// proves nothing was written (pre-delivery) — that fact outranks every
		// delivered-shaped classification below, including the reap-severed
		// marker (a saturated never-warmed backend crossing the probation-reap
		// boundary must NOT convert a pre-delivery refusal into the
		// non-retryable "delivered" 500 — bot #493 P2). Retryable 503, NEVER
		// teardown: the backend is HEALTHY; tearing down would kill every
		// delivered in-flight request and re-enter the same fan-out cold (the
		// third identity class in the #492 error-shape model; see
		// ErrTooManyPending in host.go).
		if errors.Is(err, ErrTooManyPending) {
			writeRPCErrorStatus(w, req.ID, http.StatusServiceUnavailable, rpcErrInternalError, err.Error(), 5)
			return
		}
		// Our request-hold-ceiling fired (client ctx still live): the request was
		// delivered and may have partially executed → non-retryable controlled error.
		if p.isColdHoldCeilingDeadline(r.Context(), err) {
			p.writeColdRequestHeldError(w, req.ID, p.isWarmed())
			return
		}
		// The probation watchdog severed this DELIVERED in-flight request (F1):
		// same non-retryable controlled error, not a retryable-looking -32603. Branch A
		// only reaps a !warmed backend and already tore it down, so pass warmed=false
		// and do NOT onSendFailure again.
		if p.wasReapSevered(gen) {
			p.writeColdRequestHeldError(w, req.ID, false)
			return
		}
		// Differentiate client-cancel from backend failure. A client disconnect or
		// its own timeout returns context.Canceled / DeadlineExceeded even when the
		// backend is healthy; tearing down on client cancel would force avoidable
		// rematerialization for every other caller of the same proxy.
		if isClientCancelErr(r.Context(), err) {
			writeRPCError(w, req.ID, rpcErrInternalError, err.Error())
			return
		}
		// Backend died mid-stream or stdio channel failed. Evict the cached
		// endpoint (gen-guarded), clear the inflight gate (so the next call
		// re-materializes), and mark the registry as Failed so `status` surfaces it.
		p.onSendFailure(gen, err)
		writeRPCError(w, req.ID, rpcErrInternalError, err.Error())
		return
	}
	// First successful response ends probation and reconciles LifecycleActive.
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
	// Predicate 1: a generic forwarded method is a REQUEST — await under
	// min(client-ctx, ColdRequestHoldCeiling), no send-path probation 503. See
	// handleToolsCall for the delivered-then-do-not-retry rationale and the
	// universal-warm-bound note.
	callCtx, cancel := context.WithTimeout(r.Context(), p.cfg.ColdRequestHoldCeiling)
	defer cancel()
	stopHeld := p.armColdForwardHeldEvent(req.Method)
	resp, err := ep.SendRequest(callCtx, req)
	stopHeld()
	if err != nil {
		// Request-hold-ceiling fired (delivered) → non-retryable controlled error.
		// Saturation refusal FIRST — pre-delivery identity outranks every
		// delivered-shaped branch below (see handleToolsCall; bot #493 P2).
		if errors.Is(err, ErrTooManyPending) {
			writeRPCErrorStatus(w, req.ID, http.StatusServiceUnavailable, rpcErrInternalError, err.Error(), 5)
			return
		}
		if p.isColdHoldCeilingDeadline(r.Context(), err) {
			p.writeColdRequestHeldError(w, req.ID, p.isWarmed())
			return
		}
		// Probation watchdog severed this delivered in-flight request (F1) — see
		// handleToolsCall.
		if p.wasReapSevered(gen) {
			p.writeColdRequestHeldError(w, req.ID, false)
			return
		}
		// Client-cancel is not a backend failure — see handleToolsCall.
		if isClientCancelErr(r.Context(), err) {
			writeRPCError(w, req.ID, rpcErrInternalError, err.Error())
			return
		}
		p.onSendFailure(gen, err)
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
// timeout while the backend is in first-request probation (!warmed). NOTIFICATION-
// ONLY (Predicate 1): the request handlers now await under ColdRequestHoldCeiling
// instead. handleDocLifecycle uses this so a didOpen/didClose during cold indexing
// fast-fails to a 202-delivered at the budget instead of holding the connection.
// Once warmed the client context passes through unmodified.
func (p *LazyProxy) boundedCallCtx(ctx context.Context, warmed bool) (context.Context, context.CancelFunc) {
	if warmed {
		return ctx, func() {}
	}
	return context.WithTimeout(ctx, p.cfg.MaterializeWaitBudget)
}

// isProbationDeadline reports whether err is our own probation-budget deadline
// (not the client's own cancel/deadline). NOTIFICATION-ONLY (Predicate 1): only
// handleDocLifecycle consults it, to classify the 202-delivered notification case.
// Fires only while !warmed, only when the CLIENT context is still live, and only
// for a DeadlineExceeded error.
func (p *LazyProxy) isProbationDeadline(clientCtx context.Context, warmed bool, err error) bool {
	return !warmed && clientCtx.Err() == nil && errors.Is(err, context.DeadlineExceeded)
}

// coldForwardHeldEventFn is the injectable test seam for the
// lsp-cold-forward-held observability event. It is read by DETACHED
// time.AfterFunc callbacks: timer.Stop() does not wait for a callback that has
// already started, so a callback can outlive the arming request (and, in tests,
// the arming test) — a plain package var would DATA-RACE a test's cleanup
// restore-write against such a late read (caught under -race). Atomic access
// makes the seam race-free by construction; nil selects the production diag
// write in emitColdForwardHeldEvent.
var coldForwardHeldEventFn atomic.Pointer[func(backendKind, workspacePath, method, heldBeyond, ceiling string)]

// emitColdForwardHeldEvent routes the lsp-cold-forward-held event through the
// atomic seam (test override) or the best-effort daemon diag writer (production).
func emitColdForwardHeldEvent(backendKind, workspacePath, method, heldBeyond, ceiling string) {
	if fn := coldForwardHeldEventFn.Load(); fn != nil {
		(*fn)(backendKind, workspacePath, method, heldBeyond, ceiling)
		return
	}
	fmt.Fprintf(daemonDiagWriter(),
		"warn: lazy_proxy: lsp-cold-forward-held backend=%s workspace=%s method=%s held_beyond=%s ceiling=%s\n",
		backendKind, workspacePath, method, heldBeyond, ceiling)
}

// armColdForwardHeldEvent (Predicate 1, reliability #5) emits the structured
// lsp-cold-forward-held observability event if a forwarded REQUEST is still in
// flight after ~MaterializeWaitBudget — the only fleet-visible signal for a
// 40-60s cold hold now that the request path no longer 503s at 15s. Returns a
// stop func the caller invokes once SendRequest returns (a fast/warm forward
// stops the timer before it fires, so no event).
func (p *LazyProxy) armColdForwardHeldEvent(method string) func() {
	t := time.AfterFunc(p.cfg.MaterializeWaitBudget, func() {
		emitColdForwardHeldEvent(p.cfg.BackendKind, p.cfg.WorkspacePath, method,
			p.cfg.MaterializeWaitBudget.String(), p.cfg.ColdRequestHoldCeiling.String())
	})
	return func() { t.Stop() }
}

// isColdHoldCeilingDeadline reports whether err is our OWN request-hold-ceiling
// deadline (Predicate 1): the client's own context is still live and err is
// DeadlineExceeded (from the min(client, ColdRequestHoldCeiling) timeout). Applies
// to warm and cold requests alike — the ceiling is the universal request bound.
func (p *LazyProxy) isColdHoldCeilingDeadline(clientCtx context.Context, err error) bool {
	return clientCtx.Err() == nil && errors.Is(err, context.DeadlineExceeded)
}

// coldStartInProgressMessage is the operator/agent-facing body for the retryable
// 503-warming message. It serves two PRE-delivery / fire-and-forget cases (both
// retry-safe): the NOTIFICATION probation deadline, AND the request-path
// materialize-in-flight refusal (ErrMaterializeInFlight, via writeColdStartRefusal)
// which fires BEFORE anything is written to the backend. The POST-delivery request
// hold-ceiling case uses writeColdRequestHeldError (500, non-retryable) instead.
func (p *LazyProxy) coldStartInProgressMessage() string {
	return fmt.Sprintf("language backend cold start in progress (%s, %s); retry in ~15s",
		p.cfg.BackendKind, p.cfg.WorkspacePath)
}

// writeColdStartInProgress emits the 503 + JSON-RPC -32603 + Retry-After: 15
// cold-start-in-progress response. Used by the NOTIFICATION probation path AND the
// request-path PRE-delivery materialize-in-flight refusal (writeColdStartRefusal) —
// both retry-safe (nothing written to the backend yet). Distinct from the
// POST-delivery writeColdRequestHeldError (500, non-retryable).
func (p *LazyProxy) writeColdStartInProgress(w http.ResponseWriter, id json.RawMessage) {
	writeRPCErrorStatus(w, id, http.StatusServiceUnavailable, rpcErrInternalError,
		p.coldStartInProgressMessage(), 15)
}

// writeColdRequestHeldError emits the NON-retryable controlled error for a REQUEST
// that was DELIVERED to the backend but did not complete within ColdRequestHoldCeiling
// (Predicate 1, reliability #2), OR that a probation reap severed mid-flight (F1). It
// deliberately carries NO retry wording and NO Retry-After header — the call may have
// partially executed, so an auto-retry of a mutating tool would double-execute. HTTP
// 500 is chosen (NOT the retryable 503-cold-start, NOT a raw router 504): a fail-loud,
// terminal, non-retryable status the router passes through verbatim, distinct from both
// the retryable materialize 503 and the router's own upstream-timeout 504 (which the
// proxy's earlier-firing ceiling prevents the client from ever seeing). The
// lsp-cold-forward-held event is the fleet monitoring signal, so operators alert on the
// event, not the raw 500.
//
// warmed distinguishes the two hold causes for an accurate operator/agent message
// (F5): a COLD backend was still indexing; a WARM backend's single query exceeded the
// universal request-hold ceiling (a legitimately slow workspace/symbol on a huge repo,
// not indexing) — misreporting "still indexing" on the warm case would misdirect
// diagnosis. Both keep the 500 + do-not-retry semantics.
func (p *LazyProxy) writeColdRequestHeldError(w http.ResponseWriter, id json.RawMessage, warmed bool) {
	var cause string
	if warmed {
		cause = fmt.Sprintf("request exceeded the %s hold ceiling on a warm backend (slow query, not indexing)", p.cfg.ColdRequestHoldCeiling)
	} else {
		cause = fmt.Sprintf("language backend still indexing after %s (cold start)", p.cfg.ColdRequestHoldCeiling)
	}
	msg := fmt.Sprintf("%s; the call was delivered and may have partially executed — do not auto-retry mutating calls (%s, %s)",
		cause, p.cfg.BackendKind, p.cfg.WorkspacePath)
	writeRPCErrorStatus(w, id, http.StatusInternalServerError, rpcErrInternalError, msg, 0)
}

// wasReapSevered reports whether the probation watchdog (reapWedgedProbation Branch A)
// severed a delivered in-flight request of this exact generation (F1). Generations are
// strictly monotonic so a recorded match is unambiguous and never reused.
func (p *LazyProxy) wasReapSevered(gen uint64) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.reapSeveredGeneration != 0 && p.reapSeveredGeneration == gen
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
	// docEpoch identifies the docRefs map instance the transition landed in; the
	// rollback sites below pass it so a rollback racing a teardown's resetDocRefs
	// no-ops instead of injecting a phantom count into the fresh map (r4-F1).
	forward, docEpoch, err := p.applyDocRef(uri, isOpen)
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
		p.rollbackDocRef(uri, isOpen, docEpoch)
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
		// DELIVERED-then-await-cut classification (Edge 2), keyed on ERROR IDENTITY
		// (bot PR #492 r3 P2). The SendRPC invariant this rests on: SendRPC returns
		// a context-shaped error (bare ctx.Err() — context.Canceled/
		// DeadlineExceeded) ONLY from its post-write select, i.e. only after
		// writeStdin returned nil; and per the io.Writer contract a nil write error
		// means the FULL notification line was accepted by the backend's stdin.
		// Every pre-/mid-write failure surfaces as a wrapped NON-context error
		// ("invalid JSON-RPC request:", "marshal rewritten:", "write stdin: %w" —
		// writeStdin takes no ctx, so a context error can never hide inside it) or
		// as the death-arm strings ("backend host stopped"/"backend subprocess
		// exited"). Therefore: err is context-shaped ⇔ the notification was fully
		// written before the wait was cut ⇔ DELIVERED. A broken-pipe/EOF/host-
		// stopped error must NEVER classify as delivered — even under an already-
		// canceled client ctx (isClientCancelErr now enforces this by identity, not
		// ctx state). A textDocument/didOpen|didClose is fire-and-forget, so both
		// DELIVERED shapes answer 202 and KEEP the refcount:
		//   (a) our own probation budget fired while the client ctx is still live
		//       (cold path, !warmed — isProbationDeadline); and
		//   (b) the client's own ctx canceled/deadlined AFTER delivery — the WARM
		//       path awaits under the raw client ctx with NO budget bound (see
		//       boundedCallCtx), so a slow-but-delivered notification that outlives
		//       the client deadline lands here (isClientCancelErr). Before this fix
		//       the warm path rolled the refcount back on (b), desyncing the count:
		//       the proxy thought the doc was closed while the backend held it open,
		//       so the next didOpen duplicated the upstream open (protocol violation)
		//       / a didClose fired a premature repeated close. Keeping the refcount +
		//       202-delivered is the established notification contract. No teardown —
		//       the backend is healthy, just slow; warmed stays false until it
		//       actually RESPONDS.
		// (The r2 RESIDUAL — "client-cancel + broken-pipe keeps a phantom refcount"
		// — is CLOSED by the identity-keyed isClientCancelErr: a pre-delivery io
		// failure under a canceled ctx now classifies NOT-delivered and takes the
		// failure path below.)
		if p.isProbationDeadline(r.Context(), warmed, err) || isClientCancelErr(r.Context(), err) {
			// Bot PR #492 r2 P2: a ctx-shaped error does NOT prove the backend
			// outlived the call — Go's select picks pseudo-randomly among READY
			// cases, so SendRPC can return its ctx arm even though done/childExited
			// was ALSO ready after the write. Keeping on that shape would cache a
			// DEAD endpoint (skipping onSendFailure) and retain a refcount the
			// dead backend no longer honors: the next didOpen for this uri would be
			// absorbed (never forwarded) and the dead endpoint would linger until
			// some unrelated forward trips teardown. So before keeping, probe the
			// backend's own death arms (BackendAlive: non-blocking select-default
			// on the SAME done/childExited channels SendRPC selects on). Alive →
			// genuine post-delivery cancel/budget-expiry: 202 + keep, exactly as
			// before. Dead → fall through to the failure path: the refcount
			// bookkeeping is moot (teardown's resetDocRefs clears it anyway) and
			// teardown-now beats a lingering cached dead endpoint. BOTH delivered
			// arms are guarded — the probation-budget deadline (a) races childExited
			// identically (our budget and the child's exit simultaneously ready).
			// Endpoints without the optional probe facet keep the prior contract
			// (assumed alive).
			if endpointBackendAlive(ep) {
				w.WriteHeader(http.StatusAccepted)
				return
			}
			err = fmt.Errorf("backend dead at doc-notification cancel/deadline (delivered, endpoint unusable): %w", err)
		}
		// Saturation refusal (SendRPC pending cap): pre-delivery, backend
		// HEALTHY — roll back the optimistic refcount (nothing was written) but
		// do NOT tear down; retryable 503 (see handleToolsCall's branch).
		if errors.Is(err, ErrTooManyPending) {
			p.rollbackDocRef(uri, isOpen, docEpoch)
			writeRPCErrorStatus(w, req.ID, http.StatusServiceUnavailable, rpcErrInternalError, err.Error(), 5)
			return
		}
		// Send failure with the backend dead or the write never committed: roll
		// back the optimistic refcount transition and tear down so the next request
		// re-materializes a fresh backend.
		p.rollbackDocRef(uri, isOpen, docEpoch)
		p.onSendFailure(gen, err)
		writeRPCError(w, req.ID, rpcErrInternalError, err.Error())
		return
	}
	// The backend served a request → first success ends probation (F2).
	p.markWarmedOnFirstSuccess(gen)
	if resp != nil && resp.Error != nil {
		// Backend rejected the lifecycle notification, so do not retain a
		// refcount that would suppress a future legitimate open/close. The
		// client-visible HTTP contract remains notification-like (202/no body).
		p.rollbackDocRef(uri, isOpen, docEpoch)
	}
	// Notification delivered. JSON-RPC notifications take no response body.
	w.WriteHeader(http.StatusAccepted)
}

// applyDocRef records an open/close against uri and reports whether the
// operation should be forwarded to the upstream backend, plus the docRefs map
// EPOCH the transition landed in (pass it to rollbackDocRef). didOpen forwards
// on the 0->1 transition; didClose forwards on the 1->0 transition. A didClose
// against an already-zero count is absorbed and never drives the count
// negative (a spurious or duplicate close from one agent must not affect the
// document state another agent still depends on).
func (p *LazyProxy) applyDocRef(uri string, open bool) (bool, uint64, error) {
	if len(uri) > maxDocURIBytes {
		return false, 0, fmt.Errorf("textDocument.uri exceeds %d bytes", maxDocURIBytes)
	}
	p.docRefsMu.Lock()
	defer p.docRefsMu.Unlock()
	if open {
		prev := p.docRefs[uri]
		if prev == 0 && len(p.docRefs) >= maxTrackedDocRefs {
			return false, 0, fmt.Errorf("too many tracked text documents (max %d)", maxTrackedDocRefs)
		}
		p.docRefs[uri] = prev + 1
		return prev == 0, p.docRefsEpoch, nil
	}
	prev := p.docRefs[uri]
	if prev <= 0 {
		// Close with no recorded open — nothing upstream to release.
		return false, p.docRefsEpoch, nil
	}
	if prev == 1 {
		delete(p.docRefs, uri)
		return true, p.docRefsEpoch, nil
	}
	p.docRefs[uri] = prev - 1
	return false, p.docRefsEpoch, nil
}

// rollbackDocRef undoes the most recent applyDocRef transition for uri when
// the corresponding upstream forward could not be delivered. It is the exact
// inverse: a failed didOpen decrements (back toward 0), a failed didClose
// re-increments (restoring the open the close was about to release).
//
// epoch is the value applyDocRef returned. The rollback no-ops when the map
// epoch has moved on (r4-F1): a concurrent teardown's resetDocRefs replaced the
// map, so the transition being "undone" no longer exists — applying the inverse
// anyway would inject a phantom refcount into the FRESH map (e.g. a rolled-back
// didClose re-incrementing to 1 on a backend with zero open docs), permanently
// absorbing the next legitimate didOpen for that URI.
func (p *LazyProxy) rollbackDocRef(uri string, open bool, epoch uint64) {
	p.docRefsMu.Lock()
	defer p.docRefsMu.Unlock()
	if p.docRefsEpoch != epoch {
		return
	}
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

// resetDocRefs clears all per-URI open counts and bumps the map epoch so a
// late rollbackDocRef for a pre-reset transition no-ops (r4-F1). Called
// whenever the backend is torn down (Stop / onSendFailure / reapIdleBackend /
// reapWedgedProbation): the fresh backend that the next request materializes
// has no open documents, so any retained (or re-injected) count would wrongly
// absorb the first didOpen after the restart.
func (p *LazyProxy) resetDocRefs() {
	p.docRefsMu.Lock()
	p.docRefs = make(map[string]int)
	p.docRefsEpoch++
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

// isClientCancelErr reports whether err IS a context cancel/deadline error
// attributable to the client's own request context — the single owner of the
// client-cancel classification for all three forward handlers (tools/call,
// generic forward, doc lifecycle).
//
// Both conjuncts are load-bearing (bot PR #492 r3 P2 — the ROOT fix):
//
//   - ERROR IDENTITY (errors.Is Canceled/DeadlineExceeded): the classification
//     keys on what the returned error IS, never on ambient ctx state. The old
//     shape ("ctx.Err() != nil → true for ANY err") classified a broken-pipe /
//     EOF / host-stopped send failure as client-cancel whenever the client had
//     ALSO canceled — for the doc path that kept a refcount for a notification
//     that was NEVER delivered (permanent phantom: the retained count absorbs
//     the retry, so no later send ever trips teardown), and for the request
//     paths it skipped the teardown of a genuinely dead backend. The old
//     comment's fear ("SendRequest wraps context.Canceled where errors.Is would
//     miss it") is false for the actual error surface: SendRPC returns ctx.Err()
//     BARE from its post-write select, and every pre-/mid-write failure wraps a
//     NON-context io/marshal error ("write stdin: %w" — writeStdin takes no
//     ctx), so errors.Is is exact.
//   - CLIENT ATTRIBUTION (ctx.Err() != nil, on the CLIENT's ctx, not the
//     ceiling/budget-wrapped callCtx): a context-shaped error while the client
//     is still live came from an internal bound (hold ceiling / probation
//     budget) and is owned by the earlier isColdHoldCeilingDeadline /
//     isProbationDeadline checks — this conjunct keeps the predicate correct
//     even if a caller reorders those checks.
func isClientCancelErr(ctx context.Context, err error) bool {
	if err == nil {
		return false
	}
	return (errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)) &&
		ctx.Err() != nil
}

// backendAliveProber is the OPTIONAL MCPEndpoint facet the doc-lifecycle
// delivered⇒keep classification consults before retaining a refcount on a
// ctx-shaped SendRequest error (bot PR #492 r2 P2). It is a separate facet
// rather than a new MCPEndpoint method because the probe is stdio-host
// death-channel knowledge: stdioHostEndpoint (the only production endpoint the
// lazy proxy fronts) implements it; an endpoint without the facet is assumed
// alive, which preserves the prior delivered⇒keep contract exactly.
type backendAliveProber interface {
	BackendAlive() bool
}

// endpointBackendAlive probes ep through the optional backendAliveProber facet.
// Returns true (assume alive → keep) when the endpoint does not expose a probe.
func endpointBackendAlive(ep MCPEndpoint) bool {
	if pr, ok := ep.(backendAliveProber); ok {
		return pr.BackendAlive()
	}
	return true
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
			// Predicate 2: reconcile the row to its derived running-state at every
			// endpoint acquisition. Idempotent (shadow) so warm traffic does not
			// churn; authoritative so a row a concurrent reserve downgraded to
			// Starting is restored to Active here.
			p.reconcileRegistryLifecycleLocked(gen)
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

	// Finding 2: a concurrent caller may have published (and warmed) an endpoint
	// between our fast-path miss above and here. Re-check under the lock before
	// reserving a slot — reserveMaterializedSlot would otherwise downgrade the
	// registry row Active->Starting and the gate would re-materialize a redundant
	// wrapper that throws the warm backend back under probation.
	p.mu.Lock()
	if p.endpoint != nil {
		ep := p.endpoint
		gen := p.endpointGeneration
		p.reconcileRegistryLifecycleLocked(gen)
		p.beginBackendRequestLocked(time.Now().UTC())
		p.mu.Unlock()
		return ep, gen, nil
	}
	p.mu.Unlock()

	// Slow path: reserve a cold-start slot (marks Starting BEFORE entering the
	// gate so `status` shows "starting" while the singleflight is in flight),
	// then go through the gate.
	wroteStarting, err := p.reserveMaterializedSlot()
	if err != nil {
		return nil, 0, err
	}
	if wroteStarting {
		// reserve wrote Starting out-of-band under the flock — invalidate the
		// reconcile shadow so the acquisition-return reconcile re-derives and
		// re-writes the TRUE state (Active if a concurrent caller warmed the
		// endpoint after our reserve). Without this the shadow could still read
		// Active from a prior reconcile while the registry now reads Starting,
		// leaving the row stuck Starting (Predicate 2).
		p.mu.Lock()
		p.lastWrittenLifecycle = ""
		p.mu.Unlock()
	}
	p.markStartingReserved(time.Now().UTC())

	key := p.inflightKey()
	_, err = p.gate.DoBounded(ctx, key, p.cfg.MaterializeWaitBudget, func(fnCtx context.Context) (any, error) {
		// Finding 2 (tight-race backstop): if a concurrent caller published an
		// endpoint after we reserved but before this singleflight fn ran, do NOT
		// re-materialize — return the live endpoint so publishMaterializedEndpoint
		// is skipped and warmed/generation are preserved.
		p.mu.Lock()
		if live := p.endpoint; live != nil {
			// Predicate 2: restore the derived state (Active if the concurrent
			// publisher already warmed) — this is a load-bearing site for the
			// stuck-Starting fix when our own reserve downgraded the row.
			p.reconcileRegistryLifecycleLocked(p.endpointGeneration)
			p.mu.Unlock()
			return live, nil
		}
		p.mu.Unlock()
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
			// F2 (bot r3): this caller may have downgraded the row to Starting via
			// its reserve and is now aborting WITHOUT reaching the acquisition-
			// return reconcile. In the known race (a concurrent caller publishes +
			// warms between our pre-reserve snapshot and the reserve's flock write)
			// the registry would linger Starting while the proxy is live+warmed,
			// until some later request reconciles. Restore the derived state here.
			// Gated on endpoint != nil so the common budget-expiry 503 (no endpoint
			// yet — the detached fn owns that outcome and reconciles at publish)
			// does not churn a redundant Starting write; skipped when closing
			// (Stop owns final state).
			if !p.closed.Load() {
				p.mu.Lock()
				if p.endpoint != nil {
					p.reconcileRegistryLifecycleLocked(p.endpointGeneration)
				}
				p.mu.Unlock()
			}
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
	// Predicate 2: the authoritative acquisition-return reconcile. Every caller
	// (winner + losers) passes here, so a row a concurrent reserve downgraded to
	// Starting is reconciled back to its derived state before use.
	p.reconcileRegistryLifecycleLocked(gen)
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
		// This flight is ending WITHOUT a publish (bot PR #492 r1 P2): release its
		// startingSince marker so a mid-reap final reconcile cannot derive a
		// phantom Starting row from it — a marker whose flight is dead counts
		// against every other proxy's cold-start gate / hard cap (until Branch B's
		// ≤5m probation timer) while no materialize exists to ever warm it. The
		// clear is FLIGHT-SCOPED by the atomicity of this closed/reaping
		// observation under p.mu: while reaping is held, ensureMaterialized's spin
		// blocks every new caller BEFORE reserve/markStartingReserved, and a
		// closed proxy refuses new callers outright — so no post-rejection flight
		// can have re-armed the marker; any armed value was set by a caller
		// feeding THIS singleflight (the fn is the flight's guaranteed terminal
		// executor even when every bounded waiter abandoned the join, which is why
		// the clear lives here and not in ensureMaterialized's error branch). No
		// reconcile here: for the reaping case the reap's own final reconcile
		// settles the row to derived truth (and writing Configured now would
		// violate the Stop()-before-Configured ordering); for the closed case
		// LazyProxy.Stop owns the final state (and clears the marker itself).
		p.startingSince = time.Time{}
		p.mu.Unlock()
		_ = ep.Close()
		if stopErr := p.cfg.Lifecycle.Stop(); stopErr != nil {
			fmt.Fprintf(daemonDiagWriter(), "warn: lazy_proxy: lifecycle stop after unpublishable materialize: %v\n", stopErr)
		}
		p.gate.Forget(p.inflightKey())
		return errLazyProxyUnpublishable
	}
	// Finding 2: a concurrent caller already published an endpoint (and may have
	// warmed it) while this materialize was in flight. Materialize is idempotent
	// and returned a fresh wrapper around the SAME already-running host
	// (b.host != nil), so overwriting p.endpoint here would reset warmed=false /
	// endpointPublishedAt and bump the generation, throwing an ACTIVE backend
	// back under probation (spurious 503s + Starting-slot occupancy). Keep the
	// existing endpoint and discard the redundant wrapper. Close ONLY the wrapper
	// — do NOT call Lifecycle.Stop(), which would kill the shared host the live
	// endpoint depends on. The caller re-reads the live p.endpoint.
	if p.endpoint != nil {
		p.mu.Unlock()
		_ = ep.Close()
		return nil
	}
	p.endpoint = ep
	p.endpointGeneration++
	p.lastWrittenLifecycle = "" // Predicate 2: reset the shadow on the gen bump
	p.warmed = false
	p.endpointPublishedAt = now
	p.startingSince = time.Time{}
	// Reconcile the freshly published (unwarmed) endpoint to Starting under the
	// new generation. Idempotent; the acquisition-return reconcile is the backstop.
	p.reconcileRegistryLifecycleLocked(p.endpointGeneration)
	p.mu.Unlock()
	return nil
}

// markStartingReserved records when reserveMaterializedSlot set this row Starting
// without an endpoint yet published, so the probation watchdog's belt-and-braces
// branch can reap an orphan Starting row (F1).
//
// Marker lifecycle (who clears startingSince, and why the not-cleared paths are
// deliberate — bot PR #492 r1 P2 triage of every post-mark exit):
//   - publish success → publishMaterializedEndpoint zeroes it (endpoint installed).
//   - publish REJECTED (errLazyProxyUnpublishable) → the rejection branch zeroes it
//     at the fn's terminal moment, so a mid-reap reconcile cannot derive a phantom
//     Starting from a dead flight's marker (see the comment there).
//   - teardowns (Stop / onSendFailure / both probation-reap branches / idle reap)
//     zero it in their own critical sections.
//   - ErrMaterializeInFlight / waiter ctx-cancel → KEPT armed: the detached flight
//     is still running; the marker is the truthful "materializing" signal that the
//     reconcile derives Starting from, and Branch B exempts active flights.
//   - genuine materialize error (fn ran, failed) and throttle-cached error → KEPT
//     armed DELIBERATELY: the fn/caller stamps Failed, and the armed marker is
//     Branch B's only handle on the double-fault where that swallowed registry
//     write fails (row stuck Starting with no flight — Branch B reaps it ≤5m via
//     the marker; clearing here would orphan that row forever). When the Failed
//     stamp landed, Branch B disarms the marker at its next tick without a write.
func (p *LazyProxy) markStartingReserved(now time.Time) {
	p.mu.Lock()
	// Only arm the orphan timer while no endpoint is published; a concurrent
	// publish/warm must win over a late reserve stamp.
	if p.endpoint == nil {
		p.startingSince = now
	}
	p.mu.Unlock()
}

// reconcileRegistryLifecycleLocked is the SINGLE authoritative writer of the
// running-state lifecycle transitions (Configured / Starting / Active). It MUST
// be called while holding p.mu, in the SAME critical section that mutated the
// state it derives from (endpoint, warmed, startingSince), so there is no
// check-then-write gap (Predicate 2). Death/missing states (Failed / Missing) are
// NOT written here — they are written by the teardown / fn-error paths, which
// bump the generation (resetting the shadow) so the next reconcile starts fresh.
//
// gen is the endpoint generation the caller captured; a stale caller
// (gen != p.endpointGeneration) is a no-op, preserving the "a late success can
// not overwrite a Failed a concurrent teardown just wrote" guarantee (F6a). This
// generalizes the old markWarmedOnFirstSuccess lock-across-write posture: a
// concurrent teardown bumps the generation UNDER p.mu before its own lock-free
// Failed write, so holding p.mu across our re-check + write forces a safe order.
//
// Idempotent via the lastWrittenLifecycle shadow: it writes YAML only when the
// derived state differs from what this proxy last wrote, so warm request traffic
// (derived Active, shadow Active) does not churn the registry. The shadow is
// reset on every generation bump and after reserveMaterializedSlot's out-of-band
// Starting write, so a genuine transition is never missed.
//
// Lock order: p.mu is held across PutLifecycle (which takes the workspaces.yaml
// flock internally) — i.e. p.mu → flock. This is safe because no path takes the
// flock and THEN p.mu: reserveMaterializedSlot snapshots p.mu BEFORE its flock and
// never re-acquires p.mu while holding it. A write only happens on an actual
// state change (shadow miss), so warm traffic never holds p.mu across I/O.
func (p *LazyProxy) reconcileRegistryLifecycleLocked(gen uint64) {
	if gen != p.endpointGeneration {
		return
	}
	var derived string
	switch {
	case p.endpoint != nil && p.warmed:
		derived = api.LifecycleActive
	case p.endpoint != nil || !p.startingSince.IsZero():
		derived = api.LifecycleStarting
	default:
		derived = api.LifecycleConfigured
	}
	if derived == p.lastWrittenLifecycle {
		return
	}
	var writeErr error
	if derived == api.LifecycleActive {
		// Stamp LastMaterializedAt on the Starting→Active transition; a zero
		// LastToolsCallAt preserves any existing value.
		writeErr = api.NewRegistry(p.cfg.RegistryPath).PutLifecycleWithTimestamps(
			p.cfg.WorkspaceKey, p.cfg.Language, api.LifecycleActive, "",
			time.Now().UTC(), time.Time{},
		)
	} else {
		writeErr = api.NewRegistry(p.cfg.RegistryPath).PutLifecycle(
			p.cfg.WorkspaceKey, p.cfg.Language, derived, "")
	}
	if writeErr != nil {
		// F1 (bot r3): do NOT record the shadow on a FAILED write. The shadow is a
		// mirror of what the registry actually holds; recording `derived` here
		// would make every later reconcile hit the shadow fast-path and never
		// retry the write — a transiently-failed Starting→Active move would leave
		// the row stuck Starting forever (mis-status + consuming cold-start
		// capacity while actually warm). Left untouched, the next reconcile
		// re-derives, misses the shadow, and retries.
		fmt.Fprintf(daemonDiagWriter(),
			"warn: lazy_proxy: lifecycle reconcile write (%s) failed (will retry on next reconcile): %v\n",
			derived, writeErr)
		return
	}
	p.lastWrittenLifecycle = derived
}

// markWarmedOnFirstSuccess flips warmed true on a gen-matching successful
// forwarded response and reconciles the row to Active. It is the fix for the
// stuck-Starting class (Predicate 2): warmed is dropped from the WRITE decision
// (it only avoids re-flipping) so that EVEN a "second" success (warmed already
// true) reconciles→Active idempotently — restoring Active if a concurrent
// reserveMaterializedSlot downgraded the row to Starting. gen-guarded (a stale
// caller no-ops, preserving F6a). Registry I/O happens inside
// reconcileRegistryLifecycleLocked, under p.mu.
func (p *LazyProxy) markWarmedOnFirstSuccess(gen uint64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.endpoint == nil || p.endpointGeneration != gen {
		return
	}
	p.warmed = true
	p.reconcileRegistryLifecycleLocked(gen)
}

// isWarmed reports whether the current backend has already served a first
// successful response (so the request handlers can skip the probation bound).
func (p *LazyProxy) isWarmed() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.warmed
}

// reserveMaterializedSlot marks this proxy's row Starting under the workspaces.yaml
// flock (load-bearing for the cross-proxy cold-start gate's atomic check-and-set)
// and reports whether it actually wrote Starting. It returns wroteStarting=false
// without any registry write when this proxy already holds a published endpoint
// (Predicate 2): a concurrent caller materialized after our fast-path miss, so
// downgrading the row Active→Starting would flicker it — the acquisition-return
// reconcile is the authoritative writer. The alreadyLive snapshot is taken under
// p.mu strictly BEFORE the flock and p.mu is released before the flock, so the
// p.mu→flock lock order is preserved (no AB-BA with reconcile).
func (p *LazyProxy) reserveMaterializedSlot() (bool, error) {
	p.mu.Lock()
	alreadyLive := p.endpoint != nil
	p.mu.Unlock()
	if alreadyLive {
		return false, nil
	}

	reg := api.NewRegistry(p.cfg.RegistryPath)
	capOn := p.cfg.MaterializedHardCap > 0
	coldGateOn := p.cfg.ColdStartConcurrency > 0
	if !capOn && !coldGateOn {
		if err := reg.PutLifecycle(p.cfg.WorkspaceKey, p.cfg.Language, api.LifecycleStarting, ""); err != nil {
			return false, err
		}
		return true, nil
	}
	unlock, err := reg.Lock()
	if err != nil {
		return false, fmt.Errorf("reserve materialized LSP backend slot: %w", err)
	}
	defer unlock()
	if err := reg.Load(); err != nil {
		return false, fmt.Errorf("reserve materialized LSP backend slot: %w", err)
	}
	entry, ok := reg.Get(p.cfg.WorkspaceKey, p.cfg.Language)
	if !ok {
		return false, fmt.Errorf("reserve materialized LSP backend slot: registry entry missing for %s/%s",
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
		// Predicate 3: only Starting/Active rows contribute to the cap/gate, so
		// gate the 300ms port dial behind the Lifecycle filter — a Configured /
		// Failed / Missing row's dial is pure waste that lengthens the workspaces.yaml
		// flock hold. This bounds the flock hold to (#Starting|Active) × 300ms.
		if e.Lifecycle != api.LifecycleStarting && e.Lifecycle != api.LifecycleActive {
			continue
		}
		live := e.Port > 0 && materializedSlotPortLiveFn != nil && materializedSlotPortLiveFn(e.Port)
		if !live {
			continue
		}
		active++
		if e.Lifecycle == api.LifecycleStarting {
			startingLive++
		}
	}
	if capOn && active >= p.cfg.MaterializedHardCap {
		return false, fmt.Errorf("materialized LSP backend cap reached: %d active/starting backends, cap %d",
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
		return false, &coldStartSlotsBusyError{warming: startingLive}
	}
	entry.Lifecycle = api.LifecycleStarting
	entry.LastError = ""
	reg.Put(entry)
	if err := reg.Save(); err != nil {
		return false, fmt.Errorf("reserve materialized LSP backend slot: %w", err)
	}
	return true, nil
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
	//
	// watchdogOn deliberately does NOT depend on ColdStartConcurrency (r4-F2): the
	// watchdog's usefulness is independent of the cold-start GATE — a wedged
	// never-warmed backend holds a Starting row + live port against the
	// MaterializedHardCap (and mis-reports status) even when the gate is disabled
	// (ColdStartConcurrency < 0), and reapWedgedProbation itself self-skips only on
	// ColdStartMaxProbation <= 0. This also makes the flag-help / CLAUDE.md claim
	// ("the probation watchdog stays active whenever probation is configured") true.
	idleOn := p.cfg.IdleBackendTTL > 0
	watchdogOn := p.cfg.ColdStartMaxProbation > 0
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
	var gen uint64
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
	// Capture the generation THIS reap bumped so the terminal Configured write
	// (below) can no-op if a materialize republished an endpoint over our teardown
	// and bumped the generation again (Edge 1).
	gen = p.endpointGeneration
	p.lastWrittenLifecycle = "" // Predicate 2: reset the shadow on the gen bump
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
	// Edge 1: route the terminal Configured write through the SINGLE authoritative
	// lifecycle writer (reconcileRegistryLifecycleLocked) under p.mu instead of an
	// out-of-band PutLifecycle. Post-teardown the derived state is Configured
	// (endpoint nil, not starting). Two distinct protections replace the old bare
	// write's stomp exposure:
	//   - The gen guard: every OTHER generation bumper except LazyProxy.Stop
	//     refuses while p.reaping is held, so the guard's real production trigger
	//     is a concurrent proxy Stop() mid-reap — a stale reap's write then no-ops
	//     instead of resurrecting a Configured row over a torn-down proxy.
	//   - The reserve race (the bug doc's original stomp: a cold-path reserve's
	//     flock Starting write landing before this reconcile): the reconcile
	//     derives Starting from startingSince, so once markStartingReserved has
	//     run this write preserves Starting instead of stomping it. RESIDUAL: a
	//     reserve that has written its flock Starting but not yet returned to
	//     markStartingReserved leaves a microseconds-wide gap where this
	//     reconcile still derives Configured — narrowed from the old ~Stop()-wide
	//     window, not fully closed; self-heals at publish-time reconcile.
	//     The INVERSE hazard — a DEAD flight's stale startingSince making this
	//     reconcile persist a phantom Starting (bot PR #492 r1 P2) — is closed at
	//     the flight's terminal moment: publishMaterializedEndpoint's rejection
	//     branch zeroes the marker before this reconcile can observe it (full
	//     marker lifecycle: markStartingReserved doc).
	// Folding it in also keeps the lastWrittenLifecycle shadow consistent with
	// the registry — the last non-reconcile RUNNING-lifecycle Configured writer is
	// removed (the pre-concurrency startup seeds in ListenAndServe and the CLI
	// pre-Bind remain, both gen-0 before Serve). Stop() still precedes the write,
	// preserving "don't claim Configured until the backend is actually stopped".
	p.mu.Lock()
	p.reconcileRegistryLifecycleLocked(gen)
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
		// F1: if a delivered request is in flight over this never-warmed backend,
		// record its generation so the severed handler emits the non-retryable
		// controlled error (delivered, may have partially executed) rather than a
		// retryable-looking -32603 when its SendRPC returns "backend host stopped".
		if p.inflightBackendRequests > 0 {
			p.reapSeveredGeneration = p.endpointGeneration
		}
		p.reaping = true
		p.endpoint = nil
		p.endpointGeneration++
		p.lastWrittenLifecycle = "" // Predicate 2: reset the shadow on the gen bump
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
	p.mu.Unlock()

	// Only reap if the registry row really is still Starting; if it has already
	// advanced (a teardown stamped Failed/Configured, or a warm stamped Active
	// elsewhere) there is no orphan slot to free. startingSince is NOT cleared
	// before this verification (r4-F4): a transient registry-read failure must
	// leave the orphan timer armed so the next tick retries — clearing first
	// permanently disarmed Branch B for the very orphan it exists to clean.
	rowStarting, loadErr := p.registryRowIsStarting()
	if loadErr != nil {
		// Transient read failure (flock contention, torn write): retain
		// startingSince and retry on the next tick.
		fmt.Fprintf(daemonDiagWriter(), "warn: lazy_proxy: probation watchdog registry read (will retry next tick): %v\n", loadErr)
		p.mu.Lock()
		p.reaping = false
		p.mu.Unlock()
		return
	}
	if !rowStarting {
		// Row genuinely advanced (or was unregistered) — no orphan to free;
		// disarm the orphan timer.
		p.mu.Lock()
		p.startingSince = time.Time{}
		p.reaping = false
		p.mu.Unlock()
		return
	}
	p.mu.Lock()
	p.startingSince = time.Time{}
	p.mu.Unlock()
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
// re-stamping a row that already advanced away from Starting. A registry LOAD
// error is returned distinctly (r4-F4): the caller must treat it as
// "unknown, retry later" — NOT as "row advanced" — or a transient read failure
// would permanently disarm the orphan reap. A missing row reads as (false, nil):
// genuinely nothing to reap (unregistered).
func (p *LazyProxy) registryRowIsStarting() (bool, error) {
	reg := api.NewRegistry(p.cfg.RegistryPath)
	if err := reg.Load(); err != nil {
		return false, err
	}
	e, ok := reg.Get(p.cfg.WorkspaceKey, p.cfg.Language)
	return ok && e.Lifecycle == api.LifecycleStarting, nil
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
// gen is the endpoint generation the failing request captured. onSendFailure
// no-ops on a generation mismatch (F4): a late failure surfacing from an OLD,
// already-torn-down endpoint must not evict + stamp Failed over a freshly
// republished newer-generation healthy endpoint. Symmetric with the gen guard in
// markWarmedOnFirstSuccess / reconcileRegistryLifecycleLocked — the whole refactor
// gen-guards every lifecycle mutation.
func (p *LazyProxy) onSendFailure(gen uint64, err error) {
	p.mu.Lock()
	if p.reaping || p.endpointGeneration != gen {
		p.mu.Unlock()
		return
	}
	p.reaping = true
	if p.endpoint != nil {
		_ = p.endpoint.Close()
		p.endpoint = nil
	}
	p.endpointGeneration++
	p.lastWrittenLifecycle = "" // Predicate 2: reset the shadow on the gen bump
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
	// Predicate 2: update ONLY LastToolsCallAt — do NOT write the Lifecycle column
	// here. reconcileRegistryLifecycleLocked is the single owner of the running
	// state; a tools/call timestamp refresh must not be a second Active-writer
	// (which could otherwise overwrite a concurrent Failed on a dying backend).
	// PutLastToolsCallAt preserves Lifecycle + LastError.
	_ = api.NewRegistry(p.cfg.RegistryPath).PutLastToolsCallAt(
		p.cfg.WorkspaceKey, p.cfg.Language, now.UTC(),
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
