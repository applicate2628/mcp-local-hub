//go:build windows

// Package cli — Task 6.1 Windows pipe-path helper for the supervisor
// IPC channel.
//
// Spec §"Q11 Windows: named pipe via github.com/Microsoft/go-winio"
// + plan Task 6.1.
//
// Pipe-path convention (matches the per-user discriminator the
// SDDL allowlist already encodes via api.BuildAllowlistSDDL —
// see supervise_ipc_windows.go):
//
//	\\.\pipe\mcphub-supervisor-<USERNAME>
//
// USERNAME is sanitized to remove spaces. The real per-user-SID
// embedding can land later when the supervisor needs to coexist with
// a service-account variant; for v0.5.0 GA on a single-user dev
// workstation the env-derived username is sufficient — the SDDL on
// the pipe itself enforces owner-only access regardless of the name.
package cli

import "mcp-local-hub/internal/api"

// defaultPipePathOS returns the Windows named-pipe path for the
// supervisor IPC channel. The stateDir argument is ignored on Windows
// (pipes live in the kernel namespace, not on disk); it is accepted
// here to keep the cross-platform call site signature uniform.
func defaultPipePathOS(_ string) string {
	return api.SupervisorIPCAddress("")
}
