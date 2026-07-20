package cli

import (
	"context"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync/atomic"
	"time"

	"mcp-local-hub/internal/api"
)

// Goroutine-stack capture for the supervisor IPC accept-path stall class
// (work-items/bugs/2026-07-20-supervisor-hello-latency-vs-client-budget.md).
//
// THE PROBLEM THIS SOLVES. On the operator's live host the supervisor
// accepts an IPC connection essentially instantly (0-62 ms) but then takes
// median 2.1 s / p90 13.7 s / max 65.3 s to write an 80-byte hello frame.
// The GUI status client budgets 5 s for the WHOLE exchange
// (internal/api/supervisor_ipc_status_client.go:56), so 14 of 20 polls fail
// and the dashboard flaps red while MCP itself keeps working. WHY the accept
// path stalls is unknown and NOT obtainable from the deployed build: the
// blocking site needs a goroutine stack, and read-only probing cannot reach
// it. This file makes the stall observable. It does NOT fix the stall and it
// does NOT touch the client budget — both are deliberately out of scope.
//
// THE DESIGN CONSTRAINT THAT SHAPES EVERYTHING HERE: the trigger must not
// depend on the path that stalls. An IPC command (`dump-stacks`) is exactly
// wrong — IPC accept IS the stalled path, so the command cannot get in
// precisely when it is needed. Neither arm below requires the supervisor to
// service a new connection:
//
//   - The AUTOMATIC arm is a passive measurement on a connection that has
//     ALREADY been accepted. It observes; it never asks for service.
//   - The MANUAL arm is a sentinel file polled by an independent goroutine.
//     It touches the filesystem only.
//
// A self-probe dial was considered and REJECTED: it would exercise the
// suspect path, but the bug file records a positive-feedback path where each
// abandoned dial consumes an accept cycle and emits another error row
// (bug file "The contract defect"), so a periodic self-dial would worsen the
// condition it measures.
//
// WHAT THE TRIGGER SHARES WITH THE STALLED PATH — stated explicitly, because
// a trigger that shares the blocking resource is the same defect wearing a
// different hat:
//
//   - The hot path (serveIPCConn) does exactly two things: one monotonic
//     time delta and one NON-BLOCKING channel send. It takes no mutex,
//     no flock, and never allocates the dump buffer. It cannot block.
//   - Capture runs on ONE dedicated goroutine, off both the accept loop and
//     the per-connection goroutines. runtime.Stack(all=true) stops the world
//     for the duration of the copy; that cost is real and is the accepted
//     price of the diagnostic. Serializing it on one goroutine plus the
//     cooldown below bounds how often it is paid.
//   - The capture goroutine's event emit CAN park on the event log's
//     in-process mutex (SupervisorEventLog.emit takes a blocking Lock before
//     its timeout applies — see the serveIPCConn TryEmit note in
//     supervise.go). That parks only the capture goroutine, never the
//     supervisor, and the dump FILE is written BEFORE the emit, so a wedged
//     event log costs the pointer row, never the evidence.
//   - The dump file goes through api.WriteStateFileBytesAtomic, whose flock
//     leaf is `<dumpfile>.lock`. Each capture writes a UNIQUELY TIMESTAMPED
//     file, and the single capture goroutine is the only writer, so that
//     flock has no contender by construction. It is NOT the shared
//     state-dir lock, and it is not the supervisor event-log flock.
//
// RELATIONSHIP TO PR #570 (stderr sink) — declared, not assumed. #570 adds a
// stderr sink so Go's CTRL_BREAK_EVENT stack dump has a destination. That is
// an operator-driven, whole-process mechanism with its own open-time 10 MB
// rotation shared with everything else the process writes to stderr. This
// file writes its own per-capture files instead, for three reasons: (1) a
// dedicated file carries a structured header (which arm fired, the measured
// latency, the suppression count) that an undifferentiated stderr stream
// cannot; (2) per-capture files get per-capture retention, so a recurring
// stall cannot evict unrelated stderr; (3) it must work on a build where
// #570 is absent. Both ship in the same release with unknown merge order, so
// nothing here references #570's sink — if #570 lands first the two are
// complementary (its sink still serves CTRL_BREAK), and if it does not, this
// arm degrades to exactly nothing.
const (
	// stallDumpDirLeaf is the per-capture dump directory under <state-dir>.
	stallDumpDirLeaf = "supervisor-stalls"

	// stallDumpSentinelLeaf is the MANUAL trigger. An operator watching the
	// panel flap creates <state-dir>/supervisor-dump-stacks (any content,
	// including empty) and the supervisor captures within one poll interval.
	//
	// A sentinel file is the right primitive precisely because it touches
	// nothing the stall touches: no pipe, no accept cycle, no supervisor
	// mutex, no event loop. Alternatives considered and rejected:
	//   - IPC command: disqualified by the design constraint above.
	//   - OS signal: on Windows the supervisor is spawned DETACHED_PROCESS |
	//     CREATE_NEW_PROCESS_GROUP and per Microsoft docs cannot receive
	//     CTRL_C_EVENT from another process (see the cold-restart flow in
	//     CLAUDE.md), so a signal-based trigger is not deliverable on the
	//     one platform where the bug reproduces.
	//   - Watching the file's mtime instead of consuming it: leaves the
	//     trigger armed, so one operator touch fires every poll forever.
	stallDumpSentinelLeaf = "supervisor-dump-stacks"
)

