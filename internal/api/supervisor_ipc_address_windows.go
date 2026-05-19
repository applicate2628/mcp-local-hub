//go:build windows

package api

import (
	"os"
	"strings"
)

// SupervisorIPCAddress returns the Windows named-pipe path for the
// supervisor IPC channel. The stateDir argument is accepted for API parity
// with POSIX; Windows pipes live in the kernel namespace.
//
// The pipe name is keyed off the current process token's user SID
// (kernel-authoritative), NOT the USERNAME environment variable
// (caller-controllable). Both supervisor (listener) and client
// (DialSupervisorIPCStatus / DialSupervisorIPCExit) call this from
// inside the same user process so they always derive the same SID
// and converge on the same pipe path. The fallback for SID-resolution
// failures uses USERNAME with a documented-stable suffix, preserving
// compatibility for environments where token-query genuinely fails
// (rare on a single-user solo install).
//
// PR #212 r3 security finding 1: pre-existing reliance on USERNAME let
// an attacker who could spoof the env var redirect the new
// DialSupervisorIPCExit client to a fake pipe under a SID-matching
// supervisor.lock.owner.json. The SID-based discriminator closes that
// surface for both listener and client at once — the listener still
// SDDL-restricts the pipe to the owner SID + LocalSystem, but the
// pipe NAME no longer depends on a caller-controllable channel.
func SupervisorIPCAddress(_ string) string {
	if sid, err := currentUserSIDString(); err == nil && sid != "" {
		return `\\.\pipe\mcphub-supervisor-` + sid
	}
	// Fallback for the rare token-query failure path. The pipe is
	// still SDDL-restricted to the owner SID via the listener
	// security descriptor, so an attacker who spoofs USERNAME to
	// land on this branch still cannot connect — they would lack the
	// owner SID in the DACL. Only the discriminator is degraded.
	user := os.Getenv("USERNAME")
	user = strings.ReplaceAll(user, " ", "-")
	if user == "" {
		user = "default"
	}
	return `\\.\pipe\mcphub-supervisor-` + user
}
