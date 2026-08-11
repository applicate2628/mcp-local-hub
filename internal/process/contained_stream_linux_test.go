//go:build linux

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
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

const (
	linuxClassifierTestHelperEnv  = "MCPHUB_LINUX_CLASSIFIER_TEST_HELPER"
	linuxClassifierTestPGIDEnv    = "MCPHUB_LINUX_CLASSIFIER_TEST_PGID"
	linuxClassifierTestHelperName = "TestLinuxClassifierSubprocess"
)

type linuxClassifierInvocation struct {
	executable string
	args       []string
	cmd        *exec.Cmd
}

func TestLinuxClassifierSubprocess(t *testing.T) {
	mode := os.Getenv(linuxClassifierTestHelperEnv)
	if mode == "" {
		return
	}
	switch mode {
	case "block":
		reader, writer, err := os.Pipe()
		if err != nil {
			os.Exit(2)
		}
		defer reader.Close()
		defer writer.Close()
		_, _ = reader.Read(make([]byte, 1))
	case "close-deadline":
		_, _ = io.WriteString(os.Stdout, linuxClassifierFramePrefix+"failure:close\n")
		reader, writer, err := os.Pipe()
		if err != nil {
			os.Exit(2)
		}
		defer reader.Close()
		defer writer.Close()
		_, _ = reader.Read(make([]byte, 1))
	case "read":
		_, _ = io.WriteString(os.Stdout, linuxClassifierFramePrefix+"failure:read\n")
		os.Exit(0)
	case "protocol":
		_, _ = io.WriteString(os.Stdout, "invalid-frame\n")
		os.Exit(0)
	case "empty":
		os.Exit(0)
	case "worker":
		pgid, err := strconv.Atoi(os.Getenv(linuxClassifierTestPGIDEnv))
		if err != nil || RunLinuxProcfsClassifierHelper(pgid, os.Stdout) != nil {
			os.Exit(2)
		}
		os.Exit(0)
	default:
		os.Exit(2)
	}
}

func TestLinuxClassifierFrameIsBoundedAndClosedVocabulary(t *testing.T) {
	for _, frame := range []string{
		linuxClassifierFramePrefix + "settled\n",
		linuxClassifierFramePrefix + "live\n",
		linuxClassifierFramePrefix + "failure:open,read,parse,close\n",
	} {
		if _, err := parseLinuxClassifierFrame([]byte(frame), false); err != nil {
			t.Fatalf("valid frame %q: %v", frame, err)
		}
	}
	for _, frame := range []string{
		"",
		linuxClassifierFramePrefix + "settled",
		linuxClassifierFramePrefix + "settled\nextra\n",
		"wrong:live\n",
		linuxClassifierFramePrefix + "failure:unknown\n",
		linuxClassifierFramePrefix + "failure:read,open\n",
		linuxClassifierFramePrefix + "failure:read,read\n",
	} {
		if _, err := parseLinuxClassifierFrame([]byte(frame), false); !errors.Is(err, errLinuxHelperProtocolInvalid) {
			t.Fatalf("invalid frame %q err=%v", frame, err)
		}
	}
	output := &boundedLinuxHelperOutput{}
	input := strings.Repeat("x", linuxClassifierMaxFrameBytes+64)
	if n, err := output.Write([]byte(input)); err != nil || n != len(input) {
		t.Fatalf("bounded write n/err=%d/%v", n, err)
	}
	if !output.overflow || len(output.data) != linuxClassifierMaxFrameBytes+1 {
		t.Fatalf("bounded output len/overflow=%d/%v", len(output.data), output.overflow)
	}
	if _, err := parseLinuxClassifierFrame(output.data, output.overflow); !errors.Is(err, errLinuxHelperProtocolInvalid) {
		t.Fatalf("oversize frame err=%v", err)
	}
}

func linuxClassifierTestFactory(mode string, invocation *linuxClassifierInvocation) linuxHelperCommandFactory {
	return func(ctx context.Context, executable string, args ...string) *exec.Cmd {
		cmd := exec.CommandContext(ctx, executable, "-test.run=^"+linuxClassifierTestHelperName+"$")
		pgid := ""
		if len(args) == 2 {
			pgid = args[1]
		}
		cmd.Env = append(os.Environ(),
			linuxClassifierTestHelperEnv+"="+mode,
			linuxClassifierTestPGIDEnv+"="+pgid,
		)
		invocation.executable = executable
		invocation.args = append([]string(nil), args...)
		invocation.cmd = cmd
		return cmd
	}
}

