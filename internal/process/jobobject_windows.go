//go:build windows

package process

import (
	"errors"
	"fmt"
	"os/exec"
	"unsafe"

	"golang.org/x/sys/windows"
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
