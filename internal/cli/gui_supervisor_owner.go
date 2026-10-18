package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"mcp-local-hub/internal/api"
	"mcp-local-hub/internal/gui"
	processowner "mcp-local-hub/internal/process"
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
	spawned           bool
	proc              *os.Process
	createdPID        int
	createdGeneration api.SupervisorLockOwner
	stateDir          string
	control           SupervisorControlBinding
	exitDone          chan struct{}
	exitResult        exitInfo
	stoppedOnce       sync.Once
	stoppedErr        error
	// Original Stop sender outcome; read only after stoppedOnce and ipcDone.
	ipcDone       chan struct{}
	ipcErr        error
	killErr       error
	killCommitted bool // original Process.Kill actually accepted; read after Stop
	attempt       *supervisorCreationAttempt

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

// These copied facts convey observation only, never process/control custody.
type supervisorCreationSnapshot struct {
	Entered, PreStart, Created, GenerationAvailable bool
	PID                                             int
	Generation                                      api.SupervisorLockOwner
	WaitReceived, Finished                          bool
	WaitError, Cause, SettlementError               error
}

type supervisorAttemptReceiver func(supervisorCreationSnapshot) error

type supervisorCreatorOperations struct {
	capturePeer   func(*os.Process, api.SupervisorLockOwner) (api.SupervisorProcessIdentityV1, error)
	settleUnbound func(*supervisorOwner) error
}

func (s supervisorCreationSnapshot) diagnosticFields() map[string]any {
	fields := map[string]any{
		"entered": s.Entered, "pre_start": s.PreStart, "created": s.Created,
		"pid": s.PID, "generation_available": s.GenerationAvailable,
		"wait_received": s.WaitReceived, "finished": s.Finished,
	}
	if s.GenerationAvailable {
		fields["generation"] = s.Generation
	}
	for key, cause := range map[string]error{"cause": s.Cause, "wait_error": s.WaitError, "settlement_error": s.SettlementError} {
		if cause != nil {
			fields[key] = cause.Error()
		}
	}
	return fields
}

// The original creator and its sole monitor serialize incremental publication.
// Receivers must only copy facts under short synchronization; no I/O or waits.
type supervisorCreationAttempt struct {
	mu         sync.Mutex
	snapshot   supervisorCreationSnapshot
	receive    supervisorAttemptReceiver
	receiveErr error
}

