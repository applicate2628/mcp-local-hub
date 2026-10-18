//go:build windows

package process

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"sync/atomic"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

type windowsContainedChild struct {
	attributeClose                    ProcThreadAttributeListCloseObservation
	cmd                               *exec.Cmd
	job                               *Job
	process                           windows.Handle
	thread                            windows.Handle
	original                          containedJobOriginal
	memberSource                      *originalMemberSource
	rootWaitEntered, rootWaitReturned bool
	observed                          bool
	creationTime                      string
	creationFILETIME                  uint64
	creationError                     error
	threadCloseAttempted              bool
	threadCloseError                  error
	processCloseAttempted             bool
	processCloseError                 error
	jobCloseAttempted                 bool
	jobCloseError                     error
	wrapperOwned                      bool
	wrapperCloseAttempted             bool
	wrapperCloseError                 error
	waitStarted                       atomic.Bool
	waitFinished                      atomic.Bool
	inheritedCloseError               error
	inheritedCloseAttempts            int
	inheritedHandles                  []originalInheritedHandle
	attributeOwner                    *procThreadAttributeListObserved
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
) (retErr error) {
	if c == nil || c.job == nil || c.job.Handle() == 0 || cmd == nil || c.cmd != cmd {
		return fixedContainedError(ContainedStageContainment, errors.New("invalid Windows containment"))
	}
	if cmd.Path == "" {
		return fixedContainedError(ContainedStageStart, errors.New("empty command path"))
	}
	// The Job is fresh and dedicated to this launch. Bind the original process
	// handle to that empty Job before any child could have entered it.
	before, err := c.job.queryAccounting()
	if err != nil {
		return fixedContainedError(ContainedStageContainment, fmt.Errorf("prelaunch Job accounting: %w", err))
	}
	if before.TotalProcesses != 0 || before.ActiveProcesses != 0 {
		return fixedContainedError(ContainedStageContainment, errors.New("contained Job was not empty before launch"))
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

	attrList, err := newProcThreadAttributeListObserved(2)
	if attrList != nil {
		defer func() {
			var closeErr error
			c.attributeClose, closeErr = attrList.CloseObserved()
			retErr = errors.Join(retErr, closeErr)
		}()
	}
	if err != nil {
		return fixedContainedError(ContainedStageContainment, err)
	}

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
		ProcThreadAttributeList: (*windows.ProcThreadAttributeList)(attrList.List()),
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
	if c.observed && pi.Process != 0 {
		c.original = containedJobOriginal{job: c.job, handle: pi.Process, pid: pi.ProcessId, atCreate: true}
		var created, exited, kernel, user windows.Filetime
		c.creationError = windows.GetProcessTimes(pi.Process, &created, &exited, &kernel, &user)
		if c.creationError == nil {
			if created.HighDateTime == 0 && created.LowDateTime == 0 {
				c.creationError = errors.New("original creation time unavailable")
			} else {
				c.creationTime = time.Unix(0, created.Nanoseconds()).UTC().Format(time.RFC3339Nano)
				c.creationFILETIME = uint64(created.HighDateTime)<<32 | uint64(created.LowDateTime)
			}
		}
	}
	if pi.Thread != 0 {
		if c.observed {
			c.threadCloseAttempted = true
		}
		err := windows.CloseHandle(pi.Thread)
		if c.observed {
			c.threadCloseError = err
		}
		if err == nil {
			c.thread = 0
		}
	}
	if pi.Process == 0 {
		return fixedContainedError(ContainedStageStart, errors.New("CreateProcess returned no process handle"))
	}
	c.original = containedJobOriginal{job: c.job, handle: pi.Process, pid: pi.ProcessId, atCreate: true}
	return nil
}

func containedWindowsCreationFlags() uint32 {
	return uint32(windows.EXTENDED_STARTUPINFO_PRESENT) |
		uint32(windows.CREATE_UNICODE_ENVIRONMENT) |
		uint32(windows.CREATE_NO_WINDOW)
}

func (c *windowsContainedChild) wait() containedWaitResult {
	if c != nil && c.memberSource != nil {
		g := c.memberSource.gate
		g.mu.Lock()
		if c.rootWaitEntered {
			g.mu.Unlock()
			return containedWaitResult{err: errors.New("original root waiter already entered")}
		}
		c.rootWaitEntered = true
		g.mu.Unlock()
		defer func() { g.mu.Lock(); c.rootWaitReturned = true; g.mu.Unlock() }()
	}
	if c == nil || c.process == 0 {
		return containedWaitResult{err: errors.New("invalid Windows process handle")}
	}
	if c.observed {
		c.waitStarted.Store(true)
	}
	event, err := windows.WaitForSingleObject(c.process, windows.INFINITE)
	if err != nil {
		return containedWaitResult{err: err}
	}
	if event != uint32(windows.WAIT_OBJECT_0) {
		return containedWaitResult{err: errors.New("unexpected process wait result")}
	}
	if c.observed {
		c.waitFinished.Store(true)
	}
	var code uint32
	if err := windows.GetExitCodeProcess(c.process, &code); err != nil {
		return containedWaitResult{err: err}
	}
	return containedWaitResult{exitCode: int(code), exited: true}
}

func (c *windowsContainedChild) terminate(timeoutMs uint32) error {
	if c == nil || c.job == nil {
		return &JobTerminationProofError{Stage: "invalid_job", Cause: errors.New("missing contained Job")}
	}
	return c.job.terminateForContained(timeoutMs, c.original)
}

func (c *windowsContainedChild) close() error {
	if c != nil && c.memberSource != nil {
		permit, err := c.memberSource.gate.beginTerminal(nil)
		if err != nil {
			return err
		}
		defer permit.finish()
		return c.closeOriginalWithPermit(permit)
	}
	return c.closeOriginalWithPermit(nil)
}
func (c *windowsContainedChild) closeOriginalWithPermit(permit *originalMemberTerminalPermit) error {
	if err := c.originalMemberCloseReady(permit); err != nil {
		return err
	}
	if c == nil {
		return nil
	}
	var out error
	if c.observed {
		waitPending := c.waitStarted.Load() && !c.waitFinished.Load()
		if waitPending {
			out = errors.Join(out, errors.New("original root wait pending"))
		}
		if !waitPending && c.wrapperOwned && c.cmd != nil && c.cmd.Process != nil && !c.wrapperCloseAttempted {
			c.wrapperCloseAttempted = true
			c.wrapperCloseError = c.cmd.Process.Release()
		}
		_ = c.closeContainedJobBackstopWithPermit(permit)
		if !waitPending && c.process != 0 && !c.processCloseAttempted {
			c.processCloseAttempted = true
			c.processCloseError = windows.CloseHandle(c.process)
			if c.processCloseError == nil {
				c.process = 0
			}
		}
		if c.thread != 0 && !c.threadCloseAttempted {
			c.threadCloseAttempted = true
			c.threadCloseError = windows.CloseHandle(c.thread)
			if c.threadCloseError == nil {
				c.thread = 0
			}
		}
		return errors.Join(out, c.jobCloseError, c.processCloseError, c.threadCloseError, c.wrapperCloseError, c.inheritedSettlementError(), c.attributeClose.Error)
	}
	if c.job != nil {
		out = errors.Join(out, c.job.closeWithMemberPermit(permit))
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
	return errors.Join(out, c.attributeClose.Error)
}

func (c *windowsContainedChild) prepareContainedObservation() { c.observed = true }
func (c *windowsContainedChild) containedCreated() bool {
	return c.original.atCreate && c.original.handle != 0 && c.original.handle != windows.InvalidHandle && c.original.pid != 0
}
func (c *windowsContainedChild) containedStartLive(ctx context.Context) (bool, error) {
	if c != nil && c.memberSource != nil {
		exit, err := c.memberSource.gate.enterQuery(nil)
		if err != nil {
			return false, err
		}
		defer exit()
	}
	if !c.attributeClose.Complete() {
		return false, errors.Join(c.attributeClose.Error, errors.New("original attribute allocation closure unavailable"))
	}
	if err := creatorClosureContext(ctx); err != nil {
		return false, err
	}
	if !c.containedCreated() {
		return false, ErrContainmentUnavailable
	}
	if _, err := c.containedRootIdentity(); err != nil {
		return false, err
	}
	member, err := jobMemberFromHandle(c.original.handle, c.job)
	if err != nil || !member {
		return false, errors.Join(err, errors.New("original child Job membership unavailable"))
	}
	status, err := windows.WaitForSingleObject(c.original.handle, 0)
	if err != nil {
		return false, err
	}
	if status == uint32(windows.WAIT_TIMEOUT) {
		return true, nil
	}
	if status == uint32(windows.WAIT_OBJECT_0) {
		return false, nil
	}
	return false, errors.New("unexpected original live wait result")
}
func (c *windowsContainedChild) closeContainedJobBackstop() error {
	if c != nil && c.memberSource != nil {
		permit, err := c.memberSource.gate.beginTerminal(nil)
		if err != nil {
			return err
		}
		defer permit.finish()
		return c.closeContainedJobBackstopWithPermit(permit)
	}
	return c.closeContainedJobBackstopWithPermit(nil)
}
func (c *windowsContainedChild) closeContainedJobBackstopWithPermit(permit *originalMemberTerminalPermit) error {
	if c != nil && c.memberSource != nil {
		if err := permit.validate(c.memberSource.gate); err != nil {
			return err
		}
	}
	if c.job != nil && !c.jobCloseAttempted {
		c.jobCloseAttempted = true
		c.jobCloseError = c.job.closeWithMemberPermit(permit)
	}
	return c.jobCloseError
}
func (c *windowsContainedChild) containedRootIdentity() (ContainedRootIdentity, error) {
	v := ContainedRootIdentity{Known: c.original.atCreate && c.original.pid != 0 && c.creationTime != "" && c.creationError == nil, PID: int(c.original.pid), StartedAt: c.creationTime, NativeBirthFILETIME: c.creationFILETIME}
	if !v.Known {
		return v, errors.Join(c.creationError, errors.New("contained original identity unavailable"))
	}
	return v, nil
}
func (c *windowsContainedChild) settleContained(ctx context.Context, natural bool) (ContainedNativeSnapshot, error) {
	if c != nil && c.memberSource != nil {
		permit, owned, err := c.job.enterMemberTerminal(ctx)
		if err != nil {
			return ContainedNativeSnapshot{}, err
		}
		if owned {
			defer permit.finish()
		}
		ctx = withOriginalMemberTerminalPermit(ctx, permit)
	}
	var v ContainedNativeSnapshot
	if c != nil {
		v.RootIdentity, _ = c.containedRootIdentity()
	}
	if err := creatorClosureContext(ctx); err != nil {
		return v, err
	}
	if c == nil || c.job == nil || c.job.Handle() == 0 {
		return v, ErrContainmentUnavailable
	}
	if c.process == 0 {
		if c.memberSource != nil {
			permit, err := originalMemberPermitFromContext(ctx, c.memberSource.gate)
			if err != nil {
				return v, err
			}
			if err := permit.markNativeEntry(ctx); err != nil {
				return v, err
			}
		}
		accounting, err := c.job.queryAccounting()
		v.AccountingKnown = err == nil
		v.InitialTotal = accounting.TotalProcesses
		v.FinalActive = accounting.ActiveProcesses
		v.NoChildKnown = err == nil && accounting.TotalProcesses == 0 && accounting.ActiveProcesses == 0
		if !v.NoChildKnown {
			return v, errors.Join(err, errors.New("contained no-child release unproved"))
		}
		v.TerminalAction = string(creatorTerminalNotRequested)
		return v, nil
	}
	original := c.original
	original.required = true
	var observation jobTerminationObservation
	var err error
	if natural {
		observation, err = c.job.completeCreatorAfterExit(ctx, original)
	} else {
		deadline, _ := ctx.Deadline()
		ms := time.Until(deadline).Milliseconds()
		if ms <= 0 {
			return v, context.DeadlineExceeded
		}
		if ms > int64(^uint32(0)) {
			ms = int64(^uint32(0))
		}
		observation, err = c.job.observeCurrentJob(uint32(ms), original, ctx, true)
	}
	v.TerminalAction = string(observation.terminalAction)
	v.MembersExited = observation.ObservedCurrentMembersExited
	v.AccountingKnown = observation.native.InitialAccountingKnown && observation.native.FinalAccountingKnown
	v.InitialTotal = observation.native.InitialTotal
	v.FinalActive = observation.native.FinalActive
	v.CapturedMemberPIDs = append([]uint32(nil), observation.native.CapturedMemberPIDs...)
	v.Error = errors.Join(err, observation.terminalActionErr, c.creationError, c.attributeClose.Error)
	return v, v.Error
}
