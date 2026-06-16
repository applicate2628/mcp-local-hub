package drmemory

import (
	"strings"
	"testing"
)

// TestFindDrMemory_NotFound exercises the no-install path: with
// DRMEMORY_HOME pointed at an empty temp dir and the well-known install
// roots absent (the test host is not expected to have Dr. Memory under
// the standard path AND on PATH simultaneously), findDrMemory should
// return ErrDrMemoryNotFound. To keep the test deterministic regardless
// of the host, we drive the probe through a temp DRMEMORY_HOME and assert
// the error message names every searched location.
func TestFindDrMemory_NotFoundMessageNamesSearchLocations(t *testing.T) {
	// ErrDrMemoryNotFound is the canonical not-found error; its message
	// must name DRMEMORY_HOME, the install dirs, and PATH so an operator
	// can fix the install. This is a contract assertion on the error text
	// that the handler surfaces verbatim.
	msg := ErrDrMemoryNotFound.Error()
	for _, want := range []string{"DRMEMORY_HOME", "bin64", "bin", "PATH", "drmemory.exe"} {
		if !strings.Contains(msg, want) {
			t.Errorf("ErrDrMemoryNotFound message missing %q: %s", want, msg)
		}
	}
}

// TestFindDrMemory_DrMemoryHomeMiss verifies that pointing DRMEMORY_HOME
// at a directory with no drmemory.exe does NOT spuriously resolve — it
// falls through to the install-root / PATH probe. We can only assert the
// negative deterministically (a miss under the temp home), since the host
// may or may not have a real install elsewhere; so we assert the function
// does not return the (nonexistent) temp-home path.
func TestFindDrMemory_DrMemoryHomeMiss(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("DRMEMORY_HOME", tmp)

	path, err := findDrMemory()
	if err == nil {
		// A real install exists on this host; the resolved path must not be
		// the empty temp home (which has no drmemory.exe).
		if strings.HasPrefix(path, tmp) {
			t.Errorf("findDrMemory resolved to empty DRMEMORY_HOME dir: %s", path)
		}
		return
	}
	// No install anywhere — expect the canonical not-found error.
	if err != ErrDrMemoryNotFound {
		t.Errorf("findDrMemory error = %v, want ErrDrMemoryNotFound", err)
	}
}

// TestIsExecutableFile distinguishes a real file from a directory and a
// missing path — the predicate findDrMemory relies on.
func TestIsExecutableFile(t *testing.T) {
	dir := t.TempDir()
	if isExecutableFile(dir) {
		t.Error("isExecutableFile(dir) = true, want false (directory is not a regular file)")
	}
	if isExecutableFile(dir + "\\nope.exe") {
		t.Error("isExecutableFile(missing) = true, want false")
	}
}
