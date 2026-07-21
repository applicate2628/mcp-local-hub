// internal/cli/gui.go
package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"mcp-local-hub/internal/api"
	"mcp-local-hub/internal/api/lsp_routing"
	"mcp-local-hub/internal/api/serena_routing"
	"mcp-local-hub/internal/config"
	"mcp-local-hub/internal/gui"
	"mcp-local-hub/internal/process"
	"mcp-local-hub/internal/tray"

	"github.com/spf13/cobra"
	"golang.org/x/term"
)

// selfRestartHandoffEnv mirrors gui.SelfRestartHandoffEnv. RestartV3 carries a
// structured handoff descriptor in this variable and dispatches the replacement
// directly into the reservation-aware standby child path. The literal "1" is
// retained only as a cross-version upgrade bridge: an already-running v1 binary
// can spawn a rename-aside-deployed v3 binary, whose normal startup path still
// recognizes the old signal. The gui package owns the canonical name and a unit
// test pins the two package constants equal.
const selfRestartHandoffEnv = "MCPHUB_GUI_SELF_RESTART_HANDOFF"

// selfRestartHandoffAcquireDeadline / selfRestartHandoffAcquireBackoff
// bound only the legacy literal-"1" upgrade-bridge acquire poll. RestartV3's
// structured child uses its reservation protocol instead. After this bridge
// deadline the child falls through to the normal busy/handshake path so a
// genuinely stuck incumbent is still diagnosed.
var (
	selfRestartHandoffAcquireDeadline = 10 * time.Second
	selfRestartHandoffAcquireBackoff  = 100 * time.Millisecond
)

func isRestartV3ChildLaunch(restartV3Enabled bool, handoff string) bool {
	return restartV3Enabled && handoff != "" && handoff != "1"
}

func consumeSelfRestartHandoff(handoff string) string {
	_ = os.Unsetenv(selfRestartHandoffEnv)
	return handoff
}

type restartV3ChildRuntimeStarter func(
	context.Context,
	*gui.Server,
	*gui.GUIListenerOwner,
	net.Listener,
	gui.SingleInstanceLease,
) error

type restartV3ChildStartupConfig struct {
	Handoff      string
	PID          int
	PidportPath  string
	Port         int
	Version      string
	Deadlines    gui.RestartDeadlines
	StartRuntime restartV3ChildRuntimeStarter
}

type releaseOnceLease struct {
	lease gui.SingleInstanceLease
	once  sync.Once
}

func (l *releaseOnceLease) Release() {
	if l == nil {
		return
	}
	l.once.Do(func() {
		if l.lease != nil {
			l.lease.Release()
		}
	})
}

type restartV3ParentRuntime struct {
	SettingsGet func(string) (string, error)
	Spawn       func([]string, gui.SelfRestartHandoff) (gui.RestartParentChild, error)
	Confirm     func(context.Context, int, []byte, gui.AuthenticatedReadinessIdentity) error
	Exit        func()
}

func defaultRestartV3ParentRuntime(exitRequested *atomic.Bool) restartV3ParentRuntime {
	return restartV3ParentRuntime{
		SettingsGet: func(key string) (string, error) { return api.NewAPI().SettingsGet(key) },
		Spawn:       gui.SpawnRestartV3GUI,
		Confirm:     gui.ConfirmAuthenticatedStandby,
		Exit:        selfRestartProcessExitBoundary(exitRequested, gui.RequestSelfRestartExit),
	}
}

func selfRestartProcessExitBoundary(requested *atomic.Bool, exit func()) func() {
	return func() {
		if requested != nil {
			requested.Store(true)
		}
		if exit != nil {
			exit()
		}
	}
}

type guiSupervisorManagerStopper interface {
	Stop(context.Context, int) error
}

func stopSupervisorManagerUnlessSelfRestart(ctx context.Context, manager guiSupervisorManagerStopper, exitRequested *atomic.Bool) error {
	if manager == nil || (exitRequested != nil && exitRequested.Load()) {
		return nil
	}
	return manager.Stop(ctx, 5000)
}

// buildRestartV3ParentDependencies composes CLI-owned facts without moving
// argv parsing or lease ownership into the gui package. TargetPort captures
// one typed persisted-port classification; Spawn consumes that same value so
// a settings race cannot produce argv/marker disagreement.
func buildRestartV3ParentDependencies(
	ctx context.Context,
	cmd *cobra.Command,
	lease gui.SingleInstanceLease,
	pidportPath string,
	oldPort func() int,
	argv []string,
	runtime restartV3ParentRuntime,
) (gui.RestartCoordinatorDependencies, error) {
	if ctx == nil || cmd == nil || lease == nil || strings.TrimSpace(pidportPath) == "" || oldPort == nil {
		return gui.RestartCoordinatorDependencies{}, errors.New("restart v3 parent composition is incomplete")
	}
	if runtime.SettingsGet == nil || runtime.Spawn == nil || runtime.Confirm == nil || runtime.Exit == nil {
		return gui.RestartCoordinatorDependencies{}, errors.New("restart v3 parent runtime seams are incomplete")
	}
	var intentMu sync.Mutex
	var intent guiPortIntent
	intentResolved := false
	argvCopy := append([]string(nil), argv...)
	targetPort := func(actualPort int) (int, error) {
		if !validPersistedGUIPort(actualPort) {
			return 0, fmt.Errorf("restart v3 actual GUI port %d is outside the supported loopback range [1024,65535]", actualPort)
		}
		raw, err := runtime.SettingsGet("gui_server.port")
		if err != nil {
			return 0, fmt.Errorf("read desired GUI port for restart: %w", err)
		}
		resolved := classifyPersistedGUIPort(raw)
		if resolved.Kind == guiPortIntentInvalid {
			fallback := guiPortFallbackEphemeral
			if cmd.Flags().Changed("port") {
				fallback = guiPortFallbackExplicitFlag
			}
			// DEFERRED-CLOSURE site: this closure is DEFINED here, ABOVE the
			// sink-install call in startGuiServerWithStartup — but source order
			// is NOT invocation order. targetPort is never called inline; it runs only
			// when RestartCoordinator.Start invokes it
			// (internal/gui/gui_restart_protocol.go), which fires on
			// /api/gui/restart — long AFTER startGuiServerWithStartup installed
			// the sinks. At INVOCATION time cobra's getOut() resolves
			// OutOrStderr() to the guiRuntimeStdout sink, so OutOrStderr() would
			// silently land this warning on stdout — exactly the regression this
			// change fixes. ErrOrStderr() is therefore required. Unlike the
			// genuinely pre-sink inline site in newGuiCmdReal's RunE, there is no
			// embedding-caller concern here: by invocation time the sink owns
			// BOTH streams, so ErrOrStderr() reaches the durable stderr sink, not
			// a caller's writer.
			emitInvalidGUIPortWarning(cmd.ErrOrStderr(), resolved, fallback)
		}
		intentMu.Lock()
		intent = resolved
		intentResolved = true
		intentMu.Unlock()
		if resolved.Kind == guiPortIntentValid {
			return resolved.Port, nil
		}
		// The structured handoff pins the already-proved actual port for
		// unset/invalid intent; the child consumes that target directly.
		return actualPort, nil
	}
	spawn := func(handoff gui.SelfRestartHandoff) (gui.RestartParentChild, error) {
		intentMu.Lock()
		resolved, ok := intent, intentResolved
		intentMu.Unlock()
		if !ok {
			return nil, errors.New("restart v3 target port was not resolved before spawn")
		}
		rebuilt, err := RebuildSelfRestartArgv(argvCopy, cmd.Flags(), resolved)
		if err != nil {
			return nil, err
		}
		return runtime.Spawn(rebuilt, handoff)
	}
	deadlines := gui.DefaultRestartDeadlines()
	return gui.RestartCoordinatorDependencies{
		Context: ctx, StateDir: filepath.Dir(pidportPath), OldPort: oldPort, TargetPort: targetPort,
		ParentPID: os.Getpid(), Lease: lease, MarkerStore: gui.NewHandoffMarkerStore(filepath.Dir(pidportPath), deadlines),
		Deadlines: deadlines, Spawn: spawn, Confirm: runtime.Confirm, Exit: runtime.Exit,
	}, nil
}

// runRestartV3ChildStartup composes the gated child half. Before the flock it
// only consumes the one-shot nonce file and binds the challenged standby
// listener. The injected runtime owns every mutable GUI side effect and is not
// invoked until SpawnedGUIChild has acquired and revalidated the reservation.
func runRestartV3ChildStartup(ctx context.Context, cfg restartV3ChildStartupConfig) error {
	if ctx == nil {
		return errors.New("restart child context is nil")
	}
	if cfg.PidportPath == "" || cfg.Port <= 0 || cfg.StartRuntime == nil {
		return errors.New("restart child startup configuration is incomplete")
	}

	stateDir := filepath.Dir(cfg.PidportPath)
	child, err := gui.NewSpawnedGUIChildFromEnvironment(cfg.Handoff, cfg.PID, stateDir)
	if err != nil {
		return err
	}
	defer child.Close()
	if child.Handoff.TargetPort != cfg.Port {
		return fmt.Errorf("restart child target port %d does not match resolved GUI port %d", child.Handoff.TargetPort, cfg.Port)
	}

	server := gui.NewServer(gui.Config{
		Port:             cfg.Port,
		Version:          cfg.Version,
		PID:              cfg.PID,
		RestartV3Enabled: true,
	})
	defer server.Broadcaster().Close()
	owner := server.GUIListenerOwner()
	bindBudget := cfg.Deadlines.Bind
	if child.Handoff.OldPort == child.Handoff.TargetPort {
		bindBudget += cfg.Deadlines.Quiesce
	}
	bound, err := bindRestartV3ChildStandby(ctx, bindBudget, func(bindCtx context.Context) (net.Listener, error) {
		return owner.BindStandby(bindCtx, cfg.Port, child.Readiness.Handler())
	})
	if err != nil {
		return err
	}

	store := gui.NewHandoffMarkerStore(stateDir, cfg.Deadlines)
	deps := gui.DefaultRestartChildDependencies(cfg.Deadlines)
	deps.MarkerStore = store
	deps.Standby = owner
	deps.Events = server.Broadcaster()
	runtime := gui.NewRestartChildRuntimeSettlement()
	deps.Runtime = runtime
	deps.Acquire = func(context.Context) (gui.SingleInstanceLease, error) {
		lease, err := child.AcquireSingleInstanceAt(cfg.PidportPath, cfg.Port, store, cfg.Deadlines)
		if err != nil {
			return nil, err
		}
		return &releaseOnceLease{lease: lease}, nil
	}

	deps.Activate = func(activateCtx context.Context, lease gui.SingleInstanceLease) error {
		go func() {
			runtime.Stop(cfg.StartRuntime(activateCtx, server, owner, bound, lease))
		}()
		go func() {
			select {
			case <-activateCtx.Done():
				runtime.Stop(activateCtx.Err())
			case <-runtime.Done():
			}
		}()
		select {
		case <-owner.Activated():
			return nil
		case <-runtime.Done():
			runtimeErr := runtime.Err()
			if runtimeErr == nil {
				runtimeErr = errors.New("restart child runtime stopped before listener activation")
			}
			return runtimeErr
		case <-activateCtx.Done():
			return activateCtx.Err()
		}
	}

	result, err := child.Run(ctx, deps)
	if err != nil {
		return err
	}
	if !result.Activated {
		return errors.New("restart child completed without an activated runtime")
	}

	<-runtime.Done()
	runtimeErr := runtime.Err()
	if errors.Is(runtimeErr, context.Canceled) && errors.Is(ctx.Err(), context.Canceled) {
		return nil
	}
	return runtimeErr
}

