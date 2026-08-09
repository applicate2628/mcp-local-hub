package api

import (
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/gofrs/flock"
)

func assertRegistryReleased(t testing.TB, release func() error) {
	t.Helper()
	if release == nil {
		t.Fatal("nil Registry release callback")
	}
	if err := release(); err != nil {
		t.Fatalf("release Registry lock: %v", err)
	}
}

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

func TestLockLeafLedgered_ConcurrentWaiterSeesReleasePoison(t *testing.T) {
	reg := NewRegistry(filepath.Join(t.TempDir(), "workspaces.yaml"))
	lockPath := reg.LockPath()
	releaseCause := errors.New("injected retained registry lock")

	previousUnlock := flockUnlockFn
	var stranded []*flock.Flock
	flockUnlockFn = func(fl *flock.Flock) error {
		if fl.Path() == lockPath {
			stranded = append(stranded, fl)
			return releaseCause
		}
		return previousUnlock(fl)
	}
	t.Cleanup(func() {
		flockUnlockFn = previousUnlock
		for _, fl := range stranded {
			_ = fl.Unlock()
		}
		unconfirmedLockReleasesMu.Lock()
		delete(unconfirmedLockReleases, lockPath)
		unconfirmedLockReleasesMu.Unlock()
	})

	release, err := reg.Lock()
	if err != nil {
		t.Fatalf("first Lock: %v", err)
	}
	waiterResult := make(chan error, 1)
	waiterStarted := make(chan struct{})
	go func() {
		close(waiterStarted)
		secondRelease, lockErr := reg.Lock()
		if secondRelease != nil {
			_ = secondRelease()
		}
		waiterResult <- lockErr
	}()

	<-waiterStarted
	time.Sleep(50 * time.Millisecond)
	// The first release still owns the per-leaf lifecycle gate. The waiter must
	// not enter the OS flock where a retained handle could strand it forever.
	select {
	case waiterErr := <-waiterResult:
		t.Fatalf("waiter returned before the first lifecycle released its gate: %v", waiterErr)
	default:
	}
	if releaseErr := release(); !errors.Is(releaseErr, releaseCause) {
		t.Fatalf("first release=%v, want injected cause", releaseErr)
	}
	select {
	case waiterErr := <-waiterResult:
		if !errors.Is(waiterErr, ErrLockReleaseUnconfirmed) || !errors.Is(waiterErr, releaseCause) {
			t.Fatalf("waiter error=%v, want fail-fast poison and cause", waiterErr)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("second waiter hung behind a retained flock instead of observing the release poison")
	}
}

func TestReleaseAndJoinAppliedSettlementMatrix(t *testing.T) {
	releaseCause := errors.New("release failed")
	for _, tc := range []struct {
		name        string
		primary     error
		release     error
		applied     bool
		wantApplied bool
		wantRelease bool
	}{
		{"success-nil", nil, nil, false, false, false},
		{"success-primary", errors.New("primary"), nil, true, false, false},
		{"release-unapplied", nil, fmt.Errorf("%w: %w", ErrLockReleaseUnconfirmed, releaseCause), false, false, true},
		{"release-applied", nil, fmt.Errorf("%w: %w", ErrLockReleaseUnconfirmed, releaseCause), true, true, true},
		{"primary-and-release-applied", errors.New("primary"), fmt.Errorf("%w: %w", ErrLockReleaseUnconfirmed, releaseCause), true, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.primary
			calls := 0
			releaseAndJoinApplied(&err, func() error { calls++; return tc.release }, "test release", tc.applied)
			if calls != 1 {
				t.Fatalf("release calls=%d, want 1", calls)
			}
			if got := IsAppliedLockReleaseUnconfirmed(err); got != tc.wantApplied {
				t.Fatalf("applied=%t, want %t; err=%v", got, tc.wantApplied, err)
			}
			if got := errors.Is(err, ErrLockReleaseUnconfirmed); got != tc.wantRelease {
				t.Fatalf("release class=%t, want %t; err=%v", got, tc.wantRelease, err)
			}
			if tc.primary != nil && !errors.Is(err, tc.primary) {
				t.Fatalf("primary cause lost: %v", err)
			}
			if tc.wantRelease && !errors.Is(err, releaseCause) {
				t.Fatalf("release cause lost: %v", err)
			}
		})
	}
}
