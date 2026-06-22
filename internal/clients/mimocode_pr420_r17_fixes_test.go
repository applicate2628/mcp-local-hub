package clients

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestMimoCode_SymlinkToRegularLayer_NotAFault pins bot PR #420 r17 finding B3:
// MimoCodeNonRegularActiveLayer must FOLLOW symlinks (os.Stat, not os.Lstat) so a
// layer that is a symlink RESOLVING TO A REGULAR FILE is NOT classified as a
// config fault. The actual reader (readRawConfig → os.ReadFile) follows the
// symlink fine, so the servers in that layer are valid and must not vanish.
func TestMimoCode_SymlinkToRegularLayer_NotAFault(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation requires elevation on Windows")
	}
	isolateMimoCodeEnv(t)
	dir := t.TempDir()

	// A real regular config file off to the side, and the write-target layer is a
	// SYMLINK to it. mimocode.json is a global layer name, so the scan-path client
	// resolves the in-dir layer set.
	real := filepath.Join(dir, "real-mimocode.json")
	if err := os.WriteFile(real, []byte(`{"mcp":{"serena":{"type":"remote","url":"http://localhost:9121/mcp","enabled":true}}}`), 0600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "mimocode.json")
	if err := os.Symlink(real, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	if bad, isBad := MimoCodeNonRegularActiveLayer(link); isBad {
		t.Fatalf("a symlink-to-regular layer must NOT be a fault, got bad=%q", bad)
	}
	// And the merged read actually surfaces the server (the reader follows it).
	merged, err := MimoCodeMergedConfig(link)
	if err != nil {
		t.Fatalf("merged read through a symlink-to-regular layer must succeed: %v", err)
	}
	servers, _ := merged["mcp"].(map[string]any)
	if _, ok := servers["serena"]; !ok {
		t.Errorf("merged read must surface serena from the symlinked-to-regular layer: %+v", merged)
	}
}

// TestMimoCode_SymlinkToDirectoryLayer_IsAFault confirms the B3 fix still flags a
// genuine non-regular target: a symlink resolving to a DIRECTORY is a fault (the
// reader would fail reading a directory), exactly as a plain directory layer is.
func TestMimoCode_SymlinkToDirectoryLayer_IsAFault(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation requires elevation on Windows")
	}
	isolateMimoCodeEnv(t)
	dir := t.TempDir()
	targetDir := filepath.Join(dir, "a-directory")
	if err := os.Mkdir(targetDir, 0700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "mimocode.json")
	if err := os.Symlink(targetDir, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	if _, isBad := MimoCodeNonRegularActiveLayer(link); !isBad {
		t.Fatalf("a symlink-to-directory layer must be a fault")
	}
}

// TestMimoCode_EnabledOnlyTrueOverlay_NotAShadow pins bot PR #420 r17 finding B5:
// a higher layer that carries ONLY `enabled: true` (no type/command/url) overlays
// just the flag onto the lower write-target entry — the write-target content still
// supplies the url, so the hub write IS effective. AddEntry must NOT refuse it as
// a shadow.
func TestMimoCode_EnabledOnlyTrueOverlay_NotAShadow(t *testing.T) {
	isolateMimoCodeEnv(t)
	globalDir := t.TempDir()
	globalPath := filepath.Join(globalDir, "mimocode.json")
	// A higher layer (mimocode.jsonc sibling) with an enabled-only:true overlay.
	jsoncPath := filepath.Join(globalDir, "mimocode.jsonc")
	if err := os.WriteFile(jsoncPath, []byte(`{"mcp":{"serena":{"enabled":true}}}`), 0600); err != nil {
		t.Fatal(err)
	}
	o := &mimoCodeClient{path: globalPath}
	if err := o.AddEntry(MCPEntry{Name: "serena", URL: "http://localhost:9121/mcp"}); err != nil {
		t.Fatalf("AddEntry must SUCCEED with an enabled-only:true higher overlay (not shadow-refused): %v", err)
	}
	if e, _ := o.GetEntry("serena"); e == nil {
		t.Errorf("server must be installed to the write target despite the enabled-only:true overlay")
	}
}

