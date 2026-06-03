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
//
// Test isolation (Windows-only flakiness fix, bug
// 2026-05-29-cli-supervise-ipc-tests-flaky-in-full-suite.md): the
// SID-based name is per-USER, which is exactly right in production
// (one supervisor per user). But the cli supervise IPC tests spin up
// many in-process supervisors under the SAME user/SID within one
// `go test ./internal/cli/` invocation; absent a per-test discriminator
// they ALL bind `\\.\pipe\mcphub-supervisor-<SID>` and contend. POSIX is
// unaffected (the socket lives at <stateDir>/supervisor.sock, already
// per-test-unique). testPipeDiscriminator folds a hash of the per-test
// state dir into the pipe name — but it is BUILD-TAG-GATED to
// `test_state_path_env` (see supervisor_ipc_pipe_disc_{testenv,release}_windows.go),
// the SAME tag that gates MCPHUB_STATE_DIR_OVERRIDE state resolution. A
// RELEASE binary (built without the tag) compiles in the no-op variant that
// always returns "", so NO production client ever branches on the env var.
//
// Codex bot PR #264 P2 closure: an ungated env read would let an operator
// who set MCPHUB_STATE_DIR_OVERRIDE in a production shell dial a
// `-test-<hash>` pipe that an autostart supervisor (started without that
// env) isn't listening on. Build-tag gating removes the branch from release
// binaries entirely.
func SupervisorIPCAddress(_ string) string {
	if disc := testPipeDiscriminator(); disc != "" {
		return `\\.\pipe\mcphub-supervisor-test-` + disc
	}
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
