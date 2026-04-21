// Package cli — migrate-legacy command (M4 Task 14).
//
// `mcphub migrate-legacy` detects disabled mcp-language-server entries in
// Codex + Claude Code configs (see api.DetectLegacyLanguageServerEntries)
// and converts each one into a proper workspace registration. The CLI is
// a thin wrapper around api.MigrateLegacy; all behavior lives in
// internal/api so CLI and future GUI frontends share one code path.
package cli

import (
	"encoding/json"
	"fmt"

	"mcp-local-hub/internal/api"

	"github.com/spf13/cobra"
)

// newMigrateLegacyCmdReal is the concrete cobra.Command wired by root.go's
// stub. Usage: `mcphub migrate-legacy [--dry-run] [--yes] [--json]`.
func newMigrateLegacyCmdReal() *cobra.Command {
	var dryRun, yes, jsonOut bool
	c := &cobra.Command{
		Use:   "migrate-legacy",
		Short: "Detect + migrate disabled mcp-language-server entries into managed registry",
		Long: `Scan every installed MCP client config (Codex + Claude Code) for
disabled entries whose command is mcp-language-server. For each unique
workspace, emit one 'mcphub register' — which allocates ports, creates
scheduler tasks, and writes new client entries for ALL manifest
languages — and THEN delete the original disabled entries.

Lazy-mode note: one 'register' call covers every manifest language at
once, so migration dedupes the detected rows by workspace and emits
exactly one register per unique workspace (not one per language).

Interactive by default: prompts per workspace. --yes skips every prompt.
--dry-run prints the plan without changing any state.

Examples:
  mcphub migrate-legacy --dry-run    # preview
  mcphub migrate-legacy              # interactive
  mcphub migrate-legacy --yes        # non-interactive

See also: register, workspaces.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			entries, err := api.DetectLegacyLanguageServerEntries()
			if err != nil {
				return err
			}
			a := api.NewAPI()
			report, err := a.MigrateLegacy(entries, api.LegacyMigrateOpts{
				DryRun: dryRun,
				Yes:    yes,
				Writer: cmd.OutOrStdout(),
				In:     cmd.InOrStdin(),
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
					return fmt.Errorf("%d legacy row(s) failed to migrate", len(report.Failed))
				}
				return nil
			}
			for _, e := range report.Applied {
				fmt.Fprintf(cmd.OutOrStdout(), "\u2713 migrated %s entry %q (workspace %s, language %s)\n",
					e.Client, e.EntryName, e.Workspace, e.Language)
			}
			for _, f := range report.Failed {
				fmt.Fprintf(cmd.OutOrStderr(), "\u2717 %s/%s: %s\n", f.Entry.Client, f.Entry.EntryName, f.Err)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "\nPlanned: %d  Applied: %d  Skipped: %d  Failed: %d\n",
				len(report.Planned), len(report.Applied), len(report.Skipped), len(report.Failed))
			if dryRun {
				fmt.Fprintln(cmd.OutOrStdout(), "(dry-run — no files modified)")
			}
			if len(report.Failed) > 0 {
				return fmt.Errorf("%d legacy row(s) failed to migrate", len(report.Failed))
			}
			return nil
		},
	}
	c.Flags().BoolVar(&dryRun, "dry-run", false, "print plan without changing any state")
	c.Flags().BoolVar(&yes, "yes", false, "skip per-workspace prompts (non-interactive)")
	c.Flags().BoolVar(&jsonOut, "json", false, "machine-readable JSON output")
	return c
}
