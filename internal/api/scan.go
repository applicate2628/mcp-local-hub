package api

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"mcp-local-hub/internal/clients"

	toml "github.com/pelletier/go-toml/v2"
	"gopkg.in/yaml.v3"
)

// ScanOpts provides per-client config paths so tests can point at temp dirs.
// Production callers pass "" for each to use the OS default discovery.
type ScanOpts struct {
	ClaudeConfigPath      string
	CodexConfigPath       string
	CursorConfigPath      string
	VSCodeConfigPath      string
	GeminiConfigPath      string
	QwenConfigPath        string
	AntigravityConfigPath string
	ManifestDir           string
	WithProcessCount      bool // populate ScanEntry.ProcessCount via wmic
}

// probeClientConfigPresence reports whether each known MCP client's
// config file exists on disk and is stat-able. Used by the Servers
// matrix UI to distinguish "client installed but has no entries for
// this server yet" (operator can migrate to it) from "client not
// installed on this host" (cell disabled).
//
// Bug-bash A2 (#13) closure: pre-fix, the UI inferred client presence
// from per-entry `client_presence` keys, which collapsed when a
// wholesale demigrate emptied `mcpServers`. After fix, the UI reads
// this top-level map and renders an "available" cell whenever the
// client is "ok" even with no server entries.
//
// v0.4.5 init-button: distinguish "config file absent but the client's
// root directory exists" (the user has the client installed, just
// never created an MCP config — `missing-init-possible`, GUI shows
// an Initialize affordance) from "neither the file nor its parent
// directory exists" (`missing`, GUI keeps the column disabled because
// creating the parent tree would be a surprising side effect of
// a refresh / one-click action). The distinction is parent-dir
// granular: the immediate dirname must Lstat successfully as a
// directory.
//
// v0.4.5 PR #208 codex r1 F2: a third "missing-init-blocked-symlink"
// state covers parents that are symlinks (e.g. dotfile-management
// setups). The hardened init pipeline refuses to follow parent
// symlinks (O_NOFOLLOW on POSIX, FILE_FLAG_OPEN_REPARSE_POINT on
// Windows), so the GUI suppresses the Initialize button for such
// rows rather than offering a click that would deterministically
// fail with INIT_FAILED.
//
// Only paths the caller actually passed via ScanOpts are probed —
// keeps tempdir-based tests deterministic.
func probeClientConfigPresence(opts ScanOpts) map[string]string {
	out := map[string]string{}
	pairs := []struct {
		name string
		path string
	}{
		{"claude-code", opts.ClaudeConfigPath},
		{"codex-cli", opts.CodexConfigPath},
		{"cursor", opts.CursorConfigPath},
		{"vscode", opts.VSCodeConfigPath},
		{"gemini-cli", opts.GeminiConfigPath},
		{"qwen-cli", opts.QwenConfigPath},
		{"antigravity", opts.AntigravityConfigPath},
	}
	for _, p := range pairs {
		if p.path == "" {
			continue
		}
		// PR #208 deep-sec Lane B rounds 4-6 P2 closure: Lstat-first
		// probe + symlink resolution that matches the write pipeline's
		// default-relax contract.
		//
		// Wrong-shape entries to refuse:
		//   - directory / named pipe / device / junction at config path
		//     → "error" (round 5; matrix shows config-error diagnostic
		//     instead of an "ok" cell that migrate/backup will choke on
		//     when readJSON sees a non-regular target)
		//   - dangling symlink → "error" (round 4; init button +
		//     secure-create refusal lose otherwise)
		//   - any symlink, default OR strict mode → "error" (post-PR
		//     #209: the secure-write pipeline now refuses pre-existing
		//     symlinks in ALL modes — `resolveSymlinkForSecureWrite`
		//     was removed from `secureWriteWithOperatorOpt`. Reporting
		//     "ok" while writes deterministically fail with symlink-
		//     refuse errors is the exact UX trap bot codex-r7 flagged:
		//     user sees a green matrix column, clicks Apply, every
		//     write fails. Aligning presence with the active write
		//     contract restores invariant "ok = write will succeed".
		//     Dotfile-symlink setups that used to rely on default-mode
		//     resolve-to-target are now unsupported by design — the
		//     security boundary closure in PR #209 traded that path
		//     for confused-deputy integrity protection).
		if lst, lerr := os.Lstat(p.path); lerr == nil {
			isSymlink := lst.Mode()&os.ModeSymlink != 0
			if !lst.Mode().IsRegular() && !isSymlink {
				out[p.name] = "error"
				continue
			}
			if isSymlink {
				out[p.name] = "error"
				continue
			}
		}
		if _, err := os.Stat(p.path); err == nil {
			out[p.name] = "ok"
		} else if os.IsNotExist(err) {
			out[p.name] = classifyMissingClientConfig(p.path)
		} else {
			out[p.name] = "error"
		}
	}
	return out
}

