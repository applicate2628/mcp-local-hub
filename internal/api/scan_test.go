package api

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// TestScanClassifiesEntries verifies the three key classifications:
// via-hub (HTTP entry pointing at our daemon), can-migrate (stdio entry
// matching one of our manifest names), unknown (stdio entry with no
// manifest), per-session (well-known per-session server like playwright).
func TestScanClassifiesEntries(t *testing.T) {
	tmp := t.TempDir()

	// Fake Claude Code config with 4 entries.
	claudeCfg := map[string]any{
		"mcpServers": map[string]any{
			"memory":     map[string]any{"type": "http", "url": "http://localhost:9123/mcp"},
			"filesystem": map[string]any{"command": "npx", "args": []string{"-y", "@x/filesystem"}},
			"my-thing":   map[string]any{"command": "python", "args": []string{"my.py"}},
			"playwright": map[string]any{"command": "npx", "args": []string{"-y", "@playwright/mcp"}},
		},
	}
	claudePath := filepath.Join(tmp, ".claude.json")
	b, _ := json.Marshal(claudeCfg)
	_ = os.WriteFile(claudePath, b, 0600)

	// Manifest dir with memory + filesystem.
	manifestDir := filepath.Join(tmp, "servers")
	_ = os.MkdirAll(filepath.Join(manifestDir, "memory"), 0755)
	_ = os.WriteFile(filepath.Join(manifestDir, "memory", "manifest.yaml"),
		[]byte("name: memory\nkind: global\ntransport: stdio-bridge\ncommand: npx\ndaemons:\n  - name: default\n    port: 9123\n"), 0644)
	_ = os.MkdirAll(filepath.Join(manifestDir, "filesystem"), 0755)
	_ = os.WriteFile(filepath.Join(manifestDir, "filesystem", "manifest.yaml"),
		[]byte("name: filesystem\nkind: global\ntransport: stdio-bridge\ncommand: npx\ndaemons:\n  - name: default\n    port: 9130\n"), 0644)

	a := NewAPI()
	result, err := a.ScanFrom(ScanOpts{
		ClaudeConfigPath:      claudePath,
		CodexConfigPath:       "",
		GeminiConfigPath:      "",
		AntigravityConfigPath: "",
		ManifestDir:           manifestDir,
	})
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}

	byName := map[string]ScanEntry{}
	for _, e := range result.Entries {
		byName[e.Name] = e
	}

	if got := byName["memory"].Status; got != "via-hub" {
		t.Errorf("memory.Status: got %q, want via-hub", got)
	}
	if got := byName["filesystem"].Status; got != "can-migrate" {
		t.Errorf("filesystem.Status: got %q, want can-migrate", got)
	}
	if got := byName["my-thing"].Status; got != "unknown" {
		t.Errorf("my-thing.Status: got %q, want unknown", got)
	}
	if got := byName["playwright"].Status; got != "per-session" {
		t.Errorf("playwright.Status: got %q, want per-session", got)
	}
}

