package api

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestSecureWriteWithOperatorOpt_DefaultRelaxOnGateFailure pins the
// v0.4.0 flip: when SecureWriteClientConfig hits the parent-dir gate
// AND neither env var is set, the wrapper RUNS the hardened pipeline
// AGAIN with the parent-dir gate bypassed. Solo-developer Windows
// hosts (the common case) no longer need to opt in.
//
// Pre-v0.4.0 behavior (strict by default, opt-in to relax) is now
// reversed; the pre-v0.4.0 test that pinned the strict-by-default
// path moved to TestSecureWriteWithOperatorOpt_StrictModeRequired
// below.
//
// PR #185 r3 (codex deep-sec P1 closure): the relax lane no longer
// uses os.CreateTemp + path-based DACL — it re-runs the SAME
// handle-relative hardened pipeline with parent-dir gate disabled,
// closing the temp-create-to-DACL-apply race window.
func TestSecureWriteWithOperatorOpt_DefaultRelaxOnGateFailure(t *testing.T) {
	t.Setenv(AllowUnhardenedClientWriteEnv, "") // no legacy opt-in
	t.Setenv(RequireSingleUserHomeEnv, "")      // no strict opt-in

	dst := filepath.Join(t.TempDir(), "client.json")
	want := []byte(`{"servers":{"x":1}}`)
	err := secureWriteWithOperatorOpt(dst, want)
	if err != nil {
		// On a host where t.TempDir() happens to satisfy the strict
		// gate (clean 0700 tmpdir owned by test user), the underlying
		// SecureWriteClientConfig succeeded and there was no gate
		// rejection to relax — the wrapper still returns nil and the
		// write still landed. So failure here is real.
		t.Fatalf("default-relax should succeed (strict path or skip-gate path); got err: %v", err)
	}
	got, readErr := os.ReadFile(dst)
	if readErr != nil {
		t.Fatalf("read written file: %v", readErr)
	}
	if string(got) != string(want) {
		t.Errorf("written contents = %q, want %q", got, want)
	}
	if runtime.GOOS != "windows" {
		if info, _ := os.Stat(dst); info != nil && (info.Mode().Perm()&0o077) != 0 {
			t.Errorf("written file mode %v has group/world bits; hardened pipeline (gate-disabled lane) must still write 0600", info.Mode())
		}
	}
}

// TestSecureWriteWithOperatorOpt_StrictModeRequired pins the
// opt-IN-to-strict branch: when MCPHUB_REQUIRE_SINGLE_USER_HOME=1
// is set, a parent-dir gate failure surfaces an error and does NOT
// fall back to the unhardened write. This is the corp-managed /
// multi-tenant posture explicitly chosen by the operator.
func TestSecureWriteWithOperatorOpt_StrictModeRequired(t *testing.T) {
	t.Setenv(RequireSingleUserHomeEnv, "1")     // explicit strict
	t.Setenv(AllowUnhardenedClientWriteEnv, "") // legacy opt-in inert

	dst := filepath.Join(t.TempDir(), "client.json")
	err := secureWriteWithOperatorOpt(dst, []byte(`{"servers":{}}`))
	if err == nil {
		t.Skip("t.TempDir() unexpectedly satisfied the parent-dir gate; cannot pin strict-mode rejection on this host")
	}
	if !errors.Is(err, ErrSecureWriteParentInsecure) {
		t.Fatalf("error not wrapped with ErrSecureWriteParentInsecure: %v", err)
	}
	if !strings.Contains(err.Error(), RequireSingleUserHomeEnv) {
		t.Errorf("error must mention %q so operator knows which env var enforces this; got %v",
			RequireSingleUserHomeEnv, err)
	}
	if _, statErr := os.Stat(dst); !os.IsNotExist(statErr) {
		t.Errorf("file at %s exists despite strict-mode rejection; stat err = %v", dst, statErr)
	}
}

