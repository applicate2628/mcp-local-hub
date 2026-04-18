package godbolt

import (
	"github.com/spf13/cobra"
)

// NewCommand returns a cobra.Command that runs the godbolt MCP server
// over stdio. Used by both the standalone cmd/godbolt binary and the
// mcphub subcommand, so the same entry point works in both shapes.
func NewCommand() *cobra.Command {
	return &cobra.Command{
		Use:    "godbolt",
		Short:  "Godbolt Compiler Explorer MCP server (stdio)",
		Hidden: true, // internal transport helper when embedded
		RunE: func(cmd *cobra.Command, args []string) error {
			return Run(cmd.Context())
		},
	}
}
