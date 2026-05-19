// read_hardening.go — platform-neutral entry point for the hardened
// overlay-file open used by Load (Task 2.4 of the v0.5.x Servers
// matrix revamp).
//
// The actual hardenedOpen implementation lives in the platform-
// specific siblings:
//
//   - read_hardening_posix.go (!windows): uses
//     os.OpenFile(path, O_RDONLY|O_NOFOLLOW, 0). The kernel refuses
//     to traverse a symlink at the leaf; ELOOP propagates upward as a
//     plain syscall error and Load() wraps it with %s: open: %w.
//
//   - read_hardening_windows.go (windows): opens via
//     windows.CreateFile(path, GENERIC_READ, FILE_SHARE_READ, ...,
//     OPEN_EXISTING, FILE_FLAG_OPEN_REPARSE_POINT |
//     FILE_FLAG_BACKUP_SEMANTICS, 0) so the open itself does NOT
//     traverse a reparse point; then GetFileInformationByHandle
//     reports the attributes and the helper refuses any file with
//     FILE_ATTRIBUTE_REPARSE_POINT set. The handle is wrapped via
//     os.NewFile(uintptr(h), path) so Load can keep using *os.File
//     unchanged.
//
// Spec: docs/superpowers/specs/2026-05-19-servers-matrix-lsp-and-env-revamp-design.md
// §"Read-side hardening" (B-V4-1, B-V4-4).

package daemon_env_overlay

// AllowUnhardenedStateReadEnv is the operator opt-out for the read-
// side parent-DACL gate (checkStateDirParentReadSafe in
// parent_check.go). Set to "1" or "true" (case-insensitive) on hosts
// where the parent directory is broadened in a way the operator
// cannot tighten (corporate GPO, Codex CLI's CodexSandboxUsers, AD
// orphan SIDs). Symmetric with AllowUnhardenedStateWriteEnv in
// internal/api/client_write_init.go so a single operator decision
// covers both directions.
//
// Strict-mode override: when MCPHUB_REQUIRE_SINGLE_USER_HOME=1 is
// also set, the strict gate takes precedence and this opt-out is
// ignored — the strict posture's invariant (multi-tenant /
// corp-managed hosts get hardening regardless of per-operator env
// vars) holds.
const AllowUnhardenedStateReadEnv = "MCPHUB_ALLOW_UNHARDENED_STATE_READ"
