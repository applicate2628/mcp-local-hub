package cli

import (
	"context"

	"github.com/spf13/cobra"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// Tests for the supervisor accept-path stall dump trigger
// (supervise_stall_dump.go). The class under test is
// work-items/bugs/2026-07-20-supervisor-hello-latency-vs-client-budget.md:
// the accept path stalls for seconds, the client abandons at its 5s budget,
// and WHY it stalls is not obtainable without a goroutine stack taken DURING
// the stall.

// shortStallDumpConfig compresses the production tunables so a test can
// exercise a "stall" in milliseconds.
//
// The config is a per-trigger VALUE, so nothing process-global is mutated
// and no leaked goroutine from a prior test can race a cleanup restore — the
// exact race `go test -race` caught when these were package-level vars.
func shortStallDumpConfig(threshold, cooldown time.Duration) stallDumpConfig {
	cfg := defaultStallDumpConfig()
	cfg.AcceptHelloThreshold = threshold
	cfg.Cooldown = cooldown
	cfg.SentinelPollInterval = 10 * time.Millisecond
	return cfg
}

// readStallDumps returns the sorted dump file names under the trigger's dir.
func readStallDumps(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatalf("read dump dir %s: %v", dir, err)
	}
	var names []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasPrefix(e.Name(), "stall-") && strings.HasSuffix(e.Name(), ".txt") {
			names = append(names, e.Name())
		}
	}
	return names
}

// startStallDumpWorkers starts the capture worker (and optionally the
// sentinel watcher) and JOINS them before the test's temp dir is removed.
//
// Joining is not optional hygiene: a bare `defer cancel()` returns while the
// goroutines are still running, so a capture mid-write can create a file
// underneath t.TempDir's RemoveAll — observed as a real
// "TempDir RemoveAll cleanup: ... The directory is not empty" failure under
// -race -count=5. t.Cleanup is LIFO and t.TempDir registered its cleanup
// earlier (inside makeTestDeps), so this cleanup runs FIRST and the workers
// are fully stopped before removal begins.
func startStallDumpWorkers(t *testing.T, trig *stallDumpTrigger, stateDir string, withSentinel bool) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); trig.Run(ctx) }()
	if withSentinel {
		wg.Add(1)
		go func() { defer wg.Done(); runStallDumpSentinelWatcher(ctx, stateDir, trig) }()
	}
	t.Cleanup(func() {
		cancel()
		wg.Wait()
	})
}

