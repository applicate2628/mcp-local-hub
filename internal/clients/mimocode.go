package clients

import (
	"fmt"
	"os"
	"path/filepath"
)

// NewMimoCode returns a Client bound to MiMoCode's global config file.
//
// MiMoCode (Xiaomi MiMo Code, github.com/XiaomiMiMo/MiMo-Code) is built as a
// FORK of OpenCode and keeps OpenCode's MCP config: MCP server definitions
// live in the top-level `mcp` object of its JSON config, with the IDENTICAL
// local/remote entry shapes. Where this adapter diverges from the OpenCode
// adapter it is to stay FAITHFUL to MiMoCode's own config resolution
// (packages/opencode/src/config/paths.ts +
// packages/shared/src/global.ts), NOT to OpenCode's simpler model:
//
//   - MiMoCode honors a richer env-override chain for the global config
//     location (MIMOCODE_CONFIG / MIMOCODE_CONFIG_DIR / MIMOCODE_HOME /
//     XDG_CONFIG_HOME) — see resolveMimoCodeConfigPath.
//   - MiMoCode loads `mimocode.json` AND `mimocode.jsonc` from the resolved
//     dir as deep-merged layers (.jsonc wins per key) — see
//     mimoCodeLayerFiles / readJSON / deleteMember.
//   - MiMoCode local entries store `command` as an ARRAY (and env under
//     `environment`) — handled by the scanner + extractor, and the reason
//     GetEntry returns nil for a URL-less local entry.
//
// Two config scopes exist (same as OpenCode):
//
//   - Global: the resolved dir's `mimocode.json` / `mimocode.jsonc` (see
//     resolveMimoCodeConfigDir). On every OS MiMoCode resolves the default
//     global config from ~/.config/mimocode/ — like OpenCode it does NOT
//     follow the Windows %APPDATA% / macOS ~/Library convention (paths.ts
//     uses the xdg-basedir config dir joined with the "mimocode" app name).
//   - Project: a per-repo config in the repository root and per-project
//     `.mimocode` directories (highest precedence, merged over global at
//     load time).
//
// The hub writes the GLOBAL file so a single per-user hub entry is visible in
// every project, matching every other adapter's user-scoped posture. The
// project-root / cross-DIR `.mimocode` overlay (project + home + CONFIG_DIR)
// is a documented LIMITATION shared with the OpenCode adapter — this adapter
// targets the resolved global dir only. The in-dir TWO-FILE merge
// (mimocode.json + mimocode.jsonc in the resolved dir) IS implemented; the
// cross-DIR overlay is not.
//
// Transport choice — HTTP-direct (NOT relay-stdio). Like OpenCode, MiMoCode
// supports remote MCP servers natively over Streamable HTTP. A remote entry
// is keyed by server name under `mcp` and discriminated by `"type":"remote"`
// with a `url` endpoint and an `enabled` flag:
//
//	{
//	  "mcp": {
//	    "<server-name>": {
//	      "type": "remote",
//	      "url": "http://localhost:9121/mcp",
//	      "enabled": true
//	    }
//	  }
//	}
//
// (Local stdio servers use "type":"local" with a `command` ARRAY plus an
// optional `environment` object instead; the hub never WRITES that shape
// because the daemon is already an HTTP endpoint, but the scanner/extractor
// READ it faithfully.) Optional `headers` is emitted when MCPEntry.Headers
// is non-empty.
//
// Sources (verified 2026-06 against the live source):
//   - paths.ts (packages/opencode/src/config/paths.ts) — `files()` loads ONLY
//     `${name}.jsonc` / `${name}.json` (NO config.json); `directories()`
//     prepends the global config dir, then project + home `.mimocode`, then
//     the MIMOCODE_CONFIG_DIR overlay.
//   - global.ts (packages/shared/src/global.ts) — resolveMimocodeHome:
//     MIMOCODE_HOME (absolute, → $HOME/config) else the xdg-basedir config
//     dir joined with APP="mimocode" (→ XDG_CONFIG_HOME/mimocode or
//     ~/.config/mimocode). A relative MIMOCODE_HOME is a hard error there;
//     this adapter conservatively IGNORES relative env values.
//   - config.ts (packages/opencode/src/config/config.ts) — Flag.MIMOCODE_CONFIG
//     is a custom config FILE loaded verbatim; Flag.MIMOCODE_CONFIG_DIR is a
//     custom config DIR loading mimocode.json / mimocode.jsonc.
//
// IMPORTANT divergences from the JSON family (mcpServers + disabled:false),
// inherited from OpenCode:
//   - top-level key is `mcp`, NOT `mcpServers` — so this is a standalone
//     struct (like vscode/zed/opencode), not an embedding of jsonMCPClient.
//   - the active flag is `enabled` (true = on), NOT `disabled` (false =
//     on). The hub writes `enabled:true`.
//   - remote entries carry `"type":"remote"`.
//
// The hub-shape guard (isHubURLShapeEntry) keys off the `url` field and the
// absence of a `command` key, which holds for MiMoCode's remote entry shape,
// so demigrate/rollback recognize a hub-managed MiMoCode entry.
func NewMimoCode() (Client, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	return newLockingClient(&mimoCodeClient{path: defaultMimoCodeConfigPath(home)}), nil
}

