//go:build !windows

package api

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

func readSecretsInitRecoveryDirectory(root string) ([]os.DirEntry, error) {
	return readSecretsInitRecoveryDirectoryAfterValidation(root, nil)
}

func readSecretsInitRecoveryDirectoryAfterValidation(root string, afterValidation func(*os.File) error) (entries []os.DirEntry, err error) {
	if root == "" || !filepath.IsAbs(root) {
		return nil, errors.New("invalid recovery directory root")
	}
	fd, err := unix.Open(root, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("open recovery directory: %w", err)
	}
	rawOwned := true
	defer func() {
		if rawOwned {
			if closeErr := unix.Close(fd); closeErr != nil {
				err = errors.Join(err, fmt.Errorf("close recovery directory descriptor: %w", closeErr))
			}
			if err != nil {
				entries = nil
			}
		}
	}()
	if err = verifyPosixParentDirFromFd(fd); err != nil {
		return nil, fmt.Errorf("recovery directory protection: %w", err)
	}
	file := os.NewFile(uintptr(fd), root)
	if file == nil {
		return nil, errors.New("recovery directory file conversion failed")
	}
	rawOwned = false
	defer func() {
		if closeErr := file.Close(); closeErr != nil {
			err = errors.Join(err, fmt.Errorf("close recovery directory file: %w", closeErr))
		}
		if err != nil {
			entries = nil
		}
	}()
	if afterValidation != nil {
		if err = afterValidation(file); err != nil {
			return nil, err
		}
	}
	entries, err = file.ReadDir(-1)
	if err != nil {
		return nil, fmt.Errorf("enumerate recovery directory: %w", err)
	}
	return entries, nil
}
