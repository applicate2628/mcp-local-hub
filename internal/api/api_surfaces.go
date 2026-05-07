// Package api — Task 0 foundational ctx-aware API surfaces (watchdog plan v13 §32, §47, §59).
//
// This file is the single owner of the ctx-aware wrappers, the immutable
// ownership snapshot, and the typed self-quarantine reason that later
// watchdog tasks (1-12) build on. Production callers only see the public
// methods on *API; tests in api_surfaces_test.go drive package-level seam
// vars below to substitute fakes without spinning up a real Task Scheduler.
//
// Best-effort cancellation contract (§32 + §59):
//   StatusContext / RestartContext / RestartContextWithSnapshot run the
//   underlying op in a goroutine. When ctx is cancelled, the wrapper
//   returns ctx.Err() to the caller within ~10ms. The underlying op
//   continues to completion in the background — its result is dropped.
//   This is documented as best-effort because Status() and Restart()
//   delegate to schtasks, which we cannot interrupt mid-call. The
//   acceptable trade-off is that the watchdog loop never blocks past
//   its 4-min ctx deadline.
//
// Stubs deferred to later tasks:
//   - IntentAuditEntry / AppendIntentAudit                              → Task 3
//   - OwnedXMLValidator real validation logic (XML export/parse/limits) → Task 6
//   The minimal interfaces / structs / package-level seams below let
//   later tasks satisfy the contract without re-shaping Task 0 surfaces.
//
// Task 2 wired-up surfaces (now owned by daemon_intent.go):
//   - DaemonIntent struct, Reason / Desired enum constants, IsActiveStop
//     predicate, ReadDaemonIntent / WriteDaemonIntent / ClearDaemonIntent
//     methods. The readDaemonIntentFn seam below is bound during init()
//     in daemon_intent.go to a thin adapter over ReadDaemonIntent.
package api

import (
	"context"
	"strings"
	"time"

	"mcp-local-hub/internal/scheduler"
)

// WatchdogTaskName is the canonical scheduled-task name installed by
// `mcphub watchdog install` and removed by UninstallWatchdogTask /
// UninstallWatchdogTaskInternal. Kept as a package-level constant so
// later tasks (5, 9, 10) reference one canonical literal.
const WatchdogTaskName = "\\mcp-local-hub-watchdog"

// ---------------------------------------------------------------------------
// Test seams (package-level fn vars).
//
// Production: nil → fall back to the real implementation. Tests in this
// package set these to deterministic fakes inside install*Fn helpers.
//
// Why package-level vars rather than fields on *API: the Task 0 implementation
// constraint says api.go must not be touched (Status/Restart wrappers are
// added here without modifying their existing definitions). Adding fields
// to the API struct would require editing api.go. Package-level seams keep
// the change surface inside the two new files this task owns.
// ---------------------------------------------------------------------------

// statusContextSrcFn, when non-nil, replaces (*API).Status() inside
// StatusContext. Used by tests to inject deterministic []DaemonStatus rows
// without spinning up a scheduler.
var statusContextSrcFn func() ([]DaemonStatus, error)

// restartContextSrcFn, when non-nil, replaces (*API).Restart() inside
// RestartContext. The general-purpose Restart wrapper.
var restartContextSrcFn func(server, daemonFilter string) ([]RestartResult, error)

// restartContextWithSnapshotSrcFn, when non-nil, replaces the body of
// RestartContextWithSnapshot. Tests assert the snapshot is forwarded
// (kill-by-port targets snap.PortMap, not live manifest).
var restartContextWithSnapshotSrcFn func(server, daemonFilter string, snap OwnershipSnapshot) ([]RestartResult, error)

// schedulerFactoryFn, when non-nil, replaces scheduler.New() for
// UninstallWatchdogTask / UninstallWatchdogTaskInternal. Tests inject
// an in-memory scheduler that records Delete calls.
var schedulerFactoryFn func() (scheduler.Scheduler, error)

// appendIntentAuditFn, when non-nil, replaces the audit-append path
// invoked by UninstallWatchdogTaskInternal. Task 3 (intent_audit.go)
// will own the production implementation; for Task 0 the seam lets
// tests verify the audit-entry shape (Action + Reason canonical).
var appendIntentAuditFn func(IntentAuditEntry) error

