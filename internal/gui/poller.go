// Package gui — StatusPoller.
//
// StatusPoller samples statusProvider.Status() on a fixed interval and
// publishes a "daemon-state" event onto the Broadcaster on every
// observed change in (Server, State, PID, Port). Fetch errors are
// surfaced as "poller-error" events and the loop continues on the next
// tick. Servers that disappear between samples emit a terminal
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
// a daemon-state event on every observed change in (Server, State, PID,
// Port). The event body matches spec §3.6.
type StatusPoller struct {
	status   statusProvider
	events   *Broadcaster
	interval time.Duration
	last     map[string]api.DaemonStatus
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
	seen := map[string]struct{}{}
	for _, r := range rows {
		seen[r.Server] = struct{}{}
		prev, ok := p.last[r.Server]
		if ok && prev.State == r.State && prev.PID == r.PID && prev.Port == r.Port {
			continue
		}
		p.last[r.Server] = r
		p.events.Publish(Event{
			Type: "daemon-state",
			Body: map[string]any{
				"server": r.Server,
				"state":  r.State,
				"pid":    r.PID,
				"port":   r.Port,
			},
		})
	}
	// Removed rows: server in last but not in this fetch.
	for name := range p.last {
		if _, still := seen[name]; !still {
			delete(p.last, name)
			p.events.Publish(Event{
				Type: "daemon-state",
				Body: map[string]any{"server": name, "state": "Gone"},
			})
		}
	}
}
