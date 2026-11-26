//go:build windows

// Windows autostart backend: installs one `\mcp-local-hub-supervisor` Task
// Scheduler entry with a LogonTrigger that fires the selected persisted owner
// (`mcphub.exe gui --no-browser` or `mcphub.exe supervise`) at user logon.
//
// Reuses the existing `internal/scheduler.Scheduler` primitives so the
// XML-generation, schtasks shell-out, and per-user identity gating all
// stay in one place. Test-only callers swap the scheduler factory via
// the package-level `schedulerFactoryFn` seam.
package autostart

import (
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"strings"

	"mcp-local-hub/internal/scheduler"
)

// schedulerFactoryFn is the test seam — production paths leave it
// pointing at scheduler.New, but autostart tests inject a recording
// fake here so the real Task Scheduler is never touched. The
// `t.Cleanup` pattern restores the prior factory between tests.
var schedulerFactoryFn = scheduler.New

// windowsBackend is the per-OS Backend implementation. Stateless;
// every call re-derives the scheduler handle through the seam so
// tests can swap fakes without re-constructing the Backend.
type windowsBackend struct{}

// newPlatformBackend is the dispatcher entry point called from
// autostart.New(). Lives in this file (windows.go) under the
// `//go:build windows` tag; the Linux and macOS files supply their
// own implementations.
func newPlatformBackend() (Backend, error) {
	return &windowsBackend{}, nil
}

// superviseArgs returns the argv slice the LogonTrigger passes to the
// mcphub binary for the selected single owner mode.
//
// As of 2026-05-18 the autostart entry launches `mcphub gui` instead
// of `mcphub supervise` — see the "GUI owns supervisor lifecycle"
// design note in internal/cli/gui_supervisor_owner.go. The GUI process
// adopts (or spawns) the supervisor on startup and shuts it down on
// exit. One process tree, one tray indicator: tray visible = mcphub
// running.
//
// `--strict-mode` (when set) still threads through so the GUI's
// supervisor spawn inherits the right intent flag. Drift detection
// relies on the exact-element diff between the two modes.
//
// Both owner modes use the same canonical task name.
func superviseArgs(opts Options) ([]string, error) {
	// --no-browser: the autostart/logon/liveness-recovery GUI is a headless
	// server + tray indicator, NOT an interactive `mcphub gui`. Without it,
	// EVERY relaunch (user logon OR a `supervise --ensure-alive` recovery after
	// the fleet is killed) auto-opened a browser window — spamming the operator
	// with tabs on every fleet churn. The tray icon signals "running"; the
	// operator opens the dashboard from the tray. (Bug 2026-07-18: repeated
	// test-sweeps of mcphub.exe triggered repeated autostart relaunches, each
	// popping a browser.) See memory feedback_gui_always_tray (tray on, browser
	// off) + gui.go's --no-browser flag.
	mode := opts.OwnerMode
	if mode == "" {
		mode = OwnerModeGUI
	}
	var args []string
	switch mode {
	case OwnerModeGUI:
		args = []string{"gui", "--no-browser"}
	case OwnerModeSupervise:
		args = []string{"supervise"}
	default:
		return nil, fmt.Errorf("invalid owner mode %q", mode)
	}
	if opts.StrictMode {
		args = append(args, "--strict-mode")
	}
	return args, nil
}

// Enable creates the autostart task after confirmed absence. An existing
// registration is retained, including one whose visible arguments match.
func (w *windowsBackend) Enable(opts Options) error {
	args, err := superviseArgs(opts)
	if err != nil {
		return err
	}
	sched, err := schedulerFactoryFn()
	if err != nil {
		return fmt.Errorf("scheduler factory: %w", err)
	}
	cmd, err := resolveWindowsOwnedEntrypoint(opts)
	if err != nil {
		return err
	}
	prior, err := observeWindowsAutostartDefinition(sched)
	if err != nil {
		return err
	}
	if len(prior) != 0 {
		return NewObservedTaskReplacementRefusal(WindowsTaskName, prior, nil)
	}
	spec := scheduler.TaskSpec{
		Name:         WindowsTaskName,
		Description:  "mcp-local-hub supervisor (autostart shim, plan §2531-2541)",
		Command:      cmd,
		Args:         args,
		LogonTrigger: true,
		// RestartOnFailure intentionally false: the supervisor's own
		// crash policy is handled by the supervise runtime (Job
		// Object child reaping + watchdog tick). Task Scheduler's
		// RestartOnFailure historically races with our recovery
		// state machine (see CLAUDE.md "Watchdog (Phase 3B-II)"
		// section on RestartOnFailure / IgnoreNew); we keep that
		// decision narrow to the watchdog task and out of autostart.
		RestartOnFailure: false,
	}
	if err := sched.Create(spec); err != nil {
		createErr := fmt.Errorf("create autostart task %s: %w", WindowsTaskName, err)
		observed, readErr := observeWindowsAutostartDefinition(sched)
		if readErr != nil {
			return errors.Join(createErr, readErr)
		}
		if len(observed) != 0 {
			return NewObservedTaskReplacementRefusal(WindowsTaskName, observed, createErr)
		}
		return createErr
	}
	return nil
}

