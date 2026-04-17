package cli

import (
	"fmt"

	"mcp-local-hub/internal/api"
	"mcp-local-hub/internal/clients"

	"github.com/spf13/cobra"
)

// newInstallCmdReal is the concrete cobra.Command wired by root.go's stub
// newInstallCmd. It is a thin wrapper over api.Install — all behavior lives
// in internal/api so CLI and future GUI share one code path.
func newInstallCmdReal() *cobra.Command {
	var server string
	var daemonFilter string
	var dryRun bool
	c := &cobra.Command{
		Use:   "install",
		Short: "Install an MCP server as shared daemon(s)",
		RunE: func(cmd *cobra.Command, args []string) error {
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
	return c
}

// mustAllClients builds the map of {client-name -> Client}. Used by cli
// commands that have not yet migrated to the api package (rollback,
// uninstall wrapper pre-refactor). Kept in cli so those commands compile;
// api owns its own private copy.
func mustAllClients() map[string]clients.Client {
	result := map[string]clients.Client{}
	for _, factory := range []func() (clients.Client, error){
		clients.NewClaudeCode, clients.NewCodexCLI, clients.NewGeminiCLI, clients.NewAntigravity,
	} {
		c, err := factory()
		if err != nil {
			continue
		}
		result[c.Name()] = c
	}
	return result
}
