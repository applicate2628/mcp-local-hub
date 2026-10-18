//go:build windows

package process

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"sort"
	"strings"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// procThreadAttributeJobList is the PROC_THREAD_ATTRIBUTE_JOB_LIST
// constant from winnt.h, computed per ProcThreadAttributeValue macro
// as ProcThreadAttributeValue(PROC_THREAD_ATTRIBUTE_JOB_LIST=13,
// Thread=FALSE, Input=TRUE, Additive=FALSE) = 0x0002000D.
//
// Not exported by golang.org/x/sys/windows v0.43.0 (the package ships
// PROC_THREAD_ATTRIBUTE_HANDLE_LIST, _PARENT_PROCESS, _GROUP_AFFINITY,
// _PREFERRED_NODE, _IDEAL_PROCESSOR, _MITIGATION_POLICY, _UMS_THREAD,
// _PROTECTION_LEVEL, _PSEUDOCONSOLE — but not _JOB_LIST), so defined
// locally per the v0.5.0 supervisor spec Q2 v7 closure.
const procThreadAttributeJobList uintptr = 0x0002000D

// StartWithJob spawns cmd as a process associated with job at create
// time via STARTUPINFOEX.lpAttributeList + PROC_THREAD_ATTRIBUTE_JOB_LIST.
//
// This closes the v0.4.x Start-then-Assign race documented at
// internal/process/jobobject_windows.go:65-71 — the existing
// Job.Assign(cmd *exec.Cmd) helper runs AFTER cmd.Start(), so there is
// a brief window where the child exists outside the Job and any
// grandchild it spawns escapes the Job's cleanup contract.
// StartWithJob removes that window by passing the Job handle to the
// kernel's CreateProcess as part of the new process's thread-attribute
// list; the assignment is atomic with the create.
//
// The existing Job.Assign post-Start method is preserved for
// non-supervisor callers (mcphub gui, ad-hoc tests, etc.) and remains
// the right tool when callers cannot route through this primitive.
//
// Returns the child PID on success. The caller is responsible for
// reaping the child via cmd.Wait() / cmd.Process.Wait() — this helper
// only fills in cmd.Process; it does not start a wait goroutine.
//
// Side effects on the passed-in cmd:
//   - cmd.Process is populated with the spawned process.
//   - cmd.Args / cmd.Env / cmd.Dir are honored when building the
//     command line (Args), environment block (Env), and working dir
//     (Dir); SysProcAttr is NOT honored — supervisor v0.5.0 daemons
//     do not need creation-flag overrides beyond what this helper
//     already sets.
func StartWithJob(job *Job, cmd *exec.Cmd) (int, error) {
	return startWithJobFiles(job, cmd, nil, nil, nil)
}

// StartWithJobFiles starts cmd atomically inside job while forwarding the
// caller-owned standard files. The caller retains ownership of the files and
// must keep them open until cmd.Wait returns. All three files are required so
// the child never receives a partially-defined STARTF_USESTDHANDLES contract.
func StartWithJobFiles(job *Job, cmd *exec.Cmd, stdin, stdout, stderr *os.File) (int, error) {
	return startWithJobFiles(job, cmd, stdin, stdout, stderr)
}

// startWithJobFiles is the sole Windows at-create containment owner. The
// exported legacy path deliberately supplies no standard handles; the strict
// runner supplies all three and receives an allowlisted child handle set.
func startWithJobFiles(job *Job, cmd *exec.Cmd, stdin, stdout, stderr *os.File) (int, error) {
	return startWithJobFilesObserved(job, cmd, stdin, stdout, stderr, nil)
}

// startWithJobFilesObserved extends only the at-create owner; nil options
// follow the ordinary launch path without retaining any additional handle.
type startWithJobObserverOptions struct {
	retainCreated             func(windows.ProcessInformation)
	retainOriginal            func(windows.Handle, uint32) error
	findProcess               func(int) (*os.Process, error)
	childHandles              []windows.Handle
	childHandleClosed         func(windows.Handle, error)
	childHandleAcquired       func(windows.Handle)
	originalChildHandleClosed func(windows.Handle, originalInheritedCloseOutcome, error)
	attributeListOwner        func(*procThreadAttributeListObserved)
	duplicateHandle           func(*os.File) (windows.Handle, error)
	closeChildHandle          func(windows.Handle) (originalInheritedCloseOutcome, error)
	newAttributeList          func(uint32) (*procThreadAttributeListObserved, error)
	attributeListClosed       func(ProcThreadAttributeListCloseObservation)
}

