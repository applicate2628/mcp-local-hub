// internal/api/lsp_client_router_snapshot.go
//
// Exact per-(client, language) pre-state capture + restore for the shared LSP
// router client entries.
//
// WHY THIS EXISTS (codex bot PR #588 P1 closure). `mcphub install
// --reconcile-mcp-front` rewrites each client's canonical
// `mcp-language-server-<language>` entry from one loopback router port to
// another (the GUI's port -> the settings-owned mcp_front.port). Its rollback
// used to call RollbackLSPRouterClientEntries, which is a DIFFERENT operation:
// the router-to-legacy DEMOTION routine. That routine reconstructs
// per-workspace legacy entries from the registry and then REMOVES the
// canonical router entry — so on an already-migrated host (the normal case,
// where the forward pass only changed a port) "rollback" deleted the shared
// LSP router entry instead of putting its previous port back. A rollback that
// silently leaves a host mis-pointed is worse than no rollback at all.
//
// The reversal of "I changed this entry's URL" is "put the previous URL back",
// and that requires knowing the previous URL. Hence: capture it before the
// write (SnapshotLSPRouterClientEntries), persist it with the run, and drive
// the rollback from it (RestoreLSPRouterClientEntriesSnapshot).
//
// Scope boundary: this file does NOT replace RollbackLSPRouterClientEntries.
// That routine remains the correct owner of its own operation (demoting the
// shared router back to per-workspace legacy entries, reconstructed from the
// registry); it is simply not the inverse of a port rewrite.
package api

import (
	"errors"
	"fmt"

	"mcp-local-hub/internal/clients"
)

// LSPRouterEntrySnapshot is the pre-write state of ONE client's canonical
// `mcp-language-server-<language>` entry.
//
// Present distinguishes "the entry existed and looked like this" from "the
// entry did not exist" — the two have opposite inverses (rewrite back vs
// remove), and a zero-valued row must never be read as the former.
//
// Disabled is captured for diagnosis only. Restore does NOT re-apply it: the
// client adapters' AddEntry has no disabled knob, so the FORWARD pass already
// cannot preserve that bit either (entryMatchesLSPRouter treats a disabled
// entry as non-matching and rewrites it enabled). Recording it keeps the
// artifact honest about what the pre-state was without implying a guarantee
// the write path cannot make.
type LSPRouterEntrySnapshot struct {
	Client    string `json:"client"`
	Language  string `json:"language"`
	EntryName string `json:"entry_name"`
	Present   bool   `json:"present"`
	URL       string `json:"url,omitempty"`
	RelayURL  string `json:"relay_url,omitempty"`
	Disabled  bool   `json:"disabled,omitempty"`
}

// SnapshotLSPRouterClientEntries records the current canonical router entry of
// every present client, for every in-scope language, BEFORE any write.
//
// FAIL-CLOSED: a per-client read error aborts the whole snapshot with an
// error rather than emitting a row that would be indistinguishable from
// "entry absent". A caller that proceeded on a partial snapshot would later
// "restore to absence" and DELETE an entry it never captured — the same class
// of defect the adopt-provenance capture gate closes (see CLAUDE.md, "Adopt
// provenance": fail-closed present-at-Build check).
//
// The snapshot deliberately covers EVERY present client, not just the ones
// the forward reconcile's enablement filter will actually touch. A row for an
// untouched client is a no-op at restore time (its live entry already equals
// the recorded pre-state), so a superset is free; a subset computed from a
// second, independently-derived enablement predicate would be a drift hazard.
//
// opts.GUIPort is not consulted — this is a pure read of what is on disk now.
func (a *API) SnapshotLSPRouterClientEntries(opts LSPClientRouterOpts) ([]LSPRouterEntrySnapshot, error) {
	languages, err := loadLSPRouterLanguages(opts.Languages)
	if err != nil {
		return nil, err
	}
	clientMap := opts.Clients
	if clientMap == nil {
		clientMap = clients.AllClients()
	}
	out := make([]LSPRouterEntrySnapshot, 0, len(clientMap)*len(languages))
	for _, clientName := range sortedLSPClientNames(clientMap) {
		adapter := clientMap[clientName]
		if adapter == nil || !adapter.Exists() {
			continue
		}
		for _, language := range languages {
			entryName := LSPRouterEntryName(language)
			live, readErr := adapter.GetEntry(entryName)
			if readErr != nil {
				return nil, fmt.Errorf("snapshot lsp router pre-state: read %s entry %s: %w", clientName, entryName, readErr)
			}
			row := LSPRouterEntrySnapshot{Client: clientName, Language: language, EntryName: entryName}
			if live != nil {
				row.Present = true
				row.URL = live.URL
				row.RelayURL = live.RelayURL
				row.Disabled = live.Disabled
			}
			out = append(out, row)
		}
	}
	return out, nil
}

