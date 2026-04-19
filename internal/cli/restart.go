package cli

import (
	"fmt"

	"mcp-local-hub/internal/api"

	"github.com/spf13/cobra"
)

func newRestartCmdReal() *cobra.Command {
	var server, daemonFilter string
	var all bool
	c := &cobra.Command{
		Use:   "restart",
		Short: "Restart daemon(s): stop + re-run scheduler tasks",
		Long: `Kill the live daemon process (by port) and re-run its scheduler task(s).
Equivalent to 'mcphub stop' + 'schtasks /Run' in one step, with the
kill-before-rerun ordering that a naive 'schtasks /End + /Run' misses
(scheduler /End only ends the task action, not the spawned daemon; the
port stays bound, next /Run silently fails).

Examples:
  mcphub restart --server serena                # restart all daemons for one server
  mcphub restart --server serena --daemon codex # just one daemon
  mcphub restart --all                          # restart every mcp-local-hub-* task
                                                # (skips -weekly-refresh tasks)

When to use:
  - After rebuilding the mcphub binary — pick up the new embedded code
  - After editing a manifest (daemons read manifests at startup, not live)
  - After updating a secret that a daemon consumes via env
  - When 'status' shows a task Stopped and you need it back up

See also: stop, status, scheduler upgrade, setup.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			a := api.NewAPI()
			var (
				results []api.RestartResult
				err     error
			)
			switch {
			case all && server != "":
				return fmt.Errorf("--all is mutually exclusive with --server")
			case all:
				results, err = a.RestartAll()
			case server != "":
				results, err = a.Restart(server, daemonFilter)
			default:
				return cmd.Help()
			}
			if err != nil {
				return err
			}
			anyFail := false
			for _, r := range results {
				if r.Err != "" {
					anyFail = true
					fmt.Fprintf(cmd.OutOrStderr(), "✗ %s: %s\n", r.TaskName, r.Err)
				} else {
					fmt.Fprintf(cmd.OutOrStdout(), "✓ Restarted %s\n", r.TaskName)
				}
			}
			if anyFail {
				return fmt.Errorf("one or more daemons failed to restart")
			}
			return nil
		},
	}
	c.Flags().StringVar(&server, "server", "", "restart only daemons for this server")
	c.Flags().StringVar(&daemonFilter, "daemon", "", "daemon name within the server manifest")
	c.Flags().BoolVar(&all, "all", false, "restart all mcp-local-hub daemons")
	return c
}
