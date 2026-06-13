package gui

// force_kill_sentinel_test.go — falsifying regressions for the GUI
// `mcphub gui --force --kill` already-gone-holder handling.
//
// pr301 r5 Finding 3 (owner-SID arm): the owner-SID identity gate must NOT
// convert an already-gone flock holder into VerdictKillRefused — it must let
// the recovery path run. The owner-SID arm (processOwnerSIDMatchesCurrentFn)
// can return the canonical process.ErrProcessAlreadyExited sentinel when the
// recorded PID exits between the image/start-time probe and the gate's
// OpenProcess. A gone holder's exit is exactly what releases the flock, so the
// gate must NOT refuse — refusing would strand the operator on a stuck lock
// whose holder is already dead.
//
// pr301 r6 (bot Finding, second gate): the r5 fix made CheckIdentityGate
// return refused=false on the sentinel, but that alone does NOT reach recovery.
// KillRecordedHolder runs a SECOND gate — the final pre-kill processID recheck
// (single_instance.go ~683-715). On Windows/Linux a dead PID makes processID
// return Alive=false with empty image/argv/start-time, so re-running the shared
// gate on that empty Verdict tripped the image arm → VerdictKillRefused BEFORE
// the kill+acquire-poll recovery path could handle the already-gone holder. The
// r6 fix skips the gate re-run when the recheck observes !Alive (the holder is
// gone) and falls through to recovery. A LIVE-but-mismatched holder still
// refuses (SEC-F3 preserved). The r5 sentinel test only called
// CheckIdentityGate and missed this second gate; the full-path tests below
// drive KillRecordedHolder end-to-end through the final recheck at ~683-715.

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gofrs/flock"

	"mcp-local-hub/internal/process"
)

// TestCheckIdentityGate_OwnerSIDSentinel_DoesNotRefuse is the FALSIFYING CORE of
// pr301 r5 Finding 3. With image/argv/start-time all passing, the owner-SID arm
// returns ErrProcessAlreadyExited; the gate must NOT refuse (refused=false), so
// the recovery/acquire-poll path can run. Pre-fix the sentinel was a non-nil
// error that tripped the generic unverifiable-owner branch → VerdictKillRefused.
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

// TestKillRecordedHolder_RaceDeadHolder_ReachesRecovery is the FALSIFYING CORE
// of pr301 r6 (bot Finding, second gate). It drives the FULL KillRecordedHolder
// path so the final pre-kill processID recheck (single_instance.go ~683-715) is
// exercised — NOT just CheckIdentityGate.
//
// Setup: the recorded PID is ALIVE with a valid mcphub-gui identity at probe
// time (so the verdict reaches VerdictLiveUnreachable and passes the first gate
// at ~647), but the holder EXITS before the final recheck — processID's second
// call returns Alive=false with empty identity. Pre-fix the recheck rebuilt a
// Verdict from the empty identity and re-ran the shared gate, which tripped the
// image arm (matchBasename("")) → VerdictKillRefused, stranding the operator on
// a stuck lock whose holder is already dead. Post-fix the recheck observes
// !Alive, skips the gate re-run, and falls through to the kill+acquire-poll
// recovery path → VerdictKilledRecovered.
//
// No real flock is held by the synthetic holder, so the acquire-poll succeeds
// and the recovered verdict is reached.
func TestKillRecordedHolder_RaceDeadHolder_ReachesRecovery(t *testing.T) {
	if runtime.GOOS == "darwin" {
		t.Skip("macOS short-circuits before the recheck; --force --kill is blocked on darwin")
	}

	// Do NOT set identityGateOverride: the final recheck calls
	// checkIdentityGateInternal, which honors the override first — overriding
	// would mask the very production gate logic this test must exercise. Instead
	// we drive the real gate with synthetic-but-valid identity from processID and
	// pass the owner-SID arm via its seam.
	prevOwner := processOwnerSIDMatchesCurrentFn
	t.Cleanup(func() { processOwnerSIDMatchesCurrentFn = prevOwner })
	processOwnerSIDMatchesCurrentFn = func(int) (bool, error) { return true, nil } // same owner

	// Stateful processID: call 1 (probeOnce) → alive valid mcphub-gui identity →
	// VerdictLiveUnreachable (reaches the gate). Call 2 (final recheck) → the
	// holder has exited: Alive=false with empty identity (the production
	// dead-PID shape on Windows/Linux).
	var processIDCalls atomic.Int32
	prevProcessID := ProcessIDForTest()
	t.Cleanup(func() { RestoreProcessID(prevProcessID) })
	SetProcessIDOverride(func(pid int) (ProcessIdentity, error) {
		n := processIDCalls.Add(1)
		if n == 1 {
			return ProcessIdentity{
				Alive:     true,
				ImagePath: mcphubBinaryNameForTest(),
				Cmdline:   []string{mcphubBinaryNameForTest(), "gui"},
				StartTime: time.Now().Add(-1 * time.Hour),
			}, nil
		}
		// Holder exited mid-window: dead PID → Alive=false, empty identity.
		return ProcessIdentity{Alive: false}, nil
	})

	// No-op kill so we never signal a real PID. killProcess of a gone PID
	// would error in production; the line ~735 already-gone branch (processID
	// confirms !Alive) handles that. The override returning nil is fine — the
	// fall-through is driven by the recheck's !Alive skip, then the wait loop
	// breaks immediately on Alive=false.
	prevKill := killProcessOverride
	t.Cleanup(func() { killProcessOverride = prevKill })
	killProcessOverride = func(pid int) error { return nil }

	dir := t.TempDir()
	pidport := filepath.Join(dir, "gui.pidport")
	const probablyClosedPort = 1 // ping fails → not Healthy
	if err := os.WriteFile(pidport, []byte(formatPidport(os.Getpid(), probablyClosedPort)), 0o600); err != nil {
		t.Fatal(err)
	}
	// Backdate mtime so probe does NOT retry — single LiveUnreachable
	// observation → deterministic processID call ordering.
	oldMtime := time.Now().Add(-1 * time.Hour)
	if err := os.Chtimes(pidport, oldMtime, oldMtime); err != nil {
		t.Fatalf("Chtimes: %v", err)
	}

	// Deliberately DO NOT pre-acquire the flock: the synthetic holder is
	// already gone, so the lock is free and the acquire-poll must succeed.
	opts := KillOpts{
		KillExitBackoff:  5 * time.Millisecond,
		KillExitDeadline: 1 * time.Second,
		AcquireBackoff:   5 * time.Millisecond,
		AcquireDeadline:  500 * time.Millisecond,
	}
	lock, v, err := KillRecordedHolder(context.Background(), pidport, opts)
	if lock != nil {
		defer lock.Release()
	}

	if v.Class != VerdictKilledRecovered {
		t.Fatalf("pr301 r6 regression (second gate): an already-gone holder must reach the "+
			"kill+acquire-poll recovery path, NOT be refused at the final pre-kill recheck on "+
			"empty image/argv/start-time identity. Class = %v (want VerdictKilledRecovered); "+
			"Diagnose=%q", v.Class, v.Diagnose)
	}
	if err != nil {
		t.Errorf("recovered kill returned non-nil error: %v", err)
	}
	if lock == nil {
		t.Errorf("VerdictKilledRecovered but lock is nil — caller has nothing to Release")
	}
}