// TestScanCoversManagedClients seeds Codex TOML plus JSON configs for Gemini,
// Antigravity, Cursor, VS Code, and Qwen with "memory" entries and checks
// each is represented in the ClientPresence map with the correct transport tag.
func TestScanCoversManagedClients(t *testing.T) {
	tmp := t.TempDir()

	// Codex (TOML)
	codexPath := filepath.Join(tmp, "config.toml")
	_ = os.WriteFile(codexPath, []byte(`[mcp_servers.memory]
url = "http://localhost:9123/mcp"
`), 0600)

	// Gemini (JSON w/ mcpServers { url + type: http })
	geminiPath := filepath.Join(tmp, "settings.json")
	_ = os.WriteFile(geminiPath, []byte(`{"mcpServers":{"memory":{"url":"http://localhost:9123/mcp","type":"http"}}}`), 0600)

	// Antigravity — relay (stdio with command=mcphub.exe args=[relay, --server, memory])
	agPath := filepath.Join(tmp, "mcp_config.json")
	_ = os.WriteFile(agPath, []byte(`{"mcpServers":{"memory":{"command":"D:/dev/mcphub.exe","args":["relay","--server","memory","--daemon","default"],"disabled":false}}}`), 0600)

	cursorPath := filepath.Join(tmp, "cursor-mcp.json")
	_ = os.WriteFile(cursorPath, []byte(`{"mcpServers":{"memory":{"url":"http://localhost:9123/mcp","type":"http"}}}`), 0600)

	vscodePath := filepath.Join(tmp, "vscode-mcp.json")
	_ = os.WriteFile(vscodePath, []byte(`{"servers":{"memory":{"url":"http://localhost:9123/mcp","type":"http"}}}`), 0600)

	qwenPath := filepath.Join(tmp, "qwen-settings.json")
	_ = os.WriteFile(qwenPath, []byte(`{"mcpServers":{"memory":{"httpUrl":"http://localhost:9123/mcp","timeout":10000}}}`), 0600)

	a := NewAPI()
	result, err := a.ScanFrom(ScanOpts{
		CodexConfigPath:       codexPath,
		GeminiConfigPath:      geminiPath,
		AntigravityConfigPath: agPath,
		CursorConfigPath:      cursorPath,
		VSCodeConfigPath:      vscodePath,
		QwenConfigPath:        qwenPath,
		ManifestDir:           t.TempDir(),
	})
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	var memEntry *ScanEntry
	for i := range result.Entries {
		if result.Entries[i].Name == "memory" {
			memEntry = &result.Entries[i]
		}
	}
	if memEntry == nil {
		t.Fatal("no memory entry found")
	}
	if got := memEntry.ClientPresence["codex-cli"].Transport; got != "http" {
		t.Errorf("codex-cli.Transport: got %q, want http", got)
	}
	if got := memEntry.ClientPresence["gemini-cli"].Transport; got != "http" {
		t.Errorf("gemini-cli.Transport: got %q, want http", got)
	}
	if got := memEntry.ClientPresence["antigravity"].Transport; got != "relay" {
		t.Errorf("antigravity.Transport: got %q, want relay", got)
	}
	if got := memEntry.ClientPresence["cursor"].Transport; got != "http" {
		t.Errorf("cursor.Transport: got %q, want http", got)
	}
	if got := memEntry.ClientPresence["vscode"].Transport; got != "http" {
		t.Errorf("vscode.Transport: got %q, want http", got)
	}
	if got := memEntry.ClientPresence["qwen-cli"].Transport; got != "http" {
		t.Errorf("qwen-cli.Transport: got %q, want http", got)
	}
}

// TestScanWithProcessCountPopulates verifies ScanFrom populates ProcessCount
// when WithProcessCount is true. We don't assert an exact number (test runs
// on real host), just that the field is either zero or positive and that the
// call doesn't error.
func TestScanWithProcessCountPopulates(t *testing.T) {
	tmp := t.TempDir()
	_ = os.WriteFile(filepath.Join(tmp, ".claude.json"),
		[]byte(`{"mcpServers":{"memory":{"type":"http","url":"http://localhost:9123/mcp"}}}`), 0600)
	_ = os.MkdirAll(filepath.Join(tmp, "servers", "memory"), 0755)
	_ = os.WriteFile(filepath.Join(tmp, "servers", "memory", "manifest.yaml"),
		[]byte("name: memory\nkind: global\ntransport: stdio-bridge\ncommand: npx\nbase_args:\n  - \"-y\"\n  - \"@modelcontextprotocol/server-memory\"\ndaemons:\n  - name: default\n    port: 9123\n"), 0644)

	a := NewAPI()
	result, err := a.ScanFrom(ScanOpts{
		ClaudeConfigPath: filepath.Join(tmp, ".claude.json"),
		ManifestDir:      filepath.Join(tmp, "servers"),
		WithProcessCount: true,
	})
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	found := false
	for _, e := range result.Entries {
		if e.Name == "memory" {
			found = true
			if e.ProcessCount < 0 {
				t.Errorf("ProcessCount must be non-negative, got %d", e.ProcessCount)
			}
		}
	}
	if !found {
		t.Error("memory entry missing from scan result")
	}
}

