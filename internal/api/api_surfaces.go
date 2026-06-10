// Package api — ctx-aware API surfaces (StatusContext / RestartContext /
// WaitDaemonRunning-free) + the scheduler-task management surface used by
// `mcphub status`, `mcphub restart`, and the maintenance-task install/remove
// paths.
//
// The v0.6 redesign (spec §5 Phase D) deleted the watchdog recovery engine
// that originally co-owned this file (OwnershipSnapshot / DaemonRegistry /
// OwnedXMLValidator / RecoverStoppedDaemons / the Install/UninstallWatchdog
// task surface). What remains here is the general ctx-aware wrappers and the
// audit/intent/scheduler seams the supervisor + CLI still consume.
//
// Best-effort cancellation contract (§32):
//
//	StatusContext / RestartContext run the underlying op in a goroutine.
//	When ctx is cancelled, the wrapper returns ctx.Err() to the caller
//	within ~10ms. The underlying op continues to completion in the
//	background — its result is dropped. This is best-effort because
//	Status() and Restart() delegate to schtasks, which we cannot interrupt
//	mid-call.
//
// Surfaces owned elsewhere:
//   - IntentAuditEntry / AppendIntentAudit                 → intent_audit.go
//   - DaemonIntent / Reason / Desired / IsActiveStop /
//     ReadDaemonIntent / WriteDaemonIntent                 → daemon_intent.go
//     (the readDaemonIntentFn seam below is bound by daemon_intent.go's
//     init() to a thin adapter over ReadDaemonIntent.)
package api

import (
	"context"
	"time"

	"mcp-local-hub/internal/scheduler"
)

// ---------------------------------------------------------------------------
// Test seams (package-level fn vars).
//
// Production: nil → fall back to the real implementation. Tests in this
// package set these to deterministic fakes inside install*Fn helpers.
//
// Why package-level vars rather than fields on *API: the Task 0 implementation
// constraint says api.go must not be touched (Status/Restart wrappers are
// added here without modifying their existing definitions). Adding fields
// to the API struct would require editing api.go. Package-level seams keep
// the change surface inside the two new files this task owns.
// ---------------------------------------------------------------------------

// statusContextSrcFn, when non-nil, replaces (*API).Status() inside
// StatusContext. Used by tests to inject deterministic []DaemonStatus rows
// without spinning up a scheduler.
var statusContextSrcFn func() ([]DaemonStatus, error)

// restartContextSrcFn, when non-nil, replaces (*API).Restart() inside
// RestartContext. The general-purpose Restart wrapper.
var restartContextSrcFn func(server, daemonFilter string) ([]RestartResult, error)

// schedulerFactoryFn, when non-nil, replaces scheduler.New() for the
// maintenance-task install/remove paths (liveness + legacy-watchdog
// cleanup) and ListManagedTasks. Tests inject an in-memory scheduler that
// records ImportXML / Delete calls.
var schedulerFactoryFn func() (scheduler.Scheduler, error)

// appendIntentAuditFn, when non-nil, replaces the audit-append path
// behind the appendAudit dispatcher. intent_audit.go's init() binds the
// production implementation; tests verify the audit-entry shape.
var appendIntentAuditFn func(IntentAuditEntry) error

// readDaemonIntentFn, when non-nil, replaces the intent-file read path
// invoked by IntentStillRunning. daemon_intent.go owns the production
// implementation; the seam unblocks unit tests.
var readDaemonIntentFn func(taskName string) (DaemonIntent, bool, error)

// ---------------------------------------------------------------------------
// Surfaces owned by sibling files.
// ---------------------------------------------------------------------------

// (DaemonIntent / IntentDesired* / IsActiveStop live in daemon_intent.go.
// The readDaemonIntentFn seam above is bound to the production reader by
// daemon_intent.go's init().)

// (IntentAuditEntry / NewIntentAuditEntry / newSystemAuditEntry /
// MarshalJSON / UnmarshalJSON / IsSystemEntry / RedactIntentAuditEntryForNonOwner
// live in intent_audit.go. The appendIntentAuditFn seam above is bound to
// the production AppendIntentAudit by intent_audit.go's init().)

// ---------------------------------------------------------------------------
// StatusContext (§32).
// ---------------------------------------------------------------------------

// StatusContext wraps (*API).Status with a goroutine + ctx-select pattern.
// On ctx.Done() the wrapper returns (nil, ctx.Err()) immediately; the
// underlying call continues to completion in the goroutine and its
// result is dropped (best-effort cancellation per §32).
//
// PR #215 r2 (codex review Finding 2): the inner production path now
// calls statusInternal(ctx) rather than Status(), so the IPC dial
// deadline is derived from the caller's ctx. Result: caller cancel
// propagates immediately to the supervisor pipe read instead of
// always waiting up to 5s under outage. The outer goroutine pattern
// is retained for the §32 best-effort contract on the legacy
// schtasks fallback path inside statusInternal (StatusWithOpts is
// ctx-blind and can block beyond ctx.Done()).
//
// Production callers that don't need cancellation should keep using
// Status() directly — this wrapper costs one goroutine + one channel
// allocation per call.
func (a *API) StatusContext(ctx context.Context) ([]DaemonStatus, error) {
	type result struct {
		rows []DaemonStatus
		err  error
	}
	// Snapshot the test seam before spawning (see RestartContext): on ctx
	// cancellation the select returns without waiting, so this goroutine can
	// outlive the caller and reading the global inside it would race a later
	// test's t.Cleanup restore. Capture into a local.
	srcFn := statusContextSrcFn
	ch := make(chan result, 1)
	go func() {
		var rows []DaemonStatus
		var err error
		if srcFn != nil {
			rows, err = srcFn()
		} else {
			rows, err = a.statusInternal(ctx)
		}
		// Buffered channel of cap 1 + only-one-sender → never blocks.
		ch <- result{rows: rows, err: err}
	}()
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case r := <-ch:
		return r.rows, r.err
	}
}

