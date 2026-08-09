//go:build linux || darwin || dragonfly || freebsd || netbsd || openbsd

package process

import (
	"errors"
	"os/exec"
)

// waitStrictContainedCommand observes the direct leader's exit without
// reaping it. The zombie leader pins its PID/PGID while the remaining process
// group is terminated; only then may Command.Wait release the numeric identity.
func waitStrictContainedCommand(cmd *exec.Cmd) error {
	return waitStrictContainedCommandWith(cmd, observePlatformContainedExit, killProcessGroup, cmd.Wait)
}

func waitStrictContainedCommandWith(
	cmd *exec.Cmd,
	observe func(int) error,
	signal func(*exec.Cmd),
	reap func() error,
) error {
	if cmd == nil || cmd.Process == nil || cmd.Process.Pid <= 0 || observe == nil || signal == nil || reap == nil {
		return errors.New("invalid strict contained POSIX wait")
	}
	observeErr := observe(cmd.Process.Pid)
	// Even when observation fails, this owner has not reaped the leader, so its
	// numeric group identity is still safe to signal before the mandatory reap.
	signal(cmd)
	return errors.Join(observeErr, reap())
}
