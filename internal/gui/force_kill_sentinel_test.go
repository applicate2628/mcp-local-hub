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

	"mcp-local-hub/internal/api/apitest"
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

	dir := apitest.HardenedTempDir(t)
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

// TestKillRecordedHolder_RaceDeadHolder_DoesNotKill is the FALSIFYING CORE of
// pr301 r7 Finding 2 (P2 #717). It drives the FULL KillRecordedHolder path with
// an already-gone holder (same setup as the r6 RaceDeadHolder test) but adds the
// assertion the r6 test omitted: killProcess must NOT be called in the gone case.
//
// The r6 fix skipped only the gate re-run when the final recheck saw !Alive, then
// FELL THROUGH to kill(v.PID). With the holder already gone there is no validated
// target — and a PID REUSED between the recheck and the kill (or a transient
// not-Alive probe) could be terminated WITHOUT the image/argv/start-time/owner
// gate that was just skipped. The r7 fix makes the gone case go STRAIGHT to the
// acquire-poll recovery path: no gate re-run AND no kill.
//
// Assertions: (1) killProcess is NEVER invoked; (2) the verdict still reaches
// VerdictKilledRecovered via acquire-poll (the holder's exit released the flock).
func TestKillRecordedHolder_RaceDeadHolder_DoesNotKill(t *testing.T) {
	if runtime.GOOS == "darwin" {
		t.Skip("macOS short-circuits before the recheck; --force --kill is blocked on darwin")
	}

	prevOwner := processOwnerSIDMatchesCurrentFn
	t.Cleanup(func() { processOwnerSIDMatchesCurrentFn = prevOwner })
	processOwnerSIDMatchesCurrentFn = func(int) (bool, error) { return true, nil } // same owner

	// Stateful processID: call 1 (probeOnce) → alive valid mcphub-gui identity →
	// VerdictLiveUnreachable (reaches the gate). Call 2+ (final recheck and the
	// wait-for-exit loop) → the holder has exited: Alive=false (the production
	// dead-PID shape on Windows/Linux). Returning !Alive on every subsequent
	// call keeps the holder gone for the whole recovery window so the test is
	// not sensitive to how many times processID is consulted downstream.
	var processIDCalls atomic.Int32
	prevProcessID := ProcessIDForTest()
	t.Cleanup(func() { RestoreProcessID(prevProcessID) })
	SetProcessIDOverride(func(pid int) (ProcessIdentity, error) {
		if processIDCalls.Add(1) == 1 {
			return ProcessIdentity{
				Alive:     true,
				ImagePath: mcphubBinaryNameForTest(),
				Cmdline:   []string{mcphubBinaryNameForTest(), "gui"},
				StartTime: time.Now().Add(-1 * time.Hour),
			}, nil
		}
		// Holder exited mid-window and stays gone: dead PID → Alive=false.
		return ProcessIdentity{Alive: false}, nil
	})

	// Spy: the kill must NEVER fire when the recheck observed the holder gone.
	// A reused PID terminated here would be killed without the identity gate —
	// exactly the bug P2 #717 flags.
	var killCalled atomic.Bool
	prevKill := killProcessOverride
	t.Cleanup(func() { killProcessOverride = prevKill })
	killProcessOverride = func(pid int) error {
		killCalled.Store(true)
		return nil
	}

	dir := apitest.HardenedTempDir(t)
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

	if killCalled.Load() {
		t.Fatal("pr301 r7 Finding 2 regression: killProcess fired on an already-gone holder. " +
			"The final recheck observed !Alive, so the kill must be SKIPPED entirely — falling " +
			"through to kill(v.PID) can terminate a PID reused between the recheck and the kill " +
			"WITHOUT the identity gate that was just skipped. The gone case must go straight to " +
			"acquire-poll recovery.")
	}
	if v.Class != VerdictKilledRecovered {
		t.Fatalf("an already-gone holder must still reach the acquire-poll recovery path "+
			"(VerdictKilledRecovered) without killing anything; Class = %v, Diagnose=%q",
			v.Class, v.Diagnose)
	}
	if err != nil {
		t.Errorf("recovered (no-kill) path returned non-nil error: %v", err)
	}
	if lock == nil {
		t.Errorf("VerdictKilledRecovered but lock is nil — caller has nothing to Release")
	}
}

