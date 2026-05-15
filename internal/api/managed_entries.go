// managed_entries.go — positive ownership marker for mcphub-installed
// client-config entries.
//
// Background (B4 / PR #186 reverted): demigrate cannot reliably tell
// whether a live entry was installed by mcphub or by the user from
// the entry's URL alone. `IsHubHTTPURL` matches any loopback HTTP
// URL — but a user can legitimately configure a localhost HTTP MCP
// server with the same name and the same port/path that mcphub's
// manifest would have produced. A heuristic-based demigrate fallback
// that called RemoveEntry on URL-match was caught by codex bot r1
// P1 on PR #186 as a data-loss risk and reverted.
//
// This file implements the positive-marker design: mcphub writes a
// state-dir file `managed-entries.json` recording every (client,
// server) pair it installs. Demigrate consults this file before
// calling RemoveEntry — only entries mcphub demonstrably installed
// are eligible for the rollback-to-no-entry path. Pre-existing
// user-owned entries are not in the marker, so they remain
// fail-closed (Demigrate refuses to delete them).
//
// File layout (`<state-dir>/managed-entries.json`, 0600):
//
//	{
//	  "version": 1,
//	  "entries": [
//	    {"client": "claude-code", "server": "time", "installed_at": "2026-05-15T03:20:00Z"},
//	    {"client": "gemini-cli",  "server": "memory", "installed_at": "2026-05-15T03:21:10Z"}
//	  ]
//	}
//
// Write path: migrate.go appends a tuple after every successful
// adapter.AddEntry. Idempotent — a re-migrate of the same (client,
// server) updates installed_at rather than duplicating the row.
//
// Read path: demigrate.go calls IsManagedEntry(client, server). True
// → mcphub demonstrably installed this entry → RemoveEntry safe.
// False → no positive proof of ownership → refuse to delete.
//
// Existing users post-upgrade have no marker file. Their entries
// fail-closed on demigrate (same as pre-PR-187 behavior). Recovery
// path: re-migrate (uncheck + Apply, then check + Apply through the
// matrix) populates the marker, after which demigrate works
// normally. No automatic backfill — that would require a URL
// heuristic identical to the one codex bot rejected on PR #186 r1.
//
// Race / concurrency: read-modify-write under flock on the state
// dir. Concurrent migrate operations across processes serialize on
// the lock; in-process operations serialize on a package mutex.

package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sync"
	"time"
)

const managedEntriesFileLeaf = "managed-entries.json"

// managedEntriesSchemaVersion is the on-disk format version. Bumping
// requires a migration step in readManagedEntries.
const managedEntriesSchemaVersion = 1

// managedEntriesMu serializes in-process read-modify-write cycles on
// the marker file. Cross-process serialization is handled by the
// underlying SecureWriteClientConfig pipeline's atomic rename
// (writers never see partial state).
var managedEntriesMu sync.Mutex

// ManagedEntry records a single (client, server) tuple that mcphub
// has installed into a client's config. installed_at is the UTC
// timestamp of the most recent migrate that produced this entry.
type ManagedEntry struct {
	Client      string    `json:"client"`
	Server      string    `json:"server"`
	InstalledAt time.Time `json:"installed_at"`
}

// ManagedEntries is the file root.
type ManagedEntries struct {
	Version int            `json:"version"`
	Entries []ManagedEntry `json:"entries"`
}

// readManagedEntries returns the parsed marker file, or an empty
// ManagedEntries{Version: managedEntriesSchemaVersion} if the file
// does not yet exist. Any other read/parse error propagates.
func readManagedEntries() (*ManagedEntries, error) {
	raw, err := readHubMcpStateFile(managedEntriesFileLeaf)
	if err != nil {
		if isHubMcpStateMissingErr(err) {
			return &ManagedEntries{Version: managedEntriesSchemaVersion}, nil
		}
		return nil, err
	}
	var m ManagedEntries
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("parse managed-entries.json: %w", err)
	}
	if m.Version == 0 {
		// Older write that predated the version field — accept and
		// rewrite at the current version on next mutation.
		m.Version = managedEntriesSchemaVersion
	}
	if m.Version != managedEntriesSchemaVersion {
		return nil, fmt.Errorf("managed-entries.json: unknown schema version %d (this build expects %d)", m.Version, managedEntriesSchemaVersion)
	}
	return &m, nil
}

