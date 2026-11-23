//go:build windows && amd64

package process

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

const backgroundConsoleProbeEnv = "MCPHUB_TEST_BACKGROUND_CONSOLE_PROBE"

type backgroundConsoleProbeReport struct {
	PID           uint32                         `json:"pid"`
	StartupFlags  uint32                         `json:"startup_flags"`
	ShowWindow    uint16                         `json:"show_window"`
	InitialWindow uint64                         `json:"initial_window"`
	Consoles      [2]backgroundConsoleAllocation `json:"consoles"`
	Input         []byte                         `json:"input"`
	Error         string                         `json:"error,omitempty"`
}

type backgroundConsoleAllocation struct {
	Window    uint64 `json:"window"`
	Allocated bool   `json:"allocated"`
	Visible   bool   `json:"visible"`
	Released  bool   `json:"released"`
}

// This root is serial. Unknown original custody remains strongly reachable and
// stops the receiving test process before its fixture cleanup or another child.
var backgroundConsoleRetained []any

var backgroundConsoleKernel = windows.NewLazySystemDLL("kernel32.dll")

func backgroundConsoleProbe() (report backgroundConsoleProbeReport) {
	report.PID = windows.GetCurrentProcessId()
	startupProc := backgroundConsoleKernel.NewProc("GetStartupInfoW")
	visibleProc := windows.NewLazySystemDLL("user32.dll").NewProc("IsWindowVisible")
	for _, proc := range []*windows.LazyProc{startupProc, visibleProc} {
		if err := proc.Find(); err != nil {
			report.Error = err.Error()
			return report
		}
	}
	var startup windows.StartupInfo
	startupProc.Call(uintptr(unsafe.Pointer(&startup)))
	report.StartupFlags, report.ShowWindow = startup.Flags, startup.ShowWindow
	window, _, _ := backgroundConsoleKernel.NewProc("GetConsoleWindow").Call()
	report.InitialWindow = uint64(window)
	// Refuse before AllocConsole if the startup contract is absent. The success
	// path still requires a real allocation and native visibility observation.
	if startup.Flags&windows.STARTF_USESHOWWINDOW == 0 || startup.ShowWindow != windows.SW_HIDE || window != 0 {
		report.Error = "background startup hide contract absent"
		return report
	}
	count, errno, _ := childSpawnConsoleProbe()
	if count == 0 && errno != uintptr(windows.ERROR_INVALID_HANDLE) {
		report.Error = fmt.Sprintf("GetConsoleProcessList: count=0 error=%d", errno)
		return report
	}
	if count != 0 {
		if err := backgroundConsoleBoolean("FreeConsole"); err != nil {
			report.Error = err.Error()
			return report
		}
	}
	for index := range report.Consoles {
		allocation := &report.Consoles[index]
		if err := backgroundConsoleBoolean("AllocConsole"); err != nil {
			report.Error = fmt.Sprintf("allocation %d: %v", index+1, err)
			return report
		}
		allocation.Allocated = true
		window, _, _ = backgroundConsoleKernel.NewProc("GetConsoleWindow").Call()
		allocation.Window = uint64(window)
		visible, _, _ := visibleProc.Call(window)
		allocation.Visible = visible != 0
		// No ShowWindow call can conceal a flash before this observation.
		releaseErr := backgroundConsoleBoolean("FreeConsole")
		allocation.Released = releaseErr == nil
		if window == 0 || allocation.Visible || releaseErr != nil {
			report.Error = fmt.Sprintf("allocation %d: window=%d visible=%t release=%v", index+1, window, allocation.Visible, releaseErr)
			return report
		}
		window, _, _ = backgroundConsoleKernel.NewProc("GetConsoleWindow").Call()
		if window != 0 {
			report.Error = "released console still has a window"
			return report
		}
	}
	// os.Stdin retains the original inherited file/NUL handle from process
	// initialization. AllocConsole must not replace the input used by this probe.
	input, err := io.ReadAll(io.LimitReader(os.Stdin, 1025))
	report.Input = input
	if err != nil || len(report.Input) > 1024 {
		report.Error = fmt.Sprintf("original stdin: bytes=%d err=%v", len(report.Input), err)
	}
	return report
}

func backgroundConsoleBoolean(name string) error {
	proc := backgroundConsoleKernel.NewProc(name)
	if err := proc.Find(); err != nil {
		return err
	}
	ok, _, err := proc.Call()
	if ok != 0 {
		return nil
	}
	if err == syscall.Errno(0) {
		return fmt.Errorf("%s returned false without a native error", name)
	}
	return fmt.Errorf("%s: %w", name, err)
}

