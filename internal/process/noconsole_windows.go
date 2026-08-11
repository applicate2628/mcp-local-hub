//go:build windows

// Package process collects cross-platform helpers for spawning child
// processes from mcphub. The Windows build needs SysProcAttr tweaks
// to keep the GUI/headless parents from popping a console window for
// every utility invocation.
package process

import (
	"os/exec"
	"syscall"

	"golang.org/x/sys/windows"
)

// NoConsole configures cmd so launching it on Windows does NOT pop a
// console window. Required when the parent is built with
// `windowsgui` subsystem (no attached console) — the kernel
// otherwise allocates a fresh console for every child, surfacing as
// empty Terminal tabs / blank cmd windows that flicker open and
// close. Apply at every exec.Command site for: daemon subprocess
// spawn (host.go, http_host.go, launcher.go), scheduler ops
// (schtasks Create/Delete/Run/End/Query), kill ops (taskkill),
// status queries (wmic, netstat, powershell).
//
// Idempotent — re-calling adds the flag a second time which the
// kernel ignores.
//
// CREATE_NO_WINDOW suppresses a visible console window while preserving the
// normal child environment and redirected stdio behavior. DETACHED_PROCESS is
// deliberately not a common default: a target-Windows A/B probe showed that it
// changes PowerShell/CIM local-time interpretation, breaking process-identity
// timestamps by the host UTC offset.
func NoConsole(cmd *exec.Cmd) {
	if cmd == nil {
		return
	}
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.HideWindow = true
	cmd.SysProcAttr.CreationFlags |= windows.CREATE_NO_WINDOW
}
