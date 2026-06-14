//go:build windows

package gui

import (
	"errors"
	"os/exec"

	"golang.org/x/sys/windows"
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
	if e2 := retry.Start(); e2 != nil {
		return retry, e2
	}
	return retry, nil
}
