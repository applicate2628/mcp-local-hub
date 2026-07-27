package api

import (
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// TestSupervisorEventLockStateReleasedOnCleanEmit pins the baseline: an emit
// that acquires and releases the flock normally leaves NO residue, so a reader
// may truthfully report "confirmed released".
func TestSupervisorEventLockStateReleasedOnCleanEmit(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "supervisor-events.log")
	defer ResetSupervisorEventLockStateForPathForTest(path)

	logger, err := OpenSupervisorEventLog(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = logger.Close() }()

	if err := logger.Emit(SupervisorEvent{
		Severity: "info",
		Source:   "lifecycle",
		Event:    "lock-health-clean-emit",
	}); err != nil {
		t.Fatalf("Emit: %v", err)
	}
	if got := SupervisorEventLockStateForPath(path); got != SupervisorEventLockReleased {
		t.Fatalf("lock state after a clean emit = %q, want %q", got, SupervisorEventLockReleased)
	}
}

// TestSupervisorEventLockStateStrandedAfterDiscardedSyncRelease is the CLASS
// regression guard. It emits through the exact shape 131 call sites use —
// `_ = logger.Emit(event)`, the returned error DISCARDED — with a release that
// fails, and asserts the stranded flock is still observable afterwards.
//
// Before the single owner, this was invisible by construction: the only channel
// carrying the verdict was the error the caller just threw away.
//
// MUTATION: delete `noteSupervisorEventLockReleaseFailed(l.path)` from the
// synchronous releaser in emitPrepared. The state stays "released" and this
// test fails with:
//
//	lock state after a DISCARDED emit whose release failed = "released", want "stranded"
func TestSupervisorEventLockStateStrandedAfterDiscardedSyncRelease(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "supervisor-events.log")
	defer ResetSupervisorEventLockStateForPathForTest(path)

	logger, err := OpenSupervisorEventLog(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = logger.Close() }()

	restoreUnlock := SetSupervisorEventUnlockFnForTest(func(*SupervisorEventLog) error {
		return errors.New("UnlockFileEx: simulated persistent failure")
	})
	defer func() {
		restoreUnlock()
		_ = logger.lock.Unlock()
	}()

	// The point of the test: the verdict is dropped exactly as production drops it.
	_ = logger.Emit(SupervisorEvent{
		Severity: "info",
		Source:   "recover",
		Event:    "lock-health-discarded-release-failure",
	})

	if got := SupervisorEventLockStateForPath(path); got != SupervisorEventLockStranded {
		t.Fatalf("lock state after a DISCARDED emit whose release failed = %q, want %q", got, SupervisorEventLockStranded)
	}
}

// TestSupervisorEventLockStateStrandedAfterDiscardedReplayRelease covers the
// fourth producer site: TryReplayPending's own releaser. Replay is
// opportunistic by contract, so its error is routinely discarded — but a failed
// release there strands the flock just as hard.
//
// MUTATION: delete `noteSupervisorEventLockReleaseFailed(l.path)` from
// TryReplayPending's deferred releaser. This test fails with:
//
//	lock state after a DISCARDED TryReplayPending whose release failed = "released", want "stranded"
func TestSupervisorEventLockStateStrandedAfterDiscardedReplayRelease(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "supervisor-events.log")
	defer ResetSupervisorEventLockStateForPathForTest(path)

	logger, err := OpenSupervisorEventLog(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = logger.Close() }()

	restoreUnlock := SetSupervisorEventUnlockFnForTest(func(*SupervisorEventLog) error {
		return errors.New("UnlockFileEx: simulated persistent failure")
	})
	defer func() {
		restoreUnlock()
		_ = logger.lock.Unlock()
	}()

	_ = logger.TryReplayPending()

	if got := SupervisorEventLockStateForPath(path); got != SupervisorEventLockStranded {
		t.Fatalf("lock state after a DISCARDED TryReplayPending whose release failed = %q, want %q", got, SupervisorEventLockStranded)
	}
}

