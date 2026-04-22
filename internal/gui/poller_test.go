package gui

import (
	"context"
	"testing"
	"time"

	"mcp-local-hub/internal/api"
)

type scriptedStatus struct {
	frames [][]api.DaemonStatus
	idx    int
}

func (s *scriptedStatus) Status() ([]api.DaemonStatus, error) {
	if s.idx >= len(s.frames) {
		return s.frames[len(s.frames)-1], nil
	}
	out := s.frames[s.idx]
	s.idx++
	return out, nil
}

func TestPoller_EmitsDeltaOnStateChange(t *testing.T) {
	frames := [][]api.DaemonStatus{
		{{Server: "memory", State: "Running", Port: 9123}},
		{{Server: "memory", State: "Stopped", Port: 9123}},
	}
	status := &scriptedStatus{frames: frames}
	b := NewBroadcaster()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch := b.Subscribe(ctx)

	p := NewStatusPoller(status, b, 50*time.Millisecond)
	go p.Run(ctx)

	seen := map[string]int{}
	deadline := time.After(2 * time.Second)
	for seen["Stopped"] < 1 {
		select {
		case ev := <-ch:
			if ev.Type == "daemon-state" {
				s, _ := ev.Body["state"].(string)
				seen[s]++
			}
		case <-deadline:
			t.Fatalf("never saw 'Stopped' delta; seen=%v", seen)
		}
	}
}

// TestPoller_DistinctDaemonsDoNotCollide regression-guards the serena-style
// multi-daemon scenario: api.Status() returns one row per daemon, so a cache
// keyed by Server alone would overwrite the first daemon's row with the
// second each cycle and emit spurious deltas forever. With the composite
// "<server>/<daemon>" key, the first cycle inserts both rows (two Running
// deltas) and the second cycle emits exactly one Stopped delta for codex.
func TestPoller_DistinctDaemonsDoNotCollide(t *testing.T) {
	frames := [][]api.DaemonStatus{
		{
			{Server: "serena", Daemon: "claude", State: "Running", Port: 9121},
			{Server: "serena", Daemon: "codex", State: "Running", Port: 9122},
		},
		{
			{Server: "serena", Daemon: "claude", State: "Running", Port: 9121},
			{Server: "serena", Daemon: "codex", State: "Stopped", Port: 9122},
		},
	}
	status := &scriptedStatus{frames: frames}
	b := NewBroadcaster()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch := b.Subscribe(ctx)

	p := NewStatusPoller(status, b, 50*time.Millisecond)
	go p.Run(ctx)

	// First cycle: 2 inserts (claude, codex). Second cycle: 1 delta (codex
	// Stopped). Third cycle onward: no deltas — the cache now tracks both
	// daemons independently, so the second frame (repeated on replay) is a
	// no-op. Collect exactly the first three deltas.
	deltas := map[string]map[string]int{} // server -> state -> count
	deadline := time.After(2 * time.Second)
	for total := 0; total < 3; {
		select {
		case ev := <-ch:
			if ev.Type != "daemon-state" {
				continue
			}
			srv, _ := ev.Body["server"].(string)
			st, _ := ev.Body["state"].(string)
			if deltas[srv] == nil {
				deltas[srv] = map[string]int{}
			}
			deltas[srv][st]++
			total++
		case <-deadline:
			t.Fatalf("never saw expected deltas: %+v", deltas)
		}
	}
	if deltas["serena"]["Stopped"] != 1 {
		t.Errorf("expected exactly one Stopped delta, got %v", deltas)
	}
	if deltas["serena"]["Running"] != 2 {
		t.Errorf("expected two Running deltas (claude, codex initial inserts), got %v", deltas)
	}
}
