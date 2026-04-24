package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"mcp-local-hub/internal/clients"
	"mcp-local-hub/internal/config"

	toml "github.com/pelletier/go-toml/v2"
)

// ScanOpts provides per-client config paths so tests can point at temp dirs.
// Production callers pass "" for each to use the OS default discovery.
type ScanOpts struct {
	ClaudeConfigPath      string
	CodexConfigPath       string
	GeminiConfigPath      string
	AntigravityConfigPath string
	ManifestDir           string
	WithProcessCount      bool // populate ScanEntry.ProcessCount via wmic
}

// perSessionServers are MCP servers whose sessions must remain isolated
// per local client/process. Even when an upstream tool supports a session_id
// parameter, we conservatively keep debuggers per-session unless the hub
// enforces caller authentication and session ownership.
var perSessionServers = map[string]bool{
	"gdb":        true,
	"playwright": true,
}

// ScanFrom builds a unified cross-client view. Exposed (rather than Scan) so
// tests can pass arbitrary paths.
func (a *API) ScanFrom(opts ScanOpts) (*ScanResult, error) {
	entries := map[string]*ScanEntry{}

	if opts.ClaudeConfigPath != "" {
		if err := scanClaude(entries, opts.ClaudeConfigPath); err != nil {
			return nil, fmt.Errorf("claude: %w", err)
		}
	}
	if opts.CodexConfigPath != "" {
		if err := scanCodex(entries, opts.CodexConfigPath); err != nil {
			return nil, fmt.Errorf("codex: %w", err)
		}
	}
	if opts.GeminiConfigPath != "" {
		if err := scanGemini(entries, opts.GeminiConfigPath); err != nil {
			return nil, fmt.Errorf("gemini: %w", err)
		}
	}
	if opts.AntigravityConfigPath != "" {
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

	out := &ScanResult{At: time.Now()}
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
// Delegates to clients.IsMcphubBinary so the 4-name allowlist lives in
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
	m, err := config.ParseManifest(bytes.NewReader(data))
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
		GeminiConfigPath:      filepath.Join(home, ".gemini", "settings.json"),
		AntigravityConfigPath: filepath.Join(home, ".gemini", "antigravity", "mcp_config.json"),
		// Empty ManifestDir → ScanFrom uses the embed-first resolution
		// path. The on-disk defaultManifestDir stays available as a
		// secondary source for dev-checkout scenarios where a freshly-
		// added manifest hasn't been compiled into the binary yet.
		ManifestDir: "",
	})
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
// manifest" flow. The draft always includes bindings for all four clients;
// users edit as desired before saving.
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
	// hub-HTTP (Claude/Codex/Gemini) or hub-relay with empty-command
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
	var b strings.Builder
	fmt.Fprintf(&b, "name: %s\n", name)
	fmt.Fprintln(&b, "kind: global")
	fmt.Fprintln(&b, "transport: stdio-bridge")
	fmt.Fprintf(&b, "command: %s\n", cmd)
	if len(args) > 0 {
		fmt.Fprintln(&b, "base_args:")
		for _, a := range args {
			fmt.Fprintf(&b, "  - %q\n", a)
		}
	}
	if len(env) > 0 {
		fmt.Fprintln(&b, "env:")
		for k, v := range env {
			fmt.Fprintf(&b, "  %s: %q\n", k, v)
		}
	}
	fmt.Fprintln(&b, "")
	fmt.Fprintln(&b, "daemons:")
	fmt.Fprintln(&b, "  - name: default")
	fmt.Fprintf(&b, "    port: %d\n", port)
	fmt.Fprintln(&b, "")
	fmt.Fprintln(&b, "client_bindings:")
	for _, c := range []string{"claude-code", "codex-cli", "gemini-cli", "antigravity"} {
		fmt.Fprintf(&b, "  - client: %s\n", c)
		fmt.Fprintln(&b, "    daemon: default")
		fmt.Fprintln(&b, "    url_path: /mcp")
	}
	fmt.Fprintln(&b, "")
	fmt.Fprintln(&b, "weekly_refresh: false")
	return b.String()
}
