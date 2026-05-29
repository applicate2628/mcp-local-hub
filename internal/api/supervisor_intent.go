package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// SupervisorIntentFile is the on-disk schema for <state-dir>/supervisor-intent.json.
// Spec §"supervisor-intent.json (NEW)".
type SupervisorIntentFile struct {
	Version           int                `json:"version"`
	UpdatedAt         string             `json:"updated_at"`
	Daemons           []SupervisorDaemon `json:"daemons"`
	MaintenanceTimers []MaintenanceTimer `json:"maintenance_timers,omitempty"`
	StrictMode        bool               `json:"strict_mode"`
}

// SupervisorDaemon is one daemon descriptor keyed by canonical
// leading-backslash task name. Reconcile-prune (Q12) strips the
// prefix at compare time to match production install.go:1639-1642
// BARE form planned map.
type SupervisorDaemon struct {
	TaskName     string            `json:"task_name"` // canonical, e.g. "\\mcp-local-hub-memory-default"
	Server       string            `json:"server"`
	Daemon       string            `json:"daemon"`
	Command      string            `json:"command"`
	Args         []string          `json:"args"`
	Env          map[string]string `json:"env,omitempty"`
	Workspace    string            `json:"workspace,omitempty"`
	Port         int               `json:"port"`
	ManifestHash string            `json:"manifest_hash"`
}

// MaintenanceTimer schedules a fixed-cadence in-process job. Two
// kinds in v0.5.0: workspace-weekly-refresh, server-weekly-refresh.
// No cron parser; new kinds get new in-tree evaluators.
type MaintenanceTimer struct {
	Name    string   `json:"name"` // canonical task name for migration provenance
	Kind    string   `json:"kind"` // "workspace-weekly-refresh" | "server-weekly-refresh"
	Server  string   `json:"server,omitempty"`
	Command string   `json:"command"`
	Args    []string `json:"args"`
	// Enabled is the operator-visible off-switch for a maintenance
	// timer. Tri-state via *bool to preserve backward compatibility
	// with v0.5.x supervisor-intent.json files written before PR #243:
	//
	//   - nil    — field absent in JSON (legacy install); scheduler
	//              treats as "enabled" (default-on). New installs
	//              MAY emit &true explicitly for clarity but are
	//              not required to.
	//   - &true  — explicitly enabled; scheduler honors the timer.
	//   - &false — explicitly disabled; scheduler skips the timer
	//              entirely (no fire, no transient PID, no audit
	//              event). The operator-supported off-switch
	//              consultant strategic concern #1 on PR #243 named
	//              as a pre-merge blocker — the alternative (manual
	//              JSON surgery on supervisor-intent.json or
	//              `maintenance_fired_at`) is too weak for unattended
	//              weekly execution.
	//
	// Live reload: the IntentWatcher already re-reads
	// supervisor-intent.json on mtime change and refreshes the
	// controller's intent cache. Operator action is therefore: edit
	// the timer's Enabled to false in supervisor-intent.json, save —
	// next scheduler Tick (within 60s) honors the new state. No
	// supervisor restart required.
	//
	// The disable lever does NOT clear `maintenance_fired_at[kind]` —
	// re-enabling a disabled timer resumes from the last-fired
	// baseline (no catch-up storm of the disabled-window's missed
	// fires).
	Enabled *bool `json:"enabled,omitempty"`
}