// readDaemonIntentFn, when non-nil, replaces the intent-file read path
// invoked by IntentStillRunning. Task 2 (daemon_intent.go) will own the
// production implementation; for Task 0 the seam unblocks unit tests.
var readDaemonIntentFn func(taskName string) (DaemonIntent, bool, error)

// ---------------------------------------------------------------------------
// Stub types pending later tasks.
// ---------------------------------------------------------------------------

// (DaemonIntent / IntentDesired* / IsActiveStop moved to daemon_intent.go
// when Task 2 landed. The readDaemonIntentFn seam above is bound to the
// production reader by daemon_intent.go's init().)

// (IntentAuditEntry / NewIntentAuditEntry / newSystemAuditEntry /
// MarshalJSON / UnmarshalJSON / IsSystemEntry / RedactIntentAuditEntryForNonOwner
// moved to intent_audit.go when Task 3 landed. The appendIntentAuditFn
// seam above is bound to the production AppendIntentAudit by
// intent_audit.go's init(); UninstallWatchdogTaskInternal continues to
// reach the real audit log through the unchanged appendAudit dispatcher
// below.)

// ---------------------------------------------------------------------------
// SelfQuarantineReason — typed enum (§39, §56).
// ---------------------------------------------------------------------------

// SelfQuarantineReason is the typed enum carried in
// UninstallWatchdogTaskInternal calls and the resulting audit Reason field.
// Per §39: compile-time-typed prevents arbitrary content injection.
type SelfQuarantineReason string

const (
	// QuarantineFourStrikes30Min is set when the watchdog hits ≥4
	// CorruptStrikeWindow strikes in a 30-minute sliding window (§28).
	QuarantineFourStrikes30Min SelfQuarantineReason = "4-strikes-30min"
	// (extension point: future reasons keep the typed surface — e.g.
	// QuarantineAuditDegraded reserved per Security #7 INFO note.)
)

// SuggestedAction returns the operator-facing recovery instructions
// associated with each reason (§56). Used by `mcphub watchdog status`
// when rendering the WATCHDOG SELF-QUARANTINED block (§53).
func (r SelfQuarantineReason) SuggestedAction() string {
	switch r {
	case QuarantineFourStrikes30Min:
		return "verify state files clean; review .corrupt-* quarantines; then `mcphub watchdog install` to resume"
	default:
		return "manual investigation required; run `mcphub watchdog install` after resolving"
	}
}

// ---------------------------------------------------------------------------
// OwnershipSnapshot (v9 §47/§59).
// ---------------------------------------------------------------------------

// OwnershipSnapshot is the immutable per-tick view of the ownership
// universe consumed by the watchdog driver. Builds at `--once` start
// from manifest + workspace registry + manifest port map. Passed
// through RecoverStoppedDaemons + RestartContextWithSnapshot +
// NewOwnedXMLValidatorFromSnapshot so all ownership decisions in one
// tick see the same frozen state — defeats mid-tick rotation races.
//
// One-tick-scope-only: callers MUST pass a fresh LoadOwnershipSnapshot()
// per tick. Reusing a stale snapshot across ticks is incorrect (the
// PortMap/ManifestDaemons may be hours old).
type OwnershipSnapshot struct {
	// ManifestServers is server name → present in the manifest set.
	// Derived from listManifestNamesEmbedFirst().
	ManifestServers map[string]bool
	// ManifestDaemons is server → daemon-name → present (per
	// config.ServerManifest.Daemons []DaemonSpec; verified shape per
	// plan §5 v8-update). Empty inner maps are valid for servers
	// declared with no Daemons block.
	ManifestDaemons map[string]map[string]bool
	// WorkspaceTasksByKey maps a workspace-key composite ID to the
	// scheduler task name. Derived from the workspace registry
	// (internal/api/workspace_registry.go). Empty for non-workspace setups.
	WorkspaceTasksByKey map[string]string
	// PortMap maps task name → expected daemon port. Derived from
	// manifest at snapshot time per §59. Used by
	// RestartContextWithSnapshot to drive kill-by-port without re-reading
	// the manifest on a possibly-rotated tick.
	PortMap map[string]int
	// SnapshottedAt is the UTC time the snapshot was minted (forensic
	// correlation; used by status display and audit entries).
	SnapshottedAt time.Time
}