// TestKillRecordedHolder_LiveMismatchedHolder_StillRefusedAtRecheck is the
// SECURITY-PRESERVATION control for pr301 r6. It proves the !Alive skip does NOT
// weaken the live-holder refusal: a holder that is ALIVE at the final recheck
// but whose identity does NOT match (image mismatch) must STILL be refused
// (VerdictKillRefused), and no kill is sent.
//
// Setup: probe sees a valid mcphub-gui identity (passes the first gate at
// ~647), but the final recheck's processID call returns a LIVE process with a
// NON-mcphub image. The recheck (~710) re-runs the shared gate on the live
// identity and must refuse on the image arm. This exercises the same
// ~683-715 recheck the falsifying test drives, on the opposite branch.
func TestKillRecordedHolder_LiveMismatchedHolder_StillRefusedAtRecheck(t *testing.T) {
	if runtime.GOOS == "darwin" {
		t.Skip("macOS short-circuits before the recheck; --force --kill is blocked on darwin")
	}

	prevOwner := processOwnerSIDMatchesCurrentFn
	t.Cleanup(func() { processOwnerSIDMatchesCurrentFn = prevOwner })
	processOwnerSIDMatchesCurrentFn = func(int) (bool, error) { return true, nil } // same owner

	var processIDCalls atomic.Int32
	prevProcessID := ProcessIDForTest()
	t.Cleanup(func() { RestoreProcessID(prevProcessID) })
	SetProcessIDOverride(func(pid int) (ProcessIdentity, error) {
		n := processIDCalls.Add(1)
		if n == 1 {
			// Probe: valid mcphub-gui identity → passes the first gate.
			return ProcessIdentity{
				Alive:     true,
				ImagePath: mcphubBinaryNameForTest(),
				Cmdline:   []string{mcphubBinaryNameForTest(), "gui"},
				StartTime: time.Now().Add(-1 * time.Hour),
			}, nil
		}
		// Final recheck: PID recycled to a LIVE, non-mcphub process.
		nonMcphubImage := "/usr/bin/evil"
		if runtime.GOOS == "windows" {
			nonMcphubImage = `C:\Windows\System32\evil.exe`
		}
		return ProcessIdentity{
			Alive:     true,
			ImagePath: nonMcphubImage,
			Cmdline:   []string{nonMcphubImage},
			StartTime: time.Now().Add(-1 * time.Hour),
		}, nil
	})

	// Spy: the kill must NOT fire on a refused recheck.
	var killCalled atomic.Bool
	prevKill := killProcessOverride
	t.Cleanup(func() { killProcessOverride = prevKill })
	killProcessOverride = func(pid int) error {
		killCalled.Store(true)
		return nil
	}

	dir := t.TempDir()
	pidport := filepath.Join(dir, "gui.pidport")
	if err := os.WriteFile(pidport, []byte(formatPidport(os.Getpid(), 1)), 0o600); err != nil {
		t.Fatal(err)
	}
	oldMtime := time.Now().Add(-1 * time.Hour)
	if err := os.Chtimes(pidport, oldMtime, oldMtime); err != nil {
		t.Fatalf("Chtimes: %v", err)
	}
	// Pre-acquire the flock so KillRecordedHolder reaches the gate path
	// (mirrors the established RefusesNonMcphubImage setup).
	fl := flock.New(pidport + ".lock")
	if ok, _ := fl.TryLock(); !ok {
		t.Fatal("could not pre-lock")
	}
	defer fl.Unlock()

	_, v, err := KillRecordedHolder(context.Background(), pidport, KillOpts{})
	if v.Class != VerdictKillRefused {
		t.Fatalf("SEC-F3 regression: a LIVE holder whose identity does NOT match must be "+
			"REFUSED at the final pre-kill recheck; Class = %v (want VerdictKillRefused); "+
			"Diagnose=%q", v.Class, v.Diagnose)
	}
	if err == nil {
		t.Errorf("expected non-nil error on kill-refused; got nil")
	}
	if killCalled.Load() {
		t.Errorf("killProcess fired despite a refused recheck — the !Alive skip must not let a " +
			"live-mismatched holder reach the kill")
	}
}
