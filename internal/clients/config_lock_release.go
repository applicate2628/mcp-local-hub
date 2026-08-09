package clients

import (
	"errors"
	"fmt"
	"sync"

	"github.com/gofrs/flock"
)

// ErrConfigLockReleaseUnconfirmed classifies a config-lock release that failed.
// The underlying flock handle remains retained by gofrs/flock on that path, so
// later same-process acquisitions must fail fast instead of blocking forever.
var ErrConfigLockReleaseUnconfirmed = errors.New("client config lock release could not be confirmed; this process may still hold the lock leaf")

// configFlockUnlockFn is the release fault-injection seam for client config
// locks. Tests replace it for one exact leaf and restore it in cleanup.
var configFlockUnlockFn = func(fl *flock.Flock) error { return fl.Unlock() }

var (
	unconfirmedConfigLockReleasesMu sync.Mutex
	unconfirmedConfigLockReleases   = map[string]error{}
)

func recordUnconfirmedConfigLockRelease(lockPath string, cause error) error {
	unconfirmedConfigLockReleasesMu.Lock()
	if _, exists := unconfirmedConfigLockReleases[lockPath]; !exists {
		unconfirmedConfigLockReleases[lockPath] = cause
	}
	unconfirmedConfigLockReleasesMu.Unlock()
	return fmt.Errorf("%w: %s: %w", ErrConfigLockReleaseUnconfirmed, lockPath, cause)
}

func unconfirmedConfigLockRelease(lockPath string) error {
	unconfirmedConfigLockReleasesMu.Lock()
	cause, exists := unconfirmedConfigLockReleases[lockPath]
	unconfirmedConfigLockReleasesMu.Unlock()
	if !exists {
		return nil
	}
	return fmt.Errorf("%w: %s: %w", ErrConfigLockReleaseUnconfirmed, lockPath, cause)
}

func newConfigLockRelease(fl *flock.Flock, lockPath string) func() error {
	unlockFn := configFlockUnlockFn
	var once sync.Once
	var outcome error
	return func() error {
		once.Do(func() {
			if err := unlockFn(fl); err != nil {
				outcome = recordUnconfirmedConfigLockRelease(lockPath, err)
			}
		})
		return outcome
	}
}
