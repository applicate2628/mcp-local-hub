// Package tray runs the system tray icon as a SEPARATE OS PROCESS
// to guarantee the tray menu remains responsive even when the main
// GUI process is busy, blocked, or its message pump is starved.
//
// Architecture (PR #25):
//
//	┌──────────────────┐  child stdin (state JSON lines)   ┌───────────────────┐
//	│  mcphub gui      │ ────────────────────────────────► │  mcphub tray      │
//	│  (main process)  │                                   │  (child process)  │
//	│                  │  child stdout (event JSON lines)  │  - direct Win32   │
//	│  - HTTP server   │ ◄──────────────────────────────── │  - own message    │
//	│  - status poller │                                   │    pump, no       │
//	│  - this Run()    │                                   │    network I/O    │
//	│    spawns child  │                                   │                   │
//	└──────────────────┘                                   └───────────────────┘
//
// Why a subprocess: previous in-process designs (getlantern/systray
// then fyne.io/systray) suffered from "right-click menu doesn't
// appear after external foreground change" symptoms because (a) the
// click handler made an HTTP self-call to /api/activate-window
// (latency on the menu thread), and (b) Win32 TrackPopupMenu
// requires the calling thread to be the foreground thread — any
// foreground-grab from another process (e.g. Chrome tab tear-off)
// could silently disable the popup. Moving the tray to its own
// process puts the menu's message pump on a fresh thread that owns
// no shared state and never makes HTTP calls.
//
// The child implements the tray via direct Win32 syscalls (user32
// + shell32) — no CGo and no third-party tray library. The public
// API is platform-agnostic; on non-Windows hosts the child is a
// no-op (tray is Windows-only per spec §2.2).
package tray

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
	"sync"
	"time"

	"mcp-local-hub/internal/process"
)

// Config is what Run needs to produce the menu and route actions.
type Config struct {
	// ActivateWindow is called when the user picks "Open dashboard"
	// from the right-click menu (or left-clicks the icon).
	ActivateWindow func()
	// Quit is called when the user picks "Quit (keep daemons)".
	Quit func()
	// QuitAndStopAll is called when the user picks "Quit and stop all
	// daemons" from the tray menu. Implementer should stop every
	// running daemon (api.StopAll) and then trigger the same GUI
	// shutdown path as Quit. Optional: if nil, the menu falls back
	// to plain Quit semantics so the menu item never silently no-ops.
	QuitAndStopAll func()
	// RunAllDaemons is called when the user picks "Run all daemons"
	// from the tray menu. Implementer should call api.RestartAll —
	// restart of a stopped daemon is functionally a start, so this
	// serves as "Run all" for the user. Fire-and-forget: GUI stays
	// open. Optional: silently no-op if nil.
	RunAllDaemons func()
	// StopAllDaemons is called when the user picks "Stop all daemons"
	// from the tray menu. Implementer should call api.StopAll.
	// Fire-and-forget: GUI stays open. Optional: silently no-op if nil.
	StopAllDaemons func()
	// RescanClients triggers a fresh /api/scan reload. The GUI process
	// is the only place that holds the cached scan state, so the tray
	// hands the click off via SSE/event broadcaster. Optional: nil
	// silently no-ops.
	RescanClients func()
	// OpenLogsFolder opens the OS file manager at the canonical
	// daemon-log directory. Implementer typically calls
	// gui.OpenPath(api.DefaultLogDir()). Optional.
	OpenLogsFolder func()
	// OpenDataFolder opens the OS file manager at the mcp-local-hub
	// per-user data directory (parent of gui-preferences.yaml /
	// secrets vault). Optional.
	OpenDataFolder func()
	// StateCh delivers TrayState transitions. The parent forwards
	// each value to the child as a JSON state line.
	StateCh <-chan TrayState
	// StateReadRelaxCh delivers the current value of
	// HKCU\Environment\MCPHUB_ALLOW_UNHARDENED_STATE_READ. Each value
	// is forwarded to the child as a state-line carrying
	// {state_read_relax: bool}; the child uses it to toggle the
	// MF_CHECKED glyph next to the "Allow strict-DACL relax" menu
	// item. Optional: nil channel disables the menu's check-state
	// tracking (the item still appears, just always unchecked).
	StateReadRelaxCh <-chan bool
	// ToggleStateReadRelax is called when the user picks the
	// "Allow strict-DACL relax" menu item. Implementer POSTs to
	// /api/settings/state-read-relax with the inverse of the
	// currently-known value. Optional: silently no-op if nil.
	ToggleStateReadRelax func()
	// LifecycleReport receives bounded, path-free child recovery facts.
	// It is optional and is invoked synchronously; the GUI binds it to its
	// existing non-blocking Broadcaster.Publish sink.
	LifecycleReport func(LifecycleReport)
}

