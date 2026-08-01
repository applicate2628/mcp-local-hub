package clients

import (
	"errors"
	"path/filepath"
	"testing"

	"github.com/gofrs/flock"
)

func TestWithConfigLock_ReleaseFailureIsJoinedAndLaterAcquireFailsFast(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "client.json")
	lockPath := configPath + ".lock"
	unlockFailure := errors.New("injected config unlock failure")
	previous := configFlockUnlockFn
	var stranded []*flock.Flock
	configFlockUnlockFn = func(fl *flock.Flock) error {
		if fl.Path() == lockPath {
			stranded = append(stranded, fl)
			return unlockFailure
		}
		return previous(fl)
	}
	t.Cleanup(func() {
		configFlockUnlockFn = previous
		for _, fl := range stranded {
			_ = fl.Unlock()
		}
		unconfirmedConfigLockReleasesMu.Lock()
		delete(unconfirmedConfigLockReleases, lockPath)
		unconfirmedConfigLockReleasesMu.Unlock()
	})

	primary := errors.New("primary mutation failure")
	err := withConfigLock(configPath, func() error { return primary })
	if !errors.Is(err, primary) {
		t.Fatalf("withConfigLock error = %v, want primary cause", err)
	}
	if !errors.Is(err, unlockFailure) {
		t.Fatalf("withConfigLock error = %v, want unlock cause", err)
	}
	if !errors.Is(err, ErrConfigLockReleaseUnconfirmed) {
		t.Fatalf("withConfigLock error = %v, want ErrConfigLockReleaseUnconfirmed", err)
	}

	reentered := false
	err = withConfigLock(configPath, func() error {
		reentered = true
		return nil
	})
	if reentered {
		t.Fatal("later acquire entered the critical section despite an unconfirmed retained lock")
	}
	if !errors.Is(err, ErrConfigLockReleaseUnconfirmed) {
		t.Fatalf("later acquire error = %v, want ErrConfigLockReleaseUnconfirmed", err)
	}
}
