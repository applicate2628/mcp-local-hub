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
// Multi-server constraint: when multiple servers were migrated from the
// same client, each migration takes its own timestamped backup capturing
// state at that moment. The latest backup therefore holds earlier-
// migrated servers ALREADY in hub-HTTP/relay form and only the most-
// recently-migrated server in stdio form. Demigrate can auto-restore
// ONLY that last-migrated server from the latest backup. Earlier-
// migrated servers hit ErrBackupEntryAlreadyMigrated and surface as
// Failed rows; the operator must restore those from the `-original`
// sentinel manually. Ordering of demigrate calls does not help — the
// latest backup's content is frozen and does not change when earlier
// entries are rewritten in the live file. This is intentional:
// silently re-writing hub-HTTP data is strictly worse than a clear
// failure directing the operator to the sentinel.
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
			if err := adapter.RestoreEntryFromBackup(backupPath, server); err != nil {
				errMsg := err.Error()
				if errors.Is(err, clients.ErrBackupEntryAlreadyMigrated) {
					errMsg = fmt.Sprintf(
						"latest backup holds %q already in hub-managed form — Demigrate can only auto-restore the most-recently-migrated server per client. Restore manually from the -original sentinel (%s).",
						server, backupPath)
				}
				report.Failed = append(report.Failed, FailedMigration{
					Server: server, Client: binding.Client, Err: errMsg,
				})
				continue
			}
			report.Restored = append(report.Restored, RestoredMigration{
				Server: server, Client: binding.Client,
			})
			fmt.Fprintf(opts.Writer, "restored %s for %s from %s\n", server, binding.Client, backupPath)
		}
	}
	return report, nil
}
