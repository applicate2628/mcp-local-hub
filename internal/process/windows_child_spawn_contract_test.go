//go:build windows

package process

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"syscall"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

const (
	childSpawnContractHelperEnv = "MCPHUB_CHILD_SPAWN_CONTRACT_HELPER"
	childSpawnContractEventEnv  = "MCPHUB_CHILD_SPAWN_CONTRACT_EVENT"
)

var (
	childSpawnKernel32              = syscall.NewLazyDLL("kernel32.dll")
	childSpawnGetConsoleProcessList = childSpawnKernel32.NewProc("GetConsoleProcessList")
	childSpawnGetStdHandle          = childSpawnKernel32.NewProc("GetStdHandle")
	childSpawnGetFileType           = childSpawnKernel32.NewProc("GetFileType")
	childSpawnGetConsoleWindow      = childSpawnKernel32.NewProc("GetConsoleWindow")
	childSpawnUser32                = syscall.NewLazyDLL("user32.dll")
	childSpawnEnumWindows           = childSpawnUser32.NewProc("EnumWindows")
	childSpawnGetWindowThreadPID    = childSpawnUser32.NewProc("GetWindowThreadProcessId")
	childSpawnIsWindowVisible       = childSpawnUser32.NewProc("IsWindowVisible")
)

type childSpawnContractReport struct {
	PID           int      `json:"pid"`
	StdIn         uintptr  `json:"stdin"`
	StdOut        uintptr  `json:"stdout"`
	StdErr        uintptr  `json:"stderr"`
	StdInValid    bool     `json:"stdin_valid"`
	StdOutValid   bool     `json:"stdout_valid"`
	StdErrValid   bool     `json:"stderr_valid"`
	HasConsole    bool     `json:"has_console"`
	ConsoleCount  uintptr  `json:"console_count"`
	ConsoleError  uintptr  `json:"console_error"`
	ConsolePIDs   []uint32 `json:"console_pids"`
	ConsoleWindow uintptr  `json:"console_window"`
}

func childSpawnConsoleProbe() (uintptr, uintptr, []uint32) {
	var processIDs [4]uint32
	count, _, callErr := childSpawnGetConsoleProcessList.Call(
		uintptr(unsafe.Pointer(&processIDs[0])), uintptr(len(processIDs)))
	var errno uintptr
	if value, ok := callErr.(syscall.Errno); ok {
		errno = uintptr(value)
	}
	used := int(count)
	if used > len(processIDs) {
		used = len(processIDs)
	}
	return count, errno, append([]uint32(nil), processIDs[:used]...)
}

func childSpawnStdHandle(slot uintptr) uintptr {
	handle, _, _ := childSpawnGetStdHandle.Call(slot)
	return handle
}

func childSpawnHandleValid(handle uintptr) bool {
	if handle == 0 || handle == ^uintptr(0) {
		return false
	}
	fileType, _, callErr := childSpawnGetFileType.Call(handle)
	if errno, ok := callErr.(syscall.Errno); ok && errno != 0 {
		return false
	}
	return fileType != 0
}

func childSpawnVisibleWindowCount(pid uint32) (int, error) {
	count := 0
	callback := syscall.NewCallback(func(window, _ uintptr) uintptr {
		var ownerPID uint32
		childSpawnGetWindowThreadPID.Call(window, uintptr(unsafe.Pointer(&ownerPID)))
		if ownerPID == pid {
			visible, _, _ := childSpawnIsWindowVisible.Call(window)
			if visible != 0 {
				count++
			}
		}
		return 1
	})
	result, _, callErr := childSpawnEnumWindows.Call(callback, 0)
	if result == 0 {
		if errno, ok := callErr.(syscall.Errno); ok && errno != 0 {
			return 0, errno
		}
	}
	return count, nil
}

func runChildSpawnContractHelper() error {
	eventName, err := windows.UTF16PtrFromString(os.Getenv(childSpawnContractEventEnv))
	if err != nil {
		return fmt.Errorf("event name: %w", err)
	}
	event, err := windows.OpenEvent(windows.SYNCHRONIZE, false, eventName)
	if err != nil {
		return fmt.Errorf("open release event: %w", err)
	}
	defer windows.CloseHandle(event)

	stdin := childSpawnStdHandle(^uintptr(9))
	stdout := childSpawnStdHandle(^uintptr(10))
	stderr := childSpawnStdHandle(^uintptr(11))
	consoleCount, consoleError, consolePIDs := childSpawnConsoleProbe()
	consoleWindow, _, _ := childSpawnGetConsoleWindow.Call()
	report := childSpawnContractReport{
		PID:           os.Getpid(),
		StdIn:         stdin,
		StdOut:        stdout,
		StdErr:        stderr,
		StdInValid:    childSpawnHandleValid(stdin),
		StdOutValid:   childSpawnHandleValid(stdout),
		StdErrValid:   childSpawnHandleValid(stderr),
		HasConsole:    consoleCount != 0,
		ConsoleCount:  consoleCount,
		ConsoleError:  consoleError,
		ConsolePIDs:   consolePIDs,
		ConsoleWindow: consoleWindow,
	}
	if err := json.NewEncoder(os.Stdout).Encode(report); err != nil {
		return fmt.Errorf("encode report: %w", err)
	}
	if status, err := windows.WaitForSingleObject(event, windows.INFINITE); err != nil {
		return fmt.Errorf("wait for release event: %w", err)
	} else if status != windows.WAIT_OBJECT_0 {
		return fmt.Errorf("release event wait status=%#x", status)
	}
	return nil
}

