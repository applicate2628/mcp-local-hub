package cli

import (
	"fmt"

	"mcp-local-hub/internal/api"

	"github.com/spf13/cobra"
)

func newStopCmdReal() *cobra.Command {
	var server, daemonFilter string
	var all bool
	c := &cobra.Command{
		Use:   "stop",
		Short: "Stop daemon(s) without uninstalling (registration and configs remain)",
		Long: `Stop the selected daemons. Supervisor-owned daemons are stopped through
the supervisor first; anything left over falls through to the legacy
scheduler-task path (kill by port, then /End the task).

Registration, client-config entries, and backups are untouched — you can
'restart' the same daemon back up later, or 'uninstall' to remove it
entirely.

Examples:
  mcphub stop --server serena                # all serena daemons
  mcphub stop --server serena --daemon codex # one daemon
  mcphub stop --all                          # every mcp-local-hub-* daemon

When to use:
  - Before 'mcphub setup' if the canonical binary is in use
  - Before a rebuild, to release the exe file lock on Windows
  - Temporarily disabling a server without uninstalling

See also: restart, uninstall, status.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if all {
				if server != "" {
					return fmt.Errorf("--all is mutually exclusive with --server")
				}
				a := api.NewAPI()
				results, err := a.StopAll()
				if err != nil {
					return err
				}
				anyFail := false
				for _, r := range results {
					if r.Err != "" {
						anyFail = true
						fmt.Fprintf(cmd.OutOrStderr(), "✗ %s: %s\n", r.TaskName, r.Err)
					} else {
						fmt.Fprintf(cmd.OutOrStdout(), "✓ Stopped %s\n", r.TaskName)
					}
				}
				if anyFail {
					return fmt.Errorf("one or more daemons failed to stop")
				}
				return nil
			}
			if server == "" {
				return fmt.Errorf("--server or --all is required")
			}
			a := api.NewAPI()
			results, err := a.Stop(server, daemonFilter)
			if err != nil {
				return err
			}
			anyFail := false
			for _, r := range results {
				if r.Err != "" {
					anyFail = true
					fmt.Fprintf(cmd.OutOrStderr(), "✗ %s: %s\n", r.TaskName, r.Err)
				} else {
					fmt.Fprintf(cmd.OutOrStdout(), "✓ Stopped %s\n", r.TaskName)
				}
			}
			if anyFail {
				return fmt.Errorf("one or more daemons failed to stop")
			}
			return nil
		},
	}
	c.Flags().StringVar(&server, "server", "", "server name")
	c.Flags().StringVar(&daemonFilter, "daemon", "", "daemon name within the server manifest")
	c.Flags().BoolVar(&all, "all", false, "stop every running daemon")
	return c
}
