package api

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// TestSecureWriteClientConfigBasicRoundTrip verifies the writer
// produces the exact bytes at the requested path. Build-neutral —
// runs on every platform.
//
// Uses hardenedTempDir so the parent-dir DACL gate (added in the
// parent-dir-dacl-missing fix) doesn't reject %TEMP%'s inherited
// Authenticated Users ACE. The POSIX shim returns t.TempDir() as-is.
func TestSecureWriteClientConfigBasicRoundTrip(t *testing.T) {
	dir := hardenedTempDir(t)
	target := filepath.Join(dir, "client-config.json")
	payload := []byte(`{"mcpServers":{"foo":{"url":"http://127.0.0.1:9200/mcp"}}}`)
	if err := SecureWriteClientConfig(target, payload); err != nil {
		t.Fatalf("SecureWriteClientConfig: %v", err)
	}
	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if string(got) != string(payload) {
		t.Errorf("content roundtrip = %q, want %q", got, payload)
	}
	info, err := os.Stat(target)
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() != int64(len(payload)) {
		t.Errorf("size = %d, want %d", info.Size(), len(payload))
	}
}

// TestSecureWriteClientConfigOverwritesExisting ensures the second
// write replaces the first content. Atomic rename + REPLACE_IF_EXISTS
// (Windows) / renameat-over-existing (POSIX) are the relevant
// guarantees from the spec sequence.
func TestSecureWriteClientConfigOverwritesExisting(t *testing.T) {
	dir := hardenedTempDir(t)
	target := filepath.Join(dir, "client-config.json")
	if err := SecureWriteClientConfig(target, []byte(`{"v":1}`)); err != nil {
		t.Fatalf("first write: %v", err)
	}
	if err := SecureWriteClientConfig(target, []byte(`{"v":2}`)); err != nil {
		t.Fatalf("second write: %v", err)
	}
	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != `{"v":2}` {
		t.Errorf("overwrite = %q, want %q", got, `{"v":2}`)
	}
}

// TestSecureWriteClientConfigRefusesSymlinkTarget pins the
// O_NOFOLLOW (POSIX) / refusePreexistingReparsePoint (Windows)
// invariant: the writer must REFUSE to overwrite a pre-existing
// symlink. On platforms where symlinks need elevated permissions
// (Windows), the test skips when symlink creation fails.
//
// Uses hardenedTempDir so the parent-dir DACL gate doesn't pre-empt
// the symlink check — we want to assert that the symlink-specific
// refusal fires, not the parent-dir gate.
func TestSecureWriteClientConfigRefusesSymlinkTarget(t *testing.T) {
	dir := hardenedTempDir(t)
	real := filepath.Join(dir, "real.json")
	if err := os.WriteFile(real, []byte("{}"), 0600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link.json")
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("symlink unsupported on this host: %v", err)
	}
	err := SecureWriteClientConfig(link, []byte(`{"v":1}`))
	if err == nil {
		t.Fatalf("SecureWriteClientConfig must refuse to overwrite a symlink target")
	}
	// The real file should be untouched.
	got, err := os.ReadFile(real)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "{}" {
		t.Errorf("real file mutated through symlink: %q", got)
	}
}

// TestSecureWriteWithOperatorOpt_SymlinkOptInResolvesAndWrites pins
// the MCPHUB_ALLOW_CLIENT_CONFIG_SYMLINK opt-in: when set,
// secureWriteWithOperatorOpt resolves the symlink to its target
// and writes through, leaving the symlink intact. The target file
// receives the new bytes; the symlink at the original path still
// resolves to the target (lstat shows symlink mode).
func TestSecureWriteWithOperatorOpt_SymlinkOptInResolvesAndWrites(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation requires elevation on Windows; the cross-platform path is exercised on POSIX")
	}
	t.Setenv(RequireSingleUserHomeEnv, "")
	t.Setenv(AllowClientConfigSymlinkEnv, "1")
	dir := hardenedTempDir(t)
	real := filepath.Join(dir, "real.json")
	if err := os.WriteFile(real, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link.json")
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("symlink unsupported on this host: %v", err)
	}

	if err := secureWriteWithOperatorOpt(link, []byte(`{"v":1}`)); err != nil {
		t.Fatalf("secureWriteWithOperatorOpt with opt-in: %v", err)
	}

	got, err := os.ReadFile(real)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != `{"v":1}` {
		t.Errorf("target content via opt-in resolve: got %q, want %q", got, `{"v":1}`)
	}
	if lst, err := os.Lstat(link); err != nil {
		t.Fatalf("lstat link after write: %v", err)
	} else if lst.Mode()&os.ModeSymlink == 0 {
		t.Errorf("opt-in resolve rewrote the symlink to a regular file; want symlink preserved")
	}
}