// classifyMissingClientConfig returns one of:
//
//   - "missing-init-possible"        : the config file's immediate
//     parent directory exists and is a regular directory (operator can
//     click Initialize; the hardened init pipeline will accept it).
//   - "missing-init-blocked-symlink" : the parent path resolves through
//     a symlink (Init pipeline opens the parent with O_NOFOLLOW on POSIX
//     / FILE_FLAG_OPEN_REPARSE_POINT on Windows, both of which refuse
//     to follow the link — so an Initialize click would deterministically
//     fail with INIT_FAILED, broken UX). Common with dotfile-management
//     setups where the operator symlinks ~/.config/Claude/ to a real
//     dotfile repo location. The GUI suppresses the Initialize button
//     for this state; operators who want the stub seeded should either
//     remove the symlink (and let mcphub create the config in the real
//     parent), or seed the file manually inside the symlinked target.
//   - "missing"                       : neither file nor parent dir
//     exists (client genuinely not installed on this host).
//
// Split out for testability and to keep probeClientConfigPresence flat.
//
// v0.4.5 PR #208 codex r1 F2 closure: prior version used os.Stat which
// follows symlinks; a symlinked parent passed the IsDir check, scan
// classified as missing-init-possible, GUI showed Initialize button,
// click failed with INIT_FAILED. The Lstat-first probe now distinguishes
// the symlinked-parent case so the UI matches the actual write contract.
func classifyMissingClientConfig(path string) string {
	parent := filepath.Dir(path)
	if parent == "" || parent == "." {
		return "missing"
	}
	lst, lerr := os.Lstat(parent)
	if lerr != nil {
		// Parent path itself is absent OR stat failed; either way,
		// initializing through this path is not a clean operation.
		return "missing"
	}
	if lst.Mode()&os.ModeSymlink != 0 {
		// Symlinked parent. The hardened init pipeline refuses to open
		// the parent through a symlink (POSIX O_NOFOLLOW, Windows
		// FILE_FLAG_OPEN_REPARSE_POINT — the latter opens the reparse
		// point itself without auto-resolving for relative child ops,
		// which similarly breaks the init create flow). Classify so
		// the GUI suppresses the Initialize affordance.
		return "missing-init-blocked-symlink"
	}
	if !lst.IsDir() {
		// Parent path exists but is not a directory (regular file,
		// device, pipe). Not initializable.
		return "missing"
	}
	return "missing-init-possible"
}

// perSessionServers are MCP servers whose sessions must remain isolated
// per local client/process. Even when an upstream tool supports a session_id
// parameter, we conservatively keep them per-session unless the hub
// enforces caller authentication and session ownership.
//
// gdb was previously listed here; PR #24 restored servers/gdb/manifest.yaml
// as a hub-managed daemon because GDB-MCP has built-in session management
// (modules/{gdb,lldb}/sessionManager.py) where each client call carries a
// session_id, so one daemon serves N concurrent debug sessions safely.
// Keeping gdb in this map after restoring the manifest would force
// CanMigrate=false in scan results and contradict the manifest contract.
// Codex bot review on PR #24.
var perSessionServers = map[string]bool{
	"playwright": true,
}