func TestLinuxGroupSettlementClassifiesZombieOnlyWithoutMaskingLiveMember(t *testing.T) {
	for _, tc := range []struct {
		name  string
		stats []string
		want  bool
		bad   bool
	}{
		{"empty", nil, true, false},
		{"all-zombie", []string{"1 (helper) Z 1 42 1 1", "2 (helper with spaces) Z 1 42 1 1"}, true, false},
		{"unrelated-live", []string{"3 (other) S 1 99 1 1", "1 (helper) Z 1 42 1 1"}, true, false},
		{"live-member", []string{"1 (helper) Z 1 42 1 1", "2 (helper with spaces) S 1 42 1 1"}, false, false},
		{"malformed", []string{"malformed"}, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			settlement := linuxGroupSettlement{pgid: 42}
			for _, stat := range tc.stats {
				err := settlement.observe(stat)
				if tc.bad {
					if err == nil {
						t.Fatal("malformed stat accepted")
					}
					return
				}
				if err != nil {
					t.Fatal(err)
				}
			}
			if got := settlement.settled(); got != tc.want {
				t.Fatalf("settled=%v, want %v", got, tc.want)
			}
		})
	}
}

func TestLinuxGroupSettlementHonorsClassifierDeadline(t *testing.T) {
	testCtx, testCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer testCancel()
	deadline := time.Now().Add(180 * time.Millisecond)
	startedAt := time.Now()
	var budget posixSettlementBudget
	var invocation linuxClassifierInvocation
	done := make(chan error, 1)
	go func() {
		done <- settlePOSIXGroup(deadline, 42, func(int) error { return syscall.EPERM }, func(ctx context.Context, pgid int, actual posixSettlementBudget) (bool, error) {
			budget = actual
			return runLinuxGroupClassifierWithFactory(ctx, pgid, actual, linuxClassifierTestFactory("block", &invocation))
		})
	}()
	err := receiveContainedTest(t, testCtx, done, "classifier deadline")
	if err != errLinuxGroupSettlementBudgetExhausted {
		t.Fatalf("err=%v, want %v", err, errLinuxGroupSettlementBudgetExhausted)
	}
	if errors.Is(err, ErrCleanupTimeout) {
		t.Fatalf("classifier budget exhaustion mislabeled cleanup timeout: %v", err)
	}
	if elapsed := time.Since(startedAt); elapsed <= budget.joinReserve {
		t.Fatalf("blocked helper elapsed=%v, want greater than join reserve %v", elapsed, budget.joinReserve)
	}
	if remaining := time.Until(deadline); remaining < budget.joinReserve {
		t.Fatalf("remaining=%v, want full join reserve %v", remaining, budget.joinReserve)
	}
	if invocation.cmd == nil || invocation.cmd.ProcessState == nil {
		t.Fatalf("helper was not joined: %+v", invocation.cmd)
	}
	if signalErr := invocation.cmd.Process.Signal(syscall.Signal(0)); !errors.Is(signalErr, os.ErrProcessDone) {
		t.Fatalf("helper remains signalable after Wait: %v", signalErr)
	}
	executable, resolveErr := os.Executable()
	if resolveErr != nil {
		t.Fatal(resolveErr)
	}
	executable, resolveErr = filepath.EvalSymlinks(executable)
	if resolveErr != nil {
		t.Fatal(resolveErr)
	}
	if invocation.executable != executable || !slices.Equal(invocation.args, []string{LinuxProcfsClassifierHelperCommand, "42"}) {
		t.Fatalf("invocation executable/args=%q/%v, want %q/%v", invocation.executable, invocation.args, executable, []string{LinuxProcfsClassifierHelperCommand, "42"})
	}
}

func TestPOSIXSettlementDeadlinePreservesJoinReserveOnClassifierFailure(t *testing.T) {
	runContainedSynctest(t, func(t *testing.T) {
		deadline := time.Now().Add(180 * time.Millisecond)
		var budget posixSettlementBudget
		err := settlePOSIXGroup(deadline, 42, func(int) error { return syscall.EPERM }, func(ctx context.Context, _ int, actual posixSettlementBudget) (bool, error) {
			budget = actual
			<-ctx.Done()
			return false, errors.Join(errLinuxProcCloseFailed, ctx.Err())
		})
		if !errors.Is(err, errLinuxProcCloseFailed) || !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("err=%v, want close failure plus context deadline", err)
		}
		if errors.Is(err, errLinuxGroupSettlementBudgetExhausted) || !errors.Is(err, errLinuxGroupSettlementIndeterminate) {
			t.Fatalf("err=%v, want indeterminate and not pure budget", err)
		}
		if remaining := time.Until(deadline); remaining < budget.joinReserve {
			t.Fatalf("remaining=%v, want full join reserve %v", remaining, budget.joinReserve)
		}
	})
}