// mimoCodeConfigFileNames are the ONLY config files MiMoCode loads from a
// resolved config directory, lowest precedence first. This matches paths.ts
// `files()` (`targets: ['${name}.jsonc', '${name}.json']`) and the in-dir
// loader in config.ts (`for (const file of ["mimocode.json",
// "mimocode.jsonc"])`): `.json` is the base layer and `.jsonc` deep-merges
// over it (wins per key). There is deliberately NO `config.json` here — the
// real source loads only `${name}.{json,jsonc}` in a config DIR (the lone
// `config.json` reference in config.ts is a one-shot migration of a legacy
// TOML `config` file into the GLOBAL dir only, not a documented config
// surface the `mimo` CLI loads on a clean install — confirmed by the codex
// bot's paths.ts citation across PR #420 rounds).
var mimoCodeConfigFileNames = []string{"mimocode.json", "mimocode.jsonc"}

// defaultMimoCodeConfigPath returns the WRITE target path: the highest-layer
// existing file in the resolved config dir, or `mimocode.json` when neither
// layer exists yet. Writes always target the top layer (.jsonc when present),
// matching the OpenCode adapter's "prefer an existing .jsonc" rule and the
// "writes still target the top layer" guidance from the bot review.
//
// home is the OS home dir (os.UserHomeDir at the call site); env overrides
// (MIMOCODE_CONFIG / MIMOCODE_CONFIG_DIR / MIMOCODE_HOME / XDG_CONFIG_HOME)
// take precedence over it inside resolveMimoCodeConfigDir.
func defaultMimoCodeConfigPath(home string) string {
	// MIMOCODE_CONFIG is an absolute config FILE used verbatim — it bypasses
	// directory probing entirely (config.ts loads Flag.MIMOCODE_CONFIG
	// directly).
	if f := absoluteEnv("MIMOCODE_CONFIG"); f != "" {
		return f
	}
	dir := resolveMimoCodeConfigDir(home)
	// Prefer an existing higher layer (.jsonc) so a hub entry is not written
	// into a separate lower-priority .json the client never reads; otherwise
	// target mimocode.json — the file the adapter seeds on a fresh host.
	if jsonc := filepath.Join(dir, "mimocode.jsonc"); isRegularFile(jsonc) {
		return jsonc
	}
	return filepath.Join(dir, "mimocode.json")
}

