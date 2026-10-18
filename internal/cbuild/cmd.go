package cbuild

import "github.com/spf13/cobra"

// NewCommand exposes CBuild as a hidden mcphub stdio subcommand. The product
// owns one executable; this command owns only CBuild's composition boundary.
func NewCommand() *cobra.Command {
	var defaultDir string
	cmd := &cobra.Command{
		Use:    "cbuild",
		Short:  "CMake and vcpkg MCP server (stdio)",
		Hidden: true,
		Args:   cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return Run(cmd.Context(), defaultDir, cmd.InOrStdin(), cmd.OutOrStdout())
		},
	}
	cmd.Flags().StringVarP(&defaultDir, "working-dir", "w", "", "default working directory for tool calls that omit working_dir")
	return cmd
}
