package oneapi

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// makeOneAPILayout builds a fake oneAPI root under a temp dir with the
// given components, each created as "<root>\<component>\latest\bin"; a
// component listed in withDLL also gets a dummy "x.dll" inside its bin dir.
// Returns the root path. Uses the REAL os filesystem so the production
// realDirExists / realDirHasDLL / realListComponentDirs probes are
// exercised end-to-end (no seam override needed for these tests).
func makeOneAPILayout(t *testing.T, components, withDLL []string) string {
	t.Helper()
	root := filepath.Join(t.TempDir(), "oneAPI")
	dll := map[string]bool{}
	for _, c := range withDLL {
		dll[c] = true
	}
	for _, c := range components {
		bin := filepath.Join(root, c, "latest", "bin")
		if err := os.MkdirAll(bin, 0o755); err != nil {
			t.Fatalf("MkdirAll %s: %v", bin, err)
		}
		if dll[c] {
			f := filepath.Join(bin, "x.dll")
			if err := os.WriteFile(f, []byte("MZ"), 0o644); err != nil {
				t.Fatalf("WriteFile %s: %v", f, err)
			}
		}
	}
	return root
}

// TestDetectRootFound asserts DetectRoot resolves ONEAPI_ROOT when it
// points at an existing directory.
func TestDetectRootFound(t *testing.T) {
	if len(rootProbePaths()) == 0 {
		// POSIX: rootProbePaths is empty regardless of env, so DetectRoot can
		// never find a root. Assert the platform contract and return.
		t.Setenv("ONEAPI_ROOT", t.TempDir())
		if got, ok := DetectRoot(); ok {
			t.Fatalf("POSIX DetectRoot returned ok=true with no probe candidates; got %q", got)
		}
		return
	}
	root := makeOneAPILayout(t, []string{"mkl"}, []string{"mkl"})
	t.Setenv("ONEAPI_ROOT", root)

	got, ok := DetectRoot()
	if !ok {
		t.Fatalf("DetectRoot: want found, got ok=false")
	}
	if filepath.Clean(got) != filepath.Clean(root) {
		t.Fatalf("DetectRoot = %q, want %q", got, root)
	}
}

// TestDetectRootMissing asserts the ("",false) contract when the root does
// not exist on disk.
func TestDetectRootMissing(t *testing.T) {
	t.Setenv("ONEAPI_ROOT", filepath.Join(t.TempDir(), "nope"))
	// Override dirExists so the test is platform-independent: nothing exists.
	restore := SetSeamsForTest(func(string) bool { return false }, nil, nil)
	defer restore()

	got, ok := DetectRoot()
	if ok || got != "" {
		t.Fatalf("DetectRoot with nothing on disk = (%q, %v), want (\"\", false)", got, ok)
	}
}

// TestDLLDirsEnumeratesPriorityFirst asserts DLLDirs returns the
// mkl/tbb/compiler bin dirs FIRST (in that order), then any other
// dll-bearing component sorted, and EXCLUDES a component dir with no *.dll.
func TestDLLDirsEnumeratesPriorityFirst(t *testing.T) {
	// Components present: compiler, mkl, tbb (priority, all with DLLs),
	// mpi + ipp (others, with DLLs), dnnl (no DLL → excluded), and an
	// empty-bin "extra" (no DLL → excluded).
	components := []string{"compiler", "mkl", "tbb", "mpi", "ipp", "dnnl", "extra"}
	withDLL := []string{"compiler", "mkl", "tbb", "mpi", "ipp"}
	root := makeOneAPILayout(t, components, withDLL)

	got := DLLDirs(root)

	bin := func(c string) string { return filepath.Join(root, c, "latest", "bin") }
	want := []string{
		bin("mkl"), bin("tbb"), bin("compiler"), // priority, fixed order
		bin("ipp"), bin("mpi"), // others, sorted (ipp < mpi)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("DLLDirs =\n  %v\nwant\n  %v", got, want)
	}
}

// TestDLLDirsExcludesNoDLLComponent asserts a component dir whose bin holds
// no *.dll is excluded even though the dir exists.
func TestDLLDirsExcludesNoDLLComponent(t *testing.T) {
	root := makeOneAPILayout(t, []string{"mkl", "tbb"}, []string{"mkl"}) // tbb bin exists but has no dll
	got := DLLDirs(root)
	want := []string{filepath.Join(root, "mkl", "latest", "bin")}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("DLLDirs = %v, want %v (tbb excluded — no *.dll)", got, want)
	}
}

// TestDLLDirsEmptyRootIsNil asserts a root with no qualifying component bin
// dir returns nil (the no-op-with-warn path for the caller).
func TestDLLDirsEmptyRootIsNil(t *testing.T) {
	// Root exists but no component has a dll-bearing latest/bin.
	root := makeOneAPILayout(t, []string{"mkl"}, nil)
	if got := DLLDirs(root); got != nil {
		t.Fatalf("DLLDirs with no dll-bearing component = %v, want nil", got)
	}
	if got := DLLDirs(""); got != nil {
		t.Fatalf("DLLDirs(\"\") = %v, want nil", got)
	}
}

// TestDLLDirsAgainstSeam exercises DLLDirs through the injectable seams (no
// real filesystem), pinning the priority-first + dll-gating logic directly.
func TestDLLDirsAgainstSeam(t *testing.T) {
	root := `C:\fake\oneAPI`
	join := func(c string) string { return filepath.Join(root, c, "latest", "bin") }

	// Present component bin dirs: mkl, tbb, compiler, mpi (all "exist").
	existing := map[string]bool{
		join("mkl"):      true,
		join("tbb"):      true,
		join("compiler"): true,
		join("mpi"):      true,
	}
	// All present bins have a dll EXCEPT tbb (→ excluded).
	hasDLL := map[string]bool{
		join("mkl"):      true,
		join("tbb"):      false,
		join("compiler"): true,
		join("mpi"):      true,
	}

	restore := SetSeamsForTest(
		func(p string) bool { return existing[p] },
		func(p string) bool { return hasDLL[p] },
		func(string) []string { return []string{"mkl", "tbb", "compiler", "mpi"} },
	)
	defer restore()

	got := DLLDirs(root)
	want := []string{join("mkl"), join("compiler"), join("mpi")} // tbb excluded; mkl,compiler priority; mpi other
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("DLLDirs(seam) =\n  %v\nwant\n  %v", got, want)
	}
}
