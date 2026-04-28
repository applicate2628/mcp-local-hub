package cli

import (
	"context"
	"sync"
	"testing"
	"time"

	"mcp-local-hub/internal/api"
	"mcp-local-hub/internal/tray"
)

// fakeToaster captures toast calls for assertion. Thread-safe
// because the aggregator fires the toast in a separate goroutine.
type fakeToaster struct {
	mu    sync.Mutex
	calls []toastCall
}

type toastCall struct {
	title string
	body  string
}

func (f *fakeToaster) show(title, body string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, toastCall{title: title, body: body})
	return nil
}

func (f *fakeToaster) snapshot() []toastCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]toastCall, len(f.calls))
	copy(out, f.calls)
	return out
}

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

// TestAggregateTrayState_ToastFiresOnFailureOnset asserts the
// aggregator fires exactly one toast when a daemon transitions
// into a failed state, and no further toasts on subsequent
// snapshots that show the daemon still failed (onset, not
// repeated alerts). Critical UX contract: spam protection
// without losing the first signal.
func TestAggregateTrayState_ToastFiresOnFailureOnset(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	snaps := make(chan []api.DaemonStatus, 8)
	out := make(chan tray.TrayState, 8)
	toaster := &fakeToaster{}
	go aggregateTrayStateWithToast(ctx, snaps, out, toaster.show)

	// Snapshot 1: all healthy.
	snaps <- []api.DaemonStatus{{Server: "memory", State: "Running"}}
	// Snapshot 2: memory failed (state contains "fail").
	snaps <- []api.DaemonStatus{{Server: "memory", State: "FailedToLaunch"}}
	// Snapshot 3: memory still failed — should NOT fire another toast.
	snaps <- []api.DaemonStatus{{Server: "memory", State: "FailedToLaunch"}}
	// Snapshot 4: memory recovered.
	snaps <- []api.DaemonStatus{{Server: "memory", State: "Running"}}
	// Snapshot 5: memory failed again — fresh onset, should fire.
	snaps <- []api.DaemonStatus{{Server: "memory", State: "FailedToLaunch"}}

	// Wait for goroutines to drain. The toast goroutines fire async,
	// so poll up to 2s for the expected count.
	deadline := time.After(2 * time.Second)
	for {
		if len(toaster.snapshot()) >= 2 {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("expected 2 toast calls within 2s, got %d", len(toaster.snapshot()))
		default:
			time.Sleep(20 * time.Millisecond)
		}
	}
	calls := toaster.snapshot()
	if len(calls) != 2 {
		t.Errorf("expected exactly 2 onset toasts, got %d: %+v", len(calls), calls)
	}
	for _, c := range calls {
		if c.title != "mcp-local-hub: daemon failed" {
			t.Errorf("title = %q, want %q", c.title, "mcp-local-hub: daemon failed")
		}
		if c.body == "" {
			t.Errorf("body should be non-empty (server/daemon + state details)")
		}
	}
}

// TestAggregateTrayState_ToastIgnoresTaskSchedulerInfoCodes is the
// critical regression guard for the spam-toast bug observed on PR
// #22 initial push. Task Scheduler reports informational codes in
// the 0x41300-0x4130F range (e.g., 0x41303 = task has not yet run,
// the default state for orphan/never-run scheduler entries). Those
// are NOT failures; treating them as such fired a "daemon failed"
// toast every 5s for every never-run task on the host, which was
// the user-visible symptom that broke the C4 wiring.
//
// This test feeds a daemon row with LastResult=0x41303 across
// multiple snapshots and asserts NO toasts fire. If the predicate
// ever regresses to plain `LastResult != 0`, this test will spike
// 3+ toasts and fail.
func TestAggregateTrayState_ToastIgnoresTaskSchedulerInfoCodes(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	snaps := make(chan []api.DaemonStatus, 8)
	out := make(chan tray.TrayState, 8)
	toaster := &fakeToaster{}
	go aggregateTrayStateWithToast(ctx, snaps, out, toaster.show)

	// Three snapshots of an orphan daemon with TS info-code "never
	// run yet" — no toast may fire.
	for i := 0; i < 3; i++ {
		snaps <- []api.DaemonStatus{
			{Server: "gdb", State: "Ready", LastResult: 0x41303},
		}
	}

	// Wait long enough for any spurious toasts to surface (~200ms
	// is plenty for a goroutine fire). If no toasts arrive, the
	// suppression is working.
	time.Sleep(300 * time.Millisecond)
	calls := toaster.snapshot()
	if len(calls) != 0 {
		t.Errorf("TS info code 0x41303 must NOT fire toasts; got %d calls: %+v",
			len(calls), calls)
	}
}

// TestAggregateTrayState_ToastUsesLastResult asserts a row whose
// LastResult != 0 (Task Scheduler's most-recent exit code) fires a
// toast even if its state field is currently "Running" — the
// failure code is the canonical "something went wrong" signal and
// must reach the user.
func TestAggregateTrayState_ToastUsesLastResult(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	snaps := make(chan []api.DaemonStatus, 8)
	out := make(chan tray.TrayState, 8)
	toaster := &fakeToaster{}
	go aggregateTrayStateWithToast(ctx, snaps, out, toaster.show)

	// Snapshot 1: clean.
	snaps <- []api.DaemonStatus{{Server: "memory", State: "Running"}}
	// Snapshot 2: state still Running but LastResult flipped to 1.
	snaps <- []api.DaemonStatus{{Server: "memory", State: "Running", LastResult: 1}}

	deadline := time.After(2 * time.Second)
	for {
		if len(toaster.snapshot()) >= 1 {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("expected 1 toast within 2s, got %d", len(toaster.snapshot()))
		default:
			time.Sleep(20 * time.Millisecond)
		}
	}
}
