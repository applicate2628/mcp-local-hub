package clients

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/pelletier/go-toml/v2"
)

// NewCodexCLI returns a Client bound to ~/.codex/config.toml.
func NewCodexCLI() (Client, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	return newLockingClient(&codexCLI{path: filepath.Join(home, ".codex", "config.toml")}), nil
}

type codexCLI struct {
	path string
}

func (c *codexCLI) Name() string       { return "codex-cli" }
func (c *codexCLI) ConfigPath() string { return c.path }

// IsRelayStdio reports false: codex-cli is a URL-native HTTP MCP client.
func (c *codexCLI) IsRelayStdio() bool { return false }

func (c *codexCLI) Exists() bool {
	_, err := os.Stat(c.path)
	return err == nil
}

func (c *codexCLI) Backup() (string, error) {
	return writeBackup(c.path, c.Name(), 0)
}

func (c *codexCLI) BackupKeep(keepN int) (string, error) {
	return writeBackup(c.path, c.Name(), keepN)
}

// InitEmpty seeds ~/.codex/config.toml with an empty `[mcp_servers]`
// TOML table if the file is absent. The table header is intentionally
// declared (rather than dropping an empty file) so a user inspecting
// the stub sees exactly where AddEntry will append new servers.
// codex-cli reads many other settings from the same config.toml, but
// because InitEmpty fires only when the file is missing, no
// user-authored configuration can be clobbered.
func (c *codexCLI) InitEmpty() (created bool, err error) {
	return EnsureClientConfigStub(c.path, []byte("[mcp_servers]\n"))
}

func (c *codexCLI) Restore(backupPath string) error {
	// Route the live-config rewrite through WriteConfigFile so
	// production restores inherit the SecureWriteClientConfig pipeline.
	data, err := os.ReadFile(backupPath)
	if err != nil {
		return err
	}
	return WriteConfigFile(c.path, data)
}

// readTOML / writeTOML round-trip through map[string]any so unknown sections survive.
func (c *codexCLI) readTOML() (map[string]any, error) {
	data, err := os.ReadFile(c.path)
	if err != nil {
		if os.IsNotExist(err) {
			return map[string]any{}, nil
		}
		return nil, err
	}
	var m map[string]any
	if err := toml.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("parse %s: %w", c.path, err)
	}
	if m == nil {
		m = map[string]any{}
	}
	return m, nil
}

func (c *codexCLI) writeTOML(m map[string]any) error {
	return c.writeTOMLWithWriter(m, nil)
}

func (c *codexCLI) writeTOMLWithWriter(m map[string]any, writer WriteConfigFileFunc) error {
	out, err := toml.Marshal(m)
	if err != nil {
		return err
	}
	// Route through WriteConfigFile so production gets the
	// SecureWriteClientConfig pipeline (handle-relative + DACL-bound)
	// for token-bearing rewrites; tests get the os.WriteFile fallback.
	return writeConfigFileWith(writer, c.path, out)
}

func (c *codexCLI) AddEntry(entry MCPEntry) error {
	return c.AddEntryWithConfigWriter(entry, nil)
}

func (c *codexCLI) AddEntryWithConfigWriter(entry MCPEntry, writer WriteConfigFileFunc) error {
	m, err := c.readTOML()
	if err != nil {
		return err
	}
	servers, _ := m["mcp_servers"].(map[string]any)
	if servers == nil {
		servers = map[string]any{}
	}
	// Replace the entry wholesale — this drops any stdio-era fields like `command`/`args`.
	entryMap := map[string]any{
		"url":                 entry.URL,
		"startup_timeout_sec": 10.0,
	}
	if len(entry.Headers) > 0 {
		entryMap["http_headers"] = entry.Headers
	}
	servers[entry.Name] = entryMap
	m["mcp_servers"] = servers
	return c.writeTOMLWithWriter(m, writer)
}

func (c *codexCLI) RemoveEntry(name string) error {
	m, err := c.readTOML()
	if err != nil {
		return err
	}
	servers, _ := m["mcp_servers"].(map[string]any)
	if servers == nil {
		return nil
	}
	delete(servers, name)
	m["mcp_servers"] = servers
	return c.writeTOML(m)
}

func (c *codexCLI) GetEntry(name string) (*MCPEntry, error) {
	m, err := c.readTOML()
	if err != nil {
		return nil, err
	}
	servers, _ := m["mcp_servers"].(map[string]any)
	if servers == nil {
		return nil, nil
	}
	raw, ok := servers[name].(map[string]any)
	if !ok {
		return nil, nil
	}
	return classifyURLRawEntry(name, raw, "url", "http_headers"), nil
}

// LatestBackupPath delegates to the shared helper.
func (c *codexCLI) LatestBackupPath() (string, bool, error) {
	return latestBackup(c.path, c.Name())
}

// RestoreEntryFromBackup reads the TOML backup, extracts the
// [mcp_servers.<name>] table (if present), and writes it over the live
// config's corresponding entry. Other [mcp_servers.*] tables are left
// untouched.
//
// Defensively refuses if the backup's copy of the named entry is
// already in hub-HTTP form (has a `url` key and no `command` key) —
// see ErrBackupEntryAlreadyMigrated.
func (c *codexCLI) RestoreEntryFromBackup(backupPath, name string) error {
	return c.restoreEntryFromBackup(backupPath, name, false)
}

