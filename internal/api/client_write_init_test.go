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
