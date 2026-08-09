package api

import "sync"

// SupervisorEventLockState answers ONE question for ONE supervisor-event-log
// path: can THIS process still be holding that log's cross-process flock?
//
// It exists because the answer is PROCESS-scoped while the channel that used to
// carry it — the error returned by an individual Emit — is CALL-scoped. A
// process-scoped fact on a call-scoped channel has no per-call consumer, so
// essentially every call site correctly concluded there was nothing to do with
// it and wrote `_ = logger.Emit(...)`. Measured on this tree that is 131
// discarded call sites across 22 files, and the release verdict was therefore
// owned by nobody. Six per-caller fixes did not converge; the seventh
// occurrence of the class appeared inside the sixth fix.
//
// Decision: work-items/decisions/2026-07-27-supervisor-event-flock-release-single-owner.md
type SupervisorEventLockState string

const (
	// SupervisorEventLockReleased means every flock this process took on the
	// path was observed released. It is the only state that may be reported to
	// an operator as "confirmed released".
	SupervisorEventLockReleased SupervisorEventLockState = "released"
	// SupervisorEventLockOutstanding means a bounded-emit worker goroutine
	// still owns the flock.
	//
	// This state is TRANSIENT and it is NOT rare. An earlier revision of this
	// comment claimed the worker "has BY CONSTRUCTION already blown its
	// caller's deadline (it only exists on the emitTimeout path), so this is
	// not transient contention". The first half is true and the conclusion does
	// not follow from it: the worker is spawned, and this handoff recorded, at
	// supervisor_events.go:766-770 — BEFORE the select at :798 that decides
	// whether the deadline was blown at all. So every bounded emit
	// (EmitWithTimeout / EmitWithTimeoutTracked / EmitPreparedWithTimeoutTracked)
	// occupies this state for the whole duration of its write, including the
	// healthy sub-millisecond case that returns success.
	//
	// Falsified by TestSupervisorEventLockStateOutstandingDuringHealthyBoundedEmit,
	// which samples the state from inside a write that then COMPLETES inside its
	// deadline and asserts the emit returned nil.
	//
	// The consequence for readers: "outstanding" means "wait", never "restart
	// this process". A reader that cannot distinguish it from Stranded will
	// eventually raise a permanent alarm for a healthy concurrent emit.
	SupervisorEventLockOutstanding SupervisorEventLockState = "outstanding"
	// SupervisorEventLockStranded means a release was attempted and FAILED.
	// This process may hold the flock for the rest of its lifetime, blocking
	// every other emitter — the supervisor, the install CLI — until it exits.
	//
	// Unlike Outstanding this is PERMANENT for the process: nothing clears a
	// stranded record (see noteSupervisorEventLockReleaseFailed, which does not
	// prune, and pruneSupervisorEventLockRecordLocked, which refuses to delete
	// while stranded is set). Only "restart this process" resolves it.
	SupervisorEventLockStranded SupervisorEventLockState = "stranded"
)

// Valid reports whether the state belongs to the public lock-state enum.
func (s SupervisorEventLockState) Valid() bool {
	switch s {
	case SupervisorEventLockReleased, SupervisorEventLockOutstanding, SupervisorEventLockStranded:
		return true
	default:
		return false
	}
}

// SupervisorEventLockSnapshot is an atomic observation of one path's
// process-local lock state. Revision advances only when that path's effective
// state changes.
type SupervisorEventLockSnapshot struct {
	State    SupervisorEventLockState
	Revision uint64
}

// SupervisorEventLockObserver receives effective-state transitions after the
// owner mutex is released.
type SupervisorEventLockObserver func(SupervisorEventLockSnapshot)

// SupervisorEventLockSubscription is the physical-state owner's narrow
// subscription handle. Close is idempotent. TryCloseAtTerminal linearizes a
// terminal claim against the current revision under supervisorEventLockMu, so a
// delayed callback cannot publish or unsubscribe behind a newer transition.
type SupervisorEventLockSubscription struct {
	path   string
	id     uint64
	closed bool
}

// supervisorEventLockRecord is the per-path tally. A record is DELETED once it
// returns to clean, so the map is empty in the steady state and a test's
// t.TempDir()-keyed entry does not outlive the test.
type supervisorEventLockRecord struct {
	outstanding int
	stranded    bool
}

var (
	supervisorEventLockMu        sync.Mutex
	supervisorEventLockHealth    = map[string]*supervisorEventLockRecord{}
	supervisorEventLockRevisions = map[string]uint64{}
	supervisorEventLockObservers = map[string]map[uint64]SupervisorEventLockObserver{}
	supervisorEventLockNextID    uint64
)

