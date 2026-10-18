//go:build windows || linux || darwin

package flock

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// No file is opened and no native lock is invoked. Fake first-close/unlock
// operations exercise the actual owner bookkeeping and cached failure paths.
func TestObservedReleaseSourceOneShot(t *testing.T) {
	for _, name := range []string{"complete", "unlock-failed", "close-failed", "both-failed"} {
		t.Run(name, func(t *testing.T) {
			unlockFailure := errors.New("synthetic unlock failure")
			closeFailure := errors.New("synthetic close failure")
			unlockCalls, closeCalls := 0, 0
			state := &observedAcquisition{generation: 1, attempted: true, returned: true, held: true, filesOpened: 1}
			f := &Flock{fh: &os.File{}, l: true, observed: state, observedGeneration: 1}
			f.observedUnlock = func() error {
				unlockCalls++
				if name == "unlock-failed" || name == "both-failed" {
					return unlockFailure
				}
				return nil
			}
			f.observedCloseFile = func(*os.File) error {
				closeCalls++
				if name == "close-failed" || name == "both-failed" {
					return closeFailure
				}
				return nil
			}
			view, err := f.ReleaseObserved()
			again, againErr := f.ReleaseObserved()
			if unlockCalls != 1 || closeCalls != 1 || view.Complete != (name == "complete") || again.Complete != view.Complete || (err == nil) != (againErr == nil) {
				t.Fatalf("one-shot release differs: %+v %+v %v %v", view, again, err, againErr)
			}
			if name != "complete" {
				if !view.Pending || err == nil {
					t.Fatal("uncertain release was accepted")
				}
				ctx, cancel := context.WithTimeout(context.Background(), time.Second)
				defer cancel()
				if _, err := f.TryLockContextObserved(ctx, time.Millisecond); err == nil {
					t.Fatal("pending original owner reacquired")
				}
			}
		})
	}
}

func TestObservedReleaseSourceFailedAcquireAndLegacy(t *testing.T) {
	f := &Flock{observed: &observedAcquisition{generation: 1, attempted: true, returned: true, filesOpened: 2, closeAttempts: 2, filesClosed: 2}}
	view, err := f.ReleaseObserved()
	if err != nil || !view.AllocatedFilesClosed || view.Complete || view.UnlockAttempted {
		t.Fatal("failed acquisition fabricated held release")
	}
	legacy := &Flock{fh: &os.File{}, l: true}
	if _, err := legacy.ReleaseObserved(); err == nil {
		t.Fatal("legacy ownership minted observed release")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err := legacy.TryLockContextObserved(ctx, time.Millisecond); err == nil {
		t.Fatal("legacy ownership became observed acquisition")
	}
	// A legacy release of an observed acquisition cannot cause another native
	// unlock or retroactively mint observed closure, even if its close succeeds.
	unlockCalls, closeCalls := 0, 0
	mixed := &Flock{fh: &os.File{}, l: true, observed: &observedAcquisition{generation: 7, attempted: true, returned: true, held: true, filesOpened: 1}}
	mixed.observedUnlock = func() error { unlockCalls++; return nil }
	mixed.observedCloseFile = func(*os.File) error { closeCalls++; return nil }
	if err := mixed.RLock(); err == nil {
		t.Fatal("legacy conversion entered an observed exclusive acquisition")
	}
	if err := mixed.Unlock(); err != nil {
		t.Fatal(err)
	}
	view, err = mixed.ReleaseObserved()
	if err == nil || view.Complete || !view.LegacyReleaseAttempted || unlockCalls != 1 || closeCalls != 1 {
		t.Fatal("legacy release reused native action or certified observed closure")
	}
	// Opening a nonexistent child under the owned test temp root fails before
	// descriptor allocation. That new attempt must still invalidate old credit.
	prior := ReleaseObservation{Generation: 9, Complete: true}
	stale := &Flock{path: filepath.Join(t.TempDir(), "missing", "lock"), observedGeneration: 9, observed: &observedAcquisition{generation: 9, attempted: true, returned: true, held: true, release: &prior}}
	if err := stale.setFh(os.O_RDONLY); err == nil {
		t.Fatal("synthetic nonexistent allocation succeeded")
	}
	if _, err := stale.ReleaseObserved(); err == nil {
		t.Fatal("later legacy allocation attempt reused stale release credit")
	}
}
