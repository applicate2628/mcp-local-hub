package api

import (
	"errors"
	"fmt"
	"sync"

	"github.com/gofrs/flock"
)

// ErrLockReleaseUnconfirmed marks a lock leaf whose operating-system release
// failed. The process must not acquire that leaf again: the flock handle may
// still own it, so treating the next attempt as contention could deadlock.
var ErrLockReleaseUnconfirmed = errors.New("lock release unconfirmed")

var unconfirmedLockReleases = struct {
	sync.RWMutex
	byPath map[string]error
}{byPath: make(map[string]error)}

// unconfirmedLockRelease returns the first release failure recorded for path.
// Entries are deliberately process-lifetime state and are never cleared.
func unconfirmedLockRelease(path string) error {
	unconfirmedLockReleases.RLock()
	defer unconfirmedLockReleases.RUnlock()
	return unconfirmedLockReleases.byPath[path]
}

// recordUnconfirmedLockRelease is the sole writer of stranded-release state.
// The first failure wins so later callers retain the original diagnosis.
func recordUnconfirmedLockRelease(path string, cause error) error {
	if cause == nil {
		return nil
	}
	unconfirmedLockReleases.Lock()
	defer unconfirmedLockReleases.Unlock()
	if prior := unconfirmedLockReleases.byPath[path]; prior != nil {
		return prior
	}
	err := fmt.Errorf("%w for %s: %w", ErrLockReleaseUnconfirmed, path, cause)
	unconfirmedLockReleases.byPath[path] = err
	return err
}

// newLedgeredFlockRelease turns one acquired flock leaf into a concurrency-safe
// one-shot release. The underlying unlock runs at most once; every invocation
// observes the same memoized result.
func newLedgeredFlockRelease(path string, unlock func() error) func() error {
	var once sync.Once
	var result error
	return func() error {
		once.Do(func() {
			if err := unlock(); err != nil {
				result = recordUnconfirmedLockRelease(path, err)
			}
		})
		return result
	}
}

func lockLeafLedgered(path string) (func() error, error) {
	if err := unconfirmedLockRelease(path); err != nil {
		return nil, err
	}
	lock := flock.New(path)
	if err := lock.Lock(); err != nil {
		return nil, fmt.Errorf("lock %s: %w", path, err)
	}
	return newLedgeredFlockRelease(path, lock.Unlock), nil
}

// tryLockLeafLedgered is the non-blocking leaf primitive used by Registry and
// the supervisor-intent repair lock. Only real foreign contention returns
// (nil, false, nil); a process-local stranded release is always an error.
func tryLockLeafLedgered(path string) (func() error, bool, error) {
	if err := unconfirmedLockRelease(path); err != nil {
		return nil, false, err
	}
	lock := flock.New(path)
	locked, err := lock.TryLock()
	if err != nil {
		return nil, false, fmt.Errorf("try-lock %s: %w", path, err)
	}
	if !locked {
		return nil, false, nil
	}
	return newLedgeredFlockRelease(path, lock.Unlock), true, nil
}

// ReleaseAndJoin is the sole spelling for preserving a caller's primary error
// while also reporting release failure from a deferred one-shot callback.
func ReleaseAndJoin(primary *error, release func() error) {
	if primary == nil || release == nil {
		return
	}
	*primary = errors.Join(*primary, release())
}
