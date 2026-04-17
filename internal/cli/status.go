package cli

import (
	"encoding/json"
	"fmt"

	"mcp-local-hub/internal/api"

	"github.com/spf13/cobra"
)

func newStatusCmdReal() *cobra.Command {
	var jsonOut bool
	c := &cobra.Command{
		Use:   "status",
		Short: "Show state of all mcp-local-hub scheduler tasks",
		RunE: func(cmd *cobra.Command, args []string) error {
			a := api.NewAPI()
			rows, err := a.Status()
			if err != nil {
				return err
			}
			if jsonOut {
				enc := json.NewEncoder(cmd.OutOrStdout())
				enc.SetIndent("", "  ")
				return enc.Encode(rows)
			}
			// Header row
			fmt.Fprintf(cmd.OutOrStdout(), "%-45s %-10s %-6s %-8s %-8s %-10s\n",
				"NAME", "STATE", "PORT", "PID", "RAM(MB)", "UPTIME")
			for _, r := range rows {
				ram := ""
				if r.RAMBytes > 0 {
					ram = fmt.Sprintf("%d", r.RAMBytes/(1024*1024))
				}
				uptime := ""
				if r.UptimeSec > 0 {
					uptime = fmt.Sprintf("%dh%dm", r.UptimeSec/3600, (r.UptimeSec%3600)/60)
				}
				port := ""
				if r.Port > 0 {
					port = fmt.Sprintf("%d", r.Port)
				}
				pid := ""
				if r.PID > 0 {
					pid = fmt.Sprintf("%d", r.PID)
				}
				fmt.Fprintf(cmd.OutOrStdout(), "%-45s %-10s %-6s %-8s %-8s %-10s\n",
					r.TaskName, r.State, port, pid, ram, uptime)
			}
			return nil
		},
	}
	c.Flags().BoolVar(&jsonOut, "json", false, "machine-readable JSON output")
	return c
}
