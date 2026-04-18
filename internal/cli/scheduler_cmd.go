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
		Long: `Manage Windows Task Scheduler task metadata directly, without
reinstalling servers.

Subcommands:
  scheduler upgrade                  # rewrite every task's <Command> to
                                     # the current canonical mcphub path.
                                     # Fixes tasks left by an old install
                                     # after 'mcphub setup' moved the binary.
  scheduler weekly-refresh set <DAY>  # install a hub-wide weekly task that
                  <HH:MM>              # runs 'mcphub restart --all' on the
                                       # given schedule (3-letter day, 24h time)
  scheduler weekly-refresh disable    # remove the hub-wide weekly task

See also: install, setup, restart.`,
	}
	root.AddCommand(newSchedulerUpgradeCmd())
	root.AddCommand(newSchedulerWeeklyRefreshCmd())
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

func newSchedulerWeeklyRefreshCmd() *cobra.Command {
	var setFlag string
	var disableFlag bool
	c := &cobra.Command{
		Use:   "weekly-refresh",
		Short: "Configure the hub-wide weekly-refresh task",
		Long: `Manages a single scheduler task that runs 'mcphub restart --all' weekly.
Pass --set "SUN 03:00" to enable, --disable to remove.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			a := api.NewAPI()
			if disableFlag {
				if err := a.WeeklyRefreshDisable(); err != nil {
					return err
				}
				fmt.Fprintln(cmd.OutOrStdout(), "✓ Disabled weekly refresh")
				return nil
			}
			if setFlag == "" {
				return fmt.Errorf("--set or --disable is required")
			}
			if err := a.WeeklyRefreshSet(setFlag); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "✓ Weekly refresh scheduled: %s\n", setFlag)
			return nil
		},
	}
	c.Flags().StringVar(&setFlag, "set", "", `schedule as "<DAY> <HH:MM>", e.g. "SUN 03:00"`)
	c.Flags().BoolVar(&disableFlag, "disable", false, "remove the weekly-refresh task")
	return c
}