// resolveMimoCodeConfigDir resolves the global config DIRECTORY honoring
// MiMoCode's documented env precedence (highest first):
//
//	MIMOCODE_CONFIG_DIR (absolute DIR, used verbatim)
//	  > MIMOCODE_HOME    (absolute DIR → $MIMOCODE_HOME/config)
//	  > XDG_CONFIG_HOME  (absolute DIR → $XDG_CONFIG_HOME/mimocode)
//	  > ~/.config/mimocode
//
// (MIMOCODE_CONFIG — the absolute FILE override — is handled one level up in
// defaultMimoCodeConfigPath since it bypasses dir probing.) Relative env
// values are IGNORED: global.ts rejects a relative MIMOCODE_HOME outright and
// the XDG spec ignores a relative XDG_CONFIG_HOME, so an absolute-only gate is
// the faithful and the safe choice.
func resolveMimoCodeConfigDir(home string) string {
	if d := absoluteEnv("MIMOCODE_CONFIG_DIR"); d != "" {
		return d
	}
	if h := absoluteEnv("MIMOCODE_HOME"); h != "" {
		return filepath.Join(h, "config")
	}
	if xdg := absoluteEnv("XDG_CONFIG_HOME"); xdg != "" {
		return filepath.Join(xdg, "mimocode")
	}
	return filepath.Join(home, ".config", "mimocode")
}

// absoluteEnv returns the named env var's value only when it is set AND an
// absolute path; otherwise "". A relative value is treated as unset, matching
// MiMoCode's own absolute-only handling (global.ts throws on a relative
// MIMOCODE_HOME; XDG ignores a relative XDG_CONFIG_HOME).
func absoluteEnv(name string) string {
	v := os.Getenv(name)
	if v == "" || !filepath.IsAbs(v) {
		return ""
	}
	return v
}

// isRegularFile reports whether path is an existing regular file. A stat error
// other than not-exist (e.g. a permission failure) is treated as "absent" so
// resolution never fails closed on a transient probe error.
func isRegularFile(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && fi.Mode().IsRegular()
}

// mimoCodeClient is a standalone adapter (NOT an embedding of jsonMCPClient)
// because MiMoCode uses the top-level `mcp` key rather than the JSON family's
// `mcpServers`, AND a distinct entry shape (`type:"remote"` + `enabled:true`
// rather than `disabled:false`). It mirrors the OpenCode adapter's
// standalone-struct + HTTP-direct pattern with the same key/field set, plus
// MiMoCode-faithful env precedence and in-dir layer merge.
type mimoCodeClient struct {
	path string
}

// mimoCodeMCPKey is the single owner of MiMoCode's top-level MCP section
// name. Every method that reaches into the parsed config map uses it.
const mimoCodeMCPKey = "mcp"

func (o *mimoCodeClient) Name() string       { return "mimocode" }
func (o *mimoCodeClient) ConfigPath() string { return o.path }

// IsRelayStdio reports false: mimocode is a URL-native HTTP MCP client.
func (o *mimoCodeClient) IsRelayStdio() bool { return false }

// Exists treats MiMoCode as installed when EITHER any resolved layer file is
// present OR the config directory exists, mirroring the
// opencode/cursor/vscode/kiro "directory means installed" heuristic so an
// operator who has MiMoCode installed but no MCP config yet still gets the
// Initialize / install affordance.
func (o *mimoCodeClient) Exists() bool {
	for _, f := range o.layerFiles() {
		if _, err := os.Stat(f); err == nil {
			return true
		}
	}
	st, err := os.Stat(filepath.Dir(o.path))
	return err == nil && st.IsDir()
}

func (o *mimoCodeClient) Backup() (string, error) {
	return o.BackupKeep(0)
}

// BackupKeep ensures the nested config parent directory exists, seeds an empty
// `{"mcp": {}}` stub at the write target if the config is absent, then writes
// the timestamped backup (pruning to keepN). The parent dir does not exist on
// a clean install, so the MkdirAll here is load-bearing — without it
// writeBackup/InitEmpty would fail on a fresh host. Mirrors the
// opencode/cursor/vscode/kiro/windsurf BackupKeep wrappers.
func (o *mimoCodeClient) BackupKeep(keepN int) (string, error) {
	if dir := filepath.Dir(o.path); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return "", err
		}
	}
	if _, err := o.InitEmpty(); err != nil {
		return "", err
	}
	return writeBackup(o.path, o.Name(), keepN)
}

