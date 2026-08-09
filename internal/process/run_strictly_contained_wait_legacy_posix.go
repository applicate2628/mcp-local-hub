//go:build aix || illumos || solaris

package process

import "os/exec"

// These legacy targets do not yet expose the non-reaping observer used by the
// Linux/BSD owner. Preserve their existing ordering until an equivalent kernel
// primitive is verified for each target.
func waitStrictContainedCommand(cmd *exec.Cmd) error {
	err := cmd.Wait()
	killProcessGroup(cmd)
	return err
}
