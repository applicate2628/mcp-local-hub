// internal/gui/probe_windows.go
//go:build windows

package gui

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"syscall"
	"time"
	"unicode/utf16"
	"unsafe"

	"golang.org/x/sys/windows"
)

// processIDImpl is the Windows implementation. Uses
// PROCESS_QUERY_LIMITED_INFORMATION (works without admin in most
// cases) for image path + creation time; reads the command line via
// NtQueryInformationProcess + PEB walk; treats ACCESS_DENIED as
// alive=true,denied=true (Claude r2 #2: refuses take-over of a
// SYSTEM/scheduler-launched lock).
func processIDImpl(pid int) (ProcessIdentity, error) {
	const (
		PROCESS_QUERY_LIMITED_INFORMATION = 0x1000
		STILL_ACTIVE                      = 259
	)
	h, err := windows.OpenProcess(PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		// ERROR_ACCESS_DENIED → process exists but we can't query it.
		// Match Unix EPERM semantics: alive=true, denied=true.
		if errors.Is(err, windows.ERROR_ACCESS_DENIED) {
			return ProcessIdentity{Alive: true, Denied: true}, nil
		}
		// Other errors (ERROR_INVALID_PARAMETER for dead PID, etc.) → not alive.
		return ProcessIdentity{Alive: false}, nil
	}
	defer windows.CloseHandle(h)

	// Liveness via GetExitCodeProcess.
	var exitCode uint32
	if err := windows.GetExitCodeProcess(h, &exitCode); err != nil {
		return ProcessIdentity{Alive: false}, nil
	}
	if exitCode != STILL_ACTIVE {
		return ProcessIdentity{Alive: false}, nil
	}

	// Image path via QueryFullProcessImageName.
	imagePath := queryImagePath(h)

	// Creation time via GetProcessTimes.
	var creation, exit, kernel, user windows.Filetime
	startTime := time.Time{}
	if err := windows.GetProcessTimes(h, &creation, &exit, &kernel, &user); err == nil {
		startTime = time.Unix(0, creation.Nanoseconds())
	}

	// Command line via NtQueryInformationProcess + PEB walk.
	cmdline := queryCmdline(uint32(pid))

	return ProcessIdentity{
		Alive:     true,
		Denied:    false,
		ImagePath: imagePath,
		Cmdline:   cmdline,
		StartTime: startTime,
	}, nil
}

// killProcessImpl uses PROCESS_TERMINATE + TerminateProcess. Errors
// other than "process gone" propagate to the caller.
func killProcessImpl(pid int) error {
	const PROCESS_TERMINATE = 0x0001
	h, err := windows.OpenProcess(PROCESS_TERMINATE, false, uint32(pid))
	if err != nil {
		return fmt.Errorf("OpenProcess(PROCESS_TERMINATE, %d): %w", pid, err)
	}
	defer windows.CloseHandle(h)
	if err := windows.TerminateProcess(h, 1); err != nil {
		return fmt.Errorf("TerminateProcess(%d): %w", pid, err)
	}
	return nil
}

// queryImagePath returns the canonical executable path for an open
// process handle. Returns "" on failure.
func queryImagePath(h windows.Handle) string {
	buf := make([]uint16, windows.MAX_LONG_PATH)
	size := uint32(len(buf))
	if err := windows.QueryFullProcessImageName(h, 0, &buf[0], &size); err != nil {
		return ""
	}
	return windows.UTF16ToString(buf[:size])
}