// ScanFrom builds a unified cross-client view. Exposed (rather than Scan) so
// tests can pass arbitrary paths.
//
// PR #208 deep-sec Lane B round 6 P2 closure: presence is computed
// FIRST so adapter reads can be skipped for clients whose config
// path is not a readable regular file (directory, FIFO, dangling
// symlink, etc.). Previously the per-adapter `scan*` calls hit
// `os.ReadFile` before presence was known, and any non-IsNotExist
// read error propagated as a whole-response 500 SCAN_FAILED —
// hiding the per-client diagnostic the frontend needs to render the
// `config-error` cell. With the reorder, a wrong-shape entry at one
// client's config path leaves the rest of the scan intact and the
// per-client diagnostic surfaces in `client_config_presence`.
func (a *API) ScanFrom(opts ScanOpts) (*ScanResult, error) {
	entries := map[string]*ScanEntry{}
	presence := probeClientConfigPresence(opts)
	scanIfReadable := func(name string) bool {
		// "ok" is the only state for which an adapter read is
		// guaranteed to find a regular file. "missing" /
		// "missing-init-possible" / "error" all imply that the
		// adapter's `os.ReadFile` would either return IsNotExist
		// (which adapters already absorb to "no entries") or
		// hit the wrong-shape failure we want to avoid.
		return presence[name] == "ok"
	}

	if opts.ClaudeConfigPath != "" && scanIfReadable("claude-code") {
		if err := scanClaude(entries, opts.ClaudeConfigPath); err != nil {
			return nil, fmt.Errorf("claude: %w", err)
		}
	}
	if opts.CodexConfigPath != "" && scanIfReadable("codex-cli") {
		if err := scanCodex(entries, opts.CodexConfigPath); err != nil {
			return nil, fmt.Errorf("codex: %w", err)
		}
	}
	if opts.CursorConfigPath != "" && scanIfReadable("cursor") {
		if err := scanCursor(entries, opts.CursorConfigPath); err != nil {
			return nil, fmt.Errorf("cursor: %w", err)
		}
	}
	if opts.VSCodeConfigPath != "" && scanIfReadable("vscode") {
		if err := scanVSCode(entries, opts.VSCodeConfigPath); err != nil {
			return nil, fmt.Errorf("vscode: %w", err)
		}
	}
	if opts.GeminiConfigPath != "" && scanIfReadable("gemini-cli") {
		if err := scanGemini(entries, opts.GeminiConfigPath); err != nil {
			return nil, fmt.Errorf("gemini: %w", err)
		}
	}
	if opts.QwenConfigPath != "" && scanIfReadable("qwen-cli") {
		if err := scanQwen(entries, opts.QwenConfigPath); err != nil {
			return nil, fmt.Errorf("qwen: %w", err)
		}
	}
	if opts.AntigravityConfigPath != "" && scanIfReadable("antigravity") {
		if err := scanAntigravity(entries, opts.AntigravityConfigPath); err != nil {
			return nil, fmt.Errorf("antigravity: %w", err)
		}
	}

	manifestNames, err := readManifestNames(opts.ManifestDir)
	if err != nil {
		return nil, fmt.Errorf("manifests: %w", err)
	}
	for name, e := range entries {
		e.Name = name
		e.ManifestExists = manifestNames[name]
		e.CanMigrate = e.ManifestExists && !perSessionServers[name]
		e.Status = classify(e, name, manifestNames)
	}

	out := &ScanResult{
		At:                   time.Now(),
		ClientConfigPresence: presence,
	}
	for _, e := range entries {
		out.Entries = append(out.Entries, *e)
	}
	if opts.WithProcessCount {
		// One process-snapshot shared across every entry. Previously
		// CountProcesses launched wmic per entry — for ~20 scan rows
		// that's ~13 s wall time. Single snapshot + in-memory count
		// drops the scan to ~1 s.
		snap := takeProcessSnapshot()
		for i := range out.Entries {
			patterns := patternsForServer(out.Entries[i].Name, opts.ManifestDir)
			if len(patterns) == 0 {
				continue
			}
			out.Entries[i].ProcessCount = a.CountProcessesFromSnapshot(snap, patterns)
		}
	}
	return out, nil
}

// isOurRelayBinary returns true when the given path points at our CLI
// binary — either the current name (mcphub.exe) or the legacy name
// (mcp.exe) that early installations may still have in client configs.
// Delegates to clients.IsMcphubBinary so the binary-name allowlist lives in
// exactly one place; this wrapper is kept only because the classifier
// below reads better when paired with a local name. If a future client
// ever persists a different binary name, update only IsMcphubBinary.
func isOurRelayBinary(cmd string) bool {
	return clients.IsMcphubBinary(cmd)
}

// genericInterpreters are command names that match far too many unrelated
// processes (every npx-invoked tool, every python script, etc.) to be useful
// as identifying patterns. We skip them when building patterns and rely on
// server-specific tokens (package names, script paths) for identification.
var genericInterpreters = map[string]bool{
	"npx": true, "npx.cmd": true,
	"node": true, "node.exe": true,
	"python": true, "python.exe": true, "python3": true,
	"uv": true, "uvx": true, "uv.exe": true, "uvx.exe": true,
	"cmd": true, "cmd.exe": true,
	"sh": true, "bash": true,
}

