// Package migration drives the v0.4.x → v0.5.0 supervisor migration.
//
// This file (journal.go) is the driver body: forward migration, rollback
// migration, and resume classification all live here. The driver writes
// a per-migration journal directory under <state-dir>/migration-journal-<UTC-ts>/
// with marker files (`prepared`, `pre-os-mutating`, `os-mutating-complete`,
// `committed`, `rollback-in-progress`) that allow operator-driven recovery
// across crash, reboot, or operator-issued abort.
//
// Cross-platform: the orchestrator functions compile on POSIX (the journal
// layout is filesystem only, lock acquisition uses the cross-platform flock
// surface, marker writes are os.WriteFile). The OS-mutating steps (schtasks,
// taskkill, PowerShell probe) live behind injected function pointers so
// production wires the Windows implementations and tests inject fakes. POSIX
// callers can drive the orchestrator end-to-end against fakes too, which is
// how the test suite exercises it — POSIX has no v0.4.x to migrate from per
// spec Q9, so no production POSIX path exists, but the function bodies
// compile and the orchestrator returns ErrPosixNotSupported when invoked
// without the Windows dependencies.
//
// Spec: docs/superpowers/specs/2026-05-16-v0.5.0-supervisor-architecture.md
// §"Migration journal (detail)" (line 205-335).
package migration

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"mcp-local-hub/internal/api"
	"mcp-local-hub/internal/scheduler"
)

// ProcessIdentity is the cross-platform shape of the 4-gate ownership
// check input. The real internal/process.ProcessIdentity is Windows-only
// (build-tag `windows`); production callers wire a thin adapter that
// copies fields from process.ProcessIdentity into this shape. POSIX
// callers do not invoke RunForward (Q9), but the type must compile on
// every OS for cross-platform builds.
//
// Field-by-field byte-for-byte parity with process.ProcessIdentity:
//
//	PID              — input PID echoed for caller convenience.
//	Basename         — executable file name (mcphub.exe).
//	CommandLine      — full command-line verbatim.
//	ExecutablePath   — absolute path to executable image.
//	CreationDateUnix — Unix seconds (UTC).
type ProcessIdentity struct {
	PID              int
	Basename         string
	CommandLine      string
	ExecutablePath   string
	CreationDateUnix int64
}

// ---------------------------------------------------------------------------
// Exit codes (consumed by CLI layer).
// ---------------------------------------------------------------------------

const (
	// ExitInstallBusy is returned to the CLI when migration.lock is held
	// by another live install attempt. Spec §"Lock ordering" line 234.
	ExitInstallBusy = 8
	// ExitStrictModeBusy is the strict-mode counterpart (same lock,
	// different command). Reserved here for completeness; mcphub
	// strict-mode owns its own exit-code translation.
	ExitStrictModeBusy = 9
	// ExitRollbackTokenMismatch indicates the rollback caller lacks
	// PROCESS_TERMINATE rights on the running supervisor — typically
	// rollback invoked from a non-elevated shell after supervisor was
	// started under runas /user:Administrator. Spec line 294.
	ExitRollbackTokenMismatch = 13
	// ExitMigrationPowerShellLocked indicates the host has both wmic
	// deprecated AND PowerShell Get-CimInstance blocked; migration's
	// PID-identity check cannot run. Spec line 272 + §"Forward
	// migration steps" step 0.
	ExitMigrationPowerShellLocked = 14
)

// ---------------------------------------------------------------------------
// Sentinel errors (string-tagged so callers can `errors.Is`).
// ---------------------------------------------------------------------------

var (
	// ErrRollbackOrphanDaemonsRemain is returned when rollback's
	// post-force-kill port verification reports daemons still bound to
	// their listening ports after the bounded retry window. Spec line 296.
	ErrRollbackOrphanDaemonsRemain = errors.New("migration: rollback orphan daemons remain")
	// ErrMigrationPortLookupInconsistent is returned when port-PID
	// lookup keeps returning ok=false but a matching mcphub.exe daemon
	// is in the process list. Spec line 258.
	ErrMigrationPortLookupInconsistent = errors.New("migration: port lookup inconsistent")
	// ErrPowerShellLocked maps to ExitMigrationPowerShellLocked when the
	// caller surfaces this to the operator. The constant is a separate
	// sentinel so internal callers can `errors.Is` without reading the
	// exit-code integer.
	ErrPowerShellLocked = errors.New("migration: PowerShell CLM-locked and wmic absent")
	// ErrMigrationHardDeviation is returned when classifier reports
	// HasUnsupportedAbort and --discard-scheduler-customizations was not
	// passed. Spec line 278.
	ErrMigrationHardDeviation = errors.New("migration: hard XML deviation detected")
	// ErrPosixNotSupported is returned by RunForward / RunRollback when
	// invoked on POSIX without injected Windows-only callbacks. Forward
	// migration from v0.4.x has no POSIX path per Q9.
	ErrPosixNotSupported = errors.New("migration: forward/rollback is Windows-only (POSIX has no v0.4.x to migrate from)")
	// ErrProcessNotFound is the cross-platform sentinel the injected
	// LookupProcessIdentity callback returns when the queried PID has
	// no matching system process row (the daemon already exited). The
	// production adapter wraps process.ErrProcessNotFound into this
	// migration-side sentinel so tests can drive the same behavior on
	// POSIX without depending on the Windows-only process package. Spec
	// Lane F P0 #2: this is the ONLY lookup-error class that may be
	// treated as "genuine unbound" after a fresh process-list cross-
	// check confirms no matching daemon argv is still running.
	ErrProcessNotFound = errors.New("migration: PID not found in system process list")
)

// ---------------------------------------------------------------------------
// State + journal layout primitives.
// ---------------------------------------------------------------------------

// State carries the on-disk paths the driver mutates. Construction is
// the caller's responsibility — production wires StateDir to
// %LOCALAPPDATA%\mcp-local-hub\, InstallDir to filepath.Dir(os.Executable()),
// and Now to time.Now. Tests inject t.TempDir() for both paths and a
// fixed clock when timestamp determinism matters.
type State struct {
	// StateDir is the per-user mcphub state directory. Owner of
	// daemon-intent.json, supervisor-intent.json, watchdog-state.json,
	// migration-journal-* directories, migration.lock, --once.lock.
	StateDir string
	// InstallDir is the directory containing the running mcphub binary;
	// used as the 4-gate ownership anchor (ExecutablePath under
	// <install-dir>). Spec line 263.
	InstallDir string
	// Now is the clock function. Defaults to time.Now in production;
	// tests inject a fixed-time function for deterministic journal
	// directory names.
	Now func() time.Time
}

// JournalRoot returns the migration-journal-* directory created by the
// most recent RunForward invocation OR the one chosen by FindLatestJournal
// during resume. The path is <state-dir>/migration-journal-<UTC-timestamp>/.
//
// Production callers should always go through FindLatestJournal or store
// the JournalDir returned by RunForward — this helper is for tests that
// want to construct the path from a fixed clock without round-tripping
// through filesystem enumeration.
func (s State) journalDirForTime(t time.Time) string {
	// Format chosen to sort lexicographically — RFC3339 with `:`
	// replaced by `-` (Windows path rules forbid `:` in filenames
	// outside drive letters).
	stamp := t.UTC().Format("20060102T150405Z")
	// Nanosecond suffix so two consecutive sub-millisecond invocations
	// do not collide. Tests use a fixed clock and rely on this to
	// produce distinct dirs.
	stamp = fmt.Sprintf("%s-%09d", stamp, t.UTC().Nanosecond())
	return filepath.Join(s.StateDir, "migration-journal-"+stamp)
}

// nowOrDefault returns s.Now() or time.Now() when the field is nil.
func (s State) nowOrDefault() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

// ---------------------------------------------------------------------------
// Marker primitives.
// ---------------------------------------------------------------------------

// Journal markers per spec §"Journal layout" line 209-223.
const (
	MarkerPrepared           = "prepared"
	MarkerPreOsMutating      = "pre-os-mutating"
	MarkerOsMutatingComplete = "os-mutating-complete"
	MarkerCommitted          = "committed"
	MarkerRollbackInProgress = "rollback-in-progress"
)

// allForwardMarkers in order — used by ClassifyResume to detect which
// stage the journal reached.
var allForwardMarkers = []string{
	MarkerPrepared,
	MarkerPreOsMutating,
	MarkerOsMutatingComplete,
	MarkerCommitted,
}

// touchMarker creates an empty marker file. Idempotent (existing marker
// is left untouched). Used at every forward-step transition.
//
// Note: this is intentionally a separate write from any JSON payload —
// marker presence/absence is the resume signal, never the content.
func touchMarker(journalDir, name string) error {
	if err := os.MkdirAll(journalDir, 0700); err != nil {
		return fmt.Errorf("touch %s: mkdir: %w", name, err)
	}
	path := filepath.Join(journalDir, name)
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0600)
	if err != nil {
		return fmt.Errorf("touch %s: %w", name, err)
	}
	return f.Close()
}