// queryCmdline reads the target process's argv via PEB inspection.
// Returns nil on all failure paths; callers must nil-check Cmdline
// before indexing.
//
// Implementation note: NtQueryInformationProcess(ProcessBasicInformation)
// returns a PROCESS_BASIC_INFORMATION whose PebBaseAddress points
// into the target process's address space. We read PEB → ProcessParameters
// → CommandLine using ReadProcessMemory.
func queryCmdline(pid uint32) []string {
	const PROCESS_QUERY_INFORMATION = 0x0400
	const PROCESS_VM_READ = 0x0010
	h, err := windows.OpenProcess(PROCESS_QUERY_INFORMATION|PROCESS_VM_READ, false, pid)
	if err != nil {
		return nil
	}
	defer windows.CloseHandle(h)

	type processBasicInformation struct {
		Reserved1       uintptr
		PebBaseAddress  uintptr
		Reserved2       [2]uintptr
		UniqueProcessId uintptr
		Reserved3       uintptr
	}
	var pbi processBasicInformation
	var retLen uint32
	ntdll := syscall.NewLazyDLL("ntdll.dll")
	procNtQuery := ntdll.NewProc("NtQueryInformationProcess")
	r1, _, _ := procNtQuery.Call(
		uintptr(h),
		0, // ProcessBasicInformation
		uintptr(unsafe.Pointer(&pbi)),
		unsafe.Sizeof(pbi),
		uintptr(unsafe.Pointer(&retLen)),
	)
	if r1 != 0 || pbi.PebBaseAddress == 0 {
		return nil
	}

	// PEB layout (Windows 10/11 x64): ProcessParameters at offset 0x20.
	const pebProcessParametersOffset = 0x20
	var paramsAddr uintptr
	var n uintptr
	if err := windows.ReadProcessMemory(h, pbi.PebBaseAddress+pebProcessParametersOffset,
		(*byte)(unsafe.Pointer(&paramsAddr)), unsafe.Sizeof(paramsAddr), &n); err != nil {
		return nil
	}

	// RTL_USER_PROCESS_PARAMETERS.CommandLine is a UNICODE_STRING
	// at offset 0x70 (Windows 10/11 x64).
	const commandLineOffset = 0x70
	type unicodeString struct {
		Length        uint16
		MaximumLength uint16
		Buffer        uintptr
	}
	var us unicodeString
	if err := windows.ReadProcessMemory(h, paramsAddr+commandLineOffset,
		(*byte)(unsafe.Pointer(&us)), unsafe.Sizeof(us), &n); err != nil {
		return nil
	}
	if us.Length == 0 || us.Buffer == 0 {
		return nil
	}
	// Align down to even bytes before allocation: an odd us.Length from a
	// hostile/corrupted process would cause ReadProcessMemory to write one
	// byte past the end of the slice.
	readLen := uint16(us.Length &^ 1)
	if readLen == 0 {
		return nil
	}
	wbuf := make([]uint16, readLen/2)
	if err := windows.ReadProcessMemory(h, us.Buffer,
		(*byte)(unsafe.Pointer(&wbuf[0])), uintptr(readLen), &n); err != nil {
		return nil
	}
	cmdline := string(utf16.Decode(wbuf))
	return splitCommandLineW(cmdline)
}

// splitCommandLineW honors CommandLineToArgvW quoting rules so paths
// with spaces and quoted args parse correctly. Reimplemented here to
// avoid a syscall to a UI DLL (shell32).
func splitCommandLineW(s string) []string {
	var args []string
	var cur strings.Builder
	inQuote := false
	flush := func() {
		if cur.Len() > 0 {
			args = append(args, cur.String())
			cur.Reset()
		}
	}
	i := 0
	// First arg (executable) parses with simple quote handling.
	for i < len(s) {
		c := s[i]
		if c == '"' {
			inQuote = !inQuote
			i++
			continue
		}
		if !inQuote && (c == ' ' || c == '\t') {
			// CommandLineToArgvW treats both space and tab as
			// argv separators. The remaining-args loop already
			// handles tab; the first-arg loop must too, otherwise
			// `mcphub.exe<TAB>daemon` returns a single argv element
			// and cmdlineIsGui's len(argv)==1 branch (Explorer
			// no-arg auto-gui) would pass for a non-GUI subcommand.
			// (Codex iter-4 P2 #1.)
			i++
			break
		}
		cur.WriteByte(c)
		i++
	}
	flush()
	// Remaining args use full backslash-aware Microsoft rules.
	for i < len(s) {
		c := s[i]
		if c == ' ' || c == '\t' {
			if !inQuote {
				flush()
				i++
				continue
			}
		}
		if c == '\\' {
			// Count backslashes.
			j := i
			for j < len(s) && s[j] == '\\' {
				j++
			}
			backslashes := j - i
			if j < len(s) && s[j] == '"' {
				// 2N backslashes + " → N backslashes + toggle quote
				// 2N+1 + " → N backslashes + literal "
				cur.WriteString(strings.Repeat(`\`, backslashes/2))
				if backslashes%2 == 1 {
					cur.WriteByte('"')
				} else {
					inQuote = !inQuote
				}
				i = j + 1
				continue
			}
			cur.WriteString(strings.Repeat(`\`, backslashes))
			i = j
			continue
		}
		if c == '"' {
			inQuote = !inQuote
			i++
			continue
		}
		cur.WriteByte(c)
		i++
	}
	flush()
	return args
}

// matchBasename returns true iff filepath.Base(path) equals
// "mcphub.exe" (case-insensitive). Used by single_instance.go's
// Probe + KillRecordedHolder.
func matchBasename(path string) bool {
	return strings.EqualFold(filepath.Base(path), "mcphub.exe")
}
