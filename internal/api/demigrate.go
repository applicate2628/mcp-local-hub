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
		// Track which clients a manifest binding already covers for this
		// server so the synthesized-binding pass below does not demigrate a
		// client twice.
		boundClients := map[string]bool{}
		for _, binding := range m.ClientBindings {
			boundClients[binding.Client] = true
			if !includedClient(binding.Client) {
				continue
			}
			demigrateOneBinding(report, allClients, m, server, binding, opts.Writer, true)
		}
		// Servers-matrix fix (symmetry with MigrateFrom): the matrix
		// renders a toggleable cell for EVERY detected client, not only
		// the manifest's static client_bindings. Unchecking + Apply for a
		// targeted client outside client_bindings was previously a silent
		// no-op (the iteration above skipped it), so a stale hub-loopback
		// entry on that client could never be removed. Synthesize a
		// binding for each such targeted client and run the SAME
		// per-binding demigrate body (DRY via demigrateOneBinding).
		// Eligibility: (a) named in ClientsInclude, (b) NOT already
		// covered by a manifest binding, (c) a known adapter, (d)
		// adapter.Exists().
		for _, client := range opts.ClientsInclude {
			if boundClients[client] {
				continue
			}
			adapter := allClients[client]
			if adapter == nil {
				continue
			}
			if !adapter.Exists() {
				continue
			}
			primaryDaemon, ok := primaryDaemonName(m)
			if !ok {
				report.Failed = append(report.Failed, FailedMigration{
					Server: server, Client: client,
					Err: fmt.Sprintf("manifest %s: no daemons declared; cannot demigrate non-binding client %q", server, client),
				})
				continue
			}
			synth := config.ClientBinding{
				Client:  client,
				Daemon:  primaryDaemon,
				URLPath: "/mcp",
			}
			demigrateOneBinding(report, allClients, m, server, synth, opts.Writer, false)
		}
	}
	return report, nil
}