// markerExists is the boolean form of os.Stat for resume classification.
func markerExists(journalDir, name string) bool {
	_, err := os.Stat(filepath.Join(journalDir, name))
	return err == nil
}

// ---------------------------------------------------------------------------
// Resume classification + journal enumeration.
// ---------------------------------------------------------------------------

// ResumeVerdict is the classification of an existing migration journal
// from the resume path. Action codes are stable strings consumed by the
// CLI layer to render operator guidance.
type ResumeVerdict struct {
	// Action is one of:
	//   "safe-abort-delete-journal"        — prepared only, no mutation occurred.
	//   "operator-choice-forward-or-rollback" — pre-os-mutating or os-mutating-complete present.
	//   "already-committed-no-resume-needed" — committed present.
	//   "rollback-must-complete"           — rollback-in-progress marker present.
	//   "no-journal"                       — JournalDir is empty / not found.
	Action string
	// JournalDir is the absolute path to the classified journal. Empty
	// when Action="no-journal".
	JournalDir string
	// Markers is the list of present markers in canonical order. Useful
	// for diagnostic logging.
	Markers []string
}

// ClassifyResume inspects an existing journal directory and returns the
// resume verdict. Empty journalDir → Action="no-journal".
//
// Spec §"Resume / partial-state recovery" line 327-335.
func ClassifyResume(journalDir string) ResumeVerdict {
	if journalDir == "" {
		return ResumeVerdict{Action: "no-journal"}
	}
	if _, err := os.Stat(journalDir); err != nil {
		return ResumeVerdict{Action: "no-journal"}
	}

	v := ResumeVerdict{JournalDir: journalDir}

	// Rollback marker takes precedence — its presence means a rollback
	// started but did not delete the marker at step 12, regardless of
	// what forward markers might also be present from the original
	// migration.
	if markerExists(journalDir, MarkerRollbackInProgress) {
		v.Markers = append(v.Markers, MarkerRollbackInProgress)
		v.Action = "rollback-must-complete"
		// Also record any forward markers for diagnostic visibility.
		for _, m := range allForwardMarkers {
			if markerExists(journalDir, m) {
				v.Markers = append(v.Markers, m)
			}
		}
		return v
	}

	// Walk forward markers in order; record presence.
	for _, m := range allForwardMarkers {
		if markerExists(journalDir, m) {
			v.Markers = append(v.Markers, m)
		}
	}

	// Classify by the latest marker found.
	switch {
	case markerExists(journalDir, MarkerCommitted):
		v.Action = "already-committed-no-resume-needed"
	case markerExists(journalDir, MarkerOsMutatingComplete):
		v.Action = "operator-choice-forward-or-rollback"
	case markerExists(journalDir, MarkerPreOsMutating):
		v.Action = "operator-choice-forward-or-rollback"
	case markerExists(journalDir, MarkerPrepared):
		v.Action = "safe-abort-delete-journal"
	default:
		// Directory exists but no markers — treat as no-journal so a
		// stray empty dir does not block resume classification.
		v.Action = "no-journal"
		v.JournalDir = ""
	}
	return v
}

// FindLatestJournal returns the most recent migration-journal-* directory
// under stateDir, sorted by name (which encodes UTC timestamp + nanos so
// lexicographic order == chronological order). Empty string returned when
// no journals exist.
//
// Side effect (spec line 227): on entry, any .pruning-* debris from a
// prior crashed prune is finished via os.RemoveAll. This sweep runs
// BEFORE journal enumeration so resume classification never sees a
// half-pruned journal masquerading as a real journal.
func FindLatestJournal(stateDir string) (string, error) {
	// Phase 1: sweep .pruning-* debris.
	if err := sweepPruningDebris(stateDir); err != nil {
		// Sweep failure is non-fatal — log-worthy but should not block
		// resume. Caller surfaces the wrapped error if it cares.
		return "", fmt.Errorf("sweep prune debris: %w", err)
	}

	// Phase 2: enumerate journal directories.
	entries, err := os.ReadDir(stateDir)
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", fmt.Errorf("read state dir: %w", err)
	}
	var names []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasPrefix(name, "migration-journal-") {
			continue
		}
		// Filter out anything that looks like prune debris that the
		// sweep missed (e.g. an os.RemoveAll failure for one entry).
		if strings.HasPrefix(name, ".pruning-") {
			continue
		}
		names = append(names, name)
	}
	if len(names) == 0 {
		return "", nil
	}
	sort.Strings(names) // lex == chronological
	return filepath.Join(stateDir, names[len(names)-1]), nil
}

// sweepPruningDebris finishes any half-completed prune from a prior
// crash. The two-phase prune leaves directories renamed to
// `.pruning-<basename>/` on crash; this helper completes the
// os.RemoveAll. Per-dir failures are logged via the global hook (left
// nil by default; production wires it to the supervisor event log).
func sweepPruningDebris(stateDir string) error {
	entries, err := os.ReadDir(stateDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasPrefix(name, ".pruning-") {
			continue
		}
		// Non-fatal per-dir failure: continue with the rest.
		_ = os.RemoveAll(filepath.Join(stateDir, name))
	}
	return nil
}

// PruneOldJournals retains the 5 newest migration-journal-* dirs under
// stateDir and removes the rest via the two-phase atomic rename pattern.
// Crash-safe: if the process dies between rename and os.RemoveAll, the
// .pruning-* prefix survives the crash and is finished by the next call
// to sweepPruningDebris (typically the next FindLatestJournal).
//
// Per-dir failures (rename fails because another process still holds an
// open handle, RemoveAll partial failure on a non-empty subtree) are
// logged via the prune log hook and treated as non-fatal — the journal
// driver continues. Spec line 227.
func PruneOldJournals(stateDir string) error {
	// Phase 0: clean up debris from a prior crashed prune so the count
	// below is accurate.
	if err := sweepPruningDebris(stateDir); err != nil {
		return fmt.Errorf("sweep prune debris: %w", err)
	}

	entries, err := os.ReadDir(stateDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("read state dir: %w", err)
	}
	var names []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasPrefix(name, "migration-journal-") {
			continue
		}
		if strings.HasPrefix(name, ".pruning-") {
			continue
		}
		names = append(names, name)
	}
	if len(names) <= journalRetention {
		return nil
	}
	sort.Strings(names) // ascending: oldest first
	// Victims = everything except the last journalRetention.
	victims := names[:len(names)-journalRetention]
	for _, v := range victims {
		src := filepath.Join(stateDir, v)
		dst := filepath.Join(stateDir, ".pruning-"+v)
		// Phase 1: rename. Atomic on local FS.
		if err := os.Rename(src, dst); err != nil {
			// Best-effort: a held handle on the source means we cannot
			// rename right now. Skip and let the next prune retry.
			continue
		}
		// Phase 2: blow it away. Per-entry failures inside the subtree
		// are tolerated — the .pruning-* prefix means the next sweep
		// will retry.
		_ = os.RemoveAll(dst)
	}
	return nil
}

// journalRetention is the spec-pinned number of journals to retain
// after a successful migration (spec line 227).
const journalRetention = 5

// ---------------------------------------------------------------------------
// Forward migration.
// ---------------------------------------------------------------------------

// SchedulerBackend is the subset of internal/scheduler the migration
// driver invokes. Production wires a thin adapter around scheduler.New()
// + EnumerateAllMcphubTasks; tests inject a fake recording every call.
//
// The CreateXML method does NOT exist on the production
// (*windowsScheduler) today — for migration's rollback step 8 we need
// `schtasks /Create /TN <task> /XML <path> /F` which IS the existing
// ImportXML(name, []byte) method. The CreateXML / ImportXML naming
// difference is just an interface adapter; production binds CreateXML
// to ImportXML.
type SchedulerBackend interface {
	// EnumerateAllMcphubTasks lists every \mcp-local-hub-* scheduled
	// task regardless of Run As account. Spec line 240.
	EnumerateAllMcphubTasks() ([]scheduler.TaskStatus, error)
	// ExportXML returns the raw Task Scheduler XML for a task.
	// Returns scheduler.ErrTaskNotFound for absent tasks.
	ExportXML(name string) (string, error)
	// Delete removes a task. No-op on absent tasks.
	Delete(name string) error
	// CreateXML re-creates a task from raw XML
	// (schtasks /Create /XML /F). Used by rollback step 8.
	CreateXML(name, xml string) error
	// Run triggers an immediate execution. Used by rollback step 9.
	Run(name string) error
}

