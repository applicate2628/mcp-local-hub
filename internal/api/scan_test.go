package api

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	toml "github.com/pelletier/go-toml/v2"
)

// TestReadManifestNames_ExcludesCompanion is the companion source-filter guard:
// a kind=companion manifest is a hub-managed NON-MCP process and must never enter
// the scan name set (which feeds classify / the Servers matrix / via-hub
// detection), while a normal kind=global manifest stays.
func TestReadManifestNames_ExcludesCompanion(t *testing.T) {
	dir := t.TempDir()
	write := func(name, yaml string) {
		md := filepath.Join(dir, name)
		if err := os.MkdirAll(md, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(md, "manifest.yaml"), []byte(yaml), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	absCwd := "/opt/canvas"
	if runtime.GOOS == "windows" {
		absCwd = "C:/opt/canvas"
	}
	write("excalidraw-canvas", "name: excalidraw-canvas\nkind: companion\ntransport: process\ncommand: node\n"+
		"base_args: [dist/server.js]\ndaemons:\n  - name: default\n    cwd: \""+absCwd+"\"\n")
	write("memory", "name: memory\nkind: global\ntransport: stdio-bridge\ncommand: npx\n"+
		"daemons:\n  - name: default\n    port: 9128\n")

	names, err := readManifestNames(dir)
	if err != nil {
		t.Fatal(err)
	}
	if names["excalidraw-canvas"] {
		t.Error("kind=companion manifest must be EXCLUDED from the scan name set (source-filter)")
	}
	if !names["memory"] {
		t.Error("kind=global manifest must remain in the scan name set")
	}
}

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

// TestScanExternalAndManagedFlag pins the Discovery-view contract end to
// end through ScanFrom:
//   - a client-present non-hub remote HTTP entry classifies as "external"
//     (NOT "not-installed") with Managed=false, so it surfaces instead of
//     vanishing;
//   - a hub-routed entry carries Managed=true;
//   - a manifest-only server with zero client presence stays "not-installed"
//     with Managed=false (the matrix-row-must-not-vanish row).
func TestScanExternalAndManagedFlag(t *testing.T) {
	// Redirect live-state lookups (DefaultRegistryPath consults LOCALAPPDATA
	// first) at a temp dir so the test never reads or mutates the operator's
	// real registry/state. The registry Load is best-effort, but redirect
	// it anyway per the live-state-safety discipline.
	t.Setenv("LOCALAPPDATA", t.TempDir())
	t.Setenv("MCPHUB_STATE_DIR_OVERRIDE", t.TempDir())

	tmp := t.TempDir()

	claudeCfg := map[string]any{
		"mcpServers": map[string]any{
			// Hub-routed → via-hub, Managed=true.
			"memory": map[string]any{"type": "http", "url": "http://localhost:9123/mcp"},
			// Real external remote (non-hub http) → external, Managed=false.
			"context7": map[string]any{"type": "http", "url": "https://mcp.context7.com/mcp"},
			// Second external remote in a different client below.
		},
	}
	claudePath := filepath.Join(tmp, ".claude.json")
	b, _ := json.Marshal(claudeCfg)
	if err := os.WriteFile(claudePath, b, 0600); err != nil {
		t.Fatalf("write claude cfg: %v", err)
	}

	cursorCfg := map[string]any{
		"mcpServers": map[string]any{
			// Same external remote, different client — still external.
			"qt-docs": map[string]any{"url": "https://qt.io/mcp"},
		},
	}
	cursorPath := filepath.Join(tmp, ".cursor-mcp.json")
	cb, _ := json.Marshal(cursorCfg)
	if err := os.WriteFile(cursorPath, cb, 0600); err != nil {
		t.Fatalf("write cursor cfg: %v", err)
	}

	// Manifest dir: memory (manifest-known) + a "ghost" manifest with no
	// client presence (must stay not-installed, not vanish).
	manifestDir := filepath.Join(tmp, "servers")
	_ = os.MkdirAll(filepath.Join(manifestDir, "memory"), 0755)
	_ = os.WriteFile(filepath.Join(manifestDir, "memory", "manifest.yaml"),
		[]byte("name: memory\nkind: global\ntransport: stdio-bridge\ncommand: npx\ndaemons:\n  - name: default\n    port: 9123\n"), 0644)
	_ = os.MkdirAll(filepath.Join(manifestDir, "ghost"), 0755)
	_ = os.WriteFile(filepath.Join(manifestDir, "ghost", "manifest.yaml"),
		[]byte("name: ghost\nkind: global\ntransport: stdio-bridge\ncommand: npx\ndaemons:\n  - name: default\n    port: 9131\n"), 0644)

	a := NewAPI()
	result, err := a.ScanFrom(ScanOpts{
		ClaudeConfigPath: claudePath,
		CursorConfigPath: cursorPath,
		ManifestDir:      manifestDir,
	})
	if err != nil {
		t.Fatalf("ScanFrom: %v", err)
	}

	byName := map[string]ScanEntry{}
	for _, e := range result.Entries {
		byName[e.Name] = e
	}

	// memory → via-hub, Managed=true.
	if got := byName["memory"].Status; got != "via-hub" {
		t.Errorf("memory.Status: got %q, want via-hub", got)
	}
	if !byName["memory"].Managed {
		t.Errorf("memory.Managed: got false, want true (hub-routed)")
	}

	// context7 → external, Managed=false. Must NOT be not-installed (the bug).
	c7, ok := byName["context7"]
	if !ok {
		t.Fatalf("context7 missing from scan entirely (the vanishing bug): %+v", byName)
	}
	if c7.Status != "external" {
		t.Errorf("context7.Status: got %q, want external", c7.Status)
	}
	if c7.Managed {
		t.Errorf("context7.Managed: got true, want false (unmanaged external remote)")
	}

	// qt-docs (cursor) → external, Managed=false.
	qt, ok := byName["qt-docs"]
	if !ok {
		t.Fatalf("qt-docs missing from scan (the vanishing bug)")
	}
	if qt.Status != "external" {
		t.Errorf("qt-docs.Status: got %q, want external", qt.Status)
	}
	if qt.Managed {
		t.Errorf("qt-docs.Managed: got true, want false")
	}

	// ghost (manifest-only, no presence) → not-installed, Managed=false.
	ghost, ok := byName["ghost"]
	if !ok {
		t.Fatalf("ghost (manifest-only) missing — matrix-row-must-not-vanish broke")
	}
	if ghost.Status != "not-installed" {
		t.Errorf("ghost.Status: got %q, want not-installed (manifest-only, no presence)", ghost.Status)
	}
	if ghost.Managed {
		t.Errorf("ghost.Managed: got true, want false")
	}
}

// TestResolveManifestDaemonPort_EmbedFirst pins the port-lookup helper
// the supervisor status seam uses to enrich Port=0 supervisor-intent
// rows. PR #211 and earlier wrote supervisor-intent.json with Port=0
// for every daemon (migration did not seed the field from the
// manifest); the GUI matrix renders "—" for those daemons even though
// the daemon is listening on the manifest-declared port.
// ResolveManifestDaemonPort is the read-time enrichment that surfaces
// the correct port without requiring an intent-file migration.
//
// The empty manifestDir parameter forces the embedded-first lookup
// (production path). For one of the shipped mcphub manifests, port
// must be returned verbatim. (Shipped manifests do not change ports
// at runtime — this is a stable contract.)
func TestResolveManifestDaemonPort_EmbedFirst(t *testing.T) {
	// time is one of the global mcphub manifests with a stable port
	// (9128) declared at servers/time/manifest.yaml. The embed loader
	// returns the same yaml the supervisor reads, so the manifest is
	// guaranteed to exist in the embedded FS.
	port, ok := ResolveManifestDaemonPort("time", "default")
	if !ok {
		t.Fatalf("ResolveManifestDaemonPort(time, default) returned !ok; want a port")
	}
	if port != 9128 {
		t.Errorf("port: got %d, want 9128 (shipped manifest)", port)
	}
}

// TestResolveManifestDaemonPort_UnknownReturnsZeroFalse pins the
// fail-safe contract — callers can treat (0, false) as "not
// authoritative" without crashing. Used by the supervisor status seam
// to fall back to the intent-stored Port=0 when no manifest exists
// (e.g. a hand-edited supervisor-intent.json with an unknown server).
func TestResolveManifestDaemonPort_UnknownReturnsZeroFalse(t *testing.T) {
	port, ok := ResolveManifestDaemonPort("does-not-exist", "default")
	if ok {
		t.Errorf("expected !ok for unknown server; got port=%d", port)
	}
	if port != 0 {
		t.Errorf("port: got %d, want 0", port)
	}
}

// TestScanIncludesManifestOnlyServers pins the visibility fix on the
// matrix-row-vanishes UX bug. Before the fix, ScanFrom assembled its
// entries map purely from per-client scan output — so a server whose
// manifest existed on disk but had been demigrated from every client
// would disappear from the matrix entirely, leaving the operator with
// no row to click to re-enable it. After the fix, every manifest-known
// server appears as a row with empty ClientPresence so the matrix
// renders an "available" cell per non-disabled client column.
func TestScanIncludesManifestOnlyServers(t *testing.T) {
	tmp := t.TempDir()

	// Empty Claude config — no mcpServers at all.
	claudeCfg := map[string]any{"mcpServers": map[string]any{}}
	claudePath := filepath.Join(tmp, ".claude.json")
	b, _ := json.Marshal(claudeCfg)
	_ = os.WriteFile(claudePath, b, 0600)

	// Manifest dir with one server.
	manifestDir := filepath.Join(tmp, "servers")
	_ = os.MkdirAll(filepath.Join(manifestDir, "lonely-server"), 0755)
	_ = os.WriteFile(filepath.Join(manifestDir, "lonely-server", "manifest.yaml"),
		[]byte("name: lonely-server\nkind: global\ntransport: stdio-bridge\ncommand: npx\ndaemons:\n  - name: default\n    port: 9999\n"), 0644)

	a := NewAPI()
	result, err := a.ScanFrom(ScanOpts{
		ClaudeConfigPath: claudePath,
		ManifestDir:      manifestDir,
	})
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}

	var lonely *ScanEntry
	for i := range result.Entries {
		if result.Entries[i].Name == "lonely-server" {
			lonely = &result.Entries[i]
			break
		}
	}
	if lonely == nil {
		t.Fatalf("expected lonely-server in matrix entries even with empty client config; got %d entries", len(result.Entries))
	}
	if !lonely.ManifestExists {
		t.Errorf("ManifestExists: got false, want true")
	}
	if !lonely.CanMigrate {
		t.Errorf("CanMigrate: got false, want true (non-per-session manifest)")
	}
	if len(lonely.ClientPresence) != 0 {
		t.Errorf("ClientPresence: got %d entries, want 0 (no client has the server configured yet)", len(lonely.ClientPresence))
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

// TestScanCoversWave2Clients exercises the eight opt-in clients wired into
// the scan surface (PR #306 added the adapters; this PR wired them into
// /api/scan). Each config is written in the EXACT shape that client's
// adapter (internal/clients/<client>.go) emits for a hub binding:
//
//   - zed:      context_servers.<name> relay-stdio (command=mcphub, args[0]=relay)
//   - kiro:     mcpServers.<name>.url  (loopback hub URL)
//   - windsurf: mcpServers.<name>.serverUrl (Windsurf's url key)
//   - cline:    mcpServers.<name>.url
//   - kilocode: mcpServers.<name>.url
//   - opencode: mcp.<name>.{type:"remote", url}
//   - hermes:   YAML mcp_servers.<name>.url
//   - openclaw: NESTED mcp.servers.<name>.url
//
// The assertions confirm the per-client transport classification: zed →
// "relay" (recognised like antigravity), every HTTP-direct client → "http".
// classify() then maps both shapes to via-hub (covered by TestClassify).
func TestScanCoversWave2Clients(t *testing.T) {
	tmp := t.TempDir()

	zedPath := filepath.Join(tmp, "zed-settings.json")
	_ = os.WriteFile(zedPath, []byte(`{"context_servers":{"memory":{"command":"D:/dev/mcphub.exe","args":["relay","--url","http://localhost:9123/mcp"]}}}`), 0600)

	kiroPath := filepath.Join(tmp, "kiro-mcp.json")
	_ = os.WriteFile(kiroPath, []byte(`{"mcpServers":{"memory":{"url":"http://localhost:9123/mcp","disabled":false}}}`), 0600)

	windsurfPath := filepath.Join(tmp, "windsurf-mcp.json")
	_ = os.WriteFile(windsurfPath, []byte(`{"mcpServers":{"memory":{"serverUrl":"http://localhost:9123/mcp"}}}`), 0600)

	clinePath := filepath.Join(tmp, "cline-mcp.json")
	_ = os.WriteFile(clinePath, []byte(`{"mcpServers":{"memory":{"type":"streamableHttp","url":"http://localhost:9123/mcp"}}}`), 0600)

	kilocodePath := filepath.Join(tmp, "kilocode-mcp.json")
	_ = os.WriteFile(kilocodePath, []byte(`{"mcpServers":{"memory":{"type":"streamable-http","url":"http://localhost:9123/mcp"}}}`), 0600)

	opencodePath := filepath.Join(tmp, "opencode.json")
	_ = os.WriteFile(opencodePath, []byte(`{"mcp":{"memory":{"type":"remote","url":"http://localhost:9123/mcp","enabled":true}}}`), 0600)

	hermesPath := filepath.Join(tmp, "hermes-config.yaml")
	_ = os.WriteFile(hermesPath, []byte("mcp_servers:\n  memory:\n    url: http://localhost:9123/mcp\n"), 0600)

	openclawPath := filepath.Join(tmp, "openclaw.json")
	_ = os.WriteFile(openclawPath, []byte(`{"mcp":{"servers":{"memory":{"url":"http://localhost:9123/mcp","transport":"streamable-http","enabled":true}}}}`), 0600)

	// Manifest fixture so the PORT-AWARE via-hub gate can recognise the
	// loopback bindings (port 9123 must match memory's manifest daemon
	// port). Without a manifest, every loopback entry classifies external.
	manifestDir := filepath.Join(tmp, "servers")
	_ = os.MkdirAll(filepath.Join(manifestDir, "memory"), 0755)
	_ = os.WriteFile(filepath.Join(manifestDir, "memory", "manifest.yaml"),
		[]byte("name: memory\nkind: global\ntransport: stdio-bridge\ncommand: npx\ndaemons:\n  - name: default\n    port: 9123\n"), 0644)

	a := NewAPI()
	result, err := a.ScanFrom(ScanOpts{
		ZedConfigPath:      zedPath,
		KiroConfigPath:     kiroPath,
		WindsurfConfigPath: windsurfPath,
		ClineConfigPath:    clinePath,
		KiloCodeConfigPath: kilocodePath,
		OpenCodeConfigPath: opencodePath,
		HermesConfigPath:   hermesPath,
		OpenClawConfigPath: openclawPath,
		ManifestDir:        manifestDir,
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
	// zed is relay-stdio (like antigravity), the rest are HTTP-direct.
	if got := memEntry.ClientPresence["zed"].Transport; got != "relay" {
		t.Errorf("zed.Transport: got %q, want relay", got)
	}
	if got := memEntry.ClientPresence["zed"].RelayURL; got != "http://localhost:9123/mcp" {
		t.Errorf("zed.RelayURL: got %q, want the relay --url target", got)
	}
	for _, client := range []string{"kiro", "windsurf", "cline", "kilocode", "opencode", "hermes", "openclaw"} {
		if got := memEntry.ClientPresence[client].Transport; got != "http" {
			t.Errorf("%s.Transport: got %q, want http", client, got)
		}
		if got := memEntry.ClientPresence[client].Endpoint; got != "http://localhost:9123/mcp" {
			t.Errorf("%s.Endpoint: got %q, want the loopback hub URL", client, got)
		}
	}
	// End-to-end: every wave-2 client points at the hub → classify is via-hub.
	if memEntry.Status != "via-hub" {
		t.Errorf("memory Status with all-wave2-hub bindings: got %q, want via-hub", memEntry.Status)
	}
}

// TestScanCoversMimoCode is the scan-pipeline counterpart of the mimocode
// adapter wiring: the clientScanners() registry must have a mimocode entry so
// ScanFrom records ClientPresence["mimocode"] for an installed mimo config.
// MiMoCode is an OpenCode fork sharing the top-level `mcp` map, so the hub
// binding shape — and scanMimoCode — are identical to opencode's.
func TestScanCoversMimoCode(t *testing.T) {
	manifestFixture := func(dir string) string {
		manifestDir := filepath.Join(dir, "servers")
		_ = os.MkdirAll(filepath.Join(manifestDir, "memory"), 0755)
		_ = os.WriteFile(filepath.Join(manifestDir, "memory", "manifest.yaml"),
			[]byte("name: memory\nkind: global\ntransport: stdio-bridge\ncommand: npx\ndaemons:\n  - name: default\n    port: 9123\n"), 0644)
		return manifestDir
	}
	memEntryFor := func(t *testing.T, res *ScanResult) *ScanEntry {
		t.Helper()
		for i := range res.Entries {
			if res.Entries[i].Name == "memory" {
				return &res.Entries[i]
			}
		}
		t.Fatal("no memory entry found")
		return nil
	}

	t.Run("plain json mimocode.json hub binding -> via-hub", func(t *testing.T) {
		tmp := t.TempDir()
		mimoPath := filepath.Join(tmp, "mimocode.json")
		_ = os.WriteFile(mimoPath, []byte(`{"mcp":{"memory":{"type":"remote","url":"http://localhost:9123/mcp","enabled":true}}}`), 0600)
		manifestDir := manifestFixture(tmp)

		a := NewAPI()
		res, err := a.ScanFrom(ScanOpts{MimoCodeConfigPath: mimoPath, ManifestDir: manifestDir})
		if err != nil {
			t.Fatalf("Scan: %v", err)
		}
		mem := memEntryFor(t, res)
		if got := mem.ClientPresence["mimocode"].Transport; got != "http" {
			t.Errorf("mimocode.Transport: got %q, want http", got)
		}
		if got := mem.ClientPresence["mimocode"].Endpoint; got != "http://localhost:9123/mcp" {
			t.Errorf("mimocode.Endpoint: got %q, want the loopback hub URL", got)
		}
		if mem.Status != "via-hub" {
			t.Errorf("memory Status with a mimocode hub binding: got %q, want via-hub", mem.Status)
		}
	})

	t.Run("entry with absent `enabled` still records presence", func(t *testing.T) {
		tmp := t.TempDir()
		mimoPath := filepath.Join(tmp, "mimocode.json")
		// enabled omitted entirely → the generic url/command shaper records it.
		_ = os.WriteFile(mimoPath, []byte(`{"mcp":{"memory":{"type":"remote","url":"http://localhost:9123/mcp"}}}`), 0600)
		manifestDir := manifestFixture(tmp)

		a := NewAPI()
		res, err := a.ScanFrom(ScanOpts{MimoCodeConfigPath: mimoPath, ManifestDir: manifestDir})
		if err != nil {
			t.Fatalf("Scan: %v", err)
		}
		mem := memEntryFor(t, res)
		if _, present := mem.ClientPresence["mimocode"]; !present {
			t.Error("entry with absent `enabled` must still record mimocode presence")
		}
		if mem.Status != "via-hub" {
			t.Errorf("hub entry Status: got %q, want via-hub", mem.Status)
		}
	})
}

// isolateMimoCodeScanEnv clears MiMoCode config-resolution env vars so a scan
// test never lets an inherited MIMOCODE_*/XDG_CONFIG_HOME redirect the in-dir
// layer resolver toward the developer's real ~/.config/mimocode. All paths in
// these tests are explicit temp files; this is belt-and-suspenders since an
// explicit non-layer-named path already bypasses dir recomputation.
func isolateMimoCodeScanEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{"MIMOCODE_CONFIG", "MIMOCODE_CONFIG_CONTENT", "MIMOCODE_CONFIG_DIR", "MIMOCODE_HOME", "XDG_CONFIG_HOME"} {
		t.Setenv(k, "")
	}
}

// TestScanMimoCode_Faithful exercises the three source-accurate scan behaviors
// the generic OpenCode scan path lacks: JSONC decode, local command-array
// parsing, and enabled:false → absent presence. State-safe: temp file named
// mimocode.jsonc/.json so the in-dir layer resolver stays inside the temp dir.
func TestScanMimoCode_Faithful(t *testing.T) {
	manifestFixture := func(dir string) string {
		manifestDir := filepath.Join(dir, "servers")
		_ = os.MkdirAll(filepath.Join(manifestDir, "memory"), 0755)
		_ = os.WriteFile(filepath.Join(manifestDir, "memory", "manifest.yaml"),
			[]byte("name: memory\nkind: global\ntransport: stdio-bridge\ncommand: npx\ndaemons:\n  - name: default\n    port: 9123\n"), 0644)
		return manifestDir
	}
	entryFor := func(t *testing.T, res *ScanResult, name string) *ScanEntry {
		t.Helper()
		for i := range res.Entries {
			if res.Entries[i].Name == name {
				return &res.Entries[i]
			}
		}
		return nil
	}

	t.Run("JSONC mimocode.jsonc decodes (comments + trailing comma)", func(t *testing.T) {
		isolateMimoCodeScanEnv(t)
		tmp := t.TempDir()
		mimoPath := filepath.Join(tmp, "mimocode.jsonc")
		_ = os.WriteFile(mimoPath, []byte("{\n  // operator comment\n  \"mcp\": {\n    \"memory\": {\"type\":\"remote\",\"url\":\"http://localhost:9123/mcp\",\"enabled\":true},\n  },\n}\n"), 0600)
		manifestDir := manifestFixture(tmp)

		a := NewAPI()
		res, err := a.ScanFrom(ScanOpts{MimoCodeConfigPath: mimoPath, ManifestDir: manifestDir})
		if err != nil {
			t.Fatalf("Scan on commented .jsonc must not fail: %v", err)
		}
		mem := entryFor(t, res, "memory")
		if mem == nil {
			t.Fatal("memory entry missing from a commented mimocode.jsonc scan")
		}
		if got := mem.ClientPresence["mimocode"].Transport; got != "http" {
			t.Errorf("mimocode.Transport from .jsonc: got %q, want http", got)
		}
	})

	t.Run("local command array surfaces the executable as endpoint", func(t *testing.T) {
		isolateMimoCodeScanEnv(t)
		tmp := t.TempDir()
		mimoPath := filepath.Join(tmp, "mimocode.json")
		_ = os.WriteFile(mimoPath, []byte(`{"mcp":{"localsrv":{"type":"local","command":["npx","-y","some-mcp"],"enabled":true}}}`), 0600)
		manifestDir := manifestFixture(tmp)

		a := NewAPI()
		res, err := a.ScanFrom(ScanOpts{MimoCodeConfigPath: mimoPath, ManifestDir: manifestDir})
		if err != nil {
			t.Fatalf("Scan: %v", err)
		}
		ent := entryFor(t, res, "localsrv")
		if ent == nil {
			t.Fatal("localsrv entry missing")
		}
		ce := ent.ClientPresence["mimocode"]
		if ce.Transport != "stdio" {
			t.Errorf("local array command Transport: got %q, want stdio", ce.Transport)
		}
		if ce.Endpoint != "npx" {
			t.Errorf("local array command Endpoint: got %q, want the executable 'npx' (NOT empty 'Unknown stdio')", ce.Endpoint)
		}
	})

	t.Run("enabled:false → absent presence (clobber-protection)", func(t *testing.T) {
		isolateMimoCodeScanEnv(t)
		tmp := t.TempDir()
		mimoPath := filepath.Join(tmp, "mimocode.json")
		// A DISABLED hub entry must NOT classify as active http/via-hub.
		_ = os.WriteFile(mimoPath, []byte(`{"mcp":{"memory":{"type":"remote","url":"http://localhost:9123/mcp","enabled":false}}}`), 0600)
		manifestDir := manifestFixture(tmp)

		a := NewAPI()
		res, err := a.ScanFrom(ScanOpts{MimoCodeConfigPath: mimoPath, ManifestDir: manifestDir})
		if err != nil {
			t.Fatalf("Scan: %v", err)
		}
		mem := entryFor(t, res, "memory")
		if mem == nil {
			t.Fatal("memory entry missing")
		}
		if got := mem.ClientPresence["mimocode"].Transport; got != "absent" {
			t.Errorf("disabled (enabled:false) entry Transport: got %q, want absent", got)
		}
		if mem.Status == "via-hub" {
			t.Errorf("a disabled hub entry must NOT classify via-hub: got %q", mem.Status)
		}
	})
}

// TestShapeMimoCodeEntry_Unit pins the shaper's classification directly.
func TestShapeMimoCodeEntry_Unit(t *testing.T) {
	cases := []struct {
		name      string
		raw       map[string]any
		transport string
		endpoint  string
	}{
		{
			name:      "remote http",
			raw:       map[string]any{"type": "remote", "url": "http://localhost:9121/mcp", "enabled": true},
			transport: "http", endpoint: "http://localhost:9121/mcp",
		},
		{
			name:      "local command array",
			raw:       map[string]any{"type": "local", "command": []any{"uvx", "serena"}, "enabled": true},
			transport: "stdio", endpoint: "uvx",
		},
		{
			name:      "local string command (defensive)",
			raw:       map[string]any{"type": "local", "command": "uvx", "enabled": true},
			transport: "stdio", endpoint: "uvx",
		},
		{
			name:      "disabled remote → absent",
			raw:       map[string]any{"type": "remote", "url": "http://localhost:9121/mcp", "enabled": false},
			transport: "absent", endpoint: "",
		},
		{
			name:      "disabled local → absent",
			raw:       map[string]any{"type": "local", "command": []any{"uvx", "serena"}, "enabled": false},
			transport: "absent", endpoint: "",
		},
		{
			name:      "missing enabled defaults active (http)",
			raw:       map[string]any{"type": "remote", "url": "http://localhost:9123/mcp"},
			transport: "http", endpoint: "http://localhost:9123/mcp",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := shapeMimoCodeEntry(c.raw)
			if got.Transport != c.transport {
				t.Errorf("Transport = %q, want %q", got.Transport, c.transport)
			}
			if got.Endpoint != c.endpoint {
				t.Errorf("Endpoint = %q, want %q", got.Endpoint, c.endpoint)
			}
		})
	}
}

// TestScanMimoCode_InDirLayerMerge confirms ScanFrom sees an entry that lives
// only in the lower mimocode.json layer when a higher mimocode.jsonc exists
// (the scan reuses the adapter's merged read). State-safe: both files are in a
// temp dir, the scan path is the top layer.
func TestScanMimoCode_InDirLayerMerge(t *testing.T) {
	isolateMimoCodeScanEnv(t)
	tmp := t.TempDir()
	manifestDir := filepath.Join(tmp, "servers")
	_ = os.MkdirAll(filepath.Join(manifestDir, "memory"), 0755)
	_ = os.WriteFile(filepath.Join(manifestDir, "memory", "manifest.yaml"),
		[]byte("name: memory\nkind: global\ntransport: stdio-bridge\ncommand: npx\ndaemons:\n  - name: default\n    port: 9123\n"), 0644)

	jsonPath := filepath.Join(tmp, "mimocode.json")
	jsoncPath := filepath.Join(tmp, "mimocode.jsonc")
	// memory lives in the LOWER .json layer; the .jsonc has only unrelated keys.
	_ = os.WriteFile(jsonPath, []byte(`{"mcp":{"memory":{"type":"remote","url":"http://localhost:9123/mcp","enabled":true}}}`), 0600)
	_ = os.WriteFile(jsoncPath, []byte("{\n  // top layer, unrelated\n  \"theme\": \"dark\"\n}\n"), 0600)

	a := NewAPI()
	// Scan path is the top layer (.jsonc) — the merge must still surface the
	// lower-layer .json memory entry.
	res, err := a.ScanFrom(ScanOpts{MimoCodeConfigPath: jsoncPath, ManifestDir: manifestDir})
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	var mem *ScanEntry
	for i := range res.Entries {
		if res.Entries[i].Name == "memory" {
			mem = &res.Entries[i]
		}
	}
	if mem == nil {
		t.Fatal("lower-layer mimocode.json memory entry invisible to scan merge")
	}
	if got := mem.ClientPresence["mimocode"].Transport; got != "http" {
		t.Errorf("merged-layer Transport: got %q, want http", got)
	}
}

// TestScanMimoCode_PresencePromotion_RestrictedToMissing pins bot PR #420
// finding 2 (refinement): the MiMoCode lower-layer presence promotion upgrades
// only an ABSENT write-target verdict to "ok". A config-FAULT write target
// (directory / FIFO at the path → "error", or a refused symlink →
// "error-symlink") must NOT be promoted even when a separate lower layer exists —
// otherwise the GUI renders a green cell over a write target Apply/backup cannot
// write to. State-safe: temp dir, env isolated.
func TestScanMimoCode_PresencePromotion_RestrictedToMissing(t *testing.T) {
	t.Run("error (directory at write target) is NOT promoted despite a present config.json layer", func(t *testing.T) {
		isolateMimoCodeScanEnv(t)
		tmp := t.TempDir()
		// Write target mimocode.json is itself a DIRECTORY → generic probe = "error".
		writeTarget := filepath.Join(tmp, "mimocode.json")
		if err := os.MkdirAll(writeTarget, 0o755); err != nil {
			t.Fatal(err)
		}
		// A real lower-layer config.json with an entry that WOULD trigger promotion
		// if the gate keyed on `!= "ok"`.
		if err := os.WriteFile(filepath.Join(tmp, "config.json"),
			[]byte(`{"mcp":{"memory":{"type":"remote","url":"http://localhost:9123/mcp","enabled":true}}}`), 0o600); err != nil {
			t.Fatal(err)
		}
		a := NewAPI()
		res, err := a.ScanFrom(ScanOpts{MimoCodeConfigPath: writeTarget})
		if err != nil {
			t.Fatalf("ScanFrom: %v", err)
		}
		if got := res.ClientConfigPresence["mimocode"]; got != "error" {
			t.Errorf("a config-error write target must stay \"error\" (not promoted to ok by a lower layer): got %q", got)
		}
	})

	t.Run("missing write target IS promoted to ok by a present config.json layer (positive control)", func(t *testing.T) {
		isolateMimoCodeScanEnv(t)
		tmp := t.TempDir()
		writeTarget := filepath.Join(tmp, "mimocode.json") // absent
		if err := os.WriteFile(filepath.Join(tmp, "config.json"),
			[]byte(`{"mcp":{"memory":{"type":"remote","url":"http://localhost:9123/mcp","enabled":true}}}`), 0o600); err != nil {
			t.Fatal(err)
		}
		a := NewAPI()
		res, err := a.ScanFrom(ScanOpts{MimoCodeConfigPath: writeTarget})
		if err != nil {
			t.Fatalf("ScanFrom: %v", err)
		}
		if got := res.ClientConfigPresence["mimocode"]; got != "ok" {
			t.Errorf("an absent write target with a present config.json lower layer must promote to ok: got %q", got)
		}
	})
}

// TestScanMimoCode_PresencePromotion_NotForSymlinkBlockedParent pins bot PR #420
// (mimo r10) finding 2 follow-up: the lower-layer presence promotion must EXCLUDE
// the "missing-init-blocked-symlink" state. A write target whose parent resolves
// through a symlink cannot be CREATED by the hardened init/write pipeline
// (O_NOFOLLOW / FILE_FLAG_OPEN_REPARSE_POINT refuse to descend), so even with a
// present lower layer (config.json) an Apply that needs to create the write
// target would deterministically fail. Promoting it to "ok" would show a normal
// enabled cell whose later Apply fails — the same broken-UX hazard the
// config-FAULT exclusion already prevents. So the symlink-blocked absent state
// must stay un-promoted. State-safe: temp dir, env isolated; POSIX-only (symlink
// creation needs elevation on Windows).
func TestScanMimoCode_PresencePromotion_NotForSymlinkBlockedParent(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation requires elevation on Windows; the POSIX path exercises the symlink-blocked exclusion")
	}
	isolateMimoCodeScanEnv(t)
	tmp := t.TempDir()
	// Real dir where the config actually lives, plus a symlink standing in for the
	// config dir's parent (dotfile-management setup). The write target's immediate
	// parent IS the symlink, so the generic probe classifies it
	// "missing-init-blocked-symlink".
	realDir := filepath.Join(tmp, "real-dotfiles", "mimocode")
	if err := os.MkdirAll(realDir, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(tmp, "mimocode")
	if err := os.Symlink(realDir, link); err != nil {
		t.Fatalf("create symlink: %v", err)
	}
	// A present lower-layer config.json (through the symlink) that WOULD promote
	// presence to "ok" if the gate keyed on every absent state.
	if err := os.WriteFile(filepath.Join(link, "config.json"),
		[]byte(`{"mcp":{"memory":{"type":"remote","url":"http://localhost:9123/mcp","enabled":true}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	writeTarget := filepath.Join(link, "mimocode.json") // absent, parent is a symlink
	a := NewAPI()
	res, err := a.ScanFrom(ScanOpts{MimoCodeConfigPath: writeTarget})
	if err != nil {
		t.Fatalf("ScanFrom: %v", err)
	}
	if got := res.ClientConfigPresence["mimocode"]; got != "missing-init-blocked-symlink" {
		t.Errorf("a symlink-blocked write-target parent must NOT be promoted to ok by a lower config.json layer: got %q, want missing-init-blocked-symlink", got)
	}
}

// TestProbeClientConfigPresence_Wave2Clients confirms the eight wave-2
// clients participate in the per-client presence probe the Servers matrix
// uses to gate column visibility + the Initialize affordance. Mirrors
// TestProbeClientConfigPresence_StateMachine for the new client ids.
func TestProbeClientConfigPresence_Wave2Clients(t *testing.T) {
	tmp := t.TempDir()

	// zed: file present → ok.
	zedPath := filepath.Join(tmp, "zed-settings.json")
	if err := os.WriteFile(zedPath, []byte(`{"context_servers":{}}`), 0o600); err != nil {
		t.Fatalf("write zed settings: %v", err)
	}
	// kiro: parent dir exists, file absent → missing-init-possible.
	kiroParent := filepath.Join(tmp, ".kiro", "settings")
	if err := os.MkdirAll(kiroParent, 0o755); err != nil {
		t.Fatalf("mkdir kiro parent: %v", err)
	}
	kiroPath := filepath.Join(kiroParent, "mcp.json")
	// hermes: parent dir absent → missing.
	hermesPath := filepath.Join(tmp, "no-such-dir", "config.yaml")

	out := probeClientConfigPresence(ScanOpts{
		ZedConfigPath:    zedPath,
		KiroConfigPath:   kiroPath,
		HermesConfigPath: hermesPath,
		// Other wave-2 paths intentionally omitted → absent from the map.
	})
	if got := out["zed"]; got != "ok" {
		t.Errorf("zed (file present) = %q, want ok", got)
	}
	if got := out["kiro"]; got != "missing-init-possible" {
		t.Errorf("kiro (parent dir present, file absent) = %q, want missing-init-possible", got)
	}
	if got := out["hermes"]; got != "missing" {
		t.Errorf("hermes (parent dir absent) = %q, want missing", got)
	}
	if _, ok := out["windsurf"]; ok {
		t.Errorf("windsurf should not appear when its path is empty; got %v", out["windsurf"])
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
		// daemonPorts is the manifest daemon-port set for serverName, as
		// ScanFrom would pass it to classify(). The port-aware via-hub gate
		// (security review) requires a loopback entry's URL port to match one
		// of these; non-loopback cases can leave it nil.
		daemonPorts []int
		guiPort     int
		want        string
	}{
		{
			name:        "per-session takes precedence",
			entry:       &ScanEntry{ClientPresence: map[string]ClientEntry{"x": {Transport: "http", Endpoint: "http://localhost:9100/mcp"}}},
			serverName:  firstPerSessionServer(t),
			daemonPorts: []int{9100},
			want:        "per-session",
		},
		{
			name:        "http + localhost + matching port -> via-hub",
			entry:       &ScanEntry{ClientPresence: map[string]ClientEntry{"claude-code": {Transport: "http", Endpoint: "http://localhost:9200/mcp"}}},
			serverName:  "memory",
			daemonPorts: []int{9200},
			want:        "via-hub",
		},
		{
			name:        "http + 127.0.0.1 + matching port -> via-hub (Codex R1)",
			entry:       &ScanEntry{ClientPresence: map[string]ClientEntry{"claude-code": {Transport: "http", Endpoint: "http://127.0.0.1:9200/mcp"}}},
			serverName:  "memory",
			daemonPorts: []int{9200},
			want:        "via-hub",
		},
		{
			// PORT-AWARE FIX (security review): a loopback-http entry whose
			// URL port does NOT match any of this server's manifest daemon
			// ports is a stale/foreign binding (e.g. fetch pointed at 9121,
			// serena's port, when fetch's own daemon is 9133). It must NOT
			// show as a deceptive green via-hub cell — classify as an
			// external/unmanaged remote so it surfaces read-only.
			name:        "http + loopback + NON-matching port -> external (stale-port)",
			entry:       &ScanEntry{ClientPresence: map[string]ClientEntry{"claude-code": {Transport: "http", Endpoint: "http://localhost:9121/mcp"}}},
			serverName:  "fetch",
			daemonPorts: []int{9133},
			want:        "external",
		},
		{
			// A loopback entry for a server with NO manifest daemon ports
			// (manifest absent / no daemons) cannot match any port → external,
			// never via-hub. This is the operator's-own-local-server case the
			// security review flagged: it must not be mislabeled hub-managed.
			name:        "http + loopback + no manifest ports -> external",
			entry:       &ScanEntry{ClientPresence: map[string]ClientEntry{"claude-code": {Transport: "http", Endpoint: "http://localhost:7777/mcp"}}},
			serverName:  "my-own-local-server",
			daemonPorts: nil,
			want:        "external",
		},
		{
			// Multi-daemon manifest: a loopback entry matching the SECOND
			// declared daemon port still counts as via-hub.
			name:        "http + loopback + matches second daemon port -> via-hub",
			entry:       &ScanEntry{ClientPresence: map[string]ClientEntry{"claude-code": {Transport: "http", Endpoint: "http://localhost:9302/mcp"}}},
			serverName:  "multi",
			daemonPorts: []int{9301, 9302},
			want:        "via-hub",
		},
		{
			// Loopback with no explicit port (defaults to 80) cannot be a
			// daemon binding → external, not via-hub.
			name:        "http + loopback + no explicit port -> external",
			entry:       &ScanEntry{ClientPresence: map[string]ClientEntry{"claude-code": {Transport: "http", Endpoint: "http://localhost/mcp"}}},
			serverName:  "memory",
			daemonPorts: []int{9200},
			want:        "external",
		},
		{
			name:       "relay transport -> via-hub (Codex R1)",
			entry:      &ScanEntry{ClientPresence: map[string]ClientEntry{"antigravity": {Transport: "relay", Endpoint: "mcphub.exe"}}},
			serverName: "memory",
			want:       "via-hub",
		},
		{
			name:        "serena relay router on live GUI port -> via-hub",
			entry:       &ScanEntry{ClientPresence: map[string]ClientEntry{"antigravity": {Transport: "relay", Endpoint: "mcphub.exe", RelayURL: "http://127.0.0.1:9125/serena/mcp"}}},
			serverName:  "serena",
			daemonPorts: []int{9121},
			guiPort:     9125,
			want:        "via-hub",
		},
		{
			name:        "serena relay router on stale GUI port -> external",
			entry:       &ScanEntry{ClientPresence: map[string]ClientEntry{"antigravity": {Transport: "relay", Endpoint: "mcphub.exe", RelayURL: "http://127.0.0.1:9124/serena/mcp"}}},
			serverName:  "serena",
			daemonPorts: []int{9121},
			guiPort:     9125,
			want:        "external",
		},
		{
			name:        "serena relay without resolved URL -> external",
			entry:       &ScanEntry{ClientPresence: map[string]ClientEntry{"antigravity": {Transport: "relay", Endpoint: "mcphub.exe"}}},
			serverName:  "serena",
			daemonPorts: []int{9121},
			guiPort:     9125,
			want:        "external",
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
		{
			// Discovery view: a client-present non-hub remote HTTP entry (a real
			// external remote MCP — e.g. context7 -> mcp.context7.com) used to
			// hit the final "not-installed" branch and the screen DROPPED it.
			// Now it classifies as "external" so it surfaces.
			name:       "http + non-hub remote URL -> external",
			entry:      &ScanEntry{ClientPresence: map[string]ClientEntry{"claude-code": {Transport: "http", Endpoint: "https://mcp.context7.com/mcp"}}},
			serverName: "context7",
			want:       "external",
		},
		{
			// Multiple clients, all pointing at the same external remote, still
			// external (no hub, no stdio anywhere).
			name:       "http + non-hub remote in two clients -> external",
			entry:      &ScanEntry{ClientPresence: map[string]ClientEntry{"claude-code": {Transport: "http", Endpoint: "https://qt.io/mcp"}, "cursor": {Transport: "http", Endpoint: "https://qt.io/mcp"}}},
			serverName: "qt-docs",
			want:       "external",
		},
		{
			// Hub + external in the same row: hub wins (it's still hub-routed,
			// the external entry is in a different client). via-hub keeps its
			// richer status; the external branch is ordered after hub/stdio.
			name:        "hub + non-hub remote -> via-hub (hub wins over external)",
			entry:       &ScanEntry{ClientPresence: map[string]ClientEntry{"claude-code": {Transport: "http", Endpoint: "http://localhost:9200/mcp"}, "cursor": {Transport: "http", Endpoint: "https://mcp.context7.com/mcp"}}},
			serverName:  "memory",
			daemonPorts: []int{9200},
			want:        "via-hub",
		},
		{
			// Manifest-only-no-presence (empty ClientPresence) stays
			// "not-installed" — the matrix-row-must-not-vanish pass relies on
			// this. The external branch must NOT capture an empty-presence row.
			name:       "empty client presence -> not-installed (manifest-only row)",
			entry:      &ScanEntry{ClientPresence: map[string]ClientEntry{}},
			serverName: "memory",
			want:       "not-installed",
		},
		{
			// SERENA ROUTER (serena-client-revert-on-manifest-sync read-side): a
			// CLI scans do not know the live GUI port. In that mode, the serena
			// router special-case stays port-agnostic so a router-shaped entry does
			// not regress from r1 behavior.
			name:        "serena /serena/mcp router + unknown GUI port -> via-hub",
			entry:       &ScanEntry{ClientPresence: map[string]ClientEntry{"claude-code": {Transport: "http", Endpoint: "http://127.0.0.1:9125/serena/mcp"}}},
			serverName:  "serena",
			daemonPorts: []int{9121},
			guiPort:     0,
			want:        "via-hub",
		},
		{
			name:        "serena /serena/mcp router on live GUI port -> via-hub",
			entry:       &ScanEntry{ClientPresence: map[string]ClientEntry{"claude-code": {Transport: "http", Endpoint: "http://127.0.0.1:9125/serena/mcp"}}},
			serverName:  "serena",
			daemonPorts: []int{9121},
			guiPort:     9125,
			want:        "via-hub",
		},
		{
			name:        "serena /serena/mcp router on stale GUI port -> external",
			entry:       &ScanEntry{ClientPresence: map[string]ClientEntry{"claude-code": {Transport: "http", Endpoint: "http://127.0.0.1:9124/serena/mcp"}}},
			serverName:  "serena",
			daemonPorts: []int{9121},
			guiPort:     9125,
			want:        "external",
		},
		{
			// STALE serena router at the LEGACY DAEMON port (9121): even though 9121
			// IS serena's manifest daemon port, a router-shaped URL not on the live
			// GUI port is a dead endpoint and must NOT be reclassified via-hub by the
			// daemon-port fallback — the serena-shape case bypasses it (#379 r5).
			name:        "serena /serena/mcp router on legacy daemon port (stale) -> external",
			entry:       &ScanEntry{ClientPresence: map[string]ClientEntry{"claude-code": {Transport: "http", Endpoint: "http://127.0.0.1:9121/serena/mcp"}}},
			serverName:  "serena",
			daemonPorts: []int{9121},
			guiPort:     9125,
			want:        "external",
		},
		{
			// The serena router special-case is NAME-GATED: a NON-serena server at a
			// loopback /serena/mcp-shaped URL whose port does not match its daemon
			// ports stays external (no cross-server leakage of the serena rule).
			name:        "non-serena server at /serena/mcp-shaped loopback + wrong port -> external",
			entry:       &ScanEntry{ClientPresence: map[string]ClientEntry{"claude-code": {Transport: "http", Endpoint: "http://127.0.0.1:9125/serena/mcp"}}},
			serverName:  "memory",
			daemonPorts: []int{9200},
			want:        "external",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := classify(tc.entry, tc.serverName, manifests, tc.daemonPorts, tc.guiPort)
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
//     exists (operator has the client
//     installed; GUI Initialize button is
//     offered)
//   - "missing"                : neither file nor parent dir exists
//     (client genuinely not installed)
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
		VSCodeConfigPath: vscodePath,
		CursorConfigPath: cursorPath,
		ClaudeConfigPath: claudePath,
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
// the config path must surface as a symlink-refusal state, not
// "missing-init-possible". Previously os.Stat followed the symlink,
// returned IsNotExist (target absent), and classifyMissingClientConfig
// saw the parent dir exists → "missing-init-possible" → GUI offered
// Initialize → secure-create refused → 500 INIT_FAILED. The Lstat-first
// probe now classifies symlinks (dangling or not) as "error-symlink"
// so the matrix renders the symlink-specific diagnostic instead.
//
// 2026-05-19 message-accuracy fix: the symlink-refusal branch reports
// "error-symlink" (distinct from the generic "error" stat-failure /
// wrong-shape state) so the Servers tooltip tells the operator their
// dotfile-symlink setup is refused by design.
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
	if got := out["vscode"]; got != "error-symlink" {
		t.Errorf("dangling symlink at config path classified as %q, want \"error-symlink\"", got)
	}
}

// TestProbeClientConfigPresence_DirectoryAtConfigPath pins the v0.4.5
// deep-sec PR #208 Lane B round 5 P2 closure: a directory at the
// config path must surface as "error", not "ok". Previously the
// Lstat probe only rejected symlinks; a directory passed through to
// os.Stat which succeeded and the cell was classified as "ok" —
// migrate/backup would then fail later when adapter.readJSON tried
// to read a directory.
//
// 2026-05-19 message-accuracy fix: a directory is a non-regular
// non-symlink shape, so it stays in the GENERIC "error" category
// (NOT "error-symlink") — the symlink-specific tooltip would be
// wrong for a directory.
func TestProbeClientConfigPresence_DirectoryAtConfigPath(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, "mcp.json")
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatalf("seed directory at config path: %v", err)
	}

	out := probeClientConfigPresence(ScanOpts{VSCodeConfigPath: path})
	if got := out["vscode"]; got != "error" {
		t.Errorf("directory at config path classified as %q, want \"error\" (generic, not symlink)", got)
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
	if got := out["vscode"]; got != "error-symlink" {
		t.Errorf("symlink-to-regular in default mode classified as %q, want \"error-symlink\" (write pipeline refuses symlinks post-PR-#209)", got)
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
	if got := out["vscode"]; got != "error-symlink" {
		t.Errorf("symlink with opt-in env BUT strict mode set classified as %q, want \"error-symlink\" (strict overrides opt-in)", got)
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
	if got := out["vscode"]; got != "error-symlink" {
		t.Errorf("dangling symlink with opt-in env classified as %q, want \"error-symlink\" (target must be regular file)", got)
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
	if got := out["vscode"]; got != "error-symlink" {
		t.Errorf("symlink-to-regular in strict mode classified as %q, want \"error-symlink\"", got)
	}
}

// TestProbeClientConfigPresence_SymlinkVsStatErrorDistinct is the
// canonical regression for the 2026-05-19 message-accuracy fix
// (work-items/bugs/2026-05-19-codex-config-symlink-blocked-by-pr209.md):
// the symlink-refusal category MUST be reported distinctly from the
// generic stat/wrong-shape error so the Servers matrix can tell the
// operator their dotfile-symlink setup is unsupported by design
// (replace the symlink / edit the target) rather than rendering the
// misleading "stat error — check permissions and disk health" tooltip.
//
// One scan, two refused clients: a symlinked config (codex-cli, the
// exact reported case) → "error-symlink"; a directory at the config
// path (vscode, a genuine wrong-shape) → generic "error". The two
// categories must NOT collapse to the same value.
func TestProbeClientConfigPresence_SymlinkVsStatErrorDistinct(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation requires elevation on Windows; the cross-platform Lstat probe is exercised by the POSIX path")
	}
	// Ensure neither strict mode nor symlink opt-in is active so the
	// symlink hits the plain default-mode refusal branch.
	t.Setenv(RequireSingleUserHomeEnv, "")
	t.Setenv(AllowClientConfigSymlinkEnv, "")
	tmp := t.TempDir()

	// codex-cli: symlink to a real regular file → refused by PR #209
	// secure-write contract → "error-symlink" (the reported bug).
	target := filepath.Join(tmp, "real-config.toml")
	if err := os.WriteFile(target, []byte("[mcp_servers]\n"), 0o600); err != nil {
		t.Fatalf("seed symlink target: %v", err)
	}
	codexLink := filepath.Join(tmp, "config.toml")
	if err := os.Symlink(target, codexLink); err != nil {
		t.Fatalf("create codex symlink: %v", err)
	}

	// vscode: a directory at the config path → genuine wrong-shape
	// → generic "error".
	vscodeDir := filepath.Join(tmp, "mcp.json")
	if err := os.MkdirAll(vscodeDir, 0o755); err != nil {
		t.Fatalf("seed vscode directory: %v", err)
	}

	out := probeClientConfigPresence(ScanOpts{
		CodexConfigPath:  codexLink,
		VSCodeConfigPath: vscodeDir,
	})

	if got := out["codex-cli"]; got != "error-symlink" {
		t.Errorf("symlinked codex config classified as %q, want \"error-symlink\"", got)
	}
	if got := out["vscode"]; got != "error" {
		t.Errorf("directory at vscode config classified as %q, want generic \"error\"", got)
	}
	if out["codex-cli"] == out["vscode"] {
		t.Errorf("symlink-refusal and stat-error collapsed to the same category %q; they must be distinct so the matrix renders the right diagnostic", out["codex-cli"])
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

// TestClassifyMissingClientConfig_Creatable pins the G17 (2026-06-18)
// "missing-init-creatable" state: when the config file AND its parent
// directory are absent BUT the path is under the user home and the
// existing prefix is a real directory chain, classify returns
// "missing-init-creatable" so the GUI offers Initialize (which securely
// creates the parent). Paths outside the home, or with a non-directory
// in the existing prefix, stay "missing".
func TestClassifyMissingClientConfig_Creatable(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)

	// Absent parent under home, real-dir prefix → creatable. The path's
	// parent (.cursor) does not exist yet.
	creatable := filepath.Join(home, ".cursor", "mcp.json")
	if got := classifyMissingClientConfig(creatable); got != "missing-init-creatable" {
		t.Errorf("absent parent under home: got %q, want missing-init-creatable", got)
	}

	// Deeper absent chain under home (multiple missing components) →
	// still creatable.
	deep := filepath.Join(home, ".config", "SomeClient", "User", "mcp.json")
	if got := classifyMissingClientConfig(deep); got != "missing-init-creatable" {
		t.Errorf("deep absent chain under home: got %q, want missing-init-creatable", got)
	}

	// A path whose existing prefix passes through a regular FILE (not a
	// dir) is NOT creatable → "missing".
	regularFile := filepath.Join(home, "afile")
	if err := os.WriteFile(regularFile, []byte("x"), 0o600); err != nil {
		t.Fatalf("seed regular file: %v", err)
	}
	overFile := filepath.Join(regularFile, "sub", "mcp.json")
	if got := classifyMissingClientConfig(overFile); got != "missing" {
		t.Errorf("absent chain whose prefix is a file: got %q, want missing", got)
	}
}

// TestClassifyMissingClientConfig_OutsideHome pins that an absent parent
// OUTSIDE the user home is NOT creatable — the affordance must not be
// offered for a path the secure parent-create would refuse (blast-radius
// bound). Returns "missing".
func TestClassifyMissingClientConfig_OutsideHome(t *testing.T) {
	home := t.TempDir()
	other := t.TempDir() // sibling temp dir, NOT under home
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)

	outside := filepath.Join(other, "elsewhere", "mcp.json")
	if got := classifyMissingClientConfig(outside); got != "missing" {
		t.Errorf("absent parent outside home: got %q, want missing", got)
	}
}

// TestClassifyMissingClientConfig_CreatableThroughSymlinkPrefix pins
// that an absent parent whose longest-existing prefix passes through a
// symlink classifies as "missing-init-blocked-symlink" (suppressed), not
// "missing-init-creatable" — the secure parent-create refuses to descend
// through a symlink, so the affordance must stay hidden.
func TestClassifyMissingClientConfig_CreatableThroughSymlinkPrefix(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation requires elevation on Windows; POSIX path exercises the prefix-symlink refusal")
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)

	// A real target dir + a symlink to it under home. The config path's
	// parent is BELOW the symlinked component (absent), so the existing
	// prefix passes through the symlink.
	target := filepath.Join(home, "real-config-root")
	if err := os.MkdirAll(target, 0o700); err != nil {
		t.Fatalf("mkdir target: %v", err)
	}
	link := filepath.Join(home, ".config")
	if err := os.Symlink(target, link); err != nil {
		t.Fatalf("create symlink: %v", err)
	}
	// .config (symlink) exists; SomeClient (below it) is absent.
	through := filepath.Join(link, "SomeClient", "mcp.json")
	if got := classifyMissingClientConfig(through); got != "missing-init-blocked-symlink" {
		t.Errorf("absent parent through symlinked prefix: got %q, want missing-init-blocked-symlink", got)
	}
}

// seedLSPRegistry writes a workspaces.yaml at tmpHome/workspaces.yaml carrying
// the supplied entries and points the package-level defaultRegistryPathFn test
// seam at it so classifyLSPEntries can find it under hermetic test conditions.
func seedLSPRegistry(t *testing.T, tmpHome string, entries []WorkspaceEntry) string {
	t.Helper()
	path := filepath.Join(tmpHome, "workspaces.yaml")
	reg := NewRegistry(path)
	reg.Workspaces = entries
	if err := reg.Save(); err != nil {
		t.Fatalf("seedLSPRegistry: Save: %v", err)
	}
	prev := defaultRegistryPathFn
	defaultRegistryPathFn = func() (string, error) { return path, nil }
	t.Cleanup(func() { defaultRegistryPathFn = prev })
	return path
}

// seedCodexConfigTOML writes a ~/.codex/config.toml whose [mcp_servers.*]
// blocks come from `servers`. Each entry value must be a map[string]any
// suitable for marshalling via toml.Marshal — http: { "url": "..." }; stdio:
// { "command": "...", "args": [...] }.
func seedCodexConfigTOML(t *testing.T, tmpHome string, servers map[string]map[string]any) string {
	t.Helper()
	path := filepath.Join(tmpHome, ".codex", "config.toml")
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatalf("seedCodexConfigTOML: mkdir: %v", err)
	}
	root := map[string]any{"mcp_servers": map[string]any{}}
	mcp := root["mcp_servers"].(map[string]any)
	for name, body := range servers {
		mcp[name] = body
	}
	data, err := toml.Marshal(root)
	if err != nil {
		t.Fatalf("seedCodexConfigTOML: marshal: %v", err)
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatalf("seedCodexConfigTOML: write: %v", err)
	}
	return path
}

// TestScanRecognizesHubManagedLSP pins Rule 1 of classifyLSPEntries: a
// hub-managed LSP entry under the canonical `mcp-language-server-<lang>`
// name with transport=http is recognised as via-hub. Without a legacy
// stdio coexistence row, LegacyConflict stays nil.
func TestScanRecognizesHubManagedLSP(t *testing.T) {
	tmp := t.TempDir()

	// Workspace registry knows about one clangd registration for codex-cli.
	seedLSPRegistry(t, tmp, []WorkspaceEntry{{
		WorkspaceKey:  "abcd1234",
		WorkspacePath: filepath.Join(tmp, "proj"),
		Language:      "clangd",
		Backend:       "mcp-language-server",
		Port:          9201,
		TaskName:      "mcp-local-hub-lsp-abcd1234-clangd",
		ClientEntries: map[string]string{"codex-cli": "mcp-language-server-clangd"},
	}})

	// Codex config carries one hub HTTP entry under the canonical name.
	codexPath := seedCodexConfigTOML(t, tmp, map[string]map[string]any{
		"mcp-language-server-clangd": {"url": "http://localhost:9201/mcp"},
	})

	a := NewAPI()
	result, err := a.ScanFrom(ScanOpts{
		CodexConfigPath: codexPath,
		ManifestDir:     t.TempDir(),
	})
	if err != nil {
		t.Fatalf("ScanFrom: %v", err)
	}

	var entry *ScanEntry
	for i := range result.Entries {
		if result.Entries[i].Name == "mcp-language-server-clangd" {
			entry = &result.Entries[i]
			break
		}
	}
	if entry == nil {
		t.Fatalf("expected entry mcp-language-server-clangd in scan; got %d entries", len(result.Entries))
	}
	if got := entry.Status; got != "via-hub" {
		t.Errorf("Status: got %q, want via-hub", got)
	}
	if got := entry.ClientPresence["codex-cli"].Transport; got != "http" {
		t.Errorf("ClientPresence[codex-cli].Transport: got %q, want http", got)
	}
	if entry.LegacyConflict != nil {
		t.Errorf("LegacyConflict: got %v, want nil (no stdio coexistence)", entry.LegacyConflict)
	}
}

// TestScanRecognizesCoexistenceAnomaly pins Rule 2 (direct-stdio
// mcp-language-server) plus the coexistence-collapse step: when a hub
// row AND a separate stdio row both exist for the same (client, lang)
// pair, the stdio row is moved into the hub row's LegacyConflict map
// keyed by clientName and its dangling row disappears.
func TestScanRecognizesCoexistenceAnomaly(t *testing.T) {
	tmp := t.TempDir()

	// Registry maps both names to codex-cli (the hub-managed one and the
	// direct-stdio one). The hub registration is canonical; the
	// direct-stdio is the legacy coexistence row.
	seedLSPRegistry(t, tmp, []WorkspaceEntry{{
		WorkspaceKey:  "f3a07e91",
		WorkspacePath: filepath.Join(tmp, "proj", "main"),
		Language:      "rust",
		Backend:       "mcp-language-server",
		Port:          9202,
		TaskName:      "mcp-local-hub-lsp-f3a07e91-rust",
		ClientEntries: map[string]string{"codex-cli": "mcp-language-server-rust"},
	}})

	codexPath := seedCodexConfigTOML(t, tmp, map[string]map[string]any{
		"mcp-language-server-rust": {"url": "http://localhost:9202/mcp"},
		"rust-langserver-direct": {
			"command": "mcp-language-server",
			"args":    []any{"--lsp", "rust-analyzer", "--workspace", "/proj/main"},
		},
	})

	a := NewAPI()
	result, err := a.ScanFrom(ScanOpts{
		CodexConfigPath: codexPath,
		ManifestDir:     t.TempDir(),
	})
	if err != nil {
		t.Fatalf("ScanFrom: %v", err)
	}

	// Inventory: dangling rust-langserver-direct row must be gone; the hub
	// row must carry the moved stdio entry under LegacyConflict[codex-cli].
	var hub *ScanEntry
	for i := range result.Entries {
		if result.Entries[i].Name == "mcp-language-server-rust" {
			hub = &result.Entries[i]
		}
		if result.Entries[i].Name == "rust-langserver-direct" {
			t.Errorf("rust-langserver-direct should have been pruned after coexistence-collapse; still present with ClientPresence=%v", result.Entries[i].ClientPresence)
		}
	}
	if hub == nil {
		t.Fatalf("expected hub entry mcp-language-server-rust in scan; got %d entries", len(result.Entries))
	}
	if got := hub.ClientPresence["codex-cli"].Transport; got != "http" {
		t.Errorf("hub ClientPresence[codex-cli].Transport: got %q, want http", got)
	}
	if hub.LegacyConflict == nil {
		t.Fatalf("hub LegacyConflict: got nil, want a stdio coexistence entry under codex-cli")
	}
	leg, ok := hub.LegacyConflict["codex-cli"]
	if !ok {
		t.Fatalf("hub LegacyConflict[codex-cli]: missing; keys=%v", keysOfLegacyConflict(hub.LegacyConflict))
	}
	if leg.Transport != "stdio" {
		t.Errorf("LegacyConflict[codex-cli].Transport: got %q, want stdio", leg.Transport)
	}
	if base := filepath.Base(strings.TrimSuffix(leg.Endpoint, ".exe")); base != "mcp-language-server" {
		t.Errorf("LegacyConflict[codex-cli].Endpoint basename: got %q, want mcp-language-server", base)
	}
}

func keysOfLegacyConflict(m map[string]ClientEntry) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// TestScanRecognizesCoexistenceAnomaly_MultiWorkspaceDeterministic
// closes the bot-review PR #222 P2 gap (scan.go:1218): when one client
// has TWO hub-managed workspaces for the same language AND a legacy
// stdio donor for that language, the donor must attach to BOTH hub
// rows. Pre-fix, `hubByPair[clientName+lang]` overwrote the first hub
// with the second, then the donor landed on whichever one survived
// map iteration order — nondeterministic. The fix tracks every
// matching hub row in a slice and attaches the donor to all of them.
func TestScanRecognizesCoexistenceAnomaly_MultiWorkspaceDeterministic(t *testing.T) {
	tmp := t.TempDir()

	// Register clangd in TWO workspaces under codex-cli. Use full
	// 8-hex suffixes to dodge the 4-hex prefix-collision fallback.
	seedLSPRegistry(t, tmp, []WorkspaceEntry{
		{
			WorkspaceKey:  "abcdef01",
			WorkspacePath: filepath.Join(tmp, "proj", "alpha"),
			Language:      "clangd",
			Backend:       "mcp-language-server",
			Port:          9210,
			TaskName:      "mcp-local-hub-lsp-abcdef01-clangd",
			ClientEntries: map[string]string{"codex-cli": "mcp-language-server-clangd-abcdef01"},
		},
		{
			WorkspaceKey:  "12345678",
			WorkspacePath: filepath.Join(tmp, "proj", "beta"),
			Language:      "clangd",
			Backend:       "mcp-language-server",
			Port:          9211,
			TaskName:      "mcp-local-hub-lsp-12345678-clangd",
			ClientEntries: map[string]string{"codex-cli": "mcp-language-server-clangd-12345678"},
		},
	})

	codexPath := seedCodexConfigTOML(t, tmp, map[string]map[string]any{
		"mcp-language-server-clangd-abcdef01": {"url": "http://localhost:9210/mcp"},
		"mcp-language-server-clangd-12345678": {"url": "http://localhost:9211/mcp"},
		// Legacy stdio donor with no workspace context — its workspace
		// affinity is ambiguous, so it must surface on EVERY matching
		// hub row.
		"clangd-direct": {
			"command": "mcp-language-server",
			"args":    []any{"--lsp", "clangd", "--workspace", "/some/proj"},
		},
	})

	a := NewAPI()
	result, err := a.ScanFrom(ScanOpts{
		CodexConfigPath: codexPath,
		ManifestDir:     t.TempDir(),
	})
	if err != nil {
		t.Fatalf("ScanFrom: %v", err)
	}

	var hubAlpha, hubBeta *ScanEntry
	for i := range result.Entries {
		switch result.Entries[i].Name {
		case "mcp-language-server-clangd-abcdef01":
			hubAlpha = &result.Entries[i]
		case "mcp-language-server-clangd-12345678":
			hubBeta = &result.Entries[i]
		case "clangd-direct":
			t.Errorf("clangd-direct donor should have been pruned after coexistence-collapse; still present with ClientPresence=%v", result.Entries[i].ClientPresence)
		}
	}
	if hubAlpha == nil || hubBeta == nil {
		t.Fatalf("expected both hub entries; alpha=%v beta=%v", hubAlpha, hubBeta)
	}

	// BOTH hub rows must carry the legacy conflict — that's the
	// deterministic-honest semantic. Pre-fix, only one would have it
	// (whichever survived map iteration); the other would silently
	// miss the anomaly.
	for _, h := range []*ScanEntry{hubAlpha, hubBeta} {
		if h.LegacyConflict == nil {
			t.Errorf("%s: LegacyConflict nil; want stdio donor attached to every matching hub", h.Name)
			continue
		}
		leg, ok := h.LegacyConflict["codex-cli"]
		if !ok {
			t.Errorf("%s: LegacyConflict[codex-cli] missing; keys=%v", h.Name, keysOfLegacyConflict(h.LegacyConflict))
			continue
		}
		if leg.Transport != "stdio" {
			t.Errorf("%s: LegacyConflict[codex-cli].Transport = %q, want stdio", h.Name, leg.Transport)
		}
	}
}

// TestScanMimoCode_PresenceFromLowerLayerWhenWriteTargetAbsent pins bot PR #420
// finding 2: when the registered scan path (the WRITE target — mimocode.json) is
// ABSENT but a lower config.json layer (or an overlay layer) defines servers,
// the scan must STILL run scanMimoCode so the operator's real entries appear.
// Pre-fix the presence probe stat'd only the absent write target → "missing" →
// the scanIfReadable gate skipped scanMimoCode → entries vanished.
func TestScanMimoCode_PresenceFromLowerLayerWhenWriteTargetAbsent(t *testing.T) {
	manifestFixture := func(dir string) string {
		manifestDir := filepath.Join(dir, "servers")
		_ = os.MkdirAll(filepath.Join(manifestDir, "memory"), 0755)
		_ = os.WriteFile(filepath.Join(manifestDir, "memory", "manifest.yaml"),
			[]byte("name: memory\nkind: global\ntransport: stdio-bridge\ncommand: npx\ndaemons:\n  - name: default\n    port: 9123\n"), 0644)
		return manifestDir
	}
	memEntry := func(t *testing.T, res *ScanResult) *ScanEntry {
		t.Helper()
		for i := range res.Entries {
			if res.Entries[i].Name == "memory" {
				return &res.Entries[i]
			}
		}
		return nil
	}

	t.Run("server only in lower config.json layer (write target absent)", func(t *testing.T) {
		isolateMimoCodeScanEnv(t)
		tmp := t.TempDir()
		// memory lives ONLY in the lowest config.json layer; mimocode.json (the
		// registered write/scan target) does NOT exist.
		_ = os.WriteFile(filepath.Join(tmp, "config.json"),
			[]byte(`{"mcp":{"memory":{"type":"remote","url":"http://localhost:9123/mcp","enabled":true}}}`), 0600)
		mimoPath := filepath.Join(tmp, "mimocode.json") // intentionally absent on disk
		manifestDir := manifestFixture(tmp)

		a := NewAPI()
		res, err := a.ScanFrom(ScanOpts{MimoCodeConfigPath: mimoPath, ManifestDir: manifestDir})
		if err != nil {
			t.Fatalf("Scan: %v", err)
		}
		if got := res.ClientConfigPresence["mimocode"]; got != "ok" {
			t.Errorf("mimocode presence with a lower-layer-only config.json: got %q, want ok", got)
		}
		mem := memEntry(t, res)
		if mem == nil {
			t.Fatal("config.json-layer memory entry vanished from scan when mimocode.json write target is absent")
		}
		if got := mem.ClientPresence["mimocode"].Transport; got != "http" {
			t.Errorf("lower-layer memory Transport: got %q, want http", got)
		}
	})

	t.Run("server only in MIMOCODE_CONFIG_DIR overlay (write target absent)", func(t *testing.T) {
		isolateMimoCodeScanEnv(t)
		tmp := t.TempDir()
		overlay := t.TempDir()
		// memory lives ONLY in the overlay; the global dir has no layer files at all.
		_ = os.WriteFile(filepath.Join(overlay, "mimocode.json"),
			[]byte(`{"mcp":{"memory":{"type":"remote","url":"http://localhost:9123/mcp","enabled":true}}}`), 0600)
		t.Setenv("MIMOCODE_CONFIG_DIR", overlay)
		mimoPath := filepath.Join(tmp, "mimocode.json") // absent
		manifestDir := manifestFixture(tmp)

		a := NewAPI()
		res, err := a.ScanFrom(ScanOpts{MimoCodeConfigPath: mimoPath, ManifestDir: manifestDir})
		if err != nil {
			t.Fatalf("Scan: %v", err)
		}
		if got := res.ClientConfigPresence["mimocode"]; got != "ok" {
			t.Errorf("mimocode presence with an overlay-only definition: got %q, want ok", got)
		}
		mem := memEntry(t, res)
		if mem == nil {
			t.Fatal("overlay-only memory entry vanished from scan when the write target is absent")
		}
	})

	t.Run("server only in MIMOCODE_CONFIG_CONTENT inline layer (no file on disk)", func(t *testing.T) {
		// bot PR #420 finding 1: a profile whose ONLY mimo config layer is the
		// INLINE MIMOCODE_CONFIG_CONTENT has NO file to stat — the file-only
		// presence promotion never fires — yet MimoCodeMergedConfig parses the
		// inline layer. The scan must promote presence to "ok" on the parseable
		// inline content so scanMimoCode runs and the inline servers appear.
		isolateMimoCodeScanEnv(t)
		tmp := t.TempDir()
		// NO config file is written anywhere; the only definition is inline.
		t.Setenv("MIMOCODE_CONFIG_CONTENT",
			`{"mcp":{"memory":{"type":"remote","url":"http://localhost:9123/mcp","enabled":true}}}`)
		mimoPath := filepath.Join(tmp, "mimocode.json") // a global-layer name, absent on disk
		manifestDir := manifestFixture(tmp)

		a := NewAPI()
		res, err := a.ScanFrom(ScanOpts{MimoCodeConfigPath: mimoPath, ManifestDir: manifestDir})
		if err != nil {
			t.Fatalf("Scan: %v", err)
		}
		if got := res.ClientConfigPresence["mimocode"]; got != "ok" {
			t.Errorf("mimocode presence with an inline-only MIMOCODE_CONFIG_CONTENT: got %q, want ok", got)
		}
		mem := memEntry(t, res)
		if mem == nil {
			t.Fatal("inline-only memory entry vanished from scan (presence not promoted for MIMOCODE_CONFIG_CONTENT)")
		}
		if got := mem.ClientPresence["mimocode"].Transport; got != "http" {
			t.Errorf("inline-layer memory Transport: got %q, want http", got)
		}
	})

	t.Run("malformed MIMOCODE_CONFIG_CONTENT does NOT promote presence", func(t *testing.T) {
		// A non-parseable inline string is not a valid layer; presence must stay
		// missing (it mirrors a present-but-malformed FILE layer: the merged read
		// would surface a parse error rather than green-then-fail).
		isolateMimoCodeScanEnv(t)
		tmp := t.TempDir()
		t.Setenv("MIMOCODE_CONFIG_CONTENT", `{ this is not json `)
		mimoPath := filepath.Join(tmp, "mimocode.json") // absent on disk
		manifestDir := manifestFixture(tmp)

		a := NewAPI()
		res, err := a.ScanFrom(ScanOpts{MimoCodeConfigPath: mimoPath, ManifestDir: manifestDir})
		if err != nil {
			t.Fatalf("Scan: %v", err)
		}
		if got := res.ClientConfigPresence["mimocode"]; got == "ok" {
			t.Errorf("malformed inline content must NOT promote presence to ok: got %q", got)
		}
	})
}

// TestShapeMimoCodeEntry_NormalizesLSPArgsFromCommandArray pins bot PR #420
// finding 6 at the unit level: a non-canonical mimo mcp-language-server entry
// keeps its LSP tokens in the `command` ARRAY (["mcp-language-server","--lsp",
// "go"]); shapeMimoCodeEntry must produce a Raw whose `args` carry those tokens
// so the LSP args-reverse-lookup (extractLSPLanguageFromArgs) identifies the
// language.
func TestShapeMimoCodeEntry_NormalizesLSPArgsFromCommandArray(t *testing.T) {
	// `--lsp <BINARY>` (gopls), reverse-mapped to "go" by lspCommandToLanguage.
	ce := shapeMimoCodeEntry(map[string]any{
		"type":    "local",
		"command": []any{"mcp-language-server", "--lsp", "gopls"},
		"enabled": true,
	})
	if ce.Transport != "stdio" || ce.Endpoint != "mcp-language-server" {
		t.Fatalf("shaper: got transport=%q endpoint=%q, want stdio/mcp-language-server", ce.Transport, ce.Endpoint)
	}
	if got := extractLSPLanguageFromArgs(ce.Raw); got != "go" {
		t.Errorf("normalized Raw must let the LSP arg-reverse-lookup find go (from --lsp gopls): got %q (Raw[args]=%v)", got, ce.Raw["args"])
	}
	// A separate args array is preserved AFTER the command-array tail.
	ce2 := shapeMimoCodeEntry(map[string]any{
		"type":    "local",
		"command": []any{"mcp-language-server"},
		"args":    []any{"--lsp", "rust-analyzer"},
		"enabled": true,
	})
	if got := extractLSPLanguageFromArgs(ce2.Raw); got != "rust" {
		t.Errorf("command-array + separate args: got %q, want rust (from --lsp rust-analyzer; Raw[args]=%v)", got, ce2.Raw["args"])
	}
}

// TestScanMimoCode_NonCanonicalLSPRecognized pins finding 6 end-to-end: a mimo
// LOCAL mcp-language-server entry whose NAME does not follow the canonical
// mcp-language-server-<lang> shape is still recognized as a direct-stdio LSP
// legacy via the `--lsp` reverse-lookup over the command array. It collapses
// into the hub row's LegacyConflict when a same-(client,lang) hub row exists.
func TestScanMimoCode_NonCanonicalLSPRecognized(t *testing.T) {
	isolateMimoCodeScanEnv(t)
	tmp := t.TempDir()
	seedLSPRegistry(t, tmp, []WorkspaceEntry{{
		WorkspaceKey:  "aa11bb22",
		WorkspacePath: filepath.Join(tmp, "proj"),
		Language:      "go",
		Backend:       "mcp-language-server",
		Port:          9301,
		TaskName:      "mcp-local-hub-lsp-aa11bb22-go",
		ClientEntries: map[string]string{"mimocode": "mcp-language-server-go"},
	}})
	mimoPath := filepath.Join(tmp, "mimocode.json")
	// One canonical hub row + one NON-canonically-named local LSP entry whose
	// language only the command-array `--lsp gopls` reveals (gopls → go via
	// lspCommandToLanguage).
	_ = os.WriteFile(mimoPath, []byte(`{"mcp":{
  "mcp-language-server-go": {"type":"remote","url":"http://localhost:9301/mcp","enabled":true},
  "my-go-langserver": {"type":"local","command":["mcp-language-server","--lsp","gopls","--workspace","/proj"],"enabled":true}
}}`), 0600)
	manifestDir := filepath.Join(tmp, "servers")
	_ = os.MkdirAll(manifestDir, 0755)

	a := NewAPI()
	res, err := a.ScanFrom(ScanOpts{MimoCodeConfigPath: mimoPath, ManifestDir: manifestDir})
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	var hub *ScanEntry
	for i := range res.Entries {
		if res.Entries[i].Name == "my-go-langserver" {
			t.Errorf("non-canonical LSP row should be collapsed into the hub row's LegacyConflict, not stand alone: %v", res.Entries[i].ClientPresence)
		}
		if res.Entries[i].Name == "mcp-language-server-go" {
			hub = &res.Entries[i]
		}
	}
	if hub == nil {
		t.Fatal("hub row mcp-language-server-go missing")
	}
	if hub.LegacyConflict == nil {
		t.Fatalf("non-canonical mimo LSP entry not recognized: hub LegacyConflict is nil (the --lsp go in the command array was not reverse-looked-up)")
	}
	leg, ok := hub.LegacyConflict["mimocode"]
	if !ok {
		t.Fatalf("LegacyConflict[mimocode] missing; keys=%v", keysOfLegacyConflict(hub.LegacyConflict))
	}
	if leg.Transport != "stdio" {
		t.Errorf("LegacyConflict[mimocode].Transport: got %q, want stdio", leg.Transport)
	}
}
