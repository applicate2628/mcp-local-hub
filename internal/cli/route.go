// internal/cli/route.go
//
// `mcphub route` — Increment 1 of the MCP front-daemon decision
// (work-items/decisions/2026-07-25-mcp-data-plane-off-gui-onto-supervised-
// front-daemon.md). It serves /serena/mcp and /lsp/<language>/mcp on a
// SECONDARY port, independent of the GUI process, so serena+LSP MCP keep
// answering when the GUI dies. It is intended to run as a supervisor-managed
// child (a Job-Object daemon like every other MCP backend), always on.
//
// Non-negotiable constraints (see the decision record):
//   - Port P (the GUI's own port, default 9125), every client config, the
//     gui.pidport single-instance lock, and the RestartV3 coordinator/handoff
//     are completely untouched by this file.
//   - READ-ONLY on the registry + supervisor-intent, ENFORCED (not merely
//     omission-based — bot/architect review finding F1, 2026-07-25): this
//     command never wires api.SetSerenaAutoRegisterCutoverPrimitives (the
//     supervisor reap+install+start cutover stays exclusively GUI-owned —
//     see internal/cli/gui.go's runGUI) and starts no GUI-owned
//     idle/prune/event-subscriber loop. Its own in-memory session cleanup and
//     backend-loss reconcile tickers are process-local and do not touch
//     registry or intent. Omitting the
//     cutover primitives is NOT sufficient on its own: the router deps'
//     AutoRegisterFn (new-workspace registration) and WakeIdleFn (which can
//     clear an idle-stop on supervisor-intent) are both real registry/intent
//     WRITES reachable on the happy path regardless of the cutover wiring, so
//     this command constructs the router deps via gui.Server's dedicated
//     SetSerenaRouterReadOnly / SetLSPRouterReadOnly (AutoRegisterFn/
//     WakeIdleFn nil) instead of SetSerenaRouterProduction/
//     SetLSPRouterProduction, and sets gui.Config.ReadOnlyRouterMode so the
//     one remaining write call site independent of deps wiring
//     (maybePersistSerenaActivity's debounced registry LastToolsCallAt
//     write, internal/gui/serena_idle_sweeper.go) is also gated off. An
//     unregistered-workspace tool-call therefore fails loud with a 503
//     "register workspace first" — registration and activity-persistence
//     stay exclusively GUI-owned; this process only ever forwards to
//     ALREADY-registered workspaces.
//
// Implementation note (Adjacent finding, see the Increment-1 implementation
// report): the serena/LSP router HANDLER itself (internal/gui/serena_router.go
// + lsp_router.go) was not relocated into a GUI-independent package this
// increment — a verified, evidenced dependency/test-coupling conflict (not a
// judgment call) blocks a clean literal move within this phase's scope. This
// command therefore reuses the existing, UNCHANGED gui.Server construction and
// handler wiring directly via the new gui.Server.RouteHandler() adapter
// (internal/gui/route_adapter.go), which mounts ONLY /serena/mcp and
// /lsp/<language>/mcp — none of the GUI's other HTTP surface (dashboard,
// settings, secrets, migration, hub-aggregate, tray) is reachable through it.
// What this command deliberately does NOT do is call gui.Server.Start /
// ContinueWithGUIListener (which also binds the hub-aggregate listener, reads
// gui-preferences.yaml, and starts several GUI-only background goroutines) or
// composeGuiServerRestartV3 (RestartV3 + single-instance lock) — this process
// is a bare HTTP listener serving RouteHandler() and nothing more.
package cli

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"mcp-local-hub/internal/api"
	"mcp-local-hub/internal/api/lsp_routing"
	"mcp-local-hub/internal/api/serena_routing"
	"mcp-local-hub/internal/config"
	"mcp-local-hub/internal/gui"
)

