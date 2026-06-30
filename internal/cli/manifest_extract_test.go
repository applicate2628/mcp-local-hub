package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"mcp-local-hub/internal/config"
)

// TestManifestExtractSupportedClients_IncludesMimocode pins bot PR #420 finding
// 6: `mcphub manifest extract` must offer mimocode (the extract command wires a
// MimoCodeConfigPath, so mimocode IS a supported source), and the required-client
// error + --client flag help render from a SINGLE owner so the two can't drift
// apart again. Before this fix both strings listed only the old 4 clients.
func TestManifestExtractSupportedClients_IncludesMimocode(t *testing.T) {
	if !slices.Contains(manifestExtractSupportedClients, "mimocode") {
		t.Fatalf("mimocode missing from manifestExtractSupportedClients: %v", manifestExtractSupportedClients)
	}
	// The other clients the extract command wires a ScanOpts path for must also
	// be present (claude-code, codex-cli, gemini-cli, antigravity).
	for _, want := range []string{"claude-code", "codex-cli", "gemini-cli", "antigravity"} {
		if !slices.Contains(manifestExtractSupportedClients, want) {
			t.Errorf("extract-supported list missing %q: %v", want, manifestExtractSupportedClients)
		}
	}
	help := manifestExtractClientsHelp()
	if !strings.Contains(help, "mimocode") {
		t.Errorf("extract --client help omits mimocode: %q", help)
	}
	// The single-owner help string must be the source for BOTH the error and the
	// flag help — assert the flag help on the built command renders it.
	cmd := newManifestExtractCmd()
	flag := cmd.Flags().Lookup("client")
	if flag == nil {
		t.Fatal("extract command has no --client flag")
	}
	if !strings.Contains(flag.Usage, "mimocode") {
		t.Errorf("--client flag usage omits mimocode: %q", flag.Usage)
	}
	// The flag usage must render from the same single-owner help (no drift).
	if !strings.Contains(flag.Usage, help) {
		t.Errorf("--client flag usage %q does not embed the single-owner client list %q", flag.Usage, help)
	}
}

func TestManifestExtractSupportedClients_IncludesOpenCode(t *testing.T) {
	if !slices.Contains(manifestExtractSupportedClients, "opencode") {
		t.Fatalf("opencode missing from manifestExtractSupportedClients: %v", manifestExtractSupportedClients)
	}
	help := manifestExtractClientsHelp()
	if !strings.Contains(help, "opencode") {
		t.Errorf("extract --client help omits opencode: %q", help)
	}
	cmd := newManifestExtractCmd()
	flag := cmd.Flags().Lookup("client")
	if flag == nil {
		t.Fatal("extract command has no --client flag")
	}
	if !strings.Contains(flag.Usage, "opencode") {
		t.Errorf("--client flag usage omits opencode: %q", flag.Usage)
	}
}

func TestManifestExtract_OpenCodeCommandArrayAndEnvironment(t *testing.T) {
	tmp := t.TempDir()
	home := filepath.Join(tmp, "home")
	xdg := filepath.Join(tmp, "xdg")
	localAppData := filepath.Join(tmp, "localappdata")
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("XDG_CONFIG_HOME", xdg)
	t.Setenv("LOCALAPPDATA", localAppData)

	opencodeDir := filepath.Join(xdg, "opencode")
	if err := os.MkdirAll(opencodeDir, 0755); err != nil {
		t.Fatal(err)
	}
	opencodePath := filepath.Join(opencodeDir, "opencode.json")
	cfg := `{"mcp":{"fetch":{"type":"local","command":["uvx","mcp-server-fetch"],"environment":{"FETCH_TOKEN":"secret-x"},"enabled":true}}}`
	if err := os.WriteFile(opencodePath, []byte(cfg), 0600); err != nil {
		t.Fatal(err)
	}

	cmd := newManifestExtractCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"fetch", "--client", "opencode"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("manifest extract opencode: %v", err)
	}
	m, err := config.ParseManifest(strings.NewReader(out.String()))
	if err != nil {
		t.Fatalf("ParseManifest: %v\n%s", err, out.String())
	}
	if m.Command != "uvx" {
		t.Errorf("Command = %q, want uvx", m.Command)
	}
	if len(m.BaseArgs) != 1 || m.BaseArgs[0] != "mcp-server-fetch" {
		t.Errorf("BaseArgs = %v, want [mcp-server-fetch]", m.BaseArgs)
	}
	if m.Env["FETCH_TOKEN"] != "secret-x" {
		t.Errorf("env not translated from OpenCode environment: %#v", m.Env)
	}
}
