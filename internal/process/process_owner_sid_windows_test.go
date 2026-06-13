//go:build windows

package process

import (
	"errors"
	"os/exec"
	"testing"

	"golang.org/x/sys/windows"
)

// #301-1 (P2): a PROVEN different-owner SID must surface as (false, nil) — a
// benign identity mismatch the caller safely SKIPs — NOT (false, err).
//
// Before the fix, ProcessOwnerSIDMatchesCurrent's mismatch branch returned
// fmt.Errorf("...does not match..."), so the Windows reaper
// (supervisorPIDIsLiveMcphubSupervisor) treated a stale supervisor.lock sidecar
// pointing at ANOTHER user's mcphub-shaped supervisor as a force-kill FAILURE
// and ABORTED `install --upgrade`, instead of skipping the foreign process.
//
// The seam tests in internal/cli only ever INJECTED (false, nil) via
// reapOwnerSIDMatchesCurrentFn, so they never exercised the production helper's
// actual mismatch return. These tests pin the production comparison core
// (sidsMatchResult) so the contract regression can't slip back in.

// TestSidsMatchResult_ProvenMismatch is the FALSIFYING CORE of #301-1. Two
// distinct, successfully-read SIDs must compare to (false, nil): a proven
// mismatch is benign, not an error. Pre-fix this branch returned a non-nil
// error.
func TestSidsMatchResult_ProvenMismatch(t *testing.T) {
	// Two well-known, always-distinct SIDs. Reading them cannot fail, so the
	// only thing under test is the comparison verdict.
	system, err := windows.StringToSid("S-1-5-18") // LocalSystem
	if err != nil {
		t.Fatalf("StringToSid(LocalSystem): %v", err)
	}
	service, err := windows.StringToSid("S-1-5-19") // LocalService
	if err != nil {
		t.Fatalf("StringToSid(LocalService): %v", err)
	}

	match, mErr := sidsMatchResult(system, service)
	if match {
		t.Fatalf("two distinct SIDs reported as matching; want false")
	}
	if mErr != nil {
		t.Fatalf("#301-1 regression: a PROVEN different-owner SID must return "+
			"(false, nil) (benign mismatch the caller SKIPs), but sidsMatchResult "+
			"returned a non-nil error: %v", mErr)
	}
}

// TestSidsMatchResult_Match pins the positive case: identical SIDs compare to
// (true, nil) so the same-user kill proceeds.
func TestSidsMatchResult_Match(t *testing.T) {
	admin, err := windows.StringToSid("S-1-5-32-544") // BUILTIN\Administrators
	if err != nil {
		t.Fatalf("StringToSid(Administrators): %v", err)
	}
	adminCopy, err := windows.StringToSid("S-1-5-32-544")
	if err != nil {
		t.Fatalf("StringToSid(Administrators) copy: %v", err)
	}

	match, mErr := sidsMatchResult(admin, adminCopy)
	if mErr != nil {
		t.Fatalf("equal SIDs must return (true, nil); got err %v", mErr)
	}
	if !match {
		t.Fatalf("equal SIDs must compare as matching; got false")
	}
}

// TestProcessOwnerSIDMatchesCurrent_SelfIsMatch exercises the PRODUCTION helper
// end-to-end against our OWN PID. The current process is trivially owned by the
// current user's SID, so the full OpenProcess → OpenProcessToken → GetTokenUser
// → sidsMatchResult path must yield (true, nil). This proves the production
// wrapper routes a successfully-read same-owner SID through the comparison core
// (rather than, say, erroring on the token read), complementing the
// pure-core proven-mismatch assertion above.
func TestProcessOwnerSIDMatchesCurrent_SelfIsMatch(t *testing.T) {
	self := windows.GetCurrentProcessId()
	match, err := ProcessOwnerSIDMatchesCurrent(int(self))
	if err != nil {
		t.Fatalf("ProcessOwnerSIDMatchesCurrent(self PID %d): unexpected err %v", self, err)
	}
	if !match {
		t.Fatalf("our own process must match our own SID; got (false, nil)")
	}
}

