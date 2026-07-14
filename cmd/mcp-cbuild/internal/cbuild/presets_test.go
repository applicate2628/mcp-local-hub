package cbuild

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// writeFile is a test helper that writes content to dir/name.
func writeFile(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
}

// findPreset returns the PresetInfo with the given name, or fails.
func findPreset(t *testing.T, ps []PresetInfo, name string) PresetInfo {
	t.Helper()
	for _, p := range ps {
		if p.Name == name {
			return p
		}
	}
	t.Fatalf("preset %q not found in %+v", name, ps)
	return PresetInfo{}
}

func TestLoadPresetsIncludeAndInherits(t *testing.T) {
	dir := t.TempDir()

	// An included base file supplies two hidden base presets.
	writeFile(t, dir, "common.json", `{
      "version": 3,
      "configurePresets": [
        {
          "name": "base",
          "hidden": true,
          "generator": "Ninja",
          "binaryDir": "${sourceDir}/build/${presetName}",
          "cacheVariables": { "CMAKE_CXX_COMPILER": "clang++" }
        },
        {
          "name": "base2",
          "hidden": true,
          "generator": "Unix Makefiles",
          "cacheVariables": { "CMAKE_C_COMPILER": "gcc" }
        }
      ]
    }`)

	// The top file includes common.json and defines a preset that inherits both
	// bases (base first — it must win on the shared generator field).
	writeFile(t, dir, "CMakePresets.json", `{
      "version": 3,
      "include": ["common.json"],
      "configurePresets": [
        {
          "name": "dev",
          "displayName": "Dev build",
          "inherits": ["base", "base2"],
          "cacheVariables": { "CMAKE_BUILD_TYPE": "Debug" }
        }
      ],
      "buildPresets": [ { "name": "dev-build", "configurePreset": "dev" } ],
      "testPresets": [ { "name": "dev-test", "configurePreset": "dev" } ]
    }`)

	p, err := LoadPresets(dir)
	if err != nil {
		t.Fatalf("LoadPresets: %v", err)
	}
	res := p.Result()

	if res.Version != 3 {
		t.Errorf("version = %d, want 3", res.Version)
	}
	if len(res.Files) != 2 {
		t.Errorf("Files = %v, want both presets files loaded", res.Files)
	}

	dev := findPreset(t, res.ConfigurePresets, "dev")
	if dev.DisplayName != "Dev build" {
		t.Errorf("dev.DisplayName = %q", dev.DisplayName)
	}
	if len(dev.Inherits) != 2 || dev.Inherits[0] != "base" || dev.Inherits[1] != "base2" {
		t.Errorf("dev.Inherits = %v", dev.Inherits)
	}
	// generator: base wins over base2 (first in inherits list).
	if dev.ResolvedGenerator != "Ninja" {
		t.Errorf("dev.ResolvedGenerator = %q, want Ninja (base wins over base2)", dev.ResolvedGenerator)
	}
	// compiler + binaryDir are inherited from base.
	if dev.ResolvedCompiler != "clang++" {
		t.Errorf("dev.ResolvedCompiler = %q, want clang++", dev.ResolvedCompiler)
	}
	if dev.BinaryDir != "${sourceDir}/build/${presetName}" {
		t.Errorf("dev.BinaryDir = %q", dev.BinaryDir)
	}

	// Named build/test presets are surfaced with their configurePreset link.
	bp := findPreset(t, res.BuildPresets, "dev-build")
	if bp.ConfigurePreset != "dev" {
		t.Errorf("build preset configurePreset = %q, want dev", bp.ConfigurePreset)
	}
	tp := findPreset(t, res.TestPresets, "dev-test")
	if tp.ConfigurePreset != "dev" {
		t.Errorf("test preset configurePreset = %q, want dev", tp.ConfigurePreset)
	}

	// ResolvedBinaryDir returns the merged (inherited) binaryDir.
	bd, err := p.ResolvedBinaryDir("dev")
	if err != nil {
		t.Fatalf("ResolvedBinaryDir: %v", err)
	}
	if bd != "${sourceDir}/build/${presetName}" {
		t.Errorf("ResolvedBinaryDir = %q", bd)
	}
}

func TestLoadPresetsMissing(t *testing.T) {
	if _, err := LoadPresets(t.TempDir()); err == nil {
		t.Fatal("expected error for a directory with no presets file")
	}
}

func TestResolveBuildDirWithinSourceGuard(t *testing.T) {
	src := t.TempDir()

	// Inside the source tree: allowed, returns the resolved absolute path.
	got, err := resolveBuildDirWithinSource("${sourceDir}/build", src, "dev")
	if err != nil {
		t.Fatalf("inside-source binaryDir rejected: %v", err)
	}
	if got != filepath.Join(filepath.Clean(src), "build") {
		t.Errorf("resolved = %q, want %q", got, filepath.Join(src, "build"))
	}

	// Escaping the source tree: refused.
	if _, err := resolveBuildDirWithinSource("${sourceDir}/../evil", src, "dev"); err == nil {
		t.Error("expected refusal for a binaryDir escaping the source directory")
	}
	// The source directory itself: refused.
	if _, err := resolveBuildDirWithinSource("${sourceDir}", src, "dev"); err == nil {
		t.Error("expected refusal for a binaryDir equal to the source directory")
	}
	// Unresolved macro: refused.
	if _, err := resolveBuildDirWithinSource("${unknownMacro}/build", src, "dev"); err == nil {
		t.Error("expected refusal for a binaryDir with unresolved macros")
	}
}

