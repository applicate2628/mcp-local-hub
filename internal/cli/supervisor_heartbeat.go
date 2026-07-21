package cli

import (
	"os"
	"time"

	"mcp-local-hub/internal/api"
)

// supervisorHeartbeatInterval is the cadence of the `supervisor-heartbeat`
// liveness row.
//
// WHY A HEARTBEAT EXISTS. Commit 835ee3e4 (#566) correctly stopped
// auditing read-only status polls, removing ~96% of the event-log rows.
// The correct fix had an incorrect side effect: the supervisor now emits
// NOTHING for hours at a time, so a healthy-and-quiet supervisor is
// indistinguishable on disk from a dead one. A periodic row inverts that —
// the ABSENCE of heartbeats becomes positive evidence of death, with a
// bounded detection latency, rather than an unfalsifiable silence.
//
// WHY 60 SECONDS — argued against the two constraints that bound it.
//
// Against the 10 MB rotation budget: one heartbeat row is ~290 bytes
// (envelope + four body fields + the always-present gap_seconds; the
// delayed/attribution fields appear only on an anomalous beat), so 60 s
// costs 1440 rows/day ≈ 420 KB/day.
// The active log alone therefore holds ~24 days of continuous liveness
// history, and ~48 days across the active+.1 pair. The forensic
// investigation that motivated this work needed a 42-hour window; 24 days
// clears it by more than an order of magnitude. For scale, this is ~4% of
// the ~1400 rows/HOUR the log carried BEFORE #566 removed the poll flood —
// heartbeats restore continuous liveness evidence at a small fraction of
// the row rate that was previously considered tolerable.
//
// Against detection latency: the observed death sessions include 19 s and
// 59 s lifetimes, and the `\mcp-local-hub-liveness` task relaunches a dead
// supervisor at a ~60 s cadence. An interval materially longer than that
// (5 min was considered) would let an entire birth-death-relaunch cycle
// fall inside one silent gap, blurring several sessions into one and
// defeating the "when did it die" question. Going materially shorter
// (10 s) buys no resolution the 60 s liveness cadence can use, while
// costing ~2.2 MB/day and rotating the forensic window away every ~4 days.
//
// STATED DETECTION LATENCY. The first heartbeat is emitted immediately at
// startup, before the ticker, so even a 19-second session leaves exactly
// one heartbeat. A death is therefore localized to the ≤60 s window
// following the last observed heartbeat, and is conclusive after two
// consecutive missed beats (≤120 s) — the second beat absorbs a beat merely
// DELAYED by event-log flock contention (emits block rather than drop; see
// the Emit-mode note below) without a false death call.
const supervisorHeartbeatInterval = 60 * time.Second

// runSupervisorHeartbeat emits a `supervisor-heartbeat` row immediately and
// then every interval until done closes. It is decision-inert: nothing in
// the restart, backoff, quarantine, or spawn paths reads it, so a wrong
// number here can only produce a wrong dashboard, never a control-flow
// change.
//
// interval is a parameter rather than a direct read of
// supervisorHeartbeatInterval so tests can drive many beats quickly without
// a 60-second wall clock.
func runSupervisorHeartbeat(
	done <-chan struct{},
	events *api.SupervisorEventLog,
	tracker *DaemonRuntimeTracker,
	sinkPath string,
	startedAt time.Time,
	interval time.Duration,
) {
	if events == nil || interval <= 0 {
		return
	}

	// Gap accounting. A heartbeat whose ABSENCE means death must be able to
	// explain a hole it survived, otherwise a stalled beat is indistinguishable
	// from the death it is supposed to detect — trading one ambiguity for
	// another. lastBeatAt anchors the observed gap; lastEmitBlock attributes it.
	var lastBeatAt time.Time
	var lastEmitBlock time.Duration

	emit := func() {
		now := time.Now().UTC()
		daemonCount, runningCount := supervisorDaemonCounts(tracker)
		body := map[string]any{
			"pid":                  os.Getpid(),
			"uptime_seconds":       int64(time.Since(startedAt).Seconds()),
			"daemon_count":         daemonCount,
			"running_daemon_count": runningCount,
		}

		if !lastBeatAt.IsZero() {
			gap := now.Sub(lastBeatAt)
			// Always present after the first beat so a reader never has to
			// diff timestamps by hand to see the shape of the series.
			body["gap_seconds"] = int64(gap.Seconds())
			if gap > interval+interval/2 {
				// The beat is LATE. Say so explicitly rather than leaving the
				// operator (or a future me) to infer a phantom outage from a
				// hole in the series.
				body["beat_delayed"] = true
				body["expected_interval_seconds"] = int64(interval.Seconds())
			}
			// Attribution: how long the PREVIOUS emit was parked inside the
			// event log. This is what distinguishes "the log was wedged" from
			// "the process was descheduled/suspended" — the gap alone cannot.
			// Reported only when it actually explains something.
			if lastEmitBlock > interval/2 {
				body["previous_emit_block_ms"] = lastEmitBlock.Milliseconds()
			}
		}
		// Surface the open-time-rotation-only residual documented on
		// api.RotateSupervisorStderrSinkIfOversize: a session that writes
		// past the ceiling is reported rather than growing silently.
		if sinkPath != "" && api.SupervisorStderrSinkOversize(sinkPath) {
			body["stderr_sink_oversize"] = true
		}
		// Blocking Emit, deliberately — NOT TryEmit. TryEmit is lossy on
		// flock contention (supervisor_events.go documents it as the sole
		// best-effort path), and a DROPPED heartbeat is indistinguishable
		// from a dead supervisor: it would manufacture exactly the false
		// death signal this row exists to make reliable. Blocking rides out
		// momentary contention and merely delays a beat. A permanently
		// wedged event-log flock would silence every other row too, so it is
		// not a failure mode the heartbeat alone should trade accuracy for.
		emitStart := time.Now()
		_ = events.Emit(api.SupervisorEvent{
			Severity: api.SupervisorEventSeverityInfo,
			Source:   api.SupervisorEventSourceLifecycle,
			Event:    "supervisor-heartbeat",
			Body:     body,
		})
		// Measured AFTER the emit returns, so it is carried by the NEXT beat —
		// a beat cannot report how long its own write blocked.
		lastEmitBlock = time.Since(emitStart)
		// Anchored to the PRE-emit instant so gap_seconds matches the deltas an
		// operator reads off the rows' own `ts` fields (marshalled at emit entry).
		lastBeatAt = now
	}

	// Beat immediately so a session shorter than one interval still proves
	// it existed — the 19 s and 59 s sessions in the forensic window would
	// otherwise leave no heartbeat at all and be indistinguishable from a
	// supervisor that never started.
	emit()

	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-done:
			return
		case <-ticker.C:
			emit()
		}
	}
}

// supervisorDaemonCounts returns (total tracked daemons, daemons in the
// running state). Reads the tracker's own snapshot so it observes the same
// state the persisted supervisor-state.json is derived from.
func supervisorDaemonCounts(tracker *DaemonRuntimeTracker) (int, int) {
	snapshot := tracker.Snapshot()
	running := 0
	for _, entry := range snapshot {
		if entry.State == daemonRuntimeStateRunning {
			running++
		}
	}
	return len(snapshot), running
}
