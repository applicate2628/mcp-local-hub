//go:build !linux

package process

import "os/exec"

// SetParentDeathSignal is a no-op on platforms without
// prctl(PR_SET_PDEATHSIG). Windows handles orphan protection via
// Job Object KILL_ON_JOB_CLOSE (see jobobject_windows.go). macOS
// and BSDs need a kqueue NOTE_EXIT watcher goroutine — separate
// follow-up.
func SetParentDeathSignal(_ *exec.Cmd) {}