func startWithJobFilesObserved(job *Job, cmd *exec.Cmd, stdin, stdout, stderr *os.File, observer *startWithJobObserverOptions) (pid int, retErr error) {
	if job == nil {
		return 0, startWithJobError(StartWithJobContainment, errors.New("StartWithJob: nil job"))
	}
	if cmd == nil {
		return 0, startWithJobError(StartWithJobInvalid, errors.New("StartWithJob: nil cmd"))
	}
	if cmd.Path == "" {
		return 0, startWithJobError(StartWithJobInvalid, errors.New("StartWithJob: cmd.Path is empty (use exec.Command or set Path explicitly)"))
	}
	if (stdin == nil) != (stdout == nil) || (stdin == nil) != (stderr == nil) {
		return 0, startWithJobError(StartWithJobInvalid, errors.New("StartWithJob: standard files must be all nil or all present"))
	}
	// Normalize empty cmd.Args — os/exec.Cmd lets callers construct a
	// Cmd directly with only Path set (Args left nil), in which case the
	// argv[0]-derivation below (`cmd.Args[1:]`) would panic on nil-slice
	// indexing. exec.Command() itself initializes Args to []string{name}
	// internally for the same reason; we match that contract here.
	if len(cmd.Args) == 0 {
		cmd.Args = []string{cmd.Path}
	}
	jobHandle := job.Handle()
	if jobHandle == 0 {
		return 0, startWithJobError(StartWithJobContainment, errors.New("StartWithJob: job handle is 0 (closed?)"))
	}

	// Allocate a one-attribute thread-attribute list via x/sys helper.
	// The container owns the LocalAlloc'd buffer; Delete frees it.
	attributeCount := 1
	if stdin != nil || (observer != nil && len(observer.childHandles) != 0) {
		attributeCount++
	}
	newAttributeList := newProcThreadAttributeListObserved
	if observer != nil && observer.newAttributeList != nil {
		newAttributeList = observer.newAttributeList
	}
	attrList, err := newAttributeList(uint32(attributeCount))
	if attrList != nil {
		if observer != nil && observer.attributeListOwner != nil {
			observer.attributeListOwner(attrList)
		}
		defer func() {
			view, closeErr := attrList.CloseObserved()
			if observer != nil && observer.attributeListClosed != nil {
				observer.attributeListClosed(view)
			}
			if closeErr != nil {
				if pid != 0 {
					closeErr = fmt.Errorf("%w: attribute allocation close: %w", ErrSpawnPostCreate, closeErr)
				}
				retErr = errors.Join(retErr, startWithJobError(StartWithJobContainment, closeErr))
			}
		}()
	}
	if err != nil {
		return 0, startWithJobError(StartWithJobContainment, fmt.Errorf("NewProcThreadAttributeList: %w", err))
	}

	// Pass &jobHandle (a *windows.Handle) as the attribute value.
	// The kernel reads sizeof(HANDLE) bytes; the container's Update
	// method also retains the pointer in al.pointers to prevent GC
	// from moving the backing storage until Delete fires.
	if err := attrList.Update(
		procThreadAttributeJobList,
		unsafe.Pointer(&jobHandle),
		unsafe.Sizeof(jobHandle),
	); err != nil {
		return 0, startWithJobError(StartWithJobContainment, fmt.Errorf("ProcThreadAttributeList.Update(JOB_LIST): %w", err))
	}

	var childHandles []windows.Handle
	duplicateHandle := duplicateInheritableHandle
	if observer != nil && observer.duplicateHandle != nil {
		duplicateHandle = observer.duplicateHandle
	}
	closeChildHandle := func(handle windows.Handle) error {
		var outcome originalInheritedCloseOutcome
		var reported error
		if observer != nil && observer.closeChildHandle != nil {
			outcome, reported = observer.closeChildHandle(handle)
		} else {
			outcome.NativeError = windows.CloseHandle(handle)
			outcome.ClosedKnown = outcome.NativeError == nil
			reported = outcome.NativeError
		}
		if observer != nil && observer.childHandleClosed != nil {
			observer.childHandleClosed(handle, errors.Join(outcome.NativeError, reported))
		}
		if observer != nil && observer.originalChildHandleClosed != nil {
			observer.originalChildHandleClosed(handle, outcome, reported)
		}
		return errors.Join(outcome.NativeError, reported)
	}
	if stdin != nil {
		for _, file := range []*os.File{stdin, stdout, stderr} {
			duplicate, duplicateErr := duplicateHandle(file)
			if duplicateErr != nil {
				for _, handle := range childHandles {
					_ = closeChildHandle(handle)

				}
				return 0, startWithJobError(StartWithJobLaunch, duplicateErr)
			}
			childHandles = append(childHandles, duplicate)
			if observer != nil && observer.childHandleAcquired != nil {
				observer.childHandleAcquired(duplicate)
			}
		}
		defer func() {
			for _, handle := range childHandles {
				_ = closeChildHandle(handle)

			}
		}()
	}
	allowedHandles := childHandles
	if observer != nil && len(observer.childHandles) != 0 {
		allowedHandles = append(append([]windows.Handle(nil), childHandles...), observer.childHandles...)
	}
	if len(allowedHandles) != 0 {
		if err := attrList.Update(
			windows.PROC_THREAD_ATTRIBUTE_HANDLE_LIST,
			unsafe.Pointer(&allowedHandles[0]),
			uintptr(len(allowedHandles))*unsafe.Sizeof(allowedHandles[0]),
		); err != nil {
			return 0, startWithJobError(StartWithJobLaunch, fmt.Errorf("ProcThreadAttributeList.Update(HANDLE_LIST): %w", err))
		}
	}

	// Build STARTUPINFOEX. Cb must be sizeof(StartupInfoEx) — the
	// kernel uses this to decide whether to read the extended fields.
	si := &windows.StartupInfoEx{
		StartupInfo: windows.StartupInfo{
			Cb: uint32(unsafe.Sizeof(windows.StartupInfoEx{})),
		},
		ProcThreadAttributeList: (*windows.ProcThreadAttributeList)(attrList.List()),
	}
	if stdin != nil {
		si.StartupInfo.Flags |= windows.STARTF_USESTDHANDLES
		si.StartupInfo.StdInput = childHandles[0]
		si.StartupInfo.StdOutput = childHandles[1]
		si.StartupInfo.StdErr = childHandles[2]
	}

	commandLine, cwdPtr, envPtr, err := prepareWindowsCommand(cmd)
	if err != nil {
		return 0, startWithJobError(StartWithJobInvalid, err)
	}

	// Always-required creation flags:
	//   - EXTENDED_STARTUPINFO_PRESENT: tells the kernel to read
	//     STARTUPINFOEX (otherwise the attribute list is ignored).
	//   - CREATE_UNICODE_ENVIRONMENT: when cmd.Env is non-nil we pass
	//     a UTF-16 environment block; the kernel must be told it is
	//     wide-char rather than ANSI. Setting this flag is harmless
	//     when envPtr is NULL (the kernel inherits the parent's env
	//     in both cases).
	creationFlags := startWithJobCreationFlags()

	var pi windows.ProcessInformation
	// lpStartupInfo: take &si.StartupInfo (the embedded first field of StartupInfoEx).
	// Windows reads this as a STARTUPINFOEX because (a) si.Cb == sizeof(StartupInfoEx),
	// (b) creationFlags has EXTENDED_STARTUPINFO_PRESENT, and (c) the embedded layout
	// places ProcThreadAttributeList immediately after the StartupInfo bytes with no
	// padding. Verified against windows.StartupInfoEx struct definition in
	// golang.org/x/sys/windows.
	if err := windows.CreateProcess(
		nil,                      // lpApplicationName (NULL → use cmd line argv[0])
		commandLine,              // lpCommandLine
		nil,                      // lpProcessAttributes
		nil,                      // lpThreadAttributes
		len(allowedHandles) != 0, // bInheritHandles; HANDLE_LIST restricts inheritance
		creationFlags,            // dwCreationFlags
		envPtr,                   // lpEnvironment
		cwdPtr,                   // lpCurrentDirectory
		&si.StartupInfo,          // lpStartupInfo (see comment above)
		&pi,                      // lpProcessInformation
	); err != nil {
		return 0, startWithJobError(StartWithJobLaunch, fmt.Errorf("CreateProcess: %w", err))
	}

	retainedOriginal := false
	if observer != nil && observer.retainCreated != nil {
		// The provisional lifetime owner adopts both creation handles before
		// either can be closed, including post-create wrapper failures.
		observer.retainCreated(pi)
		retainedOriginal = true
	} else {
		// Ordinary/CST callers retain their established thread ownership.
		_ = windows.CloseHandle(pi.Thread)
	}
	if observer != nil && observer.retainOriginal != nil {
		if retainErr := observer.retainOriginal(pi.Process, pi.ProcessId); retainErr != nil {
			_ = windows.CloseHandle(pi.Process)
			return int(pi.ProcessId), startWithJobError(StartWithJobLaunch, fmt.Errorf("%w: retain original process handle: %v", ErrSpawnPostCreate, retainErr))
		}
		retainedOriginal = true
	}

	// Populate cmd.Process so callers can use Wait()/Kill() through
	// the standard os/exec surface. NOTE: this constructs an
	// *os.Process from the PID while the original creation handle is held.
	// os.FindProcess opens its own handle; an observing lifetime owner retains
	// the creator handle separately. Caller workflows that
	// already use exec.Cmd.Wait() will go through os.FindProcess,
	// which is the expected path.
	findProcess := os.FindProcess
	if observer != nil && observer.findProcess != nil {
		findProcess = observer.findProcess
	}
	p, err := findProcess(int(pi.ProcessId))
	if p == nil && err == nil {
		err = errors.New("FindProcess returned nil process")
	}
	if err != nil {
		// Post-create orphan case: CreateProcess succeeded, the kernel
		// allocated PID pi.ProcessId, and the child is alive in the
		// OS. But we cannot acquire a usable os.Process handle to
		// drive cmd.Wait / cmd.Kill, so the child is unreachable from
		// Go. Wrap with ErrSpawnPostCreate so the supervisor can
		// distinguish this case from a true pre-child failure and
		// avoid the backoff-respawn-while-orphan-alive race that
		// would otherwise spawn duplicate daemons.
		//
		// Closes bot finding on PR #236 1c0ea09 (P2 #5).
		if !retainedOriginal {
			_ = windows.CloseHandle(pi.Process)
		}
		if stdin != nil && !retainedOriginal {
			_ = job.TerminateAll(1)
			_ = job.Close()
		}
		return int(pi.ProcessId), startWithJobError(StartWithJobLaunch, fmt.Errorf("%w: os.FindProcess(pid=%d): %v", ErrSpawnPostCreate, pi.ProcessId, err))
	}
	cmd.Process = p

	// We opened our own handle via FindProcess; close the duplicate
	// returned by CreateProcess to avoid leaking it for the lifetime
	// of the supervisor.
	if !retainedOriginal {
		_ = windows.CloseHandle(pi.Process)
	}
	runtime.KeepAlive(allowedHandles)

	return int(pi.ProcessId), nil
}