func bindRestartV3ChildStandby(
	ctx context.Context,
	bindBudget time.Duration,
	bind func(context.Context) (net.Listener, error),
) (net.Listener, error) {
	if bindBudget <= 0 || bind == nil {
		return nil, errors.New("restart child standby bind budget and binder are required")
	}
	bindCtx, cancel := context.WithTimeout(ctx, bindBudget)
	defer cancel()
	var lastErr error
	for {
		listener, err := bind(bindCtx)
		if err == nil {
			return listener, nil
		}
		lastErr = err
		if !api.IsPortBindRefusedErr(err) && !errors.Is(err, syscall.EADDRINUSE) {
			return nil, err
		}
		timer := time.NewTimer(25 * time.Millisecond)
		select {
		case <-bindCtx.Done():
			timer.Stop()
			return nil, errors.Join(lastErr, bindCtx.Err())
		case <-timer.C:
		}
	}
}

// acquireSingleInstanceWithHandoff owns the normal GUI single-instance acquire.
// When RestartV3 is enabled, every ordinary entrant uses the reservation-aware
// path so it cannot take the flock during a designated child's reservation.
// The structured child supplies its nonce through runRestartV3ChildStartup. A
// handoff value of exactly "1" enables a bounded retry only for the cross-version
// upgrade bridge where an already-running v1 parent launches a rename-aside-
// deployed v3 binary.
//
// Only ErrSingleInstanceBusy is retried; any other acquire error (write
// failure, etc.) returns immediately. On deadline expiry the last busy
// error is returned so the caller's existing handshake/--force flow runs
// exactly as before for a genuinely-occupied lock.
func acquireSingleInstanceWithHandoff(ctx context.Context, pidportPath string, port int) (*gui.SingleInstanceLock, error) {
	restartV3Enabled := gui.RestartV3Enabled()
	var options gui.SingleInstanceAcquireOptions
	if restartV3Enabled {
		// The reservation-aware acquire path validates the pidport as ABSOLUTE
		// (validateGUIOwnerLeasePath); PidportPath can return a relative path
		// under a relative XDG_STATE_HOME/LOCALAPPDATA — the legacy no-options
		// path tolerated it, the options path rejects it, which would block an
		// ordinary gate-on startup. Normalize to absolute first (bot #568 P2);
		// the resolved file is identical to the legacy CWD-relative one.
		if abs, err := filepath.Abs(pidportPath); err == nil {
			pidportPath = abs
		}
		deadlines := gui.DefaultRestartDeadlines()
		options = gui.SingleInstanceAcquireOptions{
			RestartV3Enabled: true,
			MarkerStore:      gui.NewHandoffMarkerStore(filepath.Dir(pidportPath), deadlines),
			Deadlines:        deadlines,
		}
	}
	acquire := func() (*gui.SingleInstanceLock, error) {
		if !restartV3Enabled {
			return gui.AcquireSingleInstanceAt(pidportPath, port)
		}
		return gui.AcquireSingleInstanceAt(pidportPath, port, options)
	}

	lock, err := acquire()
	if err == nil || !errors.Is(err, gui.ErrSingleInstanceBusy) {
		return lock, err
	}
	if os.Getenv(selfRestartHandoffEnv) != "1" {
		return lock, err
	}
	deadline := time.Now().Add(selfRestartHandoffAcquireDeadline)
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return nil, err
		case <-time.After(selfRestartHandoffAcquireBackoff):
		}
		lock, err = acquire()
		if err == nil || !errors.Is(err, gui.ErrSingleInstanceBusy) {
			return lock, err
		}
	}
	return lock, err
}

// inputIsTerminal reports whether r is a terminal-backed *os.File. The
// non-TTY guard for --force --kill must check the SAME stream the
// confirmation prompt reads from (cmd.InOrStdin) so test / embedded
// callers that override input via cmd.SetIn(...) get consistent
// behavior. term.IsTerminal needs an int fd, so non-*os.File readers
// (bytes.Buffer, strings.Reader) return false unconditionally —
// matching the documented "scripted input ⇒ non-interactive" intent.
// Codex bot review on PR #23 P2 (round 3).
func inputIsTerminal(r io.Reader) bool {
	f, ok := r.(*os.File)
	if !ok {
		return false
	}
	return term.IsTerminal(int(f.Fd()))
}

