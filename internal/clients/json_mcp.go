package clients

import (
	"fmt"
	"os"
)

// jsonMCPClient is a reusable struct that handles JSON-format MCP configs
// with the `mcpServers.<name>.httpUrl` schema shared by Gemini CLI and Antigravity.
// clientName and urlField distinguish the two (field name is "httpUrl" for both,
// kept parameterized in case a future client uses a different field).
type jsonMCPClient struct {
	path       string
	clientName string
	urlField   string // "httpUrl" for both known cases
	// serversKey is the top-level JSON object key the entries live under.
	// Empty defaults to "mcpServers" (the JSON-family schema shared by
	// gemini-cli / antigravity / cursor / qwen and the relay clients), so every
	// existing embedder — which leaves it empty — keeps identical behavior. A
	// non-empty value lets an embedder target a different FLAT top-level key,
	// including a VS-Code-style dotted key like "amp.mcpServers": the
	// comment-preserving write path (jsonc.go) builds an RFC-6901 JSON Pointer
	// where the dot is one literal key character (only `/` separates pointer
	// tokens), so the entry lands under the flat dotted key exactly as VS Code
	// settings.json requires — no re-implementation of the Client interface.
	serversKey string
}

func (j *jsonMCPClient) Name() string       { return j.clientName }
func (j *jsonMCPClient) ConfigPath() string { return j.path }

// sectionKey returns the top-level JSON object key the entries live under,
// defaulting to "mcpServers" when serversKey is unset. Single owner of the
// default so every read/write seam below resolves the same key.
func (j *jsonMCPClient) sectionKey() string {
	if j.serversKey == "" {
		return "mcpServers"
	}
	return j.serversKey
}

// IsRelayStdio reports false: the JSON-family adapters (and every adapter
// that embeds jsonMCPClient without overriding this) are URL-native HTTP
// clients. The relay-stdio adapters declare true on their own concrete
// struct (Antigravity overrides this promoted method; Zed is standalone).
func (j *jsonMCPClient) IsRelayStdio() bool { return false }

func (j *jsonMCPClient) Exists() bool {
	_, err := os.Stat(j.path)
	return err == nil
}

func (j *jsonMCPClient) Backup() (string, error) {
	return writeBackup(j.path, j.clientName, 0)
}

func (j *jsonMCPClient) BackupKeep(keepN int) (string, error) {
	return writeBackup(j.path, j.clientName, keepN)
}

// InitEmpty seeds the JSON family config file with `{"mcpServers": {}}`
// if it is absent. Inherited by gemini-cli (~/.gemini/settings.json)
// and the Antigravity adapter (~/.gemini/antigravity/mcp_config.json).
// Both write subsequent entries into the same top-level `mcpServers`
// map that AddEntry merges into.
func (j *jsonMCPClient) InitEmpty() (created bool, err error) {
	stub := fmt.Sprintf("{\n  %q: {}\n}\n", j.sectionKey())
	return EnsureClientConfigStub(j.path, []byte(stub))
}

func (j *jsonMCPClient) Restore(backupPath string) error {
	// Route the live-config rewrite through WriteConfigFile so
	// production restores inherit the SecureWriteClientConfig pipeline.
	data, err := os.ReadFile(backupPath)
	if err != nil {
		return err
	}
	return WriteConfigFile(j.path, data)
}

// readConfigBytes returns the raw on-disk config bytes (nil for an absent
// file). The single owner of the live-config read for the comment-preserving
// write seams (setMember / deleteMember), which need the ORIGINAL bytes to
// patch through hujson rather than a rebuilt map.
func (j *jsonMCPClient) readConfigBytes() ([]byte, error) {
	return readRawConfig(j.path)
}

func (j *jsonMCPClient) readJSON() (map[string]any, error) {
	data, err := j.readConfigBytes()
	if err != nil {
		return nil, err
	}
	m, err := parseJSONCBytes(data)
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", j.path, err)
	}
	return m, nil
}

