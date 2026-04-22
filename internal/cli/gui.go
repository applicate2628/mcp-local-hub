// internal/cli/gui.go
package cli

import (
	"errors"
	"fmt"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"mcp-local-hub/internal/gui"

	"github.com/spf13/cobra"
)

func newGuiCmdReal() *cobra.Command {
	var (
		port      int
		noBrowser bool
		noTray    bool
		force     bool
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
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, stop := signal.NotifyContext(cmd.Context(), syscall.SIGINT, syscall.SIGTERM)
			defer stop()

			// Resolve pidport location (test seam: override via env for subprocess tests).
			pidportPath, err := gui.PidportPath()
			if err != nil {
				return err
			}
			if d := os.Getenv("MCPHUB_GUI_TEST_PIDPORT_DIR"); d != "" {
				pidportPath = filepath.Join(d, "gui.pidport")
			}

			// Pre-bind a listener to resolve the actual port (port=0 → OS picks
			// one). We immediately Close it and let gui.Server rebind; the race
			// window is single-digit microseconds on loopback and acceptable
			// for a local-only GUI server.
			listener, err := listenForPort(port)
			if err != nil {
				return err
			}
			boundPort := listener.Addr().(*net.TCPAddr).Port
			_ = listener.Close()

			// Phase A: acquire single-instance lock, falling back to
			// handshake-and-exit if another instance is live.
			lock, err := gui.AcquireSingleInstanceAt(pidportPath, boundPort)
			if err != nil {
				if !errors.Is(err, gui.ErrSingleInstanceBusy) {
					return err
				}
				if force {
					fmt.Fprintln(cmd.OutOrStderr(), "warning: --force not implemented yet; falling back to handshake")
				}
				if err := gui.TryActivateIncumbent(pidportPath, 2*time.Second); err != nil {
					return fmt.Errorf("another mcphub gui is running but unreachable (%v) — use --force to take over", err)
				}
				fmt.Fprintln(cmd.OutOrStdout(), "activated existing mcphub gui")
				return nil
			}
			defer lock.Release()

			// Phase B: start the HTTP server.
			s := gui.NewServer(gui.Config{Port: boundPort, Version: versionString()})
			s.OnActivateWindow(func() {
				// Phase 3B-II will wire tray/browser here. MVP: just log.
				fmt.Fprintln(cmd.OutOrStdout(), "activate-window received")
			})

			ready := make(chan struct{})
			errCh := make(chan error, 1)
			go func() { errCh <- s.Start(ctx, ready) }()
			select {
			case <-ready:
				fmt.Fprintf(cmd.OutOrStdout(), "GUI listening on http://127.0.0.1:%d\n", s.Port())
			case err := <-errCh:
				return err
			case <-ctx.Done():
				return ctx.Err()
			}

			_ = noBrowser
			_ = noTray
			return <-errCh
		},
	}
	c.Flags().IntVar(&port, "port", 0, "TCP port on 127.0.0.1 (0 = auto-pick from ephemeral)")
	c.Flags().BoolVar(&noBrowser, "no-browser", false, "do not auto-launch a browser window")
	c.Flags().BoolVar(&noTray, "no-tray", false, "do not show the system-tray icon")
	c.Flags().BoolVar(&force, "force", false, "take over a stuck single-instance mutex if pidport probe fails")
	return c
}

// listenForPort picks an OS-assigned free port when port==0, or binds
// the requested port. Returns the bound listener so the caller can
// forward the port number to Server without a TOCTOU window wider than
// an immediate Close+reopen.
func listenForPort(port int) (*net.TCPListener, error) {
	addr := &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: port}
	return net.ListenTCP("tcp", addr)
}

// versionString returns the linker-baked version. Ephemeral placeholder
// for MVP; Phase 3B-II wires build-time ldflags through here.
func versionString() string {
	if v := os.Getenv("MCPHUB_VERSION"); v != "" {
		return v
	}
	return "dev"
}