// TestSecureWriteWithOperatorOpt_StrictBeatsLegacyAllow pins the
// precedence: when BOTH env vars are set, strict wins (defensive —
// operators who want corp-managed posture should not be silently
// downgraded by a stale legacy opt-in in their shell profile).
//
// Codex deep-sec PR #185 r2 P2 closure: asserts the destination
// file is ABSENT after strict rejection. Earlier version only
// checked the error class, which would have passed a future
// refactor that ran the fallback write first and then returned a
// strict-looking error.
func TestSecureWriteWithOperatorOpt_StrictBeatsLegacyAllow(t *testing.T) {
	t.Setenv(RequireSingleUserHomeEnv, "1")      // strict
	t.Setenv(AllowUnhardenedClientWriteEnv, "1") // legacy relax
	dst := filepath.Join(t.TempDir(), "client.json")
	err := secureWriteWithOperatorOpt(dst, []byte(`{"servers":{}}`))
	if err == nil {
		t.Skip("t.TempDir() unexpectedly satisfied the parent-dir gate; cannot pin precedence on this host")
	}
	if !errors.Is(err, ErrSecureWriteParentInsecure) {
		t.Fatalf("strict-mode should reject even with legacy opt-in present; got %v", err)
	}
	if !strings.Contains(err.Error(), RequireSingleUserHomeEnv) {
		t.Errorf("error must mention %q (strict wins); got %v", RequireSingleUserHomeEnv, err)
	}
	// P2 closure: no fallback write must have happened.
	if _, statErr := os.Stat(dst); !os.IsNotExist(statErr) {
		t.Errorf("file at %s exists despite strict-mode rejection; strict path must NOT leak a write on its way out (stat err = %v)", dst, statErr)
	}
}

// TestSecureWriteWithOperatorOpt_GateRejectionFallsBackWhenOpted
// pins the legacy opt-in branch: same path that rejects under
// strict mode now succeeds via the gate-disabled hardened pipeline
// when MCPHUB_ALLOW_UNHARDENED_CLIENT_WRITE=1 is set. Backward
// compat for operators who already had the env var set pre-v0.4.0.
func TestSecureWriteWithOperatorOpt_GateRejectionFallsBackWhenOpted(t *testing.T) {
	t.Setenv(AllowUnhardenedClientWriteEnv, "1")
	t.Setenv(RequireSingleUserHomeEnv, "") // legacy opt-in path

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
			t.Errorf("written file mode %v has group/world bits; relax-lane (hardened pipeline, gate disabled) must still write 0600", info.Mode())
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

// TestOperatorRequiresSingleUserHome_AcceptsOneAndTrue mirrors the
// AllowUnhardenedClientWrite env-var parsing contract for the
// strict-mode opt-in introduced in v0.4.0. Anything other than
// "1"/"true" (case-insensitive, trimmed) is false.
func TestOperatorRequiresSingleUserHome_AcceptsOneAndTrue(t *testing.T) {
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
		{"yes", false},
		{"on", false},
		{"garbage", false},
	}
	for _, c := range cases {
		t.Setenv(RequireSingleUserHomeEnv, c.val)
		got := operatorRequiresSingleUserHome()
		if got != c.want {
			t.Errorf("env=%q: operatorRequiresSingleUserHome() = %v, want %v",
				c.val, got, c.want)
		}
	}
}

