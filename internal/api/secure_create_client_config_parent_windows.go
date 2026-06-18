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
// with NtCreateFile relative to the currently-held verified parent
// handle. Each ObjectName is a single path component; the walk never
// re-resolves an absolute descendant after a prefix has been verified.
// Existing components are opened relative to that same parent handle with
// FILE_OPEN_REPARSE_POINT and verified to be real directories. A reparse
// point / junction / non-directory in the chain is REFUSED — the walk
// never follows it. The restrictive allowlist DACL is installed at create
// time via OBJECT_ATTRIBUTES.SecurityDescriptor so each new directory is
// BORN owner-only.

package api

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/windows"
)

var secureCreateClientConfigParentDirAfterVerifyHook func(verifiedPath string) error

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
	curHandle := anchorHandle
	curPath := home
	defer func() {
		if curHandle != windows.InvalidHandle {
			_ = windows.CloseHandle(curHandle)
		}
	}()

	// Build the restrictive SD once; reused as the create-time security
	// descriptor for every NtCreateFile directory create so each new dir
	// is born owner-only.
	sd, err := buildRestrictiveSecurityDescriptor()
	if err != nil {
		return fmt.Errorf("secure mkparent: build SD: %w", err)
	}

	// Descend the chain handle-relative. curHandle stays live until the
	// next component has been opened and verified, closing the fresh
	// absolute path-walk window between verified prefix N and child N+1.
	for _, comp := range rel {
		nextPath := filepath.Join(curPath, comp)
		nextHandle, err := mkdirOrVerifyRealDirWindows(curHandle, comp, nextPath, sd, !skipParentGate)
		if err != nil {
			return err
		}
		if secureCreateClientConfigParentDirAfterVerifyHook != nil {
			if err := secureCreateClientConfigParentDirAfterVerifyHook(nextPath); err != nil {
				_ = windows.CloseHandle(nextHandle)
				return fmt.Errorf("secure mkparent: post-verify hook %s: %w", nextPath, err)
			}
		}

		oldHandle := curHandle
		curHandle = windows.InvalidHandle
		closeErr := windows.CloseHandle(oldHandle)
		if closeErr != nil {
			_ = windows.CloseHandle(nextHandle)
			return fmt.Errorf("secure mkparent: close verified parent %s: %w", curPath, closeErr)
		}
		curHandle = nextHandle
		curPath = nextPath
	}
	return nil
}

// mkdirOrVerifyRealDirWindows ensures `name` exists below `parentHandle`
// as a real directory (not a reparse point). It creates the component
// fresh via NtCreateFile(FILE_CREATE|FILE_DIRECTORY_FILE) with the
// restrictive create-time security descriptor when absent, or opens an
// existing component relative to the same parent handle with
// FILE_OPEN_REPARSE_POINT before verifying it.
func mkdirOrVerifyRealDirWindows(parentHandle windows.Handle, name, full string, sd *windows.SECURITY_DESCRIPTOR, verifyDACL bool) (windows.Handle, error) {
	if !singleWindowsPathComponent(name) {
		return windows.InvalidHandle, fmt.Errorf("secure mkparent: invalid path component %q", name)
	}

	h, err := ntCreateRelative(
		parentHandle,
		name,
		windows.FILE_LIST_DIRECTORY|windows.FILE_READ_ATTRIBUTES|windows.READ_CONTROL|windows.SYNCHRONIZE,
		windows.FILE_CREATE,
		windows.FILE_DIRECTORY_FILE|windows.FILE_SYNCHRONOUS_IO_NONALERT,
		sd,
	)
	if err != nil {
		if !isAlreadyExistsErr(err) {
			return windows.InvalidHandle, fmt.Errorf("secure mkparent: ntcreate dir %s: %w", full, err)
		}
		h, err = ntOpenDirRelative(parentHandle, name, windows.FILE_LIST_DIRECTORY|windows.FILE_READ_ATTRIBUTES|windows.READ_CONTROL)
		if err != nil {
			return windows.InvalidHandle, fmt.Errorf("secure mkparent: relative open %s: %w", full, err)
		}
	}

	closeOnErr := true
	defer func() {
		if closeOnErr {
			_ = windows.CloseHandle(h)
		}
	}()

	if rerr := refuseReparsePointHandle(h); rerr != nil {
		return windows.InvalidHandle, fmt.Errorf("secure mkparent: refuse to descend through reparse point / symlink at %s: %w", full, rerr)
	}
	if verifyDACL {
		if verr := verifyWindowsDACLFromHandle(h); verr != nil {
			return windows.InvalidHandle, fmt.Errorf("%w (path %s): %v", ErrSecureWriteParentInsecure, full, verr)
		}
	}
	closeOnErr = false
	return h, nil
}

func ntOpenDirRelative(parentHandle windows.Handle, name string, desiredAccess uint32) (windows.Handle, error) {
	return ntCreateRelative(
		parentHandle,
		name,
		desiredAccess|windows.SYNCHRONIZE,
		windows.FILE_OPEN,
		windows.FILE_DIRECTORY_FILE|windows.FILE_SYNCHRONOUS_IO_NONALERT|windows.FILE_OPEN_REPARSE_POINT,
		nil,
	)
}

func singleWindowsPathComponent(name string) bool {
	return name != "" && name != "." && name != ".." && !filepath.IsAbs(name) && !strings.ContainsAny(name, `\/`)
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
