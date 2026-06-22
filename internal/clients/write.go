// write.go — shared adapter write path.
//
// Every adapter in this package serializes its config to a byte slice
// then routes the actual disk write through `WriteConfigFile`. The
// indirection exists for two reasons:
//
//  1. Production (Phase 5 install reconciler + ALL per-server
//     installs): `internal/api/init()` swaps `WriteConfigFile` to
//     `SecureWriteClientConfig`. That writer performs a handle-relative
//     atomic-rename with parent-dir + file DACL allowlist verification
//     (current-user, LocalSystem, BuiltinAdministrators on Windows;
//     0600 + owner-uid on POSIX), defeating the TOCTOU race a path-
//     swapping attacker could otherwise win between the adapter's
//     read/parse and the final rename. Token-bearing client configs
//     (G4 Phase 5 mcphub-hub aggregate entry carries the per-client
//     hub token in headers) cannot afford that race.
//
//  2. Tests (in-package and cross-package): the default
//     fallbackWriteConfigFile remains a plain os.WriteFile wrapper so
//     existing adapter tests that use `t.TempDir()` (which on Windows
//     lives under %TEMP%'s Authenticated Users-readable DACL) continue
//     to pass without forcing every test through hardenedTempDir.
//     Dedicated DACL adapter tests use the override hook explicitly.
//
// Spec: docs/superpowers/specs/2026-05-12-g4-unified-hub-mcp-design-v3.md
// §"SecureWriteClientConfig sequence" + §"Bidirectional install
// reconciler" (the install path must inherit the same DACL pipeline).
// Plan: docs/superpowers/plans/2026-05-12-g4-unified-hub-mcp.md Task 5.1.

package clients

import (
	"fmt"
	"os"
	"path/filepath"
)

// WriteConfigFile writes `contents` to `path`, creating parent
// directories as needed. The production binary swaps this variable to
// api.SecureWriteClientConfig during init() so every adapter inherits
// the handle-relative + DACL-bound write pipeline; the default
// fallbackWriteConfigFile keeps in-package adapter tests working
// without forcing every test through hardenedTempDir.
//
// The swap is one-way (production overrides default) and not protected
// by a mutex — init() is single-threaded so a concurrent overwrite is
// impossible. Tests that want to revert MUST capture the previous
// value and restore in t.Cleanup.
var WriteConfigFile = fallbackWriteConfigFile

// CreateConfigFileIfMissing is the production swap point for the
// init-time atomic create-new helper. The test default is the same
// temp+hardlink pattern that's safe on POSIX (mode 0o600 honored
// regardless of parent perms) but leaves a microsecond window on
// Windows where the just-published file inherits the parent's
// broadened DACL before the next AddEntry rewrites with the
// allowlist-only DACL via SecureWriteClientConfig.
//
// `internal/api/init()` swaps this variable to
// `api.SecureCreateClientConfigIfMissing` which embeds the
// allowlist-only DACL via OBJECT_ATTRIBUTES.SecurityDescriptor at
// NtCreateFile time on Windows (file BORN with restrictive DACL —
// zero window) and adds the strict-mode parent-DACL gate. POSIX
// keeps the temp+hardlink pattern because mode 0o600 is the
// load-bearing security boundary there.
//
// Signature mirrors EnsureClientConfigStub: returns
// (created bool, err error) so adapter InitEmpty() can surface the
// race-winner verdict to the operator.
//
// Deep-sec PR #208 Lane C #1 closure.
var CreateConfigFileIfMissing = fallbackCreateConfigFileIfMissing

// SecureCreateParentDir is the production swap point for the write-target
// PARENT-DIRECTORY creation that withConfigLock (config_lock.go) performs
// before opening the advisory flock. The test default is a plain
// os.MkdirAll — acceptable because in-package and cross-package adapter
// tests run under t.TempDir() where there is no symlink/reparse-point
// threat, exactly the rationale fallbackWriteConfigFile /
// fallbackCreateConfigFileIfMissing use for keeping a plain os-level
// default.
//
// `internal/api/init()` swaps this variable to
// `api.SecureCreateParentDirForConfigLock` which creates the missing
// parent chain COMPONENT-BY-COMPONENT, REFUSING any symlink / reparse-point
// component (POSIX O_NOFOLLOW openat; Windows NtCreateFile +
// FILE_OPEN_REPARSE_POINT-refusal) and anchored at the nearest existing
// ancestor. That closes the bot PR #420 finding 1 (r16) P1 regression: the
// previous blind os.MkdirAll here FOLLOWED a symlinked prefix and could
// create the write-target parent OUTSIDE the intended path, after which the
// hardened SecureWriteClientConfig would publish token-bearing client config
// through that redirected parent.
//
// Mode 0o700 in the fallback (NOT 0o755): a fresh mcphub-created config dir
// with no operator mode to preserve, and the secure-write parent-dir gate
// rejects group/world bits on POSIX — a 0o755 dir would make a subsequent
// strict-mode SecureWriteClientConfig reject the very dir just created. The
// injected production impl creates each component owner-only too.
var SecureCreateParentDir = fallbackSecureCreateParentDir

