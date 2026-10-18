package cbuild

import (
	"context"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestProjectPathKeyCasePolicy(t *testing.T) {
	// Pure key checks cover UNC volumes without requiring a network share.
	for _, root := range []string{`\\Server\Share\repo`, `C:\Repo`, "/Repo"} {
		a, b := projectPathKey(root), projectPathKey(strings.ToLower(root))
		if (a == b) != (runtime.GOOS == "windows") {
			t.Fatalf("case identity on %s for %q: %q vs %q", runtime.GOOS, root, a, b)
		}
		dev := configuredCacheKey{sourceDir: a, preset: "dev"}
		release := configuredCacheKey{sourceDir: a, preset: "release"}
		if dev == release {
			t.Fatal("preset identity was folded with the source path")
		}
	}
}

func TestConfiguredBindingUsesNormalizedSourceKey(t *testing.T) {
	source := t.TempDir()
	buildDir := writeCleanCache(t, source, "dev")
	b := &builder{configured: make(map[configuredCacheKey]string)}
	if _, err := b.settleConfiguredCache(source, "dev", "-- Build files have been written to: "+buildDir); err != nil {
		t.Fatal(err)
	}
	canonical, err := canonicalExistingDir(source)
	if err != nil {
		t.Fatal(err)
	}
	key := configuredCacheKey{sourceDir: projectPathKey(canonical), preset: "dev"}
	if _, found := b.configured[key]; !found {
		t.Fatal("settled binding did not use the normalized key")
	}
	alias := filepath.Join(source, ".")
	if runtime.GOOS == "windows" {
		alias = strings.ToUpper(alias)
	}
	want, err := canonicalExistingDir(buildDir)
	if err != nil {
		t.Fatal(err)
	}
	got, err := b.configuredCacheDir(context.Background(), alias, "dev")
	if err != nil || !sameCanonicalPath(got, want) {
		t.Fatalf("equivalent-root lookup: %q %v", got, err)
	}
	if _, err := b.configuredCacheDir(context.Background(), alias, "release"); err == nil {
		t.Fatal("different preset reused the binding")
	}
	if err := b.forgetConfiguredProject(alias); err != nil {
		t.Fatal(err)
	}
	if len(b.configured) != 0 {
		t.Fatal("equivalent-root invalidation retained the binding")
	}
}
