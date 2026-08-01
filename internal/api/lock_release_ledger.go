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

// flockUnlockFn is the shared release fault-injection seam for registry and
// weekly singleton flocks.
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
