package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"mcp-local-hub/internal/api"

	"github.com/spf13/cobra"
)

func newMigrateCmdReal() *cobra.Command {
	var clientsFlag string
	var dryRun, jsonOut bool
	c := &cobra.Command{
		Use:   "migrate <server>...",
		Short: "Switch stdio client entries to hub HTTP for the specified servers",
		Long: `Rewrite each listed server's entry in every managed client config from
its old stdio form (command + args) to an HTTP reference pointing at the
hub daemon (or a stdio-relay for Antigravity).

Unlike 'install', migrate does NOT create scheduler tasks or start
daemons — it assumes the server is already installed. Use it to bring
existing client configs into alignment with a server that was installed
before a particular client was installed on this machine.

Examples:
  mcphub migrate serena             # migrate in all managed clients
  mcphub migrate serena memory time # multiple servers in one pass
  mcphub migrate serena --clients claude-code,codex-cli  # subset
  mcphub migrate serena --dry-run   # preview without writing

See also: scan, install, rollback.`,
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			var include []string
			if clientsFlag != "" {
				include = strings.Split(clientsFlag, ",")
			}
			a := api.NewAPI()
			home, err := os.UserHomeDir()
			if err != nil {
				return err
			}
			report, err := a.MigrateFrom(api.MigrateOpts{
				Servers:        args,
				ClientsInclude: include,
				DryRun:         dryRun,
				ScanOpts: api.ScanOpts{
					ClaudeConfigPath:      filepath.Join(home, ".claude.json"),
					CodexConfigPath:       filepath.Join(home, ".codex", "config.toml"),
					GeminiConfigPath:      filepath.Join(home, ".gemini", "settings.json"),
					AntigravityConfigPath: filepath.Join(home, ".gemini", "antigravity", "mcp_config.json"),
					ManifestDir:           scanManifestDir(),
				},
			})
			if err != nil {
				return err
			}
			if jsonOut {
				enc := json.NewEncoder(cmd.OutOrStdout())
				enc.SetIndent("", "  ")
				return enc.Encode(report)
			}
			for _, app := range report.Applied {
				fmt.Fprintf(cmd.OutOrStdout(), "✓ %s/%s → %s\n", app.Server, app.Client, app.URL)
			}
			for _, f := range report.Failed {
				fmt.Fprintf(cmd.OutOrStderr(), "✗ %s/%s: %s\n", f.Server, f.Client, f.Err)
			}
			if dryRun {
				fmt.Fprintln(cmd.OutOrStdout(), "\n(dry-run — no files modified)")
			}
			return nil
		},
	}
	c.Flags().StringVar(&clientsFlag, "clients", "", "comma-separated subset of clients (default: all four)")
	c.Flags().BoolVar(&dryRun, "dry-run", false, "show what would change, don't write")
	c.Flags().BoolVar(&jsonOut, "json", false, "machine-readable JSON output")
	return c
}