// RestoreEntryFromBackupForRollback restores the backup's entry verbatim,
// bypassing the ErrBackupEntryAlreadyMigrated guard (see the interface
// doc on Client.RestoreEntryFromBackupForRollback). Install rollback and
// Serena migrate rollback use it when the timestamped backup is the source of
// truth.
func (c *codexCLI) RestoreEntryFromBackupForRollback(backupPath, name string) error {
	return c.restoreEntryFromBackup(backupPath, name, true)
}

func (c *codexCLI) RestoreEntryFromBackupForRollbackWithConfigWriter(backupPath, name string, writer WriteConfigFileFunc) error {
	return c.restoreEntryFromBackupWithWriter(backupPath, name, true, writer)
}

// restoreEntryFromBackup is the shared body. When allowHubEntry is false
// (demigrate) it refuses a hub-HTTP-shaped backup entry with
// ErrBackupEntryAlreadyMigrated; when true (migrate rollback) it writes
// the backup bytes verbatim regardless of shape.
func (c *codexCLI) restoreEntryFromBackup(backupPath, name string, allowHubEntry bool) error {
	return c.restoreEntryFromBackupWithWriter(backupPath, name, allowHubEntry, nil)
}

func (c *codexCLI) restoreEntryFromBackupWithWriter(backupPath, name string, allowHubEntry bool, writer WriteConfigFileFunc) error {
	backupData, err := os.ReadFile(backupPath)
	if err != nil {
		return fmt.Errorf("read backup %s: %w", backupPath, err)
	}
	// Re-attach the backup file path to any error the path-free core returns, so
	// the file-backed demigrate/rollback caller keeps a "which backup failed"
	// diagnostic (the core stays path-free for Phase 3's in-memory snapshot
	// bytes). %w preserves ErrBackupEntryAlreadyMigrated for errors.Is callers.
	if err := c.restoreEntryFromBytes(backupData, name, allowHubEntry, writer); err != nil {
		return fmt.Errorf("restore backup %s: %w", backupPath, err)
	}
	return nil
}

// restoreEntryFromBytes is the post-ReadFile restore core: given the
// already-read backup bytes it parses them, reads the live config once, and
// surgically restores (or strips) the named entry.
// restoreEntryFromBackupWithWriter is the thin file-reading wrapper over this
// core. The parse error omits the source path because this core also serves
// callers that pass in-memory bytes with no backing file.
func (c *codexCLI) restoreEntryFromBytes(backupData []byte, name string, allowHubEntry bool, writer WriteConfigFileFunc) error {
	// err is declared here because the os.ReadFile that previously declared it
	// now lives in the wrapper; the demigrate branch below assigns it via
	// `liveMap, err = c.readTOML()`.
	var err error
	var backupMap map[string]any
	if len(backupData) > 0 {
		if err := toml.Unmarshal(backupData, &backupMap); err != nil {
			return fmt.Errorf("parse backup: %w", err)
		}
	}
	if backupMap == nil {
		backupMap = map[string]any{}
	}
	backupServers, _ := backupMap["mcp_servers"].(map[string]any)

	// The live map is read exactly ONCE per path and never falls back to a stale
	// snapshot (design round-7, Sol+Terra P1). Rollback (allowHubEntry=true) reads
	// AFTER the whole-file recovery helper so the surgical write reflects current
	// on-disk state; demigrate (false) keeps its single early read.
	var liveMap map[string]any
	var liveServers map[string]any
	if allowHubEntry {
		// Whole-file-gone recovery FIRST (design round-5): SecureWrite path #1 may have
		// REMOVED the write-target file (target entry + siblings). Recover the whole
		// backup before the entry-scoped skip, which would else false-skip the
		// both-absent case or surgically recreate only the target entry (losing siblings).
		if handled, werr := wholeFileRestoreIfWriteTargetGone(c.path, backupData); handled {
			return werr
		}
		// The helper fell through (handled=false) — either the common file-present
		// path #2, or a create/no-replace CONFLICT (an external process recreated
		// c.path, possibly with a NEW sibling S', in the stat→create window; the
		// no-replace create sees EEXIST and did NOT publish the backup bytes). Read
		// the live map ONCE here, AFTER the helper, and treat it as AUTHORITATIVE:
		// the surgical write below (unlike the JSONC bodies' setMember/deleteMember,
		// which re-read at mutate time) serializes THIS whole map, so it must reflect
		// current on-disk state and preserve S' rather than clobber it. For the
		// non-race path #2 the read equals the pre-helper state (no-op-equivalent).
		// A read FAILURE (transient / partial TOML written by the racing recreate)
		// must ABORT with that error — it flows to the Install rollback closure
		// (InstallClientRollbackIncompleteError) so adopt PRESERVES the provenance;
		// it must NEVER fall back to a stale/earlier map and silently clobber S'
		// while reporting success (design round-7, Sol+Terra P1). Still under the
		// single withConfigLock hold, so this stays TOCTOU-free.
		freshMap, rerr := c.readTOML()
		if rerr != nil {
			return rerr
		}
		liveMap = freshMap
		liveServers, _ = liveMap["mcp_servers"].(map[string]any)
		if liveServers == nil {
			liveServers = map[string]any{}
		}
		// Rollback-only atomic entry-scoped skip-if-unchanged (design round-4): the
		// live read above + the write below run under the SAME withConfigLock hold, so
		// this compare-then-restore is TOCTOU-free. If the write-target already holds
		// the backup's entry value (the client was never mutated, or a sibling edit
		// left THIS entry untouched) return nil WITHOUT writing — no redundant/damaging
		// restore.
		le, lp := liveServers[name]
		be, bp := backupServers[name]
		if entryRestoreIsNoop(le, lp, be, bp) {
			return nil
		}
	} else {
		// Demigrate path: single early read, unchanged. It keeps its
		// ErrBackupEntryAlreadyMigrated guard (below) exactly as before.
		liveMap, err = c.readTOML()
		if err != nil {
			return err
		}
		liveServers, _ = liveMap["mcp_servers"].(map[string]any)
		if liveServers == nil {
			liveServers = map[string]any{}
		}
	}
	if backupServers != nil {
		if backupEntry, present := backupServers[name]; present {
			// Defensive: refuse hub-HTTP-shaped backup entries for
			// Codex CLI (loopback `url` present, `command` absent).
			// User-configured remote HTTP entries (non-loopback url)
			// pass through. The rollback caller (allowHubEntry=true)
			// bypasses this guard to restore the pre-reconcile legacy
			// hub entry verbatim.
			if !allowHubEntry {
				if rawMap, ok := backupEntry.(map[string]any); ok {
					if isHubURLShapeEntry(rawMap, "url") {
						return ErrBackupEntryAlreadyMigrated
					}
				}
			}
			liveServers[name] = backupEntry
			liveMap["mcp_servers"] = liveServers
			return c.writeTOMLWithWriter(liveMap, writer)
		}
	}
	delete(liveServers, name)
	liveMap["mcp_servers"] = liveServers
	return c.writeTOMLWithWriter(liveMap, writer)
}

