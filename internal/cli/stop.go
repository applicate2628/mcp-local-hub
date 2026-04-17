package cli

import (
	"fmt"

	"mcp-local-hub/internal/api"

	"github.com/spf13/cobra"
)

func newStopCmdReal() *cobra.Command {
	var server, daemonFilter string
	var all bool
	c := &cobra.Command{
		Use:   "stop",
		Short: "Stop daemon(s) without uninstalling (tasks and configs remain)",
		RunE: func(cmd *cobra.Command, args []string) error {
			if all {
				if server != "" {
					return fmt.Errorf("--all is mutually exclusive with --server")
				}
				a := api.NewAPI()
				results, err := a.StopAll()
				if err != nil {
					return err
				}
				for _, r := range results {
					if r.Err != "" {
						fmt.Fprintf(cmd.OutOrStderr(), "✗ %s: %s\n", r.TaskName, r.Err)
					} else {
						fmt.Fprintf(cmd.OutOrStdout(), "✓ Stopped %s\n", r.TaskName)
					}
				}
				return nil
			}
			if server == "" {
				return fmt.Errorf("--server or --all is required")
			}
			a := api.NewAPI()
			if err := a.Stop(server, daemonFilter); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "✓ Stopped %s\n", server)
			return nil
		},
	}
	c.Flags().StringVar(&server, "server", "", "server name")
	c.Flags().StringVar(&daemonFilter, "daemon", "", "daemon name within the server manifest")
	c.Flags().BoolVar(&all, "all", false, "stop every running daemon")
	return c
}
