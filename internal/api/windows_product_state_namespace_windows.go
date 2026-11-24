package api

import (
	"context"
	"errors"
	"fmt"
	"io"
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

type productBoundFileSnapshot struct {
	identity   ConfigParentDirectoryIdentityV1
	extent     int64
	attributes uint32
	created    windows.Filetime
	written    windows.Filetime
}

func snapshotProductBoundFile(h windows.Handle) (productBoundFileSnapshot, error) {
	info, err := hubRouteFileInformation(h)
	if err != nil {
		return productBoundFileSnapshot{}, err
	}
	if err := verifyWindowsDACLFromHandle(h); err != nil {
		return productBoundFileSnapshot{}, err
	}
	identity, err := queryConfigParentDirectoryIdentity(h)
	if err != nil {
		return productBoundFileSnapshot{}, err
	}
	extent := int64(info.FileSizeHigh)<<32 | int64(info.FileSizeLow)
	if extent < 0 {
		return productBoundFileSnapshot{}, errHubRouteStateUnavailable
	}
	return productBoundFileSnapshot{identity, extent, info.FileAttributes, info.CreationTime, info.LastWriteTime}, nil
}

func (n *productStateNamespaceNative) withBoundFileReader(ctx context.Context, leaf string, read func(io.Reader) error) (retErr error) {
	if !ordinaryProductStateLeafName(leaf) {
		return errHubRouteStateUnavailable
	}
	n.mu.Lock()
	if err := n.validateLocked(ctx); err != nil {
		n.mu.Unlock()
		return err
	}
	parent, volume := n.parent.parent, n.identity.VolumeSerialNumber
	n.borrows++
	n.mu.Unlock()
	var closeErr error
	defer func() { retErr = n.finishBoundFileRead(ctx, retErr, closeErr) }()

	const access = windows.GENERIC_READ | windows.READ_CONTROL
	const share = windows.FILE_SHARE_READ
	handle, err := ntOpenRelativeWithShareAccess(parent, leaf, access, share)
	if isNotFoundErr(err) {
		return os.ErrNotExist
	}
	if err != nil {
		return err
	}
	file := os.NewFile(uintptr(handle), leaf)
	if file == nil {
		closeErr = windows.CloseHandle(handle)
		return errHubRouteStateUnavailable
	}
	defer func() { closeErr = errors.Join(closeErr, file.Close()) }()
	original, err := snapshotProductBoundFile(handle)
	if err != nil {
		return err
	}
	if original.identity.VolumeSerialNumber != volume {
		return errHubRouteStateChanged
	}

	if err := readProductBoundFile(ctx, file, original.extent, read); err != nil {
		return err
	}
	if err := n.validate(ctx); err != nil {
		return err
	}
	// Reopen the exact name relative to the same guarded parent while the
	// original file still denies writes and deletion.
	fresh, err := ntOpenRelativeWithShareAccess(parent, leaf, access, share)
	if err != nil {
		return err
	}
	defer func() { closeErr = errors.Join(closeErr, windows.CloseHandle(fresh)) }()
	actual, err := snapshotProductBoundFile(fresh)
	if err != nil {
		return err
	}
	if actual != original {
		return errHubRouteStateChanged
	}
	return nil
}

func (n *productStateNamespaceNative) finishBoundFileRead(ctx context.Context, readErr, closeErr error) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	if closeErr != nil {
		n.keepLocked(closeErr)
	}
	n.borrows--
	return errors.Join(readErr, closeErr, ctx.Err())
}

type productBoundFileReader struct {
	mu     sync.Mutex
	ctx    context.Context
	source *io.LimitedReader
	err    error
}

func (r *productBoundFileReader) Read(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.source == nil {
		return 0, io.ErrClosedPipe
	}
	if r.err != nil {
		return 0, r.err
	}
	if err := r.ctx.Err(); err != nil {
		r.err = err
		return 0, err
	}
	n, err := r.source.Read(p)
	if err != io.EOF {
		r.err = err
	}
	return n, err
}

func readProductBoundFile(ctx context.Context, source io.Reader, extent int64, read func(io.Reader) error) (retErr error) {
	if err := ctx.Err(); err != nil {
		return err
	}
	limited := &io.LimitedReader{R: source, N: extent}
	reader := &productBoundFileReader{ctx: ctx, source: limited}
	defer func() {
		reader.mu.Lock()
		defer reader.mu.Unlock()
		reader.source = nil
		retErr = errors.Join(retErr, reader.err, ctx.Err())
		if retErr == nil && limited.N != 0 {
			retErr = io.ErrUnexpectedEOF
		}
	}()
	return read(reader)
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