// demigrateOneBinding performs the per-(server, client) reverse-migration for
// a single binding — real (from m.ClientBindings) or synthesized (for a
// targeted non-binding client). It appends exactly one row to report.Restored
// or report.Failed, or none when the client is skipped (missing adapter /
// non-existent client). For synthesized non-binding clients, timestamped
// backups that lack the entry are not treated as rollback proof because they
// may come from an unrelated hub operation before a user-created same-name
// entry; those clients must fall through to explicit marker-based removal.
func demigrateOneBinding(
	report *DemigrateReport,
	allClients map[string]clients.Client,
	m *config.ServerManifest,
	server string,
	binding config.ClientBinding,
	writer io.Writer,
	allowBackupMissingEntryRestore bool,
) {
	adapter := allClients[binding.Client]
	if adapter == nil {
		return
	}
	if !adapter.Exists() {
		return
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
		return
	}
	// Do NOT early-fail on an empty current-codename backup set: the
	// legacy-prefix and RemoveEntry fallbacks below MUST still run.
	// A cross-rename host may have ONLY a legacy mcp-sync/phase2
	// backup (no `bak-mcp-local-hub-*` was ever written on this
	// machine), or the entry was mcphub-installed-from-scratch with
	// the managed-entries marker as the only ownership evidence. An
	// empty set simply makes the restoreIfEligible loop a no-op
	// (restoredFrom stays ""); tryLegacyPrefixRestore then
	// tryMarkerOrBackfillRemove decide the outcome. (bot PR #257 P2)
	latestBackupPath := ""
	if len(backups) > 0 {
		latestBackupPath = backups[0]
	}
	// restoreIfEligible returns nil on success, or one of
	// three error classes the iteration treats as "skip this
	// backup, try older":
	//
	//   1. errBackupMissingEntry — sentinel backup does not
	//      contain the entry (predates the server install).
	//      Skip and keep searching older candidates. For manifest-bound
	//      clients only, a timestamped missing-entry backup may instead
	//      be restored as the pre-migrate state (removing the live entry).
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
			if !strings.HasSuffix(path, ".bak-mcp-local-hub-original") && allowBackupMissingEntryRestore {
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
	// If iteration over every mcp-local-hub backup exhausted
	// without a restore (all skipped via
	// ErrBackupEntryAlreadyMigrated or errBackupMissingEntry),
	// there is no pre-hub form among the current-codename
	// backups. Before falling back to deletion, consult the
	// LEGACY-codename backups (pre-rename mcp-sync, phase2,
	// plain/underscore-date install artifacts). A user who
	// upgraded across the April-2026 mcp-sync→mcp-local-hub
	// rename may have their only pre-hub snapshot there — the
	// originally-reported bug case. Restoring it is strictly
	// better than deleting the entry.
	sawLegacy := false
	if restoredFrom == "" && err == nil {
		sawLegacy, err = tryLegacyPrefixRestore(adapter, server, &restoredFrom)
	}
	// If neither current-codename nor legacy-codename backups
	// hold a pre-hub form, fall back to marker+backfill+
	// RemoveEntry — the ONLY case where deletion is safe is
	// when positive ownership evidence proves mcphub installed
	// the entry AND no backup (current OR legacy) captures a
	// pre-hub shape (the entry genuinely never existed in
	// pre-hub form). The two iterations above guarantee the
	// "no pre-hub form available" precondition; the marker
	// check guarantees the "mcphub installed it" precondition.
	// allowURLBackfill = allowBackupMissingEntryRestore && (len(backups) > 0 || sawLegacy),
	// and allowHubURLBackfill = allowBackupMissingEntryRestore: URL-backfill
	// RemoveEntry is corroborated only for real manifest bindings
	// when mcphub demonstrably ran on this host (a current OR legacy backup file
	// exists). Synthesized non-binding clients require explicit marker ownership;
	// an unrelated backup plus a same-name user entry is not enough evidence. With
	// ZERO backups of ANY codename the URL match is uncorroborated and must fail
	// closed (bot PR #257 r2/r3).
	if restoredFrom == "" && err == nil {
		err = tryMarkerOrBackfillRemove(
			adapter, binding, m, server,
			fmt.Sprintf("no backup (of %d candidates, newest %s) contains %q in pre-hub form — last skip: %v",
				len(backups), latestBackupPath, server, lastSkipReason),
			allowBackupMissingEntryRestore && (len(backups) > 0 || sawLegacy),
			allowBackupMissingEntryRestore,
			&restoredFrom,
		)
	}
	if err != nil {
		report.Failed = append(report.Failed, FailedMigration{
			Server: server, Client: binding.Client, Err: err.Error(),
		})
		return
	}
	report.Restored = append(report.Restored, RestoredMigration{
		Server: server, Client: binding.Client,
	})
	fmt.Fprintf(writer, "restored %s for %s from %s\n", server, binding.Client, restoredFrom)
}

// tryMarkerOrBackfillRemove is the LAST-RESORT recovery path used by
// Demigrate when iterating every backup newest-first found NO
// candidate with the entry in pre-hub form. Two precondition
// guarantees from the caller's iteration:
//
//  1. Every backup (timestamped + sentinel) either lacks the entry
//     entirely OR holds it in hub-managed form.
//  2. The live config still has the entry in hub-managed form
//     (otherwise demigrate would have nothing to roll back).
//
// Under both preconditions there is no recoverable pre-hub state,
// so the rollback target is one of:
//
//	a. RemoveEntry — when positive ownership evidence shows mcphub
//	   installed the entry (marker file says so, or backfill helper
//	   confirms the live URL exactly matches the manifest binding).
//	b. Fail-closed — otherwise, the entry MIGHT be user-owned and
//	   indistinguishable from a mcphub install via these signals;
//	   refuse to delete it. Operator workaround: edit the config
//	   manually.
//
// Evidence sources, in order:
//
//  1. Managed-entries marker (PR #187). Populated by migrate.go
//     AFTER successful adapter.AddEntry. A row for (client, server)
//     means mcphub installed this entry.
//  2. Backfill (PR #186 r2 / #192 r6). For v0.4.x users whose
//     entries predate the marker subsystem: when the live entry's
//     URL exactly equals what mcphub would have written
//     (http://localhost:<daemon.port><url_path> or 127.0.0.1 / [::1]
//     / Antigravity relay shape), record the marker inline and
//     treat as mcphub-owned.
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
	allowURLBackfill bool,
	allowHubURLBackfill bool,
	restoredFrom *string,
) error {
	managed, mErr := IsManagedEntry(binding.Client, server)
	if !managed && mErr == nil && (allowURLBackfill || (allowHubURLBackfill && liveEntryHasHubURL(adapter, server))) {
		// v0.4.x upgrade backfill: existing users have hub-form entries
		// that were never marked (B4 marker introduced in PR #187 only
		// marks fresh migrates). Two corroboration sources for the URL
		// backfill, either of which proves mcphub owns the entry:
		//
		//  1. allowURLBackfill — a backup file (current OR legacy codename)
		//     exists, proving mcphub demonstrably ran on this host. (bot PR
		//     #257 r2/r3.)
		//
		//  2. liveEntryHasHubURL — the live entry's URL is a hub loopback
		//     URL (clients.IsHubHTTPURL). No operator manually points a
		//     client at the hub's own loopback port: a hub URL is itself
		//     ownership corroboration, so the no-backup http-from-start
		//     via-hub case (an entry installed by mcphub directly into
		//     hub-HTTP shape — e.g. `fetch` at http://localhost:9133/mcp —
		//     with NO pre-hub stdio form ever captured, hence no backup)
		//     can still RemoveEntry. The bug: unchecking such a cell and
		//     Apply left the entry, so the matrix cell never unchecked.
		//     Non-hub URLs (an operator's own remote MCP server) still fail
		//     IsHubHTTPURL → still fail closed here. The actual deletion
		//     decision below remains gated on the strict manifest match
		//     (backfillMarkerIfEntryMatchesManifest + the default-branch
		//     liveEntryMatchesManifestBinding re-check), so a hub URL whose
		//     port/path does NOT match the manifest binding still fails
		//     closed. (work-items/bugs/2026-05-15-demigrate-fallback-when-
		//     no-pre-hub-form.md — fetch checkbox.)
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
		live, gErr := adapter.GetEntry(server)
		if gErr != nil {
			return fmt.Errorf("%s, marker confirms mcphub-managed, but failed reading live entry before RemoveEntry: %w",
				reasonPrefix, gErr)
		}
		if live == nil {
			return fmt.Errorf("%s, marker confirms mcphub-managed, but live entry is missing — refusing to RemoveEntry",
				reasonPrefix)
		}
		if matched, _ := liveEntryMatchesManifestBinding(live, server, binding, m); !matched {
			return fmt.Errorf("%s, marker confirms mcphub-managed, but live entry no longer matches manifest-managed shape — refusing to RemoveEntry (entry may be user-modified); edit %s manually or re-run migrate",
				reasonPrefix, adapter.ConfigPath())
		}
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

// liveEntryHasHubURL reports whether the live client-config entry for
// `server` exists and its URL points at the local hub
// (clients.IsHubHTTPURL: http://localhost: / 127.0.0.1: / [::1]:). It is
// the corroboration source that lets tryMarkerOrBackfillRemove proceed
// with a URL backfill even on a host with NO backup of any codename: a
// hub loopback URL is itself proof of mcphub ownership, because no
// operator manually points a client at the hub's own loopback port. A
// non-hub URL (an operator's own remote MCP server) returns false, so
// the caller keeps failing closed for it. Any read error or missing
// entry returns false (keep the conservative default).
func liveEntryHasHubURL(adapter clients.Client, server string) bool {
	live, err := adapter.GetEntry(server)
	if err != nil || live == nil {
		return false
	}
	return clients.IsHubHTTPURL(live.URL)
}

// tryLegacyPrefixRestore iterates this client's LEGACY-codename backups
// (pre-rename mcp-sync, phase2, plain/underscore-date install artifacts,
// in clients.LegacyBackupsNewestFirst bucket+recency order) looking for
// the first backup that holds `server` in its true PRE-HUB (direct/stdio)
// form, and restores from THAT backup. It is the bridge for operators who
// upgraded across the April-2026 mcp-sync→mcp-local-hub rename, whose only
// pre-hub snapshot may live under a legacy prefix.
//
// Destructive-safety (the load-bearing PR #218 invariant): a legacy
// backup is used ONLY when BackupContainsEntry is true AND
// BackupEntryIsHubManaged is false — i.e. it positively contains the
// entry in pre-hub form. Backups that LACK the entry are SKIPPED (never
// allowed to trigger RestoreEntryFromBackup's delete-on-absent branch),
// and backups that hold the entry in hub-managed form are SKIPPED (no
// pre-hub state). So this path can only ever RESTORE a pre-hub form; it
// can never delete an entry. Deletion stays exclusively in
// tryMarkerOrBackfillRemove, which runs only after this returns without
// a restore.
//
// On success, *restoredFrom is set to the chosen backup path and nil is
// returned. When no legacy backup holds a pre-hub form, *restoredFrom is
// left untouched and nil is returned (caller proceeds to the RemoveEntry
// fallback). A non-nil error is returned only for a hard filesystem
// failure enumerating legacy backups or an unreadable backup that
// positively matched the contains+pre-hub gate.
func tryLegacyPrefixRestore(adapter clients.Client, server string, restoredFrom *string) (sawLegacy bool, err error) {
	legacy, lerr := clients.LegacyBackupsNewestFirst(adapter.ConfigPath(), adapter.Name())
	if lerr != nil {
		return false, fmt.Errorf("enumerate legacy-codename backups: %w", lerr)
	}
	// sawLegacy records that legacy backup FILES exist on this host, which
	// proves mcphub (under the old mcp-sync codename) ran here. The caller
	// uses it to corroborate a URL-backfill RemoveEntry: a host with NO backup
	// of ANY codename has never run mcphub, so an uncorroborated URL match
	// there must fail closed; a legacy-only host HAS run mcphub and may
	// RemoveEntry once no pre-hub form is found. (bot PR #257 r3)
	sawLegacy = len(legacy) > 0
	for _, candidate := range legacy {
		has, hErr := adapter.BackupContainsEntry(candidate, server)
		if hErr != nil {
			// Unreadable/malformed legacy backup: we CANNOT prove it lacks a
			// pre-hub form of the entry, so we must NOT silently skip it and
			// let the caller fall through to a marker/backfill RemoveEntry
			// (that would delete an entry whose only pre-hub snapshot might
			// live in THIS very backup). Fail closed. (bot PR #257 r2)
			return sawLegacy, fmt.Errorf("legacy-codename backup %s unreadable (cannot prove it lacks %q in pre-hub form): %w", candidate, server, hErr)
		}
		if !has {
			continue // legacy backup predates the entry — never delete from here
		}
		hubManaged, mErr := adapter.BackupEntryIsHubManaged(candidate, server)
		if mErr != nil {
			// Entry IS present but its shape is undeterminable — it MIGHT be
			// a pre-hub form. Fail closed rather than skip-then-delete.
			return sawLegacy, fmt.Errorf("legacy-codename backup %s: cannot determine whether %q is hub-managed: %w", candidate, server, mErr)
		}
		if hubManaged {
			continue // legacy backup already hub-managed — no pre-hub state here
		}
		// Positively a pre-hub form. RestoreEntryFromBackup takes its
		// restore branch (entry present) and cannot hit
		// ErrBackupEntryAlreadyMigrated (entry is not hub-managed).
		if rErr := adapter.RestoreEntryFromBackup(candidate, server); rErr != nil {
			return sawLegacy, fmt.Errorf("restore %q from legacy-codename backup %s: %w", server, candidate, rErr)
		}
		*restoredFrom = candidate
		return sawLegacy, nil
	}
	return sawLegacy, nil
}