// ForwardOptions controls forward-migration semantics. All function
// pointer fields are required when their corresponding step executes;
// tests inject fakes and production wires real backends.
type ForwardOptions struct {
	// DiscardSchedulerCustomizations bypasses the classifier-abort path
	// for HasUnsupportedAbort. Spec line 245.
	DiscardSchedulerCustomizations bool
	// StrictTemplate flips KindUnknownDrift into an abort condition.
	// Spec line 248.
	StrictTemplate bool
	// PreMigrationStrictMode is the operator's strict_mode setting at
	// migration entry. Recorded in the journal so rollback can restore
	// it. Spec line 219.
	PreMigrationStrictMode bool
	// Scheduler is the injected backend. Required.
	Scheduler SchedulerBackend
	// PowerShellProbe returns (ok, err) for the CLM probe at step 0.
	// Defaults to process.ProbePowerShellCLM in production.
	PowerShellProbe func() (bool, error)
	// WmicPresent reports whether wmic.exe is on PATH. Defaults to
	// exec.LookPath("wmic.exe") != nil in production.
	WmicPresent func() bool
	// LookupProcessIdentity is the PID resolver. Production wires
	// process.LookupProcessIdentity (Windows-only). Required when the
	// migration reaches step 9 (pre-unregister daemon stop).
	LookupProcessIdentity func(pid int) (ProcessIdentity, error)
	// PortForPID resolves the listening port for a daemon PID. Backed
	// by netstat -ano with the per-iteration cache. Returns
	// (port, false) when the PID has no listener.
	PortForPID func(pid int) (int, bool)
	// PIDForServerDaemon resolves the in-system PID for a given
	// server/daemon argv pair via the process list. Used in the
	// MIGRATION_PORT_LOOKUP_INCONSISTENT abort guard at spec line 258.
	PIDForServerDaemon func(server, daemon string) (int, bool)
	// KillPID kills a process by PID (taskkill /F /T /PID on Windows).
	KillPID func(pid int) error
	// PortBindWait blocks until the port is unbound or the timeout
	// elapses; returns nil on unbound, error on timeout. Used after
	// each kill at step 9 (up to 10s).
	PortBindWait func(port int, timeout time.Duration) error
	// ShimInstaller installs the supervisor autostart shim. Strict
	// mode is forwarded from PreMigrationStrictMode.
	ShimInstaller func(strictMode bool) error
	// SupervisorSpawner starts the supervisor process. Production
	// wires this to cli.RunInstallUpgrade's startup path.
	SupervisorSpawner func() error
	// ReconcileReady blocks up to timeout for the supervisor's IPC
	// `status` to report reconcile-ready. Returns nil on ready, error
	// on timeout (caller triggers auto-rollback).
	ReconcileReady func(timeout time.Duration) error
	// CurrentUser is the local user account name used by the XML
	// classifier as the user-specific anchor. Required.
	CurrentUser string
	// QuarantineTranslator is invoked just before forward-migration
	// commit to translate any pre-existing supervisor-state quarantine
	// rows into daemon-intent chronic-failure entries. Forward path
	// does NOT use this — it is here for symmetric type wiring; the
	// rollback path is where translation actually fires.
	QuarantineTranslator func(state State) error
	// RollbackOnFailure is the auto-rollback callback for Lane C P0 #2.
	// When forward step 14 (ReconcileReady) times out, the migration
	// must NOT exit with a "consider --rollback-to-legacy" error and
	// leave the host in a half-migrated state (legacy tasks deleted,
	// supervisor not converged). Instead the caller wires this hook to
	// produce a fully-populated RollbackOptions so RunForward can drive
	// RunRollback inline. If RollbackOnFailure is nil or returns nil,
	// RunForward falls back to the manual-rollback error message — the
	// operator must invoke `mcphub rollback-to-legacy` by hand. The
	// auto-rollback fires AFTER the forward migration's locks are
	// released so RunRollback can acquire them itself; the committed
	// marker is never touched.
	RollbackOnFailure func() *RollbackOptions
}

// killedDaemonRecord is one row appended to killed-daemons.json during
// step 9 of forward migration. Used by operator forensics + future
// rollback restoration.
//
// GateSkipped vs GateFailed: GateSkipped is the legacy "log + continue"
// signal (no-running-daemon, genuine-unbound). GateFailed (Lane F P0 #1
// + #2) is the abort signal naming WHICH of the four ownership gates
// rejected the foreign process; the migration returned
// ErrMigrationPortLookupInconsistent and the kill did NOT happen.
type killedDaemonRecord struct {
	Task         string `json:"task"`
	Server       string `json:"server"`
	Daemon       string `json:"daemon"`
	PID          int    `json:"pid"`
	Port         int    `json:"port,omitempty"`
	KilledAtUnix int64  `json:"killed_at_unix"`
	OwnershipOK  bool   `json:"ownership_gate_ok"`
	GateSkipped  string `json:"gate_skipped_reason,omitempty"`
	GateFailed   string `json:"gate_failed,omitempty"`
}

// killedDaemonsFile is the on-disk shape for killed-daemons.json.
type killedDaemonsFile struct {
	Version int                  `json:"version"`
	Killed  []killedDaemonRecord `json:"killed"`
}

// classificationReportFile is the on-disk shape for
// legacy-tasks-classification.json (spec line 217).
type classificationReportFile struct {
	Version int                          `json:"version"`
	Tasks   map[string]DeviationReport   `json:"tasks"`
}

