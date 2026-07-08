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
// volume root vs the user home).
//
// STRICT-MODE DACL GATE SCOPE (bot PR #420 r17 finding B1). UNLIKE the
// home-anchored G17 leg — which DACL-verifies EVERY existing component because
// every prefix it walks is UNDER $HOME (operator-owned, where a broadened ACE
// is a real signal) — this leg descends from the VOLUME ROOT through
// SYSTEM-OWNED ancestors the operator does not control (C:\Users grants
// Authenticated Users read; gating it would make every fresh strict-mode
// install fail ErrSecureWriteParentInsecure). So the strict gate fires AT MOST
// ONCE, on the DEEPEST EXISTING PREFIX — the parent of the FIRST freshly-created
// component (or `dir` itself when every component already exists). This MIRRORS
// the POSIX leg's single-gate semantics (secure_create_parent_anywhere_posix.go:
// the `gated` flag fires on the held parent fd at the first created component).
// Freshly-created dirs are born owner-only via the restrictive SD and need no
// post-verify; the deepest existing prefix is the one dir this function does NOT
// create and therefore must trust, so it is the one meaningful gate. The
// per-component REPARSE-POINT REFUSAL (the F1 security fix) is UNCHANGED — every
// component, existing or created, is still refused if it is a symlink/junction.

package api

import (
	"fmt"
	"os"
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
	// deepest-existing-prefix DACL verify needs relative to it). The root carries
	// NO strict gate — it is a system-owned dir whose DACL is neither controlled
	// nor relevant. The "deepest existing prefix" verification (what the removed
	// nearest-existing-ancestor anchor gate did) fires ONCE below, on the parent
	// of the first freshly-created component — NOT on every existing prefix from
	// the root down (bot PR #420 r17 finding B1; mirrors the POSIX leg).
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
	// curIsAnchor tracks POSITION in the descent (a logical fact that cannot
	// alias), NOT a raw handle value (a kernel resource Windows recycles after
	// CloseHandle). It is true ONLY on the first loop iteration — before any
	// descent — when curHandle genuinely IS the volume-root anchor by
	// construction. It is flipped false after the first handle swap and never set
	// back. The root-skip gate below keys on this flag, NOT on
	// `curHandle != anchorHandle`: anchorHandle is CLOSED inside the loop on the
	// first iteration (see the swap below), so a later real directory's curHandle
	// could be handed the recycled numeric value of the now-closed anchorHandle and
	// be MISCLASSIFIED as the root — skipping the strict DACL gate on a non-root
	// deepest-existing-prefix (bot PR #420 r18 HIGH finding: handle-value-reuse
	// gate bypass). The flag mirrors the POSIX leg's identity-stable
	// `curFd != anchorFd` (there anchorFd is held open for the whole function, so
	// the comparison never aliases — Windows must close the anchor mid-loop, so it
	// cannot rely on the handle value and uses position instead).
	curIsAnchor := true
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
	// step the G17 creator uses, with ONE difference: the per-component
	// verifyDACL is passed FALSE here (no per-component gate). The single
	// strict-mode DACL gate fires once, below, on the deepest existing prefix —
	// the held parent of the FIRST freshly-created component (bot PR #420 r17
	// finding B1). The per-component reparse-point refusal is unchanged (it is
	// inside mkdirOrVerifyRealDirWindows regardless of verifyDACL).
	//
	// gated tracks that the single strict-mode parent-dir gate has fired so it
	// runs at most once. skipParentGate seeds it true (skip lane: gate already
	// satisfied). Mirrors the POSIX leg's `gated` flag.
	gated := skipParentGate
	for _, comp := range parts {
		nextPath := filepath.Join(curPath, comp)
		nextHandle, created, err := mkdirOrVerifyRealDirWindows(curHandle, comp, nextPath, sd, false)
		if err != nil {
			return err
		}

		// First freshly-created component: curHandle (its PARENT, still held) is
		// the deepest existing prefix and must be DACL-gated in strict mode before
		// we trust it as the create root. Verify it ONCE here. Skip ONLY when
		// curHandle is the volume-root anchor (curIsAnchor — true exclusively on the
		// first iteration, before any descent) — the root is a system-owned dir
		// whose DACL is neither operator-controlled nor meaningful (gating C:\ would
		// re-introduce the very fail-every-install bug B1 fixes). The skip keys on
		// the position flag, NOT `curHandle != anchorHandle`, because anchorHandle is
		// closed below on the first iteration and its recycled numeric value could
		// otherwise alias a real deepest-existing-prefix into a wrongful root-skip
		// (bot PR #420 r18 HIGH finding). On a gate FAILURE, REMOVE the just-created
		// child (nextPath) before returning so a strict refusal leaves NOTHING
		// created under the rejected prefix — matching the home-anchored G17 leg's
		// no-child-on-refusal guarantee. Only this iteration's child was created
		// (deeper components are not reached), so removing nextPath is a complete
		// cleanup.
		if created && !gated && !curIsAnchor {
			if verr := verifyWindowsDACLFromHandle(curHandle); verr != nil {
				_ = windows.CloseHandle(nextHandle)
				// Remove the just-created empty owner-only child so the refusal leaves
				// nothing behind. The dir is empty (just created) and owner-only, so an
				// absolute RemoveDirectory is safe here; a residual cleanup failure is
				// surfaced but does not mask the gate refusal.
				if rmErr := os.Remove(nextPath); rmErr != nil {
					return fmt.Errorf("%w; ALSO failed to remove the just-created child %s: %v", wrapParentGateRefusal(curPath, verr), nextPath, rmErr)
				}
				return wrapParentGateRefusal(curPath, verr)
			}
		}
		if created {
			// Whether or not the prefix needed a (root-skipped) verify, the gate
			// for the chain has now been resolved at the deepest existing prefix —
			// no later component can be a deeper existing prefix.
			gated = true
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
		// Past the first iteration curHandle is a DESCENDED directory, never the
		// volume-root anchor. Flip the position flag so every deeper
		// deepest-existing-prefix is gated (handle-value reuse is now irrelevant —
		// the decision no longer compares handle values).
		curIsAnchor = false
	}

	// `dir` fully existed (nothing created) and strict mode requested but the
	// gate never fired: gate the final held handle, which is `dir` itself — its
	// own owner-only posture is the deepest existing prefix in that all-exists
	// case. No anchor-skip term is needed here: `parts` is non-empty (the
	// len(parts)==0 all-root case returned at line 71 before the anchor was even
	// opened), so the loop ran at least once and swapped curHandle off the anchor —
	// the held handle here is ALWAYS a descended directory, never the volume root.
	// (The old `curHandle != anchorHandle` term was both unreachable-when-true and,
	// worse, exposed to the same handle-value-reuse aliasing as the in-loop gate.)
	// Mirrors the POSIX leg's trailing !gated gate.
	if !gated {
		if verr := verifyWindowsDACLFromHandle(curHandle); verr != nil {
			return wrapParentGateRefusal(curPath, verr)
		}
	}
	return nil
}
