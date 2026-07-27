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
	// SupervisorEventLockOutstanding means an abandoned bounded-emit worker
	// still owns the flock. That worker has BY CONSTRUCTION already blown its
	// caller's deadline (it only exists on the emitTimeout path), so this is
	// not transient contention.
	SupervisorEventLockOutstanding SupervisorEventLockState = "outstanding"
	// SupervisorEventLockStranded means a release was attempted and FAILED.
	// This process may hold the flock for the rest of its lifetime, blocking
	// every other emitter — the supervisor, the install CLI — until it exits.
	SupervisorEventLockStranded SupervisorEventLockState = "stranded"
)

// supervisorEventLockRecord is the per-path tally. A record is DELETED once it
// returns to clean, so the map is empty in the steady state and a test's
// t.TempDir()-keyed entry does not outlive the test.
type supervisorEventLockRecord struct {
	outstanding int
	stranded    bool
}

var (
	supervisorEventLockMu     sync.Mutex
	supervisorEventLockHealth = map[string]*supervisorEventLockRecord{}
)

// noteSupervisorEventLockHandoff records that ownership of the flock passed to
// an abandoned bounded-emit worker. Pairs 1:1 with
// noteSupervisorEventLockHandoffDone.
func noteSupervisorEventLockHandoff(path string) {
	supervisorEventLockMu.Lock()
	defer supervisorEventLockMu.Unlock()
	rec := supervisorEventLockHealth[path]
	if rec == nil {
		rec = &supervisorEventLockRecord{}
		supervisorEventLockHealth[path] = rec
	}
	rec.outstanding++
}

// noteSupervisorEventLockHandoffDone closes out a handoff with the worker's own
// release outcome.
func noteSupervisorEventLockHandoffDone(path string, releaseErr error) {
	supervisorEventLockMu.Lock()
	defer supervisorEventLockMu.Unlock()
	rec := supervisorEventLockHealth[path]
	if rec == nil {
		// Defensive: a done without a handoff cannot happen on any current
		// path, but a failed release must never be dropped on the floor —
		// dropping it is the exact class this owner exists to end.
		if releaseErr == nil {
			return
		}
		supervisorEventLockHealth[path] = &supervisorEventLockRecord{stranded: true}
		return
	}
	if rec.outstanding > 0 {
		rec.outstanding--
	}
	if releaseErr != nil {
		rec.stranded = true
	}
	pruneSupervisorEventLockRecordLocked(path, rec)
}

// noteSupervisorEventLockReleaseFailed records a synchronous release failure —
// the caller still holds the flock and has no worker to account for.
func noteSupervisorEventLockReleaseFailed(path string) {
	supervisorEventLockMu.Lock()
	defer supervisorEventLockMu.Unlock()
	rec := supervisorEventLockHealth[path]
	if rec == nil {
		rec = &supervisorEventLockRecord{}
		supervisorEventLockHealth[path] = rec
	}
	rec.stranded = true
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
	supervisorEventLockMu.Lock()
	defer supervisorEventLockMu.Unlock()
	rec := supervisorEventLockHealth[path]
	switch {
	case rec == nil:
		return SupervisorEventLockReleased
	case rec.stranded:
		// Stranded outranks outstanding: a confirmed failed release is worse
		// than a worker that has not reported yet.
		return SupervisorEventLockStranded
	case rec.outstanding > 0:
		return SupervisorEventLockOutstanding
	default:
		return SupervisorEventLockReleased
	}
}

// ResetSupervisorEventLockStateForPathForTest clears one path's tally. Only
// tests may call it; production never resets, because a stranded flock stays
// stranded for the process's lifetime.
func ResetSupervisorEventLockStateForPathForTest(path string) {
	supervisorEventLockMu.Lock()
	defer supervisorEventLockMu.Unlock()
	delete(supervisorEventLockHealth, path)
}
