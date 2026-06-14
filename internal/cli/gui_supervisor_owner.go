package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"mcp-local-hub/internal/api"
)

// supervisorOwner manages a supervisor process from the GUI side.
//
// Two modes:
//
//   - spawned=true: the GUI process started `mcphub supervise` as a
//     detached child during ensureSupervisorRunning(). GUI owns the
//     shutdown — Stop() will send IPC exit{graceful:true} and wait,
//     then force-kill on grace-window expiry.
//
//   - spawned=false: a supervisor was already running when the GUI
//     started (lock owner sidecar + live IPC handshake). GUI adopts
//     the existing process for status reads only; Stop() is a no-op
//     so the original owner's shutdown contract is preserved.
//
// The owner is thread-safe — Stop() is idempotent and safe to call
// concurrently with passive status reads.
type supervisorOwner struct {
	spawned     bool
	proc        *os.Process
	stateDir    string
	stoppedOnce sync.Once
	stoppedErr  error

	// PR #212 r6 reliability finding 1: monitor channel used by the
	// background watcher to publish the supervisor's exit. Buffered
	// 1 so Wait() never blocks. stop() drains this channel instead
	// of launching its own Wait goroutine; if the watcher fires
	// before stop() is called (supervisor crashed mid-runtime), the
	// monitor logs the unexpected exit to stderr with the captured
	// stderr tail so the operator sees the actionable diagnostic
	// instead of an empty Dashboard.
	exitedCh      chan exitInfo
	stderrBuf     *boundedBuffer
	stderrSink    io.Writer
	stopRequested atomic.Bool // set by Stop before signaling exit; read by monitor to classify expected vs unexpected
}

// exitInfo carries the result of cmd.Wait into stop() (or the early-
// exit monitor). exitErr is nil for clean exit code 0, non-nil
// otherwise — *exec.ExitError carries the platform-specific exit
// code in its Sys() field.
type exitInfo struct {
	exitErr error
}

// supervisorReadyPollInterval bounds how often ensureSupervisorRunning
// re-probes the supervisor IPC pipe while waiting for a freshly-spawned
// supervisor to bind. 200ms is the same cadence sendIPCWithResponse
// uses in the migration install path for similar reachability waits.
const supervisorReadyPollInterval = 200 * time.Millisecond

// ensureSupervisorRunning verifies a supervisor is reachable via IPC.
// If not, it spawns `mcphub supervise` as a detached child (using the
// caller-provided binary path so dev / installed builds work) and
// waits up to `waitFor` for the new process to bind its IPC pipe.
//
// Cross-platform spawn detach lives in
// gui_supervisor_owner_{windows,other}.go via the
// configureSupervisorDetach hook.
//
// On error the spawned process (if any) is killed and reaped so this
// function never leaks zombies. The caller is responsible for calling
// Stop on the returned owner during shutdown.
// stderrSinkFromContext returns the io.Writer the GUI uses to log
// supervisor monitor warnings. Defaults to os.Stderr if the caller
// did not inject one; tests inject a buffer to capture log output.
// Package-level so the test seam is centralized.
var supervisorMonitorStderr io.Writer = os.Stderr

