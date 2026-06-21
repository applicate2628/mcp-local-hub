package api

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
)

func restoreMissingBinaryAside(aside, target string) error {
	temp := target + ".restore-tmp"
	if err := ensureRestoreTargetAbsent(target); err != nil {
		return err
	}
	if err := copyFileExclusiveFsync(aside, temp); err != nil {
		_ = os.Remove(temp)
		return err
	}
	if err := ensureRestoreTargetAbsent(target); err != nil {
		_ = os.Remove(temp)
		return err
	}
	if err := os.Rename(temp, target); err != nil {
		_ = os.Remove(temp)
		return fmt.Errorf("rename restore temp %q to target %q: %w", temp, target, err)
	}
	syncDirBestEffort(filepath.Dir(target))
	_ = os.Remove(aside)
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
	dst, err := os.OpenFile(dstPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o700)
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
	if err := dst.Chmod(info.Mode().Perm()); err != nil {
		_ = dst.Close()
		return fmt.Errorf("chmod restore target %q: %w", dstPath, err)
	}
	if err := dst.Sync(); err != nil {
		_ = dst.Close()
		return fmt.Errorf("fsync restore target %q: %w", dstPath, err)
	}
	if err := dst.Close(); err != nil {
		return fmt.Errorf("close restore target %q: %w", dstPath, err)
	}
	cleanupDst = false
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
