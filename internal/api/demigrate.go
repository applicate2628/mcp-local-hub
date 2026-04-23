package api

import (
	"errors"
	"fmt"
	"io"

	"mcp-local-hub/internal/clients"
)

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
			restoredFrom := backupPath
			err = adapter.RestoreEntryFromBackup(backupPath, server)
			if errors.Is(err, clients.ErrBackupEntryAlreadyMigrated) {
				// Latest backup already holds this entry in hub-managed form
				// (multi-server or repeat-migrate case). Try the pristine
				// -original sentinel, which by definition captures pre-hub
				// state and therefore cannot hold a hub-managed shape for
				// any entry that existed before mcphub touched the file.
				//
				// BUT: RestoreEntryFromBackup silently deletes the live
				// entry when the backup lacks it — correct when migrate
				// added the server from scratch, WRONG when the operator
				// added the server AFTER the sentinel was written and
				// then migrated it (twice, in the bot-R1-P1 reproducer).
				// Pre-check sentinel containment to avoid the destructive
				// silent-delete path.
				sentinelPath := adapter.ConfigPath() + ".bak-mcp-local-hub-original"
				hasInSentinel, sentCheckErr := adapter.BackupContainsEntry(sentinelPath, server)
				switch {
				case sentCheckErr != nil:
					err = fmt.Errorf(
						"latest backup %s holds %q already in hub-managed form, and -original sentinel at %s is unreadable: %w",
						backupPath, server, sentinelPath, sentCheckErr)
				case !hasInSentinel:
					err = fmt.Errorf(
						"latest backup %s holds %q already in hub-managed form, and -original sentinel at %s does not contain %q (server was added after the sentinel was written; auto-rollback would silently delete it — inspect older timestamped backups manually)",
						backupPath, server, sentinelPath, server)
				default:
					if sentErr := adapter.RestoreEntryFromBackup(sentinelPath, server); sentErr == nil {
						restoredFrom = sentinelPath
						err = nil
					} else {
						err = fmt.Errorf(
							"latest backup %s holds %q already in hub-managed form, and -original sentinel fallback at %s failed: %w",
							backupPath, server, sentinelPath, sentErr)
					}
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
