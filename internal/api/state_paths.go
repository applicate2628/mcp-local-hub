// Package api — Task 1 cross-platform state-directory resolver
// (watchdog plan v13 §15, §16, §17).
//
// This file is the platform-shared core for the watchdog state files
// (daemon-intent.json, watchdog-state.json, intent-audit.log,
// watchdog.log). It owns:
//
//   - The exported entry points DaemonStateDir / OpenStateFile.
//   - Shared error sentinels (errKnownFolderUnavailable,
//     errStateParentInsecure, errStateNameInvalid).
//   - The POSIX resolver posixStateDir + sanity-check.
//   - The package-level test seam knownFolderResolverFn that the
//     production Windows resolver in state_paths_windows.go writes
//     to during init.
//
// The actual daemonStateDir() function has TWO variants gated by the
// test_state_path_env build tag:
//
//   - state_paths_prod.go            (//go:build !test_state_path_env) — production
//   - state_paths_envfallback.go     (//go:build test_state_path_env)  — test build with env fallback
//
// Production binary therefore CANNOT compile the env fallback (compile-
// time guarantee per plan §16). Tests run with -tags=test_state_path_env
// so the fallback variant is selected.
//
// Per §15: POSIX dir is created at 0700 + explicit Chmod (defense vs
// umask drift); state files use OpenStateFile with O_CREATE|O_EXCL and
// pre-create at 0600 + post-open Chmod. Sanity check after MkdirAll
// stat()s the parent and rejects mode & 0o022 != 0 (group/world
// writable) OR Uid mismatch (non-owned) — both surface as
// errStateParentInsecure so the watchdog CLI can translate to exit 8.
package api

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// stateDirName is the leaf folder under the per-user app-data root
// that holds every watchdog state file. Kept as a single constant so
// later tasks (2, 3, 7, 9) reference one canonical literal.
const stateDirName = "mcp-local-hub"

// marketplaceCacheFileLeaf is the G5 Marketplace cache file. Joins
// the validateStateFileName allowlist (single-component, no path
// separators). Reads/writes route through readHubMcpStateFile /
// writeHubMcpStateFile so the G4 hardening pipeline (atomic tempfile
// + rename + post-rename DACL re-verify) applies unchanged.
const marketplaceCacheFileLeaf = "marketplace-cache.json"

// ---------------------------------------------------------------------
// Error sentinels.
// ---------------------------------------------------------------------

// errKnownFolderUnavailable is returned by daemonStateDir when the
// Windows SHGetKnownFolderPath API fails AND no test-build env
// fallback is compiled in. The watchdog CLI translates this into
// exit code 8 per plan §16 v9 (production fail-closed contract).
var errKnownFolderUnavailable = errors.New("api: SHGetKnownFolderPath(FOLDERID_LocalAppData) unavailable; production build refuses env fallback")

// errStateParentInsecure is returned when the POSIX sanity check
// rejects the state-dir parent — either world/group writable
// (mode & 0o022 != 0) or owned by a different UID. The watchdog CLI
// translates this into exit code 8 per plan §16 v9.
var errStateParentInsecure = errors.New("api: state-dir parent has insecure permissions or non-owner UID")

// errStateNameInvalid is returned by OpenStateFile when the requested
// file name contains path separators, parent-dir traversal segments,
// drive letters, or any other shape that could escape the owned state
// root. Defense-in-depth — production callers pass hardcoded names
// (daemon-intent.json, watchdog.log, ...).
var errStateNameInvalid = errors.New("api: state file name must be a single path component without traversal segments")

// ---------------------------------------------------------------------
// Test seams.
// ---------------------------------------------------------------------

// knownFolderResolverFn returns the Windows LocalAppData path. Set in
// state_paths_windows.go's init() to the real
// windows.KnownFolderPath wrapper; nil on non-Windows. Tests overwrite
// it via installKnownFolderStub to inject deterministic results
// without spinning up a real Win32 call.
var knownFolderResolverFn func() (string, error)

// daemonStateRootOverride, when non-empty, replaces the platform
// resolver entirely — DaemonStateDir / OpenStateFile use this value
// as the state root. Reserved for future test paths that need to
// bypass platform resolution; production code never sets it.
var daemonStateRootOverride string

// ---------------------------------------------------------------------
// Public API.
// ---------------------------------------------------------------------

// DaemonStateDir returns the absolute path to the per-user state
// directory used by every watchdog component. Creates the directory
// at 0700 (POSIX) on first call and asserts the parent's permissions
// match plan §15 expectations.
//
// Returns errKnownFolderUnavailable when the Windows resolver fails
// in production (no env fallback compiled in). Returns
// errStateParentInsecure when the POSIX sanity check rejects an
// existing parent directory.
func DaemonStateDir() (string, error) {
	return daemonStateDir()
}

