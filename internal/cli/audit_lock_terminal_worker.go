package cli

import (
	"github.com/spf13/cobra"

	"mcp-local-hub/internal/gui"
)

// newAuditLockTerminalWorkerCmd is process-internal only. The GUI launches the
// current executable directly and sends its bounded protocol over stdin.
func newAuditLockTerminalWorkerCmd() *cobra.Command {
	return &cobra.Command{
		Use:           "audit-lock-terminal-worker",
		Hidden:        true,
		Args:          cobra.NoArgs,
		SilenceErrors: true,
		SilenceUsage:  true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return gui.RunAuditLockTerminalWorker(cmd.InOrStdin(), cmd.OutOrStdout())
		},
	}
}
