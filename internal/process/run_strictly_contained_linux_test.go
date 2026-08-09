//go:build linux

package process

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestRunStrictlyContained_NormalExitSettlesDescendantBeforeReap(t *testing.T) {
	if os.Getenv("MCPHUB_STRICT_PROCESS_GROUP_HELPER") == "1" {
		devNull, err := os.OpenFile(os.DevNull, os.O_RDWR, 0)
		if err != nil {
			os.Exit(2)
		}
		child := exec.Command(os.Args[0], "-test.run=^TestRunStrictlyContained_NormalExitSettlesDescendantBeforeReap$")
		child.Env = append(os.Environ(), "MCPHUB_STRICT_PROCESS_GROUP_DESCENDANT=1")
		child.Stdin, child.Stdout, child.Stderr = devNull, devNull, devNull
		if err := child.Start(); err != nil {
			os.Exit(3)
		}
		_, _ = os.Stdout.WriteString(strconv.Itoa(child.Process.Pid))
		os.Exit(0)
	}
	if os.Getenv("MCPHUB_STRICT_PROCESS_GROUP_DESCENDANT") == "1" {
		time.Sleep(time.Hour)
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.Command(os.Args[0], "-test.run=^TestRunStrictlyContained_NormalExitSettlesDescendantBeforeReap$")
	cmd.Env = append(os.Environ(), "MCPHUB_STRICT_PROCESS_GROUP_HELPER=1")
	result, err := RunStrictlyContained(ctx, StrictRunInvocation{
		Command: cmd, InputLimit: 1, StdoutLimit: 64, StderrLimit: 64,
	})
	if err != nil {
		t.Fatalf("RunStrictlyContained() error = %v", err)
	}
	descendantPID, err := strconv.Atoi(strings.TrimSpace(string(result.Stdout.Prefix)))
	if err != nil {
		t.Fatalf("parse descendant PID from %q: %v", result.Stdout.Prefix, err)
	}
	t.Cleanup(func() { _ = syscall.Kill(descendantPID, syscall.SIGKILL) })
	deadline := time.Now().Add(2 * time.Second)
	for {
		err = syscall.Kill(descendantPID, 0)
		if errors.Is(err, syscall.ESRCH) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("descendant PID %d remains after contained normal completion: %v", descendantPID, err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
