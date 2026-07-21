package cli

import (
	"fmt"
	"runtime"
	"time"

	"mcp-local-hub/internal/api"
)

// supervisorGoroutinePanicStackCap bounds the traceback captured into the
// event body. 16 KB is the per-entry envelope cap
// (supervisorEventMaxBytes) and `body` is the only truncatable field, so a
// stack larger than this budget would cost the WHOLE body — including the
// `role` and `recovered` fields that carry the attribution. 8 KB leaves
// generous headroom for the envelope while still holding a deep traceback.
// The untruncated traceback is independently available in the stderr sink,
// which the re-raised panic writes to.
const supervisorGoroutinePanicStackCap = 8192

// supervisorGoroutinePanicEmitBudget caps how long the panic guard will wait
// for its audit row before abandoning it and re-raising. Sized to comfortably
// win an uncontended flock while being far too short to matter to a process
// that is dying anyway.
const supervisorGoroutinePanicEmitBudget = 2 * time.Second

// emitSupervisorPanicEventBounded writes the panic row without ever blocking
// the caller for longer than supervisorGoroutinePanicEmitBudget.
//
// WHY A GOROUTINE + SELECT RATHER THAN JUST EmitWithTimeout. Both are needed,
// and the outer bound is the load-bearing one:
//
//	SupervisorEventLog.emit (supervisor_events.go:305-317) takes the
//	in-process mutex `l.mu` with an UNBOUNDED Lock() for every mode except
//	TryEmit, and then — in the blocking mode — holds that mutex across an
//	UNBOUNDED l.lock.Lock() flock wait (:324). So a single concurrent
//	blocking Emit (the supervisor heartbeat is exactly such a caller) that is
//	parked on a wedged flock holds `mu` indefinitely, and a subsequent
//	EmitWithTimeout parks on `mu` BEFORE its own timeout logic is ever
//	reached. EmitWithTimeout alone therefore does NOT bound this call site.
//
// Bounding the wait in the GUARD, independent of the event log's internals,
// is the only construction that holds regardless of which mode another
// goroutine is wedged in.
//
// WHY IT MATTERS SO MUCH HERE. Without the bound, a panic in any guarded
// goroutine turned into a permanently HUNG supervisor: it never reached
// panic(r), so it never produced the runtime traceback the stderr sink exists
// to capture, never exited, and kept holding supervisor.lock — so the
// liveness task saw a live holder and did not relaunch. A crash became a
// silent permanent outage, inverting the purpose of this whole change.
//
// EVIDENCE BEFORE BOOKKEEPING: the durable evidence is the runtime traceback
// produced by the re-raise (captured by the stderr sink). This event row is
// bookkeeping and must never be allowed to delay or prevent it — matching the
// ordering discipline the sibling stall-dump work uses.
//
// The abandoned goroutine is deliberately not waited on: it may stay parked on
// the wedged lock forever, but the process is about to be terminated by the
// runtime, so it cannot outlive anything.
func emitSupervisorPanicEventBounded(events *api.SupervisorEventLog, evt api.SupervisorEvent) {
	done := make(chan struct{})
	go func() {
		defer close(done)
		// Inner bound keeps the common (merely contended) case tidy; the
		// outer select is what makes the total wait guaranteed.
		_ = events.EmitWithTimeout(evt, supervisorGoroutinePanicEmitBudget)
	}()
	select {
	case <-done:
	case <-time.After(supervisorGoroutinePanicEmitBudget):
	}
}

// guardSupervisorGoroutine makes an otherwise-silent goroutine panic
// attributable, then RE-RAISES it.
//
// WHY. api.EventLoop.SetPanicHandler (supervisor_event_loop.go:139-155)
// covers exactly one goroutine: the loop dispatcher. A panic on ANY other
// long-lived supervisor goroutine — the IPC accept loop, a per-connection
// handler, a child-wait goroutine, a reconcile or maintenance timer —
// crashes the process via the Go runtime with no deferred Emit reached, no
// `supervisor-exit` row, and (on Windows) no Error Reporting record. In the
// 2026-07-20 forensic window that shape accounted for deaths that left zero
// trace. This guard closes the gap for every goroutine it is installed on.
//
// CONTRACT: capture-then-re-raise, NEVER swallow. Continuing a supervisor
// past a panic would leave a daemon in a half-applied restart-policy state
// and convert a loud crash into silent corruption — strictly worse than the
// crash. The re-raise preserves today's death semantics exactly (runtime
// traceback, exit 2, recovery layer respawns); the only thing that changes
// is that the death is now attributable. This mirrors the existing
// convention at supervisor_event_loop.go:139-155, which also re-panics.
//
// COMPOSES WITH THE STDERR SINK. runtime.Stack called here retains the
// ORIGINAL panicking frames (verified 2026-07-20 — the frames are still on
// the stack while deferred functions run), so the event body carries a real
// traceback rather than just the recover site. The re-raised panic then
// writes the full untruncated traceback to stderr, which the sink captures.
// Structured attribution and raw traceback land in two independent places.
//
// USAGE — must be deferred DIRECTLY (recover only works in a function
// deferred by the panicking frame's goroutine):
//
//	go func() {
//		defer guardSupervisorGoroutine(events, "ipc-accept", "")
//		acceptIPCConnections(listener, deps)
//	}()
//
// A nil events log is tolerated (the guard still re-raises) so the guard is
// never itself a source of failure.
func guardSupervisorGoroutine(events *api.SupervisorEventLog, role string, taskName string) {
	r := recover()
	if r == nil {
		return
	}

	if events != nil {
		buf := make([]byte, supervisorGoroutinePanicStackCap)
		n := runtime.Stack(buf, false)
		emitSupervisorPanicEventBounded(events, api.SupervisorEvent{
			Severity: api.SupervisorEventSeverityError,
			Source:   api.SupervisorEventSourceLifecycle,
			Event:    "supervisor-goroutine-panic",
			TaskName: taskName,
			Body: map[string]any{
				"role":      role,
				"recovered": fmt.Sprint(r),
				"stack":     string(buf[:n]),
			},
		})
	}

	// Re-raise UNCONDITIONALLY — reached whether the emit above succeeded,
	// timed out, or was abandoned. The supervisor dies exactly as it did
	// before this guard existed (runtime traceback, exit 2, recovery layer
	// respawns); only attribution is added.
	panic(r)
}
