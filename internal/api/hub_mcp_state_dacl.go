// Package api — VerifyHubMcpStateDACL (Phase 1, Task 1.5).
//
// VerifyHubMcpStateDACL is the read-side gate for hub-mcp state files
// (hub-mcp-tokens.json, hub-mcp.endpoint.json, hub-mcp-control.token,
// hub-mcp.log) loaded by Phase 2+ startup. Refuses to trust any file
// whose effective access grants read to a SID / uid outside the
// single-user allowlist:
//
//   - POSIX: owner-uid == current uid AND mode & 0o077 == 0.
//   - Windows: canonical DACL evaluation; every read-capable ALLOW ACE
//     names a SID in {current-user, LocalSystem, BuiltinAdministrators}.
//
// Spec: docs/superpowers/specs/2026-05-12-g4-unified-hub-mcp-design-v3.md
// §"Windows DACL verification" (allowlist form, codex r3 F-S3 closure).
//
// Caller contract: any error is HARD — Phase 2's hub-mcp loader MUST
// refuse to start the hub and surface a clear diagnostic. The expected
// recovery on enterprise Group-Policy-managed paths is documented in
// the spec §"Enterprise stance — Group Policy / MDM-managed ACLs".

package api

import "errors"

// VerifyHubMcpStateDACL opens path with reparse-defeat flags (POSIX:
// O_NOFOLLOW; Windows: FILE_FLAG_OPEN_REPARSE_POINT), stats from the
// open handle, and asserts the file is single-user-safe per the
// allowlist model. Errors are sentinel values defined below so the
// loader can map them to operator-actionable diagnostics.
//
// Spec: §"Windows DACL verification".
func VerifyHubMcpStateDACL(path string) error {
	return verifyHubMcpStateDACLImpl(path)
}

// ErrIrregularFile is returned when the path is a symlink, junction,
// or other irregular filesystem object that we refuse to trust.
var ErrIrregularFile = errors.New("hub-mcp state file is a symlink or irregular")

// ErrWrongOwner is returned when the file's owner uid (POSIX) or
// owner SID (Windows) is not the current user. Indicates a swap
// attack or misconfigured profile root.
var ErrWrongOwner = errors.New("hub-mcp state file owner is not current user")

// ErrTooLoose is returned when the file's effective mode bits
// (POSIX) grant any group / other access. Windows callers see
// ErrDaclOutsideAllowlist instead.
var ErrTooLoose = errors.New("hub-mcp state file mode is group/world accessible")

// ErrDaclOutsideAllowlist is returned (Windows) when the canonical
// DACL evaluation finds a read-capable ALLOW ACE granting a SID
// outside {current-user, LocalSystem, BuiltinAdministrators}.
// Common cause: Group-Policy-managed paths with corporate management
// or Domain Users inherited ACEs. See spec §"Enterprise stance".
var ErrDaclOutsideAllowlist = errors.New("hub-mcp state file DACL grants read to a SID outside {current-user, LocalSystem, BuiltinAdministrators}")
