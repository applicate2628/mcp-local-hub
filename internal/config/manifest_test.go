package config

import (
	"strings"
	"testing"
)

func TestParseManifest_ExpandsEnvInBaseArgsAndEnv(t *testing.T) {
	t.Setenv("HOME", "/tmp/test-home")
	t.Setenv("MY_TEST_VAR", "MY_VALUE")

	yaml := `
name: t
kind: global
transport: stdio-bridge
command: bash
base_args:
  - "${HOME}/script.sh"
  - "literal"
env:
  CONFIG_PATH: "${HOME}/.config/t.yaml"
  PASSTHROUGH: "${MY_TEST_VAR}"
daemons:
  - name: default
    port: 9999
`
	m, err := ParseManifest(strings.NewReader(yaml))
	if err != nil {
		t.Fatalf("ParseManifest: %v", err)
	}
	if m.BaseArgs[0] != "/tmp/test-home/script.sh" {
		t.Errorf("BaseArgs[0] = %q, want /tmp/test-home/script.sh", m.BaseArgs[0])
	}
	if m.BaseArgs[1] != "literal" {
		t.Errorf("BaseArgs[1] = %q, want literal", m.BaseArgs[1])
	}
	if m.Env["CONFIG_PATH"] != "/tmp/test-home/.config/t.yaml" {
		t.Errorf("Env[CONFIG_PATH] = %q", m.Env["CONFIG_PATH"])
	}
	if m.Env["PASSTHROUGH"] != "MY_VALUE" {
		t.Errorf("Env[PASSTHROUGH] = %q", m.Env["PASSTHROUGH"])
	}
}

// TestParseManifest_MissingEnvIsErrorNotSilentEmpty is the regression
// guard for the finding 'manifest env expansion returns empty string
// up to resolver validation'. Previously expandEnvCrossPlatform
// silently substituted "" when a ${VAR} reference had no value; that
// collapsed e.g. 'MEMORY_FILE_PATH: ${HOME}/.local/share/mcp-memory/x'
// into '/.local/share/mcp-memory/x' on a bare Windows shell where
// neither HOME nor USERPROFILE was set — and the daemon wrote its
// memory.jsonl to the root of the system drive. Now the reference
// must resolve; otherwise ParseManifest rejects the manifest with an
// actionable error listing every missing variable.
func TestParseManifest_MissingEnvIsErrorNotSilentEmpty(t *testing.T) {
	t.Setenv("HOME", "")
	t.Setenv("USERPROFILE", "")
	t.Setenv("TOTALLY_UNSET_VAR", "")

	yaml := `
name: t
kind: global
transport: stdio-bridge
command: bash
base_args: ["${TOTALLY_UNSET_VAR}/script.sh"]
daemons: [{name: default, port: 9999}]
`
	_, err := ParseManifest(strings.NewReader(yaml))
	if err == nil {
		t.Fatal("expected ParseManifest to reject manifest with unresolved ${VAR}")
	}
	if !strings.Contains(err.Error(), "TOTALLY_UNSET_VAR") {
		t.Errorf("error should name the missing variable: %v", err)
	}
}

func TestParseManifest_HOMEFallsBackToUserProfile(t *testing.T) {
	// Cross-platform niceness: ${HOME} on Windows where HOME is unset
	// should still resolve via USERPROFILE so the same manifest works
	// from cmd.exe / PowerShell as well as bash.
	t.Setenv("HOME", "")
	t.Setenv("USERPROFILE", "C:/Users/probe")

	yaml := `
name: t
kind: global
transport: stdio-bridge
command: bash
base_args: ["${HOME}/x"]
daemons: [{name: default, port: 9999}]
`
	m, err := ParseManifest(strings.NewReader(yaml))
	if err != nil {
		t.Fatalf("ParseManifest: %v", err)
	}
	if m.BaseArgs[0] != "C:/Users/probe/x" {
		t.Errorf("BaseArgs[0] = %q, want C:/Users/probe/x (HOME→USERPROFILE fallback failed)", m.BaseArgs[0])
	}
}

