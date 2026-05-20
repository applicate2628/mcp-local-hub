package cli

import (
	"context"
	"time"

	"mcp-local-hub/internal/api"
)

// respawnFailureWindow is the sliding window inside which crashes
// count toward the quarantine threshold. 30 minutes is the v0.5.0
// spec default (supervisor_state_machine.go references the same
// window for the legacy SM design).
const respawnFailureWindow = 30 * time.Minute

// respawnQuarantineThreshold is the count of failures within
// respawnFailureWindow that triggers quarantine. After this many
// failures the dispatcher stops respawning the daemon until the
// supervisor cold-restarts (which clears the in-memory window).
const respawnQuarantineThreshold = 10

// respawnBackoffStep is the base unit for the exponential backoff
// schedule: 1s, 2s, 4s, 8s, 16s, 32s, then capped at respawnBackoffMax.
const respawnBackoffStep = 1 * time.Second

// respawnBackoffMax caps the exponential backoff so long-running
// degraded states still get a respawn attempt at least once a minute.
const respawnBackoffMax = 60 * time.Second

// computeRespawnBackoff returns the wait duration before the
// `failures`-th respawn attempt. failures=1 → 1s, failures=2 → 2s,
// failures=3 → 4s, ..., capped at respawnBackoffMax. failures<=0
// returns 0 (no backoff).
func computeRespawnBackoff(failures int) time.Duration {
	if failures <= 0 {
		return 0
	}
	// Exponential: 2^(failures-1) * step.
	// failures=1 → 2^0 = 1; failures=7 → 2^6 = 64; ...
	n := failures - 1
	if n > 30 {
		n = 30 // guard against overflow in the bit-shift below
	}
	d := time.Duration(int64(1)<<uint(n)) * respawnBackoffStep
	if d > respawnBackoffMax || d <= 0 {
		return respawnBackoffMax
	}
	return d
}

// isStoppedFn is the per-task "should respawn be suppressed because
// the operator deliberately stopped this daemon?" probe. Production
// re-reads daemon-intent.json on each call so a mid-session
// `mcphub stop` takes effect for the next respawn decision. Tests
// inject a fake function that returns a fixed verdict.
//
// Closes bot v1 P1-1 (PR #230 round 1): without this, the dispatcher
// would auto-respawn daemons immediately after `mcphub stop` killed
// them — making `stop` ineffective while supervisor is running.
type isStoppedFn func(taskName string) bool

// runRespawnDispatcher consumes crash events from crashCh and
// schedules respawn attempts via spawn after an exponential backoff.
// Per-task failure counts use a 30-min sliding window tracked in the
// runtime tracker; at 10 failures the daemon is quarantined (no more
// respawn attempts until supervisor cold-restart, which clears the
// in-memory window).
//
// The dispatcher runs as a long-lived goroutine started by
// runSupervise. It exits when ctx is canceled (graceful shutdown,
// IPC exit, signal). Each scheduled respawn runs in its own sleep
// goroutine to keep the dispatcher loop responsive — a slow spawn
// must NOT block subsequent crash events from being processed.
//
// isStopped (bot v1 P1-1 fix) is consulted both BEFORE recording a
// crash (don't bump the sliding window for operator-initiated stops)
// AND after the backoff sleep elapses (operator may have stopped the
// daemon during the wait — respect that). Production passes a closure
// that re-reads daemon-intent.json each call so mid-session stops
// take effect without supervisor restart.
//
// Spawn failure handling (bot v1 P1-2 fix): a spawn that returns
// non-nil (e.g. cmd.Start failure — no child process exists, so the
// cmd.Wait goroutine never fires and never re-enters the dispatcher
// via crashCh) is treated as a synthetic crash event: the failure
// is recorded against the sliding window via scheduleRespawnAttempt
// called recursively, so backoff + quarantine progression continue
// uninterrupted. Without this, one failed respawn attempt would
// permanently strand the daemon until manual intervention.
//
// Emitted events:
//   - daemon-respawn-scheduled (info): respawn pending in <backoff>s
//   - daemon-respawn-fired (debug): respawn attempt started after backoff
//   - daemon-respawn-spawn-failed (warn): spawn returned error after backoff
//   - daemon-respawn-suppressed-stop-intent (info): operator-stopped daemon
//   - daemon-quarantined (error): 10+ failures in 30-min window
//
// Spec reference: supervisor_state_machine.go transitions
// StRunning + EvChildExit → StBackoffWaiting → StSpawning |
// StQuarantined. This dispatcher implements the same semantics
// without using the formal Transition state machine — a smaller
// scope that unblocks operator-visible auto-respawn for the v0.5.0
// supervisor while a full Transition() wiring (per the
// docs/superpowers/plans/2026-05-20-serena-supervisor-unified.md
// Phase A.2 plan) follows in a separate PR.
func runRespawnDispatcher(
	ctx context.Context,
	crashCh <-chan crashEvent,
	spawn SpawnFunc,
	tracker *DaemonRuntimeTracker,
	events *api.SupervisorEventLog,
	isStopped isStoppedFn,
	statePath string,
) {
	for {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-crashCh:
			if !ok {
				return
			}
			scheduleRespawnAttempt(ctx, ev.Daemon, ev.ExitCode, spawn, tracker, events, isStopped, statePath)
		}
	}
}

