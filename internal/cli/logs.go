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
		Long: `Print the last N lines of a daemon's stdout/stderr log file.
Logs live at %LOCALAPPDATA%\mcp-local-hub\logs\<server>-<daemon>.log
on Windows (equivalent XDG state dir on Linux/macOS).

An absent log file is NOT an error — stdio-only daemons that produce
no stderr (perftools, time, sequential-thinking, embedded Go servers
with no diagnostics) never create a log file. In that case logs
prints a human-readable '(no output yet)' placeholder instead of the
OS 'file not found' error.

Examples:
  mcphub logs serena                    # last 100 lines, default daemon
  mcphub logs serena --daemon codex     # pick one of multiple daemons
  mcphub logs serena --tail 500         # more history
  mcphub logs serena --tail 0           # full log

Log rotation: daemon.Launch tees to these files with 10MB rotation,
5 rotations kept. Old logs appear as <name>.log.1 / .log.2 / etc.
'logs' only tails the current .log — inspect rotated copies manually.

See also: status, restart.`,
		Args: cobra.ExactArgs(1),
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