// TestSupervisorEventLockStateOutstandingWhileAbandonedWorkerHoldsFlock covers
// the state the seventh occurrence of this class lived in: the bounded-emit
// caller gave up, the worker kept BOTH locks, and every existing channel
// reported success.
//
// The stall window is engineered LARGE (the write blocks on a channel the test
// controls) rather than raced against a natural one, so a fast machine cannot
// turn this into a flake.
//
// MUTATION: delete `noteSupervisorEventLockHandoff(l.path)` from the handoff
// point in emitPrepared. The state reads "released" while the worker still owns
// the flock and this test fails with:
//
//	lock state while an abandoned worker still holds the flock = "released", want "outstanding"
func TestSupervisorEventLockStateOutstandingWhileAbandonedWorkerHoldsFlock(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "supervisor-events.log")
	defer ResetSupervisorEventLockStateForPathForTest(path)

	logger, err := OpenSupervisorEventLog(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = logger.Close() }()

	release := make(chan struct{})
	var releaseOnce sync.Once
	safeRelease := func() { releaseOnce.Do(func() { close(release) }) }
	restoreWrite := SetSupervisorEventWriteFnForTest(func(l *SupervisorEventLog, raw []byte) error {
		<-release // simulates the filesystem/AV stall the emit timeout exists for
		return l.writeEventLine(raw)
	})
	defer func() {
		safeRelease()
		// Join on the worker before restoring the seam or dropping the temp dir.
		logger.mu.Lock()
		logger.mu.Unlock()
		restoreWrite()
	}()

	pending, err := logger.EmitWithTimeoutTracked(SupervisorEvent{
		Severity: "info",
		Source:   "recover",
		Event:    "lock-health-abandoned-worker",
	}, 100*time.Millisecond)
	if !errors.Is(err, ErrSupervisorEventEmitTimeout) {
		t.Fatalf("EmitWithTimeoutTracked with a stalled write = %v, want ErrSupervisorEventEmitTimeout", err)
	}
	if pending == nil {
		t.Fatal("pending handle is nil after a genuine timeout")
	}

	// The worker is still inside the stalled write, holding both locks.
	if got := SupervisorEventLockStateForPath(path); got != SupervisorEventLockOutstanding {
		t.Fatalf("lock state while an abandoned worker still holds the flock = %q, want %q", got, SupervisorEventLockOutstanding)
	}

	// Letting the worker finish cleanly must clear the tally — the owner reports
	// a live condition, not a sticky one-way flag.
	safeRelease()
	if waitErr := pending.Wait(10 * time.Second); waitErr != nil {
		t.Fatalf("pending.Wait after unblocking the write = %v, want nil", waitErr)
	}
	if got := SupervisorEventLockStateForPath(path); got != SupervisorEventLockReleased {
		t.Fatalf("lock state after the worker released cleanly = %q, want %q", got, SupervisorEventLockReleased)
	}
}

// TestSupervisorEventLockStateStrandedOutranksOutstanding pins the precedence:
// a CONFIRMED failed release is worse news than a worker that has not reported
// yet, so it must not be masked by a concurrent outstanding handoff.
func TestSupervisorEventLockStateStrandedOutranksOutstanding(t *testing.T) {
	path := filepath.Join(t.TempDir(), "supervisor-events.log")
	defer ResetSupervisorEventLockStateForPathForTest(path)

	noteSupervisorEventLockHandoff(path)
	noteSupervisorEventLockReleaseFailed(path)
	if got := SupervisorEventLockStateForPath(path); got != SupervisorEventLockStranded {
		t.Fatalf("stranded+outstanding = %q, want %q", got, SupervisorEventLockStranded)
	}

	// Closing out the outstanding handoff cleanly must NOT clear the strand.
	noteSupervisorEventLockHandoffDone(path, nil)
	if got := SupervisorEventLockStateForPath(path); got != SupervisorEventLockStranded {
		t.Fatalf("stranded after the outstanding handoff closed = %q, want %q", got, SupervisorEventLockStranded)
	}
}

// TestSupervisorEventLockStateIsPerPath pins the scope choice: the owner is
// per-LOCK. One log family's stranded flock must never be reported against
// another's, and this is also what keeps each test's t.TempDir() isolated.
func TestSupervisorEventLockStateIsPerPath(t *testing.T) {
	dir := t.TempDir()
	stranded := filepath.Join(dir, "supervisor-events.log")
	other := filepath.Join(dir, "other-supervisor-events.log")
	defer ResetSupervisorEventLockStateForPathForTest(stranded)
	defer ResetSupervisorEventLockStateForPathForTest(other)

	noteSupervisorEventLockReleaseFailed(stranded)
	if got := SupervisorEventLockStateForPath(stranded); got != SupervisorEventLockStranded {
		t.Fatalf("stranded path = %q, want %q", got, SupervisorEventLockStranded)
	}
	if got := SupervisorEventLockStateForPath(other); got != SupervisorEventLockReleased {
		t.Fatalf("unrelated path = %q, want %q", got, SupervisorEventLockReleased)
	}
}
