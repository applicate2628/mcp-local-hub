// Package gui — StatusPoller.
//
// StatusPoller samples statusProvider.Status() on a fixed interval and
// publishes a "daemon-state" event onto the Broadcaster on every
// observed change in (Server, Daemon, State, PID, Port, OrphanPID,
// StalePID, LastResult, JobProtection). On a rising FAILURE edge (a row that newly
// trips api.IsRealFailure(LastResult) or whose State contains "fail") it
// ALSO publishes a "daemon-failed" event for the Dashboard/toast alert
// surface, and on the symmetric FALLING edge (failed -> healthy) a
// "daemon-recovered" all-clear event. Fetch errors are surfaced as "poller-error" events and the
// loop continues on the next tick. Daemons that disappear between samples
// emit a terminal daemon-state event with state="Gone".
//
// Spec: §3.6 (real-time event bus).
// Task 12 lays the pump; Task 13 wires it into `mcphub gui` RunE.
package gui

import (
	"context"
	"strings"
	"time"

	"mcp-local-hub/internal/api"
)

// StatusPoller samples api.Status() on a fixed interval and publishes
// a daemon-state event on every observed change in (Server, Daemon,
// State, PID, Port, OrphanPID, StalePID, JobProtection). The event body matches
// spec §3.6.
//
// The cache is keyed by the composite "<server>/<daemon>" tuple because
// api.Status() returns one row per daemon: a multi-daemon server like
// serena (claude + codex) would otherwise collide on Server alone,
// overwriting the first daemon's row each cycle and emitting spurious
// deltas on the next cycle. An empty Daemon falls back to "default" so
// single-daemon servers stay correct.
type StatusPoller struct {
	status     statusProvider
	events     *Broadcaster
	interval   time.Duration
	last       map[string]api.DaemonStatus // key: "<server>/<daemon>"
	snapshotCh chan<- []api.DaemonStatus   // optional, see SetSnapshotChannel
	errorCh    chan<- error                // optional, see SetErrorChannel
}

// SetSnapshotChannel installs an optional sink that receives the full
// status snapshot on every poll (not just deltas). The tray uses this
// to compute an aggregate TrayState without re-querying api.Status()
// itself, avoiding double work. Send is non-blocking via buffered
// channel + select-default; consumers should make ch buffered = 1
// so a slow consumer drops to "latest snapshot" instead of stalling
// the poller.
//
// Pass nil (or never call) to disable. SetSnapshotChannel is not
// safe to call concurrently with Run; wire it before Run starts.
func (p *StatusPoller) SetSnapshotChannel(ch chan<- []api.DaemonStatus) {
	p.snapshotCh = ch
}

// SetErrorChannel installs an optional sink that receives the fetch
// error on every poll cycle that fails (status.Status() returned err).
// The tray aggregator uses this to map a down supervisor to a degraded
// tray icon (StateError): on the poll-error path poll() early-returns
// BEFORE fanning a snapshot to snapshotCh, so without this signal the
// tray aggregator would never recompute and the icon would FREEZE at
// its last value — a fail-quiet on the operator's primary at-a-glance
// health signal (PR #281 round-2 P2). An empty []DaemonStatus snapshot
// is NOT a usable substitute because Aggregate(empty) == StateHealthy,
// which would paint a green icon over a down supervisor.
//
// Send is non-blocking via buffered channel + select-default; make ch
// buffered = 1 so a slow consumer drops to "latest error" instead of
// stalling the poller, matching the snapshotCh discipline.
//
// Pass nil (or never call) to disable. SetErrorChannel is not safe to
// call concurrently with Run; wire it before Run starts.
func (p *StatusPoller) SetErrorChannel(ch chan<- error) {
	p.errorCh = ch
}

// NewStatusPoller constructs a StatusPoller. It does not start any
// goroutines; call Run(ctx) to begin polling.
func NewStatusPoller(status statusProvider, events *Broadcaster, interval time.Duration) *StatusPoller {
	return &StatusPoller{
		status:   status,
		events:   events,
		interval: interval,
		last:     map[string]api.DaemonStatus{},
	}
}