func newGuiCmdReal() *cobra.Command {
	var (
		port       int
		noBrowser  bool
		noTray     bool
		force      bool
		kill       bool
		yes        bool
		resetPort  bool
		strictMode bool
		reveal     bool
		foreground bool
	)
	c := &cobra.Command{
		Use:   "gui",
		Short: "Launch the local GUI (browser window + tray icon served by mcphub itself)",
		Long: `mcphub gui starts an HTTP server on 127.0.0.1 that serves a local-only
browser GUI for managing MCP servers. A Windows tray icon and auto-launched
Chrome/Edge app-mode window accompany it by default.

The server binds 127.0.0.1 only — no remote access, no auth, no TLS.
A Windows named mutex guards against a second instance: a second invocation
activates the first window and exits 0.`,
		PreRunE: func(cmd *cobra.Command, args []string) error {
			// --kill and --yes are stuck-instance-recovery modifiers
			// for --force; both are silently inert without it. Reject
			// the misuse with a usage error so cobra prints help and
			// exits 1, instead of silently dropping the destructive
			// intent. (Exit-code map for stuck-instance recovery is
			// reserved for runtime outcomes — operator misuse uses
			// cobra's default 1.)
			if kill && !force {
				return fmt.Errorf("--kill requires --force; pass `--force --kill` for stuck-instance kill recovery")
			}
			// `--yes` is the confirmation bypass for `--force --kill`,
			// not for bare `--force`. Reject `--force --yes` (without
			// --kill) too — otherwise `mcphub gui --force --yes` runs
			// the bare-diagnostic path silently and a typo-skipped
			// `--kill` looks like a handled force flow in automation.
			// `kill && !force` is enforced above, so `yes && !kill`
			// also covers the lone `--yes` case (no --force, no --kill).
			// codex bot phase5 task 5.3 closure: --yes is also a valid
			// modifier for --reset-port (confirms credential-rotation
			// warning acceptance in non-TTY contexts). The check below
			// rejects --yes only when neither --kill nor --reset-port
			// is set.
			if yes && !kill && !resetPort {
				return fmt.Errorf("--yes requires --force --kill OR --reset-port; pass `--force --kill --yes` or `--reset-port --yes` to confirm non-interactive operation")
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, stop := signal.NotifyContext(cmd.Context(), syscall.SIGINT, syscall.SIGTERM)
			defer stop()

			// Phase 5 Task 5.3: --reset-port is a state-dir operation
			// (clears the persisted Port in hub-mcp.endpoint.json)
			// that intentionally does NOT start a server. The
			// credential-rotation warning is load-bearing — a previous
			// bind to the persisted port could have leaked tokens to a
			// pre-binding process per spec §"Pre-bind handling".
			//
			// codex bot phase5 r7 P2 closure on PR #160: gate the
			// reset on hub-not-running. If the GUI is currently
			// running, its hub-mcp listener is still bound on the
			// old port while we clear the persisted port to 0 —
			// downstream commands (regenerate-token, install
			// --reconcile-hub-mode) key off endpoint.Port and would
			// treat the hub as "not running", silently dropping the
			// reload call to the live listener. Use the same
			// single-instance flock the GUI server takes to detect
			// liveness: if it can be acquired, no GUI is running;
			// if it returns ErrSingleInstanceBusy, refuse the reset
			// with operator guidance to stop the hub first. The
			// lock is released immediately so this command doesn't
			// hold pidport across the credential-rotation messages.
			if resetPort {
				if !yes && !inputIsTerminal(cmd.InOrStdin()) {
					fmt.Fprintln(cmd.ErrOrStderr(), "non-TTY input requires --yes to confirm --reset-port (credential-rotation guidance below requires explicit acknowledgement)")
					return &forceExitError{code: 6}
				}
				pidportPath, ppErr := gui.PidportPath()
				if ppErr != nil {
					return fmt.Errorf("resolve pidport path: %w", ppErr)
				}
				if d := os.Getenv("MCPHUB_GUI_TEST_PIDPORT_DIR"); d != "" {
					pidportPath = filepath.Join(d, gui.PidportFileLeaf)
				}
				lock, lockErr := gui.AcquireSingleInstanceAt(pidportPath, 0)
				if lockErr != nil {
					if errors.Is(lockErr, gui.ErrSingleInstanceBusy) {
						fmt.Fprintln(cmd.ErrOrStderr(),
							"--reset-port refused: another `mcphub gui` is running and its hub-mcp listener still holds the old port. "+
								"Stop the GUI first (close the tray/window, or `mcphub gui --force --kill --yes`), then rerun `mcphub gui --reset-port`.")
						return &forceExitError{code: 3}
					}
					return fmt.Errorf("acquire single-instance lock: %w", lockErr)
				}
				defer lock.Release()
				// The hub port is baked into gate-ON client and group URLs. Refuse
				// unless every source is proved clear: an unreadable client or
				// groups.yaml may contain a live URL that would be orphaned by a
				// reset. The single-instance lock above already proved the GUI is
				// not running, so this read sees the at-rest config.
				deps := api.ProbeHubPortDependencies()
				if deps.State != api.DependencyStateClear {
					var message strings.Builder
					message.WriteString("--reset-port refused:\n")
					if len(deps.GatedClients) > 0 || len(deps.Groups) > 0 {
						message.WriteString("Proved dependencies pinned to the current hub port:\n")
						if len(deps.GatedClients) > 0 {
							fmt.Fprintf(&message,
								"- %d client(s) are gate-ON (hub-aggregate mode) and their /clients/<client>/mcp URLs are pinned: %s.\n"+
									"  Resetting the port would orphan every gated client URL (the next hub bind grabs a NEW ephemeral port → connection refused for ALL aggregated servers).\n",
								len(deps.GatedClients), strings.Join(deps.GatedClients, ", "))
						}
						if len(deps.Groups) > 0 {
							fmt.Fprintf(&message,
								"- %d group(s) have /g/<group>/mcp URLs pinned: %s.\n"+
									"  Resetting the port would orphan those URLs; no reconcile path rewrites group URLs, so re-copy them from the Groups screen after a port change.\n",
								len(deps.Groups), strings.Join(deps.Groups, ", "))
						}
					}
					if len(deps.Errors) > 0 {
						message.WriteString("Unreadable sources; reset safety cannot be proven:\n")
						for _, source := range deps.Errors {
							fmt.Fprintf(&message, "- %s %s (%s)\n", source.Kind, source.Name, source.Err)
						}
						message.WriteString("Fix the unreadable file (repair its DACL or parse error), then retry.\n")
					}
					message.WriteString("Gate OFF first, THEN retry --reset-port. Headless: `mcphub settings set gui_server.hub_endpoint_enabled false` then `mcphub install --reconcile-hub-mode` (removes the on-disk mcphub-hub entries) — or in the GUI: Settings → uncheck \"Expose a single aggregated hub URL\" + restart.\n")
					message.WriteString("(If the GUI itself is stuck, `mcphub gui --force --kill` is the separate recovery and is NOT blocked by this guard.)\n")
					fmt.Fprint(cmd.ErrOrStderr(), message.String())
					return &forceExitError{code: 8}
				}
				if err := api.ResetHubPort(); err != nil {
					return fmt.Errorf("reset hub port: %w", err)
				}
				fmt.Fprintln(cmd.OutOrStdout(),
					"Reset hub-mcp port. instance_id preserved (use `mcphub hub-mcp regenerate-instance-id` for full rotation).")
				fmt.Fprintln(cmd.OutOrStdout(),
					"WARNING: credentials may have leaked to a pre-binding process before --reset-port. "+
						"Run `mcphub hub-mcp regenerate-token --client <each>` AND "+
						"`mcphub hub-mcp regenerate-instance-id` before reinstalling.")
				fmt.Fprintln(cmd.OutOrStdout(), "Then: `mcphub install` for each affected client.")
				return nil
			}

			// Resolve pidport location (test seam: override via env for subprocess tests).
			pidportPath, err := gui.PidportPath()
			if err != nil {
				return err
			}
			if d := os.Getenv("MCPHUB_GUI_TEST_PIDPORT_DIR"); d != "" {
				pidportPath = filepath.Join(d, gui.PidportFileLeaf)
			}

			// Bug-bash A5 (#18/#19/#20): resolve the effective port using
			// the explicit flag → persisted setting → 0 (auto-pick) order.
			// Pre-fix, the persisted `gui_server.port` setting was purely
			// cosmetic — startup always bound `port` as passed by the flag
			// (default 0). Operators who changed Settings + restarted were
			// still bound to an OS-assigned ephemeral port; the
			// "Restart required to take effect" warning was misleading.
			flagChanged := cmd.Flags().Changed("port")
			settingVal, _ := api.NewAPI().SettingsGet("gui_server.port")
			portIntent := classifyPersistedGUIPort(settingVal)
			port = resolveGuiPort(flagChanged, port, settingVal)
			// Reject an explicit privileged --port at the front door BEFORE the
			// persisted-fallback warning: the persisted-config and restart-handoff
			// protocol only carry [1024,65535], so a GUI launched on a privileged
			// port could start but never restart (bot #563). Hard-fail the explicit
			// flag rather than silently binding a port the restart action cannot
			// handle. Ordered before the warning so a refused explicit flag never
			// emits a misleading "fallback=explicit-flag" line for the same value.
			if err := validateExplicitGUIPortFlag(flagChanged, port); err != nil {
				return err
			}
			if portIntent.Kind == guiPortIntentInvalid {
				fallback := guiPortFallbackEphemeral
				if flagChanged {
					fallback = guiPortFallbackExplicitFlag
				}
				// PRE-SINK site — see the sibling call in the restart-argv path.
				// installGUIRuntimeSinks has not run yet, so OutOrStderr() still
				// means stderr-or-the-caller's-stream, which is what is wanted.
				emitInvalidGUIPortWarning(cmd.OutOrStderr(), portIntent, fallback)
			}

			// Resolve the console-lifetime policy ONCE, here, where the
			// flags live — the runtime below consumes this decision and
			// never re-derives it.
			//
			// The real discriminator is whether a console exists to
			// release, not whether the tray is on. Before this, the
			// release was gated on `!noTray`, which silently overloaded a
			// cosmetic flag into a process-lifetime policy: an operator
			// who wanted "hub + GUI, no tray icon" also got terminal-
			// coupled lifetime, and "tray on, keep my console" was
			// inexpressible. --foreground now expresses that directly.
			//
			// --no-tray keeps implying --foreground deliberately: it is
			// the documented dev workflow (`mcphub gui --no-browser
			// --no-tray --port 9125`, CLAUDE.md) and the E2E fixtures,
			// where Ctrl-C must keep working. Making it stop implying
			// foreground would silently take the console away from
			// exactly the runs that need it.
			releaseConsole := resolveReleaseConsole(ConsoleAttached(), foreground, noTray)

			handoff := os.Getenv(selfRestartHandoffEnv)
			if isRestartV3ChildLaunch(gui.RestartV3Enabled(), handoff) {
				handoff = consumeSelfRestartHandoff(handoff)
				descriptor, decodeErr := gui.DecodeSelfRestartHandoff(handoff)
				if decodeErr != nil {
					return decodeErr
				}
				return startRestartV3GUIChild(
					cmd, ctx, stop, handoff, pidportPath, descriptor.TargetPort,
					noBrowser, noTray, strictMode, releaseConsole,
				)
			}

			// Phase A: acquire the single-instance lock BEFORE binding any
			// port. If we bind first and the requested --port is already in
			// use (e.g. because the incumbent GUI owns it), ListenTCP fails
			// with "address already in use" and we never reach the
			// handshake path that would activate the incumbent. The
			// pidport file initially records the requested port (which may
			// be 0 = auto); once the server actually binds, we rewrite it
			// with the resolved port so second-instance handshake probes
			// reach the right place.
			//
			// RestartV3 structured handoffs were dispatched above into the
			// reservation-aware standby child path and never reach this normal
			// acquire. acquireSingleInstanceWithHandoff recognizes only the
			// literal "1" compatibility signal so a v1 parent from before a
			// rename-aside upgrade can still hand off to this v3 binary.
			lock, err := acquireSingleInstanceWithHandoff(ctx, pidportPath, port)
			if err != nil {
				if !errors.Is(err, gui.ErrSingleInstanceBusy) {
					return err
				}
				// PR #23 C1 stuck-instance recovery. Three flows:
				//   - default ErrSingleInstanceBusy without --force →
				//     try handshake; on failure, exit 1 with concise
				//     "rerun with --force" message (legacy).
				//   - bare --force → run Probe, print structured
				//     diagnostic, exit 2 (PRINT-ONLY; opening the lock
				//     folder is opt-in via --force --reveal).
				//   - --force --reveal → as bare --force, plus open the
				//     lock folder in the file manager.
				//   - --force --kill → KillRecordedHolder (with
				//     three-part identity gate); on success continue
				//     normal startup; on failure map Verdict to the
				//     appropriate exit code.
				if force {
					if kill {
						// Codex iter-10 P2 #1: pass signal-aware ctx
						// (from signal.NotifyContext above) so Ctrl+C
						// during the kill path actually cancels the
						// destructive operation. cmd.Context() is the
						// cobra parent context and ignores SIGINT.
						newLock, exitCode := runForceKill(ctx, cmd, pidportPath, yes)
						if newLock != nil {
							// Take-over succeeded: continue into Phase B
							// with the freshly-acquired lock. Helper
							// extraction (no goto) keeps repo style intact.
							return startGuiServer(cmd, ctx, stop, newLock, port, noBrowser, noTray, strictMode, releaseConsole, pidportPath)
						}
						return forceExit(exitCode)
					}
					exitCode := runForceDiagnostic(ctx, cmd, pidportPath, reveal)
					return forceExit(exitCode)
				}
				activationErr := gui.TryActivateIncumbent(pidportPath, 2*time.Second)
				noTarget, activationErr := handleIncumbentActivationResult(cmd.OutOrStdout(), activationErr)
				if activationErr != nil {
					return fmt.Errorf(
						"another mcphub gui is running but unreachable (%v); "+
							"rerun with --force for diagnostic, or --force --kill to recover",
						activationErr)
				}
				if noTarget {
					return nil
				}
				fmt.Fprintln(cmd.OutOrStdout(), "activated existing mcphub gui")
				return nil
			}
			return startGuiServer(cmd, ctx, stop, lock, port, noBrowser, noTray, strictMode, releaseConsole, pidportPath)
		},
	}
	c.Flags().IntVar(&port, "port", 0, "TCP port on 127.0.0.1 in [1024,65535] (0 = auto-pick from ephemeral)")
	c.Flags().BoolVar(&noBrowser, "no-browser", false, "do not auto-launch a browser window")
	c.Flags().BoolVar(&noTray, "no-tray", false, "do not show the system-tray icon (also implies --foreground: the GUI stays attached to the launching terminal and exits when that terminal closes)")
	c.Flags().BoolVar(&foreground, "foreground", false, "keep the launching terminal's console: Ctrl-C works and the GUI exits when that terminal closes. Default is background — the GUI releases the console at startup and survives the terminal. Implied by --no-tray.")
	c.Flags().BoolVar(&force, "force", false, "stuck-instance recovery: print the diagnostic (PRINT-ONLY; add --reveal to also open the lock folder in the file manager). Add --kill to terminate the recorded PID after a three-part identity gate.")
	c.Flags().BoolVar(&kill, "kill", false, "with --force: kill the recorded PID (image/argv/start-time gate); SIGKILL/TerminateProcess. The kernel releases the flock as a side effect.")
	c.Flags().BoolVar(&yes, "yes", false, "with --force --kill or --reset-port: skip the confirmation prompt (required in non-interactive shells).")
	c.Flags().BoolVar(&resetPort, "reset-port", false, "discard the persistent hub-mcp port (instance_id preserved) and emit credential-rotation guidance — does NOT start the server")
	c.Flags().BoolVar(&strictMode, "strict-mode", false, "pass --strict-mode through to the supervisor process the GUI spawns (corp-managed Windows hosts; see CLAUDE.md \"Hardened state-file writes\")")
	c.Flags().BoolVar(&reveal, "reveal", false, "during stuck-instance recovery, also open the lock folder in the file manager (off by default — the diagnostic already prints the path; opt in to avoid leaking a persistent explorer.exe window under the Windows 'launch folder windows in a separate process' option)")
	_ = c.Flags().MarkHidden("force")
	_ = c.Flags().MarkHidden("kill")
	_ = c.Flags().MarkHidden("yes")
	_ = c.Flags().MarkHidden("reveal")
	return c
}

// handleIncumbentActivationResult is the single owner for the typed
// reachable-but-no-target outcome. It reports reason-specific guidance and
// marks that outcome handled; all other errors remain errors for the caller.
func handleIncumbentActivationResult(stdout io.Writer, err error) (noTarget bool, otherErr error) {
	if err == nil {
		return false, nil
	}
	var ina *gui.IncumbentNoActivationTargetError
	if !errors.As(err, &ina) {
		return false, err
	}
	if ina.Reason == gui.ReasonHeadless {
		fmt.Fprintf(stdout,
			"mcphub gui is already running headless on port %d. SSH-tunnel and visit http://127.0.0.1:%d/\n",
			ina.Port, ina.Port)
		return true, nil
	}
	fmt.Fprintf(stdout,
		"mcphub gui is already running but has no dashboard window to focus. Open http://127.0.0.1:%d/ in a browser.\n",
		ina.Port)
	return true, nil
}

func wireDashboardActivation(
	s *gui.Server,
	noBrowser bool,
	stderr io.Writer,
	focusWindow func(string) error,
	launchBrowser func(string) error,
	headlessSession func() bool,
) {
	s.OnActivateWindow(func() error {
		return activateDashboardWindow(
			noBrowser,
			s.Port(),
			stderr,
			focusWindow,
			launchBrowser,
			headlessSession,
		)
	})
}

func activateDashboardWindow(
	noBrowser bool,
	port int,
	stderr io.Writer,
	focusWindow func(string) error,
	launchBrowser func(string) error,
	headlessSession func() bool,
) error {
	err := focusWindow("Local Dashboard")
	if err == nil {
		return nil
	}
	if !errors.Is(err, gui.ErrFocusNoWindow) {
		// Win32 transient failure — log + best-effort 204 so the
		// second invocation prints "activated" (incumbent IS
		// reachable, just focus jitter happened).
		fmt.Fprintf(stderr,
			"activate-window: focus failed (no fallback for non-no-window error): %v\n", err)
		return nil
	}
	if headlessSession() {
		fmt.Fprintln(stderr,
			"activate-window: focus failed and headless session — no browser to open")
		return &gui.ActivationNoTargetError{Reason: gui.ReasonHeadless}
	}
	if noBrowser {
		fmt.Fprintln(stderr,
			"activate-window: focus failed and --no-browser is set — not launching a browser")
		return &gui.ActivationNoTargetError{Reason: gui.ReasonNoBrowserWindow}
	}
	url := fmt.Sprintf("http://127.0.0.1:%d/", port)
	if launchErr := launchBrowser(url); launchErr != nil {
		fmt.Fprintf(stderr,
			"activate-window: focus failed (%v); browser launch also failed: %v\n",
			err, launchErr)
	}
	return nil
}

func activateDashboardFromTray(
	pidportPath string,
	port int,
	stderr io.Writer,
	tryActivate func(string, time.Duration) error,
	launchBrowser func(string) error,
) {
	err := tryActivate(pidportPath, 500*time.Millisecond)
	if err == nil {
		return
	}
	var noTarget *gui.IncumbentNoActivationTargetError
	if !errors.As(err, &noTarget) {
		fmt.Fprintf(stderr, "tray: activate-window failed: %v\n", err)
		return
	}
	// Build the URL from the port the handshake ALREADY VERIFIED via /api/ping,
	// not from `port`: that is the REQUESTED port, which is 0 under the default
	// `--port 0` (the server bound an ephemeral one), so a tray click would open
	// http://127.0.0.1:0/. The typed error carries the verified port precisely so
	// callers need not re-read the pidport, which races a successor's pre-bind
	// port=0 write. Fall back to the requested port only if the error somehow
	// carries none (an explicit `--port N` run).
	activePort := noTarget.Port
	if activePort == 0 {
		activePort = port
	}
	url := fmt.Sprintf("http://127.0.0.1:%d/", activePort)
	fmt.Fprintf(stderr,
		"tray: incumbent has no dashboard window to focus; opening %s\n", url)
	if err := launchBrowser(url); err != nil {
		fmt.Fprintf(stderr, "tray: browser launch failed: %v\n", err)
	}
}

// startGuiServer runs Phase B: server start, status poller, optional
// browser launch, optional tray icon. Extracted from the RunE body so
// both the normal-acquire path AND the `--force --kill` recovery path
// share one implementation. Caller MUST own a non-nil lock; this
// helper takes ownership of the lock's release.
//
// Helper-extraction approach (preferred over goto + label) keeps the
// repo's existing control-flow style. See plan task 4 §"alternative".
func startGuiServer(cmd *cobra.Command, ctx context.Context, stop context.CancelFunc,
	lock *gui.SingleInstanceLock, port int, noBrowser, noTray, strictMode, releaseConsole bool, pidportPath string) error {
	return startGuiServerWithStartup(
		cmd, ctx, stop, lock, port, noBrowser, noTray, strictMode, releaseConsole, pidportPath, nil,
	)
}

type guiServerStartup struct {
	server        *gui.Server
	listenerOwner *gui.GUIListenerOwner
	bound         net.Listener
}

func composeGuiServerRestartV3(
	ctx context.Context,
	cmd *cobra.Command,
	ownedLease *releaseOnceLease,
	port int,
	pidportPath string,
	startup *guiServerStartup,
	argv []string,
	runtime restartV3ParentRuntime,
) (*gui.Server, error) {
	restartV3Enabled := gui.RestartV3Enabled()
	var s *gui.Server
	if startup == nil {
		s = gui.NewServer(gui.Config{
			Port:             port,
			Version:          versionString(),
			RestartV3Enabled: restartV3Enabled,
		})
	} else {
		s = startup.server
		if s == nil {
			return nil, errors.New("restart child startup server is nil")
		}
	}
	if !restartV3Enabled {
		return s, nil
	}

	deps, err := buildRestartV3ParentDependencies(
		ctx, cmd, ownedLease, pidportPath, s.Port, argv, runtime,
	)
	if err != nil {
		return nil, fmt.Errorf("compose restart v3 parent: %w", err)
	}
	if err := s.ConfigureRestartCoordinator(deps); err != nil {
		return nil, fmt.Errorf("configure restart v3 parent: %w", err)
	}
	return s, nil
}

func startRestartV3GUIChild(
	cmd *cobra.Command,
	ctx context.Context,
	stop context.CancelFunc,
	handoff string,
	pidportPath string,
	port int,
	noBrowser bool,
	noTray bool,
	strictMode bool,
	releaseConsole bool,
) error {
	deadlines := gui.DefaultRestartDeadlines()
	return runRestartV3ChildStartup(ctx, restartV3ChildStartupConfig{
		Handoff:     handoff,
		PID:         os.Getpid(),
		PidportPath: pidportPath,
		Port:        port,
		Version:     versionString(),
		Deadlines:   deadlines,
		StartRuntime: func(
			runtimeCtx context.Context,
			server *gui.Server,
			owner *gui.GUIListenerOwner,
			bound net.Listener,
			lease gui.SingleInstanceLease,
		) error {
			return startGuiServerWithStartup(
				cmd,
				runtimeCtx,
				stop,
				lease,
				port,
				noBrowser,
				noTray,
				strictMode,
				releaseConsole,
				pidportPath,
				&guiServerStartup{server: server, listenerOwner: owner, bound: bound},
			)
		},
	})
}

// shouldAutoLaunchBrowser reports whether this GUI launch should open a
// browser: only the INITIAL launch (startup == nil) with the browser not
// suppressed. A restart child (startup != nil) inherits a live operator tab
// that the Phase-H frontend redirects to the new origin, so it must never open
// a second window — otherwise every self-restart floods a fresh browser app
// window (bot #563 deep-sec F3, same family as the autostart browser-flood fix).
func shouldAutoLaunchBrowser(startup *guiServerStartup, noBrowser bool) bool {
	return startup == nil && !noBrowser
}

func startGuiServerWithStartup(cmd *cobra.Command, ctx context.Context, stop context.CancelFunc,
	lock gui.SingleInstanceLease, port int, noBrowser, noTray, strictMode, releaseConsole bool, pidportPath string,
	startup *guiServerStartup) error {
	ownedLease, ok := lock.(*releaseOnceLease)
	if !ok {
		ownedLease = &releaseOnceLease{lease: lock}
	}
	defer ownedLease.Release()

	// Route this command's output through the switchable diagnostic sinks
	// BEFORE anything is written, so every cmd.OutOrStdout()/ErrOrStderr()
	// site below — including ones added later, which is the point — keeps
	// working after the console is released. Pass-through until then.
	//
	// Below this line the stderr accessor is ErrOrStderr(), NOT OutOrStderr():
	// cobra resolves OutOrStderr() through getOut(), which returns the out
	// writer once SetOut is installed here, so an OutOrStderr() diagnostic
	// would land on STDOUT. See gui_diagnostic_sink.go. The `--force` /
	// `--reset-port` paths run BEFORE this call and are unaffected either way.
	installGUIRuntimeSinks(cmd)

	// Phase B: start the HTTP server. Server.Start binds 127.0.0.1
	// on the configured port (0 = OS-assigned) and signals ready
	// once the listener is live.
	api.SupervisorIPCStatusFn = api.DialSupervisorIPCStatus
	// §3 fail-loud (backend-loss reconcile FALLBACK signal): wire the serena
	// router's IPC status reader so a serena daemon restart/death is reconciled
	// against the router's live sessions even when no client request is in
	// flight to surface the dead forward. The always-on forward-failure floor
	// is the primary signal; this is the safety net behind it.
	gui.SetSerenaBackendStatusFn(api.DialSupervisorIPCStatus)
	// v0.6 idle-shutdown (#6, spec §6): wire the idle sweeper's two seams.
	// The threshold reader resolves the GUI-settable daemons.serena_idle_shutdown
	// each tick (so an operator change takes effect within ~60s, no restart); the
	// stop writer records Desired=stopped+IntentReasonIdle on the unified
	// supervisor-intent stops sub-block via the §4/Phase-E corrected stop path.
	gui.SetSerenaIdleShutdownFns(
		func() (time.Duration, bool) {
			v, err := api.NewAPI().SettingsGet(api.SerenaIdleShutdownSettingKey)
			if err != nil {
				// Read failure → disable idle-shutdown for this tick (fail-safe:
				// never idle a daemon on a settings read error).
				return 0, false
			}
			return api.SerenaIdleShutdownThreshold(v)
		},
		func(taskName string, now time.Time) (bool, error) {
			return api.NewAPI().WriteSerenaIdleStopResult(taskName, now)
		},
	)
	var selfRestartExitRequested atomic.Bool
	s, err := composeGuiServerRestartV3(
		ctx,
		cmd,
		ownedLease,
		port,
		pidportPath,
		startup,
		os.Args[1:],
		defaultRestartV3ParentRuntime(&selfRestartExitRequested),
	)
	if err != nil {
		return err
	}

	// Phase C.2 wiring (v0.5.x plan §C.2): construct the serena routing
	// dependencies and hand them to the GUI server. The /serena/mcp
	// handler is registered unconditionally by NewServer but emits HTTP
	// 503 with `phase_e_status: deferred` until production deps land.
	// Wiring failures (registry path resolution) degrade gracefully —
	// the dashboard + every other route still works; only the
	// path-aware serena routing is unavailable.
	registryPath, regErr := api.DefaultRegistryPath()
	if regErr == nil {
		reg := api.NewRegistry(registryPath)
		if err := reg.Load(); err != nil {
			fmt.Fprintf(cmd.ErrOrStderr(),
				"serena-router: registry load warning (will retry lazily on first call): %v\n", err)
		}
		resolver := serena_routing.NewWorkspaceResolver(reg, registryPath)
		sessions := serena_routing.NewSessionRouter()
		s.SetSerenaRouterProduction(resolver, sessions)
		if rawManifest, err := api.NewAPI().ManifestGet("mcp-language-server"); err != nil {
			fmt.Fprintf(cmd.ErrOrStderr(),
				"lsp-router: manifest load failed; /lsp/<language>/mcp will return errors until next restart: %v\n", err)
		} else if m, err := config.ParseManifest(strings.NewReader(rawManifest)); err != nil {
			fmt.Fprintf(cmd.ErrOrStderr(),
				"lsp-router: manifest parse failed; /lsp/<language>/mcp will return errors until next restart: %v\n", err)
		} else {
			// Separate registry handle for the LSP resolver: it refreshes
			// (reg.Load() + LSPEntries()) under its own RWMutex, independently
			// of the serena resolver above. *api.Registry has no in-process
			// mutex, so sharing one object would let the two resolvers' caches
			// race on the Workspaces slice after a registry mtime change — a
			// data race under concurrent /serena/mcp + /lsp/<lang>/mcp traffic
			// (bot PR #266 r5).
			lspReg := api.NewRegistry(registryPath)
			lspResolver := lsp_routing.NewWorkspaceResolver(lspReg, registryPath, m.Languages)
			lspSessions := lsp_routing.NewSessionRouter()
			s.SetLSPRouterProduction(lspResolver, lspSessions, m.Languages)
			go runLSPSessionCleanupTicker(ctx, lspSessions, time.Hour, lsp_routing.DefaultSessionTTL)
		}
		// Phase 5 (bot PR #253 finding 1): wire the one-time supervisor
		// cutover the auto-register-on-miss path runs when introducing the
		// FIRST serena runtime_spec while a supervisor is running (reap the
		// running supervisor → install → start the current binary). The
		// primitives are the migrate cutover ones (Windows-only; the
		// non-Windows stubs fail loud, so the introduce path 503s off-Windows).
		// Progress text goes to io.Discard — auto-register surfaces the wrapped
		// error to the client (→ 503) and the supervisor-events audit log.
		api.SetSerenaAutoRegisterCutoverPrimitives(
			func(c context.Context) error { return defaultMigrateSerenaReap(c, io.Discard) },
			func(c context.Context) error { return defaultMigrateSerenaStart(c, io.Discard) },
			defaultMigrateSerenaStartSupported,
			// Phase 2 / Starter A: the introduce-while-running cutover holds the
			// SAME supervisor.lock interlock the migrate uses, so the two reaping
			// flows are mutually exclusive (neither force-kills the other's
			// lock-holding process). Windows-only real lock; the non-Windows binding
			// is a no-op (the introduce path 503s off-Windows anyway).
			defaultAcquireSupervisorInterlock,
		)
		// Sweep all three serena session stores on one ticker: the
		// cross-package sticky-routing SessionRouter AND (Finding 2) the
		// two router-owned stores (routerSessionStore + daemonSessionStore)
		// via the gui Server's SweepSerenaSessions. Reusing this existing
		// goroutine (rather than spawning a second ticker) keeps one
		// correctly-shutdown background loop and ages every store on the
		// same 24h idle clock.
		go runSessionCleanupTicker(ctx, s, sessions, time.Hour, serena_routing.DefaultSessionTTL)
		// §3 fail-loud reconcile FALLBACK: a faster, lighter ticker that polls
		// the supervisor IPC status and tears down router sessions for any
		// serena workspace whose daemon restarted (PID changed) or vanished
		// since the last tick. Scoped to only the workspaces the router has live
		// sessions for, so it is a cheap no-op when the router is idle. 30s
		// bounds the post-death zombie window for the case where NO client
		// request fires to trip the always-on forward-failure floor.
		go runSerenaBackendLossReconcileTicker(ctx, s, 30*time.Second)
		go s.RunSerenaBackendLossEventSubscriber(ctx)
		// v0.6 idle-shutdown (#6, spec §6): the 60s in-GUI idle sweeper. Each
		// tick it stops every RUNNING serena pool daemon idle longer than the
		// operator-configured threshold (daemons.serena_idle_shutdown) by
		// writing IntentReasonIdle on the unified intent; the next /serena/mcp
		// request wakes it (WakeIdleFn). It reads per-daemon LAST-ACTIVITY (not
		// wall-clock since spawn), so a daemon mid-call or recently-active is
		// never idled. A separate goroutine from the §3 fallback because their
		// cadences differ (60s vs 30s) and their concerns are orthogonal
		// (idle-stop vs backend-loss teardown); both exit on ctx cancel.
		go runSerenaIdleShutdownTicker(ctx, s, 60*time.Second)

		// Phase-1 workspace-daemon auto-prune (#prune): a separate 60s in-GUI
		// sweeper that auto-removes daemons whose workspace is structurally dead —
		// an ephemeral .claude/worktrees/agent-* worktree, or a deleted directory —
		// so the per-workspace serena+LSP daemon set stops growing without bound.
		// Gated by daemons.auto_prune_workspaces (default on); skips a workspace
		// mid serena call; non-destructive (re-registers on next open).
		go runWorkspacePruneTicker(ctx, s, 60*time.Second)
	} else {
		fmt.Fprintf(cmd.ErrOrStderr(),
			"serena-router: registry path resolution failed; /serena/mcp will return 503 until next restart: %v\n", regErr)
	}
	wireDashboardActivation(
		s,
		noBrowser,
		cmd.ErrOrStderr(),
		gui.FocusBrowserWindow,
		gui.LaunchBrowser,
		gui.HeadlessSession,
	)

	ready := make(chan struct{})
	errCh := make(chan error, 1)
	if startup == nil {
		go func() { errCh <- s.Start(ctx, ready) }()
	} else {
		go func() {
			errCh <- s.ContinueWithGUIListener(ctx, ready, startup.listenerOwner, startup.bound)
		}()
	}

	// Poll daemon status every 5s and push daemon-state events onto /api/events.
	//
	// v0.6 Workstream B (§3.1) — route the poller through the server's
	// supervisor-IPC-owned, fail-loud snapshot (s.StatusProvider() →
	// DaemonStatusSnapshot) rather than gui.RealStatusProvider{} →
	// api.Status()'s scheduler-fallback path. A down supervisor then
	// emits a `poller-error` event on the SSE channel instead of stale
	// scheduler `daemon-state` deltas that would clear the Dashboard's
	// degraded banner and re-introduce the false-negative this phase
	// removes on the polling channel. The Dashboard subscribes to
	// `poller-error` (Dashboard.tsx) and sets its degraded banner from
	// it; the same error is also fanned to the tray aggregator's error
	// channel below so the tray icon goes red (PR #281 round-2 P2/P3).
	poller := gui.NewStatusPoller(s.StatusProvider(), s.Broadcaster(), 5*time.Second)
	// Tray state plumbing (C3): wire a snapshot channel between
	// poller and tray. Aggregator goroutine reads each snapshot,
	// computes a TrayState, and pushes onto trayStateCh ONLY when
	// the aggregate changes — avoids redundant SetIcon calls when
	// individual daemons flap but the overall state is steady.
	//
	// Both channels are size-1 buffered with non-blocking sends
	// at every send site so a stalled tray cannot back up the
	// poller, and a stalled poller cannot back up status reads.
	snapshotCh := make(chan []api.DaemonStatus, 1)
	trayStateCh := make(chan tray.TrayState, 1)
	// pollerErrCh feeds the tray aggregator the poller's fetch errors so
	// a down supervisor drives the tray icon to StateError instead of
	// freezing at its last value. The poll-error path early-returns
	// before fanning a snapshot, so the snapshot channel alone would
	// starve the aggregator and the icon would stay green over a down
	// supervisor (PR #281 round-2 P2). Size-1 buffered, non-blocking
	// send at the poller, drop-stale — matching snapshotCh.
	pollerErrCh := make(chan error, 1)
	poller.SetSnapshotChannel(snapshotCh)
	poller.SetErrorChannel(pollerErrCh)
	select {
	case <-ready:
		// Now we know the actual bound port. Unconditionally rewrite
		// the pidport with our PID + the bound port. The flock on
		// *.lock still gates ownership; the pidport file is
		// ownership metadata the lock holder freely updates.
		//
		// Codex PR #23 P2 #2: previously this branch only ran when
		// actualPort != port (the requested port). After a
		// successful --force --kill takeover the pidport still held
		// the killed incumbent's PID + port; if the user requested
		// an explicit port that we then bound, the conditional
		// short-circuited and the stale PID/port persisted forever.
		// The new unconditional write is idempotent on the normal-
		// acquire path (pidport already has our PID + the requested
		// port from AcquireSingleInstanceAt) and corrective on the
		// takeover path.
		actualPort := s.Port()
		if err := gui.WritePidport(pidportPath, os.Getpid(), actualPort); err != nil {
			fmt.Fprintf(cmd.ErrOrStderr(), "warning: pidport rewrite: %v\n", err)
		}
		fmt.Fprintf(cmd.OutOrStdout(), "GUI listening on http://127.0.0.1:%d\n", actualPort)
	case err := <-errCh:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
	select {
	case <-s.Activated():
	case err := <-errCh:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
	go aggregateTrayState(ctx, snapshotCh, pollerErrCh, trayStateCh)
	go poller.Run(ctx)

	// "GUI owns supervisor lifecycle" — the GUI process is the operator-
	// visible mcphub. When GUI is running (tray icon present), the
	// supervisor (and its 14 daemon children) must also be running.
	// When GUI exits, the supervisor exits with it. This contract
	// matches standard tray-app expectations (Steam, Discord, Docker
	// Desktop): one process tree, one lifecycle, one tray indicator.
	//
	// ensureSupervisorRunning is fail-soft — if the spawn/adopt probe
	// fails, GUI keeps running so the operator can investigate via Logs
	// screen and Dashboard banner; we don't want a transient IPC hiccup
	// to lock the operator out of the recovery surface.
	//
	// E2E test seam: MCPHUB_E2E_SUPERVISOR=none suppresses the entire
	// spawn block so Playwright fixtures (which spawn `mcphub gui`
	// per-test under a temp HOME) don't time out 15s on every test
	// waiting for IPC bind that will never happen — they have no
	// supervisor-intent.json in the temp dir. Mirror of the existing
	// MCPHUB_E2E_SCHEDULER=none pattern at status_enrich.go.
	//
	// PR #212 r5 silent-failure-hunt finding 4: emit a visible
	// warning when the seam fires so a production operator who
	// accidentally inherits the env var from a CI shell can spot
	// the suppression instead of seeing a permanently-empty
	// Dashboard with no diagnostic.
	if os.Getenv("MCPHUB_E2E_SUPERVISOR") == "none" {
		fmt.Fprintln(cmd.ErrOrStderr(),
			"warning: MCPHUB_E2E_SUPERVISOR=none is set — supervisor spawn suppressed (test seam; not for production use)")
		return <-errCh
	}
	supervisorBin, binErr := resolveMCPHubBinary()
	if binErr != nil {
		fmt.Fprintf(cmd.ErrOrStderr(), "warning: resolve mcphub binary for supervisor spawn: %v\n", binErr)
	}
	// The manager owns the swappable "current live supervisor owner"
	// handle. For a GUI-SPAWNED supervisor it also runs a bounded
	// respawn loop so an unexpected supervisor-child death under a live
	// GUI self-heals (startExitMonitor only LOGS the death; the loop
	// respawns it). An ADOPTED supervisor gets no manager and no loop —
	// the GUI does not own its lifecycle, so Stop is a no-op there.
	var manager *supervisorManager
	if supervisorBin != "" {
		supervisor, spawnErr := ensureSupervisorRunning(ctx, supervisorBin, strictMode, 15*time.Second)
		if spawnErr != nil {
			fmt.Fprintf(cmd.ErrOrStderr(), "warning: supervisor: %v\n", spawnErr)
		} else if supervisor.Spawned() {
			fmt.Fprintf(cmd.OutOrStdout(), "supervisor: spawned PID %d (GUI owns lifecycle)\n", supervisor.Pid())
		} else {
			fmt.Fprintln(cmd.OutOrStdout(), "supervisor: adopted (running externally)")
		}
		// The Spawned()-gated construct-and-arm decision is extracted into
		// armSupervisorManager so the REAL wiring (manager constructed AND
		// its respawn loop launched, seeded from the package-level
		// spawnSupervisorFn) is unit-testable WITHOUT a real `mcphub
		// supervise` binary — the seam-based newTestManager tests inject
		// spawnFn directly and so never exercise this gate. A nil/adopted
		// owner returns nil here (no manager, no loop), matching the
		// adopt contract. §5 deploy-verification gap.
		manager = armSupervisorManager(ctx, supervisor, supervisorBin, strictMode)
	}
	// This defer is registered after the lock.Release defer at the
	// top of startGuiServer. Under Go's LIFO defer stack, this
	// supervisor-shutdown defer therefore runs FIRST on function
	// return — supervisor stops before the single-instance lock is
	// released, so the next gui invocation cannot race a still-
	// shutting-down supervisor.
	//
	// manager.Stop latches shuttingDown + snapshots the CURRENT owner
	// under one mutex, so it always stops the RESPAWNED handle (not a
	// stale captured one) and the respawn loop can never install a fresh
	// supervisor after this returns.
	defer func() {
		if manager == nil {
			return
		}
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := stopSupervisorManagerUnlessSelfRestart(shutdownCtx, manager, &selfRestartExitRequested); err != nil {
			fmt.Fprintf(cmd.ErrOrStderr(), "supervisor shutdown: %v\n", err)
		}
	}()

	// Startup is complete: the listener is bound, the port has been
	// reported, and the supervisor is up. From here `mcphub gui` is a
	// long-lived BACKGROUND app, so release the parent console — otherwise
	// the terminal it was launched from owns its lifetime and closing that
	// terminal kills the GUI and its tray icon (CTRL_CLOSE_EVENT to every
	// attached console client). See process.ReleaseParentConsole.
	//
	// releaseConsole is resolved ONCE by newGuiCmdReal from the injected
	// console state plus --foreground/--no-tray; this layer consumes the
	// decision and deliberately does not re-derive it from a flag.
	//
	// Placed here for two ordering reasons:
	//   - AFTER every operator-facing startup line ("GUI listening on …",
	//     "supervisor: …"), because releasing the console discards all
	//     later console-backed stdout/stderr writes. Diagnostics written
	//     after this point survive via the durable sink engaged below, not
	//     by luck.
	//   - BEFORE the browser and tray spawns below. `mcphub tray` is the
	//     same Windows-subsystem binary and attaches to ITS parent's
	//     console at startup; releasing first means the tray has no console
	//     to inherit, so it survives the terminal too. Releasing after the
	//     tray spawn would leave the exact "tray disappears" symptom.
	//
	// The GUI-spawned SUPERVISOR above is NOT protected by this ordering —
	// it is spawned before this point and, being the same binary, used to
	// attach itself to the very console we are about to drop. Its immunity
	// comes from configureSupervisorDetach marking the child
	// attach-suppressed, which holds no matter where this release sits.
	if releaseConsole {
		releaseConsoleForBackgroundGUI(process.ReleaseParentConsole)
	}

	if shouldAutoLaunchBrowser(startup, noBrowser) {
		url := fmt.Sprintf("http://127.0.0.1:%d/", s.Port())
		if err := gui.LaunchBrowser(url); err != nil {
			fmt.Fprintf(cmd.ErrOrStderr(), "warning: could not auto-launch browser: %v\n", err)
		}
	}
	if !noTray {
		go func() {
			// PR #24 added child-failure propagation: tray.Run
			// returns non-nil when the tray subprocess exits
			// unexpectedly while ctx is alive. Surface the error
			// so the GUI doesn't silently lose tray functionality.
			// Tray callbacks dispatch through the SAME HTTP endpoints as the
			// Dashboard buttons. Going through HTTP (rather than calling
			// api.NewAPI() directly) means the SSE Broadcaster fires
			// bulk-action lifecycle events that any open Dashboard tab
			// observes — buttons flash "Starting…" / "Stopping…" exactly
			// as if the user had clicked them in the browser. Without
			// this round-trip the tray would mutate daemon state silently
			// and the Dashboard would only catch up via the per-daemon
			// SSE updates, with no overall progress indicator. One
			// pipeline, one source of truth.
			port := int(s.Port())
			postBulk := func(action string) error {
				url := fmt.Sprintf("http://127.0.0.1:%d/api/%s-all", port, action)
				req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, nil)
				if err != nil {
					return err
				}
				// requireSameOrigin gate accepts requests with no Origin
				// header (CSRF middleware allows non-browser clients);
				// adding the loopback Origin makes the request indistinguishable
				// from a Dashboard fetch on the wire.
				req.Header.Set("Origin", fmt.Sprintf("http://127.0.0.1:%d", port))
				resp, err := http.DefaultClient.Do(req)
				if err != nil {
					return err
				}
				defer resp.Body.Close()
				// 207 Multi-Status is partial failure (per-task errors).
				// 4xx (e.g. 403 from requireSameOrigin) and 5xx are
				// real failures. Only 200 is a clean success — flag
				// everything else so the tray surfaces it to stderr
				// instead of letting QuitAndStopAll silently shut
				// down without confirming the action ran. Codex bot
				// review on PR #38 commit ef0f4ea P2 ("Treat all
				// non-2xx tray bulk POST responses as failures").
				if resp.StatusCode == http.StatusMultiStatus {
					return fmt.Errorf("HTTP 207 partial: at least one task failed; see daemon logs")
				}
				if resp.StatusCode >= 400 {
					return fmt.Errorf("HTTP %d", resp.StatusCode)
				}
				return nil
			}
			// state-read-relax broadcast channel — buffered so a
			// quick init-push doesn't block during tray.Run's
			// goroutine startup window.
			stateRelaxCh := make(chan bool, 4)
			go pollStateReadRelaxForTray(ctx, port, stateRelaxCh)

			if err := tray.Run(ctx, tray.Config{
				ActivateWindow: func() {
					activateDashboardFromTray(
						pidportPath,
						port,
						cmd.ErrOrStderr(),
						gui.TryActivateIncumbent,
						gui.LaunchBrowser,
					)
				},
				StateCh:          trayStateCh,
				StateReadRelaxCh: stateRelaxCh,
				ToggleStateReadRelax: func() {
					if err := postToggleStateRelax(ctx, port, stateRelaxCh); err != nil {
						fmt.Fprintf(cmd.ErrOrStderr(), "tray: toggle state-read-relax: %v\n", err)
					}
				},
				Quit: stop,
				QuitAndStopAll: func() {
					// Stop all via HTTP (so the Dashboard sees the SSE
					// lifecycle), then trigger the GUI shutdown. Errors
					// don't block the shutdown — partial cleanup beats
					// a hung GUI.
					if err := postBulk("stop"); err != nil {
						fmt.Fprintf(cmd.ErrOrStderr(), "tray: POST /api/stop-all: %v\n", err)
					}
					stop()
				},
				RunAllDaemons: func() {
					if err := postBulk("restart"); err != nil {
						fmt.Fprintf(cmd.ErrOrStderr(), "tray: POST /api/restart-all: %v\n", err)
					}
				},
				StopAllDaemons: func() {
					if err := postBulk("stop"); err != nil {
						fmt.Fprintf(cmd.ErrOrStderr(), "tray: POST /api/stop-all: %v\n", err)
					}
				},
				RescanClients: func() {
					// Publish an SSE event so any open Servers/Migration
					// screen re-fetches its scan state. Same SSE bus the
					// Dashboard already subscribes to (PR #38), so the
					// pipeline stays single-source-of-truth.
					s.Broadcaster().Publish(gui.Event{Type: "clients-rescan"})
				},
				OpenLogsFolder: func() {
					// In-process spawn — best-effort, errors logged to
					// the parent's stderr so a failed spawn doesn't
					// silently no-op the menu click.
					//
					// MkdirAll first: on first-run hosts the daemon
					// hasn't written any log yet so the dir doesn't
					// exist, and explorer.exe / xdg-open / open all
					// fail on a non-existent path. Same precedent as
					// /api/logs-folder backend handler and the
					// advanced.open_app_data_folder Settings action.
					// Codex bot review on PR #48 P2.
					dir := api.DefaultLogDir()
					if err := os.MkdirAll(dir, 0700); err != nil {
						fmt.Fprintf(cmd.ErrOrStderr(), "tray: mkdir logs folder: %v\n", err)
						return
					}
					if err := gui.OpenPath(dir); err != nil {
						fmt.Fprintf(cmd.ErrOrStderr(), "tray: open logs folder: %v\n", err)
					}
				},
				OpenDataFolder: func() {
					// Same MkdirAll-before-spawn precedent as
					// OpenLogsFolder. The data dir holds gui-preferences
					// + secrets; first-run hosts have neither yet.
					// Codex bot review on PR #48 P2.
					dir := filepath.Dir(api.SettingsPath())
					if err := os.MkdirAll(dir, 0700); err != nil {
						fmt.Fprintf(cmd.ErrOrStderr(), "tray: mkdir data folder: %v\n", err)
						return
					}
					if err := gui.OpenPath(dir); err != nil {
						fmt.Fprintf(cmd.ErrOrStderr(), "tray: open data folder: %v\n", err)
					}
				},
			}); err != nil {
				fmt.Fprintf(cmd.ErrOrStderr(), "tray: %v (GUI continues without tray)\n", err)
			}
		}()
	}

	// Auto-cleanup ticker: every 5 min, POST to the GUI's own
	// /api/cleanup/orphans with {apply:true} so orphan
	// mcp-language-server processes left behind by un-migrated
	// agent direct-stdio don't accumulate. Opt out by setting
	// MCPHUB_DISABLE_AUTO_CLEANUP=1. Runs as a sibling goroutine
	// to the tray loop (NOT inside it) so the ticker fires even
	// when --no-tray suppresses the tray.
	go runAutoCleanupTicker(ctx, int(s.Port()))

	return <-errCh
}

// runSessionCleanupTicker drops serena session bindings whose lastSeen
// is older than ttl. It is owned by the GUI server lifecycle and exits
// when ctx is cancelled.
//
// Finding 2: each tick sweeps BOTH the cross-package sticky-routing
// SessionRouter (sessions.CleanupWithTTL) AND the gui Server's two
// router-owned session stores (s.SweepSerenaSessions — routerSessionStore
// + daemonSessionStore). Before this, only the sticky router was swept;
// an initialize-then-disconnect client (e.g. the Phase-3 reconcile probe,
// which never DELETEs) left router-session + daemon-session entries
// forever because their expire-on-read only fires on reuse/DELETE. s may
// be nil in tests that exercise only the sticky-router path; the sweep is
// skipped then.
func runSessionCleanupTicker(ctx context.Context, s *gui.Server, sessions *serena_routing.SessionRouter, interval, ttl time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			_ = sessions.CleanupWithTTL(now, ttl)
			if s != nil {
				_ = s.SweepSerenaSessions(now, ttl)
			}
		}
	}
}

