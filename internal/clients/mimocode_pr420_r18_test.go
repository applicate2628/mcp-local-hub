package clients

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// TestMimoCode_GetEntry_EnabledOnlyTrueOverlayOverDisabledWriteTarget_NotDisabled
// pins bot PR #420 r18 P2: when the write target carries {enabled:false} but a
// HIGHER layer is an enabled-only:TRUE overlay, the merge overlays enabled:true and
// MiMoCode LOADS the server — so GetEntry must report Disabled=false (computed from
// the merged-effective enabled), NOT the write-target ownRaw's enabled:false. A
// false Disabled:true would make api.GatedOnClients skip a LIVE aggregate and let
// `mcphub gui --reset-port` orphan its URL.
func TestMimoCode_GetEntry_EnabledOnlyTrueOverlayOverDisabledWriteTarget_NotDisabled(t *testing.T) {
	isolateMimoCodeEnv(t)
	globalDir := t.TempDir()
	globalPath := filepath.Join(globalDir, "mimocode.json")
	// Write target: a DISABLED local stub for the server.
	if err := os.WriteFile(globalPath, []byte(`{"mcp":{"serena":{"type":"local","command":["serena"],"enabled":false}}}`), 0600); err != nil {
		t.Fatal(err)
	}
	// Higher layer mimocode.jsonc: an enabled-only:TRUE overlay (flips enabled).
	jsoncPath := filepath.Join(globalDir, "mimocode.jsonc")
	if err := os.WriteFile(jsoncPath, []byte(`{"mcp":{"serena":{"enabled":true}}}`), 0600); err != nil {
		t.Fatal(err)
	}
	o := &mimoCodeClient{path: globalPath}
	e, err := o.GetEntry("serena")
	if err != nil {
		t.Fatalf("GetEntry: %v", err)
	}
	if e == nil {
		t.Fatal("GetEntry must return the merged entry, got nil")
	}
	if e.Disabled {
		t.Errorf("an enabled-only:true overlay flips the merged-effective enabled to true; GetEntry must report Disabled=false (gate sees it ACTIVE), got Disabled=true")
	}
	// No-copy-up invariant: Raw (when carried) must be the WRITE-TARGET own value,
	// never a copied-up lower/higher layer command — verified here by the entry
	// staying url-less local (Raw carries the write-target stub) and SourceBelowWriteTarget
	// false (the name is defined at/above the write target).
	if e.SourceBelowWriteTarget {
		t.Errorf("serena is defined at the write target (and a higher layer); SourceBelowWriteTarget must be false, got true")
	}
}

// TestMimoCode_GetEntry_DisablingOverlayOverEnabledWriteTarget_IsDisabled is the
// inverse control for r18 P2: a DISABLING (enabled:false) higher overlay over an
// ENABLED write target merges to disabled, so GetEntry must report Disabled=true.
func TestMimoCode_GetEntry_DisablingOverlayOverEnabledWriteTarget_IsDisabled(t *testing.T) {
	isolateMimoCodeEnv(t)
	globalDir := t.TempDir()
	globalPath := filepath.Join(globalDir, "mimocode.json")
	if err := os.WriteFile(globalPath, []byte(`{"mcp":{"serena":{"type":"remote","url":"http://hub/mcp","enabled":true}}}`), 0600); err != nil {
		t.Fatal(err)
	}
	jsoncPath := filepath.Join(globalDir, "mimocode.jsonc")
	if err := os.WriteFile(jsoncPath, []byte(`{"mcp":{"serena":{"enabled":false}}}`), 0600); err != nil {
		t.Fatal(err)
	}
	o := &mimoCodeClient{path: globalPath}
	e, err := o.GetEntry("serena")
	if err != nil {
		t.Fatalf("GetEntry: %v", err)
	}
	if e == nil {
		t.Fatal("GetEntry must return the merged entry, got nil")
	}
	if !e.Disabled {
		t.Errorf("a disabling overlay merges the server to enabled:false; GetEntry must report Disabled=true, got false")
	}
}

