package gdb

import (
	"github.com/spf13/cobra"
)

// NewCommand returns a cobra.Command that runs the native gdb MCP server over
// stdio (the `mcphub gdb-bridge` subcommand). It mirrors the
// drmemory/godbolt/perftools NewCommand pattern: a thin RunE that delegates to
// Run, which owns all runtime behavior.
//
// Unlike internal/lldb (which needs platform-specific applyNoWindow helpers and
// thus a cmd_windows.go / cmd_other.go split), this command needs no platform
// split: the only console-suppression site is the gdb spawn in session.go, which
// uses the shared cross-platform process.NoConsole helper.
func NewCommand() *cobra.Command {
	return &cobra.Command{
		Use:    "gdb-bridge",
		Short:  "Native GDB (GDB/MI) MCP server (stdio) — replaces the external GDB-MCP python server",
		Hidden: true, // internal transport helper when embedded under mcphub
		RunE: func(cmd *cobra.Command, args []string) error {
			return Run(cmd.Context())
		},
	}
}
