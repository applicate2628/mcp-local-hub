// Package api — shared state-file read hardening sentinels.
//
// The live read-side gate is readStateFileInodeAnchored, which combines
// symlink refusal, owner / DACL checks, and the content read on the same
// inode-anchored file handle / fd.

package api

import (
	"errors"
	"fmt"
	"strings"
)

const StateFileDACLRunbookTitle = "secret daemons exit 1 on a sandbox-broadened %LOCALAPPDATA%"

const StateFileDACLRunbookPointer = `tighten this file's DACL to owner-only (your account + SYSTEM + Administrators); see the "` + StateFileDACLRunbookTitle + `" runbook in CLAUDE.md for the exact icacls / chmod command, or run: mcphub repair-state-dacl --path <file>. If the refusal names the PARENT DIRECTORY instead of a file, tighten the parent directory to owner-only too — see the runbook for the directory icacls / chmod command — because repair-state-dacl repairs a state FILE, not a directory.`

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

// DACLAllowlistViolation preserves the offending SID from the Windows DACL
// verifier while keeping ErrDaclOutsideAllowlist in the error chain.
type DACLAllowlistViolation struct {
	SID  string
	Mask uint32
}

func (e *DACLAllowlistViolation) Error() string {
	return fmt.Sprintf("%s: SID %s grants access (mask=0x%08x)", ErrDaclOutsideAllowlist, e.SID, e.Mask)
}

func (e *DACLAllowlistViolation) Unwrap() error {
	return ErrDaclOutsideAllowlist
}

type StateFileDACLRemediationDetails struct {
	Path         string
	OffendingSID string
}

func StateFileDACLRemediationDetailsFor(path string, cause error) StateFileDACLRemediationDetails {
	return StateFileDACLRemediationDetails{
		Path:         path,
		OffendingSID: stateFileDACLOffendingSID(cause),
	}
}

func stateFileDACLOffendingSID(cause error) string {
	var violation *DACLAllowlistViolation
	if !errors.As(cause, &violation) || violation == nil {
		return ""
	}
	sid := strings.TrimSpace(strings.TrimPrefix(violation.SID, "*"))
	if sid == "" || sid == "<nil>" || sid == "<unresolved-sid>" {
		return ""
	}
	return sid
}