// TestMimoCode_DisablingOverlay_IsAShadow confirms the B5 fix still refuses a
// DISABLING (enabled:false) higher overlay — it merges enabled:false onto the hub
// entry, so the hub write would have no effect.
func TestMimoCode_DisablingOverlay_IsAShadow(t *testing.T) {
	isolateMimoCodeEnv(t)
	globalDir := t.TempDir()
	globalPath := filepath.Join(globalDir, "mimocode.json")
	jsoncPath := filepath.Join(globalDir, "mimocode.jsonc")
	if err := os.WriteFile(jsoncPath, []byte(`{"mcp":{"serena":{"enabled":false}}}`), 0600); err != nil {
		t.Fatal(err)
	}
	o := &mimoCodeClient{path: globalPath}
	err := o.AddEntry(MCPEntry{Name: "serena", URL: "http://localhost:9121/mcp"})
	var shadowErr *ErrMimoCodeOverlayShadowsServer
	if !errors.As(err, &shadowErr) {
		t.Fatalf("a DISABLING (enabled:false) higher overlay must shadow-refuse AddEntry, got %v", err)
	}
}

// TestMimoCode_FullRedefinitionOverlay_IsAShadow confirms a full higher
// redefinition (carrying type/url) still shadows.
func TestMimoCode_FullRedefinitionOverlay_IsAShadow(t *testing.T) {
	isolateMimoCodeEnv(t)
	globalDir := t.TempDir()
	globalPath := filepath.Join(globalDir, "mimocode.json")
	jsoncPath := filepath.Join(globalDir, "mimocode.jsonc")
	if err := os.WriteFile(jsoncPath, []byte(`{"mcp":{"serena":{"type":"remote","url":"http://other/mcp","enabled":true}}}`), 0600); err != nil {
		t.Fatal(err)
	}
	o := &mimoCodeClient{path: globalPath}
	err := o.AddEntry(MCPEntry{Name: "serena", URL: "http://localhost:9121/mcp"})
	var shadowErr *ErrMimoCodeOverlayShadowsServer
	if !errors.As(err, &shadowErr) {
		t.Fatalf("a FULL higher redefinition must shadow-refuse AddEntry, got %v", err)
	}
}

// TestMimoCode_RemoveEntry_HigherLayerRetains_FailsLoud pins bot PR #420 r17
// finding B4: when the hub removes mcp.<name> from the write target but a HIGHER
// layer the hub cannot remove still defines it, RemoveEntry must FAIL LOUD
// (ErrMimoCodeHigherLayerRetainsServer) — not falsely report success while
// MiMoCode keeps loading the higher-layer value.
func TestMimoCode_RemoveEntry_HigherLayerRetains_FailsLoud(t *testing.T) {
	isolateMimoCodeEnv(t)
	globalDir := t.TempDir()
	globalPath := filepath.Join(globalDir, "mimocode.json")
	// Hub wrote serena to the write target.
	if err := os.WriteFile(globalPath, []byte(`{"mcp":{"serena":{"type":"remote","url":"http://hub/mcp","enabled":true}}}`), 0600); err != nil {
		t.Fatal(err)
	}
	// A HIGHER layer (mimocode.jsonc) ALSO fully defines serena.
	jsoncPath := filepath.Join(globalDir, "mimocode.jsonc")
	if err := os.WriteFile(jsoncPath, []byte(`{"mcp":{"serena":{"type":"remote","url":"http://higher/mcp","enabled":true}}}`), 0600); err != nil {
		t.Fatal(err)
	}
	o := &mimoCodeClient{path: globalPath}
	err := o.RemoveEntry("serena")
	var retainErr *ErrMimoCodeHigherLayerRetainsServer
	if !errors.As(err, &retainErr) {
		t.Fatalf("RemoveEntry must fail loud when a higher layer still retains the server, got %v", err)
	}
	if retainErr.Server != "serena" || retainErr.WriteTarget != globalPath {
		t.Errorf("retain error fields wrong: %+v", retainErr)
	}
	// The write-target value MUST be gone (the hub did its part).
	if v, ok, _ := mimoCodeFileEntryValue(globalPath, "serena"); ok {
		t.Errorf("write-target serena must be deleted even though the higher layer retains it: %+v", v)
	}
}

// TestMimoCode_RemoveEntry_BelowLayerReemerges_Succeeds confirms the B4 guard does
// NOT fire for the INTENDED rollback case: a name re-emerging from config.json
// (BELOW the write target) is the operator's prior re-surfacing, not a false
// success — RemoveEntry must succeed.
func TestMimoCode_RemoveEntry_BelowLayerReemerges_Succeeds(t *testing.T) {
	isolateMimoCodeEnv(t)
	globalDir := t.TempDir()
	globalPath := filepath.Join(globalDir, "mimocode.json")
	if err := os.WriteFile(globalPath, []byte(`{"mcp":{"serena":{"type":"remote","url":"http://hub/mcp","enabled":true}}}`), 0600); err != nil {
		t.Fatal(err)
	}
	// config.json BELOW the write target also defines serena (the operator's prior).
	belowPath := filepath.Join(globalDir, "config.json")
	if err := os.WriteFile(belowPath, []byte(`{"mcp":{"serena":{"type":"local","command":["serena"],"enabled":true}}}`), 0600); err != nil {
		t.Fatal(err)
	}
	o := &mimoCodeClient{path: globalPath}
	if err := o.RemoveEntry("serena"); err != nil {
		t.Fatalf("RemoveEntry must succeed when the name re-emerges only from a BELOW layer (intended rollback), got %v", err)
	}
}

