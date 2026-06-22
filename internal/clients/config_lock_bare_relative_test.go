package clients

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// rejectNonAbsoluteSecureCreateParentDir mimics the PRODUCTION secure parent
// creator's contract for the purpose of these tests: it REFUSES a "." / empty /
// non-absolute dir exactly as api.secureCreateParentDirAnywhereImpl does
// (windows leg: "has no volume name (not an absolute drive/UNC path)"; posix leg:
// "is not absolute"), and for an absolute dir it delegates to a plain idempotent
// MkdirAll so the surrounding flow proceeds. The test DEFAULT
// (fallbackSecureCreateParentDir) is a bare os.MkdirAll that ACCEPTS "." as a
// no-op success, so it would NOT reproduce the bare-relative regression — this
// mock restores the production rejection the bug depends on.
func rejectNonAbsoluteSecureCreateParentDir(t *testing.T) func() string {
	t.Helper()
	prev := SecureCreateParentDir
	t.Cleanup(func() { SecureCreateParentDir = prev })
	var lastArg string
	SecureCreateParentDir = func(dir string) error {
		lastArg = dir
		if dir == "" || dir == "." || !filepath.IsAbs(dir) {
			return errors.New("secure mkparent: dir is not absolute (production contract)")
		}
		return os.MkdirAll(dir, 0o700)
	}
	return func() string { return lastArg }
}

// withCwd switches the process working directory to dir for the duration of the
// test and restores it on cleanup, so a bare-relative config write lands inside
// the sandbox temp dir rather than the repository tree. Process cwd is global
// state, so these tests must NOT run in parallel (no t.Parallel()).
func withCwd(t *testing.T, dir string) {
	t.Helper()
	orig, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatalf("chdir %s: %v", dir, err)
	}
	t.Cleanup(func() { _ = os.Chdir(orig) })
}

// TestConfigLock_BareRelativeConfigPathWriteSucceeds pins bot PR #420 r18
// LOW/MEDIUM finding: withConfigLock must normalize a BARE-RELATIVE config path's
// parent to ABSOLUTE before the unconditional secure-parent-create (r18 P2a), so
// adapters with a documented bare-relative fallback config path — copilot-cli
// ("mcp-config.json") and qoder ("mcp-settings.json"), which both degrade to a
// bare basename when os.UserHomeDir() fails — keep working. Without the
// filepath.Abs normalization, filepath.Dir is "." and the production secure
// creator rejects "." (not absolute) → the write FAILS at the chokepoint.
//
// The test uses the production-contract mock (rejects non-absolute) so the
// regression is reproducible (the default MkdirAll fallback accepts ".").
func TestConfigLock_BareRelativeConfigPathWriteSucceeds(t *testing.T) {
	cases := []struct {
		name       string
		barePath   string
		newAdapter func(barePath string) Client
	}{
		{
			name:     "copilot-cli",
			barePath: "mcp-config.json",
			newAdapter: func(p string) Client {
				return newLockingClient(&copilotCLIClient{jsonMCPClient: &jsonMCPClient{
					path: p, clientName: "copilot-cli", urlField: "url",
				}})
			},
		},
		{
			name:     "qoder",
			barePath: "mcp-settings.json",
			newAdapter: func(p string) Client {
				return newLockingClient(&qoderClient{jsonMCPClient: &jsonMCPClient{
					path: p, clientName: "qoder", urlField: "url",
				}})
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sandbox := t.TempDir()
			withCwd(t, sandbox)
			lastAbs := rejectNonAbsoluteSecureCreateParentDir(t)

			c := tc.newAdapter(tc.barePath)
			if err := c.AddEntry(MCPEntry{Name: "a", URL: "http://127.0.0.1:9121/mcp"}); err != nil {
				t.Fatalf("bare-relative AddEntry must succeed through withConfigLock, got: %v", err)
			}
			// The secure creator must have been called with an ABSOLUTE dir (the
			// filepath.Abs normalization), not the bare ".".
			got := lastAbs()
			if !filepath.IsAbs(got) {
				t.Fatalf("SecureCreateParentDir called with non-absolute %q — withConfigLock did not normalize the bare-relative parent to absolute", got)
			}
			// The bare-relative file must have been written inside the sandbox cwd.
			if _, err := os.Stat(filepath.Join(sandbox, tc.barePath)); err != nil {
				t.Fatalf("write-target %q not created in sandbox cwd: %v", tc.barePath, err)
			}
		})
	}
}

// TestConfigLock_AbsoluteSymlinkedParentStillRefused confirms the bare-relative
// fix does NOT reopen the r18 P2a symlink gap for REAL absolute config paths: an
// absolute config path whose parent dir is a SYMLINK must still be refused by the
// secure creator (filepath.Abs is a Clean-only no-op on an already-absolute dir,
// so the symlink-refusing volume-root descent applies unchanged). The mock here
// rejects any dir that resolves through a symlink, standing in for the production
// reparse-point refusal (which needs the real OS-specific descent, exercised by
// the internal/api secure-create tests).
func TestConfigLock_AbsoluteSymlinkedParentStillRefused(t *testing.T) {
	if runtime.GOOS == "windows" {
		// Symlink creation needs admin/developer-mode on Windows; the production
		// reparse refusal is covered by the internal/api windows secure-create
		// tests. This clients-layer test pins the POSIX-creatable symlink case.
		t.Skip("symlink creation requires elevated privileges on Windows; reparse refusal covered in internal/api")
	}
	root := t.TempDir()
	realParent := filepath.Join(root, "real")
	if err := os.Mkdir(realParent, 0o700); err != nil {
		t.Fatalf("mkdir real parent: %v", err)
	}
	linkParent := filepath.Join(root, "link")
	if err := os.Symlink(realParent, linkParent); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	// Absolute config path whose parent dir is the symlink.
	configPath := filepath.Join(linkParent, "mcp.json")

	errInsecureParent := errors.New("secure mkparent: refuse symlinked parent (production reparse-refusal stand-in)")
	prev := SecureCreateParentDir
	t.Cleanup(func() { SecureCreateParentDir = prev })
	SecureCreateParentDir = func(dir string) error {
		// Stand-in for the production volume-root descent: refuse a parent that is
		// (or resolves through) a symlink. filepath.EvalSymlinks differing from the
		// cleaned input means a symlink component is present.
		resolved, err := filepath.EvalSymlinks(dir)
		if err == nil && resolved != filepath.Clean(dir) {
			return errInsecureParent
		}
		return os.MkdirAll(dir, 0o700)
	}

	c := newLockingClient(&cursorClient{jsonMCPClient: &jsonMCPClient{
		path: configPath, clientName: "cursor", urlField: "url",
	}})
	err := c.AddEntry(MCPEntry{Name: "a", URL: "http://127.0.0.1:9121/mcp"})
	if err == nil {
		t.Fatal("AddEntry through a symlinked absolute parent must be REFUSED, got nil")
	}
	if !errors.Is(err, errInsecureParent) {
		t.Fatalf("expected a secure-create refusal for a symlinked absolute parent, got: %v", err)
	}
}
