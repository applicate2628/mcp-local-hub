package vtune

import (
	"github.com/spf13/cobra"
)

// NewCommand returns a cobra.Command that runs the Intel VTune Profiler MCP
// server over stdio. Used by both a potential standalone binary and the
// mcphub subcommand, so the same entry point works in either shape. Mirrors
// the drmemory / oneapi-run / godbolt NewCommand pattern.
func NewCommand() *cobra.Command {
	return &cobra.Command{
		Use:    "vtune",
		Short:  "Intel VTune profiler MCP server (stdio)",
		Hidden: true, // internal transport helper when embedded under mcphub
		RunE: func(cmd *cobra.Command, args []string) error {
			return Run(cmd.Context())
		},
	}
}