// ReadSupervisorIntent reads + parses with DisallowUnknownFields per
// the daemon-intent.json precedent at internal/api/daemon_intent.go:570-580.
//
// As a defensive post-parse step, legacy one-shot command entries
// (e.g. `mcphub watchdog --once`) are stripped from the Daemons slice.
// A migration pass on a v0.4.x install captured the watchdog scheduled
// task into supervisor-intent.json as if it were a long-lived daemon
// descriptor; the supervisor's reconcile loop then re-spawns it on
// every cycle because the `--once` command exits immediately. The
// filter here makes that self-healing on the read side so existing
// broken intent files don't continue the spawn-then-exit loop.
//
// See also: filterSupervisorIntentOneshotDaemons() for the criteria.
func ReadSupervisorIntent(path string) (*SupervisorIntentFile, error) {
	if !operatorAllowsUnhardenedStateRead() {
		if err := checkStateDirParentWriteSafe(filepath.Dir(path)); err != nil {
			return nil, fmt.Errorf("read %s: insecure parent directory (set %s=1 to opt into the relax lane on operator-managed Windows hosts whose %%LOCALAPPDATA%% inherits AD-pushed groups, or tighten the parent's DACL): %w",
				path, AllowUnhardenedStateReadEnv, err)
		}
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		// Include the path in the error so callers (and Sentry-style
		// log aggregators) can correlate failures to a specific
		// installation's file location without having to prepend a
		// prefix themselves. PR #212 r5 finding 5A.
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var f SupervisorIntentFile
	if err := dec.Decode(&f); err != nil {
		return nil, fmt.Errorf("decode %s: %w", path, err)
	}
	filterSupervisorIntentOneshotDaemons(&f)
	return &f, nil
}

// filterSupervisorIntentOneshotDaemons strips legacy one-shot command
// entries from the Daemons slice. A "one-shot" entry is recognized by
// the daemon's first CLI argument matching a known one-shot subcommand
// that does not run as a long-lived process (e.g. "watchdog --once").
//
// Why: the v0.4.x→v0.5.0 migration captured the watchdog scheduled
// task in supervisor-intent.json, but `mcphub watchdog --once` exits
// after one tick rather than staying alive. Supervisor's reconcile
// loop spawns it, sees the immediate exit via cmd.Wait reaper, marks
// the daemon Stopped, then the next reconcile tick spawns it again —
// a wasteful respawn loop that pollutes supervisor-events.log with
// "daemon-spawned" entries and confuses operators who see one watchdog
// in Task Scheduler AND another in Dashboard.
//
// The post-parse filter is the migration's self-healing surface: once
// existing intent files are loaded through ReadSupervisorIntent and
// re-written via WriteSupervisorIntent (any state-mutating supervisor
// path), the on-disk artifact heals automatically.
func filterSupervisorIntentOneshotDaemons(f *SupervisorIntentFile) {
	if f == nil || len(f.Daemons) == 0 {
		return
	}
	kept := make([]SupervisorDaemon, 0, len(f.Daemons))
	for _, d := range f.Daemons {
		if isLegacyOneshotDaemon(d) {
			continue
		}
		kept = append(kept, d)
	}
	f.Daemons = kept
}

// isLegacyOneshotDaemon reports whether the descriptor is for a known
// one-shot subcommand that must NOT be treated as a long-lived daemon.
//
// Currently matches: `mcphub watchdog --once` exactly. The strict
// two-arg match avoids accidentally stripping a future long-lived
// daemon whose first arg happens to be `watchdog` (e.g. an `mcphub
// watchdog serve` daemon variant). Add new patterns here as future
// migrations surface them.
func isLegacyOneshotDaemon(d SupervisorDaemon) bool {
	if len(d.Args) < 2 {
		return false
	}
	return d.Args[0] == "watchdog" && d.Args[1] == "--once"
}

// WriteSupervisorIntent goes through WriteStateFileAtomic (Task 1.1).
func WriteSupervisorIntent(path string, f *SupervisorIntentFile) error {
	return WriteStateFileAtomic(path, f)
}

// supervisorIntentLockSuffix is the gofrs/flock lock-leaf suffix for
// supervisor-intent.json. It is exactly the `<path>.lock` form
// WriteStateFileAtomic derives internally (state_file_helper.go:85), so a
// caller that wants an atomic read-modify-write across the supervisor-intent
// file can acquire `intentPath + supervisorIntentLockSuffix` and serialize
// against WriteStateFileAtomic writers (migration, autostart,
// InstallParsedManifest) across goroutines AND processes.
const supervisorIntentLockSuffix = ".lock"

// writeSupervisorIntentLockHeld marshals + writes the supervisor-intent file
// WITHOUT acquiring the per-file flock, for callers that already hold
// `path + supervisorIntentLockSuffix`. It mirrors daemon_intent.go's
// readIntentLocked/writeIntentLocked split: WriteSupervisorIntent (and the
// WriteStateFileAtomic it wraps) re-acquires the same flock, so calling it
// while the lock is held would DEADLOCK on Windows LockFileEx. This helper
// runs only the lock-free secure-write body (marshal + mkdir +
// secureWriteStateFileWithOperatorOpt), preserving the hardened DACL/atomic-
// rename pipeline and the MCPHUB_REQUIRE_SINGLE_USER_HOME posture.
//
// PRECONDITION: the caller MUST hold the supervisor-intent flock. Calling this
// without the lock loses the cross-process write serialization
// WriteStateFileAtomic otherwise provides.
func writeSupervisorIntentLockHeld(path string, f *SupervisorIntentFile) error {
	raw, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("mkdir: %w", err)
	}
	return secureWriteStateFileWithOperatorOpt(path, raw)
}
