//go:build windows

// Package api: dacl_shared_windows.go defines the v0.5.0 supervisor-state
// allowlist primitive. The single SID allowlist (current process user +
// LocalSystem + BuiltinAdministrators) is rendered in two output forms
// per the call site's resource type:
//
//   - File form  (AllowlistMaskFile): GENERIC_ALL, retains BA. Consumed
//     by SecureWriteClientConfig (file-create call sites) via
//     SecurityDescriptorFromString → *windows.SECURITY_DESCRIPTOR.
//   - Pipe form  (AllowlistMaskPipe): GENERIC_READ | GENERIC_WRITE, drops
//     BA. Consumed by go-winio PipeConfig.SecurityDescriptor (string
//     SDDL field) for v0.5.0 supervisor IPC pipes. Dropping BA is the
//     defense-in-depth posture from v13 Q11 closure: an admin token
//     cannot issue supervisor commands (exit/restart/quiesce-timers)
//     without owner consent.
//
// Refactor history (v0.5.0 phase 1, task 1.2):
//   - secure_write_windows.go:498 `buildRestrictiveSecurityDescriptor`
//     previously built the SD via EXPLICIT_ACCESS entries +
//     ACLFromEntries + NewSecurityDescriptor + SetDACL + SetControl.
//     That path produces a SECURITY_DESCRIPTOR whose on-disk ACE binary
//     form is identical to the SDDL "D:P(A;;GA;;;<sid>)(A;;GA;;;SY)
//     (A;;GA;;;BA)" output of BuildAllowlistSDDL/BuildAllowlistSD:
//     same SE_DACL_PROTECTED control bit, same ACCESS_ALLOWED_ACE
//     type with same mask, same SID set, same lack of inheritance
//     flags. Trustee-type hints (TRUSTEE_IS_USER vs
//     TRUSTEE_IS_WELL_KNOWN_GROUP vs TRUSTEE_IS_GROUP) are build-time
//     ACLFromEntries inputs only; they do not survive into the
//     persisted ACE format, which carries only SID identity. The
//     verify path (verifyWindowsDACLFromHandleMasked in
//     hub_mcp_state_dacl_windows.go) matches by SID equality, so the
//     SDDL-built SD passes the same gate.

package api

import (
	"fmt"

	"golang.org/x/sys/windows"
)

// AllowlistMaskMode discriminates the per-resource ACE mask form.
type AllowlistMaskMode int

const (
	// AllowlistMaskFile -- GENERIC_ALL for the {current-user,
	// LocalSystem, BuiltinAdministrators} allowlist. Used by file-
	// create call sites (SecureWriteClientConfig pipeline).
	AllowlistMaskFile AllowlistMaskMode = iota

	// AllowlistMaskPipe -- GENERIC_READ | GENERIC_WRITE for the
	// {current-user, LocalSystem} allowlist (BA dropped per v13 Q11
	// closure). Used by v0.5.0 supervisor named-pipe call sites.
	AllowlistMaskPipe
)

// BuildAllowlistSDDL returns the SDDL text string representing the
// supervisor-state allowlist for the given mode. The output is consumed
// by `winio.PipeConfig.SecurityDescriptor` (string field) for the pipe
// form, and converted to a `*windows.SECURITY_DESCRIPTOR` via
// `windows.SecurityDescriptorFromString` for the file form (existing
// SecureWriteClientConfig pipeline; see BuildAllowlistSD below).
//
// SDDL semantics used here:
//
//	D:P            -- DACL present + SE_DACL_PROTECTED (blocks inheritance
//	                  from the parent dir's ACEs).
//	(A;;<mask>;;;<sid>) -- ALLOW ACE, no ACE-flags (no inheritance), the
//	                       given access mask, no object guids, the SID.
//
// SDDL well-known SID aliases used here:
//
//	SY -- LocalSystem (S-1-5-18).
//	BA -- BuiltinAdministrators (S-1-5-32-544).
func BuildAllowlistSDDL(mode AllowlistMaskMode) (string, error) {
	userSIDStr, err := currentUserSIDString()
	if err != nil {
		return "", fmt.Errorf("current user SID: %w", err)
	}
	switch mode {
	case AllowlistMaskFile:
		// File form: GENERIC_ALL, allowlist = {user, SY, BA}.
		// Equivalent to the legacy ACLFromEntries-built SD at
		// secure_write_windows.go:498 — same control bit, same ACE
		// set, same masks. See refactor history comment above.
		return fmt.Sprintf("D:P(A;;GA;;;%s)(A;;GA;;;SY)(A;;GA;;;BA)", userSIDStr), nil
	case AllowlistMaskPipe:
		// Pipe form: GRGW, allowlist = {user, SY}. BA dropped per
		// v13 Q11 closure (defense-in-depth: admin tokens cannot
		// issue supervisor IPC commands without owner consent).
		return fmt.Sprintf("D:P(A;;GRGW;;;%s)(A;;GRGW;;;SY)", userSIDStr), nil
	default:
		return "", fmt.Errorf("unknown allowlist mode: %d", mode)
	}
}