// ---------------------------------------------------------------------------
// DaemonRegistry — interface + impl (§32).
// ---------------------------------------------------------------------------

// DaemonRegistry is the immutable per-tick lookup the watchdog driver
// uses to filter orphan tasks. IsManagedDaemon answers "did mcp-local-hub
// install this task?" against a frozen view of {scheduler-status ∪
// manifest-known}. Construction (LoadDaemonRegistry) takes a defensive
// copy; subsequent IsManagedDaemon calls are pure lookups.
type DaemonRegistry interface {
	IsManagedDaemon(taskName string) bool
}

// daemonRegistryImpl is the immutable defensive-copy implementation
// returned by LoadDaemonRegistry. The managed-set is built once and
// never mutated after construction.
type daemonRegistryImpl struct {
	managed map[string]bool
}

// IsManagedDaemon performs a case-sensitive exact-match lookup. The
// watchdog driver passes the scheduler row's TaskName verbatim (with
// the leading backslash) so the registry stores names in that form.
func (r *daemonRegistryImpl) IsManagedDaemon(taskName string) bool {
	return r.managed[taskName]
}

// LoadDaemonRegistry returns a frozen DaemonRegistry by unioning:
//   - Every TaskName in the live Status() snapshot (catches workspace-
//     scoped lazy-proxy tasks not declared in any single manifest).
//   - Every (server, daemon) pair in the manifest set (catches tasks
//     known-installable but transiently absent from scheduler — install
//     mid-rollback, schtasks transient failure).
//
// The returned registry is detached from both sources; mutating the
// originals after construction has no effect.
func (a *API) LoadDaemonRegistry() DaemonRegistry {
	managed := make(map[string]bool)

	// Source 1: live scheduler status (defensive copy of the slice
	// before iteration — guards against caller mutating *during* the
	// iteration in tests that set seam fns).
	rows, _ := a.statusForRegistry()
	for _, r := range rows {
		if r.TaskName == "" {
			continue
		}
		managed[r.TaskName] = true
	}

	// Source 2: manifest-known servers + their declared daemons.
	names, _ := listManifestNamesEmbedFirst()
	for _, name := range names {
		data, err := loadManifestYAMLEmbedFirst(name)
		if err != nil {
			continue
		}
		m, err := parseManifestForName(name, data)
		if err != nil {
			continue
		}
		// Per-daemon task names follow the canonical pattern
		// "\mcp-local-hub-<server>-<daemon>" (parsed by parseTaskName
		// in status_enrich.go). Mirror that here.
		for _, d := range m.Daemons {
			tn := "\\mcp-local-hub-" + m.Name + "-" + d.Name
			managed[tn] = true
		}
	}

	return &daemonRegistryImpl{managed: managed}
}

// statusForRegistry returns a defensive snapshot of the current daemon
// status rows. Goes through the StatusContext seam if set (so tests
// see deterministic data); otherwise calls a.Status() directly.
//
// Errors are suppressed — the registry is best-effort. A status read
// failure leaves Source 1 empty; Source 2 still populates the managed
// set from manifest knowledge.
func (a *API) statusForRegistry() ([]DaemonStatus, error) {
	if statusContextSrcFn != nil {
		rows, err := statusContextSrcFn()
		// Defensive copy so caller mutations don't leak into the
		// registry's frozen state.
		out := make([]DaemonStatus, len(rows))
		copy(out, rows)
		return out, err
	}
	rows, err := a.Status()
	out := make([]DaemonStatus, len(rows))
	copy(out, rows)
	return out, err
}

// ---------------------------------------------------------------------------
// OwnedXMLValidator — interface + snapshot-bound impl (§32, §47).
// ---------------------------------------------------------------------------