// writeManagedEntries serializes m and writes it via the state-file
// pipeline (handle-relative, DACL-hardened temp+atomic-rename).
func writeManagedEntries(m *ManagedEntries) error {
	m.Version = managedEntriesSchemaVersion
	raw, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal managed-entries: %w", err)
	}
	return writeHubMcpStateFile(managedEntriesFileLeaf, raw)
}

// RecordManagedEntry adds (or refreshes) a (client, server) tuple in
// the marker file. Idempotent — re-recording an existing tuple
// updates installed_at to time.Now().UTC() instead of duplicating
// the row.
//
// Called from migrate.go after a successful adapter.AddEntry.
func RecordManagedEntry(client, server string) error {
	if client == "" || server == "" {
		return errors.New("RecordManagedEntry: client and server must be non-empty")
	}
	managedEntriesMu.Lock()
	defer managedEntriesMu.Unlock()

	m, err := readManagedEntries()
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	for i, e := range m.Entries {
		if e.Client == client && e.Server == server {
			m.Entries[i].InstalledAt = now
			return writeManagedEntries(m)
		}
	}
	m.Entries = append(m.Entries, ManagedEntry{
		Client:      client,
		Server:      server,
		InstalledAt: now,
	})
	return writeManagedEntries(m)
}

// ForgetManagedEntry removes a (client, server) tuple from the marker
// file. Called from demigrate.go after a successful adapter.RemoveEntry
// so the demigrated entry no longer claims mcphub ownership.
//
// Idempotent — removing a tuple that does not exist is a no-op.
func ForgetManagedEntry(client, server string) error {
	if client == "" || server == "" {
		return errors.New("ForgetManagedEntry: client and server must be non-empty")
	}
	managedEntriesMu.Lock()
	defer managedEntriesMu.Unlock()

	m, err := readManagedEntries()
	if err != nil {
		return err
	}
	filtered := m.Entries[:0]
	for _, e := range m.Entries {
		if e.Client == client && e.Server == server {
			continue
		}
		filtered = append(filtered, e)
	}
	m.Entries = filtered
	return writeManagedEntries(m)
}

// IsManagedEntry reports whether the marker file lists the (client,
// server) tuple. Returns (false, nil) when the file does not exist
// (no entries managed yet, common state for fresh installs and for
// users post-upgrade who haven't re-migrated yet).
//
// Read errors (corrupt file, DACL violation) propagate to the caller
// so demigrate fails closed rather than silently treating them as
// "not managed".
func IsManagedEntry(client, server string) (bool, error) {
	if client == "" || server == "" {
		return false, errors.New("IsManagedEntry: client and server must be non-empty")
	}
	managedEntriesMu.Lock()
	defer managedEntriesMu.Unlock()

	m, err := readManagedEntries()
	if err != nil {
		return false, err
	}
	for _, e := range m.Entries {
		if e.Client == client && e.Server == server {
			return true, nil
		}
	}
	return false, nil
}

// managedEntriesPath returns the absolute path to the marker file
// (used by tests and `mcphub status`). Returns "" if the state-dir
// resolver fails (extremely rare; surface as missing-marker).
func managedEntriesPath() string {
	dir, err := DaemonStateDir()
	if err != nil {
		return ""
	}
	return joinStateFilePath(dir, managedEntriesFileLeaf)
}

// joinStateFilePath wraps filepath.Join. Kept as a thin helper so
// tests can stub the path without importing filepath.
func joinStateFilePath(dir, name string) string {
	return dir + string(os.PathSeparator) + name
}
