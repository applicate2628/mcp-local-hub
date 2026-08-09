//go:build windows

package process

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

func TestRunStrictlyContained_WindowsRealThreeChannelsAndJob(t *testing.T) {
	if os.Getenv("MCPHUB_STRICT_WINDOWS_CHANNEL_HELPER") == "1" {
		buf := make([]byte, 7)
		if _, err := io.ReadFull(os.Stdin, buf); err != nil {
			os.Exit(21)
		}
		one := make([]byte, 1)
		if n, err := os.Stdin.Read(one); n != 0 || !errors.Is(err, io.EOF) {
			os.Exit(22)
		}
		ambient := "ambient-excluded"
		if raw := os.Getenv("MCPHUB_STRICT_WINDOWS_AMBIENT_HANDLE"); raw != "" {
			if value, parseErr := strconv.ParseUint(raw, 10, 0); parseErr == nil {
				if _, handleErr := windows.GetFileType(windows.Handle(value)); handleErr == nil {
					ambient = "ambient-inherited"
				}
			}
		}
		_, _ = os.Stdout.Write(append(append([]byte("out:"), buf...), []byte(":"+ambient)...))
		_, _ = os.Stderr.Write(bytes.Repeat([]byte("s"), 16*1024+17))
		os.Exit(3)
	}
	first := runWindowsRealThreeChannels(t)
	second := runWindowsRealThreeChannels(t)
	if first.pid == second.pid {
		t.Fatalf("second execution reused live PID %d", first.pid)
	}
}

type windowsStrictRunObservation struct {
	pid int
}

func runWindowsRealThreeChannels(t *testing.T) windowsStrictRunObservation {
	t.Helper()
	job, err := NewKillOnCloseJob()
	if err != nil {
		t.Fatal(err)
	}
	if job.Handle() == 0 {
		t.Fatal("new Job has zero handle")
	}
	ambientRead, ambientWrite, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer ambientRead.Close()
	defer ambientWrite.Close()
	if err := windows.SetHandleInformation(windows.Handle(ambientRead.Fd()), windows.HANDLE_FLAG_INHERIT, windows.HANDLE_FLAG_INHERIT); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestRunStrictlyContained_WindowsRealThreeChannelsAndJob$")
	cmd.Env = append(os.Environ(), "MCPHUB_STRICT_WINDOWS_CHANNEL_HELPER=1", "MCPHUB_STRICT_WINDOWS_AMBIENT_HANDLE="+strconv.FormatUint(uint64(ambientRead.Fd()), 10))
	pipeCalls := 0
	var endpoints []*os.File
	startedCalls := 0
	member := false
	pid := 0
	result, runErr := runStrictlyContainedWithJob(context.Background(), StrictRunInvocation{
		Command: cmd, Input: []byte("payload"), InputLimit: 7, StdoutLimit: 1024, StderrLimit: 16 * 1024,
		pipe: func() (*os.File, *os.File, error) {
			pipeCalls++
			left, right, pipeErr := os.Pipe()
			if pipeErr == nil {
				endpoints = append(endpoints, left, right)
			}
			return left, right, pipeErr
		},
		started: func(startedJob *Job, startedPID int) {
			startedCalls++
			pid = startedPID
			member = startedJob == job && job.HasMember(startedPID)
		},
	}, job)
	var strictErr *StrictRunError
	if !errors.As(runErr, &strictErr) || strictErr.Kind != StrictRunExecutionFailed {
		t.Fatalf("nonzero helper error=%v, want execution failure", runErr)
	}
	if pipeCalls != 3 || len(endpoints) != 6 || startedCalls != 1 {
		t.Fatalf("resource counts: pipe=%d endpoints=%d started=%d", pipeCalls, len(endpoints), startedCalls)
	}
	if !member {
		t.Fatal("real child was not a member of the supplied Job before input")
	}
	if job.Handle() != 0 {
		t.Fatalf("owned Job handle survived return: %v", job.Handle())
	}
	if pid <= 0 || IsPidAlive(pid) {
		t.Fatalf("worker PID after return=%d alive=%t", pid, IsPidAlive(pid))
	}
	assertWindowsStrictEndpointsClosed(t, endpoints)
	wantStdout := "out:payload:ambient-excluded"
	if string(result.Stdout.Prefix) != wantStdout || result.Stdout.Bytes != len(wantStdout) || result.Stdout.Truncated {
		t.Fatalf("stdout=%+v want exact %q", result.Stdout, wantStdout)
	}
	if result.Stderr.Bytes != 16*1024+17 || !result.Stderr.Truncated || len(result.Stderr.Prefix) != 16*1024 {
		t.Fatalf("stderr capture=%+v", result.Stderr)
	}
	return windowsStrictRunObservation{pid: pid}
}

