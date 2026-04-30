package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"mcp-local-hub/internal/config"
	"mcp-local-hub/internal/daemon"
	"mcp-local-hub/internal/secrets"
	"mcp-local-hub/servers"

	"github.com/spf13/cobra"
)

func newDaemonCmdReal() *cobra.Command {
	var server, daemonName string
	c := &cobra.Command{
		Use:   "daemon",
		Short: "Run a daemon (invoked by scheduler, not by humans)",
		Long: `Run a single mcp-local-hub daemon. This is the actual server process
that Task Scheduler launches per the scheduler task XML's <Exec>/<Command>
and <Arguments> fields. Not intended for interactive use.

Flow:
  1. Reads the server's manifest from the binary's //go:embed servers/
  2. Sets up env per manifest (including secret:KEY dereferencing)
  3. For native-http servers: launches the upstream binary directly
  4. For stdio-bridge servers: spawns child + multiplexes HTTP clients
     onto it via the in-process Go stdio-host
  5. Tees child stdout+stderr to %LOCALAPPDATA%\mcp-local-hub\logs\<s>-<d>.log
  6. Exits non-zero on unexpected child-process death — Task Scheduler's
     RestartOnFailure (3 retries × 1 min) auto-recovers

The scheduler task's XML is created by 'mcphub install'; you should never
need to call this manually.

See also: install, logs, restart, status.`,
		RunE: func(cmd *cobra.Command, args []string) (err error) {
			if server == "" || daemonName == "" {
				return fmt.Errorf("--server and --daemon are required")
			}
			// DM-3: capture launch failure into the daemon log file. Without
			// this wrap, pre-child errors (manifest open/parse, env resolve,
			// host construction, port bind, etc.) only reach mcphub's own
			// stderr — which Task Scheduler does NOT preserve. last_result=1
			// with no log entry leaves the user with no way to diagnose
			// (this happened with serena: never figured out the cause). The
			// wrap appends a timestamped diagnostic line so the cause
			// survives in the standard daemon log path. logPath must be
			// computed BEFORE the manifest load so manifest-open failures
			// also reach the log.
			logPath := filepath.Join(logBaseDir(), server+"-"+daemonName+".log")
			defer func() {
				if err != nil {
					writeLaunchFailure(logPath, server, daemonName, err)
				}
			}()
			// Load the manifest from the embed.FS baked into the binary.
			// This works regardless of where mcphub.exe is installed
			// (canonical ~/.local/bin, dev checkout, anywhere on PATH)
			// — manifests travel with the binary so scheduler tasks
			// don't need to find a servers/ directory on disk.
			f, err := servers.Manifests.Open(server + "/manifest.yaml")
			if err != nil {
				return fmt.Errorf("open embedded manifest %s: %w", server, err)
			}
			defer f.Close()
			var mReader io.Reader = f
			m, err := config.ParseManifest(mReader)
			if err != nil {
				return err
			}
			var spec *config.DaemonSpec
			for i := range m.Daemons {
				if m.Daemons[i].Name == daemonName {
					spec = &m.Daemons[i]
					break
				}
			}
			if spec == nil {
				return fmt.Errorf("no daemon %q in %s manifest", daemonName, server)
			}
			// Resolve env.
			vault, _ := secrets.OpenVault(defaultKeyPath(), defaultVaultPath())
			resolver := secrets.NewResolver(vault, nil) // TODO config.local.yaml in later task
			env, err := resolver.ResolveMap(m.Env)
			if err != nil {
				return err
			}
			// Build launch spec.
			childArgs := append([]string{}, m.BaseArgs...)
			childArgs = append(childArgs, spec.ExtraArgs...)
			// Resolve `command: mcphub` to the running binary's absolute path.
			// Manifests that wrap a hub-internal subcommand (e.g. lldb-bridge)
			// need the exact same exe that carries that subcommand; Go's exec
			// would otherwise use PATH and can find an older install of
			// mcphub whose subcommand set is out of date.
			cmdPath := m.Command
			if m.Command == "mcphub" {
				if self, err := os.Executable(); err == nil {
					cmdPath = self
				}
			}
			if m.Transport == config.TransportNativeHTTP {
				// Native-http path: upstream subprocess listens on an INTERNAL
				// port (spec.Port + api.NativeHTTPInternalPortOffset). mcphub's
				// HTTPHost listens on the external spec.Port and reverse-proxies
				// to upstream while applying the ProtocolBridge transforms
				// (inject synthetic __read_resource__/__list_prompts__/
				// __get_prompt__, rewrite their calls to the matching native
				// MCP methods). Internal ports are chosen by a fixed offset so
				// the mapping is predictable and stable across restarts.
				internalPort := spec.Port + config.NativeHTTPInternalPortOffset
				childArgs = append(childArgs, "--port", fmt.Sprintf("%d", internalPort))
				h, err := daemon.NewHTTPHost(daemon.HTTPHostConfig{
					Command:      cmdPath,
					Args:         childArgs,
					Env:          env,
					UpstreamPort: internalPort,
					LogPath:      logPath,
				})
				if err != nil {
					return fmt.Errorf("NewHTTPHost: %w", err)
				}
				ctx := cmd.Context()
				if err := h.Start(ctx); err != nil {
					return fmt.Errorf("httphost.Start: %w", err)
				}
				srv := &http.Server{
					Addr:              fmt.Sprintf("127.0.0.1:%d", spec.Port),
					Handler:           h.HTTPHandler(),
					ReadHeaderTimeout: 10 * time.Second,
					// WriteTimeout 0: SSE streams are long-lived; handler owns cancellation.
				}
				errCh := make(chan error, 1)
				go func() { errCh <- srv.ListenAndServe() }()
				select {
				case err := <-errCh:
					_ = h.Stop()
					if errors.Is(err, http.ErrServerClosed) {
						return nil
					}
					return fmt.Errorf("http server: %w", err)
				case <-ctx.Done():
					_ = h.Stop()
					shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
					defer cancel()
					_ = srv.Shutdown(shutdownCtx)
					return nil
				case <-h.ChildExited():
					// Upstream server died. Same recovery policy as stdio-bridge:
					// return non-zero → Task Scheduler's RestartOnFailure respawns.
					//
					// Capture the child's ProcessState BEFORE Stop() — exit code,
					// PID, and (on POSIX) signal info are the only diagnostic
					// we get when the subprocess crashed silently with no
					// stderr. Without this the launch-failure log line just
					// said "exited unexpectedly" with no way to tell controlled
					// sys.exit from native crash from parent kill.
					exitMsg := formatChildExit(h.ExitState())
					_ = h.Stop()
					shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
					defer cancel()
					_ = srv.Shutdown(shutdownCtx)
					return fmt.Errorf("native-http upstream exited unexpectedly%s", exitMsg)
				}
			} else if m.Transport == config.TransportStdioBridge {
				// Native Go stdio-host: spawns the inner stdio MCP server as
				// a subprocess and exposes it on HTTP via the in-process host.
				// Replaces the previous npx supergateway wrapper (bridge.go),
				// removing the node/npm dependency from the runtime.
				h, err := daemon.NewStdioHost(daemon.HostConfig{
					Command: cmdPath,
					Args:    childArgs,
					Env:     env,
					LogPath: logPath,
				})
				if err != nil {
					return fmt.Errorf("NewStdioHost: %w", err)
				}
				ctx := cmd.Context()
				if err := h.Start(ctx); err != nil {
					return fmt.Errorf("host.Start: %w", err)
				}
				srv := &http.Server{
					Addr:              fmt.Sprintf("127.0.0.1:%d", spec.Port),
					Handler:           h.HTTPHandler(),
					ReadHeaderTimeout: 10 * time.Second,
					ReadTimeout:       30 * time.Second,
					// WriteTimeout intentionally 0 (unlimited): SSE streams are long-lived;
					// writes are bounded per-line via the handler's own select/context,
					// not by the server socket.
				}
				errCh := make(chan error, 1)
				go func() { errCh <- srv.ListenAndServe() }()
				select {
				case err := <-errCh:
					_ = h.Stop()
					if errors.Is(err, http.ErrServerClosed) {
						return nil
					}
					return fmt.Errorf("http server: %w", err)
				case <-ctx.Done():
					// Stop() first so handleSSE and handlePOST goroutines observe h.done
					// and return; then Shutdown can complete without waiting on long-lived SSE.
					_ = h.Stop()
					shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
					defer cancel()
					_ = srv.Shutdown(shutdownCtx)
					return nil
				case <-h.ChildExited():
					// Stdio child died unexpectedly (npx/uvx servers like memory,
					// sequential-thinking, time are known to exit silently after
					// serving N requests). Surface this to the scheduler by
					// returning a non-nil error from RunE so mcphub exits
					// non-zero; Windows Task Scheduler's RestartOnFailure policy
					// (3 retries, 1 minute apart — configured in install.go and
					// scheduler_windows.go) will re-launch the task, which
					// respawns the child. Scheduler owns the retry budget; we
					// do not add in-process respawn logic here.
					_ = h.Stop()
					shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
					defer cancel()
					_ = srv.Shutdown(shutdownCtx)
					return fmt.Errorf("stdio child exited unexpectedly")
				}
			} else {
				return fmt.Errorf("unsupported transport %q", m.Transport)
			}
		},
	}
	c.Flags().StringVar(&server, "server", "", "server name")
	c.Flags().StringVar(&daemonName, "daemon", "", "daemon name within the server manifest")
	// Phase 3 workspace-scoped: lazy proxy subcommand the scheduler task
	// launches per (workspace, language). Hidden — users don't run it.
	c.AddCommand(newDaemonWorkspaceProxyCmd())
	return c
}

