// internal/cli/gui.go
package cli

import (
	"fmt"
	"os/signal"
	"syscall"

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

			s := gui.NewServer(gui.Config{Port: port})
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

			// TODO (Task 4+): tray + browser launch + single-instance.
			// For now, just block until the server returns.
			_ = noBrowser
			_ = noTray
			_ = force
			return <-errCh
		},
	}
	c.Flags().IntVar(&port, "port", 0, "TCP port on 127.0.0.1 (0 = auto-pick from ephemeral)")
	c.Flags().BoolVar(&noBrowser, "no-browser", false, "do not auto-launch a browser window")
	c.Flags().BoolVar(&noTray, "no-tray", false, "do not show the system-tray icon")
	c.Flags().BoolVar(&force, "force", false, "take over a stuck single-instance mutex if pidport probe fails")
	return c
}