// stallDumpConfig holds the capture tunables.
//
// It is a per-trigger VALUE, snapshotted at construction and never mutated
// afterwards, rather than a set of package-level vars. That is deliberate: a
// reusable leaf must hold no mutable process-global state. Package-level
// tunables would be written by tests while the accept path's per-connection
// goroutines still read them — which is a genuine data race, not a test
// artifact (it was caught by `go test -race` during this change, with the
// read at maybeArmOnHello and the write at a test cleanup). Per-trigger
// config removes the shared mutable state entirely instead of papering over
// it with a mutex.
type stallDumpConfig struct {
	// AcceptHelloThreshold arms the AUTOMATIC capture: a connection
	// whose accept-to-hello interval reaches this is treated as a stall.
	//
	// It is anchored to a CONTRACT BOUNDARY, not chosen as a round number.
	// 5 s is the GUI status client's budget for the entire exchange
	// (internal/api/supervisor_ipc_status_client.go:56). At or above it, the
	// poll has already failed for the operator — so every capture
	// corresponds one-to-one with a real, user-visible failure, and no
	// capture is spent on a poll nobody noticed.
	//
	// Why not the measured median (2 142 ms): 20 of 40 dials exceeded 2 s,
	// so a median-anchored threshold fires on roughly half of all polls —
	// mostly on exchanges that still SUCCEEDED. Why not p90 (13 705 ms):
	// that abandons the 5-15 s band, which is both the common case and
	// exactly the band the 60 s heartbeat cannot resolve (bug file, "Bearing
	// on PR #570"). Why strictly below the server's own 10 s hello deadline
	// (ipcHelloWriteTimeout, supervise_ipc_common.go:23): the client aborts
	// first, so that server-side bound is unreachable in production and
	// never classifies the failure — capturing at 5 s puts the dump inside
	// the window where the stall is live rather than after the client has
	// already given up on it.
	//
	// The threshold decides WHAT COUNTS as a stall. It deliberately does not
	// carry the flood protection; Cooldown does.
	AcceptHelloThreshold time.Duration

	// Cooldown is the minimum spacing between AUTOMATIC captures.
	// The bug file reports the stall recurring on a 30-90 s cadence, so 60 s
	// yields at most one capture per stall episode. Stall-then-drain makes
	// this load-bearing: 8 parallel connects all completed at 11 s and 7
	// hellos landed within 5 ms of each other, so ONE episode can arm the
	// threshold 8 times in 5 ms. Without the cooldown that episode alone
	// would write 8 near-identical whole-process dumps.
	//
	// The MANUAL arm deliberately bypasses this: a human touching a file is
	// inherently rate-limited, and an operator who asks for a capture NOW
	// must not be silently told to wait.
	Cooldown time.Duration

	// Per-process capture budgets, counted separately so an automatic flood
	// can never consume the operator's ability to ask for a dump by hand.
	MaxAutoCaptures   int
	MaxManualCaptures int

	// MaxBytes caps ONE dump. A whole-process dump of a few hundred
	// goroutines runs to low hundreds of KB; 4 MiB is headroom, not a
	// target. Worst case on disk is bounded by this times RetainFiles
	// (32 MiB), comparable to the event log's own 10 MB + 10 MB backfile.
	MaxBytes int

	// RetainFiles bounds the dump directory. Oldest are pruned.
	RetainFiles int

	// SentinelPollInterval bounds operator latency on the manual arm. A
	// stat() every 2 s is free relative to everything else the supervisor
	// does, and 2 s keeps the capture inside a stall the operator is
	// actively watching.
	SentinelPollInterval time.Duration
}

