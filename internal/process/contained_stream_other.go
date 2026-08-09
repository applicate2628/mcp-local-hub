//go:build !windows

package process

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"
)

const containedGroupPollInterval = 10 * time.Millisecond

var (
	errLinuxGroupSettlementBudgetExhausted = errors.New("LINUX_GROUP_SETTLEMENT_BUDGET_EXHAUSTED")
	errLinuxGroupSettlementIndeterminate   = errors.New("LINUX_GROUP_SETTLEMENT_INDETERMINATE")
	errPOSIXGroupLiveTimeout               = errors.New("POSIX_GROUP_LIVE_TIMEOUT")
)

type posixGroupProbe func(int) error
type posixGroupClassifier func(context.Context, int, posixSettlementBudget) (bool, error)

type posixSettlementBudget struct {
	probeDeadline         time.Time
	helperDeadline        time.Time
	settlementDeadline    time.Time
	helperShutdownReserve time.Duration
	joinReserve           time.Duration
}

type posixContainedChild struct {
	cmd            *exec.Cmd
	pid            int
	classifier     posixGroupClassifier
	waitObserved   chan struct{}
	waitObserveErr error
	reapStartOnce  sync.Once
	reapDone       chan struct{}
	reapResult     containedWaitResult
}

func newPlatformContainedChild(cmd *exec.Cmd) (containedChild, error) {
	child := &posixContainedChild{cmd: cmd, classifier: platformContainedGroupClassifier()}
	initializePlatformContainedWait(child)
	return child, nil
}

func openContainedNull() (containedInputFile, error) {
	return os.Open(os.DevNull)
}

func (c *posixContainedChild) start(
	cmd *exec.Cmd,
	stdin containedInputFile,
	stdout containedWriteFile,
	stderr containedWriteFile,
) error {
	if c == nil || cmd == nil || c.cmd != cmd {
		return fixedContainedError(ContainedStageStart, errors.New("invalid POSIX child"))
	}
	cmd.Stdin = stdin
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	prepareProcessGroup(cmd)
	SetParentDeathSignal(cmd)
	if err := cmd.Start(); err != nil {
		return fixedContainedError(ContainedStageStart, err)
	}
	if cmd.Process == nil || cmd.Process.Pid <= 0 {
		return fixedContainedError(ContainedStageStart, errors.New("start returned no process"))
	}
	c.pid = cmd.Process.Pid
	return nil
}

func (c *posixContainedChild) waitCommand() containedWaitResult {
	if c == nil || c.cmd == nil {
		return containedWaitResult{err: errors.New("invalid POSIX child")}
	}
	err := c.cmd.Wait()
	if c.cmd.ProcessState == nil {
		return containedWaitResult{err: err}
	}
	return containedWaitResult{
		exitCode: c.cmd.ProcessState.ExitCode(),
		exited:   c.cmd.ProcessState.Exited(),
		err:      err,
	}
}

func (c *posixContainedChild) terminate(timeoutMs uint32) error {
	return c.terminateBy(time.Now().Add(time.Duration(timeoutMs) * time.Millisecond))
}

func (c *posixContainedChild) terminateBy(deadline time.Time) error {
	if c == nil || c.pid <= 0 {
		return nil
	}
	if err := syscall.Kill(-c.pid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
		// The direct child is still unreaped, so its positive PID cannot be
		// recycled here. Kill that stable child identity to unblock the one
		// eventual reaper even though descendant cleanup is indeterminate.
		var directErr error
		if c.cmd != nil && c.cmd.Process != nil {
			directErr = c.cmd.Process.Kill()
		}
		startPlatformContainedLeaderReapAfterSignal(c)
		return errors.Join(fmt.Errorf("terminate contained process group: %w", err), directErr)
	}
	// The group signal MUST precede cmd.Wait. Once sent, reaping the direct
	// leader is safe: descendants keep the old group alive and prevent PGID
	// reuse; with no descendants, no later group signal is needed. Keeping the
	// zombie through settlement would make kill(-pgid, 0) report it alive until
	// the cleanup deadline.
	startPlatformContainedLeaderReapAfterSignal(c)
	if err := settlePOSIXGroup(deadline, c.pid, probePOSIXGroup, c.classifier); err != nil {
		return err
	}
	return reapPlatformContainedGroup(deadline, c.pid)
}

func probePOSIXGroup(pgid int) error {
	return syscall.Kill(-pgid, 0)
}

func newPOSIXSettlementBudget(now, deadline time.Time, classified bool) posixSettlementBudget {
	remaining := max(time.Duration(0), deadline.Sub(now))
	if !classified {
		return posixSettlementBudget{probeDeadline: deadline, settlementDeadline: deadline}
	}
	joinReserve := min(containedGroupPollInterval, remaining/4)
	settlementDeadline := deadline.Add(-joinReserve)
	probeDeadline := now.Add(settlementDeadline.Sub(now) / 2)
	helperWindow := settlementDeadline.Sub(probeDeadline)
	helperShutdownReserve := min(containedGroupPollInterval, max(time.Duration(0), helperWindow/4))
	helperDeadline := settlementDeadline.Add(-helperShutdownReserve)
	return posixSettlementBudget{
		probeDeadline:         probeDeadline,
		helperDeadline:        helperDeadline,
		settlementDeadline:    settlementDeadline,
		helperShutdownReserve: helperShutdownReserve,
		joinReserve:           joinReserve,
	}
}

func settlePOSIXGroup(deadline time.Time, pgid int, probe posixGroupProbe, classify posixGroupClassifier) error {
	budget := newPOSIXSettlementBudget(time.Now(), deadline, classify != nil)
	probeUntil := budget.probeDeadline
	if !time.Now().Before(probeUntil) {
		if classify != nil {
			return errLinuxGroupSettlementBudgetExhausted
		}
		return errors.Join(ErrCleanupTimeout, errPOSIXGroupLiveTimeout)
	}
	for {
		err := probe(pgid)
		if errors.Is(err, syscall.ESRCH) {
			return nil
		}
		if err != nil && !errors.Is(err, syscall.EPERM) {
			return fmt.Errorf("POSIX_GROUP_PROBE_FAILED: %w", err)
		}
		if !time.Now().Before(probeUntil) {
			break
		}
		if !sleepPOSIXUntil(probeUntil) {
			break
		}
	}

	if classify == nil {
		return errors.Join(ErrCleanupTimeout, errPOSIXGroupLiveTimeout)
	}
	if !time.Now().Before(budget.settlementDeadline) {
		return errLinuxGroupSettlementBudgetExhausted
	}
	ctx, cancel := context.WithDeadline(context.Background(), budget.settlementDeadline)
	defer cancel()
	settled, err := classify(ctx, pgid, budget)
	if err != nil {
		if err == errLinuxGroupSettlementBudgetExhausted {
			return errLinuxGroupSettlementBudgetExhausted
		}
		return fmt.Errorf("%w: %w", errLinuxGroupSettlementIndeterminate, err)
	}
	if settled {
		return nil
	}
	return errors.Join(ErrCleanupTimeout, errPOSIXGroupLiveTimeout)
}

func sleepPOSIXUntil(deadline time.Time) bool {
	remaining := time.Until(deadline)
	if remaining <= 0 {
		return false
	}
	timer := time.NewTimer(min(containedGroupPollInterval, remaining))
	defer timer.Stop()
	<-timer.C
	return time.Now().Before(deadline)
}

func (c *posixContainedChild) close() error {
	return nil
}
