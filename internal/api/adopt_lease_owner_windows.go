//go:build windows

package api

import (
	"errors"
	"fmt"

	"golang.org/x/sys/windows"
)

type windowsAdoptLease struct {
	namespace windows.Handle
	handle    windows.Handle
	leaf      string
	identity  windows.ByHandleFileInformation
}

var adoptLeaseWindowsFailureHook func(stage string) error

func injectedWindowsAdoptLeaseFailure(stage string) error {
	if adoptLeaseWindowsFailureHook == nil {
		return nil
	}
	if err := adoptLeaseWindowsFailureHook(stage); err != nil {
		return fmt.Errorf("adopt lease: %s failed: %w", stage, err)
	}
	return nil
}

// acquireAdoptManifestLeasePlatform owns the complete namespace protocol:
// lock the verified guard first, then open and lock the manifest slot.
func acquireAdoptManifestLeasePlatform(manifestName string) (adoptManifestLeasePlatform, bool, error) {
	ns, err := openWindowsAdoptLeaseNamespace()
	if err != nil {
		var namespaceFailure *LeaseNamespaceFailure
		if errors.As(err, &namespaceFailure) {
			return nil, false, newLeaseNamespaceFailure(namespaceFailure.ReasonID, namespaceFailure.Action, err)
		}
		return nil, false, newLeaseNamespaceFailure(AdoptLeaseReasonNamespaceUnrecognized, AdoptLeaseActionInspect, err)
	}
	guard, _, err := openOrCreateWindowsAdoptLeaseLeaf(ns, adoptLeaseNamespaceLockLeaf)
	if err != nil {
		return nil, false, leaseCleanupFailure(err, windows.CloseHandle(ns))
	}
	guardLocked, err := lockWindowsAdoptLease(guard, true)
	err = errors.Join(err, injectedWindowsAdoptLeaseFailure("guard-lock"))
	if err != nil {
		var unlockErr error
		if guardLocked {
			unlockErr = unlockWindowsAdoptLease(guard)
		}
		return nil, false, leaseCleanupFailure(err, unlockErr, windows.CloseHandle(guard), windows.CloseHandle(ns))
	}
	if !guardLocked {
		if cleanup := errors.Join(windows.CloseHandle(guard), windows.CloseHandle(ns)); cleanup != nil {
			return nil, false, leaseCleanupFailure(cleanup)
		}
		return nil, false, newLeaseFailure(adoptLeaseFailureGuardBusy, true, false)
	}
	guardCleanup := func() error {
		return errors.Join(
			unlockWindowsAdoptLease(guard),
			injectedWindowsAdoptLeaseFailure("guard-unlock"),
			windows.CloseHandle(guard),
			injectedWindowsAdoptLeaseFailure("guard-close"),
		)
	}
	if adoptLeaseBeforeSlotOpenHook != nil {
		if err := adoptLeaseBeforeSlotOpenHook(); err != nil {
			return nil, false, leaseCleanupFailure(err, guardCleanup(), windows.CloseHandle(ns))
		}
	}
	leaf := manifestName + adoptManifestLeaseSuffix
	h, _, err := openOrCreateWindowsAdoptLeaseLeaf(ns, leaf)
	if err != nil {
		return nil, false, leaseCleanupFailure(err, guardCleanup(), windows.CloseHandle(ns))
	}
	locked, err := lockWindowsAdoptLease(h, true)
	err = errors.Join(err, injectedWindowsAdoptLeaseFailure("leaf-lock"))
	if err != nil {
		var unlockErr error
		if locked {
			unlockErr = unlockWindowsAdoptLease(h)
		}
		return nil, false, leaseCleanupFailure(err, unlockErr, windows.CloseHandle(h), guardCleanup(), windows.CloseHandle(ns))
	}
	if !locked {
		if cleanup := errors.Join(windows.CloseHandle(h), guardCleanup(), windows.CloseHandle(ns)); cleanup != nil {
			return nil, false, leaseCleanupFailure(cleanup)
		}
		return nil, false, newLeaseFailure(adoptLeaseFailureBusy, true, false)
	}
	var identity windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(h, &identity); err != nil {
		return nil, false, leaseCleanupFailure(err, unlockWindowsAdoptLease(h), windows.CloseHandle(h), guardCleanup(), windows.CloseHandle(ns))
	}
	if err := guardCleanup(); err != nil {
		return nil, false, leaseCleanupFailure(err, unlockWindowsAdoptLease(h), windows.CloseHandle(h), windows.CloseHandle(ns))
	}
	return &windowsAdoptLease{namespace: ns, handle: h, leaf: leaf, identity: identity}, true, nil
}