// boolPtrEqual returns true when two *bool values are value-equal.
// Used by the poller's change-detection key for the tri-state
// JobProtection field. Pointer-equality is wrong because two
// successive api.Status() calls can return distinct *bool pointers
// even when they encode the same value; value-equality is the
// desired semantic (nil == nil, &true == &true, &false == &false,
// &true != &false, nil != &true, nil != &false).
func boolPtrEqual(a, b *bool) bool {
	if a == nil && b == nil {
		return true
	}
	if a == nil || b == nil {
		return false
	}
	return *a == *b
}

// isFailedDaemonState reports whether a daemon row looks failed, using the
// SAME canonical predicate as the tray (cli.isFailedRow / tray.Aggregate): a
// real Task-Scheduler/exit-code failure via api.IsRealFailure(LastResult), OR a
// state string containing "fail" (defensive — deriveState emits "Failed"
// historically and labels like "FailedToLaunch" should keep tripping). Sharing
// api.IsRealFailure keeps the SSE daemon-failed onset on the same gate as the
// tray icon + toast, so the operator never sees one signal without the other.
func isFailedDaemonState(r api.DaemonStatus) bool {
	if api.IsRealFailure(r.LastResult) {
		return true
	}
	return strings.Contains(strings.ToLower(r.State), "fail")
}

// keyFor produces the composite cache / delta key for one DaemonStatus
// row. An empty Daemon field (single-daemon manifests) falls back to
// "default" to match the convention used by the logs adapter and the
// dashboard UI.
func keyFor(r api.DaemonStatus) string {
	d := r.Daemon
	if d == "" {
		d = "default"
	}
	return r.Server + "/" + d
}

func isSerenaBackendLossRow(r api.DaemonStatus) bool {
	return strings.EqualFold(r.Server, "serena")
}

func isConfirmedDeadDaemonRow(r api.DaemonStatus) bool {
	return r.PID == 0 && r.StalePID == 0 && !strings.EqualFold(r.State, "Running")
}

// Run blocks until ctx is canceled. Polls every interval and publishes
// deltas. Fetch errors are surfaced as "poller-error" events and the
// loop continues on the next tick.
func (p *StatusPoller) Run(ctx context.Context) {
	p.poll(ctx)
	t := time.NewTicker(p.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			p.poll(ctx)
		}
	}
}

