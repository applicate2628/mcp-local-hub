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
	"os"
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
// The task identity remains unchanged across owner modes; Enable always
// replaces its argv in place so no second task/process family is introduced.
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

// Enable installs (or replaces) the autostart Task Scheduler entry.
//
// Replacement strategy: scheduler.Create rejects "task already exists"
// with an error, so we Delete first (idempotent) and then Create. This
// keeps the operation atomic-from-the-user-perspective even when the
// shim was previously installed with different args.
func (w *windowsBackend) Enable(opts Options) error {
	// Validate the exact owner argv before touching the existing canonical
	// task. Delete-before-create is the scheduler replacement protocol, but an
	// invalid requested mode is a caller error, not a reason to remove a
	// working owner.
	args, err := superviseArgs(opts)
	if err != nil {
		return err
	}
	sched, err := schedulerFactoryFn()
	if err != nil {
		return fmt.Errorf("scheduler factory: %w", err)
	}
	cmd, err := resolveMCPHubPath(opts)
	if err != nil {
		return err
	}
	// Always Delete first — idempotent at the scheduler layer and
	// guarantees Create won't trip on a stale entry.
	if err := sched.Delete(WindowsTaskName); err != nil {
		return fmt.Errorf("delete prior task: %w", err)
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
		return fmt.Errorf("create task: %w", err)
	}
	// Best-effort: remove the legacy v0.4.x watchdog Task Scheduler
	// entry. In v0.5.0 the supervisor owns daemon revival via Job
	// Object + reconcile loop, so the 5-min-cadence watchdog task is
	// a no-op vestige that writes "suspicious-xml" warnings to
	// watchdog.log every cycle (its v0.4.x validator doesn't
	// understand the v0.5.0 supervisor task XML format and rejects it).
	// Deleting it here makes `mcphub autostart enable` the canonical
	// transition point — operators don't have to remember to delete
	// the watchdog task manually after migration.
	//
	// scheduler.Delete is idempotent (returns nil for absent tasks),
	// so this is safe to call unconditionally. Non-Absent errors
	// (e.g. permissions, scheduler-access transient) are surfaced to
	// stderr — best-effort but visible, so an operator running into
	// the failure doesn't silently retain the legacy watchdog task
	// spamming "suspicious-xml" warnings every 5 min.
	const legacyWatchdogTaskName = `\mcp-local-hub-watchdog`
	if err := sched.Delete(legacyWatchdogTaskName); err != nil && !isAbsentErrorMsg(err) {
		fmt.Fprintf(os.Stderr, "autostart: legacy watchdog task cleanup failed: %v (manual: schtasks /Delete /TN %q /F)\n",
			err, legacyWatchdogTaskName)
	}
	return nil
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

// StatusSnapshot reports State plus a spec fingerprint derived from the Task
// Scheduler XML's non-liveness shim fields. The live Status() state is used
// only for running-vs-stopped classification and is deliberately excluded from
// the fingerprint.
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
		// Windows schtasks /Query reports "cannot find the file
		// specified" via exit-non-zero rather than mapping to
		// ErrTaskNotFound (only ExportXML does that mapping). So
		// fall back to the string-match the scheduler package
		// already uses for Delete idempotence — any error whose
		// message contains "cannot find" or "does not exist" is
		// the absent-task signal in disguise.
		if isAbsentErrorMsg(err) {
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
		spec = windowsShimSpecFingerprint(taskSpec)
		drift = windowsTaskSpecDrifted(taskSpec, opts)
	} else if failClosedXML {
		return StatusSnapshot{State: windowsTaskStatusState(st)}, fmt.Errorf("scheduler task XML snapshot unavailable")
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

// isAbsentErrorMsg matches the schtasks "task not found" failure mode
// for callers that didn't go through ExportXML (which maps to the
// typed ErrTaskNotFound sentinel). Keeps Status absent-detection
// resilient to the Status call path which doesn't pre-translate.
func isAbsentErrorMsg(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "cannot find") || strings.Contains(msg, "does not exist")
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
	Enabled   string
}

func parseWindowsTaskSpec(xmlBlob []byte) (windowsTaskSpec, bool) {
	type execNode struct {
		Command   string `xml:"Command"`
		Arguments string `xml:"Arguments"`
	}
	type actionsNode struct {
		Exec execNode `xml:"Exec"`
	}
	type settingsNode struct {
		Enabled string `xml:"Enabled"`
	}
	type taskRoot struct {
		Actions  actionsNode  `xml:"Actions"`
		Settings settingsNode `xml:"Settings"`
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
		Enabled:   t.Settings.Enabled,
	}, true
}

func windowsTaskSpecDrifted(spec windowsTaskSpec, opts Options) bool {
	want, err := resolveMCPHubPath(opts)
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

func windowsShimSpecFingerprint(spec windowsTaskSpec) string {
	enabled := strings.ToLower(strings.TrimSpace(spec.Enabled))
	if enabled == "" {
		enabled = "unknown"
	}
	return shimSpecFingerprint(
		"windows",
		"installed=true",
		"enabled="+enabled,
		"command="+strings.ToLower(strings.TrimSpace(spec.Command)),
		"args="+strings.Join(strings.Fields(spec.Arguments), " "),
	)
}
