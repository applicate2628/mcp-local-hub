package clients

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// MCPEntry describes one MCP server entry in a client's config.
// The hub uses this to add/update/remove entries idempotently.
//
// Most adapters consume the URL directly (clients that speak HTTP MCP
// natively). Adapters for stdio-only clients — currently only Antigravity —
// consume the RelayServer/RelayDaemon/RelayExePath triple instead and
// write a 'command'+'args' entry invoking `mcphub.exe relay`. Install.go
// populates all fields so individual adapters ignore what they don't need.
type MCPEntry struct {
	Name    string            // server name, e.g., "serena"
	URL     string            // full URL, e.g., "http://localhost:9121/mcp"
	Headers map[string]string // optional HTTP headers
	Env     map[string]string // only used by stdio entries (for rollback); URL entries leave this nil

	// Relay-based stdio adapters (Antigravity): these three fields identify
	// the manifest lookup the stdio client should perform when it spawns
	// mcphub.exe relay as its child process.
	RelayServer  string // server name in manifest, e.g., "serena"
	RelayDaemon  string // daemon name within that manifest, e.g., "claude"
	RelayExePath string // absolute path to mcphub.exe (from os.Executable() at install time)
}

// Client is the OS-/format-abstracted interface for a single MCP client config file.
// Implementations live in one file per client.
type Client interface {
	// Name returns a stable identifier ("claude-code", "codex-cli", "gemini-cli", "antigravity")
	// used in manifest client_bindings.
	Name() string

	// ConfigPath returns the absolute path to the config file this client reads.
	// Used for display, backup, and existence checks.
	ConfigPath() string

	// Exists reports whether the config file is present. If false, AddEntry/RemoveEntry
	// are no-ops and Backup returns ErrClientNotInstalled.
	Exists() bool

	// Backup copies the current config to a sibling file ending in ".bak-mcp-local-hub-<timestamp>"
	// and returns the path. Overwrites any previous backup with the same timestamp-second.
	//
	// As a side effect, the first ever Backup call also writes a one-shot pristine
	// sentinel "<path>.bak-mcp-local-hub-original" that captures the config as it
	// existed before mcp-local-hub touched it. The sentinel is never overwritten
	// on subsequent calls — it stays pointing at the user's pre-hub state so a
	// full uninstall can always reach a clean slate regardless of how many
	// install/migrate cycles have happened in between.
	//
	// Backup does NOT prune older timestamped backups. Use BackupKeep for that.
	Backup() (string, error)

	// BackupKeep behaves like Backup (sentinel + timestamped copy) but, after
	// writing the new timestamped backup, prunes older timestamped backups so
	// that at most keepN of them remain on disk. The pristine `-original`
	// sentinel is never pruned. If keepN <= 0, no pruning happens (same
	// behavior as Backup).
	BackupKeep(keepN int) (string, error)

	// Restore copies the named backup over the live config, overwriting current content.
	Restore(backupPath string) error

	// AddEntry adds or replaces the MCP server entry named entry.Name.
	// Creates parent `mcpServers` / `[mcp_servers.*]` section if missing.
	AddEntry(entry MCPEntry) error

	// RemoveEntry removes the MCP server entry with the given name.
	// Returns nil if the entry does not exist (idempotent).
	RemoveEntry(name string) error

	// GetEntry returns the current value of the named entry, or nil if missing.
	GetEntry(name string) (*MCPEntry, error)

	// LatestBackupPath returns the absolute path to the most recent
	// mcp-local-hub backup of this client's config. Timestamped
	// backups (.bak-mcp-local-hub-<YYYYMMDD-HHMMSS>) take precedence
	// over the pristine -original sentinel. Returns (path, true, nil)
	// when a backup exists, ("", false, nil) when none do, (_, _, err)
	// on a filesystem error.
	LatestBackupPath() (string, bool, error)

	// RestoreEntryFromBackup reads the backup file at backupPath,
	// extracts the entry named `name`, and writes that raw pre-migrate
	// shape to the live config — overwriting any current entry with
	// the same name. If the backup does NOT contain the entry (i.e.
	// migrate added it from scratch and there was no prior entry),
	// removes the current entry. Returns an error if the backup file
	// cannot be opened or parsed. Idempotent if the live config is
	// already in the backup's shape. Other entries in the live config
	// are untouched.
	RestoreEntryFromBackup(backupPath, name string) error
}

