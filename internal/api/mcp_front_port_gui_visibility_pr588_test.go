// internal/api/mcp_front_port_gui_visibility_pr588_test.go
//
// These two tests keep the mcp_front.port registry comment honest across the
// Go settings owner and its two management surfaces:
//
//   - the surface that DOES work (the settings CLI/registry path) is pinned
//     positively, so "CLI-managed" is a verified statement rather than
//     another assertion nobody checked;
//   - the explicit Advanced-section frontend binding is pinned by a positive
//     cross-language drift gate, so removing any link from the literal key to
//     the typed registry definition, shared save flow, or rendered input fails.
package api

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// TestMCPFrontPortSetting_IsManageableThroughTheSettingsCLI pins the
// management surface the corrected registry comment names.
//
// `mcphub settings {list,get,set}` is registry-driven, so this exercises the
// same code path the CLI does: the key must appear in the listing with its
// registry metadata, and a set/get round-trip must persist and validate.
func TestMCPFrontPortSetting_IsManageableThroughTheSettingsCLI(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("LOCALAPPDATA", tmp)
	t.Setenv("USERPROFILE", tmp)
	t.Setenv("HOME", tmp)
	t.Cleanup(SetDaemonStateRootForTest(tmp))

	a := NewAPI()

	settings, err := a.SettingsList()
	if err != nil {
		t.Fatalf("SettingsList: %v", err)
	}
	if _, ok := settings[MCPFrontPortSettingKey]; !ok {
		t.Fatalf("%s must appear in SettingsList (that map IS what `mcphub settings list` renders); keys=%d", MCPFrontPortSettingKey, len(settings))
	}
	def := findDef(MCPFrontPortSettingKey)
	if def == nil {
		t.Fatalf("%s must have a registry definition (that is what makes it CLI-manageable)", MCPFrontPortSettingKey)
	}
	if def.Section != "advanced" {
		t.Fatalf("%s section = %q, want \"advanced\" (the registry comment documents this placement and why)", MCPFrontPortSettingKey, def.Section)
	}
	if def.Default != strconv.Itoa(DefaultMCPFrontPort) {
		t.Fatalf("%s default = %q, want %d", MCPFrontPortSettingKey, def.Default, DefaultMCPFrontPort)
	}

	const newPort = 9236
	if err := a.SettingsSet(MCPFrontPortSettingKey, strconv.Itoa(newPort)); err != nil {
		t.Fatalf("SettingsSet %s: %v", MCPFrontPortSettingKey, err)
	}
	got, err := a.SettingsGet(MCPFrontPortSettingKey)
	if err != nil {
		t.Fatalf("SettingsGet %s: %v", MCPFrontPortSettingKey, err)
	}
	if strings.TrimSpace(got) != strconv.Itoa(newPort) {
		t.Fatalf("set/get round-trip for %s: got %q, want %q", MCPFrontPortSettingKey, got, strconv.Itoa(newPort))
	}
	resolved, rerr := a.ResolveMCPFrontPort()
	if rerr != nil || resolved != newPort {
		t.Fatalf("the resolver must observe the CLI-set value: got (%d, %v), want (%d, nil)", resolved, rerr, newPort)
	}
	// Range validation must still be enforced through the same surface.
	if err := a.SettingsSet(MCPFrontPortSettingKey, "80"); err == nil {
		t.Fatalf("SettingsSet must reject a below-min port through the registry validator; it accepted 80")
	}
}

// TestSectionAdvanced_RendersMCPFrontPortThroughExplicitControl is the
// positive cross-language drift gate for the registry comment. The frontend
// has no Go import boundary, so this test pins the complete source-level
// binding instead: literal key, Advanced-section ownership, typed registry
// lookup, shared save flow, and the rendered input's read/write path.
func TestSectionAdvanced_RendersMCPFrontPortThroughExplicitControl(t *testing.T) {
	path := filepath.Join("..", "gui", "frontend", "src", "components", "settings", "SectionAdvanced.tsx")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read frontend source at %s: %v", path, err)
	}
	src := strings.Join(strings.Fields(string(raw)), " ")

	markers := []struct {
		name string
		text string
	}{
		{name: "literal registry key", text: `const MCP_FRONT_PORT_KEY = "mcp_front.port";`},
		{name: "Advanced section ownership", text: `const SECTION_KEYS = [MCP_FRONT_PORT_KEY];`},
		{name: "shared section save flow", text: `useSectionSaveFlow(snapshot, SECTION_KEYS, onDirtyChange)`},
		{name: "typed registry definition lookup", text: `setting.key === MCP_FRONT_PORT_KEY && setting.type === "int"`},
		{name: "rendered control", text: `data-testid="mcp-front-port-input"`},
		{name: "registry-backed value", text: `value={flow.effective(MCP_FRONT_PORT_KEY)}`},
		{name: "deferred and busy disablement", text: `disabled={portDef.deferred || flow.busy}`},
		{name: "local edit path", text: `flow.setLocal( MCP_FRONT_PORT_KEY,`},
		{name: "shared save footer", text: `<SectionFooter flow={footerFlow} />`},
	}
	for _, marker := range markers {
		if !strings.Contains(src, marker.text) {
			t.Errorf("SectionAdvanced.tsx lost the explicit mcp_front.port %s binding; missing normalized source marker %q", marker.name, marker.text)
		}
	}
}