// OwnedXMLValidator answers IsOwnedAndValid(taskName) — the watchdog
// driver's last gate before issuing a restart. Per §32:
//   - Plain validator (constructed from a fresh manifest read) exists
//     for non-watchdog callers (tests, future tools).
//   - Snapshot-bound validator (NewOwnedXMLValidatorFromSnapshot) wraps
//     a frozen OwnershipSnapshot so structural checks are tick-stable
//     even if the manifest rotates mid-tick.
//
// Real XML validation (DOCTYPE rejection, depth cap, schtasks deadline,
// principal/command/args assertions) lands in Task 6
// (watchdog_xml_validator.go). Task 0 returns an interface + snapshot-
// bound stub that does ownership-only checks.
type OwnedXMLValidator interface {
	IsOwnedAndValid(taskName string) bool
}

// NewOwnedXMLValidatorFromSnapshot constructs a snapshot-bound validator.
// Per Task 6 (watchdog_xml_validator.go) the returned validator runs the
// FULL hardened check chain on each call: schtasks /Query /XML with a 2s
// deadline, 64KB+1 size cap, byte-level DOCTYPE rejection, depth cap (32),
// strict decoder with Entity=nil + CharsetReader=nil, command / principal /
// run-level / logon-type field assertions, and structural ownership
// (manifest server+daemon or workspace registry TaskName byte-match).
//
// The snap argument is captured by reference; callers must NOT mutate it
// after passing (LoadOwnershipSnapshot already returns a defensive copy).
//
// Tests in watchdog_xml_validator_test.go inject the schtasksQueryXMLFn,
// canonicalMcphubPathFn, and currentWindowsUserFn seams to drive
// deterministic XML payloads without touching the host's real Task
// Scheduler.
func NewOwnedXMLValidatorFromSnapshot(snap OwnershipSnapshot) OwnedXMLValidator {
	return &ownedXMLValidator{snap: snap}
}

// ---------------------------------------------------------------------------
// LoadOwnershipSnapshot (§47, §59).
// ---------------------------------------------------------------------------

// LoadOwnershipSnapshot builds an immutable snapshot of the four
// ownership maps (ManifestServers, ManifestDaemons, WorkspaceTasksByKey,
// PortMap) plus a SnapshottedAt timestamp. Each map is a fresh copy;
// mutating the returned struct's fields cannot leak into shared state
// or affect future LoadOwnershipSnapshot calls.
//
// Errors during manifest/registry loads degrade silently — the
// corresponding map stays empty rather than fail-closed. The watchdog
// driver treats an empty PortMap as "no kill-by-port targets known"
// and falls back to scheduler-only restart, which is conservative.
func (a *API) LoadOwnershipSnapshot() OwnershipSnapshot {
	snap := OwnershipSnapshot{
		ManifestServers:     make(map[string]bool),
		ManifestDaemons:     make(map[string]map[string]bool),
		WorkspaceTasksByKey: make(map[string]string),
		PortMap:             make(map[string]int),
		SnapshottedAt:       time.Now().UTC(),
	}

	// Manifest set + per-server daemons + per-task PortMap.
	names, _ := listManifestNamesEmbedFirst()
	for _, name := range names {
		data, err := loadManifestYAMLEmbedFirst(name)
		if err != nil {
			continue
		}
		m, err := parseManifestForName(name, data)
		if err != nil {
			continue
		}
		snap.ManifestServers[m.Name] = true
		inner := make(map[string]bool, len(m.Daemons))
		for _, d := range m.Daemons {
			inner[d.Name] = true
			if d.Port != 0 {
				tn := "\\mcp-local-hub-" + m.Name + "-" + d.Name
				snap.PortMap[tn] = d.Port
			}
		}
		snap.ManifestDaemons[m.Name] = inner
	}

	// Workspace registry: task name keyed by (workspace, language).
	// Best-effort; failure leaves the map empty.
	if regPath, regErr := DefaultRegistryPath(); regErr == nil {
		reg := NewRegistry(regPath)
		if err := reg.Load(); err == nil {
			for _, e := range reg.Workspaces {
				if e.TaskName == "" {
					continue
				}
				key := e.WorkspaceKey + "-" + e.Language
				snap.WorkspaceTasksByKey[key] = e.TaskName
				if e.Port != 0 {
					snap.PortMap[e.TaskName] = e.Port
				}
			}
		}
	}

	return snap
}

