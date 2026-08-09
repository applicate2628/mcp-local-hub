// internal/cli/mcp_front_port_test.go
//
// Mechanical drift-gate for the two independently-declared literals that
// must agree: internal/cli.DefaultRouteDaemonPort (the compiled fallback
// route.go's --port flag defaults to) and internal/api.DefaultMCPFrontPort
// (the settings-registry default for mcp_front.port). internal/api cannot
// import internal/cli.DefaultRouteDaemonPort (internal/cli imports
// internal/api; the reverse would cycle), so the two constants are
// duplicated literals — this test is the drift gate that keeps a future
// edit to either one from silently diverging from the other.
package cli

import (
	"strconv"
	"testing"

	"mcp-local-hub/internal/api"
)

func TestMCPFrontPortSettingDefaultMatchesRouteDaemonPort(t *testing.T) {
	if api.DefaultMCPFrontPort != DefaultRouteDaemonPort {
		t.Fatalf("api.DefaultMCPFrontPort (%d) != cli.DefaultRouteDaemonPort (%d) — these two constants must be kept in sync manually (internal/api cannot import internal/cli)",
			api.DefaultMCPFrontPort, DefaultRouteDaemonPort)
	}

	var found *api.SettingDef
	for i := range api.SettingsRegistry {
		if api.SettingsRegistry[i].Key == api.MCPFrontPortSettingKey {
			found = &api.SettingsRegistry[i]
			break
		}
	}
	if found == nil {
		t.Fatalf("SettingsRegistry has no entry for key %q", api.MCPFrontPortSettingKey)
	}
	wantDefault := strconv.Itoa(DefaultRouteDaemonPort)
	if found.Default != wantDefault {
		t.Fatalf("mcp_front.port SettingDef.Default = %q, want %q (must match cli.DefaultRouteDaemonPort)", found.Default, wantDefault)
	}
	if found.Type != api.TypeInt {
		t.Fatalf("mcp_front.port SettingDef.Type = %q, want TypeInt", found.Type)
	}
	if found.Min == nil || *found.Min != 1024 {
		t.Fatalf("mcp_front.port SettingDef.Min = %v, want 1024", found.Min)
	}
	if found.Max == nil || *found.Max != 65535 {
		t.Fatalf("mcp_front.port SettingDef.Max = %v, want 65535", found.Max)
	}
}

// TestResolveMCPFrontPortFn_FreshHostUsesCompiledDefault exercises the
// production closure (not a stub) against a redirected, empty settings
// state dir — a genuinely fresh host with no gui-preferences.yaml yet —
// and asserts it returns DefaultRouteDaemonPort (via the registry default,
// api.DefaultMCPFrontPort, which the mechanical drift-gate test above
// proves equals DefaultRouteDaemonPort) rather than erroring or returning
// 0. internal/api/mcp_front_port_test.go separately covers the corrupt-file
// error-fallback branch that this fresh-host case does not exercise.
func TestResolveMCPFrontPortFn_FreshHostUsesCompiledDefault(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("LOCALAPPDATA", tmp)
	t.Setenv("MCPHUB_STATE_DIR_OVERRIDE", tmp)

	got := resolveMCPFrontPortFn()
	if got != DefaultRouteDaemonPort {
		t.Fatalf("resolveMCPFrontPortFn() = %d, want DefaultRouteDaemonPort (%d) on a fresh host with no persisted setting", got, DefaultRouteDaemonPort)
	}
}
