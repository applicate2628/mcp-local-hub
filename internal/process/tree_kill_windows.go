//go:build windows

package process

import (
	"errors"
	"fmt"
	"os/exec"
	"strconv"
)

// TreeKillByPID force-terminates the process identified by pid AND its
// entire descendant tree. On Windows it shells out to
// `taskkill /F /T /PID <pid>`: /T walks the child-process tree the
// kernel maintains via parent-PID links, /F forces termination.
//
// This is the tree-kill primitive the supervisor uses for
// fire-and-forget maintenance transients, which — unlike daemons — are
// NOT wrapped in a per-task Job Object (see ADR #239 for the daemon
// containment model). BestEffortKillByPID opens a single PID handle and
// calls TerminateProcess; it kills only the root and is the wrong tool
// when a maintenance command (e.g. `mcphub workspace refresh` spawning
// git/npm) has descendants that must die too.
//
// Returns nil when the tree is gone, including taskkill's
// "process not found" exit (128), which means the target already
// exited.
func TreeKillByPID(pid int) error {
	if pid <= 0 {
		return nil
	}
	cmd := exec.Command("taskkill", "/F", "/T", "/PID", strconv.Itoa(pid))
	NoConsole(cmd)
	out, err := cmd.CombinedOutput()
	if err == nil {
		return nil
	}
	// Exit 128 = "the process <pid> not found" — already dead, treat as
	// success so a shutdown race (child exited between snapshot and kill)
	// is not reported as a kill failure.
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && exitErr.ExitCode() == 128 {
		return nil
	}
	return fmt.Errorf("taskkill /F /T /PID %d: %w (%s)", pid, err, string(out))
}

// NewProcessGroup is a no-op on Windows: TreeKillByPID uses
// `taskkill /T`, which walks the descendant tree via kernel parent-PID
// links and needs no spawn-side process-group setup. The POSIX variant
// sets SysProcAttr.Setpgid so kill(-pgid) can reach the tree. Defining
// the helper unconditionally lets the spawner avoid build-tag
// conditionals.
func NewProcessGroup(_ *exec.Cmd) {}
