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
