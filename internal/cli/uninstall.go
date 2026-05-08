package cli

import (
	"fmt"

	"mcp-local-hub/internal/api"

	"github.com/spf13/cobra"
)

// newUninstallCmdReal is the concrete cobra.Command wired by root.go. It is
// a thin wrapper over api.Uninstall: the api does the work and returns a
// structured report; the CLI renders that report to cmd.OutOrStdout().
//
// Plan v13 Task 11.1 ordering:
//
//  1. runUninstallWatchdog FIRST — disable/delete the watchdog
//     scheduled task so it does not race per-server uninstall mid-
//     teardown. Also removes the EventLog source per §60 (idempotent
//     / non-fatal).
//  2. api.Uninstall(server) — existing per-server flow. Already
//     marks intents + emits server-uninstalled audit entries via
//     recordUninstallIntentForTasks (Task 10).
//  3. Render the per-server report to stdout.
func newUninstallCmdReal() *cobra.Command {
	var server string
	c := &cobra.Command{
		Use:   "uninstall",
		Short: "Remove an installed MCP server (scheduler + client bindings)",
		Long: `Reverse of 'install': deletes the scheduler tasks and removes the
server's entry from every managed client config declared by the manifest.

What uninstall does:
  1. Disables the watchdog scheduled task FIRST (plan Task 11.1) so it does
     not race the per-server teardown.
  2. Reads the manifest to know which tasks + which clients to touch.
  3. Marks each 'mcp-local-hub-<server>-*' task as Desired=stopped +
     Reason=uninstalled (plan Task 10) BEFORE deleting.
  4. Deletes each 'mcp-local-hub-<server>-*' Task Scheduler task.
  5. Removes the server's entry from each client config.
  6. Removes the Windows EventLog source 'mcp-local-hub' (plan §60;
     POSIX no-op).
  7. Does NOT delete .bak-mcp-local-hub-* backup files — they remain on disk.
  8. Does NOT delete live daemon processes — Task Scheduler's task delete
     only removes task metadata; existing processes keep running until they
     exit naturally. Use 'mcphub stop --server <n>' first to kill them.

Examples:
  mcphub uninstall --server wolfram

Recovery:
  'mcphub rollback' restores the latest client config backup
  'mcphub rollback --original' restores the pristine pre-hub-ever sentinel

See also: install, stop, rollback, backups list.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if server == "" {
				return fmt.Errorf("--server is required")
			}
			// Plan Task 11.1 step 1: watchdog teardown FIRST.
			//
			// Codex bot P2: pass the target server name so the helper
			// can gate watchdog + EventLog removal on "no remaining
			// managed servers AFTER this uninstall". Removing one
			// server when multiple are installed must not silently
			// strip the global watchdog from peer servers.
			if err := runUninstallWatchdog(cmd.OutOrStdout(), server); err != nil {
				return err
			}
			// Step 2: per-server uninstall (existing flow).
			a := api.NewAPI()
			report, err := a.Uninstall(server)
			if err != nil {
				return err
			}
			for _, name := range report.TasksDeleted {
				cmd.Printf("✓ Deleted task: %s\n", name)
			}
			for _, warn := range report.TaskDeleteWarns {
				cmd.Printf("⚠ %s\n", warn)
			}
			for _, client := range report.ClientsUpdated {
				cmd.Printf("✓ Removed %s from %s\n", report.Server, client)
			}
			for _, warn := range report.ClientWarns {
				cmd.Printf("⚠ %s\n", warn)
			}
			cmd.Println("Uninstall complete. Client config backups (.bak-mcp-local-hub-*) remain on disk.")
			return nil
		},
	}
	c.Flags().StringVar(&server, "server", "", "server name")
	return c
}