func waitForStallDump(t *testing.T, dir string, want int, timeout time.Duration) []string {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		names := readStallDumps(t, dir)
		if len(names) >= want {
			return names
		}
		if time.Now().After(deadline) {
			t.Fatalf("expected >=%d stall dumps in %s within %s, got %d", want, dir, timeout, len(names))
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestStallDumpCapturesGoroutineStacksOnSlowHello is the primary contract: a
// connection whose accept-to-hello interval exceeds the threshold produces a
// dump file containing real goroutine frames.
//
// It drives the REAL accept loop (acceptIPCConnections -> serveIPCConn) so
// the timing plumbed from the loop into the arm is exercised end to end, not
// just the trigger in isolation.
func TestStallDumpCapturesGoroutineStacksOnSlowHello(t *testing.T) {
	deps, logPath := makeTestDeps(t)
	trig := newStallDumpTriggerWithConfig(deps.stateDir, deps.events,
		shortStallDumpConfig(20*time.Millisecond, time.Hour))
	deps.stallDump = trig

	startStallDumpWorkers(t, trig, deps.stateDir, false)

	listener := &fakeAcceptor{
		queue: []fakeAcceptResult{{conn: &fakeConn{}}},
		// Simulate the stall: the hello write takes far longer than the
		// threshold. This is the injected stall.
		helloFn: func(net.Conn) error {
			time.Sleep(60 * time.Millisecond)
			return nil
		},
	}
	runAcceptLoop(t, listener, deps, 5*time.Second)

	names := waitForStallDump(t, trig.dir, 1, 3*time.Second)
	if len(names) != 1 {
		t.Fatalf("stall dumps = %d, want exactly 1; got %v", len(names), names)
	}

	raw, err := os.ReadFile(filepath.Join(trig.dir, names[0]))
	if err != nil {
		t.Fatalf("read dump: %v", err)
	}
	body := string(raw)

	// Header: which arm fired and what it measured.
	for _, want := range []string{
		"# reason: accept-hello-latency",
		"# accept_to_hello_ms: ",
		"# accept_dwell_ms: ",
		"# threshold_ms: 20",
		"# suppressed_since_last_capture: 0",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("dump header missing %q; got:\n%s", want, body[:min(len(body), 900)])
		}
	}

	// Payload: recognizable goroutine frames. runtime.Stack(all=true)
	// always includes the goroutine doing the capture, so this frame is a
	// deterministic anchor rather than a racy one.
	if !strings.Contains(body, "goroutine ") {
		t.Error("dump body has no goroutine header line")
	}
	if !strings.Contains(body, "captureGoroutineDump") {
		t.Errorf("dump body has no recognizable mcphub frame; got:\n%s", body[:min(len(body), 1500)])
	}

	// The dump must be discoverable from the existing audit channel.
	evt := waitForSupervisorEvent(t, logPath, `"event":"supervisor-stall-dump-captured"`, 3*time.Second)
	if !strings.Contains(evt, `"reason":"accept-hello-latency"`) {
		t.Errorf("captured event missing reason; got:\n%s", evt)
	}
}

// TestStallDumpSilentOnFastHello is the falsifier for the test above: a
// normal handshake must produce NOTHING. Without it, an implementation that
// dumps unconditionally would pass the positive test.
func TestStallDumpSilentOnFastHello(t *testing.T) {
	deps, logPath := makeTestDeps(t)
	trig := newStallDumpTriggerWithConfig(deps.stateDir, deps.events,
		shortStallDumpConfig(500*time.Millisecond, time.Hour))
	deps.stallDump = trig

	startStallDumpWorkers(t, trig, deps.stateDir, false)

	listener := &fakeAcceptor{
		queue: []fakeAcceptResult{{conn: &fakeConn{}}, {conn: &fakeConn{}}},
		// Fast hello: returns immediately, far under the threshold.
		helloFn: func(net.Conn) error { return nil },
	}
	runAcceptLoop(t, listener, deps, 5*time.Second)

	// Give any (incorrect) capture time to land before asserting absence.
	time.Sleep(200 * time.Millisecond)

	if names := readStallDumps(t, trig.dir); len(names) != 0 {
		t.Fatalf("fast handshake must produce no stall dump, got %v", names)
	}
	if raw, err := os.ReadFile(logPath); err == nil {
		if strings.Contains(string(raw), "supervisor-stall-dump-captured") {
			t.Errorf("fast handshake must not emit a capture event; got:\n%s", raw)
		}
	}
	if got := trig.dropped.Load(); got != 0 {
		t.Errorf("fast handshake must not even request a capture, dropped=%d", got)
	}
}

// TestStallDumpRateLimitHoldsAndReportsSuppression covers the third
// requirement: a stall that recurs every 30-90s must not produce a gigabyte
// of dumps, AND the suppressed captures must not be silently invisible.
//
// Drives capture() directly (no Run goroutine) so the gate and the
// suppression accounting are deterministic rather than timing-dependent.
func TestStallDumpRateLimitHoldsAndReportsSuppression(t *testing.T) {
	deps, _ := makeTestDeps(t)
	trig := newStallDumpTriggerWithConfig(deps.stateDir, deps.events,
		shortStallDumpConfig(time.Millisecond, time.Hour))

	req := stallDumpRequest{
		Reason: stallDumpReasonHelloLatency,
		At:     time.Now(),
		Body:   map[string]any{"accept_to_hello_ms": int64(9000)},
	}

	// One episode's worth of arming: the bug's stall-then-drain shape fires
	// the threshold 8 times within milliseconds.
	const burst = 8
	for range burst {
		trig.capture(req)
	}

	names := readStallDumps(t, trig.dir)
	if len(names) != 1 {
		t.Fatalf("cooldown must collapse a %d-request burst to 1 dump, got %d: %v", burst, len(names), names)
	}
	if got := trig.dropped.Load(); got != burst-1 {
		t.Fatalf("suppressed count = %d, want %d", got, burst-1)
	}

	// Non-blindness: the suppressed count must surface in the NEXT capture,
	// so an operator reading a dump can tell how many stalls went
	// uncaptured. Release the cooldown and fire once more. Safe to write
	// cfg here specifically because this test drives capture() on its own
	// goroutine with no Run worker — there is no concurrent reader.
	trig.cfg.Cooldown = 0
	trig.capture(req)

	names = readStallDumps(t, trig.dir)
	if len(names) != 2 {
		t.Fatalf("second capture after cooldown release: dumps = %d, want 2 (%v)", len(names), names)
	}
	raw, err := os.ReadFile(filepath.Join(trig.dir, names[1]))
	if err != nil {
		t.Fatalf("read second dump: %v", err)
	}
	want := "# suppressed_since_last_capture: 7"
	if !strings.Contains(string(raw), want) {
		t.Errorf("second dump must report the suppressed burst (%q); header:\n%s", want, firstLines(string(raw), 14))
	}
	if got := trig.dropped.Load(); got != 0 {
		t.Errorf("suppressed count must reset after being reported, got %d", got)
	}
}

// TestStallDumpSentinelFiresWithoutTouchingIPC covers the manual arm. There
// is deliberately NO listener and NO accept loop in this test: if the
// sentinel path depended on IPC in any way, this test could not pass.
func TestStallDumpSentinelFiresWithoutTouchingIPC(t *testing.T) {
	deps, logPath := makeTestDeps(t)
	trig := newStallDumpTriggerWithConfig(deps.stateDir, deps.events,
		shortStallDumpConfig(time.Millisecond, time.Hour))

	startStallDumpWorkers(t, trig, deps.stateDir, true)

	sentinel := filepath.Join(deps.stateDir, stallDumpSentinelLeaf)
	if err := os.WriteFile(sentinel, nil, 0o600); err != nil {
		t.Fatalf("create sentinel: %v", err)
	}

	names := waitForStallDump(t, trig.dir, 1, 3*time.Second)
	raw, err := os.ReadFile(filepath.Join(trig.dir, names[0]))
	if err != nil {
		t.Fatalf("read dump: %v", err)
	}
	body := string(raw)
	if !strings.Contains(body, "# reason: operator-sentinel") {
		t.Errorf("dump must record the operator arm; header:\n%s", firstLines(body, 14))
	}
	if !strings.Contains(body, "captureGoroutineDump") {
		t.Error("operator dump body has no recognizable goroutine frames")
	}

	// Consume-by-remove: one operator touch must yield exactly one capture,
	// not one per poll tick.
	if _, err := os.Stat(sentinel); !os.IsNotExist(err) {
		t.Errorf("sentinel must be consumed (removed) by the watcher, stat err = %v", err)
	}
	time.Sleep(120 * time.Millisecond) // >10 poll ticks
	if names := readStallDumps(t, trig.dir); len(names) != 1 {
		t.Fatalf("one sentinel touch must yield exactly 1 dump, got %d: %v", len(names), names)
	}

	waitForSupervisorEvent(t, logPath, `"reason":"operator-sentinel"`, 3*time.Second)
}

// TestStallDumpRequestNeverBlocksWhenCaptureBusy is the safety contract: a
// diagnostic that can wedge the process it diagnoses is worse than no
// diagnostic. The hot path's hand-off must be non-blocking even when the
// capture worker is absent (here: no Run goroutine at all, so the depth-1
// queue stays full forever).
func TestStallDumpRequestNeverBlocksWhenCaptureBusy(t *testing.T) {
	deps, _ := makeTestDeps(t)
	trig := newStallDumpTrigger(deps.stateDir, deps.events)

	req := stallDumpRequest{Reason: stallDumpReasonHelloLatency, At: time.Now()}

	done := make(chan struct{})
	go func() {
		defer close(done)
		// First fills the depth-1 queue; the rest must all fall through
		// the default branch rather than parking.
		for range 1000 {
			trig.Request(req)
		}
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Request blocked when the capture queue was full — the hot path can be wedged by the diagnostic")
	}

	if got := trig.dropped.Load(); got != 999 {
		t.Errorf("dropped = %d, want 999 (1 queued, rest counted not lost)", got)
	}
	// Nil-receiver safety: every existing call site builds a bare
	// ipcDispatchDeps{} with no trigger.
	var nilTrig *stallDumpTrigger
	nilTrig.Request(req)
	nilTrig.maybeArmOnHello(ipcAcceptTiming{acceptedAt: time.Now().Add(-time.Hour)}, nil)
}

// TestStallDumpRetentionPrunesOldestDumps proves the bounded-disk claim: a
// stall recurring over a long supervisor lifetime must not accumulate dumps
// without limit, and pruning must drop the OLDEST, keeping the freshest
// evidence.
func TestStallDumpRetentionPrunesOldestDumps(t *testing.T) {
	const retain = 3
	cfg := shortStallDumpConfig(time.Millisecond, 0)
	cfg.RetainFiles = retain

	deps, _ := makeTestDeps(t)
	trig := newStallDumpTriggerWithConfig(deps.stateDir, deps.events, cfg)

	req := stallDumpRequest{Reason: stallDumpReasonHelloLatency, At: time.Now()}
	var written []string
	for range 6 {
		trig.capture(req)
		names := readStallDumps(t, trig.dir)
		written = append(written, names[len(names)-1])
	}

	names := readStallDumps(t, trig.dir)
	if len(names) != retain {
		t.Fatalf("retained dumps = %d, want %d: %v", len(names), retain, names)
	}
	// Names are timestamp-prefixed, so lexicographic order is chronological:
	// the survivors must be the last three written.
	wantNewest := written[len(written)-retain:]
	for i, want := range wantNewest {
		if names[i] != want {
			t.Errorf("survivor[%d] = %s, want %s (prune must drop the OLDEST)", i, names[i], want)
		}
	}
	// The per-file flock leaves WriteStateFileBytesAtomic creates must be
	// pruned alongside their dump, or the directory grows anyway.
	entries, err := os.ReadDir(trig.dir)
	if err != nil {
		t.Fatalf("read dump dir: %v", err)
	}
	var locks int
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".lock") {
			locks++
		}
	}
	if locks > retain {
		t.Errorf("orphaned flock leaves = %d, want <= %d", locks, retain)
	}
}