// TestMimoCode_HomeMimocodeOnlyServer_VisibleInRead pins bot PR #420 r18 P2 (read
// side): a server defined ONLY in $HOME/.mimocode/mimocode.json(c) must be VISIBLE
// to the read merge (readMergedLayers) and GetEntry read-membership — before the
// fix, P1a added the home layer to the SHADOW walk (AddEntry refuses a same-named
// install) but NOT to the read path, so a home-only server was hidden from the
// matrix and read-membership while AddEntry shadow-refused the same name.
func TestMimoCode_HomeMimocodeOnlyServer_VisibleInRead(t *testing.T) {
	isolateMimoCodeEnv(t)
	home := t.TempDir()
	homeMimo := filepath.Join(home, ".mimocode")
	if err := os.MkdirAll(homeMimo, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(homeMimo, "mimocode.json"), []byte(`{"mcp":{"home-only":{"type":"remote","url":"http://home/mcp","enabled":true}}}`), 0600); err != nil {
		t.Fatal(err)
	}
	// Write target lives in a SEPARATE global dir; claudeHome set to the fake home
	// (state-safe — direct construction with an explicit claudeHome, never the real
	// home).
	globalDir := t.TempDir()
	globalPath := filepath.Join(globalDir, "mimocode.json")
	if err := os.WriteFile(globalPath, []byte(`{"mcp":{}}`), 0600); err != nil {
		t.Fatal(err)
	}
	o := &mimoCodeClient{path: globalPath, claudeHome: home}

	// Read merge must surface the home-only server.
	merged, err := o.readMergedLayers()
	if err != nil {
		t.Fatalf("readMergedLayers: %v", err)
	}
	servers, _ := merged["mcp"].(map[string]any)
	if _, ok := servers["home-only"]; !ok {
		t.Errorf("a server defined only in ~/.mimocode must appear in the read merge, got %+v", merged)
	}
	// GetEntry read-membership must see it too (non-nil).
	e, err := o.GetEntry("home-only")
	if err != nil {
		t.Fatalf("GetEntry: %v", err)
	}
	if e == nil {
		t.Error("GetEntry must surface a home-.mimocode-only server (read membership), got nil")
	}
}

// TestMimoCode_HomeMimocodeRead_StateSafeWithoutClaudeHome confirms the home read
// layer is NOT consulted when claudeHome is "" (direct/test construction) — so a
// test never reaches the developer's real ~/.mimocode through the read path.
func TestMimoCode_HomeMimocodeRead_StateSafeWithoutClaudeHome(t *testing.T) {
	isolateMimoCodeEnv(t)
	globalDir := t.TempDir()
	globalPath := filepath.Join(globalDir, "mimocode.json")
	// claudeHome left "" — the home layer must not appear in the resolved files.
	layers := mimoCodeReadLayerFiles(globalPath, "", "", "")
	for _, f := range layers {
		if filepath.Base(filepath.Dir(f)) == ".mimocode" {
			t.Errorf("home .mimocode layer must NOT be resolved when claudeHome is empty, got %q", f)
		}
	}
}

