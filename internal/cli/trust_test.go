// Package cli — tests for the `mcphub trust` / `untrust` verb group (area-5
// gap-b). The commands are PURE adapters over the api trusted-roots owners, so
// the tests cover (1) input-validation parity with `setup --trusted-root` and
// the GUI add-root handler, and (2) the end-to-end round-trip through the
// REDIRECTED trusted-roots store (state dir pointed at a temp tree via
// api.SetDaemonStateRootForTest + LOCALAPPDATA/XDG so the real %LOCALAPPDATA%
// store is NEVER touched).
package cli

import (
	"bytes"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"mcp-local-hub/internal/api"
)

// withTrustStore redirects the trusted-roots store to a per-test temp tree so
// the `trust`/`untrust`/`list` round-trips never touch the developer's real
// store. Mirrors withStateDir (workspace_cmd_test.go): both LOCALAPPDATA/XDG
// (for DefaultRegistryPath-style env resolution) and the cross-package
// SetDaemonStateRootForTest seam (which takes precedence in BOTH build variants
// for DaemonStateDir, where lsp-trusted-roots.json lands).
func withTrustStore(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("LOCALAPPDATA", dir)
	t.Setenv("XDG_STATE_HOME", dir)
	restore := api.SetDaemonStateRootForTest(dir)
	t.Cleanup(restore)
	return dir
}

// runTrustCmdGroup dispatches `mcphub trust ...` through the real cobra command
// and returns combined stdout/stderr + error.
func runTrustCmdGroup(t *testing.T, args ...string) (string, error) {
	t.Helper()
	c := newTrustCmd()
	buf := &bytes.Buffer{}
	c.SetOut(buf)
	c.SetErr(buf)
	c.SetArgs(args)
	err := c.Execute()
	return buf.String(), err
}

// runUntrustCmdGroup dispatches `mcphub untrust ...`.
func runUntrustCmdGroup(t *testing.T, args ...string) (string, error) {
	t.Helper()
	c := newUntrustCmd()
	buf := &bytes.Buffer{}
	c.SetOut(buf)
	c.SetErr(buf)
	c.SetArgs(args)
	err := c.Execute()
	return buf.String(), err
}

// absForTest returns an absolute path under tmp that exists, so canonicalization
// in BlessDefaultTrustedRoot resolves it.
func absForTest(t *testing.T) string {
	t.Helper()
	return t.TempDir()
}

// ---------------------------------------------------------------------------
// Validation parity (no store touched — the validators reject before any write).
// ---------------------------------------------------------------------------

func TestTrust_ValidationParity_RelativeRejected(t *testing.T) {
	if _, err := validateTrustArg("relative/path"); err == nil {
		t.Fatal("trust must reject a relative path")
	} else if !strings.Contains(err.Error(), "absolute") {
		t.Errorf("error should mention 'absolute'; got %q", err.Error())
	}
}

func TestTrust_ValidationParity_EmptyRejected(t *testing.T) {
	for _, in := range []string{"", "   "} {
		if _, err := validateTrustArg(in); err == nil {
			t.Fatalf("trust must reject empty/blank path %q", in)
		}
	}
}

func TestUntrust_ValidationParity_EmptyRejected(t *testing.T) {
	for _, in := range []string{"", "   "} {
		if _, err := validateUntrustArg(in); err == nil {
			t.Fatalf("untrust must reject empty/blank path %q", in)
		}
	}
}

func TestUntrust_ValidationParity_RelativeAccepted(t *testing.T) {
	// untrust does NOT require absolute (removal is by canonical equality and an
	// absent root is an idempotent no-op).
	if _, err := validateUntrustArg("relative/path"); err != nil {
		t.Fatalf("untrust must accept a non-empty relative path; got %v", err)
	}
}

// The command path itself must reject a relative trust arg (parity end-to-end).
func TestTrustCmd_RelativePath_Rejected(t *testing.T) {
	withTrustStore(t)
	out, err := runTrustCmdGroup(t, "rel/ative")
	if err == nil {
		t.Fatalf("trust rel/ative should fail; output: %s", out)
	}
	if !strings.Contains(err.Error(), "absolute") {
		t.Errorf("error should mention 'absolute'; got %q", err.Error())
	}
}