func childSpawnTestExecutable(t *testing.T, subsystem uint16) string {
	t.Helper()
	source, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	image, err := os.ReadFile(source)
	if err != nil {
		t.Fatalf("read test executable: %v", err)
	}
	if len(image) < 0x40 || string(image[:2]) != "MZ" {
		t.Fatal("test executable has no DOS header")
	}
	peOffset := int(binary.LittleEndian.Uint32(image[0x3c:0x40]))
	optionalOffset := peOffset + 24
	subsystemOffset := optionalOffset + 68
	if peOffset < 0 || subsystemOffset+2 > len(image) || string(image[peOffset:peOffset+4]) != "PE\x00\x00" {
		t.Fatal("test executable has no valid PE optional header")
	}
	magic := binary.LittleEndian.Uint16(image[optionalOffset : optionalOffset+2])
	if magic != 0x10b && magic != 0x20b {
		t.Fatalf("test executable optional-header magic=%#x", magic)
	}
	// Make an isolated PE2 or PE3 copy of the already-built test binary. No
	// linker, shell, or visible helper is launched to create either fixture.
	binary.LittleEndian.PutUint16(image[subsystemOffset:subsystemOffset+2], subsystem)
	target := fmt.Sprintf(`%s\child-spawn-contract-%d.exe`, t.TempDir(), subsystem)
	if err := os.WriteFile(target, image, 0o700); err != nil {
		t.Fatalf("write CUI child fixture: %v", err)
	}
	return target
}

func assertChildCreationDateRecent(t *testing.T, pid int) {
	t.Helper()
	identity, err := LookupProcessIdentity(pid)
	if err != nil {
		t.Fatalf("lookup live child creation date: %v", err)
	}
	now := time.Now().Unix()
	if identity.PID != pid {
		t.Fatalf("identity PID=%d, live child PID=%d", identity.PID, pid)
	}
	if identity.CreationDateUnix <= 0 || now-identity.CreationDateUnix > 24*60*60 || identity.CreationDateUnix > now+60 {
		t.Fatalf("live child CreationDateUnix=%d outside recent bound at now=%d", identity.CreationDateUnix, now)
	}
}

