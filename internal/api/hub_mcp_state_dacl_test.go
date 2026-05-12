package api

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// TestVerifyHubMcpStateDACLAcceptsFreshlyCreatedFile is the happy-path
// gate: a file just written by SecureWriteClientConfig must pass the
// allowlist check. Build-neutral — the POSIX impl checks owner-uid +
// mode mask, the Windows impl checks the canonical DACL on both the
// file and its immediate parent dir.
//
// Uses hardenedTempDir so the parent-dir DACL gate (added in the
// parent-dir-dacl-missing fix) doesn't reject %TEMP%'s inherited
// Authenticated Users ACE on Windows. POSIX shim is pass-through.
func TestVerifyHubMcpStateDACLAcceptsFreshlyCreatedFile(t *testing.T) {
	dir := hardenedTempDir(t)
	target := filepath.Join(dir, "hub-mcp-tokens.json")
	if err := SecureWriteClientConfig(target, []byte("{}")); err != nil {
		t.Fatalf("SecureWriteClientConfig: %v", err)
	}
	if err := VerifyHubMcpStateDACL(target); err != nil {
		t.Errorf("VerifyHubMcpStateDACL = %v, want nil for own freshly created file", err)
	}
}

// TestVerifyHubMcpStateDACLRejectsBroadlyReadable: POSIX-side check
// that 0644 (group+other readable) is refused. The Windows-side broad-
// SID test lives in the Windows-build-tagged synthesis test that
// applies an Authenticated Users ALLOW ACE via SetNamedSecurityInfo.
func TestVerifyHubMcpStateDACLRejectsBroadlyReadable(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("posix-specific; windows broad-SID case covered in the synthesis test")
	}
	dir := t.TempDir()
	target := filepath.Join(dir, "loose.json")
	if err := os.WriteFile(target, []byte("{}"), 0644); err != nil {
		t.Fatal(err)
	}
	err := VerifyHubMcpStateDACL(target)
	if err == nil {
		t.Errorf("VerifyHubMcpStateDACL must reject 0644 mode (group/other readable)")
	}
}

// TestVerifyHubMcpStateDACLRejectsSymlink confirms the
// O_NOFOLLOW / FILE_FLAG_OPEN_REPARSE_POINT invariant: the verifier
// must refuse to follow a symlink to confirm the target file's
// attributes. Otherwise an attacker could swap the target between
// stat and read.
func TestVerifyHubMcpStateDACLRejectsSymlink(t *testing.T) {
	dir := t.TempDir()
	real := filepath.Join(dir, "real.json")
	if err := os.WriteFile(real, []byte("{}"), 0600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link.json")
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("symlink unsupported on this host: %v", err)
	}
	err := VerifyHubMcpStateDACL(link)
	if err == nil {
		t.Errorf("VerifyHubMcpStateDACL must refuse to follow a symlink target")
	}
}
