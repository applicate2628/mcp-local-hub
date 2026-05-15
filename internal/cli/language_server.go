// Package cli — language-server subcommand tree.
//
// Today this tree exposes a single action: `cleanup`. It scans every
// installed MCP client's config (claude-code, codex-cli, cursor,
// vscode, gemini-cli, qwen-cli, antigravity) for legacy stdio entries
// that invoke `mcp-language-server --lsp <X>` directly, and removes
// them after a backup is taken.
//
// Why: `mcphub register` installs HTTP-backed daemons named
// `mcp-language-server-<lang>` so multiple agents share one running
// LSP per language. If the user's pre-mcphub stdio entries (typically
// named `clangd`, `pylsp`, etc.) are left in place, each agent
// continues to spawn its OWN stdio LSP next to the shared HTTP one —
// defeating mcphub's process-tail-compression value prop. This
// command surfaces and removes those legacy stdio entries.
//
// Safety:
//   - The matcher fires only when `command` basename is exactly
//     "mcp-language-server" (case-insensitive, .exe stripped) AND the
//     args list contains "--lsp <X>" or "--lsp=X". User-owned stdio
//     servers that happen to be named "clangd" / "fortran" but invoke
//     a different binary are NEVER touched.
//   - HTTP entries (no `command`) cannot match, so the mcphub-written
//     `mcp-language-server-<lang>` entries are safe.
//   - Each client is backed up via BackupKeep before any RemoveEntry
//     call — operator can `mcphub rollback` to undo the cleanup.
//   - --dry-run prints what would change without writing anything.

package cli

import (
	"bufio"
	"fmt"
	"io"
	"sort"
	"strings"

	"mcp-local-hub/internal/api"
	"mcp-local-hub/internal/clients"

	"github.com/spf13/cobra"
)

// newLanguageServerCmdReal is the concrete cobra.Command wired by
// root.go's stub. It exposes the `cleanup` subcommand today; future
// language-server-scoped actions (e.g. `bench`, `list`) can be added
// to the same parent.
func newLanguageServerCmdReal() *cobra.Command {
	c := &cobra.Command{
		Use:   "language-server",
		Short: "Manage mcp-language-server entries across MCP client configs",
		Long: `Subcommands for the mcp-language-server LSP integration.

After running 'mcphub register <workspace>', the HTTP-backed daemons
named 'mcp-language-server-<lang>' are written into every installed
client's config. Legacy pre-mcphub stdio entries that invoke
'mcp-language-server --lsp <X>' directly remain in place by default.

Use 'mcphub language-server cleanup' to remove those legacy stdio
entries so each agent uses ONE shared HTTP daemon per language
instead of spawning its own LSP child.

See also: register, unregister, workspaces.`,
	}
	c.AddCommand(newLanguageServerCleanupCmd())
	return c
}