// DefaultRouteDaemonPort is the fixed secondary port `mcphub route` binds by
// default. It is intentionally distinct from the GUI's default port
// (config.ReservedGUIPort, 9125) — Increment 1 is contract-neutral: no client
// config points at it, so any unused port works, but a fixed default (rather
// than always-ephemeral) lets an operator or the supervisor descriptor reach
// it at a predictable address.
//
// Port choice (bot/architect review finding F2, 2026-07-25): 9126 collided
// with godbolt (configs/ports.yaml). The repo's single-owner port-map
// convention (documented in internal/api/serena_dynamic_pool.go and
// internal/api/global_port_alloc.go) is:
//
//	9121–9149  hand-assigned globals (configs/ports.yaml; highest assigned
//	           today is vtune at 9136 — room to grow up to 9149)
//	9150–9199  serena dynamic pool (internal/api.EffectiveSerenaPortPool)
//	9200–9299  legacy LSP workspace-proxy rows
//	9300–9399  marketplace single-daemon globals
//	9400–9599  current LSP workspace-proxy pool
//
// 9137 sits in the "hand-assigned globals, room to grow" band, immediately
// above the highest port configs/ports.yaml currently assigns (9136/vtune),
// and well below the serena dynamic pool's 9150 start — it collides with
// neither. TestDefaultRouteDaemonPort_NotInPortsYAMLOrGUIOrSerenaPool
// (internal/cli/route_port_test.go) mechanically re-verifies this against
// the live configs/ports.yaml and the serena pool's effective range on every
// build, so a future edit to either cannot silently reintroduce a collision.
const DefaultRouteDaemonPort = 9137

// routeShutdownGrace bounds how long the HTTP server waits for in-flight
// requests to finish on a graceful shutdown (SIGINT/SIGTERM) before this
// process exits. Mirrors the GUI's own shutdown posture.
const routeShutdownGrace = 5 * time.Second

// routeBackendLossReconcileInterval is the route daemon's fallback cadence for
// observing a supervisor-reported Serena process restart or absence. The
// reconciler owns only this process's in-memory router, daemon-session, and
// sticky stores; it never writes registry or supervisor intent.
const routeBackendLossReconcileInterval = 30 * time.Second

var (
	routeDialSupervisorIPCStatus = api.DialSupervisorIPCStatus
	routeLifecycleAuditFn        = api.RouteReadOnlyStderrSink
)

// routeSerenaBackendStatus reads the supervisor's durable status for the
// route-owned backend-loss reconciler. A read failure is transient rather than
// a backend-loss proof, so the reconciler retains its stores; this wrapper only
// emits the route daemon's existing redacted structured diagnostic envelope.
func routeSerenaBackendStatus(ctx context.Context) ([]api.DaemonStatus, error) {
	rows, err := routeDialSupervisorIPCStatus(ctx)
	if err != nil {
		_ = routeLifecycleAuditFn("warn", "serena-supervisor-status-read-failed", map[string]any{
			"trigger": "route-backend-loss-reconcile",
		})
	}
	return rows, err
}

// startRouteSerenaBackendLossReconcile wires and starts the existing
// cancellable reconciler for the stores owned by this route Server. It does not
// add a second reconciliation engine or a detached poll: cancellation is the
// route daemon context, and ReconcileSerenaBackendLossViaIPC remains the sole
// owner of first-observation, idle-grace, PID-change, and absence semantics.
func startRouteSerenaBackendLossReconcile(ctx context.Context, s *gui.Server, interval time.Duration) {
	if s == nil {
		return
	}
	gui.SetSerenaBackendStatusFn(routeSerenaBackendStatus)
	go runSerenaBackendLossReconcileTicker(ctx, s, interval)
}

