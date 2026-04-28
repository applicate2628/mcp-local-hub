package cli

import (
	"context"
	"testing"
	"time"

	"mcp-local-hub/internal/api"
	"mcp-local-hub/internal/tray"
)

// TestAggregateTrayState_ForwardsOnChange asserts the aggregator
// forwards a TrayState exactly once when the aggregate transitions
// (Healthy → Partial → Healthy). Coalescing identical adjacent
// states is the whole point — a regression that forwards every
// snapshot regardless would cause SetIcon to fire on every poll
// cycle, flickering the tray.
func TestAggregateTrayState_ForwardsOnChange(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	snaps := make(chan []api.DaemonStatus, 8)
	out := make(chan tray.TrayState, 8)
	go aggregateTrayState(ctx, snaps, out)

	// Sequence: Healthy, Healthy (dup), Partial, Partial (dup),
	// Healthy. Expect: Healthy, Partial, Healthy — exactly 3
	// forwards, dropping both consecutive duplicates.
	healthy := []api.DaemonStatus{{Server: "x", State: "Running"}}
	partial := []api.DaemonStatus{{Server: "x", State: "Running"}, {Server: "y", State: "Stopped"}}
	for _, snap := range [][]api.DaemonStatus{healthy, healthy, partial, partial, healthy} {
		snaps <- snap
	}

	got := []tray.TrayState{}
	timeout := time.After(2 * time.Second)
	for len(got) < 3 {
		select {
		case s := <-out:
			got = append(got, s)
		case <-timeout:
			t.Fatalf("timed out after %d forwards: %v", len(got), got)
		}
	}
	want := []tray.TrayState{tray.StateHealthy, tray.StatePartial, tray.StateHealthy}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("forward %d: got %v, want %v", i, got[i], want[i])
		}
	}
	// And no extra forward queued behind: drain non-blockingly.
	select {
	case extra := <-out:
		t.Errorf("unexpected 4th forward: %v (regression — duplicate states should coalesce)", extra)
	case <-time.After(100 * time.Millisecond):
		// good
	}
}

// TestAggregateTrayState_ExitsOnCtxCancel asserts the goroutine
// returns when ctx is canceled, even if more snapshots are in
// flight. Without this, `go aggregateTrayState` in cli/gui.go
// would leak a goroutine on every `mcphub gui` invocation.
func TestAggregateTrayState_ExitsOnCtxCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	snaps := make(chan []api.DaemonStatus, 1)
	out := make(chan tray.TrayState, 1)
	done := make(chan struct{})
	go func() {
		aggregateTrayState(ctx, snaps, out)
		close(done)
	}()
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("aggregateTrayState did not return within 2s of ctx cancel")
	}
}

// TestAggregateTrayState_ExitsOnSnapshotChannelClose asserts the
// goroutine returns when the producer closes the snapshot channel.
// StatusPoller never closes it during normal operation, but a
// future refactor that does (e.g. test-driven shutdown) shouldn't
// leak the goroutine.
func TestAggregateTrayState_ExitsOnSnapshotChannelClose(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	snaps := make(chan []api.DaemonStatus)
	out := make(chan tray.TrayState, 1)
	done := make(chan struct{})
	go func() {
		aggregateTrayState(ctx, snaps, out)
		close(done)
	}()
	close(snaps)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("aggregateTrayState did not return within 2s of snapshot channel close")
	}
}
