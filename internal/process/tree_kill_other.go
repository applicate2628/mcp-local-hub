//go:build !windows

package process

import (
	"errors"
	"fmt"
	"os/exec"
	"syscall"
)

// TreeKillByPID force-terminates pid AND its process group. On POSIX it
// sends SIGKILL to the negated PID (the process GROUP led by pid) so
// child shells / npx wrappers / subprocess trees die too.
//
// This REQUIRES the target to have been spawned as a process-group
// leader (SysProcAttr.Setpgid = true — see NewProcessGroup). When the
// target is NOT a group leader, kill(-pid) returns ESRCH (no group with
// that id) and this falls back to a single-PID SIGKILL so at least the
// leaf dies. The negated-PID kill is safe even without Setpgid: a
// non-leader PID is not a valid PGID, so kill(-pid) cannot accidentally
// reach the supervisor's own group.
//
// This is the tree-kill primitive for fire-and-forget maintenance
// transients, which — unlike daemons — are not wrapped in a containment
// primitive. BestEffortKillByPID sends SIGKILL to a single PID and is
// the wrong tool when descendants must die too.
func TreeKillByPID(pid int) error {
	if pid <= 0 {
		return nil
	}
	if err := syscall.Kill(-pid, syscall.SIGKILL); err != nil {
		if errors.Is(err, syscall.ESRCH) {
			// pid is not a group leader (or the group is already gone).
			// Fall back to a leaf-only kill; ESRCH there means already
			// dead → success.
			if ferr := syscall.Kill(pid, syscall.SIGKILL); ferr != nil && !errors.Is(ferr, syscall.ESRCH) {
				return fmt.Errorf("kill(pid=%d, SIGKILL) fallback: %w", pid, ferr)
			}
			return nil
		}
		return fmt.Errorf("kill(-pid=%d, SIGKILL): %w", pid, err)
	}
	return nil
}

// NewProcessGroup configures cmd so the spawned child becomes a
// process-GROUP leader (PGID == child PID) via SysProcAttr.Setpgid.
// Required so TreeKillByPID's kill(-pgid) reaches the child's whole
// descendant tree. Without it the supervisor inherits the child into
// its own group and tree-kill degrades to a leaf-only kill (the
// documented spawn-side gap that the daemon reaper works around — see
// internal/cli/supervise_reaper_posix.go killProcessGroupSIGKILL). The
// Windows variant is a no-op (taskkill /T needs no group).
func NewProcessGroup(cmd *exec.Cmd) {
	if cmd == nil {
		return
	}
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
}