// scheduleRespawnAttempt handles one crash event: stop-intent
// suppression check, failure-count accounting, backoff computation,
// then a sleep+spawn goroutine that recursively re-enters this
// function on spawn failure (bot v1 P1-2 fix).
//
// Recursion is bounded by respawnQuarantineThreshold — the sliding
// window count keeps increasing on each failure, eventually crossing
// the threshold and stopping further respawn attempts.
func scheduleRespawnAttempt(
	ctx context.Context,
	d api.SupervisorDaemon,
	exitCode int,
	spawn SpawnFunc,
	tracker *DaemonRuntimeTracker,
	events *api.SupervisorEventLog,
	isStopped isStoppedFn,
	statePath string,
) {
	// Stop-intent check BEFORE recording the crash. An operator-
	// initiated stop (mcphub stop) sets Desired=stopped in
	// daemon-intent.json and then force-kills the process; cmd.Wait
	// reports a non-clean exit which lands here. Suppress respawn
	// AND skip the sliding-window increment so the failure count
	// doesn't drift on operator-initiated stops.
	if isStopped != nil && isStopped(d.TaskName) {
		_ = events.Emit(api.SupervisorEvent{
			Severity: "info",
			Source:   "lifecycle",
			Event:    "daemon-respawn-suppressed-stop-intent",
			TaskName: d.TaskName,
			Body: map[string]any{
				"exit_code": exitCode,
				"reason":    "daemon-intent.json Desired=stopped (operator-initiated)",
			},
		})
		return
	}
	now := time.Now().UTC()
	failures := tracker.RecordCrashAndCountInWindow(d.TaskName, now, respawnFailureWindow)
	if failures >= respawnQuarantineThreshold {
		// Bot v2 P2 (PR #230 round 2): transition tracker state +
		// persist so status/GUI consumers see "quarantined" instead
		// of "Stopped". Without this, the quarantined daemon appears
		// indistinguishable from a normally-stopped one until next
		// supervisor restart.
		tracker.MarkQuarantined(d.TaskName)
		_ = persistDaemonRuntimeTracker(events, tracker, statePath, d.TaskName)
		_ = events.Emit(api.SupervisorEvent{
			Severity: "error",
			Source:   "lifecycle",
			Event:    "daemon-quarantined",
			TaskName: d.TaskName,
			Body: map[string]any{
				"failures_in_30m": failures,
				"reason":          "10+ failures in 30-min sliding window; respawn attempts suspended until supervisor restart",
				"exit_code":       exitCode,
			},
		})
		return
	}
	// Bot v2 P2 (PR #230 round 2): transition tracker state to
	// backoff + persist so status/GUI consumers see "Restarting"
	// during the backoff window. cmd.Wait goroutine already called
	// MarkExited (state=idle), so without this the daemon appears
	// "Stopped" between scheduling and actual respawn.
	tracker.MarkBackoff(d.TaskName)
	_ = persistDaemonRuntimeTracker(events, tracker, statePath, d.TaskName)
	backoff := computeRespawnBackoff(failures)
	_ = events.Emit(api.SupervisorEvent{
		Severity: "info",
		Source:   "lifecycle",
		Event:    "daemon-respawn-scheduled",
		TaskName: d.TaskName,
		Body: map[string]any{
			"failures_in_30m": failures,
			"backoff_seconds": int(backoff / time.Second),
			"exit_code":       exitCode,
		},
	})
	// Sleep + respawn in a separate goroutine so the dispatcher loop
	// stays responsive to other crash events. Cancel-respecting
	// timer ensures graceful shutdown doesn't block on a pending
	// backoff.
	go func(d api.SupervisorDaemon, sleep time.Duration) {
		t := time.NewTimer(sleep)
		defer t.Stop()
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		// Re-check stop intent after backoff: the operator may have
		// run `mcphub stop` during the wait. Suppress in that case.
		if isStopped != nil && isStopped(d.TaskName) {
			_ = events.Emit(api.SupervisorEvent{
				Severity: "info",
				Source:   "lifecycle",
				Event:    "daemon-respawn-suppressed-stop-intent",
				TaskName: d.TaskName,
				Body: map[string]any{
					"reason": "daemon-intent.json Desired=stopped during backoff sleep",
				},
			})
			return
		}
		_ = events.Emit(api.SupervisorEvent{
			Severity: "debug",
			Source:   "lifecycle",
			Event:    "daemon-respawn-fired",
			TaskName: d.TaskName,
		})
		if err := spawn(d); err != nil {
			// Bot v1 P1-2 fix: a failed spawn means no child process
			// was ever created — cmd.Wait goroutine never fires and
			// the dispatcher never re-enters the loop via crashCh.
			// Treat the failure as a synthetic crash and re-enter
			// scheduleRespawnAttempt so the backoff + quarantine
			// progression continues.
			_ = events.Emit(api.SupervisorEvent{
				Severity: "warn",
				Source:   "lifecycle",
				Event:    "daemon-respawn-spawn-failed",
				TaskName: d.TaskName,
				Body: map[string]any{
					"err": err.Error(),
				},
			})
			scheduleRespawnAttempt(ctx, d, -1, spawn, tracker, events, isStopped, statePath)
		}
		// On success, the spawn's own cmd.Wait goroutine will post
		// any future crash event back to the dispatcher via crashCh,
		// so we don't need to track success separately here.
	}(d, backoff)
}
