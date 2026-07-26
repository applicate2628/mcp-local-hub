// internal/api/mcp_front_port_gui_visibility_pr588_test.go
//
// codex bot PR #588 P2: the mcp_front.port registry entry claimed the
// "advanced" section made it visible in the GUI "via the generic
// FieldRenderer". It did not — SectionAdvanced renders only hard-coded
// controls, and FieldRenderer is referenced by no section at all.
//
// The claim is now retracted in the registry comment. These two tests keep
// the CORRECTED claim honest from both sides:
//
//   - the surface that DOES work (the settings CLI/registry path) is pinned
//     positively, so "CLI-managed" is a verified statement rather than
//     another assertion nobody checked;
//   - the absent surface is pinned by a cross-language drift gate that FAILS
//     the moment a generic registry-field rendering path is added to
//     SectionAdvanced — forcing the comment to be updated in the same change
//     instead of rotting into a second false claim.
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

// TestSectionAdvanced_StillHasNoGenericRegistryFieldRendering is the
// cross-language drift gate for the corrected registry comment.
//
// This is a deliberate INVERTED guard: it asserts an ABSENCE that the
// registry comment currently documents. When someone adds the frontend
// control or the generic-field rendering path (tracked in
// work-items/bugs/2026-07-26-mcp-front-port-not-rendered-in-gui-advanced-section.md),
// this test FAILS — and its message says to delete it and update the
// mcp_front.port comment in settings_registry.go. That is the intended
// workflow: a Go comment making a claim about TypeScript cannot be checked by
// the compiler, so the only thing that stops it from rotting a second time is
// a gate that trips when the claim stops being true.
func TestSectionAdvanced_StillHasNoGenericRegistryFieldRendering(t *testing.T) {
	path := filepath.Join("..", "gui", "frontend", "src", "components", "settings", "SectionAdvanced.tsx")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Skipf("frontend source not available at %s (%v); the drift gate only runs from a full checkout", path, err)
	}
	src := string(raw)

	for _, marker := range []string{"FieldRenderer", "useSectionSaveFlow", MCPFrontPortSettingKey} {
		if !strings.Contains(src, marker) {
			continue
		}
		t.Fatalf("SectionAdvanced.tsx now references %q, so it very likely DOES render registry-declared settings — which the mcp_front.port comment in settings_registry.go currently says it does NOT.\n"+
			"ACTION: if the advanced section now renders %s (or any registry field), delete this test and update that comment to describe the new GUI surface; also close "+
			"work-items/bugs/2026-07-26-mcp-front-port-not-rendered-in-gui-advanced-section.md.", marker, MCPFrontPortSettingKey)
	}
}
