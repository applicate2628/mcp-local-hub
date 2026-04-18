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
		RunE: func(cmd *cobra.Command, args []string) error {
			if server == "" || daemonName == "" {
				return fmt.Errorf("--server and --daemon are required")
			}
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
			logPath := filepath.Join(logBaseDir(), server+"-"+daemonName+".log")
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
				childArgs = append(childArgs, "--port", fmt.Sprintf("%d", spec.Port))
				ls := daemon.LaunchSpec{
					Command: cmdPath,
					Args:    childArgs,
					Env:     env,
					LogPath: logPath,
				}
				code, err := daemon.Launch(ls)
				if err != nil {
					return err
				}
				os.Exit(code)
			} else if m.Transport == config.TransportStdioBridge {
				// Native Go stdio-host: spawns the inner stdio MCP server as
				// a subprocess and exposes it on HTTP via the in-process host.
				// Replaces the previous npx supergateway wrapper (bridge.go),
				// removing the node/npm dependency from the runtime.
				h, err := daemon.NewStdioHost(daemon.HostConfig{
					Command: cmdPath,
					Args:    childArgs,
					Env:     env,
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
			return nil
		},
	}
	c.Flags().StringVar(&server, "server", "", "server name")
	c.Flags().StringVar(&daemonName, "daemon", "", "daemon name within the server manifest")
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
