// Package cli — Task 5.2 quiesce-timers side-goroutine drain handler
// for the v0.5.0 supervisor IPC channel.
//
// Spec §"Control IPC trust boundary (detail)" + §"Graceful exit +
// quiesce drain" + §"quiesce-timers" in the term index.
//
// The `quiesce-timers` IPC command drains in-flight maintenance-timer
// transient PIDs from `supervisor-state.transient_pids` before
// supervisor exit. It runs on a SEPARATE goroutine (not in the FIFO
// event loop) so the loop can continue processing while drain is in
// progress, then posts `quiesce-complete` back into the loop when
// done.
//
// Two-frame IPC response (spec §"Wire format"):
//
//   - Frame 1 (immediate): `{accepted: true}` — caller-side sent BEFORE
//     invoking Drain; gives the client a prompt ack while drain runs.
//   - Frame 2 (final): `{drained: N, still_running: [pids], final: true}`
//     — caller-side sent AFTER Drain returns.
//
// Frame ordering is the IPC dispatcher's responsibility; this handler
// is just the drain primitive. The handler's only IPC visibility is
// the QuiesceResult it returns from Drain.
package cli

import (
	"context"
	"sync/atomic"
	"time"

	"mcp-local-hub/internal/api"
)

// QuiesceHandler drains in-flight maintenance-timer transient PIDs
// from supervisor-state.transient_pids before supervisor exit. Runs
// on a side goroutine (NOT in FIFO event loop) so the loop can
// continue processing.
//
// The handler is cross-platform: liveness probing uses
// os.FindProcess + Signal(0), which works on both Windows and POSIX
// per the existing isOwnerLive() pattern at
// internal/api/supervisor_lock.go:113-141.
type QuiesceHandler struct {
	state     *api.SupervisorStateFile
	statePath string
	// inProgress is the atomic flag the FIFO event loop's per-timer
	// fire path checks to suppress new timer fires while drain is
	// active (spec §"Graceful exit + quiesce drain" step 1).
	inProgress atomic.Bool
	// initialCount snapshots the transient-PID count when Drain
	// starts; used by Drain's drained-count math (initial - still_alive).
	// Snapshotted rather than re-computed so concurrent state-file
	// mutations during drain don't skew the reported drained count.
	initialCount int
}

// NewQuiesceHandler constructs a handler bound to a snapshot of the
// supervisor state. The state pointer is NOT deep-copied; the caller
// guarantees the slice isn't mutated for the duration of the Drain
// call (Phase 5 wires Drain on the IPC goroutine while the FIFO
// loop owns state mutations — the in-progress flag suppresses new
// transient-PID appends).
func NewQuiesceHandler(state *api.SupervisorStateFile, statePath string) *QuiesceHandler {
	return &QuiesceHandler{state: state, statePath: statePath}
}

// InProgress reports whether a quiesce drain is currently active.
// Read by FIFO event-loop handlers to suppress new timer fires per
// spec §"Graceful exit + quiesce drain" step 1.
func (q *QuiesceHandler) InProgress() bool {
	return q.inProgress.Load()
}

// QuiesceResult is the body of the final frame for the quiesce-timers
// IPC response per spec §"Wire format":
//
//	{"id": 46, "ok": true,
//	 "result": {"drained": 1, "still_running": []},
//	 "final": true}
//
// `Drained` = (initial transient_pid count) - (still_alive count) at
// completion. `StillRunning` is the surviving PIDs after the timeout
// expired; callers feed this to taskkill /F /T (Windows) or
// `kill -KILL` (POSIX) as the post-drain force-kill phase.
type QuiesceResult struct {
	Drained      int   `json:"drained"`
	StillRunning []int `json:"still_running"`
}

