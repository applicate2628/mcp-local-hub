//go:build windows

package gui

import (
	"errors"
	"os/exec"

	"golang.org/x/sys/windows"

	"mcp-local-hub/internal/api"
)

// startDetachedSupervisorTolerant starts the detached supervisor cmd produced
// by build (configureDetached has already set DETACHED|NEW_GROUP|BREAKAWAY).
// CREATE_BREAKAWAY_FROM_JOB lets the manual-restart supervisor escape an
// inherited KILL_ON_JOB_CLOSE job — the same orphan-escape the AUTOMATIC spawn
// paths in internal/cli gained in the §5 fix. Per Microsoft docs, CreateProcess
// FAILS with ERROR_ACCESS_DENIED when the parent job lacks
// JOB_OBJECT_LIMIT_BREAKAWAY_OK; on THAT specific error we retry once with the
// breakaway flag cleared (still detached) so a locked-down corp host never gets
// a hard manual-restart failure — it just loses the orphan-escape there.
//
// §5 residual close: ERROR_ACCESS_DENIED is NOT exclusively the breakaway
// rejection — some hardened / corp-managed hosts (AppLocker / WDAC publisher
// allowlists, restrictive process-creation policy) also deny CreateProcess
// when DETACHED_PROCESS | CREATE_NEW_PROCESS_GROUP are set. In that case the
// breakaway-cleared retry STILL carries the base flags and STILL fails
// ERROR_ACCESS_DENIED, leaving the operator with a hard manual-restart
// failure. So when the breakaway-cleared retry ALSO returns
// ERROR_ACCESS_DENIED, we do ONE final retry with the minimal flag set (all
// CreationFlags cleared — plain spawn). The supervisor then runs as a plain
// child of the GUI process (it loses detach + orphan-escape, so it may not
// outlive a GUI exit), but a running supervisor on a locked-down host beats a
// dead Dashboard with no recovery affordance. The fallback is logged as a
// structured warn event so the degraded posture is operator-visible, mirroring
// the per-spawn Job-Object non-fatal-fallback pattern (ADR #239).
//
// Kept gui-local (mirrors internal/cli's startSupervisorDetachedBreakaway)
// because the gui package must not import internal/cli; folding both onto a
// shared internal/process helper is a tracked future cleanup.
func startDetachedSupervisorTolerant(build func() *exec.Cmd) (*exec.Cmd, error) {
	cmd := build()
	err := cmd.Start()
	if err == nil {
		return cmd, nil
	}
	if !errors.Is(err, windows.ERROR_ACCESS_DENIED) {
		return cmd, err
	}
	retry := build()
	if retry.SysProcAttr != nil {
		retry.SysProcAttr.CreationFlags &^= winCreateBreakawayFromJob
	}
	e2 := retry.Start()
	if e2 == nil {
		return retry, nil
	}
	if !errors.Is(e2, windows.ERROR_ACCESS_DENIED) {
		// A non-ACCESS_DENIED failure on the breakaway-cleared retry is a
		// real error (e.g. binary vanished mid-restart); propagate it rather
		// than masking it with a further flag-stripping retry.
		return retry, e2
	}
	// The base DETACHED|NEW_GROUP flags themselves are being denied. Final
	// retry with the minimal flag set (all CreationFlags cleared); the
	// supervisor still spawns, just without detach / orphan-escape.
	minimal := build()
	if minimal.SysProcAttr != nil {
		minimal.SysProcAttr.CreationFlags = 0
	}
	if e3 := minimal.Start(); e3 != nil {
		return minimal, e3
	}
	_ = api.LogHubMcpEvent("warn", "supervisor-restart-detach-flags-denied", map[string]any{
		"reason":    "CreateProcess denied DETACHED_PROCESS|CREATE_NEW_PROCESS_GROUP (ERROR_ACCESS_DENIED); spawned with minimal flag set",
		"spawned":   true,
		"degraded":  "no-detach-no-orphan-escape",
		"new_pid":   minimal.Process.Pid,
		"breakaway": "already-cleared",
	})
	return minimal, nil
}
