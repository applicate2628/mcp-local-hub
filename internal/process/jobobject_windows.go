//go:build windows

package process

import (
	"errors"
	"fmt"
	"os/exec"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// IsProcessInJob is not exported by golang.org/x/sys/windows v0.43.0,
// so resolve it lazily from kernel32.dll. Used by Job.HasMember below
// and by the StartWithJob assign-at-create test.
var (
	procIsProcessInJob = syscall.NewLazyDLL("kernel32.dll").NewProc("IsProcessInJob")
)

// Job wraps a Windows Job Object configured with
// JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE so that when the last open handle
// to the job is closed — including the kernel-driven cleanup that fires
// when our process is force-killed via `taskkill /F mcphub.exe` —
// every process still assigned to the job is terminated by the kernel.
//
// This is the only reliable mechanism on Windows for "kill descendants
// when parent dies." The cooperative tree-kill in
// internal/daemon/treekill.go runs only on graceful Stop(); when the
// parent is killed without warning, no Go defer / goroutine cleanup
// fires, leaving subprocess trees as orphans (uvx → python → serena
// dashboards on ports 24282-24290 was the observed failure mode).
//
// Caller MUST hold the *Job for the lifetime of the parent process.
// The handle is closed by Close() during graceful shutdown; for
// force-kill the kernel reclaims the handle and the job action fires
// automatically.
//
// Lifetime note on nested jobs: Task Scheduler typically places its
// children in a job already. Win8+ supports nested jobs transparently,
// so AssignProcessToJobObject works even when the calling process is
// itself in a job (see Microsoft docs:
// JOBOBJECT_BASIC_LIMIT_INFORMATION's nested-job semantics). For older
// Windows the assignment fails — log + continue without the
// orphan-protection guarantee rather than break the daemon.
type Job struct {
	handle windows.Handle
}

// NewKillOnCloseJob creates a job object with the kill-on-close limit.
// Returns nil + error on syscall failure; callers should treat the
// error as non-fatal (orphan protection is best-effort) and log it.
func NewKillOnCloseJob() (*Job, error) {
	h, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return nil, fmt.Errorf("CreateJobObject: %w", err)
	}
	var info windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION
	info.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	if _, err := windows.SetInformationJobObject(
		h,
		windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)),
		uint32(unsafe.Sizeof(info)),
	); err != nil {
		_ = windows.CloseHandle(h)
		return nil, fmt.Errorf("SetInformationJobObject: %w", err)
	}
	return &Job{handle: h}, nil
}

// Assign places cmd's process into the job. cmd.Process must be set —
// call after exec.Cmd.Start(). Once assigned, any further process the
// child spawns is automatically placed in the same job by the kernel,
// so the entire descendant tree is covered. There is a tiny race
// between Start() and Assign() where a very fast child could spawn
// grandchildren before the assignment — for our spawn targets (uvx →
// python startup is hundreds of milliseconds) this is not material.
func (j *Job) Assign(cmd *exec.Cmd) error {
	if j == nil || j.handle == 0 {
		return errors.New("Job is nil or already closed")
	}
	if cmd == nil || cmd.Process == nil {
		return errors.New("cmd.Process is nil; call Assign after Start")
	}
	procH, err := windows.OpenProcess(
		windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE,
		false,
		uint32(cmd.Process.Pid),
	)
	if err != nil {
		return fmt.Errorf("OpenProcess pid=%d: %w", cmd.Process.Pid, err)
	}
	defer windows.CloseHandle(procH)
	if err := windows.AssignProcessToJobObject(j.handle, procH); err != nil {
		return fmt.Errorf("AssignProcessToJobObject pid=%d: %w", cmd.Process.Pid, err)
	}
	return nil
}

// Handle returns the underlying Job Object handle. Used by
// StartWithJob to thread the handle through STARTUPINFOEX's attribute
// list (PROC_THREAD_ATTRIBUTE_JOB_LIST) so the child is associated at
// create time, closing the Start-then-Assign race documented above.
//
// Returns 0 if the receiver is nil or already closed; callers that
// pass this through unsafe.Pointer must check for zero first.
func (j *Job) Handle() windows.Handle {
	if j == nil {
		return 0
	}
	return j.handle
}

