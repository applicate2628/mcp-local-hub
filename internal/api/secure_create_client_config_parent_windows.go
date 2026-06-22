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
		// G17 home-anchored leg: keep the per-component verifyDACL posture
		// (every prefix walked is under $HOME — see the documented POSIX/Windows
		// divergence in secure_create_client_config_parent_posix.go). The new
		// `created` verdict is for the volume-root leg's deepest-existing-prefix
		// gate only and is intentionally ignored here.
		nextHandle, _, err := mkdirOrVerifyRealDirWindows(curHandle, comp, nextPath, sd, !skipParentGate)
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
//
// The returned `created` bool is the Windows analog of the POSIX leg's
// mkdirat created-vs-EEXIST verdict (secure_create_parent_anywhere_posix.go):
// true when the component was freshly created via FILE_CREATE, false when it
// already existed (the isAlreadyExistsErr branch). The volume-root-anchored
// caller (secure_create_parent_anywhere_windows.go) uses it to identify the
// deepest existing prefix — the parent of the FIRST freshly-created component
// — so the strict-mode DACL gate fires there exactly ONCE rather than on every
// system-owned ancestor. The home-anchored G17 caller
// (secureCreateClientConfigParentDirImpl) ignores it and keeps its
// per-component verifyDACL posture (every prefix it walks is under $HOME).
func mkdirOrVerifyRealDirWindows(parentHandle windows.Handle, name, full string, sd *windows.SECURITY_DESCRIPTOR, verifyDACL bool) (handle windows.Handle, created bool, err error) {
	if !singleWindowsPathComponent(name) {
		return windows.InvalidHandle, false, fmt.Errorf("secure mkparent: invalid path component %q", name)
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
			return windows.InvalidHandle, false, fmt.Errorf("secure mkparent: ntcreate dir %s: %w", full, err)
		}
		// Existing component: open it relative to the held parent handle
		// (FILE_OPEN_REPARSE_POINT) and refuse a reparse point / non-dir
		// via the SHARED descent step (also used by the AF-1 F1
		// resolved-symlink-target walk in client_write_resolve_windows.go).
		h, err = openExistingRealDirAt(parentHandle, name)
		if err != nil {
			return windows.InvalidHandle, false, fmt.Errorf("secure mkparent: refuse to descend through reparse point / symlink at %s: %w", full, err)
		}
		closeOnErr := true
		defer func() {
			if closeOnErr {
				_ = windows.CloseHandle(h)
			}
		}()
		if verifyDACL {
			if verr := verifyWindowsDACLFromHandle(h); verr != nil {
				return windows.InvalidHandle, false, fmt.Errorf("%w (path %s): %v", ErrSecureWriteParentInsecure, full, verr)
			}
		}
		closeOnErr = false
		return h, false, nil
	}

	// Freshly created via FILE_CREATE: it is a real directory by
	// construction (FILE_DIRECTORY_FILE), so re-assert the reparse/non-dir
	// refusal (defends against a hostile swap in the create window) and
	// verify the DACL if requested.
	closeOnErr := true
	defer func() {
		if closeOnErr {
			_ = windows.CloseHandle(h)
		}
	}()

	if rerr := refuseReparsePointHandle(h); rerr != nil {
		return windows.InvalidHandle, false, fmt.Errorf("secure mkparent: refuse to descend through reparse point / symlink at %s: %w", full, rerr)
	}
	if verifyDACL {
		if verr := verifyWindowsDACLFromHandle(h); verr != nil {
			return windows.InvalidHandle, false, fmt.Errorf("%w (path %s): %v", ErrSecureWriteParentInsecure, full, verr)
		}
	}
	closeOnErr = false
	return h, true, nil
}