func openWindowsAdoptLeaseNamespace() (windows.Handle, error) {
	stateDir, err := DaemonStateDir()
	if err != nil {
		return windows.InvalidHandle, newLeaseNamespaceOperationFailure(AdoptLeaseReasonStateRootUnavailable, AdoptLeaseActionLeaveUnchanged, err)
	}
	root, err := openDirHandleNoReparse(stateDir)
	if err != nil {
		return windows.InvalidHandle, newLeaseNamespaceOperationFailure(AdoptLeaseReasonStateRootRefused, AdoptLeaseActionLeaveUnchanged, err)
	}
	if err := refuseReparsePointHandle(root); err != nil {
		return windows.InvalidHandle, newLeaseNamespaceOperationFailure(AdoptLeaseReasonStateRootRefused, AdoptLeaseActionLeaveUnchanged, errors.Join(err, windows.CloseHandle(root)))
	}
	if err := verifyWindowsDACLFromHandle(root); err != nil {
		return windows.InvalidHandle, newLeaseNamespaceOperationFailure(AdoptLeaseReasonStateRootRefused, AdoptLeaseActionLeaveUnchanged, errors.Join(err, windows.CloseHandle(root)))
	}
	sd, err := buildRestrictiveSecurityDescriptor()
	if err != nil {
		return windows.InvalidHandle, newLeaseNamespaceOperationFailure(AdoptLeaseReasonNamespaceCreateRefused, AdoptLeaseActionInspect, errors.Join(err, windows.CloseHandle(root)))
	}
	ns, _, err := mkdirOrVerifyRealDirWindows(root, adoptProvenanceSnapshotSubdir, "adopt-provenance", sd, true)
	if err != nil {
		reason, action := classifyWindowsAdoptLeaseNamespaceRefusal(root)
		return windows.InvalidHandle, newLeaseNamespaceOperationFailure(reason, action, errors.Join(err, windows.CloseHandle(root)))
	}
	closeErr := windows.CloseHandle(root)
	if closeErr != nil {
		return windows.InvalidHandle, errors.Join(fmt.Errorf("adopt lease namespace: state root close failed"), windows.CloseHandle(ns), closeErr)
	}
	if err := injectedWindowsAdoptLeaseFailure("namespace-open"); err != nil {
		return windows.InvalidHandle, errors.Join(err, windows.CloseHandle(ns))
	}
	return ns, nil
}

func openOrCreateWindowsAdoptLeaseLeaf(namespace windows.Handle, leaf string) (windows.Handle, bool, error) {
	if !singleWindowsPathComponent(leaf) {
		return windows.InvalidHandle, false, fmt.Errorf("adopt lease: invalid leaf")
	}
	openStage, verifyStage := "leaf-open", "leaf-verify"
	if leaf == adoptLeaseNamespaceLockLeaf {
		openStage, verifyStage = "guard-open", "guard-verify"
	}
	sd, err := buildRestrictiveSecurityDescriptor()
	if err != nil {
		return windows.InvalidHandle, false, fmt.Errorf("adopt lease: security descriptor unavailable")
	}
	access := uint32(windows.DELETE | windows.GENERIC_WRITE | windows.SYNCHRONIZE | windows.WRITE_DAC | windows.READ_CONTROL | windows.FILE_READ_ATTRIBUTES)
	h, err := ntCreateRelative(namespace, leaf, access, windows.FILE_CREATE, windows.FILE_NON_DIRECTORY_FILE|windows.FILE_SYNCHRONOUS_IO_NONALERT, sd)
	created := err == nil
	if err != nil {
		if !isAlreadyExistsErr(err) {
			return windows.InvalidHandle, false, fmt.Errorf("adopt lease: leaf open denied: %w", err)
		}
		h, err = ntOpenRelative(namespace, leaf, access)
		if err != nil {
			return windows.InvalidHandle, false, fmt.Errorf("adopt lease: leaf open denied: %w", err)
		}
	}
	if err := injectedWindowsAdoptLeaseFailure(openStage); err != nil {
		return windows.InvalidHandle, false, errors.Join(err, windows.CloseHandle(h))
	}
	if err := verifyWindowsAdoptLeaseLeaf(h); err != nil {
		return windows.InvalidHandle, false, errors.Join(err, windows.CloseHandle(h))
	}
	if err := injectedWindowsAdoptLeaseFailure(verifyStage); err != nil {
		return windows.InvalidHandle, false, errors.Join(err, windows.CloseHandle(h))
	}
	return h, created, nil
}