// RunForward executes the v0.4.x → v0.5.0 forward migration. Returns
// nil on success, a wrapped sentinel on classifier-abort or PS-locked
// abort, or an arbitrary I/O error wrapped with the step that produced
// it. The CLI layer maps sentinels to numeric exit codes.
//
// Spec §"Forward migration steps" line 270-290.
func RunForward(state State, opts ForwardOptions) error {
	if state.StateDir == "" {
		return fmt.Errorf("RunForward: empty state-dir")
	}
	if opts.Scheduler == nil {
		return fmt.Errorf("RunForward: nil scheduler backend")
	}
	// Lane C P1 #7: InstallDir is the authoritative anchor for the
	// 4-gate ownership check (Gate 4 — ExecutablePath under InstallDir).
	// If it is empty or unreachable, the gate is silently bypassed and
	// any same-user impostor mcphub.exe daemon would pass. Fail closed
	// BEFORE any OS-mutating step runs.
	if state.InstallDir == "" {
		return fmt.Errorf("RunForward: InstallDir is required for 4-gate ownership check; resolve via os.Executable() + filepath.Dir")
	}
	if _, err := os.Stat(state.InstallDir); err != nil {
		return fmt.Errorf("RunForward: InstallDir is required for 4-gate ownership check; resolve via os.Executable() + filepath.Dir: stat %q: %w", state.InstallDir, err)
	}

	// Step 0: PowerShell CLM probe.
	if err := probePowerShellOrAbort(opts); err != nil {
		return err
	}

	// Step 1-2: lock acquisition (migration.lock → --once.lock).
	ls, err := AcquireMigrationLocks(state.StateDir)
	if err != nil {
		if errors.Is(err, ErrMigrationLockHeld) {
			return &ExitCodeError{Code: ExitInstallBusy, Err: err}
		}
		return err
	}
	defer ls.Release()
	lockAcquiredUnix := state.nowOrDefault().Unix()

	// Create the journal directory. Resume path re-uses an existing
	// journal; for the cold-start path we mint a fresh timestamp.
	journalDir, err := initOrResumeJournalDir(state)
	if err != nil {
		return fmt.Errorf("init journal dir: %w", err)
	}

	// Step 3: enumerate legacy tasks.
	tasks, err := opts.Scheduler.EnumerateAllMcphubTasks()
	if err != nil {
		return fmt.Errorf("step 3 enumerate: %w", err)
	}

	// Step 4: render canonical-template-snapshot. We render once per
	// migration (NOT per task — the snapshot is reference text and the
	// renderer's output is deterministic given (spec, user)). We write
	// the rendering for the FIRST task as the snapshot artifact; the
	// classifier compares each task's observed XML against the
	// pinned-defaults map directly, not against the snapshot file.
	//
	// Lane C P1 #6 (cross-version resume): if the journal already
	// carries a canonical-template-snapshot.xml from an earlier
	// (possibly older-versioned) RunForward attempt, we MUST read it
	// verbatim rather than re-render. The deviation classifier needs
	// the SAME baseline across resume cycles — re-rendering against
	// current V04xTemplateXML would silently flip classifications when
	// the pinned-defaults map changes between versions.
	snapshotPath := filepath.Join(journalDir, "canonical-template-snapshot.xml")
	if _, statErr := os.Stat(snapshotPath); errors.Is(statErr, os.ErrNotExist) {
		if err := writeCanonicalTemplateSnapshot(journalDir, tasks, opts.CurrentUser); err != nil {
			return fmt.Errorf("step 4 snapshot: %w", err)
		}
	}

	// Step 5: export each task's raw XML.
	xmlByTask := make(map[string]string, len(tasks))
	for _, t := range tasks {
		raw, exportErr := opts.Scheduler.ExportXML(t.Name)
		if exportErr != nil {
			return fmt.Errorf("step 5 export %s: %w", t.Name, exportErr)
		}
		xmlByTask[t.Name] = raw
		// Persist under legacy-tasks/<sanitized>.xml. Spec line 216.
		if err := writeLegacyTaskXML(journalDir, t.Name, raw); err != nil {
			return fmt.Errorf("step 5 persist %s: %w", t.Name, err)
		}
	}

	// Step 6: classify deviations. Aggregate per-task reports into
	// legacy-tasks-classification.json. Abort on HasUnsupportedAbort
	// unless DiscardSchedulerCustomizations is set.
	report, hasAbort, hasUnknownDrift := classifyAllTasks(tasks, xmlByTask, opts.CurrentUser)
	if err := writeClassificationReport(journalDir, report); err != nil {
		return fmt.Errorf("step 6 classification report: %w", err)
	}
	if hasAbort && !opts.DiscardSchedulerCustomizations {
		return fmt.Errorf("%w: pass --discard-scheduler-customizations to bypass", ErrMigrationHardDeviation)
	}
	if opts.StrictTemplate && hasUnknownDrift {
		return fmt.Errorf("%w (strict-template): unknown XML drift detected", ErrMigrationHardDeviation)
	}

	// Step 7: derive supervisor-intent.json. Resume optimization: if
	// the journal already carries derived-supervisor-intent.json,
	// reuse it (preserves operator edits between resume attempts).
	intent, err := deriveOrLoadIntent(journalDir, tasks, xmlByTask, opts.PreMigrationStrictMode, state.nowOrDefault())
	if err != nil {
		return fmt.Errorf("step 7 derive intent: %w", err)
	}
	if err := writeDerivedIntent(journalDir, intent); err != nil {
		return fmt.Errorf("step 7 persist intent: %w", err)
	}

	// Persist pre-migration-strict-mode marker per spec line 219.
	if opts.PreMigrationStrictMode {
		if err := touchMarker(journalDir, "pre-migration-strict-mode"); err != nil {
			return fmt.Errorf("step 7 pre-migration-strict-mode marker: %w", err)
		}
	}

	// Step 8: prepared marker.
	if err := touchMarker(journalDir, MarkerPrepared); err != nil {
		return fmt.Errorf("step 8 prepared marker: %w", err)
	}

	// ---------------------------------------------------------------
	// OS-mutating phase begins. Any failure past this point is
	// recoverable only via resume (operator-choice forward-or-rollback).
	// ---------------------------------------------------------------

	// Step 9: pre-unregister daemon stop. Per-task: parse argv → resolve
	// PID → 4-gate ownership check → kill → 10s port-release wait.
	//
	// Lane C P0 #1: the pre-os-mutating marker is written INSIDE the
	// per-task loop immediately after the first successful KillPID —
	// NOT after the whole function completes. Otherwise a later
	// port-bind-wait failure on the same task or a kill failure on a
	// subsequent task would leave host state mutated (a daemon dead)
	// while the journal still classifies as `prepared`-only and resume
	// would safe-abort.
	//
	// Lane F P0 #1/#2: gate-4 failure and non-ErrProcessNotFound
	// lookup failures abort with ErrMigrationPortLookupInconsistent
	// rather than log+skip. The partially-built audit row slice is
	// persisted regardless of return so the operator can diagnose.
	killed, stepErr := preUnregisterDaemonStop(journalDir, tasks, xmlByTask, opts, state, lockAcquiredUnix)
	if writeErr := writeKilledDaemons(journalDir, killed); writeErr != nil {
		if stepErr != nil {
			return fmt.Errorf("step 9 pre-unregister: %w (also failed to persist killed-daemons.json: %v)", stepErr, writeErr)
		}
		return fmt.Errorf("step 9 killed-daemons.json: %w", writeErr)
	}
	if stepErr != nil {
		return fmt.Errorf("step 9 pre-unregister: %w", stepErr)
	}

	// Step 10: schtasks /Delete each legacy task.
	for _, t := range tasks {
		if err := opts.Scheduler.Delete(t.Name); err != nil {
			return fmt.Errorf("step 10 delete %s: %w", t.Name, err)
		}
	}

	// Step 11: os-mutating-complete marker.
	if err := touchMarker(journalDir, MarkerOsMutatingComplete); err != nil {
		return fmt.Errorf("step 11 os-mutating-complete: %w", err)
	}

	// Step 12: install supervisor autostart shim.
	if opts.ShimInstaller != nil {
		if err := opts.ShimInstaller(opts.PreMigrationStrictMode); err != nil {
			return fmt.Errorf("step 12 shim install: %w", err)
		}
	}

	// Step 13: explicitly start supervisor.
	if opts.SupervisorSpawner != nil {
		if err := opts.SupervisorSpawner(); err != nil {
			return fmt.Errorf("step 13 spawn supervisor: %w", err)
		}
	}

	// Step 14: wait reconcile-ready within 30s.
	//
	// Lane C P0 #2: on timeout the migration MUST drive auto-rollback
	// when opts.RollbackOnFailure is wired — leaving the host in a
	// half-migrated state (legacy tasks deleted, supervisor not
	// converged) is a hard regression. The auto-rollback path:
	//
	//   1. Build the RollbackOptions via the caller-supplied factory.
	//   2. Release this RunForward call's locks so RunRollback can
	//      acquire migration.lock + --once.lock itself (deferred
	//      ls.Release stays in place — it is idempotent).
	//   3. Invoke RunRollback. Bubble its result back to the caller
	//      regardless of outcome.
	//   4. The committed marker is never touched. Operators inspect
	//      the journal to see what happened.
	//
	// If RollbackOnFailure is nil OR returns nil, fall back to the
	// historical manual-rollback error message so existing callers
	// keep working unchanged.
	if opts.ReconcileReady != nil {
		if rcErr := opts.ReconcileReady(30 * time.Second); rcErr != nil {
			if opts.RollbackOnFailure != nil {
				rbOpts := opts.RollbackOnFailure()
				if rbOpts != nil {
					// Release the forward locks BEFORE invoking
					// RunRollback so the rollback path can acquire
					// them. ls.Release is idempotent so the deferred
					// Release at the top of RunForward is a no-op.
					ls.Release()
					if rbErr := RunRollback(state, *rbOpts); rbErr != nil {
						return fmt.Errorf("step 14 reconcile-ready timed out and auto-rollback failed: reconcile=%v rollback=%w", rcErr, rbErr)
					}
					return fmt.Errorf("step 14 reconcile-ready: %w (auto-rollback completed)", rcErr)
				}
			}
			return fmt.Errorf("step 14 reconcile-ready: %w (consider --rollback-to-legacy)", rcErr)
		}
	}

	// Step 15: committed marker.
	if err := touchMarker(journalDir, MarkerCommitted); err != nil {
		return fmt.Errorf("step 15 committed marker: %w", err)
	}

	// Step 16: prune old journals (5 newest retained).
	if err := PruneOldJournals(state.StateDir); err != nil {
		// Spec line 227: per-dir delete failures are non-fatal. Wrap
		// and return so the caller can log, but do not fail the
		// migration — committed marker is already on disk.
		_ = err // intentional swallow per spec; production wires a logger.
	}

	// Step 17-18: LIFO release via deferred ls.Release().
	return nil
}

// probePowerShellOrAbort runs the step-0 PS CLM probe + wmic-present
// fallback and returns ErrPowerShellLocked (wrapped in ExitCodeError)
// when both are unavailable.
func probePowerShellOrAbort(opts ForwardOptions) error {
	psOK := true
	if opts.PowerShellProbe != nil {
		ok, err := opts.PowerShellProbe()
		if err != nil {
			return fmt.Errorf("step 0 PS probe: %w", err)
		}
		psOK = ok
	}
	if psOK {
		return nil
	}
	wmicOK := false
	if opts.WmicPresent != nil {
		wmicOK = opts.WmicPresent()
	}
	if wmicOK {
		return nil
	}
	return &ExitCodeError{Code: ExitMigrationPowerShellLocked, Err: ErrPowerShellLocked}
}

// ExitCodeError pairs a sentinel error with a numeric exit code so the
// CLI layer can `errors.As` + `os.Exit` without re-mapping every error
// kind. Used for INSTALL_BUSY, ROLLBACK_TOKEN_MISMATCH,
// MIGRATION_POWERSHELL_LOCKED.
type ExitCodeError struct {
	Code int
	Err  error
}

func (e *ExitCodeError) Error() string {
	if e.Err == nil {
		return fmt.Sprintf("exit code %d", e.Code)
	}
	return e.Err.Error()
}

// Unwrap so callers can errors.Is on the underlying sentinel.
func (e *ExitCodeError) Unwrap() error { return e.Err }

