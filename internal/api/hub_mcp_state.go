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
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/gofrs/flock"
)

// hubMcpLockRetryDelay is the poll interval acquireHubMcpLockContext
// passes to flock.TryLockContext. Matches the daemon-intent reader's
// retry granularity (internal/api/daemon_intent.go) — short enough to
// react to ctx cancellation within ~10 ms, long enough not to spin on
// a busy disk.
const hubMcpLockRetryDelay = 10 * time.Millisecond

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
	// v0.4.2 fix: state-file writes inherit the v0.4.0
	// secure-write relax lane (PR #185). The parent-dir DACL of
	// %LOCALAPPDATA%\mcp-local-hub can be broadened on real
	// solo-developer workstations (Group Policy, MDM, third-party
	// installers granting access to non-allowlisted SIDs). The
	// strict gate would fail-closed on those hosts and break B4
	// marker writes (PR #187) and other state-file mutations
	// (tokens, hub-mcp control). Symmetric to the relax in
	// verifyHubMcpStateDACLImpl (read path) and
	// secureWriteWithOperatorOpt (client config write path).
	//
	// Post-inode-anchored-read symmetry (security-reviewer F1 on
	// the read-side fix in this commit; see
	// work-items/bugs/2026-05-19-state-file-verify-rejects-write-broadened-parent-dacl.md
	// for background): the read side now tolerates write-broadened
	// parents under default-relax because the read is bound to the
	// parent-handle inode (windows.ReadFile / unix.Read on the
	// handle/fd returned by ntOpenRelative / openat, not a fresh
	// path resolution). The write side gets the symmetric treatment
	// here for two reasons:
	//
	//   1. The hardened write pipeline
	//      (secureWriteClientConfigSkipParentGate) installs the
	//      per-file restrictive DACL on the file HANDLE at
	//      temp-create time, BEFORE any bytes hit disk. A
	//      parent-FILE_DELETE_CHILD attacker can delete the temp
	//      entry but cannot read or modify the in-progress write.
	//   2. The publish step is a dirHandle-relative atomic rename
	//      (FILE_RENAME_REPLACE_IF_EXISTS | FILE_RENAME_POSIX_SEMANTICS
	//      anchored to the verified parent handle on Windows;
	//      handle-bound rename on POSIX), so an attacker swapping
	//      the parent between handle-open and rename cannot
	//      redirect the publish.
	//
	// The previous code re-rejected write-capable parents here to
	// avoid the asymmetry of "publish a file the hub can't read
	// back" — that asymmetry no longer exists post-inode-anchored
	// read, and keeping the rejection here would strand operators
	// in a half-functional state (reads succeed, writes fail) on
	// the very hosts the read-side relaxation was meant to
	// unblock.
	if err := SecureWriteClientConfig(target, payload); err != nil {
		if !errors.Is(err, ErrSecureWriteParentInsecure) {
			return fmt.Errorf("hub-mcp state write %s: %w", name, err)
		}
		if operatorRequiresSingleUserHome() {
			return fmt.Errorf("hub-mcp state write %s: %w; %s=1 is set, so the strict parent-dir gate is enforced (unset that env var, or tighten the parent's DACL to remove the offending principal, to proceed)",
				name, err, RequireSingleUserHomeEnv)
		}
		// Best-effort audit log; never block the write on log
		// failure. Distinguish read-only vs WRITE/DAC-edit
		// broadening in the reason so operators can audit the
		// more-permissive case.
		reason := "default-relax-on-solo-host (parent grants read-only access to non-allowlisted principal)"
		if wsErr := checkStateDirParentWriteSafe(dir); wsErr != nil {
			reason = "default-relax-on-solo-host (parent grants WRITE/DAC-edit access; safe under inode-anchored read+publish because the per-file DACL is handle-bound at temp-create and the atomic rename is dirHandle-relative, so an attacker swapping the directory entry cannot redirect the publish or read the in-progress bytes)"
		}
		_ = LogHubMcpEvent("warn", "hub-mcp-state-write-unhardened-parent-fallback", map[string]any{
			"path":   target,
			"parent": dir,
			"reason": reason,
			"err":    err.Error(),
			"note":   "per-file DACL/mode applied at temp-create time (handle-bound); rename is dirHandle-relative; published file is owner-only regardless of parent DACL",
		})
		if err := secureWriteClientConfigSkipParentGate(target, payload); err != nil {
			return fmt.Errorf("hub-mcp state write %s (relax lane): %w", name, err)
		}
	}
	return nil
}