// provisionalWindowsChild keeps the original creation handles distinct from the
// standard Go wrapper. It uses the existing contained-current owner and direct
// creator-handle wait; generic and strict starters retain their own contracts.
type provisionalWindowsChild struct {
	*windowsContainedChild
	ownerSIDFormatting *SIDFormatObservation
	creationTime       string
	creationErr        error
	exitObservation    creatorExitObservation
}

func (c *provisionalWindowsChild) wait() containedWaitResult {
	result := c.windowsContainedChild.wait()
	if result.err == nil && result.exited && c.cmd.Process != nil {
		err := c.cmd.Wait()
		if state := c.cmd.ProcessState; state != nil && state.Pid() == int(c.original.pid) {
			c.exitObservation.known, c.exitObservation.code = true, state.ExitCode()
		} else {
			c.exitObservation.err = creatorClosureUnavailable(errors.New("original standard wait observation is unavailable"))
		}
		var rawExit *exec.ExitError
		if errors.As(err, &rawExit) {
			c.exitObservation.raw = rawExit
		}
		if err != nil {
			var exitErr *exec.ExitError
			if !errors.As(err, &exitErr) {
				result.err = err
			}
		}
	}
	return result
}

func (c *provisionalWindowsChild) creatorBinding() (creatorClosureBinding, error) {
	if c == nil || c.windowsContainedChild == nil || c.cmd == nil || c.cmd.Process == nil || c.job == nil || c.job.handle == 0 || !c.original.atCreate || c.original.job != c.job || c.original.handle != c.process || c.process == 0 || c.process == windows.InvalidHandle || c.thread == 0 || c.thread == windows.InvalidHandle || c.original.pid == 0 {
		return creatorClosureBinding{}, errors.New("original at-create Windows custody is unavailable")
	}
	if c.creationErr != nil {
		return creatorClosureBinding{}, c.creationErr
	}
	return creatorClosureBinding{pid: int(c.original.pid), startedAt: c.creationTime}, nil
}