// patternsForServer returns the substring patterns used to identify running
// processes of this server. Generic interpreters (npx, node, python, uvx)
// are skipped because they match thousands of unrelated processes; only
// server-specific tokens (package names, script paths) reliably identify
// a server's own processes. For non-manifested (unknown/per-session) servers,
// falls back to the server name — callers treat counts for unknown servers
// as an upper bound.
func patternsForServer(serverName, manifestDir string) []string {
	var (
		data []byte
		err  error
	)
	if manifestDir == "" {
		// Production path: embed first, disk fallback.
		data, err = loadManifestYAMLEmbedFirst(serverName)
	} else {
		// Test-pathway: explicit dir only.
		data, err = os.ReadFile(filepath.Join(manifestDir, serverName, "manifest.yaml"))
	}
	if err != nil {
		return []string{serverName}
	}
	m, err := parseManifestForName(serverName, data)
	if err != nil {
		return []string{serverName}
	}
	var out []string
	if m.Command != "" && !genericInterpreters[m.Command] {
		out = append(out, m.Command)
	}
	for _, arg := range m.BaseArgs {
		if len(arg) <= 3 || strings.HasPrefix(arg, "-") {
			continue
		}
		// Apply the same generic-interpreter filter to BaseArgs so manifests
		// that embed an interpreter in their args (e.g. gdb's "python",
		// "server.py") don't contribute substrings that match unrelated
		// processes system-wide. Dropbox ships a bundled Python, VS Code
		// ships node, MSYS2 fills every shell with "python"/"node" tokens —
		// matching any of those as an orphan pattern is a false-positive
		// bomb. Keep only paths / package-name tokens that are unique to
		// the server.
		base := strings.ToLower(filepath.Base(arg))
		if genericInterpreters[base] {
			continue
		}
		out = append(out, arg)
	}
	if len(out) == 0 {
		out = append(out, serverName)
	}
	return out
}

func scanClaude(entries map[string]*ScanEntry, path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	var cfg struct {
		MCPServers map[string]map[string]any `json:"mcpServers"`
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		return err
	}
	for name, raw := range cfg.MCPServers {
		e := entries[name]
		if e == nil {
			e = &ScanEntry{ClientPresence: map[string]ClientEntry{}}
			entries[name] = e
		}
		e.ClientPresence["claude-code"] = shapeClaudeEntry(raw)
	}
	return nil
}

func shapeClaudeEntry(raw map[string]any) ClientEntry {
	if url, ok := raw["url"].(string); ok {
		return ClientEntry{Transport: "http", Endpoint: url, Raw: raw}
	}
	cmd, _ := raw["command"].(string)
	return ClientEntry{Transport: "stdio", Endpoint: cmd, Raw: raw}
}

func scanCodex(entries map[string]*ScanEntry, path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	var root map[string]any
	if err := toml.Unmarshal(data, &root); err != nil {
		return err
	}
	srv, _ := root["mcp_servers"].(map[string]any)
	for name, raw := range srv {
		m, _ := raw.(map[string]any)
		e := entries[name]
		if e == nil {
			e = &ScanEntry{ClientPresence: map[string]ClientEntry{}}
			entries[name] = e
		}
		e.ClientPresence["codex-cli"] = shapeCodexEntry(m)
	}
	return nil
}

func shapeCodexEntry(raw map[string]any) ClientEntry {
	if url, ok := raw["url"].(string); ok {
		return ClientEntry{Transport: "http", Endpoint: url, Raw: raw}
	}
	cmd, _ := raw["command"].(string)
	return ClientEntry{Transport: "stdio", Endpoint: cmd, Raw: raw}
}

func scanCursor(entries map[string]*ScanEntry, path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	var cfg struct {
		MCPServers map[string]map[string]any `json:"mcpServers"`
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		return err
	}
	for name, raw := range cfg.MCPServers {
		e := entries[name]
		if e == nil {
			e = &ScanEntry{ClientPresence: map[string]ClientEntry{}}
			entries[name] = e
		}
		e.ClientPresence["cursor"] = shapeCursorEntry(raw)
	}
	return nil
}

func shapeCursorEntry(raw map[string]any) ClientEntry {
	if url, ok := raw["url"].(string); ok {
		return ClientEntry{Transport: "http", Endpoint: url, Raw: raw}
	}
	cmd, _ := raw["command"].(string)
	return ClientEntry{Transport: "stdio", Endpoint: cmd, Raw: raw}
}

