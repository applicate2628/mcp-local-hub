package cli

import (
	"fmt"
	"runtime"

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
		_ = events.Emit(api.SupervisorEvent{
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

	// Re-raise. The supervisor dies exactly as it did before this guard
	// existed — loudly, with a runtime traceback, exit 2 — but now with a
	// durable row naming which goroutine did it.
	panic(r)
}