func supervisorEventLockStateLocked(path string) SupervisorEventLockState {
	rec := supervisorEventLockHealth[path]
	switch {
	case rec == nil:
		return SupervisorEventLockReleased
	case rec.stranded:
		return SupervisorEventLockStranded
	case rec.outstanding > 0:
		return SupervisorEventLockOutstanding
	default:
		return SupervisorEventLockReleased
	}
}

func supervisorEventLockTransitionLocked(path string, before SupervisorEventLockState) (SupervisorEventLockSnapshot, []SupervisorEventLockObserver, bool) {
	after := supervisorEventLockStateLocked(path)
	if after == before {
		return SupervisorEventLockSnapshot{}, nil, false
	}
	supervisorEventLockRevisions[path]++
	snapshot := SupervisorEventLockSnapshot{
		State:    after,
		Revision: supervisorEventLockRevisions[path],
	}
	observers := make([]SupervisorEventLockObserver, 0, len(supervisorEventLockObservers[path]))
	for _, observer := range supervisorEventLockObservers[path] {
		observers = append(observers, observer)
	}
	return snapshot, observers, true
}

func notifySupervisorEventLockObservers(snapshot SupervisorEventLockSnapshot, observers []SupervisorEventLockObserver) {
	for _, observer := range observers {
		observer(snapshot)
	}
}

// noteSupervisorEventLockHandoff records that ownership of the flock passed to
// an abandoned bounded-emit worker. Pairs 1:1 with
// noteSupervisorEventLockHandoffDone.
func noteSupervisorEventLockHandoff(path string) {
	supervisorEventLockMu.Lock()
	before := supervisorEventLockStateLocked(path)
	rec := supervisorEventLockHealth[path]
	if rec == nil {
		rec = &supervisorEventLockRecord{}
		supervisorEventLockHealth[path] = rec
	}
	rec.outstanding++
	snapshot, observers, changed := supervisorEventLockTransitionLocked(path, before)
	supervisorEventLockMu.Unlock()
	if changed {
		notifySupervisorEventLockObservers(snapshot, observers)
	}
}

// noteSupervisorEventLockHandoffDone closes out a handoff with the worker's own
// release outcome.
func noteSupervisorEventLockHandoffDone(path string, releaseErr error) {
	supervisorEventLockMu.Lock()
	before := supervisorEventLockStateLocked(path)
	rec := supervisorEventLockHealth[path]
	if rec == nil {
		// Defensive: a done without a handoff cannot happen on any current
		// path, but a failed release must never be dropped on the floor —
		// dropping it is the exact class this owner exists to end.
		if releaseErr == nil {
			supervisorEventLockMu.Unlock()
			return
		}
		supervisorEventLockHealth[path] = &supervisorEventLockRecord{stranded: true}
	} else {
		if rec.outstanding > 0 {
			rec.outstanding--
		}
		if releaseErr != nil {
			rec.stranded = true
		}
		pruneSupervisorEventLockRecordLocked(path, rec)
	}
	snapshot, observers, changed := supervisorEventLockTransitionLocked(path, before)
	supervisorEventLockMu.Unlock()
	if changed {
		notifySupervisorEventLockObservers(snapshot, observers)
	}
}

// noteSupervisorEventLockReleaseFailed records a synchronous release failure —
// the caller still holds the flock and has no worker to account for.
func noteSupervisorEventLockReleaseFailed(path string) {
	supervisorEventLockMu.Lock()
	before := supervisorEventLockStateLocked(path)
	rec := supervisorEventLockHealth[path]
	if rec == nil {
		rec = &supervisorEventLockRecord{}
		supervisorEventLockHealth[path] = rec
	}
	rec.stranded = true
	snapshot, observers, changed := supervisorEventLockTransitionLocked(path, before)
	supervisorEventLockMu.Unlock()
	if changed {
		notifySupervisorEventLockObservers(snapshot, observers)
	}
}

func pruneSupervisorEventLockRecordLocked(path string, rec *supervisorEventLockRecord) {
	if rec.outstanding == 0 && !rec.stranded {
		delete(supervisorEventLockHealth, path)
	}
}

// SupervisorEventLockStateForPath reports whether this process can still be
// holding the cross-process flock on the supervisor event log at path.
//
// `path` is the LOG path (the same value passed to OpenSupervisorEventLog), not
// the `.lock` sidecar.
//
// A caller that is about to tell an operator "the lock was released" must gate
// that claim on SupervisorEventLockReleased. Anything else means the claim
// cannot be made — the remedy is always "restart this process", never "retry
// the operation".
func SupervisorEventLockStateForPath(path string) SupervisorEventLockState {
	return SupervisorEventLockSnapshotForPath(path).State
}

