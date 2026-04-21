// Package api — legacy mcp-language-server migration (M4 Task 14).
//
// Lazy-mode adaptation from the eager plan: Register in lazy mode
// configures ALL manifest languages per call, so migration dedupes the
// detected LegacyLSEntry slice by workspace and emits ONE Register(ws,
// nil, opts) per unique workspace instead of one per (workspace,
// language) pair. That produces fewer, simpler scheduler tasks (exactly
// one proxy per (workspace, language) rather than N overlapping task
// sets).
//
// Ordering contract: a legacy entry is deleted from its original client
// config ONLY after the covering Register for its workspace succeeds. A
// failed register keeps the legacy rows intact so the user can re-run
// the migration.
package api

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
)

// LegacyMigrateOpts controls MigrateLegacy behavior.
type LegacyMigrateOpts struct {
	DryRun bool
	Yes    bool      // skip per-workspace prompts (non-interactive)
	In     io.Reader // defaults to os.Stdin for prompts; overridden in tests
	Writer io.Writer // progress output; nil = os.Stderr
}

// LegacyMigrateReport lists the (per-entry) outcome of a migration run.
// Planned is the full input slice; Applied / Skipped / Failed together
// cover every input entry in non-dry-run mode. In dry-run mode Applied is
// always empty.
type LegacyMigrateReport struct {
	Planned []LegacyLSEntry     `json:"planned"`
	Applied []LegacyLSEntry     `json:"applied"`
	Skipped []LegacyLSEntry     `json:"skipped"`
	Failed  []FailedLegacyEntry `json:"failed,omitempty"`
}

// FailedLegacyEntry carries the error string so the report serializes
// cleanly to JSON without exposing Go error values.
type FailedLegacyEntry struct {
	Entry LegacyLSEntry `json:"entry"`
	Err   string        `json:"err"`
}

