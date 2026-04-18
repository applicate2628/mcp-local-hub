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
		Long: `Find MCP-server processes that match a manifest pattern, are old enough
(>60s by default), and have NO live mcphub daemon in their parent chain.
These are typically leftover from dead client sessions (IDE restarts,
Ctrl-C not propagating to children, stale uvx/npx after daemon crash).

Default is dry-run (reports only). Pass --confirm to actually kill.

Safety guards (to avoid false-positives like killing Dropbox):
  - Our own binaries (mcphub/godbolt/lldb-bridge/perftools) are always excluded
  - Processes whose ancestor chain contains 'mcphub.exe daemon' are excluded
    (walks up to 16 levels, catches uv/npx/python sub-processes)
  - Generic interpreter substrings (python, node, npx) are NOT used as match
    patterns — they'd false-match Dropbox/VS Code/MSYS2 shell processes
  - Min-age filter skips processes younger than 60s (they may be legitimate
    in-flight installs)

Examples:
  mcphub cleanup --dry-run              # safe preview (default)
  mcphub cleanup --confirm              # actually kill
  mcphub cleanup --server serena        # limit to one server's patterns
  mcphub cleanup --min-age-sec 300      # stricter: ignore <5min processes

Pair with 'stop --all' for a nuclear reset:
  mcphub stop --all && mcphub cleanup --confirm

See also: stop, restart, status.`,
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
				return nil
			}
			killed, skipped := 0, 0
			for _, o := range orphans {
				if o.KillErr != "" {
					skipped++
				} else {
					killed++
				}
			}
			fmt.Fprintf(cmd.OutOrStdout(), "killed: %d · skipped: %d · total: %d\n",
				killed, skipped, len(orphans))
			for _, o := range orphans {
				if o.KillErr != "" {
					fmt.Fprintf(cmd.OutOrStderr(), "  ✗ PID %d: %s\n", o.PID, o.KillErr)
				}
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
