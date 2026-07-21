//go:build windows

// intent_audit_caller_windows.go — per-OS CallerStartTime helper for
// Windows. Per plan §25 v9: GetProcessTimes returns FILETIME (100ns
// ticks since 1601-01-01 UTC). Filetime.Nanoseconds() already converts
// to Unix-epoch nanoseconds, so the result is a simple time.Unix(0, n)
// in UTC.
//
// Stable for fresh process start: GetProcessTimes returns the kernel-
// recorded creation time of the current process, not now(). Tests in
// intent_audit_test.go assert the emitted value equals this function's own
// result. They deliberately do NOT compare against a ±2min window around
// time.Now(): that older assertion tested how long the api suite takes to
// reach the test rather than the field's correctness, and it failed the
// CORRECT implementation once the package run exceeded two minutes.

package api

import (
	"time"

	"golang.org/x/sys/windows"
)

// CallerStartTime returns the running process's start time in UTC as
// a RFC3339Nano-formattable instant. Uses
// SHGetProcessTimes(GetCurrentProcess()) via golang.org/x/sys/windows.
// Falls back to time.Now().UTC() on API failure so the audit-line
// path never crashes on a kernel weirdness; the fallback is documented
// as best-effort (rare on supported Windows versions).
func CallerStartTime() time.Time {
	var creation, exit, kernel, user windows.Filetime
	handle := windows.CurrentProcess() // pseudo-handle, no Close needed
	if err := windows.GetProcessTimes(handle, &creation, &exit, &kernel, &user); err != nil {
		return time.Now().UTC()
	}
	// Filetime.Nanoseconds converts 100ns-ticks-since-1601 → ns-since-Unix.
	ns := creation.Nanoseconds()
	if ns <= 0 {
		return time.Now().UTC()
	}
	return time.Unix(0, ns).UTC()
}
