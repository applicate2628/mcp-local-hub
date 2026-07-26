//go:build linux

package gui

import (
	"errors"
	"syscall"
	"testing"
)

// ---------------------------------------------------------------------------
// Residual 1(a) review fix: classifyKillError must carry ambiguous kernel
// errors as Indeterminate, never coerce them to Alive:false — only ESRCH
// (kill(2)'s documented "no such process" signal) may claim definitive
// death.
//
// UNVERIFIED ON A REAL LINUX HOST this session (the implementer environment
// for this change is Windows-only — no Linux runner was available to
// execute this file). Verification step: run this file's tests, plus a
// smoke test that Kill(exitedPID, 0) returns exactly syscall.ESRCH, on a
// real Linux CI runner or dev host before relying on the ESRCH mapping
// beyond this static/logical review. This is the same construction already
// smoke-tested for the equivalent Windows classifier
// (classifyOpenProcessError in probe_windows_test.go).
// ---------------------------------------------------------------------------

// TestClassifyKillError_ESRCHIsDefinitiveDead is the ONLY case that may
// report Alive:false.
func TestClassifyKillError_ESRCHIsDefinitiveDead(t *testing.T) {
	got, err := classifyKillError(syscall.ESRCH)
	if err != nil {
		t.Fatalf("err = %v, want nil (a definitive dead verdict reports no error)", err)
	}
	if got.Alive {
		t.Errorf("Alive = true, want false")
	}
	if got.Indeterminate {
		t.Errorf("Indeterminate = true, want false (this is the ONE definitive-dead case)")
	}
}

// TestClassifyKillError_EPERMIsAliveDenied pins the pre-existing
// EPERM-mirroring behavior: permission denied means the process EXISTS but
// we cannot signal it — never Indeterminate, never dead.
func TestClassifyKillError_EPERMIsAliveDenied(t *testing.T) {
	got, err := classifyKillError(syscall.EPERM)
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if !got.Alive || !got.Denied {
		t.Errorf("got = %+v, want Alive:true Denied:true", got)
	}
	if got.Indeterminate {
		t.Errorf("Indeterminate = true, want false")
	}
}

// TestClassifyKillError_EveryOtherErrorIsIndeterminate reproduces the
// residual 1(a) danger directly: BEFORE this fix, "ESRCH or other: not
// alive" collapsed every non-EPERM Kill(0) failure into Alive:false — which
// single_instance.go's probeOnce turns into VerdictDeadPID, the ONLY class
// that authorizes a destructive relaunch/kill. kill(2) documents only
// EINVAL/EPERM/ESRCH and EINVAL cannot occur for signal 0, but a future
// kernel/libc surprise (or any unrecognized errno) must never reach
// VerdictDeadPID.
//
// MUTATION: revert classifyKillError to return ProcessIdentity{Alive: false}
// for any error that is not EPERM — this test's "want Indeterminate:true,
// Alive:false" assertions fail for EINVAL and the synthetic error.
func TestClassifyKillError_EveryOtherErrorIsIndeterminate(t *testing.T) {
	cases := []struct {
		name string
		err  error
	}{
		{"EINVAL (documented but unreachable for signal 0)", syscall.EINVAL},
		{"unrecognized synthetic error", errors.New("injected ambiguous kernel failure")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := classifyKillError(tc.err)
			if got.Alive {
				t.Errorf("Alive = true, want false (an ambiguous error must never claim liveness either)")
			}
			if !got.Indeterminate {
				t.Errorf("Indeterminate = false, want true — this error must NOT be treated as proof of death")
			}
			if !errors.Is(err, tc.err) {
				t.Errorf("returned err = %v, want the original classified error preserved (%v)", err, tc.err)
			}
		})
	}
}
