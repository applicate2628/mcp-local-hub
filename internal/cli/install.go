package cli

import (
	"fmt"

	"mcp-local-hub/internal/api"

	"github.com/spf13/cobra"
)

// newInstallCmdReal is the concrete cobra.Command wired by root.go's stub
// newInstallCmd. It is a thin wrapper over api.Install — all behavior lives
// in internal/api so CLI and future GUI share one code path.
func newInstallCmdReal() *cobra.Command {
	var server string
	var daemonFilter string
	var dryRun bool
	var all bool
	c := &cobra.Command{
		Use:   "install",
		Short: "Install an MCP server as shared daemon(s)",
		RunE: func(cmd *cobra.Command, args []string) error {
			if all {
				if server != "" || daemonFilter != "" {
					return fmt.Errorf("--all is mutually exclusive with --server/--daemon")
				}
				a := api.NewAPI()
				results := a.InstallAll(dryRun, cmd.OutOrStdout())
				for _, r := range results {
					if r.Err != nil {
						fmt.Fprintf(cmd.OutOrStderr(), "\u2717 %s: %v\n", r.Server, r.Err)
					} else {
						fmt.Fprintf(cmd.OutOrStdout(), "\u2713 %s\n", r.Server)
					}
				}
				return nil
			}
			if server == "" {
				return fmt.Errorf("--server is required")
			}
			a := api.NewAPI()
			return a.Install(api.InstallOpts{
				Server:       server,
				DaemonFilter: daemonFilter,
				DryRun:       dryRun,
				Writer:       cmd.OutOrStdout(),
			})
		},
	}
	c.Flags().StringVar(&server, "server", "", "server name (matches servers/<name>/manifest.yaml)")
	c.Flags().StringVar(&daemonFilter, "daemon", "", "install only this daemon (+ its client bindings); omit to install all")
	c.Flags().BoolVar(&dryRun, "dry-run", false, "print planned actions without making changes")
	c.Flags().BoolVar(&all, "all", false, "install every manifest under servers/")
	return c
}

