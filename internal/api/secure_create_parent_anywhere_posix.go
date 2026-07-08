//go:build !windows

// internal/api/secure_create_parent_anywhere_posix.go
//
// POSIX leg of SecureCreateParentDirForConfigLock (bot PR #420 finding 1 + its
// residual F1). Creates the missing parent-directory chain of a client config
// WRITE TARGET COMPONENT-BY-COMPONENT, descending from the VOLUME ROOT "/"
// (NOT the user home — see the package doc in secure_create_parent_anywhere.go
// for why the home-containment bound is dropped here; NOT the nearest existing
// ancestor re-opened by absolute path — see the F1-residual note in that same
// package doc for why an absolute-path anchor re-open followed intermediate
// symlinks), refusing to follow any symlink at EVERY component.
//
// It reuses the SAME single-owner per-component create-or-open primitives
// (unix.Mkdirat + openExistingRealDirAt from
// secure_create_client_config_parent_posix.go — the two steps that
// mkdirOrOpenRealDirAt itself composes) as the G17 home-bounded creator, so the
// symlink-refusing mkdirat-0700 + O_NOFOLLOW open walk is NOT re-implemented —
// only the trust anchor differs (the volume root vs the user home). They are
// composed directly here (rather than calling mkdirOrOpenRealDirAt) ONLY so the
// existed-vs-created verdict can be surfaced for the single strict-mode gate;
// the create/open/fchmod semantics are byte-identical to mkdirOrOpenRealDirAt.

package api

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

func secureCreateParentDirAnywhereImpl(dir string, skipParentGate bool) error {
	cleaned := filepath.Clean(dir)
	if cleaned == "" || cleaned == "." {
		return fmt.Errorf("secure mkparent: empty dir")
	}
	if !filepath.IsAbs(cleaned) {
		return fmt.Errorf("secure mkparent: dir %q is not absolute; cannot descend from volume root", dir)
	}

	// Split into components below the volume root. The LAST component IS part of
	// the descent (`dir` is the directory to create, not a file whose base is
	// dropped). Empty / "." entries are skipped defensively (filepath.Clean
	// already collapses them).
	rel := strings.TrimPrefix(cleaned, "/")
	parts := strings.Split(rel, "/")

	// Volume-root anchor. O_NOFOLLOW|O_DIRECTORY keep the descent uniform (the
	// root is never a symlink). The root carries no strict gate — the "deepest
	// existing prefix" verification (what the removed nearest-existing-ancestor
	// anchor used to gate) moves INTO the descent: the held parent fd at the
	// moment of the FIRST created component is gated once below. Plain read fd
	// (NOT the resolve-lane O_PATH search-only split) because this creator MUST
	// mkdirat into the deepest existing prefix and fchmod fresh dirs, exactly as
	// the G17 creator does from its home anchor.
	anchorFd, err := unix.Open("/", unix.O_DIRECTORY|unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return fmt.Errorf("secure mkparent: open volume-root anchor /: %w", err)
	}
	defer unix.Close(anchorFd)

	// Descend every component fd-relative, creating each missing one. curFd is
	// always an open fd on the current real directory. gated tracks that the
	// single strict-mode parent-dir gate has fired so it runs at most once, on
	// the deepest existing prefix (the held parent fd of the first created
	// component). When `dir` already fully exists, no component is created and
	// the gate fires on `dir` itself's deepest existing parent — i.e. the last
	// existing prefix before the (already-existing) target, preserving the old
	// "gate the nearest existing ancestor" semantics.
	curFd := anchorFd
	closeCur := func() {
		if curFd != anchorFd {
			_ = unix.Close(curFd)
		}
	}
	curPath := "/"
	gated := skipParentGate // skip lane: treat the gate as already satisfied.

	for _, comp := range parts {
		if comp == "" || comp == "." {
			continue
		}
		nextPath := curPath
		if curPath == "/" {
			nextPath = "/" + comp
		} else {
			nextPath = curPath + "/" + comp
		}

		// mkdirat IS the atomic existence probe + create. EEXIST → the component
		// already exists (existing prefix); nil → freshly created (so curFd, its
		// parent, is the deepest existing prefix and must be gated before we
		// trust it as the create root).
		mkErr := unix.Mkdirat(curFd, comp, 0o700)
		created := mkErr == nil
		if mkErr != nil && !errors.Is(mkErr, unix.EEXIST) {
			closeCur()
			return fmt.Errorf("secure mkparent: mkdirat %s: %w", nextPath, mkErr)
		}

		if created && !gated {
			// curFd is the deepest existing prefix (parent of the first created
			// component). Strict gate: it must be owner-only. This is the single
			// gate that replaces the removed nearest-existing-ancestor anchor
			// gate; created dirs are 0700 by construction so only this existing
			// prefix needs verifying (the documented POSIX divergence — see
			// secure_create_client_config_parent_posix.go's package doc).
			if verr := verifyPosixParentDirFromFd(curFd); verr != nil {
				closeCur()
				return wrapParentGateRefusal(curPath, verr)
			}
			gated = true
		}

		// Open the component fd-relative O_NOFOLLOW so a symlink at this slot
		// (pre-existing OR race-planted between mkdirat-EEXIST and here) is
		// refused (ELOOP) and a non-dir fails (ENOTDIR). Same shared descent step
		// the G17 creator uses.
		nextFd, openErr := openExistingRealDirAt(curFd, comp)
		if openErr != nil {
			closeCur()
			if created {
				// Created it but cannot re-open as a real dir — a hostile swap in
				// the microsecond window. Surface loud.
				return fmt.Errorf(
					"secure mkparent: created %s but re-open as real dir failed (possible concurrent swap): %w",
					nextPath, openErr,
				)
			}
			// EEXIST path: the existing entry is a symlink (ELOOP) or non-dir
			// (ENOTDIR). Refuse — never follow it.
			return fmt.Errorf(
				"secure mkparent: refuse to descend through non-directory or symlink at %s: %w",
				nextPath, openErr,
			)
		}
		if created {
			// Freshly created — re-assert 0700 against umask drift (matches
			// mkdirOrOpenRealDirAt).
			if cherr := unix.Fchmod(nextFd, 0o700); cherr != nil {
				_ = unix.Close(nextFd)
				closeCur()
				return fmt.Errorf("secure mkparent: fchmod %s: %w", nextPath, cherr)
			}
		}
		closeCur()
		curFd = nextFd
		curPath = nextPath
	}

	// `dir` fully existed (nothing created) and strict mode requested but the
	// gate never fired (no first-created component): gate the final held fd,
	// which is `dir` itself — its own owner-only posture is the deepest existing
	// prefix in that all-exists case. (Mirrors the old impl, which gated the
	// nearest existing ancestor = `dir` when `dir` already existed.)
	if !gated {
		if verr := verifyPosixParentDirFromFd(curFd); verr != nil {
			closeCur()
			return wrapParentGateRefusal(curPath, verr)
		}
	}
	closeCur()
	return nil
}
