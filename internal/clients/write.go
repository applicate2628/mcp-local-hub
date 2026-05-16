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
	if existed, refusal := classifyExistingForInit(path); refusal != nil {
		return false, refusal
	} else if existed {
		return false, nil
	}
	dir := filepath.Dir(path)
	// os.CreateTemp atomically allocates a unique filename via an
	// internal retry loop, so concurrent goroutines never collide on
	// the temp basename. It opens with O_CREAT|O_EXCL at 0o600.
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
			// Lost the publish race — re-classify the winner. If it
			// turned out to be a symlink/non-regular (attacker-planted
			// between our pre-check and the link), surface the refusal
			// honestly.
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
