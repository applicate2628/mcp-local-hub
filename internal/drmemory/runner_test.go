package drmemory

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// TestBuildDrMemoryArgs asserts the flag wiring without spawning a
// process: -batch always present, -light / -no_check_uninitialized only
// on their toggles, -symcache_dir only when a cache dir is supplied (and
// before -logdir), -logdir + -- separator + target + args in order.
func TestBuildDrMemoryArgs(t *testing.T) {
	tests := []struct {
		name        string
		symcache    string
		light       bool
		checkUninit bool
		want        []string
	}{
		{
			name:        "defaults",
			light:       false,
			checkUninit: true,
			want:        []string{"-batch", "-logdir", "LOG", "--", "t.exe", "-a", "-b"},
		},
		{
			name:        "light",
			light:       true,
			checkUninit: true,
			want:        []string{"-batch", "-light", "-logdir", "LOG", "--", "t.exe", "-a", "-b"},
		},
		{
			name:        "no-uninit",
			light:       false,
			checkUninit: false,
			want:        []string{"-batch", "-no_check_uninitialized", "-logdir", "LOG", "--", "t.exe", "-a", "-b"},
		},
		{
			name:        "light+no-uninit",
			light:       true,
			checkUninit: false,
			want:        []string{"-batch", "-light", "-no_check_uninitialized", "-logdir", "LOG", "--", "t.exe", "-a", "-b"},
		},
		{
			name:        "with-symcache",
			symcache:    "SYM",
			light:       true,
			checkUninit: true,
			want:        []string{"-batch", "-light", "-symcache_dir", "SYM", "-logdir", "LOG", "--", "t.exe", "-a", "-b"},
		},
		{
			name:        "empty-symcache-omits-flag",
			symcache:    "",
			light:       false,
			checkUninit: true,
			want:        []string{"-batch", "-logdir", "LOG", "--", "t.exe", "-a", "-b"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := buildDrMemoryArgs("LOG", tc.symcache, "t.exe", []string{"-a", "-b"}, tc.light, tc.checkUninit)
			if strings.Join(got, "|") != strings.Join(tc.want, "|") {
				t.Errorf("buildDrMemoryArgs = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestSymcacheDirUsesPrivateUserCacheOnNonWindows(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("non-Windows cache location test")
	}
	cacheRoot := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", cacheRoot)

	got := symcacheDir()
	want := filepath.Join(cacheRoot, "mcp-local-hub", "drmemory", "symcache")
	if got != want {
		t.Fatalf("symcacheDir() = %q, want %q", got, want)
	}
	info, err := os.Stat(got)
	if err != nil {
		t.Fatal(err)
	}
	if mode := info.Mode().Perm(); mode != 0o700 {
		t.Fatalf("symcacheDir mode = %o, want 700", mode)
	}
}

// TestReadResultsTxt_FindsPerRunSubdir verifies the logdir traversal:
// Dr. Memory writes results.txt under DrMemory-<exe>.<pid>.NNN/, and
// readResultsTxt must locate and read it.
func TestReadResultsTxt_FindsPerRunSubdir(t *testing.T) {
	logdir := t.TempDir()
	sub := filepath.Join(logdir, "DrMemory-app.exe.4242.000")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	want := "Error #1: LEAK 8 direct bytes\n"
	if err := os.WriteFile(filepath.Join(sub, "results.txt"), []byte(want), 0o644); err != nil {
		t.Fatal(err)
	}

	path, body := readResultsTxt(logdir)
	if body != want {
		t.Errorf("body = %q, want %q", body, want)
	}
	if filepath.Base(path) != "results.txt" {
		t.Errorf("path = %q, want a results.txt path", path)
	}
}

// TestReadResultsTxt_EmptyLogdir returns no path / no body when Dr. Memory
// produced no results.txt (e.g. it failed before writing one).
func TestReadResultsTxt_EmptyLogdir(t *testing.T) {
	logdir := t.TempDir()
	path, body := readResultsTxt(logdir)
	if path != "" || body != "" {
		t.Errorf("expected empty path/body for empty logdir, got path=%q body=%q", path, body)
	}
}

// TestReadResultsTxt_PicksNewestSubdir verifies that when a logdir holds
// several per-run subdirs the newest results.txt wins.
func TestReadResultsTxt_PicksNewestSubdir(t *testing.T) {
	logdir := t.TempDir()
	older := filepath.Join(logdir, "DrMemory-app.exe.1.000")
	newer := filepath.Join(logdir, "DrMemory-app.exe.2.000")
	for _, d := range []string{older, newer} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(older, "results.txt"), []byte("OLD"), 0o644); err != nil {
		t.Fatal(err)
	}
	newerResults := filepath.Join(newer, "results.txt")
	if err := os.WriteFile(newerResults, []byte("NEW"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Make the newer file's mtime strictly later so the selection is
	// deterministic regardless of filesystem timestamp granularity.
	future := time.Now().Add(time.Hour)
	if err := os.Chtimes(newerResults, future, future); err != nil {
		t.Fatal(err)
	}

	_, body := readResultsTxt(logdir)
	if body != "NEW" {
		t.Errorf("body = %q, want NEW (newest subdir's results.txt)", body)
	}
}

// TestFormatCommandLine quotes space-bearing tokens (the real drmemory.exe
// lives under "C:\Program Files (x86)\Dr. Memory\bin64\") so the surfaced
// command line stays one pasteable token per arg.
func TestFormatCommandLine(t *testing.T) {
	exe := `C:\Program Files (x86)\Dr. Memory\bin64\drmemory.exe`
	args := []string{"-batch", "-logdir", `C:\tmp\logs`, "--", `C:\proj\app.exe`, "--flag"}
	got := formatCommandLine(exe, args)

	// The space-bearing exe path must be wrapped in quotes.
	if !strings.Contains(got, `"`+exe+`"`) {
		t.Errorf("command line did not quote the space-bearing exe path: %s", got)
	}
	// Space-free args must NOT be quoted.
	if strings.Contains(got, `"-batch"`) {
		t.Errorf("space-free arg was needlessly quoted: %s", got)
	}
	// Every token must be present.
	for _, want := range append([]string{exe}, args...) {
		if !strings.Contains(got, want) {
			t.Errorf("command line missing token %q: %s", want, got)
		}
	}
}

// TestTruncate caps oversized bodies with a visible marker.
func TestTruncate(t *testing.T) {
	if got := truncate("short", 100); got != "short" {
		t.Errorf("truncate(short) = %q, want unchanged", got)
	}
	long := strings.Repeat("x", 50)
	got := truncate(long, 10)
	if len(got) <= 10 || !strings.Contains(got, "[truncated]") {
		t.Errorf("truncate did not mark clipped output: %q", got)
	}
}
