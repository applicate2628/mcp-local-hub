//go:build !windows

// internal/api/secure_create_client_config_parent_posix.go
//
// POSIX leg of SecureCreateClientConfigParentDir (G17). Creates the
// missing parent-directory chain of a client config COMPONENT-BY-
// COMPONENT, anchored fd-relative to the user home, refusing to follow
// any symlink and refusing any path outside the home. See the package
// doc in secure_create_client_config_parent.go for the full contract.
//
// On POSIX the load-bearing security boundary is the inode mode bits:
// mkdirat with mode 0700 (subject to umask, then re-asserted via fchmod)
// makes each created directory owner-only regardless of how loose the
// home is. O_NOFOLLOW on every open refuses a symlinked component
// atomically at the kernel, and mkdirat fails with EEXIST if the name
// already exists (so a race-planted entry cannot be silently adopted —
// the re-stat then classifies it).
//
// INTENTIONAL POSIX/Windows DIVERGENCE (by design — do NOT "fix" by
// adding per-prefix verification here):
//
// In strict mode the POSIX leg DACL/mode-gates the HOME ANCHOR only
// (verifyPosixParentDirFromFd on anchorFd below). It does NOT re-verify
// the mode of each EXISTING intermediate prefix it descends through:
// mkdirOrOpenRealDirAt re-opens an existing component with O_NOFOLLOW
// (a symlink/non-dir is still refused) but does not fchmod-check its
// mode. The Windows leg, by contrast, DACL-verifies EVERY existing
// prefix handle before it becomes the RootDirectory for the next
// component (see TestSecureCreateClientConfigParentDir_WindowsStrict-
// RefusesBroadenedExistingPrefix).
//
// Why this is SAFE on POSIX and NOT a missing check:
//
//   - The security boundary is the CREATED directory's own 0700 mode.
//     Every directory THIS function creates is mkdirat(0700) +
//     fchmod(0700), so it denies group/world regardless of a loose
//     ancestor. A broadened EXISTING intermediate (e.g. ~/.config at
//     0755) does not weaken the owner-only stub directory created
//     beneath it — POSIX permission checks are per-inode, not inherited
//     like a Windows DACL, so a loose ancestor grants no access to a
//     0700 child.
//   - An existing intermediate under $HOME that an attacker could have
//     broadened is already INSIDE the user's home trust boundary. A
//     co-resident principal who can chmod a directory under another
//     user's $HOME has already breached that home; the per-prefix mode
//     check would not be the control that stops them. (The robust
//     control for a genuinely-untrusted multi-tenant $HOME is
//     MCPHUB_REQUIRE_SINGLE_USER_HOME on the anchor, which IS enforced.)
//   - Windows has no per-inode owner-only equivalent: a child folder
//     inherits the parent DACL unless PROTECTED_DACL is set at create
//     AND no broadening ACE was inherited beforehand, so the parent's
//     ACL genuinely matters there — hence Windows verifies every prefix.
//     The asymmetry mirrors the OS security models, it is not an
//     accidental gap. (Behavior locked by
//     TestSecureCreateClientConfigParentDir_PosixStrictAllowsBroadened-
//     ExistingPrefix.)

package api

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
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
	rel, under := pathUnderHome(home, parent)
	if !under {
		return fmt.Errorf(
			"secure mkparent: refuse to create %q outside the user home %q (init only creates client config dirs under your home)",
			parent, home,
		)
	}

	// Open the home anchor with O_NOFOLLOW|O_DIRECTORY so a symlinked
	// home component is refused and a non-dir home fails fast.
	anchorFd, err := unix.Open(home, unix.O_DIRECTORY|unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return fmt.Errorf("secure mkparent: open home anchor %s: %w", home, err)
	}
	defer unix.Close(anchorFd)

	// Strict gate: when enforced, the home anchor must be owner-only.
	// Bypassed on the relax lane (skipParentGate=true); created dirs
	// stay 0700 regardless.
	if !skipParentGate {
		if err := verifyPosixParentDirFromFd(anchorFd); err != nil {
			return fmt.Errorf("%w (path %s): %v", ErrSecureWriteParentInsecure, home, err)
		}
	}

	// Descend the chain fd-relative, creating each missing component.
	// curFd is always an open fd on the current real directory; each
	// step either creates `comp` fresh or re-opens an existing real dir.
	curFd := anchorFd
	closeCur := func() {
		if curFd != anchorFd {
			_ = unix.Close(curFd)
		}
	}
	curPath := home
	for _, comp := range rel {
		nextPath := filepath.Join(curPath, comp)
		nextFd, err := mkdirOrOpenRealDirAt(curFd, comp, nextPath)
		closeCur()
		if err != nil {
			return err
		}
		curFd = nextFd
		curPath = nextPath
	}
	// curFd is the (now-existing) target parent. Close it (unless it is
	// the anchor — i.e. parent == home, which means parent already
	// existed and there was nothing to create).
	closeCur()
	return nil
}