// TestSecureWriteWithOperatorOpt_SymlinkNoOptInRefuses pins the
// regression-guard companion: WITHOUT the env var,
// secureWriteWithOperatorOpt does NOT resolve symlinks and the
// underlying SecureWriteClientConfig refuses with the O_NOFOLLOW
// / refusePreexistingReparsePoint guard. Default posture stays as
// PR #209 left it.
func TestSecureWriteWithOperatorOpt_SymlinkNoOptInRefuses(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation requires elevation on Windows; the cross-platform path is exercised on POSIX")
	}
	t.Setenv(RequireSingleUserHomeEnv, "")
	t.Setenv(AllowClientConfigSymlinkEnv, "")
	dir := hardenedTempDir(t)
	real := filepath.Join(dir, "real.json")
	if err := os.WriteFile(real, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link.json")
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("symlink unsupported on this host: %v", err)
	}

	err := secureWriteWithOperatorOpt(link, []byte(`{"v":1}`))
	if err == nil {
		t.Fatalf("secureWriteWithOperatorOpt without opt-in should refuse symlink")
	}
	got, _ := os.ReadFile(real)
	if string(got) != "{}" {
		t.Errorf("real file mutated despite refusal: %q", got)
	}
}

// TestSecureWriteWithOperatorOpt_SymlinkOptInStrictOverrideRefuses
// pins strict-mode precedence: with BOTH
// MCPHUB_REQUIRE_SINGLE_USER_HOME=1 and
// MCPHUB_ALLOW_CLIENT_CONFIG_SYMLINK=1 set, the strict gate wins
// and the symlink is refused. Multi-tenant / corp-managed hosts
// get the hardening regardless of per-operator env vars.
func TestSecureWriteWithOperatorOpt_SymlinkOptInStrictOverrideRefuses(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation requires elevation on Windows; the cross-platform path is exercised on POSIX")
	}
	t.Setenv(RequireSingleUserHomeEnv, "1")
	t.Setenv(AllowClientConfigSymlinkEnv, "1")
	dir := hardenedTempDir(t)
	real := filepath.Join(dir, "real.json")
	if err := os.WriteFile(real, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link.json")
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("symlink unsupported on this host: %v", err)
	}

	err := secureWriteWithOperatorOpt(link, []byte(`{"v":1}`))
	if err == nil {
		t.Fatalf("strict mode must refuse symlink even with client-config-symlink opt-in")
	}
	got, _ := os.ReadFile(real)
	if string(got) != "{}" {
		t.Errorf("real file mutated despite strict refusal: %q", got)
	}
}

// TestSecureWriteClientConfigPosixMode0600 asserts that on POSIX the
// final file mode bits are 0600 — defense vs umask drift. The handle
// fchmod in the writer guarantees this regardless of process umask.
func TestSecureWriteClientConfigPosixMode0600(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("posix-specific")
	}
	dir := hardenedTempDir(t)
	target := filepath.Join(dir, "client-config.json")
	if err := SecureWriteClientConfig(target, []byte("{}")); err != nil {
		t.Fatalf("SecureWriteClientConfig: %v", err)
	}
	info, err := os.Stat(target)
	if err != nil {
		t.Fatal(err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Errorf("mode = %o, want 0600", mode)
	}
}