// TestMimoCode_RemoveEntry_NoOpDelete_NoFalseFailure confirms the B4 guard does
// NOT fire on a no-op delete: when the write target never held the name (e.g.
// AddEntry was shadow-refused so nothing was ever written), a later RemoveEntry
// must stay a clean no-op even though a higher layer defines the name.
func TestMimoCode_RemoveEntry_NoOpDelete_NoFalseFailure(t *testing.T) {
	isolateMimoCodeEnv(t)
	globalDir := t.TempDir()
	globalPath := filepath.Join(globalDir, "mimocode.json")
	// Write target is EMPTY (no serena). A higher layer defines serena.
	if err := os.WriteFile(globalPath, []byte(`{"mcp":{}}`), 0600); err != nil {
		t.Fatal(err)
	}
	jsoncPath := filepath.Join(globalDir, "mimocode.jsonc")
	if err := os.WriteFile(jsoncPath, []byte(`{"mcp":{"serena":{"type":"remote","url":"http://higher/mcp","enabled":true}}}`), 0600); err != nil {
		t.Fatal(err)
	}
	o := &mimoCodeClient{path: globalPath}
	if err := o.RemoveEntry("serena"); err != nil {
		t.Fatalf("RemoveEntry must be a clean no-op when the write target never held the name, got %v", err)
	}
}

// TestMimoCode_HomeMimocodeDirShadow pins bot PR #420 r17 finding P1a: the HOME
// ~/.mimocode dir is resolvable from home alone and IS detected as a shadow layer.
func TestMimoCode_HomeMimocodeDirShadow(t *testing.T) {
	isolateMimoCodeEnv(t)
	// A fake home with ~/.mimocode/mimocode.json defining serena.
	home := t.TempDir()
	homeMimo := filepath.Join(home, ".mimocode")
	if err := os.MkdirAll(homeMimo, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(homeMimo, "mimocode.json"), []byte(`{"mcp":{"serena":{"type":"remote","url":"http://home/mcp","enabled":true}}}`), 0600); err != nil {
		t.Fatal(err)
	}
	// Write target lives in a SEPARATE dir (the global config dir), claudeHome set
	// to the fake home so the home-.mimocode shadow is detected.
	globalDir := t.TempDir()
	o := &mimoCodeClient{path: filepath.Join(globalDir, "mimocode.json"), claudeHome: home}

	err := o.AddEntry(MCPEntry{Name: "serena", URL: "http://localhost:9121/mcp"})
	var shadowErr *ErrMimoCodeOverlayShadowsServer
	if !errors.As(err, &shadowErr) {
		t.Fatalf("a HOME ~/.mimocode shadow must refuse AddEntry, got %v", err)
	}
	if shadowErr.SourceLabel != "home .mimocode dir" {
		t.Errorf("expected home .mimocode label, got %q", shadowErr.SourceLabel)
	}
}

// TestMimoCode_HomeMimocodeDir_StateSafeWithoutClaudeHome confirms the home
// .mimocode shadow is NOT read when claudeHome is "" (direct/test construction or
// the scan disable-flag barrier) — it never reaches the developer's real
// ~/.mimocode.
func TestMimoCode_HomeMimocodeDir_StateSafeWithoutClaudeHome(t *testing.T) {
	isolateMimoCodeEnv(t)
	globalDir := t.TempDir()
	o := &mimoCodeClient{path: filepath.Join(globalDir, "mimocode.json")} // claudeHome ""
	src, err := o.mimoCodeHomeMimocodeDirShadows("serena")
	if err != nil {
		t.Fatalf("home .mimocode shadow probe must not error with empty claudeHome: %v", err)
	}
	if src.Kind != "" {
		t.Errorf("home .mimocode shadow must report nothing when claudeHome is empty, got %+v", src)
	}
}

