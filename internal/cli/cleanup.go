package cli

import (
	"fmt"

	"mcp-local-hub/internal/api"

	"github.com/spf13/cobra"
)

func newCleanupCmdReal() *cobra.Command {
	var dryRun, confirm bool
	var server string
	var minAge int64
	c := &cobra.Command{
		Use:   "cleanup",
		Short: "Find and kill orphan MCP server processes (dry-run by default)",
		Long: `Finds MCP-server processes whose command line matches a manifest's command
but whose parent is NOT our 'mcp.exe daemon' wrapper. These are typically
leftover from dead client sessions (IDE restarts, CTRL-C not propagating
to children, etc.).

Default is dry-run (reports only). Pass --confirm to actually kill.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if confirm {
				dryRun = false
			}
			a := api.NewAPI()
			orphans, err := a.CleanupOrphans(api.CleanupOpts{
				ManifestDir: scanManifestDir(),
				Server:      server,
				DryRun:      dryRun,
				MinAgeSec:   minAge,
			})
			if err != nil {
				return err
			}
			if len(orphans) == 0 {
				fmt.Fprintln(cmd.OutOrStdout(), "No orphan processes found.")
				return nil
			}
			totalRAM := uint64(0)
			for _, o := range orphans {
				totalRAM += o.RAMBytes
				fmt.Fprintf(cmd.OutOrStdout(), "  PID %-6d  server=%-18s  RAM=%-6.0f MB  age=%-6ds  %s\n",
					o.PID, o.Server, float64(o.RAMBytes)/(1024*1024), o.AgeSec,
					truncate(o.Cmdline, 80))
			}
			fmt.Fprintf(cmd.OutOrStdout(), "\n%d orphans · %.0f MB total\n",
				len(orphans), float64(totalRAM)/(1024*1024))
			if dryRun {
				fmt.Fprintln(cmd.OutOrStdout(), "(dry-run — no processes killed. Re-run with --confirm to kill.)")
			} else {
				fmt.Fprintln(cmd.OutOrStdout(), "(killed via taskkill /F)")
			}
			return nil
		},
	}
	c.Flags().BoolVar(&dryRun, "dry-run", true, "report only, do not kill (default)")
	c.Flags().BoolVar(&confirm, "confirm", false, "actually kill the orphans (overrides --dry-run)")
	c.Flags().StringVar(&server, "server", "", "limit scan to this server's pattern (default: all manifests)")
	c.Flags().Int64Var(&minAge, "min-age-sec", 60, "ignore processes younger than this (seconds)")
	return c
}

// truncate shortens a string to at most n characters, appending "..." if truncated.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-3] + "..."
}