func observeWindowsAutostartDefinition(sched scheduler.Scheduler) ([]byte, error) {
	raw, err := sched.ExportXML(WindowsTaskName)
	if errors.Is(err, scheduler.ErrTaskNotFound) && len(raw) == 0 {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("observe autostart task %s: %w", WindowsTaskName, err)
	}
	if len(raw) == 0 {
		return nil, fmt.Errorf("observe autostart task %s: %w: empty XML", WindowsTaskName, scheduler.ErrTaskCorrupt)
	}
	return raw, nil
}

func resolveWindowsOwnedEntrypoint(opts Options) (string, error) {
	cmd, err := resolveMCPHubPath(opts)
	if err != nil {
		return "", err
	}
	// An explicit path is an already-resolved test/operator contract. The
	// production default is the same canonical mcphub.exe product image.
	if opts.MCPHubPath != "" {
		return cmd, nil
	}
	return cmd, nil
}

// Disable removes the autostart Task Scheduler entry. Idempotent —
// scheduler.Delete is documented to return nil when the task is
// already absent, so callers can re-run Disable without checking
// state first.
func (w *windowsBackend) Disable() error {
	sched, err := schedulerFactoryFn()
	if err != nil {
		return fmt.Errorf("scheduler factory: %w", err)
	}
	if err := sched.Delete(WindowsTaskName); err != nil {
		return fmt.Errorf("delete task: %w", err)
	}
	return nil
}

// Status reports the current State of the autostart shim.
//
// Decision tree:
//
//   - Status(name) returns ErrTaskNotFound → StateAbsent.
//   - Status(name) returns State == "Running" + XML matches opts →
//     StateEnabledRunning.
//   - Status(name) returns State == "Running" + XML disagrees with
//     opts → StateDrifted.
//   - Status(name) returns any other state (Ready, Disabled, …) +
//     XML matches → StateEnabledStopped.
//   - Status(name) returns any other state + XML disagrees → StateDrifted.
//
// ExportXML failures other than ErrTaskNotFound are non-fatal:
// drift detection skips silently and Status falls back to the basic
// running/stopped classification. ErrTaskNotFound from ExportXML in
// the middle of a Status call (a transient race vs concurrent
// Delete) is treated the same way — keep the running/stopped verdict
// rather than flipping to StateDrifted on best-effort failure.
func (w *windowsBackend) Status(opts Options) (State, error) {
	snapshot, err := w.statusSnapshot(opts, false)
	if errors.Is(err, scheduler.ErrUnavailable) {
		return snapshot.State, ErrStatusObservationUnavailable
	}
	return snapshot.State, err
}

// StatusSnapshot fingerprints the complete observed Task Scheduler XML and the
// runtime disabled bit. Running-vs-stopped transitions do not change it.
// Equality describes the observation and grants no generation ownership.
func (w *windowsBackend) StatusSnapshot(opts Options) (StatusSnapshot, error) {
	return w.statusSnapshot(opts, true)
}

func (w *windowsBackend) statusSnapshot(opts Options, failClosedXML bool) (StatusSnapshot, error) {
	sched, err := schedulerFactoryFn()
	if err != nil {
		return StatusSnapshot{State: StateAbsent}, fmt.Errorf("scheduler factory: %w", err)
	}
	st, err := sched.Status(WindowsTaskName)
	if err != nil {
		if errors.Is(err, scheduler.ErrTaskNotFound) {
			return StatusSnapshot{State: StateAbsent}, nil
		}
		return StatusSnapshot{State: StateAbsent}, fmt.Errorf("scheduler status: %w", err)
	}

	drift := false
	spec := ""
	xmlBlob, xmlErr := sched.ExportXML(WindowsTaskName)
	if xmlErr != nil {
		if failClosedXML {
			return StatusSnapshot{State: windowsTaskStatusState(st)}, fmt.Errorf("scheduler task XML snapshot: %w", xmlErr)
		}
	} else if taskSpec, ok := parseWindowsTaskSpec(xmlBlob); ok {
		if failClosedXML {
			disabled := st.RuntimeState == scheduler.TaskRuntimeDisabled || strings.EqualFold(st.State, "Disabled")
			spec = shimSpecFingerprint("windows", fmt.Sprint(disabled), string(xmlBlob))
		}
		drift = windowsTaskSpecDrifted(taskSpec, opts)
	} else if failClosedXML {
		return StatusSnapshot{State: windowsTaskStatusState(st)}, fmt.Errorf("scheduler task XML snapshot: %w", scheduler.ErrTaskCorrupt)
	}

	if drift {
		return StatusSnapshot{State: StateDrifted, SpecFingerprint: spec}, nil
	}
	return StatusSnapshot{State: windowsTaskStatusState(st), SpecFingerprint: spec}, nil
}