// newLanguageServerCleanupCmd builds the `cleanup` subcommand.
func newLanguageServerCleanupCmd() *cobra.Command {
	var clientFilter string
	var languageFilter string
	var dryRun bool
	var yes bool
	c := &cobra.Command{
		Use:   "cleanup",
		Short: "Remove legacy stdio mcp-language-server entries from MCP client configs",
		Long: `Scan every installed MCP client's config for stdio entries that
invoke 'mcp-language-server --lsp <X>' directly and remove them.
Each modified client is backed up first (see 'mcphub backups list'
and 'mcphub rollback' for undo).

The matcher is conservative: an entry qualifies only when its
'command' basename is exactly 'mcp-language-server' (case-
insensitive, '.exe' stripped) AND its 'args' contain '--lsp <X>' or
'--lsp=X'. Entries with the same name (e.g. 'clangd') that invoke a
different binary are never touched.

Examples:
  mcphub language-server cleanup                # all clients, all languages, with confirm
  mcphub language-server cleanup --dry-run      # preview only
  mcphub language-server cleanup --yes          # all clients, no prompt
  mcphub language-server cleanup --client codex-cli --language clangd

Flags:
  --client <name>     restrict to one client (claude-code | codex-cli |
                      cursor | vscode | gemini-cli | qwen-cli | antigravity)
  --language <lang>   remove only entries whose '--lsp' value matches
                      (substring-insensitive)
  --dry-run           print what would be removed; do not modify configs
  --yes               skip the interactive confirmation prompt

See also: register, rollback, backups list.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runLanguageServerCleanup(cmd, languageServerCleanupOpts{
				clientFilter:   clientFilter,
				languageFilter: languageFilter,
				dryRun:         dryRun,
				yes:            yes,
			})
		},
	}
	c.Flags().StringVar(&clientFilter, "client", "",
		"restrict cleanup to a single client by name (claude-code, codex-cli, cursor, vscode, gemini-cli, qwen-cli, antigravity)")
	c.Flags().StringVar(&languageFilter, "language", "",
		"remove only entries whose --lsp value matches this string (case-insensitive substring)")
	c.Flags().BoolVar(&dryRun, "dry-run", false,
		"print what would be removed without modifying any client config")
	c.Flags().BoolVar(&yes, "yes", false,
		"skip the interactive confirmation prompt")
	return c
}

type languageServerCleanupOpts struct {
	clientFilter   string
	languageFilter string
	dryRun         bool
	yes            bool
}

// scanResult bundles the per-client findings so the CLI can present
// one table, count totals, and gate the prompt + writes.
type scanResult struct {
	clientName string
	configPath string
	entries    []clients.LanguageServerStdioEntry
}

func runLanguageServerCleanup(cmd *cobra.Command, opts languageServerCleanupOpts) error {
	out := cmd.OutOrStdout()
	allClients := clients.AllClients()

	// Validate --client against the supported set so a typo
	// like "claude" (missing -code) fails loudly instead of
	// silently scanning zero clients.
	if opts.clientFilter != "" {
		if _, ok := allClients[opts.clientFilter]; !ok {
			return fmt.Errorf("unknown client %q (expected one of: %s)",
				opts.clientFilter,
				strings.Join(clients.SupportedClientNames(), ", "))
		}
	}

	// Iterate clients in the stable order returned by
	// SupportedClientNames for predictable CLI output and test
	// determinism. Skip clients whose config file does not exist
	// (Exists()=false) — no entries to find on hosts where the
	// agent is not installed.
	results := make([]scanResult, 0, len(allClients))
	for _, name := range clients.SupportedClientNames() {
		if opts.clientFilter != "" && name != opts.clientFilter {
			continue
		}
		adapter := allClients[name]
		if adapter == nil {
			continue
		}
		if !adapter.Exists() {
			continue
		}
		entries, err := adapter.FindStdioLanguageServerEntries()
		if err != nil {
			return fmt.Errorf("scan %s (%s): %w", name, adapter.ConfigPath(), err)
		}
		if opts.languageFilter != "" {
			entries = filterByLanguage(entries, opts.languageFilter)
		}
		if len(entries) == 0 {
			continue
		}
		results = append(results, scanResult{
			clientName: name,
			configPath: adapter.ConfigPath(),
			entries:    entries,
		})
	}

	totalEntries := 0
	for _, r := range results {
		totalEntries += len(r.entries)
	}
	if totalEntries == 0 {
		fmt.Fprintln(out, "No stdio mcp-language-server entries found.")
		return nil
	}

	printScanResults(out, results, totalEntries)

	if opts.dryRun {
		fmt.Fprintln(out, "\n--dry-run: no changes written.")
		return nil
	}

	if !opts.yes {
		ok, err := confirmCleanupPrompt(cmd.InOrStdin(), out)
		if err != nil {
			return err
		}
		if !ok {
			fmt.Fprintln(out, "Aborted.")
			return nil
		}
	}

	return applyCleanup(out, results, allClients)
}

// filterByLanguage keeps only entries whose Language contains
// languageFilter as a case-insensitive substring. Empty filter is
// handled upstream (we don't call this when the filter is empty).
func filterByLanguage(entries []clients.LanguageServerStdioEntry, languageFilter string) []clients.LanguageServerStdioEntry {
	needle := strings.ToLower(languageFilter)
	out := entries[:0]
	for _, e := range entries {
		if strings.Contains(strings.ToLower(e.Language), needle) {
			out = append(out, e)
		}
	}
	return out
}

// printScanResults renders the pre-removal summary so the operator
// can audit exactly what cleanup will touch. Stable key sort within
// each client preserves test-stable output.
func printScanResults(out io.Writer, results []scanResult, totalEntries int) {
	plural := "entry"
	if totalEntries != 1 {
		plural = "entries"
	}
	fmt.Fprintf(out, "Found %d stdio mcp-language-server %s across %d client(s):\n\n",
		totalEntries, plural, len(results))
	for _, r := range results {
		fmt.Fprintf(out, "  %s (%s):\n", r.clientName, r.configPath)
		sorted := append([]clients.LanguageServerStdioEntry(nil), r.entries...)
		sort.Slice(sorted, func(i, j int) bool { return sorted[i].Name < sorted[j].Name })
		for _, e := range sorted {
			lang := e.Language
			if lang == "" {
				lang = "(no --lsp arg)"
			}
			fmt.Fprintf(out, "    %-20s command=%s --lsp %s\n", e.Name, e.Command, lang)
		}
	}
}

// confirmCleanupPrompt asks the operator to confirm. Empty/no/N
// answers abort; only "y" or "yes" (case-insensitive) proceeds.
// EOF, a bare newline, or any non-affirmative input is treated as
// "no" so the safe outcome (no writes) is the default. A genuine
// I/O error on the scanner is surfaced so callers can distinguish
// "user declined" from "stdin broken".
func confirmCleanupPrompt(in io.Reader, out io.Writer) (bool, error) {
	fmt.Fprint(out, "\nRemove these entries? [y/N]: ")
	sc := bufio.NewScanner(in)
	if !sc.Scan() {
		if err := sc.Err(); err != nil {
			return false, fmt.Errorf("read confirmation: %w", err)
		}
		// EOF / no input — treat as decline.
		return false, nil
	}
	answer := strings.ToLower(strings.TrimSpace(sc.Text()))
	return answer == "y" || answer == "yes", nil
}

// applyCleanup performs the actual removal: BackupKeep first
// (honoring the user's backups.keep_n setting via the standard
// per-client rotation), then RemoveEntry for each matched entry.
// A backup failure aborts that client only — other clients still
// proceed. Per-entry RemoveEntry failures are surfaced inline but
// do not abort the run.
func applyCleanup(out io.Writer, results []scanResult, allClients map[string]clients.Client) error {
	keepN := effectiveBackupKeepN()
	totalRemoved := 0
	var firstErr error
	for _, r := range results {
		adapter := allClients[r.clientName]
		if adapter == nil {
			fmt.Fprintf(out, "  %s: adapter unavailable (skipped)\n", r.clientName)
			continue
		}
		bakPath, err := adapter.BackupKeep(keepN)
		if err != nil {
			fmt.Fprintf(out, "  %s: backup failed: %v (skipping this client)\n", r.clientName, err)
			if firstErr == nil {
				firstErr = fmt.Errorf("%s backup: %w", r.clientName, err)
			}
			continue
		}
		fmt.Fprintf(out, "%s: backed up → %s\n", r.clientName, bakPath)
		for _, e := range r.entries {
			if err := adapter.RemoveEntry(e.Name); err != nil {
				fmt.Fprintf(out, "  remove %s: %v\n", e.Name, err)
				if firstErr == nil {
					firstErr = fmt.Errorf("%s remove %s: %w", r.clientName, e.Name, err)
				}
				continue
			}
			fmt.Fprintf(out, "  removed %s\n", e.Name)
			totalRemoved++
		}
	}
	fmt.Fprintf(out, "\nRemoved %d entries across %d client(s).\n", totalRemoved, len(results))
	fmt.Fprintln(out, "Note: restart agents to pick up the new config.")
	return firstErr
}

// effectiveBackupKeepN reads backups.keep_n via the standard
// settings path and falls back to api.DefaultBackupKeepN when the
// settings layer is unreachable. Mirrors the helper used by
// migrate / register so cleanup respects the same retention budget.
func effectiveBackupKeepN() int {
	a := api.NewAPI()
	return a.EffectiveBackupKeepN()
}
