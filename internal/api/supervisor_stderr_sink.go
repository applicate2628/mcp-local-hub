// Package api — supervisor stderr sink file naming + rotation.
//
// supervisor-stderr.log is the FIFTH file in the state-dir log family
// (alongside watchdog.log, intent-audit.log, gui-events.log, and
// supervisor-events.log). Unlike the other four it is NOT JSON Lines and
// is NOT written by this process's own code: it is the destination the
// supervisor points its OS-level stderr handle at, so that output written
// by the Go RUNTIME — above all an unrecovered panic's `panic: ...` line
// plus goroutine traceback — lands on disk instead of being discarded.
//
// Why it exists (forensic gap, 2026-07-20 investigation): over a 42-hour
// window the supervisor died 8 times out of 9 starts leaving ZERO trace.
// `supervisor-exit` (supervise.go:1378,1408,1440) is emitted on all three
// graceful paths and appeared 0 times, and the loop-dispatch panic observer
// (supervisor_event_loop.go:139-155) fired 0 times. A Go runtime panic on
// any other goroutine prints to stderr and exits 2 — producing no Windows
// Error Reporting record and, with the supervisor's stderr unbound under
// detached autostart, no artifact at all. This file is that artifact.
//
// Ownership split: this package owns NAMING and ROTATION POLICY (so the
// state-dir log family has one place where leaf names and the 10 MB
// ceiling live). The OS-level handle redirect is platform-specific and is
// owned by the supervisor composition root in internal/cli.
package api

// SupervisorStderrSinkFileLeaf is the canonical file name (relative to
// DaemonStateDir) for the supervisor's captured stderr. Exposed so
// internal/cli can construct the full path without re-declaring the
// literal, mirroring SupervisorEventLogFileLeaf.
const SupervisorStderrSinkFileLeaf = "supervisor-stderr.log"

// SupervisorStderrSinkRotateSizeBytes is the 10 MB rotation threshold,
// deliberately identical to supervisorEventLogRotateSize /
// GUIEventLogRotateSizeBytes / WatchdogLogRotateSizeBytes so every file in
// the state-dir log family shares ONE ceiling. Active + .1 bounds the pair
// at ~20 MB.
const SupervisorStderrSinkRotateSizeBytes int64 = 10 * 1024 * 1024

// RotateSupervisorStderrSinkIfOversize renames path -> path+".1" when the
// existing file has reached SupervisorStderrSinkRotateSizeBytes, and
// reports whether a rotation happened.
//
// ROTATION IS AT OPEN TIME ONLY — a deliberate divergence from the JSONL
// logs, which re-check size on every Emit. The sink is written by the OS
// through a raw handle this process does not mediate, so a mid-session
// rotation would have to close that handle, rename, reopen, and re-point
// the std handle. On Windows that additionally requires opening the sink
// with FILE_SHARE_DELETE (Go's os.OpenFile does not), and every ordering
// of the sequence leaves a window in which fd 2 points at a closed handle.
// A panic landing in that window would be LOST — defeating the sink's only
// purpose. Open-time rotation has no such window.
//
// The residual (a single supervisor session writing >10 MB to stderr grows
// the active file until the next start) is bounded in practice because a
// healthy supervisor writes nothing here at all, and is made visible rather
// than silent: the supervisor's heartbeat re-checks the size and reports
// stderr_sink_oversize instead of the file growing quietly.
func RotateSupervisorStderrSinkIfOversize(path string) (bool, error) {
	size, ok := supervisorEventLogFileSize(path)
	if !ok || size < SupervisorStderrSinkRotateSizeBytes {
		return false, nil
	}
	if err := rotateLogFileToBackup(path); err != nil {
		return false, err
	}
	return true, nil
}

// SupervisorStderrSinkOversize reports whether the sink at path has reached
// the rotation threshold. Used by the supervisor heartbeat to surface the
// open-time-rotation-only residual documented above.
func SupervisorStderrSinkOversize(path string) bool {
	size, ok := supervisorEventLogFileSize(path)
	return ok && size >= SupervisorStderrSinkRotateSizeBytes
}