// initOrResumeJournalDir returns the journal dir for this migration. If
// a prior journal has been classified as "operator-choice-forward-or-rollback"
// and the operator chose forward-resume, FindLatestJournal would have
// surfaced it; for the cold-start path we mint a new timestamped dir.
//
// Implementation: if the latest journal exists and is NOT committed,
// reuse it (resume); otherwise mint a fresh one.
func initOrResumeJournalDir(state State) (string, error) {
	latest, err := FindLatestJournal(state.StateDir)
	if err != nil {
		return "", err
	}
	if latest != "" {
		verdict := ClassifyResume(latest)
		switch verdict.Action {
		case "operator-choice-forward-or-rollback":
			// Resume into the existing journal.
			return latest, nil
		case "safe-abort-delete-journal":
			// prepared-only — could be a stale dry-run; reuse it
			// rather than mint a new one (the prepared marker is
			// idempotent, classification artifacts are regenerated).
			return latest, nil
		}
	}
	dir := state.journalDirForTime(state.nowOrDefault())
	if err := os.MkdirAll(dir, 0700); err != nil {
		return "", err
	}
	return dir, nil
}

// writeCanonicalTemplateSnapshot writes a representative pinned-renderer
// XML to canonical-template-snapshot.xml. The classifier reads observed
// XML and pinned defaults separately; the snapshot is operator-facing
// reference text — useful when diffing what the migration THOUGHT the
// pinned baseline looked like vs what got installed.
//
// When tasks is empty we still write a minimal snapshot (empty TaskSpec
// rendering) so the file is always present in a committed journal.
func writeCanonicalTemplateSnapshot(journalDir string, tasks []scheduler.TaskStatus, currentUser string) error {
	var rendered strings.Builder
	if len(tasks) == 0 {
		rendered.WriteString(V04xTemplateXML(scheduler.TaskSpec{}, currentUser))
	} else {
		for _, t := range tasks {
			rendered.WriteString("<!-- task: ")
			rendered.WriteString(t.Name)
			rendered.WriteString(" -->\n")
			rendered.WriteString(V04xTemplateXML(scheduler.TaskSpec{
				Name:         t.Name,
				LogonTrigger: true,
			}, currentUser))
			rendered.WriteString("\n")
		}
	}
	return os.WriteFile(filepath.Join(journalDir, "canonical-template-snapshot.xml"), []byte(rendered.String()), 0600)
}

// writeLegacyTaskXML persists one task's exported XML under
// legacy-tasks/<sanitized>.xml. The sanitization strips the leading
// backslash and replaces remaining path separators with underscores so
// the filename round-trips across Windows and POSIX rendering.
func writeLegacyTaskXML(journalDir, taskName, raw string) error {
	subDir := filepath.Join(journalDir, "legacy-tasks")
	if err := os.MkdirAll(subDir, 0700); err != nil {
		return err
	}
	sanitized := sanitizeTaskFilename(taskName)
	return os.WriteFile(filepath.Join(subDir, sanitized+".xml"), []byte(raw), 0600)
}

func sanitizeTaskFilename(taskName string) string {
	name := strings.TrimPrefix(taskName, "\\")
	name = strings.ReplaceAll(name, "\\", "_")
	name = strings.ReplaceAll(name, "/", "_")
	return name
}

// classifyAllTasks invokes the same-package classifier on each task and
// aggregates the result into a single report ready to persist.
func classifyAllTasks(tasks []scheduler.TaskStatus, xmlByTask map[string]string, currentUser string) (classificationReportFile, bool, bool) {
	report := classificationReportFile{
		Version: 1,
		Tasks:   make(map[string]DeviationReport, len(tasks)),
	}
	hasAbort := false
	hasUnknownDrift := false
	for _, t := range tasks {
		raw, ok := xmlByTask[t.Name]
		if !ok {
			continue
		}
		// We pass a minimal spec — the classifier compares against pinned
		// defaults, not against this spec's values. LogonTrigger=true
		// matches the v0.4.x daemon-task install lineage.
		r := ClassifyXMLDeviations(raw, scheduler.TaskSpec{
			Name:         t.Name,
			Command:      "", // classifier checks command via isMcphubExe
			LogonTrigger: true,
		}, currentUser)
		report.Tasks[t.Name] = r
		if r.HasUnsupportedAbort {
			hasAbort = true
		}
		for _, d := range r.Deviations {
			if d.Kind == KindUnknownDrift {
				hasUnknownDrift = true
			}
		}
	}
	return report, hasAbort, hasUnknownDrift
}

// writeClassificationReport persists legacy-tasks-classification.json.
func writeClassificationReport(journalDir string, report classificationReportFile) error {
	raw, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(journalDir, "legacy-tasks-classification.json"), raw, 0600)
}

// deriveOrLoadIntent derives supervisor-intent.json from the observed
// tasks + classifications. If derived-supervisor-intent.json already
// exists in the journal (resume case), it is loaded verbatim.
func deriveOrLoadIntent(journalDir string, tasks []scheduler.TaskStatus, xmlByTask map[string]string, strictMode bool, now time.Time) (*api.SupervisorIntentFile, error) {
	path := filepath.Join(journalDir, "derived-supervisor-intent.json")
	if existing, err := api.ReadSupervisorIntent(path); err == nil {
		return existing, nil
	}

	// Fresh derive. Walk each task's <Exec> and synthesize one
	// SupervisorDaemon entry per. We extract Command + Arguments from
	// the observed XML — the rendering of `daemon --server X --daemon Y`
	// is the load-bearing argv.
	intent := &api.SupervisorIntentFile{
		Version:    1,
		UpdatedAt:  now.UTC().Format(time.RFC3339Nano),
		StrictMode: strictMode,
		Daemons:    nil,
	}
	for _, t := range tasks {
		raw := xmlByTask[t.Name]
		command, args := extractExecFromXML(raw)
		server, daemon := parseDaemonArgv(args)
		intent.Daemons = append(intent.Daemons, api.SupervisorDaemon{
			TaskName: t.Name,
			Server:   server,
			Daemon:   daemon,
			Command:  command,
			Args:     args,
		})
	}
	return intent, nil
}

// writeDerivedIntent persists the derived intent in the journal AND in
// the production state-dir as supervisor-intent.json (so reconcile picks
// it up on supervisor start).
func writeDerivedIntent(journalDir string, intent *api.SupervisorIntentFile) error {
	if err := api.WriteStateFileAtomic(filepath.Join(journalDir, "derived-supervisor-intent.json"), intent); err != nil {
		return err
	}
	// production location: ..parent..stateDir/supervisor-intent.json
	prodDir := filepath.Dir(journalDir)
	return api.WriteStateFileAtomic(filepath.Join(prodDir, "supervisor-intent.json"), intent)
}

// extractExecFromXML pulls Command + Arguments out of the observed
// <Exec> block via a permissive substring scan. The XML schema is
// validated by the classifier; here we only need the values.
func extractExecFromXML(raw string) (command string, args []string) {
	cmd := extractTag(raw, "Command")
	argStr := extractTag(raw, "Arguments")
	command = strings.TrimSpace(cmd)
	if argStr == "" {
		return command, nil
	}
	for _, p := range strings.Fields(strings.TrimSpace(argStr)) {
		args = append(args, p)
	}
	return command, args
}

func extractTag(body, tag string) string {
	open := "<" + tag + ">"
	close := "</" + tag + ">"
	i := strings.Index(body, open)
	if i < 0 {
		return ""
	}
	j := strings.Index(body[i+len(open):], close)
	if j < 0 {
		return ""
	}
	return body[i+len(open) : i+len(open)+j]
}

// parseDaemonArgv extracts the `--server <S>` and `--daemon <D>` values
// from the argv slice. Returns ("", "") when either flag is missing.
func parseDaemonArgv(args []string) (server, daemon string) {
	for i := 0; i+1 < len(args); i++ {
		switch args[i] {
		case "--server":
			server = args[i+1]
		case "--daemon":
			daemon = args[i+1]
		}
	}
	return server, daemon
}