// runSerenaBackendLossReconcileTicker is the §3.x backend-loss FALLBACK
// signal's driver. Every `interval`, or sooner when a daemon-backend-lost event
// coalesces a trigger, it calls s.ReconcileSerenaBackendLossViaIPC, which polls
// the supervisor IPC status and, for any serena workspace the
// router has live sessions for whose daemon restarted (PID changed) or vanished
// since the last tick, tears those sessions out of all three router stores so
// the next /serena/mcp request fails loud instead of zombie-200-ing a dead
// backend. It is owned by the GUI server lifecycle and exits when ctx is
// cancelled. The reconcile read is bounded by a short per-tick context so a
// slow/unreachable supervisor IPC cannot wedge the ticker. s may be nil in
// tests; the tick is skipped then.
func runSerenaBackendLossReconcileTicker(ctx context.Context, s *gui.Server, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		case <-s.SerenaBackendLossReconcileTrigger():
		}
		if s == nil {
			continue
		}
		tickCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		_ = s.ReconcileSerenaBackendLossViaIPC(tickCtx)
		cancel()
	}
}

func runLSPSessionCleanupTicker(ctx context.Context, sessions *lsp_routing.SessionRouter, interval, ttl time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			_ = sessions.CleanupWithTTL(now, ttl)
		}
	}
}