func firstLines(s string, n int) string {
	lines := strings.SplitN(s, "\n", n+1)
	if len(lines) > n {
		lines = lines[:n]
	}
	return strings.Join(lines, "\n")
}

// TestStallDumpArmStaysOffHotPathWhenWorkerAbsent pins the design constraint
// AT THE ARM SITE: maybeArmOnHello must only hand off to the capture worker,
// never perform the capture (STW + file write + flock + event emit) on the
// calling per-connection goroutine.
//
// QA-added (2026-07-21 review): a mutation replacing the arm's Request hand-off
// with a synchronous t.capture(req) call survived every existing test —
// TestStallDumpRequestNeverBlocksWhenCaptureBusy drives Request directly, so a
// future edit that bypasses Request at the arm site was unprotected. This test
// fails against that mutant.
//
// Determinism: no Run worker exists, so nothing races the assertions; the
// accept timestamp is a fixed offset far past the threshold. No clock, seed,
// locale, or ordering dependence beyond the monotonic elapsed check.
func TestStallDumpArmStaysOffHotPathWhenWorkerAbsent(t *testing.T) {
	deps, _ := makeTestDeps(t)
	trig := newStallDumpTriggerWithConfig(deps.stateDir, deps.events,
		shortStallDumpConfig(time.Millisecond, time.Hour))
	// Deliberately NO trig.Run goroutine: the depth-1 queue is the only sink.

	past := ipcAcceptTiming{acceptedAt: time.Now().Add(-time.Hour)}

	// Queue-empty case: the arm must ENQUEUE, not capture. If the arm ran the
	// capture itself, a dump file would exist despite the absent worker.
	trig.maybeArmOnHello(past, nil)
	if names := readStallDumps(t, trig.dir); len(names) != 0 {
		t.Fatalf("arm must not capture on the calling goroutine; found dumps %v with no worker running", names)
	}
	if got := len(trig.requests); got != 1 {
		t.Fatalf("arm must queue exactly one request, queue depth = %d", got)
	}
	if got := trig.dropped.Load(); got != 0 {
		t.Fatalf("first arm with an empty queue must not drop, dropped = %d", got)
	}

	// Queue-full case: with the queue occupied forever, the arm must return
	// promptly (drop-with-counter), never park the connection goroutine.
	done := make(chan struct{})
	go func() {
		defer close(done)
		trig.maybeArmOnHello(past, nil)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("maybeArmOnHello blocked with a full capture queue — the hot path can be wedged by the diagnostic")
	}
	if got := trig.dropped.Load(); got != 1 {
		t.Errorf("queue-full arm must count a drop, dropped = %d, want 1", got)
	}
	if names := readStallDumps(t, trig.dir); len(names) != 0 {
		t.Errorf("still no dump may exist without a worker, got %v", names)
	}
}