// preUnregisterDaemonStop implements forward step 9. Returns the list
// of kill records (always — partial list on error too, so the caller
// can still persist killed-daemons.json for operator diagnosis) and an
// error for any abort condition.
//
// Marker timing (Lane C P0 #1): the pre-os-mutating marker is written
// IMMEDIATELY after the first successful KillPID returns nil. A later
// port-bind-wait timeout (or kill error on a subsequent task) leaves
// the marker on disk so resume classifies the journal as
// operator-choice-forward-or-rollback (host state already mutated)
// rather than safe-abort.
//
// Abort policy (Lane F P0 #1 + #2):
//
//   - Gate-4 failure (and any other 4-gate ownership rejection on a
//     port-bound PID) aborts with ErrMigrationPortLookupInconsistent.
//     The legacy task would otherwise be deleted while a foreign
//     process keeps the port → supervisor restart collision.
//   - LookupProcessIdentity returning ErrProcessNotFound triggers a
//     fresh PIDForServerDaemon cross-check. If still positive → abort.
//     If no match → genuine unbound, proceed with audit row.
//   - LookupProcessIdentity returning any OTHER error (retry
//     exhaustion, transport hang, malformed JSON) aborts immediately
//     with ErrMigrationPortLookupInconsistent — never silently
//     classify as unbound.
func preUnregisterDaemonStop(journalDir string, tasks []scheduler.TaskStatus, xmlByTask map[string]string, opts ForwardOptions, state State, lockAcquiredUnix int64) ([]killedDaemonRecord, error) {
	var killed []killedDaemonRecord
	// markerWritten tracks whether we already touched pre-os-mutating
	// in THIS invocation. touchMarker uses O_TRUNC|O_CREATE so it is
	// idempotent on resume.
	markerWritten := false
	for _, t := range tasks {
		raw := xmlByTask[t.Name]
		_, args := extractExecFromXML(raw)
		server, daemon := parseDaemonArgv(args)

		// We don't always have an injected port-lookup in tests; if
		// any of the required hooks is nil, skip this step (treat as
		// "nothing to kill"). Production wires all three.
		if opts.LookupProcessIdentity == nil || opts.PortForPID == nil || opts.KillPID == nil {
			continue
		}

		pid, port, lookupOK, err := resolvePIDPortForTask(server, daemon, opts)
		if err != nil {
			return killed, err
		}
		if !lookupOK {
			// No daemon currently running for this task — nothing to
			// kill. Record an audit row anyway for forensics.
			killed = append(killed, killedDaemonRecord{
				Task:        t.Name,
				Server:      server,
				Daemon:      daemon,
				PID:         0,
				Port:        port,
				OwnershipOK: false,
				GateSkipped: "no-running-daemon",
			})
			continue
		}

		// 4-gate ownership check (spec line 259-263).
		ident, idErr := opts.LookupProcessIdentity(pid)
		if idErr != nil {
			// Lane F P0 #2: separate genuine-unbound from a real
			// transient failure that would otherwise hide a surviving
			// daemon.
			if errors.Is(idErr, ErrProcessNotFound) {
				// Cross-check: re-scan the system process list for
				// the matching daemon argv. If still present, the
				// PID was recycled or the daemon respawned — abort.
				if opts.PIDForServerDaemon != nil {
					if _, stillRunning := opts.PIDForServerDaemon(server, daemon); stillRunning {
						killed = append(killed, killedDaemonRecord{
							Task:       t.Name,
							Server:     server,
							Daemon:     daemon,
							PID:        pid,
							Port:       port,
							GateFailed: "process-not-found-but-cross-check-positive",
						})
						return killed, fmt.Errorf("%w: PID %d server=%s daemon=%s (cross-check still finds matching daemon argv)",
							ErrMigrationPortLookupInconsistent, pid, server, daemon)
					}
				}
				// Cross-check silent → genuine unbound. Record + skip.
				killed = append(killed, killedDaemonRecord{
					Task:        t.Name,
					Server:      server,
					Daemon:      daemon,
					PID:         pid,
					Port:        port,
					OwnershipOK: false,
					GateSkipped: "process-not-found-cross-check-silent",
				})
				continue
			}
			// Non-ErrProcessNotFound failure (retry exhaustion,
			// transport hang, malformed JSON): we cannot prove the
			// daemon is gone. Abort rather than silently delete the
			// task and let a survivor steal the port.
			killed = append(killed, killedDaemonRecord{
				Task:       t.Name,
				Server:     server,
				Daemon:     daemon,
				PID:        pid,
				Port:       port,
				GateFailed: "identity-lookup-failed: " + idErr.Error(),
			})
			return killed, fmt.Errorf("%w: PID %d server=%s daemon=%s: identity lookup failed: %v",
				ErrMigrationPortLookupInconsistent, pid, server, daemon, idErr)
		}
		gateOK, gateReason := fourGateOwnershipCheck(ident, server, daemon, lockAcquiredUnix, state.InstallDir)
		if !gateOK {
			// Lane F P0 #1: a port-bound PID whose 4-gate ownership
			// check fails must abort, NOT skip. Deleting the legacy
			// task while a foreign process keeps the port produces a
			// supervisor restart collision.
			killed = append(killed, killedDaemonRecord{
				Task:       t.Name,
				Server:     server,
				Daemon:     daemon,
				PID:        pid,
				Port:       port,
				GateFailed: gateReason,
			})
			return killed, fmt.Errorf("%w: PID %d server=%s daemon=%s: gate failed: %s",
				ErrMigrationPortLookupInconsistent, pid, server, daemon, gateReason)
		}

		// Gate passed — kill the PID.
		killTs := state.nowOrDefault().Unix()
		if err := opts.KillPID(pid); err != nil {
			return killed, fmt.Errorf("kill PID %d (task %s): %w", pid, t.Name, err)
		}
		// Lane C P0 #1: touch pre-os-mutating IMMEDIATELY after the
		// first successful kill — before any port-bind-wait or
		// subsequent loop iteration that might fail. A later failure
		// MUST leave this marker on disk so resume classifies the
		// journal as operator-choice-forward-or-rollback.
		if !markerWritten {
			if err := touchMarker(journalDir, MarkerPreOsMutating); err != nil {
				// Mid-flight marker write failure is fatal — without
				// the marker on disk a crash here would safe-abort
				// and leave a killed daemon. Append the audit row
				// first so the operator sees the kill happened.
				killed = append(killed, killedDaemonRecord{
					Task:         t.Name,
					Server:       server,
					Daemon:       daemon,
					PID:          pid,
					Port:         port,
					KilledAtUnix: killTs,
					OwnershipOK:  true,
				})
				return killed, fmt.Errorf("pre-os-mutating marker write failed after first kill: %w", err)
			}
			markerWritten = true
		}
		killed = append(killed, killedDaemonRecord{
			Task:         t.Name,
			Server:       server,
			Daemon:       daemon,
			PID:          pid,
			Port:         port,
			KilledAtUnix: killTs,
			OwnershipOK:  true,
		})

		// Up to 10s port-release wait.
		if opts.PortBindWait != nil && port > 0 {
			if err := opts.PortBindWait(port, 10*time.Second); err != nil {
				return killed, fmt.Errorf("port %d release wait: %w", port, err)
			}
		}
	}
	return killed, nil
}

// resolvePIDPortForTask returns (pid, port, ok, err). When lookup is
// "ok=false" AND a matching mcphub.exe daemon IS in the process list,
// returns ErrMigrationPortLookupInconsistent per spec line 258.
func resolvePIDPortForTask(server, daemon string, opts ForwardOptions) (int, int, bool, error) {
	// We don't have a port-by-task resolver as a separate injection;
	// callers wire PortForPID + PIDForServerDaemon (the latter so the
	// process-list-cross-check below works without a port).
	if opts.PIDForServerDaemon == nil {
		return 0, 0, false, nil
	}
	pid, ok := opts.PIDForServerDaemon(server, daemon)
	if !ok {
		// No running daemon → safe to declare unbound.
		return 0, 0, false, nil
	}
	port, portOK := 0, false
	if opts.PortForPID != nil {
		port, portOK = opts.PortForPID(pid)
	}
	if !portOK {
		// PID exists but no listener. Spec line 258: this is the
		// inconsistent case — abort.
		return pid, 0, true, fmt.Errorf("%w: PID %d server=%s daemon=%s",
			ErrMigrationPortLookupInconsistent, pid, server, daemon)
	}
	return pid, port, true, nil
}

// fourGateOwnershipCheck verifies the four independent signals that
// authenticate a PID as belonging to the in-flight migration's mcphub
// install. Returns (true, "") on pass, (false, reason) on fail.
//
// Spec line 259-263.
func fourGateOwnershipCheck(ident ProcessIdentity, server, daemon string, lockAcquiredUnix int64, installDir string) (bool, string) {
	// Gate 1: image basename = mcphub.exe (case-insensitive).
	wantBase := "mcphub.exe"
	if strings.EqualFold(strings.TrimSpace(ident.Basename), wantBase) == false &&
		strings.EqualFold(strings.TrimSpace(ident.Basename), "mcphub") == false {
		return false, fmt.Sprintf("basename %q != mcphub.exe", ident.Basename)
	}

	// Gate 2: CommandLine contains daemon --server <S> --daemon <D>.
	wantArgv := fmt.Sprintf("daemon --server %s --daemon %s", server, daemon)
	if !strings.Contains(ident.CommandLine, wantArgv) {
		return false, fmt.Sprintf("argv %q does not contain %q", ident.CommandLine, wantArgv)
	}

	// Gate 3: createdUnix precedes migration.lock acquisition.
	if ident.CreationDateUnix == 0 {
		return false, "createdUnix is zero"
	}
	if ident.CreationDateUnix >= lockAcquiredUnix {
		return false, fmt.Sprintf("createdUnix %d not before lockAcquiredUnix %d",
			ident.CreationDateUnix, lockAcquiredUnix)
	}

	// Gate 4: ExecutablePath under <install-dir>.
	if installDir != "" {
		absInstall, _ := filepath.Abs(installDir)
		absExe, _ := filepath.Abs(ident.ExecutablePath)
		// Case-insensitive compare on Windows; on POSIX strings.EqualFold
		// is harmless because POSIX paths are case-sensitive anyway.
		if !pathHasPrefix(absExe, absInstall) {
			return false, fmt.Sprintf("ExecutablePath %q not under InstallDir %q",
				ident.ExecutablePath, installDir)
		}
	}

	return true, ""
}

