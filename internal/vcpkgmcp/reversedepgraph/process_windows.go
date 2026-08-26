//go:build windows

package reversedepgraph

import (
	"os/exec"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

type platformProcess struct {
	command *exec.Cmd
	job     windows.Handle
}

func startPlatformProcess(command *exec.Cmd) (*platformProcess, error) {
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return nil, err
	}
	information := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{}
	information.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	if _, err = windows.SetInformationJobObject(
		job,
		windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&information)),
		uint32(unsafe.Sizeof(information)),
	); err != nil {
		_ = windows.CloseHandle(job)
		return nil, err
	}
	command.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.CREATE_NEW_PROCESS_GROUP}
	if err = command.Start(); err != nil {
		_ = windows.CloseHandle(job)
		return nil, err
	}
	processHandle, openErr := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, uint32(command.Process.Pid))
	if openErr != nil {
		_ = command.Process.Kill()
		_ = command.Wait()
		_ = windows.CloseHandle(job)
		return nil, openErr
	}
	defer windows.CloseHandle(processHandle)
	if err = windows.AssignProcessToJobObject(job, processHandle); err != nil {
		_ = command.Process.Kill()
		_ = command.Wait()
		_ = windows.CloseHandle(job)
		return nil, err
	}
	return &platformProcess{command: command, job: job}, nil
}

func terminatePlatformProcess(process *platformProcess) error {
	if process == nil {
		return nil
	}
	if err := windows.TerminateJobObject(process.job, 1); err != nil {
		_ = process.command.Process.Kill()
		return err
	}
	return nil
}

func closePlatformProcess(process *platformProcess) {
	if process != nil && process.job != 0 {
		_ = windows.CloseHandle(process.job)
		process.job = 0
	}
}