func ensureSupervisorRunning(ctx context.Context, mcphubBin string, strictMode bool, waitFor time.Duration) (*supervisorOwner, error) {
	stateDir, err := api.DaemonStateDir()
	if err != nil {
		return nil, fmt.Errorf("supervisor owner: resolve state dir: %w", err)
	}
	if ok, probeErr := probeSupervisor(ctx, 2*time.Second); ok {
		return &supervisorOwner{spawned: false, stateDir: stateDir}, nil
	} else if probeErr != nil {
		return nil, fmt.Errorf("supervisor owner: existing supervisor IPC broken (refusing to spawn duplicate): %w", probeErr)
	}

	args := []string{"supervise"}
	if strictMode {
		args = append(args, "--strict-mode")
	}
	cmd := exec.Command(mcphubBin, args...)
	cmd.Stdin = nil
	cmd.Stdout = io.Discard
	// Capture stderr to a bounded buffer so a startup crash (corrupt
	// supervisor-intent.json, state-path sanity rejection, internal
	// panic before any audit log row lands) is visible in the
	// readiness-timeout error rather than dropped silently. 4 KiB is
	// generous enough for any Go-style fatal message plus stack-
	// trace prefix, while bounding worst-case memory pressure if the
	// supervisor floods stderr in a panic loop. PR #212 r5 finding 2.
	stderrBuf := newBoundedBuffer(4096)
	cmd.Stderr = stderrBuf
	configureSupervisorDetach(cmd)
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("supervisor owner: spawn %q: %w", mcphubBin, err)
	}
	proc := cmd.Process

	deadline := time.Now().Add(waitFor)
	for {
		ok, probeErr := probeSupervisor(ctx, 500*time.Millisecond)
		if ok {
			owner := &supervisorOwner{
				spawned:    true,
				proc:       proc,
				stateDir:   stateDir,
				exitedCh:   make(chan exitInfo, 1),
				stderrBuf:  stderrBuf,
				stderrSink: supervisorMonitorStderr,
			}
			owner.startExitMonitor(proc)
			return owner, nil
		}
		if probeErr != nil {
			_ = proc.Kill()
			_, _ = proc.Wait()
			return nil, fmt.Errorf("supervisor owner: spawned PID %d but IPC handshake broken: %w", proc.Pid, probeErr)
		}
		if time.Now().After(deadline) {
			_ = proc.Kill()
			_, _ = proc.Wait()
			stderrTail := strings.TrimSpace(stderrBuf.String())
			if stderrTail != "" {
				return nil, fmt.Errorf("supervisor owner: spawned PID %d but IPC not reachable within %s; check supervisor-events.log; supervisor stderr tail: %s", proc.Pid, waitFor, stderrTail)
			}
			return nil, fmt.Errorf("supervisor owner: spawned PID %d but IPC not reachable within %s; check supervisor-events.log", proc.Pid, waitFor)
		}
		select {
		case <-ctx.Done():
			_ = proc.Kill()
			_, _ = proc.Wait()
			return nil, ctx.Err()
		case <-time.After(supervisorReadyPollInterval):
		}
	}
}

// spawnSupervisorFn is the package-level spawn seam the
// supervisorManager calls to respawn a dead supervisor child. It
// defaults to ensureSupervisorRunning (the verbatim spawn primitive —
// the singleton flock makes a redundant spawn a safe no-op). Tests
// swap it (with t.Cleanup restore) to inject a synthetic owner so the
// respawn loop can be exercised without a real `mcphub supervise`
// binary. Mirrors the supervisorMonitorStderr swap pattern.
var spawnSupervisorFn = ensureSupervisorRunning

// GUI-side supervisor-respawn bounded-restart-policy knobs. Named next
// to supervisorReadyPollInterval / supervisorForceKillFallbackWindow so
// they are visible and (via the supervisorManager fields seeded from
// them) injectable for tests. The window/cap mirror the supervisor's
// own sliding RestartHistory discipline at a lighter weight: an
// in-memory ring, no new state file.
const (
	guiSupervisorRespawnBackoffBase    = 1 * time.Second
	guiSupervisorRespawnBackoffCap     = 30 * time.Second
	guiSupervisorRespawnWindowCapCount = 5
	guiSupervisorRespawnWindow         = 5 * time.Minute
)

// supervisorManager owns the swappable "current live supervisor owner"
// handle plus a bounded respawn loop for GUI-spawned supervisors.
//
// The GUI spawns its supervisor child exactly once at startup; before
// this manager, an unexpected supervisor death under a live GUI was
// permanently unrecoverable (startExitMonitor only LOGS the death; the
// liveness task defers to the live GUI owner). The manager closes that
// gap: it consumes the owner's existing buffered exitedCh and, on an
// UNEXPECTED exit, respawns via spawnFn with exponential backoff under
// a sliding-window cap.
//
// Concurrency: the install-vs-shutdown decision is a check-then-act, so
// `current` and `shuttingDown` are guarded together under ONE mutex (a
// bare atomic.Pointer cannot express the atomic "is shutdown latched?"
// + "install new owner" decision without a TOCTOU window that would leak
// an un-Stopped respawned supervisor). The respawn (monitor) goroutine
// is the only writer of `current`; the shutdown defer and status path
// are readers — all go through mu. The mutex is held only for O(1)
// swap/flag operations, never across a spawn or a Wait.
//
// The manager NEVER calls proc.Wait(): the owner's own startExitMonitor
// owns the sole proc.Wait() and publishes to exitedCh. The loop blocks
// on <-owner.exitedCh, guaranteeing exactly one Wait owner and exactly
// one exit consumer per live process.
type supervisorManager struct {
	mu           sync.Mutex // guards current + shuttingDown (the swap-vs-shutdown decision)
	current      *supervisorOwner
	shuttingDown bool

	ctx        context.Context
	bin        string
	strictMode bool
	waitFor    time.Duration

	window  []time.Time // sliding respawn-timestamp ring (no new state file)
	spawnFn func(context.Context, string, bool, time.Duration) (*supervisorOwner, error)

	stderrSink io.Writer

	// Test-injectable timing/cap (defaulted from the consts above) so a
	// unit test shrinks the window deterministically large per the
	// race-window-assertion discipline rather than relying on the
	// natural 5-minute window.
	backoffBase    time.Duration
	backoffCap     time.Duration
	windowCapCount int
	windowDur      time.Duration
}