// pathHasPrefix returns true when path is the prefix dir or any nested
// child. Both arguments must be absolute (filepath.Abs at the caller).
// Case-insensitive comparison so Windows hosts treat C:\App and c:\app
// as the same prefix.
func pathHasPrefix(path, prefix string) bool {
	cleanPath := filepath.Clean(path)
	cleanPrefix := filepath.Clean(prefix)
	if strings.EqualFold(cleanPath, cleanPrefix) {
		return true
	}
	// Ensure prefix ends with a separator so /foo doesn't accidentally
	// match /foobar.
	if !strings.HasSuffix(cleanPrefix, string(filepath.Separator)) {
		cleanPrefix += string(filepath.Separator)
	}
	// Case-insensitive prefix compare.
	if len(cleanPath) < len(cleanPrefix) {
		return false
	}
	return strings.EqualFold(cleanPath[:len(cleanPrefix)], cleanPrefix)
}

// writeKilledDaemons persists killed-daemons.json in the journal.
func writeKilledDaemons(journalDir string, killed []killedDaemonRecord) error {
	payload := killedDaemonsFile{Version: 1, Killed: killed}
	raw, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(journalDir, "killed-daemons.json"), raw, 0600)
}

// ---------------------------------------------------------------------------
// Rollback migration.
// ---------------------------------------------------------------------------

// RollbackOptions controls rollback semantics. As with ForwardOptions,
// every callback is injected; production wires real backends and tests
// inject fakes.
type RollbackOptions struct {
	// Scheduler is the injected backend. Required.
	Scheduler SchedulerBackend
	// SupervisorIPC sends an IPC frame to the running supervisor.
	// Production wires this to the named-pipe client; tests inject a
	// recorder. The Windows-only token-mismatch failure surfaces as
	// an error wrapping ERROR_ACCESS_DENIED (or the POSIX EPERM
	// equivalent) — see ProbeSupervisorTokenMismatch below.
	SupervisorIPC func(cmd string, args map[string]any, timeout time.Duration) error
	// ProbeSupervisorTokenMismatch performs the OpenProcess token
	// pre-flight at rollback step 1. Returns nil on access OK, a
	// non-nil error on access denied (mapped to
	// ExitRollbackTokenMismatch). When nil this step is skipped.
	ProbeSupervisorTokenMismatch func() error
	// ForceKillSupervisor is invoked when IPC exit{graceful}
	// times out. taskkill /F /T /PID on Windows; kill -KILL -<pgid>
	// on POSIX.
	ForceKillSupervisor func() error
	// PortBindWait blocks until the port is UNBOUND or timeout
	// elapses; used for the post-force-kill verification at step 3
	// (we want to prove the old supervisor and its daemon children
	// released their listening sockets before we re-register the
	// legacy scheduler tasks).
	PortBindWait func(port int, timeout time.Duration) error
	// PortBindWaitBound blocks until the port is BOUND (a daemon is
	// answering on 127.0.0.1:<port>) or timeout elapses. Used for
	// step 10 — after `schtasks /Run`, we wait for each restored
	// legacy daemon to come back up. Semantic INVERSE of PortBindWait
	// (which waits until UNBOUND); the inversion was a Lane-C/F P1
	// fix because production previously wired both step 3 and step 10
	// to the wait-until-unbound helper, which would have returned
	// "success" at step 10 the instant the post-kill window closed —
	// i.e. BEFORE any restored daemon could possibly have bound. When
	// nil this step is skipped and no warnings are recorded.
	PortBindWaitBound func(port int, timeout time.Duration) error
	// LookupProcessIdentity is the same hook as forward path's
	// counterpart. Used to verify ports unbound post-kill.
	LookupProcessIdentity func(pid int) (ProcessIdentity, error)
	// QuarantineTranslator translates supervisor-state.quarantined
	// rows into daemon-intent.json chronic-failure entries. Spec
	// line 299.
	QuarantineTranslator func(state State) error
	// ShimUninstaller removes the autostart shim. Idempotent —
	// missing shim is silent success per spec line 300.
	ShimUninstaller func() error
	// TimeWaitSettle is the inter-step pause between port-unbound
	// confirmation and schtasks /Run (spec line 301). Default 3s
	// when zero.
	TimeWaitSettle time.Duration
	// PortBindTimeout is the per-task wait at step 10 (default 60s).
	PortBindTimeout time.Duration
	// ExpectedDaemons is the list of supervisor-intent daemon
	// descriptors. Step 3's port-unbound verification iterates this
	// list. Production reads supervisor-intent.json before invoking
	// rollback; tests pass a synthetic list.
	ExpectedDaemons []api.SupervisorDaemon
}

// rollbackWarning is one entry in rollback-warnings.json per spec
// line 309-323.
type rollbackWarning struct {
	Task       string `json:"task"`
	Port       int    `json:"port,omitempty"`
	Reason     string `json:"reason"`
	ObservedAt string `json:"observed_at"`
}

type rollbackWarningsFile struct {
	Version  int               `json:"version"`
	Warnings []rollbackWarning `json:"warnings"`
}

