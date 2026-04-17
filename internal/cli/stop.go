package cli

import (
	"fmt"

	"mcp-local-hub/internal/api"

	"github.com/spf13/cobra"
)

func newStopCmdReal() *cobra.Command {
	var server, daemonFilter string
	c := &cobra.Command{
		Use:   "stop",
		Short: "Stop a daemon without uninstalling (tasks and configs remain)",
		RunE: func(cmd *cobra.Command, args []string) error {
			if server == "" {
				return fmt.Errorf("--server is required")
			}
			a := api.NewAPI()
			if err := a.Stop(server, daemonFilter); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "✓ Stopped %s\n", server)
			return nil
		},
	}
	c.Flags().StringVar(&server, "server", "", "server name (required)")
	c.Flags().StringVar(&daemonFilter, "daemon", "", "daemon name within the server manifest")
	return c
}
