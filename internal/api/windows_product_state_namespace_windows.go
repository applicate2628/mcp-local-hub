package api

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"

	"golang.org/x/sys/windows"
)

type productStateLockGuard struct {
	handle   windows.Handle
	identity ConfigParentDirectoryIdentityV1
}

type productStateNamespaceNative struct {
	mu                     sync.Mutex
	parent                 hubRouteNative
	identity               ConfigParentDirectoryIdentityV1
	stateDir               string
	locks                  map[string]productStateLockGuard
	borrows                int
	closed                 bool
	closeErr, errorPending error
	beforeLockCreate       func(string) error // controlled fixture collision
}

func openProductStateNamespace(ctx context.Context, path string) (*WindowsProductStateNamespace, error) {
	n := &productStateNamespaceNative{locks: make(map[string]productStateLockGuard), stateDir: path}
	s := &WindowsProductStateNamespace{native: n}
	identity, err := n.parent.openParent(ctx, path)
	if err != nil {
		if closeErr := n.close(); closeErr != nil {
			return s, errors.Join(err, closeErr)
		}
		return nil, err
	}
	n.identity = identity
	return s, nil
}

func (n *productStateNamespaceNative) keepLocked(cause error) {
	n.errorPending = errors.Join(n.errorPending, cause)
}
func (n *productStateNamespaceNative) retain(cause error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.keepLocked(cause)
}

func (n *productStateNamespaceNative) close() error {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.closed {
		return n.closeErr
	}
	if n.errorPending != nil {
		return fmt.Errorf("original Product namespace retained after unconfirmed owner release: %w", n.errorPending)
	}
	if n.borrows != 0 {
		return errors.New("original Product namespace still borrowed")
	}
	n.closed = true
	for _, locked := range n.locks {
		n.closeErr = errors.Join(n.closeErr, windows.CloseHandle(locked.handle))
	}
	n.closeErr = errors.Join(n.closeErr, n.parent.close())
	return n.closeErr
}

func (n *productStateNamespaceNative) validate(ctx context.Context) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.validateLocked(ctx)
}

func (n *productStateNamespaceNative) validateLocked(ctx context.Context) error {
	if ctx == nil || n.closed || n.parent.parent == 0 || n.errorPending != nil {
		return errors.Join(errHubRouteStateUnavailable, n.errorPending)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := n.parent.validateParent(); err != nil {
		return err
	}
	id, err := queryConfigParentDirectoryIdentity(n.parent.parent)
	if err != nil || id != n.identity {
		return errors.Join(errHubRouteStateChanged, err)
	}
	for _, guard := range n.locks {
		if err := verifyHubRouteLock(guard.handle); err != nil {
			return err
		}
		id, err := queryConfigParentDirectoryIdentity(guard.handle)
		if err != nil || id != guard.identity {
			return errors.Join(errHubRouteStateChanged, err)
		}
	}
	return nil
}

func (n *productStateNamespaceNative) matchParent(ctx context.Context, path string) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	if err := n.validateLocked(ctx); err != nil {
		return err
	}
	// Prove the caller's resolver selected the physical parent in custody.
	// The observed chain is never substituted for the original effect parent.
	var candidate hubRouteNative
	id, err := candidate.openParent(ctx, path)
	closeErr := candidate.close()
	if closeErr != nil {
		n.keepLocked(closeErr)
	}
	if err != nil || closeErr != nil {
		return errors.Join(err, closeErr)
	}
	if id != n.identity {
		return errHubRouteStateChanged
	}
	return nil
}

func ordinaryProductStateLeafName(name string) bool {
	return singleWindowsPathComponent(name) && !strings.Contains(name, ":") && strings.TrimRight(name, " .") == name
}

func (n *productStateNamespaceNative) openLock(ctx context.Context, path, leaf string, flags int) (*os.File, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	handle, err := n.openLockHandleLocked(ctx, leaf, flags)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(handle), path)
	if file == nil {
		closeErr := windows.CloseHandle(handle)
		if closeErr != nil {
			n.keepLocked(closeErr)
		}
		return nil, errors.Join(errHubRouteStateUnavailable, closeErr)
	}
	return file, nil
}