// ---------------------------------------------------------------------------
// StatusContext (§32).
// ---------------------------------------------------------------------------

// StatusContext wraps (*API).Status with a goroutine + ctx-select pattern.
// On ctx.Done() the wrapper returns (nil, ctx.Err()) immediately; the
// underlying Status call continues to completion in the goroutine and
// its result is dropped (best-effort cancellation per §32).
//
// Production callers that don't need cancellation should keep using
// Status() directly — this wrapper costs one goroutine + one channel
// allocation per call.
func (a *API) StatusContext(ctx context.Context) ([]DaemonStatus, error) {
	type result struct {
		rows []DaemonStatus
		err  error
	}
	ch := make(chan result, 1)
	go func() {
		var rows []DaemonStatus
		var err error
		if statusContextSrcFn != nil {
			rows, err = statusContextSrcFn()
		} else {
			rows, err = a.Status()
		}
		// Buffered channel of cap 1 + only-one-sender → never blocks.
		ch <- result{rows: rows, err: err}
	}()
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case r := <-ch:
		return r.rows, r.err
	}
}

// ---------------------------------------------------------------------------
// RestartContext (§32) + RestartContextWithSnapshot (§59).
// ---------------------------------------------------------------------------

// RestartContext wraps (*API).Restart with a goroutine + ctx-select
// pattern. Best-effort cancellation: ctx.Done() returns ctx.Err() to
// the caller within ~10ms; the underlying Restart continues until
// schtasks completes or fails. General-purpose: reads the manifest
// fresh on each invocation, suitable for `mcphub restart` CLI.
//
// Watchdog callers MUST use RestartContextWithSnapshot instead — the
// snapshot variant pins kill-by-port to a frozen PortMap and prevents
// mid-tick manifest-rotation races (§59).
func (a *API) RestartContext(ctx context.Context, server, daemonFilter string) ([]RestartResult, error) {
	type result struct {
		results []RestartResult
		err     error
	}
	ch := make(chan result, 1)
	go func() {
		var res []RestartResult
		var err error
		if restartContextSrcFn != nil {
			res, err = restartContextSrcFn(server, daemonFilter)
		} else {
			res, err = a.Restart(server, daemonFilter)
		}
		ch <- result{results: res, err: err}
	}()
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case r := <-ch:
		return r.results, r.err
	}
}

// RestartContextWithSnapshot is the watchdog-only Restart variant. Per
// §59: consumes a frozen OwnershipSnapshot whose PortMap drives kill-
// by-port discovery. One-tick-scope-only — callers MUST pass a fresh
// LoadOwnershipSnapshot() per tick; reusing across ticks is incorrect.
//
// Tests assert behavior matches snap.PortMap, NOT the live manifest.
// The seam restartContextWithSnapshotSrcFn lets tests verify the
// snapshot is forwarded to the underlying impl rather than silently
// fallen back to the fresh-manifest path.
//
// TODO(task 5): the production body should mirror Restart's port-
// discovery logic (portForTask) but consult snap.PortMap before the
// live workspaceTasksByName / manifestPortMap calls. Until Task 5
// lands, the seam is the only production caller path; the watchdog
// driver in Task 9 will populate it.
func (a *API) RestartContextWithSnapshot(ctx context.Context, server, daemonFilter string, snap OwnershipSnapshot) ([]RestartResult, error) {
	type result struct {
		results []RestartResult
		err     error
	}
	ch := make(chan result, 1)
	go func() {
		var res []RestartResult
		var err error
		if restartContextWithSnapshotSrcFn != nil {
			res, err = restartContextWithSnapshotSrcFn(server, daemonFilter, snap)
		} else {
			// TODO(task 5): production body. For Task 0 there is no
			// production caller — the watchdog driver (Task 9) is the
			// only consumer and will set restartContextWithSnapshotSrcFn
			// after Task 5 lands. Falling back to a.Restart preserves
			// the manifest-fresh semantic for any unexpected caller
			// without losing the ctx-cancellation contract.
			res, err = a.Restart(server, daemonFilter)
		}
		ch <- result{results: res, err: err}
	}()
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case r := <-ch:
		return r.results, r.err
	}
}

