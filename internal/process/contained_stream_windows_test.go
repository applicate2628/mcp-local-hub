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

func TestContainedStreamWindowsNaturalExitJobSnapshot(t *testing.T) {
	if os.Getenv("MCPHUB_CONTAINED_HELD_CHILD") == "1" {
		fmt.Fprintln(os.Stdout, "ready")
		_, _ = io.Copy(io.Discard, os.Stdin)
		return
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestContainedStreamWindowsNaturalExitJobSnapshot$")
	cmd.Env = append(os.Environ(), "MCPHUB_CONTAINED_HELD_CHILD=1")
	job, err := NewKillOnCloseJob()
	if err != nil {
		t.Fatal(err)
	}
	child := &windowsContainedChild{cmd: cmd, job: job}
	stdinRead, stdinWrite, err := os.Pipe()
	if err != nil {
		_ = child.close()
		t.Fatal(err)
	}
	stdoutRead, stdoutWrite, err := os.Pipe()
	if err != nil {
		_ = stdinRead.Close()
		_ = stdinWrite.Close()
		_ = child.close()
		t.Fatal(err)
	}
	stderrRead, stderrWrite, err := os.Pipe()
	if err != nil {
		_ = stdinRead.Close()
		_ = stdinWrite.Close()
		_ = stdoutRead.Close()
		_ = stdoutWrite.Close()
		_ = child.close()
		t.Fatal(err)
	}
	var retained []windows.Handle
	var readyDone chan struct{}
	defer func() {
		_ = stdinWrite.Close()
		_ = stdinRead.Close()
		_ = stdoutWrite.Close()
		_ = stderrWrite.Close()
		_ = stdoutRead.Close()
		_ = stderrRead.Close()
		if err := job.Close(); err != nil {
			t.Errorf("close test-owned Job: %v", err)
		}
		if child.process != 0 {
			status, waitErr := windows.WaitForSingleObject(child.process, 2000)
			if waitErr != nil || status != uint32(windows.WAIT_OBJECT_0) {
				t.Errorf("test-owned child not joined: status=%d err=%v", status, waitErr)
			}
		}
		for _, h := range retained {
			if err := windows.CloseHandle(h); err != nil {
				t.Errorf("close retained test-owned member handle: %v", err)
			}
		}
		if err := child.close(); err != nil {
			t.Errorf("close test-owned child handles: %v", err)
		}
		if readyDone != nil {
			select {
			case <-readyDone:
			case <-time.After(time.Second):
				t.Error("ready reader did not join")
			}
		}
	}()
	initial, initialErr := job.queryAccounting()
	initialList, initialListErr := job.queryProcessIDList()
	t.Logf("TARGET_WINDOWS_EMPTY_JOB total=%d active=%d terminated=%d assigned=%d listed=%d accounting_err=%v list_err=%v", initial.TotalProcesses, initial.ActiveProcesses, initial.TotalTerminatedProcesses, initialList.Assigned, len(initialList.PIDs), initialErr, initialListErr)
	if initialErr != nil || initialListErr != nil {
		t.Fatalf("initial Job snapshot unavailable: %v/%v", initialErr, initialListErr)
	}
	if err := child.start(cmd, stdinRead, stdoutWrite, stderrWrite); err != nil {
		t.Fatal(err)
	}
	readyDone = make(chan struct{})
	ready := make(chan error, 1)
	go func() {
		defer close(readyDone)
		line, readErr := bufio.NewReader(stdoutRead).ReadString('\n')
		if readErr == nil && line != "ready\n" {
			readErr = fmt.Errorf("unexpected ready marker")
		}
		ready <- readErr
	}()
	_ = stdoutWrite.Close()
	_ = stderrWrite.Close()
	_ = stdinRead.Close()
	select {
	case err := <-ready:
		if err != nil {
			t.Fatalf("held child ready marker: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("held child did not become ready")
	}
	inspect := func(h windows.Handle) (uint32, uint64, string, bool, uint32, error) {
		pid, err := windows.GetProcessId(h)
		if err != nil {
			return 0, 0, "", false, 0, err
		}
		var created, exited, kernel, user windows.Filetime
		if err := windows.GetProcessTimes(h, &created, &exited, &kernel, &user); err != nil {
			return 0, 0, "", false, 0, err
		}
		creation := uint64(created.HighDateTime)<<32 | uint64(created.LowDateTime)
		buffer := make([]uint16, 4096)
		size := uint32(len(buffer))
		if err := windows.QueryFullProcessImageName(h, 0, &buffer[0], &size); err != nil {
			return 0, 0, "", false, 0, err
		}
		base := filepath.Base(windows.UTF16ToString(buffer[:size]))
		member, err := jobMemberFromHandle(h, job)
		if err != nil {
			return 0, 0, "", false, 0, err
		}
		status, err := windows.WaitForSingleObject(h, 0)
		return pid, creation, base, member, status, err
	}
	held, heldErr := job.queryAccounting()
	list, listErr := job.queryProcessIDList()
	originalPID, originalCreation, originalBase, originalMember, originalWait, originalErr := inspect(child.process)
	t.Logf("TARGET_WINDOWS_HELD_JOB total=%d active=%d terminated=%d assigned=%d listed=%d original_pid=%d original_creation=%d original_image=%s original_member=%v original_wait=%d accounting_err=%v list_err=%v original_err=%v", held.TotalProcesses, held.ActiveProcesses, held.TotalTerminatedProcesses, list.Assigned, len(list.PIDs), originalPID, originalCreation, originalBase, originalMember, originalWait, heldErr, listErr, originalErr)
	if heldErr != nil || listErr != nil || originalErr != nil || !originalMember || originalWait != uint32(windows.WAIT_TIMEOUT) {
		t.Fatalf("held snapshot unavailable or original not live: %v/%v/%v member=%v wait=%d", heldErr, listErr, originalErr, originalMember, originalWait)
	}
	for _, pid := range list.PIDs {
		h, openErr := windows.OpenProcess(windows.SYNCHRONIZE|windows.PROCESS_QUERY_LIMITED_INFORMATION, false, pid)
		if openErr != nil {
			t.Fatalf("open listed Job member pid=%d: %v", pid, openErr)
		}
		retained = append(retained, h)
		id, created, base, member, status, inspectErr := inspect(h)
		t.Logf("TARGET_WINDOWS_HELD_MEMBER pid=%d creation=%d image=%s member=%v wait=%d err=%v", id, created, base, member, status, inspectErr)
		if inspectErr != nil || !member || id != pid {
			t.Fatalf("listed member identity unproved pid=%d: %v member=%v id=%d", pid, inspectErr, member, id)
		}
	}
	if err := stdinWrite.Close(); err != nil {
		t.Fatal(err)
	}
	status, waitErr := windows.WaitForSingleObject(child.process, 2000)
	if waitErr != nil || status != uint32(windows.WAIT_OBJECT_0) {
		t.Fatalf("released child did not join: status=%d err=%v", status, waitErr)
	}
	waited := child.wait()
	if !waited.exited || waited.err != nil || waited.exitCode != 0 {
		t.Fatalf("released child wait=%+v", waited)
	}
	after, afterErr := job.queryAccounting()
	afterList, afterListErr := job.queryProcessIDList()
	t.Logf("TARGET_WINDOWS_RELEASED_JOB total=%d active=%d terminated=%d assigned=%d listed=%d accounting_err=%v list_err=%v", after.TotalProcesses, after.ActiveProcesses, after.TotalTerminatedProcesses, afterList.Assigned, len(afterList.PIDs), afterErr, afterListErr)
	if afterErr != nil || afterListErr != nil {
		t.Fatalf("released Job snapshot unavailable: %v/%v", afterErr, afterListErr)
	}
	for _, h := range retained {
		id, created, base, member, memberWait, inspectErr := inspect(h)
		t.Logf("TARGET_WINDOWS_RELEASED_MEMBER pid=%d creation=%d image=%s member=%v wait=%d err=%v", id, created, base, member, memberWait, inspectErr)
	}
	terminationErr := child.terminate(5000)
	var proof *JobTerminationProofError
	if errors.As(terminationErr, &proof) {
		t.Logf("TARGET_WINDOWS_TERMINATE stage=%s error=%v", proof.Stage, terminationErr)
	} else {
		t.Logf("TARGET_WINDOWS_TERMINATE error=%v", terminationErr)
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
