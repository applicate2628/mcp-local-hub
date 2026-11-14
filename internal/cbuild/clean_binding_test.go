package cbuild

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeCleanCache(t *testing.T, source, name string) string {
	t.Helper()
	dir := filepath.Join(source, "out", name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, dir, "CMakeCache.txt", fmt.Sprintf("CMAKE_CACHEFILE_DIR:INTERNAL=%s\nCMAKE_HOME_DIRECTORY:INTERNAL=%s\n", dir, source))
	writeFile(t, dir, "keep.txt", "must survive unrelated clean\n")
	return dir
}

func TestCmakeCleanRejectsUnboundPreset(t *testing.T) {
	for _, boundDev := range []bool{false, true} {
		for _, purge := range []bool{false, true} {
			t.Run(fmt.Sprintf("dev_bound_%t/purge_%t", boundDev, purge), func(t *testing.T) {
				source := t.TempDir()
				dev := writeCleanCache(t, source, "dev")
				writeFile(t, source, "CMakePresets.json", `{
					"version": 3,
					"configurePresets": [
						{"name": "dev", "binaryDir": "${sourceDir}/out/dev"},
						{"name": "release", "binaryDir": "${sourceDir}/out/release"}
					]
				}`)
				b := &builder{configured: make(map[configuredCacheKey]string)}
				if boundDev {
					if _, err := b.settleConfiguredCache(source, "dev", "-- Build files have been written to: "+dev); err != nil {
						t.Fatal(err)
					}
				}
				// An empty map models a restarted daemon; the other case models
				// a live daemon that has configured only a different preset.
				_, err := callWithArgs(t, b.cmakeClean(), map[string]any{
					"working_dir": source, "preset": "release", "purge_build_dir": purge,
				})
				if err == nil || !strings.Contains(err.Error(), "cmake_configure") {
					t.Fatalf("unbound release clean error = %v, want configure-first refusal", err)
				}
				sourceKey, err := canonicalExistingDir(source)
				if err != nil {
					t.Fatal(err)
				}
				if _, ok := b.configured[configuredCacheKey{sourceDir: sourceKey, preset: "release"}]; ok {
					t.Fatal("refused clean fabricated a release binding")
				}
				for _, name := range []string{"CMakeCache.txt", "keep.txt"} {
					if _, err := os.Stat(filepath.Join(dev, name)); err != nil {
						t.Fatalf("unrelated dev file %s changed: %v", name, err)
					}
				}
			})
		}
	}
}

func TestCmakeCleanUsesOnlySettledPreset(t *testing.T) {
	source := t.TempDir()
	dev := writeCleanCache(t, source, "dev")
	release := writeCleanCache(t, source, "release")
	b := &builder{configured: make(map[configuredCacheKey]string)}
	for preset, dir := range map[string]string{"dev": dev, "release": release} {
		if _, err := b.settleConfiguredCache(source, preset, "-- Build files have been written to: "+dir); err != nil {
			t.Fatal(err)
		}
		got, err := b.configuredCacheDir(context.Background(), source, preset)
		want, wantErr := canonicalExistingDir(dir)
		if err != nil || wantErr != nil || !sameCanonicalPath(got, want) {
			t.Fatalf("preset %s resolved to %q, want %q: %v / %v", preset, got, want, err, wantErr)
		}
	}
	if _, err := callWithArgs(t, b.cmakeClean(), map[string]any{
		"working_dir": source, "preset": "release", "purge_build_dir": true,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(release); !os.IsNotExist(err) {
		t.Fatalf("release was not purged: %v", err)
	}
	if data, err := os.ReadFile(filepath.Join(dev, "keep.txt")); err != nil || string(data) != "must survive unrelated clean\n" {
		t.Fatalf("dev changed: data=%q err=%v", data, err)
	}
	// A stale binding must not fall back to the remaining dev cache either.
	if _, err := b.configuredCacheDir(context.Background(), source, "release"); err == nil {
		t.Fatal("missing release tree fell back to dev")
	}
}

func TestCmakeCleanRevalidatesSettledCache(t *testing.T) {
	source := t.TempDir()
	dev := writeCleanCache(t, source, "dev")
	b := &builder{configured: make(map[configuredCacheKey]string)}
	if _, err := b.settleConfiguredCache(source, "dev", "-- Build files have been written to: "+dev); err != nil {
		t.Fatal(err)
	}
	otherSource := t.TempDir()
	writeFile(t, dev, "CMakeCache.txt", fmt.Sprintf("CMAKE_CACHEFILE_DIR:INTERNAL=%s\nCMAKE_HOME_DIRECTORY:INTERNAL=%s\n", dev, otherSource))
	if _, err := callWithArgs(t, b.cmakeClean(), map[string]any{
		"working_dir": source, "preset": "dev", "purge_build_dir": true,
	}); err == nil {
		t.Fatal("clean accepted a cache whose source identity changed")
	}
	if _, err := os.Stat(filepath.Join(dev, "keep.txt")); err != nil {
		t.Fatalf("invalid cache tree was purged: %v", err)
	}
}