// TestSecureWriteWithOperatorOpt_StrictRefusesPreexistingSymlink pins
// that STRICT mode (MCPHUB_REQUIRE_SINGLE_USER_HOME=1) still refuses
// a pre-existing symlink/junction at the destination. This was the
// default in v0.4.0-v0.4.1; v0.4.2 inverts it to follow symlinks by
// default for the solo-dev dotfile pattern (manual smoke on workstation
// with ~/.codex/config.toml -> E:\env\Agents\.codex\config.toml).
// Strict mode preserves the v0.4.0-v0.4.1 refuse-by-default behavior
// for corp-managed / multi-tenant hosts.
//
// Threat the strict refusal addresses: an attacker with write access
// to the parent dir could plant a symlink at the destination to
// redirect the write. With strict mode, the parent-DACL gate ALSO
// fires (because broadened parent → write access by non-allowlisted
// SIDs), so this test is the second layer of defense.
//
// Symlink creation on Windows requires SeCreateSymbolicLinkPrivilege
// (typically only Administrators have it). Skip on Windows unless
// MkdirAll succeeds.
func TestSecureWriteWithOperatorOpt_StrictRefusesPreexistingSymlink(t *testing.T) {
	t.Setenv(AllowUnhardenedClientWriteEnv, "1") // legacy opt-in (path tested below)
	t.Setenv(RequireSingleUserHomeEnv, "1")       // STRICT mode — preserve v0.4.0-v0.4.1 refuse behavior

	root := t.TempDir()
	realTarget := filepath.Join(root, "real-target")
	if err := os.WriteFile(realTarget, []byte("attacker-controlled"), 0o600); err != nil {
		t.Fatalf("write real-target: %v", err)
	}
	link := filepath.Join(root, "client.json")
	if err := os.Symlink(realTarget, link); err != nil {
		// Windows non-admin: SeCreateSymbolicLinkPrivilege missing.
		t.Skipf("symlink unsupported (likely Windows non-admin): %v", err)
	}

	err := secureWriteWithOperatorOpt(link, []byte(`{"victim":"data"}`))
	if err == nil {
		t.Fatal("expected refusal for pre-existing symlink under strict mode; got nil")
	}
	// Error wording differs between Windows (reparse point refused)
	// and POSIX (pre-existing symlink refused), but both should
	// mention either "symlink" or "reparse".
	lowered := strings.ToLower(err.Error())
	if !strings.Contains(lowered, "symlink") && !strings.Contains(lowered, "reparse") {
		t.Errorf("error must mention symlink/reparse-point; got %v", err)
	}
	// The real-target file MUST be unmodified under strict mode.
	got, _ := os.ReadFile(realTarget)
	if string(got) != "attacker-controlled" {
		t.Errorf("symlink target was modified despite strict mode; got %q", got)
	}
}

// TestSecureWriteWithOperatorOpt_DefaultRefusesPreexistingSymlink covers
// default mode behavior: pre-existing symlinks at the destination are
// refused, preventing writes from being redirected to the symlink target.
func TestSecureWriteWithOperatorOpt_DefaultRefusesPreexistingSymlink(t *testing.T) {
	t.Setenv(AllowUnhardenedClientWriteEnv, "1")
	t.Setenv(RequireSingleUserHomeEnv, "") // default mode

	root := t.TempDir()
	realTarget := filepath.Join(root, "real-target")
	if err := os.WriteFile(realTarget, []byte("original-content"), 0o600); err != nil {
		t.Fatalf("write real-target: %v", err)
	}
	link := filepath.Join(root, "client.json")
	if err := os.Symlink(realTarget, link); err != nil {
		t.Skipf("symlink unsupported (likely Windows non-admin): %v", err)
	}

	err := secureWriteWithOperatorOpt(link, []byte(`{"hello":"target"}`))
	if err == nil {
		t.Fatal("expected refusal for pre-existing symlink under default mode; got nil")
	}
	lowered := strings.ToLower(err.Error())
	if !strings.Contains(lowered, "symlink") && !strings.Contains(lowered, "reparse") {
		t.Errorf("error must mention symlink/reparse-point; got %v", err)
	}
	got, _ := os.ReadFile(realTarget)
	if string(got) != "original-content" {
		t.Errorf("symlink target was modified in default mode; got %q", got)
	}
	li, lerr := os.Lstat(link)
	if lerr != nil {
		t.Fatalf("lstat link: %v", lerr)
	}
	if li.Mode()&os.ModeSymlink == 0 {
		t.Errorf("original symlink at %s was replaced (no longer a symlink); mode=%v", link, li.Mode())
	}
}

