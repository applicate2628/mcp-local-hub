package api

import (
	"errors"
	"fmt"
	"sync"

	"github.com/gofrs/flock"
)

// ErrLockReleaseUnconfirmed classifies an API lock release this process could
// not confirm. gofrs/flock retains the handle on its release-error path, so a
// later acquire of the same leaf must fail fast rather than deadlock or report
// benign contention.
var ErrLockReleaseUnconfirmed = errors.New("lock release could not be confirmed; this process still holds the lock leaf")

// ErrAppliedLockReleaseUnconfirmed is the public, path-free disposition for a
// mutation that reached durable storage before its lock release became
// unconfirmed. Composition roots may project the committed result while still
// warning that this process must not touch the poisoned leaf again.
var ErrAppliedLockReleaseUnconfirmed = errors.New("durable mutation applied before lock release became unconfirmed")

// appliedLockReleaseUnconfirmedError distinguishes a poisoned lock leaf after
// its owning mutation reached durable storage.  Callers use this distinction to
// commit the truthful durable prefix instead of attempting compensations that
// would re-acquire the same leaf in this process.
type appliedLockReleaseUnconfirmedError struct {
	cause error
}

func (e *appliedLockReleaseUnconfirmedError) Error() string {
	return fmt.Sprintf("durable mutation applied before lock release became unconfirmed: %v", e.cause)
}

func (e *appliedLockReleaseUnconfirmedError) Unwrap() []error {
	return []error{ErrAppliedLockReleaseUnconfirmed, e.cause}
}

func markAppliedLockReleaseUnconfirmed(err error) error {
	if err == nil || !errors.Is(err, ErrLockReleaseUnconfirmed) {
		return err
	}
	return &appliedLockReleaseUnconfirmedError{cause: err}
}

func isAppliedLockReleaseUnconfirmed(err error) bool {
	return errors.Is(err, ErrAppliedLockReleaseUnconfirmed)
}

// IsAppliedLockReleaseUnconfirmed is the composition-root projection of the
// ledger's durable-write disposition.  Callers use it to keep a committed
// prefix instead of compensating through the same poisoned lock leaf.
func IsAppliedLockReleaseUnconfirmed(err error) bool {
	return isAppliedLockReleaseUnconfirmed(err)
}

// flockUnlockFn is the shared release fault-injection seam for ledgered API
// flocks.
var flockUnlockFn = func(fl *flock.Flock) error { return fl.Unlock() }

var (
	unconfirmedLockReleasesMu sync.Mutex
	unconfirmedLockReleases   = map[string]error{}
	ledgeredLeafGates         sync.Map // map[string]*sync.Mutex
)

func ledgeredLeafGate(lockPath string) *sync.Mutex {
	gate, _ := ledgeredLeafGates.LoadOrStore(lockPath, &sync.Mutex{})
	return gate.(*sync.Mutex)
}

func recordUnconfirmedLockRelease(lockPath string, cause error) error {
	unconfirmedLockReleasesMu.Lock()
	if _, exists := unconfirmedLockReleases[lockPath]; !exists {
		unconfirmedLockReleases[lockPath] = cause
	}
	unconfirmedLockReleasesMu.Unlock()
	return fmt.Errorf("%w: %s: %w", ErrLockReleaseUnconfirmed, lockPath, cause)
}

func unconfirmedLockRelease(lockPath string) error {
	unconfirmedLockReleasesMu.Lock()
	cause, exists := unconfirmedLockReleases[lockPath]
	unconfirmedLockReleasesMu.Unlock()
	if !exists {
		return nil
	}
	return fmt.Errorf("%w: %s: %w", ErrLockReleaseUnconfirmed, lockPath, cause)
}

// ReleaseAndJoin attempts release and joins a failure without substituting for
// the caller's primary result.
func ReleaseAndJoin(err *error, release func() error, what string) {
	if release == nil {
		return
	}
	if releaseErr := release(); releaseErr != nil {
		*err = errors.Join(*err, fmt.Errorf("%s: %w", what, releaseErr))
	}
}

// releaseAndJoinApplied is the single mutation-owner spelling for a ledgered
// release.  A caller supplies applied only after its final durable forward
// state stands; the ledger continues to own one-shot release and poisoning.
func releaseAndJoinApplied(err *error, release func() error, what string, applied bool) {
	if release == nil {
		return
	}
	if releaseErr := release(); releaseErr != nil {
		*err = errors.Join(*err, fmt.Errorf("%s: %w", what, releaseErr))
		if applied && errors.Is(releaseErr, ErrLockReleaseUnconfirmed) {
			*err = markAppliedLockReleaseUnconfirmed(*err)
		}
	}
}

func lockLeafLedgered(lockPath string) (func() error, error) {
	return lockLeafLedgeredWithUnlock(lockPath, flockUnlockFn)
}

func lockLeafLedgeredWithUnlock(lockPath string, unlockFn func(*flock.Flock) error) (func() error, error) {
	gate := ledgeredLeafGate(lockPath)
	gate.Lock()
	if ghost := unconfirmedLockRelease(lockPath); ghost != nil {
		gate.Unlock()
		return nil, ghost
	}
	fl := flock.New(lockPath)
	if err := fl.Lock(); err != nil {
		gate.Unlock()
		return nil, err
	}
	return newLedgeredFlockReleaseWithUnlock(fl, lockPath, gate.Unlock, unlockFn), nil
}

func tryLockLeafLedgered(lockPath string) (func() error, bool, error) {
	return tryLockLeafLedgeredWithUnlock(lockPath, flockUnlockFn)
}

func tryLockLeafLedgeredWithUnlock(lockPath string, unlockFn func(*flock.Flock) error) (func() error, bool, error) {
	gate := ledgeredLeafGate(lockPath)
	if !gate.TryLock() {
		return nil, false, nil
	}
	if ghost := unconfirmedLockRelease(lockPath); ghost != nil {
		gate.Unlock()
		return nil, false, ghost
	}
	fl := flock.New(lockPath)
	locked, err := fl.TryLock()
	if err != nil {
		gate.Unlock()
		return nil, false, err
	}
	if !locked {
		gate.Unlock()
		return nil, false, nil
	}
	return newLedgeredFlockReleaseWithUnlock(fl, lockPath, gate.Unlock, unlockFn), true, nil
}

func newLedgeredFlockRelease(fl *flock.Flock, lockPath string, releaseGate func()) func() error {
	return newLedgeredFlockReleaseWithUnlock(fl, lockPath, releaseGate, flockUnlockFn)
}

func newLedgeredFlockReleaseWithUnlock(fl *flock.Flock, lockPath string, releaseGate func(), unlockFn func(*flock.Flock) error) func() error {
	var once sync.Once
	var outcome error
	return func() error {
		once.Do(func() {
			defer releaseGate()
			if err := unlockFn(fl); err != nil {
				outcome = recordUnconfirmedLockRelease(lockPath, err)
			}
		})
		return outcome
	}
}