func scanVSCode(entries map[string]*ScanEntry, path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	var cfg struct {
		Servers map[string]map[string]any `json:"servers"`
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		return err
	}
	for name, raw := range cfg.Servers {
		e := entries[name]
		if e == nil {
			e = &ScanEntry{ClientPresence: map[string]ClientEntry{}}
			entries[name] = e
		}
		e.ClientPresence["vscode"] = shapeVSCodeEntry(raw)
	}
	return nil
}

func shapeVSCodeEntry(raw map[string]any) ClientEntry {
	if url, ok := raw["url"].(string); ok {
		return ClientEntry{Transport: "http", Endpoint: url, Raw: raw}
	}
	cmd, _ := raw["command"].(string)
	return ClientEntry{Transport: "stdio", Endpoint: cmd, Raw: raw}
}

func scanGemini(entries map[string]*ScanEntry, path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	var cfg struct {
		MCPServers map[string]map[string]any `json:"mcpServers"`
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		return err
	}
	for name, raw := range cfg.MCPServers {
		e := entries[name]
		if e == nil {
			e = &ScanEntry{ClientPresence: map[string]ClientEntry{}}
			entries[name] = e
		}
		e.ClientPresence["gemini-cli"] = shapeGeminiEntry(raw)
	}
	return nil
}

func shapeGeminiEntry(raw map[string]any) ClientEntry {
	if url, ok := raw["url"].(string); ok {
		return ClientEntry{Transport: "http", Endpoint: url, Raw: raw}
	}
	if url, ok := raw["httpUrl"].(string); ok {
		return ClientEntry{Transport: "http", Endpoint: url, Raw: raw}
	}
	cmd, _ := raw["command"].(string)
	return ClientEntry{Transport: "stdio", Endpoint: cmd, Raw: raw}
}

func scanQwen(entries map[string]*ScanEntry, path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	var cfg struct {
		MCPServers map[string]map[string]any `json:"mcpServers"`
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		return err
	}
	for name, raw := range cfg.MCPServers {
		e := entries[name]
		if e == nil {
			e = &ScanEntry{ClientPresence: map[string]ClientEntry{}}
			entries[name] = e
		}
		e.ClientPresence["qwen-cli"] = shapeQwenEntry(raw)
	}
	return nil
}

func shapeQwenEntry(raw map[string]any) ClientEntry {
	if url, ok := raw["httpUrl"].(string); ok {
		return ClientEntry{Transport: "http", Endpoint: url, Raw: raw}
	}
	if url, ok := raw["url"].(string); ok {
		return ClientEntry{Transport: "http", Endpoint: url, Raw: raw}
	}
	cmd, _ := raw["command"].(string)
	return ClientEntry{Transport: "stdio", Endpoint: cmd, Raw: raw}
}

func scanAntigravity(entries map[string]*ScanEntry, path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	var cfg struct {
		MCPServers map[string]map[string]any `json:"mcpServers"`
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		return err
	}
	for name, raw := range cfg.MCPServers {
		e := entries[name]
		if e == nil {
			e = &ScanEntry{ClientPresence: map[string]ClientEntry{}}
			entries[name] = e
		}
		e.ClientPresence["antigravity"] = shapeAntigravityEntry(raw)
	}
	return nil
}

func shapeAntigravityEntry(raw map[string]any) ClientEntry {
	if url, ok := raw["serverUrl"].(string); ok {
		return ClientEntry{Transport: "http", Endpoint: url, Raw: raw}
	}
	// Detect our own relay shape: command is mcphub.exe (or legacy mcp.exe)
	// with args[0]=="relay". Accepting both names because early installs
	// used "mcp.exe" before the rename.
	if cmd, ok := raw["command"].(string); ok {
		if args, ok := raw["args"].([]any); ok && len(args) > 0 {
			if first, _ := args[0].(string); first == "relay" && isOurRelayBinary(cmd) {
				return ClientEntry{Transport: "relay", Endpoint: cmd, Raw: raw}
			}
		}
		return ClientEntry{Transport: "stdio", Endpoint: cmd, Raw: raw}
	}
	return ClientEntry{Transport: "absent", Raw: raw}
}

