package api

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestSecureWriteWithOperatorOpt_GateRejectionSurfacesEnvHint pins
// issue #161 P1 closure: when SecureWriteClientConfig hits the
// parent-dir gate AND the operator has NOT opted in, the wrapper
// must surface a clear error explicitly naming the env var. No
// silent fallback.
func TestSecureWriteWithOperatorOpt_GateRejectionSurfacesEnvHint(t *testing.T) {
	// Use t.TempDir() — on Windows it inherits %TEMP%'s
	// Authenticated Users DACL; on POSIX it's 0755 with $TMPDIR
	// inheritance. Either way the parent-dir gate rejects.
	t.Setenv(AllowUnhardenedClientWriteEnv, "") // explicit opt-out

	dst := filepath.Join(t.TempDir(), "client.json")
	err := secureWriteWithOperatorOpt(dst, []byte(`{"servers":{}}`))
	if err == nil {
		// On a host where t.TempDir() happens to satisfy the gate
		// (a clean 0700 tmpdir owned by the test user), the test
		// is not meaningful — skip it instead of false-passing.
		t.Skip("t.TempDir() unexpectedly satisfied the parent-dir gate; cannot pin opt-out behavior on this host")
	}
	if !errors.Is(err, ErrSecureWriteParentInsecure) {
		t.Fatalf("error not wrapped with ErrSecureWriteParentInsecure: %v", err)
	}
	if !strings.Contains(err.Error(), AllowUnhardenedClientWriteEnv) {
		t.Errorf("error must mention %q so operator sees the opt-in escape hatch; got %v",
			AllowUnhardenedClientWriteEnv, err)
	}
	// File MUST NOT exist — the wrapper refused to write.
	if _, statErr := os.Stat(dst); !os.IsNotExist(statErr) {
		t.Errorf("file at %s exists despite opt-out; stat err = %v", dst, statErr)
	}
}

// TestSecureWriteWithOperatorOpt_GateRejectionFallsBackWhenOpted
// pins the opt-in branch: same path that rejected above now
// succeeds via os.WriteFile when the env var is set.
func TestSecureWriteWithOperatorOpt_GateRejectionFallsBackWhenOpted(t *testing.T) {
	t.Setenv(AllowUnhardenedClientWriteEnv, "1")

	dst := filepath.Join(t.TempDir(), "client.json")
	want := []byte(`{"servers":{"x":1}}`)
	err := secureWriteWithOperatorOpt(dst, want)
	if err != nil {
		// If the underlying SecureWriteClientConfig SUCCEEDED on
		// this host (very rare: t.TempDir() satisfied the gate by
		// coincidence), the wrapper returns nil too — the test
		// still proves the wrapper doesn't reject a write that
		// would otherwise succeed.
		t.Fatalf("secureWriteWithOperatorOpt with opt-in returned err: %v", err)
	}
	got, readErr := os.ReadFile(dst)
	if readErr != nil {
		t.Fatalf("read written file: %v", readErr)
	}
	if string(got) != string(want) {
		t.Errorf("written contents = %q, want %q", got, want)
	}
	// Mode should be 0600 on POSIX. Windows reports ACL inheritance
	// as POSIX-equivalent rwx bits (the API translation is lossy),
	// so we only pin the mode bits on POSIX.
	if runtime.GOOS != "windows" {
		if info, _ := os.Stat(dst); info != nil && (info.Mode().Perm()&0o077) != 0 {
			t.Errorf("written file mode %v has group/world bits; fallback should write 0600", info.Mode())
		}
	}
}

// TestOperatorAllowedUnhardenedClientWrite_AcceptsOneAndTrue pins
// the env-var parsing contract. Anything other than "1"/"true"
// (case-insensitive, trimmed) is false.
func TestOperatorAllowedUnhardenedClientWrite_AcceptsOneAndTrue(t *testing.T) {
	cases := []struct {
		val  string
		want bool
	}{
		{"1", true},
		{"true", true},
		{"True", true},
		{"TRUE", true},
		{"  1  ", true},
		{"  true ", true},
		{"", false},
		{"0", false},
		{"false", false},
		{"no", false},
		{"yes", false}, // not in the allowlist intentionally
		{"on", false},
		{"garbage", false},
	}
	for _, c := range cases {
		t.Setenv(AllowUnhardenedClientWriteEnv, c.val)
		got := operatorAllowedUnhardenedClientWrite()
		if got != c.want {
			t.Errorf("env=%q: operatorAllowedUnhardenedClientWrite() = %v, want %v",
				c.val, got, c.want)
		}
	}
}

