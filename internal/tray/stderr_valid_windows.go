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
//
// TWO CALLERS, TWO DIFFERENT STATES. This guard was written for the
// Explorer case above (handles never populated). It now also carries a
// SECOND, unrelated state: `mcphub gui` releases its console at startup
// (process.ReleaseParentConsole) and then spawns the tray, so on a
// terminal launch the handle was VALID and has since been INVALIDATED.
// The tray spawn on that path works only because this guard fires. Do
// not prune it as "Explorer-only legacy".
//
// The GetFileType probe is load-bearing for that second state and the
// cheaper-looking alternatives are not equivalent. Measured on Windows 11
// against a real -H windowsgui build going through the production
// AttachConsole -> reopen CONOUT$ -> FreeConsole sequence:
//
//	after attach:   GetStdHandle=0x168 GetFileType=2      -> valid
//	after release:  GetStdHandle=0x168 GetFileType=0      -> INVALID
//	                (ERROR_INVALID_HANDLE)
//
// FreeConsole does NOT null the std handle slot: the numeric value is
// unchanged and only GetFileType reveals that the underlying kernel
// handle is gone. So a "simplification" to a null / INVALID_HANDLE_VALUE
// comparison would report the dead handle as valid and hand it to
// CreateProcess, breaking the tray on the primary terminal path. Note
// also that os.Stderr.Stat() keeps succeeding on that dead handle (Go
// caches the file type), so it is not a substitute probe either.
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
