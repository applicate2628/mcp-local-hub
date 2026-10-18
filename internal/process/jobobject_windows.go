//go:build windows

package process

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// IsProcessInJob is not exported by golang.org/x/sys/windows v0.43.0,
// so resolve it lazily from kernel32.dll. Used by Job.HasMember below
// and by the StartWithJob assign-at-create test.
var (
	procIsProcessInJob            = syscall.NewLazyDLL("kernel32.dll").NewProc("IsProcessInJob")
	jobInformationDLL             = windows.NewLazySystemDLL("kernel32.dll")
	procQueryInformationJobObject = jobInformationDLL.NewProc("QueryInformationJobObject")
	procSetInformationJobObject   = jobInformationDLL.NewProc("SetInformationJobObject")
)

// queryJobInformation keeps the buffer and optional length as Go pointers until
// their direct conversion in the provider's annotated Call argument list.
func queryJobInformation(job windows.Handle, class int32, information unsafe.Pointer, bytes uint32, returnedBytes *uint32) error {
	r1, _, callErr := procQueryInformationJobObject.Call(uintptr(job), uintptr(class), uintptr(information), uintptr(bytes), uintptr(unsafe.Pointer(returnedBytes)))
	return jobInformationCallError(r1, callErr)
}

func setJobInformation(job windows.Handle, class uint32, information unsafe.Pointer, bytes uint32) (int, error) {
	r1, _, callErr := procSetInformationJobObject.Call(uintptr(job), uintptr(class), uintptr(information), uintptr(bytes))
	return int(r1), jobInformationCallError(r1, callErr)
}

// A successful native result ignores stale last-error. False with no native
// error retains the existing provider's EINVAL result.
func jobInformationCallError(result uintptr, callErr error) error {
	if result != 0 {
		return nil
	}
	if callErr == nil || callErr == syscall.Errno(0) {
		return syscall.EINVAL
	}
	return callErr
}

// Job wraps a configured Windows Job Object. When KillOnClose is enabled,
// closing the last open handle terminates every assigned process. This includes
// kernel cleanup after our process is force-killed via `taskkill /F mcphub.exe`.
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
	handle         windows.Handle
	terminationOps *jobTerminationOps // per-Job fault seam; nil uses real Windows operations
	memberCustody  *originalMemberCustody
	memberPolicy   *originalMemberPolicyResult
	closeMu        sync.Mutex
	closeAttempted bool
	closeError     error
}

// NewJob creates a Job Object with exactly the requested lifecycle limits.
// Every requested option is required: creation or configuration failure is
// returned to the caller and the partially-created handle is closed.
func NewJob(options JobOptions) (*Job, error) {
	job, err := NewJobObserved(options)
	if err != nil {
		return nil, err
	}
	return job, nil
}

// NewKillOnCloseJob preserves the established best-effort supervisor API.
func NewKillOnCloseJob() (*Job, error) {
	return NewJob(JobOptions{KillOnClose: true})
}

