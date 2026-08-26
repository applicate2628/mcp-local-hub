//go:build !windows

package api

import (
	"errors"
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

type posixAdoptLease struct {
	namespace, handle int
	leaf              string
	dev, ino          uint64
}

// acquireAdoptManifestLeasePlatform serializes cooperators through the
// persistent, owner-only namespace guard before it opens a manifest slot.
func acquireAdoptManifestLeasePlatform(manifest string) (adoptManifestLeasePlatform, bool, error) {
	ns, err := openPosixAdoptLeaseNamespace()
	if err != nil {
		return nil, false, newLeaseFailure(adoptLeaseFailureNamespaceRefused, false, false, err)
	}
	guard, err := openOrCreatePosixAdoptLeaseLeaf(ns, adoptLeaseNamespaceLockLeaf)
	if err != nil {
		return nil, false, leaseCleanupFailure(err, unix.Close(ns))
	}
	guardLocked, err := lockPosixAdoptLease(guard, true)
	if err != nil {
		return nil, false, leaseCleanupFailure(err, unix.Close(guard), unix.Close(ns))
	}
	if !guardLocked {
		if cleanup := errors.Join(unix.Close(guard), unix.Close(ns)); cleanup != nil {
			return nil, false, leaseCleanupFailure(cleanup)
		}
		return nil, false, newLeaseFailure(adoptLeaseFailureGuardBusy, true, false)
	}
	guardCleanup := func() error { return errors.Join(unlockPosixAdoptLease(guard), unix.Close(guard)) }
	if adoptLeaseBeforeSlotOpenHook != nil {
		if err := adoptLeaseBeforeSlotOpenHook(); err != nil {
			return nil, false, leaseCleanupFailure(err, guardCleanup(), unix.Close(ns))
		}
	}
	leaf := manifest + adoptManifestLeaseSuffix
	h, err := openOrCreatePosixAdoptLeaseLeaf(ns, leaf)
	if err != nil {
		return nil, false, leaseCleanupFailure(err, guardCleanup(), unix.Close(ns))
	}
	locked, err := lockPosixAdoptLease(h, true)
	if err != nil {
		return nil, false, leaseCleanupFailure(err, unix.Close(h), guardCleanup(), unix.Close(ns))
	}
	if !locked {
		if cleanup := errors.Join(unix.Close(h), guardCleanup(), unix.Close(ns)); cleanup != nil {
			return nil, false, leaseCleanupFailure(cleanup)
		}
		return nil, false, newLeaseFailure(adoptLeaseFailureBusy, true, false)
	}
	var st unix.Stat_t
	if err := unix.Fstat(h, &st); err != nil {
		return nil, false, leaseCleanupFailure(err, unlockPosixAdoptLease(h), unix.Close(h), guardCleanup(), unix.Close(ns))
	}
	if err := guardCleanup(); err != nil {
		return nil, false, leaseCleanupFailure(err, unlockPosixAdoptLease(h), unix.Close(h), unix.Close(ns))
	}
	return &posixAdoptLease{namespace: ns, handle: h, leaf: leaf, dev: uint64(st.Dev), ino: st.Ino}, true, nil
}

func openPosixAdoptLeaseNamespace() (int, error) {
	root, err := DaemonStateDir()
	if err != nil {
		return -1, fmt.Errorf("adopt lease namespace: state root unavailable")
	}
	rfd, err := unix.Open(root, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return -1, fmt.Errorf("adopt lease namespace: state root denied")
	}
	if err := verifyPosixAdoptLeaseDir(rfd); err != nil {
		return -1, errors.Join(err, unix.Close(rfd))
	}
	if err := unix.Mkdirat(rfd, adoptProvenanceSnapshotSubdir, 0o700); err != nil && !errors.Is(err, unix.EEXIST) {
		return -1, errors.Join(fmt.Errorf("adopt lease namespace: create failed"), err, unix.Close(rfd))
	}
	ns, err := unix.Openat(rfd, adoptProvenanceSnapshotSubdir, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	closeErr := unix.Close(rfd)
	if err != nil {
		return -1, errors.Join(fmt.Errorf("adopt lease namespace: open failed"), err, closeErr)
	}
	if err := verifyPosixAdoptLeaseDir(ns); err != nil {
		return -1, errors.Join(err, unix.Close(ns), closeErr)
	}
	if closeErr != nil {
		return -1, errors.Join(fmt.Errorf("adopt lease namespace: state root close failed"), unix.Close(ns), closeErr)
	}
	return ns, nil
}

func verifyPosixAdoptLeaseDir(fd int) error {
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil || st.Mode&unix.S_IFMT != unix.S_IFDIR || int(st.Uid) != os.Getuid() || st.Mode&0o077 != 0 {
		return fmt.Errorf("adopt lease namespace: directory refused")
	}
	return nil
}

func openOrCreatePosixAdoptLeaseLeaf(ns int, leaf string) (int, error) {
	fd, err := unix.Openat(ns, leaf, unix.O_RDWR|unix.O_CREAT|unix.O_EXCL|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0o600)
	if errors.Is(err, unix.EEXIST) {
		fd, err = unix.Openat(ns, leaf, unix.O_RDWR|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	}
	if err != nil {
		return -1, fmt.Errorf("adopt lease: leaf open denied")
	}
	if err := verifyPosixAdoptLeaseLeaf(fd); err != nil {
		return -1, errors.Join(err, unix.Close(fd))
	}
	return fd, nil
}

func openPosixExistingAdoptLeaseLeaf(ns int, leaf string) (int, error) {
	fd, err := unix.Openat(ns, leaf, unix.O_RDWR|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return -1, fmt.Errorf("adopt lease: leaf open denied")
	}
	if err := verifyPosixAdoptLeaseLeaf(fd); err != nil {
		return -1, errors.Join(err, unix.Close(fd))
	}
	return fd, nil
}

func verifyPosixAdoptLeaseLeaf(fd int) error {
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil || st.Mode&unix.S_IFMT != unix.S_IFREG || st.Nlink != 1 || int(st.Uid) != os.Getuid() || st.Mode&0o077 != 0 {
		return fmt.Errorf("adopt lease: leaf identity refused")
	}
	return nil
}

func lockPosixAdoptLease(fd int, nonBlocking bool) (bool, error) {
	mode := unix.LOCK_EX
	if nonBlocking {
		mode |= unix.LOCK_NB
	}
	if err := unix.Flock(fd, mode); err != nil {
		if nonBlocking && (errors.Is(err, unix.EWOULDBLOCK) || errors.Is(err, unix.EAGAIN)) {
			return false, nil
		}
		return false, fmt.Errorf("adopt lease: lock failed")
	}
	return true, nil
}

func unlockPosixAdoptLease(fd int) error { return unix.Flock(fd, unix.LOCK_UN) }

func (l *posixAdoptLease) unlock() error {
	if l == nil || l.handle < 0 {
		return nil
	}
	h, ns := l.handle, l.namespace
	l.handle, l.namespace = -1, -1
	return errors.Join(unlockPosixAdoptLease(h), unix.Close(h), unix.Close(ns), injectedAdoptLeaseUnlockFailure())
}

func (l *posixAdoptLease) releaseAndRemove() error {
	if l == nil || l.handle < 0 || l.namespace < 0 {
		return leaseCleanupFailure(fmt.Errorf("adopt lease: missing settlement handle"))
	}
	guard, err := openPosixExistingAdoptLeaseLeaf(l.namespace, adoptLeaseNamespaceLockLeaf)
	if err != nil {
		return leaseCleanupFailure(err, l.unlock())
	}
	guardLocked, err := lockPosixAdoptLease(guard, true)
	if err != nil {
		return leaseCleanupFailure(err, unix.Close(guard), l.unlock())
	}
	if !guardLocked {
		if cleanup := errors.Join(unix.Close(guard), l.unlock()); cleanup != nil {
			return leaseCleanupFailure(cleanup)
		}
		return newLeaseFailure(adoptLeaseFailureGuardBusy, true, false)
	}
	guardCleanup := func() error { return errors.Join(unlockPosixAdoptLease(guard), unix.Close(guard)) }
	var primary error
	if adoptLeaseBeforeSettlementHook != nil {
		primary = adoptLeaseBeforeSettlementHook()
	}
	current, openErr := openPosixExistingAdoptLeaseLeaf(l.namespace, l.leaf)
	if openErr != nil {
		// POSIX has no descriptor-bound unlink primitive. A canonical-name open
		// failure therefore means replacement/compromise, never permission to
		// touch the name by unlink or rename.
		primary = errors.Join(primary, newLeaseFailure(adoptLeaseFailureSlotReplaced, false, true, openErr))
	} else {
		var st unix.Stat_t
		if err := unix.Fstat(current, &st); err != nil || uint64(st.Dev) != l.dev || st.Ino != l.ino {
			primary = errors.Join(primary, newLeaseFailure(adoptLeaseFailureSlotReplaced, false, true, err))
		}
		primary = errors.Join(primary, unix.Close(current))
	}
	h := l.handle
	l.handle = -1
	cleanup := errors.Join(unlockPosixAdoptLease(h), unix.Close(h))
	if primary == nil && cleanup == nil {
		if adoptLeaseBeforeFinalReadbackHook != nil {
			cleanup = errors.Join(cleanup, adoptLeaseBeforeFinalReadbackHook())
		}
		current, err := openPosixExistingAdoptLeaseLeaf(l.namespace, l.leaf)
		if err != nil {
			cleanup = errors.Join(cleanup, newLeaseFailure(adoptLeaseFailureSlotReplaced, false, true, err))
		} else {
			var st unix.Stat_t
			if err := unix.Fstat(current, &st); err != nil || uint64(st.Dev) != l.dev || st.Ino != l.ino {
				cleanup = errors.Join(cleanup, newLeaseFailure(adoptLeaseFailureSlotReplaced, false, true, err))
			}
			cleanup = errors.Join(cleanup, unix.Close(current))
		}
	}
	guardErr := guardCleanup()
	ns := l.namespace
	l.namespace = -1
	nsErr := unix.Close(ns)
	result := errors.Join(primary, cleanup, guardErr, nsErr)
	if result != nil {
		if hasLeaseFailureID(result, adoptLeaseFailureSlotReplaced) {
			return newLeaseFailure(adoptLeaseFailureSlotReplaced, false, true, result)
		}
		return leaseCleanupFailure(result)
	}
	return nil
}
