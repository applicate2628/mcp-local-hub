package api

import (
	"errors"
	"path/filepath"
	"testing"

	"github.com/gofrs/flock"
)

func TestRegistryLock_ReleaseFailureIsReportedAndReacquireFailsFast(t *testing.T) {
	reg := NewRegistry(filepath.Join(t.TempDir(), "workspaces.yaml"))
	lockPath := reg.LockPath()
	unlockFailure := errors.New("injected registry unlock failure")
	previous := flockUnlockFn
	var stranded []*flock.Flock
	flockUnlockFn = func(fl *flock.Flock) error {
		if fl.Path() == lockPath {
			stranded = append(stranded, fl)
			return unlockFailure
		}
		return previous(fl)
	}
	t.Cleanup(func() {
		flockUnlockFn = previous
		for _, fl := range stranded {
			_ = fl.Unlock()
		}
		unconfirmedLockReleasesMu.Lock()
		delete(unconfirmedLockReleases, lockPath)
		unconfirmedLockReleasesMu.Unlock()
	})

	release, err := reg.Lock()
	if err != nil {
		t.Fatalf("Lock: %v", err)
	}
	err = release()
	if !errors.Is(err, ErrLockReleaseUnconfirmed) || !errors.Is(err, unlockFailure) {
		t.Fatalf("release error = %v, want release class and underlying cause", err)
	}
	if second := release(); !errors.Is(second, ErrLockReleaseUnconfirmed) {
		t.Fatalf("one-shot release second result = %v, want memoized failure", second)
	}
	if _, err := reg.Lock(); !errors.Is(err, ErrLockReleaseUnconfirmed) {
		t.Fatalf("blocking reacquire error = %v, want fail-fast release class", err)
	}
	if _, locked, err := reg.TryLock(); locked || !errors.Is(err, ErrLockReleaseUnconfirmed) {
		t.Fatalf("TryLock after failed release = locked=%t err=%v, want fail-fast release class", locked, err)
	}
}
