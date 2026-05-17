// state_file_helper.go — v0.5.0 Fix Group 5 hardened-pipeline rewrite.
//
// WriteStateFileAtomic writes payload as JSON to path through the
// SAME handle-relative, DACL-bound pipeline used for token-bearing
// client config files (SecureWriteClientConfig). Three defenses the
// previous pre-v0.5.0 implementation lacked are now in place:
//
//   1. Per-file flock (`<path>.lock`) serializes concurrent writes
//      across goroutines AND across processes. Mirrors the watchdog
//      and hub-mcp.lock pattern.
//
//   2. Restrictive DACL installed on the file HANDLE before bytes
//      hit disk — closes the pre-hardening race window an
//      os.CreateTemp + path-based SetNamedSecurityInfo lane would
//      leave open. On POSIX the equivalent is O_CREAT mode 0600 plus
//      Fchmod(0600). The post-rename re-verify catches policy ACLs
//      that may auto-apply on some Windows paths.
//
//   3. MCPHUB_REQUIRE_SINGLE_USER_HOME=1 enforcement. On corp-managed
//      Windows hosts the operator can demand the strict parent-dir
//      gate; absent that env var, the default-relax lane proceeds —
//      but never silently. A warn event
//      "state-file-write-unhardened-fallback" lands in hub-mcp.log
//      (distinct from "client-write-unhardened-fallback" so audit
//      filters can separate the two policy domains).
//
// Lane E P0 (deep-sec on Group 5): the previous WriteStateFileAtomic
// used os.CreateTemp + os.Rename with no DACL apply, no symlink
// refusal, and no parent-dir gate. On a corp-managed Windows host
// where a co-resident has FILE_DELETE_CHILD on the parent dir, an
// attacker could replace supervisor-intent.json between the writer's
// rename and the supervisor's read — feeding attacker-controlled
// Command/Args/Env to SpawnFunc. The hardened pipeline closes that
// window on the WRITE side. (Read-side hardening — AC7 — is tracked
// separately.)

package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/gofrs/flock"
)

// WriteStateFileAtomic writes payload as JSON to path through the
// hardened secure-write pipeline (SecureWriteClientConfig chain).
//
// Pipeline:
//
//   1. MkdirAll(filepath.Dir(path), 0o700) to guarantee a parent
//      directory exists with owner-only mode.
//   2. json.MarshalIndent(payload, "", "  ") — same on-disk
//      formatting as v0.4.x to keep human-readable state files.
//   3. flock.New(path + ".lock").Lock() — serializes concurrent
//      writers across processes. Defer release.
//   4. secureWriteStateFileWithOperatorOpt(path, raw) — delegates to
//      SecureWriteClientConfig with per-domain audit-event wiring
//      (state-file-write-unhardened-fallback vs.
//      client-write-unhardened-fallback).
//
// Callers do NOT need to acquire any per-file lock themselves; this
// helper does it. Multi-step state transitions that need to lock
// across files should serialize at a higher level (e.g., hub-mcp.lock
// for token/endpoint rotations).
func WriteStateFileAtomic(path string, payload any) error {
	raw, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("mkdir: %w", err)
	}

	// Per-file flock. The lock leaf lives alongside the destination
	// so the cross-process boundary is the same as the destination
	// path; gofrs/flock retains the file after Unlock by design
	// (the file IS the lock), so subsequent writers acquire the
	// same on-disk leaf.
	lockPath := path + ".lock"
	lk := flock.New(lockPath)
	if err := lk.Lock(); err != nil {
		return fmt.Errorf("state-file flock %s: %w", lockPath, err)
	}
	defer func() { _ = lk.Unlock() }()

	return secureWriteStateFileWithOperatorOpt(path, raw)
}

// secureWriteStateFileWithOperatorOpt is the state-file analogue of
// secureWriteWithOperatorOpt (internal/api/client_write_init.go).
// Same hardened-pipeline + operator-opt-in policy, with a DISTINCT
// audit event name so the two policy domains stay separable in the
// audit log.
//
// Pipeline:
//
//   1. SecureWriteClientConfig(path, payload) — full hardened path
//      with parent-dir DACL/mode gate.
//   2. On ErrSecureWriteParentInsecure:
//      a. If operatorRequiresSingleUserHome() returns true, return
//         the strict error verbatim (with an env-var hint appended
//         so the operator knows which knob to flip).
//      b. Else, run checkStateDirParentWriteSafe(filepath.Dir(path)).
//         If that returns non-nil — the parent grants write/delete
//         access to a non-allowlisted principal — surface the
//         "TOCTOU swap risk" error and refuse the write. This
//         mirrors the writeHubMcpStateFile defense (codex bot r6 P1
//         on PR #192) so the WRITE side and the future hardened
//         READ side stay symmetric.
//      c. Else, log the warn event
//         "state-file-write-unhardened-fallback" and re-run the
//         pipeline via secureWriteClientConfigSkipParentGate(path,
//         payload). The per-file DACL/mode hardening still applies
//         at temp-create time (handle-bound), so the published file
//         is owner-only regardless of parent DACL.
//
// All other secure-write failures (open temp, write, rename, post-
// rename DACL re-verify, pre-existing symlink/reparse-point at
// destination) propagate unchanged.
func secureWriteStateFileWithOperatorOpt(path string, payload []byte) error {
	err := SecureWriteClientConfig(path, payload)
	if err == nil {
		return nil
	}
	if !errors.Is(err, ErrSecureWriteParentInsecure) {
		return err
	}
	if operatorRequiresSingleUserHome() {
		return fmt.Errorf("state-file secure write %s: %w; %s=1 is set, so the strict parent-dir gate is enforced (unset that env var, or tighten the parent's DACL to remove the offending principal, to proceed)",
			path, err, RequireSingleUserHomeEnv)
	}

	// Default-relax lane: the parent-dir gate rejected. Before
	// proceeding to the skip-gate writer, run the narrower write-
	// bits check — a parent that grants write/delete to a non-
	// allowlisted principal is a TOCTOU swap risk regardless of
	// strict mode. Symmetric with writeHubMcpStateFile's defense.
	parentDir := filepath.Dir(path)
	if wsErr := checkStateDirParentWriteSafe(parentDir); wsErr != nil {
		return fmt.Errorf("state-file secure write %s: parent %s grants write/delete access to non-allowlisted principal (TOCTOU swap risk; the read side would refuse this parent regardless of mode): %w",
			path, parentDir, wsErr)
	}

	// Read-only broadening: emit the distinct state-file audit event
	// and proceed via the skip-parent-gate hardened writer. The
	// per-file DACL apply at create time still produces an owner-
	// only file.
	if logErr := LogHubMcpEvent("warn", "state-file-write-unhardened-fallback", map[string]any{
		"path":   path,
		"parent": parentDir,
		"reason": "default-relax-on-solo-host (parent grants only read/exec to non-allowlisted principal; write/delete bits cleared)",
		"err":    err.Error(),
		"note":   "per-file DACL/mode still applied at temp-create time (handle-bound), so the published file is owner-only regardless of parent DACL",
	}); logErr != nil {
		// Best-effort: never block the write on a log failure.
		_ = logErr
	}
	return secureWriteClientConfigSkipParentGate(path, payload)
}