// runSerenaIdleShutdownTicker is the v0.6 idle-shutdown (#6, spec §6) driver.
// Every `interval` (60s in production) it calls s.SweepIdleSerenaDaemons, which
// stops every RUNNING serena pool daemon idle longer than the operator-
// configured threshold (daemons.serena_idle_shutdown) by writing
// IntentReasonIdle on the unified supervisor-intent stops sub-block. The sweep
// reads per-daemon LAST-ACTIVITY (recorded by the router on each /serena/mcp
// forward), NOT wall-clock since spawn, so a daemon mid-call or recently-active
// is never idled. It is a cheap no-op when idle-shutdown is "off", the router
// is unwired, or no serena daemon is registered. Owned by the GUI server
// lifecycle; exits when ctx is cancelled. The sweep's IPC status read is
// bounded by a short per-tick context so a slow/unreachable supervisor cannot
// wedge the ticker. s may be nil in tests; the tick is skipped then.
func runSerenaIdleShutdownTicker(ctx context.Context, s *gui.Server, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			if s == nil {
				continue
			}
			tickCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
			_ = s.SweepIdleSerenaDaemons(tickCtx, now)
			cancel()
		}
	}
}

// runWorkspacePruneTicker is the Phase-1 workspace-daemon auto-prune driver.
// Every `interval` (60s in production) it calls s.SweepPruneWorkspaces, which
// auto-removes daemons whose workspace is structurally dead (an ephemeral
// .claude/worktrees/agent-* worktree, or a deleted directory) so the per-
// workspace serena+LSP daemon set stops growing without bound. Gated by
// daemons.auto_prune_workspaces (default on) and skips any workspace mid serena
// call; prune is non-destructive (re-registers on next open). Owned by the GUI
// server lifecycle; exits when ctx is cancelled. s may be nil in tests.
func runWorkspacePruneTicker(ctx context.Context, s *gui.Server, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			if s == nil {
				continue
			}
			tickCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
			_ = s.SweepPruneWorkspaces(tickCtx, now)
			cancel()
		}
	}
}