// defaultStallDumpConfig is the production tuning. See the field docs above
// for the reasoning behind each number.
func defaultStallDumpConfig() stallDumpConfig {
	return stallDumpConfig{
		AcceptHelloThreshold: 5 * time.Second,
		Cooldown:             60 * time.Second,
		MaxAutoCaptures:      20,
		MaxManualCaptures:    20,
		MaxBytes:             4 << 20,
		RetainFiles:          8,
		SentinelPollInterval: 2 * time.Second,
	}
}

type stallDumpReason string

const (
	stallDumpReasonHelloLatency stallDumpReason = "accept-hello-latency"
	stallDumpReasonOperator     stallDumpReason = "operator-sentinel"
)

// stallDumpRequest is what the hot path hands to the capture goroutine.
type stallDumpRequest struct {
	Reason stallDumpReason
	At     time.Time
	Body   map[string]any
}

// ipcAcceptTiming carries the per-connection accept timestamps from the
// accept loop into serveIPCConn. Both fields are measured on the accept
// loop's own goroutine so neither depends on when the per-connection
// goroutine happens to be scheduled.
type ipcAcceptTiming struct {
	// acceptedAt is when listener.Accept() returned this connection.
	acceptedAt time.Time

	// acceptDwell is how long THIS loop iteration sat inside Accept().
	//
	// It is recorded but NOT armed on, because on an idle supervisor a long
	// dwell just means nobody dialed. Its diagnostic value is in the dump
	// header: it discriminates the two candidate stall sites the bug file
	// leaves open. A capture whose accept_to_hello_ms is large while
	// accept_dwell_ms is small says the connection was delivered promptly
	// and the delay is AFTER accept. A batch of captures that all show a
	// large dwell says Accept() itself was delivering late — the
	// stall-then-drain shape, which points at go-winio's listenerRoutine.
	acceptDwell time.Duration
}

// WHEN THE AUTOMATIC ARM ACTUALLY FIRES — the honest limit, widened per the
// 2026-07-21 review. `ASSUMPTION (UNVERIFIED)` until the first real capture.
//
// The arm runs where WriteHello RETURNS (supervise.go, serveIPCConn), so the
// dump is taken at the END of the observed episode, not necessarily during
// it. There are three cases, and only one is a true mid-stall capture:
//
//  1. The server goroutine is parked INSIDE the pipe write when the client
//     abandons at its 5 s budget. WriteHello then returns ERROR_NO_DATA and
//     the arm fires while the underlying condition is still live. This is a
//     genuine mid-stall capture, and per the bug file's mechanism it is
//     plausibly the common case — but that is not guaranteed.
//  2. The stall sits AFTER accept but BEFORE the write begins (goroutine
//     never scheduled, or blocked on something upstream of the write). The
//     arm still fires, but only once the goroutine finally runs — the
//     drain edge again.
//  3. The stall is upstream of Accept() RETURNING. The per-connection
//     goroutine does not exist yet, so nothing observes the episode until
//     the batch drains; the arm then fires with a large accept_dwell_ms, or
//     does not fire at all if the post-accept path is fast.
//
// Cases 2 and 3 yield an end-of-episode dump, which still shows where every
// OTHER goroutine is parked and still carries accept_dwell_ms as the
// discriminator — weaker evidence than a mid-stall stack, but not nothing.
// The operator sentinel arm exists precisely to cover them: a human watching
// the panel flap can capture at a moment of their choosing, with no
// dependence on any connection completing. Settling probe: read
// accept_dwell_ms across the first real captures on the affected host.