// TestClassify exercises the server-status classifier against the five
// possible outcomes (per-session, via-hub, can-migrate, unknown,
// not-installed) and verifies the PR #4 Codex R1 fix: relay transport
// AND 127.0.0.1 endpoints are both recognised as hub-routed.
func TestClassify(t *testing.T) {
	manifests := map[string]bool{"serena": true}
	cases := []struct {
		name       string
		entry      *ScanEntry
		serverName string
		want       string
	}{
		{
			name:       "per-session takes precedence",
			entry:      &ScanEntry{ClientPresence: map[string]ClientEntry{"x": {Transport: "http", Endpoint: "http://localhost:9100/mcp"}}},
			serverName: firstPerSessionServer(t),
			want:       "per-session",
		},
		{
			name:       "http + localhost -> via-hub",
			entry:      &ScanEntry{ClientPresence: map[string]ClientEntry{"claude-code": {Transport: "http", Endpoint: "http://localhost:9200/mcp"}}},
			serverName: "memory",
			want:       "via-hub",
		},
		{
			name:       "http + 127.0.0.1 -> via-hub (Codex R1)",
			entry:      &ScanEntry{ClientPresence: map[string]ClientEntry{"claude-code": {Transport: "http", Endpoint: "http://127.0.0.1:9200/mcp"}}},
			serverName: "memory",
			want:       "via-hub",
		},
		{
			name:       "relay transport -> via-hub (Codex R1)",
			entry:      &ScanEntry{ClientPresence: map[string]ClientEntry{"antigravity": {Transport: "relay", Endpoint: "mcphub.exe"}}},
			serverName: "memory",
			want:       "via-hub",
		},
		{
			name:       "stdio + manifest -> can-migrate",
			entry:      &ScanEntry{ClientPresence: map[string]ClientEntry{"claude-code": {Transport: "stdio", Endpoint: "npx"}}},
			serverName: "serena",
			want:       "can-migrate",
		},
		{
			name:       "stdio without manifest -> unknown",
			entry:      &ScanEntry{ClientPresence: map[string]ClientEntry{"claude-code": {Transport: "stdio", Endpoint: "npx"}}},
			serverName: "random-server",
			want:       "unknown",
		},
		{
			name:       "no known transport -> not-installed",
			entry:      &ScanEntry{ClientPresence: map[string]ClientEntry{"claude-code": {Transport: "absent"}}},
			serverName: "memory",
			want:       "not-installed",
		},
		{
			name:       "mixed relay + stdio with manifest -> can-migrate (hasStdio wins over hub)",
			entry:      &ScanEntry{ClientPresence: map[string]ClientEntry{"antigravity": {Transport: "relay", Endpoint: "mcphub.exe"}, "claude-code": {Transport: "stdio", Endpoint: "npx"}}},
			serverName: "serena",
			want:       "can-migrate",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := classify(tc.entry, tc.serverName, manifests)
			if got != tc.want {
				t.Fatalf("classify(%q) = %q, want %q", tc.serverName, got, tc.want)
			}
		})
	}
}

// firstPerSessionServer returns a deterministic name known to live in
// perSessionServers, so TestClassify does not hardcode a value that
// could later rotate as that list is edited.
func firstPerSessionServer(t *testing.T) string {
	t.Helper()
	for name := range perSessionServers {
		return name
	}
	t.Fatalf("perSessionServers is empty; TestClassify expected at least one entry")
	return ""
}

// TestProbeClientConfigPresence_StateMachine pins the three-way
// classification probeClientConfigPresence emits in v0.4.5+:
//   - "ok"                     : file present on disk
//   - "missing-init-possible"  : file absent but parent directory
//                                exists (operator has the client
//                                installed; GUI Initialize button is
//                                offered)
//   - "missing"                : neither file nor parent dir exists
//                                (client genuinely not installed)
//
// The frontend gates the per-column "Initialize" affordance on
// state == "missing-init-possible", so this contract is part of the
// UI's behavior.
func TestProbeClientConfigPresence_StateMachine(t *testing.T) {
	tmp := t.TempDir()

	// vscode: parent dir exists, file absent → missing-init-possible.
	vscodeParent := filepath.Join(tmp, "Code", "User")
	if err := os.MkdirAll(vscodeParent, 0o755); err != nil {
		t.Fatalf("mkdir vscode parent: %v", err)
	}
	vscodePath := filepath.Join(vscodeParent, "mcp.json")

	// cursor: parent dir DOES NOT exist → missing (genuine "not installed").
	cursorPath := filepath.Join(tmp, "no-such-dir", "mcp.json")

	// claude-code: file present → ok.
	claudePath := filepath.Join(tmp, ".claude.json")
	if err := os.WriteFile(claudePath, []byte(`{"mcpServers":{}}`), 0o600); err != nil {
		t.Fatalf("write claude.json: %v", err)
	}

	// codex-cli: empty (not passed) — must be omitted from the result map.
	out := probeClientConfigPresence(ScanOpts{
		VSCodeConfigPath:  vscodePath,
		CursorConfigPath:  cursorPath,
		ClaudeConfigPath:  claudePath,
		// CodexConfigPath intentionally omitted.
	})

	if got := out["vscode"]; got != "missing-init-possible" {
		t.Errorf("vscode (parent dir present, file absent) = %q, want missing-init-possible", got)
	}
	if got := out["cursor"]; got != "missing" {
		t.Errorf("cursor (parent dir absent) = %q, want missing", got)
	}
	if got := out["claude-code"]; got != "ok" {
		t.Errorf("claude-code (file present) = %q, want ok", got)
	}
	if _, ok := out["codex-cli"]; ok {
		t.Errorf("codex-cli should not appear in result when path is empty; got %v", out["codex-cli"])
	}
}

