//go:build windows

package tray

import (
	"golang.org/x/sys/windows"
)

// stderrIsValid reports whether os.Stderr is backed by a usable
// kernel handle. On Windows, GUI apps launched without an attached
// console (the normal Explorer-launch path) have invalid std
// handles, and passing an invalid *os.File to exec.Cmd's Stderr
// makes Start() fail with ERROR_INVALID_HANDLE. We probe the
// handle via GetFileType — invalid handles return FILE_TYPE_UNKNOWN
// AND a non-zero last-error; valid console / pipe / disk handles
// return one of FILE_TYPE_CHAR / FILE_TYPE_PIPE / FILE_TYPE_DISK
// with a zero last-error. Codex bot review on PR #24 P1.
func stderrIsValid() bool {
	stderr, err := windows.GetStdHandle(windows.STD_ERROR_HANDLE)
	if err != nil || stderr == windows.InvalidHandle || stderr == 0 {
		return false
	}
	t, err := windows.GetFileType(stderr)
	if err != nil {
		return false
	}
	// FILE_TYPE_UNKNOWN with no error happens for some redirected
	// targets that the runtime can still write to; conservatively
	// trust GetFileType returning ANY non-zero type with no error.
	return t != 0
}