func TestBackgroundNativeConsoleStartup(t *testing.T) {
	if os.Getenv(backgroundConsoleProbeEnv) == "1" {
		report := backgroundConsoleProbe()
		code := 37
		if report.Error != "" {
			code = 94
		}
		if err := json.NewEncoder(os.Stdout).Encode(report); err != nil {
			os.Exit(95)
		}
		if _, err := os.Stderr.Write([]byte("console-stderr\x00\xff")); err != nil {
			os.Exit(95)
		}
		os.Exit(code)
	}
	image := childSpawnTestExecutable(t, 3)
	for _, route := range []string{"go-at-create", "go-contained"} {
		t.Run(route, func(t *testing.T) {
			deadline, finite := t.Deadline()
			if !finite || time.Until(deadline) <= 5*time.Second+RetainedTerminationAllowance()+10*time.Second {
				t.Fatal("console probe requires its work and original cleanup budgets")
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			workDeadline, _ := ctx.Deadline()
			defer cancel()
			cmd := &exec.Cmd{
				Path: image, Args: []string{image, "-test.run=^TestBackgroundNativeConsoleStartup$", "-test.count=1", "-test.timeout=15s"},
				Dir: filepath.Dir(image), Env: append(os.Environ(), backgroundConsoleProbeEnv+"=1"),
			}
			var stdout, stderr []byte
			var input []byte
			var pid, exitCode int
			if route == "go-at-create" {
				input = []byte("console-stdin\x00\xff")
				files := make([]*os.File, 3)
				t.Cleanup(func() {
					for _, file := range files {
						if file != nil {
							if err := file.Close(); err != nil {
								t.Error(err)
							}
						}
					}
				})
				for index, name := range []string{"console-input.bin", "console-output.json", "console-error.bin"} {
					file, err := os.OpenFile(filepath.Join(filepath.Dir(image), name), os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
					if err != nil {
						t.Fatal(err)
					}
					files[index] = file
				}
				if count, err := files[0].Write(input); err != nil || count != len(input) {
					t.Fatalf("prepare original stdin: bytes=%d err=%v", count, err)
				}
				if _, err := files[0].Seek(0, io.SeekStart); err != nil {
					t.Fatal(err)
				}
				// Reuse the original-handle lifecycle, selecting the actual background
				// at-create builder instead of the foreground or PID-reopen route.
				deps := foregroundDependencies{newJob: NewJobObserved, start: StartWithJobFilesOriginal}
				result, runErr := runForeground(ctx, cmd, files[0], files[1], files[2], deps)
				if result.Settlement == nil || !result.Settlement.Complete() {
					backgroundConsoleRetained = []any{cmd, result, runErr, files}
					os.Exit(86)
				}
				if runErr != nil || !result.Root.Known || result.Root.Error != nil || result.Cancelled || result.Original == nil || !result.Native.HandlesClosed || len(result.Outcome.Pending) != 0 {
					t.Fatalf("original at-create receiving: result=%+v err=%v", result, runErr)
				}
				pid, exitCode = result.Native.RootIdentity.PID, result.Root.ExitCode
				// Native controls precede capture reads. The caller-owned files are
				// received separately after the original child and Job are closed.
				for index, limit := range []int64{8192, 128} {
					file := files[index+1]
					if _, err := file.Seek(0, io.SeekStart); err != nil {
						t.Fatal(err)
					}
					data, err := io.ReadAll(io.LimitReader(file, limit+1))
					if err != nil || int64(len(data)) > limit {
						t.Fatalf("closed capture %d: bytes=%d err=%v", index+1, len(data), err)
					}
					if index == 0 {
						stdout = data
					} else {
						stderr = data
					}
				}
				var closeErr error
				for index, file := range files {
					closeErr = errors.Join(closeErr, file.Close())
					files[index] = nil
				}
				if closeErr != nil {
					t.Fatal(closeErr)
				}
			} else {
				var output, errorOutput bytes.Buffer
				var original containedChild
				deps := defaultContainedStreamDependencies()
				deps.newChild = func(command *exec.Cmd) (containedChild, error) {
					child, err := newPlatformContainedChild(command)
					original = child
					return child, err
				}
				var receipt ContainedStreamReceiptV1
				runErr := runContainedStreamCore(ctx, cmd, ContainedStreamOptions{
					CleanupTimeout: RetainedTerminationAllowance(), StdoutByteLimit: 8192, StderrByteLimit: 128, Stderr: &errorOutput,
					AbsoluteCleanupDeadline: workDeadline.Add(RetainedTerminationAllowance()),
				}, func(reader io.Reader) error { _, err := io.Copy(&output, reader); return err }, deps, &receipt)
				var failure *ContainedRunError
				if original == nil && errors.As(runErr, &failure) && failure.Stage == ContainedStageValidate {
					t.Fatalf("contained pre-acquisition validation: receipt=%+v err=%v", receipt, runErr)
				}
				// Original native and IO controls precede access to both captures.
				if receipt.NativeSettlement == nil || !receipt.NativeSettlement.Complete() || !receipt.IOSettlement.Complete() {
					backgroundConsoleRetained = []any{original, receipt, runErr, &output, &errorOutput}
					os.Exit(86)
				}
				if !receipt.RootWaitKnown || !errors.As(runErr, &failure) || failure.Stage != ContainedStageExit || failure.CleanupCause != nil || receipt.CleanupError != nil {
					t.Fatalf("original contained receiving: receipt=%+v err=%v", receipt, runErr)
				}
				pid, exitCode = receipt.RootIdentity.PID, receipt.RootExitCode
				stdout, stderr = output.Bytes(), errorOutput.Bytes()
			}
			var report backgroundConsoleProbeReport
			if err := json.Unmarshal(stdout, &report); err != nil {
				t.Fatalf("closed child capture: %v", err)
			}
			if exitCode != 37 || pid <= 0 || report.PID != uint32(pid) || report.Error != "" || report.StartupFlags&windows.STARTF_USESHOWWINDOW == 0 || report.ShowWindow != windows.SW_HIDE || report.InitialWindow != 0 {
				t.Fatalf("actual background console: originalPID=%d exit=%d report=%+v", pid, exitCode, report)
			}
			for index, allocation := range report.Consoles {
				if !allocation.Allocated || allocation.Window == 0 || allocation.Visible || !allocation.Released {
					t.Fatalf("actual allocation %d: %+v", index+1, allocation)
				}
			}
			if !bytes.Equal(report.Input, input) || !bytes.Equal(stderr, []byte("console-stderr\x00\xff")) {
				t.Fatalf("original stdio changed: input=%x stderr=%x", report.Input, stderr)
			}
		})
	}
}
