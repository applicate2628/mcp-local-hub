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
	"reflect"
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

type LSPRouterRecoveryRow struct {
	Baseline            LSPRouterEntrySnapshot  `json:"baseline"`
	Applied             *LSPRouterEntrySnapshot `json:"applied,omitempty"`
	Uncertain           bool                    `json:"uncertain,omitempty"`
	Disposition         string                  `json:"disposition,omitempty"`
	DispositionReason   string                  `json:"disposition_reason,omitempty"`
	DependencyEntryName string                  `json:"dependency_entry_name,omitempty"`
}

type LSPRouterRestoreStatus string

const (
	LSPRouterRestoreBaselineOnly LSPRouterRestoreStatus = "baseline-only"
	LSPRouterRestoreRestored     LSPRouterRestoreStatus = "restored"
	LSPRouterRestoreConflict     LSPRouterRestoreStatus = "skipped-conflict"
	LSPRouterRestorePending      LSPRouterRestoreStatus = "pending"
	LSPRouterRestoreFailed       LSPRouterRestoreStatus = "failed"
)

type LSPRouterRestoreRowResult struct {
	Client              string
	Language            string
	EntryName           string
	DependencyEntryName string
	Status              LSPRouterRestoreStatus
	Reason              string
	Err                 error
}

type LSPRouterRestoreCallbacks struct {
	BeforeMutation func(LSPRouterRestoreRowResult) error
	OnDisposition  func(LSPRouterRestoreRowResult) error
}

// lspSnapshotFromEntry is the SINGLE owner of "what is THIS adapter's own state
// for this entry" — the projection every plan pre-state, every compare-and-swap
// precondition, every applied receipt and every rollback compare in this family
// is built from.
//
// MULTI-LAYER ADAPTERS. `entry` comes from Client.GetEntry, which for mimocode
// returns the MERGED multi-layer view (its own write target, the layers above
// it, the operator's config.json below it, and the ~/.claude.json MCP import
// that MiMoCode reads as a compatibility layer). Every mutation in this family
// — AddEntry / RemoveEntry — touches ONLY the write target. So a merged entry
// carrying clients.MCPEntry.SourceBelowWriteTarget is projected as ABSENT: it
// is not in the write target, the hub never wrote it, and the hub cannot
// clobber it. That is the same contract mimocode's CAS compare object already
// encodes (clients.casWriteTargetEntry returns nil for a write target with no
// own value), and the same "remove the hub's key, let the lower/import layer
// re-emerge" polarity the install/register rollback sites use.
//
// WHY IT IS LOAD-BEARING, not cosmetic. Two failures, both reachable by a real
// operator running `mcphub install --reconcile-mcp-front` on a host with
// mimocode AND claude-code installed:
//
//   - COMPARE-vs-MUTATE. Projecting the merged view made the CAS precondition
//     compare an object the mutation does not write. Clients are applied in
//     sorted order, so claude-code's OWN rewrite of ~/.claude.json lands FIRST
//     and changes mimocode's merged view — the reconcile invalidated its own
//     plan, and every mimocode operation failed `precondition` (9 of them, one
//     per manifest language) on every run. Not a race with an outside actor:
//     deterministic, self-inflicted, and it fails the whole reconcile.
//   - RECORD CORRECTNESS. The pre-state is what `--rollback` restores. Recording
//     a claude-code-derived URL as mimocode's pre-state would make rollback
//     WRITE that URL into mimocode's own config — creating an entry that never
//     existed there and shadowing the operator's import layer forever. The
//     honest inverse of "the write target had nothing" is "remove it again".
//
// Single-file adapters never set SourceBelowWriteTarget, so their projection is
// unchanged.
//
// DIRECTIONALITY — this projection closes the BELOW direction only. An entry
// defined only in a layer ABOVE mimocode's write target (mimocode.jsonc,
// MIMOCODE_CONFIG, the overlay dir, inline content) does NOT set
// SourceBelowWriteTarget, so it projects as PRESENT with the higher layer's URL
// while the write target holds nothing — the compare object is again not the
// mutated object. That is not a live defect and the invariant is not maintained
// here: no other adapter can move mimocode's higher layers, and the ABOVE
// direction is owned by mimocode's own shadow guard, which refuses AddEntry for a
// higher-layer-defined name BEFORE any write — typed
// ErrMimoCodeOverlayShadowsServer (internal/clients/mimocode.go:885, contract at
// :105, restated at :3914) — so the case fails loud instead of silently comparing
// the wrong object. Read "by construction" in ea763851 as scoped to the below
// direction plus that guard, not as a claim that this function covers both.
func lspSnapshotFromEntry(clientName, language, entryName string, entry *clients.MCPEntry) LSPRouterEntrySnapshot {
	row := LSPRouterEntrySnapshot{Client: clientName, Language: language, EntryName: entryName}
	if entry == nil || entry.SourceBelowWriteTarget {
		return row
	}
	row.Present = true
	row.URL = entry.URL
	row.RelayURL = entry.RelayURL
	row.Disabled = entry.Disabled
	row.Raw = entry.Raw
	return row
}

