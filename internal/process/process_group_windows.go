//go:build windows

package process

import "os/exec"

// On Windows the KILL_ON_JOB_CLOSE Job Object (RunUnderKillJob) owns descendant
// tree-kill, so the POSIX process-group setup/reaping is a no-op here.
func prepareProcessGroup(cmd *exec.Cmd) {}

func killProcessGroup(cmd *exec.Cmd) {}
