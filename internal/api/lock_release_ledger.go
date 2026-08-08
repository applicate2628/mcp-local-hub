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

func (e *appliedLockReleaseUnconfirmedError) Unwrap() error { return e.cause }

func markAppliedLockReleaseUnconfirmed(err error) error {
	if err == nil || !errors.Is(err, ErrLockReleaseUnconfirmed) {
		return err
	}
	return &appliedLockReleaseUnconfirmedError{cause: err}
}

func isAppliedLockReleaseUnconfirmed(err error) bool {
	var applied *appliedLockReleaseUnconfirmedError
	return errors.As(err, &applied)
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
)

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
	if ghost := unconfirmedLockRelease(lockPath); ghost != nil {
		return nil, ghost
	}
	fl := flock.New(lockPath)
	if err := fl.Lock(); err != nil {
		return nil, err
	}
	return newLedgeredFlockRelease(fl, lockPath), nil
}

func tryLockLeafLedgered(lockPath string) (func() error, bool, error) {
	if ghost := unconfirmedLockRelease(lockPath); ghost != nil {
		return nil, false, ghost
	}
	fl := flock.New(lockPath)
	locked, err := fl.TryLock()
	if err != nil {
		return nil, false, err
	}
	if !locked {
		return nil, false, nil
	}
	return newLedgeredFlockRelease(fl, lockPath), true, nil
}

func newLedgeredFlockRelease(fl *flock.Flock, lockPath string) func() error {
	unlockFn := flockUnlockFn
	var once sync.Once
	var outcome error
	return func() error {
		once.Do(func() {
			if err := unlockFn(fl); err != nil {
				outcome = recordUnconfirmedLockRelease(lockPath, err)
			}
		})
		return outcome
	}
}