// InitEmpty seeds the write-target config file with `{"mcp": {}}` if the file
// is absent. AddEntry's later merge writes into the same `mcp` map. MiMoCode
// also accepts a top-level `$schema` field and many other keys, plus JSONC
// comments / trailing commas (it accepts a `.jsonc` variant and operators
// hand-edit it): the read path parses comments via the shared JSONC helper,
// and AddEntry/RemoveEntry patch through hujson so the operator's comments and
// every unknown top-level key already present in the file are PRESERVED on
// every write (only the `mcp` map is touched) — so seeding a minimal stub does
// not clobber a hand-authored config, and on a truly fresh host this minimal
// stub is all that is needed.
func (o *mimoCodeClient) InitEmpty() (created bool, err error) {
	return EnsureClientConfigStub(o.path, []byte("{\n  \"mcp\": {}\n}\n"))
}

func (o *mimoCodeClient) Restore(backupPath string) error {
	// Route the live-config rewrite through WriteConfigFile so production
	// restores inherit the SecureWriteClientConfig pipeline.
	data, err := os.ReadFile(backupPath)
	if err != nil {
		return err
	}
	return WriteConfigFile(o.path, data)
}

// layerFiles returns the config-file layers to READ/REMOVE for this adapter,
// lowest precedence first (mimocode.json, then mimocode.jsonc — .jsonc wins).
//
// State-safety + explicit-path honoring (bot PR #420 r4): the in-dir two-file
// merge is applied ONLY when o.path is one of the resolved layer file NAMES in
// its own directory — i.e. the default-resolved global location. For an
// EXPLICIT override path (a temp/test path, or MIMOCODE_CONFIG pointing at an
// arbitrary file) the basename is not a known layer name, so we fall back to
// `[]string{o.path}` and operate ONLY on the supplied file. This keeps a
// temp/test scan from ever reaching into the real ~/.config/mimocode, and
// never recomputes the directory from env/home — it stays inside o.path's own
// directory.
func (o *mimoCodeClient) layerFiles() []string {
	return mimoCodeLayerFiles(o.path)
}

// mimoCodeLayerFiles is the pure resolver behind (*mimoCodeClient).layerFiles.
// Given a config path, when its basename is a known layer file name it returns
// every layer sibling in the SAME directory (so an entry in either
// mimocode.json or mimocode.jsonc is visible / removable); otherwise it
// returns just the supplied path verbatim (explicit override). It never reads
// or recomputes a directory other than dir(path).
func mimoCodeLayerFiles(path string) []string {
	base := filepath.Base(path)
	known := false
	for _, n := range mimoCodeConfigFileNames {
		if base == n {
			known = true
			break
		}
	}
	if !known {
		// Explicit override (e.g. MIMOCODE_CONFIG or a temp test path): operate
		// only on the supplied file; do NOT recompute the dir or pull siblings.
		return []string{path}
	}
	dir := filepath.Dir(path)
	files := make([]string, 0, len(mimoCodeConfigFileNames))
	for _, n := range mimoCodeConfigFileNames {
		files = append(files, filepath.Join(dir, n))
	}
	return files
}

// readMergedLayers reads every layer file and DEEP-MERGES them lowest-first
// (mimocode.json base, mimocode.jsonc overrides per key) so an entry present
// in either layer is visible. Missing layers are skipped (treated as empty);
// a parse error on a present layer propagates. JSONC (comments + trailing
// commas) is tolerated via the shared parseJSONCBytes helper.
func (o *mimoCodeClient) readMergedLayers() (map[string]any, error) {
	merged := map[string]any{}
	for _, f := range o.layerFiles() {
		data, err := readRawConfig(f)
		if err != nil {
			return nil, err
		}
		if len(data) == 0 {
			continue
		}
		m, err := parseJSONCBytes(data)
		if err != nil {
			return nil, fmt.Errorf("parse %s: %w", f, err)
		}
		merged = mimoCodeDeepMerge(merged, m)
	}
	return merged, nil
}

