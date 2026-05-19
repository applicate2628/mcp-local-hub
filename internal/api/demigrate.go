package api

import (
	"errors"
	"fmt"
	"io"
	"strings"

	"mcp-local-hub/internal/clients"
	"mcp-local-hub/internal/config"
)

// errBackupMissingEntry signals "this backup file does not contain
// the named entry" inside the demigrate iteration. This is only
// skippable for the pristine "-original" sentinel (which may predate
// later user installs). For timestamped backups, absence is the
// immediate pre-migrate state and must be restored (remove live entry).
var errBackupMissingEntry = errors.New("backup does not contain entry")

// DemigrateOpts controls a reverse-migration invocation. Semantics mirror
// MigrateOpts: the manifest drives the client-binding set, ClientsInclude
// narrows that set, and Writer receives human-readable progress.
type DemigrateOpts struct {
	Servers        []string
	ClientsInclude []string
	ScanOpts       ScanOpts
	Writer         io.Writer
}

// DemigrateReport carries per-(server, client) outcomes.
type DemigrateReport struct {
	Restored []RestoredMigration `json:"restored"`
	Failed   []FailedMigration   `json:"failed"` // reuses migrate's failure shape
}

// RestoredMigration is one successfully rolled-back (server, client) pair.
type RestoredMigration struct {
	Server string `json:"server"`
	Client string `json:"client"`
}