func (c *provisionalWindowsChild) creatorExitObservation() creatorExitObservation {
	return c.exitObservation
}

// Startup identity comes from the original creation capture, including when
// the standard wrapper was never obtained. Identity alone is not a known exit.
func (c *provisionalWindowsChild) creatorStartupSnapshot() (CreatorRootExitSnapshot, error) {
	if c == nil || c.windowsContainedChild == nil {
		return CreatorRootExitSnapshot{}, creatorClosureUnavailable(errors.New("original startup capture is unavailable"))
	}
	if c.original.pid == 0 {
		return CreatorRootExitSnapshot{}, nil
	}
	root := CreatorRootExitSnapshot{OriginalPID: int(c.original.pid), OriginalStartedAt: c.creationTime}
	if !c.original.atCreate || c.original.job != c.job || c.original.handle != c.process || c.process == 0 || c.process == windows.InvalidHandle {
		return root, creatorClosureUnavailable(errors.New("original startup creation binding is invalid"))
	}
	return root, c.creationErr
}

func (c *provisionalWindowsChild) observeCreatorMembers(ctx context.Context) error {
	observation, err := c.job.observeCreatorClosure(ctx, c.original)
	if err != nil {
		return err
	}
	if !observation.ObservedCurrentMembersExited {
		return creatorClosureUnavailable(errors.New("current Job members remain unsettled"))
	}
	return nil
}