// TestProbeClientConfigPresence_DanglingSymlink pins the v0.4.5
// deep-sec PR #208 Lane B round 4 P2 closure: a dangling symlink at
// the config path must surface as "error", not "missing-init-possible".
// Previously os.Stat followed the symlink, returned IsNotExist
// (target absent), and classifyMissingClientConfig saw the parent
// dir exists → "missing-init-possible" → GUI offered Initialize →
// secure-create refused → 500 INIT_FAILED. The Lstat-first probe
// now classifies symlinks (dangling or not) as "error" so the
// matrix renders the config-error diagnostic instead.
func TestProbeClientConfigPresence_DanglingSymlink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation requires elevation on Windows; the cross-platform Lstat probe is exercised by the POSIX path")
	}
	tmp := t.TempDir()
	target := filepath.Join(tmp, "no-such-target.dat")
	link := filepath.Join(tmp, "mcp.json")
	if err := os.Symlink(target, link); err != nil {
		t.Fatalf("create dangling symlink: %v", err)
	}

	out := probeClientConfigPresence(ScanOpts{VSCodeConfigPath: link})
	if got := out["vscode"]; got != "error" {
		t.Errorf("dangling symlink at config path classified as %q, want \"error\"", got)
	}
}

// TestProbeClientConfigPresence_DirectoryAtConfigPath pins the v0.4.5
// deep-sec PR #208 Lane B round 5 P2 closure: a directory at the
// config path must surface as "error", not "ok". Previously the
// Lstat probe only rejected symlinks; a directory passed through to
// os.Stat which succeeded and the cell was classified as "ok" —
// migrate/backup would then fail later when adapter.readJSON tried
// to read a directory.
func TestProbeClientConfigPresence_DirectoryAtConfigPath(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, "mcp.json")
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatalf("seed directory at config path: %v", err)
	}

	out := probeClientConfigPresence(ScanOpts{VSCodeConfigPath: path})
	if got := out["vscode"]; got != "error" {
		t.Errorf("directory at config path classified as %q, want \"error\"", got)
	}
}

// TestProbeClientConfigPresence_SymlinkToRegularDefaultMode pins
// the post-PR-#209 contract: symlink-to-existing-regular-file in
// DEFAULT mode classifies as "error" because PR #209 removed
// `resolveSymlinkForSecureWrite` from `secureWriteWithOperatorOpt` —
// writes through `SecureWriteClientConfig` now refuse pre-existing
// symlinks in ALL modes. Reporting "ok" while writes deterministically
// fail with symlink-refuse errors is the exact UX trap codex bot r7
// flagged (PR #208 review on commit de3ba74): user sees a green
// matrix column, clicks Apply, every write fails. Aligning presence
// with the active write contract restores invariant "ok = write will
// succeed".
//
// Trade-off: dotfile-symlink setups (e.g., `~/.codex/config.toml
// -> E:\dotfiles\.codex\config.toml`) that used to rely on default-
// mode resolve-to-target are now unsupported by design. The security
// boundary closure in PR #209 (confused-deputy integrity protection)
// took precedence over the dotfile UX. Operators with this pattern
// can either remove the symlink or manage the target file directly.
func TestProbeClientConfigPresence_SymlinkToRegularDefaultMode(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation requires elevation on Windows; the cross-platform Lstat probe is exercised by the POSIX path")
	}
	// Ensure strict mode is OFF for this test.
	t.Setenv(RequireSingleUserHomeEnv, "")
	tmp := t.TempDir()
	target := filepath.Join(tmp, "real-config.json")
	if err := os.WriteFile(target, []byte(`{"servers": {}}`), 0o600); err != nil {
		t.Fatalf("seed target: %v", err)
	}
	link := filepath.Join(tmp, "mcp.json")
	if err := os.Symlink(target, link); err != nil {
		t.Fatalf("create symlink: %v", err)
	}

	out := probeClientConfigPresence(ScanOpts{VSCodeConfigPath: link})
	if got := out["vscode"]; got != "error" {
		t.Errorf("symlink-to-regular in default mode classified as %q, want \"error\" (write pipeline refuses symlinks post-PR-#209)", got)
	}
}