// logBaseDir returns the per-OS directory for daemon logs.
// Windows: %LOCALAPPDATA%\mcp-local-hub\logs
// Linux/macOS: $XDG_STATE_HOME/mcp-local-hub/logs (or ~/.local/state/mcp-local-hub/logs)
func logBaseDir() string {
	if v := os.Getenv("LOCALAPPDATA"); v != "" {
		return filepath.Join(v, "mcp-local-hub", "logs")
	}
	if v := os.Getenv("XDG_STATE_HOME"); v != "" {
		return filepath.Join(v, "mcp-local-hub", "logs")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".local", "state", "mcp-local-hub", "logs")
}

// formatChildExit renders the child's ProcessState as a diagnostic
// suffix appended to "native-http upstream exited unexpectedly". The
// goal is to distinguish silent-exit failure modes that the log file
// alone cannot — Codex CLI consult, 2026-04-30:
//
//	exit code 0   — controlled sys.exit(0); upstream thinks it shut down
//	                cleanly. Hints at swallowed exception or misconfig.
//	exit code 1   — generic error path; matches Python's `sys.exit(1)`.
//	exit code 137 (POSIX) — SIGKILL from parent or OOM.
//	exit code -1 / 0xC0000005 / 0xC000013A — Windows native crash or
//	                CTRL_BREAK from parent.
//
// Returns "" when state is nil (process never spawned, still running,
// or Wait() not yet called) so the caller's Errorf format stays clean.
func formatChildExit(state *os.ProcessState) string {
	if state == nil {
		return ""
	}
	return fmt.Sprintf(" (pid=%d exit_code=%d)", state.Pid(), state.ExitCode())
}

// writeLaunchFailure appends a timestamped failure diagnostic to the
// daemon's log file so Task Scheduler's last_result=1 isn't a black hole.
// Failures to open or write the diagnostic are silently dropped — we
// don't want this wrapper to compound the original launch error or to
// fail the deferred path with a panic. The line format is grep-able:
// `[mcphub-launch-failure <RFC3339-UTC> server=<s> daemon=<d>] <err>`.
func writeLaunchFailure(logPath, server, daemonName string, launchErr error) {
	if mkErr := os.MkdirAll(filepath.Dir(logPath), 0700); mkErr != nil {
		return
	}
	f, openErr := os.OpenFile(logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0600)
	if openErr != nil {
		return
	}
	defer f.Close()
	fmt.Fprintf(f, "\n[mcphub-launch-failure %s server=%s daemon=%s] %v\n",
		time.Now().UTC().Format(time.RFC3339), server, daemonName, launchErr)
}
