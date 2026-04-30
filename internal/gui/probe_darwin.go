// internal/gui/probe_darwin.go
//go:build darwin

package gui

import (
	"path/filepath"
)

// macOS lacks /proc/<pid>/{exe,cmdline,stat}, so the Linux probe
// implementation cannot be reused. Until a libproc/sysctl-based
// macOS probe lands (tracked in docs/superpowers/plans/
// phase-3b-ii-backlog.md), processIDImpl returns an explicit
// "not supported on macOS" sentinel. probeOnce surfaces this error
// as a Malformed-class verdict with a clear message instead of the
// previous "image is not an mcphub binary" cascade that was
// confusing on macOS.
//
// Codex PR #23 P2 #3 (iter-2): split off probe_unix.go's
// `//go:build !windows` tag so darwin no longer compiles in the
// /proc-only Linux implementation. Bare `mcphub gui --force`
// (Probe-only) still works for the diagnostic block; only
// `--force --kill` is unsupported on macOS in this PR. Reboot
// remains the universally available recovery path.
//
// Why exact stub vs partial probe (e.g. syscall.Kill(0) for
// liveness only): keeping ProcessIdentity{} fully zero is the
// least-surprising signal. probeOnce branches on
// (idErr != nil && !id.Alive && id.ImagePath == "") to short-
// circuit into VerdictMalformed with the platform's own message.
// A partial probe (alive=true, image="") would either pass through
// to the identity gate (matchBasename("") false → "image '' is not
// an mcphub binary" — confusing) or require additional Verdict
// fields to disambiguate. The fully-stubbed return is cleaner.

// errMacOSProbeUnsupported is defined in probe.go (cross-platform
// var) so single_instance.go can errors.Is against it on every
// build. Only this darwin processIDImpl actually returns it.

// processIDImpl is the macOS stub. Returns a zero-valued
// ProcessIdentity plus the sentinel error. Single instance code
// surfaces this through Verdict.Diagnose so the operator gets a
// clear "macOS unsupported" message instead of the misleading
// "PID not alive" or "image not mcphub" cascades that would
// otherwise come from the empty fields.
func processIDImpl(pid int) (ProcessIdentity, error) {
	return ProcessIdentity{}, errMacOSProbeUnsupported
}

// killProcessImpl is the macOS stub. Never reached in practice
// because the probe stub above keeps KillRecordedHolder from
// passing the early-class check, but kept defined so the build
// links and so any future direct caller gets a clear error rather
// than a panic. (POSIX kill itself works on darwin; the
// limitation is identity-probe coverage, not signal delivery.)
func killProcessImpl(pid int) error {
	return errMacOSProbeUnsupported
}

// closeProcessHandle is a no-op on darwin (no handle-pinning until
// the libproc-based macOS probe lane lands).
func closeProcessHandle(_ uintptr) {}

// matchBasename mirrors the Linux POSIX rule (no .exe suffix).
// Defined here to keep probe_linux.go tagged `linux` exactly —
// the helper is pure path manipulation, identical to the Linux
// implementation. Cross-platform mcphub binaries on darwin have
// the same `mcphub` filename as Linux.
func matchBasename(path string) bool {
	return filepath.Base(path) == "mcphub"
}
