package clients

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
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

	// RelayURL, when non-empty, makes the relay-based stdio adapter
	// (Antigravity) emit a direct `relay --url <RelayURL>` invocation
	// instead of the manifest-lookup `relay --server <s> --daemon <d>`
	// form. The relay's --url escape hatch is mutually exclusive with
	// --server/--daemon (see internal/cli/relay.go resolveRelayURL), so
	// the adapter emits ONLY --url when this is set. Used by the serena
	// dynamic-pool client-reconcile to point the Antigravity relay at the
	// constant /serena/mcp router endpoint (which has no per-daemon
	// manifest port to resolve), per the descriptor-proxy design §5.
	// Empty preserves the legacy --server/--daemon behavior for every
	// existing caller.
	RelayURL string
}

// Client is the OS-/format-abstracted interface for a single MCP client config file.
// Implementations live in one file per client.
type Client interface {
	// Name returns a stable identifier such as "claude-code" or "codex-cli"
	// used in manifest client_bindings.
	Name() string

	// ConfigPath returns the absolute path to the config file this client reads.
	// Used for display, backup, and existence checks.
	ConfigPath() string

	// Exists reports whether the config file is present. If false, AddEntry/RemoveEntry
	// are no-ops and Backup returns ErrClientNotInstalled.
	Exists() bool

	// InitEmpty creates the config file with an empty client-shaped stub
	// (e.g. `{"mcpServers": {}}` for JSON-based clients, `[mcp_servers]\n`
	// for codex-cli's TOML) if and only if no regular file exists at
	// the destination. Used by the Servers matrix "Initialize"
	// affordance (v0.4.5) so an operator who has the client installed
	// but has never created an MCP config can prepare it from the GUI
	// without leaving the app.
	//
	// Returns (created, err):
	//
	//   - created=true, err=nil: this call wrote the stub bytes.
	//   - created=false, err=nil: a regular file already existed
	//     (idempotent success — covers the second-click and
	//     concurrent-writer race-lost cases).
	//   - created=false, err!=nil: destination is a symlink or
	//     other non-regular entry (refused), or an I/O failure
	//     surfaced from the underlying create.
	//
	// The call routes through clients.EnsureClientConfigStub which
	// uses an atomic temp-then-hardlink publish pattern and refuses
	// to follow symlinks/reparse points at the destination — see
	// that helper for the security rationale.
	//
	// Adapters do NOT mkdir-p the parent directory. Callers must
	// ensure the parent exists before invoking. The
	// /api/init-client-config endpoint enforces this with its own
	// pre-write stat. BackupKeep adapter wrappers (vscode, cursor,
	// qwen-cli) MkdirAll explicitly before calling InitEmpty since
	// their seed-then-backup path is the documented "create from
	// scratch on a fresh host" surface.
	InitEmpty() (created bool, err error)

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
	//
	// Defensively refuses (ErrBackupEntryAlreadyMigrated) when the
	// backup's copy of the entry is itself already in hub-managed shape
	// — the demigrate guard that prevents restoring a hub-HTTP /
	// hub-relay entry as if it were the pre-hub form. This is the
	// correct behavior for the normal demigrate flow.
	RestoreEntryFromBackup(backupPath, name string) error

	// RestoreEntryFromBackupForRollback is RestoreEntryFromBackup with
	// the ErrBackupEntryAlreadyMigrated demigrate guard BYPASSED: it
	// writes the backup's copy of the named entry to the live config
	// verbatim even when that copy is in hub-managed shape (a loopback
	// hub-HTTP URL for URL clients, or an mcphub `relay` invocation for
	// Antigravity). Every other behavior is identical to
	// RestoreEntryFromBackup (restore the snapshotted entry, or remove
	// the live entry when the backup lacks it; other entries untouched).
	//
	// This exists for ONE caller: the serena dynamic-pool migrate's
	// controlled abort-rollback (RestoreSerenaReconcileApplied). When
	// the migrate rewrites pre-cutover clients legacy-9121 → /serena/mcp
	// and then aborts before committing the dynamic-pool intent, each
	// rewritten client's pre-reconcile backup IS the legacy hub entry
	// (`http://localhost:9121/mcp`, or Antigravity's `mcphub relay`
	// form). Restoring that known pre-reconcile snapshot is exactly what
	// the rollback must do, but RestoreEntryFromBackup would reject it
	// with ErrBackupEntryAlreadyMigrated. The rollback path uses this
	// variant to put the verbatim pre-reconcile bytes back; the
	// demigrate guard stays in force for the normal demigrate flow.
	RestoreEntryFromBackupForRollback(backupPath, name string) error

	// BackupContainsEntry reports whether the backup file at backupPath
	// has a parsed server entry named `name`. Used by Demigrate's
	// sentinel-fallback path to distinguish "entry absent from sentinel"
	// (the server was added AFTER the sentinel was written, so
	// RestoreEntryFromBackup would silently delete it — destructive)
	// from "entry present in sentinel" (safe to restore). Returns
	// (false, nil) on present-but-malformed and on absent; returns
	// (_, err) only for I/O or parse errors.
	BackupContainsEntry(backupPath, name string) (bool, error)

	// BackupEntryIsHubManaged reports whether the backup file at
	// backupPath holds the entry named `name` in this adapter's
	// hub-managed shape — the same shape detection RestoreEntryFromBackup
	// uses to refuse a hub entry with ErrBackupEntryAlreadyMigrated, but
	// surfaced as a side-effect-free predicate:
	//
	//   - URL-native JSON / TOML clients: entry has a hub loopback URL
	//     (IsHubHTTPURL) AND no `command` field.
	//   - Antigravity: entry's `command` is the mcphub binary AND
	//     args[0] == "relay".
	//
	// Returns (true, nil) only when the entry is present AND in
	// hub-managed shape; (false, nil) when the entry is present in a
	// pre-hub (direct/stdio) form, absent, or present-but-malformed;
	// (_, err) only for I/O or parse errors.
	//
	// Demigrate uses this in the legacy-codename backup fallback to
	// distinguish "this legacy backup holds the entry in hub-managed
	// form" (skip — no pre-hub state) from "this legacy backup holds the
	// entry in its true pre-hub form" (restore from THIS backup) WITHOUT
	// the delete-on-entry-absent side effect that RestoreEntryFromBackup
	// carries. Mirrors the always-restore-over-delete invariant in
	// work-items/bugs/2026-05-15-demigrate-fallback-when-no-pre-hub-form.md
	// §"Failed attempt: PR #218".
	BackupEntryIsHubManaged(backupPath, name string) (bool, error)

	// AllStdioEntries returns every stdio MCP server entry currently
	// present in this client's config — that is, every entry where the
	// adapter's format-specific shape contains a non-empty `command`
	// field (and therefore corresponds to a stdio-spawned subprocess).
	// HTTP-only entries (no `command`, just `url`) are skipped.
	//
	// Used by `mcphub cleanup --scan-clients` (A6) to extract cmdline
	// patterns for reverse-lookup orphan detection: a running process
	// whose cmdline matches one of these (command, args) signatures
	// but whose ancestor chain contains neither a live mcphub daemon
	// nor a known client launcher process is treated as a leaked
	// stdio child (typically: the spawning client exited or the entry
	// was migrated to HTTP and the user has not restarted the client
	// yet).
	//
	// Returns nil with nil error when the config does not exist or has
	// no stdio entries. Read/parse failures propagate so callers can
	// distinguish "no entries" from "config broken".
	AllStdioEntries() ([]StdioEntry, error)

	// FindStdioLanguageServerEntries scans the client's config for
	// stdio entries that look like mcp-language-server invocations and
	// returns them. Used by `mcphub language-server cleanup` to
	// surface legacy stdio entries that should be removed AFTER the
	// user has run `mcphub register` to install the HTTP-backed
	// language-server daemons. With both stdio and HTTP entries live
	// in the same client config, the agent spawns two parallel LSP
	// processes per language — defeating mcphub's process-tail
	// compression value prop.
	//
	// An entry qualifies if:
	//   1. its `command` basename equals "mcp-language-server" (case-
	//      insensitive, .exe suffix stripped), AND
	//   2. its `args` contain either "--lsp <language>" (two tokens)
	//      or "--lsp=<language>" (single token).
	//
	// HTTP-shaped entries (no `command`, has `url`) are silently
	// skipped, so the mcphub-written `mcp-language-server-<lang>`
	// entries are NEVER returned. Returns nil with nil error when no
	// such entries exist; returns an error only on read/parse
	// failure. Idempotent: re-running after cleanup returns nil.
	FindStdioLanguageServerEntries() ([]LanguageServerStdioEntry, error)
}

