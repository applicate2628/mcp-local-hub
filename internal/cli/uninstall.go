package cli

import (
	"fmt"

	"mcp-local-hub/internal/api"

	"github.com/spf13/cobra"
)

// newUninstallCmdReal is the concrete cobra.Command wired by root.go. It is
// a thin wrapper over api.Uninstall: the api does the work and returns a
// structured report; the CLI renders that report to cmd.OutOrStdout().
func newUninstallCmdReal() *cobra.Command {
	var server string
	c := &cobra.Command{
		Use:   "uninstall",
		Short: "Remove an installed MCP server (scheduler + client bindings)",
		Long: `Reverse of 'install': deletes the scheduler tasks and removes the
server's entry from every managed client config (claude-code, codex-cli,
gemini-cli, antigravity).

What uninstall does:
  1. Reads the manifest to know which tasks + which clients to touch
  2. Deletes each 'mcp-local-hub-<server>-*' Task Scheduler task
  3. Removes the server's entry from each client config
  4. Does NOT delete .bak-mcp-local-hub-* backup files — they remain on disk
  5. Does NOT delete live daemon processes — Task Scheduler's task delete
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
			a := api.NewAPI()
			report, err := a.Uninstall(server)
			if err != nil {
				return err
			}
			for _, name := range report.TasksDeleted {
				cmd.Printf("\u2713 Deleted task: %s\n", name)
			}
			for _, warn := range report.TaskDeleteWarns {
				cmd.Printf("\u26a0 %s\n", warn)
			}
			for _, client := range report.ClientsUpdated {
				cmd.Printf("\u2713 Removed %s from %s\n", report.Server, client)
			}
			for _, warn := range report.ClientWarns {
				cmd.Printf("\u26a0 %s\n", warn)
			}
			cmd.Println("Uninstall complete. Client config backups (.bak-mcp-local-hub-*) remain on disk.")
			return nil
		},
	}
	c.Flags().StringVar(&server, "server", "", "server name")
	return c
}
