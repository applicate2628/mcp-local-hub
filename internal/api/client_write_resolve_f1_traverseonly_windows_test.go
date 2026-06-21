//go:build windows

package api

import (
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/windows"
)

// AF-1 F1 / Finding 1 — Windows traverse-only ancestor descent.
//
// The component descent used to open every intermediate (and the volume-root
// anchor) with FILE_LIST_DIRECTORY (openExistingRealDirAt /
// openDirHandleNoReparse). Ordinary Windows path traversal — and the old
// full-parent open — only need TRAVERSE on ancestors, so a consented symlink
// on a UNC share (or under an ancestor that grants TRAVERSE but denies
// directory LISTING) failed before reaching the writable final parent. The
// fix opens the root anchor + intermediates TRAVERSE-ONLY
// (FILE_TRAVERSE|FILE_READ_ATTRIBUTES, NO FILE_LIST_DIRECTORY) while keeping
// the FINAL parent on the full LIST/READ_CONTROL open — the Windows analog of
// the POSIX O_PATH search-only split. The reparse-point refusal MUST still
// fire on the traverse-only open (the intermediate-swap TOCTOU closure cannot
// regress).

// TestF1_TraverseOnlyAccessMask_ExcludesListDirectory is the Finding 1 flags
// assertion. The traverse-only intermediate/anchor access must request
// FILE_TRAVERSE (descend) + FILE_READ_ATTRIBUTES (so the reparse refusal's
// GetFileInformationByHandle can read the attribute bits) and must NOT request
// FILE_LIST_DIRECTORY (the right to ENUMERATE — never needed by the descent,
// and requiring it is exactly what the old open did wrong on a
// traverse-but-no-list ancestor).
func TestF1_TraverseOnlyAccessMask_ExcludesListDirectory(t *testing.T) {
	if traverseOnlyDirAccess&windows.FILE_TRAVERSE == 0 {
		t.Errorf("traverseOnlyDirAccess (%#x) must include FILE_TRAVERSE (%#x) so the descent can traverse the ancestor",
			traverseOnlyDirAccess, windows.FILE_TRAVERSE)
	}
	if traverseOnlyDirAccess&windows.FILE_READ_ATTRIBUTES == 0 {
		t.Errorf("traverseOnlyDirAccess (%#x) must include FILE_READ_ATTRIBUTES (%#x) — refuseReparsePointHandle's GetFileInformationByHandle needs it",
			traverseOnlyDirAccess, windows.FILE_READ_ATTRIBUTES)
	}
	if traverseOnlyDirAccess&windows.FILE_LIST_DIRECTORY != 0 {
		t.Errorf("traverseOnlyDirAccess (%#x) must NOT include FILE_LIST_DIRECTORY (%#x) — the descent never enumerates, and requiring LIST rejects a traverse-but-no-list ancestor (the Finding 1 bug)",
			traverseOnlyDirAccess, windows.FILE_LIST_DIRECTORY)
	}
	// FILE_LIST_DIRECTORY (0x1) and FILE_TRAVERSE (0x20) are distinct bits;
	// guard against a future refactor collapsing them.
	if windows.FILE_LIST_DIRECTORY == windows.FILE_TRAVERSE {
		t.Fatalf("FILE_LIST_DIRECTORY and FILE_TRAVERSE collapsed to the same value (%#x); the traverse-only distinction is meaningless",
			windows.FILE_TRAVERSE)
	}
}

// TestF1_TraverseOnlyDirAt_OpensRealDir_RefusesJunction proves the
// traverse-only intermediate open (a) SUCCEEDS on a real directory — so
// FILE_TRAVERSE|FILE_READ_ATTRIBUTES is sufficient to descend AND the reparse
// refusal's GetFileInformationByHandle works with the retained
// FILE_READ_ATTRIBUTES — and (b) still REFUSES a reparse point (junction), so
// the intermediate-swap TOCTOU closure is preserved on the lower-access open.
func TestF1_TraverseOnlyDirAt_OpensRealDir_RefusesJunction(t *testing.T) {
	root := hardenedTempDir(t)

	// A real child directory under the parent — the traverse-only open must
	// succeed and the reparse check must pass (it is a real dir, not a link).
	realChild := filepath.Join(root, "real-intermediate")
	if err := os.Mkdir(realChild, 0o700); err != nil {
		t.Fatalf("mkdir real intermediate: %v", err)
	}

	// A junction child pointing elsewhere — the traverse-only open must REFUSE
	// it (reparse-point refusal) rather than follow it.
	junctionTarget := filepath.Join(t.TempDir(), "junction-target")
	if err := os.Mkdir(junctionTarget, 0o700); err != nil {
		t.Fatalf("mkdir junction target: %v", err)
	}
	junctionChild := filepath.Join(root, "junction-intermediate")
	if err := createJunctionForTest(junctionChild, junctionTarget); err != nil {
		t.Skipf("junction creation unsupported in this environment: %v", err)
	}

	// Open the parent (root) handle. Use the full open here (this stands in
	// for "the handle the descent is currently holding"); the child opens are
	// the traverse-only step under test.
	parent, err := openDirHandleNoReparse(root)
	if err != nil {
		t.Fatalf("open parent root handle: %v", err)
	}
	defer windows.CloseHandle(parent)

	// (a) Traverse-only open of a REAL intermediate must succeed.
	h, err := openTraverseOnlyDirAt(parent, "real-intermediate")
	if err != nil {
		t.Fatalf("openTraverseOnlyDirAt on a real directory must succeed (traverse-only access is sufficient to descend + read attributes): %v", err)
	}
	// The reparse refusal ran inside openTraverseOnlyDirAt via
	// GetFileInformationByHandle; reaching here with a nil error proves
	// FILE_READ_ATTRIBUTES on the traverse-only handle was sufficient for that
	// attribute read.
	_ = windows.CloseHandle(h)

	// (b) Traverse-only open of a JUNCTION must be REFUSED (reparse point),
	// proving the TOCTOU closure is unchanged on the lower-access open.
	if _, err := openTraverseOnlyDirAt(parent, "junction-intermediate"); err == nil {
		t.Fatal("openTraverseOnlyDirAt followed a junction; the reparse-point refusal must fire on the traverse-only open (TOCTOU closure regressed)")
	}
}

// TestF1_TraverseOnlyAnchor_OpensVolumeRoot is a smoke test that the
// traverse-only volume-root anchor open succeeds against a real drive root.
// It proves FILE_TRAVERSE|FILE_READ_ATTRIBUTES (no FILE_LIST_DIRECTORY) is
// sufficient to anchor the descent at the volume root, which is the access
// ordinary path traversal uses.
func TestF1_TraverseOnlyAnchor_OpensVolumeRoot(t *testing.T) {
	vol := filepath.VolumeName(os.Getenv("SystemDrive"))
	if vol == "" {
		// Fall back to the volume of the temp dir.
		vol = filepath.VolumeName(t.TempDir())
	}
	if vol == "" {
		t.Skip("could not determine a volume root to anchor at")
	}
	anchorPath := vol + `\`
	h, err := openTraverseOnlyDirHandleNoReparse(anchorPath)
	if err != nil {
		t.Fatalf("openTraverseOnlyDirHandleNoReparse(%q) must succeed with traverse-only access: %v", anchorPath, err)
	}
	_ = windows.CloseHandle(h)
}
