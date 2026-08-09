package process

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"testing"
	"time"
)

func TestRunStrictlyContained_FastExitDrainsBeforeReadEndsClose(t *testing.T) {
	if os.Getenv("MCPHUB_STRICT_FAST_EXIT_HELPER") == "1" {
		_, _ = os.Stdout.Write(bytes.Repeat([]byte("o"), 32*1024+17))
		_, _ = os.Stderr.Write(bytes.Repeat([]byte("e"), 24*1024+11))
		os.Exit(0)
	}
	for iteration := 0; iteration < 40; iteration++ {
		cmd := exec.Command(os.Args[0], "-test.run=^TestRunStrictlyContained_FastExitDrainsBeforeReadEndsClose$")
		cmd.Env = append(os.Environ(), "MCPHUB_STRICT_FAST_EXIT_HELPER=1")
		result, err := RunStrictlyContained(context.Background(), StrictRunInvocation{
			Command: cmd, Input: []byte{}, InputLimit: 1, StdoutLimit: 64 * 1024, StderrLimit: 64 * 1024,
		})
		if err != nil {
			t.Fatalf("iteration %d: fast worker error=%v", iteration, err)
		}
		if result.Stdout.Bytes != 32*1024+17 || result.Stderr.Bytes != 24*1024+11 || result.Stdout.Truncated || result.Stderr.Truncated {
			t.Fatalf("iteration %d: stdout bytes=%d truncated=%t stderr bytes=%d truncated=%t", iteration, result.Stdout.Bytes, result.Stdout.Truncated, result.Stderr.Bytes, result.Stderr.Truncated)
		}
	}
}

func TestRunStrictlyContained_DeadlineReapsDirectChild(t *testing.T) {
	if os.Getenv("MCPHUB_STRICT_CONTAINED_HELPER") == "1" {
		time.Sleep(time.Hour)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 80*time.Millisecond)
	defer cancel()
	cmd := exec.Command(os.Args[0], "-test.run=^TestRunStrictlyContained_DeadlineReapsDirectChild$")
	cmd.Env = append(os.Environ(), "MCPHUB_STRICT_CONTAINED_HELPER=1")
	started := time.Now()
	_, err := RunStrictlyContained(ctx, StrictRunInvocation{Command: cmd, Input: []byte("{}"), InputLimit: 2, StdoutLimit: 1024, StderrLimit: 1024})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("RunStrictlyContained error = %v, want deadline exceeded", err)
	}
	if elapsed := time.Since(started); elapsed > 3*time.Second {
		t.Fatalf("RunStrictlyContained returned after %s; worker containment was not bounded", elapsed)
	}
	if cmd.Process == nil {
		t.Fatal("worker process was never started")
	}
	pid := cmd.Process.Pid
	if IsPidAlive(pid) {
		t.Fatalf("worker PID %d survived containment return", pid)
	}
}

func TestRunStrictlyContained_MissingExecutableIsExecutionFailure(t *testing.T) {
	cmd := exec.Command(`definitely-not-a-real-mcphub-strict-worker`)
	_, err := RunStrictlyContained(context.Background(), StrictRunInvocation{
		Command: cmd,
		Input:   []byte("{}"), InputLimit: 2, StdoutLimit: 1024, StderrLimit: 1024,
	})
	var strictErr *StrictRunError
	if !errors.As(err, &strictErr) || strictErr.Kind != StrictRunExecutionFailed {
		t.Fatalf("missing executable error=%v, want execution failure", err)
	}
	var startErr *StartWithJobError
	if !errors.As(err, &startErr) || startErr.Phase != StartWithJobLaunch {
		t.Fatalf("missing executable start error=%v, want launch phase", err)
	}
	if !errors.Is(err, os.ErrNotExist) && (cmd.Err == nil || !errors.Is(err, cmd.Err)) {
		t.Fatalf("missing executable error=%v, want original command or launch file-not-found cause", err)
	}
}

func TestRunStrictlyContained_PipePreparationClosesOwnedJob(t *testing.T) {
	for _, failAt := range []int{1, 2, 3} {
		t.Run(fmt.Sprintf("pipe-%d", failAt), func(t *testing.T) {
			job, err := NewKillOnCloseJob()
			if err != nil {
				t.Fatal(err)
			}
			sentinel := errors.New("injected pipe failure")
			calls := 0
			_, err = runStrictlyContainedWithJob(context.Background(), StrictRunInvocation{
				Command: exec.Command(os.Args[0]), Input: []byte("{}"), InputLimit: 2, StdoutLimit: 1024, StderrLimit: 1024,
				pipe: func() (*os.File, *os.File, error) {
					calls++
					if calls == failAt {
						return nil, nil, sentinel
					}
					return os.Pipe()
				},
			}, job)
			if !errors.Is(err, sentinel) {
				t.Fatalf("pipe failure=%v, want sentinel", err)
			}
			if err := job.Close(); err != nil {
				t.Fatalf("owned Job was not already closed: %v", err)
			}
		})
	}
}
