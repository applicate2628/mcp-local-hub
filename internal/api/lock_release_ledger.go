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

// lockReleaseLedgerEntry coordinates THIS process's acquire/release transition
// for one flock leaf. A blocking OS acquire is entered only when no local lease
// is held or being acquired; an active local release therefore resolves before a
// second local acquire can touch the OS lock. Foreign-process contention still
// reaches flock.Lock and remains blocking.
type lockReleaseLedgerEntry struct {
	mu        sync.Mutex
	changed   chan struct{}
	acquiring bool
	held      bool
	ghost     error
}

var unconfirmedLockReleases = struct {
	sync.Mutex
	byPath map[string]*lockReleaseLedgerEntry
}{byPath: make(map[string]*lockReleaseLedgerEntry)}

func lockReleaseLedgerEntryFor(path string) *lockReleaseLedgerEntry {
	unconfirmedLockReleases.Lock()
	defer unconfirmedLockReleases.Unlock()
	if entry := unconfirmedLockReleases.byPath[path]; entry != nil {
		return entry
	}
	entry := &lockReleaseLedgerEntry{changed: make(chan struct{})}
	unconfirmedLockReleases.byPath[path] = entry
	return entry
}

func (entry *lockReleaseLedgerEntry) notifyLocked() {
	close(entry.changed)
	entry.changed = make(chan struct{})
}

// unconfirmedLockRelease returns the first release failure recorded for path.
// Entries are deliberately process-lifetime state and are never cleared.
func unconfirmedLockRelease(path string) error {
	entry := lockReleaseLedgerEntryFor(path)
	entry.mu.Lock()
	defer entry.mu.Unlock()
	return entry.ghost
}

// recordUnconfirmedLockRelease is the sole writer of stranded-release state.
// The first failure wins so later callers retain the original diagnosis.
func recordUnconfirmedLockRelease(path string, cause error) error {
	if cause == nil {
		return nil
	}
	entry := lockReleaseLedgerEntryFor(path)
	entry.mu.Lock()
	defer entry.mu.Unlock()
	if entry.ghost != nil {
		return entry.ghost
	}
	entry.ghost = fmt.Errorf("%w for %s: %w", ErrLockReleaseUnconfirmed, path, cause)
	entry.notifyLocked()
	return entry.ghost
}

// beginBlockingAcquire reserves the local transition before flock.Lock. It
// waits only for this process's prior acquire or release; once reserved, no
// local release can still be in flight, so any later flock.Lock wait is solely
// foreign-process contention.
func (entry *lockReleaseLedgerEntry) beginBlockingAcquire(onWait func()) error {
	for {
		entry.mu.Lock()
		if entry.ghost != nil {
			err := entry.ghost
			entry.mu.Unlock()
			return err
		}
		if !entry.acquiring && !entry.held {
			entry.acquiring = true
			entry.mu.Unlock()
			return nil
		}
		changed := entry.changed
		entry.mu.Unlock()
		if onWait != nil {
			onWait()
		}
		<-changed
	}
}

func (entry *lockReleaseLedgerEntry) beginTryAcquire() (bool, error) {
	entry.mu.Lock()
	defer entry.mu.Unlock()
	if entry.ghost != nil {
		return false, entry.ghost
	}
	if entry.acquiring || entry.held {
		return false, nil
	}
	entry.acquiring = true
	return true, nil
}

func (entry *lockReleaseLedgerEntry) finishAcquire(locked bool) {
	entry.mu.Lock()
	entry.acquiring = false
	entry.held = locked
	entry.notifyLocked()
	entry.mu.Unlock()
}

func (entry *lockReleaseLedgerEntry) finishRelease() {
	entry.mu.Lock()
	entry.held = false
	entry.notifyLocked()
	entry.mu.Unlock()
}

// newLedgeredFlockRelease turns one acquired flock leaf into a concurrency-safe
// one-shot release. The underlying unlock runs at most once; every invocation
// observes the same memoized result. releaseCompleted must publish the owning
// acquire transition after the underlying unlock returns, on success or error.
func newLedgeredFlockRelease(path string, unlock func() error, releaseCompleted func()) func() error {
	var once sync.Once
	var result error
	return func() error {
		once.Do(func() {
			defer func() {
				if releaseCompleted != nil {
					releaseCompleted()
				}
			}()
			if err := unlock(); err != nil {
				result = recordUnconfirmedLockRelease(path, err)
			}
		})
		return result
	}
}

func lockLeafLedgered(path string) (func() error, error) {
	return lockLeafLedgeredWithUnlock(path, func(fl *flock.Flock) error { return fl.Unlock() })
}

func lockLeafLedgeredWithUnlock(path string, unlock func(*flock.Flock) error) (func() error, error) {
	return lockLeafLedgeredWithUnlockAndWaitObserver(path, unlock, nil)
}

// lockLeafLedgeredWithUnlockAndWaitObserver is the testable core for a blocking
// leaf acquire. onWait only observes the local-busy branch after entry.mu is
// released and before waiting on the captured state-change channel. It cannot
// inspect, decide, or mutate the ghost/reservation invariant.
func lockLeafLedgeredWithUnlockAndWaitObserver(path string, unlock func(*flock.Flock) error, onWait func()) (func() error, error) {
	entry := lockReleaseLedgerEntryFor(path)
	if err := entry.beginBlockingAcquire(onWait); err != nil {
		return nil, err
	}
	lock := flock.New(path)
	if err := lock.Lock(); err != nil {
		entry.finishAcquire(false)
		return nil, fmt.Errorf("lock %s: %w", path, err)
	}
	entry.finishAcquire(true)
	return newLedgeredFlockRelease(path, func() error { return unlock(lock) }, entry.finishRelease), nil
}

// tryLockLeafLedgered is the non-blocking leaf primitive used by Registry and
// the supervisor-intent repair lock. Only real foreign contention returns
// (nil, false, nil); a process-local stranded release is always an error.
func tryLockLeafLedgered(path string) (func() error, bool, error) {
	return tryLockLeafLedgeredWithUnlock(path, func(fl *flock.Flock) error { return fl.Unlock() })
}

func tryLockLeafLedgeredWithUnlock(path string, unlock func(*flock.Flock) error) (func() error, bool, error) {
	entry := lockReleaseLedgerEntryFor(path)
	acquire, err := entry.beginTryAcquire()
	if err != nil {
		return nil, false, err
	}
	if !acquire {
		return nil, false, nil
	}
	lock := flock.New(path)
	locked, err := lock.TryLock()
	if err != nil {
		entry.finishAcquire(false)
		return nil, false, fmt.Errorf("try-lock %s: %w", path, err)
	}
	if !locked {
		entry.finishAcquire(false)
		return nil, false, nil
	}
	entry.finishAcquire(true)
	return newLedgeredFlockRelease(path, func() error { return unlock(lock) }, entry.finishRelease), true, nil
}

// ReleaseAndJoin is the sole spelling for preserving a caller's primary error
// while also reporting release failure from a deferred one-shot callback.
func ReleaseAndJoin(primary *error, release func() error) {
	if primary == nil || release == nil {
		return
	}
	*primary = errors.Join(*primary, release())
}
