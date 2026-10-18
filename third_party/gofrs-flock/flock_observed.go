// Copyright 2026 mcp-local-hub contributors.
// Same pinned gofrs/flock owner extension; original BSD license applies.
package flock

import (
	"context"
	"errors"
	"os"
	"time"
)

type AcquisitionObservation struct {
	Generation           uint64
	Attempted            bool
	Returned             bool
	Held                 bool
	FilesOpened          uint64
	FileCloseAttempts    uint64
	FilesClosed          uint64
	AllocatedFilesClosed bool
	Error                error
}

type ReleaseObservation struct {
	Generation             uint64
	AcquisitionAttempted   bool
	AcquisitionReturned    bool
	HeldAtRelease          bool
	LegacyReleaseAttempted bool
	UnlockAttempted        bool
	UnlockReturned         bool
	UnlockError            error
	FileCloseAttempted     bool
	FileCloseReturned      bool
	FileCloseError         error
	FilesOpened            uint64
	FileCloseAttempts      uint64
	FilesClosed            uint64
	AllocatedFilesClosed   bool
	Complete               bool
	Pending                bool
	Error                  error
}

type observedAcquisition struct {
	generation                              uint64
	attempted, returned                     bool
	held                                    bool
	filesOpened, closeAttempts, filesClosed uint64
	closeErr                                error
	failedCloseOwner                        *os.File
	release                                 *ReleaseObservation
	legacyReleaseAttempted                  bool
	legacyReleaseReturned                   bool
	legacyReleaseErr                        error
}

// TryLockContextObserved uses the same platform TryLock implementation. Every
// allocated descriptor is counted by setFh/resetFh, including busy/error and
// POSIX reopen branches. Legacy returns keep their original error semantics.
func (f *Flock) TryLockContextObserved(ctx context.Context, retryDelay time.Duration) (AcquisitionObservation, error) {
	if f == nil || ctx == nil {
		return AcquisitionObservation{}, errors.New("observed flock/context required")
	}
	if err := ctx.Err(); err != nil {
		return AcquisitionObservation{}, err
	}
	f.m.Lock()
	if f.observed != nil && !f.observed.returned {
		f.m.Unlock()
		return AcquisitionObservation{}, errors.New("observed acquisition already entered")
	}
	if f.observedPending != nil {
		err := f.observedPending
		f.m.Unlock()
		return AcquisitionObservation{}, err
	}
	if f.fh != nil || f.l || f.r {
		f.m.Unlock()
		return AcquisitionObservation{}, errors.New("preexisting or unresolved flock ownership cannot become observed acquisition")
	}
	f.observedGeneration++
	f.observed = &observedAcquisition{generation: f.observedGeneration, attempted: true}
	f.m.Unlock()
	held, acquireErr := tryCtx(ctx, f.tryLockObserved, retryDelay)
	f.m.Lock()
	defer f.m.Unlock()
	state := f.observed
	state.returned = true
	state.held = held
	err := errors.Join(acquireErr, state.closeErr, ctx.Err())
	view := AcquisitionObservation{Generation: state.generation, Attempted: true, Returned: true, Held: held, FilesOpened: state.filesOpened, FileCloseAttempts: state.closeAttempts, FilesClosed: state.filesClosed, AllocatedFilesClosed: !held && state.filesOpened == state.filesClosed && state.closeErr == nil, Error: err}
	if !held && (f.fh != nil || state.closeErr != nil) {
		f.observedPending = errors.Join(f.observedPending, errors.New("observed acquisition resource closure incomplete"))
		err = errors.Join(err, f.observedPending)
		view.Error = err
	}
	return view, err
}

// ReleaseObserved captures one native unlock and one actual close per allocated
// file, then caches the result for this acquisition. An uncertain descriptor is
// never retried or used for another acquisition.
func (f *Flock) ReleaseObserved() (ReleaseObservation, error) {
	if f == nil {
		return ReleaseObservation{}, errors.New("observed flock owner required")
	}
	f.m.Lock()
	defer f.m.Unlock()
	state := f.observed
	if state == nil || !state.attempted || !state.returned {
		return ReleaseObservation{}, errors.New("original observed acquisition required")
	}
	if state.release != nil {
		if f.fh != nil || f.l || f.r {
			return ReleaseObservation{}, errors.New("substituted legacy flock ownership cannot reuse observed release")
		}
		return *state.release, state.release.Error
	}
	view := ReleaseObservation{Generation: state.generation, AcquisitionAttempted: true, AcquisitionReturned: true, HeldAtRelease: state.held}
	if state.legacyReleaseAttempted {
		view.LegacyReleaseAttempted = true
		view.UnlockAttempted, view.UnlockReturned, view.UnlockError = true, state.legacyReleaseReturned, state.legacyReleaseErr
		f.observedPending = errors.Join(f.observedPending, state.legacyReleaseErr, errors.New("legacy release cannot mint an observed acquisition closure"))
		if f.fh != nil {
			f.resetFh()
			f.l = false
			f.r = false
		}
	} else if state.held && f.fh != nil && (f.l || f.r) {
		view.UnlockAttempted = true
		view.UnlockError = f.unlockLocked()
		view.UnlockReturned = true
		if view.UnlockError != nil {
			f.observedPending = errors.Join(f.observedPending, view.UnlockError)
			f.resetFh()
			f.l = false
			f.r = false
		}
	} else if f.fh != nil {
		f.resetFh()
	}
	view.FilesOpened, view.FileCloseAttempts, view.FilesClosed = state.filesOpened, state.closeAttempts, state.filesClosed
	view.FileCloseAttempted = state.closeAttempts > 0
	view.FileCloseReturned = state.closeAttempts > 0
	view.FileCloseError = state.closeErr
	view.AllocatedFilesClosed = state.filesOpened == state.filesClosed && state.closeErr == nil && f.fh == nil
	view.Complete = !state.legacyReleaseAttempted && state.held && view.UnlockAttempted && view.UnlockReturned && view.UnlockError == nil && state.filesOpened > 0 && view.AllocatedFilesClosed
	view.Error = errors.Join(view.UnlockError, view.FileCloseError, f.observedPending)
	if state.held && !view.Complete && view.Error == nil {
		view.Error = errors.New("original observed release incomplete")
		f.observedPending = view.Error
	}
	view.Pending = view.Error != nil
	state.release = &view
	return view, view.Error
}

func (f *Flock) observedAcquireGuard(original bool) error {
	if f.observedPending != nil {
		return f.observedPending
	}
	if !original && f.observed != nil && f.observed.release == nil {
		return errors.New("original observed acquisition must settle before legacy acquisition")
	}
	if original && (f.observed == nil || f.observed.returned) {
		return errors.New("original observed acquisition is not entered")
	}
	return nil
}

func (f *Flock) unlockLegacyLocked() error {
	state := f.observed
	if state != nil && state.release == nil {
		state.legacyReleaseAttempted = true
	}
	err := f.unlockLocked()
	if state != nil && state.release == nil {
		state.legacyReleaseReturned = true
		state.legacyReleaseErr = errors.Join(state.legacyReleaseErr, err)
	}
	return err
}
