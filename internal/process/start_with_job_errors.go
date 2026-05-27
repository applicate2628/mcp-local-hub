// Package process - cross-platform sentinel errors for StartWithJob.
package process

import "errors"

// ErrSpawnPostCreate marks a StartWithJob failure that occurred AFTER
// the OS child process was created. On Windows this is the FindProcess-
// after-CreateProcess case (start_with_job_windows.go:181-186): the
// kernel has allocated a PID for the child, but the caller cannot
// acquire a usable os.Process handle to drive cmd.Wait / cmd.Kill.
// The child IS alive at the OS level but unobservable from Go.
//
// On POSIX (Linux/macOS) this sentinel is defined but never returned -
// the POSIX StartWithJob path (start_with_job_other.go) only fails at
// cmd.Start which has no analogous post-create handle-acquisition
// step; any cmd.Start error means the child was never created.
//
// Caller semantic: errors.Is(err, ErrSpawnPostCreate) distinguishes
// "child exists but unreachable" from "child never existed". The
// supervisor (internal/cli/supervise.go) uses this to avoid mis-
// classifying the post-create case as errSpawnPreChild, which would
// trigger a backoff respawn while the orphan is still alive.
//
// Closes bot finding on PR #236 1c0ea09 (P2 #5 - Windows StartWithJob
// post-CreateProcess orphan misclassified).
var ErrSpawnPostCreate = errors.New("process: child created but handle acquisition failed (orphan)")
