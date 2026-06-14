//go:build !windows

package cli

import "os/exec"

// startSupervisorDetachedBreakaway: POSIX has no Windows Job Object
// cascade, so there is no breakaway flag to set. The detach is already
// handled by configureSupervisorDetach (Setsid). This variant just
// starts the cmd; rebuild and onDegrade are unused on POSIX. Kept as a
// matching signature so the cross-platform callers (ensureSupervisorRunning)
// compile on every OS.
func startSupervisorDetachedBreakaway(cmd *exec.Cmd, _ func() *exec.Cmd, _ func(error)) (*exec.Cmd, error) {
	if err := cmd.Start(); err != nil {
		return cmd, err
	}
	return cmd, nil
}
