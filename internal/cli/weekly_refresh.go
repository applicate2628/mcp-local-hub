// Package cli — workspace-weekly-refresh subcommand (Phase 3, M5 Task 17).
//
// This is the CLI counterpart to api.WeeklyRefreshAll. It is invoked by the
// shared scheduler task (api.WeeklyRefreshTaskName) created at register
// time: once a week, Task Scheduler fires `mcphub workspace-weekly-refresh`
// which calls WeeklyRefreshAll() and restarts every per-(workspace,
// language) scheduler task whose WeeklyRefresh flag is true.
//
// Hidden (not exposed in --help) so it does not pollute the default CLI
// surface — only operators debugging the refresh path or the scheduler
// task itself invoke it.
package cli

import (
	"encoding/json"
	"fmt"

	"mcp-local-hub/internal/api"

	"github.com/spf13/cobra"
)

// newWeeklyRefreshCmdReal wires the hidden `workspace-weekly-refresh`
// subcommand. Flags: --json for machine output. Delegates to
// api.WeeklyRefreshAll; prints the restart count + any per-entry warnings.
func newWeeklyRefreshCmdReal() *cobra.Command {
	var jsonOut bool
	c := &cobra.Command{
		Use:   "workspace-weekly-refresh",
		Short: "Restart every weekly-refresh-enabled lazy proxy (scheduler counterpart)",
		Long: `Internal subcommand invoked by the shared scheduler task
'mcp-local-hub-workspace-weekly-refresh'. Reads the workspace registry
and triggers a scheduler Run for every entry with weekly_refresh=true.

The proxy's own startup stamps Lifecycle=Configured; the next tools/call
lazily re-materializes the heavy backend. This command never blocks on
backend materialization — it returns as soon as the scheduler has
accepted every Run request.

Human invocation is supported for diagnostics; operators typically let
the weekly trigger fire the command on its own schedule.

Examples:
  mcphub workspace-weekly-refresh         # pretty report
  mcphub workspace-weekly-refresh --json  # machine-readable
`,
		Hidden: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			a := api.NewAPI()
			report, err := a.WeeklyRefreshAll()
			if err != nil {
				return err
			}
			if jsonOut {
				enc := json.NewEncoder(cmd.OutOrStdout())
				enc.SetIndent("", "  ")
				return enc.Encode(report)
			}
			fmt.Fprintf(cmd.OutOrStdout(),
				"Weekly refresh: restarted %d task(s)\n", len(report.Restarted))
			for _, t := range report.Restarted {
				fmt.Fprintf(cmd.OutOrStdout(), "  restarted: %s\n", t)
			}
			for _, w := range report.Warnings {
				fmt.Fprintf(cmd.OutOrStderr(), "warning: %s\n", w)
			}
			return nil
		},
	}
	c.Flags().BoolVar(&jsonOut, "json", false, "machine-readable JSON output")
	return c
}
