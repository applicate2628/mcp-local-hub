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
// regular file exists at `path`. Idempotent across legitimate
// concurrent writers and resistant to attacker-planted symlinks.
//
// Contract:
//
//   - If a regular file already exists at `path`, returns nil and
//     does NOT touch the bytes. This covers the "second-click" case
//     where another tab or another `mcphub` invocation already wrote
//     the file between the GUI's scan refresh and the Initialize POST.
//   - If a symlink, junction, named pipe, or any non-regular entry
//     exists at `path`, returns a refusal error. This is the
//     defense against PR #208 deep-sec Lane C: in default-relax mode,
//     the production WriteConfigFile pipeline would otherwise follow
//     a pre-planted symlink and write the stub bytes through it to
//     an attacker-chosen location. Init must never follow symlinks.
//   - Otherwise, atomically creates `path` with `O_CREAT|O_EXCL` —
//     on POSIX with `O_NOFOLLOW` so the create is symlink-safe at
//     the kernel level; on Windows the L-stat pre-check above is the
//     defense (the residual window between Lstat and CreateFileW is
//     small and not directly addressable without NtCreateFile +
//     FILE_FLAG_OPEN_REPARSE_POINT — tracked for v0.4.6+). If a
//     concurrent writer wins the create race, the function returns
//     nil (treats EEXIST as idempotent success).
//
// Parent directories are auto-created (0755) before the create
// attempt; callers that want to gate on "parent must already exist"
// (the /api/init-client-config endpoint) MUST verify parent presence
// separately — this helper is intentionally permissive on the parent
// so it can also serve adapter BackupKeep paths where seeding an
// empty stub from scratch is the documented behavior.
//
// The atomic O_EXCL create bypasses the WriteConfigFile / production
// SecureWriteClientConfig pipeline because that pipeline uses
// FILE_RENAME_REPLACE_IF_EXISTS (Windows) / replacing renameat
// (POSIX) which has stat-then-replace semantics inherently — exactly
// the race surface this helper exists to close. The trade-off is
// that the freshly-created stub file inherits the parent's default
// DACL rather than the allowlist-only DACL that SecureWriteClientConfig
// installs. For an empty config file this is acceptable: every
// subsequent AddEntry / install / migrate write goes through
// SecureWriteClientConfig which atomic-replaces the file with proper
// DACL via temp-file rename, so the weak DACL window only exists for
// non-sensitive empty stub content.
func EnsureClientConfigStub(path string, stub []byte) error {
	if st, err := os.Lstat(path); err == nil {
		if st.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf(
				"refuse to initialize through pre-existing symlink at %s "+
					"(remove the symlink and retry; init must not follow symlinks "+
					"because the production write pipeline could be redirected to "+
					"an attacker-chosen target)",
				path,
			)
		}
		if st.Mode().IsRegular() {
			return nil
		}
		return fmt.Errorf(
			"refuse to initialize over non-regular existing entry at %s (mode %s)",
			path, st.Mode(),
		)
	} else if !os.IsNotExist(err) {
		return err
	}
	if dir := filepath.Dir(path); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	flags := os.O_CREATE | os.O_EXCL | os.O_WRONLY
	flags |= createNoFollowFlag()
	f, err := os.OpenFile(path, flags, 0o600)
	if err != nil {
		if os.IsExist(err) {
			return nil
		}
		return err
	}
	if _, writeErr := f.Write(stub); writeErr != nil {
		_ = f.Close()
		return writeErr
	}
	return f.Close()
}