// TestMimoCode_AllStdioEntries_DropsDisabled pins bot PR #420 r18 P3: a DISABLED
// stdio entry (enabled:false) must NOT be surfaced by AllStdioEntries — MiMoCode
// never spawns it, and the destructive gopls-mcp cleanup (register.go) backs up +
// RemoveEntry-s every returned match, so returning a disabled operator-authored
// `gopls mcp` entry would wrongly delete it.
func TestMimoCode_AllStdioEntries_DropsDisabled(t *testing.T) {
	isolateMimoCodeEnv(t)
	globalDir := t.TempDir()
	globalPath := filepath.Join(globalDir, "mimocode.json")
	// One ACTIVE local stdio entry, one DISABLED gopls-mcp stdio entry.
	body := `{"mcp":{` +
		`"active":{"type":"local","command":["mytool","--flag"],"enabled":true},` +
		`"gopls-disabled":{"type":"local","command":["gopls","mcp"],"enabled":false}` +
		`}}`
	if err := os.WriteFile(globalPath, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	o := &mimoCodeClient{path: globalPath}
	entries, err := o.AllStdioEntries()
	if err != nil {
		t.Fatalf("AllStdioEntries: %v", err)
	}
	for _, e := range entries {
		if e.Name == "gopls-disabled" {
			t.Errorf("a DISABLED stdio entry must be dropped from AllStdioEntries (cleanup must not delete it), got %+v", e)
		}
	}
	// Positive control: the active entry is still surfaced.
	var sawActive bool
	for _, e := range entries {
		if e.Name == "active" {
			sawActive = true
		}
	}
	if !sawActive {
		t.Errorf("the ACTIVE stdio entry must still be surfaced by AllStdioEntries, got %+v", entries)
	}
}

// TestMimoCode_RemoveEntry_DisableOnlyOverlay_Succeeds pins bot PR #420 r18 P3 (B4
// over-fire): the write target holds the real hub entry; a HIGHER layer carries
// ONLY {enabled:false} (a disable-only overlay, no url/command/type). After
// RemoveEntry deletes the real write-target entry, the bare {enabled:false} stub
// has nothing to merge onto and MiMoCode loads NOTHING — the active hub entry WAS
// removed, so RemoveEntry must SUCCEED (no false ErrMimoCodeHigherLayerRetainsServer).
func TestMimoCode_RemoveEntry_DisableOnlyOverlay_Succeeds(t *testing.T) {
	isolateMimoCodeEnv(t)
	globalDir := t.TempDir()
	globalPath := filepath.Join(globalDir, "mimocode.json")
	// Hub wrote the real serena entry to the write target.
	if err := os.WriteFile(globalPath, []byte(`{"mcp":{"serena":{"type":"remote","url":"http://hub/mcp","enabled":true}}}`), 0600); err != nil {
		t.Fatal(err)
	}
	// A HIGHER layer (mimocode.jsonc) carries ONLY {enabled:false} for serena.
	jsoncPath := filepath.Join(globalDir, "mimocode.jsonc")
	if err := os.WriteFile(jsoncPath, []byte(`{"mcp":{"serena":{"enabled":false}}}`), 0600); err != nil {
		t.Fatal(err)
	}
	o := &mimoCodeClient{path: globalPath}
	if err := o.RemoveEntry("serena"); err != nil {
		t.Fatalf("RemoveEntry must SUCCEED when the only higher layer is a disable-only overlay (the active entry was removed), got %v", err)
	}
	// The write-target value must be gone (the hub did its part).
	if v, ok, _ := mimoCodeFileEntryValue(globalPath, "serena"); ok {
		t.Errorf("write-target serena must be deleted, got %+v", v)
	}
}

// TestMimoCode_RemoveEntry_DisabledFullRedefinition_FailsLoud confirms the B4 fix
// does NOT over-correct: a higher layer with a DISABLED FULL entry
// ({type,url,enabled:false}) still re-emerges a server-shaped key (it carries
// type/url, not a content-less enabled-only stub), so the conservative
// "DISABLED ENTRIES COUNT" semantic keeps failing loud — consistent with
// mimoCodeNameReResolvesAfterWriteTargetRemoval.
func TestMimoCode_RemoveEntry_DisabledFullRedefinition_FailsLoud(t *testing.T) {
	isolateMimoCodeEnv(t)
	globalDir := t.TempDir()
	globalPath := filepath.Join(globalDir, "mimocode.json")
	if err := os.WriteFile(globalPath, []byte(`{"mcp":{"serena":{"type":"remote","url":"http://hub/mcp","enabled":true}}}`), 0600); err != nil {
		t.Fatal(err)
	}
	// Higher layer: a DISABLED FULL entry (carries type/url).
	jsoncPath := filepath.Join(globalDir, "mimocode.jsonc")
	if err := os.WriteFile(jsoncPath, []byte(`{"mcp":{"serena":{"type":"remote","url":"http://higher/mcp","enabled":false}}}`), 0600); err != nil {
		t.Fatal(err)
	}
	o := &mimoCodeClient{path: globalPath}
	err := o.RemoveEntry("serena")
	var retainErr *ErrMimoCodeHigherLayerRetainsServer
	if !errors.As(err, &retainErr) {
		t.Fatalf("a DISABLED FULL higher redefinition (type/url present) must still fail loud, got %v", err)
	}
}