// StdioEntry is the format-agnostic shape of one stdio MCP server
// entry surfaced by AllStdioEntries. Name is the entry key in the
// client config; Command is the raw `command` value; Args is the
// raw `args` slice (string elements only — non-string members of
// the parsed JSON/TOML array are dropped because they cannot
// participate in cmdline-substring matching).
type StdioEntry struct {
	Name    string
	Command string
	Args    []string
}

// collectStdioEntries iterates servers (a parsed client-config map
// from any adapter's format) and returns every ACTIVE stdio entry
// with a non-empty `command` field as a StdioEntry. Entries that are
// either HTTP (no `command`, has `url`) or explicitly marked
// `"disabled": true` are skipped. Stable sort by Name keeps CLI
// output and test fixtures deterministic.
//
// The `disabled` flag is supported by several client schemas
// (Antigravity, Cursor, VS Code, jsonMCPClient-based adapters).
// Codex bot r3 P2 on PR #190: a user who turns off an entry
// without removing it is signaling "this is not running" — the
// reverse-lookup orphan detector must not derive kill-patterns from
// those entries, or it could match unrelated workstation processes
// that happen to share the same signature.
func collectStdioEntries(servers map[string]any) []StdioEntry {
	if len(servers) == 0 {
		return nil
	}
	var out []StdioEntry
	for name, raw := range servers {
		entryMap, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		cmd, _ := entryMap["command"].(string)
		if cmd == "" {
			continue
		}
		if disabled, _ := entryMap["disabled"].(bool); disabled {
			continue
		}
		// Codex CLI uses `enabled: false` (default true) instead
		// of `disabled`. An entry with enabled=false is configured
		// but not active — treat as disabled so its (command, args)
		// don't contribute to A6 kill-pattern derivation.
		if enabled, present := entryMap["enabled"]; present {
			if b, ok := enabled.(bool); ok && !b {
				continue
			}
		}
		args := extractStringSlice(entryMap["args"])
		out = append(out, StdioEntry{
			Name:    name,
			Command: cmd,
			Args:    args,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// extractStringSlice returns the string members of a parsed JSON
// or TOML array, in original order. Non-string elements are
// dropped (e.g. numbers, booleans, nested objects). nil/non-list
// input returns nil.
func extractStringSlice(raw any) []string {
	list, ok := raw.([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(list))
	for _, a := range list {
		if s, ok := a.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

// LanguageServerStdioEntry describes one stdio mcp-language-server
// entry surfaced by FindStdioLanguageServerEntries. Name is the
// adapter-format-specific entry key (e.g. TOML table name for codex,
// JSON object key for claude/gemini/cursor/vscode/qwen); Command is the
// raw `command` value from the entry (for diagnostic display);
// Language is the value extracted from "--lsp <X>" or "--lsp=<X>" in
// the args list, or "" when the args list does not declare one.
type LanguageServerStdioEntry struct {
	Name     string
	Command  string
	Language string
}

// matchLanguageServerStdio classifies one parsed entry map as a stdio
// mcp-language-server invocation. Returns the raw command string, the
// extracted language (always non-empty when ok=true), and ok=true
// only when BOTH:
//   - the command basename matches the LSP binary (case-insensitive,
//     .exe stripped, separator-agnostic), AND
//   - the args list declares "--lsp <X>" or "--lsp=X".
//
// Refusing matches without an explicit --lsp arg keeps cleanup from
// deleting standalone or experimental `mcp-language-server` entries
// that operators may have configured for purposes other than
// language-routed LSP (codex bot r1 P1.2).
func matchLanguageServerStdio(raw map[string]any) (cmd, language string, ok bool) {
	cmd, _ = raw["command"].(string)
	if !isLanguageServerBinary(cmd) {
		return "", "", false
	}
	language = extractLspLanguageArg(raw["args"])
	if language == "" {
		return "", "", false
	}
	return cmd, language, true
}

// isLanguageServerBinary reports whether cmd's basename (case-
// insensitive, .exe suffix stripped) equals "mcp-language-server".
// Empty string never matches. Used to keep the matcher specific to
// the well-known LSP binary and avoid catching unrelated stdio MCP
// servers the user may have named "clangd" / "fortran" / etc.
//
// Path separators are normalized via basenameAcrossSeparators so a
// Windows-style absolute path like `C:\Users\u\.local\bin\mcp-
// language-server.exe` matches on POSIX hosts too. Cross-environment
// configs (e.g. WSL pointing at a shared Windows dotfile) and
// regression tests on Linux CI both depend on that normalization
// (codex bot r1 P1.1).
func isLanguageServerBinary(cmd string) bool {
	if cmd == "" {
		return false
	}
	base := strings.ToLower(basenameAcrossSeparators(cmd))
	base = strings.TrimSuffix(base, ".exe")
	return base == "mcp-language-server"
}

// basenameAcrossSeparators returns the trailing path component of p
// regardless of separator. filepath.Base recognizes only the host
// OS's separator, so a Windows path on POSIX (or vice versa) is
// returned verbatim as one segment. This helper folds backslashes to
// forward slashes first, then takes the substring after the last
// slash. Empty string returns empty; trailing separators collapse to
// "".
func basenameAcrossSeparators(p string) string {
	p = strings.ReplaceAll(p, "\\", "/")
	if i := strings.LastIndex(p, "/"); i >= 0 {
		return p[i+1:]
	}
	return p
}

// extractLspLanguageArg scans the args slice for "--lsp <X>" (two-
// token form) or "--lsp=<X>" (single-token form) and returns X.
// Returns "" when args is not a slice, the flag is absent, or the
// value following the two-token "--lsp" is not a string. Non-string
// elements between/around the flag are ignored gracefully.
func extractLspLanguageArg(args any) string {
	list, ok := args.([]any)
	if !ok {
		return ""
	}
	for i, a := range list {
		s, ok := a.(string)
		if !ok {
			continue
		}
		if s == "--lsp" && i+1 < len(list) {
			if next, ok := list[i+1].(string); ok {
				return next
			}
		}
		if rest, ok := strings.CutPrefix(s, "--lsp="); ok {
			return rest
		}
	}
	return ""
}

// findLanguageServerStdioInMap iterates servers (a map of
// entry-name -> entry-data parsed from any adapter's config format)
// and returns every entry that classifies as stdio mcp-language-
// server per matchLanguageServerStdio. Results are sorted by Name for
// stable CLI output. nil/empty servers yields a nil slice.
func findLanguageServerStdioInMap(servers map[string]any) []LanguageServerStdioEntry {
	if len(servers) == 0 {
		return nil
	}
	var out []LanguageServerStdioEntry
	for name, raw := range servers {
		entryMap, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		cmd, lang, ok := matchLanguageServerStdio(entryMap)
		if !ok {
			continue
		}
		out = append(out, LanguageServerStdioEntry{
			Name:     name,
			Command:  cmd,
			Language: lang,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
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
// migrate of the same client had already rewritten the entry — the
// common case being a second migrate of a different server which
// snapshots the live file while the first server is already in hub
// form. Restoring from such a backup would silently re-write the
// hub-managed form, defeating demigrate. Callers (Demigrate) must
// surface this as a Failed row and instruct the operator to restore
// manually from the `-original` sentinel (the one-shot pre-hub
// snapshot; never overwritten). Demigrate itself can only auto-
// restore the most-recently-migrated server per client; earlier
// servers in the same client require the sentinel.
var ErrBackupEntryAlreadyMigrated = errors.New("clients: backup copy of entry is already in hub-managed shape")

// IsHubHTTPURL reports whether urlStr looks like a mcp-local-hub
// managed loopback URL (`http://localhost:<port>/...`,
// `http://127.0.0.1:<port>/...`, or `http://[::1]:<port>/...`).
// Used by the per-adapter defensive
// check in RestoreEntryFromBackup to distinguish hub-managed entries
// from legitimate user-configured remote HTTP MCP servers (e.g. a
// context7-style `https://api.example.com/mcp`). A naïve "has url,
// no command" check would false-reject those. Loopback-only is a
// narrow-enough heuristic: user-run local MCP HTTP servers are rare
// and even then their backup-original shape was HTTP anyway, so
// refusing to restore them is at worst a no-op (same HTTP → same
// HTTP), never data corruption.
func IsHubHTTPURL(urlStr string) bool {
	return strings.HasPrefix(urlStr, "http://localhost:") ||
		strings.HasPrefix(urlStr, "http://127.0.0.1:") ||
		strings.HasPrefix(urlStr, "http://[::1]:")
}

// isHubURLShapeEntry reports whether a parsed client-config entry map
// is in mcphub's hub-HTTP shape for URL-native clients: the entry's
// urlField value is a hub loopback URL (IsHubHTTPURL) AND the entry has
// no `command` key. User-configured remote HTTP MCP servers (non-loopback
// url) and stdio entries (have `command`) are NOT hub-managed. This is
// the single owner of the URL-shape detection rule shared by every
// URL-native adapter's RestoreEntryFromBackup guard and by
// BackupEntryIsHubManaged. urlField is the adapter's URL key ("url" or
// "httpUrl").
func isHubURLShapeEntry(rawMap map[string]any, urlField string) bool {
	if urlStr, _ := rawMap[urlField].(string); IsHubHTTPURL(urlStr) {
		if _, hasCmd := rawMap["command"]; !hasCmd {
			return true
		}
	}
	return false
}

// isHubRelayShapeEntry reports whether a parsed client-config entry map
// is in mcphub's hub-relay shape for Antigravity: `command` is the
// mcphub binary AND args[0] == "relay". This is the single owner of the
// relay-shape detection rule shared by the Antigravity
// RestoreEntryFromBackup guard and BackupEntryIsHubManaged.
func isHubRelayShapeEntry(rawMap map[string]any) bool {
	if cmd, _ := rawMap["command"].(string); IsMcphubBinary(cmd) {
		if args, ok := rawMap["args"].([]any); ok && len(args) > 0 {
			if first, _ := args[0].(string); first == "relay" {
				return true
			}
		}
	}
	return false
}

// IsMcphubBinary reports whether cmd's basename matches our CLI
// binary name. Accepts the current names (mcphub / mcphub.exe) AND
// the legacy names (mcp / mcp.exe) that early installations may
// still have persisted into Antigravity client configs — matches
// the existing isOurRelayBinary classifier at internal/api/scan.go:99
// and the cleanup allowlist at internal/api/cleanup.go:38. Without
// the legacy names, the Antigravity relay-reject in
// ExtractManifestFromClient (internal/api/scan.go) would silently
// accept legacy mcp.exe relay entries as "user stdio" and happily
// draft a manifest pointing at the legacy binary. The adapter
// RestoreEntryFromBackup hub-relay detection uses the same helper,
// so one widening covers both call sites. Case-insensitive basename
// match. Exported so internal/api/scan.go (package api) can call it
// via clients.IsMcphubBinary; within package clients it is called
// as the unqualified IsMcphubBinary.
func IsMcphubBinary(cmd string) bool {
	if cmd == "" {
		return false
	}
	// Normalize Windows separators FIRST so a command written on Windows
	// (e.g. "C:\bin\mcphub.exe") is basenamed correctly when this check
	// runs on Linux/macOS — filepath.Base does NOT split backslashes on
	// POSIX, so a backup created on Windows but inspected on Linux CI (or a
	// migrated home) would otherwise mis-classify the relay binary. On
	// Windows filepath.Base already handles both separators, so the
	// replace is a harmless no-op there. (bot PR #257 P1)
	normalized := strings.ReplaceAll(cmd, `\`, "/")
	base := strings.ToLower(filepath.Base(normalized))
	return base == "mcphub" || base == "mcphub.exe" ||
		base == "mcp" || base == "mcp.exe"
}

// SupportedClientNames returns every client id understood by this build.
// The order is stable for CLI help, docs, and tests.
func SupportedClientNames() []string {
	return []string{
		"claude-code",
		"codex-cli",
		"cursor",
		"vscode",
		"gemini-cli",
		"qwen-cli",
		"antigravity",
	}
}

// DefaultInstallClientNames returns the clients touched by install when the
// user does not request a narrower or wider target set. Heavy/experimental
// clients remain opt-in so a fresh install does not silently mutate every
// assistant on the workstation.
func DefaultInstallClientNames() []string {
	return []string{"claude-code", "codex-cli", "cursor"}
}

// ConfigPathForName returns the default config path for a supported client.
func ConfigPathForName(name string) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	switch name {
	case "claude-code":
		return filepath.Join(home, ".claude.json"), nil
	case "codex-cli":
		return filepath.Join(home, ".codex", "config.toml"), nil
	case "cursor":
		return filepath.Join(home, ".cursor", "mcp.json"), nil
	case "vscode":
		return defaultVSCodeConfigPath(home), nil
	case "gemini-cli":
		return filepath.Join(home, ".gemini", "settings.json"), nil
	case "qwen-cli":
		return filepath.Join(home, ".qwen", "settings.json"), nil
	case "antigravity":
		return filepath.Join(home, ".gemini", "antigravity", "mcp_config.json"), nil
	default:
		return "", fmt.Errorf("unknown client %q (expected %s)", name, strings.Join(SupportedClientNames(), " | "))
	}
}

func defaultVSCodeConfigPath(home string) string {
	switch runtime.GOOS {
	case "windows":
		if appData := os.Getenv("APPDATA"); appData != "" {
			return filepath.Join(appData, "Code", "User", "mcp.json")
		}
		return filepath.Join(home, "AppData", "Roaming", "Code", "User", "mcp.json")
	case "darwin":
		return filepath.Join(home, "Library", "Application Support", "Code", "User", "mcp.json")
	default:
		if xdg := os.Getenv("XDG_CONFIG_HOME"); xdg != "" {
			return filepath.Join(xdg, "Code", "User", "mcp.json")
		}
		return filepath.Join(home, ".config", "Code", "User", "mcp.json")
	}
}

// AllClients returns the map of {client-name -> Client} for every supported
// adapter. Factories that return an error (e.g. UserHomeDir failure) are
// silently skipped, so callers that iterate the map see only adapters that
// could be constructed on the current host. This is the shared accessor
// used by both internal/api and internal/cli.
func AllClients() map[string]Client {
	result := map[string]Client{}
	for _, factory := range []func() (Client, error){
		NewClaudeCode, NewCodexCLI, NewCursor, NewVSCode, NewGeminiCLI, NewQwenCLI, NewAntigravity,
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

// BackupsNewestFirst returns every mcp-local-hub backup path for
// livePath, sorted newest-first. Timestamped copies
// (livePath + ".bak-mcp-local-hub-<ts>") come first in
// lexicographic-reverse order (timestamps use the 20060102-150405
// layout, which sorts correctly as a string), then the pristine
// "-original" sentinel (semantically the oldest snapshot — taken
// on first Backup() call before any timestamped backup).
// Directories with matching names are ignored.
//
// Used by the demigrate flow to iterate through every backup
// candidate when the latest backup is in hub-managed form but
// older backups may contain the pre-hub form of the entry —
// closing the gap documented in
// work-items/bugs/2026-05-15-demigrate-fallback-when-no-pre-hub-form.md
// §"Quality: Iterate timestamped backups newest-first".
//
// Returns an empty slice (not nil) when no backups exist. Returns
// an error only on filesystem read errors.
func BackupsNewestFirst(livePath, clientName string) ([]string, error) {
	dir := filepath.Dir(livePath)
	prefix := filepath.Base(livePath) + ".bak-mcp-local-hub-"
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return []string{}, nil
		}
		return nil, err
	}
	var timestamped []string
	var sentinel string
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
			sentinel = filepath.Join(dir, name)
			continue
		}
		timestamped = append(timestamped, filepath.Join(dir, name))
	}
	sort.Sort(sort.Reverse(sort.StringSlice(timestamped)))
	out := make([]string, 0, len(timestamped)+1)
	out = append(out, timestamped...)
	if sentinel != "" {
		out = append(out, sentinel)
	}
	return out, nil
}

// legacyBackupTimeLayouts are the best-effort time layouts used to order
// legacy-codename backups newest-first. A filename that matches none of
// these still participates (with zero time, sorting last within its
// bucket) — ordering is a heuristic, presence is not.
var legacyBackupTimeLayouts = []string{
	"20060102-150405",     // plain bak-YYYYMMDD-HHMMSS
	"2006-01-02_15-04-05", // older underscore-date format
	"2006-01-02",          // date-only (e.g. bak-2026-04-15-mcp-sync)
}

// legacyBackupPrefixOrder lists the legacy-codename backup buckets in the
// priority order Demigrate consults them, per
// work-items/bugs/2026-05-15-demigrate-fallback-when-no-pre-hub-form.md
// §"Quality: Legacy-codename prefix fallback". Lower index = consulted
// first. Each entry is a bucket classifier over the filename suffix that
// follows "<base>.bak-".
//
//	mcp-sync : pre-rename codename. The real on-disk artifact observed in
//	           the bug (e.g. settings.json.bak-2026-04-15-mcp-sync) carries
//	           "mcp-sync" as a SUFFIX after a date, not the idealized
//	           "mcp-sync-<ts>" prefix — so the classifier matches any
//	           non-mcp-local-hub backup whose suffix contains "mcp-sync".
//	plain    : bak-YYYYMMDD-HHMMSS with no codename token at all.
//	phase2   : bak-phase2-* install artifact.
//	dashdate : bak-YYYY-MM-DD_HH-MM-SS older underscore-date format.
const (
	legacyBucketMcpSync = iota
	legacyBucketPlain
	legacyBucketPhase2
	legacyBucketDashDate
	legacyBucketCount
)

// classifyLegacyBackupSuffix maps a backup filename suffix (the part
// after "<base>.bak-") to one of the legacy buckets, or returns ok=false
// when the suffix does not look like any known legacy-codename backup.
// The mcp-local-hub-prefixed suffixes are filtered out by the caller
// before this is reached, so they never classify here.
func classifyLegacyBackupSuffix(suffix string) (bucket int, ok bool) {
	switch {
	case strings.Contains(suffix, "mcp-sync"):
		return legacyBucketMcpSync, true
	case strings.HasPrefix(suffix, "phase2"):
		return legacyBucketPhase2, true
	case matchesPlainTimestampSuffix(suffix):
		return legacyBucketPlain, true
	case matchesUnderscoreDateSuffix(suffix):
		return legacyBucketDashDate, true
	default:
		return 0, false
	}
}

// matchesPlainTimestampSuffix reports whether suffix is exactly the
// bak-YYYYMMDD-HHMMSS shape (8 digits, dash, 6 digits) with no trailing
// codename token.
func matchesPlainTimestampSuffix(suffix string) bool {
	_, err := time.Parse("20060102-150405", suffix)
	return err == nil
}

// matchesUnderscoreDateSuffix reports whether suffix is exactly the
// older bak-YYYY-MM-DD_HH-MM-SS shape.
func matchesUnderscoreDateSuffix(suffix string) bool {
	_, err := time.Parse("2006-01-02_15-04-05", suffix)
	return err == nil
}

// parseLegacyBackupTime extracts a best-effort time from a legacy backup
// filename suffix for newest-first ordering. It tries the whole suffix
// against each known layout, then falls back to scanning for an embedded
// date/timestamp token (covers suffixes like "2026-04-15-mcp-sync" where
// the codename trails the date). Returns the zero time when nothing
// parses — such files still sort, just last within their bucket.
func parseLegacyBackupTime(suffix string) time.Time {
	for _, layout := range legacyBackupTimeLayouts {
		if ts, err := time.Parse(layout, suffix); err == nil {
			return ts
		}
	}
	// Embedded-token fallback: try a leading YYYYMMDD-HHMMSS or
	// YYYY-MM-DD prefix of the suffix (the common "date then codename"
	// real-world shape).
	for _, n := range []int{len("20060102-150405"), len("2006-01-02_15-04-05"), len("2006-01-02")} {
		if len(suffix) < n {
			continue
		}
		head := suffix[:n]
		for _, layout := range legacyBackupTimeLayouts {
			if ts, err := time.Parse(layout, head); err == nil {
				return ts
			}
		}
	}
	return time.Time{}
}

// LegacyBackupsNewestFirst returns every legacy-codename backup of
// livePath — backups written under prefixes that predate the current
// ".bak-mcp-local-hub-<ts>" naming (the April-2026 mcp-sync→mcp-local-hub
// codename rename and earlier install artifacts). Results are grouped by
// bucket priority (mcp-sync, plain-timestamp, phase2, underscore-date)
// and ordered newest-first within each bucket by a best-effort timestamp
// parsed from the filename.
//
// The current "-mcp-local-hub-" backups and the "-original" sentinel are
// NEVER included — those are handled by BackupsNewestFirst. Returns an
// empty slice (not nil) when no legacy backups exist; an error only on a
// filesystem read failure. The clientName parameter is reserved for
// future per-client diagnostic context (unused today, mirroring
// BackupsNewestFirst).
//
// Demigrate consults this AFTER exhausting every mcp-local-hub backup
// without finding a pre-hub form, and BEFORE the RemoveEntry last resort,
// so a user who upgraded across the codename rename recovers their true
// pre-hub state instead of having the entry deleted — see
// work-items/bugs/2026-05-15-demigrate-fallback-when-no-pre-hub-form.md.
func LegacyBackupsNewestFirst(livePath, _ string) ([]string, error) {
	dir := filepath.Dir(livePath)
	base := filepath.Base(livePath)
	bakPrefix := base + ".bak-"
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return []string{}, nil
		}
		return nil, err
	}
	type legacyBak struct {
		path   string
		bucket int
		ts     time.Time
	}
	var found []legacyBak
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasPrefix(name, bakPrefix) {
			continue
		}
		suffix := strings.TrimPrefix(name, bakPrefix)
		// Exclude every current-codename backup (timestamped + sentinel).
		// "mcp-local-hub-..." suffixes belong to BackupsNewestFirst.
		if strings.HasPrefix(suffix, "mcp-local-hub-") {
			continue
		}
		bucket, ok := classifyLegacyBackupSuffix(suffix)
		if !ok {
			continue
		}
		found = append(found, legacyBak{
			path:   filepath.Join(dir, name),
			bucket: bucket,
			ts:     parseLegacyBackupTime(suffix),
		})
	}
	sort.SliceStable(found, func(i, j int) bool {
		if found[i].bucket != found[j].bucket {
			return found[i].bucket < found[j].bucket
		}
		// Newest first within a bucket. Equal/zero timestamps fall back
		// to reverse-lexicographic path for deterministic ordering.
		if !found[i].ts.Equal(found[j].ts) {
			return found[i].ts.After(found[j].ts)
		}
		return found[i].path > found[j].path
	})
	out := make([]string, 0, len(found))
	for _, b := range found {
		out = append(out, b.path)
	}
	return out, nil
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
//
// Kept for callers that only need the single newest path; new code
// iterating through every candidate should use BackupsNewestFirst
// instead (see demigrate.go).
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

// extractHeaders pulls a string-keyed string map out of a parsed JSON/TOML
// node at the given field name. Returns nil when the field is missing,
// the wrong type, or contains no string values. Used by adapter GetEntry
// implementations so install rollback's priorEntry snapshot round-trips
// Headers losslessly.
func extractHeaders(raw map[string]any, field string) map[string]string {
	rawMap, ok := raw[field].(map[string]any)
	if !ok {
		return nil
	}
	hdrs := make(map[string]string, len(rawMap))
	for k, v := range rawMap {
		if s, ok := v.(string); ok {
			hdrs[k] = s
		}
	}
	if len(hdrs) == 0 {
		return nil
	}
	return hdrs
}