// runForceDiagnostic implements bare `mcphub gui --force`: probe the
// stuck incumbent and print the diagnostic, returning exit code 2 (or 0
// on a Healthy fall-through to the handshake). The diagnostic block
// already prints the lock-folder path, so opening that folder in the file
// manager is an OPT-IN convenience gated behind --reveal.
//
// ctx is the signal-aware context from RunE so Ctrl+C/SIGTERM
// during Probe (which makes a network call) cancels promptly.
// (Codex iter-10 P2 #1.)
//
// Default (reveal=false) is PRINT-ONLY — that print-only default is the
// durable mitigation for the reveal-window orphan flood (bug
// 2026-06-22-explorer-folder-window-orphan-flood): an empirical probe on
// a SeparateProcess=1 host proved `explorer.exe /select,<path>` HANDS OFF
// — the launched process exits within seconds and the persistent window
// is a DIFFERENT, handed-off PID — so no reliable reaper exists. --reveal
// is the opt-in that accepts ONE un-reapable persistent explorer.exe
// window per invocation; repeated `--force` (no --reveal) leaks nothing.
func runForceDiagnostic(ctx context.Context, cmd *cobra.Command, pidportPath string, reveal bool) int {
	v := gui.Probe(ctx, pidportPath)
	if v.Class == gui.VerdictHealthy {
		// Healthy → fall through to TryActivateIncumbent (legacy
		// handshake). Returning 0 signals the caller to handshake.
		noTarget, err := handleIncumbentActivationResult(
			cmd.OutOrStdout(),
			gui.TryActivateIncumbent(pidportPath, 2*time.Second),
		)
		if err != nil {
			fmt.Fprintf(cmd.OutOrStderr(), "incumbent reported healthy but activate-window failed: %v\n", err)
			return 1
		}
		if !noTarget {
			fmt.Fprintln(cmd.OutOrStdout(), "activated existing mcphub gui")
		}
		return 0
	}
	fmt.Fprintln(cmd.OutOrStdout(), formatDiagnostic(v, pidportPath))
	if reveal {
		_ = gui.OpenFolderAt(pidportPath)
	}
	return 2
}