// SupervisorEventLockSnapshotForPath atomically reads state and revision.
func SupervisorEventLockSnapshotForPath(path string) SupervisorEventLockSnapshot {
	supervisorEventLockMu.Lock()
	defer supervisorEventLockMu.Unlock()
	return SupervisorEventLockSnapshot{
		State:    supervisorEventLockStateLocked(path),
		Revision: supervisorEventLockRevisions[path],
	}
}

// ClaimSupervisorEventLockSnapshot linearizes one bounded comparison against
// the physical-state owner. The claim itself is the acknowledgement boundary;
// callers must perform any filesystem work after this function returns so a
// stalled store cannot block physical-state transitions process-wide.
func ClaimSupervisorEventLockSnapshot(path string, expected SupervisorEventLockSnapshot) (current SupervisorEventLockSnapshot, claimed bool) {
	supervisorEventLockMu.Lock()
	defer supervisorEventLockMu.Unlock()
	current = SupervisorEventLockSnapshot{
		State:    supervisorEventLockStateLocked(path),
		Revision: supervisorEventLockRevisions[path],
	}
	return current, current == expected
}

// CommitIfSupervisorEventLockSnapshot executes commit only while path still has
// exactly expected physical state. The caller supplies a bounded, non-reentrant
// commit: callbacks, subscriptions, logging, and further lock-state reads are
// forbidden while this owner mutex is held. On a mismatch current is the
// authoritative snapshot and commit is not called.
func CommitIfSupervisorEventLockSnapshot(path string, expected SupervisorEventLockSnapshot, commit func() error) (current SupervisorEventLockSnapshot, committed bool, err error) {
	supervisorEventLockMu.Lock()
	defer supervisorEventLockMu.Unlock()
	current = SupervisorEventLockSnapshot{
		State:    supervisorEventLockStateLocked(path),
		Revision: supervisorEventLockRevisions[path],
	}
	if current != expected {
		return current, false, nil
	}
	if err := commit(); err != nil {
		return current, false, err
	}
	return current, true, nil
}

func removeSupervisorEventLockObserverLocked(path string, id uint64) {
	delete(supervisorEventLockObservers[path], id)
	if len(supervisorEventLockObservers[path]) == 0 {
		delete(supervisorEventLockObservers, path)
	}
}

// Close removes the subscription exactly once.
func (s *SupervisorEventLockSubscription) Close() {
	if s == nil {
		return
	}
	supervisorEventLockMu.Lock()
	defer supervisorEventLockMu.Unlock()
	if s.closed {
		return
	}
	removeSupervisorEventLockObserverLocked(s.path, s.id)
	s.closed = true
}

// TryCloseAtTerminal removes this subscription only when revision is still the
// current physical revision and that revision is terminal. On failure it
// returns the newer authoritative snapshot and leaves the subscription active.
func (s *SupervisorEventLockSubscription) TryCloseAtTerminal(revision uint64) (SupervisorEventLockSnapshot, bool) {
	if s == nil {
		return SupervisorEventLockSnapshot{}, false
	}
	supervisorEventLockMu.Lock()
	defer supervisorEventLockMu.Unlock()
	current := SupervisorEventLockSnapshot{
		State:    supervisorEventLockStateLocked(s.path),
		Revision: supervisorEventLockRevisions[s.path],
	}
	if s.closed ||
		current.Revision != revision ||
		(current.State != SupervisorEventLockReleased && current.State != SupervisorEventLockStranded) {
		return current, false
	}
	removeSupervisorEventLockObserverLocked(s.path, s.id)
	s.closed = true
	return current, true
}

// SubscribeSupervisorEventLockState atomically registers an observer and
// returns the initial snapshot plus a revision-aware subscription handle.
func SubscribeSupervisorEventLockState(path string, observer SupervisorEventLockObserver) (SupervisorEventLockSnapshot, *SupervisorEventLockSubscription) {
	supervisorEventLockMu.Lock()
	supervisorEventLockNextID++
	id := supervisorEventLockNextID
	if supervisorEventLockObservers[path] == nil {
		supervisorEventLockObservers[path] = map[uint64]SupervisorEventLockObserver{}
	}
	supervisorEventLockObservers[path][id] = observer
	initial := SupervisorEventLockSnapshot{
		State:    supervisorEventLockStateLocked(path),
		Revision: supervisorEventLockRevisions[path],
	}
	supervisorEventLockMu.Unlock()
	return initial, &SupervisorEventLockSubscription{path: path, id: id}
}

// ResetSupervisorEventLockStateForPathForTest clears one path's tally. Only
// tests may call it; production never resets, because a stranded flock stays
// stranded for the process's lifetime.
func ResetSupervisorEventLockStateForPathForTest(path string) {
	supervisorEventLockMu.Lock()
	defer supervisorEventLockMu.Unlock()
	delete(supervisorEventLockHealth, path)
	delete(supervisorEventLockRevisions, path)
	delete(supervisorEventLockObservers, path)
}