func (n *productStateNamespaceNative) openLockHandleLocked(ctx context.Context, leaf string, flags int) (held windows.Handle, retErr error) {
	if !ordinaryProductStateLeafName(leaf) || flags & ^(os.O_CREATE|os.O_RDWR|os.O_WRONLY) != 0 {
		return 0, errHubRouteStateUnavailable
	}
	if err := n.validateLocked(ctx); err != nil {
		return 0, err
	}
	parent := n.parent.parent
	access := uint32(windows.GENERIC_READ | windows.FILE_READ_ATTRIBUTES | windows.READ_CONTROL)
	if flags&(os.O_RDWR|os.O_WRONLY) != 0 {
		access |= windows.GENERIC_WRITE
	}
	h, err := ntOpenRelativeWithShareAccess(parent, leaf, access, hubRouteGuardShare)
	first := n.locks[leaf]
	if isNotFoundErr(err) && first.handle == 0 && flags&os.O_CREATE != 0 {
		if n.beforeLockCreate != nil {
			if err := n.beforeLockCreate(leaf); err != nil {
				return 0, err
			}
		}
		sd, err := buildRestrictiveSecurityDescriptor()
		if err != nil {
			return 0, err
		}
		h, err = ntCreateRelativeWithShareAccess(parent, leaf, access|windows.SYNCHRONIZE, windows.FILE_CREATE,
			windows.FILE_NON_DIRECTORY_FILE|windows.FILE_SYNCHRONOUS_IO_NONALERT|windows.FILE_OPEN_REPARSE_POINT, sd, hubRouteGuardShare)
		if isAlreadyExistsErr(err) {
			h, err = ntOpenRelativeWithShareAccess(parent, leaf, access, hubRouteGuardShare)
		}
		if err != nil {
			return 0, err
		}
	} else if err != nil {
		return 0, err
	}
	defer func() {
		if held == 0 {
			closeErr := windows.CloseHandle(h)
			if closeErr != nil {
				n.keepLocked(closeErr)
			}
			retErr = errors.Join(retErr, closeErr)
		}
	}()
	if err := verifyHubRouteLock(h); err != nil {
		return 0, err
	}
	id, err := queryConfigParentDirectoryIdentity(h)
	if err != nil {
		return 0, err
	}
	if id.VolumeSerialNumber != n.identity.VolumeSerialNumber {
		return 0, errHubRouteStateChanged
	}
	if first.handle != 0 {
		if first.identity != id {
			return 0, errHubRouteStateChanged
		}
	} else {
		// While h is still no-delete guarded, retain a read handle for the same
		// object across flock's busy-close/reopen attempts. It takes no byte lock.
		guard, err := ntOpenRelativeWithShareAccess(parent, leaf, windows.GENERIC_READ|windows.READ_CONTROL, hubRouteGuardShare)
		if err != nil {
			return 0, err
		}
		guardID, readErr := queryConfigParentDirectoryIdentity(guard)
		if readErr != nil || guardID != id {
			closeErr := windows.CloseHandle(guard)
			if closeErr != nil {
				n.keepLocked(closeErr)
			}
			return 0, errors.Join(errHubRouteStateChanged, readErr, closeErr)
		}
		n.locks[leaf] = productStateLockGuard{guard, id}
	}
	return h, nil
}

func (n *productStateNamespaceNative) read(ctx context.Context, leaf string) ([]byte, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if !ordinaryProductStateLeafName(leaf) {
		return nil, errHubRouteStateUnavailable
	}
	if err := n.validateLocked(ctx); err != nil {
		return nil, err
	}
	data, err := n.parent.read(leaf, maxStateFileBytes)
	if err != nil {
		return nil, err
	}
	if !data.present {
		return nil, os.ErrNotExist
	}
	if data.identity.VolumeSerialNumber != n.identity.VolumeSerialNumber {
		return nil, errHubRouteStateChanged
	}
	return data.data, ctx.Err()
}

func (n *productStateNamespaceNative) withHub(ctx context.Context, run func(*HubRouteState) error) error {
	n.mu.Lock()
	if err := n.validateLocked(ctx); err != nil {
		n.mu.Unlock()
		return err
	}
	lock, err := n.openLockHandleLocked(ctx, hubMcpLockFileLeaf, os.O_RDWR)
	if err != nil {
		n.mu.Unlock()
		return err
	}
	n.borrows++
	c := &hubRouteStateCore{ctx: ctx, parent: n.identity, stateDir: n.stateDir, native: hubRouteNative{parent: n.parent.parent, guards: n.parent.guards, lock: lock, borrowedParent: true}}
	n.mu.Unlock()
	defer func() { n.mu.Lock(); n.borrows--; n.mu.Unlock() }()
	return runHubRouteState(c, run, n.retain)
}
