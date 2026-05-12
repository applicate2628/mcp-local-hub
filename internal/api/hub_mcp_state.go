// hub_mcp_state.go — Phase 2 Task 2.2 (G4 unified hub MCP).
//
// Atomic write + load helpers for the hub-mcp state files
// (hub-mcp.endpoint.json, hub-mcp-tokens.json, hub-mcp-control.token).
// Both legs route through Phase 1 helpers:
//
//   - writeHubMcpStateFile delegates to SecureWriteClientConfig
//     (internal/api/secure_write_client_config.go). The state files
//     live inside the per-user state-dir, so the parent-dir DACL gate
//     and handle-relative open/rename pipeline apply unchanged.
//   - readHubMcpStateFile delegates to VerifyHubMcpStateDACL
//     (internal/api/hub_mcp_state_dacl.go) BEFORE any bytes are read.
//     The reparse-defeat + handle-bound stat + DACL allowlist gate
//     refuses any file that's a symlink, owned by a different uid,
//     world/group accessible (POSIX), or carries an ALLOW ACE outside
//     the {current-user, LocalSystem, BuiltinAdministrators} allowlist
//     (Windows).
//
// State-file mutation paths (token generate/rotate, endpoint-file
// create, install reconciler) serialize on acquireHubMcpLock — a flock
// over <state-dir>/hub-mcp.lock. Same pattern as the watchdog state
// flock, just under a different leaf.
//
// Caller contract: any error from these helpers is HARD — Phase 2+
// consumers (Tasks 2.3, 2.4, Phase 4 control endpoint, Phase 5
// install reconciler) MUST surface the error to the operator rather
// than silently regenerate state.
//
// Spec: docs/superpowers/specs/2026-05-12-g4-unified-hub-mcp-design-v3.md
// §"Token + endpoint state hardening" (atomic-write + load-time
// validation blocks). Plan: Task 2.2.

package api

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/gofrs/flock"
)

// hubMcpLockFileLeaf names the flock file serializing every state-
// mutating path. One leaf shared by token/endpoint/control writes so
// the order of operations across files is total (no deadlock between
// "I'm rotating tokens" and "I'm publishing a new endpoint" — both
// queue behind the same flock).
const hubMcpLockFileLeaf = "hub-mcp.lock"

// hubMcpEndpointFileLeaf, hubMcpTokensFileLeaf, hubMcpControlTokenFileLeaf
// are the canonical state-file basenames. Listed here so future
// consumers reference one literal each rather than duplicating the
// string. validateStateFileName already accepts these (see
// state_paths_hubmcp_test.go).
const (
	hubMcpEndpointFileLeaf     = "hub-mcp.endpoint.json"
	hubMcpTokensFileLeaf       = "hub-mcp-tokens.json"
	hubMcpControlTokenFileLeaf = "hub-mcp-control.token"
)

// writeHubMcpStateFile writes payload to <state-dir>/<name> atomically
// via SecureWriteClientConfig. The caller is expected to hold
// hub-mcp.lock for the surrounding generate/rotate operation (the
// SecureWriteClientConfig pipeline does its own crypto/rand temp-name
// + O_EXCL + handle-bound rename, so even races against an unrelated
// writer cannot corrupt the destination; the flock exists to serialize
// multi-step state transitions, not to make a single rename atomic).
//
// `name` is validated by validateStateFileName so callers cannot
// escape the state root via path separators or traversal segments.
func writeHubMcpStateFile(name string, payload []byte) error {
	if err := validateStateFileName(name); err != nil {
		return err
	}
	dir, err := DaemonStateDir()
	if err != nil {
		return err
	}
	target := filepath.Join(dir, name)
	if err := SecureWriteClientConfig(target, payload); err != nil {
		return fmt.Errorf("hub-mcp state write %s: %w", name, err)
	}
	return nil
}

// readHubMcpStateFile gates the read on VerifyHubMcpStateDACL first
// (handle-bound uid/mode/DACL check that refuses symlinks, foreign
// owners, and non-allowlist read ACEs) and then loads bytes. The
// extra os.ReadFile call after the verify keeps the existing function
// surface flat — VerifyHubMcpStateDACL closes its handle on return,
// so a tiny race window exists between verify and read on hostile
// hosts. The per-user state-dir DACL/mode is the broader trust
// boundary that protects that window; see spec §"Why every step uses
// dirHandle-relative ops" for the wider analysis.
func readHubMcpStateFile(name string) ([]byte, error) {
	if err := validateStateFileName(name); err != nil {
		return nil, err
	}
	dir, err := DaemonStateDir()
	if err != nil {
		return nil, err
	}
	target := filepath.Join(dir, name)
	if err := VerifyHubMcpStateDACL(target); err != nil {
		return nil, fmt.Errorf("hub-mcp state verify %s: %w", name, err)
	}
	raw, err := os.ReadFile(target)
	if err != nil {
		return nil, fmt.Errorf("hub-mcp state read %s: %w", name, err)
	}
	return raw, nil
}

// acquireHubMcpLock obtains an exclusive flock on
// <state-dir>/hub-mcp.lock. Callers MUST `defer lk.Unlock()` and route
// every state-mutating operation through this lock for the duration
// of the multi-step transition (e.g., load → mutate → write → publish
// for token/instance-id rotations).
//
// Returns the *flock.Flock so callers can release explicitly. The
// flock file itself is created on-demand by gofrs/flock; no separate
// initialization step is required.
func acquireHubMcpLock() (*flock.Flock, error) {
	dir, err := DaemonStateDir()
	if err != nil {
		return nil, err
	}
	lockPath := filepath.Join(dir, hubMcpLockFileLeaf)
	lk := flock.New(lockPath)
	if err := lk.Lock(); err != nil {
		return nil, fmt.Errorf("hub-mcp flock: %w", err)
	}
	return lk, nil
}
