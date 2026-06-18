//go:build windows

// internal/api/secure_create_client_config_parent_windows.go
//
// Windows leg of SecureCreateClientConfigParentDir (G17). Creates the
// missing parent-directory chain of a client config COMPONENT-BY-
// COMPONENT, refusing to descend through any reparse point (symlink /
// junction) and refusing any path outside the user home. See the
// package doc in secure_create_client_config_parent.go for the full
// contract.
//
// Windows has no mkdirat equivalent in x/sys, so directories are created
// by absolute path with CreateDirectoryW (which fails with
// ERROR_ALREADY_EXISTS if the name exists — it NEVER follows or replaces
// a reparse point, so a fresh CreateDirectoryW cannot be redirected
// through a symlink). After each create-or-exists, the component is
// re-opened with openDirHandleNoReparse (FILE_FLAG_OPEN_REPARSE_POINT,
// so the open does not auto-follow a reparse point) and verified to be a
// REAL directory (no FILE_ATTRIBUTE_REPARSE_POINT). A reparse point /
// junction / non-directory in the chain is REFUSED — the walk never
// follows it. The restrictive allowlist DACL is installed at create time
// via SecurityAttributes so each new directory is BORN owner-only.

package api

import (
	"fmt"
	"os"
	"path/filepath"
	"unsafe"

	"golang.org/x/sys/windows"
)

func secureCreateClientConfigParentDirImpl(configPath string, skipParentGate bool) error {
	parent := filepath.Dir(configPath)
	if parent == "" || parent == "." {
		return fmt.Errorf("secure mkparent: empty parent for path %q", configPath)
	}
	parent = filepath.Clean(parent)

	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("secure mkparent: resolve home: %w", err)
	}
	home = filepath.Clean(home)
	rel, under := pathUnderHome(home, parent)
	if !under {
		return fmt.Errorf(
			"secure mkparent: refuse to create %q outside the user home %q (init only creates client config dirs under your home)",
			parent, home,
		)
	}

	// Open + verify the home anchor. openDirHandleNoReparse opens with
	// FILE_FLAG_OPEN_REPARSE_POINT so a reparse-point home is opened as
	// the reparse point itself (then rejected by the DACL/reparse checks
	// below) rather than silently followed.
	anchorHandle, err := openDirHandleNoReparse(home)
	if err != nil {
		return fmt.Errorf("secure mkparent: open home anchor %s: %w", home, err)
	}
	if rerr := refuseReparsePointHandle(anchorHandle); rerr != nil {
		windows.CloseHandle(anchorHandle)
		return fmt.Errorf("secure mkparent: home anchor %s: %w", home, rerr)
	}
	if !skipParentGate {
		if verr := verifyWindowsDACLFromHandle(anchorHandle); verr != nil {
			windows.CloseHandle(anchorHandle)
			return fmt.Errorf("%w (path %s): %v", ErrSecureWriteParentInsecure, home, verr)
		}
	}
	windows.CloseHandle(anchorHandle)

	// Build the restrictive SD once; reused as the SecurityAttributes
	// for every CreateDirectoryW so each new dir is born owner-only.
	sd, err := buildRestrictiveSecurityDescriptor()
	if err != nil {
		return fmt.Errorf("secure mkparent: build SD: %w", err)
	}
	sa := &windows.SecurityAttributes{
		SecurityDescriptor: sd,
		InheritHandle:      0,
	}
	sa.Length = uint32(unsafe.Sizeof(*sa))

	// Descend the chain by absolute path, creating each missing
	// component and verifying every component is a real directory.
	cur := home
	for _, comp := range rel {
		cur = filepath.Join(cur, comp)
		if err := mkdirOrVerifyRealDirWindows(cur, sa); err != nil {
			return err
		}
	}
	return nil
}

// mkdirOrVerifyRealDirWindows ensures `full` exists as a real directory
// (not a reparse point). It creates it fresh via CreateDirectoryW with
// the restrictive `sa` (born owner-only) when absent, accepts an
// existing real directory, and REFUSES a reparse point / non-directory.
//
// CreateDirectoryW never follows or replaces a reparse point: on an
// existing entry it returns ERROR_ALREADY_EXISTS without touching it, so
// it cannot be redirected through a planted symlink. The post-create
// reparse-refusal open is what catches a pre-existing or race-planted
// reparse point at the slot.
func mkdirOrVerifyRealDirWindows(full string, sa *windows.SecurityAttributes) error {
	pathW, err := windows.UTF16PtrFromString(full)
	if err != nil {
		return fmt.Errorf("secure mkparent: utf16 %q: %w", full, err)
	}
	createErr := windows.CreateDirectory(pathW, sa)
	if createErr != nil && createErr != windows.ERROR_ALREADY_EXISTS {
		return fmt.Errorf("secure mkparent: CreateDirectory %s: %w", full, createErr)
	}
	// Open WITHOUT following a reparse point and verify it is a real
	// directory. This catches a pre-existing symlink/junction at the
	// slot (createErr == ERROR_ALREADY_EXISTS) and a hostile reparse
	// point race-planted in the create/open window.
	h, err := openDirHandleNoReparse(full)
	if err != nil {
		return fmt.Errorf("secure mkparent: re-open %s: %w", full, err)
	}
	defer windows.CloseHandle(h)
	if rerr := refuseReparsePointHandle(h); rerr != nil {
		return fmt.Errorf("secure mkparent: refuse to descend through reparse point / symlink at %s: %w", full, rerr)
	}
	return nil
}

// refuseReparsePointHandle returns a non-nil error when the open handle
// `h` refers to a reparse point (symlink / junction) or is not a
// directory. Used to refuse following a symlinked component while
// descending the parent chain.
func refuseReparsePointHandle(h windows.Handle) error {
	var fi windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(h, &fi); err != nil {
		return fmt.Errorf("get file info: %w", err)
	}
	if fi.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		return fmt.Errorf("entry is a reparse point")
	}
	if fi.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY == 0 {
		return fmt.Errorf("entry is not a directory")
	}
	return nil
}
