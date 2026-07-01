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
//   3. Strict-mode enforcement. On corp-managed Windows hosts the
//      operator can demand the strict parent-dir gate either via
//      MCPHUB_REQUIRE_SINGLE_USER_HOME=1 or persisted
//      supervisor-intent.json strict_mode=true. When neither strict
//      source is active, the default-relax lane proceeds — but never
//      silently. A warn event
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
	"time"

	"github.com/gofrs/flock"
)

// WriteStateFileAtomic writes payload as JSON to path through the
// hardened secure-write pipeline (SecureWriteClientConfig chain).
//
// Pipeline:
//
//  1. MkdirAll(filepath.Dir(path), 0o700) to guarantee a parent
//     directory exists with owner-only mode.
//  2. json.MarshalIndent(payload, "", "  ") — same on-disk
//     formatting as v0.4.x to keep human-readable state files.
//  3. flock.New(path + ".lock").Lock() — serializes concurrent
//     writers across processes. Defer release.
//  4. secureWriteStateFileWithOperatorOpt(path, raw) — delegates to
//     SecureWriteClientConfig with per-domain audit-event wiring
//     (state-file-write-unhardened-fallback vs.
//     client-write-unhardened-fallback).
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
	return WriteStateFileBytesAtomic(path, raw)
}

// WriteStateFileBytesAtomic writes an already-marshaled state-file payload
// through the same hardened state-file pipeline as WriteStateFileAtomic.
//
// This exists for state files whose on-disk format is not JSON (for example,
// operator-authored YAML) but that still need the state-file parent-DACL relax
// policy, audit event, atomic temp+rename write, and per-file flock.
func WriteStateFileBytesAtomic(path string, raw []byte) error {
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

	return WriteStateFileBytesLockHeld(path, raw)
}

// WriteStateFileBytesLockHeld writes an already-marshaled state-file payload
// through the hardened state-file secure writer without acquiring the per-file
// flock. Callers use this only for read-modify-write helpers that already hold
// path + ".lock"; otherwise use WriteStateFileBytesAtomic.
func WriteStateFileBytesLockHeld(path string, raw []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("mkdir: %w", err)
	}
	return secureWriteStateFileWithOperatorOpt(path, raw)
}

// secureWriteStateFileWithParentRelax is the SINGLE OWNER of the strict-vs-relax
// parent-directory-DACL-gate decision shared by every state-file write path
// (deep-review P3 arch-abstraction): writeHubMcpStateFile and
// secureWriteStateFileWithOperatorOpt previously open-coded the SAME four-step
// gate, so a future security change to the gate risked touching only one path.
// The security-critical decision now lives here once:
//
//  1. SecureWriteClientConfig(path, payload) succeeds → done.
//  2. A non-parent-gate failure (open temp, write, rename, post-rename DACL
//     re-verify, pre-existing symlink/reparse-point at destination) →
//     propagate, wrapped with `label`.
//  3. Strict mode (operatorRequiresSingleUserHome — MCPHUB_REQUIRE_SINGLE_USER_HOME=1
//     or a persisted strict_mode) → hard-fail with the strict-mode remediation
//     hint appended.
//  4. A wrong-owner parent (stateFileParentGateAllowsDefaultRelax == false) →
//     propagate, wrapped — only an OWNER-CORRECT broadened parent may relax.
//  5. Default solo-host relax → emit the caller's domain audit event, then
//     re-run via secureWriteClientConfigSkipParentGate. The per-file owner-only
//     DACL/mode is installed handle-bound at temp-create and the publish is
//     dir-handle-relative, so the destination file is owner-only regardless of
//     the parent DACL/mode.
//
// The caller supplies only the domain-specific bits: `label` identifies the
// write in the operator error message, and `emitRelaxAudit(path, parentDir,
// gateErr)` records the relax-lane fallback to the caller's policy-domain audit
// channel (hub-mcp.log for hub-mcp state, supervisor-events.log for supervisor
// state) — the two audit domains stay separable while the GATE decision is one.
func secureWriteStateFileWithParentRelax(path string, payload []byte, label string, emitRelaxAudit func(path, parentDir string, gateErr error)) error {
	err := SecureWriteClientConfig(path, payload)
	if err == nil {
		return nil
	}
	if !errors.Is(err, ErrSecureWriteParentInsecure) {
		return fmt.Errorf("%s: %w", label, err)
	}
	if operatorRequiresSingleUserHome() {
		return fmt.Errorf("%s: %w; strict mode is active (via %s=1, or via persisted supervisor-intent.json strict_mode set by `mcphub strict-mode enable`), so the strict parent-dir gate is enforced (unset that env var or run `mcphub strict-mode disable`, or tighten the parent's DACL to remove the offending principal, to proceed)",
			label, err, RequireSingleUserHomeEnv)
	}
	if !stateFileParentGateAllowsDefaultRelax(err) {
		return fmt.Errorf("%s: %w", label, err)
	}
	emitRelaxAudit(path, filepath.Dir(path), err)
	if err := secureWriteClientConfigSkipParentGate(path, payload); err != nil {
		return fmt.Errorf("%s (relax lane): %w", label, err)
	}
	return nil
}

