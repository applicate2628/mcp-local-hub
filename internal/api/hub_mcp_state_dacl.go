// Package api — shared state-file read hardening sentinels.
//
// The live read-side gate is readStateFileInodeAnchored, which combines
// symlink refusal, owner / DACL checks, and the content read on the same
// inode-anchored file handle / fd.

package api

import "errors"

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