func TestParseManifest_SerenaMinimal(t *testing.T) {
	yaml := `
name: serena
kind: global
transport: native-http
command: uvx
base_args: [--from, git+https://github.com/oraios/serena@f0a3a279b7c48d28b9e7e4aea1ed9caed846906b, serena, start-mcp-server]
daemons:
  - name: claude
    context: claude-code
    port: 9121
    extra_args: [--context, claude-code, --transport, streamable-http]
client_bindings:
  - client: claude-code
    daemon: claude
    url_path: /mcp
weekly_refresh: false
`
	m, err := ParseManifest(strings.NewReader(yaml))
	if err != nil {
		t.Fatalf("ParseManifest: %v", err)
	}
	if m.Name != "serena" {
		t.Errorf("Name = %q, want serena", m.Name)
	}
	if m.Kind != "global" {
		t.Errorf("Kind = %q, want global", m.Kind)
	}
	if len(m.Daemons) != 1 {
		t.Fatalf("len(Daemons) = %d, want 1", len(m.Daemons))
	}
	if m.Daemons[0].Port != 9121 {
		t.Errorf("Daemons[0].Port = %d, want 9121", m.Daemons[0].Port)
	}
	if m.WeeklyRefresh {
		t.Error("WeeklyRefresh = true, want false")
	}
}

func TestParseManifest_MissingName(t *testing.T) {
	yaml := `kind: global`
	_, err := ParseManifest(strings.NewReader(yaml))
	if err == nil {
		t.Fatal("expected error for missing name, got nil")
	}
	if !strings.Contains(err.Error(), "name") {
		t.Errorf("error should mention 'name', got: %v", err)
	}
}

func TestParseManifest_InvalidKind(t *testing.T) {
	yaml := `
name: foo
kind: nonsense
transport: native-http
command: echo
`
	_, err := ParseManifest(strings.NewReader(yaml))
	if err == nil {
		t.Fatal("expected error for invalid kind, got nil")
	}
}

func TestParseManifest_WorkspaceScopedSchema(t *testing.T) {
	yaml := `
name: mcp-language-server
kind: workspace-scoped
transport: stdio-bridge
command: mcp-language-server
port_pool:
  start: 9200
  end: 9299
languages:
  - name: python
    backend: mcp-language-server
    transport: stdio
    lsp_command: pyright-langserver
    extra_flags: ["--stdio"]
  - name: go
    backend: gopls-mcp
    transport: stdio
    lsp_command: gopls
    extra_flags: ["mcp"]
weekly_refresh: false
`
	m, err := ParseManifest(strings.NewReader(yaml))
	if err != nil {
		t.Fatalf("ParseManifest: %v", err)
	}
	if m.Kind != KindWorkspaceScoped {
		t.Errorf("Kind = %q, want workspace-scoped", m.Kind)
	}
	if m.PortPool == nil || m.PortPool.Start != 9200 || m.PortPool.End != 9299 {
		t.Errorf("PortPool = %+v, want {9200,9299}", m.PortPool)
	}
	if len(m.Languages) != 2 {
		t.Fatalf("len(Languages) = %d, want 2", len(m.Languages))
	}
	if m.Languages[0].Backend != "mcp-language-server" {
		t.Errorf("Languages[0].Backend = %q", m.Languages[0].Backend)
	}
	if m.Languages[1].Backend != "gopls-mcp" {
		t.Errorf("Languages[1].Backend = %q", m.Languages[1].Backend)
	}
	if m.Languages[0].Transport != "stdio" {
		t.Errorf("Languages[0].Transport = %q, want stdio", m.Languages[0].Transport)
	}
}

func TestParseManifest_LanguageTransportDefault(t *testing.T) {
	// transport omitted -> defaults to "stdio"
	yaml := `
name: mcp-language-server
kind: workspace-scoped
transport: stdio-bridge
command: mcp-language-server
port_pool: {start: 9200, end: 9299}
languages:
  - name: python
    backend: mcp-language-server
    lsp_command: pyright-langserver
`
	m, err := ParseManifest(strings.NewReader(yaml))
	if err != nil {
		t.Fatalf("ParseManifest: %v", err)
	}
	if m.Languages[0].Transport != "stdio" {
		t.Errorf("Transport default = %q, want stdio", m.Languages[0].Transport)
	}
}

func TestParseManifest_LanguageTransportEnum(t *testing.T) {
	yaml := `
name: mcp-language-server
kind: workspace-scoped
transport: stdio-bridge
command: mcp-language-server
port_pool: {start: 9200, end: 9299}
languages:
  - name: python
    backend: mcp-language-server
    transport: something-unknown
    lsp_command: pyright-langserver
`
	_, err := ParseManifest(strings.NewReader(yaml))
	if err == nil {
		t.Fatal("expected error for unknown transport value")
	}
	if !strings.Contains(err.Error(), "transport") {
		t.Errorf("error should mention transport: %v", err)
	}
}