// LifecycleReport is a typed tray-child recovery fact. The GUI composition
// root maps it onto its existing event broadcaster; tray owns neither the
// broadcaster nor durable event storage.
type LifecycleReport struct {
	Type             string
	Phase            string
	Attempt          int
	NextRetryMS      int64
	Failures         int
	OutageDurationMS int64
	AttemptCount     int
	ErrorDetail      string
}

type lifecyclePhase string

const (
	lifecyclePhaseSelfPath   lifecyclePhase = "self-path"
	lifecyclePhaseStdinPipe  lifecyclePhase = "stdin-pipe"
	lifecyclePhaseStdoutPipe lifecyclePhase = "stdout-pipe"
	lifecyclePhaseSpawn      lifecyclePhase = "spawn"
	lifecyclePhaseChildExit  lifecyclePhase = "child-exit"

	lifecycleEventUnavailable     = "tray-child-unavailable"
	lifecycleEventRetrySummary    = "tray-child-retry-summary"
	lifecycleEventProtocolWarning = "tray-child-protocol-warning"
	lifecycleEventStable          = "tray-child-stable"

	lifecycleStableAfter     = 30 * time.Second
	lifecycleSummaryInterval = 30 * time.Second
	lifecycleShutdownTimeout = 2 * time.Second
)

var lifecycleRetryDelays = [...]time.Duration{
	100 * time.Millisecond,
	250 * time.Millisecond,
	500 * time.Millisecond,
	time.Second,
	2 * time.Second,
}

type lifecycleClock interface {
	Now() time.Time
	Wait(context.Context, time.Duration) bool
}

type realLifecycleClock struct{}

func (realLifecycleClock) Now() time.Time { return time.Now() }

