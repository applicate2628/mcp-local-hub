//go:build windows

package process

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

const containedWindowsHelperEnv = "MCPHUB_CONTAINED_WINDOWS_HELPER"

var procGetHandleInformationForContainedTest = syscall.NewLazyDLL("kernel32.dll").NewProc("GetHandleInformation")

func TestContainedStreamWindowsHelper(t *testing.T) {
	mode := os.Getenv(containedWindowsHelperEnv)
	if mode == "" {
		return
	}
	switch mode {
	case "probe":
		var inJob int32
		r1, _, _ := procIsProcessInJob.Call(
			uintptr(windows.CurrentProcess()),
			0,
			uintptr(unsafe.Pointer(&inJob)),
		)
		fmt.Printf("job=%d\n", boolToInt(r1 != 0 && inJob != 0))
		fmt.Fprintln(os.Stderr, "bounded diagnostic")
	case "handle":
		raw, _ := strconv.ParseUint(os.Getenv("MCPHUB_UNRELATED_HANDLE"), 10, 64)
		var flags uint32
		r1, _, _ := procGetHandleInformationForContainedTest.Call(
			uintptr(windows.Handle(raw)),
			uintptr(unsafe.Pointer(&flags)),
		)
		fmt.Printf("unrelated_inherited=%d\n", boolToInt(r1 != 0))
	case "tree":
		exe, err := os.Executable()
		if err != nil {
			t.Fatal(err)
		}
		grandchild := exec.Command(exe, "-test.run=^TestContainedStreamWindowsHelper$")
		grandchild.Env = append(os.Environ(), containedWindowsHelperEnv+"=hold")
		grandchild.Stdout = os.Stdout
		grandchild.Stderr = os.Stderr
		if err := grandchild.Start(); err != nil {
			t.Fatal(err)
		}
		fmt.Printf("child=%d\ngrandchild=%d\npipe-held\n", os.Getpid(), grandchild.Process.Pid)
		_ = grandchild.Wait()
	case "hold":
		fmt.Println("grandchild-ready")
		time.Sleep(30 * time.Second)
	case "sentinel":
		if err := os.WriteFile(os.Getenv("MCPHUB_SENTINEL_PATH"), []byte("started"), 0o600); err != nil {
			t.Fatal(err)
		}
	case "exit7":
		os.Exit(7)
	case "environment":
		wd, err := os.Getwd()
		if err != nil {
			t.Fatal(err)
		}
		fmt.Printf("case=%s\nsystemroot=%s\npwd=%s\nwd=%s\n", os.Getenv("MCPHUB_PR591_CASE"), os.Getenv("SYSTEMROOT"), os.Getenv("PWD"), wd)
	case "sleep":
		fmt.Println("started")
		time.Sleep(30 * time.Second)
	default:
		t.Fatalf("unknown helper mode %q", mode)
	}
}

