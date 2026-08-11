//go:build windows

package main

import (
	"fmt"
	"os"
	"syscall"

	"mcp-local-hub/internal/cli"
)

// ATTACH_PARENT_PROCESS is the sentinel understood by AttachConsole.
const attachParentProcess = ^uint32(0) // (DWORD)-1

var (
	kernel32               = syscall.NewLazyDLL("kernel32.dll")
	procAttachConsole      = kernel32.NewProc("AttachConsole")
	procAllocConsole       = kernel32.NewProc("AllocConsole")
	procSetConsoleOutputCP = kernel32.NewProc("SetConsoleOutputCP")
	procSetConsoleCP       = kernel32.NewProc("SetConsoleCP")
)

const WindowsDebugConsoleUnavailableID = "E_WINDOWS_DEBUG_CONSOLE_UNAVAILABLE"

type windowsConsoleAPI struct {
	attachParent func() error
	allocate     func() error
	prepare      func()
}

type windowsDebugConsoleUnavailableError struct {
	attachErr   error
	allocateErr error
}

func (e *windowsDebugConsoleUnavailableError) Error() string {
	return fmt.Sprintf("%s: attach parent: %v; allocate: %v", WindowsDebugConsoleUnavailableID, e.attachErr, e.allocateErr)
}

func (e *windowsDebugConsoleUnavailableError) FailureID() string {
	return WindowsDebugConsoleUnavailableID
}

var productionWindowsConsoleAPI = windowsConsoleAPI{
	attachParent: func() error {
		ret, _, callErr := procAttachConsole.Call(uintptr(attachParentProcess))
		if ret == 0 {
			return callErr
		}
		return nil
	},
	allocate: func() error {
		ret, _, callErr := procAllocConsole.Call()
		if ret == 0 {
			return callErr
		}
		return nil
	},
	prepare: prepareDebugConsole,
}

// applyWindowsConsolePolicy is the sole Windows console-state writer. Disabled
// mode is a true no-op. Explicit mode attaches to the parent first and falls
// back to allocating a console; failure is returned before Cobra executes.
func applyWindowsConsolePolicy(policy cli.WindowsConsolePolicy) (bool, error) {
	return applyWindowsConsolePolicyWithAPI(policy, productionWindowsConsoleAPI)
}

func applyWindowsConsolePolicyWithAPI(policy cli.WindowsConsolePolicy, api windowsConsoleAPI) (bool, error) {
	if policy == cli.WindowsConsoleDisabled {
		return false, nil
	}
	if policy != cli.WindowsConsoleDebugExplicit {
		return false, fmt.Errorf("invalid Windows console policy %d", policy)
	}
	attachErr := api.attachParent()
	if attachErr == nil {
		api.prepare()
		return true, nil
	}
	allocateErr := api.allocate()
	if allocateErr != nil {
		return false, &windowsDebugConsoleUnavailableError{attachErr: attachErr, allocateErr: allocateErr}
	}
	api.prepare()
	return true, nil
}

func prepareDebugConsole() {
	reopenIfInvalid("CONIN$", os.O_RDONLY, &os.Stdin)
	reopenIfInvalid("CONOUT$", os.O_WRONLY, &os.Stdout)
	reopenIfInvalid("CONOUT$", os.O_WRONLY, &os.Stderr)
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
