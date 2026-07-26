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
	"sort"

	"mcp-local-hub/internal/clients"
)

// LSPRouterEntrySnapshot is the pre-write state of ONE client's canonical
// `mcp-language-server-<language>` entry.
//
// Present distinguishes "the entry existed and looked like this" from "the
// entry did not exist" — the two have opposite inverses (rewrite back vs
// remove), and a zero-valued row must never be read as the former.
//
// Disabled records that the pre-state entry was present but in a state the
// client does NOT load. The forward pass rewrites such an entry ENABLED
// (entryMatchesLSPRouter treats a disabled entry as non-matching, and the
// hub-owned guard it consults does not exclude disabled entries), so
// "disabled" is a real bit the cutover changes and the rollback therefore owes
// back. Leaving it enabled after a rollback is not a cosmetic drift: the
// operator gets an ACTIVE entry pointing at a port the rollback just took out
// of service, which their client reports as a broken MCP server — strictly
// worse than the inert entry they had.
//
// Raw is the adapter's verbatim on-disk entry value when the adapter can
// produce one (clients.MCPEntry.Raw — MiMoCode/OpenCode today). It is what
// makes a COMPLETE restore possible: AddEntry writes Raw verbatim when set, so
// replaying it puts the entry back exactly as it was, disabled bit included.
// Adapters that leave Raw nil cannot express "disabled" through AddEntry at
// all; RestoreLSPRouterClientEntriesSnapshot fails those rows loudly rather
// than silently restoring a URL and dropping the bit.
type LSPRouterEntrySnapshot struct {
	Client    string         `json:"client"`
	Language  string         `json:"language"`
	EntryName string         `json:"entry_name"`
	Present   bool           `json:"present"`
	URL       string         `json:"url,omitempty"`
	RelayURL  string         `json:"relay_url,omitempty"`
	Disabled  bool           `json:"disabled,omitempty"`
	Raw       map[string]any `json:"raw,omitempty"`
}

// restorable reports whether this row describes a pre-state that a restore
// could actually put back. A row recorded PRESENT but carrying neither URL nor
// RelayURL is not restorable-by-URL and is documented as skipped (the forward
// pass's own hub-owned guard could not have rewritten such an entry, so there
// is nothing to reverse and synthesizing a URL would be fabrication).
//
// This is single-owned because BOTH the pending classification and the restore
// loop must agree on it: if they drifted, a row one of them considers inert
// could block consumption forever, or a row that genuinely needs restoring
// could be silently dropped.
func (s LSPRouterEntrySnapshot) restorable() bool {
	return s.Present && (s.URL != "" || s.RelayURL != "")
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
				row.Raw = live.Raw
			}
			out = append(out, row)
		}
	}
	return out, nil
}

