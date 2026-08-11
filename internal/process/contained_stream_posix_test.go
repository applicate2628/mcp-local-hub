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

func receiveContainedTest[T any](t *testing.T, ctx context.Context, ch <-chan T, what string) T {
	t.Helper()
	select {
	case value := <-ch:
		return value
	case <-ctx.Done():
		t.Fatalf("%s: %v", what, ctx.Err())
		var zero T
		return zero
	}
}

type deadlineScriptedContainedChild struct {
	*scriptedContainedChild
	terminateByFn func(time.Time) error
}

func (c *deadlineScriptedContainedChild) terminateBy(deadline time.Time) error {
	c.terminateCalls.Add(1)
	if c.terminateByFn != nil {
		return c.terminateByFn(deadline)
	}
	return nil
}

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
	case "zombie-owner":
		exe, err := os.Executable()
		if err != nil {
			t.Fatal(err)
		}
		grandchild := exec.Command(exe, "-test.run=^TestContainedStreamPOSIXHelper$")
		grandchild.Env = append(os.Environ(), containedPOSIXHelperEnv+"=exit-now")
		if err := grandchild.Start(); err != nil {
			t.Fatal(err)
		}
		fmt.Printf("child=%d\nzombie=%d\nzombie-launched\n", os.Getpid(), grandchild.Process.Pid)
		// Intentionally do not Wait: the parent stays alive while the test
		// observes the exited child as Z, then product cleanup kills this owner.
		time.Sleep(30 * time.Second)
	case "exit-now":
		return
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
	state, _ := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/status")
	var waitStatus syscall.WaitStatus
	reaped, waitErr := syscall.Wait4(pid, &waitStatus, syscall.WNOHANG, nil)
	t.Fatalf("pid %d survived contained runner return (test pid %d, diagnostic wait=%d err=%v): %s", pid, os.Getpid(), reaped, waitErr, state)
}

func TestRunContainedStreamPOSIX_CleanupTimeoutIsTyped(t *testing.T) {
	runContainedSynctest(t, func(t *testing.T) {
		testCtx, testCancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer testCancel()
		child := &scriptedContainedChild{waitCh: make(chan containedWaitResult, 1)}
		started := make(chan struct{})
		child.startFn = func(containedInputFile, containedWriteFile, containedWriteFile) error {
			close(started)
			return nil
		}
		child.terminateFn = func(uint32) error {
			child.terminateOnce.Do(func() {
				child.waitCh <- containedWaitResult{exitCode: 0, exited: true}
			})
			return ErrCleanupTimeout
		}
		h := &containedDependencyHarness{child: child}
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		done := make(chan error, 1)
		go func() { done <- runHarness(ctx, h, time.Second, io.Discard, drainContainedReader) }()
		receiveContainedTest(t, testCtx, started, "contained runner start")
		cancel()
		err := receiveContainedTest(t, testCtx, done, "contained runner completion")
		if !errors.Is(err, ErrCleanupTimeout) {
			t.Fatalf("err=%v, want ErrCleanupTimeout", err)
		}
		if child.terminateCalls.Load() != 1 || child.waitCalls.Load() != 1 {
			t.Fatalf("terminate/wait=%d/%d, want exact-one", child.terminateCalls.Load(), child.waitCalls.Load())
		}
	})
}

func TestPOSIXSettlementSlowZombieClassifierLeavesJoinReserve(t *testing.T) {
	testCtx, testCancel := context.WithTimeout(context.Background(), time.Second)
	defer testCancel()
	deadline := time.Now().Add(160 * time.Millisecond)
	budget := newPOSIXSettlementBudget(time.Now(), deadline, true)
	if budget.joinReserve <= 0 {
		t.Fatalf("join reserve=%v, want positive", budget.joinReserve)
	}
	classifierStarted := make(chan struct{})
	var receivedBudget posixSettlementBudget
	classifier := func(ctx context.Context, _ int, actual posixSettlementBudget) (bool, error) {
		receivedBudget = actual
		select {
		case <-classifierStarted:
		default:
			close(classifierStarted)
		}
		timer := time.NewTimer(25 * time.Millisecond)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return false, ctx.Err()
		case <-timer.C:
			return true, nil
		}
	}
	done := make(chan error, 1)
	go func() {
		done <- settlePOSIXGroup(deadline, 42, func(int) error { return syscall.EPERM }, classifier)
	}()
	receiveContainedTest(t, testCtx, classifierStarted, "slow classifier start")
	if err := receiveContainedTest(t, testCtx, done, "slow classifier completion"); err != nil {
		t.Fatalf("settlePOSIXGroup: %v", err)
	}
	if remaining := time.Until(deadline); remaining < receivedBudget.joinReserve {
		t.Fatalf("remaining=%v, want full join reserve %v", remaining, receivedBudget.joinReserve)
	}
}

func TestRunContainedStreamPOSIX_ReadyCompletionsBeatJoinDeadline(t *testing.T) {
	runContainedSynctest(t, func(t *testing.T) {
		testCtx, testCancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer testCancel()
		base := &scriptedContainedChild{waitCh: make(chan containedWaitResult, 1)}
		started := make(chan struct{})
		base.startFn = func(containedInputFile, containedWriteFile, containedWriteFile) error {
			close(started)
			return nil
		}
		child := &deadlineScriptedContainedChild{scriptedContainedChild: base}
		child.terminateByFn = func(deadline time.Time) error {
			base.terminateOnce.Do(func() {
				base.waitCh <- containedWaitResult{exitCode: 0, exited: true}
			})
			timer := time.NewTimer(max(time.Duration(0), time.Until(deadline)) + 5*time.Millisecond)
			defer timer.Stop()
			select {
			case <-testCtx.Done():
				return testCtx.Err()
			case <-timer.C:
				return nil
			}
		}
		h := &containedDependencyHarness{child: base}
		deps := h.dependencies()
		deps.newChild = func(*exec.Cmd) (containedChild, error) { return child, nil }
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		done := make(chan error, 1)
		go func() {
			done <- runContainedStreamWithDependencies(ctx, exec.Command("contained-test-helper"), ContainedStreamOptions{CleanupTimeout: 40 * time.Millisecond, Stderr: io.Discard}, drainContainedReader, deps)
		}()
		receiveContainedTest(t, testCtx, started, "ready-completion runner start")
		cancel()
		err := receiveContainedTest(t, testCtx, done, "ready-completion runner completion")
		if !errors.Is(err, context.Canceled) || errors.Is(err, ErrCleanupTimeout) {
			t.Fatalf("err=%v, want context.Canceled without cleanup timeout", err)
		}
		if base.terminateCalls.Load() != 1 || base.waitCalls.Load() != 1 {
			t.Fatalf("terminate/wait=%d/%d, want exact-one", base.terminateCalls.Load(), base.waitCalls.Load())
		}
		if len(h.pipes) != 2 {
			t.Fatalf("pipe count=%d, want stdout and stderr", len(h.pipes))
		}
		for index, pipe := range h.pipes {
			pipe.mu.Lock()
			readClosed, writeClosed := pipe.readClosed, pipe.writeClosed
			pipe.mu.Unlock()
			if !readClosed || !writeClosed {
				t.Fatalf("pipe %d joined read/write=%v/%v", index, readClosed, writeClosed)
			}
		}
	})
}