// setMember sets mcpServers.<name> = value in the live config, preserving the
// operator's comments, unrelated keys, and formatting when the file already
// has JSONC content. An empty/absent file falls back to a clean indented
// marshal of a freshly-built `{ "mcpServers": { <name>: value } }` so a
// brand-new config is born readable rather than as a single packed line.
// Routes the resulting bytes through the UNCHANGED WriteConfigFile pipeline.
func (j *jsonMCPClient) setMember(name string, value any) error {
	return j.mutateMember(name, value, false)
}

func (j *jsonMCPClient) setMemberWithWriter(name string, value any, writer WriteConfigFileFunc) error {
	return j.mutateMemberWithWriter(name, value, false, writer)
}

// deleteMember removes mcpServers.<name> from the live config, preserving the
// operator's comments and unrelated keys. Absence is a no-op.
func (j *jsonMCPClient) deleteMember(name string) error {
	return j.mutateMember(name, nil, true)
}

func (j *jsonMCPClient) deleteMemberWithWriter(name string, writer WriteConfigFileFunc) error {
	return j.mutateMemberWithWriter(name, nil, true, writer)
}

func (j *jsonMCPClient) mutateMember(name string, value any, del bool) error {
	return j.mutateMemberWithWriter(name, value, del, nil)
}

func (j *jsonMCPClient) mutateMemberWithWriter(name string, value any, del bool, writer WriteConfigFileFunc) error {
	return mutateJSONObjectMemberWithWriter(j.path, j.sectionKey(), name, value, del, writer)
}

func (j *jsonMCPClient) AddEntry(entry MCPEntry) error {
	return j.AddEntryWithConfigWriter(entry, nil)
}

func (j *jsonMCPClient) AddEntryWithConfigWriter(entry MCPEntry, writer WriteConfigFileFunc) error {
	serverEntry := map[string]any{
		j.urlField: entry.URL,
		"disabled": false,
	}
	if len(entry.Headers) > 0 {
		serverEntry["headers"] = entry.Headers
	}
	// Comment-preserving set: patches mcpServers.<name> into the original
	// on-disk bytes via hujson so the operator's comments and unrelated keys
	// survive (a full map re-marshal would drop both).
	return j.setMemberWithWriter(entry.Name, serverEntry, writer)
}

func (j *jsonMCPClient) RemoveEntry(name string) error {
	// Comment-preserving delete: removes mcpServers.<name> from the original
	// bytes, leaving comments + unrelated keys intact. Absence is a no-op.
	return j.deleteMember(name)
}

func (j *jsonMCPClient) GetEntry(name string) (*MCPEntry, error) {
	m, err := j.readJSON()
	if err != nil {
		return nil, err
	}
	servers, _ := m[j.sectionKey()].(map[string]any)
	if servers == nil {
		return nil, nil
	}
	raw, ok := servers[name].(map[string]any)
	if !ok {
		return nil, nil
	}
	url, _ := raw[j.urlField].(string)
	return &MCPEntry{Name: name, URL: url, Headers: extractHeaders(raw, "headers"), Disabled: mcpEntryDisabled(raw)}, nil
}

// LatestBackupPath delegates to the shared helper.
func (j *jsonMCPClient) LatestBackupPath() (string, bool, error) {
	return latestBackup(j.path, j.clientName)
}

// RestoreEntryFromBackup reads the JSON backup, extracts mcpServers[name]
// (if present), and writes it (or removes the current live entry) to
// the live config. Other entries in mcpServers are untouched.
// Inherited by geminiCLI and antigravityClient via struct embedding.
//
// Defensively refuses if the backup's copy of the named entry is
// already in hub-managed shape. Shape detection is adapter-specific:
//   - For URL-native JSON clients (urlField = "url" or "httpUrl"): entry
//     has a hub loopback URL and no `command`.
//   - For Antigravity (urlField = "command"): entry's `command` is the
//     mcphub binary AND args[0] == "relay".
//
// Both paths return ErrBackupEntryAlreadyMigrated so Demigrate can
// surface a clear operator-facing failure row.
func (j *jsonMCPClient) RestoreEntryFromBackup(backupPath, name string) error {
	return j.restoreEntryFromBackup(backupPath, name, false)
}

