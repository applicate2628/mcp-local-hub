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
//     see internal/cli/gui.go's runGUI) and starts no idle/prune/reconcile
//     ticker (session cleanup, backend-loss reconcile, idle-shutdown,
//     workspace-prune all stay GUI-owned background loops). Omitting the
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
// default. It is intentionally distinct from the GUI's default port 9125 —
// Increment 1 is contract-neutral: no client config points at it, so any
// unused port works, but a fixed default (rather than always-ephemeral) lets
// an operator or the supervisor descriptor reach it at a predictable address.
const DefaultRouteDaemonPort = 9126

// routeShutdownGrace bounds how long the HTTP server waits for in-flight
// requests to finish on a graceful shutdown (SIGINT/SIGTERM) before this
// process exits. Mirrors the GUI's own shutdown posture.
const routeShutdownGrace = 5 * time.Second

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
runs no idle/prune/reconcile ticker. Client configs and the GUI's own port
are completely untouched.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, stop := signal.NotifyContext(cmd.Context(), syscall.SIGINT, syscall.SIGTERM)
			defer stop()
			return runRoute(ctx, cmd, port)
		},
	}
	cmd.Flags().IntVar(&port, "port", DefaultRouteDaemonPort,
		"port to bind on 127.0.0.1 (0 = OS-assigned ephemeral port)")
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
func runRoute(ctx context.Context, cmd *cobra.Command, port int) error {
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		return fmt.Errorf("bind route daemon listener on port %d: %w", port, err)
	}
	actualPort := ln.Addr().(*net.TCPAddr).Port

	s := gui.NewServer(gui.Config{
		Port:               actualPort,
		Version:            versionString(),
		PID:                os.Getpid(),
		ReadOnlyRouterMode: true,
	})

	registryPath, regErr := api.DefaultRegistryPath()
	if regErr != nil {
		_ = ln.Close()
		return fmt.Errorf("resolve registry path: %w", regErr)
	}
	reg := api.NewRegistry(registryPath)
	if err := reg.Load(); err != nil {
		fmt.Fprintf(cmd.ErrOrStderr(),
			"route: registry load warning (will retry lazily on first call): %v\n", err)
	}
	resolver := serena_routing.NewWorkspaceResolver(reg, registryPath)
	sessions := serena_routing.NewSessionRouter()
	// Read-only wiring (F1): nils AutoRegisterFn + WakeIdleFn — see
	// gui.Server.SetSerenaRouterReadOnly's doc comment and the file header.
	s.SetSerenaRouterReadOnly(resolver, sessions)

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
		lspResolver := lsp_routing.NewWorkspaceResolver(lspReg, registryPath, m.Languages)
		lspSessions := lsp_routing.NewSessionRouter()
		// Read-only wiring (F1): nils AutoRegisterFn — see
		// gui.Server.SetLSPRouterReadOnly's doc comment and the file header.
		s.SetLSPRouterReadOnly(lspResolver, lspSessions, m.Languages)
	}

	// Deliberately absent (Increment 1 constraint — see file header):
	//   - api.SetSerenaAutoRegisterCutoverPrimitives: the supervisor
	//     reap+install+start cutover stays exclusively GUI-owned. A serena
	//     workspace that needs first-time auto-register while no supervisor
	//     is running fails loud (503) from THIS process instead of silently
	//     reaping/installing/starting the supervisor from a second daemon.
	//   - every GUI background ticker (session cleanup, backend-loss IPC
	//     reconcile, backend-loss event subscriber, idle-shutdown sweep,
	//     workspace auto-prune sweep): none are started here.
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
