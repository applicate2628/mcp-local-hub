package api

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"mcp-local-hub/internal/clients"
	"mcp-local-hub/internal/config"
)

// TestExtractManifestFromClientPreservesCommandAndEnv asserts that the
// extracted manifest has command/base_args/env matching the original stdio
// entry, plus a port auto-picked and all supported client bindings as defaults.
func TestExtractManifestFromClientPreservesCommandAndEnv(t *testing.T) {
	tmp := t.TempDir()
	cfg := map[string]any{
		"mcpServers": map[string]any{
			"fetch": map[string]any{
				"command": "uvx",
				"args":    []string{"--from", "mcp-server-fetch", "mcp-server-fetch"},
				"env":     map[string]any{"CACHE_DIR": "/tmp/fetch"},
			},
		},
	}
	path := filepath.Join(tmp, ".claude.json")
	b, _ := json.Marshal(cfg)
	_ = os.WriteFile(path, b, 0600)

	a := NewAPI()
	yaml, err := a.ExtractManifestFromClient("claude-code", "fetch", ScanOpts{
		ClaudeConfigPath: path,
		ManifestDir:      t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(yaml, "name: fetch") {
		t.Error("missing name: fetch")
	}
	if !strings.Contains(yaml, "command: uvx") {
		t.Error("missing command: uvx")
	}
	if !strings.Contains(yaml, "CACHE_DIR") {
		t.Error("env CACHE_DIR lost")
	}
	if !strings.Contains(yaml, "client_bindings:") {
		t.Error("missing client_bindings")
	}
}

func TestExtractManifestFromClient_ClaudeCode_AcceptsNoTypeField(t *testing.T) {
	// Older Claude configs sometimes omit `type` for stdio entries.
	// scan.go's existing scanner accepts no-type + command as stdio
	// (scan.go:196-202); extract must agree.
	tmp := t.TempDir()
	claudePath := filepath.Join(tmp, ".claude.json")
	if err := os.WriteFile(claudePath, []byte(
		`{"mcpServers":{"memory":{"command":"npx","args":["-y","mem"],"env":{"DEBUG":"1"}}}}`),
		0600); err != nil {
		t.Fatal(err)
	}
	a := NewAPI()
	yaml, err := a.ExtractManifestFromClient("claude-code", "memory", ScanOpts{
		ClaudeConfigPath: claudePath,
		ManifestDir:      t.TempDir(),
	})
	if err != nil {
		t.Fatalf("ExtractManifestFromClient: %v", err)
	}
	m, err := config.ParseManifest(strings.NewReader(yaml))
	if err != nil {
		t.Fatalf("ParseManifest: %v\nyaml:\n%s", err, yaml)
	}
	if err := m.Validate(); err != nil {
		t.Fatalf("manifest invalid: %v", err)
	}
	if m.Command != "npx" {
		t.Errorf("Command = %q, want npx", m.Command)
	}
}

func TestExtractManifestFromClient_CodexCLI(t *testing.T) {
	tmp := t.TempDir()
	codexPath := filepath.Join(tmp, "config.toml")
	body := `[mcp_servers.memory]
command = "npx"
args = ["-y", "@modelcontextprotocol/server-memory"]

[mcp_servers.memory.env]
DEBUG = "1"
`
	if err := os.WriteFile(codexPath, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	a := NewAPI()
	yaml, err := a.ExtractManifestFromClient("codex-cli", "memory", ScanOpts{
		CodexConfigPath: codexPath,
		ManifestDir:     t.TempDir(),
	})
	if err != nil {
		t.Fatalf("ExtractManifestFromClient: %v", err)
	}
	m, err := config.ParseManifest(strings.NewReader(yaml))
	if err != nil {
		t.Fatalf("ParseManifest: %v\n%s", err, yaml)
	}
	if err := m.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if m.Command != "npx" {
		t.Errorf("Command = %q", m.Command)
	}
	if m.Env["DEBUG"] != "1" {
		t.Errorf("Env[DEBUG] = %q", m.Env["DEBUG"])
	}
}

func TestExtractManifestFromClient_GeminiCLI(t *testing.T) {
	tmp := t.TempDir()
	geminiPath := filepath.Join(tmp, ".gemini", "settings.json")
	_ = os.MkdirAll(filepath.Dir(geminiPath), 0700)
	if err := os.WriteFile(geminiPath, []byte(
		`{"mcpServers":{"memory":{"command":"npx","args":["-y","mem"]}}}`), 0600); err != nil {
		t.Fatal(err)
	}
	a := NewAPI()
	yaml, err := a.ExtractManifestFromClient("gemini-cli", "memory", ScanOpts{
		GeminiConfigPath: geminiPath,
		ManifestDir:      t.TempDir(),
	})
	if err != nil {
		t.Fatalf("ExtractManifestFromClient: %v", err)
	}
	m, err := config.ParseManifest(strings.NewReader(yaml))
	if err != nil {
		t.Fatalf("ParseManifest: %v\n%s", err, yaml)
	}
	if err := m.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if m.Command != "npx" {
		t.Errorf("Command = %q, want npx", m.Command)
	}
}

func TestExtractManifestFromClient_NewJSONClients(t *testing.T) {
	tmp := t.TempDir()
	cases := []struct {
		name   string
		client string
		path   string
		opts   func(string) ScanOpts
		body   string
	}{
		{
			name:   "cursor",
			client: "cursor",
			path:   filepath.Join(tmp, ".cursor", "mcp.json"),
			opts:   func(p string) ScanOpts { return ScanOpts{CursorConfigPath: p, ManifestDir: t.TempDir()} },
			body:   `{"mcpServers":{"memory":{"command":"npx","args":["-y","mem"]}}}`,
		},
		{
			name:   "vscode",
			client: "vscode",
			path:   filepath.Join(tmp, "Code", "User", "mcp.json"),
			opts:   func(p string) ScanOpts { return ScanOpts{VSCodeConfigPath: p, ManifestDir: t.TempDir()} },
			body:   `{"servers":{"memory":{"command":"npx","args":["-y","mem"]}}}`,
		},
		{
			name:   "qwen",
			client: "qwen-cli",
			path:   filepath.Join(tmp, ".qwen", "settings.json"),
			opts:   func(p string) ScanOpts { return ScanOpts{QwenConfigPath: p, ManifestDir: t.TempDir()} },
			body:   `{"mcpServers":{"memory":{"command":"npx","args":["-y","mem"]}}}`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := os.MkdirAll(filepath.Dir(tc.path), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(tc.path, []byte(tc.body), 0600); err != nil {
				t.Fatal(err)
			}
			yaml, err := NewAPI().ExtractManifestFromClient(tc.client, "memory", tc.opts(tc.path))
			if err != nil {
				t.Fatalf("ExtractManifestFromClient: %v", err)
			}
			m, err := config.ParseManifest(strings.NewReader(yaml))
			if err != nil {
				t.Fatalf("ParseManifest: %v\n%s", err, yaml)
			}
			if err := m.Validate(); err != nil {
				t.Fatalf("Validate: %v", err)
			}
			if m.Command != "npx" {
				t.Errorf("Command = %q, want npx", m.Command)
			}
			// The draft binds one row per supported client, derived from the
			// canonical registry (clients.SupportedClientNames) so the count
			// tracks the adapters this build wires up — core + opt-in wave-2.
			supported := clients.SupportedClientNames()
			if len(m.ClientBindings) != len(supported) {
				t.Errorf("len(ClientBindings) = %d, want %d (clients.SupportedClientNames)", len(m.ClientBindings), len(supported))
			}
			// Regression guard: a wave-2 opt-in client must be present, proving
			// the draft no longer hardcodes only the seven core ids.
			haveZed := false
			for _, b := range m.ClientBindings {
				if b.Client == "zed" {
					haveZed = true
					break
				}
			}
			if !haveZed {
				t.Errorf("draft client_bindings missing wave-2 client 'zed'; got %d bindings", len(m.ClientBindings))
			}
		})
	}
}

func TestExtractManifestFromClient_Antigravity_RejectsHubRelay(t *testing.T) {
	tmp := t.TempDir()
	agPath := filepath.Join(tmp, "mcp_config.json")
	if err := os.WriteFile(agPath, []byte(
		`{"mcpServers":{"serena":{"command":"C:/mcphub.exe","args":["relay","--server","serena","--daemon","claude"],"disabled":false}}}`),
		0600); err != nil {
		t.Fatal(err)
	}
	a := NewAPI()
	_, err := a.ExtractManifestFromClient("antigravity", "serena", ScanOpts{
		AntigravityConfigPath: agPath,
		ManifestDir:           t.TempDir(),
	})
	if err == nil {
		t.Fatal("expected rejection for mcphub-relay entry, got nil")
	}
	if !strings.Contains(err.Error(), "relay") {
		t.Errorf("expected error to mention relay, got %v", err)
	}
}

func TestExtractManifestFromClient_Antigravity_AcceptsGenuineStdio(t *testing.T) {
	tmp := t.TempDir()
	agPath := filepath.Join(tmp, "mcp_config.json")
	if err := os.WriteFile(agPath, []byte(
		`{"mcpServers":{"custom":{"command":"/usr/local/bin/custom-mcp","args":["--flag"]}}}`),
		0600); err != nil {
		t.Fatal(err)
	}
	a := NewAPI()
	yaml, err := a.ExtractManifestFromClient("antigravity", "custom", ScanOpts{
		AntigravityConfigPath: agPath,
		ManifestDir:           t.TempDir(),
	})
	if err != nil {
		t.Fatalf("ExtractManifestFromClient: %v", err)
	}
	m, err := config.ParseManifest(strings.NewReader(yaml))
	if err != nil {
		t.Fatalf("ParseManifest: %v\n%s", err, yaml)
	}
	if err := m.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if m.Command != "/usr/local/bin/custom-mcp" {
		t.Errorf("Command = %q", m.Command)
	}
}

func TestExtractManifestFromClient_RejectsHubHTTPOnly_Claude(t *testing.T) {
	// A migrated entry has `url` but no `command`. renderDraftManifestYAML
	// would emit an empty `command:` line and the resulting manifest
	// would fail Validate(). Reject early with an actionable message
	// pointing the operator at demigrate as the likely remedy.
	tmp := t.TempDir()
	claudePath := filepath.Join(tmp, ".claude.json")
	if err := os.WriteFile(claudePath, []byte(
		`{"mcpServers":{"memory":{"type":"http","url":"http://localhost:9200/mcp"}}}`),
		0600); err != nil {
		t.Fatal(err)
	}
	a := NewAPI()
	_, err := a.ExtractManifestFromClient("claude-code", "memory", ScanOpts{
		ClaudeConfigPath: claudePath,
		ManifestDir:      t.TempDir(),
	})
	if err == nil {
		t.Fatal("expected rejection for HTTP-only entry, got nil")
	}
	if !strings.Contains(err.Error(), "no `command`") || !strings.Contains(err.Error(), "demigrate") {
		t.Errorf("expected error to mention missing command + demigrate remedy, got %v", err)
	}
}

func TestExtractManifestFromClient_RejectsHubHTTPOnly_Gemini(t *testing.T) {
	tmp := t.TempDir()
	geminiPath := filepath.Join(tmp, "settings.json")
	if err := os.WriteFile(geminiPath, []byte(
		`{"mcpServers":{"memory":{"url":"http://localhost:9200/mcp","type":"http"}}}`),
		0600); err != nil {
		t.Fatal(err)
	}
	a := NewAPI()
	_, err := a.ExtractManifestFromClient("gemini-cli", "memory", ScanOpts{
		GeminiConfigPath: geminiPath,
		ManifestDir:      t.TempDir(),
	})
	if err == nil {
		t.Fatal("expected rejection for HTTP-only entry, got nil")
	}
	if !strings.Contains(err.Error(), "no `command`") {
		t.Errorf("expected error to mention missing command, got %v", err)
	}
}

func TestExtractManifestFromClient_RejectsHubHTTPOnly_Codex(t *testing.T) {
	tmp := t.TempDir()
	codexPath := filepath.Join(tmp, "config.toml")
	body := `[mcp_servers.memory]
url = "http://localhost:9200/mcp"
`
	if err := os.WriteFile(codexPath, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	a := NewAPI()
	_, err := a.ExtractManifestFromClient("codex-cli", "memory", ScanOpts{
		CodexConfigPath: codexPath,
		ManifestDir:     t.TempDir(),
	})
	if err == nil {
		t.Fatal("expected rejection for HTTP-only entry, got nil")
	}
	if !strings.Contains(err.Error(), "no `command`") {
		t.Errorf("expected error to mention missing command, got %v", err)
	}
}

func TestExtractManifestFromClient_Antigravity_AcceptsRelayFirstArgWithNonMcphubCmd(t *testing.T) {
	// Regression guard: a user could have a genuine stdio server whose
	// first argument happens to be the literal string "relay" (e.g. a
	// custom relay tool at /usr/local/bin/mymcp). The hub-relay reject
	// must check BOTH command (is the mcphub binary) AND args[0]=="relay"
	// — not args[0] alone.
	tmp := t.TempDir()
	agPath := filepath.Join(tmp, "mcp_config.json")
	if err := os.WriteFile(agPath, []byte(
		`{"mcpServers":{"custom":{"command":"/usr/local/bin/mymcp","args":["relay","--target","remote"]}}}`),
		0600); err != nil {
		t.Fatal(err)
	}
	a := NewAPI()
	yaml, err := a.ExtractManifestFromClient("antigravity", "custom", ScanOpts{
		AntigravityConfigPath: agPath,
		ManifestDir:           t.TempDir(),
	})
	if err != nil {
		t.Fatalf("ExtractManifestFromClient: %v (expected accept — command is not mcphub, relay-first-arg alone must not reject)", err)
	}
	m, err := config.ParseManifest(strings.NewReader(yaml))
	if err != nil {
		t.Fatalf("ParseManifest: %v\n%s", err, yaml)
	}
	if err := m.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if m.Command != "/usr/local/bin/mymcp" {
		t.Errorf("Command = %q, want /usr/local/bin/mymcp", m.Command)
	}
	if len(m.BaseArgs) == 0 || m.BaseArgs[0] != "relay" {
		t.Errorf("BaseArgs = %v, want [relay, --target, remote]", m.BaseArgs)
	}
}

func TestRenderDraftManifestYAML_EscapesUntrustedFields(t *testing.T) {
	yaml := renderDraftManifestYAML(
		"good\ntransport: pwned",
		"cmd\nweekly_refresh: true",
		[]string{"ok"},
		map[string]string{"A\nkind: hacked": "v"},
		9121,
	)

	if strings.Count(yaml, "\ntransport:") != 1 {
		t.Fatalf("unexpected transport key injection in output:\n%s", yaml)
	}
	if strings.Count(yaml, "\nweekly_refresh:") != 1 {
		t.Fatalf("unexpected weekly_refresh key injection in output:\n%s", yaml)
	}

	m, err := config.ParseManifest(strings.NewReader(yaml))
	if err != nil {
		t.Fatalf("ParseManifest: %v\n%s", err, yaml)
	}
	if m.Name != "good\ntransport: pwned" {
		t.Fatalf("name changed: got %q", m.Name)
	}
	if m.Command != "cmd\nweekly_refresh: true" {
		t.Fatalf("command changed: got %q", m.Command)
	}
	if m.Env["A\nkind: hacked"] != "v" {
		t.Fatalf("env key/value changed: %#v", m.Env)
	}
}

// TestExtractManifestFromClient_MimoCode covers Finding 5: a mimocode stdio
// entry surfaced in Discovery (from-client=mimocode) must extract to a valid
// draft manifest. MiMoCode (OpenCode format) stores the launch command as an
// ARRAY with no separate `args`, so the extractor must split the array head
// into `command` and the tail into base_args. The config is JSONC-tolerant
// (comments / .jsonc), matching the scan read path.
func TestExtractManifestFromClient_MimoCode(t *testing.T) {
	tmp := t.TempDir()
	mimoPath := filepath.Join(tmp, "mimocode.json")
	if err := os.WriteFile(mimoPath, []byte(
		`{"mcp":{"memory":{"type":"local","command":["npx","-y","@modelcontextprotocol/server-memory"],"env":{"DEBUG":"1"}}}}`),
		0600); err != nil {
		t.Fatal(err)
	}
	a := NewAPI()
	yaml, err := a.ExtractManifestFromClient("mimocode", "memory", ScanOpts{
		MimoCodeConfigPath: mimoPath,
		ManifestDir:        t.TempDir(),
	})
	if err != nil {
		t.Fatalf("ExtractManifestFromClient: %v", err)
	}
	m, err := config.ParseManifest(strings.NewReader(yaml))
	if err != nil {
		t.Fatalf("ParseManifest: %v\n%s", err, yaml)
	}
	if err := m.Validate(); err != nil {
		t.Fatalf("Validate: %v\n%s", err, yaml)
	}
	if m.Command != "npx" {
		t.Errorf("Command = %q, want npx (array head)", m.Command)
	}
	wantArgs := []string{"-y", "@modelcontextprotocol/server-memory"}
	if len(m.BaseArgs) != len(wantArgs) {
		t.Fatalf("BaseArgs = %#v, want %#v (array tail)", m.BaseArgs, wantArgs)
	}
	for i := range wantArgs {
		if m.BaseArgs[i] != wantArgs[i] {
			t.Errorf("BaseArgs[%d] = %q, want %q", i, m.BaseArgs[i], wantArgs[i])
		}
	}
	if m.Env["DEBUG"] != "1" {
		t.Errorf("Env[DEBUG] = %q, want 1", m.Env["DEBUG"])
	}
}

// TestExtractManifestFromClient_MimoCode_JSONC confirms the extractor tolerates
// comments / trailing commas in a hand-edited mimocode.jsonc, same as the scan
// path.
func TestExtractManifestFromClient_MimoCode_JSONC(t *testing.T) {
	tmp := t.TempDir()
	mimoPath := filepath.Join(tmp, "mimocode.jsonc")
	if err := os.WriteFile(mimoPath, []byte(
		"{\n  // operator note\n  \"mcp\": {\n    \"memory\": {\"type\":\"local\",\"command\":[\"uvx\",\"mcp-server-fetch\"],},\n  },\n}\n"),
		0600); err != nil {
		t.Fatal(err)
	}
	a := NewAPI()
	yaml, err := a.ExtractManifestFromClient("mimocode", "memory", ScanOpts{
		MimoCodeConfigPath: mimoPath,
		ManifestDir:        t.TempDir(),
	})
	if err != nil {
		t.Fatalf("ExtractManifestFromClient: %v", err)
	}
	m, err := config.ParseManifest(strings.NewReader(yaml))
	if err != nil {
		t.Fatalf("ParseManifest: %v\n%s", err, yaml)
	}
	if m.Command != "uvx" {
		t.Errorf("Command = %q, want uvx", m.Command)
	}
}

// TestExtractManifestFromClient_MimoCode_RemoteRejected confirms a hub-managed /
// remote (url-only, no command) mimo entry is rejected with the standard
// no-command guidance rather than producing an empty-command manifest.
func TestExtractManifestFromClient_MimoCode_RemoteRejected(t *testing.T) {
	tmp := t.TempDir()
	mimoPath := filepath.Join(tmp, "mimocode.json")
	if err := os.WriteFile(mimoPath, []byte(
		`{"mcp":{"memory":{"type":"remote","url":"http://localhost:9123/mcp","enabled":true}}}`),
		0600); err != nil {
		t.Fatal(err)
	}
	a := NewAPI()
	_, err := a.ExtractManifestFromClient("mimocode", "memory", ScanOpts{
		MimoCodeConfigPath: mimoPath,
		ManifestDir:        t.TempDir(),
	})
	if err == nil {
		t.Fatal("expected an error extracting a remote/url-only mimo entry, got nil")
	}
	if !strings.Contains(err.Error(), "no `command`") {
		t.Errorf("error = %q, want the no-command guidance", err.Error())
	}
}