// RestoreEntryFromBackupForRollback restores the backup's entry verbatim,
// bypassing the ErrBackupEntryAlreadyMigrated guard (see the interface
// doc on Client.RestoreEntryFromBackupForRollback). Install rollback and
// Serena migrate rollback use it when the timestamped backup is the source of
// truth. Inherited by geminiCLI, qwenCLI, cursorClient, and antigravityClient
// via struct embedding, so shared rollback behavior stays one owner.
func (j *jsonMCPClient) RestoreEntryFromBackupForRollback(backupPath, name string) error {
	return j.restoreEntryFromBackup(backupPath, name, true)
}

func (j *jsonMCPClient) RestoreEntryFromBackupForRollbackWithConfigWriter(backupPath, name string, writer WriteConfigFileFunc) error {
	return j.restoreEntryFromBackupWithWriter(backupPath, name, true, writer)
}

// restoreEntryFromBackup is the shared body. When allowHubEntry is false
// (demigrate) it refuses a backup entry already in hub-managed shape
// (hub-HTTP loopback URL for URL clients; mcphub `relay` invocation for
// Antigravity) with ErrBackupEntryAlreadyMigrated; when true (migrate
// rollback) it writes the backup bytes verbatim regardless of shape.
func (j *jsonMCPClient) restoreEntryFromBackup(backupPath, name string, allowHubEntry bool) error {
	return j.restoreEntryFromBackupWithWriter(backupPath, name, allowHubEntry, nil)
}

func (j *jsonMCPClient) restoreEntryFromBackupWithWriter(backupPath, name string, allowHubEntry bool, writer WriteConfigFileFunc) error {
	backupData, err := os.ReadFile(backupPath)
	if err != nil {
		return fmt.Errorf("read backup %s: %w", backupPath, err)
	}
	backupMap, err := parseJSONCBytes(backupData)
	if err != nil {
		return fmt.Errorf("parse backup %s: %w", backupPath, err)
	}
	backupServers, _ := backupMap[j.sectionKey()].(map[string]any)
	// Rollback-only atomic entry-scoped skip-if-unchanged (design round-4): read
	// the SINGLE write-target layer under the held withConfigLock and, if it
	// already holds the backup's entry value (unmutated client, or a sibling edit
	// left THIS entry untouched), return nil WITHOUT writing. A read/parse error
	// FALLS THROUGH to the existing restore (no new failure mode). Gated on
	// allowHubEntry so demigrate keeps its ErrBackupEntryAlreadyMigrated guard.
	if allowHubEntry {
		// Whole-file-gone recovery FIRST (design round-5): SecureWrite path #1 may have
		// REMOVED the write-target file (target entry + siblings). Recover the whole
		// backup before the entry-scoped skip, which would else false-skip the
		// both-absent case or surgically recreate only the target entry (losing siblings).
		if handled, werr := wholeFileRestoreIfWriteTargetGone(j.path, backupData); handled {
			return werr
		}
		if liveMap, rerr := j.readJSON(); rerr == nil {
			liveServers, _ := liveMap[j.sectionKey()].(map[string]any)
			le, lp := liveServers[name]
			be, bp := backupServers[name]
			if entryRestoreIsNoop(le, lp, be, bp) {
				return nil
			}
		}
	}
	if backupServers != nil {
		if backupEntry, present := backupServers[name]; present {
			// Defensive guard (demigrate flow only — the rollback
			// caller passes allowHubEntry=true to restore the
			// pre-reconcile legacy hub entry verbatim).
			if !allowHubEntry {
				if rawMap, ok := backupEntry.(map[string]any); ok {
					if j.backupEntryMapIsHubManaged(rawMap) {
						return ErrBackupEntryAlreadyMigrated
					}
				}
			}
			// Comment-preserving set into the LIVE config (its comments +
			// unrelated keys survive; the backup's entry VALUE is written).
			return j.setMemberWithWriter(name, backupEntry, writer)
		}
	}
	return j.deleteMemberWithWriter(name, writer)
}

