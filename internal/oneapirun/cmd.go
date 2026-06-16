package oneapirun

import (
	"github.com/spf13/cobra"
)

// NewCommand returns a cobra.Command that runs the oneapi-run MCP server
// over stdio. Used by both a possible standalone cmd/oneapi-run binary and
// the mcphub subcommand, so the same entry point works in both shapes.
//
// The command is wired into internal/cli/root.go in a later serialized
// step; this package only owns the command and the server it runs.
func NewCommand() *cobra.Command {
	return &cobra.Command{
		Use:    "oneapi-run",
		Short:  "Run commands under the VS + Intel oneAPI environment (stdio MCP server)",
		Hidden: true, // internal transport helper when embedded
		RunE: func(cmd *cobra.Command, args []string) error {
			return Run(cmd.Context())
		},
	}
}