// ErrClientNotInstalled signals the client's config file does not exist on this machine.
type ErrClientNotInstalled struct{ Client string }

func (e *ErrClientNotInstalled) Error() string {
	return "client not installed: " + e.Client
}

// ErrBackupEntryAlreadyMigrated is returned by RestoreEntryFromBackup
// when the backup file's copy of the named entry is already in
// hub-HTTP form (for JSON/TOML clients) or hub-relay form (for
// Antigravity). This happens when a backup was taken AFTER an earlier
// migrate of the same client had already rewritten the entry —
// typically the "newest" backup when multiple servers are migrated
// sequentially from the same client. Restoring from such a backup
// would silently re-write the hub-managed form, defeating demigrate.
// Callers (Demigrate) must surface this as a Failed row and instruct
// the operator to demigrate newest-first or restore manually from
// the `-original` sentinel.
var ErrBackupEntryAlreadyMigrated = errors.New("clients: backup copy of entry is already in hub-managed shape")

// IsMcphubBinary reports whether cmd's basename matches the mcphub
// executable name. Case-insensitive to cover Windows (mcphub.exe) and
// POSIX (mcphub). Used by both internal/api/scan.go's Antigravity
// relay-reject branch and the per-adapter RestoreEntryFromBackup
// hub-relay detection to avoid false positives against user stdio
// entries whose first argument happens to be the literal string
// "relay". Exported so internal/api/scan.go (package api) can call it
// via clients.IsMcphubBinary; within package clients it is called as
// the unqualified IsMcphubBinary.
func IsMcphubBinary(cmd string) bool {
	if cmd == "" {
		return false
	}
	base := strings.ToLower(filepath.Base(cmd))
	return base == "mcphub" || base == "mcphub.exe"
}

// AllClients returns the map of {client-name -> Client} for every supported
// adapter. Factories that return an error (e.g. UserHomeDir failure) are
// silently skipped, so callers that iterate the map see only adapters that
// could be constructed on the current host. This is the shared accessor
// used by both internal/api and internal/cli.
func AllClients() map[string]Client {
	result := map[string]Client{}
	for _, factory := range []func() (Client, error){
		NewClaudeCode, NewCodexCLI, NewGeminiCLI, NewAntigravity,
	} {
		c, err := factory()
		if err != nil {
			continue
		}
		result[c.Name()] = c
	}
	return result
}

// backupSuffixPrefix is the shared filename fragment that identifies every
// backup file produced by mcp-local-hub. Both the pristine sentinel and the
// rolling timestamped copies start with this prefix.
const backupSuffixPrefix = ".bak-mcp-local-hub-"

// originalSentinelSuffix names the one-shot pristine backup written the very
// first time an adapter backs up a config file. It captures the user's
// pre-hub state so a full uninstall can always reach a clean slate.
const originalSentinelSuffix = backupSuffixPrefix + "original"