// AllStdioEntries returns every stdio entry from mcpServers.
// Inherited by geminiCLI, qwenCLI, cursorClient, and antigravityClient
// via struct embedding. Antigravity entries DO surface here because
// they have `command='mcphub'`; the cleanup pipeline filters them
// out via isOurOwnProcess, so the inheritance is safe.
func (j *jsonMCPClient) AllStdioEntries() ([]StdioEntry, error) {
	m, err := j.readJSON()
	if err != nil {
		return nil, err
	}
	servers, _ := m[j.sectionKey()].(map[string]any)
	return collectStdioEntries(servers), nil
}

// FindStdioLanguageServerEntries scans mcpServers for stdio entries
// matching the mcp-language-server invocation pattern. Inherited by
// geminiCLI, qwenCLI, cursorClient, and antigravityClient via struct
// embedding. Antigravity entries never match because their `command`
// resolves to mcphub, not mcp-language-server.
func (j *jsonMCPClient) FindStdioLanguageServerEntries() ([]LanguageServerStdioEntry, error) {
	m, err := j.readJSON()
	if err != nil {
		return nil, err
	}
	servers, _ := m[j.sectionKey()].(map[string]any)
	return findLanguageServerStdioInMap(servers), nil
}

// BackupContainsEntry reports whether the backup file at backupPath
// has an mcpServers[name] entry. Inherited by both geminiCLI and
// antigravityClient via struct embedding.
func (j *jsonMCPClient) BackupContainsEntry(backupPath, name string) (bool, error) {
	data, err := os.ReadFile(backupPath)
	if err != nil {
		return false, fmt.Errorf("read backup %s: %w", backupPath, err)
	}
	m, err := parseJSONCBytes(data)
	if err != nil {
		return false, fmt.Errorf("parse backup %s: %w", backupPath, err)
	}
	servers, _ := m[j.sectionKey()].(map[string]any)
	if servers == nil {
		return false, nil
	}
	// Require the entry to be an object — a scalar value at this
	// key would be malformed and, if fed to RestoreEntryFromBackup,
	// would corrupt the live config. Treat non-object values as
	// absent so the sentinel fallback refuses with a clear error.
	entry, ok := servers[name].(map[string]any)
	return ok && entry != nil, nil
}

// backupEntryMapIsHubManaged classifies one parsed entry map by this
// adapter's hub shape. URL-native variants (urlField "url"/"httpUrl",
// inherited by geminiCLI, qwenCLI, cursorClient) use the hub-HTTP shape;
// the Antigravity variant (urlField "command") uses the hub-relay shape.
// Single owner of the per-adapter shape decision shared by
// restoreEntryFromBackup and BackupEntryIsHubManaged.
func (j *jsonMCPClient) backupEntryMapIsHubManaged(rawMap map[string]any) bool {
	if j.urlField == "url" || j.urlField == "httpUrl" {
		return isHubURLShapeEntry(rawMap, j.urlField)
	}
	return isHubRelayShapeEntry(rawMap)
}

// BackupEntryIsHubManaged reports whether mcpServers[name] in the backup
// at backupPath is in this adapter's hub-managed shape. Inherited by
// geminiCLI, qwenCLI, cursorClient, and antigravityClient via struct
// embedding (the antigravity variant routes to the relay-shape branch
// because its urlField is "command"). See Client.BackupEntryIsHubManaged.
func (j *jsonMCPClient) BackupEntryIsHubManaged(backupPath, name string) (bool, error) {
	data, err := os.ReadFile(backupPath)
	if err != nil {
		return false, fmt.Errorf("read backup %s: %w", backupPath, err)
	}
	m, err := parseJSONCBytes(data)
	if err != nil {
		return false, fmt.Errorf("parse backup %s: %w", backupPath, err)
	}
	servers, _ := m[j.sectionKey()].(map[string]any)
	if servers == nil {
		return false, nil
	}
	entry, ok := servers[name].(map[string]any)
	if !ok || entry == nil {
		return false, nil
	}
	return j.backupEntryMapIsHubManaged(entry), nil
}
