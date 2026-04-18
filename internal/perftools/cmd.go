package perftools

import (
	"github.com/spf13/cobra"
)

// NewCommand returns a cobra.Command that runs the perf-toolbox MCP
// server over stdio. Used by both the standalone cmd/perftools binary
// and the mcphub subcommand so the same entry point works in either
// shape.
func NewCommand() *cobra.Command {
	return &cobra.Command{
		Use:    "perftools",
		Short:  "Perf-analysis toolbox MCP server (clang-tidy, hyperfine, llvm-objdump, include-what-you-use)",
		Hidden: true, // internal transport helper when embedded under mcphub
		RunE: func(cmd *cobra.Command, args []string) error {
			return Run(cmd.Context())
		},
	}
}