// ---------------------------------------------------------------------------
// WaitDaemonRunning (§32).
// ---------------------------------------------------------------------------

// WaitDaemonRunning polls StatusContext every 1s until either a row
// matching taskName has State == "Running" (returns true) or ctx is
// done (returns false). The first poll fires immediately so a daemon
// already running returns true with sub-second latency.
//
// Polling pattern: time.NewTicker(1*time.Second) + select on
// ticker.C / ctx.Done. The ticker is stopped on exit so the goroutine
// does not leak.
//
// Used by the watchdog driver (Task 9) to verify a restart actually
// reached the Running state within the post-restart observation window.
func (a *API) WaitDaemonRunning(ctx context.Context, taskName string) bool {
	// Initial poll before starting the ticker so an already-Running
	// daemon returns immediately.
	if a.daemonIsRunning(ctx, taskName) {
		return true
	}
	if err := ctx.Err(); err != nil {
		return false
	}
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return false
		case <-ticker.C:
			if a.daemonIsRunning(ctx, taskName) {
				return true
			}
		}
	}
}

// daemonIsRunning is the per-poll helper for WaitDaemonRunning. It
// performs one StatusContext call and matches on TaskName + State.
// A status error returns false (the next tick will retry).
func (a *API) daemonIsRunning(ctx context.Context, taskName string) bool {
	rows, err := a.StatusContext(ctx)
	if err != nil {
		return false
	}
	for _, r := range rows {
		if r.TaskName == taskName && r.State == "Running" {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// IntentStillRunning (§32).
// ---------------------------------------------------------------------------

// IntentStillRunning reports whether the operator-recorded intent for
// taskName permits auto-revive. Returns true iff the intent is NOT an
// active stop directive at evaluation time `now`. Concretely:
//   - missing intent              → true (no recorded preference)
//   - intent.Desired = running    → true
//   - intent.Desired = stopped + fresh (within TTL) → false
//   - intent.Desired = stopped + stale (past TTL)   → true
//
// Wraps ReadDaemonIntent (Task 2 owner, daemon_intent.go) +
// DaemonIntent.IsActiveStop. The readDaemonIntentFn seam is bound by
// daemon_intent.go's init() to a thin adapter over ReadDaemonIntent;
// tests overwrite it via installTestIntentReader for deterministic
// fakes (cleanup restores the production binding).
func (a *API) IntentStillRunning(taskName string, now time.Time) bool {
	if readDaemonIntentFn == nil {
		// Defensive: daemon_intent.go's init() should always have wired
		// this. If we ever observe nil here it means Task 2's init()
		// never ran (unlikely; would imply a binary built without
		// daemon_intent.go), so degrade to "no recorded preference".
		return true
	}
	intent, ok, err := readDaemonIntentFn(taskName)
	if err != nil || !ok {
		// Read failure or no entry → no active stop directive.
		return true
	}
	active, _ := intent.IsActiveStop(now)
	return !active
}

// ---------------------------------------------------------------------------
// UninstallWatchdogTask + UninstallWatchdogTaskInternal (§32, §39, §63).
// ---------------------------------------------------------------------------

// UninstallWatchdogTask is the public idempotent removal of the
// scheduled task that drives `mcphub watchdog --once`. Backed by
// scheduler.Delete which already treats "task not found" as success
// (idempotent). Used by `mcphub watchdog uninstall` (Task 10 wires
// the CLI surface; Task 0 owns the API contract).
//
// Per §64: the CLI `watchdog uninstall` command adds an interactive
// confirm + `--yes` flag + non-TTY exit-6. Those concerns belong to
// the CLI layer; this API method is the unconditional execution path
// used by both the public uninstall and any future scripted callers.
func (a *API) UninstallWatchdogTask() error {
	sch, err := newScheduler()
	if err != nil {
		return err
	}
	return sch.Delete(WatchdogTaskName)
}

// UninstallWatchdogTaskInternal is the typed-reason variant called
// ONLY by the self-quarantine path (§39). Writes a sealed audit entry
// with Action="watchdog-self-quarantined" + Reason=<enum-string-value>
// per §63 v12 canonical contract.
//
// Audit-then-delete ordering: emit audit BEFORE calling scheduler.Delete
// so a delete failure still leaves a forensic record. Audit-write
// failures are surfaced to the caller (the watchdog driver in Task 9
// is responsible for the §38 audit-degraded fallback cascade).
//
// TODO(task 3): the audit-write path currently routes through the
// appendIntentAuditFn seam. When Task 3 (intent_audit.go) lands, the
// production AppendIntentAudit implementation should be the default
// behind that seam. The (*API).UninstallWatchdogTaskInternal contract
// — Action literal, Reason enum string — must remain stable.
func (a *API) UninstallWatchdogTaskInternal(reason SelfQuarantineReason) error {
	// Validate the reason is a known enum value. Guards against future
	// callers passing untyped strings via reflection or downstream
	// API misuse. Empty / unknown reasons fall back to a generic label.
	if !isKnownQuarantineReason(reason) {
		// Don't reject — record the unknown reason verbatim. Forensic
		// visibility beats fail-closed here; the audit trail will show
		// exactly what the caller passed.
	}

	// Task 3 wired AppendIntentAudit behind the appendAudit dispatcher
	// via init() in intent_audit.go. Caller-fingerprint fields (CallerPID,
	// CallerExe, CallerStartTime, CallerUser) are auto-populated by
	// AppendIntentAudit; we only set the canonical contract fields here
	// (Action literal, Task identity, Reason enum value, Priority high).
	entry := IntentAuditEntry{
		TS:       time.Now().UTC(),
		Action:   "watchdog-self-quarantined",
		Task:     WatchdogTaskName,
		Reason:   string(reason),
		Priority: "high",
	}
	if err := appendAudit(entry); err != nil {
		// §38 v9: audit-degraded cascade lives in Task 3 + driver. For
		// Task 0 we surface the failure verbatim; the driver will
		// translate into the watchdog.log + stderr + eventlog cascade.
		return err
	}

	sch, err := newScheduler()
	if err != nil {
		return err
	}
	return sch.Delete(WatchdogTaskName)
}

// isKnownQuarantineReason returns true for declared SelfQuarantineReason
// constants. Case-sensitive exact match.
func isKnownQuarantineReason(r SelfQuarantineReason) bool {
	switch r {
	case QuarantineFourStrikes30Min:
		return true
	default:
		return false
	}
}

// ---------------------------------------------------------------------------
// Internal helpers.
// ---------------------------------------------------------------------------

// newScheduler is the package-level scheduler factory. Routes through
// the schedulerFactoryFn seam if set (tests), otherwise scheduler.New().
func newScheduler() (scheduler.Scheduler, error) {
	if schedulerFactoryFn != nil {
		return schedulerFactoryFn()
	}
	return scheduler.New()
}

// appendAudit is the package-level audit dispatcher. Routes through
// the appendIntentAuditFn seam if set, otherwise returns a sentinel
// error indicating Task 3 has not yet wired the production path.
//
// TODO(task 3): set appendIntentAuditFn to the real AppendIntentAudit
// implementation in an init() inside intent_audit.go (Task 3 owner).
func appendAudit(e IntentAuditEntry) error {
	if appendIntentAuditFn != nil {
		return appendIntentAuditFn(e)
	}
	// Production: until Task 3 lands, treat audit as a no-op rather
	// than fail-closed. The watchdog driver (Task 9) refuses to emit
	// UninstallWatchdogTaskInternal calls until Task 3 ships, so this
	// fallback is unreachable in supported deployment timelines.
	return nil
}

// trimTaskPrefix is a tiny helper kept here in case future Task 0+
// consumers need to strip the leading backslash. Task Scheduler returns
// names with a backslash (\mcp-local-hub-X); manifest-derived names
// omit it. The Task 0 implementation prefers to keep the backslash on
// registry/snapshot lookups so callers don't have to normalize at every
// call site.
//
// Currently unused inside Task 0; kept exported for Task 1+ readers.
//
//nolint:unused // referenced by later watchdog tasks per plan v13.
func trimTaskPrefix(name string) string {
	return strings.TrimPrefix(name, "\\")
}