// Assign places cmd's process into the job. cmd.Process must be set —
// call after exec.Cmd.Start(). Once assigned, any further process the
// child spawns is automatically placed in the same job by the kernel,
// so the entire descendant tree is covered. There is a tiny race
// between Start() and Assign() where a very fast child could spawn
// grandchildren before the assignment — for our spawn targets (uvx →
// python startup is hundreds of milliseconds) this is not material.
func (j *Job) Assign(cmd *exec.Cmd) error {
	if j == nil || j.handle == 0 || j.closeAttempted {
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
	j.closeMu.Lock()
	defer j.closeMu.Unlock()
	if j.closeAttempted {
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
	if j == nil || j.handle == 0 || j.closeAttempted {
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

type jobTerminateTimeoutError struct {
	timeoutMs       uint32
	activeProcesses uint32
}

// JobTerminationProofError identifies the observation that prevented a timed
// termination from proving every original Job member settled.
type JobTerminationProofError struct {
	Stage string
	Cause error
}

func (e *JobTerminationProofError) Error() string {
	return "Job timed termination proof " + e.Stage + ": " + e.Cause.Error()
}
func (e *JobTerminationProofError) Unwrap() error { return e.Cause }

type jobProcessIDList struct {
	Assigned uint32
	PIDs     []uint32
}

type jobTerminationOps struct {
	accounting   func(*Job) (jobObjectBasicAccountingInformation, error)
	listMembers  func(*Job) (jobProcessIDList, error)
	openProcess  func(uint32) (windows.Handle, error)
	isMember     func(windows.Handle, *Job) (bool, error)
	terminate    func(*Job) error
	wait         func(windows.Handle) (uint32, error)
	closeProcess func(windows.Handle) error
	closeJob     func(windows.Handle) error
	now          func() time.Time
	sleep        func(time.Duration)
}

// containedJobOriginal is minted only by the at-create contained launcher.
// The child owns the handle; Job may observe it but must never close it.
type containedJobOriginal struct {
	job      *Job
	handle   windows.Handle
	pid      uint32
	atCreate bool
	required bool
}

type jobTerminationObservation struct {
	ObservedCurrentMembersExited bool
	StrictOriginalsSettled       bool
	strictFailure                error
	terminalAction               creatorTerminalAction
	terminalActionErr            error
	native                       CreatorJobObservation
}

func (j *Job) terminationOperations() jobTerminationOps {
	var ops jobTerminationOps
	if j.terminationOps != nil {
		ops = *j.terminationOps
	}
	if ops.accounting == nil {
		ops.accounting = (*Job).queryAccounting
	}
	if ops.listMembers == nil {
		ops.listMembers = (*Job).queryProcessIDList
	}
	if ops.openProcess == nil {
		ops.openProcess = func(pid uint32) (windows.Handle, error) {
			return windows.OpenProcess(windows.SYNCHRONIZE|windows.PROCESS_QUERY_LIMITED_INFORMATION, false, pid)
		}
	}
	if ops.isMember == nil {
		ops.isMember = jobMemberFromHandle
	}
	if ops.terminate == nil {
		ops.terminate = func(job *Job) error { return windows.TerminateJobObject(job.handle, 1) }
	}
	if ops.wait == nil {
		ops.wait = func(h windows.Handle) (uint32, error) { return windows.WaitForSingleObject(h, 0) }
	}
	if ops.closeProcess == nil {
		ops.closeProcess = windows.CloseHandle
	}
	if ops.closeJob == nil {
		ops.closeJob = windows.CloseHandle
	}
	if ops.now == nil {
		ops.now = time.Now
	}
	if ops.sleep == nil {
		ops.sleep = time.Sleep
	}
	return ops
}

func (j *Job) queryAccounting() (jobObjectBasicAccountingInformation, error) {
	var info jobObjectBasicAccountingInformation
	var used uint32
	err := queryJobInformation(j.handle, windows.JobObjectBasicAccountingInformation, unsafe.Pointer(&info), uint32(unsafe.Sizeof(info)), &used)
	if err != nil {
		return info, err
	}
	if used != 0 && used < uint32(unsafe.Sizeof(info)) {
		return info, fmt.Errorf("short Job accounting response: %d", used)
	}
	return info, nil
}

func jobMemberFromHandle(process windows.Handle, job *Job) (bool, error) {
	var member int32
	r1, _, callErr := procIsProcessInJob.Call(uintptr(process), uintptr(job.handle), uintptr(unsafe.Pointer(&member)))
	if r1 == 0 {
		if callErr != nil && callErr != syscall.Errno(0) {
			return false, callErr
		}
		return false, errors.New("IsProcessInJob failed")
	}
	return member != 0, nil
}

func (e *jobTerminateTimeoutError) Error() string {
	return fmt.Sprintf(
		"TerminateAll: timeout after %dms with %d processes still in job",
		e.timeoutMs,
		e.activeProcesses,
	)
}

func (e *jobTerminateTimeoutError) Unwrap() error {
	return ErrCleanupTimeout
}

// terminateAllPollInterval caps the per-call adaptive Job proof polling
// cadence. Short proof windows observe up to ten times before expiry.
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
// Returns a copy of the complete PID list; an incomplete or malformed
// response is an error, never a proof of membership.
func (j *Job) processIDs() ([]uint32, error) {
	if j == nil || j.handle == 0 || j.closeAttempted {
		return nil, nil
	}
	list, err := j.queryProcessIDList()
	return list.PIDs, err
}

func (j *Job) queryProcessIDList() (jobProcessIDList, error) {
	// JOBOBJECT_BASIC_PROCESS_ID_LIST header is 8 bytes
	// (NumberOfAssignedProcesses uint32 + NumberOfProcessIdsInList uint32),
	// followed by NumberOfProcessIdsInList × uintptr PID entries.
	// Start with room for ~32 PIDs; grow on ERROR_MORE_DATA.
	ptrSize := uint32(unsafe.Sizeof(uintptr(0)))
	bufSize := uint32(8) + 32*ptrSize
	for range 8 {
		buf := make([]byte, bufSize)
		var retlen uint32
		err := queryJobInformation(
			j.handle,
			windows.JobObjectBasicProcessIdList,
			unsafe.Pointer(&buf[0]),
			bufSize,
			&retlen,
		)
		if err != nil {
			// ERROR_MORE_DATA → grow buffer and retry.
			if errors.Is(err, windows.ERROR_MORE_DATA) {
				bufSize *= 2
				continue
			}
			return jobProcessIDList{}, fmt.Errorf("QueryInformationJobObject(BasicProcessIdList): %w", err)
		}
		if retlen < 8 {
			return jobProcessIDList{}, fmt.Errorf("short Job member-list header: %d", retlen)
		}
		numAssigned := *(*uint32)(unsafe.Pointer(&buf[0]))
		numInList := *(*uint32)(unsafe.Pointer(&buf[4]))
		if numAssigned > numInList {
			// More PIDs assigned than the buffer fit — grow + retry.
			bufSize *= 2
			continue
		}
		if numAssigned != numInList || uint64(8)+uint64(numInList)*uint64(ptrSize) > uint64(retlen) || retlen > bufSize {
			return jobProcessIDList{}, fmt.Errorf("malformed Job member-list header assigned=%d listed=%d bytes=%d", numAssigned, numInList, retlen)
		}
		out := make([]uint32, 0, numInList)
		seen := make(map[uint32]struct{}, numInList)
		for i := range numInList {
			off := 8 + i*ptrSize
			word := *(*uintptr)(unsafe.Pointer(&buf[off]))
			if word == 0 || uint64(word) > uint64(^uint32(0)) {
				return jobProcessIDList{}, fmt.Errorf("invalid Job member PID at index %d", i)
			}
			pid := uint32(word)
			if _, duplicate := seen[pid]; duplicate {
				return jobProcessIDList{}, fmt.Errorf("duplicate Job member PID %d", pid)
			}
			seen[pid] = struct{}{}
			out = append(out, pid)
		}
		return jobProcessIDList{Assigned: numAssigned, PIDs: out}, nil
	}
	return jobProcessIDList{}, fmt.Errorf("processIDs: buffer grew past safety limit")
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
// Grandchildren born in the microsecond window between
// processIDs() returning and TerminateJobObject firing are NOT in
// our handles list (they did not exist at enumeration time), so
// their full-exit guarantee rests on ActiveProcesses==0 alone —
// per-handle wait does not cover them. The window is sub-millisecond
// in practice (two adjacent syscalls) and ActiveProcesses is
// kernel-authoritative for Job membership, so this is acceptable
// degradation. Closes sonnet pr-review-toolkit P3 doc-completeness
// nit on PR #241.
//
// PRECONDITION: callers MUST use this against a per-spawn Job (one
// daemon per Job). Calling against a shared Job (the pre-ADR-#239
// design where runSupervise allocated one Job for the whole
// supervisor) would terminate every healthy daemon along with the
// intended target. Bot P1 on PR #238 331b0df documented this hazard.
func (j *Job) TerminateAll(timeoutMs uint32) error {
	observation, err := j.terminateAllObserved(timeoutMs, containedJobOriginal{})
	if err != nil {
		return err
	}
	if timeoutMs == 0 || observation.StrictOriginalsSettled {
		return nil
	}
	return observation.strictFailure
}

// terminateForContained projects only current-tree quiescence. It cannot
// authorize a supervisor respawn or a strict-runner timeout-root deletion.
func (j *Job) terminateForContained(timeoutMs uint32, original containedJobOriginal) error {
	original.required = true
	observation, err := j.terminateAllObserved(timeoutMs, original)
	if err != nil {
		return err
	}
	if observation.ObservedCurrentMembersExited {
		return nil
	}
	return &JobTerminationProofError{Stage: "member_wait", Cause: errors.New("current Job members not proved exited")}
}

func (j *Job) terminateAllObserved(timeoutMs uint32, original containedJobOriginal) (jobTerminationObservation, error) {
	return j.observeCurrentJob(timeoutMs, original, nil, true)
}

func (j *Job) observeCreatorClosure(ctx context.Context, original containedJobOriginal) (jobTerminationObservation, error) {
	if err := creatorClosureContext(ctx); err != nil {
		return jobTerminationObservation{}, err
	}
	original.required = true
	deadline, _ := ctx.Deadline()
	ms := time.Until(deadline).Milliseconds() + 1
	if ms <= 0 {
		return jobTerminationObservation{}, context.DeadlineExceeded
	}
	if ms > int64(^uint32(0)) {
		ms = int64(^uint32(0))
	}
	return j.observeCurrentJob(uint32(ms), original, ctx, false)
}

// Observation capture and current/strict projections have one owner. Natural
// observation stays read-only; the creator's after-exit policy is explicit.
func (j *Job) observeCurrentJob(timeoutMs uint32, original containedJobOriginal, ctx context.Context, terminate bool) (jobTerminationObservation, error) {
	return j.observeCurrentJobPolicy(timeoutMs, original, ctx, terminate, false)
}

func (j *Job) completeCreatorAfterExit(ctx context.Context, original containedJobOriginal) (jobTerminationObservation, error) {
	observation := jobTerminationObservation{terminalAction: creatorTerminalNotRequested}
	if err := creatorClosureContext(ctx); err != nil {
		return observation, err
	}
	original.required = true
	deadline, _ := ctx.Deadline()
	ms := time.Until(deadline).Milliseconds() + 1
	if ms <= 0 {
		return observation, context.DeadlineExceeded
	}
	if ms > int64(^uint32(0)) {
		ms = int64(^uint32(0))
	}
	return j.observeCurrentJobPolicy(uint32(ms), original, ctx, false, true)
}

func (j *Job) observeCurrentJobPolicy(timeoutMs uint32, original containedJobOriginal, ctx context.Context, terminate, terminalAfterExit bool) (jobTerminationObservation, error) {
	if j != nil && j.memberCustody != nil {
		var cancel context.CancelFunc
		if ctx == nil && timeoutMs != 0 {
			ctx, cancel = context.WithTimeout(context.Background(), time.Duration(timeoutMs)*time.Millisecond)
			defer cancel()
		}
		permit, owned, err := j.enterMemberTerminal(ctx)
		if err != nil {
			return jobTerminationObservation{}, err
		}
		if owned {
			defer permit.finish()
		}
		return j.observeCurrentJobPolicyEntered(timeoutMs, original, ctx, terminate, terminalAfterExit, permit)
	}
	return j.observeCurrentJobPolicyCore(timeoutMs, original, ctx, terminate, terminalAfterExit, nil)
}

func (j *Job) Close() error {
	if j != nil && j.memberCustody != nil {
		permit, err := j.memberCustody.beginTerminal(nil)
		if err != nil {
			return err
		}
		defer permit.finish()
		return j.closeWithMemberPermit(permit)
	}
	return j.closeWithMemberPermit(nil)
}

func (j *Job) observeCurrentJobPolicyCore(timeoutMs uint32, original containedJobOriginal, ctx context.Context, terminate, terminalAfterExit bool, permit *originalMemberTerminalPermit) (jobTerminationObservation, error) {
	var observation jobTerminationObservation
	if terminalAfterExit {
		observation.terminalAction = creatorTerminalNotRequested
		if err := creatorClosureContext(ctx); err != nil {
			return observation, err
		}
	}
	if timeoutMs == 0 && !terminate {
		return observation, context.DeadlineExceeded
	}
	if j == nil || j.handle == 0 || j.closeAttempted {
		if timeoutMs == 0 {
			return observation, nil
		}
		return observation, &JobTerminationProofError{Stage: "invalid_job", Cause: errors.New("nil or closed Job cannot prove timed termination")}
	}
	ops := j.terminationOperations()
	if err := permit.markNativeEntry(ctx); err != nil {
		return observation, err
	}
	if timeoutMs == 0 {
		if err := ops.terminate(j); err != nil && !errors.Is(err, windows.ERROR_ACCESS_DENIED) {
			return observation, fmt.Errorf("TerminateJobObject: %w", err)
		}
		return observation, nil
	}
	var problems []error
	add := func(stage string, cause error) {
		problems = append(problems, &JobTerminationProofError{Stage: stage, Cause: cause})
	}
	initial, snapshotErr := ops.accounting(j)
	observation.native = CreatorJobObservation{Version: 1, Members: make([]CreatorMemberObservation, 0)}
	if snapshotErr == nil {
		observation.native.InitialAccountingKnown = true
		observation.native.InitialTotal = initial.TotalProcesses
		observation.native.InitialActive = initial.ActiveProcesses
	}
	if snapshotErr != nil {
		add("snapshot", snapshotErr)
	}
	borrowed := original.atCreate && original.job == j && original.handle != 0 && original.handle != windows.InvalidHandle && original.pid != 0
	if original.required && !borrowed {
		add("borrowed_handle", errors.New("invalid at-create original process evidence"))
	}
	var list jobProcessIDList
	if snapshotErr == nil {
		var listErr error
		list, listErr = ops.listMembers(j)
		if listErr == nil {
			observation.native.MemberListQueryKnown = true
			observation.native.RawAssignedMembers = list.Assigned
			observation.native.CapturedMemberPIDs = append(make([]uint32, 0, len(list.PIDs)), list.PIDs...)
		}
		if listErr != nil {
			add("member_list", listErr)
		} else {
			seen := make(map[uint32]struct{}, len(list.PIDs))
			if uint32(len(list.PIDs)) != list.Assigned || (len(list.PIDs) == 0 && (!borrowed || initial.ActiveProcesses != 0)) {
				add("member_list", fmt.Errorf("incomplete Job member list assigned=%d listed=%d", list.Assigned, len(list.PIDs)))
			}
			for _, pid := range list.PIDs {
				if pid == 0 {
					add("member_list", errors.New("zero Job member PID"))
					break
				}
				if _, duplicate := seen[pid]; duplicate {
					add("member_list", fmt.Errorf("duplicate Job member PID %d", pid))
					break
				}
				seen[pid] = struct{}{}
			}
			if len(problems) == 0 {
				covered := list.Assigned
				if borrowed {
					seenBorrowed := false
					for _, pid := range list.PIDs {
						if pid == original.pid {
							seenBorrowed = true
							break
						}
					}
					if !seenBorrowed {
						covered++
					}
				}
				if covered != initial.TotalProcesses {
					observation.strictFailure = &JobTerminationProofError{Stage: "membership_changed", Cause: fmt.Errorf("lifetime count=%d captured originals=%d", initial.TotalProcesses, covered)}
				}
			}
		}
	}
	if len(problems) == 0 {
		observation.native.MemberListKnown = true
		observation.native.AssignedMembers = list.Assigned
	}
	handles := make([]windows.Handle, 0, len(list.PIDs))
	memberPositions := make(map[windows.Handle]int)
	if len(problems) == 0 {
		for _, pid := range list.PIDs {
			if terminalAfterExit {
				if err := creatorClosureContext(ctx); err != nil {
					add("context", err)
					break
				}
			}
			h, openErr := ops.openProcess(pid)
			if openErr != nil || h == 0 {
				if openErr == nil {
					openErr = errors.New("zero member handle")
				}
				add("member_handle", fmt.Errorf("pid=%d: %w", pid, openErr))
				continue
			}
			handles = append(handles, h)
			memberPositions[h] = len(observation.native.Members)
			observation.native.Members = append(observation.native.Members, CreatorMemberObservation{PID: pid})
			member, memberErr := ops.isMember(h, j)
			if memberErr != nil {
				add("member_identity", fmt.Errorf("pid=%d: %w", pid, memberErr))
			} else if !member {
				add("member_identity", fmt.Errorf("pid=%d not in original Job", pid))
			} else {
				observation.native.Members[memberPositions[h]].OriginalJobMemberKnown = true
			}
		}
	}

	if terminalAfterExit && len(problems) == 0 {
		if err := creatorClosureContext(ctx); err != nil {
			add("context", err)
		}
		emptyAtCapture := initial.ActiveProcesses == 0
		if emptyAtCapture {
			for _, handle := range handles {
				if err := creatorClosureContext(ctx); err != nil {
					add("context", err)
					break
				}
				status, err := ops.wait(handle)
				if err != nil {
					add("member_wait", err)
					break
				}
				switch status {
				case uint32(windows.WAIT_OBJECT_0):
				case uint32(windows.WAIT_TIMEOUT):
					emptyAtCapture = false
				default:
					add("member_wait", fmt.Errorf("unexpected wait status %d", status))
				}
				if len(problems) != 0 {
					break
				}
			}
		}
		// A genuine completed original wait is also required at the native
		// mutation boundary. Never turn pending-root cleanup into a force trigger.
		if len(problems) == 0 {
			status, err := ops.wait(original.handle)
			if err != nil {
				add("borrowed_wait", err)
			} else if status != uint32(windows.WAIT_OBJECT_0) {
				add("borrowed_wait", errors.New("original root is not signaled before terminal policy"))
			}
		}
		if len(problems) == 0 {
			if err := creatorClosureContext(ctx); err != nil {
				add("context", err)
			} else if !emptyAtCapture {
				observation.terminalAction = creatorTerminalRequestedUnknown
				observation.terminalActionErr = ops.terminate(j)
				if observation.terminalActionErr != nil {
					observation.terminalAction = creatorTerminalRequestFailed
					add("terminal_action", observation.terminalActionErr)
				} else {
					observation.terminalAction = creatorTerminalRequestSucceeded
				}
			}
		}
	}
	if terminate && ctx != nil {
		if err := creatorClosureContext(ctx); err != nil {
			add("context", err)
		}
	}
	if terminate && (ctx == nil || len(problems) == 0) {
		observation.terminalAction = creatorTerminalRequestedUnknown
		terminateErr := ops.terminate(j)
		observation.terminalActionErr = terminateErr
		if terminateErr == nil {
			observation.terminalAction = creatorTerminalRequestSucceeded
		} else {
			observation.terminalAction = creatorTerminalRequestFailed
		}
		if terminateErr != nil && !errors.Is(terminateErr, windows.ERROR_ACCESS_DENIED) {
			problems = append(problems, fmt.Errorf("TerminateJobObject: %w", terminateErr))
		}
	}
	pollMs := timeoutMs / 10
	if pollMs < 1 {
		pollMs = 1
	}
	pollInterval := time.Duration(pollMs) * time.Millisecond
	if pollInterval > terminateAllPollInterval {
		pollInterval = terminateAllPollInterval
	}
	deadline := ops.now().Add(time.Duration(timeoutMs) * time.Millisecond)
	if ctx != nil {
		deadline, _ = ctx.Deadline()
	}
	if len(problems) == 0 {
		for {
			if ctx != nil {
				if err := creatorClosureContext(ctx); err != nil {
					add("context", err)
					break
				}
			}
			info, err := ops.accounting(j)
			if err != nil {
				add("accounting", err)
				break
			}
			if info.TotalProcesses != initial.TotalProcesses {
				add("membership_changed", fmt.Errorf("lifetime count changed %d to %d", initial.TotalProcesses, info.TotalProcesses))
				break
			}
			allSignaled := info.ActiveProcesses == 0
			if allSignaled {
				for _, h := range handles {
					if terminalAfterExit {
						if err := creatorClosureContext(ctx); err != nil {
							add("context", err)
							allSignaled = false
							break
						}
					}
					status, waitErr := ops.wait(h)
					if waitErr == nil {
						observation.native.Members[memberPositions[h]].WaitKnown = true
						observation.native.Members[memberPositions[h]].WaitResult = status
					} else {
						observation.native.Members[memberPositions[h]].ControlledFailure = "member-wait-failed"
					}
					if waitErr != nil {
						add("member_wait", waitErr)
						allSignaled = false
						break
					}
					switch status {
					case uint32(windows.WAIT_OBJECT_0):
					case uint32(windows.WAIT_TIMEOUT):
						allSignaled = false
					default:
						add("member_wait", fmt.Errorf("unexpected wait status %d", status))
						allSignaled = false
					}
					if len(problems) != 0 {
						break
					}
				}
				if allSignaled && borrowed {
					if terminalAfterExit {
						if err := creatorClosureContext(ctx); err != nil {
							add("context", err)
							break
						}
					}
					status, waitErr := ops.wait(original.handle)
					if waitErr == nil {
						observation.native.OriginalRootWaitKnown = true
						observation.native.OriginalRootWaitResult = status
					}
					if waitErr != nil {
						add("borrowed_wait", waitErr)
						allSignaled = false
					} else if status == uint32(windows.WAIT_TIMEOUT) {
						allSignaled = false
					} else if status != uint32(windows.WAIT_OBJECT_0) {
						add("borrowed_wait", fmt.Errorf("unexpected wait status %d", status))
						allSignaled = false
					}
				}
			}
			if len(problems) != 0 {
				break
			}
			if !ops.now().Before(deadline) {
				if ctx != nil {
					add("context", context.DeadlineExceeded)
				} else {
					add("member_wait", &jobTerminateTimeoutError{timeoutMs: timeoutMs, activeProcesses: info.ActiveProcesses})
				}
				break
			}
			if allSignaled {
				final, finalErr := ops.accounting(j)
				if finalErr == nil {
					observation.native.FinalAccountingKnown = true
					observation.native.FinalTotal = final.TotalProcesses
					observation.native.FinalActive = final.ActiveProcesses
				}
				if finalErr != nil {
					add("accounting", finalErr)
				} else if final.TotalProcesses != initial.TotalProcesses {
					add("membership_changed", fmt.Errorf("final lifetime count changed %d to %d", initial.TotalProcesses, final.TotalProcesses))
				} else if final.ActiveProcesses != 0 {
					add("accounting", fmt.Errorf("final active member count=%d", final.ActiveProcesses))
				}
				if !ops.now().Before(deadline) {
					if ctx != nil {
						add("context", context.DeadlineExceeded)
					} else {
						add("member_wait", &jobTerminateTimeoutError{timeoutMs: timeoutMs, activeProcesses: final.ActiveProcesses})
					}
				}
				if ctx != nil {
					if err := creatorClosureContext(ctx); err != nil {
						add("context", err)
					}
				}
				if len(problems) == 0 {
					observation.ObservedCurrentMembersExited = true
				}
				break
			}
			pause := deadline.Sub(ops.now())
			if pause > pollInterval {
				pause = pollInterval
			}
			if ctx == nil {
				ops.sleep(pause)
			} else {
				timer := time.NewTimer(pause)
				select {
				case <-ctx.Done():
				case <-timer.C:
				}
				if !timer.Stop() {
					select {
					case <-timer.C:
					default:
					}
				}
			}
		}
	}
	for _, h := range handles {
		position := memberPositions[h]
		if err := ops.closeProcess(h); err != nil {
			add("handle_close", err)
			observation.native.Members[position].ControlledFailure = "member-handle-close-failed"
		} else {
			observation.native.Members[position].HandleClosed = true
		}
	}
	observation.native.MemberHandlesClosed = true
	for _, member := range observation.native.Members {
		if !member.HandleClosed {
			observation.native.MemberHandlesClosed = false
		}
	}
	if len(problems) != 0 {
		return observation, errors.Join(problems...)
	}
	if observation.ObservedCurrentMembersExited && observation.strictFailure == nil {
		observation.StrictOriginalsSettled = true
	}
	if observation.strictFailure == nil && !observation.StrictOriginalsSettled {
		observation.strictFailure = &JobTerminationProofError{Stage: "member_list", Cause: errors.New("lifetime member coverage not proved")}
	}
	return observation, nil
}

// Close releases the job handle. When this is the last handle, the
// kernel applies KILL_ON_JOB_CLOSE and terminates every process still
// in the job. Idempotent.
func (j *Job) closeWithMemberPermit(permit *originalMemberTerminalPermit) error {
	if j == nil {
		return nil
	}
	j.closeMu.Lock()
	defer j.closeMu.Unlock()
	if j != nil && j.memberCustody != nil {
		if err := permit.validate(j.memberCustody); err != nil {
			return err
		}
	}
	if j == nil {
		return nil
	}
	if j.closeAttempted {
		return j.closeError
	}
	if j.handle == 0 {
		return nil
	}
	j.closeAttempted = true
	j.closeError = j.terminationOperations().closeJob(j.handle)
	if j.closeError == nil {
		j.handle = 0
	}
	return j.closeError
}