func lspSnapshotStateEqual(a, b LSPRouterEntrySnapshot) bool {
	return a.Client == b.Client &&
		a.Language == b.Language &&
		snapshotEntryName(a) == snapshotEntryName(b) &&
		a.Present == b.Present &&
		a.URL == b.URL &&
		a.RelayURL == b.RelayURL &&
		a.Disabled == b.Disabled &&
		reflect.DeepEqual(a.Raw, b.Raw)
}

// ReadLSPRouterEntrySnapshot reads one exact entry projection. It is the shared
// settlement/readback owner for the version-3 journal.
func ReadLSPRouterEntrySnapshot(
	clientName, language, entryName string,
	clientMap map[string]clients.Client,
) (LSPRouterEntrySnapshot, error) {
	if clientMap == nil {
		clientMap = clients.AllClients()
	}
	adapter := clientMap[clientName]
	if adapter == nil || !adapter.Exists() {
		return LSPRouterEntrySnapshot{}, fmt.Errorf("client %s is unavailable", clientName)
	}
	entry, err := adapter.GetEntry(entryName)
	if err != nil {
		return LSPRouterEntrySnapshot{}, err
	}
	return lspSnapshotFromEntry(clientName, language, entryName, entry), nil
}

// RestoreLSPRouterRecoveryRows restores version-3 rows from exact applied
// ownership evidence. Dependency groups are processed legacy-first; the
// canonical route is preserved unless every required legacy inverse verifies.
func (a *API) RestoreLSPRouterRecoveryRows(
	rows []LSPRouterRecoveryRow,
	opts LSPClientRouterOpts,
	callbacks LSPRouterRestoreCallbacks,
) (*LSPClientRouterReport, []LSPRouterRestoreRowResult, error) {
	report := &LSPClientRouterReport{}
	if len(rows) == 0 {
		return report, nil, nil
	}
	clientMap := opts.Clients
	if clientMap == nil {
		clientMap = clients.AllClients()
	}
	keepN := opts.BackupKeepN
	if keepN == 0 {
		keepN = a.EffectiveBackupKeepN()
	}
	groups := map[string][]LSPRouterRecoveryRow{}
	var groupKeys []string
	for _, row := range rows {
		key := row.Baseline.Client + "\x00" + row.Baseline.Language
		if _, ok := groups[key]; !ok {
			groupKeys = append(groupKeys, key)
		}
		groups[key] = append(groups[key], row)
	}
	sort.Strings(groupKeys)
	var results []LSPRouterRestoreRowResult
	emit := func(result LSPRouterRestoreRowResult) error {
		results = append(results, result)
		if callbacks.OnDisposition != nil {
			return callbacks.OnDisposition(result)
		}
		return nil
	}
	backedUp := map[string]bool{}
	for _, groupKey := range groupKeys {
		groupRows := groups[groupKey]
		sort.SliceStable(groupRows, func(i, j int) bool {
			iCanonical := snapshotEntryName(groupRows[i].Baseline) == LSPRouterEntryName(groupRows[i].Baseline.Language)
			jCanonical := snapshotEntryName(groupRows[j].Baseline) == LSPRouterEntryName(groupRows[j].Baseline.Language)
			if iCanonical != jCanonical {
				return !iCanonical
			}
			return snapshotEntryName(groupRows[i].Baseline) < snapshotEntryName(groupRows[j].Baseline)
		})
		legacyRetryableBarrier := false
		legacyConflictBarrier := false
		legacyConflictEntryName := ""
		markLegacyConflict := func(entryName string) {
			legacyConflictBarrier = true
			if legacyConflictEntryName == "" {
				legacyConflictEntryName = entryName
			}
		}
		for _, recovery := range groupRows {
			row := recovery.Baseline
			entryName := snapshotEntryName(row)
			isCanonical := entryName == LSPRouterEntryName(row.Language)
			legacyReadinessDisposition := !isCanonical &&
				(recovery.Disposition == string(LSPRouterRestoreBaselineOnly) ||
					recovery.Disposition == string(LSPRouterRestoreRestored))
			if isCanonical && (legacyRetryableBarrier || legacyConflictBarrier) {
				result := LSPRouterRestoreRowResult{
					Client: row.Client, Language: row.Language, EntryName: entryName,
					DependencyEntryName: legacyConflictEntryName,
				}
				if legacyConflictBarrier {
					result.Status = LSPRouterRestoreConflict
					result.Reason = "skipped-dependency-conflict"
					report.Skipped = append(report.Skipped, LSPClientRouterChange{
						Client: row.Client, Language: row.Language, EntryName: entryName,
					})
				} else {
					result.Status = LSPRouterRestorePending
					result.Reason = "rollback-route-preservation-blocked"
					report.Pending = append(report.Pending, LSPClientRouterChange{
						Client: row.Client, Language: row.Language, EntryName: entryName,
					})
				}
				if err := emit(result); err != nil {
					return report, results, err
				}
				continue
			}
			if recovery.Disposition == string(LSPRouterRestoreConflict) {
				if !isCanonical {
					markLegacyConflict(entryName)
				}
				report.Skipped = append(report.Skipped, LSPClientRouterChange{
					Client: row.Client, Language: row.Language, EntryName: entryName,
				})
				if err := emit(LSPRouterRestoreRowResult{
					Client: row.Client, Language: row.Language, EntryName: entryName,
					DependencyEntryName: recovery.DependencyEntryName,
					Status:              LSPRouterRestoreConflict, Reason: recovery.DispositionReason,
				}); err != nil {
					return report, results, err
				}
				continue
			}
			if isCanonical &&
				(recovery.Disposition == string(LSPRouterRestoreBaselineOnly) ||
					recovery.Disposition == string(LSPRouterRestoreRestored)) {
				status := LSPRouterRestoreBaselineOnly
				if recovery.Disposition == string(LSPRouterRestoreRestored) {
					status = LSPRouterRestoreRestored
				}
				if err := emit(LSPRouterRestoreRowResult{
					Client: row.Client, Language: row.Language, EntryName: entryName,
					DependencyEntryName: recovery.DependencyEntryName,
					Status:              status, Reason: recovery.DispositionReason,
				}); err != nil {
					return report, results, err
				}
				continue
			}
			if recovery.Uncertain {
				result := LSPRouterRestoreRowResult{
					Client: row.Client, Language: row.Language, EntryName: entryName,
					Status: LSPRouterRestorePending, Reason: "pending-ownership-unknown",
				}
				report.Pending = append(report.Pending, LSPClientRouterChange{Client: row.Client, Language: row.Language, EntryName: entryName})
				if !isCanonical {
					legacyRetryableBarrier = true
				}
				if err := emit(result); err != nil {
					return report, results, err
				}
				continue
			}
			adapter := clientMap[row.Client]
			if adapter == nil || !adapter.Exists() {
				if recovery.Applied == nil && isCanonical {
					if err := emit(LSPRouterRestoreRowResult{
						Client: row.Client, Language: row.Language, EntryName: entryName,
						Status: LSPRouterRestoreBaselineOnly, Reason: "no-effective-applied-receipt",
					}); err != nil {
						return report, results, err
					}
					continue
				}
				result := LSPRouterRestoreRowResult{
					Client: row.Client, Language: row.Language, EntryName: entryName,
					Status: LSPRouterRestorePending, Reason: "rollback-client-unreachable",
				}
				report.Pending = append(report.Pending, LSPClientRouterChange{Client: row.Client, Language: row.Language, EntryName: entryName})
				if !isCanonical {
					legacyRetryableBarrier = true
				}
				if err := emit(result); err != nil {
					return report, results, err
				}
				continue
			}
			liveEntry, readErr := adapter.GetEntry(entryName)
			if readErr != nil {
				if recovery.Applied == nil && !isCanonical {
					legacyRetryableBarrier = true
				}
				result := LSPRouterRestoreRowResult{
					Client: row.Client, Language: row.Language, EntryName: entryName,
					Status: LSPRouterRestoreFailed, Reason: "rollback-read-failed", Err: readErr,
				}
				report.Failed = append(report.Failed, lspFailure(row.Client, row.Language, entryName, "read", readErr))
				if !isCanonical {
					legacyRetryableBarrier = true
				}
				if err := emit(result); err != nil {
					return report, results, err
				}
				continue
			}
			live := lspSnapshotFromEntry(row.Client, row.Language, entryName, liveEntry)
			if legacyReadinessDisposition {
				if legacyRouteReady(live, row) {
					status := LSPRouterRestoreBaselineOnly
					if recovery.Disposition == string(LSPRouterRestoreRestored) {
						status = LSPRouterRestoreRestored
					}
					if err := emit(LSPRouterRestoreRowResult{
						Client: row.Client, Language: row.Language, EntryName: entryName,
						Status: status, Reason: "legacy-baseline-live",
					}); err != nil {
						return report, results, err
					}
				} else {
					markLegacyConflict(entryName)
					report.Skipped = append(report.Skipped, LSPClientRouterChange{
						Client: row.Client, Language: row.Language, EntryName: entryName,
					})
					if err := emit(LSPRouterRestoreRowResult{
						Client: row.Client, Language: row.Language, EntryName: entryName,
						Status: LSPRouterRestoreConflict, Reason: "legacy-baseline-not-routable",
					}); err != nil {
						return report, results, err
					}
				}
				continue
			}
			if recovery.Applied == nil {
				if isCanonical {
					if err := emit(LSPRouterRestoreRowResult{
						Client: row.Client, Language: row.Language, EntryName: entryName,
						Status: LSPRouterRestoreBaselineOnly, Reason: "no-effective-applied-receipt",
					}); err != nil {
						return report, results, err
					}
					continue
				}
				if legacyRouteReady(live, row) {
					if err := emit(LSPRouterRestoreRowResult{
						Client: row.Client, Language: row.Language, EntryName: entryName,
						Status: LSPRouterRestoreBaselineOnly, Reason: "legacy-baseline-live",
					}); err != nil {
						return report, results, err
					}
				} else {
					markLegacyConflict(entryName)
					report.Skipped = append(report.Skipped, LSPClientRouterChange{
						Client: row.Client, Language: row.Language, EntryName: entryName,
					})
					if err := emit(LSPRouterRestoreRowResult{
						Client: row.Client, Language: row.Language, EntryName: entryName,
						Status: LSPRouterRestoreConflict, Reason: "legacy-baseline-not-routable",
					}); err != nil {
						return report, results, err
					}
				}
				continue
			}
			if lspSnapshotStateEqual(live, row) {
				if !isCanonical && !legacyRouteReady(live, row) {
					markLegacyConflict(entryName)
					report.Skipped = append(report.Skipped, LSPClientRouterChange{
						Client: row.Client, Language: row.Language, EntryName: entryName,
					})
					if err := emit(LSPRouterRestoreRowResult{
						Client: row.Client, Language: row.Language, EntryName: entryName,
						Status: LSPRouterRestoreConflict, Reason: "legacy-baseline-not-routable",
					}); err != nil {
						return report, results, err
					}
					continue
				}
				if err := emit(LSPRouterRestoreRowResult{
					Client: row.Client, Language: row.Language, EntryName: entryName,
					Status: LSPRouterRestoreRestored, Reason: "already-baseline",
				}); err != nil {
					return report, results, err
				}
				continue
			}
			if !lspSnapshotStateEqual(live, *recovery.Applied) {
				result := LSPRouterRestoreRowResult{
					Client: row.Client, Language: row.Language, EntryName: entryName,
					Status: LSPRouterRestoreConflict, Reason: "rollback-live-diverged",
				}
				report.Skipped = append(report.Skipped, LSPClientRouterChange{
					Client: row.Client, Language: row.Language, EntryName: entryName, URL: snapshotPriorURL(live),
				})
				if !isCanonical {
					markLegacyConflict(entryName)
				}
				if err := emit(result); err != nil {
					return report, results, err
				}
				continue
			}
			mutator, ok := adapter.(clients.ConditionalEntryMutator)
			if !ok {
				capabilityErr := errors.New("adapter lacks conditional entry mutation capability")
				result := LSPRouterRestoreRowResult{
					Client: row.Client, Language: row.Language, EntryName: entryName,
					Status: LSPRouterRestoreFailed, Reason: "rollback-capability-missing", Err: capabilityErr,
				}
				report.Failed = append(report.Failed, lspFailure(row.Client, row.Language, entryName, "capability", capabilityErr))
				if !isCanonical {
					legacyRetryableBarrier = true
				}
				if err := emit(result); err != nil {
					return report, results, err
				}
				continue
			}
			var operation clients.EntryMutationOperation
			var inverseEntry clients.MCPEntry
			if !row.Present {
				operation = clients.EntryMutationRemove
			} else {
				priorURL := snapshotPriorURL(row)
				var prepErr error
				inverseEntry, prepErr = lspLegacyMCPEntryForClient(opts, adapter, entryName, priorURL)
				if prepErr != nil {
					result := LSPRouterRestoreRowResult{
						Client: row.Client, Language: row.Language, EntryName: entryName,
						Status: LSPRouterRestoreFailed, Reason: "rollback-prepare-failed", Err: prepErr,
					}
					report.Failed = append(report.Failed, lspFailure(row.Client, row.Language, entryName, "prepare", prepErr))
					if !isCanonical {
						legacyRetryableBarrier = true
					}
					if err := emit(result); err != nil {
						return report, results, err
					}
					continue
				}
				inverseEntry.Raw = row.Raw
				operation = clients.EntryMutationAdd
			}
			var backupKeepN *int
			if !backedUp[row.Client] {
				backupKeepN = &keepN
			}
			var durablePrepareErr error
			request := clients.ConditionalEntryMutationRequest{
				EntryName: entryName,
				ExpectedLive: func(entry *clients.MCPEntry) bool {
					return lspSnapshotStateEqual(
						lspSnapshotFromEntry(row.Client, row.Language, entryName, entry),
						*recovery.Applied,
					)
				},
				BackupKeepN: backupKeepN,
				Operation:   operation,
				Entry:       inverseEntry,
				BeforeMutation: func(clients.EntryMutationPreparation) error {
					if callbacks.BeforeMutation != nil {
						durablePrepareErr = callbacks.BeforeMutation(LSPRouterRestoreRowResult{
							Client: row.Client, Language: row.Language, EntryName: entryName,
							Status: LSPRouterRestorePending, Reason: "rollback-prepared",
						})
					}
					return durablePrepareErr
				},
			}
			var observed clients.EntryMutationObserved
			conflictScope := ""
			conflictEntryName := ""
			var dependencyFailure *clients.EntryMutationDependencyFailure
			if isCanonical {
				dependencies := lspRollbackCanonicalDependencies(groupRows, row.Client, row.Language)
				if len(dependencies) == 0 {
					observed = mutator.ConditionalEntryMutation(request)
				} else {
					groupMutator, groupOK := adapter.(clients.ConditionalEntryGroupMutator)
					if !groupOK {
						capabilityErr := errors.New("adapter lacks conditional entry group mutation capability")
						result := LSPRouterRestoreRowResult{
							Client: row.Client, Language: row.Language, EntryName: entryName,
							Status: LSPRouterRestorePending, Reason: "rollback-route-preservation-blocked", Err: capabilityErr,
						}
						report.Pending = append(report.Pending, LSPClientRouterChange{Client: row.Client, Language: row.Language, EntryName: entryName})
						if err := emit(result); err != nil {
							return report, results, err
						}
						continue
					}
					groupObserved := groupMutator.ConditionalEntryGroupMutation(clients.ConditionalEntryGroupMutationRequest{
						ConditionalEntryMutationRequest: request,
						Dependencies:                    dependencies,
					})
					observed = groupObserved.EntryMutationObserved
					conflictScope = groupObserved.ConflictScope
					conflictEntryName = groupObserved.ConflictEntryName
					dependencyFailure = groupObserved.DependencyFailure
				}
			} else {
				observed = mutator.ConditionalEntryMutation(request)
			}
			if observed.BackupPath != "" {
				report.Backups = append(report.Backups, LSPClientRouterBackup{Client: row.Client, Path: observed.BackupPath})
				backedUp[row.Client] = true
			}
			if durablePrepareErr != nil {
				return report, results, durablePrepareErr
			}
			if observed.PreconditionConflict {
				reason := "rollback-live-diverged"
				if isCanonical && conflictScope == "dependency" {
					reason = "skipped-dependency-conflict"
				}
				dependencyEntryName := ""
				if conflictScope == "dependency" {
					dependencyEntryName = conflictEntryName
				}
				if dependencyFailure != nil {
					dependencyEntryName = dependencyFailure.EntryName
				}
				result := LSPRouterRestoreRowResult{
					Client: row.Client, Language: row.Language, EntryName: entryName,
					DependencyEntryName: dependencyEntryName,
					Status:              LSPRouterRestoreConflict, Reason: reason,
				}
				report.Skipped = append(report.Skipped, LSPClientRouterChange{
					Client: row.Client, Language: row.Language, EntryName: entryName,
				})
				if !isCanonical {
					markLegacyConflict(entryName)
				}
				if err := emit(result); err != nil {
					return report, results, err
				}
				continue
			}
			if isCanonical && !observed.Invoked && observed.ObservationErr != nil {
				dependencyEntryName := ""
				if dependencyFailure != nil {
					dependencyEntryName = dependencyFailure.EntryName
				}
				result := LSPRouterRestoreRowResult{
					Client: row.Client, Language: row.Language, EntryName: entryName,
					DependencyEntryName: dependencyEntryName,
					Status:              LSPRouterRestorePending, Reason: "rollback-route-preservation-blocked", Err: observed.ObservationErr,
				}
				report.Pending = append(report.Pending, LSPClientRouterChange{Client: row.Client, Language: row.Language, EntryName: entryName})
				if err := emit(result); err != nil {
					return report, results, err
				}
				continue
			}
			if isCanonical && observed.Invoked && dependencyFailure != nil {
				result := LSPRouterRestoreRowResult{
					Client:              row.Client,
					Language:            row.Language,
					EntryName:           entryName,
					DependencyEntryName: dependencyFailure.EntryName,
					Status:              LSPRouterRestorePending,
					Reason:              "rollback-ownership-unknown",
					Err:                 dependencyFailure.Cause,
				}
				report.Pending = append(report.Pending, LSPClientRouterChange{Client: row.Client, Language: row.Language, EntryName: entryName})
				if err := emit(result); err != nil {
					return report, results, err
				}
				continue
			}
			if observed.PreparationErr != nil || observed.MutationErr != nil {
				inverseErr := observed.PreparationErr
				if inverseErr == nil {
					inverseErr = observed.MutationErr
				}
				result := LSPRouterRestoreRowResult{
					Client: row.Client, Language: row.Language, EntryName: entryName,
					Status: LSPRouterRestoreFailed, Reason: "rollback-write-failed", Err: inverseErr,
				}
				report.Failed = append(report.Failed, lspFailure(row.Client, row.Language, entryName, "inverse", inverseErr))
				if !isCanonical {
					legacyRetryableBarrier = true
				}
				if err := emit(result); err != nil {
					return report, results, err
				}
				continue
			}
			verified := lspSnapshotFromEntry(row.Client, row.Language, entryName, observed.After)
			verifyErr := observed.ObservationErr
			if verifyErr != nil || !lspSnapshotStateEqual(verified, row) {
				if verifyErr == nil {
					verifyErr = errors.New("inverse readback differs from immutable baseline")
				}
				result := LSPRouterRestoreRowResult{
					Client: row.Client, Language: row.Language, EntryName: entryName,
					Status: LSPRouterRestoreFailed, Reason: "rollback-verify-failed", Err: verifyErr,
				}
				report.Failed = append(report.Failed, lspFailure(row.Client, row.Language, entryName, "verify", verifyErr))
				if !isCanonical {
					legacyRetryableBarrier = true
				}
				if err := emit(result); err != nil {
					return report, results, err
				}
				continue
			}
			if !isCanonical && !legacyRouteReady(verified, row) {
				markLegacyConflict(entryName)
				report.Skipped = append(report.Skipped, LSPClientRouterChange{
					Client: row.Client, Language: row.Language, EntryName: entryName,
				})
				if err := emit(LSPRouterRestoreRowResult{
					Client: row.Client, Language: row.Language, EntryName: entryName,
					Status: LSPRouterRestoreConflict, Reason: "legacy-baseline-not-routable",
				}); err != nil {
					return report, results, err
				}
				continue
			}
			if row.Present {
				report.Applied = append(report.Applied, LSPClientRouterChange{
					Client: row.Client, Language: row.Language, EntryName: entryName, URL: snapshotPriorURL(row),
				})
			} else {
				report.Removed = append(report.Removed, LSPClientRouterChange{Client: row.Client, Language: row.Language, EntryName: entryName})
			}
			if err := emit(LSPRouterRestoreRowResult{
				Client: row.Client, Language: row.Language, EntryName: entryName,
				Status: LSPRouterRestoreRestored, Reason: "inverse-verified",
			}); err != nil {
				return report, results, err
			}
		}
	}
	return report, results, lspRouterReportError(report, "lsp client router recovery restore")
}