// OpenStateFile opens (or creates) `name` under the daemon state
// directory for write. The file is created with 0600 perms on POSIX;
// the post-open Chmod defends against umask drift. Returns
// errStateNameInvalid if name is anything other than a single path
// component (no traversal, no separators, no drive letters).
//
// Caller contract:
//   - Only one writer at a time on the returned file (no fcntl lock
//     applied here; later tasks layer flock via gofrs/flock).
//   - Caller MUST Close the returned *os.File. The wrapper is a thin
//     layer over os.OpenFile and does not retain a reference.
func OpenStateFile(name string) (*os.File, error) {
	if err := validateStateFileName(name); err != nil {
		return nil, err
	}
	dir, err := daemonStateDir()
	if err != nil {
		return nil, err
	}
	target := filepath.Join(dir, name)

	// Pre-create at 0600 + O_CREATE|O_EXCL. If the file already exists,
	// fall through to the truncate path so callers re-using a state
	// file (rotated logs, atomic-rename targets) keep working.
	flags := os.O_RDWR | os.O_CREATE | os.O_TRUNC
	f, err := os.OpenFile(target, flags, 0o600)
	if err != nil {
		return nil, err
	}
	// Post-open Chmod defends against umask drift on POSIX (plan §15).
	// Windows ignores POSIX bits so the call is a cheap no-op there.
	if chErr := os.Chmod(target, 0o600); chErr != nil {
		_ = f.Close()
		_ = os.Remove(target)
		return nil, fmt.Errorf("chmod %s: %w", target, chErr)
	}
	return f, nil
}

// validateStateFileName refuses anything that could escape the owned
// state root. Names must be a single path component (no `/`, no `\`,
// no `..`, no leading absolute markers).
func validateStateFileName(name string) error {
	if name == "" {
		return errStateNameInvalid
	}
	if strings.ContainsAny(name, `/\`) {
		return errStateNameInvalid
	}
	if name == "." || name == ".." {
		return errStateNameInvalid
	}
	if strings.HasPrefix(name, "..") {
		return errStateNameInvalid
	}
	// filepath.Clean normalizes; if normalization changes the input,
	// or the result is not a single component, refuse.
	cleaned := filepath.Clean(name)
	if cleaned != name {
		return errStateNameInvalid
	}
	if filepath.IsAbs(cleaned) {
		return errStateNameInvalid
	}
	if filepath.Base(cleaned) != cleaned {
		return errStateNameInvalid
	}
	return nil
}

// ---------------------------------------------------------------------
// POSIX resolver.
// ---------------------------------------------------------------------

// posixStateDir returns the per-user state path on Linux/macOS,
// applies plan §15 permissions, and runs the parent sanity check.
// Returns errStateParentInsecure if the existing parent dir has
// world/group writable bits or a foreign UID.
func posixStateDir() (string, error) {
	parent, err := posixParentDir()
	if err != nil {
		return "", err
	}
	target := filepath.Join(parent, stateDirName)

	// Create the entire tree at 0700 then explicit Chmod (the umask
	// may have widened MkdirAll's output to 0755). Plan §15.
	if err := os.MkdirAll(target, 0o700); err != nil {
		return "", fmt.Errorf("mkdir state dir: %w", err)
	}
	if err := os.Chmod(target, 0o700); err != nil {
		return "", fmt.Errorf("chmod state dir: %w", err)
	}

	// Sanity check the immediate parent (e.g. ~/.local/share) — if
	// somebody pre-seeded it with insecure perms, refuse to write
	// any state. The leaf was just chmodded by us; checking the
	// parent is what catches a hostile environment.
	if err := posixDirSanityCheck(parent); err != nil {
		return "", err
	}
	// Defense in depth: also check the leaf itself. If our chmod
	// raced or got reverted by an antivirus / FUSE layer, fail
	// closed before any caller writes secrets.
	if err := posixDirSanityCheck(target); err != nil {
		return "", err
	}
	return target, nil
}

// posixParentDir resolves the OS-specific parent under which
// `stateDirName` is mounted. Linux honors XDG_DATA_HOME; macOS uses
// the canonical `~/Library/Application Support` per plan §15.
//
// The XDG fallback (when XDG_DATA_HOME is empty) and the macOS path
// both consult $HOME via os.UserHomeDir, which itself respects the
// HOME env var on POSIX. Tests rely on `t.Setenv("HOME", ...)` to
// redirect resolution.
func posixParentDir() (string, error) {
	switch runtime.GOOS {
	case "linux":
		if xdg := os.Getenv("XDG_DATA_HOME"); xdg != "" {
			return xdg, nil
		}
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("user home: %w", err)
		}
		return filepath.Join(home, ".local", "share"), nil
	case "darwin":
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("user home: %w", err)
		}
		return filepath.Join(home, "Library", "Application Support"), nil
	default:
		return "", fmt.Errorf("unsupported GOOS for state dir: %q", runtime.GOOS)
	}
}

// posixDirSanityCheck enforces plan §16 v9: the directory must NOT
// be group- or world-writable, and (when the OS reports a Uid) MUST
// be owned by the calling process's UID. Returns errStateParentInsecure
// on any violation.
//
// Windows is handled by callers — POSIX paths are the only ones that
// reach this function — but the impl deliberately uses portable Go
// APIs so an accidental Windows entry returns gracefully.
func posixDirSanityCheck(path string) error {
	st, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("stat %s: %w", path, err)
	}
	if !st.IsDir() {
		return fmt.Errorf("%s: not a directory", path)
	}
	mode := st.Mode().Perm()
	if mode&0o022 != 0 {
		return fmt.Errorf("%s: mode %o is group- or world-writable: %w", path, mode, errStateParentInsecure)
	}
	// UID check: only meaningful on POSIX. Use the helper exposed
	// by the platform-specific files (state_paths_unix.go,
	// state_paths_windows.go) so this file stays buildable on every
	// platform without conditional compilation.
	if uid, ok := statUID(st); ok {
		if uid != os.Getuid() {
			return fmt.Errorf("%s: owner UID %d != current UID %d: %w", path, uid, os.Getuid(), errStateParentInsecure)
		}
	}
	return nil
}
