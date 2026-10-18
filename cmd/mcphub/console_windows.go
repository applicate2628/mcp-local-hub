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
const WindowsCLIStdioUnavailableID = "E_WINDOWS_CLI_STDIO_UNAVAILABLE"

type windowsConsoleAPI struct {
	attachParent func() error
	allocate     func() error
	prepareDebug func()
	prepareCLI   func()
	outputsValid func() (stdout bool, stderr bool)
}

type windowsDebugConsoleUnavailableError struct {
	attachErr   error
	allocateErr error
}

type windowsCLIStdioUnavailableError struct {
	attachErr   error
	stdoutValid bool
	stderrValid bool
}

func (e *windowsCLIStdioUnavailableError) Error() string {
	if e.attachErr != nil {
		return fmt.Sprintf("%s: attach parent: %v", WindowsCLIStdioUnavailableID, e.attachErr)
	}
	return fmt.Sprintf("%s: output handles remain invalid after parent attach (stdout=%t stderr=%t)", WindowsCLIStdioUnavailableID, e.stdoutValid, e.stderrValid)
}

func (e *windowsCLIStdioUnavailableError) Unwrap() error { return e.attachErr }

func (e *windowsCLIStdioUnavailableError) FailureID() string {
	return WindowsCLIStdioUnavailableID
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
	prepareDebug: prepareDebugConsole,
	prepareCLI:   prepareCLIConsoleOutputs,
	outputsValid: windowsCLIOutputsValid,
}

// applyWindowsConsolePolicy is the sole Windows console-state writer. Ordinary
// GUI/background/protocol modes are no-ops; CLI mode inspects and repairs only
// invalid output handles. Explicit debug mode attaches first and may allocate a
// console for GUI/CLI only. Every failure is returned before Cobra executes.
func applyWindowsConsolePolicy(mode windowsEntrypointMode, policy cli.WindowsConsolePolicy) (bool, error) {
	return applyWindowsConsolePolicyWithAPI(mode, policy, productionWindowsConsoleAPI)
}

func applyWindowsConsolePolicyWithAPI(mode windowsEntrypointMode, policy cli.WindowsConsolePolicy, api windowsConsoleAPI) (bool, error) {
	if policy != cli.WindowsConsoleDisabled && policy != cli.WindowsConsoleDebugExplicit {
		return false, fmt.Errorf("invalid Windows console policy %d", policy)
	}
	if mode == windowsEntrypointBackground || mode == windowsEntrypointStdioProtocol {
		return false, nil
	}
	if mode != windowsEntrypointCLI && mode != windowsEntrypointGUI {
		return false, fmt.Errorf("invalid Windows entrypoint mode %d", mode)
	}
	if policy == cli.WindowsConsoleDisabled {
		if mode == windowsEntrypointGUI {
			return false, nil
		}
		if api.outputsValid == nil || api.attachParent == nil || api.prepareCLI == nil {
			return false, &windowsCLIStdioUnavailableError{attachErr: fmt.Errorf("console composition is incomplete")}
		}
		stdoutValid, stderrValid := api.outputsValid()
		if stdoutValid && stderrValid {
			return false, nil
		}
		if err := api.attachParent(); err != nil {
			return false, &windowsCLIStdioUnavailableError{attachErr: err, stdoutValid: stdoutValid, stderrValid: stderrValid}
		}
		api.prepareCLI()
		stdoutValid, stderrValid = api.outputsValid()
		if !stdoutValid || !stderrValid {
			return false, &windowsCLIStdioUnavailableError{stdoutValid: stdoutValid, stderrValid: stderrValid}
		}
		return false, nil
	}
	attachErr := api.attachParent()
	if attachErr == nil {
		api.prepareDebug()
		return true, nil
	}
	allocateErr := api.allocate()
	if allocateErr != nil {
		return false, &windowsDebugConsoleUnavailableError{attachErr: attachErr, allocateErr: allocateErr}
	}
	api.prepareDebug()
	return true, nil
}

func prepareCLIConsoleOutputs() {
	reopenIfInvalid("CONOUT$", os.O_WRONLY, &os.Stdout)
	reopenIfInvalid("CONOUT$", os.O_WRONLY, &os.Stderr)
}

func windowsCLIOutputsValid() (stdout bool, stderr bool) {
	return windowsFileHandleValid(os.Stdout), windowsFileHandleValid(os.Stderr)
}

func windowsFileHandleValid(file *os.File) bool {
	if file == nil {
		return false
	}
	_, err := file.Stat()
	return err == nil
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
