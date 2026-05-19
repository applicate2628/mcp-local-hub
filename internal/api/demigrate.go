package api

import (
	"errors"
	"fmt"
	"io"

	"mcp-local-hub/internal/clients"
	"mcp-local-hub/internal/config"
)

// errSentinelMissingEntry is returned by the demigrate safe-restore
// helper when the sentinel backup exists but does not contain the
// named entry — meaning the entry was added AFTER mcphub first
// touched the client config. Splitting this out as a sentinel error
// lets the marker-fallback recovery path catch this case via
// errors.Is and route it through the same marker+backfill gate as
// the both-hub-managed case. Previously this branch went straight
// to a "sentinel fallback failed" wrapper error, bypassing the
// marker check and blocking demigrate on the common "entry
// installed after first migrate" scenario.
var errSentinelMissingEntry = errors.New("sentinel does not contain entry")

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
			backupPath, ok, err := adapter.LatestBackupPath()
			if err != nil {
				report.Failed = append(report.Failed, FailedMigration{
					Server: server, Client: binding.Client, Err: err.Error(),
				})
				continue
			}
			if !ok {
				report.Failed = append(report.Failed, FailedMigration{
					Server: server, Client: binding.Client,
					Err: "no backup found (migration may never have run on this machine)",
				})
				continue
			}
			sentinelPath := adapter.ConfigPath() + ".bak-mcp-local-hub-original"
			// safeRestore wraps adapter.RestoreEntryFromBackup with a
			// containment pre-check when the backup path is the
			// -original sentinel. Rationale: RestoreEntryFromBackup
			// silently deletes the live entry when the backup lacks
			// it. That's correct semantics when the backup is a
			// TIMESTAMPED snapshot taken right before migrate (migrate
			// added the server from scratch, so demigrate removes it).
			// But when the backup is the pristine sentinel AND the
			// sentinel lacks the entry, the server must have been
			// added AFTER mcphub first touched the config — silently
			// deleting it is destructive, not a rollback. Refuse in
			// that case with a clear message. The main flow can reach
			// this scenario whenever `LatestBackupPath` returns the
			// sentinel directly (e.g. timestamped backups were
			// pruned); the ErrBackupEntryAlreadyMigrated fallback
			// reaches it explicitly.
			safeRestore := func(path string) error {
				if path == sentinelPath {
					has, err := adapter.BackupContainsEntry(path, server)
					if err != nil {
						return fmt.Errorf("sentinel %s unreadable: %w", path, err)
					}
					if !has {
						// Wrap the sentinel-missing-entry case in a
						// typed sentinel error so the marker-fallback
						// recovery path can errors.Is-detect it and
						// route to the same marker+backfill gate
						// used for the both-hub-managed case.
						return fmt.Errorf(
							"-original sentinel at %s does not contain %q (server added after sentinel was written): %w",
							path, server, errSentinelMissingEntry)
					}
				}
				return adapter.RestoreEntryFromBackup(path, server)
			}
			restoredFrom := backupPath
			err = safeRestore(backupPath)
			if errors.Is(err, clients.ErrBackupEntryAlreadyMigrated) {
				// Latest timestamped backup already holds this entry in
				// hub-managed form (multi-server or repeat-migrate case).
				// Fall back to the pristine sentinel — safeRestore's
				// pre-check applies.
				if sentErr := safeRestore(sentinelPath); sentErr == nil {
					restoredFrom = sentinelPath
					err = nil
				} else if errors.Is(sentErr, clients.ErrBackupEntryAlreadyMigrated) {
					// Path A: both the latest timestamped backup AND
					// the pristine sentinel hold the entry in
					// hub-managed form. No pre-hub state to restore.
					// Route through the marker+backfill+RemoveEntry
					// helper.
					err = tryMarkerOrBackfillRemove(
						adapter, binding, m, server,
						fmt.Sprintf("latest backup %s and -original sentinel both hold %q in hub-managed form",
							backupPath, server),
						&restoredFrom,
					)
				} else if errors.Is(sentErr, errSentinelMissingEntry) {
					// Path B: latest timestamped backup is hub-managed
					// AND the pristine sentinel exists but does not
					// contain the entry (the server was installed
					// AFTER mcphub first backed up this client config).
					// Previously this branch fell through to the
					// "sentinel fallback failed" wrapper error,
					// bypassing the marker check and blocking
					// demigrate on the common pattern of "user ran
					// `mcphub register` or `mcphub migrate <server>`
					// after the initial install".
					//
					// Safety: same reasoning as Path A. The marker
					// check positively confirms mcphub installed the
					// entry (positive-ownership evidence per
					// codex-bot P1 closure on PR #186 r1). When the
					// marker has no record, the backfill helper checks
					// that the live URL exactly matches what mcphub
					// would have written for this manifest binding —
					// structurally indistinguishable from a mcphub
					// install. Both gates are strictly narrower than
					// the "blind URL-equality" the original codex
					// closure refused. Strict mode is unaffected.
					err = tryMarkerOrBackfillRemove(
						adapter, binding, m, server,
						fmt.Sprintf("latest backup %s holds %q in hub-managed form, and -original sentinel does not contain the entry (server installed after sentinel was written)",
							backupPath, server),
						&restoredFrom,
					)
				} else {
					err = fmt.Errorf(
						"latest backup %s holds %q already in hub-managed form, and -original sentinel fallback failed: %w",
						backupPath, server, sentErr)
				}
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

// tryMarkerOrBackfillRemove is the shared marker+backfill+RemoveEntry
// recovery path used when the safe-restore chain (latest timestamped
// backup → -original sentinel) cannot produce a pre-hub form of the
// entry. Two failure modes share this recovery:
//
//   Path A: both the latest timestamped backup AND the sentinel hold
//           the entry in hub-managed form. No pre-hub state was ever
//           captured.
//   Path B: latest timestamped backup is hub-managed AND the sentinel
//           does not contain the entry at all (server was installed
//           AFTER mcphub first backed up this config).
//
// In both cases the live config has the entry in hub-managed shape.
// We need positive-ownership evidence that mcphub installed it before
// calling RemoveEntry — otherwise we could delete a user-owned
// localhost HTTP entry (codex-bot P1 closure on PR #186 r1).
//
// Evidence sources, in order:
//   1. Managed-entries marker file (PR #187). If (client, server) has
//      a record, mcphub demonstrably installed this entry.
//   2. Backfill check (PR #186 r2 / #192 r6). If the live entry's URL
//      exactly equals what mcphub would have written for this
//      manifest binding (or Antigravity relay shape matches), record
//      the marker inline and treat as mcphub-owned. Strictly narrower
//      than the rejected blind URL-equality fallback.
//
// On success: RemoveEntry runs, the marker row is forgotten
// (best-effort; stale rows self-heal on next migrate), restoredFrom
// is updated, returns nil.
// On marker-read failure: returns a wrapper error including the
// underlying mErr.
// On no-marker-no-backfill: returns a fail-closed error directing
// the operator to edit manually or re-run migrate to populate the
// marker. Strict mode is implicit in the migration setup (the marker
// itself is the strict-mode artifact).
//
// reasonPrefix is the human-readable description of WHY the
// safe-restore chain failed (used as the leading clause of any error
// surfaced from this helper). restoredFrom is updated on success to
// describe how the rollback was achieved, for the operator-visible
// "restored ... from ..." line.
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
		// PR #187 only marks fresh migrates). When the live URL
		// strictly equals `http://localhost:<daemon.Port><url_path>`
		// for the daemon this binding references (or 127.0.0.1 /
		// [::1] / Antigravity relay shape), backfill the marker
		// inline so the existing user can roll back from the
		// matrix without manual edits.
		if backfilled := backfillMarkerIfEntryMatchesManifest(adapter, server, binding, m); backfilled {
			managed = true
		}
	}
	switch {
	case mErr != nil:
		return fmt.Errorf(
			"%s, AND consulting managed-entries marker failed: %w",
			reasonPrefix, mErr)
	case !managed:
		return fmt.Errorf(
			"%s, but managed-entries marker has no record that mcphub installed this entry — refusing to RemoveEntry (entry may be user-owned); to roll back this entry, edit %s manually, or re-run migrate first to populate the marker",
			reasonPrefix, adapter.ConfigPath())
	default:
		// (client, server) is in the marker (or was just backfilled
		// because the live URL exactly matches manifest expectation).
		// mcphub installed this entry — RemoveEntry is the correct
		// rollback target.
		if rmErr := adapter.RemoveEntry(server); rmErr != nil {
			return fmt.Errorf(
				"%s, marker confirmed mcphub-managed, AND RemoveEntry failed: %w",
				reasonPrefix, rmErr)
		}
		// Best-effort forget — a leftover marker row for a removed
		// entry is a minor stale-state concern, not a correctness
		// bug. The next migrate would refresh installed_at and the
		// next demigrate would consult the updated row, so any
		// stale info self-heals.
		if fErr := ForgetManagedEntry(binding.Client, server); fErr != nil {
			_ = LogHubMcpEvent("warn", "managed-entries-forget-failed", map[string]any{
				"server": server,
				"client": binding.Client,
				"err":    fErr.Error(),
			})
		}
		*restoredFrom = "(marker confirmed mcphub-managed; removed entry from client config)"
		return nil
	}
}