// HasMember returns true if pid is currently a member of this Job
// Object. Used by StartWithJob tests and (in v0.5.0) by the cold-start
// reaper to verify assignment after re-adopting an orphan daemon.
//
// Returns false on any syscall failure — OpenProcess can fail because
// the PID has already been reaped, because the calling process lacks
// PROCESS_QUERY_LIMITED_INFORMATION rights, or because IsProcessInJob
// itself fails. The helper is safe to call defensively; callers that
// need to distinguish "not a member" from "could not determine" must
// use the kernel32 API directly.
func (j *Job) HasMember(pid int) bool {
	if j == nil || j.handle == 0 {
		return false
	}
	if pid <= 0 {
		return false
	}
	hProc, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return false
	}
	defer windows.CloseHandle(hProc)
	var isMember int32
	r1, _, _ := procIsProcessInJob.Call(
		uintptr(hProc),
		uintptr(j.handle),
		uintptr(unsafe.Pointer(&isMember)),
	)
	if r1 == 0 {
		// IsProcessInJob returned FALSE — syscall failed.
		return false
	}
	return isMember != 0
}

// jobObjectBasicAccountingInformation mirrors Windows
// JOBOBJECT_BASIC_ACCOUNTING_INFORMATION. golang.org/x/sys/windows
// exposes the JobObjectBasicAccountingInformation info class
// constant (= 1) and the QueryInformationJobObject syscall, but not
// the struct definition. ActiveProcesses is the field
// TerminateAll's poll loop reads to detect "all members exited".
//
// Layout per Microsoft docs:
// https://learn.microsoft.com/windows/win32/api/winnt/ns-winnt-jobobject_basic_accounting_information
// 4 × LARGE_INTEGER (8B each) + 4 × DWORD (4B each) = 48 bytes.
type jobObjectBasicAccountingInformation struct {
	TotalUserTime             int64
	TotalKernelTime           int64
	ThisPeriodTotalUserTime   int64
	ThisPeriodTotalKernelTime int64
	TotalPageFaultCount       uint32
	TotalProcesses            uint32
	ActiveProcesses           uint32
	TotalTerminatedProcesses  uint32
}

// terminateAllPollInterval is how often TerminateAll re-queries
// JobObjectBasicAccountingInformation while waiting for
// ActiveProcesses to reach zero. 50ms is a balance between
// responsiveness (sub-deadline tearing-down processes typically
// exit in milliseconds on modern Windows) and syscall pressure
// (each poll is one QueryInformationJobObject call).
const terminateAllPollInterval = 50 * time.Millisecond

// processIDs enumerates the PIDs currently assigned to the Job via
// JobObjectBasicProcessIdList. Used by TerminateAll to wait on each
// individual process's handle AFTER TerminateJobObject — because the
// kernel decrements ActiveProcesses (per Job accounting) before the
// process handles become signaled, so a caller that respawns on
// ActiveProcesses==0 alone still races the orphan's still-tearing-
// down socket. Pre-enumeration is required because TerminateJobObject
// removes PIDs from the id-list as part of teardown; once kicked off
// the list is unreliable.
//
// Returns a copy of the PID list; failures are surfaced to the
// caller, which may proceed with ActiveProcesses-only polling as
// a degraded fallback.
func (j *Job) processIDs() ([]uint32, error) {
	if j == nil || j.handle == 0 {
		return nil, nil
	}
	// JOBOBJECT_BASIC_PROCESS_ID_LIST header is 8 bytes
	// (NumberOfAssignedProcesses uint32 + NumberOfProcessIdsInList uint32),
	// followed by NumberOfProcessIdsInList × uintptr PID entries.
	// Start with room for ~32 PIDs; grow on ERROR_MORE_DATA.
	ptrSize := uint32(unsafe.Sizeof(uintptr(0)))
	bufSize := uint32(8) + 32*ptrSize
	for range 8 {
		buf := make([]byte, bufSize)
		var retlen uint32
		err := windows.QueryInformationJobObject(
			j.handle,
			windows.JobObjectBasicProcessIdList,
			uintptr(unsafe.Pointer(&buf[0])),
			bufSize,
			&retlen,
		)
		if err != nil {
			// ERROR_MORE_DATA → grow buffer and retry.
			if errors.Is(err, windows.ERROR_MORE_DATA) {
				bufSize *= 2
				continue
			}
			return nil, fmt.Errorf("QueryInformationJobObject(BasicProcessIdList): %w", err)
		}
		if retlen < 8 {
			return nil, nil
		}
		numAssigned := *(*uint32)(unsafe.Pointer(&buf[0]))
		numInList := *(*uint32)(unsafe.Pointer(&buf[4]))
		if numAssigned > numInList {
			// More PIDs assigned than the buffer fit — grow + retry.
			bufSize *= 2
			continue
		}
		out := make([]uint32, 0, numInList)
		for i := range numInList {
			off := 8 + i*ptrSize
			if off+ptrSize > retlen {
				break
			}
			pid := uint32(*(*uintptr)(unsafe.Pointer(&buf[off])))
			if pid > 0 {
				out = append(out, pid)
			}
		}
		return out, nil
	}
	return nil, fmt.Errorf("processIDs: buffer grew past safety limit")
}