// writeBackup is the shared Backup implementation for every adapter. It
// reads livePath, writes (exactly once) the pristine `-original` sentinel
// if it does not already exist, writes a fresh timestamped backup, and
// optionally prunes older timestamped backups so only keepN remain.
//
// If livePath does not exist, returns ErrClientNotInstalled{Client: clientName}
// to preserve the error contract every adapter already had.
//
// If keepN <= 0, pruning is skipped (matching the pre-rotation Backup()
// contract used by install.go / migrate.go).
func writeBackup(livePath, clientName string, keepN int) (string, error) {
	if _, err := os.Stat(livePath); err != nil {
		if os.IsNotExist(err) {
			return "", &ErrClientNotInstalled{Client: clientName}
		}
		return "", err
	}

	// One-shot pristine sentinel: written only when missing, never overwritten.
	// This is what makes a full uninstall reversible even after many rolling
	// backups have aged out.
	sentinel := livePath + originalSentinelSuffix
	if _, err := os.Stat(sentinel); os.IsNotExist(err) {
		if err := copyFile(livePath, sentinel, 0600); err != nil {
			return "", fmt.Errorf("write sentinel: %w", err)
		}
	}

	// Timestamped rolling backup. Windows filesystems give second-resolution
	// mtime only, so two calls in the same second land on the same filename
	// and the second call overwrites the first — harmless, since the content
	// is the current live config either way.
	bakPath := livePath + backupSuffixPrefix + time.Now().Format("20060102-150405")
	if err := copyFile(livePath, bakPath, 0600); err != nil {
		return "", err
	}

	if keepN > 0 {
		pruneOldTimestamped(livePath, keepN)
	}
	return bakPath, nil
}

func copyFile(src, dst string, perm os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, perm)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	// Close explicitly so flush/commit errors (disk full, NFS fsync failure)
	// surface here instead of being swallowed by a deferred Close — otherwise
	// writeBackup reports success on a truncated backup file.
	if err := out.Close(); err != nil {
		return err
	}
	return nil
}

// pruneOldTimestamped keeps only the keepN most recent timestamped backups
// of livePath. The pristine `-original` sentinel is always preserved.
// Errors during listing or removal are intentionally swallowed — pruning is
// best-effort; a failed unlink must not break a successful Backup call.
func pruneOldTimestamped(livePath string, keepN int) {
	dir := filepath.Dir(livePath)
	base := filepath.Base(livePath)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	prefix := base + backupSuffixPrefix
	type bak struct {
		path    string
		modTime time.Time
	}
	var timestamped []bak
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasPrefix(name, prefix) {
			continue
		}
		if name == base+originalSentinelSuffix {
			continue // sentinel, never touch
		}
		fi, err := e.Info()
		if err != nil {
			continue
		}
		timestamped = append(timestamped, bak{
			path:    filepath.Join(dir, name),
			modTime: fi.ModTime(),
		})
	}
	if len(timestamped) <= keepN {
		return
	}
	// Newest first, then drop everything past index keepN-1.
	sort.Slice(timestamped, func(i, j int) bool {
		return timestamped[i].modTime.After(timestamped[j].modTime)
	})
	for _, b := range timestamped[keepN:] {
		_ = os.Remove(b.path)
	}
}

// latestBackup returns the most recent mcp-local-hub backup path for
// livePath. Timestamped copies (livePath + ".bak-mcp-local-hub-<ts>")
// take precedence over the pristine "-original" sentinel; within
// timestamped copies the lexicographically-largest name wins (timestamps
// use the 20060102-150405 layout, which sorts correctly as a string).
// Directories with matching names are ignored. Returns ("", false, nil)
// when no backup files are present and (_, _, err) on filesystem error.
// The second parameter (clientName) is currently unused but reserved for
// future per-client log/diagnostic context.
func latestBackup(livePath, _ string) (string, bool, error) {
	dir := filepath.Dir(livePath)
	prefix := filepath.Base(livePath) + ".bak-mcp-local-hub-"
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return "", false, nil
		}
		return "", false, err
	}
	var timestamped []string
	var original string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasPrefix(name, prefix) {
			continue
		}
		suffix := strings.TrimPrefix(name, prefix)
		if suffix == "original" {
			original = filepath.Join(dir, name)
			continue
		}
		timestamped = append(timestamped, filepath.Join(dir, name))
	}
	if len(timestamped) > 0 {
		sort.Strings(timestamped)
		return timestamped[len(timestamped)-1], true, nil
	}
	if original != "" {
		return original, true, nil
	}
	return "", false, nil
}