// TestSecureWriteWithOperatorOpt_RelaxWritesWhenAbsent pins the
// happy path of the relax lane: when the destination does not exist,
// the write succeeds and the file lands. Equivalent to the prior
// TestFallbackWriteRefusingSymlink_WritesWhenAbsent but routed
// through secureWriteWithOperatorOpt (which now uses the hardened
// pipeline with gate disabled instead of a separate fallback path).
func TestSecureWriteWithOperatorOpt_RelaxWritesWhenAbsent(t *testing.T) {
	t.Setenv(AllowUnhardenedClientWriteEnv, "1")
	t.Setenv(RequireSingleUserHomeEnv, "")
	dst := filepath.Join(t.TempDir(), "client.json")
	want := []byte(`{"hello":"world"}`)
	if err := secureWriteWithOperatorOpt(dst, want); err != nil {
		t.Fatalf("relax-lane write to absent path: %v", err)
	}
	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(got) != string(want) {
		t.Errorf("written contents = %q, want %q", got, want)
	}
}

// TestSecureWriteWithOperatorOpt_RelaxOverwritesRegularFile pins
// that pre-existing REGULAR files at the destination are still
// overwritten by the relax lane (the refusal is narrow — only
// symlinks/reparse-points).
func TestSecureWriteWithOperatorOpt_RelaxOverwritesRegularFile(t *testing.T) {
	t.Setenv(AllowUnhardenedClientWriteEnv, "1")
	t.Setenv(RequireSingleUserHomeEnv, "")
	dst := filepath.Join(t.TempDir(), "client.json")
	if err := os.WriteFile(dst, []byte("old"), 0o600); err != nil {
		t.Fatalf("seed regular file: %v", err)
	}
	want := []byte(`{"new":"value"}`)
	if err := secureWriteWithOperatorOpt(dst, want); err != nil {
		t.Fatalf("relax overwrite of regular file: %v", err)
	}
	got, _ := os.ReadFile(dst)
	if string(got) != string(want) {
		t.Errorf("written contents = %q, want %q", got, want)
	}
}

// TestSecureWriteWithOperatorOpt_RelaxTightensModeOnExistingFile
// pins that when a pre-existing file has loose permissions (e.g.
// 0644 from a prior tool), the relax lane (hardened pipeline with
// gate disabled) MUST tighten to 0600. The handle-relative
// O_CREAT|O_EXCL|0600 + Fchmod(0600) gives a fresh 0600 inode; the
// atomic renameat replaces the loose file with the tight one.
//
// POSIX-only assertion (Windows mode bits are an ACL translation
// and don't reflect ACL inheritance the operator accepted; Windows
// DACL coverage is in TestSecureWriteWithOperatorOpt_RelaxOnGateFailure_WindowsDACLHardened).
func TestSecureWriteWithOperatorOpt_RelaxTightensModeOnExistingFile(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX-only: Windows uses ACL inheritance, mode bits are translated")
	}
	t.Setenv(AllowUnhardenedClientWriteEnv, "1")
	t.Setenv(RequireSingleUserHomeEnv, "")
	dst := filepath.Join(t.TempDir(), "client.json")
	if err := os.WriteFile(dst, []byte("pre-existing"), 0o644); err != nil {
		t.Fatalf("seed loose file: %v", err)
	}
	// Force the mode in case the FS or umask flipped a bit.
	if err := os.Chmod(dst, 0o644); err != nil {
		t.Fatalf("chmod seed: %v", err)
	}
	if err := secureWriteWithOperatorOpt(dst, []byte("fresh-content")); err != nil {
		t.Fatalf("relax write: %v", err)
	}
	info, err := os.Stat(dst)
	if err != nil {
		t.Fatalf("stat after write: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("after relax write, mode = %v; want 0600 (the relax lane must tighten loose pre-existing perms via the hardened pipeline)", info.Mode().Perm())
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
