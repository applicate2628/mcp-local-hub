package api

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"
)

type restoreMissingBinaryAsideOps struct {
	link   func(string, string) error
	rename func(string, string) error
	remove func(string) error
}

func restoreMissingBinaryAsideWithOps(aside, target string, ops restoreMissingBinaryAsideOps) error {
	if ops.link == nil {
		ops.link = os.Link
	}
	if ops.rename == nil {
		ops.rename = os.Rename
	}
	if ops.remove == nil {
		ops.remove = os.Remove
	}

	if err := ops.link(aside, target); err == nil {
		// The target is now restored (hard-linked to the aside's inode). Removing
		// the consumed aside is BEST-EFFORT: a failure here leaves a harmless
		// .old-<ts> (same inode, swept later by SweepOldBinaries) and must NOT
		// fail the restore.
		_ = ops.remove(aside)
		return nil
	} else if !isRestoreFallbackError(err) {
		return err
	}

	if err := ensureRestoreTargetAbsent(target); err != nil {
		return err
	}
	if err := ops.rename(aside, target); err == nil {
		return nil
	} else if !isRestoreFallbackError(err) {
		return err
	}

	if err := copyFileExclusiveFsync(aside, target); err != nil {
		return err
	}
	_ = ops.remove(aside)
	return nil
}

func ensureRestoreTargetAbsent(target string) error {
	if _, err := os.Stat(target); err == nil {
		return fmt.Errorf("restore missing binary: target already exists: %s", target)
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("stat restore target %q: %w", target, err)
	}
	return nil
}

func isRestoreFallbackError(err error) bool {
	return errors.Is(err, syscall.EXDEV) ||
		errors.Is(err, syscall.EOPNOTSUPP) ||
		errors.Is(err, syscall.ENOTSUP) ||
		errors.Is(err, syscall.ENOSYS) ||
		errors.Is(err, syscall.EPERM)
}

func copyFileExclusiveFsync(srcPath, dstPath string) error {
	src, err := os.Open(srcPath)
	if err != nil {
		return fmt.Errorf("open restore aside %q: %w", srcPath, err)
	}
	defer src.Close()

	info, err := src.Stat()
	if err != nil {
		return fmt.Errorf("stat restore aside %q: %w", srcPath, err)
	}
	dst, err := os.OpenFile(dstPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, info.Mode().Perm())
	if err != nil {
		return fmt.Errorf("create restore target %q: %w", dstPath, err)
	}
	cleanupDst := true
	defer func() {
		if cleanupDst {
			_ = os.Remove(dstPath)
		}
	}()

	if _, err := io.Copy(dst, src); err != nil {
		_ = dst.Close()
		return fmt.Errorf("copy restore aside %q to %q: %w", srcPath, dstPath, err)
	}
	if err := dst.Sync(); err != nil {
		_ = dst.Close()
		return fmt.Errorf("fsync restore target %q: %w", dstPath, err)
	}
	if err := dst.Close(); err != nil {
		return fmt.Errorf("close restore target %q: %w", dstPath, err)
	}
	cleanupDst = false
	syncDirBestEffort(filepath.Dir(dstPath))
	return nil
}

func syncDirBestEffort(dir string) {
	d, err := os.Open(dir)
	if err != nil {
		return
	}
	defer d.Close()
	_ = d.Sync()
}