// fallbackWriteConfigFile is the test-friendly default. It creates
// parent dirs (matching the prior writeJSON / writeTOML behavior in
// each adapter), opens the destination with O_CREATE|O_WRONLY|O_TRUNC
// at 0600, and writes the bytes.
//
// This intentionally does NOT acquire the secure-write pipeline's
// DACL guarantees. Production callers MUST ensure WriteConfigFile is
// pointed at api.SecureWriteClientConfig.
func fallbackWriteConfigFile(path string, contents []byte) error {
	if dir := filepath.Dir(path); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	return os.WriteFile(path, contents, 0o600)
}

// fallbackSecureCreateParentDir is the test-friendly default for
// SecureCreateParentDir. It creates `dir` and any missing ancestors via a
// plain os.MkdirAll(0o700). This intentionally does NOT acquire the
// symlink-refusing component-walk guarantees of the production impl — it is
// safe only in test sandboxes (t.TempDir()) where no co-resident principal
// can plant a symlinked prefix. Production callers MUST ensure
// SecureCreateParentDir is pointed at api.SecureCreateParentDirForConfigLock
// (wired by internal/api/init()).
func fallbackSecureCreateParentDir(dir string) error {
	return os.MkdirAll(dir, 0o700)
}

// EnsureClientConfigStub is the canonical helper for adapter
// InitEmpty() implementations (v0.4.5 init-button feature). It
// atomically creates `path` populated with `stub` if and only if no
// regular file exists at `path`.
//
// Returns (created, err):
//
//   - created=true, err=nil: this call wrote the stub bytes.
//   - created=false, err=nil: a regular file already existed at the
//     destination (either before this call, or as the winner of a
//     publish race) — idempotent success.
//   - created=false, err!=nil: refusal (symlink, junction, non-
//     regular entry) or I/O failure. The caller MUST surface this
//     to the operator; the production endpoint maps it to a 500
//     INIT_FAILED response.
//
// Contract beyond the return shape:
//
//   - The function does NOT create the parent directory. Callers
//     must MkdirAll explicitly before calling if they need
//     directory creation. This is the v0.4.5 deep-sec Lane A #1
//     follow-up: the endpoint stats the parent for gating; an
//     implicit MkdirAll here would re-create a parent that was
//     deleted in the TOCTOU window and produce a silent 200 success
//     where the operator expects 412 PARENT_MISSING.
//
//   - Two-phase atomic publish: stub bytes are written to a temp
//     file in the same directory, then the temp file is published
//     to `path` via `os.Link` (hardlink). Hardlink semantics are
//     no-replace on both POSIX and Windows, so the publish step
//     atomically commits the file with content present. This
//     closes the v0.4.5 deep-sec Lane A #2 "concurrent reader sees
//     an empty file" window: between the temp's `O_CREAT|O_EXCL`
//     open and the hardlink commit, only the temp path is visible,
//     not `path`. After the hardlink commits, both paths point at
//     the same inode; the temp is unlinked, leaving `path` with
//     fully-written content. Hardlink requires same-volume
//     filesystem — every realistic client-config parent (HOME-
//     relative, %APPDATA%-relative) satisfies this.
//
//   - Symlink and non-regular-entry refusal happens BOTH before
//     the temp write (cheap pre-check) AND after a publish-race
//     EEXIST (re-Lstat to verify the winner is a regular file).
//     Closes the v0.4.5 deep-sec Lane B + Lane C #2 "loser doesn't
//     re-check" finding: previously an attacker who planted a
//     symlink in the race window between the initial Lstat and the
//     create publish could cause the loser branch to silently
//     return nil with a non-regular winner.
//
// Production trade-off: this helper deliberately bypasses the
// WriteConfigFile / SecureWriteClientConfig pipeline because that
// pipeline uses FILE_RENAME_REPLACE_IF_EXISTS (Windows) / replacing
// renameat (POSIX) — exactly the stat-then-replace surface this
// helper exists to close. The freshly-created stub file therefore
// inherits the parent's default DACL rather than the allowlist-only
// DACL that SecureWriteClientConfig installs. For default-mode
// operators this is fine: stub content is non-sensitive, and the
// next AddEntry / install / migrate write goes through
// SecureWriteClientConfig and atomic-renames the file with proper
// DACL. For strict-mode operators (MCPHUB_REQUIRE_SINGLE_USER_HOME=1)
// this is a known gap: the Init endpoint refuses in strict mode
// pending the v0.4.6+ SecureCreateClientConfigIfMissing helper —
// see internal/gui/init_client_config.go for the refusal logic.
func EnsureClientConfigStub(path string, stub []byte) (created bool, err error) {
	return CreateConfigFileIfMissing(path, stub)
}

