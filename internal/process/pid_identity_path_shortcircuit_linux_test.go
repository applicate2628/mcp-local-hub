//go:build linux

package process

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// POSIX twin of the Windows short-circuit security tests. The gate proves the
// process at a PID is the binary we spawned rather than an unrelated process
// that inherited a recycled PID; these assert the rejection path still rejects
// and that nothing is memoized between calls.
//
// The test process itself is the subject: /proc/self/exe is a real, live,
// verifiable process whose executable path we can compare against.

func testSelfExe(t *testing.T) string {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	resolved, err := filepath.EvalSymlinks(exe)
	if err != nil {
		t.Fatalf("EvalSymlinks(%q): %v", exe, err)
	}
	return filepath.Clean(resolved)
}

// TestVerifyPIDExecutablePathAcceptsOwnBinary pins that the short-circuit does
// not break the legitimate match.
func TestVerifyPIDExecutablePathAcceptsOwnBinary(t *testing.T) {
	if err := verifyPIDExecutablePath(os.Getpid(), testSelfExe(t)); err != nil {
		t.Fatalf("identity gate rejected our own binary: %v", err)
	}
}

// TestVerifyPIDExecutablePathRejectsForeignBinary is the anti-recycled-PID
// assertion: a different executable path must not satisfy the gate.
func TestVerifyPIDExecutablePathRejectsForeignBinary(t *testing.T) {
	foreign := filepath.Join(t.TempDir(), "attacker")
	if err := os.WriteFile(foreign, []byte("attacker"), 0o700); err != nil {
		t.Fatalf("write foreign binary: %v", err)
	}
	err := verifyPIDExecutablePath(os.Getpid(), foreign)
	if err == nil {
		t.Fatalf("identity gate accepted a foreign executable path %q for our own PID", foreign)
	}
	if !errors.Is(err, ErrProcessIdentityMismatch) {
		t.Fatalf("expected ErrProcessIdentityMismatch, got %v", err)
	}
}

// TestVerifyPIDExecutablePathRejectsSameBasenameForeignDirectory covers the
// planted-sibling case: same basename, different directory.
func TestVerifyPIDExecutablePathRejectsSameBasenameForeignDirectory(t *testing.T) {
	planted := filepath.Join(t.TempDir(), filepath.Base(testSelfExe(t)))
	if err := os.WriteFile(planted, []byte("planted"), 0o700); err != nil {
		t.Fatalf("write planted binary: %v", err)
	}
	if err := verifyPIDExecutablePath(os.Getpid(), planted); err == nil {
		t.Fatalf("identity gate accepted a same-basename executable from a foreign directory %q", planted)
	}
}

// TestVerifyPIDExecutablePathRejectsEmptyExpected pins the fail-closed posture
// for a missing proof.
func TestVerifyPIDExecutablePathRejectsEmptyExpected(t *testing.T) {
	if err := verifyPIDExecutablePath(os.Getpid(), ""); err == nil {
		t.Fatal("identity gate accepted an empty expected path")
	}
}

// TestNormalizeExpectedExecutablePathFollowsSymlinkRepoint is the anti-cache
// assertion on the POSIX normalizer: repointing a symlink between two calls
// must change the answer. A memo would return the first call's value.
func TestNormalizeExpectedExecutablePathFollowsSymlinkRepoint(t *testing.T) {
	root := t.TempDir()
	a := filepath.Join(root, "a", "mcphub")
	b := filepath.Join(root, "b", "mcphub")
	for _, p := range []string{a, b} {
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(p, []byte(filepath.Base(filepath.Dir(p))), 0o700); err != nil {
			t.Fatalf("write %q: %v", p, err)
		}
	}
	link := filepath.Join(root, "mcphub")
	if err := os.Symlink(a, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	first, err := normalizeExpectedExecutablePath(link)
	if err != nil {
		t.Fatalf("first normalize: %v", err)
	}
	if first != a {
		t.Fatalf("first normalize = %q, want %q", first, a)
	}

	if err := os.Remove(link); err != nil {
		t.Fatalf("remove link: %v", err)
	}
	if err := os.Symlink(b, link); err != nil {
		t.Fatalf("relink: %v", err)
	}

	second, err := normalizeExpectedExecutablePath(link)
	if err != nil {
		t.Fatalf("second normalize: %v", err)
	}
	if second == first {
		t.Fatalf("stale resolution: normalizeExpectedExecutablePath returned %q for %q both before and after the target was repointed from %q to %q", second, link, a, b)
	}
	if second != b {
		t.Fatalf("post-repoint normalize = %q, want %q", second, b)
	}
}
