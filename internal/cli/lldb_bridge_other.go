//go:build !windows

package cli

import "os/exec"

// applyNoWindow is a no-op outside Windows — there is no HWND to suppress.
func applyNoWindow(cmd *exec.Cmd) {}