// maxStateFileBytes caps a single state-file read at 1 MiB. The
// hub-mcp state files (tokens.json, control.token, endpoint.json,
// the various supervisor-*.json files, managed-entries.json) all
// fit comfortably under a few KiB in practice — managed-entries
// scales with the daemon count and was sized to <100 KiB even on
// pathological setups. 1 MiB is the OOM-protection ceiling: an
// attacker who replaces the file with a multi-GiB payload cannot
// exhaust process memory before the read errors out.
const maxStateFileBytes = 1 << 20

// readHubMcpStateFile reads a hub-mcp state file via the inode-
// anchored verify+read pipeline (hub_mcp_state_read_inode_{windows,posix}.go).
// The verify and read steps share the same parent-directory handle
// / fd so a co-resident principal cannot replace the file between
// our check and our read — the read is bound to the verified inode
// regardless of subsequent directory-entry changes.
//
// Pre-v0.4.6 this function called VerifyHubMcpStateDACL (which
// closed its handle on return) and then performed a path-based
// os.ReadFile, leaving a small TOCTOU swap window on hosts whose
// parent directory granted write/delete/DAC-edit access to a
// non-allowlisted SID. The Windows leg's verifyHubMcpStateDACLImpl
// compensated by refusing such parent DACLs even in default-relax
// mode (hub_mcp_state_dacl_windows.go:143-145), which blocked
// demigrate on solo-developer hosts with CodexSandboxUsers or
// orphan AD SID ACEs on %LOCALAPPDATA% — see
// work-items/bugs/2026-05-19-state-file-verify-rejects-write-broadened-parent-dacl.md.
// The inode-anchored read closes the swap window at the kernel
// level, so the parent-DACL gate can safely relax under
// default-relax (strict mode is unchanged).
func readHubMcpStateFile(name string) ([]byte, error) {
	if err := validateStateFileName(name); err != nil {
		return nil, err
	}
	dir, err := DaemonStateDir()
	if err != nil {
		return nil, err
	}
	target := filepath.Join(dir, name)
	raw, err := readStateFileInodeAnchored(target)
	if err != nil {
		return nil, fmt.Errorf("hub-mcp state read %s: %w", name, err)
	}
	return raw, nil
}

// isHubMcpStateMissingErr returns true if err indicates the state
// file does not yet exist on disk. Routine startup paths (first call
// to EnsureHubEndpoint, first call to EnsureHubTokens on a fresh
// install) get a "not found" error from readHubMcpStateFile and treat
// it as "generate fresh state"; every other read error must surface
// to the operator (a corrupt or DACL-violating file is NOT silently
// regenerated per spec §"Bind ordering" step 4).
//
// The check unwraps through the fmt.Errorf wrapping installed by
// readHubMcpStateFile. POSIX bubbles up os.ErrNotExist via syscall;
// Windows surfaces STATUS_OBJECT_NAME_NOT_FOUND /
// STATUS_OBJECT_PATH_NOT_FOUND from the NT relative-open inside
// VerifyHubMcpStateDACL, plus ERROR_FILE_NOT_FOUND /
// ERROR_PATH_NOT_FOUND from os.ReadFile. errors.Is(err,
// os.ErrNotExist) matches the latter; the NTStatus branch is handled
// by isHubMcpStateMissingErrPlatform (defined per-GOOS).
func isHubMcpStateMissingErr(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, os.ErrNotExist) {
		return true
	}
	return isHubMcpStateMissingErrPlatform(err)
}