// RestoreLSPRouterClientEntriesSnapshot drives each recorded entry back to the
// exact state SnapshotLSPRouterClientEntries captured:
//
//   - recorded PRESENT with a URL -> rewrite the entry to that URL (no-op when
//     it already matches).
//   - recorded ABSENT -> remove the entry, but ONLY when the live entry is a
//     hub-owned LSP router entry for that language. An entry an operator
//     created after the forward run is reported Skipped, never deleted.
//   - recorded PRESENT with neither URL nor RelayURL -> skipped. The forward
//     pass's own hub-owned guard (entryIsHubOwnedLSPClientEntry) could not
//     have rewritten such an entry, so there is nothing to reverse and
//     synthesizing a URL for it would be a fabrication.
//
// opts.GUIPort is the port the forward run wrote, used as ownership evidence
// before any overwrite/removal. It must be in range: a zero port would make
// the ownership check weaker than the forward pass's own, so it fails closed.
func (a *API) RestoreLSPRouterClientEntriesSnapshot(snapshot []LSPRouterEntrySnapshot, opts LSPClientRouterOpts) (*LSPClientRouterReport, error) {
	report := &LSPClientRouterReport{}
	if len(snapshot) == 0 {
		return report, nil
	}
	port, err := resolvedLSPRouterGUIPort(opts.GUIPort)
	if err != nil {
		return report, err
	}
	clientMap := opts.Clients
	if clientMap == nil {
		clientMap = clients.AllClients()
	}
	keepN := opts.BackupKeepN
	if keepN == 0 {
		keepN = a.EffectiveBackupKeepN()
	}

	byClient := map[string][]LSPRouterEntrySnapshot{}
	for _, row := range snapshot {
		byClient[row.Client] = append(byClient[row.Client], row)
	}

	for _, clientName := range sortedLSPClientNames(clientMap) {
		rows := byClient[clientName]
		if len(rows) == 0 {
			continue
		}
		adapter := clientMap[clientName]
		if adapter == nil || !adapter.Exists() {
			continue
		}
		ops := make([]lspClientRouterOp, 0, len(rows))
		for _, row := range rows {
			entryName := row.EntryName
			if entryName == "" {
				entryName = LSPRouterEntryName(row.Language)
			}
			live, readErr := adapter.GetEntry(entryName)
			if readErr != nil {
				report.Failed = append(report.Failed, lspFailure(clientName, row.Language, entryName, "read", readErr))
				continue
			}
			owned := false
			if live != nil {
				owned, _ = entryIsOwnedLSPRouterForLanguage(entryName, live, row.Language, port)
			}

			if !row.Present {
				if live == nil {
					continue
				}
				if !owned {
					report.Skipped = append(report.Skipped, LSPClientRouterChange{
						Client: clientName, Language: row.Language, EntryName: entryName, URL: entryLSPRouterURL(live),
					})
					continue
				}
				ops = append(ops, lspClientRouterOp{
					kind:      "remove",
					language:  row.Language,
					entryName: entryName,
				})
				continue
			}

			priorURL := row.URL
			if priorURL == "" {
				priorURL = row.RelayURL
			}
			if priorURL == "" {
				continue
			}
			if entryMatchesURL(live, priorURL) {
				continue
			}
			if live != nil && !owned {
				report.Failed = append(report.Failed, lspFailure(clientName, row.Language, entryName, "ownership",
					errors.New("live entry is not a hub-owned LSP router entry; refusing to overwrite")))
				continue
			}
			entry, prepErr := lspLegacyMCPEntryForClient(opts, adapter, entryName, priorURL)
			if prepErr != nil {
				report.Failed = append(report.Failed, lspFailure(clientName, row.Language, entryName, "prepare", prepErr))
				continue
			}
			ops = append(ops, lspClientRouterOp{
				kind:      "add",
				language:  row.Language,
				entryName: entryName,
				entry:     entry,
			})
		}
		applyLSPRouterOps(opts, adapter, clientName, keepN, ops, report)
	}
	return report, lspRouterReportError(report, "lsp client router snapshot restore")
}