// openExistingRealDirAt opens an EXISTING real directory `name` relative to
// the already-held `parentHandle` (FILE_OPEN_REPARSE_POINT so a reparse
// point at the slot is opened as the link itself, then REFUSED, never
// followed), verifies it is a real directory (not a reparse point / file),
// and returns the open handle. It is the single per-component descent step
// shared by
//
//   - mkdirOrVerifyRealDirWindows (G17 parent-create — its EEXIST branch), and
//   - secureWriteThroughResolvedParentHandle's volume-root descent (the
//     AF-1 F1 fix in client_write_resolve_windows.go), which walks an
//     already-existing resolved chain and creates nothing.
//
// It does NOT create, set a DACL, or DACL-verify — it ONLY opens an existing
// component refusing reparse-follow, so the descent never walks through a
// swapped intermediate. Holding the returned handle as the anchor for the
// next component is what closes the intermediate-component re-walk TOCTOU
// (FILE_FLAG_OPEN_REPARSE_POINT on a single path-based open of the whole
// parent string protects only the FINAL component; the object manager
// re-walks every intermediate at open time). On error the (possibly opened)
// handle is closed before returning.
func openExistingRealDirAt(parentHandle windows.Handle, name string) (windows.Handle, error) {
	if !singleWindowsPathComponent(name) {
		return windows.InvalidHandle, fmt.Errorf("invalid path component %q", name)
	}
	h, err := ntOpenDirRelative(parentHandle, name, windows.FILE_LIST_DIRECTORY|windows.FILE_READ_ATTRIBUTES|windows.READ_CONTROL)
	if err != nil {
		return windows.InvalidHandle, err
	}
	if rerr := refuseReparsePointHandle(h); rerr != nil {
		_ = windows.CloseHandle(h)
		return windows.InvalidHandle, rerr
	}
	return h, nil
}

// openTraverseOnlyDirAt opens an EXISTING real directory `name` relative to
// the already-held `parentHandle` TRAVERSE-ONLY for the resolved-symlink
// intermediate-ancestor descent (Finding 1 — the Windows analog of the POSIX
// openSearchOnlyDirAt / O_PATH split). It is the Windows mirror of
// openExistingRealDirAt with one difference: it requests
// FILE_TRAVERSE|FILE_READ_ATTRIBUTES (the minimal access to descend one
// O_NOFOLLOW-relative component and read the reparse/dir attributes) WITHOUT
// FILE_LIST_DIRECTORY, so a consented symlink on a UNC share — or under an
// ancestor that grants TRAVERSE but denies directory LISTING — is still
// reachable. Ordinary Windows path traversal and the old full-parent open
// only ever needed TRAVERSE on ancestors.
//
// The reparse-point refusal is PRESERVED: ntOpenDirRelative opens with
// FILE_OPEN_REPARSE_POINT (so a swapped intermediate junction/symlink is
// opened as the link itself, never followed) and refuseReparsePointHandle
// then REFUSES it via GetFileInformationByHandle (which needs the retained
// FILE_READ_ATTRIBUTES). The F1 intermediate-swap TOCTOU closure is therefore
// unchanged; only the LIST requirement on ancestors is dropped.
//
// It is SEPARATE from openExistingRealDirAt by design: G17's
// mkdirOrVerifyRealDirWindows DACL-verify (verifyWindowsDACLFromHandle ->
// GetSecurityInfo) needs the full LIST/READ_CONTROL handle, so that shared
// step keeps openExistingRealDirAt UNCHANGED; only this resolved-symlink walk
// drops the LIST requirement on ancestors. The FINAL parent of the descent
// still uses openExistingRealDirAt (the normal full open).
func openTraverseOnlyDirAt(parentHandle windows.Handle, name string) (windows.Handle, error) {
	if !singleWindowsPathComponent(name) {
		return windows.InvalidHandle, fmt.Errorf("invalid path component %q", name)
	}
	h, err := ntOpenDirRelative(parentHandle, name, traverseOnlyDirAccess)
	if err != nil {
		return windows.InvalidHandle, err
	}
	if rerr := refuseReparsePointHandle(h); rerr != nil {
		_ = windows.CloseHandle(h)
		return windows.InvalidHandle, rerr
	}
	return h, nil
}

// traverseOnlyDirAccess is the minimal NtCreateFile desired-access mask for a
// traverse-only intermediate-ancestor open in the resolved-symlink descent:
// FILE_TRAVERSE (right to resolve a path component THROUGH the directory) plus
// FILE_READ_ATTRIBUTES (required by GetFileInformationByHandle in
// refuseReparsePointHandle to read the reparse/directory attribute bits).
// FILE_LIST_DIRECTORY (the right to ENUMERATE the directory) is deliberately
// EXCLUDED — the descent never lists, so requiring it would reject a
// traverse-but-no-list ancestor (e.g. on a UNC share). ntOpenDirRelative adds
// SYNCHRONIZE itself.
const traverseOnlyDirAccess = windows.FILE_TRAVERSE | windows.FILE_READ_ATTRIBUTES

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