// lspRollbackCanonicalDependencies returns the exact legacy baseline predicates
// required before a canonical inverse may proceed. The wrapper evaluates these
// under the canonical target's config lock; prior barrier observations only
// provide ordering and must never authorize the mutation.
func lspRollbackCanonicalDependencies(groupRows []LSPRouterRecoveryRow, clientName, language string) []clients.EntryMutationDependency {
	dependencies := make([]clients.EntryMutationDependency, 0, len(groupRows))
	canonicalName := LSPRouterEntryName(language)
	for _, recovery := range groupRows {
		baseline := recovery.Baseline
		entryName := snapshotEntryName(baseline)
		if entryName == canonicalName {
			continue
		}
		baselineCopy := baseline
		dependencies = append(dependencies, clients.EntryMutationDependency{
			EntryName: entryName,
			ExpectedLive: func(entry *clients.MCPEntry) bool {
				return legacyRouteReady(
					lspSnapshotFromEntry(clientName, language, entryName, entry),
					baselineCopy,
				)
			},
		})
	}
	return dependencies
}

// legacyRouteReady is the sole dependency predicate for removing/restoring the
// canonical route. Exact baseline equality alone is insufficient: the legacy
// entry must also be present, enabled, and carry a routable URL shape.
func legacyRouteReady(live, baseline LSPRouterEntrySnapshot) bool {
	return lspSnapshotStateEqual(live, baseline) &&
		live.Present &&
		!live.Disabled &&
		snapshotPriorURL(live) != ""
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
// BOTH MUTATED SHAPES ARE CAPTURED (codex bot PR #588). The forward pass does
// not only rewrite the canonical `mcp-language-server-<language>` entry — it
// also DELETES every legacy registry-backed per-workspace entry that still
// points at a registry-owned proxy port. Capturing only the canonical shape
// left a client on the legacy shape with its true pre-state unrecorded: the
// cutover removed those entries and the rollback, which iterates this
// snapshot, had nothing to put back. Both sides now derive the legacy set from
// the one owner (collectLegacyLSPEntriesToMigrate), so the captured surface
// equals the mutated surface by construction rather than by two authors
// agreeing.
//
// opts.GUIPort is not consulted — this is a pure read of what is on disk now.
func (a *API) SnapshotLSPRouterClientEntries(opts LSPClientRouterOpts) ([]LSPRouterEntrySnapshot, error) {
	languages, err := loadLSPRouterLanguages(opts.Languages)
	if err != nil {
		return nil, err
	}
	// The legacy shape is defined by the registry (which per-workspace entry
	// names exist, and which proxy ports they may point at), exactly as the
	// forward pass resolves it.
	regEntries, err := loadLSPRouterRegistryEntries()
	if err != nil {
		return nil, fmt.Errorf("snapshot lsp router pre-state: load registry: %w", err)
	}
	portsByLanguage := lspRegistryPortsByLanguage(regEntries)
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
			// Projection goes through the family's one owner: it also decides
			// what a multi-layer adapter's OWN state is (see
			// lspSnapshotFromEntry). Re-typing the field copy here would let
			// this capture drift from the pre-state the plan compares against.
			out = append(out, lspSnapshotFromEntry(clientName, language, entryName, live))

			// Legacy per-workspace entries the forward pass will DELETE. Only
			// entries that are present AND legacy-shaped are recorded — the
			// same predicate that decides the deletion — so the snapshot
			// carries a restorable row for each one and nothing else.
			legacyEntries, legacyReadErrs := collectLegacyLSPEntriesToMigrate(
				adapter, regEntries, portsByLanguage[language], language, clientName, entryName)
			if len(legacyReadErrs) > 0 {
				// FAIL-CLOSED, same posture as the canonical read above: an
				// unreadable candidate must not be emitted as (or silently
				// reduced to) "no legacy entry here", because the forward pass
				// may still delete it and the rollback would then have no row.
				first := legacyReadErrs[0]
				return nil, fmt.Errorf("snapshot lsp router pre-state: read %s legacy entry %s: %w", clientName, first.Name, first.Err)
			}
			for _, legacy := range legacyEntries {
				out = append(out, lspSnapshotFromEntry(clientName, language, legacy.Name, legacy.Entry))
			}
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
