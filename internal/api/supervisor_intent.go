package api

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// SupervisorIntentFile is the on-disk schema for <state-dir>/supervisor-intent.json.
// Spec §"supervisor-intent.json (NEW)".
type SupervisorIntentFile struct {
	Version           int                `json:"version"`
	UpdatedAt         string             `json:"updated_at"`
	Daemons           []SupervisorDaemon `json:"daemons"`
	MaintenanceTimers []MaintenanceTimer `json:"maintenance_timers,omitempty"`
	StrictMode        bool               `json:"strict_mode"`

	// Stops is the v0.6 Phase 4-E dual-intent collapse target: the
	// per-task operator-stop / quarantine override sub-block that used
	// to live in the separate daemon-intent.json file. Keyed by canonical
	// leading-backslash task_name (same key shape as Daemons[].TaskName).
	// The value is the SAME DaemonIntent record the legacy file used, so
	// the pure IsActiveStop predicate (daemon_intent.go:309) ports
	// verbatim with no I/O — the SM input shape is unchanged (spec §5.1-E
	// "api.Transition is untouched").
	//
	// Deliberately a SEPARATE sub-record rather than widening
	// SupervisorDaemon (spec §15 P1-c): a per-daemon Desired/Reason field
	// would force a 31-caller round-trip and re-couple immutable runtime
	// descriptors with mutable stop-overrides, WIDENING write-contention.
	// The separate map keeps the two concerns independent — a stop write
	// touches only Stops, never the Daemons slice.
	//
	// Additive-first (Phase E1): omitempty so every pre-collapse
	// supervisor-intent.json round-trips byte-unchanged through
	// ReadSupervisorIntent (plain json.Unmarshal, NOT DisallowUnknownFields
	// — supervisor_intent.go:169 — so an OLD binary that predates this
	// field simply ignores it). In E1 the legacy daemon-intent.json STAYS
	// on disk and STAYS written by install_intent.go's writers; this
	// sub-block is the merged recovery baseline + the new canonical home
	// readers learn. E2 later deletes daemon-intent.json + its writers and
	// makes this sub-block the sole stop source.
	Stops map[string]DaemonIntent `json:"stops,omitempty"`
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

	// StartupBindDeadlineSeconds bounds how long a freshly-spawned child may
	// take to FIRST bind its port before the liveness sweep treats port_unbound
	// as a restart trigger (P1b first-bind deadline). Before the first observed
	// bind of the current generation the sweep grants this deadline instead of
	// the flat 5s post-bind grace, so a slow-starting daemon (e.g. a
	// serena-proxy whose language-server subprocess takes tens of seconds to
	// come up — measured go LSP cold = 46s) is no longer terminate-first-then-
	// respawned mid-startup. After the first bind, the 5s post-bind rule applies
	// byte-for-byte as before.
	//
	// 0 = default: 60s for global/legacy daemons, 120s for serena-proxy
	// descriptors (resolved by supervisorStartupBindDeadline). Additive +
	// omitempty: old binaries ignore it; old intent files read as 0 → default
	// resolution. Config validation caps it at 600s.
	StartupBindDeadlineSeconds int `json:"startup_bind_deadline_seconds,omitempty"`

	// RuntimeSpec is the MATERIALIZED child runtime spec for daemons whose
	// launcher (e.g. `mcphub daemon serena-proxy`) must NOT re-read the
	// server manifest at spawn. It carries the final child command + args
	// (incl. --project <workspace> and an appended --context <value>), the
	// raw env refs (secret:KEY verbatim), the internal + external ports, and
	// the canonical workspace path — everything the launcher needs without
	// touching the embedded manifest.
	//
	// nil for legacy/global daemons that the supervisor spawns via the
	// generic `mcphub daemon --server --daemon` path. Additive + omitempty:
	// existing pre-RuntimeSpec supervisor-intent.json files round-trip
	// unchanged through ReadSupervisorIntent (nil spec); a new supervisor
	// reading them re-materializes on next install. Mirrors the additive-field
	// discipline Lifecycle /
	// LastMaterializedAt use on WorkspaceEntry.
	//
	// Design ref: docs/superpowers/specs/2026-05-29-serena-migrate-redesign-descriptor-proxy.md §3.
	RuntimeSpec *DaemonRuntimeSpec `json:"runtime_spec,omitempty"`
}