func assertWindowsStrictEndpointsClosed(t *testing.T, endpoints []*os.File) {
	t.Helper()
	for index, endpoint := range endpoints {
		if err := endpoint.Close(); !errors.Is(err, os.ErrClosed) {
			t.Fatalf("endpoint %d second close error=%v, want os.ErrClosed", index, err)
		}
	}
}

func TestRunStrictlyContained_WindowsValidationClosesOwnedJob(t *testing.T) {
	validInvocation := func() StrictRunInvocation {
		return StrictRunInvocation{Command: exec.Command(os.Args[0]), Input: []byte("{}"), InputLimit: 2, StdoutLimit: 1024, StderrLimit: 1024}
	}
	tests := []struct {
		name      string
		ctx       context.Context
		mutate    func(*StrictRunInvocation)
		wantCause string
	}{
		{name: "nil-context", ctx: nil, wantCause: "nil context"},
		{name: "nil-command", ctx: context.Background(), mutate: func(i *StrictRunInvocation) { i.Command = nil }, wantCause: "nil command"},
		{name: "empty-command-path", ctx: context.Background(), mutate: func(i *StrictRunInvocation) { i.Command = &exec.Cmd{} }, wantCause: "command path is empty"},
		{name: "nonpositive-limit", ctx: context.Background(), mutate: func(i *StrictRunInvocation) { i.StdoutLimit = 0 }, wantCause: "strict stream limits must be positive"},
		{name: "oversized-input", ctx: context.Background(), mutate: func(i *StrictRunInvocation) { i.InputLimit = 1 }, wantCause: "strict input exceeds limit"},
		{name: "owned-stream", ctx: context.Background(), mutate: func(i *StrictRunInvocation) { i.Command.Stdin = strings.NewReader("ambient") }, wantCause: "command standard streams must be nil"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			job, err := NewKillOnCloseJob()
			if err != nil || job.Handle() == 0 {
				t.Fatalf("new Job: handle=%v err=%v", job.Handle(), err)
			}
			invocation := validInvocation()
			if tc.mutate != nil {
				tc.mutate(&invocation)
			}
			pipeCalls, startedCalls := 0, 0
			invocation.pipe = func() (*os.File, *os.File, error) { pipeCalls++; return os.Pipe() }
			invocation.started = func(*Job, int) { startedCalls++ }
			_, err = runStrictlyContainedWithJob(tc.ctx, invocation, job)
			var strictErr *StrictRunError
			if !errors.As(err, &strictErr) || strictErr.Kind != StrictRunInvalidInvocation || !strings.Contains(strictErr.Cause.Error(), tc.wantCause) {
				t.Fatalf("error=%v, want invalid invocation containing %q", err, tc.wantCause)
			}
			if job.Handle() != 0 || pipeCalls != 0 || startedCalls != 0 {
				t.Fatalf("after return: handle=%v pipe=%d started=%d", job.Handle(), pipeCalls, startedCalls)
			}
		})
	}

	t.Run("validation-and-close-causes-joined", func(t *testing.T) {
		job, err := NewKillOnCloseJob()
		if err != nil || job.Handle() == 0 {
			t.Fatalf("new Job: handle=%v err=%v", job.Handle(), err)
		}
		if err := windows.CloseHandle(job.Handle()); err != nil {
			t.Fatal(err)
		}
		_, err = runStrictlyContainedWithJob(nil, validInvocation(), job)
		var strictErr *StrictRunError
		if !errors.As(err, &strictErr) || strictErr.Kind != StrictRunInvalidInvocation || !strings.Contains(strictErr.Cause.Error(), "nil context") {
			t.Fatalf("primary validation error=%v", err)
		}
		if !errors.Is(err, windows.ERROR_INVALID_HANDLE) {
			t.Fatalf("joined close error=%v, want ERROR_INVALID_HANDLE", err)
		}
		if job.Handle() != 0 {
			t.Fatalf("invalid underlying handle was not zeroed: %v", job.Handle())
		}
	})
}

