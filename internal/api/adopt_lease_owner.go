package api

import (
	"errors"
	"fmt"
)

// adoptLeaseBeforeSettlementHook is an isolated deterministic race seam. It is
// nil in production and exists so the owner can be falsified at the exact
// handle-to-namespace boundary rather than by timing-dependent path races.
var adoptLeaseBeforeSettlementHook func() error

// adoptLeaseBeforeFinalReadbackHook is a deterministic late-race seam. Both
// platform owners invoke it only after the retained manifest handle is closed
// and immediately before the canonical slot readback. Production leaves it nil.
var adoptLeaseBeforeFinalReadbackHook func() error

// adoptLeaseBeforeSlotOpenHook is a deterministic ordering seam. It runs only
// after the namespace guard is locked and before the manifest slot is opened.
// Production leaves it nil; tests use it to make the guard/slot ordering
// observable without timing a filesystem race.
var adoptLeaseBeforeSlotOpenHook func() error

// adoptLeaseUnlockFailureHook is a package-local fault seam for all lock-only
// callers (de-adopt, forget and conservative GC). The platform owner performs
// the real unlock/close first, then joins this injected observation so tests
// cannot manufacture a stranded OS lock.
var adoptLeaseUnlockFailureHook func() error

func injectedAdoptLeaseUnlockFailure() error {
	if adoptLeaseUnlockFailureHook == nil {
		return nil
	}
	return adoptLeaseUnlockFailureHook()
}

const (
	adoptLeaseFailureGuardBusy        = "E_ADOPT_LEASE_GUARD_BUSY"
	adoptLeaseFailureBusy             = "E_ADOPT_LEASE_BUSY"
	adoptLeaseFailureNamespaceRefused = "E_ADOPT_LEASE_NAMESPACE_REFUSED"
	adoptLeaseFailureSlotReplaced     = "E_ADOPT_LEASE_SLOT_REPLACED"
	adoptLeaseFailureCleanup          = "E_ADOPT_LEASE_CLEANUP"
)

// AdoptLease is the settlement handle returned by an AdoptLeaseOwner. Its
// concrete implementation remains platform-owned; callers use this narrow
// contract to preserve the one acquire/one settlement lifecycle.
type AdoptLease interface {
	Unlock() error
	ReleaseAndRemove() error
}

// AdoptLeaseOwner owns acquisition of the per-manifest lease. NewAPI's normal
// execution path binds NewAdoptLeaseOwner; the dependency exists so alternate
// in-process composition can retain the same acquire/settle contract without
// bypassing the real platform owner.
type AdoptLeaseOwner interface {
	AcquireAdoptLease(manifestName string) (AdoptLease, bool, error)
}

type realAdoptLeaseOwner struct{}

// NewAdoptLeaseOwner returns the production per-manifest lease owner.
func NewAdoptLeaseOwner() AdoptLeaseOwner { return realAdoptLeaseOwner{} }

func (realAdoptLeaseOwner) AcquireAdoptLease(manifestName string) (AdoptLease, bool, error) {
	return tryAcquireAdoptManifestLease(manifestName)
}

// LeaseFailure is the public, redacted lease outcome. The cause is retained
// only for in-process protected diagnostics; Error deliberately projects only
// the stable failure identifier.
type LeaseFailure struct {
	FailureID        string
	Retryable        bool
	RecoveryRequired bool
	cause            error
}

func (e *LeaseFailure) Error() string {
	if e == nil {
		return ""
	}
	return e.FailureID
}

func (e *LeaseFailure) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.cause
}

func newLeaseFailure(id string, retryable, recoveryRequired bool, causes ...error) error {
	return &LeaseFailure{
		FailureID:        id,
		Retryable:        retryable,
		RecoveryRequired: recoveryRequired,
		cause:            errors.Join(causes...),
	}
}

func leaseCleanupFailure(causes ...error) error {
	return newLeaseFailure(adoptLeaseFailureCleanup, false, false, causes...)
}

func hasLeaseFailureID(err error, failureID string) bool {
	if err == nil {
		return false
	}
	if leaseFailure, ok := err.(*LeaseFailure); ok {
		if leaseFailure.FailureID == failureID {
			return true
		}
		return hasLeaseFailureID(leaseFailure.cause, failureID)
	}
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		for _, cause := range joined.Unwrap() {
			if hasLeaseFailureID(cause, failureID) {
				return true
			}
		}
		return false
	}
	if wrapped, ok := err.(interface{ Unwrap() error }); ok {
		return hasLeaseFailureID(wrapped.Unwrap(), failureID)
	}
	return false
}

// AdoptManifestLease is the sole owner of a per-manifest lease handle. It is
// deliberately handle-backed: no operation reopens the manifest leaf by an
// absolute path after acquisition.
type AdoptManifestLease struct{ impl adoptManifestLeasePlatform }

type adoptManifestLeasePlatform interface {
	unlock() error
	releaseAndRemove() error
}

// Unlock preserves the lock-only lifecycle used by recovery, de-adopt, and
// garbage collection. A completed adopt must use ReleaseAndRemove.
func (l *AdoptManifestLease) Unlock() error {
	if l == nil || l.impl == nil {
		return nil
	}
	if err := l.impl.unlock(); err != nil {
		return leaseCleanupFailure(err)
	}
	return nil
}

// ReleaseAndRemove settles an ordinary adopt. It is explicit so cleanup
// failures remain observable by ExecuteAdoptWithOpts.
func (l *AdoptManifestLease) ReleaseAndRemove() error {
	if l == nil || l.impl == nil {
		return fmt.Errorf("adopt lease: missing settlement owner")
	}
	return l.impl.releaseAndRemove()
}

func tryAcquireAdoptManifestLease(manifestName string) (*AdoptManifestLease, bool, error) {
	if _, err := adoptManifestLeasePath(manifestName); err != nil {
		return nil, false, err
	}
	impl, acquired, err := acquireAdoptManifestLeasePlatform(manifestName)
	if err != nil || !acquired {
		return nil, acquired, err
	}
	return &AdoptManifestLease{impl: impl}, true, nil
}