// secureWriteStateFileWithOperatorOpt is the state-file analogue of
// secureWriteWithOperatorOpt (internal/api/client_write_init.go). It delegates
// the strict-vs-relax parent-gate decision to the shared single owner
// secureWriteStateFileWithParentRelax and supplies the state-file domain's audit
// event via emitStateFileFallbackEvent.
//
// Audit channel: supervisor-events.log (NOT hub-mcp.log). State-file fallbacks
// are supervisor-domain events — operators monitoring supervisor-events.log for
// audit-posture downgrades must see the relax-lane fire there, alongside the
// lifecycle / IPC / restart-policy events that share the same envelope.
// hub-mcp.log remains the audit channel for client-config fallbacks
// ("client-write-unhardened-fallback") so the two policy domains stay separable.
// Spec §Q13: supervisor-events.log is the canonical audit log for
// supervisor-domain events with the {schema_version, ts, severity, source,
// event, task_name, body, _truncated} envelope.
func secureWriteStateFileWithOperatorOpt(path string, payload []byte) error {
	return secureWriteStateFileWithParentRelax(path, payload,
		fmt.Sprintf("state-file secure write %s", path),
		emitStateFileFallbackEvent)
}

// stateFileParentGateAllowsDefaultRelax is the single parent-relax decision for
// state-file read/write lanes. Call it only with parent-gate errors: wrong-owner
// parents never relax, while owner-correct broadened parents may enter the
// default solo-host relax lane when the caller's stricter mode permits it.
func stateFileParentGateAllowsDefaultRelax(err error) bool {
	if err == nil || errors.Is(err, ErrWrongOwner) {
		return false
	}
	return errors.Is(err, ErrSecureWriteParentInsecure) ||
		errors.Is(err, ErrTooLoose) ||
		errors.Is(err, ErrDaclOutsideAllowlist)
}