// runForceKill implements `--force --kill`. Returns
// (acquiredLock, exitCode). On success acquiredLock is non-nil and
// exitCode==0; the caller continues into Phase B.
//
// ctx is the signal-aware context from RunE (from signal.NotifyContext)
// so Ctrl+C/SIGTERM during the kill path is honored — including the
// post-kill wait-for-exit loop and the acquire-poll loop inside
// KillRecordedHolder. cmd.Context() would NOT receive SIGINT.
// (Codex iter-10 P2 #1.)
func runForceKill(ctx context.Context, cmd *cobra.Command, pidportPath string, yes bool) (*gui.SingleInstanceLock, int) {
	// Probe FIRST so the healthy-incumbent early-exit can run without
	// requiring --yes in non-TTY contexts. The original ordering put
	// Gate 0 (non-TTY ⇒ require --yes) before the probe, which broke
	// CI/cron usage of `mcphub gui --force --kill` as a defensive
	// idempotent activate: a healthy incumbent should always route to
	// activate-only (no kill, no destructive consent needed). Codex
	// bot review on PR #23 P1.
	v := gui.Probe(ctx, pidportPath)

	// Gate 1: Healthy early-exit (Codex r5 #7b): never kill a healthy gui.
	if v.Class == gui.VerdictHealthy {
		fmt.Fprintf(cmd.OutOrStdout(), "incumbent is healthy (PID %d); activating instead of killing\n", v.PID)
		_, err := handleIncumbentActivationResult(
			cmd.OutOrStdout(),
			gui.TryActivateIncumbent(pidportPath, 2*time.Second),
		)
		if err != nil {
			fmt.Fprintf(cmd.OutOrStderr(), "activate-window failed: %v\n", err)
			return nil, 1
		}
		return nil, 0
	}

	// Gate 2 (Codex iter-6 P2 #1): only LiveUnreachable is a kill
	// target. Malformed and DeadPID skip the destructive prompt and
	// exit with the documented unrecoverable / already-recovered
	// codes. Without this gate, a corrupt pidport (PID 0) or a dead
	// recorded PID would still ask "Kill PID 0?" before any kill,
	// and "Enter to cancel" would silently exit 0 even though
	// nothing could have been killed.
	//
	// Codex bot review on PR #23 P2 (round 2): this verdict-classification
	// MUST run BEFORE the non-TTY/--yes guard. Otherwise CI/cron
	// callers hit exit 6 even for VerdictMalformed (4) or DeadPID (3)
	// where no kill is attempted; that masks the proper exit codes
	// and forces automation to add --yes for non-destructive paths.
	switch v.Class {
	case gui.VerdictMalformed:
		// Codex iter-8 P2 #2: kill-mode malformed maps to exit 4
		// (pidport unrecoverable) per memo §"Exit codes". Bare
		// --force diagnostic uses exit 2; --force --kill is a
		// distinct contract and CI scripts must distinguish them.
		fmt.Fprintln(cmd.OutOrStdout(), formatDiagnostic(v, pidportPath))
		return nil, 4
	case gui.VerdictDeadPID:
		// Probe says the recorded PID is already gone — the OS
		// should have released the flock as a side effect. Map to
		// exit 3 (race-lost / already-recovered semantic per memo
		// §Exit codes).
		fmt.Fprintln(cmd.OutOrStdout(), formatDiagnostic(v, pidportPath))
		return nil, 3
	case gui.VerdictLiveUnreachable:
		// fall through to identity gate + prompt + KillRecordedHolder
	default:
		fmt.Fprintf(cmd.OutOrStderr(),
			"internal: unexpected verdict class %q from Probe; refusing kill\n",
			v.Class.String())
		return nil, 1
	}

	// Gate 0 (Claude r2 #3): non-TTY without --yes → exit 6.
	// Reached only when verdict == LiveUnreachable — the path that
	// actually attempts a kill. Non-TTY callers that truly want the
	// kill must pass --yes. Healthy / Malformed / DeadPID short-circuit
	// above without consent (no kill happens).
	//
	// Codex bot review on PR #23 P2 (round 3): probe TTY-ness on the
	// SAME stream the prompt reads from (cmd.InOrStdin), not os.Stdin.
	// Otherwise tests / embedded callers that override input via
	// cmd.SetIn(...) get inconsistent behavior — guard skips --yes
	// even though scripted input is non-interactive, then the prompt
	// EOFs and silently exits 0 without performing the recovery.
	if !yes && !inputIsTerminal(cmd.InOrStdin()) {
		fmt.Fprintln(cmd.OutOrStderr(), "non-interactive shell — pass --yes to confirm --kill")
		return nil, 6
	}

	// Print diagnostic so the operator sees what we're about to kill.
	fmt.Fprintln(cmd.OutOrStdout(), formatDiagnostic(v, pidportPath))

	// Codex iter-9 P2 #1: run the identity gate BEFORE the prompt.
	// Without this, the operator could be asked "Kill PID X
	// (mcphub gui)?" for a PID that the gate later refuses (e.g.
	// the recorded PID is `mcphub daemon`, a recycled PID belonging
	// to another process, or macOS-unsupported) — they consent to a
	// kill that never happens, then see the refusal afterward.
	// KillRecordedHolder still re-runs the same gate internally for
	// defense in depth; this pre-prompt invocation guards UX, not
	// safety.
	if refused, reason := gui.CheckIdentityGate(v); refused {
		fmt.Fprintln(cmd.OutOrStderr(), "kill refused:", reason)
		return nil, 7
	}

	// Gate 3: confirmation prompt unless --yes.
	if !yes {
		fmt.Fprintf(cmd.OutOrStdout(), "Kill PID %d (mcphub gui)? [y/N]: ", v.PID)
		// Read in a goroutine + select on ctx.Done so Ctrl+C / SIGTERM
		// during the prompt actually unblocks the wait. cmd.InOrStdin()
		// is the prompt's input stream so callers using cmd.SetIn(...)
		// for scripted input get honored.
		//
		// Sonnet review on PR #23 F2: pipe the read through an
		// io.Pipe whose reader we close on ctx.Done. Closing pr
		// unblocks Fscanln with io.ErrClosedPipe so the consumer
		// goroutine exits cleanly when ctx fires.
		//
		// Codex CLI xhigh round-4 follow-up: the source-side io.Copy
		// goroutine still blocks on the original cmd.InOrStdin (an
		// os.File or buffer with no cancellation primitive that we
		// own). This is a documented residual leak bounded by process
		// lifetime: the CLI is single-shot, so on ctx-cancel the
		// process is exiting anyway and the goroutine is reaped by
		// the OS. The Fscanln consumer side (the actual blocker the
		// prior bug worried about — operator hits Ctrl+C and stays
		// stuck) IS unblocked. A future embedding context (planned
		// A4-b HTTP /api/force-kill) needs a longer-lived solution
		// (close the source-side fd via an *os.File assertion + ctx
		// goroutine) — out of scope for the CLI surface.
		pr, pw := io.Pipe()
		go func() {
			_, _ = io.Copy(pw, cmd.InOrStdin())
			_ = pw.Close()
		}()
		respCh := make(chan string, 1)
		errCh := make(chan error, 1)
		go func() {
			var resp string
			_, err := fmt.Fscanln(pr, &resp)
			if err != nil {
				errCh <- err
				return
			}
			respCh <- resp
		}()
		var resp string
		select {
		case resp = <-respCh:
		case <-errCh:
			// Fscanln error (EOF, bad input). Treat as cancel:
			// the prompt was implicitly declined.
			_ = pr.Close()
			fmt.Fprintln(cmd.OutOrStdout(), "cancelled")
			return nil, 0
		case <-ctx.Done():
			// Unblock the Fscanln goroutine via pipe close.
			_ = pr.Close()
			fmt.Fprintln(cmd.OutOrStderr(), "interrupted")
			return nil, 1
		}
		// Drain pipe reader so the io.Copy goroutine can finish on
		// stdin EOF without blocking on a write to a still-open pipe.
		_ = pr.Close()
		resp = strings.TrimSpace(strings.ToLower(resp))
		if resp != "y" && resp != "yes" {
			fmt.Fprintln(cmd.OutOrStdout(), "cancelled")
			return nil, 0
		}
	}

	// Codex iter-5 P1: pass the identity tuple the cli already saw
	// (and printed/confirmed with the user) into KillRecordedHolder
	// so its internal re-probe refuses with VerdictRaceLost (exit 3)
	// if a competitor rewrote pidport during the prompt window. The
	// guard runs even on --yes because the prompt-skip path still
	// has a sub-second TOCTOU window between this Probe and the one
	// inside KillRecordedHolder.
	lock, killVerdict, err := gui.KillRecordedHolder(ctx, pidportPath, gui.KillOpts{
		Expected: gui.ExpectedIdentity{PID: v.PID, Port: v.Port, Mtime: v.Mtime},
	})
	if killVerdict.Class == gui.VerdictKilledRecovered {
		fmt.Fprintln(cmd.OutOrStdout(), killVerdict.Diagnose)
		return lock, 0
	}
	if killVerdict.Class == gui.VerdictHealthy {
		// Codex PR #23 P2 #2 (iter-2): KillRecordedHolder's internal
		// re-probe found the incumbent healthy after this cli's first
		// Probe (e.g., Server.Start finally bound during the user
		// confirmation prompt above). Honor "never kill healthy"
		// exactly as the early-exit at the top of runForceKill:
		// route to TryActivateIncumbent and exit 0. Handled before
		// the stderr-diagnose preamble below so the success path
		// stays on stdout.
		fmt.Fprintf(cmd.OutOrStdout(), "incumbent recovered to healthy (PID %d); activating instead of killing\n", killVerdict.PID)
		_, err := handleIncumbentActivationResult(
			cmd.OutOrStdout(),
			gui.TryActivateIncumbent(pidportPath, 2*time.Second),
		)
		if err != nil {
			fmt.Fprintf(cmd.OutOrStderr(), "activate-window failed: %v\n", err)
			return nil, 1
		}
		return nil, 0
	}
	// Map class to exit code.
	fmt.Fprintln(cmd.OutOrStderr(), killVerdict.Diagnose)
	if killVerdict.Hint != "" {
		fmt.Fprintln(cmd.OutOrStderr(), "hint:", killVerdict.Hint)
	}
	switch killVerdict.Class {
	case gui.VerdictKillRefused:
		return nil, 7
	case gui.VerdictKillFailed:
		return nil, 4
	case gui.VerdictRaceLost:
		return nil, 3
	case gui.VerdictMalformed:
		return nil, 4
	case gui.VerdictDeadPID:
		// Probe said dead but acquire failed afterward — treat as
		// race-lost because the OS should have released flock when
		// the dead process exited; if we can't acquire, someone
		// else holds it now.
		return nil, 3
	default:
		// Forward-compat safety net: Verdict will grow in the A4-b
		// HTTP API path. If a future class lands without a switch
		// arm, surface the class + err to stderr instead of silently
		// exiting 1 with no diagnostic.
		fmt.Fprintf(cmd.OutOrStderr(), "internal: unrecognized verdict class %q (err=%v)\n", killVerdict.Class.String(), err)
		return nil, 1
	}
}