// readManifestNames returns the set of available server names.
// Empty dir selects the production path (embedded manifests union
// on-disk defaultManifestDir). A non-empty dir restricts to that
// directory only — used by tests to inject a hermetic manifest set.
func readManifestNames(dir string) (map[string]bool, error) {
	names := map[string]bool{}
	if dir == "" {
		list, err := listManifestNamesEmbedFirst()
		if err != nil {
			return nil, err
		}
		for _, n := range list {
			names[n] = true
		}
		return names, nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return names, nil
		}
		return nil, err
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if _, err := os.Stat(filepath.Join(dir, e.Name(), "manifest.yaml")); err == nil {
			names[e.Name()] = true
		}
	}
	return names, nil
}

func classify(e *ScanEntry, name string, manifestNames map[string]bool) string {
	if perSessionServers[name] {
		return "per-session"
	}
	hasHub := false
	hasStdio := false
	for _, c := range e.ClientPresence {
		if c.Transport == "http" && clients.IsHubHTTPURL(c.Endpoint) {
			hasHub = true
		}
		if c.Transport == "relay" {
			// Antigravity's hub-routed shape: the hub rewrites Antigravity
			// bindings into a relay command (mcphub binary + args[0]=="relay").
			// scan.go:310 flags this as Transport: "relay". Without this branch
			// hub-routed Antigravity servers fall to "not-installed" and the
			// Migration screen drops them, hiding a real demigrate candidate.
			// (PR #4 Codex R1.)
			hasHub = true
		}
		if c.Transport == "stdio" {
			hasStdio = true
		}
	}
	if hasHub && !hasStdio {
		return "via-hub"
	}
	if hasStdio && manifestNames[name] {
		return "can-migrate"
	}
	if hasStdio {
		return "unknown"
	}
	return "not-installed"
}

// Scan is the production entry point: it resolves client config paths from
// OS defaults and calls ScanFrom.
func (a *API) Scan() (*ScanResult, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	return a.ScanFrom(ScanOpts{
		ClaudeConfigPath:      filepath.Join(home, ".claude.json"),
		CodexConfigPath:       filepath.Join(home, ".codex", "config.toml"),
		CursorConfigPath:      filepath.Join(home, ".cursor", "mcp.json"),
		VSCodeConfigPath:      mustClientConfigPath("vscode"),
		GeminiConfigPath:      filepath.Join(home, ".gemini", "settings.json"),
		QwenConfigPath:        filepath.Join(home, ".qwen", "settings.json"),
		AntigravityConfigPath: filepath.Join(home, ".gemini", "antigravity", "mcp_config.json"),
		// Empty ManifestDir → ScanFrom uses the embed-first resolution
		// path. The on-disk defaultManifestDir stays available as a
		// secondary source for dev-checkout scenarios where a freshly-
		// added manifest hasn't been compiled into the binary yet.
		ManifestDir: "",
	})
}

func mustClientConfigPath(name string) string {
	path, err := clients.ConfigPathForName(name)
	if err != nil {
		return ""
	}
	return path
}

// defaultManifestDir returns the path to `servers/` resolved against the
// running binary's location. Two layouts supported:
//   - binary and servers/ in the same directory (legacy / standalone install)
//   - binary in bin/ and servers/ in bin/../ (standard Go project layout)
//
// If neither exists, returns the sibling path (exeDir/servers) without
// consulting the current working directory. This avoids untrusted CWD-based
// manifest resolution while preserving a deterministic on-disk location.
func defaultManifestDir() string {
	exe, err := os.Executable()
	if err != nil {
		// Fail closed to a deterministic, clearly invalid absolute-ish path
		// rather than a CWD-relative location.
		return filepath.Join(string(os.PathSeparator), "nonexistent", "mcphub", "servers")
	}
	exeDir := filepath.Dir(exe)
	// Legacy: exe and servers/ at same level.
	sibling := filepath.Join(exeDir, "servers")
	if st, err := os.Stat(sibling); err == nil && st.IsDir() {
		return sibling
	}
	// Standard: exe in bin/, servers/ one level up.
	parent := filepath.Join(exeDir, "..", "servers")
	if st, err := os.Stat(parent); err == nil && st.IsDir() {
		return parent
	}
	// Last resort: deterministic path near the executable (not CWD).
	return sibling
}

