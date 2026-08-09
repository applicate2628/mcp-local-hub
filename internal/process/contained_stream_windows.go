//go:build windows

package process

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"unsafe"

	"golang.org/x/sys/windows"
)

type windowsContainedChild struct {
	cmd     *exec.Cmd
	job     *Job
	process windows.Handle
	thread  windows.Handle
}

func newPlatformContainedChild(cmd *exec.Cmd) (containedChild, error) {
	job, err := NewKillOnCloseJob()
	if err != nil {
		return nil, err
	}
	return &windowsContainedChild{cmd: cmd, job: job}, nil
}

func openContainedNull() (containedInputFile, error) {
	name, err := windows.UTF16PtrFromString("NUL")
	if err != nil {
		return nil, err
	}
	sa := &windows.SecurityAttributes{
		Length:        uint32(unsafe.Sizeof(windows.SecurityAttributes{})),
		InheritHandle: 1,
	}
	handle, err := windows.CreateFile(
		name,
		windows.GENERIC_READ,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		sa,
		windows.OPEN_EXISTING,
		windows.FILE_ATTRIBUTE_NORMAL,
		0,
	)
	if err != nil {
		return nil, fmt.Errorf("open null stdin: %w", err)
	}
	return os.NewFile(uintptr(handle), os.DevNull), nil
}

func (c *windowsContainedChild) start(
	cmd *exec.Cmd,
	stdin containedInputFile,
	stdout containedWriteFile,
	stderr containedWriteFile,
) error {
	if c == nil || c.job == nil || c.job.Handle() == 0 || cmd == nil || c.cmd != cmd {
		return fixedContainedError(ContainedStageContainment, errors.New("invalid Windows containment"))
	}
	if cmd.Path == "" {
		return fixedContainedError(ContainedStageStart, errors.New("empty command path"))
	}
	if len(cmd.Args) == 0 {
		cmd.Args = []string{cmd.Path}
	}
	cmd.Stdin = stdin
	cmd.Stdout = stdout
	cmd.Stderr = stderr

	stdHandles := []windows.Handle{
		windows.Handle(stdin.Fd()),
		windows.Handle(stdout.Fd()),
		windows.Handle(stderr.Fd()),
	}
	for _, handle := range stdHandles {
		if handle == 0 || handle == windows.InvalidHandle {
			return fixedContainedError(ContainedStagePipeSetup, errors.New("invalid standard handle"))
		}
		if err := windows.SetHandleInformation(
			handle,
			windows.HANDLE_FLAG_INHERIT,
			windows.HANDLE_FLAG_INHERIT,
		); err != nil {
			return fixedContainedError(ContainedStagePipeSetup, err)
		}
	}

	attrList, err := windows.NewProcThreadAttributeList(2)
	if err != nil {
		return fixedContainedError(ContainedStageContainment, err)
	}
	defer attrList.Delete()

	jobHandle := c.job.Handle()
	if err := attrList.Update(
		procThreadAttributeJobList,
		unsafe.Pointer(&jobHandle),
		unsafe.Sizeof(jobHandle),
	); err != nil {
		return fixedContainedError(ContainedStageContainment, err)
	}
	if err := attrList.Update(
		windows.PROC_THREAD_ATTRIBUTE_HANDLE_LIST,
		unsafe.Pointer(&stdHandles[0]),
		uintptr(len(stdHandles))*unsafe.Sizeof(stdHandles[0]),
	); err != nil {
		return fixedContainedError(ContainedStageContainment, err)
	}

	commandLine, cwd, env, err := prepareWindowsCommand(cmd)
	if err != nil {
		return fixedContainedError(ContainedStageStart, err)
	}
	si := &windows.StartupInfoEx{
		StartupInfo: windows.StartupInfo{
			Cb:        uint32(unsafe.Sizeof(windows.StartupInfoEx{})),
			Flags:     windows.STARTF_USESTDHANDLES,
			StdInput:  stdHandles[0],
			StdOutput: stdHandles[1],
			StdErr:    stdHandles[2],
		},
		ProcThreadAttributeList: attrList.List(),
	}

	var pi windows.ProcessInformation
	creationFlags := containedWindowsCreationFlags()
	if err := windows.CreateProcess(
		nil,
		commandLine,
		nil,
		nil,
		true,
		creationFlags,
		env,
		cwd,
		&si.StartupInfo,
		&pi,
	); err != nil {
		return fixedContainedError(ContainedStageStart, err)
	}
	c.process = pi.Process
	c.thread = pi.Thread
	if pi.Thread != 0 {
		if err := windows.CloseHandle(pi.Thread); err == nil {
			c.thread = 0
		}
	}
	if pi.Process == 0 {
		return fixedContainedError(ContainedStageStart, errors.New("CreateProcess returned no process handle"))
	}
	return nil
}

func containedWindowsCreationFlags() uint32 {
	return uint32(windows.EXTENDED_STARTUPINFO_PRESENT) |
		uint32(windows.CREATE_UNICODE_ENVIRONMENT) |
		uint32(windows.CREATE_NO_WINDOW)
}

func (c *windowsContainedChild) wait() containedWaitResult {
	if c == nil || c.process == 0 {
		return containedWaitResult{err: errors.New("invalid Windows process handle")}
	}
	event, err := windows.WaitForSingleObject(c.process, windows.INFINITE)
	if err != nil {
		return containedWaitResult{err: err}
	}
	if event != uint32(windows.WAIT_OBJECT_0) {
		return containedWaitResult{err: errors.New("unexpected process wait result")}
	}
	var code uint32
	if err := windows.GetExitCodeProcess(c.process, &code); err != nil {
		return containedWaitResult{err: err}
	}
	return containedWaitResult{exitCode: int(code), exited: true}
}

func (c *windowsContainedChild) terminate(timeoutMs uint32) error {
	if c == nil || c.job == nil {
		return nil
	}
	return c.job.TerminateAll(timeoutMs)
}

func (c *windowsContainedChild) close() error {
	if c == nil {
		return nil
	}
	var out error
	if c.job != nil {
		out = errors.Join(out, c.job.Close())
	}
	if c.process != 0 {
		handle := c.process
		c.process = 0
		out = errors.Join(out, windows.CloseHandle(handle))
	}
	if c.thread != 0 {
		handle := c.thread
		c.thread = 0
		out = errors.Join(out, windows.CloseHandle(handle))
	}
	return out
}