// formatDiagnostic builds the human-readable diagnostic block from
// a Verdict. Output format matches memo §"Diagnostic format".
func formatDiagnostic(v gui.Verdict, pidportPath string) string {
	var b strings.Builder
	b.WriteString("Cannot acquire mcphub gui single-instance lock.\n\n")
	fmt.Fprintf(&b, "Lock file:  %s.lock\n", pidportPath)
	fmt.Fprintf(&b, "Pidport:    %s\n", pidportPath)
	fmt.Fprintf(&b, "  recorded PID:  %d\n", v.PID)
	fmt.Fprintf(&b, "  recorded port: %d\n", v.Port)
	if !v.Mtime.IsZero() {
		fmt.Fprintf(&b, "  pidport mtime: %s\n", v.Mtime.UTC().Format(time.RFC3339))
	}
	b.WriteString("\nLive-holder probe:\n")
	if v.PIDAlive {
		fmt.Fprintf(&b, "  PID %d status:    alive\n", v.PID)
		if v.PIDImage != "" {
			fmt.Fprintf(&b, "  PID %d image:     %s\n", v.PID, v.PIDImage)
		}
	} else {
		fmt.Fprintf(&b, "  PID %d status:    not alive\n", v.PID)
	}
	if v.PingMatch {
		fmt.Fprintf(&b, "  /api/ping on %d:  ok (PID matches)\n\n", v.Port)
	} else {
		fmt.Fprintf(&b, "  /api/ping on %d:  failed or PID mismatch\n\n", v.Port)
	}
	if v.Diagnose != "" {
		b.WriteString("Verdict: ")
		b.WriteString(v.Class.String())
		b.WriteString("\n")
		b.WriteString("  ")
		b.WriteString(v.Diagnose)
		b.WriteString("\n")
	}
	if v.Hint != "" {
		b.WriteString("Hint: ")
		b.WriteString(v.Hint)
		b.WriteString("\n")
	}
	return b.String()
}

// forceExitError is a typed error that carries an exit code. cmd/mcphub/main.go
// uses errors.As(err, &fe) where fe is the combined
// `interface{ ExitCode() int; IsMcphubForceExit() bool }` to map these
// errors onto os.Exit(code) — without that branch cobra defaults to
// exit 1 on error and the distinct exit codes (2/3/4/6/7) are lost.
type forceExitError struct{ code int }

func (e *forceExitError) Error() string { return fmt.Sprintf("force exit %d", e.code) }
func (e *forceExitError) ExitCode() int { return e.code }

// IsMcphubForceExit is the marker that distinguishes this CLI sentinel
// from os/exec.ExitError (which also satisfies `interface{ ExitCode() int }`).
// cmd/mcphub/main.go must match against this method to avoid silently
// suppressing diagnostic context from wrapped subprocess failures
// (editor in `mcphub manifest edit` / `mcphub secrets edit`, taskkill,
// etc. — see fmt.Errorf("...: %w", err) wrappings in those files).
// Codex iter-5 P2.
func (e *forceExitError) IsMcphubForceExit() bool { return true }

func forceExit(code int) error {
	// Code 0 is a successful outcome (e.g. healthy-incumbent activate
	// short-circuit, normal acquire after take-over). Wrapping it in
	// forceExitError would make cmd.Execute() report a non-nil error,
	// breaking the standard Cobra success contract for in-process
	// callers and emitting spurious usage output. Codex bot review on
	// PR #23 P2 (round 4).
	if code == 0 {
		return nil
	}
	return &forceExitError{code: code}
}

// versionString returns the linker-baked version. Ephemeral placeholder
// for MVP; Phase 3B-II wires build-time ldflags through here.
func versionString() string {
	if v := os.Getenv("MCPHUB_VERSION"); v != "" {
		return v
	}
	return "dev"
}