// ---------------------------------------------------------------------------
// End-to-end round-trip through the redirected store.
// ---------------------------------------------------------------------------

func TestTrustCmd_AddListUntrust_RoundTrip(t *testing.T) {
	withTrustStore(t)
	ws := absForTest(t)
	canonical, err := api.CanonicalWorkspacePath(ws)
	if err != nil {
		// Fall back to the trusted-root canonicalizer expectation: at minimum
		// the stored entry should contain the trusted root somewhere.
		canonical = ws
	}

	// trust <path>
	if out, err := runTrustCmdGroup(t, ws); err != nil {
		t.Fatalf("trust %s: %v\noutput: %s", ws, err, out)
	}

	// trust list → contains the stored root + the store path line.
	listOut, err := runTrustCmdGroup(t, "list")
	if err != nil {
		t.Fatalf("trust list: %v", err)
	}
	if !strings.Contains(listOut, "Trusted-roots store:") {
		t.Errorf("list output should name the store path; got %q", listOut)
	}
	if !trustListContainsRoot(listOut, canonical) {
		t.Errorf("list output %q should contain the trusted root %q", listOut, canonical)
	}

	// Verify on-disk via the api owner (defensive — the store round-trips).
	f, err := api.LoadDefaultLSPTrustedRoots()
	if err != nil {
		t.Fatalf("load store: %v", err)
	}
	if len(f.Roots) != 1 {
		t.Fatalf("store has %d roots, want 1: %v", len(f.Roots), f.Roots)
	}

	// untrust <path> → store empties.
	if out, err := runUntrustCmdGroup(t, ws); err != nil {
		t.Fatalf("untrust %s: %v\noutput: %s", ws, err, out)
	}
	f2, err := api.LoadDefaultLSPTrustedRoots()
	if err != nil {
		t.Fatalf("reload store: %v", err)
	}
	if len(f2.Roots) != 0 {
		t.Fatalf("store has %d roots after untrust, want 0: %v", len(f2.Roots), f2.Roots)
	}
}

func TestTrustCmd_Add_Idempotent(t *testing.T) {
	withTrustStore(t)
	ws := absForTest(t)
	if _, err := runTrustCmdGroup(t, ws); err != nil {
		t.Fatalf("first trust: %v", err)
	}
	if _, err := runTrustCmdGroup(t, ws); err != nil {
		t.Fatalf("second trust (idempotent) should succeed: %v", err)
	}
	f, err := api.LoadDefaultLSPTrustedRoots()
	if err != nil {
		t.Fatalf("load store: %v", err)
	}
	if len(f.Roots) != 1 {
		t.Fatalf("idempotent trust should keep 1 root, got %d: %v", len(f.Roots), f.Roots)
	}
}

func TestUntrustCmd_Absent_NoOpSuccess(t *testing.T) {
	withTrustStore(t)
	ws := absForTest(t)
	// Removing a never-trusted root is an idempotent no-op success.
	if out, err := runUntrustCmdGroup(t, ws); err != nil {
		t.Fatalf("untrust absent root should be a no-op success: %v\noutput: %s", err, out)
	}
}

func TestTrustCmd_ListEmpty_PrintsNone(t *testing.T) {
	withTrustStore(t)
	out, err := runTrustCmdGroup(t, "list")
	if err != nil {
		t.Fatalf("trust list on empty store: %v", err)
	}
	if !strings.Contains(out, "(none)") {
		t.Errorf("empty-store list should print (none); got %q", out)
	}
}

// trustListContainsRoot does a case-insensitive contains on Windows (the store
// lowercases the drive letter; the rest of the path is case-preserving) and an
// exact contains elsewhere — matching the storedEqualsCanonical posture.
func trustListContainsRoot(listOut, root string) bool {
	if runtime.GOOS == "windows" {
		return strings.Contains(strings.ToLower(listOut), strings.ToLower(filepath.Clean(root)))
	}
	return strings.Contains(listOut, filepath.Clean(root))
}
