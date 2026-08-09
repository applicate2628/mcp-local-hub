package gui

import (
	"context"
	"testing"
	"time"

	"mcp-local-hub/internal/api"
)

// TestPoller_EmitsDeltaOnSpawnHoldChange guards the pre-spawn existence-gate
// (P1.1) fields through the SSE delta path. TWO distinct traps live here, and
// both fail SILENTLY while leaving a WRONG message on the operator's screen:
//
//  1. The delta KEY. A daemon held across consecutive polls can differ ONLY in
//     spawn_hold_reason / spawn_hold_path. If those are not part of the
//     unchanged-continue comparison in poll(), the delta is swallowed and the
//     Dashboard never learns the cause changed. This is the same trap
//     TestPoller_EmitsDeltaOnJobProtectionChange guards for job_protection.
//
//  2. The CLEAR. The frontend delta merge is a shallow spread over the prior
//     row, so an OMITTED key keeps its stale value. The body must therefore
//     ALWAYS carry both keys. Otherwise, after the operator reinstalls mcphub,
//     the "program file missing" banner stays on screen forever even though
//     everything is running again — the failure mode most likely to make an
//     operator distrust the product right after they fixed the problem.
func TestPoller_EmitsDeltaOnSpawnHoldChange(t *testing.T) {
	const pathA = `C:\Users\dev\.local\bin\mcphub.exe`
	const pathB = `D:\relocated\bin\mcphub.exe`

	frames := [][]api.DaemonStatus{
		// Frame 1: healthy.
		{{Server: "memory", State: "Running", Port: 9123, PID: 42}},
		// Frame 2: the binary vanished — the gate holds.
		{{Server: "memory", State: "Stopped", Port: 9123,
			SpawnHoldReason: "missing-binary", SpawnHoldPath: pathA}},
		// Frame 3: still held, but on a DIFFERENT path (the intent was
		// rewritten). Every other field is identical to frame 2, so ONLY
		// spawn_hold_path distinguishes them: this frame is swallowed unless
		// the field is part of the delta key.
		{{Server: "memory", State: "Stopped", Port: 9123,
			SpawnHoldReason: "missing-binary", SpawnHoldPath: pathB}},
		// Frame 4: the operator reinstalled — running again, hold cleared.
		{{Server: "memory", State: "Running", Port: 9123, PID: 77}},
	}
	status := &scriptedStatus{frames: frames}
	b := newEphemeralBroadcaster(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch := b.Subscribe(ctx)

	p := NewStatusPoller(status, b, 50*time.Millisecond)
	go p.Run(ctx)

	type holdDelta struct {
		reason  string
		path    string
		present bool
	}
	var got []holdDelta
	deadline := time.After(3 * time.Second)
	for len(got) < 4 {
		select {
		case ev := <-ch:
			if ev.Type != "daemon-state" {
				continue
			}
			rawReason, present := ev.Body["spawn_hold_reason"]
			d := holdDelta{present: present}
			if present {
				d.reason, _ = rawReason.(string)
				d.path, _ = ev.Body["spawn_hold_path"].(string)
			}
			got = append(got, d)
		case <-deadline:
			t.Fatalf("timed out after %d daemon-state deltas, want 4: %+v", len(got), got)
		}
	}

	if got[1].reason != "missing-binary" || got[1].path != pathA {
		t.Fatalf("delta[1] = %+v, want reason=missing-binary path=%s (the hold never reached the Dashboard)", got[1], pathA)
	}
	if got[2].path != pathB {
		t.Fatalf("delta[2] path = %q, want %q — a path-only change was swallowed by the unchanged-continue; spawn_hold_path MUST be part of the delta key", got[2].path, pathB)
	}
	if !got[3].present {
		t.Fatal("delta[3] omitted spawn_hold_reason; the key must ALWAYS be sent so the frontend shallow merge CLEARS the stale warning after a reinstall")
	}
	if got[3].reason != "" {
		t.Fatalf("delta[3] reason = %q, want empty (a recovered daemon must clear the banner)", got[3].reason)
	}
}
