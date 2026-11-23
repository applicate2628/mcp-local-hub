package mcproute

import (
	"net/http"
	"sync"
	"sync/atomic"
	"time"
)

// ReadHeaderTimeout is the header-read policy of the GUI and supervised route
// listeners. Listener acquisition has its own deadline.
const ReadHeaderTimeout = 10 * time.Second

// WorkspaceRoutes owns the Serena and LSP HTTP handlers and their in-process
// routing state. The caller binds the listener and supplies its effective port.
type WorkspaceRoutes struct {
	port func() int

	// Phase C.2 (v0.5.x serena routing) -- holds the resolver +
	// session-router bundle wired by SetSerenaRouterDeps. Atomic so a
	// future hot-reload of the workspace registry can swap the bundle
	// without restarting the gui-server.
	serenaRouterDeps atomic.Pointer[serenaRouterDeps]

	// ROUTER-COMPLETION phase -- process-local cache of the
	// workspace-agnostic tools/list result the router synthesizes by
	// proxying one tools/list to any live serena daemon (see
	// serena_router_lifecycle.go). The serena tool surface is identical
	// across workspace daemons, so a single cached entry serves every
	// client; the cache is keyed by nothing and TTL-bounded.
	serenaToolsListCache toolsListCache

	// ROUTER-COMPLETION phase -- router-owned map of client-facing
	// Mcp-Session-Id -> the real upstream daemon session it is
	// multiplexed onto (see serena_router_handshake.go). Because the
	// router synthesizes `initialize` itself and mints the client
	// session id, it must perform a SEPARATE MCP handshake with the
	// workspace daemon (which issues its own session id) before
	// forwarding tool calls. This store records that binding so
	// subsequent calls forward the daemon-issued id, not the
	// router-minted client id. Distinct from serenaRouterDeps.Sessions
	// (sticky client-session -> workspace routing) -- the daemon session
	// id is a new concern this store owns. Thread-safe.
	serenaDaemonSessions daemonSessionStore

	// ROUTER-COMPLETION phase (P2 findings 4 + 5 + 7) -- router-owned
	// registry of client sessions minted by a prior `initialize` at this
	// router, plus the protocol version each negotiated (see
	// serena_router_session.go). It is the authoritative "this client
	// session was initialized here" record AND the source of each
	// session's negotiated version, consumed by the tools/list session
	// gate (Finding 4), the version-keyed tools/list cache (Finding 5),
	// and the tool-call protocol-version enforcement (Finding 7).
	// Distinct from serenaRouterDeps.Sessions (sticky session -> workspace)
	// and serenaDaemonSessions (session -> upstream daemon session).
	// Thread-safe.
	serenaRouterSessions routerSessionStore

	// §3 fail-loud IPC reconcile fallback state. serenaBackendPIDMu guards
	// serenaBackendLastPID, the per-workspace-PATH snapshot of the supervisor
	// IPC status's CurrentPID from the PREVIOUS reconcile tick. A workspace
	// whose PID changed (restart) or that disappeared from the status between
	// ticks is a backend-loss signal, so reconcileSerenaBackendLossViaIPC
	// tears down that workspace's router sessions. Owned solely by the
	// reconcile goroutine + its tests; guarded so a future caller is safe.
	serenaBackendPIDMu   sync.Mutex
	serenaBackendLastPID map[string]int
	// serenaBackendIdlePaths tracks workspace paths that were intentionally
	// idle-stopped and the remaining post-clear grace ticks. The first running
	// PID after that phase is a wake respawn, not backend loss, so reconcile
	// refreshes the baseline once and then clears this marker. A cleared idle
	// stop followed by PID0/Stopped consumes the bounded grace before normal
	// backend-loss teardown resumes. Guarded by serenaBackendPIDMu.
	serenaBackendIdlePaths map[string]int
	// serenaBackendDropGen increments whenever baseline tracking is dropped for
	// a workspace path. Reconcile snapshots this alongside serenaBackendLastPID
	// and refuses to persist stale pre-drop state if the generation advanced
	// while its IPC status read was in flight. Entries intentionally live for
	// the Server lifetime; the map has the same workspace-path cardinality as
	// serenaBackendPathByKey (workspaces ever observed), so cleanup would add a
	// second lifecycle without meaningfully improving bounds.
	// Guarded by serenaBackendPIDMu.
	serenaBackendDropGen map[string]uint64
	// serenaBackendPathByKey maps router reverse-index WorkspaceKey values to
	// the WorkspacePath keys used by serenaBackendLastPID/IdlePaths. It lets
	// last-unbind deletion avoid blockable resolver scans on hot teardown paths.
	// Guarded by serenaBackendPIDMu.
	serenaBackendPathByKey map[string]string
	// serenaBackendLossTrigger is a coalesced wake signal for the IPC
	// backend-loss reconcile ticker. The event subscriber owns writes; the GUI
	// lifecycle ticker owns reads. Nil preserves the pure interval floor for
	// bare test Servers that bypass NewServer.
	serenaBackendLossTrigger chan struct{}

	// v0.6 idle-shutdown (#6, spec §6) per-daemon LAST-ACTIVITY tracking.
	// serenaActivityMu guards serenaLastActivity: WorkspaceKey -> the wall
	// time of the daemon's most recent /serena/mcp tool-call forward
	// (recordSerenaActivity, called from the router handler). The 60s idle
	// sweeper (serena_idle_sweeper.go) reads it to decide which RUNNING serena
	// pool daemons have been idle longer than the operator-configured
	// threshold. CRITICAL: this is LAST-ACTIVITY, not wall-clock-since-spawn —
	// a daemon mid-call or recently-active has a fresh timestamp and is never
	// idled (the falsification test). The map is the only owner; entries are
	// pruned when a daemon is idle-stopped so it does not accumulate stale keys.
	serenaActivityMu   sync.Mutex
	serenaLastActivity map[string]time.Time

	// startedAt is the wall time this GUI process started (set once in
	// NewServer). The idle sweeper's never-called fallback caps a daemon's
	// idle-duration at time-since-GUI-start so a GUI RESTART cannot
	// immediately idle-kill a daemon that was active just before the restart:
	// the restart wipes serenaLastActivity (in-memory), and a daemon whose
	// supervisor uptime already exceeds the threshold (e.g. 3h) would
	// otherwise be killed on the very first post-restart sweep even though it
	// serviced a call seconds before. Capping at time-since-GUI-start gives
	// every daemon a full fresh threshold window after a GUI restart (FIX-1,
	// fable's coupled-hazard insight). Read-only after construction.
	startedAt time.Time

	// serenaStopGate owns the per-workspace stop/forward gate. A path-bound
	// /serena/mcp request enters it immediately after workspace resolution and
	// before wake/daemon-session resolution; the idle sweeper starts the same
	// workspace gate while deciding to stop and releases later entrants only
	// after the stop write plus stale daemon-session invalidation finish. A
	// non-zero count means a request has started for that workspace, even if it
	// has not reached the upstream POST yet.
	serenaStopGate serenaWorkspaceStopGate

	// LSP router dependencies for /lsp/<language>/mcp. This route is
	// intentionally separate from the Serena router because LSP workspace
	// proxies are sessionless upstreams and need no daemon-session handshake.
	lspRouterDeps atomic.Pointer[lspRouterDeps]
}

// NewWorkspaceRoutes creates empty process-local routing state. startedAt is
// the process start used by the existing idle sweep's startup grace.
func NewWorkspaceRoutes(port func() int, startedAt time.Time) *WorkspaceRoutes {
	s := &WorkspaceRoutes{
		port:                     port,
		serenaBackendLossTrigger: make(chan struct{}, 1),
		startedAt:                startedAt,
	}
	s.serenaRouterSessions.onWorkspaceEmpty = s.handleSerenaRouterWorkspaceEmpty
	return s
}

// Register mounts the existing workspace handlers with their same-origin
// checks. The listener's outer handler applies its Host guard.
func (s *WorkspaceRoutes) Register(mux *http.ServeMux) {
	mux.HandleFunc("/serena/mcp", RequireSameOrigin(s.port, s.serenaRouterHandler))
	mux.HandleFunc("/lsp/", RequireSameOrigin(s.port, s.lspRouterHandler))
}

// RouteHandler exposes only the workspace routes on a Host-guarded listener.
func (s *WorkspaceRoutes) RouteHandler() http.Handler {
	mux := http.NewServeMux()
	s.Register(mux)
	return RequireAllowedHost(s.port, mux)
}
