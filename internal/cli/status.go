package cli

import (
	"mcp-local-hub/internal/scheduler"

	"github.com/spf13/cobra"
)

func newStatusCmdReal() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Show scheduler task state for all mcp-local-hub daemons",
		RunE: func(cmd *cobra.Command, args []string) error {
			sch, err := scheduler.New()
			if err != nil {
				return err
			}
			tasks, err := sch.List("mcp-local-hub-")
			if err != nil {
				return err
			}
			if len(tasks) == 0 {
				cmd.Println("No mcp-local-hub tasks installed.")
				return nil
			}
			cmd.Printf("%-45s  %-10s  %-12s  %s\n", "NAME", "STATE", "LAST RESULT", "NEXT RUN")
			for _, t := range tasks {
				cmd.Printf("%-45s  %-10s  %-12d  %s\n", t.Name, t.State, t.LastResult, t.NextRun)
			}
			return nil
		},
	}
}
