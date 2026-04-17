package cli

import (
	"fmt"

	"mcp-local-hub/internal/api"

	"github.com/spf13/cobra"
)

func newLogsCmdReal() *cobra.Command {
	var tail int
	var daemon string
	var follow bool
	c := &cobra.Command{
		Use:   "logs <server>",
		Short: "Print (and optionally follow) daemon logs",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			a := api.NewAPI()
			if follow {
				return fmt.Errorf("--follow not yet implemented in Phase 3A.2 — coming in 3A.3")
			}
			content, err := a.LogsGet(args[0], daemon, tail)
			if err != nil {
				return err
			}
			fmt.Fprint(cmd.OutOrStdout(), content)
			return nil
		},
	}
	c.Flags().IntVar(&tail, "tail", 100, "number of trailing lines (0 = all)")
	c.Flags().StringVar(&daemon, "daemon", "default", "daemon name within server manifest")
	c.Flags().BoolVar(&follow, "follow", false, "stream new log lines (not yet implemented)")
	return c
}