// AllStdioEntries returns every stdio entry from [mcp_servers.*].
func (c *codexCLI) AllStdioEntries() ([]StdioEntry, error) {
	m, err := c.readTOML()
	if err != nil {
		return nil, err
	}
	servers, _ := m["mcp_servers"].(map[string]any)
	return collectStdioEntries(servers), nil
}

// FindStdioLanguageServerEntries scans [mcp_servers.*] for stdio
// entries matching the mcp-language-server invocation pattern.
func (c *codexCLI) FindStdioLanguageServerEntries() ([]LanguageServerStdioEntry, error) {
	m, err := c.readTOML()
	if err != nil {
		return nil, err
	}
	servers, _ := m["mcp_servers"].(map[string]any)
	return findLanguageServerStdioInMap(servers), nil
}

// BackupContainsEntry reports whether the backup file at backupPath
// has an [mcp_servers.<name>] table.
func (c *codexCLI) BackupContainsEntry(backupPath, name string) (bool, error) {
	data, err := os.ReadFile(backupPath)
	if err != nil {
		return false, fmt.Errorf("read backup %s: %w", backupPath, err)
	}
	if len(data) == 0 {
		return false, nil
	}
	var m map[string]any
	if err := toml.Unmarshal(data, &m); err != nil {
		return false, fmt.Errorf("parse backup %s: %w", backupPath, err)
	}
	servers, _ := m["mcp_servers"].(map[string]any)
	if servers == nil {
		return false, nil
	}
	// Require the entry to be a table (map). A scalar value would
	// be malformed in TOML at this path; treat as absent so
	// sentinel fallback refuses rather than silently writes
	// corrupted data via RestoreEntryFromBackup.
	entry, ok := servers[name].(map[string]any)
	return ok && entry != nil, nil
}

// BackupEntryIsHubManaged reports whether [mcp_servers.<name>] in the
// TOML backup at backupPath is in Codex CLI's hub-HTTP shape (loopback
// `url` present, `command` absent). See Client.BackupEntryIsHubManaged.
func (c *codexCLI) BackupEntryIsHubManaged(backupPath, name string) (bool, error) {
	data, err := os.ReadFile(backupPath)
	if err != nil {
		return false, fmt.Errorf("read backup %s: %w", backupPath, err)
	}
	if len(data) == 0 {
		return false, nil
	}
	var m map[string]any
	if err := toml.Unmarshal(data, &m); err != nil {
		return false, fmt.Errorf("parse backup %s: %w", backupPath, err)
	}
	servers, _ := m["mcp_servers"].(map[string]any)
	if servers == nil {
		return false, nil
	}
	entry, ok := servers[name].(map[string]any)
	if !ok || entry == nil {
		return false, nil
	}
	return isHubURLShapeEntry(entry, "url"), nil
}