// newSupervisorManager builds a manager seeded with the first
// GUI-spawned owner. The respawn loop is launched separately by the
// caller (startGuiServer) and ONLY when first.Spawned()==true — an
// adopted owner gets no loop, preserving the adopt contract.
func newSupervisorManager(ctx context.Context, bin string, strict bool, waitFor time.Duration, first *supervisorOwner) *supervisorManager {
	return &supervisorManager{
		current:        first,
		ctx:            ctx,
		bin:            bin,
		strictMode:     strict,
		waitFor:        waitFor,
		spawnFn:        spawnSupervisorFn,
		stderrSink:     supervisorMonitorStderr,
		backoffBase:    guiSupervisorRespawnBackoffBase,
		backoffCap:     guiSupervisorRespawnBackoffCap,
		windowCapCount: guiSupervisorRespawnWindowCapCount,
		windowDur:      guiSupervisorRespawnWindow,
	}
}

// armSupervisorManager is the construct-and-arm seam startGuiServer calls
// once it has obtained the seed supervisor owner from
// ensureSupervisorRunning. It encodes the load-bearing wiring decision:
//
//   - nil owner (the spawn errored) or an ADOPTED owner (Spawned()==false,
//     the GUI adopted an externally-managed supervisor) → return nil. No
//     manager, no respawn loop — the GUI does not own that supervisor's
//     lifecycle, so there is nothing to self-heal.
//   - a GUI-SPAWNED owner (Spawned()==true) → construct the manager via
//     newSupervisorManager (which seeds spawnFn from the package-level
//     spawnSupervisorFn) AND launch its bounded respawn loop, so an
//     unexpected supervisor-child death under a live GUI self-heals.
//
// This is extracted out of startGuiServer's inline body specifically so
// the REAL wiring — manager constructed from a Spawned() owner with its
// loop armed via the spawnSupervisorFn seam — is unit-testable without a
// real `mcphub supervise` binary. The seam-based newTestManager tests
// inject spawnFn directly and never reach this gate; the §5 deploy-
// verification found the live respawn loop did not visibly fire, so the
// gate itself needs a test that drives it through the production
// newSupervisorManager + runRespawnLoop path.
func armSupervisorManager(ctx context.Context, owner *supervisorOwner, bin string, strictMode bool) *supervisorManager {
	if owner == nil || !owner.Spawned() {
		return nil
	}
	manager := newSupervisorManager(ctx, bin, strictMode, 15*time.Second, owner)
	go manager.runRespawnLoop(ctx) // self-healing respawn ONLY for GUI-spawned owners
	return manager
}

// currentOwner returns the live supervisor handle under the mutex. Used
// by the shutdown defer and any status reader.
func (m *supervisorManager) currentOwner() *supervisorOwner {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.current
}

// Stop latches shutdown and stops whatever owner is currently live. It
// sets shuttingDown=true and snapshots current under mu BEFORE calling
// that owner's Stop, so the respawn loop (which re-checks shuttingDown
// under the SAME mu) can never install a fresh supervisor after this
// returns: either the loop observes shuttingDown and Stops the orphan
// it just spawned, or this snapshot already holds the installed new
// owner and stops it here. supervisorOwner.Stop is sync.Once-guarded so
// a double-stop across the two paths is impossible.
func (m *supervisorManager) Stop(ctx context.Context, graceTimeoutMs int) error {
	m.mu.Lock()
	m.shuttingDown = true
	victim := m.current
	m.mu.Unlock()
	if victim != nil {
		return victim.Stop(ctx, graceTimeoutMs)
	}
	return nil
}

