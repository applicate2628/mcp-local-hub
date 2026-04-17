//go:build windows

package cli

import (
	"os/exec"
	"syscall"
)

// applyNoWindow sets CREATE_NO_WINDOW so lldb.exe doesn't flash a console
// window when spawned from a windowless parent (scheduler-launched
// daemon, detached GUI invocation, etc.). Mirrors bridge.py's
// `subprocess.CREATE_NO_WINDOW` usage.
func applyNoWindow(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.CreationFlags |= 0x08000000 // CREATE_NO_WINDOW
}
