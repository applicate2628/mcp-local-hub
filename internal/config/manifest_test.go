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
