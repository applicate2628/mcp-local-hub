//go:build !windows

package process

import "os/exec"

// HideToolConsole has no process-creation policy to apply outside Windows.
func HideToolConsole(cmd *exec.Cmd) {}
