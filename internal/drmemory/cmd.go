package drmemory

import (
	"github.com/spf13/cobra"
)

// NewCommand returns a cobra.Command that runs the Dr. Memory MCP server
// over stdio. Used by both a potential standalone binary and the mcphub
// subcommand, so the same entry point works in either shape. Mirrors the
// godbolt / perftools NewCommand pattern.
func NewCommand() *cobra.Command {
	return &cobra.Command{
		Use:    "drmemory",
		Short:  "Dr. Memory memory-error MCP server (stdio)",
		Hidden: true, // internal transport helper when embedded under mcphub
		RunE: func(cmd *cobra.Command, args []string) error {
			return Run(cmd.Context())
		},
	}
}