// TestStallDumpAutoBudgetIsTerminalEmitsOnceAndKeepsManualArm covers the
// lifetime-budget bounds, which had NO coverage: a mutation deleting the
// auto-budget gate entirely survived the pre-existing suite.
//
// QA-added (2026-07-21 review). Asserts, driving capture() directly on one
// goroutine (no worker, no timing dependence):
//  1. the auto budget is a hard cap (exactly MaxAutoCaptures dumps land);
//  2. post-budget requests are counted, not silently lost;
//  3. the budget-exhausted event is emitted EXACTLY once for the reason;
//  4. the manual budget is separate — an operator capture still works after
//     the auto budget is gone, and its header reports the suppressed count.
func TestStallDumpAutoBudgetIsTerminalEmitsOnceAndKeepsManualArm(t *testing.T) {
	cfg := shortStallDumpConfig(time.Millisecond, 0)
	cfg.MaxAutoCaptures = 2

	deps, logPath := makeTestDeps(t)
	trig := newStallDumpTriggerWithConfig(deps.stateDir, deps.events, cfg)

	autoReq := stallDumpRequest{Reason: stallDumpReasonHelloLatency, At: time.Now()}
	for range 4 {
		trig.capture(autoReq)
	}

	names := readStallDumps(t, trig.dir)
	if len(names) != 2 {
		t.Fatalf("auto budget of 2 must cap dumps at 2, got %d: %v", len(names), names)
	}
	if got := trig.dropped.Load(); got != 2 {
		t.Fatalf("post-budget requests must be counted, dropped = %d, want 2", got)
	}

	raw, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read event log: %v", err)
	}
	log := string(raw)
	if got := strings.Count(log, `"event":"supervisor-stall-dump-budget-exhausted"`); got != 1 {
		t.Fatalf("budget-exhausted must emit exactly once, got %d occurrences:\n%s", got, log)
	}
	if !strings.Contains(log, `"captures_taken":2`) {
		t.Errorf("budget-exhausted event must carry captures_taken=2; log:\n%s", log)
	}

	// Manual arm survives auto exhaustion (separate budget) and reports the
	// suppressed burst so the drops above are never silently invisible.
	trig.capture(stallDumpRequest{Reason: stallDumpReasonOperator, At: time.Now()})
	names = readStallDumps(t, trig.dir)
	if len(names) != 3 {
		t.Fatalf("manual capture must still work after auto budget exhaustion, dumps = %d, want 3 (%v)", len(names), names)
	}
	manual, err := os.ReadFile(filepath.Join(trig.dir, names[2]))
	if err != nil {
		t.Fatalf("read manual dump: %v", err)
	}
	if want := "# suppressed_since_last_capture: 2"; !strings.Contains(string(manual), want) {
		t.Errorf("manual dump must report the auto-suppressed count (%q); header:\n%s", want, firstLines(string(manual), 14))
	}
	if got := trig.dropped.Load(); got != 0 {
		t.Errorf("suppressed count must reset after being reported, got %d", got)
	}
}