func TestRunStrictlyContained_WindowsDeadlineClosesAllResources(t *testing.T) {
	if os.Getenv("MCPHUB_STRICT_WINDOWS_DEADLINE_HELPER") == "1" {
		time.Sleep(time.Hour)
		return
	}
	job, err := NewKillOnCloseJob()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestRunStrictlyContained_WindowsDeadlineClosesAllResources$")
	cmd.Env = append(os.Environ(), "MCPHUB_STRICT_WINDOWS_DEADLINE_HELPER=1")
	pipeCalls := 0
	var endpoints []*os.File
	pid, startedCalls := 0, 0
	ctx, cancel := context.WithTimeout(context.Background(), 80*time.Millisecond)
	defer cancel()
	_, err = runStrictlyContainedWithJob(ctx, StrictRunInvocation{
		Command: cmd, Input: []byte("{}"), InputLimit: 2, StdoutLimit: 1024, StderrLimit: 1024,
		pipe: func() (*os.File, *os.File, error) {
			pipeCalls++
			left, right, pipeErr := os.Pipe()
			if pipeErr == nil {
				endpoints = append(endpoints, left, right)
			}
			return left, right, pipeErr
		},
		started: func(startedJob *Job, startedPID int) {
			startedCalls++
			pid = startedPID
			if startedJob != job || !job.HasMember(startedPID) {
				t.Error("deadline helper was not in supplied Job")
			}
		},
	}, job)
	var strictErr *StrictRunError
	if !errors.As(err, &strictErr) || strictErr.Kind != StrictRunTimeout || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("deadline error=%v, want timeout with context cause", err)
	}
	if pipeCalls != 3 || len(endpoints) != 6 || startedCalls != 1 || job.Handle() != 0 {
		t.Fatalf("deadline resources: pipe=%d endpoints=%d started=%d handle=%v", pipeCalls, len(endpoints), startedCalls, job.Handle())
	}
	if pid <= 0 || IsPidAlive(pid) {
		t.Fatalf("deadline PID after return=%d alive=%t", pid, IsPidAlive(pid))
	}
	assertWindowsStrictEndpointsClosed(t, endpoints)
}

func TestRunStrictlyContained_WindowsTimeoutClosesJobBeforeWaitWhenKillsFail(t *testing.T) {
	if os.Getenv("MCPHUB_STRICT_WINDOWS_JOB_CLOSE_HELPER") == "1" {
		time.Sleep(time.Hour)
		return
	}
	job, err := NewKillOnCloseJob()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestRunStrictlyContained_WindowsTimeoutClosesJobBeforeWaitWhenKillsFail$")
	cmd.Env = append(os.Environ(), "MCPHUB_STRICT_WINDOWS_JOB_CLOSE_HELPER=1")
	killFailure := errors.New("injected TerminateAll and Process.Kill failure")
	ctx, cancel := context.WithTimeout(context.Background(), 80*time.Millisecond)
	defer cancel()
	started := time.Now()
	_, err = runStrictlyContainedWithJob(ctx, StrictRunInvocation{
		Command: cmd, Input: []byte("{}"), InputLimit: 2, StdoutLimit: 1024, StderrLimit: 1024,
		timeoutKill: func(*Job, *exec.Cmd) error { return killFailure },
	}, job)
	if !errors.Is(err, context.DeadlineExceeded) || !errors.Is(err, killFailure) {
		t.Fatalf("timeout error=%v, want deadline and injected kill failure", err)
	}
	if time.Since(started) > 3*time.Second {
		t.Fatalf("timeout remained blocked after failed kill primitives")
	}
	if job.Handle() != 0 || cmd.Process == nil || IsPidAlive(cmd.Process.Pid) {
		t.Fatalf("job-close fallback after timeout: handle=%v process=%v alive=%t", job.Handle(), cmd.Process, cmd.Process != nil && IsPidAlive(cmd.Process.Pid))
	}
}

func TestRunStrictlyContained_ClosedJobIsContainmentFailure(t *testing.T) {
	job, err := NewKillOnCloseJob()
	if err != nil {
		t.Fatal(err)
	}
	if err := job.Close(); err != nil {
		t.Fatal(err)
	}
	_, err = runStrictlyContainedWithJob(context.Background(), StrictRunInvocation{
		Command: exec.Command(os.Args[0]), Input: []byte("{}"), InputLimit: 2, StdoutLimit: 1024, StderrLimit: 1024,
	}, job)
	var strictErr *StrictRunError
	if !errors.As(err, &strictErr) || strictErr.Kind != StrictRunContainmentFailed {
		t.Fatalf("closed Job error=%v, want containment failure", err)
	}
	var startErr *StartWithJobError
	if !errors.As(err, &startErr) || startErr.Phase != StartWithJobContainment {
		t.Fatalf("closed Job start error=%v, want containment phase", err)
	}
}