func TestRunContainedStream_ChildSeesNormalizedEnvironment(t *testing.T) {
	cmd := containedWindowsHelperCommand(t, "environment")
	dir := t.TempDir()
	cmd.Dir = dir
	cmd.Env = []string{
		containedWindowsHelperEnv + "=environment",
		"McpHub_PR591_Case=first",
		"MCPHUB_PR591_CASE=last",
		"PWD=contained-pwd",
	}
	var stdout strings.Builder
	err := RunContainedStream(
		context.Background(),
		cmd,
		ContainedStreamOptions{CleanupTimeout: 5 * time.Second},
		func(reader io.Reader) error {
			_, err := io.Copy(&stdout, reader)
			return err
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	got := stdout.String()
	for _, want := range []string{"case=last", "pwd=contained-pwd", "wd=" + dir} {
		if !strings.Contains(got, want) {
			t.Fatalf("child environment=%q, missing %q", got, want)
		}
	}
	if strings.Contains(got, "systemroot=\n") {
		t.Fatalf("child environment=%q, missing Go-owned SYSTEMROOT", got)
	}
}

func boolToInt(v bool) int {
	if v {
		return 1
	}
	return 0
}

func containedWindowsHelperCommand(t *testing.T, mode string) *exec.Cmd {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(exe, "-test.run=^TestContainedStreamWindowsHelper$")
	cmd.Env = append(os.Environ(), containedWindowsHelperEnv+"="+mode)
	return cmd
}

func TestRunContainedStreamWindows_AssignedAtCreateWithStdHandles(t *testing.T) {
	cmd := containedWindowsHelperCommand(t, "probe")
	var stdout strings.Builder
	var stderr strings.Builder
	err := RunContainedStream(
		context.Background(),
		cmd,
		ContainedStreamOptions{CleanupTimeout: 5 * time.Second, Stderr: &stderr},
		func(r io.Reader) error {
			_, err := io.Copy(&stdout, r)
			return err
		},
	)
	if err != nil {
		t.Fatalf("RunContainedStream: %v", err)
	}
	if !strings.Contains(stdout.String(), "job=1") {
		t.Fatalf("stdout=%q, child was not already in a Job", stdout.String())
	}
	if !strings.Contains(stderr.String(), "bounded diagnostic") {
		t.Fatalf("stderr=%q, standard error was not wired", stderr.String())
	}
}

func TestRunContainedStreamWindows_AssignmentSetupFailureStartsNoChild(t *testing.T) {
	sentinel := filepath.Join(t.TempDir(), "child-started")
	cmd := containedWindowsHelperCommand(t, "sentinel")
	cmd.Env = append(cmd.Env, "MCPHUB_SENTINEL_PATH="+sentinel)

	job, err := NewKillOnCloseJob()
	if err != nil {
		t.Fatal(err)
	}
	if err := job.Close(); err != nil {
		t.Fatal(err)
	}
	child := &windowsContainedChild{cmd: cmd, job: job}
	stdoutRead, stdoutWrite, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer stdoutRead.Close()
	defer stdoutWrite.Close()
	stderrRead, stderrWrite, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer stderrRead.Close()
	defer stderrWrite.Close()
	stdin, err := openContainedNull()
	if err != nil {
		t.Fatal(err)
	}
	defer stdin.Close()

	err = child.start(cmd, stdin, stdoutWrite, stderrWrite)
	var platformErr *containedPlatformError
	if !errors.As(err, &platformErr) || platformErr.stage != ContainedStageContainment {
		t.Fatalf("err=%#v, want containment-stage setup failure", err)
	}
	if _, statErr := os.Stat(sentinel); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("sentinel stat=%v, child ran without containment", statErr)
	}
}

func TestRunContainedStreamWindows_InheritsOnlyDeclaredHandles(t *testing.T) {
	sa := &windows.SecurityAttributes{
		Length:        uint32(unsafe.Sizeof(windows.SecurityAttributes{})),
		InheritHandle: 1,
	}
	probe, err := windows.CreateEvent(sa, 1, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer windows.CloseHandle(probe)

	cmd := containedWindowsHelperCommand(t, "handle")
	cmd.Env = append(cmd.Env, "MCPHUB_UNRELATED_HANDLE="+strconv.FormatUint(uint64(probe), 10))
	var stdout strings.Builder
	err = RunContainedStream(
		context.Background(),
		cmd,
		ContainedStreamOptions{CleanupTimeout: 5 * time.Second},
		func(r io.Reader) error {
			_, err := io.Copy(&stdout, r)
			return err
		},
	)
	if err != nil {
		t.Fatalf("RunContainedStream: %v", err)
	}
	if !strings.Contains(stdout.String(), "unrelated_inherited=0") {
		t.Fatalf("stdout=%q, unrelated inheritable handle crossed allowlist", stdout.String())
	}
}

func TestRunContainedStreamWindows_GrandchildPipeHolderDoesNotLeak(t *testing.T) {
	cmd := containedWindowsHelperCommand(t, "tree")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	type identities struct {
		child      int
		grandchild int
	}
	idsCh := make(chan identities, 1)
	done := make(chan error, 1)
	go func() {
		done <- RunContainedStream(
			ctx,
			cmd,
			ContainedStreamOptions{CleanupTimeout: 5 * time.Second},
			func(r io.Reader) error {
				scanner := bufio.NewScanner(r)
				var ids identities
				for scanner.Scan() {
					line := scanner.Text()
					if value, ok := strings.CutPrefix(line, "child="); ok {
						ids.child, _ = strconv.Atoi(value)
					}
					if value, ok := strings.CutPrefix(line, "grandchild="); ok {
						ids.grandchild, _ = strconv.Atoi(value)
					}
					if line == "pipe-held" {
						idsCh <- ids
					}
				}
				return scanner.Err()
			},
		)
	}()

	var ids identities
	select {
	case ids = <-idsCh:
	case <-time.After(10 * time.Second):
		t.Fatal("helper did not report coordinated pipe-holder state")
	}
	if ids.child <= 0 || ids.grandchild <= 0 {
		t.Fatalf("invalid helper identities: %+v", ids)
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("RunContainedStream err=%v, want context.Canceled", err)
	}
	waitForContainedWindowsExit(t, ids.child)
	waitForContainedWindowsExit(t, ids.grandchild)
}

func waitForContainedWindowsExit(t *testing.T, pid int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if !processAlive(pid, t) {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("pid %d survived contained runner return", pid)
}

func TestRunContainedStreamWindows_ProcessAndThreadHandlesCloseOnEveryReturnPath(t *testing.T) {
	tests := []struct {
		name    string
		mode    string
		consume func(io.Reader) error
	}{
		{name: "success", mode: "probe", consume: drainContainedReader},
		{name: "nonzero", mode: "exit7", consume: drainContainedReader},
		{name: "consumer", mode: "sleep", consume: func(io.Reader) error { return errors.New("consumer stop") }},
		{name: "start", mode: "probe", consume: drainContainedReader},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cmd := containedWindowsHelperCommand(t, tc.mode)
			if tc.name == "start" {
				cmd.Path = filepath.Join(t.TempDir(), "missing.exe")
				cmd.Args[0] = cmd.Path
			}
			var captured *windowsContainedChild
			deps := defaultContainedStreamDependencies()
			deps.newChild = func(cmd *exec.Cmd) (containedChild, error) {
				child, err := newPlatformContainedChild(cmd)
				if err == nil {
					captured = child.(*windowsContainedChild)
				}
				return child, err
			}
			_ = runContainedStreamWithDependencies(
				context.Background(),
				cmd,
				ContainedStreamOptions{CleanupTimeout: 5 * time.Second},
				tc.consume,
				deps,
			)
			if captured == nil {
				t.Fatal("platform child was not captured")
			}
			if captured.process != 0 {
				t.Fatalf("process handle %v remained owned", captured.process)
			}
			if captured.thread != 0 {
				t.Fatalf("thread handle %v remained owned", captured.thread)
			}
			if captured.job == nil || captured.job.Handle() != 0 {
				t.Fatalf("job handle remained owned: %+v", captured.job)
			}
		})
	}
}
