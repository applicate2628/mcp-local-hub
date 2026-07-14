package cbuild

import (
	"os"
	"path/filepath"
	"testing"
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
