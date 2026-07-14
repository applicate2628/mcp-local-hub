//go:build windows

package cbuild

import (
	"log"
	"os/exec"
	"sync"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

// procGroup is the Windows process-tree seam used by runCommand. The child is
// assigned to a Job Object configured with JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE:
// every descendant it spawns is automatically placed in the same job by the
// kernel, so TerminateJobObject (on timeout/cancel) — or simply closing the
// last job handle (KILL_ON_JOB_CLOSE) — reaps the WHOLE tree
// (cmake -> ninja/msbuild -> cl.exe/link.exe), never just the direct child.
//
// This package is intentionally self-contained (it must not import mcphub's
// internal/ packages); the Job Object calls mirror internal/process but are a
// small independent copy scoped to a single per-command tree kill.
type procGroup struct {
	mu     sync.Mutex
	job    windows.Handle
	closed bool
}

func newProcGroup() *procGroup { return &procGroup{} }

// configure sets a new process group (isolation from console CTRL events) and
// creates the kill-on-close Job Object. Job creation is best-effort: on failure
// the group degrades to a single-process kill rather than breaking the command.
func (p *procGroup) configure(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.CreationFlags |= windows.CREATE_NEW_PROCESS_GROUP

	h, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return
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
		return
	}
	p.job = h
}

// start assigns the freshly-started child to the job. There is a tiny window
// between Start and this assignment during which a very fast child could spawn
// grandchildren; for our targets (cmake spawning ninja/msbuild takes tens of
// milliseconds) this is not material, and close() still reaps stragglers.
func (p *procGroup) start(cmd *exec.Cmd) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.job == 0 || cmd.Process == nil {
		return
	}
	procH, err := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, uint32(cmd.Process.Pid))
	if err != nil {
		// Non-fatal: the command still runs, but without job assignment its
		// grandchildren (cmake -> ninja/msbuild -> cl.exe) may orphan if the
		// server is killed. Surface it so the loss of tree-kill protection is
		// observable (mirrors mcphub's own job-protection signal).
		log.Printf("warning: job assignment failed (OpenProcess pid=%d: %v); build grandchildren may orphan if this server is killed", cmd.Process.Pid, err)
		return
	}
	defer windows.CloseHandle(procH)
	if err := windows.AssignProcessToJobObject(p.job, procH); err != nil {
		// Same non-fatal orphan risk as the OpenProcess failure above.
		log.Printf("warning: job assignment failed (AssignProcessToJobObject pid=%d: %v); build grandchildren may orphan if this server is killed", cmd.Process.Pid, err)
	}
}

// kill terminates every process in the job (whole tree). Falls back to a
// single-process kill when no job was created. Held under the lock so it can
// never race close() into a use-after-close on the handle.
func (p *procGroup) kill(cmd *exec.Cmd) {
	p.mu.Lock()
	if p.job != 0 && !p.closed {
		err := windows.TerminateJobObject(p.job, 1)
		p.mu.Unlock()
		if err == nil {
			return
		}
	} else {
		p.mu.Unlock()
	}
	if cmd.Process != nil {
		_ = cmd.Process.Kill()
	}
}

// close releases the job handle. Closing the last handle applies
// KILL_ON_JOB_CLOSE, reaping any process still in the job. Idempotent.
func (p *procGroup) close() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed || p.job == 0 {
		return
	}
	p.closed = true
	h := p.job
	p.job = 0
	_ = windows.CloseHandle(h)
}
