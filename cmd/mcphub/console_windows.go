//go:build windows

package main

import (
	"os"
	"syscall"

	"mcp-local-hub/internal/process"
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
//
// Returns whether this process is a console CLIENT on return — the
// property that decides whether a closing terminal can kill it, and
// therefore whether the long-lived GUI path must release the console.
// The answer is measured (process.HasConsole), not inferred from
// AttachConsole's return value: the two are not the same question, and
// only the measured one is safe to act on.
//
// A process spawned with the suppression marker set (a detached
// background child — see process.SuppressConsoleAttachEnv) never
// attaches at all, so it can never become a CTRL_CLOSE_EVENT target for
// the console its parent happened to be holding at spawn time.
func attachParentConsoleIfAvailable() bool {
	if process.ConsoleAttachSuppressed() {
		// Deliberately BEFORE the AttachConsole call and before the
		// code-page calls: the whole point is that this process must
		// never become a console client, not that it tidies up after
		// becoming one. There is no console, so there is no code page
		// to set and no CONOUT$ worth reopening.
		return false
	}
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
	return process.HasConsole()
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
