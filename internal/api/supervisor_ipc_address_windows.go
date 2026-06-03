//go:build windows

package api

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"strings"
)

// supervisorIPCTestPipeDiscriminator, when non-nil, supplies a per-test
// pipe-name suffix so concurrent in-process supervisor IPC tests don't contend
// on the shared per-SID pipe. It is nil in production — NO release code path
// assigns it (only EnableSupervisorIPCTestPipeIsolation does, and that is
// called exclusively from test setup) — so a release binary's
// SupervisorIPCAddress is always SID-based. Unlike an env var, a Go package
// var cannot be set by an external caller, so this is STRUCTURALLY test-only.
//
// Codex bot PR #264 P2 (r1 + r2): an env-gated discriminator let production
// clients branch on a caller-controllable channel (an operator who set
// MCPHUB_STATE_DIR_OVERRIDE could redirect the pipe away from the running
// supervisor); a build-tag-gated one dropped the isolation from the DEFAULT
// untagged `go test ./...` build that CI (.github/workflows/ci.yml) and the
// pre-push check run, so the contention flake remained on the main path. A
// runtime func-hook is present in EVERY test build (tagged or not) yet absent
// from production — the only construction that satisfies both.
var supervisorIPCTestPipeDiscriminator func() string

// EnableSupervisorIPCTestPipeIsolation installs the per-test pipe-name
// discriminator (derived from the cli test seam MCPHUB_STATE_DIR_OVERRIDE) so
// the many in-process supervisors a single `go test ./internal/cli/` spins up
// under one user SID each bind a UNIQUE pipe instead of contending on
// `\\.\pipe\mcphub-supervisor-<SID>` (bug
// 2026-05-29-cli-supervise-ipc-tests-flaky-in-full-suite.md). Call it from
// TEST SETUP ONLY (internal/cli TestMain). Production never calls it, so the
// pipe stays SID-based in release. The POSIX build has a no-op counterpart
// (the unix socket is already per-stateDir-unique) so cross-platform test
// setup can call this unconditionally.
func EnableSupervisorIPCTestPipeIsolation() {
	supervisorIPCTestPipeDiscriminator = func() string {
		override := strings.TrimSpace(os.Getenv("MCPHUB_STATE_DIR_OVERRIDE"))
		if override == "" {
			return ""
		}
		sum := sha256.Sum256([]byte(override))
		return hex.EncodeToString(sum[:8])
	}
}

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
// The supervisorIPCTestPipeDiscriminator branch below is nil in production
// (see its doc) — only test setup installs it — so production clients always
// take the SID path.
func SupervisorIPCAddress(_ string) string {
	if supervisorIPCTestPipeDiscriminator != nil {
		if disc := supervisorIPCTestPipeDiscriminator(); disc != "" {
			return `\\.\pipe\mcphub-supervisor-test-` + disc
		}
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