// TestProbeClientConfigPresence_SymlinkToRegularOptInMode pins the
// MCPHUB_ALLOW_CLIENT_CONFIG_SYMLINK opt-in (introduced as the
// solo-developer dotfile-symlink unblock for the Codex CLI case
// documented in work-items/bugs/2026-05-19-codex-config-symlink-blocked-by-pr209.md).
// With the env var set to "1", a symlink whose target is a regular
// file classifies as "ok" so the Servers matrix renders the column
// enabled; the secure-write pipeline (secureWriteWithOperatorOpt)
// resolves the symlink to its target before calling the hardened
// writer, so writes succeed against the real file. Strict-mode
// (MCPHUB_REQUIRE_SINGLE_USER_HOME=1) overrides the opt-in inside
// OperatorAllowsClientConfigSymlink; the next test covers that.
func TestProbeClientConfigPresence_SymlinkToRegularOptInMode(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation requires elevation on Windows; the cross-platform Lstat probe is exercised by the POSIX path")
	}
	t.Setenv(RequireSingleUserHomeEnv, "")
	t.Setenv(AllowClientConfigSymlinkEnv, "1")
	tmp := t.TempDir()
	target := filepath.Join(tmp, "real-config.json")
	if err := os.WriteFile(target, []byte(`{"servers": {}}`), 0o600); err != nil {
		t.Fatalf("seed target: %v", err)
	}
	link := filepath.Join(tmp, "mcp.json")
	if err := os.Symlink(target, link); err != nil {
		t.Fatalf("create symlink: %v", err)
	}

	out := probeClientConfigPresence(ScanOpts{VSCodeConfigPath: link})
	if got := out["vscode"]; got != "ok" {
		t.Errorf("symlink-to-regular with opt-in env classified as %q, want \"ok\"", got)
	}
}

// TestProbeClientConfigPresence_SymlinkOptInStrictModeOverride pins
// the strict-mode invariant: even when
// MCPHUB_ALLOW_CLIENT_CONFIG_SYMLINK=1 is set, if
// MCPHUB_REQUIRE_SINGLE_USER_HOME=1 is ALSO set then symlinks are
// refused. This keeps the multi-tenant / corp-managed posture's
// no-symlink invariant regardless of per-operator opt-ins; matches
// the design intent documented at AllowClientConfigSymlinkEnv.
func TestProbeClientConfigPresence_SymlinkOptInStrictModeOverride(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation requires elevation on Windows; the cross-platform Lstat probe is exercised by the POSIX path")
	}
	t.Setenv(RequireSingleUserHomeEnv, "1")
	t.Setenv(AllowClientConfigSymlinkEnv, "1")
	tmp := t.TempDir()
	target := filepath.Join(tmp, "real-config.json")
	if err := os.WriteFile(target, []byte(`{"servers": {}}`), 0o600); err != nil {
		t.Fatalf("seed target: %v", err)
	}
	link := filepath.Join(tmp, "mcp.json")
	if err := os.Symlink(target, link); err != nil {
		t.Fatalf("create symlink: %v", err)
	}

	out := probeClientConfigPresence(ScanOpts{VSCodeConfigPath: link})
	if got := out["vscode"]; got != "error" {
		t.Errorf("symlink with opt-in env BUT strict mode set classified as %q, want \"error\" (strict overrides opt-in)", got)
	}
}