// ---------------------------------------------------------------------------
// RestartContext (§32).
// ---------------------------------------------------------------------------

// RestartContext wraps (*API).Restart with a goroutine + ctx-select
// pattern. Best-effort cancellation: ctx.Done() returns ctx.Err() to
// the caller within ~10ms; the underlying Restart continues until
// schtasks completes or fails. General-purpose: reads the manifest
// fresh on each invocation, suitable for `mcphub restart` CLI.
func (a *API) RestartContext(ctx context.Context, server, daemonFilter string) ([]RestartResult, error) {
	type result struct {
		results []RestartResult
		err     error
	}
	// Snapshot the test seam before spawning: on ctx cancellation the select
	// below returns without waiting, so this goroutine can outlive the caller
	// (documented best-effort cancellation). Reading restartContextSrcFn inside
	// the goroutine would then race a later test's t.Cleanup restore of the
	// global. Capture it into a local so the leaked goroutine never touches the
	// mutable package var.
	srcFn := restartContextSrcFn
	ch := make(chan result, 1)
	go func() {
		var res []RestartResult
		var err error
		if srcFn != nil {
			res, err = srcFn(server, daemonFilter)
		} else {
			res, err = a.Restart(server, daemonFilter)
		}
		ch <- result{results: res, err: err}
	}()
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case r := <-ch:
		return r.results, r.err
	}
}

// ---------------------------------------------------------------------------
// IntentStillRunning (§32).
// ---------------------------------------------------------------------------

// IntentStillRunning reports whether the operator-recorded intent for
// taskName permits auto-revive. Returns true iff the intent is NOT an
// active stop directive at evaluation time `now`. Concretely:
//   - missing intent              → true (no recorded preference)
//   - intent.Desired = running    → true
//   - intent.Desired = stopped + fresh (within TTL) → false
//   - intent.Desired = stopped + stale (past TTL)   → true
//
// Wraps ReadDaemonIntent (Task 2 owner, daemon_intent.go) +
// DaemonIntent.IsActiveStop. The readDaemonIntentFn seam is bound by
// daemon_intent.go's init() to a thin adapter over ReadDaemonIntent;
// tests overwrite it via installTestIntentReader for deterministic
// fakes (cleanup restores the production binding).
func (a *API) IntentStillRunning(taskName string, now time.Time) bool {
	if readDaemonIntentFn == nil {
		// Defensive: daemon_intent.go's init() should always have wired
		// this. If we ever observe nil here it means Task 2's init()
		// never ran (unlikely; would imply a binary built without
		// daemon_intent.go), so degrade to "no recorded preference".
		return true
	}
	// Codex deep-sec PR #135 Finding 1: normalize the lookup key so a
	// caller that passed the bare form still hits the canonical leading-
	// backslash entry that WriteDaemonIntent persists.
	taskName = canonicalIntentTaskKey(taskName)
	intent, ok, err := readDaemonIntentFn(taskName)
	if err != nil || !ok {
		// Read failure or no entry → no active stop directive.
		return true
	}
	active, _ := intent.IsActiveStop(now)
	return !active
}

// ---------------------------------------------------------------------------
// Scheduler-task management surface.
// ---------------------------------------------------------------------------

// ListManagedTasks returns the raw scheduler view of every task
// whose name starts with `mcp-local-hub-`. Used by the CLI partial-
// uninstall gate (Codex bot P2) which must determine the post-
// uninstall remaining-server set before deciding whether to remove
// the hub-wide maintenance tasks (liveness / legacy watchdog).
//
// Routes through the schedulerFactoryFn seam so test callers can
// drive deterministic returns without spinning up the real Task
// Scheduler. Production path falls back to scheduler.New().
//
// The returned slice mirrors scheduler.TaskStatus directly; callers
// only need the Name field for the gate decision but the full row
// is surfaced in case future gating policies want to consult State /
// LastResult / Owner.
func (a *API) ListManagedTasks() ([]scheduler.TaskStatus, error) {
	sch, err := newScheduler()
	if err != nil {
		return nil, err
	}
	return sch.List("mcp-local-hub-")
}

// ---------------------------------------------------------------------------
// Internal helpers.
// ---------------------------------------------------------------------------

// newScheduler is the package-level scheduler factory. Routes through
// the schedulerFactoryFn seam if set (tests), otherwise scheduler.New().
func newScheduler() (scheduler.Scheduler, error) {
	if schedulerFactoryFn != nil {
		return schedulerFactoryFn()
	}
	return scheduler.New()
}

// appendAudit is the package-level audit dispatcher. Routes through the
// appendIntentAuditFn seam, which intent_audit.go's init() binds to the
// production AppendIntentAudit. Before that binding it is a no-op rather
// than fail-closed.
func appendAudit(e IntentAuditEntry) error {
	if appendIntentAuditFn != nil {
		return appendIntentAuditFn(e)
	}
	return nil
}
