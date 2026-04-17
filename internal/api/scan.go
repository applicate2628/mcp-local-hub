package api

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

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

// perSessionServers are MCP servers whose state genuinely cannot be shared
// across clients. Note: many servers that look like they'd be per-session
// (gdb, lldb) actually have built-in session-management (SessionManager with
// sessions[session_id] dict), so they CAN be hub-shared — one daemon serves
// N concurrent debug sessions tracked by session_id.
//
// Before adding a server here, grep its source for "session_id" / "sessions["
// patterns. If present, it's session-multiplexed and belongs in a manifest,
// not here.
//
// lldb: the common lldb-bridge tool is a TCP-stdio bridge to a SINGLE LLDB
// protocol-server connection — each client connection owns its bridge and
// its LLDB. Different architecture from GDB-MCP's session manager. Stays
// per-session until/unless a session-multiplexed lldb MCP server exists.
//
// playwright: each session typically spawns its own browser context which
// is heavy; leaving as per-session until we confirm multiplexing behavior.
var perSessionServers = map[string]bool{
	"lldb":       true,
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
		for i := range out.Entries {
			patterns := patternsForServer(out.Entries[i].Name, opts.ManifestDir)
			if len(patterns) == 0 {
				continue
			}
			count, err := a.CountProcesses(patterns)
			if err == nil {
				out.Entries[i].ProcessCount = count
			}
		}
	}
	return out, nil
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
	f, err := os.Open(filepath.Join(manifestDir, serverName, "manifest.yaml"))
	if err != nil {
		return []string{serverName}
	}
	defer f.Close()
	m, err := config.ParseManifest(f)
	if err != nil {
		return []string{serverName}
	}
	var out []string
	if m.Command != "" && !genericInterpreters[m.Command] {
		out = append(out, m.Command)
	}
	for _, arg := range m.BaseArgs {
		if len(arg) > 3 && !strings.HasPrefix(arg, "-") {
			out = append(out, arg)
		}
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
	// Detect our own relay shape: command=*mcp.exe args[0]=="relay".
	if cmd, ok := raw["command"].(string); ok {
		if args, ok := raw["args"].([]any); ok && len(args) > 0 {
			if first, _ := args[0].(string); first == "relay" && strings.HasSuffix(strings.ToLower(cmd), "mcp.exe") {
				return ClientEntry{Transport: "relay", Endpoint: cmd, Raw: raw}
			}
		}
		return ClientEntry{Transport: "stdio", Endpoint: cmd, Raw: raw}
	}
	return ClientEntry{Transport: "absent", Raw: raw}
}

func readManifestNames(dir string) (map[string]bool, error) {
	names := map[string]bool{}
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
		if c.Transport == "http" && strings.Contains(c.Endpoint, "localhost") {
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
		ManifestDir:           defaultManifestDir(),
	})
}

// defaultManifestDir returns the path to `servers/` resolved against the
// running binary's location. Two layouts supported:
//   - binary and servers/ in the same directory (legacy / standalone install)
//   - binary in bin/ and servers/ in bin/../ (standard Go project layout)
// Falls back to CWD-relative "servers" if neither exists, so tests that
// don't set an explicit ManifestDir still work when run from the repo root.
func defaultManifestDir() string {
	exe, err := os.Executable()
	if err != nil {
		return "servers"
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
	// Last resort: relative to CWD.
	return "servers"
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
	default:
		return "", fmt.Errorf("extract not yet supported for client %q (extend here when needed)", client)
	}
	if raw == nil {
		return "", fmt.Errorf("server %q not found in client %q config", serverName, client)
	}

	cmd, _ := raw["command"].(string)
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
		for _, line := range strings.Split(string(data), "\n") {
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
