package process

import "os/exec"

// RunUnderKillJob runs cmd to completion with its entire descendant tree bound
// to a KILL_ON_JOB_CLOSE job, then closes the job so any ORPHAN grandchild — one
// that inherited a pipe and outlived the direct child past a context-deadline
// kill — is reaped instead of leaking and holding the pipe (and, for a server
// target, the port) open. It is the shared spawn path for the heavyweight
// arbitrary-target tools (run_in_oneapi_env, drmemory_run, vtune_profile) whose
// targets routinely launch grandchildren.
//
// Pair it with cmd.WaitDelay (set by the caller, e.g. waitDelayAfterKill): the
// WaitDelay makes cmd.Wait return promptly after the ctx kill even while a
// grandchild still holds the pipe, and the job Close then terminates that
// grandchild. WaitDelay alone unwedges the daemon; the job alone (without
// WaitDelay) could still block on the pipe — both are needed.
//
// Best-effort and fail-open: if the job can't be created (alloc failure) or
// assigned (nested-job limits on pre-Win8, permission) the run still proceeds
// under plain Start+Wait — it just loses orphan reaping, never the run itself.
// On POSIX NewKillOnCloseJob/Assign/Close are no-op stubs, so this degrades to
// Start+Wait there (process-group reaping on POSIX is a separate follow-up); the
// caller's cmd.WaitDelay + context kill still bound the direct child.
//
// The caller must have configured cmd (Env/Dir/Stdout/Stderr/WaitDelay) before
// calling; RunUnderKillJob only owns Start, Assign, Wait, and Close.
func RunUnderKillJob(cmd *exec.Cmd) error {
	job, _ := NewKillOnCloseJob() // (nil,err) on Windows alloc failure; (&Job{},nil) POSIX stub
	if err := cmd.Start(); err != nil {
		if job != nil {
			_ = job.Close()
		}
		return err
	}
	if job != nil {
		// Assign after Start (the documented contract). A failed assign
		// (nested-job constraint, permission) loses only orphan reaping; the
		// run continues and cmd.WaitDelay still bounds the direct child.
		_ = job.Assign(cmd)
		defer func() { _ = job.Close() }()
	}
	return cmd.Wait()
}