// Drain blocks until all transient_pids are gone OR timeoutMs
// expires OR ctx is cancelled. Returns the drain result. The caller
// is responsible for sending the immediate `{accepted: true}` frame
// BEFORE invoking Drain (so the IPC client gets the ack while drain
// is still in flight per spec §"Wire format").
//
// Liveness is polled at 50ms granularity — fast enough that a
// transient child exit is observed within 50ms of the syscall
// returning, slow enough that the syscall load is negligible
// (20 probes/sec across at most a handful of transient PIDs).
//
// Empty initial state (no transient PIDs) returns immediately with
// `{Drained: 0, StillRunning: []}`.
func (q *QuiesceHandler) Drain(ctx context.Context, timeoutMs int) QuiesceResult {
	q.inProgress.Store(true)
	defer q.inProgress.Store(false)

	// Snapshot initial count at the START of drain so the reported
	// `drained` value is unaffected by concurrent mutations.
	q.initialCount = q.transientCount()

	// Fast path: no transients to drain.
	if q.initialCount == 0 {
		return QuiesceResult{Drained: 0, StillRunning: []int{}}
	}

	deadline := time.Now().Add(time.Duration(timeoutMs) * time.Millisecond)
	for time.Now().Before(deadline) {
		stillRunning := q.aliveTransientPIDs()
		if len(stillRunning) == 0 {
			return QuiesceResult{Drained: q.initialCount, StillRunning: []int{}}
		}
		select {
		case <-ctx.Done():
			return QuiesceResult{
				Drained:      q.initialCount - len(stillRunning),
				StillRunning: stillRunning,
			}
		case <-time.After(50 * time.Millisecond):
		}
	}
	// Deadline expired; return whatever is still alive for force-kill.
	stillRunning := q.aliveTransientPIDs()
	return QuiesceResult{
		Drained:      q.initialCount - len(stillRunning),
		StillRunning: stillRunning,
	}
}

// aliveTransientPIDs returns the subset of state.TransientPIDs that
// are still alive per the cross-platform isPIDAlive probe.
func (q *QuiesceHandler) aliveTransientPIDs() []int {
	if q.state == nil {
		return []int{}
	}
	alive := []int{}
	for _, t := range q.state.TransientPIDs {
		if isPIDAlive(t.PID) {
			alive = append(alive, t.PID)
		}
	}
	return alive
}

// transientCount returns len(state.TransientPIDs) at call time, or 0
// when state is nil. Used to snapshot the initial drain count.
func (q *QuiesceHandler) transientCount() int {
	if q.state == nil {
		return 0
	}
	return len(q.state.TransientPIDs)
}

// isPIDAlive probes whether the given PID is still running. The
// concrete probe is platform-specific:
//
//   - Windows (supervise_quiesce_windows.go): os.FindProcess wraps
//     windows.OpenProcess with SYNCHRONIZE+PROCESS_QUERY_INFORMATION,
//     so it FAILS for non-existent PIDs. After the open we
//     WaitForSingleObject(0) to distinguish "handle open but process
//     signaled (= already exited)" from "handle open and running".
//   - POSIX (supervise_quiesce_posix.go): os.FindProcess ALWAYS
//     succeeds — Go docs say so explicitly. We follow with
//     Process.Signal(syscall.Signal(0)) which delivers signal 0 via
//     kill(2): returns ESRCH for dead, EPERM for "alive but not
//     ours". Both are treated as not-our-PID-to-wait-on.
//
// PID 0 is treated as not-live (canonical "unset" sentinel; any
// TransientPID with PID 0 is malformed). The shared zero-PID guard
// lives here so both platform variants benefit from it without
// duplication.
//
// (The function delegates to pidAliveImpl which has the
// platform-specific body in the matching _windows.go / _posix.go
// file. Keeping the zero-guard in the cross-platform shim lets a
// single test on either platform exercise the boundary.)
func isPIDAlive(pid int) bool {
	// Codex r6 P3 fix: reject negative PIDs up front. On POSIX,
	// kill(2)/Signal(0) treats a negative argument as a process-GROUP
	// probe (PGID = |pid|), so a malformed transient_pids entry like
	// -123 would be reported alive whenever PGID 123 exists — quiesce
	// then stalls until timeout and misclassifies the entry as
	// still_running. The supervisor only manages per-PID transients,
	// so anything <= 0 is invalid input from a corrupt state file.
	if pid <= 0 {
		return false
	}
	return pidAliveImpl(pid)
}