// MemberPIDs is the exported wrapper around processIDs for callers
// outside the process package — specifically the supervisor's
// orphan-cleanup audit path, which queries the surviving Job
// members AT-TIMEOUT to give the operator an accurate list of PIDs
// still holding the port. The root spawn PID may be dead while a
// descendant is alive (bot P2 finding on PR #241); using only the
// root pid for `taskkill /F /T /PID <orphan_pid>` operator
// guidance can point at the wrong process.
//
// Returns the same shape as processIDs: a copy of the PID list, or
// non-nil error if buffer/syscall handling failed. Callers that
// can degrade to "no surviving-pid hint" treat the error as
// equivalent to an empty list.
func (j *Job) MemberPIDs() ([]uint32, error) {
	return j.processIDs()
}

// TerminateAll kills every process currently in the Job Object via
// the Windows TerminateJobObject syscall AND waits up to timeoutMs
// for the kernel to actually reap the members. The wait is the
// load-bearing part of the contract: TerminateJobObject is
// asynchronous in kernel mode, and a caller that immediately
// rebinds a port (e.g., supervisor backoff respawn) races the
// orphan's still-active socket if there is no wait.
//
// Returns:
//   - nil when the job has zero active processes AND every known
//     PID handle is signaled (kill complete, port released, safe
//     for caller to rebind / respawn)
//   - wrapped error with "timeout" in the message when timeoutMs
//     elapsed before both conditions held (caller MUST treat as
//     kill-failure; do NOT race a respawn)
//   - wrapped error when TerminateJobObject or
//     QueryInformationJobObject failed
//
// Closes bot finding on PR #239 8163d8b (P2 job-wide member-count
// polling, not known-PID polling) by querying the Job's
// ActiveProcesses count via JobObjectBasicAccountingInformation
// (kernel-authoritative count including wrapper-spawned descendants
// we never recorded). The earlier alternative — IsProcessInJob over
// known PIDs only — would have reached count=0 as soon as the
// wrapper died, leaving its uvx/python descendants alive in the
// same Job and still holding the port.
//
// The TestJob_TerminateAllWaitsForExit regression on the per-task
// Job branch caught a second kernel race: Windows decrements
// ActiveProcesses (per Job accounting) before WaitForSingleObject
// on the process handle transitions to signaled. Returning on
// ActiveProcesses==0 alone leaves the caller racing the still-
// tearing-down process handle — observable as cmd.Process still
// alive when TerminateAll returned. Fix: pre-enumerate PIDs via
// JobObjectBasicProcessIdList before TerminateJobObject, open
// SYNCHRONIZE handles, and require BOTH ActiveProcesses==0 AND
// every known handle to be signaled before returning.
// ActiveProcesses stays as the primary guard for wrapper-spawned
// descendants we never recorded; per-handle wait closes the
// kernel race for the PIDs we did capture.
//
// PRECONDITION: callers MUST use this against a per-spawn Job (one
// daemon per Job). Calling against a shared Job (the pre-ADR-#239
// design where runSupervise allocated one Job for the whole
// supervisor) would terminate every healthy daemon along with the
// intended target. Bot P1 on PR #238 331b0df documented this hazard.
func (j *Job) TerminateAll(timeoutMs uint32) error {
	if j == nil || j.handle == 0 {
		return nil
	}
	// Pre-enumerate PIDs AND open SYNCHRONIZE handles BEFORE
	// TerminateJobObject — closing the PID-recycling race the bot
	// flagged as P2 on PR #241. When a job member exits quickly
	// after termination starts, Windows can recycle its PID to an
	// unrelated process; opening the handle AFTER TerminateJobObject
	// would then leave us waiting on the unrelated process and
	// reporting a spurious timeout even though the orphan tree was
	// successfully killed. The handles installed here are tied to
	// the ORIGINAL kernel process objects via the SYNCHRONIZE handle
	// (process object lives until its last handle is closed), so
	// WaitForSingleObject reports the original process's signaled
	// state regardless of PID recycling.
	//
	// Both processIDs() and per-PID OpenProcess failures are
	// non-fatal: TerminateJobObject still fires, and the loop
	// degrades to ActiveProcesses-only polling when the handles
	// slice is empty (handlesAllSignaled returns true on empty).
	pids, _ := j.processIDs()
	handles := make([]windows.Handle, 0, len(pids))
	for _, pid := range pids {
		h, err := windows.OpenProcess(windows.SYNCHRONIZE, false, pid)
		if err == nil {
			handles = append(handles, h)
		}
	}
	defer func() {
		for _, h := range handles {
			_ = windows.CloseHandle(h)
		}
	}()

	if err := windows.TerminateJobObject(j.handle, 1); err != nil {
		// ERROR_ACCESS_DENIED can occur if the job is already being
		// torn down. Continue to the wait loop below - the goal (no
		// processes remain in the job) may still be reached by the
		// kernel's in-flight teardown.
		if !errors.Is(err, windows.ERROR_ACCESS_DENIED) {
			return fmt.Errorf("TerminateJobObject: %w", err)
		}
	}
	if timeoutMs == 0 {
		// Caller opted out of wait. Behavior matches the pre-ADR
		// stub: fire-and-return-immediately. Discouraged for the
		// orphan-cleanup path (caller will race the still-tearing-
		// down processes).
		return nil
	}

	// Poll until ActiveProcesses == 0 AND every known process
	// handle is signaled, OR the deadline elapses.
	var info jobObjectBasicAccountingInformation
	infoSize := uint32(unsafe.Sizeof(info))
	deadline := time.Now().Add(time.Duration(timeoutMs) * time.Millisecond)
	for {
		var retlen uint32
		err := windows.QueryInformationJobObject(
			j.handle,
			windows.JobObjectBasicAccountingInformation,
			uintptr(unsafe.Pointer(&info)),
			infoSize,
			&retlen,
		)
		if err != nil {
			return fmt.Errorf("QueryInformationJobObject(BasicAccounting): %w", err)
		}
		if info.ActiveProcesses == 0 && handlesAllSignaled(handles) {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("TerminateAll: timeout after %dms with %d processes still in job", timeoutMs, info.ActiveProcesses)
		}
		time.Sleep(terminateAllPollInterval)
	}
}

// handlesAllSignaled returns true when every process handle in the
// list is signaled (process fully exited per the kernel). An empty
// list is trivially satisfied. WaitForSingleObject errors are
// treated as "signaled" — the handle is gone, so the underlying
// process must be too. The 0ms timeout makes this a pure state
// query, not a block.
func handlesAllSignaled(handles []windows.Handle) bool {
	for _, h := range handles {
		ev, err := windows.WaitForSingleObject(h, 0)
		if err != nil {
			continue
		}
		if ev != uint32(windows.WAIT_OBJECT_0) {
			return false
		}
	}
	return true
}

// Close releases the job handle. When this is the last handle, the
// kernel applies KILL_ON_JOB_CLOSE and terminates every process still
// in the job. Idempotent.
func (j *Job) Close() error {
	if j == nil || j.handle == 0 {
		return nil
	}
	h := j.handle
	j.handle = 0
	return windows.CloseHandle(h)
}