// TestMimoCode_ManagedConfigDirShadow_FailsLoud pins bot PR #420 r17 finding P1b
// AND r18 MEDIUM finding: the managed config dir (MIMOCODE_TEST_MANAGED_CONFIG_DIR
// override in tests) is a detect-only read-only layer; when it defines the server,
// AddEntry fails loud — and the error must be the DISTINCT
// ErrMimoCodeManagedConfigDirShadowsServer (NOT the macOS-MDM
// ErrMimoCodeManagedShadowsServer), naming the ACTUAL managed file the operator
// must edit, NOT a non-existent "MDM profile". The pre-r18 code shared
// Kind:"managed" with the MDM path, so on Windows/Linux it told the operator to
// remove an MDM profile (which does not exist there) and omitted the real file.
func TestMimoCode_ManagedConfigDirShadow_FailsLoud(t *testing.T) {
	isolateMimoCodeEnv(t)
	// Seed a managed config dir and point the test override at it (overrides the
	// non-existent path isolateMimoCodeEnv set).
	managedDir := t.TempDir()
	managedFile := filepath.Join(managedDir, "mimocode.json")
	if err := os.WriteFile(managedFile, []byte(`{"mcp":{"serena":{"type":"remote","url":"http://managed/mcp","enabled":true}}}`), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MIMOCODE_TEST_MANAGED_CONFIG_DIR", managedDir)

	globalDir := t.TempDir()
	globalPath := filepath.Join(globalDir, "mimocode.json")
	o := &mimoCodeClient{path: globalPath}
	err := o.AddEntry(MCPEntry{Name: "serena", URL: "http://localhost:9121/mcp"})

	// r18: a managed-config-dir shadow must produce the DISTINCT typed error, not
	// the macOS-MDM one.
	var mcdErr *ErrMimoCodeManagedConfigDirShadowsServer
	if !errors.As(err, &mcdErr) {
		t.Fatalf("a managed-config-dir shadow must fail loud with ErrMimoCodeManagedConfigDirShadowsServer (distinct from the MDM error), got %T: %v", err, err)
	}
	// It must NOT be mis-typed as the macOS MDM error.
	var mdmErr *ErrMimoCodeManagedShadowsServer
	if errors.As(err, &mdmErr) {
		t.Fatalf("a managed-config-dir shadow must NOT be reported as the macOS MDM error (wrong remediation surface on Windows/Linux), got %v", err)
	}
	// The error message must name the ACTUAL managed file and must NOT tell the
	// operator to remove an MDM "profile". The message quotes the path via %q, so
	// match the %q-rendered form (backslashes escaped on Windows).
	if !strings.Contains(err.Error(), fmt.Sprintf("%q", managedFile)) {
		t.Errorf("managed-config-dir error must name the actual file %q, got: %v", managedFile, err)
	}
	if strings.Contains(err.Error(), "MDM") || strings.Contains(err.Error(), "managed configuration profile") || strings.Contains(err.Error(), "Managed Preferences") {
		t.Errorf("managed-config-dir error must NOT reference an MDM profile / Managed Preferences (that surface does not exist on Windows/Linux), got: %v", err)
	}
	// The structured File field must carry the offending path.
	if mcdErr.File != managedFile {
		t.Errorf("ErrMimoCodeManagedConfigDirShadowsServer.File = %q, want %q", mcdErr.File, managedFile)
	}
	// The write target must not have been written.
	if _, statErr := os.Stat(globalPath); statErr == nil {
		t.Errorf("AddEntry must not write mimocode.json when the managed dir shadows the server")
	}
}

// TestMimoCode_ManagedConfigDirShadow_RemoveEntryNamesFile pins the RemoveEntry
// side of the r18 MEDIUM finding: when a managed-config-dir layer RETAINS the
// server after a write-target delete, ErrMimoCodeHigherLayerRetainsServer must
// name the actual managed file (via the "managed-config-dir" Source.Kind), NOT an
// MDM profile.
func TestMimoCode_ManagedConfigDirShadow_RemoveEntryNamesFile(t *testing.T) {
	isolateMimoCodeEnv(t)
	managedDir := t.TempDir()
	managedFile := filepath.Join(managedDir, "mimocode.json")
	if err := os.WriteFile(managedFile, []byte(`{"mcp":{"serena":{"type":"remote","url":"http://managed/mcp","enabled":true}}}`), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MIMOCODE_TEST_MANAGED_CONFIG_DIR", managedDir)

	globalDir := t.TempDir()
	globalPath := filepath.Join(globalDir, "mimocode.json")
	// Seed the write target with its OWN value for the same name so RemoveEntry's
	// hadOwnValue snapshot is true and the higher-layer-retention guard fires.
	if err := os.WriteFile(globalPath, []byte(`{"mcp":{"serena":{"type":"remote","url":"http://localhost:9121/mcp","enabled":true}}}`), 0600); err != nil {
		t.Fatal(err)
	}
	o := &mimoCodeClient{path: globalPath}
	err := o.RemoveEntry("serena")

	var retainErr *ErrMimoCodeHigherLayerRetainsServer
	if !errors.As(err, &retainErr) {
		t.Fatalf("a managed-config-dir layer retaining the server must fail loud, got %T: %v", err, err)
	}
	if retainErr.Source.Kind != "managed-config-dir" {
		t.Errorf("retained source Kind = %q, want %q", retainErr.Source.Kind, "managed-config-dir")
	}
	if !strings.Contains(err.Error(), managedFile) {
		t.Errorf("RemoveEntry retention error must name the actual managed file %q, got: %v", managedFile, err)
	}
	if strings.Contains(err.Error(), "MDM") || strings.Contains(err.Error(), "Managed Preferences") {
		t.Errorf("RemoveEntry retention error for a managed-config-dir must NOT reference MDM/Managed Preferences, got: %v", err)
	}
}

// TestMimoCode_ManagedConfigDir_EnabledOnlyTrue_NotAShadow confirms the managed
// config dir reader is also shadow-aware (B5): an enabled-only:true managed
// override does not shadow.
func TestMimoCode_ManagedConfigDir_EnabledOnlyTrue_NotAShadow(t *testing.T) {
	isolateMimoCodeEnv(t)
	managedDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(managedDir, "mimocode.json"), []byte(`{"mcp":{"serena":{"enabled":true}}}`), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MIMOCODE_TEST_MANAGED_CONFIG_DIR", managedDir)

	globalDir := t.TempDir()
	o := &mimoCodeClient{path: filepath.Join(globalDir, "mimocode.json")}
	if err := o.AddEntry(MCPEntry{Name: "serena", URL: "http://localhost:9121/mcp"}); err != nil {
		t.Fatalf("an enabled-only:true managed override must not shadow-refuse AddEntry, got %v", err)
	}
}

// TestMimoCode_ManagedConfigDir_AbsentNoShadow confirms a missing managed dir is
// "no shadow" (the existsSync gate), so install proceeds.
func TestMimoCode_ManagedConfigDir_AbsentNoShadow(t *testing.T) {
	isolateMimoCodeEnv(t) // sets MIMOCODE_TEST_MANAGED_CONFIG_DIR to a non-existent path
	globalDir := t.TempDir()
	o := &mimoCodeClient{path: filepath.Join(globalDir, "mimocode.json")}
	if err := o.AddEntry(MCPEntry{Name: "serena", URL: "http://localhost:9121/mcp"}); err != nil {
		t.Fatalf("install must proceed when the managed config dir is absent, got %v", err)
	}
}

// TestMimoCode_ClaudeImportHome_PrefersHOME pins bot PR #420 r17 finding B6:
// mimoCodeClaudeImportHome resolves HOME before USERPROFILE (matching MiMoCode's
// Global.Path.home), so a host where HOME != USERPROFILE reads $HOME, not
// %USERPROFILE%.
func TestMimoCode_ClaudeImportHome_PrefersHOME(t *testing.T) {
	t.Setenv("HOME", filepath.Join(t.TempDir(), "the-home"))
	t.Setenv("USERPROFILE", filepath.Join(t.TempDir(), "the-userprofile"))
	got, err := mimoCodeClaudeImportHome()
	if err != nil {
		t.Fatalf("mimoCodeClaudeImportHome: %v", err)
	}
	if want := os.Getenv("HOME"); got != want {
		t.Errorf("mimoCodeClaudeImportHome must prefer HOME (%q), got %q", want, got)
	}
}

// TestMimoCode_ClaudeImportHome_FallsBackToUSERPROFILE confirms USERPROFILE is the
// fallback when HOME is unset (the Windows-native case where Go's os.UserHomeDir
// would also return USERPROFILE).
func TestMimoCode_ClaudeImportHome_FallsBackToUSERPROFILE(t *testing.T) {
	t.Setenv("HOME", "")
	up := filepath.Join(t.TempDir(), "the-userprofile")
	t.Setenv("USERPROFILE", up)
	got, err := mimoCodeClaudeImportHome()
	if err != nil {
		t.Fatalf("mimoCodeClaudeImportHome: %v", err)
	}
	if got != up {
		t.Errorf("mimoCodeClaudeImportHome must fall back to USERPROFILE (%q), got %q", up, got)
	}
}
