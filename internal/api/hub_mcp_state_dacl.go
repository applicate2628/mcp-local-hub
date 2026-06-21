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

const (
	icaclsSystemSIDLiteral         = "*S-1-5-18"
	icaclsAdministratorsSIDLiteral = "*S-1-5-32-544"
)

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

// StateFileDACLRemediationCommand builds the Windows icacls command used by
// state-file read failures. The command resets explicit ACEs, disables
// inheritance, strips common broadening grant ACEs plus the observed offending
// SID, then replaces the allowlist principals' explicit grants.
func StateFileDACLRemediationCommand(path, ownerPrincipal string, cause error) string {
	ownerPrincipal = strings.TrimSpace(ownerPrincipal)
	if ownerPrincipal == "" {
		ownerPrincipal = "%USERNAME%"
	}
	removeArgs := stateFileDACLRemoveGrantArgs(cause)
	pathArg := quoteICACLSArg(path)
	return fmt.Sprintf("icacls %s /reset && icacls %s /inheritance:r /remove:g %s /grant:r %s %s %s",
		pathArg,
		pathArg,
		strings.Join(removeArgs, " "),
		quoteICACLSGrant(ownerPrincipal),
		quoteICACLSGrant(icaclsSystemSIDLiteral),
		quoteICACLSGrant(icaclsAdministratorsSIDLiteral))
}

func stateFileDACLRemoveGrantArgs(cause error) []string {
	sids := []string{
		"S-1-1-0",      // Everyone
		"S-1-5-11",     // Authenticated Users
		"S-1-5-32-545", // Builtin Users
	}
	var violation *DACLAllowlistViolation
	if errors.As(cause, &violation) && violation.SID != "" {
		sids = append(sids, violation.SID)
	}

	seen := make(map[string]struct{}, len(sids))
	args := make([]string, 0, len(sids))
	for _, sid := range sids {
		sid = strings.TrimSpace(strings.TrimPrefix(sid, "*"))
		if sid == "" || sid == "<nil>" || sid == "<unresolved-sid>" {
			continue
		}
		if _, ok := seen[sid]; ok {
			continue
		}
		seen[sid] = struct{}{}
		args = append(args, quoteICACLSArg("*"+sid))
	}
	return args
}

func quoteICACLSGrant(principal string) string {
	return quoteICACLSArg(icaclsSIDLiteral(principal) + ":F")
}

func quoteICACLSArg(s string) string {
	return `"` + strings.ReplaceAll(s, `"`, `\"`) + `"`
}

func icaclsSIDLiteral(principal string) string {
	principal = strings.TrimSpace(principal)
	sid := strings.TrimPrefix(principal, "*")
	if strings.HasPrefix(strings.ToUpper(sid), "S-") {
		return "*" + sid
	}
	return principal
}
