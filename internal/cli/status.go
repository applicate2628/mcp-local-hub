package cli

import (
	"encoding/json"
	"fmt"

	"mcp-local-hub/internal/api"

	"github.com/spf13/cobra"
)

func newStatusCmdReal() *cobra.Command {
	var jsonOut bool
	c := &cobra.Command{
		Use:   "status",
		Short: "Show state of all mcp-local-hub scheduler tasks",
		Long: `Print a table of every 'mcp-local-hub-*' Task Scheduler task with state,
port, PID, RAM, uptime, and next-run time (for scheduled tasks).

State column:
  Running    — port is bound, daemon process alive
  Starting   — scheduler is mid-launch; port not yet bound
  Scheduled  — task idle, no live daemon, but a future trigger will fire
               (e.g. -weekly-refresh tasks)
  Stopped    — task idle, no future trigger, no daemon. Run 'restart' to revive.
  Disabled   — scheduler marked task as disabled (rare)

Examples:
  mcphub status         # pretty table
  mcphub status --json  # machine-readable

Troubleshooting:
  - All tasks showing Stopped? The mcphub binary may have moved.
    'mcphub setup' + 'mcphub scheduler upgrade' fixes that in one pass.
  - Some tasks Stopped, others Running? Restart the Stopped ones:
    'mcphub restart --server <name>'.

See also: restart, stop, logs, scheduler upgrade.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			a := api.NewAPI()
			rows, err := a.Status()
			if err != nil {
				return err
			}
			if jsonOut {
				enc := json.NewEncoder(cmd.OutOrStdout())
				enc.SetIndent("", "  ")
				return enc.Encode(rows)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%-45s %-10s %-6s %-8s %-8s %-10s %s\n",
				"NAME", "STATE", "PORT", "PID", "RAM(MB)", "UPTIME", "NEXT RUN")
			for _, r := range rows {
				ram := ""
				if r.RAMBytes > 0 {
					ram = fmt.Sprintf("%d", r.RAMBytes/(1024*1024))
				}
				uptime := ""
				if r.UptimeSec > 0 {
					uptime = fmt.Sprintf("%dh%dm", r.UptimeSec/3600, (r.UptimeSec%3600)/60)
				}
				port := ""
				if r.Port > 0 {
					port = fmt.Sprintf("%d", r.Port)
				}
				pid := ""
				if r.PID > 0 {
					pid = fmt.Sprintf("%d", r.PID)
				}
				fmt.Fprintf(cmd.OutOrStdout(), "%-45s %-10s %-6s %-8s %-8s %-10s %s\n",
					r.TaskName, r.State, port, pid, ram, uptime, r.NextRun)
			}
			return nil
		},
	}
	c.Flags().BoolVar(&jsonOut, "json", false, "machine-readable JSON output")
	return c
}