func TestLinuxClassifierDeadlineJoinsCloseFailure(t *testing.T) {
	deadline := time.Now().Add(180 * time.Millisecond)
	budget := newPOSIXSettlementBudget(time.Now(), deadline, true)
	ctx, cancel := context.WithDeadline(context.Background(), budget.settlementDeadline)
	defer cancel()
	var invocation linuxClassifierInvocation
	_, err := runLinuxGroupClassifierWithFactory(ctx, 42, budget, linuxClassifierTestFactory("close-deadline", &invocation))
	if !errors.Is(err, errLinuxProcCloseFailed) || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err=%v, want close failure plus context deadline", err)
	}
	if invocation.cmd == nil || invocation.cmd.ProcessState == nil {
		t.Fatalf("helper was not joined: %+v", invocation.cmd)
	}
}

func TestLinuxGroupSettlementCompletedFailuresAreIndeterminate(t *testing.T) {
	for _, tc := range []struct {
		mode string
		want error
	}{
		{"read", errLinuxProcReadFailed},
		{"protocol", errLinuxHelperProtocolInvalid},
		{"empty", errLinuxHelperProtocolInvalid},
	} {
		t.Run(tc.mode, func(t *testing.T) {
			// The race-instrumented self-exec needs a deliberately wide startup
			// window so this guard proves a completed protocol outcome rather
			// than accidentally exercising the pure deadline path.
			deadline := time.Now().Add(3 * time.Second)
			var invocation linuxClassifierInvocation
			err := settlePOSIXGroup(deadline, 42, func(int) error { return syscall.EPERM }, func(ctx context.Context, pgid int, budget posixSettlementBudget) (bool, error) {
				return runLinuxGroupClassifierWithFactory(ctx, pgid, budget, linuxClassifierTestFactory(tc.mode, &invocation))
			})
			if !errors.Is(err, errLinuxGroupSettlementIndeterminate) || !errors.Is(err, tc.want) {
				t.Fatalf("err=%v, want indeterminate plus %v", err, tc.want)
			}
			if errors.Is(err, errLinuxGroupSettlementBudgetExhausted) {
				t.Fatalf("completed %s failure mislabeled pure budget: %v", tc.mode, err)
			}
			if invocation.cmd == nil || invocation.cmd.ProcessState == nil {
				t.Fatalf("helper was not joined: %+v", invocation.cmd)
			}
		})
	}
}

func TestRunContainedStreamPOSIX_LiveGroupStillTimesOut(t *testing.T) {
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
			err := settlePOSIXGroup(deadline, 42, func(int) error { return syscall.EPERM }, func(context.Context, int, posixSettlementBudget) (bool, error) {
				return false, nil
			})
			base.terminateOnce.Do(func() { base.waitCh <- containedWaitResult{exitCode: 0, exited: true} })
			return err
		}
		ctx, cancel := context.WithCancel(context.Background())
		h := &containedDependencyHarness{child: base}
		deps := h.dependencies()
		deps.newChild = func(*exec.Cmd) (containedChild, error) { return child, nil }
		done := make(chan error, 1)
		go func() {
			done <- runContainedStreamWithDependencies(ctx, exec.Command("contained-test-helper"), ContainedStreamOptions{CleanupTimeout: 120 * time.Millisecond, Stderr: io.Discard}, drainContainedReader, deps)
		}()
		receiveContainedTest(t, testCtx, started, "live-group runner start")
		cancel()
		if err := receiveContainedTest(t, testCtx, done, "live-group runner completion"); !errors.Is(err, ErrCleanupTimeout) || !errors.Is(err, errPOSIXGroupLiveTimeout) {
			t.Fatalf("err=%v, want completed-live cleanup timeout", err)
		}
		if base.terminateCalls.Load() != 1 || base.waitCalls.Load() != 1 {
			t.Fatalf("terminate/wait=%d/%d, want exact-one ownership", base.terminateCalls.Load(), base.waitCalls.Load())
		}
	})
}

