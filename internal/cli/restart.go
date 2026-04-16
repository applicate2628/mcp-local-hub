package cli

import (
	"mcp-local-hub/internal/scheduler"

	"github.com/spf13/cobra"
)

func newRestartCmdReal() *cobra.Command {
	var server string
	var all bool
	c := &cobra.Command{
		Use:   "restart",
		Short: "Restart daemon(s)",
		RunE: func(cmd *cobra.Command, args []string) error {
			sch, err := scheduler.New()
			if err != nil {
				return err
			}
			prefix := "mcp-local-hub-"
			if server != "" {
				prefix = "mcp-local-hub-" + server
			}
			if !all && server == "" {
				return cmd.Help()
			}
			tasks, err := sch.List(prefix)
			if err != nil {
				return err
			}
			for _, t := range tasks {
				// Skip weekly-refresh tasks when scope is --all (they trigger themselves).
				if all && (containsSuffix(t.Name, "-weekly-refresh")) {
					continue
				}
				_ = sch.Stop(t.Name)
				if err := sch.Run(t.Name); err != nil {
					cmd.Printf("⚠ run %s: %v\n", t.Name, err)
					continue
				}
				cmd.Printf("✓ Restarted %s\n", t.Name)
			}
			return nil
		},
	}
	c.Flags().StringVar(&server, "server", "", "restart only daemons for this server")
	c.Flags().BoolVar(&all, "all", false, "restart all mcp-local-hub daemons")
	return c
}

func containsSuffix(s, suffix string) bool {
	return len(s) >= len(suffix) && s[len(s)-len(suffix):] == suffix
}