// RunRollback executes the v0.5.0 → v0.4.x rollback path. Always returns
// nil when "rollback itself succeeded" — warnings are persisted to
// rollback-warnings.json and the caller surfaces them to the operator.
// Non-nil error only when rollback could not complete.
//
// Spec §"Rollback steps" line 292-307.
func RunRollback(state State, opts RollbackOptions) error {
	if state.StateDir == "" {
		return fmt.Errorf("RunRollback: empty state-dir")
	}
	if opts.Scheduler == nil {
		return fmt.Errorf("RunRollback: nil scheduler backend")
	}

	settle := opts.TimeWaitSettle
	if settle == 0 {
		settle = 3 * time.Second
	}
	portTimeout := opts.PortBindTimeout
	if portTimeout == 0 {
		portTimeout = 60 * time.Second
	}

	// Step 1: acquire migration.lock + token-mismatch pre-flight + touch
	// rollback-in-progress.
	ls, err := AcquireMigrationLockOnly(state.StateDir)
	if err != nil {
		if errors.Is(err, ErrMigrationLockHeld) {
			return &ExitCodeError{Code: ExitInstallBusy, Err: err}
		}
		return err
	}
	// Defer release; both locks unwind LIFO via ls.Release.
	defer ls.Release()

	if opts.ProbeSupervisorTokenMismatch != nil {
		if err := opts.ProbeSupervisorTokenMismatch(); err != nil {
			return &ExitCodeError{Code: ExitRollbackTokenMismatch, Err: fmt.Errorf("token mismatch: %w", err)}
		}
	}

	// Find the latest journal to rollback against. We need it for the
	// rollback-in-progress marker (lives inside the journal) AND for
	// the legacy-tasks-xml replay.
	journalDir, err := FindLatestJournal(state.StateDir)
	if err != nil {
		return fmt.Errorf("rollback find journal: %w", err)
	}
	if journalDir == "" {
		return fmt.Errorf("rollback: no migration journal found")
	}

	// Atomicity: if marker write fails, release lock + exit non-zero.
	if err := touchMarker(journalDir, MarkerRollbackInProgress); err != nil {
		return fmt.Errorf("step 1 rollback-in-progress marker: %w", err)
	}

	// Step 2: IPC quiesce-timers.
	if opts.SupervisorIPC != nil {
		_ = opts.SupervisorIPC("quiesce-timers", map[string]any{"timeout_ms": 30000}, 35*time.Second)
		// Spec note: this command returns accepted=true immediately;
		// the supervisor drains transients on a separate goroutine.
		// We don't block here — the next IPC call is exit{graceful}.
	}

	// Step 3: IPC exit{graceful} → on timeout force-kill.
	exitErr := error(nil)
	if opts.SupervisorIPC != nil {
		exitErr = opts.SupervisorIPC("exit", map[string]any{"graceful": true, "timeout_ms": 5000}, 6*time.Second)
	}
	if exitErr != nil {
		if opts.ForceKillSupervisor != nil {
			if err := opts.ForceKillSupervisor(); err != nil {
				return fmt.Errorf("step 3 force-kill: %w", err)
			}
		}
	}
	// Verify ports unbound (10s budget total).
	if opts.PortBindWait != nil {
		for _, d := range opts.ExpectedDaemons {
			if d.Port <= 0 {
				continue
			}
			if err := opts.PortBindWait(d.Port, 10*time.Second); err != nil {
				return fmt.Errorf("%w: port %d still bound: %v",
					ErrRollbackOrphanDaemonsRemain, d.Port, err)
			}
		}
	}

	// Step 4: acquire --once.lock.
	if err := ls.AcquireOnceLockOnto(state.StateDir); err != nil {
		return fmt.Errorf("step 4 --once.lock: %w", err)
	}

	// Step 5: find latest journal with `committed` (or pre-os-mutating
	// for partial rollback). We already have journalDir from step 1's
	// rollback-in-progress write; verify it carries one of those
	// markers — otherwise rollback has nothing to restore.
	verdict := ClassifyResume(journalDir)
	if verdict.Action != "rollback-must-complete" && verdict.Action != "already-committed-no-resume-needed" {
		// Spec says step 5 finds "latest with committed (or
		// pre-os-mutating for partial)" — the rollback-in-progress
		// marker we just wrote means our verdict is rollback-must-
		// complete; the original forward state is in v.Markers.
		// Either committed or pre-os-mutating must appear there.
		hasForwardProgress := false
		for _, m := range verdict.Markers {
			if m == MarkerCommitted || m == MarkerPreOsMutating || m == MarkerOsMutatingComplete {
				hasForwardProgress = true
				break
			}
		}
		if !hasForwardProgress {
			return fmt.Errorf("rollback: journal %s has no forward progress to undo", journalDir)
		}
	}

	// Step 6: translate quarantined entries → chronic-failure.
	if opts.QuarantineTranslator != nil {
		if err := opts.QuarantineTranslator(state); err != nil {
			return fmt.Errorf("step 6 quarantine translation: %w", err)
		}
	}

	// Step 7: uninstall supervisor autostart shim (idempotent).
	if opts.ShimUninstaller != nil {
		if err := opts.ShimUninstaller(); err != nil {
			return fmt.Errorf("step 7 shim uninstall: %w", err)
		}
	}

	// Step 8: re-register each legacy task from legacy-tasks/<name>.xml.
	// 2-3s settle BEFORE schtasks /Run (step 9). The pause is between
	// "ports unbound" (verified at step 3) and "task run" (step 9), so
	// we sleep here before either /Create or /Run.
	time.Sleep(settle)

	legacyDir := filepath.Join(journalDir, "legacy-tasks")
	// Lane C P0 #3 (round 2): do NOT swallow os.ReadDir errors. The ONLY
	// accepted error is `os.IsNotExist(err) && hasCommittedMarker` —
	// that pair means a clean zero-daemon migration committed without
	// ever creating the directory, which is a legitimate edge case.
	// Any other error (missing-no-marker, permission denied, corrupt
	// dir, ENOTDIR, etc.) must abort rollback: continuing would delete
	// supervisor state without proving the legacy XML can be replayed,
	// leaving the operator with no working scheduler tasks AND no
	// supervisor.
	legacyEntries, readDirErr := rollbackReadLegacyDirFn(legacyDir)
	hasCommittedMarker := markerExists(journalDir, MarkerCommitted)
	if readDirErr != nil {
		if !(os.IsNotExist(readDirErr) && hasCommittedMarker) {
			return fmt.Errorf("rollback: legacy-tasks dir unreadable (corrupt journal): %w", readDirErr)
		}
		// committed migration with absent legacy-tasks/: treat as
		// zero-daemon edge case. Clear entries so the loop below is a
		// no-op; warnings accumulator below records the warning.
		legacyEntries = nil
	}
	// Pre-scan to detect the genuine-zero-daemon committed case
	// BEFORE the loop strips non-xml entries silently. If committed
	// is present AND there are zero XML files, it's the edge case.
	zeroDaemonAtCommitted := hasCommittedMarker
	if zeroDaemonAtCommitted {
		for _, e := range legacyEntries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".xml") {
				continue
			}
			zeroDaemonAtCommitted = false
			break
		}
	}
	// Accumulate rollback-warnings.json entries here so the
	// zero-daemon edge case (Lane C P0 #3) can record its own warning
	// alongside step 10's port-bind-wait warnings.
	var warnings []rollbackWarning
	if zeroDaemonAtCommitted {
		warnings = append(warnings, rollbackWarning{
			Task:       "(none)",
			Reason:     "legacy-tasks/ empty on committed journal — zero-daemon migration; nothing to restore",
			ObservedAt: state.nowOrDefault().UTC().Format(time.RFC3339Nano),
		})
	}
	type restoredTask struct {
		name string
		port int
	}
	var restored []restoredTask
	for _, e := range legacyEntries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".xml") {
			continue
		}
		path := filepath.Join(legacyDir, e.Name())
		raw, readErr := os.ReadFile(path)
		if readErr != nil {
			return fmt.Errorf("step 8 read %s: %w", e.Name(), readErr)
		}
		// Recover the original task name from the XML's
		// <RegistrationInfo><URI>. Falls back to filename if absent.
		uri := extractTag(string(raw), "URI")
		taskName := strings.TrimSpace(uri)
		if taskName == "" {
			// fallback: derive from filename
			taskName = "\\" + strings.TrimSuffix(e.Name(), ".xml")
		}
		if err := opts.Scheduler.CreateXML(taskName, string(raw)); err != nil {
			return fmt.Errorf("step 8 CreateXML %s: %w", taskName, err)
		}
		// Find the expected port for this task from ExpectedDaemons.
		port := 0
		for _, d := range opts.ExpectedDaemons {
			if d.TaskName == taskName {
				port = d.Port
				break
			}
		}
		restored = append(restored, restoredTask{name: taskName, port: port})
	}

	// Step 9: schtasks /Run per task.
	for _, r := range restored {
		if err := opts.Scheduler.Run(r.name); err != nil {
			return fmt.Errorf("step 9 Run %s: %w", r.name, err)
		}
	}

	// Step 10: up to 60s wait-until-BOUND per restored task. Failures
	// → warnings. Wires through PortBindWaitBound (NOT PortBindWait —
	// those have opposite semantics; see RollbackOptions docstring).
	// (`warnings` was declared earlier so the zero-daemon committed
	// edge case from step 8 can record its own warning too.)
	for _, r := range restored {
		if r.port <= 0 {
			continue
		}
		if opts.PortBindWaitBound == nil {
			continue
		}
		// PortBindWaitBound returns nil when the daemon has bound the
		// port (net.Dial succeeded); any non-nil result is "did not
		// bind within timeout" and lands in rollback-warnings.json.
		err := opts.PortBindWaitBound(r.port, portTimeout)
		if err != nil {
			warnings = append(warnings, rollbackWarning{
				Task:       r.name,
				Port:       r.port,
				Reason:     "port-not-bound-after-" + portTimeout.String(),
				ObservedAt: state.nowOrDefault().UTC().Format(time.RFC3339Nano),
			})
		}
	}
	if len(warnings) > 0 {
		if err := writeRollbackWarnings(journalDir, warnings); err != nil {
			return fmt.Errorf("step 10 warnings persist: %w", err)
		}
	}

	// Step 11: delete supervisor-intent.json + supervisor-state.json +
	// supervisor-events.log.
	for _, fname := range []string{"supervisor-intent.json", "supervisor-state.json", "supervisor-events.log"} {
		_ = os.Remove(filepath.Join(state.StateDir, fname))
	}

	// Step 12: delete rollback-in-progress marker.
	_ = os.Remove(filepath.Join(journalDir, MarkerRollbackInProgress))

	// Step 13-14: LIFO release via deferred ls.Release().
	// Rollback exits 0 even with warnings (spec line 325).
	return nil
}

// rollbackReadLegacyDirFn is the seam for step 8's legacy-tasks/
// directory enumeration. Production binds it to os.ReadDir; tests
// override the package-level variable to inject specific error
// shapes (permission denied, ENOTDIR, corrupt index) that are
// otherwise hard to reproduce portably across OS file systems.
// The seam exists ONLY to keep TestRollback_LegacyDirUnreadableAborts
// portable between POSIX (where chmod + ENOTDIR work) and Windows
// (where os.ReadDir on a non-dir returns IsNotExist).
var rollbackReadLegacyDirFn = func(path string) ([]os.DirEntry, error) {
	return os.ReadDir(path)
}

// writeRollbackWarnings persists rollback-warnings.json under the journal.
func writeRollbackWarnings(journalDir string, warnings []rollbackWarning) error {
	payload := rollbackWarningsFile{Version: 1, Warnings: warnings}
	raw, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(journalDir, "rollback-warnings.json"), raw, 0600)
}

// _ = context to allow future ctx propagation without API churn.
var _ = context.Canceled
