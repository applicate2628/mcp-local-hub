//go:build windows

package process

import (
	"os/exec"
	"syscall"

	"golang.org/x/sys/windows"
)

// HideToolConsole gives a background tool a hidden console that its ordinary console
// children can inherit. CREATE_NO_WINDOW on the tool itself does not propagate
// to compiler, linker or other tool subprocesses. Runtime helpers without a
// tool process tree should continue to use NoConsole.
// The caller retains the existing start, wait, stream and cleanup responsibilities.
func HideToolConsole(cmd *exec.Cmd) {
	if cmd == nil {
		return
	}
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.HideWindow = true
	cmd.SysProcAttr.CreationFlags &^= windows.CREATE_NO_WINDOW | windows.DETACHED_PROCESS
	cmd.SysProcAttr.CreationFlags |= windows.CREATE_NEW_CONSOLE
}
