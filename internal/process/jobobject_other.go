//go:build !windows

package process

import "os/exec"

// Job is the cross-platform stub. On POSIX, "kill descendants when
// parent dies" is approached differently:
//   - Linux: prctl(PR_SET_PDEATHSIG, SIGKILL) on the child
//   - macOS / BSD: kqueue NOTE_EXIT or process groups + signal-on-death
//
// Neither is implemented yet. The cooperative tree-kill in
// internal/daemon/treekill.go (pkill -TERM -P) handles graceful Stop();
// orphan protection on force-kill is Windows-only at this point.
// Tracked: serena dashboard orphan repro on Windows, 2026-04-30 — POSIX
// equivalent is future work.
type Job struct{}

// NewKillOnCloseJob is a no-op on POSIX; returns a non-nil empty Job
// so callers can use it without runtime.GOOS branches.
func NewKillOnCloseJob() (*Job, error) { return &Job{}, nil }

// Assign is a no-op on POSIX.
func (j *Job) Assign(_ *exec.Cmd) error { return nil }

// Close is a no-op on POSIX.
func (j *Job) Close() error { return nil }