// RestoreLSPRouterClientEntriesSnapshot drives each recorded entry back to the
// exact state SnapshotLSPRouterClientEntries captured:
//
//   - recorded PRESENT with a URL -> rewrite the entry back to that URL AND
//     that disabled state (no-op when the live entry already matches both).
//     A row captured with an adapter-verbatim Raw value is replayed verbatim,
//     which is what makes the disabled bit restorable at all.
//   - recorded ABSENT -> remove the entry, but ONLY when the live entry is
//     still EXACTLY what this forward generation wrote for that language (the
//     router URL on opts.GUIPort, and for relay-shaped adapters the current
//     mcphub relay binary). Anything else is reported Skipped, never deleted.
//   - recorded PRESENT with neither URL nor RelayURL -> skipped. The forward
//     pass's own hub-owned guard (entryIsHubOwnedLSPClientEntry) could not
//     have rewritten such an entry, so there is nothing to reverse and
//     synthesizing a URL for it would be a fabrication.
//   - recorded for a client that is NOT reachable now (no adapter on this
//     host, or its config file no longer exists) -> reported Pending, never
//     silently treated as restored. See LSPClientRouterReport.Pending.
//
// ITERATION ORDER IS THE SNAPSHOT, NOT THE LIVE ADAPTER MAP. The snapshot is
// the authority on what must be put back; the adapter map is merely the means.
// Walking the adapter map instead silently drops every recorded row whose
// client is momentarily missing, and the caller — seeing a clean report —
// deletes the record, losing those rows permanently. Owners first, availability
// second.
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

	for _, clientName := range sortedSnapshotClientNames(byClient) {
		rows := byClient[clientName]
		adapter := clientMap[clientName]
		if adapter == nil || !adapter.Exists() {
			// The recorded client cannot be reached. Rows whose pre-state was
			// ABSENT are already satisfied (no config, no entry), but every
			// row that recorded a live entry still owes a restore and must
			// keep the record alive until the client comes back.
			for _, row := range rows {
				if !row.restorable() {
					continue
				}
				report.Pending = append(report.Pending, LSPClientRouterChange{
					Client: clientName, Language: row.Language,
					EntryName: snapshotEntryName(row), URL: snapshotPriorURL(row),
				})
			}
			continue
		}
		ops := make([]lspClientRouterOp, 0, len(rows))
		for _, row := range rows {
			entryName := snapshotEntryName(row)
			live, readErr := adapter.GetEntry(entryName)
			if readErr != nil {
				report.Failed = append(report.Failed, lspFailure(clientName, row.Language, entryName, "read", readErr))
				continue
			}

			if !row.Present {
				if live == nil {
					continue
				}
				// Removal is only correct for an entry THIS forward generation
				// created. Reserved-name ownership is not enough: the name is
				// reserved for the hub's WRITES, but an operator who creates
				// an entry under it after the cutover owns its content, and
				// deleting that is a data-loss bug wearing an ownership
				// argument. Require the exact URL + relay identity the forward
				// pass wrote (entryMatchesLSPRouter also excludes a disabled
				// entry — the forward pass creates them enabled, so a disabled
				// one has been touched since).
				if !entryMatchesLSPRouter(live, LSPRouterURL(port, row.Language)) {
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

			priorURL := snapshotPriorURL(row)
			if priorURL == "" {
				continue
			}
			// Already in the recorded pre-state, disabled bit included —
			// nothing to write. This check must precede the disabled-restore
			// guard below, so a row the forward pass never actually mutated is
			// not reported as unrestorable.
			if entryMatchesURL(live, priorURL) && live.Disabled == row.Disabled {
				continue
			}
			owned := false
			if live != nil {
				owned, _ = entryIsOwnedLSPRouterForLanguage(entryName, live, row.Language, port)
			}
			if live != nil && !owned {
				report.Failed = append(report.Failed, lspFailure(clientName, row.Language, entryName, "ownership",
					errors.New("live entry is not a hub-owned LSP router entry; refusing to overwrite")))
				continue
			}
			if row.Disabled && len(row.Raw) == 0 {
				// The pre-state was an entry the client does NOT load, and
				// this adapter gave us no verbatim value to replay, so
				// AddEntry (which has no disabled knob) can only put the URL
				// back ENABLED. Writing it anyway would hand the operator an
				// active entry pointing at a port this rollback is retiring.
				// Fail loudly instead: the caller keeps the record, and the
				// operator restores this one entry from the backup by hand.
				report.Failed = append(report.Failed, lspFailure(clientName, row.Language, entryName, "disabled-state",
					fmt.Errorf("pre-state was a DISABLED entry (%s) and the %s adapter cannot express that through AddEntry; refusing to restore it as ENABLED, which would leave an active entry pointing at a retired port", priorURL, clientName)))
				continue
			}
			entry, prepErr := lspLegacyMCPEntryForClient(opts, adapter, entryName, priorURL)
			if prepErr != nil {
				report.Failed = append(report.Failed, lspFailure(clientName, row.Language, entryName, "prepare", prepErr))
				continue
			}
			// Raw wins in the adapters that read it, so replaying it restores
			// the COMPLETE captured entry (disabled bit, and any shape the
			// lean MCPEntry cannot represent) rather than a URL projection of
			// it. Adapters that ignore Raw are byte-unchanged by this.
			entry.Raw = row.Raw
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

// snapshotEntryName is the recorded entry name, falling back to the canonical
// name for a row written before EntryName was populated.
func snapshotEntryName(row LSPRouterEntrySnapshot) string {
	if row.EntryName != "" {
		return row.EntryName
	}
	return LSPRouterEntryName(row.Language)
}

// snapshotPriorURL is the URL the restore drives the entry back to: the
// captured URL, or the relay URL for relay-shaped adapters that carry no
// plain URL.
func snapshotPriorURL(row LSPRouterEntrySnapshot) string {
	if row.URL != "" {
		return row.URL
	}
	return row.RelayURL
}

// sortedSnapshotClientNames orders the SNAPSHOT's own client keys. Distinct
// from sortedLSPClientNames (which orders a live adapter map) precisely
// because the restore must be driven by what was recorded, not by what
// happens to be installed right now.
func sortedSnapshotClientNames(byClient map[string][]LSPRouterEntrySnapshot) []string {
	names := make([]string, 0, len(byClient))
	for name := range byClient {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
