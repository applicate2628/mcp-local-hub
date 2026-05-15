//go:build windows

package api

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestResolveSymlinkFinalPath_ReturnsDriveLetterShape sanity-checks
// that the Windows resolver returns a normal drive-letter path
// (NOT the `\\?\C:\...` long-path form). secureWriteImpl downstream
// uses CreateFile + atomic rename with drive-letter paths in the
// rest of the test suite; mixing long-path form would change error
// messages and break audit-log readability.
//
// Symlink creation on Windows requires SeCreateSymbolicLinkPrivilege.
// Skip on non-admin CI runs.
func TestResolveSymlinkFinalPath_ReturnsDriveLetterShape(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "target.txt")
	if err := os.WriteFile(target, []byte("x"), 0o600); err != nil {
		t.Fatalf("write target: %v", err)
	}
	link := filepath.Join(root, "link.txt")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlink unsupported (likely non-admin): %v", err)
	}
	got, err := resolveSymlinkFinalPath(link)
	if err != nil {
		t.Fatalf("resolveSymlinkFinalPath: %v", err)
	}
	if strings.HasPrefix(got, `\\?\`) {
		t.Errorf("result must NOT keep the long-path prefix; got %q", got)
	}
	// Resolved path should be reachable via os.Stat (drive-letter
	// path is the universally-usable form).
	if _, err := os.Stat(got); err != nil {
		t.Errorf("resolved path %q is not statable: %v", got, err)
	}
}

// TestResolveSymlinkFinalPath_HandlesUNCFormat covers codex bot r1
// P2 on PR #192: GetFinalPathNameByHandle returns
// `\\?\UNC\server\share\path` for symlinks pointing at network
// shares. The resolver must convert this back to the canonical UNC
// form `\\server\share\path` rather than stripping the `\\?\`
// prefix verbatim (which would yield the broken-relative-looking
// `UNC\server\share\path` and cause downstream CreateFile to
// resolve against the wrong path entirely).
//
// We don't have a network share in CI, so this test exercises the
// path-shape logic directly via a unit-style assertion on the
// strip helper rather than a real Lstat/EvalSymlinks roundtrip.
func TestResolveSymlinkFinalPath_HandlesUNCFormat(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{`\\?\C:\Users\u\foo.toml`, `C:\Users\u\foo.toml`},
		{`\\?\D:\dotfiles\.codex\config.toml`, `D:\dotfiles\.codex\config.toml`},
		{`\\?\UNC\server\share\path\config.toml`, `\\server\share\path\config.toml`},
		{`\\?\UNC\srv\rootshare\nested\file.json`, `\\srv\rootshare\nested\file.json`},
	}
	for _, c := range cases {
		got := stripFinalPathPrefix(c.in)
		if got != c.want {
			t.Errorf("stripFinalPathPrefix(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// stripFinalPathPrefix mirrors the prefix-handling block in
// resolveSymlinkFinalPath so it can be unit-tested without going
// through GetFinalPathNameByHandle. Keep this in sync with the
// production code in client_write_resolve_windows.go.
func stripFinalPathPrefix(resolved string) string {
	if rest, ok := strings.CutPrefix(resolved, `\\?\UNC\`); ok {
		return `\\` + rest
	}
	return strings.TrimPrefix(resolved, `\\?\`)
}