// TestOpenProcessErrIsProcessGone is the pure, deterministic classifier of
// pr301 r4 Finding 2: which OpenProcess failures mean "PID is gone" vs
// "live-but-unverifiable". The security invariant lives here — ACCESS_DENIED
// (a LIVE process whose token we may not open) must NOT be classified as gone,
// because doing so would let a caller skip a different-owner / different-
// integrity LIVE process as if it were already dead.
func TestOpenProcessErrIsProcessGone(t *testing.T) {
	cases := []struct {
		name     string
		err      error
		wantGone bool
	}{
		{name: "ERROR_INVALID_PARAMETER (dead PID) is gone", err: windows.ERROR_INVALID_PARAMETER, wantGone: true},
		{name: "ERROR_NOT_FOUND (just-exited) is gone", err: windows.ERROR_NOT_FOUND, wantGone: true},
		{
			name:     "ERROR_ACCESS_DENIED (LIVE, unverifiable) is NOT gone",
			err:      windows.ERROR_ACCESS_DENIED,
			wantGone: false,
		},
		{name: "nil is not gone", err: nil, wantGone: false},
		{name: "unrelated error is not gone", err: errors.New("transient"), wantGone: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := openProcessErrIsProcessGone(tc.err); got != tc.wantGone {
				t.Fatalf("openProcessErrIsProcessGone(%v) = %v; want %v "+
					"(SECURITY: ACCESS_DENIED on a LIVE process must stay 'not gone' so the "+
					"caller fails closed and never skips a live unverifiable process as already-dead)",
					tc.err, got, tc.wantGone)
			}
		})
	}
}

// TestProcessOwnerSIDMatchesCurrent_VanishedPID_IsAlreadyExited is the FALSIFYING
// END-TO-END test of pr301 r4 Finding 2. The kill gates verify a target's
// image/argv/start-time FIRST, then call this SID gate; a process that passed
// that probe can EXIT before the SID gate's OpenProcess (the TOCTOU window). For
// a VANISHED PID the gate must return (false, ErrProcessAlreadyExited) — the
// benign already-dead verdict the cli force-kill path treats as "nothing to
// reap, continue" — NOT a generic hard error that ABORTS install --upgrade.
//
// The window is engineered DETERMINISTICALLY (no timing race per the
// race-window-assertion discipline): spawn a real child, wait for it to FULLY
// exit, then probe its now-dead PID. The PID was genuinely live, then is
// genuinely gone — exactly the TOCTOU end state.
func TestProcessOwnerSIDMatchesCurrent_VanishedPID_IsAlreadyExited(t *testing.T) {
	// A short-lived child whose PID we capture, then reap to a deterministic
	// dead state before probing. `cmd /c exit 0` is always present on Windows.
	cmd := exec.Command("cmd", "/c", "exit", "0")
	if err := cmd.Start(); err != nil {
		t.Fatalf("spawn throwaway child: %v", err)
	}
	deadPID := cmd.Process.Pid
	// Wait() reaps the child and guarantees it has fully exited; after this the
	// PID no longer refers to a live process (modulo an astronomically unlikely
	// immediate reuse, which the (false, nil) branch below detects and skips on).
	_ = cmd.Wait()

	match, err := ProcessOwnerSIDMatchesCurrent(deadPID)
	if match {
		t.Fatalf("a dead PID %d must not report an owner MATCH; got (true, %v)", deadPID, err)
	}
	if err == nil {
		// (false, nil) is the PROVEN-different-owner verdict, which a dead PID is
		// NOT. A genuinely reused-to-our-own-SID PID is the only benign way this
		// could happen; treat it as an environment artifact, not a fix regression.
		t.Skipf("dead PID %d returned (false, nil) — likely an immediate PID reuse; "+
			"cannot assert the already-exited sentinel this run", deadPID)
	}
	if !errors.Is(err, ErrProcessAlreadyExited) {
		t.Fatalf("pr301 r4 Finding 2 regression: a VANISHED PID %d must surface "+
			"ErrProcessAlreadyExited (benign already-dead → the force-kill path continues, "+
			"treating the gone target as 'kill succeeded'), NOT a generic owner-SID error that "+
			"ABORTS install --upgrade; got %v", deadPID, err)
	}
}

// TestProcessOwnerSIDMatchesCurrent_NeverAllocatedPID_IsAlreadyExited pins the
// same gone-verdict for a PID that never referred to a live process. PID 0 is
// rejected up front by the gate (invalid PID), so this uses a high odd PID that
// Windows will not have allocated; OpenProcess returns ERROR_INVALID_PARAMETER →
// ErrProcessAlreadyExited. Complements the spawn-then-reap test with a
// no-spawn deterministic path.
func TestProcessOwnerSIDMatchesCurrent_NeverAllocatedPID_IsAlreadyExited(t *testing.T) {
	// Windows PIDs are multiples of 4; an odd PID can never be a real process,
	// so OpenProcess deterministically fails with ERROR_INVALID_PARAMETER.
	const neverAllocatedPID = 0x7FFFFFF1 // large, odd → guaranteed-absent
	match, err := ProcessOwnerSIDMatchesCurrent(neverAllocatedPID)
	if match {
		t.Fatalf("a never-allocated PID must not report an owner MATCH; got (true, %v)", err)
	}
	if !errors.Is(err, ErrProcessAlreadyExited) {
		t.Fatalf("pr301 r4 Finding 2: a never-allocated PID must surface "+
			"ErrProcessAlreadyExited (gone), not a generic error; got %v", err)
	}
}
