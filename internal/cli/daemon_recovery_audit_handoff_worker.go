package cli

import (
	"github.com/spf13/cobra"

	"mcp-local-hub/internal/daemonrecovery"
)

func newDaemonRecoveryAuditHandoffWorkerCmd() *cobra.Command {
	return &cobra.Command{
		Use:           daemonrecovery.CommittedAuditHandoffWorkerCommand,
		Hidden:        true,
		Args:          cobra.NoArgs,
		SilenceErrors: true,
		SilenceUsage:  true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return daemonrecovery.RunCommittedAuditHandoffWorker(cmd.InOrStdin(), cmd.OutOrStdout())
		},
	}
}