// mimoCodeDeepMerge recursively merges src over dst (src wins per key) and
// returns dst. Nested objects are merged key-by-key; every other value
// (including arrays and scalars) is replaced wholesale by src's. This mirrors
// MiMoCode's `mergeDeep` layer semantics so a server defined in the lower
// layer survives when the higher layer only carries unrelated settings, while
// a same-named server in the higher layer wins. Kept LOCAL to the mimocode
// adapter (no shared-type change).
func mimoCodeDeepMerge(dst, src map[string]any) map[string]any {
	for k, sv := range src {
		if svMap, ok := sv.(map[string]any); ok {
			if dvMap, ok := dst[k].(map[string]any); ok {
				dst[k] = mimoCodeDeepMerge(dvMap, svMap)
				continue
			}
		}
		dst[k] = sv
	}
	return dst
}

// readJSON returns the deep-merged view across the resolved layer files. Used
// by GetEntry / AllStdioEntries / FindStdioLanguageServerEntries.
func (o *mimoCodeClient) readJSON() (map[string]any, error) {
	return o.readMergedLayers()
}

// MimoCodeMergedConfig is the SCAN-side entry point into the adapter's in-dir
// layer merge: given a config path it returns the deep-merged top-level map
// across the resolved layer files (mimocode.json + mimocode.jsonc in the
// path's own directory, .jsonc winning per key) or just the single supplied
// file for an explicit override path. Exported so internal/api/scan.go reuses
// the EXACT same layer resolution + JSONC-tolerant decode as the adapter,
// keeping the merge a single owner (no parallel reimplementation in scan.go)
// and keeping the shared clients types untouched. Returns an empty (non-nil)
// map when no layer file exists; a parse error on a present file propagates.
func MimoCodeMergedConfig(path string) (map[string]any, error) {
	return (&mimoCodeClient{path: path}).readMergedLayers()
}

// setMember sets mcp.<name> = value on the WRITE-target file (o.path),
// preserving the operator's comments + unrelated top-level keys when the file
// already has JSONC content. Writes always target the top layer; the bytes
// route through the UNCHANGED WriteConfigFile pipeline.
func (o *mimoCodeClient) setMember(name string, value any) error {
	return mutateJSONObjectMember(o.path, mimoCodeMCPKey, name, value, false)
}

// deleteMember removes mcp.<name> from EVERY resolved layer file (not just the
// write target). A lower-layer entry would otherwise remain active and
// reappear on the next scan after an uninstall/demigrate/unchecked-Apply that
// only touched the top layer (bot PR #420 r4). Delete-of-absent is a no-op per
// layer (mutateJSONObjectMember returns nil on an empty/absent file), so this
// is idempotent. The first per-layer error is returned; earlier successful
// deletes are not rolled back (a partial delete still strictly REDUCES the
// active surface, never expands it).
func (o *mimoCodeClient) deleteMember(name string) error {
	for _, f := range o.layerFiles() {
		if err := mutateJSONObjectMember(f, mimoCodeMCPKey, name, nil, true); err != nil {
			return err
		}
	}
	return nil
}

// AddEntry writes the hub-managed remote-HTTP entry under mcp.<name>.
// MiMoCode's remote entry shape is `{"type":"remote","url":...,
// "enabled":true}`; an optional `headers` object is emitted when
// MCPEntry.Headers is non-empty.
func (o *mimoCodeClient) AddEntry(entry MCPEntry) error {
	serverEntry := map[string]any{
		"type":    "remote",
		"url":     entry.URL,
		"enabled": true,
	}
	if len(entry.Headers) > 0 {
		serverEntry["headers"] = entry.Headers
	}
	// Comment-preserving set: patches mcp.<name> into the original on-disk
	// bytes via hujson so the operator's comments and unrelated keys survive (a
	// full map re-marshal would drop both).
	return o.setMember(entry.Name, serverEntry)
}

