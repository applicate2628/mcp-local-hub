package cli

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/gofrs/flock"

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
//
// Uses the inner form with emptyIntentReader to avoid invoking
// defaultIntentReader (which hits disk via api.ReadDaemonIntent).
// Disk-bound tests are flaky on bloated dev intent files; the
// production wrapper still binds defaultIntentReader and is exercised
// by TestAggregateTrayState_IntentSuppressesUserStop below.
func TestAggregateTrayState_ForwardsOnChange(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	snaps := make(chan []api.DaemonStatus, 8)
	out := make(chan tray.TrayState, 8)
	toaster := &fakeToaster{}
	go aggregateTrayStateWithToast(ctx, snaps, out, toaster.show, emptyIntentReader)

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
	toaster := &fakeToaster{}
	done := make(chan struct{})
	go func() {
		aggregateTrayStateWithToast(ctx, snaps, out, toaster.show, emptyIntentReader)
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
	toaster := &fakeToaster{}
	done := make(chan struct{})
	go func() {
		aggregateTrayStateWithToast(ctx, snaps, out, toaster.show, emptyIntentReader)
		close(done)
	}()
	close(snaps)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("aggregateTrayState did not return within 2s of snapshot channel close")
	}
}

// emptyIntentReader is the test-side intentReaderFn that yields the
// "no recorded preference" outcome (production fallback when the file
// is genuinely missing). All pre-bug-#3 tests use this — they assert
// behavior under "no intent" semantics, identical to the legacy
// Aggregate / isFailedRow predicates.
//
// Round 3 codex finding R1: returns IntentReadResult with State=missing
// and Err=nil so the aggregator's cache treats it as authoritative
// (operator has cleared every entry / fresh install) — the empty Tasks
// map IS the intended snapshot, not a degraded fallback.
func emptyIntentReader() api.IntentReadResult {
	return api.IntentReadResult{
		State: api.IntentStateMissing,
		File:  api.DaemonIntentFile{Tasks: map[string]api.DaemonIntent{}},
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
	go aggregateTrayStateWithToast(ctx, snaps, out, toaster.show, emptyIntentReader)

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
	go aggregateTrayStateWithToast(ctx, snaps, out, toaster.show, emptyIntentReader)

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

// TestAggregateTrayState_ToastIgnoresNeverRunSentinel guards the
// Codex r2 P2 finding: internal/scheduler/scheduler.go:53 documents
// LastResult = -1 as the never-run sentinel (the default in
// parseTaskQueryOutput when schtasks /Query output omits
// "Last Result:"). Without this filter, freshly installed daemons
// — before any execution attempt — fire false "daemon failed"
// toasts on the first poll snapshot.
//
// Different sentinel from the 0x41303 case above: that's the modern
// Task Scheduler 2.0 info-code that schtasks does report; -1 is the
// internal-API fallback when no value is parsed at all.
func TestAggregateTrayState_ToastIgnoresNeverRunSentinel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	snaps := make(chan []api.DaemonStatus, 8)
	out := make(chan tray.TrayState, 8)
	toaster := &fakeToaster{}
	go aggregateTrayStateWithToast(ctx, snaps, out, toaster.show, emptyIntentReader)

	for i := 0; i < 3; i++ {
		snaps <- []api.DaemonStatus{
			{Server: "memory", State: "Ready", LastResult: -1},
		}
	}

	time.Sleep(300 * time.Millisecond)
	calls := toaster.snapshot()
	if len(calls) != 0 {
		t.Errorf("LastResult = -1 (never-run sentinel) must NOT fire toasts; got %d calls: %+v",
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
	go aggregateTrayStateWithToast(ctx, snaps, out, toaster.show, emptyIntentReader)

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

// fakeIntentReader returns the supplied DaemonIntentFile wrapped in a
// State=valid IntentReadResult on every invocation. Tests use this to
// drive the intent-aware suppression branch deterministically (no disk
// I/O, no DaemonStateDir resolution). The wrapped result models the
// happy path; tests that need to exercise the cache fallback synthesise
// their own IntentReadResult (e.g. State=missing + Err=context.DeadlineExceeded).
func fakeIntentReader(file api.DaemonIntentFile) intentReaderFn {
	return func() api.IntentReadResult {
		return api.IntentReadResult{
			State: api.IntentStateValid,
			File:  file,
		}
	}
}

// TestAggregateTrayState_IntentSuppressesUserStop_NoToast_NoError is
// the headline bug #3 regression guard. Sequence: a daemon was Running,
// the operator runs `mcphub stop --server memory`, the daemon exits
// with code 1 (Node MCP server graceful-stdin-close behavior), and a
// fresh status snapshot lands. Without the intent suppression: red
// tray icon + spurious toast.
//
// With the codex deep-sec round-1 MED fix: a suppressed row is also
// excluded from the running/total denominator, so a sole intentionally-
// stopped daemon → total=0 → StateHealthy. ("Everything I wanted
// stopped IS stopped.") The aggregator coalesces same-state forwards,
// so snapshot 1 (Running) and snapshot 2 (Stopped+suppressed) both
// classify as StateHealthy and only one forward fires.
func TestAggregateTrayState_IntentSuppressesUserStop_NoToast_NoError(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	now := time.Now().UTC()
	intent := api.DaemonIntentFile{
		Tasks: map[string]api.DaemonIntent{
			"\\mcp-local-hub-memory-default": {
				Desired:   api.IntentDesiredStopped,
				Reason:    api.IntentReasonUserStop,
				UpdatedAt: now.Add(-1 * time.Minute),
			},
		},
	}

	snaps := make(chan []api.DaemonStatus, 4)
	out := make(chan tray.TrayState, 4)
	toaster := &fakeToaster{}
	go aggregateTrayStateWithToast(ctx, snaps, out, toaster.show, fakeIntentReader(intent))

	// Snapshot 1: clean (running, no intent suppression needed).
	snaps <- []api.DaemonStatus{
		{Server: "memory", TaskName: "\\mcp-local-hub-memory-default", State: "Running"},
	}
	// Snapshot 2: post-stop with exit code 1. Intent says user-stop.
	snaps <- []api.DaemonStatus{
		{Server: "memory", TaskName: "\\mcp-local-hub-memory-default", State: "Stopped", LastResult: 1},
	}

	// Collect tray-state forwards for ~500ms.
	got := []tray.TrayState{}
	deadline := time.After(500 * time.Millisecond)
collectLoop:
	for {
		select {
		case s := <-out:
			got = append(got, s)
		case <-deadline:
			break collectLoop
		}
	}

	// Last forwarded state must be StateHealthy: suppressed rows are
	// excluded from the denominator (codex deep-sec round 1 MED), so a
	// sole user-stopped daemon → total=0 → StateHealthy. The critical
	// negative invariant is "no StateError forward" — the icon must
	// not turn red on a clean operator-initiated stop.
	if len(got) == 0 {
		t.Fatalf("expected at least 1 tray-state forward, got 0")
	}
	last := got[len(got)-1]
	if last != tray.StateHealthy {
		t.Errorf("last tray state = %v, want StateHealthy (bug #3 + codex deep-sec MED — suppressed row excluded from denominator)", last)
	}
	for _, s := range got {
		if s == tray.StateError {
			t.Errorf("forwarded state stream included StateError = %v (intent must hide clean stops)", got)
			break
		}
	}
	// Toast count must be zero — the failure was suppressed.
	if calls := toaster.snapshot(); len(calls) != 0 {
		t.Errorf("expected 0 toasts (intent suppressed failure), got %d: %+v", len(calls), calls)
	}
}

// TestAggregateTrayState_ChronicFailureIntent_StillFiresToast is the
// inverse guard. ChronicFailure is the watchdog's quarantine reason
// — the operator MUST see this. Even though Desired=stopped, the
// suppression must NOT apply for chronic-failure.
func TestAggregateTrayState_ChronicFailureIntent_StillFiresToast(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	now := time.Now().UTC()
	intent := api.DaemonIntentFile{
		Tasks: map[string]api.DaemonIntent{
			"\\mcp-local-hub-memory-default": {
				Desired:   api.IntentDesiredStopped,
				Reason:    api.IntentReasonChronicFailure,
				UpdatedAt: now.Add(-1 * time.Minute),
			},
		},
	}

	snaps := make(chan []api.DaemonStatus, 4)
	out := make(chan tray.TrayState, 4)
	toaster := &fakeToaster{}
	go aggregateTrayStateWithToast(ctx, snaps, out, toaster.show, fakeIntentReader(intent))

	// Snapshot 1: healthy (no rows match intent yet).
	snaps <- []api.DaemonStatus{
		{Server: "other", TaskName: "\\mcp-local-hub-other-default", State: "Running"},
	}
	// Snapshot 2: memory now appears, exit-code-1, with chronic-failure
	// intent. Toast must fire; tray must classify as StateError.
	snaps <- []api.DaemonStatus{
		{Server: "other", TaskName: "\\mcp-local-hub-other-default", State: "Running"},
		{Server: "memory", TaskName: "\\mcp-local-hub-memory-default", State: "Stopped", LastResult: 1},
	}

	deadline := time.After(2 * time.Second)
	for {
		if len(toaster.snapshot()) >= 1 {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("expected 1 toast for chronic-failure within 2s, got %d (chronic-failure must NOT be suppressed)", len(toaster.snapshot()))
		default:
			time.Sleep(20 * time.Millisecond)
		}
	}

	// Drain forwarded states briefly and confirm StateError reaches the
	// channel.
	sawError := false
	deadline2 := time.After(500 * time.Millisecond)
forwardLoop:
	for {
		select {
		case s := <-out:
			if s == tray.StateError {
				sawError = true
			}
		case <-deadline2:
			break forwardLoop
		}
	}
	if !sawError {
		t.Errorf("expected StateError forward for chronic-failure intent (must remain visible)")
	}
}

// TestAggregateTrayState_IntentTTLExpired_BackToError guards the TTL
// boundary at the cli layer. user-stop intent older than StopIntentTTL
// (24h) must no longer suppress — a daemon that crashes a day after
// the operator stopped it should once again surface as StateError +
// fire a toast (operator forgot about it; the daemon keeps failing).
func TestAggregateTrayState_IntentTTLExpired_BackToError(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	now := time.Now().UTC()
	// 25h-old intent → past StopIntentTTL.
	intent := api.DaemonIntentFile{
		Tasks: map[string]api.DaemonIntent{
			"\\mcp-local-hub-memory-default": {
				Desired:   api.IntentDesiredStopped,
				Reason:    api.IntentReasonUserStop,
				UpdatedAt: now.Add(-25 * time.Hour),
			},
		},
	}

	snaps := make(chan []api.DaemonStatus, 4)
	out := make(chan tray.TrayState, 4)
	toaster := &fakeToaster{}
	go aggregateTrayStateWithToast(ctx, snaps, out, toaster.show, fakeIntentReader(intent))

	// Snapshot 1: healthy.
	snaps <- []api.DaemonStatus{
		{Server: "memory", TaskName: "\\mcp-local-hub-memory-default", State: "Running"},
	}
	// Snapshot 2: post-stop with exit code 1, intent TTL expired.
	snaps <- []api.DaemonStatus{
		{Server: "memory", TaskName: "\\mcp-local-hub-memory-default", State: "Stopped", LastResult: 1},
	}

	// Toast must fire (intent suppression no longer applies after TTL).
	deadline := time.After(2 * time.Second)
	for {
		if len(toaster.snapshot()) >= 1 {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("expected 1 toast for TTL-expired intent within 2s, got %d (TTL expiry must restore failure visibility)", len(toaster.snapshot()))
		default:
			time.Sleep(20 * time.Millisecond)
		}
	}
}

// TestAggregateTrayState_LockContentionPreservesUserStopSuppression is
// the round 3 codex finding R1 regression guard. The flow:
//
//   1. Snapshot 1: row with active user-stop intent → aggregator reads
//      a valid IntentReadResult, populates the cache, classifies as
//      StateHealthy (suppressed user-stop excluded from denominator).
//   2. Snapshot 2: SAME row, but the intent reader returns a degraded
//      result (State=missing + Err=context.DeadlineExceeded — the
//      shape TryReadDaemonIntent emits on flock contention). The cache
//      from cycle 1 must apply, so the row STAYS suppressed.
//   3. Snapshot 3: SAME row, intent reader now returns a valid empty
//      file (State=valid + empty Tasks) — operator cleared the intent
//      via `mcphub watchdog enable`. Cache REPLACES with the fresh
//      empty snapshot; suppression no longer applies; row classifies
//      as StateError (and a toast fires for the failure onset).
//
// This is the regression that was BROKEN by PR #142 round 3 P2's
// initial wiring: under flock contention the aggregator dropped to
// the empty intent fallback inline, bypassing user-stop suppression
// and flashing the red icon Bug #3 was supposed to suppress.
func TestAggregateTrayState_LockContentionPreservesUserStopSuppression(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	now := time.Now().UTC()
	taskName := "\\mcp-local-hub-memory-default"
	intentFile := api.DaemonIntentFile{
		Tasks: map[string]api.DaemonIntent{
			taskName: {
				Desired:   api.IntentDesiredStopped,
				Reason:    api.IntentReasonUserStop,
				UpdatedAt: now.Add(-1 * time.Minute),
			},
		},
	}

	// Programmable reader: returns the next IntentReadResult per call,
	// indexed by an int counter the aggregator goroutine reads via the
	// channel-fronted closure. Using a channel-fronted queue keeps the
	// reader goroutine-safe (the aggregator runs in its own goroutine
	// and may call the reader from there).
	type readerStep struct {
		res api.IntentReadResult
	}
	stepCh := make(chan readerStep, 8)
	reader := func() api.IntentReadResult {
		select {
		case s := <-stepCh:
			return s.res
		default:
			// Default to last known shape — empty valid (operator cleared).
			// Should never be hit in this test if the producer keeps up.
			return api.IntentReadResult{
				State: api.IntentStateValid,
				File:  api.DaemonIntentFile{Tasks: map[string]api.DaemonIntent{}},
			}
		}
	}

	// Cycle 1: valid intent (active user-stop).
	stepCh <- readerStep{res: api.IntentReadResult{
		State: api.IntentStateValid,
		File:  intentFile,
	}}
	// Cycle 2: lock-timeout fallback (degraded).
	stepCh <- readerStep{res: api.IntentReadResult{
		State: api.IntentStateMissing,
		File:  api.DaemonIntentFile{Tasks: map[string]api.DaemonIntent{}},
		Err:   fmt.Errorf("flock TryLockContext: timeout after 250ms: %w", context.DeadlineExceeded),
	}}
	// Cycle 3: operator cleared intent → fresh empty valid snapshot.
	stepCh <- readerStep{res: api.IntentReadResult{
		State: api.IntentStateValid,
		File:  api.DaemonIntentFile{Tasks: map[string]api.DaemonIntent{}},
	}}

	snaps := make(chan []api.DaemonStatus, 8)
	out := make(chan tray.TrayState, 8)
	toaster := &fakeToaster{}
	go aggregateTrayStateWithToast(ctx, snaps, out, toaster.show, reader)

	// Each snapshot is the same failed-but-user-stopped row. The
	// aggregator's behaviour must follow the cache contract:
	//   cycle 1 → suppressed (intent valid)
	//   cycle 2 → suppressed (cache covers the contention)
	//   cycle 3 → NOT suppressed (cache REPLACED by fresh empty file)
	row := api.DaemonStatus{
		Server:     "memory",
		TaskName:   taskName,
		State:      "Stopped",
		LastResult: 1, // exit code 1 = the Node MCP graceful-stdin-close case
	}
	snaps <- []api.DaemonStatus{row}
	snaps <- []api.DaemonStatus{row}
	snaps <- []api.DaemonStatus{row}

	// Drain forwarded states for ~1s. Cycle 3's transition produces
	// a Healthy → Error forward.
	got := []tray.TrayState{}
	deadline := time.After(1500 * time.Millisecond)
collectLoop:
	for {
		select {
		case s := <-out:
			got = append(got, s)
		case <-deadline:
			break collectLoop
		}
	}

	if len(got) == 0 {
		t.Fatalf("expected at least 1 tray-state forward, got 0")
	}

	// Cycle 1 produces the very first forward (sentinel → Healthy).
	if got[0] != tray.StateHealthy {
		t.Errorf("forward[0] = %v, want StateHealthy (cycle 1: valid user-stop intent)", got[0])
	}

	// The last forward MUST be StateError (cycle 3: cache replaced,
	// suppression no longer applies, exit-code-1 row classifies error).
	last := got[len(got)-1]
	if last != tray.StateError {
		t.Errorf("forward[last] = %v, want StateError after fresh empty intent replaces cache; full stream: %v",
			last, got)
	}

	// Critical NEGATIVE invariant: cycles 1+2 must NOT produce a
	// StateError forward. The aggregator coalesces same-state forwards,
	// so seeing StateError BEFORE the final cycle would mean cycle 2
	// dropped suppression and forwarded StateError before cycle 3.
	// We assert that the StateError forward is the FINAL element in
	// the stream — there must be no second StateError forward, and
	// no Healthy → Error → Healthy → Error oscillation.
	for i, s := range got {
		if s == tray.StateError && i < len(got)-1 {
			t.Errorf("StateError forwarded at index %d (not final) — cache should have suppressed cycle 2; full stream: %v",
				i, got)
			break
		}
	}

	// Toast count: cycle 3 produces ONE failure-onset toast (cycle 1+2
	// were suppressed). The cycle-2 cache hit must NOT have fired a
	// toast either.
	calls := toaster.snapshot()
	if len(calls) != 1 {
		t.Errorf("toast count = %d, want exactly 1 (cycle 3 onset only); calls: %+v", len(calls), calls)
	}
}

// TestDefaultIntentReader_DoesNotBlockOnHeldLock guards the regression
// from PR #142 round 2 P2: the tray hot path must not freeze behind a
// long-held daemon-intent.json flock.
//
// Setup: redirect the per-user state dir to a temp location, hold the
// daemon-intent.json.lock sibling on a goroutine for 3 seconds, then
// invoke defaultIntentReader. Assert that the call returns within
// the wallclock cap (well under the holder's 3s grip) with a non-nil
// empty Tasks map and a non-nil timeout-flavoured error — the
// graceful-degrade contract the aggregator relies on.
//
// Round 3 codex finding R4: previous wallclockCap (500ms) was tight
// enough to flake on slow CI hosts (250ms intent-read timeout + a
// loaded scheduler can produce ~600ms wallclock). Loosened to 1.5s,
// which still proves "non-blocking" against the 3s holder grip with
// generous CI headroom. The point of this test is to detect the
// regression where TryReadDaemonIntent reverts to a kernel-level
// blocking flock (that would observe the full 3s grip), NOT to
// measure the exact retry-budget timing.
func TestDefaultIntentReader_DoesNotBlockOnHeldLock(t *testing.T) {
	root := t.TempDir()
	restore := api.SetDaemonStateRootForTest(root)
	t.Cleanup(restore)

	// Resolve the lock path the same way the API does. DaemonStateDir()
	// will create the per-user state dir under the override on first
	// call. We rely on the API package's own resolution to land on the
	// same path defaultIntentReader → TryReadDaemonIntent acquires.
	stateDir, err := api.DaemonStateDir()
	if err != nil {
		t.Fatalf("api.DaemonStateDir: %v", err)
	}
	lockPath := filepath.Join(stateDir, "daemon-intent.json.lock")

	holder := flock.New(lockPath)
	if err := holder.Lock(); err != nil {
		t.Fatalf("holder lock: %v", err)
	}
	holderDone := make(chan struct{})
	go func() {
		defer close(holderDone)
		time.Sleep(3 * time.Second)
		_ = holder.Unlock()
	}()
	t.Cleanup(func() {
		_ = holder.Unlock()
		<-holderDone
	})

	// Wallclock cap (round 3 codex finding R4): defaultIntentReaderTimeout
	// is 250ms, but a loaded CI host can add several hundred ms of
	// scheduler/syscall overhead before the goroutine returns. 1.5s
	// is the loosened cap — still well below the 3s holder grip that
	// a regression to blocking flock would observe.
	const wallclockCap = 1500 * time.Millisecond

	start := time.Now()
	got := defaultIntentReader()
	elapsed := time.Since(start)

	if elapsed > wallclockCap {
		t.Fatalf("defaultIntentReader took %s with %s timeout (cap %s) — must not block on held lock",
			elapsed, defaultIntentReaderTimeout, wallclockCap)
	}
	if got.File.Tasks == nil {
		t.Fatalf("Tasks = nil, want empty (non-nil) map for graceful-degrade contract")
	}
	if len(got.File.Tasks) != 0 {
		t.Errorf("Tasks length = %d, want 0 on lock-acquisition timeout fallback", len(got.File.Tasks))
	}
	// Round 3 codex finding R6: timeout fallback must propagate a
	// DeadlineExceeded-wrapped error so callers (and the aggregator's
	// cache) can branch on contention vs. real I/O failure.
	if got.State != api.IntentStateMissing {
		t.Errorf("State = %q, want %q on lock-timeout fallback", got.State, api.IntentStateMissing)
	}
	if got.Err == nil {
		t.Fatalf("Err = nil, want a non-nil timeout error on lock-acquisition timeout fallback")
	}
	if !errors.Is(got.Err, context.DeadlineExceeded) {
		t.Errorf("Err = %v, want errors.Is(_, context.DeadlineExceeded) (timeout taxonomy)", got.Err)
	}
}
