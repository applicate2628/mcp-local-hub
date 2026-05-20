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
// Emitted events:
//   - daemon-respawn-scheduled (info): respawn pending in <backoff>s
//   - daemon-quarantined (error): 10+ failures in 30-min window
//   - daemon-respawn-fired (debug): respawn attempt started after backoff
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
) {
	for {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-crashCh:
			if !ok {
				return
			}
			now := time.Now().UTC()
			failures := tracker.RecordCrashAndCountInWindow(ev.Daemon.TaskName, now, respawnFailureWindow)
			if failures >= respawnQuarantineThreshold {
				_ = events.Emit(api.SupervisorEvent{
					Severity: "error",
					Source:   "lifecycle",
					Event:    "daemon-quarantined",
					TaskName: ev.Daemon.TaskName,
					Body: map[string]any{
						"failures_in_30m": failures,
						"reason":          "10+ failures in 30-min sliding window; respawn attempts suspended until supervisor restart",
						"exit_code":       ev.ExitCode,
					},
				})
				continue
			}
			backoff := computeRespawnBackoff(failures)
			_ = events.Emit(api.SupervisorEvent{
				Severity: "info",
				Source:   "lifecycle",
				Event:    "daemon-respawn-scheduled",
				TaskName: ev.Daemon.TaskName,
				Body: map[string]any{
					"failures_in_30m": failures,
					"backoff_seconds": int(backoff / time.Second),
					"exit_code":       ev.ExitCode,
				},
			})
			// Sleep + respawn in a separate goroutine so the
			// dispatcher loop stays responsive to other crash events.
			// Cancel-respecting timer ensures graceful shutdown
			// doesn't block on a pending backoff.
			go func(d api.SupervisorDaemon, sleep time.Duration) {
				t := time.NewTimer(sleep)
				defer t.Stop()
				select {
				case <-ctx.Done():
					return
				case <-t.C:
				}
				_ = events.Emit(api.SupervisorEvent{
					Severity: "debug",
					Source:   "lifecycle",
					Event:    "daemon-respawn-fired",
					TaskName: d.TaskName,
				})
				// spawn() handles its own audit events
				// (daemon-spawned, daemon-spawn-failed); errors
				// are swallowed here because the audit log already
				// has them and the dispatcher cannot meaningfully
				// recover from a spawn failure beyond the next
				// crash event from the failed child (if it spawned
				// at all).
				_ = spawn(d)
			}(ev.Daemon, backoff)
		}
	}
}
