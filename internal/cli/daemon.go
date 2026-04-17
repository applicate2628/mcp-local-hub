package cli

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"mcp-local-hub/internal/config"
	"mcp-local-hub/internal/daemon"
	"mcp-local-hub/internal/secrets"

	"github.com/spf13/cobra"
)

func newDaemonCmdReal() *cobra.Command {
	var server, daemonName string
	c := &cobra.Command{
		Use:   "daemon",
		Short: "Run a daemon (invoked by scheduler, not by humans)",
		RunE: func(cmd *cobra.Command, args []string) error {
			if server == "" || daemonName == "" {
				return fmt.Errorf("--server and --daemon are required")
			}
			// Resolve the manifest relative to the binary, not CWD —
			// scheduler tasks may launch us with an inherited cwd that
			// is not the repo root. Supports two layouts:
			//   - exe and servers/ at same level (legacy)
			//   - exe in bin/ and servers/ one level up (standard Go layout)
			exe, err := os.Executable()
			if err != nil {
				return fmt.Errorf("resolve executable: %w", err)
			}
			exeDir := filepath.Dir(exe)
			manifestPath := filepath.Join(exeDir, "servers", server, "manifest.yaml")
			if _, statErr := os.Stat(manifestPath); statErr != nil {
				alt := filepath.Join(exeDir, "..", "servers", server, "manifest.yaml")
				if _, statErr2 := os.Stat(alt); statErr2 == nil {
					manifestPath = alt
				}
			}
			f, err := os.Open(manifestPath)
			if err != nil {
				return fmt.Errorf("open manifest %s: %w", manifestPath, err)
			}
			defer f.Close()
			m, err := config.ParseManifest(f)
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
			if m.Transport == config.TransportNativeHTTP {
				childArgs = append(childArgs, "--port", fmt.Sprintf("%d", spec.Port))
				ls := daemon.LaunchSpec{
					Command: m.Command,
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
					Command: m.Command,
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
