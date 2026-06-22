package api

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// TestGatedOnClients_EnabledOnlyOverlayOverDisabledMimoAggregate_GateOn pins bot PR
// #420 r18 P2 (gating side): a `mcphub-hub` aggregate whose write-target entry is
// {enabled:false} but whose HIGHER layer (mimocode.jsonc) carries an
// enabled-only:TRUE overlay merges to enabled:true — MiMoCode LOADS the aggregate,
// so it IS gate-ON and `mcphub gui --reset-port` must be blocked (orphan risk).
// Before the fix GetEntry stamped Disabled:true from the write-target ownRaw,
// GatedOnClients skipped it, and the reset could orphan the live aggregate URL.
func TestGatedOnClients_EnabledOnlyOverlayOverDisabledMimoAggregate_GateOn(t *testing.T) {
	home := hermeticHome(t)
	// mimo's live global dir = $XDG_CONFIG_HOME/mimocode = $home/.config/mimocode.
	globalDir := filepath.Join(home, ".config", "mimocode")
	if err := os.MkdirAll(globalDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// Write target: a DISABLED mcphub-hub aggregate.
	disabled := `{"mcp":{"mcphub-hub":{"type":"remote","url":"http://127.0.0.1:3439/clients/mimocode/mcp","enabled":false}}}`
	if err := os.WriteFile(filepath.Join(globalDir, "mimocode.json"), []byte(disabled), 0o600); err != nil {
		t.Fatal(err)
	}
	// Higher layer mimocode.jsonc: an enabled-only:TRUE overlay flips it ACTIVE.
	overlay := `{"mcp":{"mcphub-hub":{"enabled":true}}}`
	if err := os.WriteFile(filepath.Join(globalDir, "mimocode.jsonc"), []byte(overlay), 0o600); err != nil {
		t.Fatal(err)
	}

	if gated := GatedOnClients(); !slices.Contains(gated, "mimocode") {
		t.Errorf("an enabled-only:true overlay over a disabled mimo aggregate merges to ACTIVE; it must be reported gate-ON (P2), got %v", gated)
	}
}
