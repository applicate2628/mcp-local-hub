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
	"path/filepath"
	"slices"
	"sync"
	"time"

	"mcp-local-hub/internal/clients"
	"mcp-local-hub/internal/config"

	"github.com/gofrs/flock"
)

const (
	managedEntriesFileLeaf     = "managed-entries.json"
	managedEntriesLockFileLeaf = "managed-entries.lock"
)

// managedEntriesSchemaVersion is the on-disk format version. Bumping
// requires a migration step in readManagedEntries.
const managedEntriesSchemaVersion = 1

// managedEntriesMu serializes in-process read-modify-write cycles on
// the marker file. Cross-process serialization is handled by the
// flock acquired in withManagedEntriesLock (codex bot r1 P2 closure
// on PR #187: a process-local mutex alone would let two concurrent
// `mcphub migrate` invocations both read the same old snapshot and
// the later writer would overwrite the earlier update, silently
// dropping one tuple).
var managedEntriesMu sync.Mutex

// withManagedEntriesLock holds BOTH the in-process mutex AND a
// cross-process flock on <state-dir>/managed-entries.lock for the
// duration of fn. Used by every read-modify-write path (Record,
// Forget). Pure-read paths (IsManagedEntry) need only the in-process
// mutex plus reliance on the atomic-rename guarantee of the state-
// file pipeline — readers never see partial state.
//
// Lock ordering: in-process mutex FIRST so we never sleep on the
// process-shared flock with another goroutine holding the in-process
// mutex (which would prevent the in-process holder from making
// progress and acquiring the flock itself). The flock is acquired
// AFTER the in-process mutex, and the lock-file path is resolved
// while the in-process mutex is held.
func withManagedEntriesLock(fn func() error) error {
	managedEntriesMu.Lock()
	defer managedEntriesMu.Unlock()

	dir, err := DaemonStateDir()
	if err != nil {
		return fmt.Errorf("managed-entries lock: resolve state dir: %w", err)
	}
	lockPath := filepath.Join(dir, managedEntriesLockFileLeaf)
	lk := flock.New(lockPath)
	if err := lk.Lock(); err != nil {
		return fmt.Errorf("managed-entries flock %s: %w", lockPath, err)
	}
	defer func() { _ = lk.Unlock() }()

	return fn()
}

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
// Holds both the in-process mutex AND the cross-process flock for
// the read-modify-write cycle (codex bot r1 P2 closure on PR #187:
// concurrent `mcphub migrate` calls across processes would otherwise
// race on the snapshot and the later writer could overwrite the
// earlier update, silently dropping one tuple).
//
// Called from migrate.go after a successful adapter.AddEntry.
func RecordManagedEntry(client, server string) error {
	if client == "" || server == "" {
		return errors.New("RecordManagedEntry: client and server must be non-empty")
	}
	return withManagedEntriesLock(func() error {
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
	})
}

// ForgetManagedEntry removes a (client, server) tuple from the marker
// file. Called from demigrate.go after a successful adapter.RemoveEntry
// so the demigrated entry no longer claims mcphub ownership.
//
// Idempotent — removing a tuple that does not exist is a no-op.
// Holds both the in-process mutex AND the cross-process flock per
// withManagedEntriesLock's contract.
func ForgetManagedEntry(client, server string) error {
	if client == "" || server == "" {
		return errors.New("ForgetManagedEntry: client and server must be non-empty")
	}
	return withManagedEntriesLock(func() error {
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
	})
}

// IsManagedEntry reports whether the marker file lists the (client,
// server) tuple. Returns (false, nil) when the file does not exist
// (no entries managed yet, common state for fresh installs and for
// users post-upgrade who haven't re-migrated yet).
//
// Read errors (corrupt file, DACL violation) propagate to the caller
// so demigrate fails closed rather than silently treating them as
// "not managed".
//
// Pure-read path: takes only the in-process mutex. Cross-process
// reads rely on the atomic-rename guarantee of the state-file
// pipeline — writers publish a complete file via tempfile+rename, so
// concurrent readers never see partial state. A stale read between
// writer's snapshot and rename returns "not managed", which falls
// through to fail-closed demigrate (safe; operator retries).
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

// backfillMarkerIfEntryMatchesManifest is the v0.4.2 unblock for
// existing v0.4.x users whose hub-form entries were never marked.
// PR #187 introduced the marker as a "positive proof" gate to
// replace the URL heuristic codex bot rejected on PR #186. Users
// who installed before #187 have hub-form entries in their client
// configs but no marker rows, so demigrate fails closed for those
// entries — making the matrix UI unusable for rollback on this
// state.
//
// The recovery path documented in #187 ("re-migrate to populate
// the marker") doesn't work from the matrix: the cell already
// shows checked (hub-routed), there's no UI affordance to force a
// re-migrate. Inline backfill plugs that gap.
//
// Backfill criterion (strict, NOT the loose URL heuristic codex
// bot rejected):
//
//  1. Live entry's URL EXACTLY matches what mcphub would write
//     for this (server, daemon, binding): scheme/host/port/path
//     derived from the manifest daemon + binding url_path.
//  2. Both loopback host variants (localhost / 127.0.0.1 / [::1])
//     are accepted because IsHubHTTPURL accepts all three on the
//     classification side.
//  3. The entry's name matches the manifest server name (this
//     is the loop variable, so already aligned at call site).
//
// On exact match, RecordManagedEntry is called with the (client,
// server) pair. The marker now contains a row, IsManagedEntry
// returns true on the next call, demigrate proceeds to
// RemoveEntry.
//
// Returns true iff backfill succeeded. Read/write errors on the
// marker file are best-effort logged and return false — caller
// keeps the fail-closed default in that case.
//
// Safety: if a user happened to configure their own MCP server
// on the exact port + path mcphub uses for this binding AND
// gave it the manifest's exact name, the backfill triggers and
// RemoveEntry runs. That deletes the user's structurally-
// identical entry; since the live shape IS what mcphub would
// have written, restoring from sentinel (when available) yields
// the same shape — the user's "own" entry was effectively
// already a mcphub-equivalent binding. No data loss beyond what
// the operator's manual config choice implied.
func backfillMarkerIfEntryMatchesManifest(adapter clients.Client, server string, binding config.ClientBinding, m *config.ServerManifest) bool {
	live, err := adapter.GetEntry(server)
	if err != nil || live == nil {
		return false
	}
	matched, reason := liveEntryMatchesManifestBinding(live, server, binding, m)
	if !matched {
		return false
	}
	if recErr := RecordManagedEntry(binding.Client, server); recErr != nil {
		_ = LogHubMcpEvent("warn", "managed-entries-backfill-failed", map[string]any{
			"server": server,
			"client": binding.Client,
			"err":    recErr.Error(),
		})
		return false
	}
	_ = LogHubMcpEvent("info", "managed-entries-backfill", map[string]any{
		"server": server,
		"client": binding.Client,
		"shape":  reason,
	})
	return true
}