func TestRunContainedStreamPOSIX_ZombieOnlyGroupDoesNotFalseTimeout(t *testing.T) {
	if os.Getpid() != 1 {
		t.Skip("requires test binary as non-reaping PID 1")
	}
	testCtx, testCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer testCancel()
	cmd := containedPOSIXHelperCommand(t, "zombie-owner")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	type identities struct{ child, zombie int }
	idsCh := make(chan identities, 1)
	done := make(chan error, 1)
	deps := defaultContainedStreamDependencies()
	productionNewChild := deps.newChild
	deps.newChild = func(command *exec.Cmd) (containedChild, error) {
		created, err := productionNewChild(command)
		if err != nil {
			return nil, err
		}
		child, ok := created.(*posixContainedChild)
		if !ok {
			return nil, fmt.Errorf("production child type = %T, want *posixContainedChild", created)
		}
		child.classifier = func(ctx context.Context, pgid int, budget posixSettlementBudget) (bool, error) {
			var invocation linuxClassifierInvocation
			return runLinuxGroupClassifierWithFactory(ctx, pgid, budget, linuxClassifierTestFactory("worker", &invocation))
		}
		return child, nil
	}
	go func() {
		done <- runContainedStreamWithDependencies(ctx, cmd, ContainedStreamOptions{CleanupTimeout: 250 * time.Millisecond}, func(r io.Reader) error {
			scanner := bufio.NewScanner(r)
			var ids identities
			for scanner.Scan() {
				line := scanner.Text()
				if value, ok := strings.CutPrefix(line, "child="); ok {
					ids.child, _ = strconv.Atoi(value)
				}
				if value, ok := strings.CutPrefix(line, "zombie="); ok {
					ids.zombie, _ = strconv.Atoi(value)
				}
				if line == "zombie-launched" {
					idsCh <- ids
				}
			}
			return scanner.Err()
		}, deps)
	}()
	var ids identities
	ids = receiveContainedTest(t, testCtx, idsCh, "zombie fixture start")
	waitLinuxState(t, ids.zombie, 'Z', 5*time.Second)
	data, err := os.ReadFile("/proc/" + strconv.Itoa(ids.zombie) + "/stat")
	if err != nil {
		t.Fatal(err)
	}
	_, pgid, err := linuxProcStateAndGroup(string(data))
	if err != nil || pgid != ids.child {
		t.Fatalf("zombie pgid=%d child=%d err=%v", pgid, ids.child, err)
	}
	cancel()
	runErr := receiveContainedTest(t, testCtx, done, "zombie fixture completion")
	if !errors.Is(runErr, context.Canceled) || errors.Is(runErr, ErrCleanupTimeout) {
		var lifecycle *ContainedRunError
		_ = errors.As(runErr, &lifecycle)
		if lifecycle != nil {
			t.Fatalf("RunContainedStream stage=%s cause=%v cleanup_stage=%s cleanup_cause=%v group=%v", lifecycle.Stage, lifecycle.Cause, lifecycle.CleanupStage, lifecycle.CleanupCause, linuxGroupSnapshot(ids.child))
		}
		t.Fatalf("RunContainedStream err=%v", runErr)
	}
	// Product cleanup owns the adopted zombie and must reap it before return;
	// a non-reaping PID 1 is the falsifying environment for this contract.
	deadline := time.Now().Add(5 * time.Second)
	for _, pid := range []int{ids.child, ids.zombie} {
		for {
			_, err := os.Stat("/proc/" + strconv.Itoa(pid))
			if errors.Is(err, os.ErrNotExist) {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("pid %d remains after product cleanup: %v", pid, err)
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
}

func linuxGroupSnapshot(pgid int) []string {
	entries, _ := os.ReadDir("/proc")
	var out []string
	for _, entry := range entries {
		if _, err := strconv.Atoi(entry.Name()); err != nil {
			continue
		}
		data, err := os.ReadFile("/proc/" + entry.Name() + "/stat")
		if err != nil {
			continue
		}
		state, group, err := linuxProcStateAndGroup(string(data))
		if err == nil && group == pgid {
			out = append(out, entry.Name()+":"+string(state))
		}
	}
	return out
}

func waitLinuxState(t *testing.T, pid int, want byte, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		data, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
		if err == nil {
			state, _, parseErr := linuxProcStateAndGroup(string(data))
			if parseErr == nil && state == want {
				return
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("pid %d did not reach state %q", pid, want)
}
