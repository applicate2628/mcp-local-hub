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

// TestPoller_EmitsDeltaOnOrphanPIDChange is the regression guard
// for the Windows post-create orphan SSE-delta path. When the
// supervisor's best-effort kill of a Windows post-create orphan
// fails and supervise.go records OrphanPID via
// MarkSpawnFailedPreservePID, the SSE delta MUST fire (so the GUI
// Dashboard clears or updates the Orphan PID card immediately
// instead of waiting for the next full-status poll).
//
// Without OrphanPID in the change-detection key, the daemon could
// transition State="Running" -> State="Running" + OrphanPID=N with
// no delta fired - the Dashboard would keep showing zero OrphanPID
// (or stale OrphanPID after recovery) until the operator forced a
// page refresh.
//
// The delta body MUST also include "orphan_pid" so the frontend
// delta-merge can apply the new value (or zero) to its DaemonStatus
// shape.
func TestPoller_EmitsDeltaOnOrphanPIDChange(t *testing.T) {
	frames := [][]api.DaemonStatus{
		{{Server: "memory", State: "Stopped", Port: 9123}},
		{{Server: "memory", State: "Stopped", Port: 9123, OrphanPID: 12345}},
		{{Server: "memory", State: "Running", Port: 9123, PID: 67890}},
	}
	status := &scriptedStatus{frames: frames}
	b := NewBroadcaster()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch := b.Subscribe(ctx)

	p := NewStatusPoller(status, b, 50*time.Millisecond)
	go p.Run(ctx)

	// Expected delta sequence:
	//   1. Initial insert from frame 1 (orphan_pid=0)
	//   2. Frame 1 -> frame 2 transition: State unchanged (Stopped)
	//      but OrphanPID 0 -> 12345 MUST emit a delta with
	//      orphan_pid=12345. Without OrphanPID in change-detection,
	//      this delta is suppressed and Dashboard never shows 12345.
	//   3. Frame 2 -> frame 3 transition: State changed
	//      (Stopped -> Running), OrphanPID 12345 -> 0 (cleared on
	//      MarkSpawned via clearOrphanPIDLocked). MUST emit a delta
	//      with orphan_pid=0 so Dashboard clears the stale row.
	var orphanDeltas []int
	deadline := time.After(2 * time.Second)
	for len(orphanDeltas) < 3 {
		select {
		case ev := <-ch:
			if ev.Type != "daemon-state" {
				continue
			}
			raw, ok := ev.Body["orphan_pid"]
			if !ok {
				t.Fatalf("daemon-state event body missing orphan_pid field; body=%v", ev.Body)
			}
			orphan, _ := raw.(int)
			orphanDeltas = append(orphanDeltas, orphan)
		case <-deadline:
			t.Fatalf("did not observe 3 orphan_pid deltas; got=%v (regression: OrphanPID change suppressed because change-detection key omits OrphanPID, OR orphan_pid missing from event body)", orphanDeltas)
		}
	}

	if orphanDeltas[0] != 0 {
		t.Errorf("delta[0] (initial insert) orphan_pid = %d, want 0", orphanDeltas[0])
	}
	if orphanDeltas[1] != 12345 {
		t.Errorf("delta[1] (orphan-set transition) orphan_pid = %d, want 12345 (regression: change-detection key missing OrphanPID)", orphanDeltas[1])
	}
	if orphanDeltas[2] != 0 {
		t.Errorf("delta[2] (orphan-cleared transition) orphan_pid = %d, want 0 (regression: cleared OrphanPID not surfaced in SSE)", orphanDeltas[2])
	}
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