// AcquireHubMcpLock is the exported entry point for cross-package
// callers (notably internal/cli/install.go::runReconcileHubMode)
// that need to serialize against the same hub-mcp.lock that
// BindHubMcpListener and the token/instance-id rotation flows
// already use.
//
// Returns the *flock.Flock; callers MUST `defer lk.Unlock()`. The
// blocking semantics match acquireHubMcpLock — appropriate for
// CLI flows whose operator can wait on a sibling holder. Long-
// lived process paths (gui-server startup / shutdown) MUST use
// the context-aware variant; cross-package CLI doesn't currently
// need that, so we don't export it.
//
// Issue #161 P2 closure (concurrency lane + endpoint/tokens
// TOCTOU): runReconcileHubMode now wraps the load → snapshot →
// apply transaction inside this lock so two concurrent reconciles
// cannot interleave plan semantics, and a concurrent
// regenerate-token / regenerate-instance-id cannot mutate
// endpoint/tokens between snapshot and apply.
func AcquireHubMcpLock() (*flock.Flock, error) { return acquireHubMcpLock() }

// acquireHubMcpLock obtains an exclusive flock on
// <state-dir>/hub-mcp.lock. Callers MUST `defer lk.Unlock()` and route
// every state-mutating operation through this lock for the duration
// of the multi-step transition (e.g., load → mutate → write → publish
// for token/instance-id rotations).
//
// Returns the *flock.Flock so callers can release explicitly. The
// flock file itself is created on-demand by gofrs/flock; no separate
// initialization step is required.
//
// This is the blocking variant — appropriate for short CLI flows
// (token rotate, install) that genuinely need to wait on a sibling
// writer. Long-lived process paths (gui-server startup / shutdown)
// MUST use acquireHubMcpLockContext so ctx cancellation can unblock
// a stuck acquisition. See codex bot phase4 r10 P1 closure on
// PR #158 for the contention-vs-shutdown analysis.
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

// acquireHubMcpLockContext is the context-aware variant of
// acquireHubMcpLock. Returns ctx.Err() (wrapped) as soon as ctx is
// canceled or its deadline passes, even if a sibling holder of
// hub-mcp.lock has not released yet. The retry cadence is fixed at
// hubMcpLockRetryDelay (10 ms) so observed cancellation latency is
// bounded by ~10 ms regardless of how long the sibling holder takes.
//
// Used by gui-server startup (BindHubMcpListener) and shutdown
// (InternalReloadHandler.Shutdown) so the gui-server's shutdown
// budget actually applies even when another process is sitting on
// the flock — without this, a held lock would freeze gui-server
// teardown indefinitely (codex bot phase4 r10 P1 closure on PR #158).
//
// Mirrors the daemon_intent.TryReadDaemonIntent pattern used by the
// tray aggregator: short retry, context-bounded total wait, distinct
// wrapping of ctx-deadline vs. non-timeout flock errors so callers
// can branch on errors.Is(err, context.DeadlineExceeded) /
// context.Canceled if needed.
func acquireHubMcpLockContext(ctx context.Context) (*flock.Flock, error) {
	if ctx == nil {
		return acquireHubMcpLock()
	}
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("hub-mcp flock: %w", err)
	}
	dir, err := DaemonStateDir()
	if err != nil {
		return nil, err
	}
	lockPath := filepath.Join(dir, hubMcpLockFileLeaf)
	lk := flock.New(lockPath)
	locked, lockErr := lk.TryLockContext(ctx, hubMcpLockRetryDelay)
	if lockErr != nil {
		return nil, fmt.Errorf("hub-mcp flock: %w", lockErr)
	}
	if !locked {
		// flock.TryLockContext returned (false, nil) is not documented
		// in practice (ctx cancellation surfaces via lockErr) but stay
		// defensive: report it as a context error so callers do not
		// proceed thinking they hold the lock.
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, fmt.Errorf("hub-mcp flock: %w", ctxErr)
		}
		return nil, fmt.Errorf("hub-mcp flock: unavailable (TryLockContext returned false)")
	}
	return lk, nil
}
