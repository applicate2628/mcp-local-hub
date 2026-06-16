package vtune

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestErrVTuneNotFound_MessageNamesSearchLocations is a contract assertion on
// the not-found error text the handler surfaces verbatim: it must name
// VTUNE_PROFILER_DIR, the install dirs, and PATH so an operator can fix the
// install.
func TestErrVTuneNotFound_MessageNamesSearchLocations(t *testing.T) {
	msg := ErrVTuneNotFound.Error()
	for _, want := range []string{"VTUNE_PROFILER_DIR", "bin64", "oneAPI", "PATH", "vtune"} {
		if !strings.Contains(msg, want) {
			t.Errorf("ErrVTuneNotFound message missing %q: %s", want, msg)
		}
	}
}

// TestFindVTune_ProfilerDirHit verifies that pointing VTUNE_PROFILER_DIR at a
// dir whose bin64 holds a (fake) vtune.exe resolves to exactly that path —
// the explicit-override probe wins over everything else.
func TestFindVTune_ProfilerDirHit(t *testing.T) {
	tmp := t.TempDir()
	bin64 := filepath.Join(tmp, "bin64")
	if err := os.MkdirAll(bin64, 0o755); err != nil {
		t.Fatal(err)
	}
	exe := filepath.Join(bin64, "vtune.exe")
	if err := os.WriteFile(exe, []byte("MZ"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("VTUNE_PROFILER_DIR", tmp)

	got, err := findVTune()
	if err != nil {
		t.Fatalf("findVTune returned error despite a vtune.exe under VTUNE_PROFILER_DIR: %v", err)
	}
	if got != exe {
		t.Errorf("findVTune = %q, want %q", got, exe)
	}
}

// TestFindVTune_ProfilerDirMiss verifies that pointing VTUNE_PROFILER_DIR at a
// directory with no vtune.exe does NOT spuriously resolve to it — it falls
// through to the install-root / PATH probe. We can only assert the negative
// deterministically (a miss under the temp dir), since the host may or may
// not have a real install elsewhere; so we assert the function does not return
// the (nonexistent) temp-dir path.
func TestFindVTune_ProfilerDirMiss(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("VTUNE_PROFILER_DIR", tmp)

	path, err := findVTune()
	if err == nil {
		// A real install exists on this host; the resolved path must not be the
		// empty temp dir (which has no bin64\vtune.exe).
		if strings.HasPrefix(path, tmp) {
			t.Errorf("findVTune resolved to empty VTUNE_PROFILER_DIR dir: %s", path)
		}
		return
	}
	// No install anywhere — expect the canonical not-found error.
	if err != ErrVTuneNotFound {
		t.Errorf("findVTune error = %v, want ErrVTuneNotFound", err)
	}
}

// TestVersionedSubdirs sorts version dirs newest-first and excludes "latest".
func TestVersionedSubdirs(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"2025.4", "2026.2", "latest"} {
		if err := os.MkdirAll(filepath.Join(root, name), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// A stray file must not be treated as a version dir.
	if err := os.WriteFile(filepath.Join(root, "readme.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	got := versionedSubdirs(root)
	want := []string{"2026.2", "2025.4"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("versionedSubdirs = %v, want %v (newest first, no 'latest', no files)", got, want)
	}
}

// TestIsExecutableFile distinguishes a real file from a directory and a
// missing path — the predicate findVTune relies on.
func TestIsExecutableFile(t *testing.T) {
	dir := t.TempDir()
	if isExecutableFile(dir) {
		t.Error("isExecutableFile(dir) = true, want false (directory is not a regular file)")
	}
	if isExecutableFile(filepath.Join(dir, "nope.exe")) {
		t.Error("isExecutableFile(missing) = true, want false")
	}
	f := filepath.Join(dir, "real.exe")
	if err := os.WriteFile(f, []byte("MZ"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !isExecutableFile(f) {
		t.Error("isExecutableFile(real file) = false, want true")
	}
}
