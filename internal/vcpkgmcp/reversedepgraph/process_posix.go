//go:build !windows

package reversedepgraph

import (
	"os/exec"
	"syscall"
)

type platformProcess struct{ command *exec.Cmd }

func startPlatformProcess(command *exec.Cmd) (*platformProcess, error) {
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := command.Start(); err != nil {
		return nil, err
	}
	return &platformProcess{command: command}, nil
}

func terminatePlatformProcess(process *platformProcess) error {
	if process == nil || process.command.Process == nil {
		return nil
	}
	if err := syscall.Kill(-process.command.Process.Pid, syscall.SIGKILL); err != nil {
		_ = process.command.Process.Kill()
		return err
	}
	return nil
}

func closePlatformProcess(*platformProcess) {}
