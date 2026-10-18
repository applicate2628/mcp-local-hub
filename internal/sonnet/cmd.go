package sonnet

import "github.com/spf13/cobra"

// NewCommand returns the hidden native Sonnet MCP stdio command used by the
// hub's stdio-bridge manifest. It accepts no flags.
func NewCommand() *cobra.Command {
	return &cobra.Command{
		Use:    "sonnet",
		Short:  "Sonnet Suites BASIC MCP server (stdio)",
		Hidden: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return Run(cmd.Context())
		},
	}
}
