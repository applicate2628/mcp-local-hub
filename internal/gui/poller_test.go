package gui

import (
	"context"
	"strings"
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

// TestPoller_EmitsDeltaOnJobProtectionChange regression-guards the
// SSE delta path for the v0.5.x per-spawn Job Object protection
// state surface (PR #242 closes consultant strategic concern #1 on
// PR #241). The tri-state contract: nil = unknown (no badge),
// &true = explicit protected (no badge), &false = explicit fallback
// fired (warning badge in GUI Dashboard).
//
// Without JobProtection in the change-detection key, a transition
// from nil → &true (successful first-time spawn) would not emit a
// delta, and the Dashboard would never receive the initial
// confirmation that the daemon is protected. Without it being in
// the event body, the delta would fire but the frontend has no way
// to update its render state.
//
// The boolPtrEqual helper exists precisely so this test does not
// race on pointer identity: even though two scripted frames may
// construct distinct *bool pointers for the same value, the
// poller must treat them as equal.
func TestPoller_EmitsDeltaOnJobProtectionChange(t *testing.T) {
	tru, fal := true, false
	frames := [][]api.DaemonStatus{
		// Frame 1: legacy/unknown — JobProtection nil.
		{{Server: "memory", State: "Running", Port: 9123, PID: 42, JobProtection: nil}},
		// Frame 2: same spawn, JobProtection explicitly set to true
		// (e.g. the supervisor's MarkJobProtection wrote the value
		// after NewKillOnCloseJob succeeded).
		{{Server: "memory", State: "Running", Port: 9123, PID: 42, JobProtection: &tru}},
		// Frame 3: respawn under fallback — JobProtection flips to
		// false (Job-create failed; warning badge should fire).
		{{Server: "memory", State: "Running", Port: 9123, PID: 99, JobProtection: &fal}},
	}
	status := &scriptedStatus{frames: frames}
	b := NewBroadcaster()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch := b.Subscribe(ctx)

	p := NewStatusPoller(status, b, 50*time.Millisecond)
	go p.Run(ctx)

	// Expected: 3 distinct daemon-state deltas.
	//   delta[0] (insert, frame 1): no job_protection field in body
	//            (nil → omitted, matches frontend "unknown" rendering)
	//   delta[1] (transition, frame 2): job_protection=true in body
	//            (Dashboard clears any prior warning badge)
	//   delta[2] (transition, frame 3): job_protection=false in body
	//            (Dashboard fires the warning badge)
	type jpDelta struct {
		present bool
		value   bool
	}
	var got []jpDelta
	deadline := time.After(2 * time.Second)
	for len(got) < 3 {
		select {
		case ev := <-ch:
			if ev.Type != "daemon-state" {
				continue
			}
			raw, present := ev.Body["job_protection"]
			d := jpDelta{present: present}
			if present {
				val, ok := raw.(bool)
				if !ok {
					t.Fatalf("delta job_protection field is not bool: %T %v", raw, raw)
				}
				d.value = val
			}
			got = append(got, d)
		case <-deadline:
			t.Fatalf("did not observe 3 job_protection deltas; got=%v (regression: change-detection key omits JobProtection OR boolPtrEqual broken OR body omits the field)", got)
		}
	}

	if got[0].present {
		t.Errorf("delta[0] (insert with nil JobProtection): job_protection field present in body; want omitted (frontend treats unknown as no badge)")
	}
	if !got[1].present || got[1].value != true {
		t.Errorf("delta[1] (nil→&true transition): job_protection = %+v, want present=true value=true (Dashboard should clear stale warning badge)", got[1])
	}
	if !got[2].present || got[2].value != false {
		t.Errorf("delta[2] (&true→&false transition): job_protection = %+v, want present=true value=false (Dashboard should fire warning badge — regression: fallback-fired daemon shows no badge)", got[2])
	}
}

func TestPoller_EmitsJobProtectionClearOnFalseToUnknown(t *testing.T) {
	fal := false
	frames := [][]api.DaemonStatus{
		{{Server: "memory", State: "Running", Port: 9123, PID: 42, JobProtection: &fal}},
		{{Server: "memory", State: "Running", Port: 9123, PID: 42, JobProtection: nil}},
	}
	status := &scriptedStatus{frames: frames}
	b := NewBroadcaster()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch := b.Subscribe(ctx)

	p := NewStatusPoller(status, b, 50*time.Millisecond)
	go p.Run(ctx)

	type jpDelta struct {
		present bool
		raw     any
	}
	var got []jpDelta
	deadline := time.After(2 * time.Second)
	for len(got) < 2 {
		select {
		case ev := <-ch:
			if ev.Type != "daemon-state" {
				continue
			}
			raw, present := ev.Body["job_protection"]
			got = append(got, jpDelta{present: present, raw: raw})
		case <-deadline:
			t.Fatalf("did not observe 2 job_protection deltas; got=%v", got)
		}
	}

	if !got[0].present || got[0].raw != false {
		t.Errorf("delta[0] job_protection = %+v, want present=true value=false", got[0])
	}
	if !got[1].present || got[1].raw != nil {
		t.Errorf("delta[1] job_protection = %+v, want present=true value=nil to clear stale frontend false", got[1])
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

// TestPoller_SupervisorDownEmitsPollerErrorNotStaleDeltas is the P1
// regression guard for v0.6 Workstream B (§3.1). When the supervisor
// IPC is unreachable, the production poller (Server.StatusProvider() →
// healthBackend.DaemonStatusSnapshot) must surface api.ErrSupervisorDown
// as a `poller-error` event and emit NO `daemon-state` deltas.
//
// Before the fix, the poller used gui.RealStatusProvider{} →
// api.NewAPI().Status() → statusInternal, which fell back to the legacy
// scheduler scan on ErrSupervisorIPCUnavailable. A down supervisor then
// produced stale scheduler rows that the poller published as
// `daemon-state` deltas; the frontend's onDelta cleared the degraded
// banner (setError(null)) and painted those stale failed/Restarting
// cards — re-introducing the exact false-negative the /api/status path
// removes, just on the SSE channel. Routing the poller through the
// fail-loud snapshot closes that second channel: a `poller-error`
// carries the degraded signal and never clears the Dashboard banner.
func TestPoller_SupervisorDownEmitsPollerErrorNotStaleDeltas(t *testing.T) {
	fake := &fakeHealth{returnDaemonErr: api.ErrSupervisorDown}
	s := NewServer(Config{Port: 9125, Version: "test", PID: 1})
	s.health = fake

	b := NewBroadcaster()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch := b.Subscribe(ctx)

	p := NewStatusPoller(s.StatusProvider(), b, 50*time.Millisecond)
	go p.Run(ctx)

	// Wait for the first poll cycle to surface a poller-error event;
	// assert no daemon-state delta is ever observed alongside it.
	deadline := time.After(2 * time.Second)
	var sawPollerErr bool
	for !sawPollerErr {
		select {
		case ev := <-ch:
			switch ev.Type {
			case "daemon-state":
				t.Fatalf("supervisor-down poller emitted a daemon-state delta (stale-card masking regression); body=%v", ev.Body)
			case "poller-error":
				errStr, _ := ev.Body["err"].(string)
				if !strings.Contains(errStr, api.ErrSupervisorDown.Error()) {
					t.Errorf("poller-error body err = %q, want it to carry ErrSupervisorDown (%q)", errStr, api.ErrSupervisorDown.Error())
				}
				sawPollerErr = true
			}
		case <-deadline:
			t.Fatal("never observed a poller-error event for a down supervisor (regression: poller fell back to scheduler scan and emitted stale daemon-state deltas instead of failing loud)")
		}
	}
}

// TestPoller_EmitsDaemonFailedOnRisingEdge guards the daemon-failed SSE event:
// it must fire EXACTLY ONCE on the rising failure edge — including the
// fail-in-place case where State stays "Running" but LastResult flips 0 -> 1
// (which only surfaces because LastResult is in the change-detection key) — and
// must NOT re-fire while the daemon stays failed, nor on the falling
// (recovery) edge.
func TestPoller_EmitsDaemonFailedOnRisingEdge(t *testing.T) {
	frames := [][]api.DaemonStatus{
		{{Server: "memory", State: "Running", Port: 9123, PID: 42, LastResult: 0}}, // healthy
		{{Server: "memory", State: "Running", Port: 9123, PID: 42, LastResult: 1}}, // fail in place
		{{Server: "memory", State: "Running", Port: 9123, PID: 99, LastResult: 0}}, // recovered
	}
	status := &scriptedStatus{frames: frames}
	b := NewBroadcaster()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch := b.Subscribe(ctx)

	p := NewStatusPoller(status, b, 50*time.Millisecond)
	go p.Run(ctx)

	// Three daemon-state deltas are expected (insert, 0->1, 1->0). Collect
	// until we have all three, counting daemon-failed events seen alongside.
	// After frame 3 the scripted source replays the last frame, so the stream
	// goes quiet — by the time the 3rd daemon-state arrives, the single
	// daemon-failed (published between deltas 2 and 3, FIFO) is already in hand.
	var failedBodies []map[string]any
	daemonStates := 0
	deadline := time.After(2 * time.Second)
	for daemonStates < 3 {
		select {
		case ev := <-ch:
			switch ev.Type {
			case "daemon-state":
				daemonStates++
			case "daemon-failed":
				failedBodies = append(failedBodies, ev.Body)
			}
		case <-deadline:
			t.Fatalf("did not observe 3 daemon-state deltas; got %d, daemon-failed=%d", daemonStates, len(failedBodies))
		}
	}

	if len(failedBodies) != 1 {
		t.Fatalf("daemon-failed fired %d times, want exactly 1 (rising edge only); bodies=%v", len(failedBodies), failedBodies)
	}
	body := failedBodies[0]
	if got, _ := body["server"].(string); got != "memory" {
		t.Errorf("daemon-failed server = %q, want memory", got)
	}
	if got, _ := body["last_result"].(int32); got != 1 {
		t.Errorf("daemon-failed last_result = %v, want int32(1)", body["last_result"])
	}
}

// TestPoller_DaemonFailedOnFailStateString guards the second failure predicate
// path: a state string containing "fail" (e.g. "FailedToLaunch") trips
// daemon-failed even with a zero LastResult, matching tray.isFailedRow.
func TestPoller_DaemonFailedOnFailStateString(t *testing.T) {
	frames := [][]api.DaemonStatus{
		{{Server: "serena", Daemon: "claude", State: "Running", Port: 9121}},
		{{Server: "serena", Daemon: "claude", State: "FailedToLaunch", Port: 9121}},
	}
	status := &scriptedStatus{frames: frames}
	b := NewBroadcaster()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch := b.Subscribe(ctx)

	p := NewStatusPoller(status, b, 50*time.Millisecond)
	go p.Run(ctx)

	deadline := time.After(2 * time.Second)
	for {
		select {
		case ev := <-ch:
			if ev.Type == "daemon-failed" {
				if got, _ := ev.Body["state"].(string); got != "FailedToLaunch" {
					t.Errorf("daemon-failed state = %q, want FailedToLaunch", got)
				}
				return // success
			}
		case <-deadline:
			t.Fatal("never observed daemon-failed for a 'fail'-containing state string")
		}
	}
}

// errStatus is a statusProvider that always returns the configured
// error, modeling a down-supervisor fail-loud snapshot.
type errStatus struct{ err error }

func (e errStatus) Status() ([]api.DaemonStatus, error) { return nil, e.err }

// TestPoller_FeedsErrorChannelOnFetchError is the PR #281 round-2 P2
// regression guard at the poller layer: when status.Status() errors,
// the poller MUST fan the error out to the error channel installed via
// SetErrorChannel so the tray aggregator can flip the icon to a
// degraded state instead of freezing (the poll-error path early-returns
// before fanning a snapshot, so the snapshot channel never sees the
// down-supervisor cycle). Without this feed the tray's only state
// source goes silent and the icon stays green over a down supervisor.
func TestPoller_FeedsErrorChannelOnFetchError(t *testing.T) {
	b := NewBroadcaster()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	errCh := make(chan error, 1)
	snapCh := make(chan []api.DaemonStatus, 1)
	p := NewStatusPoller(errStatus{err: api.ErrSupervisorDown}, b, 50*time.Millisecond)
	p.SetErrorChannel(errCh)
	p.SetSnapshotChannel(snapCh)
	go p.Run(ctx)

	select {
	case got := <-errCh:
		if !strings.Contains(got.Error(), api.ErrSupervisorDown.Error()) {
			t.Errorf("error channel carried %q, want ErrSupervisorDown (%q)", got.Error(), api.ErrSupervisorDown.Error())
		}
	case <-time.After(2 * time.Second):
		t.Fatal("poller never fed the error channel on a fetch error (regression: tray would freeze on down supervisor)")
	}

	// And no snapshot is fanned on the error path — the tray must rely
	// on the error channel, not a stale/empty snapshot.
	select {
	case snap := <-snapCh:
		t.Fatalf("poller fanned a snapshot on the error path: %v (regression: empty/stale snapshot aggregates to StateHealthy and masks the down supervisor)", snap)
	case <-time.After(200 * time.Millisecond):
		// good — no snapshot on the error path.
	}
}
