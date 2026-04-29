// Package gui — StatusPoller.
//
// StatusPoller samples statusProvider.Status() on a fixed interval and
// publishes a "daemon-state" event onto the Broadcaster on every
// observed change in (Server, Daemon, State, PID, Port). Fetch errors
// are surfaced as "poller-error" events and the loop continues on the
// next tick. Daemons that disappear between samples emit a terminal
// daemon-state event with state="Gone".
//
// Spec: §3.6 (real-time event bus).
// Task 12 lays the pump; Task 13 wires it into `mcphub gui` RunE.
package gui

import (
	"context"
	"time"

	"mcp-local-hub/internal/api"
)

// StatusPoller samples api.Status() on a fixed interval and publishes
// a daemon-state event on every observed change in (Server, Daemon,
// State, PID, Port). The event body matches spec §3.6.
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
		if ok && prev.State == r.State && prev.PID == r.PID && prev.Port == r.Port {
			continue
		}
		p.last[k] = r
		p.events.Publish(Event{
			Type: "daemon-state",
			Body: map[string]any{
				"server":         r.Server,
				"daemon":         r.Daemon,
				"state":          r.State,
				"pid":            r.PID,
				"port":           r.Port,
				"is_maintenance": r.IsMaintenance,
			},
		})
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
		}
	}
}