// MigrateLegacy converts the supplied LegacyLSEntry slice into managed
// workspace registrations. Behavior:
//
//   - DryRun prints the plan (one Register per unique workspace + the
//     legacy entries that would be removed) and returns without side
//     effects.
//   - Interactive mode (default) prompts per unique workspace before the
//     Register call. Answering "n" skips every legacy entry belonging to
//     that workspace — an all-or-nothing choice that matches the lazy
//     register contract (one call = all languages).
//   - --yes mode skips all prompts and converts every known-language
//     workspace unattended.
//
// Entries whose Language is empty (unknown LSP binary) are Skipped with
// a diagnostic. Their workspace is still eligible for Register via its
// OTHER entries, but the unknown-binary row itself is not removed — the
// user needs to rename it manually.
func (a *API) MigrateLegacy(entries []LegacyLSEntry, opts LegacyMigrateOpts) (*LegacyMigrateReport, error) {
	w := opts.Writer
	if w == nil {
		w = os.Stderr
	}
	in := opts.In
	if in == nil {
		in = os.Stdin
	}
	reader := bufio.NewReader(in)
	report := &LegacyMigrateReport{Planned: append([]LegacyLSEntry(nil), entries...)}

	// Partition: unknown-language rows get Skipped immediately; the rest
	// are grouped by workspace. A workspace with ZERO known-language rows
	// is effectively skipped (no Register call, no client-side deletes).
	var eligible []LegacyLSEntry
	for _, e := range entries {
		if e.Language == "" {
			report.Skipped = append(report.Skipped, e)
			fmt.Fprintf(w, "skip: %s entry %q — unknown LSP binary %q (manual migration required)\n",
				e.Client, e.EntryName, e.LspCommand)
			continue
		}
		if e.Workspace == "" {
			// No workspace arg at all — legacy entry is malformed. Record
			// as Failed so the caller sees it.
			report.Failed = append(report.Failed, FailedLegacyEntry{
				Entry: e,
				Err:   "legacy entry missing --workspace arg",
			})
			continue
		}
		eligible = append(eligible, e)
	}

	// Group eligible entries by workspace (first-seen preserving insertion
	// order of entries for deterministic prompt order across runs — sorted
	// after collection).
	byWorkspace := map[string][]LegacyLSEntry{}
	var workspaceOrder []string
	for _, e := range eligible {
		if _, seen := byWorkspace[e.Workspace]; !seen {
			workspaceOrder = append(workspaceOrder, e.Workspace)
		}
		byWorkspace[e.Workspace] = append(byWorkspace[e.Workspace], e)
	}
	sort.Strings(workspaceOrder)

	if opts.DryRun {
		for _, ws := range workspaceOrder {
			rows := byWorkspace[ws]
			fmt.Fprintf(w, "plan: register %s (all manifest languages) and remove %d legacy entries\n",
				ws, len(rows))
			for _, e := range rows {
				fmt.Fprintf(w, "  - %s entry %q (lang %s)\n", e.Client, e.EntryName, e.Language)
			}
		}
		return report, nil
	}

	// Real run: one Register per unique workspace, then delete every
	// legacy entry belonging to that workspace on success.
	allClients := clientsAllForRegister()
	for _, ws := range workspaceOrder {
		rows := byWorkspace[ws]
		if !opts.Yes {
			fmt.Fprintf(w, "Migrate workspace %s (register all manifest languages, remove %d legacy entries)? [Y/n] ",
				ws, len(rows))
			line, readErr := reader.ReadString('\n')
			// Stdin closed before a response arrived (io.EOF) means there is
			// no interactive user — e.g., CI without --yes, redirected input,
			// or a broken pipe. Treat as declined rather than silently
			// approving a destructive register+delete. An Enter-without-typing
			// with a real terminal still hits the default-Y branch below
			// because readErr is nil and line == "\n" → trimmed empty.
			if readErr != nil && errors.Is(readErr, io.EOF) {
				fmt.Fprintf(w, "  stdin closed without confirmation; skipping %s\n", ws)
				report.Skipped = append(report.Skipped, rows...)
				continue
			}
			if readErr != nil {
				fmt.Fprintf(w, "  read confirmation for %s: %v — skipping\n", ws, readErr)
				report.Skipped = append(report.Skipped, rows...)
				continue
			}
			ans := strings.TrimSpace(line)
			if ans != "" && !strings.EqualFold(ans, "y") && !strings.EqualFold(ans, "yes") {
				report.Skipped = append(report.Skipped, rows...)
				continue
			}
		}
		if _, err := a.Register(ws, nil, RegisterOpts{Writer: w, WeeklyRefresh: true}); err != nil {
			// Register failed — keep every legacy row intact, record per-row failure.
			for _, e := range rows {
				report.Failed = append(report.Failed, FailedLegacyEntry{
					Entry: e,
					Err:   "register: " + err.Error(),
				})
			}
			continue
		}
		// Register succeeded; now delete each legacy entry from its owning
		// client — but NOT when Register replaced the legacy entry in place.
		// If the legacy row used the canonical managed name
		// (e.g. "mcp-language-server-python"), Register.AddEntry already
		// overwrote that same key with the new workspace-proxy URL, and
		// a RemoveEntry here would delete the freshly-migrated config.
		// Load the registry once so we can ask: "did Register just write
		// this (client, entry_name)?"
		regPath, _ := registryPathForRegister()
		reg := NewRegistry(regPath)
		_ = reg.Load()
		// Constrain the in-place check to THIS workspace — otherwise a
		// name used by a different workspace (e.g., a collision-suffixed
		// "mcp-language-server-python-abcd" on workspace A, combined with
		// a plain "mcp-language-server-python" legacy row on workspace B)
		// would falsely match and skip the RemoveEntry, leaving the old
		// legacy config in place. CanonicalWorkspacePath here mirrors
		// what Register did internally; on symlinked/junction paths the
		// EvalSymlinks resolution is the same, so the keys match.
		currentWSKey := ""
		if canonical, err := CanonicalWorkspacePath(ws); err == nil {
			currentWSKey = WorkspaceKey(canonical)
		}
		entryJustWrittenByRegister := func(client, entryName string) bool {
			for _, regEntry := range reg.Workspaces {
				if regEntry.WorkspaceKey != currentWSKey {
					continue
				}
				if name, ok := regEntry.ClientEntries[client]; ok && name == entryName {
					return true
				}
			}
			return false
		}
		for _, e := range rows {
			if entryJustWrittenByRegister(e.Client, e.EntryName) {
				// Legacy row was replaced in-place with the new
				// workspace-proxy URL — migration already succeeded for
				// this row without a separate RemoveEntry.
				fmt.Fprintf(w, "  legacy entry %q in %s already replaced by Register (in-place)\n",
					e.EntryName, e.Client)
				report.Applied = append(report.Applied, e)
				continue
			}
			client, ok := allClients[e.Client]
			if !ok || !client.Exists() {
				// Client adapter unavailable — record the delete failure
				// but keep the successful Register accounted for. The
				// legacy row stays in the config; user can re-run migrate.
				report.Failed = append(report.Failed, FailedLegacyEntry{
					Entry: e,
					Err:   fmt.Sprintf("client %s unavailable for RemoveEntry", e.Client),
				})
				continue
			}
			if err := client.RemoveEntry(e.EntryName); err != nil {
				report.Failed = append(report.Failed, FailedLegacyEntry{
					Entry: e,
					Err:   "remove legacy entry: " + err.Error(),
				})
				continue
			}
			report.Applied = append(report.Applied, e)
		}
	}
	return report, nil
}
