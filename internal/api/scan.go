package api

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
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
	// Wave-2 opt-in clients (PR #306 added the adapters; this PR wires
	// them into the scan/GUI surface). One config path per client,
	// defaulted from clients.ConfigPathForName in Scan() and pointable
	// at temp dirs by tests. Empty paths are skipped by ScanFrom exactly
	// like the original seven.
	ZedConfigPath      string
	KiroConfigPath     string
	WindsurfConfigPath string
	ClineConfigPath    string
	KiloCodeConfigPath string
	OpenCodeConfigPath string
	HermesConfigPath   string
	OpenClawConfigPath string
	ManifestDir        string
	WithProcessCount   bool // populate ScanEntry.ProcessCount via wmic
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
		// Wave-2 opt-in clients — same Lstat-first presence probe as the
		// original seven (the loop body below is client-agnostic).
		{"zed", opts.ZedConfigPath},
		{"kiro", opts.KiroConfigPath},
		{"windsurf", opts.WindsurfConfigPath},
		{"cline", opts.ClineConfigPath},
		{"kilocode", opts.KiloCodeConfigPath},
		{"opencode", opts.OpenCodeConfigPath},
		{"hermes", opts.HermesConfigPath},
		{"openclaw", opts.OpenClawConfigPath},
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
		//   - dangling symlink → "error-symlink" (round 4; init button +
		//     secure-create refusal lose otherwise)
		//   - symlink-to-regular-file in DEFAULT or STRICT mode →
		//     "error-symlink". Post-PR #209 the secure-write pipeline
		//     refuses pre-existing symlinks by default
		//     (`resolveSymlinkForSecureWrite` was removed from
		//     `secureWriteWithOperatorOpt`), so reporting "ok" while a
		//     write would fail with a symlink-refuse error is the UX trap
		//     bot codex-r7 flagged: green column, click Apply, write
		//     fails. NOT unconditional, though — the
		//     OperatorAllowsClientConfigSymlink() branch BELOW reports
		//     "ok" for a symlink-to-regular-file when the operator has
		//     opted in via MCPHUB_ALLOW_CLIENT_CONFIG_SYMLINK on a
		//     non-strict host (under that env, secureWriteWithOperatorOpt
		//     resolves the symlink before writing, so "ok = write will
		//     succeed" still holds). Strict mode
		//     (MCPHUB_REQUIRE_SINGLE_USER_HOME=1) overrides the opt-in and
		//     keeps the refusal — corp-managed hosts stay hardened.
		//
		// The "error-symlink" status is therefore the DEFAULT/STRICT-mode
		// classification, returned DISTINCT from the generic
		// stat/wrong-shape failure ("error") so the Servers matrix can
		// drive an opt-in-aware tooltip (set
		// MCPHUB_ALLOW_CLIENT_CONFIG_SYMLINK on a single-user host, or
		// replace the symlink / edit the target) instead of the
		// misleading "stat error — check permissions and disk health"
		// diagnostic — NOT a claim of unconditional refusal
		// (work-items/bugs/2026-05-19-codex-config-symlink-blocked-by-pr209.md).
		// Only the reported category is split here; the write contract
		// (including the opt-in) is unchanged.
		if lst, lerr := os.Lstat(p.path); lerr == nil {
			isSymlink := lst.Mode()&os.ModeSymlink != 0
			if !lst.Mode().IsRegular() && !isSymlink {
				out[p.name] = "error"
				continue
			}
			if isSymlink {
				// MCPHUB_ALLOW_CLIENT_CONFIG_SYMLINK opt-in (post-PR
				// #209 reintroduction under explicit operator
				// consent): when set, treat a symlink whose target
				// is a regular file as "ok" so the matrix renders
				// the column enabled. Solo-developer dotfile setups
				// (chezmoi / yadm / GNU stow / plain ln -s from
				// ~/.codex/config.toml → /e/env/Agents/...) work
				// again. Strict mode short-circuits to refusal
				// inside OperatorAllowsClientConfigSymlink so
				// corp-managed hosts keep unconditional refusal.
				//
				// Uses os.Stat (kernel-level symlink follow) rather
				// than filepath.EvalSymlinks. EvalSymlinks is Go's
				// string-based resolver and chokes on the
				// POSIX-style target paths Git Bash's `ln -s` can
				// store inside Windows reparse points — the
				// resolver tries to walk the literal path string
				// and fails with ERROR_PATH_NOT_FOUND on segments
				// the kernel would have happily resolved through
				// drive-letter or junction layers. os.Stat goes
				// through the kernel which honors reparse-point
				// semantics regardless of how the stored target
				// string is formatted. A non-regular result
				// (dangling target, points to a directory or
				// special file) still classifies as "error".
				if OperatorAllowsClientConfigSymlink() {
					if rst, rstErr := os.Stat(p.path); rstErr == nil && rst.Mode().IsRegular() {
						out[p.name] = "ok"
						continue
					}
				}
				// Distinct from the generic "error" so the matrix renders
				// a symlink-specific diagnostic (see header comment).
				out[p.name] = "error-symlink"
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
		// "missing-init-possible" / "error" / "error-symlink" all
		// imply that the adapter's `os.ReadFile` would either return
		// IsNotExist (which adapters already absorb to "no entries")
		// or hit the wrong-shape / symlink-refusal failure we want
		// to avoid.
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
	// Wave-2 opt-in clients. Each scanner mirrors the shape its adapter
	// writes (see internal/clients/<client>.go). The HTTP-direct clients
	// reuse the generic per-client shaper; zed uses the relay-shape
	// detector because, like antigravity, its hub entry is a `mcphub
	// relay` stdio invocation rather than a loopback url.
	if opts.ZedConfigPath != "" && scanIfReadable("zed") {
		if err := scanZed(entries, opts.ZedConfigPath); err != nil {
			return nil, fmt.Errorf("zed: %w", err)
		}
	}
	if opts.KiroConfigPath != "" && scanIfReadable("kiro") {
		if err := scanKiro(entries, opts.KiroConfigPath); err != nil {
			return nil, fmt.Errorf("kiro: %w", err)
		}
	}
	if opts.WindsurfConfigPath != "" && scanIfReadable("windsurf") {
		if err := scanWindsurf(entries, opts.WindsurfConfigPath); err != nil {
			return nil, fmt.Errorf("windsurf: %w", err)
		}
	}
	if opts.ClineConfigPath != "" && scanIfReadable("cline") {
		if err := scanCline(entries, opts.ClineConfigPath); err != nil {
			return nil, fmt.Errorf("cline: %w", err)
		}
	}
	if opts.KiloCodeConfigPath != "" && scanIfReadable("kilocode") {
		if err := scanKiloCode(entries, opts.KiloCodeConfigPath); err != nil {
			return nil, fmt.Errorf("kilocode: %w", err)
		}
	}
	if opts.OpenCodeConfigPath != "" && scanIfReadable("opencode") {
		if err := scanOpenCode(entries, opts.OpenCodeConfigPath); err != nil {
			return nil, fmt.Errorf("opencode: %w", err)
		}
	}
	if opts.HermesConfigPath != "" && scanIfReadable("hermes") {
		if err := scanHermes(entries, opts.HermesConfigPath); err != nil {
			return nil, fmt.Errorf("hermes: %w", err)
		}
	}
	if opts.OpenClawConfigPath != "" && scanIfReadable("openclaw") {
		if err := scanOpenClaw(entries, opts.OpenClawConfigPath); err != nil {
			return nil, fmt.Errorf("openclaw: %w", err)
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
	// Pass over manifest names to ensure servers with no client
	// presence still appear in the matrix. Without this, a wholesale
	// demigrate (uncheck-and-Apply on every client column for a
	// server) would make the row vanish entirely — the operator
	// could not re-enable the server from the matrix because the
	// row to click was gone. Empty ClientPresence means every
	// non-disabled column renders as "available" — operator checks
	// the desired cells + Apply to re-install.
	for name := range manifestNames {
		if _, exists := entries[name]; exists {
			continue
		}
		e := &ScanEntry{
			Name:           name,
			ManifestExists: true,
			CanMigrate:     !perSessionServers[name],
			ClientPresence: map[string]ClientEntry{},
		}
		e.Status = classify(e, name, manifestNames)
		entries[name] = e
	}

	// Task 3.3: LSP-bridge recognition. Load the workspace registry as a
	// soft ownership hint (best-effort; nil registry still yields
	// language-aware labels). Cross-references hub HTTP rows with the
	// optional direct-stdio coexistence rows so the matrix shows the
	// anomaly under a single (server, client) cell instead of two
	// disjoint rows the operator has to mentally pair.
	var reg *Registry
	if regPath, perr := DefaultRegistryPath(); perr == nil {
		r := NewRegistry(regPath)
		if lerr := r.Load(); lerr == nil {
			reg = r
		}
	}
	classifyLSPEntries(entries, reg)

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

// ---------------------------------------------------------------------------
// Wave-2 opt-in client scanners (PR #306 adapters → scan/GUI surface).
//
// Each scanner reads the exact config shape its adapter writes
// (internal/clients/<client>.go) and records a per-(server, client)
// ClientEntry under the canonical client id. The shaper for each client
// recognises that client's url key so classify() can tag a loopback hub
// entry as "via-hub". zed is relay-stdio (like antigravity), so it reuses
// the relay-shape detector.
// ---------------------------------------------------------------------------

// scanZed reads Zed's user settings.json. Unlike the JSON family, Zed nests
// its MCP map under the top-level `context_servers` key and writes a
// relay-stdio hub entry (`command`=mcphub binary, args[0]=="relay") — the
// same shape antigravity uses. shapeAntigravityEntry already classifies the
// relay shape, so it is reused verbatim here.
func scanZed(entries map[string]*ScanEntry, path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	var cfg struct {
		ContextServers map[string]map[string]any `json:"context_servers"`
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		return err
	}
	for name, raw := range cfg.ContextServers {
		e := entries[name]
		if e == nil {
			e = &ScanEntry{ClientPresence: map[string]ClientEntry{}}
			entries[name] = e
		}
		e.ClientPresence["zed"] = shapeAntigravityEntry(raw)
	}
	return nil
}

func scanKiro(entries map[string]*ScanEntry, path string) error {
	return scanMCPServersJSON(entries, path, "kiro", shapeURLOrCommandEntry)
}

func scanWindsurf(entries map[string]*ScanEntry, path string) error {
	return scanMCPServersJSON(entries, path, "windsurf", shapeWindsurfEntry)
}

func scanCline(entries map[string]*ScanEntry, path string) error {
	return scanMCPServersJSON(entries, path, "cline", shapeURLOrCommandEntry)
}

func scanKiloCode(entries map[string]*ScanEntry, path string) error {
	return scanMCPServersJSON(entries, path, "kilocode", shapeURLOrCommandEntry)
}

// scanMCPServersJSON is the shared body for the JSON-family clients whose
// MCP map lives under the top-level `mcpServers` key (kiro, windsurf, cline,
// kilocode). The per-client url-shape difference is captured by the shaper
// callback, keeping the read/parse logic in one owner.
func scanMCPServersJSON(entries map[string]*ScanEntry, path, client string, shaper func(map[string]any) ClientEntry) error {
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
		e.ClientPresence[client] = shaper(raw)
	}
	return nil
}

// scanOpenCode reads OpenCode's config. Its MCP map lives under the top-level
// `mcp` key (not `mcpServers`) and a remote hub entry is
// `{"type":"remote","url":...}`. The url key is the standard `url`, so the
// generic url/command shaper recognises the loopback hub entry.
func scanOpenCode(entries map[string]*ScanEntry, path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	var cfg struct {
		MCP map[string]map[string]any `json:"mcp"`
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		return err
	}
	for name, raw := range cfg.MCP {
		e := entries[name]
		if e == nil {
			e = &ScanEntry{ClientPresence: map[string]ClientEntry{}}
			entries[name] = e
		}
		e.ClientPresence["opencode"] = shapeURLOrCommandEntry(raw)
	}
	return nil
}

// scanHermes reads Hermes' ~/.hermes/config.yaml. Its MCP map lives under the
// top-level YAML `mcp_servers` key and a hub entry carries a `url`. Hermes
// reads many unrelated settings from the same file, so only the mcp_servers
// section is consulted.
func scanHermes(entries map[string]*ScanEntry, path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	var cfg struct {
		MCPServers map[string]map[string]any `yaml:"mcp_servers"`
	}
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return err
	}
	for name, raw := range cfg.MCPServers {
		e := entries[name]
		if e == nil {
			e = &ScanEntry{ClientPresence: map[string]ClientEntry{}}
			entries[name] = e
		}
		e.ClientPresence["hermes"] = shapeURLOrCommandEntry(raw)
	}
	return nil
}

// scanOpenClaw reads OpenClaw's config. Its MCP map is NESTED two levels
// under `mcp.servers` (not a single top-level key) and a hub entry carries a
// `url`. The nested object is read defensively so a config missing the `mcp`
// object or its `servers` child simply yields no entries.
func scanOpenClaw(entries map[string]*ScanEntry, path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	var cfg struct {
		MCP struct {
			Servers map[string]map[string]any `json:"servers"`
		} `json:"mcp"`
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		return err
	}
	for name, raw := range cfg.MCP.Servers {
		e := entries[name]
		if e == nil {
			e = &ScanEntry{ClientPresence: map[string]ClientEntry{}}
			entries[name] = e
		}
		e.ClientPresence["openclaw"] = shapeURLOrCommandEntry(raw)
	}
	return nil
}

// shapeURLOrCommandEntry classifies a JSON-family entry whose remote endpoint
// is under the standard `url` key (kiro, cline, kilocode, opencode, hermes,
// openclaw). Mirrors shapeCursorEntry/shapeClaudeEntry: a `url` value →
// http transport; otherwise a `command` value → stdio. classify() then
// recognises a loopback `url` as the hub binding (IsHubHTTPURL).
func shapeURLOrCommandEntry(raw map[string]any) ClientEntry {
	if url, ok := raw["url"].(string); ok {
		return ClientEntry{Transport: "http", Endpoint: url, Raw: raw}
	}
	cmd, _ := raw["command"].(string)
	return ClientEntry{Transport: "stdio", Endpoint: cmd, Raw: raw}
}

// shapeWindsurfEntry classifies a Windsurf entry. Windsurf names the
// remote-HTTP endpoint field `serverUrl` (its adapter writes that key); it
// also accepts the standard `url`. Either loopback value is recognised by
// classify() as the hub binding.
func shapeWindsurfEntry(raw map[string]any) ClientEntry {
	if url, ok := raw["serverUrl"].(string); ok {
		return ClientEntry{Transport: "http", Endpoint: url, Raw: raw}
	}
	if url, ok := raw["url"].(string); ok {
		return ClientEntry{Transport: "http", Endpoint: url, Raw: raw}
	}
	cmd, _ := raw["command"].(string)
	return ClientEntry{Transport: "stdio", Endpoint: cmd, Raw: raw}
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
		// Wave-2 opt-in clients. Each path comes from the same
		// clients.ConfigPathForName resolver the CLI/install side uses, so
		// the scan surface and the write surface agree on the location.
		// mustClientConfigPath returns "" on a resolver error, which
		// ScanFrom skips — identical to the original-seven behavior.
		ZedConfigPath:      mustClientConfigPath("zed"),
		KiroConfigPath:     mustClientConfigPath("kiro"),
		WindsurfConfigPath: mustClientConfigPath("windsurf"),
		ClineConfigPath:    mustClientConfigPath("cline"),
		KiloCodeConfigPath: mustClientConfigPath("kilocode"),
		OpenCodeConfigPath: mustClientConfigPath("opencode"),
		HermesConfigPath:   mustClientConfigPath("hermes"),
		OpenClawConfigPath: mustClientConfigPath("openclaw"),
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

// lspLanguages enumerates the canonical language tokens recognised by
// classifyLSPEntries when reverse-parsing the `mcp-language-server-<lang>`
// entry-name shape produced by ResolveEntryName. Kept as a package-level var
// so future plan items (Task 3.x) can extend it without touching ScanFrom.
// Order does not matter — ParseEntryName uses longest-prefix matching.
var lspLanguages = []string{
	"clangd",
	"fortran",
	"go",
	"javascript",
	"python",
	"rust",
	"typescript",
	"vscode-css",
	"vscode-html",
}

// lspCommandToLanguage reverses the manifest mapping (LanguageSpec.lsp_command
// → LanguageSpec.name) for stdio entries whose entry-name does not itself
// match `mcp-language-server-<lang>`. Required because direct-stdio rows
// often carry operator-chosen names (e.g. "rust-langserver-direct") that lack
// the canonical prefix; the only reliable language signal in that case is the
// `--lsp <binary>` arg + the command basename. typescript-language-server is
// intentionally absent — both javascript and typescript map to it
// (servers/mcp-language-server/manifest.yaml:42,59) so reverse-lookup is
// ambiguous; the algorithm falls back to entry-name parsing for that case
// per the spec's "language is determined by entry NAME" rule.
var lspCommandToLanguage = map[string]string{
	"clangd":                      "clangd",
	"fortls":                      "fortran",
	"gopls":                       "go",
	"pyright-langserver":          "python",
	"rust-analyzer":               "rust",
	"vscode-css-language-server":  "vscode-css",
	"vscode-html-language-server": "vscode-html",
}

// extractLSPLanguageFromArgs inspects a direct-stdio ClientEntry whose
// command basename is mcp-language-server and returns the recognised
// language (or "" when not deducible). The args slice carries the lsp
// binary as `--lsp <bin>`; the bin reverse-maps via lspCommandToLanguage.
// Returns ("") for unambiguous binaries (typescript-language-server)
// since the spec requires those to be resolved from entry-name only.
func extractLSPLanguageFromArgs(raw map[string]any) string {
	args, ok := raw["args"].([]any)
	if !ok {
		return ""
	}
	for i := 0; i < len(args); i++ {
		s, _ := args[i].(string)
		if s != "--lsp" {
			continue
		}
		if i+1 >= len(args) {
			return ""
		}
		bin, _ := args[i+1].(string)
		bin = filepath.Base(strings.TrimSuffix(bin, ".exe"))
		if lang, ok := lspCommandToLanguage[bin]; ok {
			return lang
		}
		return ""
	}
	return ""
}

// classifyLSPEntries is Task 3.3's three-rule recognition pass. For each
// entry produced by the per-client scanners, walk its ClientPresence map
// and tag rows whose name follows the canonical `mcp-language-server-<lang>`
// shape — OR whose stdio invocation reveals the language via `--lsp <bin>`
// reverse-lookup. Three classification rules — all additive on Status;
// coexistence also collapses dangling direct-stdio rows into LegacyConflict
// so the matrix shows the anomaly under a single (server, client) cell
// rather than two visually-disjoint rows.
//
// Rule 1 — hub-managed HTTP entry: transport=http, name matches
// `mcp-language-server-<lang>`. Mark Status = "via-hub".
//
// Rule 2 — direct-stdio mcp-language-server: transport=stdio, command
// basename (suffix-trimmed) equals "mcp-language-server". Recognise as
// legacy. Language comes from entry-name when the name follows the
// canonical shape; otherwise from `--lsp <bin>` args reverse-lookup.
//
// Rule 3 — gopls direct: transport=stdio, command basename "gopls",
// raw args contain "mcp". Recognise as Go legacy.
//
// Coexistence collapse: when a hub row AND a separate legacy stdio
// row both exist for the same language and the same client name,
// MOVE the stdio entry from its own row's ClientPresence to the hub
// row's LegacyConflict[clientName]. If the donor row's ClientPresence
// is empty after the move, prune the row entirely so the matrix
// renders one cell instead of two.
//
// Ownership disambiguation: when the workspace registry is available,
// each row's expected client→entry-name mapping is used as a soft
// sanity gate; the actual recognition labels still come from
// ParseEntryName / args-reverse-lookup so a missing or out-of-date
// registry degrades to language-labelling only.
func classifyLSPEntries(entries map[string]*ScanEntry, reg *Registry) {
	if len(entries) == 0 {
		return
	}

	// Pass 1: classify every row + collect the legacy-stdio donor index.
	// donors maps (clientName, language) -> []donor; we keep slices because
	// multiple stdio rows can legitimately exist (e.g. one canonical
	// mcp-language-server-<lang> stdio row plus an unrelated user row that
	// also happens to call mcp-language-server). Coexistence collapse only
	// fires when the receiving HUB row covers the same (clientName, lang)
	// pair; everything else stays where it is.
	type donor struct {
		entryName  string
		clientName string
		entry      ClientEntry
	}
	// hubsByPair stores a SLICE of matching hub rows because one client
	// can legitimately host multiple hub rows for the same language —
	// e.g. when two workspaces register clangd in codex-cli, each gets
	// its own `mcp-language-server-clangd-<wsKey4hex>` entry. Pre-fix
	// (bot review PR #222 P2 scan.go:1218) the index was a single
	// *ScanEntry, so later iterations overwrote earlier ones and a
	// stdio donor was attached to whichever hub row survived map
	// iteration — nondeterministic. We now attach the donor to EVERY
	// matching hub row so the matrix surfaces the coexistence anomaly
	// on every plausible parent, deterministically.
	hubsByPair := map[string][]*ScanEntry{} // key: clientName + "\x00" + lang
	donorsByPair := map[string][]donor{}    // same key
	legacyEntryNames := map[string]bool{}   // set of entries flagged as legacy donors

	for name, e := range entries {
		nameLang, _ := ParseEntryName(name, lspLanguages)
		for clientName, ce := range e.ClientPresence {
			// Determine the language for this row+client pair. Prefer
			// entry-name parsing (canonical, unambiguous when present);
			// fall back to args reverse-lookup for direct-stdio rows
			// whose names don't follow the canonical shape.
			lang := nameLang
			if lang == "" && ce.Transport == "stdio" {
				base := filepath.Base(strings.TrimSuffix(ce.Endpoint, ".exe"))
				if base == "mcp-language-server" {
					lang = extractLSPLanguageFromArgs(ce.Raw)
				} else if base == "gopls" {
					if args, ok := ce.Raw["args"].([]any); ok {
						for _, a := range args {
							if s, _ := a.(string); s == "mcp" {
								lang = "go"
								break
							}
						}
					}
				}
			}
			if lang == "" {
				continue
			}
			pairKey := clientName + "\x00" + lang
			switch {
			case ce.Transport == "http":
				// Rule 1: hub-managed.
				e.Status = "via-hub"
				hubsByPair[pairKey] = append(hubsByPair[pairKey], e)
			case ce.Transport == "stdio":
				base := filepath.Base(strings.TrimSuffix(ce.Endpoint, ".exe"))
				// Rule 2: direct-stdio mcp-language-server.
				if base == "mcp-language-server" {
					donorsByPair[pairKey] = append(donorsByPair[pairKey], donor{
						entryName: name, clientName: clientName, entry: ce,
					})
					legacyEntryNames[name] = true
					continue
				}
				// Rule 3: gopls stdio with "mcp" arg.
				if lang == "go" && base == "gopls" {
					if args, ok := ce.Raw["args"].([]any); ok {
						for _, a := range args {
							if s, _ := a.(string); s == "mcp" {
								donorsByPair[pairKey] = append(donorsByPair[pairKey], donor{
									entryName: name, clientName: clientName, entry: ce,
								})
								legacyEntryNames[name] = true
								break
							}
						}
					}
				}
			}
		}
	}

	// Registry soft-sanity gate: for ownership disambiguation, walk the
	// workspaces and confirm at least one workspace's ClientEntries maps
	// the receiving hub row's entry name back to the same client. The
	// gate is informational only — production scans without a workspace
	// registry must still produce the recognition labels, so a failed
	// lookup never blocks the coexistence collapse below.
	_ = reg // reserved for future hard-gate evolutions; see plan §"Matrix LSP recognition"

	// Pass 2: coexistence collapse. For each donor matching ANY hub row's
	// (clientName, lang) pair, attach the stdio entry to EVERY matching
	// hub row's LegacyConflict[clientName] and remove the donor entry
	// from its origin row's ClientPresence. Attaching to every matching
	// hub is the deterministic fix for the bot-review PR #222 P2
	// nondeterminism: the donor has no workspace context (its entry
	// name is the bare `mcp-language-server-<lang>` form), so any of
	// the multiple workspace-suffixed hub rows in the same (client, lang)
	// pair could be its parent — surfacing the conflict on all of them
	// keeps the matrix honest about the ambiguity rather than silently
	// picking one based on map iteration order.
	//
	// Hubs within a pair are sorted by entry name so the LegacyConflict
	// assignment order is stable across runs (the value is the same
	// either way; sort only matters if a future refactor depends on
	// iteration order).
	for pairKey, ds := range donorsByPair {
		hubs, ok := hubsByPair[pairKey]
		if !ok || len(hubs) == 0 {
			continue
		}
		sort.Slice(hubs, func(i, j int) bool { return hubs[i].Name < hubs[j].Name })
		// Sort donors deterministically (closes bot PR#222 P2-6: when
		// multiple donors share the same clientName but different
		// entries, the previous code overwrote hub.LegacyConflict[d.clientName]
		// in donorsByPair-map iteration order, hiding all but the last
		// non-deterministically. Sorting by (clientName, entryName)
		// makes the surviving conflict entry reproducible — the
		// alphabetically-last entry for that client always wins.
		//
		// LIMITATION: this preserves ONE conflict entry per client, not
		// ALL of them. Preserving ALL would require changing
		// LegacyConflict to `map[string][]ClientEntry` which is a
		// public-API type change (types.go:103, JSON wire format,
		// downstream consumers). Tracked as a follow-up.
		sort.Slice(ds, func(i, j int) bool {
			if ds[i].clientName != ds[j].clientName {
				return ds[i].clientName < ds[j].clientName
			}
			return ds[i].entryName < ds[j].entryName
		})
		for _, d := range ds {
			for _, hub := range hubs {
				if hub.LegacyConflict == nil {
					hub.LegacyConflict = map[string]ClientEntry{}
				}
				hub.LegacyConflict[d.clientName] = d.entry
			}
			if donor, exists := entries[d.entryName]; exists {
				delete(donor.ClientPresence, d.clientName)
			}
		}
	}

	// Pass 3: prune any donor row whose ClientPresence was emptied by the
	// collapse. We keep rows that retain at least one client (the row may
	// also be present in another client's config), and we never prune a
	// row whose manifest exists on disk — those are matrix-visible by the
	// manifest-only pass in ScanFrom.
	for name := range legacyEntryNames {
		e, ok := entries[name]
		if !ok {
			continue
		}
		if len(e.ClientPresence) > 0 {
			continue
		}
		if e.ManifestExists {
			continue
		}
		delete(entries, name)
	}
}