// stallDumpTrigger owns the capture goroutine and its budgets.
//
// The counters below (autoCaptured, manualCaptured, lastAutoAt,
// budgetExhaustedEmitted) are read and written ONLY by the single capture
// goroutine in Run, so they need no synchronisation. `dropped` is the sole
// cross-goroutine field and is atomic.
type stallDumpTrigger struct {
	dir      string
	events   *api.SupervisorEventLog
	requests chan stallDumpRequest

	// cfg is immutable after construction: every reader below (including
	// the per-connection goroutines calling maybeArmOnHello) reads it
	// without synchronisation, which is only sound because nothing ever
	// writes it after newStallDumpTrigger returns.
	cfg stallDumpConfig

	// dropped counts every request that did NOT produce a dump — whether
	// the hot path could not hand it off (queue busy) or the capture
	// goroutine gated it (cooldown / budget). It is reset into the header
	// of the next successful capture, so a suppressed burst is never
	// silently invisible: the operator reading any dump sees how many
	// stalls were observed but not captured since the previous one.
	dropped atomic.Int64

	autoCaptured           int
	manualCaptured         int
	lastAutoAt             time.Time
	budgetExhaustedEmitted map[stallDumpReason]bool

	// seq disambiguates dump filenames within one wall-clock tick — see
	// stallDumpFileName. A plain counter suffices because only the single
	// capture goroutine ever increments it.
	seq uint64

	// sentinelFailures counts consecutive failed attempts to consume the
	// operator sentinel, so the warn event can be rate-limited to once per
	// escalating run rather than emitted every poll. Owned by the sentinel
	// watcher goroutine.
	sentinelFailures int
}

func newStallDumpTrigger(stateDir string, events *api.SupervisorEventLog) *stallDumpTrigger {
	return newStallDumpTriggerWithConfig(stateDir, events, defaultStallDumpConfig())
}

// newStallDumpTriggerWithConfig is the seam the tests use to compress the
// tunables without touching process-global state.
func newStallDumpTriggerWithConfig(stateDir string, events *api.SupervisorEventLog, cfg stallDumpConfig) *stallDumpTrigger {
	return &stallDumpTrigger{
		dir:    filepath.Join(stateDir, stallDumpDirLeaf),
		events: events,
		cfg:    cfg,
		// Depth 1. A second request while one is queued or in flight is
		// dropped-with-counter rather than buffered: buffering would only
		// queue near-identical whole-process dumps of the same episode.
		requests:               make(chan stallDumpRequest, 1),
		budgetExhaustedEmitted: map[stallDumpReason]bool{},
	}
}

// Request is the ONLY entry point reachable from the IPC hot path.
//
// It never blocks and never allocates a dump buffer. Nil-receiver safe so
// every existing call site that constructs a bare ipcDispatchDeps{} (the
// unit tests) keeps working unchanged.
func (t *stallDumpTrigger) Request(req stallDumpRequest) {
	if t == nil {
		return
	}
	select {
	case t.requests <- req:
	default:
		t.dropped.Add(1)
	}
}

// maybeArmOnHello is the AUTOMATIC arm. Called from serveIPCConn after the
// hello write returns, on BOTH the success and the error path.
//
// Arming on the error path is not incidental — it is the primary case. The
// bug's mechanism ends with the client abandoning at 5 s and the server's
// still-pending write hitting a closing pipe (ERROR_NO_DATA, "write hello:
// The pipe is being closed."). A capture armed only on success would miss
// exactly the exchanges the operator sees fail.
func (t *stallDumpTrigger) maybeArmOnHello(timing ipcAcceptTiming, helloErr error) {
	if t == nil || timing.acceptedAt.IsZero() {
		return
	}
	elapsed := time.Since(timing.acceptedAt)
	if elapsed < t.cfg.AcceptHelloThreshold {
		return
	}
	body := map[string]any{
		"accept_to_hello_ms": elapsed.Milliseconds(),
		"accept_dwell_ms":    timing.acceptDwell.Milliseconds(),
		"threshold_ms":       t.cfg.AcceptHelloThreshold.Milliseconds(),
		"hello_ok":           helloErr == nil,
	}
	if helloErr != nil {
		body["hello_err"] = helloErr.Error()
	}
	t.Request(stallDumpRequest{
		Reason: stallDumpReasonHelloLatency,
		At:     time.Now(),
		Body:   body,
	})
}

