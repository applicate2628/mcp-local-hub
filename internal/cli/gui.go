// internal/cli/gui.go
package cli

import (
	"errors"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"mcp-local-hub/internal/gui"
	"mcp-local-hub/internal/tray"

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

			// Phase A: acquire the single-instance lock BEFORE binding any
			// port. If we bind first and the requested --port is already in
			// use (e.g. because the incumbent GUI owns it), ListenTCP fails
			// with "address already in use" and we never reach the
			// handshake path that would activate the incumbent. The
			// pidport file initially records the requested port (which may
			// be 0 = auto); once the server actually binds, we rewrite it
			// with the resolved port so second-instance handshake probes
			// reach the right place.
			lock, err := gui.AcquireSingleInstanceAt(pidportPath, port)
			if err != nil {
				if !errors.Is(err, gui.ErrSingleInstanceBusy) {
					return err
				}
				if force {
					fmt.Fprintln(cmd.OutOrStderr(), "warning: --force not implemented yet; falling back to handshake")
				}
				if err := gui.TryActivateIncumbent(pidportPath, 2*time.Second); err != nil {
					return fmt.Errorf(
						"another mcphub gui is running but unreachable (%v); "+
							"if the incumbent process is gone, remove %s.lock and retry",
						err, pidportPath)
				}
				fmt.Fprintln(cmd.OutOrStdout(), "activated existing mcphub gui")
				return nil
			}
			defer lock.Release()

			// Phase B: start the HTTP server. Server.Start binds 127.0.0.1
			// on the configured port (0 = OS-assigned) and signals ready
			// once the listener is live.
			s := gui.NewServer(gui.Config{Port: port, Version: versionString()})
			s.OnActivateWindow(func() {
				// Phase 3B-II C2: bring the Chrome app-mode window to
				// foreground via Win32 SetForegroundWindow. Match by
				// page <title> ("mcp-local-hub"), which is stable
				// across Chrome versions in app-mode (chromeless
				// window keeps page title as window title). On
				// non-Windows, FocusBrowserWindow returns an error
				// (logged below); the tray "Open dashboard" action
				// shares the same surface and the same limitation.
				if err := gui.FocusBrowserWindow("mcp-local-hub"); err != nil {
					fmt.Fprintf(cmd.OutOrStderr(), "activate-window: %v\n", err)
				}
			})

			ready := make(chan struct{})
			errCh := make(chan error, 1)
			go func() { errCh <- s.Start(ctx, ready) }()

			// Poll daemon status every 5s and push daemon-state events onto /api/events.
			poller := gui.NewStatusPoller(gui.RealStatusProvider{}, s.Broadcaster(), 5*time.Second)
			go poller.Run(ctx)

			select {
			case <-ready:
				// Now we know the actual bound port. If the user passed
				// --port 0, rewrite the pidport file so the
				// second-instance handshake hits the right place. The
				// flock on *.lock still gates ownership; the pidport file
				// is ownership metadata the lock holder freely updates.
				actualPort := s.Port()
				if actualPort != port {
					if err := gui.RewritePidportPort(pidportPath, actualPort); err != nil {
						fmt.Fprintf(cmd.OutOrStderr(), "warning: pidport rewrite: %v\n", err)
					}
				}
				fmt.Fprintf(cmd.OutOrStdout(), "GUI listening on http://127.0.0.1:%d\n", actualPort)
			case err := <-errCh:
				return err
			case <-ctx.Done():
				return ctx.Err()
			}

			if !noBrowser {
				url := fmt.Sprintf("http://127.0.0.1:%d/", s.Port())
				if err := gui.LaunchBrowser(url); err != nil {
					fmt.Fprintf(cmd.OutOrStderr(), "warning: could not auto-launch browser: %v\n", err)
				}
			}
			if !noTray {
				go func() {
					_ = tray.Run(ctx, tray.Config{
						ActivateWindow: func() {
							// In-process handshake: hit our own activate handler to
							// trigger whatever OnActivateWindow callback is registered
							// (Phase 3B-II: focus browser window).
							_ = gui.TryActivateIncumbent(pidportPath, 500*time.Millisecond)
						},
						Quit: stop, // signal.NotifyContext's cancel function
					})
				}()
			}
			return <-errCh
		},
	}
	c.Flags().IntVar(&port, "port", 0, "TCP port on 127.0.0.1 (0 = auto-pick from ephemeral)")
	c.Flags().BoolVar(&noBrowser, "no-browser", false, "do not auto-launch a browser window")
	c.Flags().BoolVar(&noTray, "no-tray", false, "do not show the system-tray icon")
	c.Flags().BoolVar(&force, "force", false, "take over a stuck single-instance mutex if pidport probe fails")
	// --force is a Phase 3B-II placeholder: today it only prints a warning and still falls into
	// the standard handshake path. Hide it from --help so users don't expect the takeover behavior.
	_ = c.Flags().MarkHidden("force")
	return c
}

// versionString returns the linker-baked version. Ephemeral placeholder
// for MVP; Phase 3B-II wires build-time ldflags through here.
func versionString() string {
	if v := os.Getenv("MCPHUB_VERSION"); v != "" {
		return v
	}
	return "dev"
}