// --- 2026-07-21 review fixes -------------------------------------------

// TestStallDumpFileNameUniqueWithinOneClockTick is the FORCED collision for
// FIX-1. The two names are built from a byte-identical timestamp, which is
// not a contrived case: the Windows wall clock advances in ~999.5us steps, so
// 99.94% of consecutive time.Now() calls format to the same nanosecond stamp
// (measured, 200000 samples). Without the sequence number both captures
// resolve to one path and WriteStateFileBytesAtomic's rename REPLACES the
// first dump with no error surfaced anywhere — silent evidence destruction.
func TestStallDumpFileNameUniqueWithinOneClockTick(t *testing.T) {
	at := time.Date(2026, 7, 21, 3, 4, 5, 123456789, time.UTC)

	first := stallDumpFileName(at, stallDumpReasonHelloLatency, 1)
	second := stallDumpFileName(at, stallDumpReasonHelloLatency, 2)

	if first == second {
		t.Fatalf("same-tick captures collide on one path (%s) — the later dump would silently replace the earlier", first)
	}
	// Retention prunes by lexicographic order, so within one tick the
	// sequence must still sort chronologically.
	if !(first < second) {
		t.Errorf("same-tick names must sort in capture order: %s must sort before %s", first, second)
	}
	// And across ticks the timestamp must still dominate the sequence, or
	// retention would drop the wrong file.
	later := stallDumpFileName(at.Add(time.Second), stallDumpReasonHelloLatency, 1)
	if !(second < later) {
		t.Errorf("timestamp must dominate sequence across ticks: %s must sort before %s", second, later)
	}
}