// Run is the dedicated capture goroutine. Exits on ctx cancellation
// (loopCtx in runSupervise, cancelled by its deferred loopCancel).
func (t *stallDumpTrigger) Run(ctx context.Context) {
	if t == nil {
		return
	}
	for {
		select {
		case <-ctx.Done():
			return
		case req := <-t.requests:
			t.capture(req)
		}
	}
}

// capture applies the budgets, then writes the dump. Runs ONLY on the Run
// goroutine.
func (t *stallDumpTrigger) capture(req stallDumpRequest) {
	now := time.Now()

	if refusal := t.gate(req.Reason, now); refusal != "" {
		t.dropped.Add(1)
		// Deliberately NO per-suppression event row. Emitting one per
		// suppressed request would reproduce the audit-flood the bug file
		// already flags. The suppression count rides into the header of the
		// next successful capture instead. The ONE exception is a budget
		// running out, which is terminal and operator-actionable, so it is
		// emitted exactly once per reason.
		if strings.HasPrefix(refusal, "budget-exhausted") && !t.budgetExhaustedEmitted[req.Reason] {
			t.budgetExhaustedEmitted[req.Reason] = true
			t.emit(api.SupervisorEvent{
				Severity: "warn",
				Source:   "ipc",
				Event:    "supervisor-stall-dump-budget-exhausted",
				Body: map[string]any{
					"reason":            string(req.Reason),
					"captures_taken":    t.takenFor(req.Reason),
					"restart_to_re_arm": true,
				},
			})
		}
		return
	}

	dump, truncated := captureGoroutineDump(t.cfg.MaxBytes)
	suppressed := t.dropped.Swap(0)

	header := buildStallDumpHeader(req, now, len(dump), truncated, suppressed)
	t.seq++
	path := filepath.Join(t.dir, stallDumpFileName(now, req.Reason, t.seq))

	// Evidence first, bookkeeping second: count the capture and start the
	// cooldown only if the bytes actually landed. A failed write must not
	// burn the budget.
	if err := api.WriteStateFileBytesAtomic(path, append(header, dump...)); err != nil {
		t.emit(api.SupervisorEvent{
			Severity: "error",
			Source:   "ipc",
			Event:    "supervisor-stall-dump-write-failed",
			Body: map[string]any{
				"reason": string(req.Reason),
				"path":   path,
				"err":    err.Error(),
			},
		})
		// The suppression count was already swapped out of the counter;
		// fold it back so it is reported by whichever capture succeeds next
		// rather than being lost to a transient write failure.
		t.dropped.Add(suppressed + 1)
		return
	}

	switch req.Reason {
	case stallDumpReasonOperator:
		t.manualCaptured++
	default:
		t.autoCaptured++
		t.lastAutoAt = now
	}

	body := map[string]any{
		"reason":                        string(req.Reason),
		"path":                          path,
		"dump_bytes":                    len(dump),
		"truncated":                     truncated,
		"suppressed_since_last_capture": suppressed,
	}
	for k, v := range req.Body {
		body[k] = v
	}
	t.emit(api.SupervisorEvent{
		Severity: "warn",
		Source:   "ipc",
		Event:    "supervisor-stall-dump-captured",
		Body:     body,
	})

	t.prune()
}

// gate returns "" to allow the capture, or a short refusal reason.
func (t *stallDumpTrigger) gate(reason stallDumpReason, now time.Time) string {
	if reason == stallDumpReasonOperator {
		if t.manualCaptured >= t.cfg.MaxManualCaptures {
			return "budget-exhausted-manual"
		}
		// The manual arm intentionally skips the cooldown — see
		// stallDumpCooldown.
		return ""
	}
	if t.autoCaptured >= t.cfg.MaxAutoCaptures {
		return "budget-exhausted-auto"
	}
	if !t.lastAutoAt.IsZero() && now.Sub(t.lastAutoAt) < t.cfg.Cooldown {
		return "cooldown"
	}
	return ""
}

func (t *stallDumpTrigger) takenFor(reason stallDumpReason) int {
	if reason == stallDumpReasonOperator {
		return t.manualCaptured
	}
	return t.autoCaptured
}