// mkdirOrOpenRealDirAt ensures `comp` exists as a real directory under
// `dirFd` and returns an open O_NOFOLLOW|O_DIRECTORY fd on it. Either:
//
//   - mkdirat creates it fresh (mode 0700, fchmod-reasserted), or
//   - mkdirat returns EEXIST and the existing entry is a REAL directory
//     (re-opened with O_NOFOLLOW so a symlink is refused).
//
// A pre-existing symlink / reparse-point / non-directory at `comp` is
// REFUSED — the open with O_NOFOLLOW fails (ELOOP/ENOTDIR) and the error
// is surfaced; the walk never follows it.
//
// DELIBERATELY no per-prefix DACL/mode re-verification for an EXISTING
// directory (the mkErr==EEXIST branch only re-opens O_NOFOLLOW; it does
// not fchmod-check the existing mode). This is the intentional
// POSIX/Windows divergence documented in this file's package doc: on
// POSIX the created 0700 child is the boundary, so a broadened existing
// ancestor under $HOME does not weaken it. Do NOT add a strict per-prefix
// mode check here to "match Windows" — see the package doc for why the
// asymmetry mirrors the two OS security models.
//
// `fullPath` is only used for diagnostics.
func mkdirOrOpenRealDirAt(dirFd int, comp, fullPath string) (int, error) {
	mkErr := unix.Mkdirat(dirFd, comp, 0o700)
	if mkErr != nil && !errors.Is(mkErr, unix.EEXIST) {
		return -1, fmt.Errorf("secure mkparent: mkdirat %s: %w", fullPath, mkErr)
	}
	// Open the component fd-relative with O_NOFOLLOW so a symlink at
	// this slot (whether pre-existing or race-planted between mkdirat
	// EEXIST and here) is refused, and O_DIRECTORY so a non-dir fails.
	// openExistingRealDirAt is the SHARED descent step (also used by the
	// AF-1 F1 resolved-symlink-target walk in
	// client_write_resolve_posix.go); the open semantics are identical.
	fd, openErr := openExistingRealDirAt(dirFd, comp)
	if openErr != nil {
		if mkErr == nil {
			// We created it but cannot re-open as a real dir — anomalous
			// (a hostile swap in the microsecond window). Surface loud.
			return -1, fmt.Errorf(
				"secure mkparent: created %s but re-open as real dir failed (possible concurrent swap): %w",
				fullPath, openErr,
			)
		}
		// EEXIST path: the existing entry is not a followable real
		// directory (symlink → ELOOP, non-dir → ENOTDIR). Refuse.
		return -1, fmt.Errorf(
			"secure mkparent: refuse to descend through non-directory or symlink at %s: %w",
			fullPath, openErr,
		)
	}
	if mkErr == nil {
		// Freshly created — re-assert 0700 against umask drift.
		if err := unix.Fchmod(fd, 0o700); err != nil {
			_ = unix.Close(fd)
			return -1, fmt.Errorf("secure mkparent: fchmod %s: %w", fullPath, err)
		}
	}
	return fd, nil
}

// openExistingRealDirAt opens an EXISTING real directory `comp`
// relative to the already-held `dirFd`, O_NOFOLLOW so a symlink at the
// slot is refused at the kernel (ELOOP) and O_DIRECTORY so a non-dir
// fails (ENOTDIR). It is the single per-component descent step shared by
//
//   - mkdirOrOpenRealDirAt (G17 parent-create — after its mkdirat), and
//   - secureWriteThroughResolvedParentHandle's volume-root descent (the
//     AF-1 F1 fix in client_write_resolve_posix.go), which walks an
//     already-existing resolved chain and creates nothing.
//
// It does NOT mkdir, fchmod, or DACL/mode-verify — it ONLY opens an
// existing component refusing symlink-follow, so the descent never walks
// through a swapped intermediate. Holding the returned fd as the anchor
// for the next component is what closes the intermediate-component
// re-walk TOCTOU (O_NOFOLLOW on a single path-based open of the whole
// parent string protects only the FINAL component; the kernel re-walks
// every intermediate at open time).
func openExistingRealDirAt(dirFd int, comp string) (int, error) {
	return unix.Openat(dirFd, comp, unix.O_DIRECTORY|unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
}