// TestStallDumpRapidCapturesEachPersist is the capture-level half of FIX-1:
// a burst of captures must yield one file per capture, with distinct names,
// regardless of how coarse the wall clock is.
//
// The distinct-FILENAME check alone is NOT sufficient and must not be
// trusted on its own: a mutant that never increments the sequence survived
// it, because each capture's file write happens to outlast a clock tick on
// this host, so the timestamps differed anyway. That is passing by timing
// luck. The per-capture SEQUENCE assertion below is the part with teeth — it
// holds whatever the clock does.
func TestStallDumpRapidCapturesEachPersist(t *testing.T) {
	const burst = 6
	cfg := shortStallDumpConfig(time.Millisecond, 0)
	cfg.RetainFiles = burst + 1 // keep pruning out of this assertion

	deps, _ := makeTestDeps(t)
	trig := newStallDumpTriggerWithConfig(deps.stateDir, deps.events, cfg)

	req := stallDumpRequest{Reason: stallDumpReasonHelloLatency, At: time.Now()}
	for range burst {
		trig.capture(req)
	}

	names := readStallDumps(t, trig.dir)
	if len(names) != burst {
		t.Fatalf("every capture must persist its own dump: got %d files for %d captures: %v", len(names), burst, names)
	}
	seen := map[string]bool{}
	for _, n := range names {
		if seen[n] {
			t.Fatalf("duplicate dump filename %s", n)
		}
		seen[n] = true
	}

	// Clock-independent: every capture must carry its OWN sequence value.
	// Name shape is stall-<timestamp>-<seq>-<reason>.txt, so field 2 is the
	// sequence (the reason itself contains '-', the timestamp does not).
	seqs := map[string]bool{}
	for _, n := range names {
		parts := strings.Split(n, "-")
		if len(parts) < 4 {
			t.Fatalf("dump filename %s has no sequence field", n)
		}
		seq := parts[2]
		if seqs[seq] {
			t.Fatalf("sequence %q reused across captures (names: %v) — two captures in one clock tick would collide and the later would silently replace the earlier", seq, names)
		}
		seqs[seq] = true
	}
}

