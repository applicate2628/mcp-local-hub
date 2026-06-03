//go:build windows

package api

import (
	"crypto/sha256"
	"encoding/hex"
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
// `go test ./internal/cli/` invocation; absent a per-test
// discriminator they ALL bind `\\.\pipe\mcphub-supervisor-<SID>` and
// contend — a second listener's ListenPipe collides with the first's
// still-open handle, and a client may dial a leftover supervisor from
// a prior test. POSIX is unaffected (the socket lives at
// <stateDir>/supervisor.sock, already per-test-unique). To isolate
// only the test process WITHOUT changing the production wire name,
// when the documented cli test seam MCPHUB_STATE_DIR_OVERRIDE is set
// (production never sets it — see internal/cli/supervise.go
// stateDirFunc) we fold a short hash of that per-test state dir into
// the pipe name. Both the listener and any client in the same test
// process observe the same process-global env var, so they converge
// on the same per-test pipe; production (env unset) is byte-identical
// to the SID/USERNAME path above.
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

// testPipeDiscriminator returns a stable per-test pipe-name suffix
// derived from the cli test seam MCPHUB_STATE_DIR_OVERRIDE, or "" when
// the seam is unset (the production path). It is a no-op in production
// because production never sets that env var (the override lives only
// at the cli stateDirFunc test seam). The 16-hex-char SHA-256 prefix
// is collision-safe across the handful of temp dirs a single test
// binary creates and is a valid Windows pipe-name leaf (no path
// separators, well under the 256-char limit).
func testPipeDiscriminator() string {
	override := strings.TrimSpace(os.Getenv("MCPHUB_STATE_DIR_OVERRIDE"))
	if override == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(override))
	return hex.EncodeToString(sum[:8])
}
