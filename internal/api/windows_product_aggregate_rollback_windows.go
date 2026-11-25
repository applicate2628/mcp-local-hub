//go:build windows

package api

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"

	"golang.org/x/sys/windows"
)

const (
	productRecoveryJournalLeaf          = "windows-product-recovery-v3.json"
	productPairRecoveryJournalLeaf      = "windows-product-pair-recovery-v1.json"
	WindowsProductAggregateRollbackLeaf = "windows-product-aggregate-rollback-pending-v1.json"
)

// WindowsProductRollbackContinuation records the protected parent, actual file
// identity and full bytes received for this operation. A new process receives
// its own observation; this does not assert historical file identity.
type WindowsProductRollbackContinuation struct {
	parent   ConfigParentDirectoryIdentityV1
	original productBoundFileSnapshot
	raw      []byte
}

func (r *WindowsProductRollbackContinuation) Bytes() []byte { return bytes.Clone(r.raw) }

func rollbackLeafAbsent(parent windows.Handle, name string) error {
	h, err := ntOpenRelativeWithShareAccess(parent, name, windows.FILE_READ_ATTRIBUTES, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE)
	if isNotFoundErr(err) {
		return nil
	}
	if err != nil {
		return err
	}
	return errors.Join(errHubRouteStateChanged, windows.CloseHandle(h))
}

func openRollbackLeaf(parent windows.Handle, name string, mutate bool) (*os.File, error) {
	access := uint32(windows.GENERIC_READ | windows.READ_CONTROL)
	if mutate {
		access |= windows.DELETE
	}
	h, err := ntOpenRelativeWithShareAccess(parent, name, access, windows.FILE_SHARE_READ)
	if isNotFoundErr(err) {
		return nil, os.ErrNotExist
	}
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(h), name), nil
}

func receiveRollbackFile(f *os.File, parent ConfigParentDirectoryIdentityV1, capacity int64) (*WindowsProductRollbackContinuation, error) {
	snapshot, err := snapshotProductBoundFile(windows.Handle(f.Fd()))
	if err != nil || snapshot.identity.VolumeSerialNumber != parent.VolumeSerialNumber ||
		snapshot.extent < 1 || snapshot.extent > capacity {
		return nil, errors.Join(errHubRouteStateChanged, err)
	}
	raw, err := io.ReadAll(io.NewSectionReader(f, 0, snapshot.extent))
	if err != nil || int64(len(raw)) != snapshot.extent {
		return nil, errors.Join(errHubRouteStateChanged, err)
	}
	return &WindowsProductRollbackContinuation{parent: parent, original: snapshot, raw: raw}, nil
}

func (r *WindowsProductRollbackContinuation) matches(other *WindowsProductRollbackContinuation) bool {
	return r != nil && other != nil && r.parent == other.parent && r.original == other.original && bytes.Equal(r.raw, other.raw)
}

func (s *WindowsProductStateNamespace) ReceiveAggregateRollback(ctx context.Context, capacity int64) (r *WindowsProductRollbackContinuation, retErr error) {
	if err := ValidateStateFileReadCapacity(capacity); err != nil {
		return nil, err
	}
	if err := s.Validate(ctx); err != nil {
		return nil, err
	}
	n := s.native
	n.mu.Lock()
	defer n.mu.Unlock()
	if err := n.validateLocked(ctx); err != nil {
		return nil, err
	}
	f, err := openRollbackLeaf(n.parent.parent, WindowsProductAggregateRollbackLeaf, false)
	if err != nil {
		return nil, err
	}
	defer ReleaseAndJoin(&retErr, f.Close, "close received aggregate rollback")
	for _, name := range []string{productRecoveryJournalLeaf, productPairRecoveryJournalLeaf} {
		if err := rollbackLeafAbsent(n.parent.parent, name); err != nil {
			return nil, err
		}
	}
	return receiveRollbackFile(f, n.identity, capacity)
}

// Serialise this short native effect with release of the same kernel lease.
func (s *WindowsProductStateNamespace) lockAggregateRollbackFence(ctx context.Context, fence *UpgradeFenceLease) (func(), error) {
	if s == nil || fence == nil || fence.namespace != s || fence.bound == nil {
		return nil, errors.New("aggregate rollback requires its original bound Product fence")
	}
	owner := fence.bound
	owner.mu.Lock()
	if err := owner.validateHeldAtLockPathLocked(ctx, owner.path, owner.path); err != nil {
		owner.mu.Unlock()
		return nil, err
	}
	return owner.mu.Unlock, nil
}