func (c *provisionalWindowsChild) completeCreatorMembers(ctx context.Context) (creatorTerminalObservation, error) {
	observation, err := c.job.completeCreatorAfterExit(ctx, c.original)
	return creatorTerminalObservation{action: observation.terminalAction, actionErr: observation.terminalActionErr, membersExited: observation.ObservedCurrentMembersExited, native: copyCreatorJobObservation(observation.native)}, err
}

func (c *provisionalWindowsChild) terminateCreatorMembers(ctx context.Context) (creatorTerminalObservation, error) {
	if err := creatorClosureContext(ctx); err != nil {
		return creatorTerminalObservation{}, err
	}
	if c == nil || c.job == nil {
		return creatorTerminalObservation{}, creatorClosureUnavailable(errors.New("original creator Job unavailable"))
	}
	deadline, _ := ctx.Deadline()
	ms := time.Until(deadline).Milliseconds()
	if ms < 1 {
		return creatorTerminalObservation{}, context.DeadlineExceeded
	}
	if ms > int64(^uint32(0)) {
		ms = int64(^uint32(0))
	}
	original := c.original
	original.required = true
	observation, err := c.job.observeCurrentJobPolicy(uint32(ms), original, ctx, true, false)
	return creatorTerminalObservation{action: observation.terminalAction, actionErr: observation.terminalActionErr, membersExited: observation.ObservedCurrentMembersExited, native: copyCreatorJobObservation(observation.native)}, err
}