// ExtractManifestFromClient reads a stdio entry from the specified client
// config and renders a draft manifest.yaml suitable for the GUI "Create
// manifest" flow. The draft includes bindings for every supported managed
// client; users edit as desired before saving.
func (a *API) ExtractManifestFromClient(client, serverName string, opts ScanOpts) (string, error) {
	var raw map[string]any

	switch client {
	case "claude-code":
		if opts.ClaudeConfigPath == "" {
			return "", fmt.Errorf("ClaudeConfigPath empty")
		}
		data, err := os.ReadFile(opts.ClaudeConfigPath)
		if err != nil {
			return "", err
		}
		var cfg struct {
			MCPServers map[string]map[string]any `json:"mcpServers"`
		}
		if err := json.Unmarshal(data, &cfg); err != nil {
			return "", err
		}
		raw = cfg.MCPServers[serverName]

	case "codex-cli":
		if opts.CodexConfigPath == "" {
			return "", fmt.Errorf("CodexConfigPath empty")
		}
		data, err := os.ReadFile(opts.CodexConfigPath)
		if err != nil {
			return "", err
		}
		var root map[string]any
		if err := toml.Unmarshal(data, &root); err != nil {
			return "", err
		}
		servers, _ := root["mcp_servers"].(map[string]any)
		if servers != nil {
			raw, _ = servers[serverName].(map[string]any)
		}

	case "cursor":
		if opts.CursorConfigPath == "" {
			return "", fmt.Errorf("CursorConfigPath empty")
		}
		data, err := os.ReadFile(opts.CursorConfigPath)
		if err != nil {
			return "", err
		}
		var cfg struct {
			MCPServers map[string]map[string]any `json:"mcpServers"`
		}
		if err := json.Unmarshal(data, &cfg); err != nil {
			return "", err
		}
		raw = cfg.MCPServers[serverName]

	case "vscode":
		if opts.VSCodeConfigPath == "" {
			return "", fmt.Errorf("VSCodeConfigPath empty")
		}
		data, err := os.ReadFile(opts.VSCodeConfigPath)
		if err != nil {
			return "", err
		}
		var cfg struct {
			Servers map[string]map[string]any `json:"servers"`
		}
		if err := json.Unmarshal(data, &cfg); err != nil {
			return "", err
		}
		raw = cfg.Servers[serverName]

	case "gemini-cli":
		if opts.GeminiConfigPath == "" {
			return "", fmt.Errorf("GeminiConfigPath empty")
		}
		data, err := os.ReadFile(opts.GeminiConfigPath)
		if err != nil {
			return "", err
		}
		var cfg struct {
			MCPServers map[string]map[string]any `json:"mcpServers"`
		}
		if err := json.Unmarshal(data, &cfg); err != nil {
			return "", err
		}
		raw = cfg.MCPServers[serverName]

	case "qwen-cli":
		if opts.QwenConfigPath == "" {
			return "", fmt.Errorf("QwenConfigPath empty")
		}
		data, err := os.ReadFile(opts.QwenConfigPath)
		if err != nil {
			return "", err
		}
		var cfg struct {
			MCPServers map[string]map[string]any `json:"mcpServers"`
		}
		if err := json.Unmarshal(data, &cfg); err != nil {
			return "", err
		}
		raw = cfg.MCPServers[serverName]

	case "antigravity":
		if opts.AntigravityConfigPath == "" {
			return "", fmt.Errorf("AntigravityConfigPath empty")
		}
		data, err := os.ReadFile(opts.AntigravityConfigPath)
		if err != nil {
			return "", err
		}
		var cfg struct {
			MCPServers map[string]map[string]any `json:"mcpServers"`
		}
		if err := json.Unmarshal(data, &cfg); err != nil {
			return "", err
		}
		raw = cfg.MCPServers[serverName]
		// Antigravity entries written by mcphub migrate use command=mcphub,
		// args[0]="relay". Extracting a manifest from THAT would loop:
		// manifest → install → relay entry → manifest → ... Reject narrowly:
		// command must be the mcphub binary AND args[0] must equal "relay".
		// A user's genuine stdio server whose first arg happens to be "relay"
		// but whose command is not mcphub passes through unchanged. Uses the
		// shared clients.IsMcphubBinary helper (also used by RestoreEntryFromBackup
		// for hub-relay detection in adapter defensive checks).
		if raw != nil {
			cmd, _ := raw["command"].(string)
			if clients.IsMcphubBinary(cmd) {
				if args, ok := raw["args"].([]any); ok && len(args) > 0 {
					if first, ok := args[0].(string); ok && first == "relay" {
						return "", fmt.Errorf("entry %q is a mcphub-managed relay stdio (command is mcphub binary + args[0]==\"relay\") — not user-configured stdio, cannot extract a manifest from it", serverName)
					}
				}
			}
		}

	default:
		return "", fmt.Errorf("extract not yet supported for client %q (extend here when needed)", client)
	}
	if raw == nil {
		return "", fmt.Errorf("server %q not found in client %q config", serverName, client)
	}

	cmd, _ := raw["command"].(string)
	// Reject HTTP-only / hub-managed entries early. Extract is for stdio
	// servers; an entry that has no `command` cannot produce a valid
	// manifest (renderDraftManifestYAML would emit an empty `command:`
	// line and ServerManifest.Validate would then fail with a less
	// actionable error). The most common case is a user trying to
	// extract from a server they already migrated — the entry is now
	// hub-HTTP (HTTP-native clients) or hub-relay with empty-command
	// downgrades — so we guide them toward demigrate instead.
	if cmd == "" {
		return "", fmt.Errorf("server %q in client %q has no `command` field — it is an HTTP-only or hub-managed entry, not user-configured stdio (run `mcphub demigrate %s` to restore the pre-migrate shape first if this server was migrated)", serverName, client, serverName)
	}
	var args []string
	if arr, ok := raw["args"].([]any); ok {
		for _, v := range arr {
			if s, ok := v.(string); ok {
				args = append(args, s)
			}
		}
	}
	envMap := map[string]string{}
	if envAny, ok := raw["env"].(map[string]any); ok {
		for k, v := range envAny {
			if s, ok := v.(string); ok {
				envMap[k] = s
			}
		}
	}

	// Pick next free port in 9121-9139 range not already used by other manifests.
	port, err := pickNextFreePort(opts.ManifestDir)
	if err != nil {
		return "", err
	}

	return renderDraftManifestYAML(serverName, cmd, args, envMap, port), nil
}