// emit uses a BOUNDED wait. A blocking Emit could park the capture goroutine
// indefinitely behind a wedged event-log flock; TryEmit would drop the
// pointer row on momentary contention. EmitWithTimeout rides out normal
// contention without unbounded parking. The dump file is already durable at
// this point, so losing this row costs discoverability, never evidence.
func (t *stallDumpTrigger) emit(evt api.SupervisorEvent) {
	if t == nil || t.events == nil {
		return
	}
	_ = t.events.EmitWithTimeout(evt, 2*time.Second)
}

// captureGoroutineDump grows the buffer until the whole-process dump fits or
// the cap is reached. runtime.Stack truncates rather than failing, so an
// oversize dump yields partial output plus truncated=true — recorded in the
// header so a reader never mistakes a cut-off dump for a complete one.
// The starting size matters for cost, not just allocation: every undersized
// attempt is a WASTED whole-process stop-the-world traceback pass. Measured
// at a supervisor-like ~310 goroutines the dump is ~900 KB, so a 256 KiB
// start paid 3 full STW passes (256 KiB, 512 KiB, 1 MiB) where 1 MiB pays 1.
// At 20.8 ms average STW per pass that is ~42 ms of avoidable pause per
// capture. Starting at 1 MiB covers the realistic supervisor case in a
// single pass and still grows for an outlier.
func captureGoroutineDump(maxBytes int) (dump []byte, truncated bool) {
	size := 1 << 20
	if size > maxBytes {
		size = maxBytes
	}
	for {
		buf := make([]byte, size)
		n := runtime.Stack(buf, true)
		if n < len(buf) || size >= maxBytes {
			return buf[:n], n >= len(buf)
		}
		size *= 2
		if size > maxBytes {
			size = maxBytes
		}
	}
}

// stallDumpFileName produces a lexicographically-chronological name so
// retention can sort by name. Colons are illegal in Windows filenames, so
// the timestamp is the compact form rather than RFC3339.
//
// seq is a per-trigger monotonic counter and is LOAD-BEARING, not cosmetic.
// The nanosecond-formatted timestamp is NOT unique: the Windows wall clock
// advances in ~999.5 microsecond steps, so 99.94 % of consecutive
// time.Now() calls format to an identical stamp (measured on this host,
// 200 000 samples). Two same-reason captures inside one clock tick would
// then produce the same path, and WriteStateFileBytesAtomic's rename would
// REPLACE the earlier dump — silently destroying evidence, which is the
// worst failure available to a mechanism that exists to preserve it.
//
// Production defaults cannot collide (60 s cooldown, 2 s sentinel poll,
// distinct per-reason suffixes), but that is a property of the tuning, not
// of the code — and the "no flock contender by construction" argument in
// this file's header rests on path uniqueness. The counter makes the
// invariant structural instead of config-fragile.
//
// seq is placed AFTER the timestamp so lexicographic ordering stays
// chronological for retention. It is zero-padded so 10 does not sort before
// 2 within the same tick.
func stallDumpFileName(at time.Time, reason stallDumpReason, seq uint64) string {
	return fmt.Sprintf("stall-%s-%06d-%s.txt",
		at.UTC().Format("20060102T150405.000000000Z"), seq, reason)
}

// buildStallDumpHeader writes the machine-greppable preamble. Every line is
// `# key: value` so the header can be read with a grep and the body handed
// straight to a stack reader.
func buildStallDumpHeader(
	req stallDumpRequest,
	at time.Time,
	dumpBytes int,
	truncated bool,
	suppressed int64,
) []byte {
	var b strings.Builder
	b.WriteString("# mcphub supervisor goroutine stall dump\n")
	fmt.Fprintf(&b, "# captured_at: %s\n", at.UTC().Format(time.RFC3339Nano))
	fmt.Fprintf(&b, "# reason: %s\n", req.Reason)
	fmt.Fprintf(&b, "# pid: %d\n", os.Getpid())
	fmt.Fprintf(&b, "# goroutines: %d\n", runtime.NumGoroutine())
	fmt.Fprintf(&b, "# dump_bytes: %d\n", dumpBytes)
	fmt.Fprintf(&b, "# truncated: %t\n", truncated)
	// Durable even if the event row below is lost to a wedged event log.
	fmt.Fprintf(&b, "# suppressed_since_last_capture: %d\n", suppressed)
	// Sorted so the header is byte-stable across captures — a reader
	// diffing two dumps sees real differences, not map-iteration order.
	for _, k := range slices.Sorted(maps.Keys(req.Body)) {
		fmt.Fprintf(&b, "# %s: %v\n", k, req.Body[k])
	}
	b.WriteString("#\n")
	return []byte(b.String())
}

