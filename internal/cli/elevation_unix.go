//go:build !windows

// Package cli — Task 11 elevation detector (POSIX variant).
//
// Plan v13 §42 Administrator install refusal — POSIX side: an
// elevated process is one whose effective UID is 0 (root). The
// resolution is a single syscall, never errors, so the helper
// always returns nil for the error result. Per §17 the v0.3.0
// watchdog ships Windows-only; the POSIX implementation is here
// for future cross-platform consumers.
package cli

import "os"

// isElevatedReal returns true when os.Geteuid() == 0. The error
// return is always nil on POSIX — Geteuid does not fail. Kept as
// a typed signature symmetric with the Windows variant so callers
// (runSetupWatchdog) don't have to OS-branch.
func isElevatedReal() (bool, error) {
	return os.Geteuid() == 0, nil
}
