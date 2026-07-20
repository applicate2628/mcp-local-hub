//go:build windows

package cli

import (
	"os/exec"
	"syscall"

	"mcp-local-hub/internal/process"
)

// configureSupervisorDetach is the Windows variant: spawn the
// supervisor with DETACHED_PROCESS | CREATE_NEW_PROCESS_GROUP so the
// child does NOT inherit the GUI's console / process group. This
// matches the production supervisor autostart contract — the
// supervisor is a long-lived background process whose own Job Object
// owns daemon children, NOT the GUI's process group.
//
// The flag combination intentionally severs CTRL_C_EVENT propagation
// from the GUI to the supervisor; graceful shutdown is exclusively
// via IPC `exit{graceful:true}`. See CLAUDE.md "Cold-restart upgrade
// flow" note on the same `DETACHED_PROCESS | CREATE_NEW_PROCESS_GROUP`
// combination.
//
// The HideWindow attribute additionally suppresses any console flash
// — defense-in-depth against a future supervisor-side regression that
// drops the `-H windowsgui` linker flag.
//
// DETACHED_PROCESS IS NOT SUFFICIENT ON ITS OWN, and assuming it was is
// what made the GUI-spawned supervisor die with its launching terminal.
// The flag blocks console INHERITANCE at create time; it does not stop
// the child from calling AttachConsole(ATTACH_PARENT_PROCESS) afterwards
// — and the child here is the SAME binary, whose main() does exactly
// that as its first statement. Measured externally against a real
// `-H windowsgui` build: a detached child of a console-holding parent
// appears in that parent's GetConsoleProcessList, i.e. it is a full
// console client and a CTRL_CLOSE_EVENT target, and closing the terminal
// then reaps every daemon under its Job Object. So the detach is
// expressed as BOTH halves here, in one place, and neither half is
// optional. See work-items/bugs/2026-07-20-gui-spawned-supervisor-console-client.md.
//
// This is also the respawn path: the manager's spawn seam resolves to
// ensureSupervisorRunning, which builds every replacement supervisor cmd
// through this same hook.
func configureSupervisorDetach(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.CreationFlags |= windowsDetachedProcess | windowsCreateNewProcessGroup
	cmd.SysProcAttr.HideWindow = true
	process.SuppressConsoleAttach(cmd)
}

// Windows process creation flag constants. The standard library
// exposes some via golang.org/x/sys/windows but not these specific
// values — copy from the Win32 docs to avoid an extra import for
// just two constants.
const (
	windowsDetachedProcess       = 0x00000008
	windowsCreateNewProcessGroup = 0x00000200
)