// prune keeps the newest stallDumpRetainFiles dumps and removes the rest,
// along with the per-file flock leaf WriteStateFileBytesAtomic creates. All
// failures are non-fatal: a dump directory that cannot be pruned is a disk
// nuisance, never a reason to stop capturing.
func (t *stallDumpTrigger) prune() {
	entries, err := os.ReadDir(t.dir)
	if err != nil {
		return
	}
	var dumps []string
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasPrefix(name, "stall-") || !strings.HasSuffix(name, ".txt") {
			continue
		}
		dumps = append(dumps, name)
	}
	if len(dumps) <= t.cfg.RetainFiles {
		return
	}
	slices.Sort(dumps)
	for _, name := range dumps[:len(dumps)-t.cfg.RetainFiles] {
		_ = os.Remove(filepath.Join(t.dir, name))
		_ = os.Remove(filepath.Join(t.dir, name+".lock"))
	}
}

// runStallDumpSentinelWatcher is the MANUAL arm.
//
// It shares nothing with the IPC accept path: its own goroutine, its own
// ticker, and a plain os.Stat / os.Remove pair against one path. No pipe, no
// accept cycle, no supervisor mutex, no event loop, no state-dir flock.
//
// Consume-by-remove: the sentinel is removed BEFORE the capture is
// requested, and a capture is requested ONLY if the removal succeeded. That
// ordering makes one operator touch produce exactly one capture, and a file
// the watcher cannot consume can never drive a capture-per-tick loop.
//
// A consume FAILURE is reported, not swallowed. os.Stat succeeding while
// os.Remove fails is reachable in practice — a read-only attribute, a handle
// the operator still holds open, or a delete-denying parent DACL, and a
// broadened %LOCALAPPDATA% DACL is a documented recurring condition on the
// affected host class (see the corp-policy posture in CLAUDE.md). Silently
// continuing would leave the manual arm dead exactly where it matters most,
// and this arm is the designated backstop for the automatic arm's disclosed
// blind spot. That would also contradict this file's "nothing drops
// silently" contract.
//
// The report is rate-limited on an exponential-ish schedule (1st, 2nd, 4th,
// 8th, ... consecutive failure) so a permanently unconsumable file cannot
// flood the event log at one row every poll interval, while the FIRST
// failure is always surfaced immediately.
func runStallDumpSentinelWatcher(ctx context.Context, stateDir string, trig *stallDumpTrigger) {
	if trig == nil {
		return
	}
	path := filepath.Join(stateDir, stallDumpSentinelLeaf)
	ticker := time.NewTicker(trig.cfg.SentinelPollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if _, err := os.Stat(path); err != nil {
				trig.sentinelFailures = 0
				continue
			}
			if err := os.Remove(path); err != nil {
				trig.sentinelFailures++
				if isPowerOfTwo(trig.sentinelFailures) {
					// dropped is incremented too: an operator capture was
					// requested by a human and did not happen, so it belongs
					// in the same suppression accounting every other missed
					// capture uses.
					trig.dropped.Add(1)
					trig.emit(api.SupervisorEvent{
						Severity: "warn",
						Source:   "ipc",
						Event:    "supervisor-stall-dump-sentinel-unconsumable",
						Body: map[string]any{
							"path":                 path,
							"err":                  err.Error(),
							"consecutive_failures": trig.sentinelFailures,
							"effect":               "operator-requested stack capture did NOT run; remove this file manually to re-arm",
						},
					})
				}
				continue
			}
			trig.sentinelFailures = 0
			trig.Request(stallDumpRequest{
				Reason: stallDumpReasonOperator,
				At:     time.Now(),
				Body:   map[string]any{"sentinel": path},
			})
		}
	}
}

// isPowerOfTwo drives the sentinel failure-report backoff: report on the
// 1st, 2nd, 4th, 8th ... consecutive failure.
func isPowerOfTwo(n int) bool { return n > 0 && n&(n-1) == 0 }