func (c *provisionalWindowsChild) close() error {
	if c.memberSource != nil {
		permit, err := c.memberSource.gate.beginTerminal(nil)
		if err != nil {
			return err
		}
		defer permit.finish()
		return c.closeOriginalWithPermit(permit)
	}
	err := c.windowsContainedChild.close()
	if c.cmd.Process != nil && c.cmd.ProcessState == nil {
		err = errors.Join(err, c.cmd.Process.Release())
	}
	return err
}

func startProvisionalWithJob(job *Job, cmd *exec.Cmd) (containedChild, error) {
	return startProvisionalWithJobObserver(job, cmd, nil)
}

func startProvisionalWithJobObserver(job *Job, cmd *exec.Cmd, findProcess func(int) (*os.Process, error)) (containedChild, error) {
	child, err := newProvisionalWindowsChild(job, cmd)
	if err != nil {
		return child, err
	}
	before, err := job.queryAccounting()
	if err != nil || before.TotalProcesses != 0 || before.ActiveProcesses != 0 {
		return child, errors.Join(err, errors.New("provisional creator Job is not proved empty"))
	}
	return child, startPreparedProvisionalWithJob(child, findProcess)
}

func (job *Job) prepareCreatorStart(cmd *exec.Cmd) (containedChild, func() error, error) {
	if job == nil || job.handle == 0 || cmd == nil || cmd.Process != nil {
		return nil, nil, creatorClosureUnavailable(errors.New("invalid fresh creator Job or command"))
	}
	before, err := job.queryAccounting()
	if err != nil || before.TotalProcesses != 0 || before.ActiveProcesses != 0 {
		return nil, nil, errors.Join(err, errors.New("provisional creator Job is not proved empty"))
	}
	child, err := newProvisionalWindowsChild(job, cmd)
	if err != nil {
		return child, nil, err
	}
	return child, func() error {
		return child.startPreparedCreator(func() error { return startPreparedProvisionalWithJob(child, nil) })
	}, nil
}

// Only the additive retained start requires a complete original binding before
// reporting success. A failed capture stays a post-create startup failure.
func (child *provisionalWindowsChild) startPreparedCreator(start func() error) error {
	if err := start(); err != nil {
		return err
	}
	binding, err := child.creatorBinding()
	if err == nil && (binding.pid <= 0 || binding.startedAt == "") {
		err = errors.New("original creator binding is incomplete after start")
	}
	if err != nil {
		return startWithJobError(StartWithJobLaunch, fmt.Errorf("%w: original creator binding after start: %w", ErrSpawnPostCreate, err))
	}
	return nil
}

func startPreparedProvisionalWithJob(child *provisionalWindowsChild, findProcess func(int) (*os.Process, error)) error {
	job, cmd := child.job, child.cmd
	lookup := findProcess
	if lookup == nil {
		lookup = os.FindProcess
	}
	var wrapperErr error
	_, err := startWithJobFilesObserved(job, cmd, nil, nil, nil, &startWithJobObserverOptions{
		attributeListClosed: func(view ProcThreadAttributeListCloseObservation) { child.attributeClose = view },
		retainCreated: func(pi windows.ProcessInformation) {
			child.process, child.thread = pi.Process, pi.Thread
			child.original = containedJobOriginal{job: job, handle: pi.Process, pid: pi.ProcessId, atCreate: true}
			var created, exited, kernel, user windows.Filetime
			child.creationErr = windows.GetProcessTimes(pi.Process, &created, &exited, &kernel, &user)
			if child.creationErr == nil {
				if created.HighDateTime == 0 && created.LowDateTime == 0 {
					child.creationErr = errors.New("original creation time is unavailable")
				} else {
					child.creationTime = time.Unix(0, created.Nanoseconds()).UTC().Format(time.RFC3339Nano)
				}
			}
		},
		findProcess: func(pid int) (*os.Process, error) {
			process, err := lookup(pid)
			wrapperErr = err
			return process, err
		},
	})
	return errors.Join(err, wrapperErr)
}