// liveEntryMatchesManifestBinding returns (true, "<shape>") iff the
// live client-config entry's shape exactly matches what mcphub
// would have written for (server, binding):
//
//  1. HTTP shape (most clients): live.URL exactly equals
//     `http://<loopback-host>:<daemon.port><binding.url_path>` for
//     the daemon this binding references.
//
//  2. Antigravity relay shape: live.RelayServer == server AND
//     live.RelayDaemon == binding.Daemon AND
//     IsMcphubBinary(live.RelayExePath). Antigravity entries have
//     no URL field — they spawn `mcphub.exe relay --server <s>
//     --daemon <d>` as the stdio child. Codex bot r6 P2 on PR #192:
//     the v0.4.2 backfill helper originally only matched HTTP URLs
//     and silently fell through for Antigravity, leaving demigrate
//     fail-closed on Antigravity rows.
//
// Returns (false, "") if neither shape matches.
func liveEntryMatchesManifestBinding(live *clients.MCPEntry, server string, binding config.ClientBinding, m *config.ServerManifest) (bool, string) {
	daemonPort, ok := findDaemonPort(m, binding.Daemon)
	if !ok {
		return false, ""
	}
	urlPath := binding.URLPath
	if urlPath == "" {
		urlPath = "/mcp"
	}
	if IsSerenaServer(server) && (IsSerenaRouterURL(live.URL) ||
		(IsSerenaRouterURL(live.RelayURL) && clients.IsMcphubBinary(live.RelayExePath))) {
		return true, "serena dynamic-pool router entry"
	}
	// HTTP shape check. This is a RECOGNITION matcher (not a URL builder), so it
	// must accept ALL loopback spellings a live entry may carry: the legacy
	// "localhost" form (pre-127.0.0.1-migration entries, or a restore from an old
	// backup), the current canonical "127.0.0.1" form the hub now writes, and the
	// IPv6 "[::1]" form. Dropping "localhost" here would make a still-localhost
	// on-disk entry fail to match its own manifest binding.
	expectedURLs := []string{
		fmt.Sprintf("http://localhost:%d%s", daemonPort, urlPath),
		fmt.Sprintf("http://127.0.0.1:%d%s", daemonPort, urlPath),
		fmt.Sprintf("http://[::1]:%d%s", daemonPort, urlPath),
	}
	if live.URL != "" && slices.Contains(expectedURLs, live.URL) {
		return true, "v0.4.x upgrade backfill — HTTP URL exactly matches manifest expectation (url=" + live.URL + ")"
	}
	// Antigravity relay shape check (URL is empty, command/args
	// reconstructed via antigravityClient.GetEntry).
	if live.URL == "" &&
		live.RelayServer == server &&
		live.RelayDaemon == binding.Daemon &&
		clients.IsMcphubBinary(live.RelayExePath) {
		return true, "v0.4.x upgrade backfill — Antigravity relay shape exactly matches manifest binding (command=mcphub args=[relay,--server,server," + server + ",--daemon," + binding.Daemon + "])"
	}
	// Relay-URL shape check (URL empty; the entry is `mcphub relay --url
	// http://<loopback>:<port>/mcp`, which GetEntry maps back to RelayURL).
	// Relay-stdio clients (Zed, pi, pochi, zencoder) write THIS form — not the
	// --server/--daemon form above — for stable-port GLOBAL daemons (the
	// --server/--daemon form is the serena dynamic-pool manifest-lookup path).
	// Recognize it exactly like the direct-HTTP `url` branch: accept every
	// loopback spelling (localhost / 127.0.0.1 / [::1]) of the daemon's expected
	// endpoint, gated on the mcphub binary so a user-authored relay to some
	// other URL is NOT mistaken for a hub-managed entry. Without this branch,
	// demigrate refused to roll back relay-URL globals (e.g. Zed fetch/drmemory),
	// since neither the `url` exact-match nor the Antigravity --server branch
	// covers the relay-URL form.
	if live.URL == "" &&
		live.RelayURL != "" &&
		slices.Contains(expectedURLs, live.RelayURL) &&
		clients.IsMcphubBinary(live.RelayExePath) {
		return true, "v0.4.x upgrade backfill — relay --url matches manifest expectation (relay_url=" + live.RelayURL + ")"
	}
	return false, ""
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