func TestParseManifest_WorkspaceScopedRejectsMissingPortPool(t *testing.T) {
	yaml := `
name: mcp-language-server
kind: workspace-scoped
transport: stdio-bridge
command: mcp-language-server
languages:
  - name: python
    backend: mcp-language-server
    lsp_command: pyright-langserver
`
	_, err := ParseManifest(strings.NewReader(yaml))
	if err == nil {
		t.Fatal("expected error for workspace-scoped manifest without port_pool")
	}
	if !strings.Contains(err.Error(), "port_pool") {
		t.Errorf("error should mention port_pool: %v", err)
	}
}

// ===== G6 remote-http transport tests =====

// TestParseManifest_RemoteHTTPHappyPath pins the G6 schema additions
// (URL + Headers + transport=remote-http). The manifest carries
// ${secret:KEY} placeholders cleartext-free; expansion happens at
// install time.
func TestParseManifest_RemoteHTTPHappyPath(t *testing.T) {
	yaml := `
name: context7
kind: global
transport: remote-http
url: https://mcp.context7.com/mcp
headers:
  Authorization: "Bearer ${secret:CONTEXT7_TOKEN}"
  X-Tenant: acme
client_bindings:
  - client: claude-code
  - client: codex-cli
`
	m, err := ParseManifest(strings.NewReader(yaml))
	if err != nil {
		t.Fatalf("ParseManifest: %v", err)
	}
	if m.Transport != "remote-http" {
		t.Errorf("Transport = %q, want remote-http", m.Transport)
	}
	if m.URL != "https://mcp.context7.com/mcp" {
		t.Errorf("URL = %q, want https://mcp.context7.com/mcp", m.URL)
	}
	if m.Headers["Authorization"] != "Bearer ${secret:CONTEXT7_TOKEN}" {
		t.Errorf("Authorization header lost ${secret:KEY} placeholder: %q", m.Headers["Authorization"])
	}
	if m.Headers["X-Tenant"] != "acme" {
		t.Errorf("X-Tenant header = %q", m.Headers["X-Tenant"])
	}
}

// TestValidateRemoteHTTP_RejectsPlaintextURL pins the G6 §"Validation
// rules" plaintext-URL rejection. Plain http:// is out of scope —
// operators must TLS-terminate.
func TestValidateRemoteHTTP_RejectsPlaintextURL(t *testing.T) {
	m := &ServerManifest{
		Name:      "ctx7-bad",
		Kind:      KindGlobal,
		Transport: TransportRemoteHTTP,
		URL:       "http://insecure.example/mcp",
	}
	err := m.Validate()
	if err == nil {
		t.Fatal("expected plaintext-URL rejection; got nil")
	}
	if !strings.Contains(err.Error(), "https://") {
		t.Errorf("error must mention https:// for operator guidance; got %v", err)
	}
}

// TestValidateRemoteHTTP_RejectsWorkspaceScoped pins codex bot r8
// P2 closure (PR #169): the workspace-scoped kind is per-(workspace,
// language) lazy-proxy with required local LSP backends +
// port_pool. None of that maps onto a remote-only transport, so
// the combination must be rejected with a clear error instead of
// silently passing as accepted-but-nonfunctional.
func TestValidateRemoteHTTP_RejectsWorkspaceScoped(t *testing.T) {
	m := &ServerManifest{
		Name:      "ws-remote",
		Kind:      KindWorkspaceScoped,
		Transport: TransportRemoteHTTP,
		URL:       "https://mcp.context7.com/mcp",
	}
	err := m.Validate()
	if err == nil {
		t.Fatal("expected workspace-scoped + remote-http rejection; got nil")
	}
	if !strings.Contains(err.Error(), "workspace-scoped") {
		t.Errorf("error must name the offending kind for operator guidance; got %v", err)
	}
	if !strings.Contains(err.Error(), "remote-http") {
		t.Errorf("error must name the offending transport for operator guidance; got %v", err)
	}
}

// TestValidateRemoteHTTP_RequiresURL pins that transport=remote-http
// without url: is rejected.
func TestValidateRemoteHTTP_RequiresURL(t *testing.T) {
	m := &ServerManifest{
		Name:      "no-url",
		Kind:      KindGlobal,
		Transport: TransportRemoteHTTP,
	}
	err := m.Validate()
	if err == nil {
		t.Fatal("expected url-required rejection; got nil")
	}
	if !strings.Contains(err.Error(), "url:") {
		t.Errorf("error must mention url: for operator guidance; got %v", err)
	}
}