func (o *mimoCodeClient) RemoveEntry(name string) error {
	// Comment-preserving delete across every layer; absence is a no-op.
	return o.deleteMember(name)
}

// GetEntry reads mcp.<name> from the merged layers and projects it onto the
// lean MCPEntry (URL + Headers).
//
// A LOCAL entry (type:"local", a `command` array, NO `url`) CANNOT be
// represented by MCPEntry — it has no URL and MCPEntry carries no command. The
// install/register rollback paths snapshot GetEntry before AddEntry and
// restore the snapshot with AddEntry(*prior) on a downstream failure; if we
// returned a non-nil URL-less MCPEntry, that rollback would REWRITE the user's
// local command-array entry as a broken remote entry with `url:""`. Returning
// nil (as for an absent entry) makes the rollback safely SKIP the local entry
// instead of corrupting it (bot PR #420 r4/r5). A missing entry also returns
// (nil, nil).
func (o *mimoCodeClient) GetEntry(name string) (*MCPEntry, error) {
	m, err := o.readJSON()
	if err != nil {
		return nil, err
	}
	servers, _ := m[mimoCodeMCPKey].(map[string]any)
	if servers == nil {
		return nil, nil
	}
	raw, ok := servers[name].(map[string]any)
	if !ok {
		return nil, nil
	}
	url, _ := raw["url"].(string)
	if url == "" {
		// URL-less local entry (or a malformed remote) — not representable as a
		// URL MCPEntry; return nil so rollback snapshot/restore skips it rather
		// than corrupting it into {type:remote, url:""}.
		return nil, nil
	}
	return &MCPEntry{Name: name, URL: url, Headers: extractHeaders(raw, "headers")}, nil
}

func (o *mimoCodeClient) LatestBackupPath() (string, bool, error) {
	return latestBackup(o.path, o.Name())
}

func (o *mimoCodeClient) RestoreEntryFromBackup(backupPath, name string) error {
	return o.restoreEntryFromBackup(backupPath, name, false)
}

// RestoreEntryFromBackupForRollback restores the backup's entry verbatim,
// bypassing the ErrBackupEntryAlreadyMigrated guard (see the interface doc
// on Client.RestoreEntryFromBackupForRollback). Used only by the serena
// dynamic-pool migrate abort-rollback.
func (o *mimoCodeClient) RestoreEntryFromBackupForRollback(backupPath, name string) error {
	return o.restoreEntryFromBackup(backupPath, name, true)
}

// restoreEntryFromBackup is the shared body. When allowHubEntry is false
// (demigrate) it refuses a backup entry already in hub-HTTP shape (a hub
// loopback URL under `url` with no `command`) with
// ErrBackupEntryAlreadyMigrated; when true (migrate rollback) it writes the
// backup bytes verbatim regardless of shape.
func (o *mimoCodeClient) restoreEntryFromBackup(backupPath, name string, allowHubEntry bool) error {
	// os.ReadFile (NOT readRawConfig): a named backup that is missing is a
	// genuine read error the demigrate caller must see, not a silent
	// treat-as-empty. Empty / comment-only / malformed bytes are then
	// classified by parseJSONCBytes (empty map vs parse error).
	backupData, err := os.ReadFile(backupPath)
	if err != nil {
		return fmt.Errorf("read backup %s: %w", backupPath, err)
	}
	backupMap, err := parseJSONCBytes(backupData)
	if err != nil {
		return fmt.Errorf("parse backup %s: %w", backupPath, err)
	}
	backupServers, _ := backupMap[mimoCodeMCPKey].(map[string]any)
	if backupServers != nil {
		if backupEntry, present := backupServers[name]; present {
			// Defensive guard (demigrate flow only — the rollback caller
			// passes allowHubEntry=true to restore the pre-reconcile legacy
			// hub entry verbatim).
			if !allowHubEntry {
				if rawMap, ok := backupEntry.(map[string]any); ok {
					if isHubURLShapeEntry(rawMap, "url") {
						return ErrBackupEntryAlreadyMigrated
					}
				}
			}
			// Comment-preserving set into the LIVE config (its comments +
			// unrelated keys survive; the backup's entry VALUE is written).
			return o.setMember(name, backupEntry)
		}
	}
	return o.deleteMember(name)
}