// emitStateFileFallbackEvent records the audit warning for the
// state-file default-relax lane. Primary channel:
// <state-dir>/supervisor-events.log via OpenSupervisorEventLog.
//
// Fallback channel: hub-mcp.log via LogHubMcpEvent — used only when
// state-dir resolution fails (the supervisor-events.log path cannot
// be constructed). A stderr warning records the audit-channel
// degradation so an operator running with `2>` redirection still
// sees the security-boundary downgrade even when the primary
// channel is unreachable.
//
// Both channels are best-effort: supervisor-events.log uses TryEmit so lock
// contention never blocks the underlying state-file write. The pipeline's
// correctness rests on the file-level DACL/mode hardening, not on whether the
// audit row landed; absence of an audit row is dashboard-visible (no event would
// appear) but the data itself is still owner-only.
func emitStateFileFallbackEvent(path, parentDir string, gateErr error) {
	body := map[string]any{
		"path":   path,
		"parent": parentDir,
		"reason": "default-relax-on-solo-host (parent-dir gate rejected; hardened skip-parent-gate writer still applies per-file owner-only permissions at temp-create and publishes via a dir-handle-relative atomic rename)",
		"err":    gateErr.Error(),
		"note":   "strict mode still hard-fails; default-relax preserves the hardened writer while skipping only the parent gate",
	}

	stateDir, sdErr := DaemonStateDir()
	if sdErr != nil {
		// Audit-channel degradation: state-dir resolution failed, so
		// supervisor-events.log path cannot be built. Fall back to
		// hub-mcp.log so the event lands SOMEWHERE rather than
		// silently dropped, and tag the body so log analyzers can
		// detect the degraded path.
		fmt.Fprintf(os.Stderr, "mcphub: state-file-write-unhardened-fallback: supervisor-events.log unreachable (state-dir resolve failed: %v); recording in hub-mcp.log instead\n", sdErr)
		body["audit_channel_degraded"] = fmt.Sprintf("state-dir resolve failed: %v", sdErr)
		_ = LogHubMcpEvent("warn", "state-file-write-unhardened-fallback", body)
		return
	}

	eventsPath := filepath.Join(stateDir, SupervisorEventLogFileLeaf)
	logger, openErr := OpenSupervisorEventLog(eventsPath)
	if openErr != nil {
		// OpenSupervisorEventLog is currently failure-free (it does
		// no I/O on construction), but the API surface returns an
		// error to leave room for future implementations that
		// validate the parent dir at open time. Fall back to
		// hub-mcp.log on construction failure so the row is not
		// dropped.
		fmt.Fprintf(os.Stderr, "mcphub: state-file-write-unhardened-fallback: supervisor-events.log open failed (%v); recording in hub-mcp.log instead\n", openErr)
		body["audit_channel_degraded"] = fmt.Sprintf("open supervisor-events.log failed: %v", openErr)
		_ = LogHubMcpEvent("warn", "state-file-write-unhardened-fallback", body)
		return
	}
	defer func() { _ = logger.Close() }()

	if emitErr := logger.TryEmit(SupervisorEvent{
		SchemaVersion: SupervisorEventSchemaVersion,
		TS:            time.Now().UTC().Format(time.RFC3339Nano),
		Severity:      SupervisorEventSeverityWarn,
		Source:        "state-file-helper",
		Event:         "state-file-write-unhardened-fallback",
		Body:          body,
	}); emitErr != nil {
		// Emit failure is best-effort. Surface to stderr so an
		// operator running with `2>` capture sees the audit-channel
		// degradation even when both channels are unreachable.
		fmt.Fprintf(os.Stderr, "mcphub: state-file-write-unhardened-fallback: supervisor-events.log emit failed (%v); recording in hub-mcp.log instead\n", emitErr)
		body["audit_channel_degraded"] = fmt.Sprintf("emit to supervisor-events.log failed: %v", emitErr)
		_ = LogHubMcpEvent("warn", "state-file-write-unhardened-fallback", body)
	}
}

// CheckStateDirParentWriteSafe is the exported shim over the
// platform-specific unexported checkStateDirParentWriteSafe helper
// (hub_mcp_state_parent_write_check_windows.go /
// hub_mcp_state_parent_write_check_posix.go). External callers in
// the daemon_env_overlay subpackage need to reach the same gate the
// state-file write pipeline uses so the read-side parent-DACL gate
// stays symmetric with the write side.
//
// The shim does NOT change behavior — it forwards to the package-
// local helper. Keeping the original as the canonical
// implementation minimizes the change blast radius (no rename, no
// move).
//
// Spec: docs/superpowers/specs/2026-05-19-servers-matrix-lsp-and-env-revamp-design.md
// §"Read-side hardening" (B-V4-1, B-V4-4) — references the same
// parent-DACL gate as the write side.
func CheckStateDirParentWriteSafe(parentDir string) error {
	return checkStateDirParentWriteSafe(parentDir)
}