// runRespawnLoop is the ONE dedicated goroutine per GUI run that chains
// supervisor owners. It blocks on the current owner's exitedCh (NOT
// proc.Wait — startExitMonitor owns that), classifies the exit, and on
// an UNEXPECTED exit respawns with bounded backoff under a sliding
// window cap. It has exactly three exit points so it cannot leak past
// startGuiServer's return:
//
//  1. shutdown latched / ctx cancelled / this owner's Stop ran → EXPECTED,
//     re-publish the drained exit so a racing owner.Stop() still sees it,
//     then return.
//  2. respawn cap reached within the sliding window → durable warn +
//     stderr shadow, then return (no thrash).
//  3. ctx.Done() during the backoff sleep → return.
//
// Launched only when the seeded owner Spawned()==true.
func (m *supervisorManager) runRespawnLoop(ctx context.Context) {
	for {
		cur := m.currentOwner()
		if cur == nil {
			return
		}
		// Bound the receive on ctx so the loop can never (1) block
		// forever on an adopted owner's NIL exitedCh, nor (2) leak on the
		// shutdown dual-consumer race where Stop() drains the cap-1
		// channel's single buffered value first (Go delivers it to
		// exactly one of {Stop, this loop}). ctx is cancelled on GUI
		// shutdown, so this is the loop's guaranteed escape.
		var ev exitInfo
		var ok bool
		select {
		case ev, ok = <-cur.exitedCh:
			if !ok {
				return
			}
		case <-ctx.Done():
			return
		}
		// Classify under the lock. shuttingDown is the authoritative
		// gate; ctx cancel and this owner's own stopRequested cover the
		// windows where Stop ran on a specific owner before shutdown was
		// observed here.
		m.mu.Lock()
		shutting := m.shuttingDown
		m.mu.Unlock()
		if shutting || ctx.Err() != nil || cur.stopRequested.Load() {
			// EXPECTED exit. Re-publish the drained exitInfo back onto
			// the buffered (cap-1) channel non-blocking so a concurrent
			// or subsequent owner.Stop() still observes it on its fast
			// path instead of waiting out its force-kill fallback.
			select {
			case cur.exitedCh <- ev:
			default:
			}
			return
		}

		// UNEXPECTED exit: respawn with bounded backoff under a sliding
		// window cap. The SPAWN itself is retried on transient errors —
		// each attempt is its own window slot — so a persistently
		// failing/wedged spawn TRIPS THE CAP and stops, instead of
		// parking forever (a bare `continue` back to the top receive
		// would re-block on the already-drained dead owner) or
		// tight-looping. A ctx cancel during backoff/spawn returns.
		var newOwner *supervisorOwner
		for {
			now := time.Now()
			m.window = append(m.window, now)
			pruned := m.window[:0]
			for _, t := range m.window {
				if now.Sub(t) <= m.windowDur {
					pruned = append(pruned, t)
				}
			}
			m.window = pruned
			if len(m.window) > m.windowCapCount {
				_ = api.LogHubMcpEvent("warn", "gui-supervisor-respawn-cap-reached", map[string]any{
					"cap":      m.windowCapCount,
					"window_s": int(m.windowDur.Seconds()),
				})
				fmt.Fprintf(m.stderrSink,
					"warning: supervisor respawn cap reached (%d in %s); not respawning; check supervisor-events.log\n",
					m.windowCapCount, m.windowDur)
				return
			}
			// Exponential backoff indexed by the attempt count in the
			// current window, capped. Cancellable via ctx.
			backoff := m.backoffBase * time.Duration(1<<min(len(m.window)-1, 6))
			if backoff > m.backoffCap {
				backoff = m.backoffCap
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(backoff):
			}
			o, err := m.spawnFn(m.ctx, m.bin, m.strictMode, m.waitFor)
			if err != nil {
				// Transient/wedged spawn failure: log + RETRY (the inner
				// loop re-appends a window slot and re-checks the cap), so
				// a chronic failure trips the cap rather than silently
				// disabling GUI self-healing after a single error.
				_ = api.LogHubMcpEvent("warn", "gui-supervisor-respawn-failed", map[string]any{
					"error": err.Error(),
				})
				fmt.Fprintf(m.stderrSink, "warning: supervisor respawn attempt failed: %v\n", err)
				continue
			}
			newOwner = o
			break
		}
		_ = api.LogHubMcpEvent("info", "gui-supervisor-respawned-by-gui", map[string]any{
			"pid": newOwner.Pid(),
		})
		// INSTALL under the lock. If shutdown won the race during the
		// spawn, Stop the orphan ourselves and do NOT install it.
		m.mu.Lock()
		if m.shuttingDown {
			m.mu.Unlock()
			_ = newOwner.Stop(m.ctx, 5000)
			return
		}
		m.current = newOwner
		m.mu.Unlock()
		// If the respawn ADOPTED an already-bound (foreign) supervisor
		// instead of spawning a fresh GUI-owned one, ensureSupervisorRunning
		// returned spawned=false with a nil exitedCh — the GUI does not
		// own that supervisor's lifecycle and cannot monitor its exit
		// (no proc, no exitedCh). A supervisor IS running, so the recovery
		// goal is met: keep it installed for status/shutdown visibility
		// (adopted Stop is a no-op) and END the loop. Re-monitoring a
		// foreign supervisor would require polling — documented known
		// limitation; the common case (GUI-owned supervisor died, no
		// foreign one bound) spawns a fresh spawned=true owner and the
		// loop continues normally.
		if !newOwner.Spawned() {
			_ = api.LogHubMcpEvent("info", "gui-supervisor-respawn-adopted-existing", map[string]any{})
			return
		}
		// Loop continues, now consuming newOwner.exitedCh (its own
		// startExitMonitor publishes there).
	}
}