// DaemonRuntimeSpecVersion is the current DaemonRuntimeSpec.SpecVersion.
// Bumped on an INCOMPATIBLE field-shape change so a proxy reading a
// descriptor produced by a newer binary can fail loud rather than
// mis-spawn. The proxy fails loud on any value it does not support.
const DaemonRuntimeSpecVersion = 1

// DaemonRuntimeSpec carries everything the launcher needs to spawn the
// upstream child WITHOUT reading the server manifest. Build-time concerns
// (port_pool, languages, kind) are intentionally absent — they are
// register/migrate-time inputs, not runtime.
//
// Design ref: docs/superpowers/specs/2026-05-29-serena-migrate-redesign-descriptor-proxy.md §3.
type DaemonRuntimeSpec struct {
	// SpecVersion is DaemonRuntimeSpecVersion at materialization time; the
	// launcher fails loud on an unsupported value (no manifest fallback).
	SpecVersion int `json:"spec_version"`
	// ChildCommand is the materialized upstream command (e.g. "uvx") — the
	// raw interpreter the proxy execs, NOT the mcphub wrapper.
	ChildCommand string `json:"child_command"`
	// ChildArgs is FULLY materialized: m.BaseArgs ++ expanded
	// ExtraArgsTemplate (${workspace.path} already substituted) ++ a
	// trailing [--context, <DaemonTemplate.Context>] appended by the
	// materializer. It does NOT include --port; the launcher appends the
	// internal (upstream) port at spawn.
	ChildArgs []string `json:"child_args"`
	// EnvRefs is the raw env map incl. unresolved secret:KEY values — the
	// launcher resolves them against the vault at spawn so the descriptor
	// file stays cleartext-free on disk.
	EnvRefs map[string]string `json:"env_refs,omitempty"`
	// UpstreamPort is the internal port the child binds (external Port +
	// config.NativeHTTPInternalPortOffset).
	UpstreamPort int `json:"upstream_port"`
	// ExternalPort is the client-facing port the proxy binds; mirrors
	// SupervisorDaemon.Port (a build-time invariant asserts they agree).
	ExternalPort int `json:"external_port"`
	// WorkspacePath is the canonical absolute workspace path; mirrors
	// SupervisorDaemon.Workspace (a build-time invariant asserts they agree).
	WorkspacePath string `json:"workspace_path"`
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

// ReadSupervisorIntent reads + parses the supervisor's desired daemon set.
// Unknown fields are ignored deliberately: this file is rewritten by newer
// binaries, and a rollback must not brick supervisor startup just because it
// sees a future additive field. JSON type/shape errors still fail through
// Unmarshal.
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
	raw, err := readSupervisorIntentFileInodeAnchored(path)
	if err != nil {
		if isHubMcpStateMissingErr(err) {
			return nil, fmt.Errorf("read %s: %w", path, os.ErrNotExist)
		}
		// Include the path in the error so callers (and Sentry-style
		// log aggregators) can correlate failures to a specific
		// installation's file location without having to prepend a
		// prefix themselves. PR #212 r5 finding 5A.
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	var f SupervisorIntentFile
	if err := json.Unmarshal(raw, &f); err != nil {
		return nil, fmt.Errorf("decode %s: %w", path, err)
	}
	filterSupervisorIntentOneshotDaemons(&f)
	return &f, nil
}

func readSupervisorIntentFileInodeAnchored(path string) ([]byte, error) {
	return readStateFileInodeAnchored(path)
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

// DefaultSupervisorIntentPath returns the absolute path of
// supervisor-intent.json under the resolved per-user state directory. It is
// the canonical lookup the supervisor reconciler, the install seam, and the
// serena-proxy descriptor read all share, so the basename lives in exactly
// one place. Resolution honors the daemonStateRootOverride test seam
// (api.SetDaemonStateRootForTest), so cross-package tests redirect it without
// env vars.
func DefaultSupervisorIntentPath() (string, error) {
	stateDir, err := DaemonStateDir()
	if err != nil {
		return "", err
	}
	return joinStateFilePath(stateDir, supervisorIntentFileLeaf), nil
}

// SupervisorIntentPathEnvVar is the dedicated mcphub-internal env var the
// supervisor sets when spawning a descriptor whose launcher must read
// supervisor-intent.json from a control-plane path that is IMMUNE to the
// child/manifest env (e.g. the serena-proxy, whose manifest env may set
// HOME / XDG_*_HOME for the upstream serena data dir). The supervisor resolves
// the canonical intent path ONCE (against its own environment) and injects it
// here, AFTER the manifest/overlay env merge so the child env cannot clobber
// it. ResolveSupervisorIntentPathForProxy reads it.
//
// Bot PR #246 P2: without this channel the serena-proxy resolved its intent
// path via DefaultSupervisorIntentPath → DaemonStateDir, which on POSIX honors
// the (child-overlaid) HOME / XDG_STATE_HOME — so the proxy looked in the
// upstream child's configured home, missed the real supervisor-intent.json, and
// never started.
const SupervisorIntentPathEnvVar = "MCPHUB_SUPERVISOR_INTENT_PATH"

// ResolveSupervisorIntentPathForProxy returns the supervisor-intent.json path a
// descriptor-driven launcher (e.g. serena-proxy) should read. It prefers the
// explicit MCPHUB_SUPERVISOR_INTENT_PATH channel the supervisor injects (immune
// to the manifest/child env) and falls back to DefaultSupervisorIntentPath when
// the var is unset (manual invocation, legacy spawn without the channel).
//
// The fallback path's state-dir resolution honors HOME / XDG_*_HOME on POSIX,
// which is exactly why the env channel exists: a serena-proxy spawned with a
// manifest env that redirects HOME for the serena data dir must NOT resolve its
// control-plane intent path against that redirected home (bot PR #246 P2).
func ResolveSupervisorIntentPathForProxy() (string, error) {
	if p := os.Getenv(SupervisorIntentPathEnvVar); p != "" {
		return p, nil
	}
	return DefaultSupervisorIntentPath()
}

// FindSupervisorDaemonByTaskName returns a COPY of the descriptor in f whose
// TaskName matches taskName exactly, or nil if none match. The exact match is
// load-bearing for the serena-proxy descriptor lookup: the proxy execs with
// --task-name <its-own-canonical-task-name> and must resolve to exactly its
// own row (design §3.2). Returns a copy so callers cannot mutate the slice
// element through the pointer.
func (f *SupervisorIntentFile) FindSupervisorDaemonByTaskName(taskName string) *SupervisorDaemon {
	if f == nil {
		return nil
	}
	for i := range f.Daemons {
		if f.Daemons[i].TaskName == taskName {
			d := f.Daemons[i]
			return &d
		}
	}
	return nil
}

// HasRuntimeSpecRow reports whether any daemon row in f carries a non-nil
// RuntimeSpec. A runtime_spec-bearing intent file is the §7.1 split-brain
// hazard (bot PR #246 r2): an OLD supervisor binary's ReadSupervisorIntent uses
// DisallowUnknownFields and rejects the new field, so any writer that emits such
// rows must first ensure the running supervisor is this binary (or none is
// running). The spec-bearing supervisor-intent write gate in InstallParsedManifest
// uses this to scope the gate: legacy/global intents with no runtime_spec rows
// are read fine by an old supervisor and are NOT gated.
func (f *SupervisorIntentFile) HasRuntimeSpecRow() bool {
	if f == nil {
		return false
	}
	for i := range f.Daemons {
		if f.Daemons[i].RuntimeSpec != nil {
			return true
		}
	}
	return false
}

// HasSerenaDaemonForWorkspaceKey reports whether this intent carries a serena
// per-workspace daemon row for the given workspace key. Daemon task names are
// the canonical leading-backslash form, so the leading `\` is trimmed before
// comparing to the bare "mcp-local-hub-serena-<key>" the fan-out writes (the
// same match intentHasSerenaDaemonForKey uses against an on-disk intent, but
// over the IN-MEMORY desiredIntent). InstallParsedManifest uses this (via
// opts.RequireWorkspaceKey) to fail the commit BEFORE the write if the merged
// intent dropped a caller's required triggering workspace (bot PR #253 r6 P2).
func (f *SupervisorIntentFile) HasSerenaDaemonForWorkspaceKey(key string) bool {
	if f == nil {
		return false
	}
	want := "mcp-local-hub-serena-" + key
	for i := range f.Daemons {
		if strings.TrimPrefix(f.Daemons[i].TaskName, `\`) == want {
			return true
		}
	}
	return false
}

// StopsAsDaemonIntentFile returns the supervisor-intent stops sub-block
// shaped as a *DaemonIntentFile so the existing IsActiveStop readers
// consume it with NO change to their value shape. The returned file's
// Tasks map aliases f.Stops (read-only view) when present, else an empty
// non-nil map so callers never nil-deref.
//
// Phase 4-E1: this is the "new canonical path" the repointed readers
// learn. It is the recovery-baseline source UnifiedStopsFile falls back
// to when the live daemon-intent.json is absent/unreadable.
func (f *SupervisorIntentFile) StopsAsDaemonIntentFile() *DaemonIntentFile {
	if f == nil || f.Stops == nil {
		return &DaemonIntentFile{Tasks: map[string]DaemonIntent{}}
	}
	return &DaemonIntentFile{Tasks: f.Stops}
}

// UnifiedStopsFile resolves the single stop source the five repointed
// IsActiveStop readers must consult. After Phase 4-E2 the
// supervisor-intent.json `stops` sub-block is the SOLE, AUTHORITATIVE stop
// source: daemon-intent.json is deleted, its writers are gone, and the live
// stop writers (WriteStopIntent) maintain the sub-block directly.
//
// E1 → E2 PRECEDENCE FLIP. In E1 the live daemon-intent.json WON when present
// (additive, zero-regression while the legacy writers still maintained it).
// E2 inverts that: the sub-block is primary and the daemonIntentFile argument
// is IGNORED. The argument is retained only to keep the five call sites'
// signatures stable across the E1→E2 transition; a STALE daemon-intent.json
// that survives a failed/partial delete (or that an OLD binary re-creates)
// must NEVER override the sub-block now that the sub-block is the source the
// live writers maintain. Honoring a stale daemon-intent.json here would
// resurrect the exact dual-source split E2 removes. (Once every reader is
// confirmed off daemon-intent.json, a later cleanup can drop the parameter
// entirely; keeping it now narrows the E2 diff.)
//
// Returns a non-nil *DaemonIntentFile (empty Tasks when the sub-block is
// empty), so every reader indexes .Tasks without a nil guard.
//
// One owner for the source: all five repointed IsActiveStop call sites route
// through this function so the rule lives in exactly one place (no duplicated
// stop-source logic across the supervisor + GUI + tray surfaces).
func UnifiedStopsFile(supervisorIntent *SupervisorIntentFile, _ *DaemonIntentFile) *DaemonIntentFile {
	// E2: the sub-block is the sole authority. daemonIntentFile (the second
	// argument) is deliberately ignored — see the PRECEDENCE FLIP note above.
	return supervisorIntent.StopsAsDaemonIntentFile()
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