// TestValidateRemoteHTTP_RejectsConflictingFields pins that fields
// scoped to local-subprocess transports (command, base_args, env,
// daemons, languages, port_pool, idle_timeout_min) are REJECTED on
// a remote-http manifest. Silent ignore would let malformed
// manifests slip through (G6 spec §"Validation rules" + codex bot
// P2 r1 closure on PR #152).
func TestValidateRemoteHTTP_RejectsConflictingFields(t *testing.T) {
	base := func() *ServerManifest {
		return &ServerManifest{
			Name:      "ctx7",
			Kind:      KindGlobal,
			Transport: TransportRemoteHTTP,
			URL:       "https://mcp.context7.com/mcp",
		}
	}
	cases := []struct {
		name  string
		mutate func(m *ServerManifest)
		want  string
	}{
		{"command", func(m *ServerManifest) { m.Command = "npx" }, "command"},
		{"base_args", func(m *ServerManifest) { m.BaseArgs = []string{"foo"} }, "base_args"},
		{"env", func(m *ServerManifest) { m.Env = map[string]string{"K": "V"} }, "env"},
		{"daemons", func(m *ServerManifest) { m.Daemons = []DaemonSpec{{Name: "d", Port: 9100}} }, "daemons"},
		{"languages", func(m *ServerManifest) {
			m.Languages = []LanguageSpec{{Name: "go", Backend: "gopls-mcp", Transport: "stdio", LspCommand: "gopls"}}
		}, "languages"},
		{"port_pool", func(m *ServerManifest) { m.PortPool = &PortPool{Start: 9100, End: 9200} }, "port_pool"},
		{"idle_timeout_min", func(m *ServerManifest) { m.IdleTimeoutMin = 30 }, "idle_timeout_min"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := base()
			tc.mutate(m)
			err := m.Validate()
			if err == nil {
				t.Fatalf("expected rejection of %s on remote-http manifest; got nil", tc.name)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error must name %q for operator forensics; got %v", tc.want, err)
			}
		})
	}
}

// TestParseManifest_RejectsURLKeyOnNonRemoteHTTP pins codex bot r5
// P2 closure (PR #169): mentioning `url:` (with any value — null,
// empty string, or absent value) on a stdio-bridge/native-http
// manifest must be rejected at PARSE time. After decode-into-struct,
// `url: ""`/`url: null`/bare `url:` are indistinguishable from
// absent; ParseManifest does a second pass into map[string]any to
// detect key presence.
func TestParseManifest_RejectsURLKeyOnNonRemoteHTTP(t *testing.T) {
	cases := []struct {
		name string
		yaml string
	}{
		{"url: empty string", "name: x\nkind: global\ntransport: stdio-bridge\ncommand: echo\nurl: \"\"\n"},
		{"url: null", "name: x\nkind: global\ntransport: stdio-bridge\ncommand: echo\nurl: null\n"},
		{"url: bare key", "name: x\nkind: global\ntransport: stdio-bridge\ncommand: echo\nurl:\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseManifest(strings.NewReader(tc.yaml))
			if err == nil {
				t.Fatal("expected url-key rejection at parse time; got nil")
			}
			if !strings.Contains(err.Error(), "url") {
				t.Errorf("error must mention url for operator forensics; got %v", err)
			}
		})
	}
}

// TestParseManifest_RejectsHeadersKeyOnNonRemoteHTTP — same as
// above for headers:.
func TestParseManifest_RejectsHeadersKeyOnNonRemoteHTTP(t *testing.T) {
	cases := []struct {
		name string
		yaml string
	}{
		{"headers: empty map", "name: x\nkind: global\ntransport: stdio-bridge\ncommand: echo\nheaders: {}\n"},
		{"headers: null", "name: x\nkind: global\ntransport: stdio-bridge\ncommand: echo\nheaders: null\n"},
		{"headers: bare key", "name: x\nkind: global\ntransport: stdio-bridge\ncommand: echo\nheaders:\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseManifest(strings.NewReader(tc.yaml))
			if err == nil {
				t.Fatal("expected headers-key rejection at parse time; got nil")
			}
			if !strings.Contains(err.Error(), "headers") {
				t.Errorf("error must mention headers for operator forensics; got %v", err)
			}
		})
	}
}

