// parent_check.go — read-side parent-directory DACL/mode gate for
// the overlay-file load path (Task 2.4 of the v0.5.x Servers matrix
// revamp).
//
// Symmetric with the WRITE-side gate in
// internal/api/state_file_helper.go (secureWriteStateFileWithOperatorOpt).
// Three modes:
//
//   - Strict mode (MCPHUB_REQUIRE_SINGLE_USER_HOME=1): if the
//     parent-DACL check rejects, return that error verbatim.
//     Corp-managed / multi-tenant hosts opt INTO this posture.
//
//   - Explicit relax (MCPHUB_ALLOW_UNHARDENED_STATE_READ=1, strict
//     mode NOT set): operator has accepted the broadening and the
//     gate returns nil regardless of the parent DACL.
//
//   - Default mode (neither env var set): if the parent-DACL check
//     rejects, refuse the read. The write side defaults to relax-
//     on-rejection, but the read side cannot fall back the same way
//     — the write side mitigates by installing an owner-only DACL on
//     the file at CREATE time (the parent broadening is invisible to
//     the file's own ACL), but the read side has no equivalent
//     handle-bound mitigation: the bytes have already been written
//     and the file's directory entry can have been swapped by a
//     co-resident principal between write and read. Default-mode
//     read REFUSAL is what the spec calls for.
//
// Spec: docs/superpowers/specs/2026-05-19-servers-matrix-lsp-and-env-revamp-design.md
// §"Read-side hardening" (B-V4-1, B-V4-4); also see
// internal/api/state_file_helper.go for the write-side mirror.

package daemon_env_overlay

import (
	"fmt"
	"os"
	"strings"

	api "mcp-local-hub/internal/api"
)

// checkStateDirParentReadSafe inspects the overlay-file's parent
// directory and decides whether the read should proceed. The
// operator's env-var posture determines what happens when the
// underlying api.CheckStateDirParentWriteSafe gate rejects:
//
//   - Strict mode wins: a strict reject is returned verbatim.
//   - Else explicit opt-out wins: return nil.
//   - Else default mode: rejects propagate to the caller (Load),
//     which surfaces them as an overlay-load error.
//
// This is the inverse of the WRITE-side default-relax behavior in
// secureWriteStateFileWithOperatorOpt: the write side falls back to
// the skip-parent-gate hardened writer because the per-file DACL
// hardening on create still produces an owner-only file regardless
// of the parent's ACL. The read side has no equivalent fallback —
// once the bytes are on disk, a co-resident principal with parent
// FILE_DELETE_CHILD can swap the directory entry between write and
// read. The default-mode REFUSAL closes that swap window.
func checkStateDirParentReadSafe(dir string) error {
	gateErr := api.CheckStateDirParentWriteSafe(dir)
	if api.OperatorRequiresSingleUserHome() {
		if gateErr != nil {
			return fmt.Errorf("read-side parent gate: parent %s not single-user safe: %w; strict mode is active (via %s=1, or via persisted supervisor-intent.json strict_mode set by `mcphub strict-mode enable`), so the strict parent-dir gate is enforced (unset that env var or run `mcphub strict-mode disable`, or tighten the parent's DACL to remove the offending principal, to proceed)",
				dir, gateErr, "MCPHUB_REQUIRE_SINGLE_USER_HOME")
		}
		return nil
	}
	if operatorAllowsUnhardenedStateRead() {
		// Audit: operator explicitly accepted the broadened parent
		// DACL via MCPHUB_ALLOW_UNHARDENED_STATE_READ=1. Emit the
		// spec-mandated `daemon-env-overlay-read-unhardened-fallback`
		// event so the audit log records the bypass decision even
		// though the function returns nil. Only emit when there was
		// an actual gate failure — a passing gate doesn't need the
		// "fallback" framing.
		if gateErr != nil {
			_ = api.LogHubMcpEvent("warn", "daemon-env-overlay-read-unhardened-fallback", map[string]any{
				"parent_dir": dir,
				"gate_err":   gateErr.Error(),
			})
		}
		return nil
	}
	if gateErr != nil {
		return fmt.Errorf("read-side parent gate: parent %s grants access to non-allowlisted principal (TOCTOU swap risk; set %s=1 to opt into the relax lane explicitly, or tighten the parent's DACL): %w",
			dir, AllowUnhardenedStateReadEnv, gateErr)
	}
	return nil
}

// operatorAllowsUnhardenedStateRead reports whether the operator has
// explicitly opted INTO the read-side parent-DACL relax via the
// AllowUnhardenedStateReadEnv env var. Accepts "1" and "true" case-
// insensitively; everything else (including unset) returns false.
//
// Symmetric with operatorAllowsUnhardenedStateWrite in
// internal/api/client_write_init.go.
func operatorAllowsUnhardenedStateRead() bool {
	v := strings.TrimSpace(os.Getenv(AllowUnhardenedStateReadEnv))
	return v == "1" || strings.EqualFold(v, "true")
}