// TestStallDumpSentinelUnconsumableIsReportedAndRateLimited covers FIX-2: the
// manual arm must not die silently when it can see the sentinel but cannot
// consume it. A directory at the sentinel path reproduces that shape
// portably — os.Stat succeeds, os.Remove fails on a non-empty directory —
// standing in for the real causes (read-only attribute, held-open handle,
// delete-denying DACL on a broadened %LOCALAPPDATA%).
func TestStallDumpSentinelUnconsumableIsReportedAndRateLimited(t *testing.T) {
	deps, logPath := makeTestDeps(t)
	trig := newStallDumpTriggerWithConfig(deps.stateDir, deps.events,
		shortStallDumpConfig(time.Millisecond, time.Hour))

	// Unconsumable sentinel: a non-empty directory at the sentinel path.
	sentinel := filepath.Join(deps.stateDir, stallDumpSentinelLeaf)
	if err := os.MkdirAll(sentinel, 0o700); err != nil {
		t.Fatalf("create sentinel dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(sentinel, "occupant"), nil, 0o600); err != nil {
		t.Fatalf("occupy sentinel dir: %v", err)
	}

	startStallDumpWorkers(t, trig, deps.stateDir, true)

	// The FIRST failure must surface immediately, not after a backoff.
	waitForSupervisorEvent(t, logPath, `"event":"supervisor-stall-dump-sentinel-unconsumable"`, 3*time.Second)

	// Let many more poll ticks elapse (10ms interval), then assert the
	// reporting is rate-limited rather than one row per tick.
	time.Sleep(400 * time.Millisecond)

	raw, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read event log: %v", err)
	}
	got := strings.Count(string(raw), `"event":"supervisor-stall-dump-sentinel-unconsumable"`)
	if got < 1 {
		t.Fatalf("an unconsumable sentinel must be reported, got %d rows", got)
	}
	// ~40+ ticks elapsed; powers-of-two reporting caps this near 6.
	if got > 10 {
		t.Errorf("unconsumable-sentinel reporting is not rate-limited: %d rows across ~40 poll ticks", got)
	}
	// A human asked for a capture and did not get one — that belongs in the
	// same suppression accounting as every other missed capture.
	if trig.dropped.Load() < 1 {
		t.Error("an unconsumable sentinel must count as a dropped capture request")
	}
	if names := readStallDumps(t, trig.dir); len(names) != 0 {
		t.Errorf("no dump may be written when the sentinel was never consumed, got %v", names)
	}
}

// TestSuperviseDumpStacksVerbFiresTheManualArm covers FIX-4 end to end: the
// operator verb must create exactly the sentinel the supervisor's watcher
// consumes, and must do so without any IPC. There is deliberately no
// listener and no accept loop here — if the verb depended on IPC, this could
// not pass.
func TestSuperviseDumpStacksVerbFiresTheManualArm(t *testing.T) {
	deps, _ := makeTestDeps(t)
	trig := newStallDumpTriggerWithConfig(deps.stateDir, deps.events,
		shortStallDumpConfig(time.Millisecond, time.Hour))

	startStallDumpWorkers(t, trig, deps.stateDir, true)

	// This is what `mcphub supervise dump-stacks` does.
	sentinel, err := fileStallDumpRequest(deps.stateDir)
	if err != nil {
		t.Fatalf("file dump-stacks request: %v", err)
	}
	if want := filepath.Join(deps.stateDir, stallDumpSentinelLeaf); sentinel != want {
		t.Fatalf("verb wrote %s, but the watcher polls %s", sentinel, want)
	}

	names := waitForStallDump(t, trig.dir, 1, 3*time.Second)
	raw, err := os.ReadFile(filepath.Join(trig.dir, names[0]))
	if err != nil {
		t.Fatalf("read dump: %v", err)
	}
	if !strings.Contains(string(raw), "# reason: operator-sentinel") {
		t.Errorf("verb must fire the OPERATOR arm; header:\n%s", firstLines(string(raw), 14))
	}
}

// TestSuperviseDumpStacksCmdIsRegistered pins the operator affordance: the
// whole point of FIX-4 is discoverability, so the verb must actually hang
// off `mcphub supervise` and take no arguments.
func TestSuperviseDumpStacksCmdIsRegistered(t *testing.T) {
	sup := newSuperviseCmd()
	var found *cobra.Command
	for _, c := range sup.Commands() {
		if c.Name() == "dump-stacks" {
			found = c
			break
		}
	}
	if found == nil {
		t.Fatal("`mcphub supervise dump-stacks` is not registered — the manual arm is unreachable for an operator")
	}
	if found.Short == "" {
		t.Error("dump-stacks needs a Short description to be discoverable in --help")
	}
	if err := found.Args(found, []string{"unexpected"}); err == nil {
		t.Error("dump-stacks must take no positional arguments")
	}
}