// boundedBuffer is a bytes.Buffer-like sink that drops writes after
// a fixed byte cap. Used to capture supervisor stderr during the
// readiness window without unbounded memory growth. Preserves the
// FIRST `cap` bytes (what an operator actually needs for diagnosis
// — Go-style fatal messages put the actionable content at the
// start; later panic-loop output is redundant).
type boundedBuffer struct {
	cap int
	buf []byte
}

func newBoundedBuffer(cap int) *boundedBuffer { return &boundedBuffer{cap: cap} }

func (b *boundedBuffer) Write(p []byte) (int, error) {
	if len(b.buf) >= b.cap {
		return len(p), nil
	}
	room := b.cap - len(b.buf)
	if room > len(p) {
		room = len(p)
	}
	b.buf = append(b.buf, p[:room]...)
	return len(p), nil
}

func (b *boundedBuffer) String() string { return string(b.buf) }

// startExitMonitor launches the background goroutine that waits for
// the spawned supervisor process to exit, captures the exit info on
// exitedCh, and emits a stderr warning if the exit happens BEFORE
// Stop() was called (early-exit / crash). PR #212 r6 reliability
// finding 1: previously a crash mid-runtime was silently swallowed
// because stop() collapsed ErrSupervisorIPCUnavailable to "expected"
// at shutdown time.
//
// Lifecycle: the monitor runs concurrently with GUI runtime. It
// publishes exit info to the buffered channel exactly once. stop()
// drains the channel (or starts a fallback Wait if the channel is
// already consumed). The "expected vs unexpected" classification is
// done in the monitor via the stoppedOnce sentinel: if the supervisor
// exits before Stop() runs, it's unexpected.
func (s *supervisorOwner) startExitMonitor(proc *os.Process) {
	go func() {
		state, err := proc.Wait()
		var exitErr error
		if err != nil {
			exitErr = err
		} else if state != nil && !state.Success() {
			exitErr = fmt.Errorf("supervisor exited with non-zero status: %s", state.String())
		}
		// Publish exit info to stop()'s receive channel. Non-blocking
		// send via buffered chan(1); if stop() already drained, the
		// next send would block — but stop() reads exactly once per
		// owner so a second send here is a defect (handled via
		// default branch).
		select {
		case s.exitedCh <- exitInfo{exitErr: exitErr}:
		default:
		}
		// Classify: if Stop() set stopRequested before this monitor
		// observed the exit, the exit is EXPECTED (graceful shutdown
		// or force-kill on shutdown). Otherwise the supervisor died
		// mid-runtime — log to stderr so the operator sees the
		// captured stderr tail and a pointer to supervisor-events.log
		// instead of inferring "MCPs gone" from an empty Dashboard.
		if !s.stopRequested.Load() {
			stderrTail := strings.TrimSpace(s.stderrBuf.String())
			if exitErr == nil {
				fmt.Fprintf(s.stderrSink, "warning: supervisor exited unexpectedly (PID %d): clean exit before GUI shutdown; check supervisor-events.log\n",
					proc.Pid)
			} else if stderrTail != "" {
				fmt.Fprintf(s.stderrSink, "warning: supervisor exited unexpectedly (PID %d): %v; stderr tail: %s\n",
					proc.Pid, exitErr, stderrTail)
			} else {
				fmt.Fprintf(s.stderrSink, "warning: supervisor exited unexpectedly (PID %d): %v; check supervisor-events.log\n",
					proc.Pid, exitErr)
			}
		}
	}()
}