// TestProbeClientConfigPresence_DanglingSymlinkOptInStillError pins
// that the opt-in does NOT classify a dangling symlink as "ok". The
// resolve uses os.Stat on the EvalSymlinks result and requires the
// target to be a regular file; a dangling target fails both checks
// and falls through to "error" so the operator sees the breakage.
func TestProbeClientConfigPresence_DanglingSymlinkOptInStillError(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation requires elevation on Windows; the cross-platform Lstat probe is exercised by the POSIX path")
	}
	t.Setenv(RequireSingleUserHomeEnv, "")
	t.Setenv(AllowClientConfigSymlinkEnv, "1")
	tmp := t.TempDir()
	link := filepath.Join(tmp, "mcp.json")
	if err := os.Symlink(filepath.Join(tmp, "does-not-exist.json"), link); err != nil {
		t.Fatalf("create dangling symlink: %v", err)
	}

	out := probeClientConfigPresence(ScanOpts{VSCodeConfigPath: link})
	if got := out["vscode"]; got != "error" {
		t.Errorf("dangling symlink with opt-in env classified as %q, want \"error\" (target must be regular file)", got)
	}
}

// TestProbeClientConfigPresence_SymlinkToRegularStrictMode pins the
// strict-mode contract: even if the symlink target is a regular
// file, strict mode refuses any symlink because the secure-write
// pipeline refuses to follow symlinks under
// MCPHUB_REQUIRE_SINGLE_USER_HOME=1.
func TestProbeClientConfigPresence_SymlinkToRegularStrictMode(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation requires elevation on Windows")
	}
	t.Setenv(RequireSingleUserHomeEnv, "1")
	tmp := t.TempDir()
	target := filepath.Join(tmp, "real-config.json")
	if err := os.WriteFile(target, []byte(`{"servers": {}}`), 0o600); err != nil {
		t.Fatalf("seed target: %v", err)
	}
	link := filepath.Join(tmp, "mcp.json")
	if err := os.Symlink(target, link); err != nil {
		t.Fatalf("create symlink: %v", err)
	}

	out := probeClientConfigPresence(ScanOpts{VSCodeConfigPath: link})
	if got := out["vscode"]; got != "error" {
		t.Errorf("symlink-to-regular in strict mode classified as %q, want \"error\"", got)
	}
}

// TestScanFrom_DirectoryAtConfigPathDoesNotFailWholeScan pins the
// v0.4.5 deep-sec PR #208 Lane B round 6 P2 #2 closure: a directory
// at one client's config path must NOT propagate as a whole-scan
// 500 SCAN_FAILED. The presence-first ordering in ScanFrom skips
// adapter reads for non-"ok" clients so the per-client diagnostic
// (client_config_presence["vscode"] == "error") reaches the frontend.
func TestScanFrom_DirectoryAtConfigPathDoesNotFailWholeScan(t *testing.T) {
	tmp := t.TempDir()
	// Seed a directory at vscode's config path.
	vscodeBogus := filepath.Join(tmp, "vscode-mcp-as-dir")
	if err := os.MkdirAll(vscodeBogus, 0o755); err != nil {
		t.Fatalf("seed dir: %v", err)
	}
	// And a valid claude config alongside, to verify the rest of
	// the scan continues.
	claudePath := filepath.Join(tmp, ".claude.json")
	if err := os.WriteFile(claudePath, []byte(`{"mcpServers":{"memory":{"type":"http","url":"http://localhost:9123/mcp"}}}`), 0o600); err != nil {
		t.Fatalf("seed claude: %v", err)
	}

	a := NewAPI()
	result, err := a.ScanFrom(ScanOpts{
		VSCodeConfigPath: vscodeBogus,
		ClaudeConfigPath: claudePath,
		ManifestDir:      t.TempDir(),
	})
	if err != nil {
		t.Fatalf("ScanFrom returned error on directory-at-config-path: %v (expected partial success)", err)
	}
	if got := result.ClientConfigPresence["vscode"]; got != "error" {
		t.Errorf("client_config_presence[vscode]=%q, want \"error\"", got)
	}
	// Claude scan still ran and produced an entry for memory.
	foundMemory := false
	for _, e := range result.Entries {
		if e.Name == "memory" {
			foundMemory = true
		}
	}
	if !foundMemory {
		t.Errorf("memory entry from claude scan missing; partial scan should still surface valid clients")
	}
}