func startWithJobCreationFlags() uint32 {
	return uint32(windows.EXTENDED_STARTUPINFO_PRESENT) |
		uint32(windows.CREATE_UNICODE_ENVIRONMENT) |
		uint32(windows.CREATE_NO_WINDOW)
}

// prepareWindowsCommand is the single Windows command-line, working-directory,
// and environment-block owner shared by StartWithJob and RunContainedStream.
// Callers validate cmd and normalize empty Args before entering this helper.
func prepareWindowsCommand(cmd *exec.Cmd) (commandLine, cwdPtr, envPtr *uint16, err error) {
	// Build the command line argv. cmd.Args[0] may differ from cmd.Path (the
	// LookPath-resolved absolute path), so cmd.Path remains the stable argv0.
	argv := append([]string{cmd.Path}, cmd.Args[1:]...)
	commandLine, err = windows.UTF16PtrFromString(windows.ComposeCommandLine(argv))
	if err != nil {
		return nil, nil, nil, fmt.Errorf("UTF16PtrFromString(commandLine): %w", err)
	}

	if cmd.Dir != "" {
		cwdPtr, err = windows.UTF16PtrFromString(cmd.Dir)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("UTF16PtrFromString(cmd.Dir): %w", err)
		}
	}
	if cmd.Env != nil {
		for _, entry := range cmd.Env {
			if strings.IndexByte(entry, 0) >= 0 {
				return nil, nil, nil, errors.New("invalid command environment")
			}
		}
	}
	envPtr, err = createEnvBlock(cmd.Environ())
	if err != nil {
		return nil, nil, nil, fmt.Errorf("createEnvBlock: %w", err)
	}
	return commandLine, cwdPtr, envPtr, nil
}

func duplicateInheritableHandle(file *os.File) (windows.Handle, error) {
	if file == nil {
		return 0, errors.New("nil standard file")
	}
	var duplicate windows.Handle
	if err := windows.DuplicateHandle(
		windows.CurrentProcess(), windows.Handle(file.Fd()), windows.CurrentProcess(),
		&duplicate, 0, true, windows.DUPLICATE_SAME_ACCESS,
	); err != nil {
		return 0, fmt.Errorf("duplicate standard handle %s: %w", file.Name(), err)
	}
	return duplicate, nil
}

// createEnvBlock builds the UTF-16 NUL-separated environment block
// CreateProcess expects when CREATE_UNICODE_ENVIRONMENT is set.
// Returns a pointer to the first uint16 of the block; the block is
// terminated by an additional NUL beyond the last entry.
func createEnvBlock(env []string) (*uint16, error) {
	for _, entry := range env {
		if strings.IndexByte(entry, 0) >= 0 {
			return nil, errors.New("invalid command environment")
		}
	}
	if len(env) == 0 {
		empty := []uint16{0, 0}
		return &empty[0], nil
	}
	if !sort.SliceIsSorted(env, func(i, j int) bool { return windowsEnvironmentLess(env[i], env[j]) }) {
		env = append([]string(nil), env...)
		sort.Slice(env, func(i, j int) bool { return windowsEnvironmentLess(env[i], env[j]) })
	}
	var size int
	for _, s := range env {
		size += len(s) + 1 // string + NUL
	}
	size++ // final NUL terminator

	buf := make([]uint16, 0, size)
	for _, s := range env {
		w, err := windows.UTF16FromString(s)
		if err != nil {
			return nil, fmt.Errorf("UTF16FromString(%q): %w", s, err)
		}
		// UTF16FromString already appends a NUL terminator.
		buf = append(buf, w...)
	}
	buf = append(buf, 0) // final NUL
	return &buf[0], nil
}

func windowsEnvironmentLess(left, right string) bool {
	for i := 0; ; i++ {
		var l, r byte
		if i < len(left) {
			l = left[i]
		}
		if i < len(right) {
			r = right[i]
		}
		if l == '=' || r == '=' || i == len(left) || i == len(right) {
			return l < r
		}
		if l >= 'a' && l <= 'z' {
			l -= 'a' - 'A'
		}
		if r >= 'a' && r <= 'z' {
			r -= 'a' - 'A'
		}
		if l != r {
			return l < r
		}
	}
}