// AllStdioEntries returns every stdio entry from MiMoCode's merged top-level
// `mcp` key. The hub writes only HTTP-direct ("type":"remote") entries, which
// have no string `command` field and so are correctly skipped by
// collectStdioEntries. Operator-authored local ("type":"local") entries store
// `command` as an ARRAY (["npx","-y",...]) rather than a string;
// collectStdioEntries reads `command` as a string and therefore does not
// surface them — an accepted limitation of the shared cross-format cleanup
// scan (these helpers are best-effort stdio-leak detection, and the hub never
// writes the local shape). The Discovery/extract path DOES parse the array
// (see scanMimoCode + ExtractManifestFromClient).
func (o *mimoCodeClient) AllStdioEntries() ([]StdioEntry, error) {
	m, err := o.readJSON()
	if err != nil {
		return nil, err
	}
	servers, _ := m[mimoCodeMCPKey].(map[string]any)
	return collectStdioEntries(servers), nil
}

// FindStdioLanguageServerEntries scans the merged `mcp` for stdio entries
// matching the mcp-language-server invocation pattern. As with
// AllStdioEntries, MiMoCode local entries use a `command` ARRAY which the
// shared string-keyed matcher does not recognize; the hub-written HTTP entries
// never match either way.
func (o *mimoCodeClient) FindStdioLanguageServerEntries() ([]LanguageServerStdioEntry, error) {
	m, err := o.readJSON()
	if err != nil {
		return nil, err
	}
	servers, _ := m[mimoCodeMCPKey].(map[string]any)
	return findLanguageServerStdioInMap(servers), nil
}

func (o *mimoCodeClient) BackupContainsEntry(backupPath, name string) (bool, error) {
	// os.ReadFile (NOT readRawConfig): a missing named backup is a read error,
	// not a silent (false, nil); empty bytes parse to an empty map below.
	data, err := os.ReadFile(backupPath)
	if err != nil {
		return false, fmt.Errorf("read backup %s: %w", backupPath, err)
	}
	m, err := parseJSONCBytes(data)
	if err != nil {
		return false, fmt.Errorf("parse backup %s: %w", backupPath, err)
	}
	servers, _ := m[mimoCodeMCPKey].(map[string]any)
	if servers == nil {
		return false, nil
	}
	entry, ok := servers[name].(map[string]any)
	return ok && entry != nil, nil
}

// BackupEntryIsHubManaged reports whether mcp[name] in the backup at
// backupPath is in MiMoCode's hub-managed shape (a hub loopback `url` with no
// `command`). See Client.BackupEntryIsHubManaged.
func (o *mimoCodeClient) BackupEntryIsHubManaged(backupPath, name string) (bool, error) {
	// os.ReadFile (NOT readRawConfig): a missing named backup is a read error
	// the demigrate caller must see, not a silent (false, nil); empty bytes
	// parse to an empty map below.
	data, err := os.ReadFile(backupPath)
	if err != nil {
		return false, fmt.Errorf("read backup %s: %w", backupPath, err)
	}
	m, err := parseJSONCBytes(data)
	if err != nil {
		return false, fmt.Errorf("parse backup %s: %w", backupPath, err)
	}
	servers, _ := m[mimoCodeMCPKey].(map[string]any)
	if servers == nil {
		return false, nil
	}
	entry, ok := servers[name].(map[string]any)
	if !ok || entry == nil {
		return false, nil
	}
	return isHubURLShapeEntry(entry, "url"), nil
}
