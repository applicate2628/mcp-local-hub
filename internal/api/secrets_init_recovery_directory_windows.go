//go:build windows

package api

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/windows"
)

// readSecretsInitRecoveryDirectory enumerates the existing directory object
// whose type and strict protection were checked, including an empty directory.
func readSecretsInitRecoveryDirectory(root string) ([]os.DirEntry, error) {
	return readSecretsInitRecoveryDirectoryAfterValidation(root, nil)
}

// The per-call callback is a private deterministic test seam. It runs only
// after all production checks and handle ownership transfer; production always
// supplies nil. It cannot bypass or substitute the strict validators.
func readSecretsInitRecoveryDirectoryAfterValidation(root string, afterValidation func(*os.File) error) (entries []os.DirEntry, err error) {
	if root == "" || !filepath.IsAbs(root) {
		return nil, errors.New("invalid recovery directory root")
	}
	h, err := openDirHandleNoReparse(root)
	if err != nil {
		return nil, fmt.Errorf("open recovery directory: %w", err)
	}
	rawOwned := true
	defer func() {
		if rawOwned {
			if closeErr := windows.CloseHandle(h); closeErr != nil {
				err = errors.Join(err, fmt.Errorf("close recovery directory handle: %w", closeErr))
			}
			if err != nil {
				entries = nil
			}
		}
	}()
	if err = refuseReparsePointHandle(h); err != nil {
		return nil, fmt.Errorf("recovery directory type: %w", err)
	}
	if err = verifyWindowsDACLFromHandle(h); err != nil {
		return nil, fmt.Errorf("recovery directory protection: %w", err)
	}
	file := os.NewFile(uintptr(h), root)
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