// TransferAggregateRollback moves only the received fixed terminal file under
// the same guarded parent. Post-state is observed independently of a late error.
func (s *WindowsProductStateNamespace) TransferAggregateRollback(ctx context.Context, fence *UpgradeFenceLease, raw []byte) (*WindowsProductRollbackContinuation, error) {
	unlock, err := s.lockAggregateRollbackFence(ctx, fence)
	if err != nil {
		return nil, err
	}
	defer unlock()
	if err := s.Validate(ctx); err != nil {
		return nil, err
	}
	n := s.native
	n.mu.Lock()
	defer n.mu.Unlock()
	if err := n.validateLocked(ctx); err != nil {
		return nil, err
	}
	parent := n.parent.parent
	if err := errors.Join(rollbackLeafAbsent(parent, productPairRecoveryJournalLeaf), rollbackLeafAbsent(parent, WindowsProductAggregateRollbackLeaf)); err != nil {
		return nil, err
	}
	f, err := openRollbackLeaf(parent, productRecoveryJournalLeaf, true)
	if err != nil {
		return nil, err
	}
	expected, err := receiveRollbackFile(f, n.identity, int64(len(raw)))
	if err != nil || !bytes.Equal(expected.raw, raw) {
		return nil, errors.Join(errHubRouteStateChanged, err, f.Close())
	}
	err = ntRenameRelativeNoReplace(windows.Handle(f.Fd()), parent, WindowsProductAggregateRollbackLeaf)
	if n.afterRollbackRename != nil {
		err = errors.Join(err, n.afterRollbackRename())
	}
	operationErr := errors.Join(err, f.Close())
	actual, readErr := openRollbackLeaf(parent, WindowsProductAggregateRollbackLeaf, false)
	if readErr != nil {
		return nil, errors.Join(operationErr, readErr)
	}
	post, readErr := receiveRollbackFile(actual, n.identity, int64(len(raw)))
	readErr = errors.Join(readErr, actual.Close(), rollbackLeafAbsent(parent, productRecoveryJournalLeaf), n.validateLocked(ctx))
	if readErr != nil || !expected.matches(post) {
		return nil, errors.Join(errHubRouteStateChanged, operationErr, readErr)
	}
	return expected, operationErr
}

func (s *WindowsProductStateNamespace) VerifyAggregateRollback(ctx context.Context, want *WindowsProductRollbackContinuation) error {
	if want == nil {
		return errHubRouteStateUnavailable
	}
	actual, err := s.ReceiveAggregateRollback(ctx, int64(len(want.raw)))
	if err != nil || !want.matches(actual) {
		return errors.Join(errHubRouteStateChanged, err)
	}
	return nil
}

func (s *WindowsProductStateNamespace) RetireAggregateRollback(ctx context.Context, fence *UpgradeFenceLease, want *WindowsProductRollbackContinuation) error {
	unlock, err := s.lockAggregateRollbackFence(ctx, fence)
	if err != nil {
		return err
	}
	defer unlock()
	if want == nil {
		return errHubRouteStateUnavailable
	}
	if err := s.Validate(ctx); err != nil {
		return err
	}
	n := s.native
	n.mu.Lock()
	defer n.mu.Unlock()
	if err := n.validateLocked(ctx); err != nil {
		return err
	}
	parent := n.parent.parent
	if err := errors.Join(rollbackLeafAbsent(parent, productRecoveryJournalLeaf), rollbackLeafAbsent(parent, productPairRecoveryJournalLeaf)); err != nil {
		return err
	}
	f, err := openRollbackLeaf(parent, WindowsProductAggregateRollbackLeaf, true)
	if err != nil {
		return err
	}
	actual, err := receiveRollbackFile(f, n.identity, int64(len(want.raw)))
	if err != nil || !want.matches(actual) {
		return errors.Join(errHubRouteStateChanged, err, f.Close())
	}
	effect := setFileDeleteOnClose(windows.Handle(f.Fd()))
	closed := f.Close()
	if err := errors.Join(rollbackLeafAbsent(parent, WindowsProductAggregateRollbackLeaf), n.validateLocked(ctx)); err != nil {
		return errors.Join(errHubRouteStateChanged, effect, closed, err)
	}
	if late := errors.Join(effect, closed); late != nil {
		return fmt.Errorf("received aggregate rollback retired and absence confirmed: %w", late)
	}
	return nil
}