// fallbackCreateConfigFileIfMissing is the test-mode default that
// CreateConfigFileIfMissing resolves to when api.init() has not run
// (cross-package adapter tests via t.TempDir, in-package init_empty
// tests). It uses the cross-platform os.CreateTemp + os.Link pattern
// which is fully safe on POSIX (mode 0o600 honored regardless of
// parent dir mode) and leaves a microsecond residual window on
// Windows where the published file inherits the parent's default
// DACL — that window is closed in production by the api-side swap
// to SecureCreateClientConfigIfMissing which embeds the allowlist-
// only DACL at NtCreateFile time.
//
// The fallback covers POSIX production fully because POSIX file
// security is the inode mode bits (0o600 = owner-only), not parent-
// inherited ACLs; api.init() swaps to a POSIX-aware variant that
// adds parent-DACL gate enforcement in strict mode but keeps the
// same atomic temp+hardlink core.
func fallbackCreateConfigFileIfMissing(path string, stub []byte) (created bool, err error) {
	if existed, refusal := classifyExistingForInit(path); refusal != nil {
		return false, refusal
	} else if existed {
		return false, nil
	}
	dir := filepath.Dir(path)
	tmpFile, err := os.CreateTemp(dir, filepath.Base(path)+".init-stub.*.tmp")
	if err != nil {
		return false, fmt.Errorf("init stub temp create in %s: %w", dir, err)
	}
	tmpPath := tmpFile.Name()
	if _, writeErr := tmpFile.Write(stub); writeErr != nil {
		_ = tmpFile.Close()
		_ = os.Remove(tmpPath)
		return false, writeErr
	}
	if closeErr := tmpFile.Close(); closeErr != nil {
		_ = os.Remove(tmpPath)
		return false, closeErr
	}
	if linkErr := os.Link(tmpPath, path); linkErr != nil {
		_ = os.Remove(tmpPath)
		if os.IsExist(linkErr) {
			if existed, refusal := classifyExistingForInit(path); refusal != nil {
				return false, refusal
			} else if existed {
				return false, nil
			}
			return false, fmt.Errorf(
				"init stub publish lost race but destination is now absent: %w",
				linkErr,
			)
		}
		return false, linkErr
	}
	_ = os.Remove(tmpPath)
	return true, nil
}

// classifyExistingForInit inspects what's at `path` without
// following symlinks and returns:
//
//   - existed=true,  err=nil: regular file present (idempotent
//     success for the caller).
//   - existed=false, err=nil: path does not exist.
//   - existed=false, err!=nil: refusal (symlink, junction, or any
//     non-regular entry), or an unexpected stat failure (permissions,
//     I/O fault).
//
// Used twice by EnsureClientConfigStub: once before the temp write
// (cheap pre-check) and once after a publish-race EEXIST (to
// re-verify the winner). Both call sites must apply identical
// refusal rules so an attacker cannot exploit asymmetric handling.
func classifyExistingForInit(path string) (existed bool, err error) {
	st, statErr := os.Lstat(path)
	if statErr != nil {
		if os.IsNotExist(statErr) {
			return false, nil
		}
		return false, statErr
	}
	if st.Mode()&os.ModeSymlink != 0 {
		return false, fmt.Errorf(
			"refuse to initialize through symlink at %s "+
				"(remove the symlink and retry; init must not follow symlinks "+
				"because the production write pipeline could be redirected to "+
				"an attacker-chosen target)",
			path,
		)
	}
	if !st.Mode().IsRegular() {
		return false, fmt.Errorf(
			"refuse to initialize over non-regular existing entry at %s (mode %s)",
			path, st.Mode(),
		)
	}
	return true, nil
}