// TestFallbackWriteRefusingSymlink_RefusesPreexistingSymlink pins
// codex bot r1 P1 closure (PR #165): the opt-in lane MUST refuse
// to write through a pre-existing symlink/junction. Otherwise an
// attacker on a shared host could pre-create a symlink at the
// destination and harvest the token-bearing content into a target
// of their choosing.
//
// Symlink creation on Windows requires SeCreateSymbolicLinkPrivilege
// (typically only Administrators have it). Skip on Windows unless
// MkdirAll succeeds — but the broader contract still holds: every
// fallback write Lstat's first.
func TestFallbackWriteRefusingSymlink_RefusesPreexistingSymlink(t *testing.T) {
	root := t.TempDir()
	realTarget := filepath.Join(root, "real-target")
	if err := os.WriteFile(realTarget, []byte("attacker-controlled"), 0o600); err != nil {
		t.Fatalf("write real-target: %v", err)
	}
	link := filepath.Join(root, "client.json")
	if err := os.Symlink(realTarget, link); err != nil {
		// Windows non-admin: SeCreateSymbolicLinkPrivilege missing.
		// Symlink creation fails outright; skip the test on this host.
		t.Skipf("symlink unsupported (likely Windows non-admin): %v", err)
	}

	err := fallbackWriteRefusingSymlink(link, []byte(`{"victim":"data"}`))
	if err == nil {
		t.Fatal("expected refusal for pre-existing symlink; got nil")
	}
	if !strings.Contains(err.Error(), "symlink") {
		t.Errorf("error must mention symlink; got %v", err)
	}
	// The real-target file MUST be unmodified — the symlink was not
	// followed.
	got, _ := os.ReadFile(realTarget)
	if string(got) != "attacker-controlled" {
		t.Errorf("symlink target was modified (write followed the link); got %q", got)
	}
}

// TestFallbackWriteRefusingSymlink_WritesWhenAbsent pins the happy
// path: when the destination does not exist, the fallback writes
// and the file lands at the destination.
func TestFallbackWriteRefusingSymlink_WritesWhenAbsent(t *testing.T) {
	dst := filepath.Join(t.TempDir(), "client.json")
	want := []byte(`{"hello":"world"}`)
	if err := fallbackWriteRefusingSymlink(dst, want); err != nil {
		t.Fatalf("fallback write to absent path: %v", err)
	}
	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(got) != string(want) {
		t.Errorf("written contents = %q, want %q", got, want)
	}
}

// TestFallbackWriteRefusingSymlink_OverwritesRegularFile pins that
// pre-existing REGULAR files at the destination are still
// overwritten (the refusal is narrow — only symlinks).
func TestFallbackWriteRefusingSymlink_OverwritesRegularFile(t *testing.T) {
	dst := filepath.Join(t.TempDir(), "client.json")
	if err := os.WriteFile(dst, []byte("old"), 0o600); err != nil {
		t.Fatalf("seed regular file: %v", err)
	}
	want := []byte(`{"new":"value"}`)
	if err := fallbackWriteRefusingSymlink(dst, want); err != nil {
		t.Fatalf("fallback overwrite of regular file: %v", err)
	}
	got, _ := os.ReadFile(dst)
	if string(got) != string(want) {
		t.Errorf("written contents = %q, want %q", got, want)
	}
}

// TestFallbackWriteRefusingSymlink_TightensModeOnExistingFile pins
// codex bot r2 P1 closure (PR #165): when the destination already
// exists with loose permissions (e.g. 0644 from a prior tool), the
// fallback MUST tighten to 0600. Raw os.WriteFile preserves the
// existing mode on POSIX (open() returns the existing file when
// O_CREAT|O_TRUNC is set without O_EXCL, and the mode arg is
// ignored). The temp+rename path lands a fresh file with 0600.
//
// POSIX-only assertion (Windows mode bits are an ACL translation
// and don't reflect ACL inheritance the operator accepted).
func TestFallbackWriteRefusingSymlink_TightensModeOnExistingFile(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX-only: Windows uses ACL inheritance, mode bits are translated")
	}
	dst := filepath.Join(t.TempDir(), "client.json")
	if err := os.WriteFile(dst, []byte("pre-existing"), 0o644); err != nil {
		t.Fatalf("seed loose file: %v", err)
	}
	// Force the mode in case the FS or umask flipped a bit.
	if err := os.Chmod(dst, 0o644); err != nil {
		t.Fatalf("chmod seed: %v", err)
	}
	if err := fallbackWriteRefusingSymlink(dst, []byte("fresh-content")); err != nil {
		t.Fatalf("fallback write: %v", err)
	}
	info, err := os.Stat(dst)
	if err != nil {
		t.Fatalf("stat after write: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("after fallback write, mode = %v; want 0600 (the fallback must tighten loose pre-existing perms via temp+rename)", info.Mode().Perm())
	}
}

// TestSecureWriteWithOperatorOpt_NonGateErrorPropagatesUnchanged
// pins that the opt-in only narrows the parent-dir gate failure
// class. Other secure-write errors (e.g. empty base name) propagate
// regardless of the env var, so TOCTOU/symlink protections stay
// intact under opt-in.
func TestSecureWriteWithOperatorOpt_NonGateErrorPropagatesUnchanged(t *testing.T) {
	t.Setenv(AllowUnhardenedClientWriteEnv, "1") // even with opt-in
	err := secureWriteWithOperatorOpt(string(os.PathSeparator), []byte("x"))
	if err == nil {
		t.Fatal("expected error for empty-base path; got nil")
	}
	if errors.Is(err, ErrSecureWriteParentInsecure) {
		t.Errorf("non-gate error matched ErrSecureWriteParentInsecure; opt-in should not mask other failure classes: %v", err)
	}
}
