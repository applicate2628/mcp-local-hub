//go:build linux || darwin || dragonfly || freebsd || netbsd || openbsd

package process

import (
	"errors"
	"os"
	"os/exec"
	"reflect"
	"testing"
)

func TestWaitStrictContainedCommand_SignalsBeforeReap(t *testing.T) {
	cmd := &exec.Cmd{Process: &os.Process{Pid: 12345}}
	var calls []string
	err := waitStrictContainedCommandWith(
		cmd,
		func(pid int) error {
			calls = append(calls, "observe")
			if pid != cmd.Process.Pid {
				t.Fatalf("observe pid = %d, want %d", pid, cmd.Process.Pid)
			}
			return nil
		},
		func(got *exec.Cmd) {
			calls = append(calls, "signal")
			if got != cmd {
				t.Fatal("signal received a different command")
			}
		},
		func() error {
			calls = append(calls, "reap")
			return nil
		},
	)
	if err != nil {
		t.Fatalf("waitStrictContainedCommandWith() error = %v", err)
	}
	if want := []string{"observe", "signal", "reap"}; !reflect.DeepEqual(calls, want) {
		t.Fatalf("calls = %v, want %v", calls, want)
	}
}

func TestWaitStrictContainedCommand_ObservationFailureStillSignalsBeforeReap(t *testing.T) {
	cmd := &exec.Cmd{Process: &os.Process{Pid: 12345}}
	observeErr := errors.New("observe failed")
	var calls []string
	err := waitStrictContainedCommandWith(
		cmd,
		func(int) error { calls = append(calls, "observe"); return observeErr },
		func(*exec.Cmd) { calls = append(calls, "signal") },
		func() error { calls = append(calls, "reap"); return nil },
	)
	if !errors.Is(err, observeErr) {
		t.Fatalf("error = %v, want observation cause", err)
	}
	if want := []string{"observe", "signal", "reap"}; !reflect.DeepEqual(calls, want) {
		t.Fatalf("calls = %v, want %v", calls, want)
	}
}