func newRouteCmd() *cobra.Command {
	var port int
	cmd := &cobra.Command{
		Use:   "route",
		Short: "Run the standalone serena+LSP MCP front daemon (supervisor-managed)",
		// Hidden: machine-invoked (spawned by `mcphub supervise` reconcile as
		// a Job-Object child), not an interactive operator command. Mirrors
		// newDaemonCmd()'s visibility rationale in root.go.
		Hidden: true,
		Long: `mcphub route serves /serena/mcp and /lsp/<language>/mcp on a
SECONDARY port, independent of the GUI process (port P, default 9125).

It is Increment 1 of the MCP-front-daemon decision record
(work-items/decisions/2026-07-25-mcp-data-plane-off-gui-onto-supervised-
front-daemon.md): a supervisor-managed, always-on process so serena+LSP MCP
keep answering when the GUI dies. It is READ-ONLY on the registry and
supervisor-intent — it never reaps, installs, or starts the supervisor, and
runs no GUI-owned idle/prune/event-subscriber loop. Its process-local session
expiry and backend-loss reconcile ticks do not write shared state. Client
configs and the GUI's own port are completely untouched.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			// Sub-increment 2a: an explicit --port flag always wins (this is
			// what the supervisor descriptor passes — ensureBuiltinRouteDaemonAtStartup
			// resolves mcp_front.port itself and bakes it into the spawned
			// argv, so the common production path never reaches the branch
			// below). Only a manual `mcphub route` invocation with no --port
			// falls through to the settings-driven resolution, so an operator
			// running the daemon by hand for testing/probing still gets the
			// same port the supervisor would have chosen.
			if !cmd.Flags().Changed("port") {
				port = resolveMCPFrontPortFn()
			}
			ctx, stop := signal.NotifyContext(cmd.Context(), syscall.SIGINT, syscall.SIGTERM)
			defer stop()
			return runRoute(ctx, cmd, port)
		},
	}
	cmd.Flags().IntVar(&port, "port", DefaultRouteDaemonPort,
		"port to bind on 127.0.0.1 (0 = OS-assigned ephemeral port); default sources the mcp_front.port setting when omitted")
	return cmd
}

// runRoute is the body shared between the cobra command and (later, if the
// supervisor descriptor wiring needs it) a programmatic entry point. It binds
// its OWN listener before constructing gui.Server so an ephemeral (--port 0)
// bind resolves to a concrete port BEFORE Config.Port is set — the DNS-rebind
// / same-origin guard (gui.Server.allowedHost/allowedOrigin, delegating to
// internal/mcproute) reads Config.Port when the Server's own listener-bound
// atomic is unset (which it always is here, since this command never calls
// gui.Server.Start/ContinueWithGUIListener), so the guard needs a concrete,
// already-resolved port at construction time.
// buildRouteServer constructs the gui.Server the route daemon serves,
// wired EXACTLY the way runRoute wires it (read-only serena+LSP routers, no
// cutover primitives, no GUI-owned background tickers). Extracted (sub-increment 2a)
// so a test can exercise the real, shipped construction path via
// s.RouteHandler() without needing a real TCP listener/goroutine — see
// route_i6_readonly_test.go's mutation-proven regression guard for the F1
// read-only invariant (registry + supervisor-intent untouched on an
// unregistered-workspace tool-call), which this increment's new
// --reconcile-mcp-front command makes reachable from a second, additional
// client-facing port.
// routeSessionStores are the session routers buildRouteServer created, handed
// back so the caller can drive their periodic expiry.
//
// WHY THEY ARE RETURNED (codex bot PR #588). These maps are OWNED by this
// process: `mcphub route` constructs its own serena and LSP SessionRouters, and
// the sweeps that expire them (runSessionCleanupTicker /
// runLSPSessionCleanupTicker) live in the GUI's lifecycle and drive the GUI's
// OWN routers. Nothing expired these, so a long-lived route daemon — which is
// exactly what this process is designed to be, supervisor-managed and always
// on — accumulated one binding per MCP session for its entire uptime and never
// released any. The file header's GUI-owned-loop restriction is about not
// performing shared-state work; expiring and reconciling this process's own
// in-memory maps are neither.
//
// lsp may be nil: the LSP router is only wired when the mcp-language-server
// manifest loads and parses.
type routeSessionStores struct {
	serena *serena_routing.SessionRouter
	lsp    *lsp_routing.SessionRouter
}

func buildRouteServer(cmd *cobra.Command, port int) (*gui.Server, *routeSessionStores, error) {
	s := gui.NewServer(gui.Config{
		Port:               port,
		Version:            versionString(),
		PID:                os.Getpid(),
		ReadOnlyRouterMode: true,
	})
	stores := &routeSessionStores{}

	registryPath, regErr := api.DefaultRegistryPath()
	if regErr != nil {
		return nil, nil, fmt.Errorf("resolve registry path: %w", regErr)
	}
	reg := api.NewRegistry(registryPath)
	// finding 1 (adversarial cross-family review round 3): every registry
	// Load() this process performs (this call and every later reload inside
	// serena_routing.WorkspaceResolver.refresh()) must never fall back to
	// the shared api.LogHubMcpEvent sink on a default-relax parent-DACL
	// read — that would write the GUI-owned hub-mcp.log from this
	// read-only daemon exactly as surely as a registry mutation would. Set
	// BEFORE the first Load() so no read on this *Registry ever reaches
	// the default sink.
	reg.SetAuditSink(api.RouteReadOnlyStderrSink)
	if err := reg.Load(); err != nil {
		fmt.Fprintf(cmd.ErrOrStderr(),
			"route: registry load warning (will retry lazily on first call): %v\n", err)
	}
	// P2-3 fix (adversarial cross-family review): the read-only constructor
	// (never takes Registry.Lock(), the cross-process exclusive flock the
	// GUI's own writers need) — see
	// serena_routing.NewReadOnlyWorkspaceResolver's doc comment.
	resolver := serena_routing.NewReadOnlyWorkspaceResolver(reg, registryPath)
	sessions := serena_routing.NewSessionRouter()
	// Read-only wiring (F1): nils AutoRegisterFn + WakeIdleFn — see
	// gui.Server.SetSerenaRouterReadOnly's doc comment and the file header.
	s.SetSerenaRouterReadOnly(resolver, sessions)
	stores.serena = sessions

	if rawManifest, err := api.NewAPI().ManifestGet("mcp-language-server"); err != nil {
		fmt.Fprintf(cmd.ErrOrStderr(),
			"route: lsp manifest load failed; /lsp/<language>/mcp will return errors until next restart: %v\n", err)
	} else if m, err := config.ParseManifest(strings.NewReader(rawManifest)); err != nil {
		fmt.Fprintf(cmd.ErrOrStderr(),
			"route: lsp manifest parse failed; /lsp/<language>/mcp will return errors until next restart: %v\n", err)
	} else {
		// Separate registry handle mirrors internal/cli/gui.go's GUI wiring:
		// the serena resolver and the LSP resolver each refresh independently
		// under their own RWMutex, so sharing one *api.Registry object would
		// race the two resolvers' caches under concurrent /serena/mcp +
		// /lsp/<lang>/mcp traffic (the data race gui.go's comment documents at
		// its own registry-construction site).
		lspReg := api.NewRegistry(registryPath)
		// finding 1: same reasoning as the serena registry's SetAuditSink
		// call above — this resolver's refresh() also calls Load() on this
		// SAME *Registry.
		lspReg.SetAuditSink(api.RouteReadOnlyStderrSink)
		// P2-3 fix: the read-only constructor — see
		// serena_routing.NewReadOnlyWorkspaceResolver's doc comment (the LSP
		// resolver's NewReadOnlyWorkspaceResolver mirrors the same argument).
		lspResolver := lsp_routing.NewReadOnlyWorkspaceResolver(lspReg, registryPath, m.Languages)
		lspSessions := lsp_routing.NewSessionRouter()
		// Read-only wiring (F1): nils AutoRegisterFn — see
		// gui.Server.SetLSPRouterReadOnly's doc comment and the file header.
		s.SetLSPRouterReadOnly(lspResolver, lspSessions, m.Languages)
		stores.lsp = lspSessions
	}
	return s, stores, nil
}

// runRouteSessionExpiry starts the periodic session sweeps for the routers
// THIS process owns, reusing the GUI's own ticker drivers.
//
// Both goroutines stop with ctx, which runRoute cancels on SIGINT/SIGTERM, so
// they cannot outlive the daemon. The serena sweep is passed the gui.Server so
// it also reclaims the server's two router-owned session stores — those are
// in-memory maps belonging to THIS process's handler, and SweepSerenaSessions
// touches nothing else (no registry, no supervisor-intent, no shared log), so
// it is compatible with this daemon's read-only posture.
//
// interval and ttl are parameters rather than the constants read inline so a
// test can drive a real sweep to completion instead of waiting an hour. ONE
// ttl covers both routers because they age on one shared idle clock
// (serena_routing.DefaultSessionTTL == lsp_routing.DefaultSessionTTL ==
// daemonSessionTTL == 24h) — the same single-ttl shape runSessionCleanupTicker
// already uses for the sticky router and the server-owned stores.
func runRouteSessionExpiry(ctx context.Context, s *gui.Server, stores *routeSessionStores, interval, ttl time.Duration) {
	if stores == nil {
		return
	}
	if stores.serena != nil {
		go runSessionCleanupTicker(ctx, s, stores.serena, interval, ttl)
	}
	if stores.lsp != nil {
		go runLSPSessionCleanupTicker(ctx, stores.lsp, interval, ttl)
	}
}

func runRoute(ctx context.Context, cmd *cobra.Command, port int) error {
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		return fmt.Errorf("bind route daemon listener on port %d: %w", port, err)
	}
	actualPort := ln.Addr().(*net.TCPAddr).Port

	s, sessionStores, err := buildRouteServer(cmd, actualPort)
	if err != nil {
		_ = ln.Close()
		return err
	}

	// Expire this process's OWN session maps. Without it a long-lived,
	// always-on route daemon grows one binding per MCP session forever — the
	// GUI-owned sweeps drive the GUI's routers, not these (codex bot PR #588).
	runRouteSessionExpiry(ctx, s, sessionStores, sessionCleanupInterval, serena_routing.DefaultSessionTTL)
	// Reconcile only this route server's in-memory session stores against the
	// supervisor's durable daemon generation. The reconciler is the existing GUI
	// lifecycle owner; it has no registry/intent writes and exits with ctx.
	startRouteSerenaBackendLossReconcile(ctx, s, routeBackendLossReconcileInterval)

	// Deliberately absent (Increment 1 constraint — see file header):
	//   - api.SetSerenaAutoRegisterCutoverPrimitives: the supervisor
	//     reap+install+start cutover stays exclusively GUI-owned. A serena
	//     workspace that needs first-time auto-register while no supervisor
	//     is running fails loud (503) from THIS process instead of silently
	//     reaping/installing/starting the supervisor from a second daemon.
	//   - GUI-owned background work (backend-loss event subscriber,
	//     idle-shutdown sweep, workspace auto-prune sweep): none is started here.
	//     The two route-owned in-memory maintenance tickers above are deliberately
	//     separate: session expiry and the existing IPC reconciler for this
	//     server's own stores.
	//   - gui.Server.Start / ContinueWithGUIListener / composeGuiServerRestartV3:
	//     this process serves RouteHandler() on its own bare listener instead.
	//   - a NON-nil AutoRegisterFn / WakeIdleFn on either router's deps, and
	//     registry writes from maybePersistSerenaActivity: see F1 above —
	//     SetSerenaRouterReadOnly/SetLSPRouterReadOnly + ReadOnlyRouterMode
	//     jointly enforce this rather than relying on cutover-primitive
	//     omission alone.

	httpSrv := &http.Server{
		Handler:           s.RouteHandler(),
		ReadHeaderTimeout: gui.GUIReadHeaderTimeout,
	}
	errCh := make(chan error, 1)
	go func() { errCh <- httpSrv.Serve(ln) }()

	fmt.Fprintf(cmd.OutOrStdout(),
		"mcphub route: serving /serena/mcp + /lsp/<language>/mcp on 127.0.0.1:%d (pid %d)\n",
		actualPort, os.Getpid())

	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), routeShutdownGrace)
		defer cancel()
		if err := httpSrv.Shutdown(shutdownCtx); err != nil {
			return fmt.Errorf("route daemon shutdown: %w", err)
		}
		return nil
	case err := <-errCh:
		if err != nil && err != http.ErrServerClosed {
			return err
		}
		return nil
	}
}
