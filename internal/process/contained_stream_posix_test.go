//go:build !windows

package process

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

const containedPOSIXHelperEnv = "MCPHUB_CONTAINED_POSIX_HELPER"

func TestContainedStreamPOSIXHelper(t *testing.T) {
	mode := os.Getenv(containedPOSIXHelperEnv)
	if mode == "" {
		return
	}
	switch mode {
	case "group":
		fmt.Printf("pid=%d pgid=%d\n", os.Getpid(), syscall.Getpgrp())
	case "tree":
		exe, err := os.Executable()
		if err != nil {
			t.Fatal(err)
		}
		grandchild := exec.Command(exe, "-test.run=^TestContainedStreamPOSIXHelper$")
		grandchild.Env = append(os.Environ(), containedPOSIXHelperEnv+"=hold")
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
	default:
		t.Fatalf("unknown helper mode %q", mode)
	}
}

func containedPOSIXHelperCommand(t *testing.T, mode string) *exec.Cmd {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(exe, "-test.run=^TestContainedStreamPOSIXHelper$")
	cmd.Env = append(os.Environ(), containedPOSIXHelperEnv+"="+mode)
	return cmd
}

func TestRunContainedStreamPOSIX_StartsAsGroupLeader(t *testing.T) {
	cmd := containedPOSIXHelperCommand(t, "group")
	var stdout strings.Builder
	err := RunContainedStream(
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
	var pid, pgid int
	if _, err := fmt.Sscanf(strings.TrimSpace(stdout.String()), "pid=%d pgid=%d", &pid, &pgid); err != nil {
		t.Fatalf("parse helper output %q: %v", stdout.String(), err)
	}
	if pid <= 0 || pid != pgid {
		t.Fatalf("pid=%d pgid=%d, want positive group leader", pid, pgid)
	}
}

func TestRunContainedStreamPOSIX_GroupSetupFailureStartsNoUngroupedChild(t *testing.T) {
	sentinel := newPortabilitySentinelPath(t)
	cmd := containedPOSIXHelperCommand(t, "sentinel")
	cmd.Env = append(cmd.Env, "MCPHUB_SENTINEL_PATH="+sentinel)
	h := &containedDependencyHarness{
		child:       newScriptedContainedChild(containedWaitResult{}),
		newChildErr: errors.New("group setup sentinel"),
	}
	err := runContainedStreamWithDependencies(
		context.Background(),
		cmd,
		ContainedStreamOptions{CleanupTimeout: time.Second},
		drainContainedReader,
		h.dependencies(),
	)
	if !errors.Is(err, ErrContainmentUnavailable) {
		t.Fatalf("err=%v, want containment unavailable", err)
	}
	if h.child.startCalls.Load() != 0 {
		t.Fatalf("start calls=%d, want 0", h.child.startCalls.Load())
	}
	if _, statErr := os.Stat(sentinel); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("sentinel stat=%v, child ran ungrouped", statErr)
	}
}

func newPortabilitySentinelPath(t *testing.T) string {
	t.Helper()
	return t.TempDir() + string(os.PathSeparator) + "child-started"
}

func TestRunContainedStreamPOSIX_GrandchildPipeHolderDoesNotLeak(t *testing.T) {
	cmd := containedPOSIXHelperCommand(t, "tree")
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
	waitForContainedPOSIXExit(t, ids.child)
	waitForContainedPOSIXExit(t, ids.grandchild)
}

func waitForContainedPOSIXExit(t *testing.T, pid int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if errors.Is(syscall.Kill(pid, 0), syscall.ESRCH) {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("pid %d survived contained runner return", pid)
}

func TestRunContainedStreamPOSIX_CleanupTimeoutIsTyped(t *testing.T) {
	child := &scriptedContainedChild{waitCh: make(chan containedWaitResult, 1)}
	child.terminateFn = func(uint32) error {
		child.terminateOnce.Do(func() {
			child.waitCh <- containedWaitResult{exitCode: 0, exited: true}
		})
		return ErrCleanupTimeout
	}
	h := &containedDependencyHarness{child: child}
	err := runHarness(context.Background(), h, time.Second, io.Discard, drainContainedReader)
	if !errors.Is(err, ErrCleanupTimeout) {
		t.Fatalf("err=%v, want ErrCleanupTimeout", err)
	}
}