// Demigrate rolls (server, client) pairs back from hub-HTTP to their
// pre-migrate shape by reading each client's most recent backup and
// writing the named entry (or removing it, if the backup predates
// migrate adding it). The set of (server, client) pairs is derived
// from each server's manifest.client_bindings intersected with
// ClientsInclude — mirroring MigrateFrom's shape so Demigrate reverses
// exactly the rows Migrate would produce. Entries in other clients with
// the same server name are NOT touched.
//
// Multi-server / repeat-migrate behavior: when multiple servers are
// migrated from the same client — or the same server is migrated more
// than once — the latest timestamped backup may already hold earlier
// entries in hub-managed form. For those, Demigrate falls back
// automatically to the pristine `-original` sentinel (the one-shot
// pre-hub snapshot Client.Backup() writes on first call; never
// overwritten) — but only if the sentinel actually contains the
// named entry (verified via Client.BackupContainsEntry). If the
// sentinel lacks the entry, the server must have been added AFTER
// the sentinel was written, so auto-rollback from the sentinel would
// silently DELETE the user-configured entry — Demigrate refuses and
// reports a Failed row directing the operator to inspect older
// timestamped backups manually. If both the latest backup AND the
// sentinel refuse for any other reason (sentinel tampered with or
// unreadable), Demigrate surfaces a Failed row naming both paths.
// Silent rewriting of hub-managed data and silent deletion of user
// entries are both strictly worse than a clear failure.
//
// Errors per-(server, client) are captured in the report; the function
// returns nil unless a setup-level problem applies to every row.
func (a *API) Demigrate(opts DemigrateOpts) (*DemigrateReport, error) {
	if opts.Writer == nil {
		opts.Writer = io.Discard
	}
	report := &DemigrateReport{}
	allClients := clients.AllClients()

	includedClient := func(c string) bool {
		if len(opts.ClientsInclude) == 0 {
			return true
		}
		for _, x := range opts.ClientsInclude {
			if x == c {
				return true
			}
		}
		return false
	}

	for _, server := range opts.Servers {
		m, err := loadManifestForServer(opts.ScanOpts.ManifestDir, server)
		if err != nil {
			report.Failed = append(report.Failed, FailedMigration{
				Server: server, Err: err.Error(),
			})
			continue
		}
		for _, binding := range m.ClientBindings {
			if !includedClient(binding.Client) {
				continue
			}
			adapter := allClients[binding.Client]
			if adapter == nil {
				continue
			}
			if !adapter.Exists() {
				continue
			}
			// Iterate every mcp-local-hub backup newest-first.
			// Rationale: the lexicographically-latest timestamped
			// backup may hold the entry in hub-managed form (operator
			// migrated, snapshot of post-migrate state), but an OLDER
			// timestamped backup may hold the entry in its pre-hub
			// direct/stdio form (the user-installed shape captured
			// before the first migrate). Returning that older form is
			// the correct rollback target. The post-revert single-
			// backup check at this site was destructive when called
			// through the marker+RemoveEntry helper — see
			// work-items/bugs/2026-05-15-demigrate-fallback-when-no-pre-hub-form.md
			// §"Failed attempt: PR #218".
			//
			// Iteration is bounded by the operator's BackupKeep
			// retention setting (default 5 timestamped + 1 sentinel),
			// so this loop is small for normal hosts.
			backups, err := clients.BackupsNewestFirst(adapter.ConfigPath(), binding.Client)
			if err != nil {
				report.Failed = append(report.Failed, FailedMigration{
					Server: server, Client: binding.Client, Err: err.Error(),
				})
				continue
			}
			if len(backups) == 0 {
				report.Failed = append(report.Failed, FailedMigration{
					Server: server, Client: binding.Client,
					Err: "no backup found (migration may never have run on this machine)",
				})
				continue
			}
			latestBackupPath := backups[0]
			// restoreIfEligible returns nil on success, or one of
			// three error classes the iteration treats as "skip this
			// backup, try older":
			//
			//   1. errBackupMissingEntry — sentinel backup does not
			//      contain the entry (predates the server install).
			//      Skip and keep searching older candidates.
			//   2. clients.ErrBackupEntryAlreadyMigrated — backup
			//      contains the entry in hub-managed form. No
			//      pre-hub state to restore; skip.
			//   3. backup-unreadable — bubble up as a hard error
			//      (the OS-level read failed; not iteratable).
			restoreIfEligible := func(path string) error {
				has, err := adapter.BackupContainsEntry(path, server)
				if err != nil {
					return fmt.Errorf("backup %s unreadable: %w", path, err)
				}
				if !has {
					if !strings.HasSuffix(path, ".bak-mcp-local-hub-original") {
						return adapter.RestoreEntryFromBackup(path, server)
					}
					return errBackupMissingEntry
				}
				return adapter.RestoreEntryFromBackup(path, server)
			}
			restoredFrom := ""
			var lastSkipReason error
			for _, candidate := range backups {
				rerr := restoreIfEligible(candidate)
				if rerr == nil {
					restoredFrom = candidate
					err = nil
					break
				}
				if errors.Is(rerr, clients.ErrBackupEntryAlreadyMigrated) ||
					errors.Is(rerr, errBackupMissingEntry) {
					lastSkipReason = rerr
					continue
				}
				// Hard error (unreadable, malformed, etc.) — stop
				// iteration; surface it as the failure cause.
				err = rerr
				break
			}
			// If iteration exhausted every backup without a restore
			// (all skipped via ErrBackupEntryAlreadyMigrated or
			// errBackupMissingEntry), there is no pre-hub form to
			// recover. Fall back to marker+backfill+RemoveEntry —
			// the ONLY case where this is safe is when positive
			// ownership evidence proves mcphub installed the entry
			// AND no backup captures a pre-hub shape (the entry
			// genuinely never existed in pre-hub form). The
			// iteration above guarantees the "no pre-hub form
			// available" precondition; the marker check guarantees
			// the "mcphub installed it" precondition. Together they
			// produce the strict subset of cases where RemoveEntry
			// is the correct rollback target.
			if restoredFrom == "" && err == nil {
				err = tryMarkerOrBackfillRemove(
					adapter, binding, m, server,
					fmt.Sprintf("no backup (of %d candidates, newest %s) contains %q in pre-hub form — last skip: %v",
						len(backups), latestBackupPath, server, lastSkipReason),
					&restoredFrom,
				)
			}
			if err != nil {
				report.Failed = append(report.Failed, FailedMigration{
					Server: server, Client: binding.Client, Err: err.Error(),
				})
				continue
			}
			report.Restored = append(report.Restored, RestoredMigration{
				Server: server, Client: binding.Client,
			})
			fmt.Fprintf(opts.Writer, "restored %s for %s from %s\n", server, binding.Client, restoredFrom)
		}
	}
	return report, nil
}

