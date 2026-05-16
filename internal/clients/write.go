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
// InitEmpty() implementations (v0.4.5 init-button feature). It creates
// `path` populated with `stub` if and only if the file does not
// already exist. Parent directories are auto-created (0755), and the
// final write routes through WriteConfigFile so production inherits
// the SecureWriteClientConfig handle-relative + DACL-bound pipeline.
//
// Idempotent: if the file is already present, returns nil without
// touching its bytes.
//
// Callers that want to gate on "parent must already exist" (e.g. the
// /api/init-client-config endpoint, which refuses to create a fresh
// `~/.cursor/` or `%APPDATA%\Code\User\` tree on a host where the
// client is not actually installed) MUST verify parent presence
// before calling — this helper is intentionally permissive so it can
// also serve adapter BackupKeep paths where seeding an empty stub
// from scratch is the documented behavior.
func EnsureClientConfigStub(path string, stub []byte) error {
	if _, err := os.Stat(path); err == nil {
		return nil
	} else if !os.IsNotExist(err) {
		return err
	}
	if dir := filepath.Dir(path); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	return WriteConfigFile(path, stub)
}
