//go:build windows

// internal/api/secure_create_parent_anywhere_windows.go
//
// Windows leg of SecureCreateParentDirForConfigLock (bot PR #420 finding 1 +
// its residual F1). Creates the missing parent-directory chain of a client
// config WRITE TARGET COMPONENT-BY-COMPONENT, descending from the VOLUME ROOT
// (drive root "C:\" or UNC share root "\\server\share") refusing to descend
// through any reparse point (symlink / junction) at EVERY component (NOT the
// user home — see the package doc in secure_create_parent_anywhere.go for why
// the home-containment bound is dropped here; NOT the nearest existing ancestor
// re-opened by absolute path — see the F1-residual note in that same package doc
// for why an absolute-path anchor re-open followed intermediate reparse points).
//
// It reuses the SAME single-owner per-component create-or-open step
// (mkdirOrVerifyRealDirWindows from secure_create_client_config_parent_windows.go)
// plus the create-time restrictive SD (buildRestrictiveSecurityDescriptor) as
// the G17 home-bounded creator, so the NtCreateFile + FILE_OPEN_REPARSE_POINT-
// refusing walk is NOT re-implemented — only the trust anchor differs (the
// volume root vs the user home). The strict-mode DACL gate is threaded into
// mkdirOrVerifyRealDirWindows (verifyDACL = !skipParentGate) for EVERY component
// so every EXISTING prefix from the volume root down is DACL-verified (the
// documented Windows posture — see secure_create_client_config_parent_posix.go's
// package doc on the intentional POSIX/Windows divergence); freshly-created dirs
// are born owner-only via the restrictive SD and need no post-verify.

package api

import (
	"fmt"
	"path/filepath"
	"strings"

	"golang.org/x/sys/windows"
)

func secureCreateParentDirAnywhereImpl(dir string, skipParentGate bool) error {
	cleaned := filepath.Clean(dir)
	if cleaned == "" || cleaned == "." {
		return fmt.Errorf("secure mkparent: empty dir")
	}

	vol := filepath.VolumeName(cleaned)
	if vol == "" {
		return fmt.Errorf("secure mkparent: dir %q has no volume name (not an absolute drive/UNC path)", dir)
	}
	// Every directory component below the volume root, INCLUDING the final one
	// (`dir` is the directory to create, not a file whose base is dropped — this
	// is why decomposeResolvedParentWindows, which drops the base, is NOT reused
	// here). Empty components are dropped by FieldsFunc.
	rest := strings.TrimPrefix(cleaned, vol)
	rest = strings.TrimPrefix(rest, `\`)
	rest = strings.TrimPrefix(rest, `/`)
	parts := strings.FieldsFunc(rest, func(r rune) bool { return r == '\\' || r == '/' })
	if len(parts) == 0 {
		// The target IS the volume root — pathological for a config parent dir;
		// nothing to create and no operator-owned prefix to gate.
		return fmt.Errorf("secure mkparent: dir %q is the volume root; nothing to create", dir)
	}

	// Volume-root anchor: the drive root "C:\" or the UNC share root
	// "\\server\share". openDirHandleNoReparse opens with
	// FILE_FLAG_OPEN_REPARSE_POINT (LIST|READ_CONTROL — the fuller handle the
	// first existing component's per-component DACL verify needs relative to it).
	// The root carries NO strict gate — it is a system-owned dir whose DACL is
	// neither controlled nor relevant; the "deepest existing prefix"
	// verification (what the removed nearest-existing-ancestor anchor gate did)
	// moves INTO the per-component mkdirOrVerifyRealDirWindows(verifyDACL) for
	// every existing prefix below.
	anchorPath := vol + `\`
	anchorHandle, err := openDirHandleNoReparse(anchorPath)
	if err != nil {
		return fmt.Errorf("secure mkparent: open volume-root anchor %s: %w", anchorPath, err)
	}
	if rerr := refuseReparsePointHandle(anchorHandle); rerr != nil {
		windows.CloseHandle(anchorHandle)
		return fmt.Errorf("secure mkparent: volume-root anchor %s: %w", anchorPath, rerr)
	}

	curHandle := anchorHandle
	curPath := anchorPath
	defer func() {
		if curHandle != windows.InvalidHandle {
			_ = windows.CloseHandle(curHandle)
		}
	}()

	// Build the restrictive SD once; reused as the create-time security
	// descriptor for every NtCreateFile directory create so each new dir is born
	// owner-only.
	sd, err := buildRestrictiveSecurityDescriptor()
	if err != nil {
		return fmt.Errorf("secure mkparent: build SD: %w", err)
	}

	// Descend every component handle-relative. curHandle stays live until the
	// next component has been opened and verified, closing the fresh absolute
	// path-walk window between verified prefix N and child N+1. Identical descent
	// step the G17 creator uses; verifyDACL = !skipParentGate verifies every
	// EXISTING prefix's DACL (freshly-created dirs are born owner-only via sd).
	for _, comp := range parts {
		nextPath := filepath.Join(curPath, comp)
		nextHandle, err := mkdirOrVerifyRealDirWindows(curHandle, comp, nextPath, sd, !skipParentGate)
		if err != nil {
			return err
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