// TestParseManifest_RejectsLocalSubprocessKeysOnRemoteHTTP pins
// codex bot r9 P2 closure (PR #169): the symmetric guard. The
// remote-http branch in Validate only rejects NON-ZERO values
// (e.g. m.Command != ""), so YAML mentioning `command:`,
// `command: null`, `base_args:`, `env:`, `daemons:` with explicit-
// empty values would bypass the gate. ParseManifest's second-pass
// keyed scan catches the mentioning regardless of value.
func TestParseManifest_RejectsLocalSubprocessKeysOnRemoteHTTP(t *testing.T) {
	cases := []struct {
		name string
		key  string
		yaml string
	}{
		{"command empty", "command", "name: ctx7\nkind: global\ntransport: remote-http\nurl: https://x.example\ncommand: \"\"\n"},
		{"command null", "command", "name: ctx7\nkind: global\ntransport: remote-http\nurl: https://x.example\ncommand: null\n"},
		{"command bare key", "command", "name: ctx7\nkind: global\ntransport: remote-http\nurl: https://x.example\ncommand:\n"},
		{"base_args empty list", "base_args", "name: ctx7\nkind: global\ntransport: remote-http\nurl: https://x.example\nbase_args: []\n"},
		{"base_args bare", "base_args", "name: ctx7\nkind: global\ntransport: remote-http\nurl: https://x.example\nbase_args:\n"},
		{"env empty map", "env", "name: ctx7\nkind: global\ntransport: remote-http\nurl: https://x.example\nenv: {}\n"},
		{"env bare", "env", "name: ctx7\nkind: global\ntransport: remote-http\nurl: https://x.example\nenv:\n"},
		{"daemons empty list", "daemons", "name: ctx7\nkind: global\ntransport: remote-http\nurl: https://x.example\ndaemons: []\n"},
		{"daemons bare", "daemons", "name: ctx7\nkind: global\ntransport: remote-http\nurl: https://x.example\ndaemons:\n"},
		{"languages bare", "languages", "name: ctx7\nkind: global\ntransport: remote-http\nurl: https://x.example\nlanguages:\n"},
		{"port_pool bare", "port_pool", "name: ctx7\nkind: global\ntransport: remote-http\nurl: https://x.example\nport_pool:\n"},
		{"idle_timeout_min zero", "idle_timeout_min", "name: ctx7\nkind: global\ntransport: remote-http\nurl: https://x.example\nidle_timeout_min: 0\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseManifest(strings.NewReader(tc.yaml))
			if err == nil {
				t.Fatalf("expected %s-key rejection at parse time; got nil", tc.key)
			}
			if !strings.Contains(err.Error(), tc.key) {
				t.Errorf("error must name %q for operator forensics; got %v", tc.key, err)
			}
		})
	}
}

// TestValidate_RejectsExplicitEmptyHeadersOnNonRemoteHTTP pins
// codex bot r4 P2 closure (PR #169): YAML `headers: {}` decodes
// as a non-nil empty map; the pre-fix `len(Headers) != 0` check
// silently accepted that explicit-empty case. Switch to
// `Headers != nil` so the field assertion fires whenever the YAML
// MENTIONS headers (with or without entries) under a non-
// remote-http transport.
func TestValidate_RejectsExplicitEmptyHeadersOnNonRemoteHTTP(t *testing.T) {
	for _, tp := range []string{TransportStdioBridge, TransportNativeHTTP} {
		t.Run(tp, func(t *testing.T) {
			m := &ServerManifest{
				Name:      "x",
				Kind:      KindGlobal,
				Transport: tp,
				Command:   "npx",
				Headers:   map[string]string{}, // explicit non-nil empty
			}
			err := m.Validate()
			if err == nil {
				t.Fatal("expected rejection of explicit empty headers map; got nil")
			}
			if !strings.Contains(err.Error(), "headers") {
				t.Errorf("error must mention headers; got %v", err)
			}
		})
	}
}

// TestValidate_RejectsURLOnNonRemoteHTTP pins the symmetric guard:
// the new URL field is REJECTED on stdio-bridge / native-http
// transports. Silent acceptance would let a malformed manifest carry
// dead fields.
func TestValidate_RejectsURLOnNonRemoteHTTP(t *testing.T) {
	for _, tp := range []string{TransportStdioBridge, TransportNativeHTTP} {
		t.Run(tp, func(t *testing.T) {
			m := &ServerManifest{
				Name:      "x",
				Kind:      KindGlobal,
				Transport: tp,
				Command:   "npx",
				URL:       "https://nope.example",
			}
			err := m.Validate()
			if err == nil {
				t.Fatal("expected URL-on-non-remote rejection; got nil")
			}
			if !strings.Contains(err.Error(), "url") {
				t.Errorf("error must mention url; got %v", err)
			}
		})
	}
}