// tryMarkerOrBackfillRemove is the LAST-RESORT recovery path used by
// Demigrate when iterating every backup newest-first found NO
// candidate with the entry in pre-hub form. Two precondition
// guarantees from the caller's iteration:
//
//   1. Every backup (timestamped + sentinel) either lacks the entry
//      entirely OR holds it in hub-managed form.
//   2. The live config still has the entry in hub-managed form
//      (otherwise demigrate would have nothing to roll back).
//
// Under both preconditions there is no recoverable pre-hub state,
// so the rollback target is one of:
//
//   a. RemoveEntry — when positive ownership evidence shows mcphub
//      installed the entry (marker file says so, or backfill helper
//      confirms the live URL exactly matches the manifest binding).
//   b. Fail-closed — otherwise, the entry MIGHT be user-owned and
//      indistinguishable from a mcphub install via these signals;
//      refuse to delete it. Operator workaround: edit the config
//      manually.
//
// Evidence sources, in order:
//
//   1. Managed-entries marker (PR #187). Populated by migrate.go
//      AFTER successful adapter.AddEntry. A row for (client, server)
//      means mcphub installed this entry.
//   2. Backfill (PR #186 r2 / #192 r6). For v0.4.x users whose
//      entries predate the marker subsystem: when the live entry's
//      URL exactly equals what mcphub would have written
//      (http://localhost:<daemon.port><url_path> or 127.0.0.1 / [::1]
//      / Antigravity relay shape), record the marker inline and
//      treat as mcphub-owned.
//
// On RemoveEntry success the marker row is forgotten (best-effort;
// stale rows self-heal on next migrate).
//
// reasonPrefix is the human-readable description of WHY iteration
// failed to find a pre-hub form (used as the leading clause of any
// error surfaced from this helper). restoredFrom is updated on
// success to describe how the rollback was achieved.
func tryMarkerOrBackfillRemove(
	adapter clients.Client,
	binding config.ClientBinding,
	m *config.ServerManifest,
	server string,
	reasonPrefix string,
	restoredFrom *string,
) error {
	managed, mErr := IsManagedEntry(binding.Client, server)
	if !managed && mErr == nil {
		// v0.4.x upgrade backfill: existing users have hub-form
		// entries that were never marked (B4 marker introduced in
		// PR #187 only marks fresh migrates).
		if backfilled := backfillMarkerIfEntryMatchesManifest(adapter, server, binding, m); backfilled {
			managed = true
		}
	}
	switch {
	case mErr != nil:
		return fmt.Errorf("%s, AND consulting managed-entries marker failed: %w",
			reasonPrefix, mErr)
	case !managed:
		return fmt.Errorf("%s, but managed-entries marker has no record that mcphub installed this entry — refusing to RemoveEntry (entry may be user-owned); to roll back this entry, edit %s manually, or re-run migrate first to populate the marker",
			reasonPrefix, adapter.ConfigPath())
	default:
		if rmErr := adapter.RemoveEntry(server); rmErr != nil {
			return fmt.Errorf("%s, marker confirmed mcphub-managed, AND RemoveEntry failed: %w",
				reasonPrefix, rmErr)
		}
		if fErr := ForgetManagedEntry(binding.Client, server); fErr != nil {
			_ = LogHubMcpEvent("warn", "managed-entries-forget-failed", map[string]any{
				"server": server,
				"client": binding.Client,
				"err":    fErr.Error(),
			})
		}
		*restoredFrom = "(no pre-hub form in any backup; marker confirmed mcphub-managed; removed entry from client config)"
		return nil
	}
}
