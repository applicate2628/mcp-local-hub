//go:build windows

// Package cli — Task 13.1 Windows no-op stub for the cold-start
// stale-child reaper.
//
// On Windows the supervisor owns its daemon children through a Job
// Object (see internal/process). When the supervisor process exits
// — gracefully or by crash — the Job Object's KILL_ON_JOB_CLOSE limit
// kicks in and the kernel terminates every still-running member of
// the job. There is no scenario in which a Windows supervisor restart
// finds orphan children of a prior generation: the prior supervisor's
// Job Object dies with its handle, and the kernel reaper has already
// collected the children before the new supervisor binds its lock.
//
// The cross-platform API surface still exists on Windows so callers
// (newSuperviseCmd, tests) can invoke ReapStaleTransients
// unconditionally; the function returns an empty ReaperResult and a
// nil error immediately, doing no syscall, no I/O, and no sleep.
//
// Spec source: §"Fallback if step 4 IPC fails" + plan §2660. The
// fallback exists for Linux/macOS where there is no Job Object
// equivalent; the spec explicitly carves Windows out of the manual
// reap path.
package cli

import (
	"context"
	"time"

	"mcp-local-hub/internal/api"
)

// ReaperResult reports what the reaper did during this supervisor cold
// start. On Windows every field is zero-valued because the Job Object
// already reaped any prior-generation children before this function
// runs.
type ReaperResult struct {
	KilledPIDs        []int         // PIDs killed (after ownership gate)
	SkippedPIDs       []int         // PIDs alive but failed ownership gate
	DeadPIDs          []int         // PIDs already gone (no kill needed)
	ClearedTransients int           // size of supervisor-state.transient_pids[] before clear
	SettleDuration    time.Duration // actual settle wait
}

// ReaperDeps allows test injection. On Windows every field is unused
// (the function is a no-op stub); the struct is kept identical to the
// POSIX surface so cross-platform tests compile against the same API.
type ReaperDeps struct {
	StateDir         string
	ReadState        func(path string) (*api.SupervisorStateFile, error)
	WriteState       func(path string, s *api.SupervisorStateFile) error
	PIDAlive         func(pid int) bool
	ProcessIdentity  func(pid int) (basename, cmdline string, uid int, ok bool)
	CurrentUID       func() int
	KillProcessGroup func(pid int) error
	SettleDuration   time.Duration
	Now              func() time.Time
}

// ReapStaleTransients is a no-op on Windows. The Job Object holding
// the supervisor's daemon children dies with the supervisor; the
// kernel-side KILL_ON_JOB_CLOSE handler has already terminated every
// member by the time a new supervisor process starts and calls into
// this function. Returning ReaperResult{} signals "nothing to do" —
// any caller iterating over result.KilledPIDs / .DeadPIDs etc. on
// Windows will see zero entries and skip its log lines accordingly.
//
// The function takes ctx for API parity with POSIX; cancellation is
// not checked because the no-op completes in nanoseconds.
func ReapStaleTransients(ctx context.Context, deps ReaperDeps) (ReaperResult, error) {
	_ = ctx
	_ = deps
	return ReaperResult{}, nil
}
