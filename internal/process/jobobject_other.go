//go:build !windows

package process

import "os/exec"

// Job is the cross-platform stub. On POSIX, force-kill orphan
// protection is split across two layers and neither matches the
// Windows Job Object's full descendant-tree contract:
//
//   - Linux: pdeathsig_linux.go::SetParentDeathSignal sets
//     PR_SET_PDEATHSIG=SIGKILL on each spawn — best-effort
//     direct-child mitigation. Does NOT cascade through wrappers
//     like uvx/npx that fork-and-stay alive after spawning the
//     real server. Robust Linux equivalent (cgroups / systemd
//     scope) is parked behind F2/F3 `mcphub setup --server`.
//
//   - macOS / BSD: nothing yet. Future work would be a kqueue
//     NOTE_EXIT watcher goroutine that on parent exit issues
//     killpg(getpgid(child)). Tracked as F-series follow-up.
//
// The cooperative tree-kill in internal/daemon/treekill.go
// (pkill -TERM -P) still handles graceful Stop() on every POSIX
// platform — it is the force-kill path specifically that this
// Job stub cannot improve.
type Job struct{}

// NewKillOnCloseJob is a no-op on POSIX; returns a non-nil empty Job
// so callers can use it without runtime.GOOS branches.
func NewKillOnCloseJob() (*Job, error) { return &Job{}, nil }

// Assign is a no-op on POSIX.
func (j *Job) Assign(_ *exec.Cmd) error { return nil }

// Close is a no-op on POSIX.
func (j *Job) Close() error { return nil }

// TerminateAll is a no-op on POSIX. The POSIX StartWithJob path
// never returns ErrSpawnPostCreate (the Windows-specific
// FindProcess-after-CreateProcess race does not exist on POSIX),
// so this code path is never triggered in production. The stub
// exists so the supervisor's orphan-cleanup call site can be
// cross-platform without runtime.GOOS branches.
func (j *Job) TerminateAll(_ uint32) error { return nil }

// MemberPIDs is a POSIX stub that always returns an empty list +
// nil error. The supervisor's orphan-cleanup branch queries this
// to surface surviving Job members in audit events; on POSIX the
// Job is a no-op so there are never members to surface, and the
// audit body falls through to the "Job member enumeration
// returned empty" copy without breaking cross-platform compile.
func (j *Job) MemberPIDs() ([]uint32, error) { return nil, nil }