// TestKillRecordedHolder_LiveMatchedHolder_StillKills is the
// SECURITY/BEHAVIOR-PRESERVATION control for pr301 r7 Finding 2. It proves the
// gone-case kill-skip does NOT suppress the kill for a LIVE holder whose
// identity MATCHES: such a holder must STILL be killed normally (killProcess
// invoked) and the verdict reaches VerdictKilledRecovered after acquire-poll.
//
// Setup: probe sees a valid mcphub-gui identity (passes the first gate), and the
// final recheck ALSO sees the same live, matching identity (Alive=true, gate
// passes). The kill must fire on this live, gate-passed holder — the holderGone
// skip must be keyed strictly on !Alive, not applied to a live match.
func TestKillRecordedHolder_LiveMatchedHolder_StillKills(t *testing.T) {
	if runtime.GOOS == "darwin" {
		t.Skip("macOS short-circuits before the recheck; --force --kill is blocked on darwin")
	}

	prevOwner := processOwnerSIDMatchesCurrentFn
	t.Cleanup(func() { processOwnerSIDMatchesCurrentFn = prevOwner })
	processOwnerSIDMatchesCurrentFn = func(int) (bool, error) { return true, nil } // same owner

	// processID always reports a live, matching mcphub-gui identity at probe AND
	// recheck. After the kill the wait-loop's processID must report !Alive so the
	// recovery completes; flip to dead once the kill has been observed.
	var killCalled atomic.Bool
	prevProcessID := ProcessIDForTest()
	t.Cleanup(func() { RestoreProcessID(prevProcessID) })
	SetProcessIDOverride(func(pid int) (ProcessIdentity, error) {
		if killCalled.Load() {
			// Post-kill: holder is gone so the wait-for-exit loop breaks and
			// acquire-poll can succeed.
			return ProcessIdentity{Alive: false}, nil
		}
		return ProcessIdentity{
			Alive:     true,
			ImagePath: mcphubBinaryNameForTest(),
			Cmdline:   []string{mcphubBinaryNameForTest(), "gui"},
			StartTime: time.Now().Add(-1 * time.Hour),
		}, nil
	})

	prevKill := killProcessOverride
	t.Cleanup(func() { killProcessOverride = prevKill })
	killProcessOverride = func(pid int) error {
		killCalled.Store(true)
		return nil
	}

	dir := apitest.HardenedTempDir(t)
	pidport := filepath.Join(dir, "gui.pidport")
	if err := os.WriteFile(pidport, []byte(formatPidport(os.Getpid(), 1)), 0o600); err != nil {
		t.Fatal(err)
	}
	oldMtime := time.Now().Add(-1 * time.Hour)
	if err := os.Chtimes(pidport, oldMtime, oldMtime); err != nil {
		t.Fatalf("Chtimes: %v", err)
	}

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

	if !killCalled.Load() {
		t.Fatal("pr301 r7 Finding 2 over-reach guard: a LIVE holder whose identity MATCHES the " +
			"gate must STILL be killed (killProcess invoked). The gone-case kill-skip must be " +
			"keyed strictly on !Alive and must not suppress the kill for a live, gate-passed holder.")
	}
	if v.Class != VerdictKilledRecovered {
		t.Fatalf("a live matched holder must be killed and recovered (VerdictKilledRecovered); "+
			"Class = %v, Diagnose=%q", v.Class, v.Diagnose)
	}
	if err != nil {
		t.Errorf("live-matched kill+recover returned non-nil error: %v", err)
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

	dir := apitest.HardenedTempDir(t)
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
