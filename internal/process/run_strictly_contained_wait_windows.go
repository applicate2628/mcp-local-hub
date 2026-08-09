//go:build windows

package process

import "os/exec"

func waitStrictContainedCommand(cmd *exec.Cmd) error {
	return cmd.Wait()
}