// TestMergeConfigureSharedParentPrecedence pins the inherits-precedence fix: a
// grandparent shared across two inheritance branches must contribute at each
// branch's correct precedence. Here `dev` inherits [viaA, viaB]; both inherit
// `common` (binaryDir=common-build); viaB overrides it to b-build. Because viaA
// (which keeps common's value) is EARLIER in dev's inherits list, it wins, so
// dev.binaryDir must resolve to common-build. The earlier shared-visited-set
// design let the lower-precedence viaB value win.
func TestMergeConfigureSharedParentPrecedence(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "CMakePresets.json", `{
      "version": 3,
      "configurePresets": [
        { "name": "common", "hidden": true, "generator": "Ninja",
          "binaryDir": "${sourceDir}/common-build" },
        { "name": "viaA", "hidden": true, "inherits": ["common"] },
        { "name": "viaB", "hidden": true, "inherits": ["common"],
          "binaryDir": "${sourceDir}/b-build" },
        { "name": "dev", "inherits": ["viaA", "viaB"] }
      ]
    }`)

	p, err := LoadPresets(dir)
	if err != nil {
		t.Fatalf("LoadPresets: %v", err)
	}
	bd, err := p.ResolvedBinaryDir("dev")
	if err != nil {
		t.Fatalf("ResolvedBinaryDir: %v", err)
	}
	if bd != "${sourceDir}/common-build" {
		t.Errorf("dev binaryDir = %q, want ${sourceDir}/common-build (viaA outranks viaB)", bd)
	}
}

// TestResolveBuildDirUnsetEnvMacroFailClosed proves an unset $env{} macro is a
// fail-closed error, never collapsed to an empty string the purge guard trusts.
func TestResolveBuildDirUnsetEnvMacroFailClosed(t *testing.T) {
	src := t.TempDir()

	// Unset env var → refused.
	if _, err := resolveBuildDirWithinSource("$env{MCP_CBUILD_DEFINITELY_UNSET}/build", src, "dev"); err == nil {
		t.Error("expected refusal for a binaryDir referencing an unset $env{} macro")
	}
	if _, err := resolveBuildDirWithinSource("$penv{MCP_CBUILD_DEFINITELY_UNSET}/build", src, "dev"); err == nil {
		t.Error("expected refusal for a binaryDir referencing an unset $penv{} macro")
	}

	// Set env var pointing inside the tree → resolves.
	t.Setenv("MCP_CBUILD_TESTDIR", "build-from-env")
	got, err := resolveBuildDirWithinSource("$env{MCP_CBUILD_TESTDIR}", src, "dev")
	if err != nil {
		t.Fatalf("set env macro rejected: %v", err)
	}
	if got != filepath.Join(filepath.Clean(src), "build-from-env") {
		t.Errorf("resolved = %q, want %q", got, filepath.Join(src, "build-from-env"))
	}
}

// TestExpandEnvMacroSelfReferenceTerminates proves a self-referential env value
// cannot make expandEnvMacro loop forever (the scan cursor advances past each
// substitution).
func TestExpandEnvMacroSelfReferenceTerminates(t *testing.T) {
	t.Setenv("MCP_CBUILD_SELF", "$env{MCP_CBUILD_SELF}/x")

	done := make(chan string, 1)
	go func() {
		out, _ := expandEnvMacro("$env{MCP_CBUILD_SELF}", "$env{")
		done <- out
	}()
	select {
	case out := <-done:
		// The substituted value is treated as a literal (not re-expanded).
		if out != "$env{MCP_CBUILD_SELF}/x" {
			t.Errorf("expandEnvMacro = %q, want the literal single substitution", out)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("expandEnvMacro did not terminate on a self-referential value")
	}
}

// TestNamedPresetInheritance proves build/test preset `inherits` is resolved so
// an inherited configurePreset is surfaced (previously omitted).
func TestNamedPresetInheritance(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "CMakePresets.json", `{
      "version": 6,
      "configurePresets": [ { "name": "dev", "binaryDir": "${sourceDir}/build" } ],
      "buildPresets": [
        { "name": "base-build", "hidden": true, "configurePreset": "dev" },
        { "name": "child-build", "inherits": ["base-build"] }
      ],
      "testPresets": [
        { "name": "base-test", "hidden": true, "configurePreset": "dev" },
        { "name": "child-test", "inherits": ["base-test"] }
      ]
    }`)

	p, err := LoadPresets(dir)
	if err != nil {
		t.Fatalf("LoadPresets: %v", err)
	}
	res := p.Result()

	child := findPreset(t, res.BuildPresets, "child-build")
	if child.ConfigurePreset != "dev" {
		t.Errorf("child-build configurePreset = %q, want dev (inherited from base-build)", child.ConfigurePreset)
	}
	ctest := findPreset(t, res.TestPresets, "child-test")
	if ctest.ConfigurePreset != "dev" {
		t.Errorf("child-test configurePreset = %q, want dev (inherited from base-test)", ctest.ConfigurePreset)
	}
}
