package api

import (
	"errors"
	"path/filepath"
	"reflect"
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

// TestSupervisorEventLockStateOutstandingDuringHealthyBoundedEmit is the guard
// for the claim the Outstanding doc comment used to make and no longer does:
// that the state only ever describes a worker which already blew its caller's
// deadline.
//
// It does not. emitPrepared spawns the worker and records the handoff at
// supervisor_events.go:766-770, BEFORE the select at :798 that decides whether
// the deadline was blown. So a perfectly healthy bounded emit occupies
// "outstanding" for the whole duration of its write.
//
// That is what makes the state unsafe to collapse into a permanent
// "restart this process" verdict at a consumer: it is the normal state of every
// in-flight bounded emit in the process, not a rare deadline-blown residue.
//
// The window is engineered deterministically large (the write blocks until the
// observer has sampled) rather than raced against a natural one, so a fast
// machine cannot flake it. The emit still returns SUCCESS, which is what makes
// this the HEALTHY path and not a restatement of the abandoned-worker test.
//
// MUTATION: move `noteSupervisorEventLockHandoff(l.path)` from before the `go`
// statement in emitPrepared to inside the `case <-ctx.Done():` arm of the
// select (i.e. record the handoff only when the deadline actually IS blown).
// This test then fails with:
//
//	owner state sampled from inside a healthy bounded emit = "released", want "outstanding"
func TestSupervisorEventLockStateOutstandingDuringHealthyBoundedEmit(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "supervisor-events.log")
	defer ResetSupervisorEventLockStateForPathForTest(path)

	logger, err := OpenSupervisorEventLog(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = logger.Close() }()

	inWrite := make(chan struct{})
	sampled := make(chan struct{})
	restoreWrite := SetSupervisorEventWriteFnForTest(func(l *SupervisorEventLog, raw []byte) error {
		close(inWrite)
		<-sampled // hold the flock until the observer has read the owner
		return l.writeEventLine(raw)
	})
	defer restoreWrite()

	var observed SupervisorEventLockState
	go func() {
		<-inWrite
		observed = SupervisorEventLockStateForPath(path)
		close(sampled)
	}()

	// A 30s budget against a write released the instant the observer samples:
	// this emit cannot time out, so whatever the observer saw is the state
	// during a healthy bounded emit.
	if emitErr := logger.EmitWithTimeout(SupervisorEvent{
		Severity: "info",
		Source:   "recover",
		Event:    "lock-health-healthy-bounded-emit",
	}, 30*time.Second); emitErr != nil {
		t.Fatalf("EmitWithTimeout = %v, want nil; the emit must SUCCEED for this to be the healthy path", emitErr)
	}

	if observed != SupervisorEventLockOutstanding {
		t.Fatalf("owner state sampled from inside a healthy bounded emit = %q, want %q", observed, SupervisorEventLockOutstanding)
	}
	if got := SupervisorEventLockStateForPath(path); got != SupervisorEventLockReleased {
		t.Fatalf("owner state after the healthy emit returned = %q, want %q", got, SupervisorEventLockReleased)
	}
}

func TestSupervisorEventLockStateObserverSequence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "supervisor-events.log")
	defer ResetSupervisorEventLockStateForPathForTest(path)

	var observed []SupervisorEventLockSnapshot
	initial, unsubscribe := SubscribeSupervisorEventLockState(path, func(snapshot SupervisorEventLockSnapshot) {
		// The callback runs after the owner mutex is released.
		if reread := SupervisorEventLockSnapshotForPath(path); reread != snapshot {
			t.Fatalf("callback snapshot=%+v reread=%+v", snapshot, reread)
		}
		observed = append(observed, snapshot)
	})
	defer unsubscribe()
	if initial != (SupervisorEventLockSnapshot{State: SupervisorEventLockReleased}) {
		t.Fatalf("initial=%+v", initial)
	}

	noteSupervisorEventLockHandoff(path)
	noteSupervisorEventLockHandoff(path)
	noteSupervisorEventLockHandoffDone(path, nil)
	noteSupervisorEventLockHandoffDone(path, nil)

	want := []SupervisorEventLockSnapshot{
		{State: SupervisorEventLockOutstanding, Revision: 1},
		{State: SupervisorEventLockReleased, Revision: 2},
	}
	if !reflect.DeepEqual(observed, want) {
		t.Fatalf("observed=%+v want=%+v", observed, want)
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