// BuildAllowlistSD returns the file-form `*windows.SECURITY_DESCRIPTOR`
// for file-create call sites. Equivalent to
// `BuildAllowlistSDDL(AllowlistMaskFile)` followed by
// `windows.SecurityDescriptorFromString`.
//
// Callers must NOT mutate the returned SD via its setters; the SD is
// a fresh allocation per call but the contract (PROTECTED DACL +
// {user, SY, BA} ALLOW ACEs with GENERIC_ALL) is load-bearing for
// verifyWindowsDACLFromHandle and downstream test fixtures.
func BuildAllowlistSD() (*windows.SECURITY_DESCRIPTOR, error) {
	sddl, err := BuildAllowlistSDDL(AllowlistMaskFile)
	if err != nil {
		return nil, err
	}
	sd, err := windows.SecurityDescriptorFromString(sddl)
	if err != nil {
		return nil, fmt.Errorf("SD from SDDL %q: %w", sddl, err)
	}
	return sd, nil
}

// allowlistSIDs returns the canonical 3-SID allowlist —
// {current process user, LocalSystem (S-1-5-18), BuiltinAdministrators
// (S-1-5-32-544)} — as a slice of *windows.SID, in that fixed order.
//
// This is the single owner of the allowlist triple. It is consumed in
// two distinct shapes:
//
//   - verifyWindowsDACLFromHandleMasked (hub_mcp_state_dacl_windows.go)
//     uses the SIDs directly as the ownerSIDAllowed / ACE-iterator
//     allowlist.
//   - allowlistExplicitAccess (below) wraps the same SIDs into
//     EXPLICIT_ACCESS GRANT entries for the DACL-construction call
//     sites (buildRestrictiveDACL + the allowlist test fixtures).
//
// Keeping both shapes derived from this one function guarantees the
// verify side and the build side can never drift on which principals
// are in the allowlist.
func allowlistSIDs() (current, system, admin *windows.SID, err error) {
	current, err = currentUserSID()
	if err != nil {
		return nil, nil, nil, fmt.Errorf("current user sid: %w", err)
	}
	system, err = windows.StringToSid("S-1-5-18")
	if err != nil {
		return nil, nil, nil, fmt.Errorf("system sid: %w", err)
	}
	admin, err = windows.StringToSid("S-1-5-32-544")
	if err != nil {
		return nil, nil, nil, fmt.Errorf("admin sid: %w", err)
	}
	return current, system, admin, nil
}

// allowlistExplicitAccess returns the canonical 3-entry EXPLICIT_ACCESS
// slice for the {current-user, LocalSystem, BuiltinAdministrators}
// allowlist, each granting GENERIC_ALL with NO_INHERITANCE. The trustee
// types match the long-standing inline form exactly (current=USER,
// system=WELL_KNOWN_GROUP, admin=GROUP) so the resulting DACL is
// byte-identical to what each call site previously open-coded.
//
// The trustee-type hints are ACLFromEntries build-time inputs only;
// they do not survive into the persisted ACE format (which carries SID
// identity + mask), and verifyWindowsDACLFromHandleMasked matches by
// SID equality. They are preserved verbatim regardless, to keep this
// extraction a pure no-op vs the prior inline slices.
func allowlistExplicitAccess() ([]windows.EXPLICIT_ACCESS, error) {
	current, system, admin, err := allowlistSIDs()
	if err != nil {
		return nil, err
	}
	return []windows.EXPLICIT_ACCESS{
		explicitAccessAllow(current, windows.TRUSTEE_IS_USER, windows.GENERIC_ALL),
		explicitAccessAllow(system, windows.TRUSTEE_IS_WELL_KNOWN_GROUP, windows.GENERIC_ALL),
		explicitAccessAllow(admin, windows.TRUSTEE_IS_GROUP, windows.GENERIC_ALL),
	}, nil
}

// currentUserSIDString returns the current process token's user SID in
// its textual (S-1-5-...) form, suitable for SDDL literal substitution.
// Wraps the package-level currentUserSID (hub_mcp_state_dacl_windows.go)
// for string-form output.
func currentUserSIDString() (string, error) {
	sid, err := currentUserSID()
	if err != nil {
		return "", err
	}
	s := sid.String()
	if s == "" {
		return "", fmt.Errorf("current user SID: textual conversion failed")
	}
	return s, nil
}
