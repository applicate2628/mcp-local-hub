package cli

import (
	"fmt"

	"mcp-local-hub/internal/api"
	"mcp-local-hub/internal/scheduler"

	"github.com/spf13/cobra"
)

func newRestartCmdReal() *cobra.Command {
	var server string
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
  mcphub restart --server serena    # restart all daemons for one server
  mcphub restart --all              # restart every mcp-local-hub-* task
                                    # (skips -weekly-refresh tasks)

When to use:
  - After rebuilding the mcphub binary — pick up the new embedded code
  - After editing a manifest (daemons read manifests at startup, not live)
  - After updating a secret that a daemon consumes via env
  - When 'status' shows a task Stopped and you need it back up

See also: stop, status, scheduler upgrade, setup.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if all {
				if server != "" {
					return fmt.Errorf("--all is mutually exclusive with --server")
				}
				a := api.NewAPI()
				results, err := a.RestartAll()
				if err != nil {
					return err
				}
				for _, r := range results {
					if r.Err != "" {
						fmt.Fprintf(cmd.OutOrStderr(), "✗ %s: %s\n", r.TaskName, r.Err)
					} else {
						fmt.Fprintf(cmd.OutOrStdout(), "✓ Restarted %s\n", r.TaskName)
					}
				}
				return nil
			}
			// existing --server path
			sch, err := scheduler.New()
			if err != nil {
				return err
			}
			prefix := "mcp-local-hub-"
			if server != "" {
				prefix = "mcp-local-hub-" + server
			}
			if !all && server == "" {
				return cmd.Help()
			}
			tasks, err := sch.List(prefix)
			if err != nil {
				return err
			}
			for _, t := range tasks {
				// Skip weekly-refresh tasks when scope is --all (they trigger themselves).
				if all && (containsSuffix(t.Name, "-weekly-refresh")) {
					continue
				}
				_ = sch.Stop(t.Name)
				if err := sch.Run(t.Name); err != nil {
					cmd.Printf("⚠ run %s: %v\n", t.Name, err)
					continue
				}
				cmd.Printf("✓ Restarted %s\n", t.Name)
			}
			return nil
		},
	}
	c.Flags().StringVar(&server, "server", "", "restart only daemons for this server")
	c.Flags().BoolVar(&all, "all", false, "restart all mcp-local-hub daemons")
	return c
}

func containsSuffix(s, suffix string) bool {
	return len(s) >= len(suffix) && s[len(s)-len(suffix):] == suffix
}
