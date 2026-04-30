//go:build !windows

package process

import "os/exec"

// NoConsole is a no-op on POSIX. Linux/macOS exec.Command does not
// allocate consoles; the windowsgui-subsystem console-allocation
// behavior is Windows-specific. Defining the helper unconditionally
// lets call sites avoid build-tag conditionals.
func NoConsole(_ *exec.Cmd) {}
