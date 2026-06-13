package gui

// force_kill_sentinel_test.go — falsifying regression for pr301 r5 Finding 3
// (the GUI `mcphub gui --force --kill` owner-SID gate must NOT convert an
// already-gone flock holder into VerdictKillRefused — it must let the recovery
// path run).
//
// The owner-SID arm (processOwnerSIDMatchesCurrentFn) can return the canonical
// process.ErrProcessAlreadyExited sentinel when the recorded PID exits between
// the image/start-time probe and the gate's OpenProcess. A gone holder's exit
// is exactly what releases the flock, so the gate must NOT refuse — refusing
// would strand the operator on a stuck lock whose holder is already dead. The
// fix returns refused=false on the sentinel so checkIdentityGateInternal's
// caller (KillRecordedHolder) reaches the kill+acquire-poll recovery path.
//
// A LIVE-unverifiable (non-sentinel) error must STILL refuse, preserving SEC-F3.

import (
	"errors"
	"runtime"
	"testing"
	"time"

	"mcp-local-hub/internal/process"
)

// TestCheckIdentityGate_OwnerSIDSentinel_DoesNotRefuse is the FALSIFYING CORE of
// Finding 3. With image/argv/start-time all passing, the owner-SID arm returns
// ErrProcessAlreadyExited; the gate must NOT refuse (refused=false), so the
// recovery/acquire-poll path can run. Pre-fix the sentinel was a non-nil error
// that tripped the generic unverifiable-owner branch → VerdictKillRefused.
func TestCheckIdentityGate_OwnerSIDSentinel_DoesNotRefuse(t *testing.T) {
	if runtime.GOOS == "darwin" {
		t.Skip("macOS short-circuits the gate before the owner-SID arm")
	}

	orig := processOwnerSIDMatchesCurrentFn
	t.Cleanup(func() { processOwnerSIDMatchesCurrentFn = orig })

	// A Verdict that passes image + argv + start-time so only the SID arm decides.
	v := Verdict{
		PID:           4242,
		PIDImage:      mcphubBinaryNameForTest(),
		pidCmdlineRaw: []string{mcphubBinaryNameForTest(), "gui"},
		PIDStart:      time.Unix(1000, 0),
		Mtime:         time.Unix(2000, 0),
	}

	// Holder vanished mid-gate → the already-exited sentinel.
	processOwnerSIDMatchesCurrentFn = func(int) (bool, error) {
		return false, process.ErrProcessAlreadyExited
	}
	if refused, reason := CheckIdentityGate(v); refused {
		t.Fatalf("pr301 r5 Finding 3 regression: an already-gone flock holder (owner-SID "+
			"sentinel) must NOT refuse the gate — its exit releases the flock and the recovery "+
			"path must run; gate refused with %q (the pre-fix VerdictKillRefused that strands "+
			"the operator on a stuck lock whose holder is already dead)", reason)
	}

	// SECURITY-PRESERVATION control: a LIVE unverifiable owner (non-sentinel
	// error) must STILL refuse (fail closed). The sentinel handling must not
	// relax a genuinely unverifiable live holder.
	processOwnerSIDMatchesCurrentFn = func(int) (bool, error) {
		return false, errors.New("OpenProcessToken: access denied")
	}
	if refused, _ := CheckIdentityGate(v); !refused {
		t.Fatal("SEC-F3 regression: a LIVE unverifiable owner SID must REFUSE the gate (fail " +
			"closed); got refused=false — the sentinel handling must not relax a live " +
			"unverifiable holder")
	}
}
