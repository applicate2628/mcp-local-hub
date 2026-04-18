package lldb

import (
	"github.com/spf13/cobra"
)

// NewCommand returns a cobra.Command that runs the LLDB stdio↔TCP bridge.
// Used by both the standalone cmd/lldb-bridge binary and the mcphub
// `lldb-bridge` subcommand — same cobra.Command shape in both shapes, no
// duplicated flag plumbing or help text.
func NewCommand() *cobra.Command {
	var lldbPath string
	c := &cobra.Command{
		Use:    "lldb-bridge <host:port>",
		Short:  "Stdio↔TCP bridge for LLDB's built-in MCP server (with auto-spawn)",
		Hidden: true, // internal transport helper, not a user-facing verb
		Args:   cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			host, port, err := parseHostPort(args[0])
			if err != nil {
				return err
			}
			return runLldbBridge(host, port, lldbPath)
		},
	}
	c.Flags().StringVar(&lldbPath, "lldb-path", defaultLldbPath(),
		"path to lldb executable (used only if the port is not already listening)")
	return c
}
