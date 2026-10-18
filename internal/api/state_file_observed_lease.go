package api

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/gofrs/flock"
)

var ErrStateFileLeaseBorrowActive = errors.New("state file observed lease close pending on entered borrow")

type stateFileLeaseIssuer struct{ marker byte }

// StateFileObservedLease is issued only by the existing leaf ledger. It owns
// the actual original flock and never exposes a native handle to borrowers.
type StateFileObservedLease struct {
	mu               sync.Mutex
	issuer           *stateFileLeaseIssuer
	path, lockPath   string
	owner            *flock.Flock
	acquisition      flock.AcquisitionObservation
	gateRelease      func()
	borrows          uint64
	closeRequested   bool
	closeObservation *StateFileLeaseCloseObservation
	closeErr         error
}

type StateFileLeaseCloseObservation struct {
	lease      *StateFileObservedLease
	issuer     *stateFileLeaseIssuer
	generation uint64
	primitive  flock.ReleaseObservation
}

// Snapshot returns the primitive's exact captured result, never authority.
func (o *StateFileLeaseCloseObservation) Snapshot() flock.ReleaseObservation {
	if o == nil {
		return flock.ReleaseObservation{}
	}
	return o.primitive
}

func (o *StateFileLeaseCloseObservation) Validate(lease *StateFileObservedLease) error {
	if lease == nil || o == nil {
		return errors.New("original observed lease close required")
	}
	lease.mu.Lock()
	defer lease.mu.Unlock()
	if lease.issuer == nil || o.lease != lease || o.issuer != lease.issuer || o != lease.closeObservation || o.generation == 0 || o.generation != lease.acquisition.Generation || o.primitive.Generation != o.generation || !o.primitive.Complete || o.primitive.Pending || o.primitive.Error != nil || lease.closeErr != nil {
		return errors.New("original observed lease closure incomplete or substituted")
	}
	return nil
}

func observedStateFileLeasePath(path string) (string, error) {
	if strings.TrimSpace(path) == "" || strings.IndexByte(path, 0) >= 0 || !filepath.IsAbs(path) || filepath.Clean(path) != path || filepath.Dir(path) == path {
		return "", errors.New("canonical absolute state file path required")
	}
	return path, nil
}

func AcquireStateFileObservedLease(ctx context.Context, statePath string) (*StateFileObservedLease, error) {
	return acquireStateFileObservedLease(ctx, statePath)
}

// Acquisition shares the legacy ledger's gate and poison entry. Context
// cancellation applies to both metadata contention and the original flock.
func acquireStateFileObservedLease(ctx context.Context, statePath string) (*StateFileObservedLease, error) {
	if ctx == nil {
		return nil, errors.New("original lease acquisition context required")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	path, err := observedStateFileLeasePath(statePath)
	if err != nil {
		return nil, err
	}
	lockPath := supervisorIntentLockPath(path)
	gate := ledgeredLeafGate(lockPath)
	for !gate.TryLock() {
		timer := time.NewTimer(stateFileLockPollInterval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
	if err := ctx.Err(); err != nil {
		gate.Unlock()
		return nil, err
	}
	if err := unconfirmedLockRelease(lockPath); err != nil {
		gate.Unlock()
		return nil, err
	}
	lease := &StateFileObservedLease{issuer: &stateFileLeaseIssuer{marker: 1}, path: path, lockPath: lockPath, owner: flock.New(lockPath), gateRelease: gate.Unlock}
	lease.acquisition, err = lease.owner.TryLockContextObserved(ctx, stateFileLockPollInterval)
	if err != nil || !lease.acquisition.Attempted || !lease.acquisition.Returned || !lease.acquisition.Held {
		_, closeErr := lease.CloseObserved()
		return lease, errors.Join(err, errors.New("original observed lease acquisition incomplete"), closeErr)
	}
	return lease, nil
}

func (lease *StateFileObservedLease) AcquisitionObservation() flock.AcquisitionObservation {
	if lease == nil {
		return flock.AcquisitionObservation{}
	}
	lease.mu.Lock()
	defer lease.mu.Unlock()
	return lease.acquisition
}

func (lease *StateFileObservedLease) validateHeldLocked(ctx context.Context, path string) error {
	if ctx == nil {
		return errors.New("original lease borrower context required")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	canonical, err := observedStateFileLeasePath(path)
	if err != nil {
		return err
	}
	if lease.issuer == nil || lease.owner == nil || canonical != lease.path || supervisorIntentLockPath(canonical) != lease.lockPath || lease.acquisition.Generation == 0 || !lease.acquisition.Attempted || !lease.acquisition.Returned || !lease.acquisition.Held || lease.acquisition.Error != nil || lease.closeRequested || lease.closeObservation != nil || !lease.owner.Locked() {
		return errors.New("original observed lease is not held for this state path")
	}
	return unconfirmedLockRelease(lease.lockPath)
}

func (lease *StateFileObservedLease) ValidateHeld(ctx context.Context, statePath string) error {
	if lease == nil {
		return errors.New("original observed state lease required")
	}
	lease.mu.Lock()
	defer lease.mu.Unlock()
	return lease.validateHeldLocked(ctx, statePath)
}

func (lease *StateFileObservedLease) Borrow(ctx context.Context, statePath string, body func() error) (retErr error) {
	if lease == nil || body == nil {
		return errors.New("original observed lease and borrow body required")
	}
	lease.mu.Lock()
	if err := lease.validateHeldLocked(ctx, statePath); err != nil {
		lease.mu.Unlock()
		return err
	}
	lease.borrows++
	lease.mu.Unlock()
	// This same entered operation completes a pending close; no worker, timer,
	// mutex held across the body, or independent closer is introduced.
	defer func() {
		lease.mu.Lock()
		lease.borrows--
		if lease.borrows == 0 && lease.closeRequested {
			lease.closeLocked()
		}
		retErr = errors.Join(retErr, ctx.Err(), lease.closeErr)
		lease.mu.Unlock()
	}()
	return body()
}

func (lease *StateFileObservedLease) closeLocked() {
	if lease.closeObservation != nil {
		return
	}
	primitive, err := lease.owner.ReleaseObserved()
	if primitive.Generation != lease.acquisition.Generation || !primitive.AcquisitionReturned || !primitive.AllocatedFilesClosed || primitive.Pending || (lease.acquisition.Held && !primitive.Complete) {
		err = errors.Join(err, errors.New("original observed lease release incomplete"))
	}
	if err != nil {
		err = recordUnconfirmedLockRelease(lease.lockPath, err)
	}
	lease.closeObservation = &StateFileLeaseCloseObservation{lease: lease, issuer: lease.issuer, generation: lease.acquisition.Generation, primitive: primitive}
	lease.closeErr = err
	if lease.gateRelease != nil {
		lease.gateRelease()
		lease.gateRelease = nil
	}
}

func (lease *StateFileObservedLease) CloseObserved() (*StateFileLeaseCloseObservation, error) {
	if lease == nil {
		return nil, errors.New("original observed state lease required")
	}
	lease.mu.Lock()
	defer lease.mu.Unlock()
	if lease.issuer == nil || lease.owner == nil {
		return nil, errors.New("original observed lease issuer missing")
	}
	if lease.closeObservation != nil {
		return lease.closeObservation, lease.closeErr
	}
	lease.closeRequested = true
	if lease.borrows != 0 {
		return nil, ErrStateFileLeaseBorrowActive
	}
	lease.closeLocked()
	return lease.closeObservation, lease.closeErr
}