func openWindowsExistingAdoptLeaseLeaf(namespace windows.Handle, leaf string) (windows.Handle, error) {
	if !singleWindowsPathComponent(leaf) {
		return windows.InvalidHandle, fmt.Errorf("adopt lease: invalid leaf")
	}
	access := uint32(windows.DELETE | windows.GENERIC_WRITE | windows.SYNCHRONIZE | windows.WRITE_DAC | windows.READ_CONTROL | windows.FILE_READ_ATTRIBUTES)
	h, err := ntOpenRelative(namespace, leaf, access)
	if err != nil {
		return windows.InvalidHandle, fmt.Errorf("adopt lease: leaf open denied: %w", err)
	}
	if err := injectedWindowsAdoptLeaseFailure("leaf-open"); err != nil {
		return windows.InvalidHandle, errors.Join(err, windows.CloseHandle(h))
	}
	if err := verifyWindowsAdoptLeaseLeaf(h); err != nil {
		return windows.InvalidHandle, errors.Join(err, windows.CloseHandle(h))
	}
	if err := injectedWindowsAdoptLeaseFailure("leaf-verify"); err != nil {
		return windows.InvalidHandle, errors.Join(err, windows.CloseHandle(h))
	}
	return h, nil
}

func verifyWindowsAdoptLeaseLeaf(h windows.Handle) error {
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(h, &info); err != nil {
		return fmt.Errorf("adopt lease: leaf metadata unavailable")
	}
	if info.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 || info.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY != 0 || info.NumberOfLinks != 1 {
		return fmt.Errorf("adopt lease: leaf identity refused")
	}
	if err := verifyWindowsDACLFromHandle(h); err != nil {
		return fmt.Errorf("adopt lease: leaf access denied")
	}
	return nil
}

func lockWindowsAdoptLease(h windows.Handle, nonBlocking bool) (bool, error) {
	flags := uint32(windows.LOCKFILE_EXCLUSIVE_LOCK)
	if nonBlocking {
		flags |= windows.LOCKFILE_FAIL_IMMEDIATELY
	}
	var overlapped windows.Overlapped
	if err := windows.LockFileEx(h, flags, 0, 1, 0, &overlapped); err != nil {
		if nonBlocking && (errors.Is(err, windows.ERROR_LOCK_VIOLATION) || errors.Is(err, windows.ERROR_IO_PENDING)) {
			return false, nil
		}
		return false, fmt.Errorf("adopt lease: lock failed")
	}
	return true, nil
}

func unlockWindowsAdoptLease(h windows.Handle) error {
	var overlapped windows.Overlapped
	if err := windows.UnlockFileEx(h, 0, 1, 0, &overlapped); err != nil {
		return fmt.Errorf("adopt lease: unlock failed")
	}
	return nil
}

func (l *windowsAdoptLease) unlock() error {
	if l == nil || l.handle == windows.InvalidHandle {
		return nil
	}
	h, ns := l.handle, l.namespace
	l.handle, l.namespace = windows.InvalidHandle, windows.InvalidHandle
	return errors.Join(unlockWindowsAdoptLease(h), windows.CloseHandle(h), windows.CloseHandle(ns), injectedAdoptLeaseUnlockFailure())
}

func sameWindowsAdoptLeaseIdentity(left, right windows.ByHandleFileInformation) bool {
	return left.VolumeSerialNumber == right.VolumeSerialNumber && left.FileIndexHigh == right.FileIndexHigh && left.FileIndexLow == right.FileIndexLow
}