// Stop sends graceful exit IPC to a spawned supervisor and waits for
// it to terminate. For adopted supervisors (spawned=false) this is a
// no-op.
//
// Idempotent — sync.Once guards repeated calls; the first call's
// error (if any) is remembered and returned for subsequent calls.
//
// graceTimeoutMs is the IPC-layer drain budget that gets forwarded to
// the supervisor's quiesce path. The total wall-time spent in Stop is
// at most graceTimeoutMs + supervisorForceKillFallbackWindow.
func (s *supervisorOwner) Stop(ctx context.Context, graceTimeoutMs int) error {
	if s == nil {
		return nil
	}
	s.stoppedOnce.Do(func() {
		s.stoppedErr = s.stop(ctx, graceTimeoutMs)
	})
	return s.stoppedErr
}

// supervisorForceKillFallbackWindow is the additional time we wait
// for the supervisor process to exit after the graceful-exit IPC has
// been acknowledged. Picked to match the install_upgrade.go force-
// kill grace (5s) but conservatively bumped for slow-host systems
// where daemon teardown can stretch the supervisor's own exit path.
const supervisorForceKillFallbackWindow = 7 * time.Second

func (s *supervisorOwner) stop(ctx context.Context, graceTimeoutMs int) error {
	if !s.spawned {
		return nil
	}
	if s.proc == nil {
		return errors.New("supervisor owner: spawned=true but no Process handle recorded")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	// Signal the exit monitor that shutdown is intentional — its
	// post-Wait classification reads this to decide whether to emit
	// the "unexpected exit" warning. Set BEFORE issuing the IPC exit
	// to avoid a race where Wait returns between the IPC call
	// landing and Stop() marking the shutdown as expected.
	s.stopRequested.Store(true)

	// The background exit monitor (startExitMonitor) is already
	// running cmd.Wait() and will publish to s.exitedCh on exit. We
	// just drain it here. If the supervisor crashed before Stop()
	// was called, the monitor has already buffered the exit info
	// AND emitted the early-exit warning — Stop() then just sees an
	// already-dead process and returns cleanly.
	exited := s.exitedCh

	// Issue the IPC exit concurrently under a sub-deadline derived
	// from graceTimeoutMs only — never the full parent ctx deadline.
	// This bounds total Stop wall-time to graceTimeoutMs + fallback
	// even when the supervisor's IPC pipe is hung or the supervisor
	// process is unresponsive.
	ipcDone := make(chan error, 1)
	go func() {
		ipcCtx, cancel := context.WithTimeout(ctx, time.Duration(graceTimeoutMs)*time.Millisecond)
		defer cancel()
		ipcDone <- api.DialSupervisorIPCExit(ipcCtx, graceTimeoutMs)
	}()

	total := time.Duration(graceTimeoutMs)*time.Millisecond + supervisorForceKillFallbackWindow
	select {
	case <-exited:
		// Process exited within the grace window. Drain the IPC
		// result for diagnosability: if the IPC layer failed (e.g.
		// handshake mismatch) but the process is gone anyway, surface
		// the IPC failure. A pending IPC call against an already-dead
		// supervisor returns within the sub-deadline established
		// above, so this drain is bounded.
		ipcErr := <-ipcDone
		if ipcErr != nil && !errors.Is(ipcErr, api.ErrSupervisorIPCUnavailable) {
			return fmt.Errorf("supervisor owner: graceful exit IPC failed but process exited: %w", ipcErr)
		}
		return nil
	case <-time.After(total):
		// Force-kill — supervisor didn't exit in time.
		killErr := s.proc.Kill()
		// Wait for the process exit with a backstop timer. If Kill
		// failed (Windows ACCESS_DENIED on protected process, POSIX
		// EPERM, kernel-level immortal-process bug), the supervisor
		// is still alive and proc.Wait blocks forever. The 2s
		// backstop unblocks Stop() so shutdown stays responsive and
		// the operator sees an actionable error rather than a hung
		// tray-close. PR #212 r5 silent-failure-hunt finding 3.
		select {
		case <-exited:
		case <-time.After(2 * time.Second):
			if killErr != nil {
				return fmt.Errorf("supervisor owner: graceful exit timeout after %s; force-kill failed for PID %d: %v (process may still be running)",
					total, s.proc.Pid, killErr)
			}
			return fmt.Errorf("supervisor owner: graceful exit timeout after %s; force-kill issued for PID %d but process did not exit within 2s (may be unkillable)",
				total, s.proc.Pid)
		}
		// Best-effort drain of the IPC result. The IPC goroutine's
		// ipcCtx times out at graceTimeoutMs so it doesn't leak.
		var ipcErr error
		select {
		case ipcErr = <-ipcDone:
		case <-time.After(100 * time.Millisecond):
		}
		if ipcErr != nil {
			return fmt.Errorf("supervisor owner: graceful exit timeout after %s (IPC error: %v), force-killed PID %d",
				total, ipcErr, s.proc.Pid)
		}
		return fmt.Errorf("supervisor owner: graceful exit timeout after %s, force-killed PID %d",
			total, s.proc.Pid)
	}
}

// Spawned reports whether GUI owns the supervisor lifecycle. The tray
// + diagnostics paths surface this to operators so an adopted-mode
// session is visibly distinct from a GUI-owned session.
func (s *supervisorOwner) Spawned() bool {
	if s == nil {
		return false
	}
	return s.spawned
}

// Pid returns the supervisor process's PID. Returns 0 for adopted
// mode (we don't track the PID in that case — caller can read it
// from supervisor.lock.owner.json if needed).
func (s *supervisorOwner) Pid() int {
	if s == nil || s.proc == nil {
		return 0
	}
	return s.proc.Pid
}

// probeSupervisor performs a single IPC handshake + status query
// against the running supervisor. Returns (true, nil) when reachable.
// Returns (false, nil) ONLY for the expected pre-bind state
// (ErrSupervisorIPCUnavailable — no lock owner sidecar, no pipe, or
// connect-refused). Any other failure (hello-version mismatch, JSON
// decode error, ID mismatch) returns (false, error) so the caller
// can distinguish "supervisor not yet up" from "supervisor up but
// broken" and surface the actionable error to the operator.
//
// PR #212 r5 silent-failure-hunt finding 1: the previous bool-only
// signature collapsed handshake failures into "not reachable", which
// made the readiness-wait loop spin for the full 15s timeout and then
// emit a generic "IPC not reachable" message instead of the actual
// hello-mismatch text. Worse, the pre-spawn probe at the top of
// ensureSupervisorRunning collapsed the same way, causing the GUI to
// spawn a SECOND supervisor on top of a hello-broken first one.
func probeSupervisor(ctx context.Context, probeTimeout time.Duration) (bool, error) {
	probeCtx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	if _, err := api.DialSupervisorIPCStatus(probeCtx); err != nil {
		if errors.Is(err, api.ErrSupervisorIPCUnavailable) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// resolveMCPHubBinary returns the absolute, symlink-resolved path to
// the currently-running mcphub binary. Used by the GUI to spawn its
// supervisor child with the same code as the running process —
// critical for dev builds where the binary path is not on PATH and
// for guaranteeing version consistency between GUI and supervisor.
//
// EvalSymlinks closes a TOCTOU window on macOS (preview tier) where
// os.Executable can return a symlink path; an attacker with write
// access to the symlink's parent directory could swap the target
// between resolve and cmd.Start, redirecting the supervisor spawn to
// an attacker-controlled binary. EvalSymlinks resolves to the inode
// path so subsequent spawn cannot be redirected via symlink swap.
// On Linux, /proc/self/exe already returns the resolved path so
// EvalSymlinks is a no-op there; on Windows, the path is canonicalized
// at kernel-load time and symlinks aren't part of the exe-resolution
// chain. PR #212 r3 security finding 2.
func resolveMCPHubBinary() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("resolve mcphub binary: %w", err)
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	abs, err := filepath.Abs(exe)
	if err != nil {
		return exe, nil // best-effort; caller will spawn with non-abs
	}
	return abs, nil
}