func runWindowsChildSpawnContractRow(t *testing.T, subsystem uint16) {
	t.Helper()
	eventName := fmt.Sprintf("Local\\mcp-local-hub-child-contract-%d-%d", os.Getpid(), subsystem)
	eventNameUTF16, err := windows.UTF16PtrFromString(eventName)
	if err != nil {
		t.Fatal(err)
	}
	releaseEvent, err := windows.CreateEvent(nil, 1, 0, eventNameUTF16)
	if err != nil {
		t.Fatalf("create release event: %v", err)
	}
	defer windows.CloseHandle(releaseEvent)

	child := exec.Command(childSpawnTestExecutable(t, subsystem), "-test.run=^TestWindowsChildSpawnContract$")
	child.Env = append(os.Environ(),
		childSpawnContractHelperEnv+"=1",
		childSpawnContractEventEnv+"="+eventName,
	)
	stdin, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer stdin.Close()
	child.Stdin = stdin
	stdout, err := child.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	child.Stderr = &stderr
	NoConsole(child)
	if err := child.Start(); err != nil {
		t.Fatalf("start child fixture: %v", err)
	}
	released, waited := false, false
	defer func() {
		if !released {
			_ = windows.SetEvent(releaseEvent)
		}
		if !waited {
			_ = child.Process.Kill()
			_ = child.Wait()
		}
	}()

	reportCh := make(chan childSpawnContractReport, 1)
	reportErrCh := make(chan error, 1)
	go func() {
		var report childSpawnContractReport
		if err := json.NewDecoder(bufio.NewReader(stdout)).Decode(&report); err != nil {
			reportErrCh <- err
			return
		}
		reportCh <- report
	}()

	var report childSpawnContractReport
	select {
	case report = <-reportCh:
	case err := <-reportErrCh:
		_ = child.Process.Kill()
		_ = child.Wait()
		t.Fatalf("decode child report: %v (stderr=%q)", err, stderr.String())
	case <-time.After(5 * time.Second):
		_ = child.Process.Kill()
		t.Fatal("child fixture did not report before deadline")
	}

	if report.PID != child.Process.Pid {
		t.Fatalf("reported PID=%d, started PID=%d", report.PID, child.Process.Pid)
	}
	if report.HasConsole != (report.ConsoleCount != 0) {
		t.Fatalf("inconsistent console report: %+v", report)
	}
	if subsystem == 2 {
		if report.ConsoleCount != 0 || len(report.ConsolePIDs) != 0 {
			t.Fatalf("canonical GUI child has console membership: %+v", report)
		}
	} else {
		switch report.ConsoleCount {
		case 0:
			if len(report.ConsolePIDs) != 0 {
				t.Fatalf("zero-membership CUI child reported participant PIDs: %+v", report)
			}
		case 1:
			if len(report.ConsolePIDs) != 1 || int(report.ConsolePIDs[0]) != child.Process.Pid {
				t.Fatalf("CUI child membership is not exact self-only: %+v", report)
			}
		default:
			t.Fatalf("CUI child joined a parent/sibling/foreign console: %+v", report)
		}
	}
	visibleWindows, err := childSpawnVisibleWindowCount(uint32(child.Process.Pid))
	if err != nil {
		t.Fatalf("enumerate child windows: %v", err)
	}
	if visibleWindows != 0 {
		t.Fatalf("CREATE_NO_WINDOW child owns %d visible top-level window(s)", visibleWindows)
	}
	if report.ConsoleWindow != 0 {
		visible, _, _ := childSpawnIsWindowVisible.Call(report.ConsoleWindow)
		if visible != 0 {
			t.Fatalf("child console HWND %#x is visible", report.ConsoleWindow)
		}
	}
	for name, value := range map[string]struct {
		handle uintptr
		valid  bool
	}{
		"stdin": {report.StdIn, report.StdInValid}, "stdout": {report.StdOut, report.StdOutValid}, "stderr": {report.StdErr, report.StdErrValid},
	} {
		if !value.valid {
			t.Fatalf("child reported invalid %s handle %#x", name, value.handle)
		}
	}
	childHandle, err := windows.OpenProcess(windows.SYNCHRONIZE, false, uint32(child.Process.Pid))
	if err != nil {
		t.Fatalf("open live child: %v", err)
	}
	defer windows.CloseHandle(childHandle)
	if status, err := windows.WaitForSingleObject(childHandle, 0); err != nil {
		t.Fatalf("probe live child: %v", err)
	} else if status != uint32(windows.WAIT_TIMEOUT) {
		t.Fatalf("child status before release=%#x, want WAIT_TIMEOUT", status)
	}
	assertChildCreationDateRecent(t, child.Process.Pid)
	if err := windows.SetEvent(releaseEvent); err != nil {
		t.Fatalf("release child fixture: %v", err)
	}
	released = true
	if err := child.Wait(); err != nil {
		t.Fatalf("child fixture exit: %v", err)
	}
	waited = true
	if stderr.Len() != 0 {
		t.Fatalf("child fixture stderr=%q, want empty", stderr.String())
	}
}

func TestWindowsChildSpawnContract(t *testing.T) {
	if os.Getenv(childSpawnContractHelperEnv) == "1" {
		if err := runChildSpawnContractHelper(); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
		return
	}

	cmd := exec.Command("does-not-need-to-exist")
	NoConsole(cmd)
	if cmd.SysProcAttr == nil || !cmd.SysProcAttr.HideWindow || cmd.SysProcAttr.CreationFlags&windows.CREATE_NO_WINDOW == 0 {
		t.Fatalf("NoConsole attributes=%+v, want HideWindow and CREATE_NO_WINDOW", cmd.SysProcAttr)
	}
	NoConsole(cmd)
	if cmd.SysProcAttr.CreationFlags&windows.CREATE_NO_WINDOW == 0 {
		t.Fatal("NoConsole lost CREATE_NO_WINDOW on repeated application")
	}

	for _, tc := range []struct {
		name      string
		subsystem uint16
	}{
		{"canonical GUI", 2},
		{"external CUI", 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runWindowsChildSpawnContractRow(t, tc.subsystem)
		})
	}
}

func TestWindowsStartWithJobNoConsole(t *testing.T) {
	flags := startWithJobCreationFlags()
	for _, required := range []uint32{
		uint32(windows.EXTENDED_STARTUPINFO_PRESENT),
		uint32(windows.CREATE_UNICODE_ENVIRONMENT),
		uint32(windows.CREATE_NO_WINDOW),
	} {
		if flags&required == 0 {
			t.Fatalf("StartWithJob flags=%#x missing %#x", flags, required)
		}
	}
}

func TestWindowsHiddenWorkerNoConsole(t *testing.T) {
	if flags := containedWindowsCreationFlags(); flags&windows.CREATE_NO_WINDOW == 0 {
		t.Fatalf("contained hidden worker flags=%#x, want CREATE_NO_WINDOW", flags)
	}
}
