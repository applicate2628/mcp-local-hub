package cli

import (
	"fmt"

	"mcp-local-hub/internal/api"

	"github.com/spf13/cobra"
)

func newSchedulerCmdReal() *cobra.Command {
	root := &cobra.Command{
		Use:   "scheduler",
		Short: "Scheduler-level operations (upgrade tasks, manage weekly refresh)",
	}
	root.AddCommand(newSchedulerUpgradeCmd())
	return root
}

func newSchedulerUpgradeCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "upgrade",
		Short: "Regenerate scheduler tasks with the current binary path (after move/rename)",
		RunE: func(cmd *cobra.Command, args []string) error {
			a := api.NewAPI()
			results, err := a.SchedulerUpgrade()
			if err != nil {
				return err
			}
			for _, r := range results {
				if r.Err != "" {
					fmt.Fprintf(cmd.OutOrStderr(), "✗ %s: %s\n", r.TaskName, r.Err)
				} else {
					fmt.Fprintf(cmd.OutOrStdout(), "✓ Upgraded %s → %s\n", r.TaskName, r.NewCmd)
				}
			}
			return nil
		},
	}
}