func pickNextFreePort(manifestDir string) (int, error) {
	used := map[int]bool{}
	entries, _ := os.ReadDir(manifestDir)
	for _, e := range entries {
		data, err := os.ReadFile(filepath.Join(manifestDir, e.Name(), "manifest.yaml"))
		if err != nil {
			continue
		}
		// Minimal YAML scrape — we do not want to pull go-yaml just for this.
		for line := range strings.SplitSeq(string(data), "\n") {
			line = strings.TrimSpace(line)
			const p = "port:"
			if strings.HasPrefix(line, p) {
				var n int
				fmt.Sscanf(line, "port: %d", &n)
				if n > 0 {
					used[n] = true
				}
			}
		}
	}
	for p := 9121; p <= 9139; p++ {
		if !used[p] {
			return p, nil
		}
	}
	return 0, fmt.Errorf("no free port in 9121-9139 range")
}

func renderDraftManifestYAML(name, cmd string, args []string, env map[string]string, port int) string {
	doc := struct {
		Name           string            `yaml:"name"`
		Kind           string            `yaml:"kind"`
		Transport      string            `yaml:"transport"`
		Command        string            `yaml:"command"`
		BaseArgs       []string          `yaml:"base_args,omitempty"`
		Env            map[string]string `yaml:"env,omitempty"`
		Daemons        []map[string]any  `yaml:"daemons"`
		ClientBindings []map[string]any  `yaml:"client_bindings"`
		WeeklyRefresh  bool              `yaml:"weekly_refresh"`
	}{
		Name:      name,
		Kind:      "global",
		Transport: "stdio-bridge",
		Command:   cmd,
		BaseArgs:  args,
		Env:       env,
		Daemons: []map[string]any{
			{"name": "default", "port": port},
		},
		ClientBindings: []map[string]any{
			{"client": "claude-code", "daemon": "default", "url_path": "/mcp"},
			{"client": "codex-cli", "daemon": "default", "url_path": "/mcp"},
			{"client": "cursor", "daemon": "default", "url_path": "/mcp"},
			{"client": "vscode", "daemon": "default", "url_path": "/mcp"},
			{"client": "gemini-cli", "daemon": "default", "url_path": "/mcp"},
			{"client": "qwen-cli", "daemon": "default", "url_path": "/mcp"},
			{"client": "antigravity", "daemon": "default", "url_path": "/mcp"},
		},
		WeeklyRefresh: false,
	}
	out, err := yaml.Marshal(doc)
	if err != nil {
		// yaml.Marshal on this static structure should not fail; fallback keeps behavior.
		return ""
	}
	return string(out)
}
