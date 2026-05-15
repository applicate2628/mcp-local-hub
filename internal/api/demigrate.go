package api

import (
	"errors"
	"fmt"
	"io"

	"mcp-local-hub/internal/clients"
	"mcp-local-hub/internal/config"
)

// buildExpectedHubURL constructs the hub-routed URL mcphub WOULD
// write for the given (manifest, binding) pair. Used by Demigrate
// to verify that a live client-config entry actually IS the
// mcphub-managed one before falling back to RemoveEntry (codex bot
// r1 P1 closure on PR #186: blind RemoveEntry would delete legit
// user-owned localhost HTTP MCP entries that happen to match the
// loopback-URL heuristic in `IsHubHTTPURL`).
//
// Returns "" when the binding references an unknown daemon — in
// that case the caller must refuse to RemoveEntry because we
// cannot prove the live entry is ours.
//
// Mirror of migrate.go:108-120 / install.go:946 URL construction
// — must stay byte-identical or the guard breaks (live entries
// that DO match would falsely fail the equality check).
func buildExpectedHubURL(m *config.ServerManifest, binding config.ClientBinding) string {
	port, ok := findDaemonPort(m, binding.Daemon)
	if !ok {
		return ""
	}
	urlPath := binding.URLPath
	if urlPath == "" {
		urlPath = "/mcp"
	}
	return fmt.Sprintf("http://localhost:%d%s", port, urlPath)
}

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
						return fmt.Errorf(
							"-original sentinel at %s does not contain %q (server added after sentinel was written; auto-rollback would silently delete it — inspect older timestamped backups manually)",
							path, server)
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
					// PR #186 (B4 fix): both the latest timestamped
					// backup AND the pristine sentinel hold the entry
					// in hub-managed form. Common case: claude-code /
					// gemini-cli use HTTP transport natively, and
					// the April 2026 `mcp-sync → mcp-local-hub`
					// codename rename sealed sentinels post-migrate.
					//
					// Codex bot r1 P1 closure on PR #186: the
					// `ErrBackupEntryAlreadyMigrated` predicate is
					// based on a loopback-URL heuristic (IsHubHTTPURL)
					// — it matches ANY localhost HTTP URL, not just
					// mcphub-managed ones. A user who configured a
					// legitimate localhost HTTP MCP server BEFORE
					// installing mcphub would have both backup and
					// sentinel holding that user-owned entry, and a
					// blind RemoveEntry would delete it. That is a
					// data-loss regression.
					//
					// Guard: only RemoveEntry when the LIVE entry's
					// URL matches the URL mcphub WOULD have written
					// for this binding (manifest's daemon port +
					// binding.url_path; exact shape from migrate.go
					// :120 / install.go:946). If live entry matches
					// → it IS our managed entry → safe to remove.
					// If it doesn't match (different localhost URL)
					// → user-owned entry → refuse to delete; surface
					// the original sentinel-fallback failure so the
					// operator inspects manually.
					expectedURL := buildExpectedHubURL(m, binding)
					liveEntry, getErr := adapter.GetEntry(server)
					switch {
					case getErr != nil:
						err = fmt.Errorf(
							"latest backup %s and -original sentinel both hold %q in hub-managed form, but reading live entry failed: %w",
							backupPath, server, getErr)
					case liveEntry == nil:
						// No live entry — nothing to remove. Treat as
						// already-rolled-back (idempotent).
						restoredFrom = "(no pre-hub form found; live entry already absent)"
						err = nil
					case expectedURL == "":
						// Manifest doesn't yield an expected URL
						// (missing daemon port). Cannot safely
						// distinguish our entry from user's → fail.
						err = fmt.Errorf(
							"latest backup %s and -original sentinel both hold %q in hub-managed form, but cannot derive expected hub URL from manifest binding (daemon %q url_path %q) to guard RemoveEntry — inspect manually",
							backupPath, server, binding.Daemon, binding.URLPath)
					case liveEntry.URL != expectedURL:
						// Live entry exists but its URL does not
						// match what mcphub would have written. This
						// is the codex-bot-P1 protected case: a
						// user-owned localhost HTTP entry. Refuse to
						// delete it; the operator must inspect.
						err = fmt.Errorf(
							"latest backup %s and -original sentinel both hold %q in hub-managed form, but live entry URL %q does not match this manifest's expected hub URL %q — refusing to RemoveEntry (entry appears user-owned, not mcphub-managed); inspect manually",
							backupPath, server, liveEntry.URL, expectedURL)
					default:
						// Live entry URL exactly matches manifest's
						// expected hub URL → this IS our managed
						// entry → safe to RemoveEntry.
						if rmErr := adapter.RemoveEntry(server); rmErr != nil {
							err = fmt.Errorf(
								"latest backup %s and -original sentinel both hold %q in hub-managed form, AND RemoveEntry fallback failed: %w",
								backupPath, server, rmErr)
						} else {
							restoredFrom = "(no pre-hub form found — removed mcphub-managed entry from client config)"
							err = nil
						}
					}
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