func (realLifecycleClock) Wait(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

type childAttempt struct {
	stdin  io.WriteCloser
	stdout io.ReadCloser
	wait   func() error
	kill   func() error
}

type lifecycleDeps struct {
	launch func() (*childAttempt, lifecyclePhase, error)
	clock  lifecycleClock
}

type stateSnapshot struct {
	generation uint64
	hasState   bool
	state      string
	hasRelax   bool
	relax      bool
}

type stateCollector struct {
	requests chan chan stateSnapshot
	updates  chan stateSnapshot
	done     chan struct{}
}

func startStateCollector(ctx context.Context, cfg Config, cancelRun context.CancelFunc) *stateCollector {
	c := &stateCollector{
		requests: make(chan chan stateSnapshot),
		updates:  make(chan stateSnapshot, 1),
		done:     make(chan struct{}),
	}
	go func() {
		defer close(c.done)
		stateCh := cfg.StateCh
		relaxCh := cfg.StateReadRelaxCh
		var snapshot stateSnapshot
		publish := func() {
			select {
			case c.updates <- snapshot:
			default:
				select {
				case <-c.updates:
				default:
				}
				select {
				case c.updates <- snapshot:
				default:
				}
			}
		}
		for {
			select {
			case <-ctx.Done():
				return
			case reply := <-c.requests:
				reply <- snapshot
			case state, ok := <-stateCh:
				if !ok {
					cancelRun()
					return
				}
				snapshot.generation++
				snapshot.hasState = true
				snapshot.state = state.String()
				publish()
			case relax, ok := <-relaxCh:
				if !ok {
					relaxCh = nil
					continue
				}
				snapshot.generation++
				snapshot.hasRelax = true
				snapshot.relax = relax
				publish()
			}
		}
	}()
	return c
}

func (c *stateCollector) snapshot(ctx context.Context) (stateSnapshot, bool) {
	reply := make(chan stateSnapshot, 1)
	select {
	case c.requests <- reply:
	case <-ctx.Done():
		return stateSnapshot{}, false
	}
	select {
	case snapshot := <-reply:
		return snapshot, true
	case <-ctx.Done():
		return stateSnapshot{}, false
	}
}

// Run owns the restartable tray subprocess lifecycle and routes events between
// it and the in-process Config callbacks. It returns when ctx is canceled or
// StateCh closes; child setup failures and exits are retried while the parent
// lifecycle remains live. On non-Windows hosts it blocks on ctx.Done() without
// starting a collector, timer, or child.
//
// The subprocess is the running mcphub binary invoked with the
// hidden `tray` subcommand: `<self> tray`. The child reads JSON
// state lines from its stdin and writes JSON event lines to its
// stdout; see RunChild for the wire format.
func Run(ctx context.Context, cfg Config) error {
	// Tray is Windows-only in MVP — the non-Windows runChildImpl is a
	// no-op (returns immediately on stdin EOF). Spawning a child just
	// to have it exit instantly would (a) violate Run's "block until
	// ctx.Done()" contract for any caller relying on it for lifecycle
	// coordination, and (b) waste a process spawn on every Linux/macOS
	// GUI start. Block on ctx here directly so the goroutine that
	// `mcphub gui` launches stays alive for the GUI's lifetime.
	// Codex bot review on PR #24 P2 (bypass non-Windows spawn).
	if runtime.GOOS != "windows" {
		<-ctx.Done()
		return nil
	}
	return runLifecycle(ctx, cfg, lifecycleDeps{launch: launchTrayChild, clock: realLifecycleClock{}})
}

func launchTrayChild() (*childAttempt, lifecyclePhase, error) {
	selfPath, err := os.Executable()
	if err != nil {
		return nil, lifecyclePhaseSelfPath, err
	}
	c := exec.Command(selfPath, "tray") // intentionally not CommandContext; graceful stdin EOF comes first
	process.NoConsole(c)
	stdin, err := c.StdinPipe()
	if err != nil {
		return nil, lifecyclePhaseStdinPipe, err
	}
	stdout, err := c.StdoutPipe()
	if err != nil {
		_ = stdin.Close()
		return nil, lifecyclePhaseStdoutPipe, err
	}
	if stderrIsValid() {
		c.Stderr = os.Stderr
	}
	if err := c.Start(); err != nil {
		_ = stdin.Close()
		_ = stdout.Close()
		return nil, lifecyclePhaseSpawn, err
	}
	return &childAttempt{
		stdin:  stdin,
		stdout: stdout,
		wait:   c.Wait,
		kill: func() error {
			if c.Process == nil {
				return nil
			}
			return c.Process.Kill()
		},
	}, "", nil
}

func runLifecycle(ctx context.Context, cfg Config, deps lifecycleDeps) error {
	runCtx, cancelRun := context.WithCancel(ctx)
	collector := startStateCollector(runCtx, cfg, cancelRun)
	defer func() {
		cancelRun()
		<-collector.done
	}()

	retryIndex := 0
	attemptNumber := 0
	reporter := lifecycleReporter{emit: cfg.LifecycleReport, clock: deps.clock}
	for {
		if runCtx.Err() != nil {
			return nil
		}
		attemptNumber++
		attempt, phase, err := deps.launch()
		if runCtx.Err() != nil {
			if attempt != nil {
				settleUnservedAttempt(attempt, deps.clock)
			}
			return nil
		}
		if err != nil {
			delay := lifecycleRetryDelays[retryIndex]
			if retryIndex < len(lifecycleRetryDelays)-1 {
				retryIndex++
			}
			reporter.failure(phase, attemptNumber, delay, err)
			if !deps.clock.Wait(runCtx, delay) {
				return nil
			}
			continue
		}
		if runCtx.Err() != nil {
			settleUnservedAttempt(attempt, deps.clock)
			return nil
		}
		snapshot, ok := collector.snapshot(runCtx)
		if !ok {
			settleUnservedAttempt(attempt, deps.clock)
			return nil
		}
		result := serveAttempt(runCtx, cfg, attempt, snapshot, collector.updates, deps.clock, func() {
			retryIndex = 0
			reporter.stable(attemptNumber)
		}, func() {
			reporter.protocolWarning(attemptNumber)
		})
		if result.terminal || runCtx.Err() != nil {
			return nil
		}
		delay := lifecycleRetryDelays[retryIndex]
		if retryIndex < len(lifecycleRetryDelays)-1 {
			retryIndex++
		}
		reporter.failure(lifecyclePhaseChildExit, attemptNumber, delay, result.err)
		if !deps.clock.Wait(runCtx, delay) {
			return nil
		}
	}
}

type attemptResult struct {
	terminal bool
	err      error
}

func serveAttempt(ctx context.Context, cfg Config, attempt *childAttempt, snapshot stateSnapshot, updates <-chan stateSnapshot, clock lifecycleClock, onStable, onProtocolWarning func()) attemptResult {
	startedAt := clock.Now()
	stable := false
	markStable := func() {
		if stable {
			return
		}
		stable = true
		onStable()
	}
	markStableIfElapsed := func() {
		if clock.Now().Sub(startedAt) >= lifecycleStableAfter {
			markStable()
		}
	}
	attemptCtx, cancelAttempt := context.WithCancel(ctx)
	defer cancelAttempt()

	writerDone := make(chan error, 1)
	readerDone := make(chan error, 1)
	protocolWarningCh := make(chan struct{})
	waitDone := make(chan error, 1)
	stableCh := make(chan struct{}, 1)
	var workers sync.WaitGroup
	workers.Add(3)
	go func() {
		defer workers.Done()
		writerDone <- writeAttemptStates(attemptCtx, attempt.stdin, snapshot, updates)
	}()
	go func() {
		defer workers.Done()
		readerDone <- readAttemptEvents(attemptCtx, attempt.stdout, cfg, protocolWarningCh)
	}()
	go func() {
		defer workers.Done()
		if clock.Wait(attemptCtx, lifecycleStableAfter) {
			stableCh <- struct{}{}
		}
	}()
	go func() { waitDone <- attempt.wait() }()

	waited := false
	var waitErr error
	for {
		select {
		case <-ctx.Done():
			waitErr = settleAttempt(attempt, cancelAttempt, waitDone, waited, waitErr, clock)
			workers.Wait()
			return attemptResult{terminal: true, err: waitErr}
		case err := <-waitDone:
			waited, waitErr = true, err
			markStableIfElapsed()
			cancelAttempt()
			_ = attempt.stdin.Close()
			_ = attempt.stdout.Close()
			workers.Wait()
			return attemptResult{err: waitErr}
		case err := <-readerDone:
			markStableIfElapsed()
			waitErr = settleAttempt(attempt, cancelAttempt, waitDone, waited, err, clock)
			workers.Wait()
			return attemptResult{err: waitErr}
		case err := <-writerDone:
			if err == nil && attemptCtx.Err() != nil {
				continue
			}
			markStableIfElapsed()
			waitErr = settleAttempt(attempt, cancelAttempt, waitDone, waited, err, clock)
			workers.Wait()
			return attemptResult{err: waitErr}
		case <-stableCh:
			markStable()
			stableCh = nil
		case <-protocolWarningCh:
			onProtocolWarning()
		}
	}
}

func settleAttempt(attempt *childAttempt, cancel context.CancelFunc, waitDone <-chan error, waited bool, prior error, clock lifecycleClock) error {
	cancel()
	_ = attempt.stdin.Close()
	waitErr := prior
	if !waited {
		shutdownCtx, stopShutdown := context.WithCancel(context.Background())
		graceExpired := make(chan bool, 1)
		go func() { graceExpired <- clock.Wait(shutdownCtx, lifecycleShutdownTimeout) }()
		select {
		case err := <-waitDone:
			stopShutdown()
			<-graceExpired
			waitErr = err
		case expired := <-graceExpired:
			stopShutdown()
			if expired {
				_ = attempt.kill()
			}
			waitErr = <-waitDone
		}
	}
	_ = attempt.stdout.Close()
	return waitErr
}

func settleUnservedAttempt(attempt *childAttempt, clock lifecycleClock) {
	_, cancel := context.WithCancel(context.Background())
	defer cancel()
	waitDone := make(chan error, 1)
	go func() { waitDone <- attempt.wait() }()
	_ = settleAttempt(attempt, cancel, waitDone, false, nil, clock)
}

func writeAttemptStates(ctx context.Context, stdin io.WriteCloser, initial stateSnapshot, updates <-chan stateSnapshot) error {
	defer stdin.Close()
	enc := json.NewEncoder(stdin)
	lastGeneration := initial.generation
	if initial.hasState || initial.hasRelax {
		if err := enc.Encode(snapshotMessage(initial)); err != nil {
			return err
		}
	}
	for {
		select {
		case <-ctx.Done():
			return nil
		case snapshot := <-updates:
			if snapshot.generation <= lastGeneration {
				continue
			}
			if err := enc.Encode(snapshotMessage(snapshot)); err != nil {
				return err
			}
			lastGeneration = snapshot.generation
		}
	}
}

func snapshotMessage(snapshot stateSnapshot) stateMessage {
	message := stateMessage{}
	if snapshot.hasState {
		message.State = snapshot.state
	}
	if snapshot.hasRelax {
		relax := snapshot.relax
		message.StateReadRelax = &relax
	}
	return message
}

func readAttemptEvents(ctx context.Context, stdout io.Reader, cfg Config, protocolWarnings chan<- struct{}) error {
	scanner := bufio.NewScanner(stdout)
	for scanner.Scan() {
		var ev eventMessage
		if err := json.Unmarshal(scanner.Bytes(), &ev); err != nil {
			select {
			case protocolWarnings <- struct{}{}:
			case <-ctx.Done():
				return ctx.Err()
			}
			continue
		}
		dispatchEvent(ev.Event, cfg)
	}
	return scanner.Err()
}

type lifecycleReporter struct {
	emit               func(LifecycleReport)
	clock              lifecycleClock
	outageStart        time.Time
	outageStartAttempt int
	lastReport         time.Time
	failures           int
	lastProtocolWarn   time.Time
	hasProtocolWarn    bool
}

func (r *lifecycleReporter) failure(phase lifecyclePhase, attempt int, retry time.Duration, err error) {
	if r.emit == nil {
		return
	}
	now := r.clock.Now()
	if r.outageStart.IsZero() {
		r.outageStart = now
		r.outageStartAttempt = attempt
		r.lastReport = now
		r.emit(LifecycleReport{Type: lifecycleEventUnavailable, Phase: string(phase), Attempt: attempt, NextRetryMS: retry.Milliseconds(), ErrorDetail: safeErrorDetail(err)})
		return
	}
	r.failures++
	if now.Sub(r.lastReport) < lifecycleSummaryInterval {
		return
	}
	r.emit(LifecycleReport{Type: lifecycleEventRetrySummary, Phase: string(phase), Attempt: attempt, NextRetryMS: retry.Milliseconds(), Failures: r.failures, ErrorDetail: safeErrorDetail(err)})
	r.failures = 0
	r.lastReport = now
}

func (r *lifecycleReporter) stable(attempt int) {
	if r.emit == nil || r.outageStart.IsZero() {
		return
	}
	r.emit(LifecycleReport{Type: lifecycleEventStable, Attempt: attempt, OutageDurationMS: r.clock.Now().Sub(r.outageStart).Milliseconds(), AttemptCount: attempt - r.outageStartAttempt + 1})
	r.outageStart = time.Time{}
	r.outageStartAttempt = 0
	r.lastReport = time.Time{}
	r.failures = 0
}

func (r *lifecycleReporter) protocolWarning(attempt int) {
	if r.emit == nil {
		return
	}
	now := r.clock.Now()
	if r.hasProtocolWarn && now.Sub(r.lastProtocolWarn) < lifecycleSummaryInterval {
		return
	}
	r.emit(LifecycleReport{Type: lifecycleEventProtocolWarning, Phase: "event-json-invalid", Attempt: attempt})
	r.lastProtocolWarn = now
	r.hasProtocolWarn = true
}

func safeErrorDetail(err error) string {
	if err == nil {
		return "process ended"
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && exitErr.ProcessState != nil {
		return fmt.Sprintf("process exited with status %d", exitErr.ProcessState.ExitCode())
	}
	switch {
	case errors.Is(err, os.ErrPermission):
		return "permission denied"
	case errors.Is(err, os.ErrNotExist):
		return "resource not found"
	case errors.Is(err, io.ErrClosedPipe):
		return "pipe closed"
	default:
		return "operation failed"
	}
}

// RunChild is the entry point for the tray subprocess. It reads
// JSON state lines from r (stdin) and writes JSON event lines to w
// (stdout). On stdin EOF (parent closed the pipe / parent exited),
// it tears down the systray cleanly and returns.
//
// Wire format:
//
//	parent → child (stdin):  {"state":"healthy"}\n
//	                         {"state":"partial"}\n  ...
//	child → parent (stdout): {"event":"open-dashboard"}\n
//	                         {"event":"quit"}\n
//
// Lines are newline-delimited JSON; any encoding/parsing error on
// either end is logged to stderr but does not kill the connection.
func RunChild(r io.Reader, w io.Writer) error {
	return runChildImpl(r, w)
}

// dispatchEvent routes one tray event name to the matching cfg
// callback. Extracted from Run's scanner loop so unit tests can
// exercise the full event-name → callback mapping (including the
// nil-callback no-op behavior and the QuitAndStopAll fallback)
// without spawning the tray subprocess.
//
// Unknown event names land on the default branch which logs to
// stderr — keeps the helper a single source of truth for "which
// event names this tray understands".
func dispatchEvent(name string, cfg Config) {
	switch name {
	case "open-dashboard":
		if cfg.ActivateWindow != nil {
			cfg.ActivateWindow()
		}
	case "quit":
		if cfg.Quit != nil {
			cfg.Quit()
		}
	case "quit-and-stop-all":
		// Fall back to plain Quit if the GUI didn't wire the
		// stronger callback — never silently swallow a user
		// click. The fallback at least closes the GUI; daemons
		// keep running, which mirrors the existing "Quit (keep
		// daemons)" item rather than failing closed.
		switch {
		case cfg.QuitAndStopAll != nil:
			cfg.QuitAndStopAll()
		case cfg.Quit != nil:
			cfg.Quit()
		}
	case "run-all":
		if cfg.RunAllDaemons != nil {
			cfg.RunAllDaemons()
		}
	case "stop-all":
		if cfg.StopAllDaemons != nil {
			cfg.StopAllDaemons()
		}
	case "rescan-clients":
		if cfg.RescanClients != nil {
			cfg.RescanClients()
		}
	case "open-logs-folder":
		if cfg.OpenLogsFolder != nil {
			cfg.OpenLogsFolder()
		}
	case "open-data-folder":
		if cfg.OpenDataFolder != nil {
			cfg.OpenDataFolder()
		}
	case "toggle-state-relax":
		if cfg.ToggleStateReadRelax != nil {
			cfg.ToggleStateReadRelax()
		}
	default:
		fmt.Fprintf(os.Stderr, "tray: unknown event %q\n", name)
	}
}

// stateMessage is the wire-format payload parent sends to child on stdin.
// Either State (daemon-tray-state transition) OR Misc payloads carry the
// envelope; absent fields are zero-valued and skipped by the child.
type stateMessage struct {
	State string `json:"state,omitempty"`
	// StateReadRelax is the current HKCU value of
	// MCPHUB_ALLOW_UNHARDENED_STATE_READ. Sent independently of
	// State transitions so the menu's check-glyph stays in sync.
	// Use a pointer so a missing field doesn't flip the toggle to
	// false on every state-line update.
	StateReadRelax *bool `json:"state_read_relax,omitempty"`
}

// eventMessage is the wire-format payload child sends to parent on stdout.
type eventMessage struct {
	Event string `json:"event"`
}

// parseStateLabel maps a String() label back to its TrayState.
// Used by the child to decode state lines from the parent.
func parseStateLabel(label string) (TrayState, bool) {
	switch label {
	case "healthy":
		return StateHealthy, true
	case "partial":
		return StatePartial, true
	case "down":
		return StateDown, true
	case "error":
		return StateError, true
	}
	return StateHealthy, false
}