func (p *StatusPoller) poll(ctx context.Context) {
	_ = ctx // reserved for future per-call cancellation hooks
	rows, err := p.status.Status()
	if err != nil {
		p.events.Publish(Event{Type: "poller-error", Body: map[string]any{"err": err.Error()}})
		// Feed the tray a degraded signal so its icon reflects the
		// down supervisor instead of freezing at the last computed
		// state. Non-blocking drop-stale, same discipline as the
		// snapshot fan-out below: the tray aggregator only needs the
		// latest error to flip to StateError (PR #281 round-2 P2).
		if p.errorCh != nil {
			select {
			case p.errorCh <- err:
			default:
			}
		}
		return
	}
	// Snapshot fan-out: non-blocking send. A slow consumer's old
	// snapshot is dropped in favor of this fresh one; the tray loop
	// is the only consumer today and it only cares about the latest
	// state for icon updates, so drop-stale is the desired behavior.
	if p.snapshotCh != nil {
		select {
		case p.snapshotCh <- rows:
		default:
		}
	}
	seen := map[string]struct{}{}
	for _, r := range rows {
		k := keyFor(r)
		seen[k] = struct{}{}
		prev, ok := p.last[k]
		if ok &&
			prev.State == r.State &&
			prev.PID == r.PID &&
			prev.Port == r.Port &&
			prev.OrphanPID == r.OrphanPID &&
			prev.StalePID == r.StalePID &&
			prev.LastResult == r.LastResult &&
			boolPtrEqual(prev.JobProtection, r.JobProtection) {
			continue
		}
		// Rising-edge failure detection (computed against the OLD row,
		// BEFORE p.last is updated). LastResult joins the delta key above so
		// a fail-in-place — State stays "Running" while LastResult flips
		// 0 -> non-zero — is observed here instead of being silently
		// swallowed by the unchanged-continue. An unknown prev (!ok) reads as
		// non-failed (zero-value State + LastResult=0), so a daemon already
		// failed on first observation still emits once.
		nowFailed := isFailedDaemonState(r)
		failedEdge := nowFailed && !isFailedDaemonState(prev)
		backendLostEdge := isSerenaBackendLossRow(r) && ((ok && prev.PID > 0 && r.PID > 0 && prev.PID != r.PID) || (isConfirmedDeadDaemonRow(r) && !(ok && isConfirmedDeadDaemonRow(prev))))
		// Falling edge: was failed, now healthy — the supervisor's auto-restart
		// (or a manual restart) succeeded. The `ok` guard means a daemon
		// first-seen healthy does NOT spuriously announce a recovery; only a
		// real failed->healthy transition does. (failed->Gone is handled by the
		// removed-rows loop below, not here.)
		recoveredEdge := ok && !nowFailed && isFailedDaemonState(prev)
		p.last[k] = r
		body := map[string]any{
			"server":         r.Server,
			"daemon":         r.Daemon,
			"state":          r.State,
			"pid":            r.PID,
			"port":           r.Port,
			"is_maintenance": r.IsMaintenance,
			"orphan_pid":     r.OrphanPID,
		}
		// job_protection emits as bool when explicitly probed. Initial
		// nil rows omit the field; known->nil transitions emit JSON
		// null so the frontend delta merge clears a stale false badge.
		// Frontend renders the warning badge only on explicit false;
		// nil/true = no badge. This matches the tri-state contract
		// documented at api.DaemonStatus.JobProtection. Closes
		// consultant strategic concern #1 on PR #241: the SSE delta is
		// the steady-state observability path that converts the
		// fallback's non-fatal log entry into an operator-visible
		// warning.
		if r.JobProtection != nil {
			body["job_protection"] = *r.JobProtection
		} else if ok && prev.JobProtection != nil {
			body["job_protection"] = nil
		}
		p.events.Publish(Event{
			Type: "daemon-state",
			Body: body,
		})
		// On a rising failure edge, publish a dedicated daemon-failed event
		// so the Dashboard / toast surface can alert without re-deriving the
		// failure predicate from the daemon-state stream. Carries the exit
		// code (last_result) so the toast can name it. Edge-triggered (not
		// level-triggered) so a daemon that stays failed across cycles does
		// not spam — the unchanged-continue above suppresses repeat ticks,
		// and a falling edge (recovery) is intentionally silent here.
		if failedEdge {
			p.events.Publish(Event{
				Type: "daemon-failed",
				Body: map[string]any{
					"server":      r.Server,
					"daemon":      r.Daemon,
					"state":       r.State,
					"last_result": r.LastResult,
					"pid":         r.PID,
					"port":        r.Port,
				},
			})
		}
		if backendLostEdge {
			p.events.Publish(Event{
				Type: "daemon-backend-lost",
				Body: map[string]any{
					"server": r.Server,
					"daemon": r.Daemon,
					"port":   r.Port,
					"state":  r.State,
				},
			})
		}
		// Symmetric falling edge: announce the recovery so the operator who
		// got the danger toast also sees the all-clear (C4 "auto-restart done"
		// notification). Edge-triggered, so it fires exactly once per recovery.
		if recoveredEdge {
			p.events.Publish(Event{
				Type: "daemon-recovered",
				Body: map[string]any{
					"server": r.Server,
					"daemon": r.Daemon,
					"state":  r.State,
					"pid":    r.PID,
					"port":   r.Port,
				},
			})
		}
	}
	// Removed rows: key in last but not in this fetch.
	for k := range p.last {
		if _, still := seen[k]; !still {
			gone := p.last[k]
			delete(p.last, k)
			p.events.Publish(Event{
				Type: "daemon-state",
				Body: map[string]any{
					"server": gone.Server,
					"daemon": gone.Daemon,
					"state":  "Gone",
				},
			})
			if isSerenaBackendLossRow(gone) {
				p.events.Publish(Event{
					Type: "daemon-backend-lost",
					Body: map[string]any{
						"server": gone.Server,
						"daemon": gone.Daemon,
						"port":   gone.Port,
						"state":  "Gone",
					},
				})
			}
		}
	}
}
