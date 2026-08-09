//go:build windows

package process

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"sort"
	"strings"
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

// startWithJobFiles is the sole Windows at-create containment owner. The
// exported legacy path deliberately supplies no standard handles; the strict
// runner supplies all three and receives an allowlisted child handle set.
func startWithJobFiles(job *Job, cmd *exec.Cmd, stdin, stdout, stderr *os.File) (int, error) {
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
	if stdin != nil {
		attributeCount++
	}
	attrList, err := windows.NewProcThreadAttributeList(uint32(attributeCount))
	if err != nil {
		return 0, startWithJobError(StartWithJobContainment, fmt.Errorf("NewProcThreadAttributeList: %w", err))
	}
	defer attrList.Delete()

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
	if stdin != nil {
		for _, file := range []*os.File{stdin, stdout, stderr} {
			duplicate, duplicateErr := duplicateInheritableHandle(file)
			if duplicateErr != nil {
				for _, handle := range childHandles {
					_ = windows.CloseHandle(handle)
				}
				return 0, startWithJobError(StartWithJobLaunch, duplicateErr)
			}
			childHandles = append(childHandles, duplicate)
		}
		defer func() {
			for _, handle := range childHandles {
				_ = windows.CloseHandle(handle)
			}
		}()
		if err := attrList.Update(
			windows.PROC_THREAD_ATTRIBUTE_HANDLE_LIST,
			unsafe.Pointer(&childHandles[0]),
			uintptr(len(childHandles))*unsafe.Sizeof(childHandles[0]),
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
		ProcThreadAttributeList: attrList.List(),
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
	creationFlags := uint32(windows.EXTENDED_STARTUPINFO_PRESENT) |
		uint32(windows.CREATE_UNICODE_ENVIRONMENT)

	var pi windows.ProcessInformation
	// lpStartupInfo: take &si.StartupInfo (the embedded first field of StartupInfoEx).
	// Windows reads this as a STARTUPINFOEX because (a) si.Cb == sizeof(StartupInfoEx),
	// (b) creationFlags has EXTENDED_STARTUPINFO_PRESENT, and (c) the embedded layout
	// places ProcThreadAttributeList immediately after the StartupInfo bytes with no
	// padding. Verified against windows.StartupInfoEx struct definition in
	// golang.org/x/sys/windows.
	if err := windows.CreateProcess(
		nil,             // lpApplicationName (NULL → use cmd line argv[0])
		commandLine,     // lpCommandLine
		nil,             // lpProcessAttributes
		nil,             // lpThreadAttributes
		stdin != nil,    // bInheritHandles; HANDLE_LIST restricts inheritance
		creationFlags,   // dwCreationFlags
		envPtr,          // lpEnvironment
		cwdPtr,          // lpCurrentDirectory
		&si.StartupInfo, // lpStartupInfo (see comment above)
		&pi,             // lpProcessInformation
	); err != nil {
		return 0, startWithJobError(StartWithJobLaunch, fmt.Errorf("CreateProcess: %w", err))
	}

	// Close the thread handle; we don't need it for orchestration.
	_ = windows.CloseHandle(pi.Thread)

	// Populate cmd.Process so callers can use Wait()/Kill() through
	// the standard os/exec surface. NOTE: this constructs an
	// *os.Process directly from the PID — the underlying handle in
	// pi.Process is leaked because os.FindProcess on Windows opens
	// its own handle, and we cannot inject pi.Process into the
	// unexported os.Process.handle field. Caller workflows that
	// already use exec.Cmd.Wait() will go through os.FindProcess,
	// which is the expected path.
	p, err := os.FindProcess(int(pi.ProcessId))
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
		_ = windows.CloseHandle(pi.Process)
		if stdin != nil {
			_ = job.TerminateAll(1)
			_ = job.Close()
		}
		return int(pi.ProcessId), startWithJobError(StartWithJobLaunch, fmt.Errorf("%w: os.FindProcess(pid=%d): %v", ErrSpawnPostCreate, pi.ProcessId, err))
	}
	cmd.Process = p

	// We opened our own handle via FindProcess; close the duplicate
	// returned by CreateProcess to avoid leaking it for the lifetime
	// of the supervisor.
	_ = windows.CloseHandle(pi.Process)
	runtime.KeepAlive(childHandles)

	return int(pi.ProcessId), nil
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
