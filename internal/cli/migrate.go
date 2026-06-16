package cli

import (
	"encoding/json"
	"fmt"
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
			return runStdioMigrate(cmd, args, clientsFlag, dryRun, jsonOut)
		},
	}
	c.Flags().StringVar(&clientsFlag, "clients", "", "comma-separated subset of clients (default: every binding in the manifest)")
	c.Flags().BoolVar(&dryRun, "dry-run", false, "show what would change, don't write")
	c.Flags().BoolVar(&jsonOut, "json", false, "machine-readable JSON output")
	// `serena` is attached as a subcommand so `mcphub migrate serena
	// legacy-to-dynamic-pool` reaches the dynamic-pool driver. cobra
	// resolves a subcommand named `serena` before treating it as a
	// positional, so the subcommand carries its OWN RunE that delegates
	// back to runStdioMigrate (re-prepending the consumed `serena` token)
	// to preserve the documented `mcphub migrate serena [more]` stdio→HTTP
	// behavior. Only `... serena legacy-to-dynamic-pool` routes to the
	// dynamic-pool migration. See migrate_serena.go.
	c.AddCommand(newMigrateSerenaCmd())
	return c
}

// runStdioMigrate is the leaf stdio→HTTP migration body, extracted from
// the migrate command's RunE so the serena dynamic-pool subcommand (which
// must coexist under `mcphub migrate serena ...`) can delegate the
// existing `mcphub migrate <server>...` behavior without re-implementing
// it. servers is the already-resolved positional server list.
func runStdioMigrate(cmd *cobra.Command, servers []string, clientsFlag string, dryRun, jsonOut bool) error {
	var include []string
	if clientsFlag != "" {
		include = strings.Split(clientsFlag, ",")
	}
	a := api.NewAPI()
	report, err := a.MigrateFrom(api.MigrateOpts{
		Servers:        servers,
		ClientsInclude: include,
		DryRun:         dryRun,
		ScanOpts: api.ScanOpts{
			// §9.2 drift-prevention: derive the per-client config-path set
			// from the canonical registry (api.DefaultScanConfigPaths →
			// clients.SupportedClientNames + ConfigPathForName), identical
			// to the api.Scan + `mcphub scan` surfaces. A new registry
			// client is covered automatically with ZERO edits here. (The
			// actual migrate WRITE path in api.MigrateFrom is already
			// registry-driven via clients.AllClients(); these paths feed the
			// scan/presence half only — kept in lockstep so the two halves
			// never disagree on the client set.)
			ConfigPaths: api.DefaultScanConfigPaths(),
			ManifestDir: scanManifestDir(),
		},
	})
	if err != nil {
		return err
	}
	if jsonOut {
		enc := json.NewEncoder(cmd.OutOrStdout())
		enc.SetIndent("", "  ")
		if err := enc.Encode(report); err != nil {
			return err
		}
		if len(report.Failed) > 0 {
			// JSON consumers parse Failed[] directly, but exit code
			// must still signal failure so CI scripts don't treat
			// partial-success output as success.
			return fmt.Errorf("%d migration row(s) failed", len(report.Failed))
		}
		return nil
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
	if len(report.Failed) > 0 {
		return fmt.Errorf("%d migration row(s) failed", len(report.Failed))
	}
	return nil
}
