//go:build windows

package main

import (
	"os"
	"syscall"
)

// ATTACH_PARENT_PROCESS is the sentinel understood by AttachConsole.
const attachParentProcess = ^uint32(0) // (DWORD)-1

var (
	kernel32               = syscall.NewLazyDLL("kernel32.dll")
	procAttachConsole      = kernel32.NewProc("AttachConsole")
	procSetConsoleOutputCP = kernel32.NewProc("SetConsoleOutputCP")
	procSetConsoleCP       = kernel32.NewProc("SetConsoleCP")
)

// attachParentConsoleIfAvailable tries to attach this Windows-subsystem
// process to its parent's console (cmd.exe, PowerShell, etc.). When the
// parent has a console, stdin/stdout/stderr are rewired so plain fmt.Print
// calls work. When there is no parent console (Scheduler, Explorer
// double-click, detached spawn), this returns quietly.
//
// Inherited handles from a shell redirect (e.g. `mcphub.exe > out.txt`) or a
// pipe (e.g. `mcphub.exe | tee`) must be preserved: Windows passes those to
// the child even under the GUI subsystem, and rewiring os.Stdout to
// CONOUT$ on top of a valid inherited handle would send output to the
// attached (hidden) console instead of the redirected target. We only
// reopen a standard stream when the Go runtime reports it invalid —
// i.e. when the GUI subsystem zeroed it out and AttachConsole just
// allocated a fresh console for us.
func attachParentConsoleIfAvailable() {
	if ret, _, _ := procAttachConsole.Call(uintptr(attachParentProcess)); ret != 0 {
		reopenIfInvalid("CONIN$", os.O_RDONLY, &os.Stdin)
		reopenIfInvalid("CONOUT$", os.O_WRONLY, &os.Stdout)
		reopenIfInvalid("CONOUT$", os.O_WRONLY, &os.Stderr)
	}
	// Go source is UTF-8; the default Windows console output code page is
	// OEM (866 on ru_RU locales, 1251 for GUI). When UTF-8 bytes hit a
	// non-UTF-8 console, multi-byte glyphs like ✓/✗/— render as gibberish
	// and some decoded bytes land in C0/C1 control-char range, repositioning
	// the cursor and causing line overlap. Switching the attached console to
	// CP_UTF8 is a no-op when no console is present and effective otherwise.
	const cpUTF8 uintptr = 65001
	_, _, _ = procSetConsoleOutputCP.Call(cpUTF8)
	_, _, _ = procSetConsoleCP.Call(cpUTF8)
}

// reopenIfInvalid rewires *target to the named console device only when
// the current handle is unusable (Stat fails). When stdio was inherited
// from the parent via redirect or pipe, Stat reports the underlying file
// or pipe and we leave the handle alone.
func reopenIfInvalid(name string, mode int, target **os.File) {
	if *target != nil {
		if _, err := (*target).Stat(); err == nil {
			return
		}
	}
	f, err := os.OpenFile("\\\\.\\"+name, mode, 0)
	if err != nil {
		return
	}
	oldFile := *target
	*target = f
	if oldFile != nil {
		_ = oldFile.Close()
	}
}