// TestClassifyMissingClientConfig pins the helper in isolation. It is
// the canonical place to extend if v0.5.x adds further classification
// (e.g., "parent-is-symlink" or "parent-not-writable").
func TestClassifyMissingClientConfig(t *testing.T) {
	tmp := t.TempDir()

	// Case: parent dir is a directory.
	if got := classifyMissingClientConfig(filepath.Join(tmp, "mcp.json")); got != "missing-init-possible" {
		t.Errorf("file in existing dir: got %q, want missing-init-possible", got)
	}

	// Case: parent does not exist.
	if got := classifyMissingClientConfig(filepath.Join(tmp, "nope", "mcp.json")); got != "missing" {
		t.Errorf("file in non-existent parent: got %q, want missing", got)
	}

	// Case: parent path points at a file (not a directory).
	regularFile := filepath.Join(tmp, "regular")
	if err := os.WriteFile(regularFile, []byte("not a dir"), 0o600); err != nil {
		t.Fatalf("seed regular file: %v", err)
	}
	if got := classifyMissingClientConfig(filepath.Join(regularFile, "mcp.json")); got != "missing" {
		t.Errorf("parent-is-file: got %q, want missing", got)
	}
}

// TestClassifyMissingClientConfig_SymlinkedParent pins the v0.4.5
// PR #208 codex r1 F2 closure: when the config file's parent is a
// symlink (dotfile-management pattern: ~/.config/Claude/ symlinked to
// a real dotfile repo), classify must return
// "missing-init-blocked-symlink" so the GUI suppresses the Initialize
// affordance. The hardened init pipeline refuses to follow parent
// symlinks (POSIX O_NOFOLLOW; Windows FILE_FLAG_OPEN_REPARSE_POINT
// without resolution), so a button click would deterministically
// fail with INIT_FAILED — broken UX.
//
// Pre-fix: os.Stat(parent) followed the symlink, returned IsDir=true
// for the resolved target, classify reported "missing-init-possible",
// GUI offered Initialize, click failed.
//
// Post-fix: os.Lstat(parent) detects the symlink directly,
// classify reports the blocked-symlink state.
func TestClassifyMissingClientConfig_SymlinkedParent(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation requires elevation on Windows; the cross-platform Lstat probe is exercised by the POSIX path")
	}
	tmp := t.TempDir()
	// Real target dir (where dotfiles actually live).
	target := filepath.Join(tmp, "real-dotfiles", "Claude")
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatalf("mkdir target: %v", err)
	}
	// Symlinked parent (where the client expects its config dir).
	link := filepath.Join(tmp, "Claude")
	if err := os.Symlink(target, link); err != nil {
		t.Fatalf("create symlink: %v", err)
	}
	got := classifyMissingClientConfig(filepath.Join(link, "claude_desktop_config.json"))
	if got != "missing-init-blocked-symlink" {
		t.Errorf("symlinked parent: got %q, want missing-init-blocked-symlink", got)
	}
}

// TestProbeClientConfigPresence_SymlinkedParent verifies the new
// state surfaces end-to-end through probeClientConfigPresence (the
// production caller of classifyMissingClientConfig). Companion test
// to TestClassifyMissingClientConfig_SymlinkedParent that exercises
// the same case through the full presence map.
func TestProbeClientConfigPresence_SymlinkedParent(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation requires elevation on Windows")
	}
	tmp := t.TempDir()
	target := filepath.Join(tmp, "real-dotfiles", "Code", "User")
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatalf("mkdir target: %v", err)
	}
	link := filepath.Join(tmp, "Code", "User")
	if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
		t.Fatalf("mkdir link parent: %v", err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Fatalf("create symlink: %v", err)
	}
	vscodePath := filepath.Join(link, "mcp.json")
	out := probeClientConfigPresence(ScanOpts{VSCodeConfigPath: vscodePath})
	if got := out["vscode"]; got != "missing-init-blocked-symlink" {
		t.Errorf("vscode with symlinked parent: got %q, want missing-init-blocked-symlink", got)
	}
}