func (l *windowsAdoptLease) releaseAndRemove() error {
	if l == nil || l.handle == windows.InvalidHandle || l.namespace == windows.InvalidHandle {
		return leaseCleanupFailure(fmt.Errorf("adopt lease: missing settlement handle"))
	}
	guard, _, err := openOrCreateWindowsAdoptLeaseLeaf(l.namespace, adoptLeaseNamespaceLockLeaf)
	if err != nil {
		return leaseCleanupFailure(err, l.unlock())
	}
	guardLocked, err := lockWindowsAdoptLease(guard, true)
	err = errors.Join(err, injectedWindowsAdoptLeaseFailure("guard-lock"))
	if err != nil {
		var unlockErr error
		if guardLocked {
			unlockErr = unlockWindowsAdoptLease(guard)
		}
		return leaseCleanupFailure(err, unlockErr, windows.CloseHandle(guard), l.unlock())
	}
	if !guardLocked {
		if cleanup := errors.Join(windows.CloseHandle(guard), l.unlock()); cleanup != nil {
			return leaseCleanupFailure(cleanup)
		}
		return newLeaseFailure(adoptLeaseFailureGuardBusy, true, false)
	}
	guardCleanup := func() error {
		return errors.Join(
			unlockWindowsAdoptLease(guard),
			injectedWindowsAdoptLeaseFailure("guard-unlock"),
			windows.CloseHandle(guard),
			injectedWindowsAdoptLeaseFailure("guard-close"),
		)
	}
	var primary error
	if adoptLeaseBeforeSettlementHook != nil {
		primary = adoptLeaseBeforeSettlementHook()
	}
	current, openErr := openWindowsExistingAdoptLeaseLeaf(l.namespace, l.leaf)
	if openErr != nil {
		// Settlement never creates the canonical slot. A missing, substituted, or
		// no-longer-verifiable name is a non-cooperating namespace replacement;
		// preserve it and surface the recovery-required discriminator.
		primary = errors.Join(primary, newLeaseFailure(adoptLeaseFailureSlotReplaced, false, true, openErr))
	} else {
		var identity windows.ByHandleFileInformation
		if err := windows.GetFileInformationByHandle(current, &identity); err != nil || !sameWindowsAdoptLeaseIdentity(l.identity, identity) {
			primary = errors.Join(primary, newLeaseFailure(adoptLeaseFailureSlotReplaced, false, true, err))
		}
		primary = errors.Join(primary, windows.CloseHandle(current))
	}
	// The delete disposition always targets the retained original handle. On a
	// slot replacement the canonical name belongs to a foreign object, so the
	// original must still be disposed on close while the replacement survives.
	deleteMarkErr := errors.Join(setFileDeleteOnClose(l.handle), injectedWindowsAdoptLeaseFailure("delete-mark"))
	h := l.handle
	l.handle = windows.InvalidHandle
	cleanup := errors.Join(
		deleteMarkErr,
		unlockWindowsAdoptLease(h),
		injectedWindowsAdoptLeaseFailure("lease-unlock"),
		windows.CloseHandle(h),
		injectedWindowsAdoptLeaseFailure("close"),
	)
	if primary == nil && cleanup == nil {
		if adoptLeaseBeforeFinalReadbackHook != nil {
			cleanup = errors.Join(cleanup, adoptLeaseBeforeFinalReadbackHook())
		}
		if current, err := ntOpenRelative(l.namespace, l.leaf, windows.FILE_READ_ATTRIBUTES); err == nil {
			cleanup = errors.Join(cleanup, windows.CloseHandle(current), newLeaseFailure(adoptLeaseFailureSlotReplaced, false, true))
		} else if !isNotFoundErr(err) {
			cleanup = errors.Join(cleanup, err)
		}
		cleanup = errors.Join(cleanup, injectedWindowsAdoptLeaseFailure("absence-readback"))
	}
	guardErr := guardCleanup()
	ns := l.namespace
	l.namespace = windows.InvalidHandle
	nsErr := windows.CloseHandle(ns)
	result := errors.Join(primary, cleanup, guardErr, nsErr)
	if result != nil {
		if hasLeaseFailureID(result, adoptLeaseFailureSlotReplaced) {
			return newLeaseFailure(adoptLeaseFailureSlotReplaced, false, true, result)
		}
		return leaseCleanupFailure(result)
	}
	return nil
}
