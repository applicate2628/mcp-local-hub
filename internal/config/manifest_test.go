package config

import (
	"path/filepath"
	"runtime"
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

// TestParseManifest_DaemonCwd covers the per-daemon `cwd` field round-trip:
// an absolute cwd is accepted and carried onto DaemonSpec.Cwd, and ${ENV}
// tokens inside it are expanded at parse time (mirroring base_args / env).
// The platform-native absolute prefix is built via filepath so the assertion
// holds on both Windows (C:\...) and POSIX (/...). KnownFields(true) means a
// successful parse also proves the YAML tag is declared (an undeclared `cwd:`
// key would have errored).
func TestParseManifest_DaemonCwd(t *testing.T) {
	base := filepath.Join(t.TempDir(), "wsroot") // platform-native absolute path
	t.Setenv("MCPHUB_CWD_TEST_BASE", base)

	yaml := `
name: t
kind: global
transport: stdio-bridge
command: bash
daemons:
  - name: default
    port: 9999
    cwd: "${MCPHUB_CWD_TEST_BASE}"
`
	m, err := ParseManifest(strings.NewReader(yaml))
	if err != nil {
		t.Fatalf("ParseManifest: %v", err)
	}
	if len(m.Daemons) != 1 {
		t.Fatalf("len(Daemons) = %d, want 1", len(m.Daemons))
	}
	if m.Daemons[0].Cwd != base {
		t.Errorf("Daemons[0].Cwd = %q, want %q (env not expanded?)", m.Daemons[0].Cwd, base)
	}
}

// TestParseManifest_DaemonCwdEmptyOmitted confirms a daemon with no cwd
// declared leaves DaemonSpec.Cwd empty (the inherit-cwd default). Empty must
// pass validation (omitempty / optional).
func TestParseManifest_DaemonCwdEmptyOmitted(t *testing.T) {
	yaml := `
name: t
kind: global
transport: stdio-bridge
command: bash
daemons:
  - name: default
    port: 9999
`
	m, err := ParseManifest(strings.NewReader(yaml))
	if err != nil {
		t.Fatalf("ParseManifest: %v", err)
	}
	if m.Daemons[0].Cwd != "" {
		t.Errorf("Daemons[0].Cwd = %q, want empty (no cwd declared)", m.Daemons[0].Cwd)
	}
}

// TestParseManifest_DaemonCwdRelativeRejected is the validation guard: a
// relative cwd has no stable base across the supervisor / scheduler /
// interactive launch surfaces, so it is rejected at parse time rather than
// silently flowing to cmd.Dir.
func TestParseManifest_DaemonCwdRelativeRejected(t *testing.T) {
	yaml := `
name: t
kind: global
transport: stdio-bridge
command: bash
daemons:
  - name: default
    port: 9999
    cwd: "relative/path"
`
	_, err := ParseManifest(strings.NewReader(yaml))
	if err == nil {
		t.Fatal("expected ParseManifest to reject a relative daemon cwd")
	}
	if !strings.Contains(err.Error(), "absolute path") {
		t.Errorf("error should explain the absolute-path requirement: %v", err)
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

func TestParseManifest_LanguageProjectMarkers(t *testing.T) {
	yaml := `
name: mcp-language-server
kind: workspace-scoped
transport: stdio-bridge
command: mcp-language-server
port_pool: {start: 9200, end: 9299}
languages:
  - name: rust
    backend: mcp-language-server
    transport: stdio
    lsp_command: rust-analyzer
    project_markers: [Cargo.toml]
`
	m, err := ParseManifest(strings.NewReader(yaml))
	if err != nil {
		t.Fatalf("ParseManifest: %v", err)
	}
	if got := m.Languages[0].ProjectMarkers; len(got) != 1 || got[0] != "Cargo.toml" {
		t.Fatalf("ProjectMarkers = %v, want [Cargo.toml]", got)
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

func TestValidateRemoteHTTP_RejectsMalformedHTTPSURL(t *testing.T) {
	for _, tc := range []struct {
		name string
		url  string
	}{
		{"empty host", "https:///mcp"},
		{"embedded credentials", "https://user:pass@mcp.context7.com/mcp"},
		{"control byte", "https://mcp.context7.com/\x00mcp"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := &ServerManifest{
				Name:      "ctx7-bad",
				Kind:      KindGlobal,
				Transport: TransportRemoteHTTP,
				URL:       tc.url,
			}
			if err := m.Validate(); err == nil {
				t.Fatalf("expected malformed URL rejection for %q", tc.url)
			}
		})
	}
}

func TestValidateRemoteHTTP_AllowsLocalhostForHandwrittenManifest(t *testing.T) {
	for _, rawURL := range []string{
		"https://localhost/mcp",
		"https://127.0.0.1/mcp",
		"https://[::1]/mcp",
	} {
		t.Run(rawURL, func(t *testing.T) {
			m := &ServerManifest{
				Name:      "local-remote",
				Kind:      KindGlobal,
				Transport: TransportRemoteHTTP,
				URL:       rawURL,
			}
			if err := m.Validate(); err != nil {
				t.Fatalf("handwritten remote-http manifest should allow %q; got %v", rawURL, err)
			}
		})
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
		name   string
		mutate func(m *ServerManifest)
		want   string
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

// TestParseManifestRequiredBinariesServerLevel pins the Task 1.1
// schema addition: a server-level `required_binaries: [...]` slice
// must round-trip through YAML parse without tripping
// `KnownFields(true)` strictness. The field is free-form metadata
// (no Validate() logic), so the only assertion here is that the
// slice value survives decode.
//
// Spec ref: docs/superpowers/specs/2026-05-19-servers-matrix-lsp-and-env-revamp-design.md §"Manifest schema additions".
func TestParseManifestRequiredBinariesServerLevel(t *testing.T) {
	yaml := `
name: gdb
kind: global
transport: stdio-bridge
command: npx
required_binaries: [gdb]
daemons:
  - name: default
    port: 9999
`
	m, err := ParseManifest(strings.NewReader(yaml))
	if err != nil {
		t.Fatalf("ParseManifest: %v", err)
	}
	if len(m.RequiredBinaries) != 1 || m.RequiredBinaries[0] != "gdb" {
		t.Errorf("RequiredBinaries = %v, want [gdb]", m.RequiredBinaries)
	}
}

// TestParseManifestRequiredBinariesLanguageLevel pins the same
// Task 1.1 schema addition for the per-language slot. Each
// LanguageSpec gains its own optional `required_binaries:` for
// LSP-bridge recognition (e.g. clangd, pyright-langserver).
func TestParseManifestRequiredBinariesLanguageLevel(t *testing.T) {
	yaml := `
name: mcp-language-server
kind: workspace-scoped
transport: stdio-bridge
command: mcp-language-server
port_pool: {start: 9200, end: 9299}
languages:
  - name: cpp
    backend: mcp-language-server
    transport: stdio
    lsp_command: clangd
    required_binaries: [clangd]
`
	m, err := ParseManifest(strings.NewReader(yaml))
	if err != nil {
		t.Fatalf("ParseManifest: %v", err)
	}
	if len(m.Languages) != 1 {
		t.Fatalf("len(Languages) = %d, want 1", len(m.Languages))
	}
	if len(m.Languages[0].RequiredBinaries) != 1 || m.Languages[0].RequiredBinaries[0] != "clangd" {
		t.Errorf("Languages[0].RequiredBinaries = %v, want [clangd]", m.Languages[0].RequiredBinaries)
	}
}

// ===== Phase D.1 + B.1: DaemonTemplate validator tests =====

// validDaemonTemplateManifest returns a minimal valid kind=workspace-scoped
// + daemon_template manifest. Tests mutate the returned value to exercise
// each rejection path.
func validDaemonTemplateManifest() *ServerManifest {
	return &ServerManifest{
		Name:      "serena",
		Kind:      KindWorkspaceScoped,
		Transport: TransportNativeHTTP,
		Command:   "uvx",
		DaemonTemplate: &DaemonTemplate{
			Context:  "codex",
			PortPool: &PortPool{Start: 9121, End: 9199},
			// --context is NOT a template token: it is appended at spawn from
			// DaemonTemplate.Context (design §5). A --context here would double the
			// flag, which Validate now rejects (bot PR #246 r2 P2).
			ExtraArgsTemplate: []string{"--project", "${workspace.path}"},
		},
	}
}

// TestServerManifestValidate_WorkspaceScopedWithDaemonTemplate_Valid pins
// the happy path: a kind=workspace-scoped manifest with daemon_template
// and no legacy fields (no top-level port_pool / languages[] / daemons[])
// validates successfully.
func TestServerManifestValidate_WorkspaceScopedWithDaemonTemplate_Valid(t *testing.T) {
	m := validDaemonTemplateManifest()
	if err := m.Validate(); err != nil {
		t.Fatalf("expected valid manifest; got %v", err)
	}
}

// TestServerManifestValidate_WorkspaceScopedWithDaemonTemplate_RejectsContextInTemplate
// pins the duplicate-context gate (bot PR #246 r2 P2): --context must NOT appear
// in base_args or extra_args_template — it is appended at spawn from
// daemon_template.context, so a token here would materialize a doubled flag.
func TestServerManifestValidate_WorkspaceScopedWithDaemonTemplate_RejectsContextInTemplate(t *testing.T) {
	// --context in extra_args_template → rejected.
	m := validDaemonTemplateManifest()
	m.DaemonTemplate.ExtraArgsTemplate = []string{"--context", "codex", "--project", "${workspace.path}"}
	if err := m.Validate(); err == nil {
		t.Fatal("expected rejection of --context in extra_args_template; got nil")
	} else if !strings.Contains(err.Error(), "--context") {
		t.Errorf("error must mention --context; got %v", err)
	}

	// --context=value (joined form) in extra_args_template → also rejected.
	m2 := validDaemonTemplateManifest()
	m2.DaemonTemplate.ExtraArgsTemplate = []string{"--context=codex", "--project", "${workspace.path}"}
	if err := m2.Validate(); err == nil {
		t.Fatal("expected rejection of --context=value in extra_args_template; got nil")
	}

	// --context in base_args → rejected.
	m3 := validDaemonTemplateManifest()
	m3.BaseArgs = []string{"--context", "codex"}
	if err := m3.Validate(); err == nil {
		t.Fatal("expected rejection of --context in base_args; got nil")
	} else if !strings.Contains(err.Error(), "--context") {
		t.Errorf("error must mention --context; got %v", err)
	}
}

// TestServerManifestValidate_WorkspaceScopedWithDaemonTemplate_RejectsTopLevelPortPool
// pins the mutual-exclusion gate: dynamic-pool manifests must NOT carry
// a top-level port_pool — the pool moves into daemon_template.port_pool.
func TestServerManifestValidate_WorkspaceScopedWithDaemonTemplate_RejectsTopLevelPortPool(t *testing.T) {
	m := validDaemonTemplateManifest()
	m.PortPool = &PortPool{Start: 9200, End: 9299}
	err := m.Validate()
	if err == nil {
		t.Fatal("expected rejection of top-level port_pool with daemon_template; got nil")
	}
	if !strings.Contains(err.Error(), "port_pool") {
		t.Errorf("error must mention port_pool; got %v", err)
	}
	if !strings.Contains(err.Error(), "daemon_template.port_pool") {
		t.Errorf("error must guide operator to daemon_template.port_pool; got %v", err)
	}
}

// TestServerManifestValidate_WorkspaceScopedWithDaemonTemplate_RejectsTopLevelLanguages
// pins the mutual-exclusion gate: dynamic-pool serena is multi-language
// per .serena/project.yml, so the manifest must NOT carry top-level
// languages[].
func TestServerManifestValidate_WorkspaceScopedWithDaemonTemplate_RejectsTopLevelLanguages(t *testing.T) {
	m := validDaemonTemplateManifest()
	m.Languages = []LanguageSpec{
		{Name: "python", Backend: "mcp-language-server", Transport: "stdio", LspCommand: "pyright-langserver"},
	}
	err := m.Validate()
	if err == nil {
		t.Fatal("expected rejection of top-level languages[] with daemon_template; got nil")
	}
	if !strings.Contains(err.Error(), "languages") {
		t.Errorf("error must mention languages; got %v", err)
	}
}

// TestServerManifestValidate_WorkspaceScopedWithDaemonTemplate_RejectsDaemonsListBoth
// pins the migration gate: dynamic-pool manifests drop legacy daemons[]
// entirely; both-present is a migration-incomplete state.
func TestServerManifestValidate_WorkspaceScopedWithDaemonTemplate_RejectsDaemonsListBoth(t *testing.T) {
	m := validDaemonTemplateManifest()
	m.Daemons = []DaemonSpec{{Name: "legacy", Port: 9121}}
	err := m.Validate()
	if err == nil {
		t.Fatal("expected rejection of daemons[] with daemon_template; got nil")
	}
	if !strings.Contains(err.Error(), "daemons[]") {
		t.Errorf("error must mention daemons[]; got %v", err)
	}
	if !strings.Contains(err.Error(), "mutually exclusive") {
		t.Errorf("error must explain mutual-exclusion semantic; got %v", err)
	}
}

// TestServerManifestValidate_DaemonTemplateMissingWorkspacePathToken pins
// the workspace-context gate: the extra_args_template MUST mention
// ${workspace.path} (substring match) so the spawned subprocess gets
// the workspace context. Otherwise workspace identity is silently lost.
func TestServerManifestValidate_DaemonTemplateMissingWorkspacePathToken(t *testing.T) {
	m := validDaemonTemplateManifest()
	m.DaemonTemplate.ExtraArgsTemplate = []string{"--context", "codex", "--no-workspace"}
	err := m.Validate()
	if err == nil {
		t.Fatal("expected rejection of extra_args_template without ${workspace.path}; got nil")
	}
	if !strings.Contains(err.Error(), "${workspace.path}") {
		t.Errorf("error must mention the missing token; got %v", err)
	}
}

// TestServerManifestValidate_DaemonTemplateInvalidPortPoolRange pins
// the daemon_template.port_pool sanity check (start>0, end>=start).
func TestServerManifestValidate_DaemonTemplateInvalidPortPoolRange(t *testing.T) {
	cases := []struct {
		name string
		pool *PortPool
		want string
	}{
		{"missing pool", nil, "port_pool is required"},
		{"zero start", &PortPool{Start: 0, End: 100}, "start>0"},
		{"negative start", &PortPool{Start: -1, End: 100}, "start>0"},
		{"end below start", &PortPool{Start: 9200, End: 9100}, "end>=start"},
		{"end above tcp max", &PortPool{Start: 65535, End: 65536}, "end<=65535"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := validDaemonTemplateManifest()
			m.DaemonTemplate.PortPool = tc.pool
			err := m.Validate()
			if err == nil {
				t.Fatalf("expected rejection of %s; got nil", tc.name)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error must contain %q; got %v", tc.want, err)
			}
		})
	}
}

// TestServerManifestValidate_DaemonTemplateEmptyExtraArgsTemplate pins
// the non-empty check on the args template (separate from the
// ${workspace.path} token requirement — a fully empty list trips the
// non-empty gate first).
func TestServerManifestValidate_DaemonTemplateEmptyExtraArgsTemplate(t *testing.T) {
	m := validDaemonTemplateManifest()
	m.DaemonTemplate.ExtraArgsTemplate = nil
	err := m.Validate()
	if err == nil {
		t.Fatal("expected rejection of empty extra_args_template; got nil")
	}
	if !strings.Contains(err.Error(), "extra_args_template") {
		t.Errorf("error must mention extra_args_template; got %v", err)
	}
	if !strings.Contains(err.Error(), "non-empty") {
		t.Errorf("error must state the non-empty requirement; got %v", err)
	}
}

// TestServerManifestValidate_RejectsDaemonTemplateForKindGlobal pins the
// v6 cross-branch gate (codex BLOCKER): daemon_template under kind=global
// would silently pass through the kind=global branch and return nil. The
// cross-branch gate intercepts before the kind=global branch.
func TestServerManifestValidate_RejectsDaemonTemplateForKindGlobal(t *testing.T) {
	m := validDaemonTemplateManifest()
	m.Kind = KindGlobal
	err := m.Validate()
	if err == nil {
		t.Fatal("expected rejection of daemon_template under kind=global; got nil")
	}
	if !strings.Contains(err.Error(), "daemon_template") {
		t.Errorf("error must name daemon_template; got %v", err)
	}
	if !strings.Contains(err.Error(), "workspace-scoped") {
		t.Errorf("error must guide operator to workspace-scoped; got %v", err)
	}
}

// TestServerManifestValidate_RejectsDaemonTemplateForRemoteHTTP pins the
// v6 cross-branch gate: daemon_template under transport=remote-http is
// nonsensical (no local subprocess to spawn from the template). The
// cross-branch gate fires before the remote-http branch's kind check.
func TestServerManifestValidate_RejectsDaemonTemplateForRemoteHTTP(t *testing.T) {
	// remote-http requires kind=global per existing semantics; set both
	// so we hit the cross-branch gate's transport check, not the
	// transport=remote-http vs kind=workspace-scoped conflict.
	m := validDaemonTemplateManifest()
	m.Kind = KindGlobal
	m.Transport = TransportRemoteHTTP
	m.URL = "https://example.test/mcp"
	m.Command = ""
	err := m.Validate()
	if err == nil {
		t.Fatal("expected rejection of daemon_template under transport=remote-http; got nil")
	}
	if !strings.Contains(err.Error(), "daemon_template") {
		t.Errorf("error must name daemon_template; got %v", err)
	}
	// The cross-branch gate's kind-check fires first when both kind and
	// transport conflict; either error wording satisfies the contract so
	// long as daemon_template is named. Verify the kind-check is what
	// fires here by setting kind=workspace-scoped explicitly: that path
	// reaches the transport-check.
	m2 := validDaemonTemplateManifest()
	m2.Transport = TransportRemoteHTTP
	m2.URL = "https://example.test/mcp"
	// Keep Kind=KindWorkspaceScoped so the kind-check passes; the
	// transport-check is the surface under test.
	err2 := m2.Validate()
	if err2 == nil {
		t.Fatal("expected rejection of daemon_template under transport=remote-http with kind=workspace-scoped; got nil")
	}
	if !strings.Contains(err2.Error(), "remote-http") {
		t.Errorf("error must mention remote-http; got %v", err2)
	}
}

// TestServerManifestValidate_RejectsAtPrefixLanguageName pins the B.1
// dual-gate at the manifest layer: LanguageSpec.Name with '@' prefix is
// rejected so the @serena sentinel cannot collide with a real
// per-language LSP row in workspaces.yaml.
func TestServerManifestValidate_RejectsAtPrefixLanguageName(t *testing.T) {
	m := &ServerManifest{
		Name:      "mcp-language-server",
		Kind:      KindWorkspaceScoped,
		Transport: TransportStdioBridge,
		Command:   "mcp-language-server",
		PortPool:  &PortPool{Start: 9200, End: 9299},
		Languages: []LanguageSpec{
			{Name: "@serena", Backend: "mcp-language-server", Transport: "stdio", LspCommand: "noop"},
		},
	}
	err := m.Validate()
	if err == nil {
		t.Fatal("expected rejection of '@'-prefix language name; got nil")
	}
	if !strings.Contains(err.Error(), "@") {
		t.Errorf("error must mention the '@' prefix; got %v", err)
	}
	if !strings.Contains(err.Error(), "sentinel") {
		t.Errorf("error must explain sentinel-row reservation; got %v", err)
	}
}

func TestServerManifestValidate_LegacyWorkspacePortPoolRejectsEndAboveTCPMax(t *testing.T) {
	m := &ServerManifest{
		Name:      "mcp-language-server",
		Kind:      KindWorkspaceScoped,
		Transport: TransportStdioBridge,
		Command:   "mcp-language-server",
		PortPool:  &PortPool{Start: 65535, End: 65536},
		Languages: []LanguageSpec{
			{Name: "go", Backend: "gopls-mcp", Transport: "stdio", LspCommand: "gopls"},
		},
	}
	err := m.Validate()
	if err == nil {
		t.Fatal("expected rejection of port_pool end above TCP port maximum; got nil")
	}
	if !strings.Contains(err.Error(), "end<=65535") {
		t.Errorf("error must mention TCP maximum; got %v", err)
	}
}

// TestServerManifestValidate_LegacyLSPManifest_StillValidates is the
// regression guard: existing mcp-language-server / gopls-mcp manifests
// (with top-level port_pool + languages[], no daemon_template) must
// continue to validate as before D.1.
func TestServerManifestValidate_LegacyLSPManifest_StillValidates(t *testing.T) {
	m := &ServerManifest{
		Name:      "mcp-language-server",
		Kind:      KindWorkspaceScoped,
		Transport: TransportStdioBridge,
		Command:   "mcp-language-server",
		PortPool:  &PortPool{Start: 9200, End: 9299},
		Languages: []LanguageSpec{
			{Name: "python", Backend: "mcp-language-server", Transport: "stdio", LspCommand: "pyright-langserver"},
			{Name: "go", Backend: "gopls-mcp", Transport: "stdio", LspCommand: "gopls"},
		},
	}
	if err := m.Validate(); err != nil {
		t.Fatalf("legacy LSP manifest must still validate; got %v", err)
	}
}

// TestServerManifestValidate_Companion pins the kind=companion / transport=process
// contract: a valid companion (command + exactly one daemon with an absolute cwd,
// no MCP-shaped fields) validates; every MCP-shaped field and the missing-cwd /
// relative-cwd / kind-transport-mismatch cases are rejected.
func TestServerManifestValidate_Companion(t *testing.T) {
	absCwd := "/opt/excalidraw-canvas"
	if runtime.GOOS == "windows" {
		absCwd = `C:\opt\excalidraw-canvas`
	}
	valid := func() *ServerManifest {
		return &ServerManifest{
			Name:      "excalidraw-canvas",
			Kind:      KindCompanion,
			Transport: TransportProcess,
			Command:   "node",
			BaseArgs:  []string{"dist/server.js"},
			Daemons:   []DaemonSpec{{Name: "default", Cwd: absCwd}},
		}
	}
	if err := valid().Validate(); err != nil {
		t.Fatalf("valid companion must validate; got %v", err)
	}
	cases := []struct {
		name   string
		mutate func(*ServerManifest)
		want   string
	}{
		{"missing command", func(m *ServerManifest) { m.Command = "" }, "requires command"},
		{"client_bindings rejected", func(m *ServerManifest) {
			m.ClientBindings = []ClientBinding{{Client: "claude-code", Daemon: "default"}}
		}, "rejects client_bindings"},
		{"no daemons", func(m *ServerManifest) { m.Daemons = nil }, "exactly one daemons"},
		{"two daemons", func(m *ServerManifest) {
			m.Daemons = append(m.Daemons, DaemonSpec{Name: "second", Cwd: absCwd})
		}, "exactly one daemons"},
		{"daemon missing cwd", func(m *ServerManifest) { m.Daemons[0].Cwd = "" }, "cwd is required"},
		{"relative cwd", func(m *ServerManifest) { m.Daemons[0].Cwd = "relative/path" }, "absolute path"},
		{"transport not process", func(m *ServerManifest) { m.Transport = TransportStdioBridge }, "must be used together"},
		{"port_pool rejected", func(m *ServerManifest) { m.PortPool = &PortPool{Start: 1, End: 2} }, "rejects port_pool"},
		{"languages rejected", func(m *ServerManifest) {
			m.Languages = []LanguageSpec{{Name: "go"}}
		}, "rejects languages"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := valid()
			tc.mutate(m)
			err := m.Validate()
			if err == nil {
				t.Fatalf("expected error containing %q; got nil", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %v; want substring %q", err, tc.want)
			}
		})
	}
	t.Run("process transport requires companion kind", func(t *testing.T) {
		m := valid()
		m.Kind = KindGlobal
		if err := m.Validate(); err == nil || !strings.Contains(err.Error(), "must be used together") {
			t.Errorf("transport=process under kind=global must be rejected; got %v", err)
		}
	})
}

// TestContainsWorkspacePathTokenInArgs_SubstringMatch pins the helper
// contract: substring match (not exact-equality) so operators can write
// composite args like `--project=${workspace.path}/sub`.
func TestContainsWorkspacePathTokenInArgs_SubstringMatch(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want bool
	}{
		{"standalone token", []string{"--project", "${workspace.path}"}, true},
		{"composite arg", []string{"--project=${workspace.path}/src"}, true},
		{"prefix composite", []string{"prefix-${workspace.path}-suffix"}, true},
		{"middle of long arg", []string{"--data=type=path,value=${workspace.path}"}, true},
		{"no token", []string{"--context", "codex", "--no-workspace"}, false},
		{"empty list", nil, false},
		{"empty list explicit", []string{}, false},
		{"empty strings", []string{"", "", ""}, false},
		{"similar but not match", []string{"${workspace_path}", "${workspace.dir}"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := containsWorkspacePathTokenInArgs(tc.args)
			if got != tc.want {
				t.Errorf("containsWorkspacePathTokenInArgs(%v) = %v, want %v", tc.args, got, tc.want)
			}
		})
	}
}

// TestServerManifestParse_DaemonTemplate_StrictKnownFields pins the YAML
// round-trip semantic: a daemon_template block parses cleanly through
// the strict (`KnownFields(true)`) decoder, and unknown fields under
// daemon_template fail strict parse.
func TestServerManifestParse_DaemonTemplate_StrictKnownFields(t *testing.T) {
	t.Run("round-trip preserves template", func(t *testing.T) {
		yaml := `
name: serena
kind: workspace-scoped
transport: native-http
command: uvx
daemon_template:
  context: codex
  port_pool: {start: 9121, end: 9199}
  extra_args_template:
    - --project
    - "${workspace.path}"
`
		m, err := ParseManifest(strings.NewReader(yaml))
		if err != nil {
			t.Fatalf("ParseManifest: %v", err)
		}
		if m.DaemonTemplate == nil {
			t.Fatal("DaemonTemplate must round-trip non-nil")
		}
		if m.DaemonTemplate.Context != "codex" {
			t.Errorf("DaemonTemplate.Context = %q, want codex", m.DaemonTemplate.Context)
		}
		if m.DaemonTemplate.PortPool == nil ||
			m.DaemonTemplate.PortPool.Start != 9121 ||
			m.DaemonTemplate.PortPool.End != 9199 {
			t.Errorf("DaemonTemplate.PortPool = %+v, want {9121,9199}", m.DaemonTemplate.PortPool)
		}
		if len(m.DaemonTemplate.ExtraArgsTemplate) != 2 {
			t.Fatalf("ExtraArgsTemplate len = %d, want 2 (args: %v)", len(m.DaemonTemplate.ExtraArgsTemplate), m.DaemonTemplate.ExtraArgsTemplate)
		}
		if m.DaemonTemplate.ExtraArgsTemplate[1] != "${workspace.path}" {
			t.Errorf("ExtraArgsTemplate[1] = %q, want ${workspace.path}", m.DaemonTemplate.ExtraArgsTemplate[1])
		}
	})
	t.Run("unknown field under daemon_template fails strict parse", func(t *testing.T) {
		yaml := `
name: serena
kind: workspace-scoped
transport: native-http
command: uvx
daemon_template:
  context: codex
  port_pool: {start: 9121, end: 9199}
  extra_args_template: ["${workspace.path}"]
  unknown_field: 42
`
		_, err := ParseManifest(strings.NewReader(yaml))
		if err == nil {
			t.Fatal("expected strict-parse rejection of unknown daemon_template field; got nil")
		}
		// yaml.v3 strict-decode wraps the missing-field error;
		// "field unknown_field not found" is the canonical text.
		if !strings.Contains(err.Error(), "unknown_field") {
			t.Errorf("error must name the offending field; got %v", err)
		}
	})
}

// TestParseManifest_DescriptionIsAdditiveOptional is the §10 v2a guard:
// the new `description:` field must parse onto ServerManifest.Description,
// AND its absence must not break parsing/validation — a pre-v2a manifest
// (no description) parses and validates exactly as before.
func TestParseManifest_DescriptionIsAdditiveOptional(t *testing.T) {
	t.Run("present: parses onto Description", func(t *testing.T) {
		yaml := `
name: time
description: "Time and timezone utilities — current time, conversions, and timezone-aware calculations."
kind: global
transport: stdio-bridge
command: npx
base_args: ["-y", "@mcpcentral/mcp-time@0.0.5"]
daemons: [{name: default, port: 9128}]
`
		m, err := ParseManifest(strings.NewReader(yaml))
		if err != nil {
			t.Fatalf("ParseManifest with description: %v", err)
		}
		want := "Time and timezone utilities — current time, conversions, and timezone-aware calculations."
		if m.Description != want {
			t.Errorf("Description = %q, want %q", m.Description, want)
		}
	})

	t.Run("absent: parses + validates unchanged (empty Description)", func(t *testing.T) {
		yaml := `
name: time
kind: global
transport: stdio-bridge
command: npx
base_args: ["-y", "@mcpcentral/mcp-time@0.0.5"]
daemons: [{name: default, port: 9128}]
`
		m, err := ParseManifest(strings.NewReader(yaml))
		if err != nil {
			t.Fatalf("ParseManifest without description must still succeed: %v", err)
		}
		if m.Description != "" {
			t.Errorf("Description should be empty when omitted, got %q", m.Description)
		}
		// Explicitly confirm Validate() does not require it.
		if err := m.Validate(); err != nil {
			t.Errorf("Validate() must not require description: %v", err)
		}
	})
}

// TestParseCatalogFields projects name/description/kind from manifest YAML
// WITHOUT env expansion / secret resolution / Validate(), so it succeeds
// for shipped manifests whose env or secrets are unset on the test host.
func TestParseCatalogFields(t *testing.T) {
	t.Run("projects name/description/kind", func(t *testing.T) {
		yaml := `
name: serena
description: "Semantic code toolkit — LSP-backed symbol search."
kind: global
transport: native-http
command: uvx
`
		c, err := ParseCatalogFields(strings.NewReader(yaml))
		if err != nil {
			t.Fatalf("ParseCatalogFields: %v", err)
		}
		if c.Name != "serena" {
			t.Errorf("Name = %q, want serena", c.Name)
		}
		if c.Description != "Semantic code toolkit — LSP-backed symbol search." {
			t.Errorf("Description = %q", c.Description)
		}
		if c.Kind != "global" {
			t.Errorf("Kind = %q, want global", c.Kind)
		}
	})

	t.Run("tolerates unset env/secret refs without expansion", func(t *testing.T) {
		// memory's ${HOME} and wolfram's secret: refs would make a full
		// ParseManifest fail on a host without them set; the catalog
		// projection must NOT expand or validate, so it succeeds anyway.
		t.Setenv("HOME", "")
		t.Setenv("USERPROFILE", "")
		yaml := `
name: memory
description: "Persistent knowledge-graph memory."
kind: global
transport: stdio-bridge
command: npx
base_args: ["-y", "@modelcontextprotocol/server-memory"]
env:
  MEMORY_FILE_PATH: "${HOME}/.local/share/mcp-memory/memory.jsonl"
  API_KEY: "secret:some_key"
daemons: [{name: default, port: 9123}]
`
		c, err := ParseCatalogFields(strings.NewReader(yaml))
		if err != nil {
			t.Fatalf("ParseCatalogFields must tolerate unset env/secret refs: %v", err)
		}
		if c.Name != "memory" || c.Description == "" || c.Kind != "global" {
			t.Errorf("unexpected projection: %+v", c)
		}
	})

	t.Run("rejects manifest with no name", func(t *testing.T) {
		yaml := `description: "no name here"` + "\nkind: global\n"
		if _, err := ParseCatalogFields(strings.NewReader(yaml)); err == nil {
			t.Fatal("expected ParseCatalogFields to reject a manifest with no name")
		}
	})
}
