//go:build !windows

package process

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"syscall"
	"time"
)

const containedGroupPollInterval = 10 * time.Millisecond

type posixContainedChild struct {
	cmd *exec.Cmd
	pid int
}

func newPlatformContainedChild(cmd *exec.Cmd) (containedChild, error) {
	return &posixContainedChild{cmd: cmd}, nil
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

func (c *posixContainedChild) wait() containedWaitResult {
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
	if c == nil || c.pid <= 0 {
		return nil
	}
	if err := syscall.Kill(-c.pid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
		return fmt.Errorf("terminate contained process group: %w", err)
	}

	deadline := time.Now().Add(time.Duration(timeoutMs) * time.Millisecond)
	for {
		err := syscall.Kill(-c.pid, 0)
		if errors.Is(err, syscall.ESRCH) {
			return nil
		}
		if err != nil && !errors.Is(err, syscall.EPERM) {
			return fmt.Errorf("probe contained process group: %w", err)
		}
		if !time.Now().Before(deadline) {
			return errors.Join(ErrCleanupTimeout, errors.New("contained process group did not settle"))
		}
		time.Sleep(containedGroupPollInterval)
	}
}

func (c *posixContainedChild) close() error {
	return nil
}