func (a *supervisorCreationAttempt) publish(update func(*supervisorCreationSnapshot)) {
	if a == nil {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	update(&a.snapshot)
	if a.receive != nil {
		a.receiveErr = errors.Join(a.receiveErr, a.receive(a.snapshot))
	}
}

func (a *supervisorCreationAttempt) observationError() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.receiveErr
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
// supervisor monitor warnings. Tests inject a buffer to capture log
// output. Package-level so the test seam is centralized.
//
// The default is the switchable guiRuntimeStderr rather than a bare
// os.Stderr, and that is load-bearing: this sink is captured into each
// supervisorOwner at construction time, which happens BEFORE the GUI
// releases its console. A plain os.Stderr captured there would still be
// pointing at the console handle after the release, so the supervisor
// exit-attribution warning below — the only record of a supervisor crash
// that predates the supervisor's own audit log — would be written to a
// dead handle and silently lost. Going through the switchable sink means
// the already-captured reference follows the switch.
var supervisorMonitorStderr io.Writer = guiRuntimeStderr

// newGUISupervisorCmd builds the detached `<bin> supervise [--strict-mode]`
// command the GUI spawns. Package-level (rather than an inline closure) so the
// spawn CONFIGURATION can be asserted without a real `mcphub supervise`.
//
// This is also the RESPAWN path: the manager's spawn seam resolves to
// ensureSupervisorRunningObserved, so every replacement the respawn loop
// starts is built right here — and so is every degraded retry inside
// startSupervisorDetachedBreakaway, which rebuilds through this function with
// creation flags stripped. The console-attach suppression comes from
// configureSupervisorDetach and therefore rides along on all of them.
func newGUISupervisorCmd(mcphubBin string, args []string, stderr io.Writer) *exec.Cmd {
	c := exec.Command(mcphubBin, args...)
	c.Stdin = nil
	c.Stdout = io.Discard
	c.Stderr = stderr
	configureSupervisorDetach(c)
	return c
}

func ensureSupervisorRunning(ctx context.Context, mcphubBin string, strictMode bool, waitFor time.Duration) (*supervisorOwner, error) {
	return ensureSupervisorRunningObserved(ctx, mcphubBin, strictMode, waitFor, nil)
}

func ensureSupervisorRunningObserved(ctx context.Context, mcphubBin string, strictMode bool, waitFor time.Duration, receive supervisorAttemptReceiver) (retOwner *supervisorOwner, retErr error) {
	return ensureSupervisorRunningWithOperations(ctx, mcphubBin, strictMode, waitFor, receive, supervisorCreatorOperations{})
}

func ensureSupervisorRunningWithOperations(ctx context.Context, mcphubBin string, strictMode bool, waitFor time.Duration, receive supervisorAttemptReceiver, operations supervisorCreatorOperations) (retOwner *supervisorOwner, retErr error) {
	if operations.capturePeer == nil {
		operations.capturePeer = captureCreatedSupervisorPeer
	}
	if operations.settleUnbound == nil {
		operations.settleUnbound = (*supervisorOwner).settleUnboundCreator
	}
	attempt := &supervisorCreationAttempt{receive: receive}
	var owner *supervisorOwner
	attempt.publish(func(s *supervisorCreationSnapshot) { s.Entered, s.PreStart = true, true })
	defer func() {
		attempt.publish(func(s *supervisorCreationSnapshot) { s.Cause = errors.Join(s.Cause, retErr) })
		if observeErr := attempt.observationError(); observeErr != nil {
			retErr = errors.Join(retErr, observeErr)
			if owner != nil && retOwner != nil && owner.proc != nil {
				retErr = errors.Join(retErr, operations.settleUnbound(owner))
				retOwner = nil
			}
		}
		attempt.publish(func(s *supervisorCreationSnapshot) { s.Finished = true; s.Cause = errors.Join(s.Cause, retErr) })
		retErr = errors.Join(retErr, attempt.observationError())
		// A terminal receiver can itself fail after successful binding. That
		// failure still cannot discard this creator's actual retained child.
		if attempt.observationError() != nil && owner != nil && retOwner != nil && owner.proc != nil {
			retOwner = nil
			attempt.publish(func(s *supervisorCreationSnapshot) { s.Finished = false })
			retErr = errors.Join(retErr, operations.settleUnbound(owner))
			attempt.publish(func(s *supervisorCreationSnapshot) { s.Finished = true; s.Cause = errors.Join(s.Cause, retErr) })
			retErr = errors.Join(retErr, attempt.observationError())
		}
	}()
	if ctx == nil {
		return nil, errors.New("supervisor owner: context required")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	stateDir, err := api.DaemonStateDir()
	if err != nil {
		// A state-dir RESOLVE failure has different causes and different
		// remediation than the probe-path setup fault (present-but-unreadable
		// owner sidecar), so it does NOT route through supervisorSetupFaultError
		// — the three sidecar remediations (repair-state-dacl / delete-sidecar /
		// unset MCPHUB_REQUIRE_SINGLE_USER_HOME) are all dead ends here:
		//   - Windows (GA): DaemonStateDir fails only via errKnownFolderUnavailable
		//     (SHGetKnownFolderPath(FOLDERID_LocalAppData) failed/empty) or a
		//     mkdir-state-root failure — no sidecar, no DACL, and the env var is
		//     never consulted in the resolve path.
		//   - POSIX: the state-root parent is group/world-writable or not owned
		//     by the caller (errStateParentInsecure), which fires UNCONDITIONALLY;
		//     MCPHUB_REQUIRE_SINGLE_USER_HOME is not consulted in state_paths*.go.
		// So this gets a dedicated, platform-correct error naming the real cause
		// and fix, keeping the underlying cause wrapped for diagnosis (bot r2 P2 /
		// Fable-5 P3 correction).
		return nil, supervisorStateDirResolveError(err)
	}
	if ok, probeErr := probeSupervisor(ctx, 2*time.Second); ok {
		return &supervisorOwner{spawned: false, stateDir: stateDir}, nil
	} else if probeErr != nil {
		// A LOCAL setup failure (api.ErrStatusSetupFailure — the supervisor
		// owner sidecar is PRESENT but unreadable: a DACL/mode refusal on a
		// sandbox-broadened %LOCALAPPDATA% or under
		// MCPHUB_REQUIRE_SINGLE_USER_HOME=1, or corrupt bytes) is NOT a
		// live-but-broken supervisor — no supervisor is proven to be running.
		// Spawning a duplicate would not help: the fresh supervisor would hit
		// the same DACL refusal writing its own owner sidecar. Surface the
		// local-fault remediation (mcphub repair-state-dacl) instead of the
		// misleading "existing supervisor IPC broken (refusing to spawn
		// duplicate)" (bot PR #477 P3 respawn-twin classification).
		if errors.Is(probeErr, api.ErrStatusSetupFailure) {
			return nil, supervisorSetupFaultError(probeErr)
		}
		return nil, fmt.Errorf("supervisor owner: existing supervisor IPC broken (refusing to spawn duplicate): %w", probeErr)
	}

	productPath, err := filepath.Abs(mcphubBin)
	if err != nil {
		return nil, err
	}
	productPath, err = filepath.EvalSymlinks(productPath)
	if err != nil {
		return nil, err
	}
	digest, err := startedSupervisorDigest(productPath)
	if err != nil {
		return nil, err
	}
	expected := supervisorExpectation(stateDir, productPath, digest)
	args := []string{"supervise"}
	if strictMode {
		args = append(args, "--strict-mode")
	}
	// Capture stderr to a bounded buffer so a startup crash (corrupt
	// supervisor-intent.json, state-path sanity rejection, internal
	// panic before any audit log row lands) is visible in the
	// readiness-timeout error rather than dropped silently. 4 KiB is
	// generous enough for any Go-style fatal message plus stack-
	// trace prefix, while bounding worst-case memory pressure if the
	// supervisor floods stderr in a panic loop. PR #212 r5 finding 2.
	stderrBuf := newBoundedBuffer(4096)
	// Every shared-owner attempt reuses the same bounded stderr sink.
	build := func() *exec.Cmd { return newGUISupervisorCmd(mcphubBin, args, stderrBuf) }
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	// A proven direct-GUI lifecycle admits the optional flagless compatibility
	// attempt; Job-owned or ambiguous launchers resolve to required/no-retry.
	startedCmd, err := startSupervisorDetachedBreakaway(build(), build, func(degradeErr error) {
		fmt.Fprintf(supervisorMonitorStderr, "supervisor owner: CREATE_BREAKAWAY_FROM_JOB rejected by parent job (no BREAKAWAY_OK); spawned flagless — supervisor may be cascade-killed if the launcher's job closes: %v\n", degradeErr)
	}, processowner.BreakawayPolicyForCurrentProcess(), func(started *exec.Cmd) {
		proc := started.Process
		owner = &supervisorOwner{
			proc: proc, stateDir: stateDir, exitedCh: make(chan exitInfo, 1),
			createdPID: proc.Pid, exitDone: make(chan struct{}),
			stderrBuf: stderrBuf, stderrSink: supervisorMonitorStderr, attempt: attempt,
		}
		// Establish the only Wait before identity acquisition or diagnostics.
		attempt.publish(func(s *supervisorCreationSnapshot) {
			s.PreStart = false
			s.Created = true
			s.PID = proc.Pid
			owner.startExitMonitor(started)
		})
	})
	if err != nil {
		return nil, fmt.Errorf("supervisor owner: spawn %q: %w", mcphubBin, err)
	}
	proc := startedCmd.Process
	original, captureErr := captureGUISupervisorGeneration(proc)
	var peer api.SupervisorProcessIdentityV1
	if captureErr == nil {
		peer, captureErr = operations.capturePeer(proc, original)
	}
	owner.createdGeneration = original
	attempt.publish(func(s *supervisorCreationSnapshot) {
		s.Generation = original
		s.GenerationAvailable = original.PID > 0 && original.StartedAt != ""
		s.Cause = errors.Join(s.Cause, captureErr)
	})
	if captureErr != nil {
		return nil, errors.Join(captureErr, operations.settleUnbound(owner))
	}
	deadline := time.Now().Add(waitFor)
	for {
		probeCtx, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
		current, bindErr := observeCreatedSupervisorControl(probeCtx, expected, original, peer, func(candidate api.SupervisorLockOwner) error {
			return validateCreatedSupervisorNativeControl(expected, candidate)
		}, api.ReadSupervisorLockOwner)
		cancel()
		attempt.publish(func(s *supervisorCreationSnapshot) { s.Cause = errors.Join(s.Cause, bindErr) })
		if bindErr == nil {
			proof := &supervisorControlProof{creator: proc, expected: expected, owner: current, peer: peer}
			proof.originalOutcome = func() (bool, error) {
				select {
				case <-owner.exitDone:
					return true, owner.exitResult.exitErr
				default:
					return false, nil
				}
			}
			proof.validateCreator = func() error {
				if owner.control.proof != proof || owner.proc != proc || proof.owner != original || proof.expected != expected {
					return errSupervisorControlBinding
				}
				return nil
			}
			owner.control = SupervisorControlBinding{proof: proof}
			owner.spawned = true
			return owner, nil
		}
		if errors.Is(bindErr, errSupervisorControlGenerationChanged) {
			// Another owner is DATA until this exact created loser is settled.
			if settleErr := operations.settleUnbound(owner); settleErr != nil {
				return nil, errors.Join(bindErr, settleErr)
			}
			winner, readErr := api.ReadSupervisorLockOwner(filepath.Join(stateDir, "supervisor.lock"))
			if readErr != nil || winner == original {
				return nil, errors.Join(bindErr, readErr)
			}
			winnerPeer, peerErr := api.ObserveSupervisorProcessIdentityV1(winner.PID)
			if peerErr != nil {
				return nil, errors.Join(bindErr, peerErr)
			}
			if _, err := observeCreatedSupervisorControl(ctx, expected, winner, winnerPeer, func(candidate api.SupervisorLockOwner) error {
				return validateCreatedSupervisorNativeControl(expected, candidate)
			}, api.ReadSupervisorLockOwner); err != nil {
				return nil, errors.Join(bindErr, err)
			}
			// Original child exit evidence remains on the same owner. Clearing
			// its retained (now reaped) proc conveys no control over the winner.
			owner.proc = nil
			return owner, nil
		}
		if ctx.Err() != nil {
			return nil, errors.Join(ctx.Err(), bindErr, operations.settleUnbound(owner))
		}
		select {
		case <-owner.exitDone:
			return nil, errors.Join(bindErr, owner.exitResult.exitErr, errors.New("created supervisor exited before control binding"))
		default:
		}
		if !errors.Is(bindErr, os.ErrNotExist) && !errors.Is(bindErr, api.ErrSupervisorIPCUnavailable) && !errors.Is(bindErr, context.DeadlineExceeded) {
			return nil, errors.Join(bindErr, operations.settleUnbound(owner))
		}
		if time.Now().After(deadline) {
			return nil, errors.Join(fmt.Errorf("supervisor owner: original PID %d control not ready within %s; stderr: %s", proc.Pid, waitFor, strings.TrimSpace(stderrBuf.String())), bindErr, operations.settleUnbound(owner))
		}
		select {
		case <-ctx.Done():
			return nil, errors.Join(ctx.Err(), bindErr, operations.settleUnbound(owner))
		case <-time.After(supervisorReadyPollInterval):
		}
	}

}

// spawnSupervisorFn is the package-level spawn seam the
// supervisorManager calls to respawn a dead supervisor child. It
// defaults to ensureSupervisorRunningObserved (the same spawn primitive —
// the singleton flock makes a redundant spawn a safe no-op). Tests
// swap it (with t.Cleanup restore) to inject a synthetic owner so the
// respawn loop can be exercised without a real `mcphub supervise`
// binary. Mirrors the supervisorMonitorStderr swap pattern.
var spawnSupervisorFn = ensureSupervisorRunningObserved

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
// liveness task at the time DEFERRED to the live GUI owner — that defer
// is gone, see supervise_ensure_alive.go §5 PART 2). This manager closes
// the gap for a GUI-SPAWNED supervisor: it consumes the owner's existing
// buffered exitedCh and, on an
// UNEXPECTED exit, respawns via spawnFn with exponential backoff under
// a sliding-window cap.
//
// Concurrency: the install-vs-shutdown decision is a check-then-act, so
// `current` and `shuttingDown` are guarded together under ONE mutex (a
// bare atomic.Pointer cannot express the atomic "is shutdown latched?"
// + "install new owner" decision without a TOCTOU window that would leak
// an un-Stopped respawned supervisor). Respawn and explicit restart share
// one admitted operation; shutdown and status observe that same owner.
// All current publication goes through mu. The mutex is held only for O(1)
// swap/flag operations, never across a spawn or a Wait.
//
// The manager NEVER calls proc.Wait(): the owner's own startExitMonitor
// owns the sole cmd.Wait() and publishes to exitedCh. The loop blocks
// on <-owner.exitedCh, guaranteeing exactly one Wait owner and exactly
// one exit consumer per live process.
type supervisorManager struct {
	mu             sync.Mutex // guards current + shuttingDown (the swap-vs-shutdown decision)
	current        *supervisorOwner
	shuttingDown   bool
	loopCancel     context.CancelFunc
	loopDone       chan struct{} // nil until the actual loop is armed
	loopErr        error         // published before loopDone closes
	workerCancel   context.CancelFunc
	initialized    bool
	operation      *supervisorManagerOperation
	startupAttempt supervisorCreationSnapshot
	lifecycle      *gui.GUIOwnerLifecycle
	generation     string
	restartFn      func(context.Context, *supervisorManagerOperation) (gui.SupervisorRestartResult, error)

	ctx        context.Context
	bin        string
	strictMode bool
	waitFor    time.Duration

	window  []time.Time // sliding respawn-timestamp ring (no new state file)
	spawnFn func(context.Context, string, bool, time.Duration, supervisorAttemptReceiver) (*supervisorOwner, error)

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
	loopCtx, cancel := context.WithCancel(ctx)
	return &supervisorManager{
		current:        first,
		initialized:    true,
		ctx:            loopCtx,
		loopCancel:     cancel,
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

// armSupervisorManager always constructs the same lifetime coordinator.
// Only an actually GUI-created actor arms its automatic worker.
func armSupervisorManager(ctx context.Context, owner *supervisorOwner, bin string, strictMode bool) *supervisorManager {
	manager := newSupervisorManager(ctx, bin, strictMode, 15*time.Second, owner)
	manager.mu.Lock()
	manager.armWorkerLocked()
	manager.mu.Unlock()
	return manager
}

// The caller holds mu and has joined any earlier worker before rearming.
func (m *supervisorManager) armWorkerLocked() {
	if m.current == nil || !m.current.Spawned() || m.shuttingDown || m.ctx.Err() != nil {
		return
	}
	workerCtx, cancel := context.WithCancel(m.ctx)
	m.workerCancel = cancel
	m.loopDone = make(chan struct{})
	go m.runRespawnLoop(workerCtx)
}

func (m *supervisorManager) initialize(owner *supervisorOwner, bin string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.initialized {
		return errors.New("supervisor manager initialization already settled")
	}
	m.current, m.bin, m.initialized = owner, bin, true
	if m.shuttingDown || m.ctx.Err() != nil {
		return context.Canceled
	}
	m.armWorkerLocked()
	return nil
}

// currentOwner returns the live supervisor handle under the mutex. Used
// by the shutdown defer and any status reader.
func (m *supervisorManager) currentOwner() *supervisorOwner {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.current
}

// Stop receives the accepted current owner and the original respawn worker.
// Control budgets bound attempts, not original process or sender custody.
func (m *supervisorManager) Stop(ctx context.Context, graceTimeoutMs int) error {
	return m.finish(ctx, graceTimeoutMs, true)
}

// Quiesce joins the same worker and unpublished attempts while preserving the
// accepted current supervisor for authenticated GUI self-restart.
func (m *supervisorManager) Quiesce(ctx context.Context) error {
	return m.finish(ctx, 5000, false)
}

func (m *supervisorManager) finish(ctx context.Context, graceTimeoutMs int, stopCurrent bool) error {
	if m == nil {
		return nil
	}
	m.mu.Lock()
	m.shuttingDown = true
	victim := m.current
	cancel, done := m.loopCancel, m.loopDone
	operation := m.operation
	m.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	var result error
	if operation != nil && operation.kind == "restart" {
		// Cancellation is admitted above, but actual creator/transfer receiving
		// remains with this operation before current shutdown and GUI release.
		<-operation.done
		m.mu.Lock()
		result = errors.Join(result, operation.err)
		victim, done = m.current, m.loopDone
		m.mu.Unlock()
	}
	if stopCurrent {
		result = errors.Join(result, m.stopOwnerAndJoin(ctx, victim, graceTimeoutMs))
	}
	if done != nil {
		<-done
	}
	m.mu.Lock()
	result = errors.Join(result, m.loopErr)
	m.mu.Unlock()
	return result
}

func (m *supervisorManager) stopOwnerAndJoin(ctx context.Context, owner *supervisorOwner, graceTimeoutMs int) error {
	if owner == nil || !owner.Spawned() {
		return nil
	}
	result := owner.Stop(ctx, graceTimeoutMs)
	reportedPending := false
	if owner.exitDone != nil {
		select {
		case <-owner.exitDone:
		default:
			fmt.Fprintln(m.stderrSink, "warning: supervisor settlement unconfirmed; retaining GUI ownership until original process and exit sender settle")
			reportedPending = true
			<-owner.exitDone
		}
		if cause := owner.exitResult.exitErr; cause != nil && !errors.Is(result, cause) {
			result = errors.Join(result, cause)
		}
	}
	if owner.ipcDone != nil {
		select {
		case <-owner.ipcDone:
		default:
			if !reportedPending {
				fmt.Fprintln(m.stderrSink, "warning: supervisor settlement unconfirmed; retaining GUI ownership until original process and exit sender settle")
			}
			<-owner.ipcDone
		}
		if cause := owner.ipcErr; cause != nil && !errors.Is(result, cause) {
			result = errors.Join(result, cause)
		}
	}
	if cause := owner.killErr; cause != nil && !errors.Is(result, cause) {
		result = errors.Join(result, cause)
	}
	return result
}

// runRespawnLoop is the ONE dedicated goroutine per GUI run that chains
// supervisor owners. It blocks on the current owner's exitedCh (NOT
// proc.Wait — startExitMonitor owns that), classifies the exit, and on
// an UNEXPECTED exit respawns with bounded backoff under a sliding
// window cap. Every return publishes the same completion and retained result;
// manager cleanup receives it before GUI terminal publication or lease release.
//
// Launched only when the seeded owner Spawned()==true.
func (m *supervisorManager) runRespawnLoop(ctx context.Context) {
	m.mu.Lock()
	// Direct test calls arm here; production arm publishes before go.
	if m.loopDone == nil {
		m.loopDone = make(chan struct{})
	}
	done := m.loopDone
	shutting := m.shuttingDown
	m.mu.Unlock()
	var result error
	defer func() {
		m.mu.Lock()
		m.loopErr = errors.Join(m.loopErr, result)
		m.mu.Unlock()
		close(done)
	}()
	if shutting {
		return
	}
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
		case <-m.ctx.Done():
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
		var completedAttempt *supervisorManagerOperation
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
			case <-m.ctx.Done():
				return
			case <-time.After(backoff):
			}
			// Explicit restart, respawn and shutdown share this admission.
			m.mu.Lock()
			stopping := m.shuttingDown || ctx.Err() != nil || m.ctx.Err() != nil || m.current != cur
			busy := m.operation
			var operation *supervisorManagerOperation
			if !stopping && busy == nil {
				operation = &supervisorManagerOperation{kind: "respawn", done: make(chan struct{}), original: cur}
				m.operation = operation
			}
			m.mu.Unlock()
			if stopping {
				return
			}
			if busy != nil {
				select {
				case <-busy.done:
				case <-ctx.Done():
					return
				case <-m.ctx.Done():
					return
				}
				continue
			}
			o, err := m.spawnFn(m.ctx, m.bin, m.strictMode, m.waitFor, m.receiveCreationAttempt(operation))
			if err != nil {
				if errors.Is(err, errSupervisorLaunchSettlement) {
					// Recovery does not erase an acquired startup cleanup failure.
					result = errors.Join(result, err)
				}
				if o != nil {
					result = errors.Join(result, err, m.stopOwnerAndJoin(context.Background(), o, 5000))
					m.completeManagerOperation(operation, gui.SupervisorRestartResult{}, result)
					return
				}
				m.completeManagerOperation(operation, gui.SupervisorRestartResult{}, err)
				m.mu.Lock()
				stopping := m.shuttingDown || ctx.Err() != nil || m.ctx.Err() != nil
				m.mu.Unlock()
				if stopping {
					// Startup returns only after original creator settlement.
					// Preserve its shutdown error rather than retrying it.
					if !errors.Is(result, err) {
						result = errors.Join(result, err)
					}
					return
				}
				// Transient/wedged spawn failure: log + RETRY (the inner
				// loop re-appends a window slot and re-checks the cap), so
				// a chronic failure trips the cap rather than silently
				// disabling GUI self-healing after a single error.
				_ = api.LogHubMcpEvent("warn", "gui-supervisor-respawn-failed", map[string]any{
					"error":            err.Error(),
					"creation_attempt": m.creationAttemptFields(operation),
				})
				fmt.Fprintf(m.stderrSink, "warning: supervisor respawn attempt failed: %v\n", err)
				continue
			}
			// Keep admission through the actual current-publication decision.
			newOwner = o
			m.mu.Lock()
			if m.shuttingDown || ctx.Err() != nil || m.ctx.Err() != nil {
				m.mu.Unlock()
				result = errors.Join(result, m.stopOwnerAndJoin(context.Background(), newOwner, 5000))
				m.completeManagerOperation(operation, gui.SupervisorRestartResult{}, result)
				return
			}
			m.current = newOwner
			m.mu.Unlock()
			m.completeManagerOperation(operation, gui.SupervisorRestartResult{}, nil)
			completedAttempt = operation
			break
		}
		_ = api.LogHubMcpEvent("info", "gui-supervisor-respawned-by-gui", map[string]any{
			"pid":              newOwner.Pid(),
			"creation_attempt": m.creationAttemptFields(completedAttempt),
		})
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
func (s *supervisorOwner) startExitMonitor(cmd *exec.Cmd) {
	proc := cmd.Process
	go func() {
		exitErr := cmd.Wait()
		s.receiveExitOutcome(exitErr)
		// Notification is separate from committing the acquired Wait and
		// releasing original completion. It publishes the latest snapshot.
		s.attempt.publish(func(*supervisorCreationSnapshot) {})
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
				// Same blindness as the readiness-timeout path above: a
				// post-lock death writes its traceback to the child's own
				// sink, not to this buffer, so an empty tail must point the
				// operator at the sink rather than imply silence.
				fmt.Fprintf(s.stderrSink, "warning: supervisor exited unexpectedly (PID %d): %v; no stderr captured before it rebound stderr — check supervisor-events.log and %s\n",
					proc.Pid, exitErr, supervisorStderrSinkHint())
			}
		}
	}()
}

// The original monitor commits its received Wait before releasing completion.
// No observer or diagnostic is invoked at this completion boundary.
func (s *supervisorOwner) receiveExitOutcome(exitErr error) {
	s.exitResult = exitInfo{exitErr: exitErr}
	if attempt := s.attempt; attempt != nil {
		attempt.mu.Lock()
		attempt.snapshot.WaitReceived, attempt.snapshot.WaitError = true, exitErr
		attempt.mu.Unlock()
	}
	close(s.exitDone)
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
	// running cmd.Wait() and retains its outcome behind exitDone as well as
	// notifying the respawn manager. Stop observes that retained original
	// result even if the manager has already consumed its notification.
	if s.exitDone == nil {
		return errors.New("supervisor owner: original exit observation unavailable")
	}
	exited := s.exitDone
	select {
	case <-exited:
		return s.exitResult.exitErr
	default:
	}

	// Issue the IPC exit concurrently under a sub-deadline derived
	// from graceTimeoutMs only — never the full parent ctx deadline.
	// This bounds total Stop wall-time to graceTimeoutMs + fallback
	// even when the supervisor's IPC pipe is hung or the supervisor
	// process is unresponsive.
	ipcDone := make(chan error, 1)
	s.ipcDone = make(chan struct{})
	go func() {
		defer close(s.ipcDone)
		ipcCtx, cancel := context.WithTimeout(ctx, time.Duration(graceTimeoutMs)*time.Millisecond)
		defer cancel()
		s.ipcErr = s.control.ExitGraceful(ipcCtx, graceTimeoutMs)
		ipcDone <- s.ipcErr
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
			return errors.Join(s.exitResult.exitErr, fmt.Errorf("supervisor owner: graceful exit IPC failed but process exited: %w", ipcErr))
		}
		return s.exitResult.exitErr
	case <-time.After(total):
		// Force-kill — supervisor didn't exit in time.
		killErr := s.proc.Kill()
		s.killErr = killErr
		s.killCommitted = killErr == nil
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

// supervisorSetupFaultError wraps the PROBE-path LOCAL setup fault into an
// operator-actionable startup error that names remediation for EACH of the
// distinct causes this path covers, not just a DACL refusal. It is reached
// ONLY from probeSupervisor returning api.ErrStatusSetupFailure — which
// dialSupervisorIPCStatusFromStateDir attaches to ALL non-IsNotExist read
// failures on a PRESENT supervisor.lock.owner.json sidecar. That failure set is
// EXACTLY these five (verified against ReadSupervisorLockOwner →
// readStateFileInodeAnchored + json.Unmarshal, internal/api/supervisor_lock.go:158,
// internal/api/hub_mcp_state_read_inode_windows.go, internal/api/hub_mcp_state_dacl.go):
//
//	(a) DACL/mode refusal (api.ErrDaclOutsideAllowlist / ErrTooLoose — a
//	    non-allowlisted SID has read/write access) → `mcphub repair-state-dacl`.
//	(b) corrupt/malformed JSON (json.Unmarshal failed) → delete + regenerate.
//	(c) api.ErrWrongOwner (the sidecar's owner SID/uid is NOT the current user)
//	    → repair-state-dacl REFUSES a wrong-owner file (state_dacl_repair_windows.go:47-56,
//	    state_dacl_repair_posix.go:95-97), so take ownership or delete + regenerate.
//	(d) api.ErrIrregularFile (the sidecar is a symlink / reparse point /
//	    non-regular file) → repair-state-dacl REFUSES an irregular file too
//	    (state_dacl_repair_windows.go:43+171, state_dacl_repair_posix.go:33+91), so
//	    delete the symlink/irregular sidecar and let it regenerate.
//	(e) strict-parent gate (MCPHUB_REQUIRE_SINGLE_USER_HOME=1 rejected a
//	    broadened parent directory) → tighten the parent DACL or unset the env var.
//
// (An ABSENT sidecar classifies as api.ErrSupervisorIPCUnavailable → the normal
// spawn path, so reaching here means NO supervisor is proven and spawning a
// duplicate would only re-hit the same local fault. The top-of-function
// api.DaemonStateDir() RESOLVE failure has its own dedicated error —
// supervisorStateDirResolveError — because its causes and fixes differ.)
//
// A single "run repair-state-dacl" instruction would be a dead end for (b),
// (c), and (d) — that command only re-grants the DACL/mode of an OWNED, REGULAR
// file; it does not rewrite invalid JSON, take ownership of a wrong-owner file,
// or replace a symlink. So the message enumerates all five and keeps the
// underlying cause wrapped (%w) so the operator sees which one applies.
//
// The DESTRUCTIVE remediations — (b) delete+regenerate, (c)/(d) take-ownership
// or delete — are ORDER-SENSITIVE: deleting or replacing supervisor.lock.owner.json
// while a supervisor still holds supervisor.lock.lock makes the next
// AcquireSupervisorLock fail closed. With a held flock and missing/invalid owner
// metadata it returns "supervisor.lock held but owner metadata invalid"
// (internal/api/supervisor_lock.go:76-83) after only a ~2s retry, so the next
// GUI/supervisor start refuses. Each destructive remediation therefore names the
// "stop any running supervisor first" ordering guard.
func supervisorSetupFaultError(cause error) error {
	var takeOwnership string
	if runtime.GOOS == "windows" {
		takeOwnership = "`icacls <sidecar> /setowner \"<you>\"` (or `takeown /F <sidecar>`) then re-grant owner-only"
	} else {
		takeOwnership = "`chown <you> <sidecar>`"
	}
	return fmt.Errorf("supervisor owner: local supervisor state is unreadable (NOT a running supervisor); "+
		"remediation depends on the wrapped error below, which names the exact cause: "+
		"(a) DACL/mode refusal on supervisor.lock.owner.json (non-allowlisted SID has access) → repair to owner-only "+
		"(`mcphub repair-state-dacl --path <sidecar>`, or tighten the parent DACL; see the "+
		"\"secret daemons exit 1 on a sandbox-broadened %%LOCALAPPDATA%%\" runbook in CLAUDE.md); "+
		"(b) corrupt/malformed supervisor.lock.owner.json (invalid JSON) → FIRST stop any running supervisor "+
		"(or confirm none holds supervisor.lock.lock), THEN delete the corrupt sidecar so it regenerates on next start; "+
		"(c) wrong-owner sidecar (owner is not your account) → repair-state-dacl refuses this, so FIRST stop any running supervisor, "+
		"THEN take ownership ("+takeOwnership+") or delete + regenerate the sidecar; "+
		"(d) irregular sidecar (a symlink / reparse point / non-regular file) → repair-state-dacl refuses this too, so FIRST stop any "+
		"running supervisor, THEN delete the symlink/irregular sidecar and let it regenerate; "+
		"(e) sidecar or its parent refused under MCPHUB_REQUIRE_SINGLE_USER_HOME=1 → tighten the parent directory's DACL to owner-only or unset that env var. "+
		"The wrapped error below names the exact cause. Then restart: %w", cause)
}

// supervisorStateDirResolveError wraps a top-of-function api.DaemonStateDir()
// RESOLVE failure into an operator-actionable startup error. This is a
// DIFFERENT fault from the probe-path setup fault above: no supervisor state
// file is even read here — the per-user state ROOT itself could not be
// resolved/created — so the three sidecar remediations do not apply. The
// causes (and fixes) are platform-specific, so the guidance line is selected
// by runtime.GOOS. Every hint below names ONLY inputs the resolver actually
// honors (verified against internal/api/state_paths*.go):
//
//   - Windows (GA): api.DaemonStateDir resolves LocalAppData by calling
//     windows.KnownFolderPath(FOLDERID_LocalAppData) DIRECTLY — there is NO
//     %LOCALAPPDATA% env fallback in production (state_paths_prod.go /
//     state_paths_windows.go), and MCPHUB_REQUIRE_SINGLE_USER_HOME is never
//     consulted here. Two distinct sub-causes, told apart by the wrapped cause:
//     (a) the Known-Folder lookup itself failed → repair the Windows
//     Known-Folder / user-profile registration (this is NOT an env-var
//     setting); (b) the state directory could not be created under the resolved
//     path → ensure the resolved LocalAppData path is writable.
//   - Linux: the state root lives under $XDG_DATA_HOME (fallback
//     ~/.local/share); state_paths.go:249. It does NOT read $XDG_STATE_HOME.
//   - macOS: the state root lives under ~/Library/Application Support (no XDG
//     at all); state_paths.go:262.
//   - POSIX parent-insecure case (errStateParentInsecure): the state-root
//     parent is group/world-writable or not owned by you, which fires
//     UNCONDITIONALLY → `chmod go-w` / `chown` the parent so it is owner-only.
//
// The underlying cause stays wrapped (%w) so the operator sees the exact
// resolver error (e.g. the failing known-folder call or the offending
// parent-dir mode/owner) and can tell the sub-causes apart.
func supervisorStateDirResolveError(cause error) error {
	var remediation string
	switch runtime.GOOS {
	case "windows":
		remediation = "either the LocalAppData Known Folder could not be resolved " +
			"(SHGetKnownFolderPath(FOLDERID_LocalAppData) failed — repair the Windows " +
			"Known-Folder / user-profile registration; this is NOT an env-var setting) " +
			"OR the state directory could not be created under the resolved path " +
			"(ensure the resolved LocalAppData path is writable); the wrapped cause below distinguishes them"
	case "darwin":
		remediation = "the state root under ~/Library/Application Support could not be resolved or created " +
			"→ ensure $HOME is set and that ~/Library/Application Support is owner-only " +
			"(`chmod go-w` / `chown` it if the parent is group/world-writable or not owned by you)"
	default: // linux and other POSIX
		remediation = "the state root under $XDG_DATA_HOME (fallback ~/.local/share) could not be resolved or created " +
			"→ ensure $XDG_DATA_HOME (or ~/.local/share) is owner-only " +
			"(`chmod go-w` / `chown` the parent if it is group/world-writable or not owned by you)"
	}
	return fmt.Errorf("supervisor owner: cannot resolve the per-user state directory (NOT a running supervisor); "+
		"%s. Then restart: %w", remediation, cause)
}

// probeSupervisor performs a single IPC handshake + status query
// against the running supervisor. Returns (true, nil) when reachable.
// Returns (false, nil) ONLY for the expected pre-bind state
// (ErrSupervisorIPCUnavailable — no lock owner sidecar, no pipe, or
// connect-refused). Any other failure returns (false, error) so the
// caller can distinguish "supervisor not yet up" from a real fault and
// surface the actionable error to the operator. The caller further
// splits that error path: a LOCAL setup fault (api.ErrStatusSetupFailure
// — present-but-unreadable/DACL-refused owner sidecar) is NOT a
// live-but-broken supervisor and is surfaced via supervisorSetupFaultError
// (repair-state-dacl remediation), while a genuine transport/handshake
// failure (hello-version mismatch, JSON decode error, ID mismatch) keeps
// the "supervisor up but broken" wording.
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

// resolveMCPHubBinary uses the API-owned admitted runtime-image authority,
// retaining a verified active Product owner instead of selecting a helper.
func resolveMCPHubBinary() (string, error) {
	return api.RuntimeMcphubExecutable()
}

// settleUnboundCreator settles only the GUI's actual child when readiness or a
// singleton race fails. The existing exit monitor remains the sole Wait owner.
func (s *supervisorOwner) settleUnboundCreator() (result error) {
	defer func() {
		if result != nil {
			result = errors.Join(errSupervisorLaunchSettlement, result)
		}
		s.attempt.publish(func(snapshot *supervisorCreationSnapshot) {
			snapshot.SettlementError = errors.Join(snapshot.SettlementError, result)
		})
	}()
	s.stopRequested.Store(true)
	var killErr error
	select {
	case <-s.exitDone:
	default:
		killErr = s.proc.Kill()
		if errors.Is(killErr, os.ErrProcessDone) {
			killErr = nil
		}
		// Preserve the original startup cleanup's creator Wait semantics.
		// Cancellation does not abandon this original child or start a timer.
		<-s.exitDone
	}
	var exitStatus *exec.ExitError
	if s.exitResult.exitErr != nil && !errors.As(s.exitResult.exitErr, &exitStatus) {
		return errors.Join(killErr, s.exitResult.exitErr)
	}
	// A native nonzero exit is retained in exitResult, distinct from a failed
	// Kill/Wait operation; a singleton loser can be settled without exit0.
	return killErr
}