func windowsTaskStatusState(st scheduler.TaskStatus) State {
	if strings.EqualFold(st.State, "Running") {
		return StateEnabledRunning
	}
	return StateEnabledStopped
}

// detectDrift parses the on-disk Task Scheduler XML and returns true
// when the recorded <Command> or <Arguments> disagrees with what
// Enable(opts) would write today.
//
// Drift conditions:
//   - <Command> path differs from resolved opts.MCPHubPath (case-
//     insensitive — Windows paths are case-insensitive at the FS
//     layer).
//   - <Arguments> contains "--strict-mode" while opts.StrictMode is
//     false (we'd remove it).
//   - <Arguments> lacks "--strict-mode" while opts.StrictMode is true
//     (we'd add it).
//
// XML parse failure short-circuits to "no drift" — best-effort, the
// running/stopped fallback takes over.
//
// The decoder is wired with a passthrough CharsetReader because the
// real schtasks /Query /XML output declares `encoding="UTF-16"` in
// its prolog (the actual byte stream we receive from CombinedOutput
// is already converted to a Go string in this process), and Go's
// encoding/xml refuses to decode any non-UTF-8 declaration without
// an explicit reader override.
func detectDrift(xmlBlob []byte, opts Options) bool {
	taskSpec, ok := parseWindowsTaskSpec(xmlBlob)
	if !ok {
		return false
	}
	return windowsTaskSpecDrifted(taskSpec, opts)
}

type windowsTaskSpec struct {
	Command   string
	Arguments string
}

func parseWindowsTaskSpec(xmlBlob []byte) (windowsTaskSpec, bool) {
	type execNode struct {
		Command   string `xml:"Command"`
		Arguments string `xml:"Arguments"`
	}
	type actionsNode struct {
		Exec execNode `xml:"Exec"`
	}
	type taskRoot struct {
		XMLName xml.Name    `xml:"Task"`
		Actions actionsNode `xml:"Actions"`
	}
	dec := xml.NewDecoder(bytes.NewReader(xmlBlob))
	dec.CharsetReader = func(_ string, r io.Reader) (io.Reader, error) {
		// Pass through the bytes verbatim. Go has already decoded
		// the schtasks output into a UTF-8 byte slice; the XML
		// prolog's `encoding="UTF-16"` is leftover Windows
		// metadata, not a description of the bytes in `xmlBlob`.
		return r, nil
	}
	var t taskRoot
	if err := dec.Decode(&t); err != nil {
		// Drift detection unavailable — fall back to non-drift so
		// the running/stopped verdict still surfaces to the caller.
		return windowsTaskSpec{}, false
	}
	return windowsTaskSpec{
		Command:   t.Actions.Exec.Command,
		Arguments: t.Actions.Exec.Arguments,
	}, true
}

func windowsTaskSpecDrifted(spec windowsTaskSpec, opts Options) bool {
	want, err := resolveWindowsOwnedEntrypoint(opts)
	if err != nil {
		return false
	}
	if !strings.EqualFold(strings.TrimSpace(spec.Command), strings.TrimSpace(want)) {
		return true
	}
	// Subcommand drift check: the PR that switched the autostart
	// entry from `mcphub supervise` to `mcphub gui` (PR #212) made
	// the first arg the load-bearing token for which command actually
	// launches at logon. Compare against what superviseArgs(opts) would
	// emit today. Without this check, an operator who installed via
	// pre-PR #212 code (autostart Arguments="supervise") would see
	// `mcphub autostart status` report `enabled-running` instead of
	// `drifted`, masking the need to re-run `mcphub autostart enable`
	// to get the new GUI-owns-supervisor lifecycle. PR #212 r4
	// architecture-review finding 2.
	wantArgs, err := superviseArgs(opts)
	if err != nil {
		return true
	}
	gotArgs := strings.Fields(spec.Arguments)
	if len(gotArgs) != len(wantArgs) {
		return true
	}
	for i := range wantArgs {
		if gotArgs[i] != wantArgs[i] {
			return true
		}
	}
	return false
}
