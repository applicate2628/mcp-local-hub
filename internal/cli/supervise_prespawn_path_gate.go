package cli

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"mcp-local-hub/internal/api"
)

// Pre-spawn existence gate (P1.1).
//
// THE INCIDENT. An operator's host recorded
// `"command": "C:\\Users\\<user>\\.local\\bin\\mcphub.exe"` for all 12 daemons
// while that file did not exist. Every daemon IS the mcphub binary (the
// supervisor spawns itself with `daemon --server X --daemon Y`), so ALL 12
// failed identically — 541 `daemon-spawn-failed` rows, all
// `CreateProcess: The system cannot find the file specified.` Ten failures
// inside ~4 minutes burned each daemon's crash budget, producing 48
// `daemon-quarantined` rows. Quarantine is in-memory, so restarting the
// supervisor cleared it, the supervisor honestly retried, hit the same absent
// file, and re-quarantined ~4 minutes later — forever. The operator-facing
// message named only the threshold ("10+ failures in 30-min sliding window")
// and never mentioned the missing binary, so the actual remedy (reinstall)
// was invisible. That cost a working day.
//
// WHY THIS IS A PRE-SPAWN GATE AND NOT A POST-FAILURE CLASSIFIER. An earlier
// design classified spawn failures by errno. That approach was empirically
// REFUTED for the working-directory class: probes proved CreateProcessW
// collapses deleted-dir, missing-parent, file-as-dir, unreachable-UNC and
// unmounted-drive all into errno 267, so no discriminating signal exists (see
// work-items/.../REVISE-diagnosis-refuted.md). This gate deliberately does NOT
// classify errno. It asks one positive filesystem question — does the path
// exist — before create-process. That is a fact, not an inference.
//
// The "a pre-flight stat is a per-tick tax" objection does not apply here: the
// gate runs ONLY on create-process transitions (the same site as the shipped
// F1 port gate, which already pays a far more expensive netstat probe there),
// never on the reconcile/liveness tick. On a path we are about to spend a
// process creation on, two `os.Stat` calls are free.
//
// FAIL OPEN IS THE STANDING RULE. Only a definite "not found" holds. Access
// denied, I/O error, timeout, an empty path, or ANY non-local (UNC) path all
// proceed to spawn exactly as today. Probed evidence: an ordinary deny-ACE
// directory still spawns fine because bypass-traverse-checking is granted to
// Everyone by default, so treating ACCESS_DENIED as absent would park healthy
// daemons.
//
// THE HOLD IS NOT A QUARANTINE. It reuses the shipped `holdSpawnInBackoff`
// shape: the daemon returns to StBackoffWaiting with an armed re-probe timer
// and NO crash-count increment. It is deliberately NOT routed to
// errSpawnJobProtectionRefused — that target is an ABSORBING quarantine with
// no parole, so a briefly-unreachable path would be parked permanently on every
// reboot, strictly worse than today's self-healing ladder. The moment the file
// reappears — which is exactly what the operator's reinstall does — the next
// tick spawns normally with no operator action and no manual clearing.

const (
	// missingBinaryReasonID / missingWorkspaceReasonID are STABLE WIRE VALUES.
	// They travel through supervisor-state.json, the IPC status reply,
	// `mcphub status --json`, and the GUI badge. Renaming one is a contract
	// change; supervise_prespawn_binary_gate_test.go asserts the literals.
	missingBinaryReasonID    = "missing-binary"
	missingWorkspaceReasonID = "missing-workspace"
	// unavailable-* are the SAME hold with a different REMEDY. The path is not
	// deleted — the volume it lives on is not currently reachable (a
	// disconnected mapped network drive, an unmounted removable volume). Go
	// maps ERROR_BAD_NETPATH to fs.ErrNotExist
	// ($GOROOT/src/syscall/syscall_windows.go), so a disconnected `Z:\...`
	// is indistinguishable from a deleted file by the stat error alone.
	// Holding is still correct and self-healing, but telling that operator to
	// "reinstall mcphub" is the WRONG instruction: nothing is wrong with their
	// installation, and reinstalling would not fix a share that is offline.
	unavailableBinaryReasonID    = "unavailable-binary"
	unavailableWorkspaceReasonID = "unavailable-workspace"
)

// errSpawnHeldMissingPath is the sentinel executeSideEffect returns when the
// pre-spawn existence gate holds a spawn back because a path the daemon needs
// is absent. It is DISTINCT from errSpawnHeldPortSquatter so the force-respawn
// IPC path can report the honest cause rather than a misleading "port held by
// another process". Like the port-squatter sentinel it matches neither the
// `err == nil` nor the `errors.Is(err, errSpawnPreChild)` arm in
// executeSideEffect, so NO synthetic EvChildExit is posted and the crash budget
// is never touched.
var errSpawnHeldMissingPath = errors.New("spawn deferred: a path this daemon requires is absent (the supervisor re-probes and starts it automatically once the path exists)")

const (
	// missingPathHoldDelay is the re-probe cadence while a daemon is held. It
	// matches squatterForeignHoldDelay so both pre-spawn holds ride one timer
	// cadence, and it bounds operator-visible recovery latency after a
	// reinstall to ~30s.
	missingPathHoldDelay = 30 * time.Second
	// missingPathEscalateAfter is how long a hold must persist before the
	// event log escalates from warn to error.
	//
	// The first observation is deliberately warn, NOT error: `mcphub install
	// --upgrade` replaces the binary by rename-aside (MoveFileExW target ->
	// target.old-<ts>, then target.new -> target), so there is a legitimate
	// sub-second window on EVERY upgrade where the binary genuinely does not
	// exist. Emitting error there would cry wolf on the normal upgrade path.
	// A hold that outlives this interval is not an upgrade window.
	missingPathEscalateAfter = 2 * time.Minute
)

// spawnPathStatFunc is the injectable path-probe signature. It is declared here
// rather than spelled inline on the controller struct so supervisor_controller.go
// needs no new import: that file is concurrently edited by other in-flight
// branches (#569 adds backoff jitter at the arm-owner), and keeping this
// change's footprint there to a minimum keeps the merge mechanical.
type spawnPathStatFunc = func(string) (os.FileInfo, error)

// spawnPathVerdict is the three-valued result of probing one path. Only
// spawnPathAbsent holds a spawn; present and indeterminate both proceed.
type spawnPathVerdict int

const (
	spawnPathPresent spawnPathVerdict = iota
	spawnPathAbsent
	spawnPathIndeterminate
	// spawnPathUnavailable holds exactly like spawnPathAbsent but carries a
	// different remedy: the path's whole volume is unreachable rather than the
	// file being deleted.
	spawnPathUnavailable
)

// spawnHoldMarker records what the gate last emitted for a task so a daemon
// held for hours produces a bounded event stream instead of one error row per
// 30s tick per daemon (12 daemons x 2/min = the audit-flood class this repo
// already had to fix once).
type spawnHoldMarker struct {
	reasonID  string
	path      string
	firstAt   time.Time
	lastEmit  time.Time
	suppdRows int
}

// spawnHoldMarkers is the gate's per-controller dedupe state. Package-level
// state is deliberately avoided; the map hangs off the controller below.
type spawnHoldMarkers struct {
	mu sync.Mutex
	m  map[string]*spawnHoldMarker
}

func newSpawnHoldMarkers() *spawnHoldMarkers {
	return &spawnHoldMarkers{m: make(map[string]*spawnHoldMarker)}
}

// classifySpawnPath probes one path with the injected stat function.
//
// An empty path is INDETERMINATE, not absent: a descriptor with no Command or
// no Workspace has nothing to probe, and cmd.Dir is only set when Workspace is
// non-empty (supervise.go:3295), so an empty value is a legitimate shape.
//
// A NON-ABSOLUTE path is INDETERMINATE. This is load-bearing, not a nicety: a
// descriptor may legitimately carry a bare command name ("mcphub", "uvx",
// "npx", "node", "go") which the OS resolves through PATH, not relative to the
// supervisor's working directory. os.Stat simply cannot answer that question,
// and stat-ing "uvx" against the cwd would report absent for a perfectly
// installed tool and hold a healthy daemon forever.
//
// exec.LookPath was considered and REJECTED as the bare-name answer: the PATH
// the supervisor sees is not necessarily the PATH the child gets (the daemon
// env overlay rewrites the child environment), so a LookPath verdict can be
// wrong in BOTH directions. The incident this gate closes is the absolute-path
// case, which stat answers exactly; bare names fail open to today's behavior.
//
// A non-local (UNC) path is INDETERMINATE regardless of what stat says: a
// network path that is momentarily unreachable is a transient condition, and
// holding on it would be a false positive on every share hiccup.
func classifySpawnPath(stat func(string) (os.FileInfo, error), path string) spawnPathVerdict {
	p := strings.TrimSpace(path)
	if p == "" || isNonLocalSpawnPath(p) || !filepath.IsAbs(p) {
		return spawnPathIndeterminate
	}
	if stat == nil {
		stat = os.Stat
	}
	if _, err := stat(p); err == nil {
		return spawnPathPresent
	} else if errors.Is(err, fs.ErrNotExist) {
		// A definite, positive "not found" — but WHY it is not found decides
		// which remedy the operator gets. Probe the path's own volume root: if
		// the volume itself is unreachable, the file was not deleted, the drive
		// is offline (a disconnected mapped network drive reports
		// ERROR_BAD_NETPATH, which Go folds into fs.ErrNotExist). Both hold;
		// only the message differs.
		if root := spawnPathVolumeRoot(p); root != "" {
			if _, rootErr := stat(root); rootErr != nil {
				return spawnPathUnavailable
			}
		}
		return spawnPathAbsent
	}
	// Access denied, I/O error, name too long, timeout, anything else: fail
	// OPEN and let create-process render the real verdict, exactly as today.
	return spawnPathIndeterminate
}

// spawnPathVolumeRoot returns the root of the volume p lives on ("C:\" for
// `C:\a\b`), or "" when the platform has no meaningful volume to probe.
//
// On POSIX filepath.VolumeName is always empty: there is no per-volume root to
// distinguish "file deleted" from "mount gone", so those hosts always report
// the plain missing-* remedy. That is the honest degradation — a wrong-but-
// confident "the drive is offline" would be worse than the generic message.
func spawnPathVolumeRoot(p string) string {
	vol := filepath.VolumeName(p)
	if vol == "" {
		return ""
	}
	return vol + string(os.PathSeparator)
}

// isNonLocalSpawnPath reports whether p is a UNC / network path.
//
// `\\server\share\...` and `\\?\UNC\server\share\...` are UNC. `\\?\C:\...`
// and `\\.\C:\...` are extended-length LOCAL paths and must NOT be treated as
// network paths.
//
// RESIDUAL (documented, accepted): a drive-letter path backed by a MAPPED
// network drive (`Z:\...`) is not detected as non-local — that would need
// GetDriveTypeW. A disconnected mapped drive therefore reads as absent and the
// daemon is HELD. That is bounded and self-healing: the hold re-probes every
// 30s, consumes no crash budget, and recovers the moment the drive reconnects.
// It is strictly better than today's behavior for the same input (burn 10
// failures in ~4 min, quarantine, then re-quarantine on every 15-min parole).
func isNonLocalSpawnPath(p string) bool {
	q := strings.ReplaceAll(p, "/", `\`)
	if strings.HasPrefix(q, `\\?\UNC\`) || strings.HasPrefix(q, `\\.\UNC\`) {
		return true
	}
	if strings.HasPrefix(q, `\\?\`) || strings.HasPrefix(q, `\\.\`) {
		return false // extended-length LOCAL path
	}
	return strings.HasPrefix(q, `\\`)
}

// spawnHoldOperatorMessage is the SINGLE OWNER of the operator-facing wording
// for a held spawn. It names the cause, the exact path, and the remedy — the
// three things the incident's operator never got. The GUI composes its own
// short badge text from the stable reason id (a badge, a tooltip and a CLI line
// are three renderings of one fact); this string is the log + CLI rendering.
func spawnHoldOperatorMessage(reasonID, path string) string {
	switch reasonID {
	case missingBinaryReasonID:
		return fmt.Sprintf(
			"the mcphub program file is missing at %s — reinstall or update mcphub to restore it. Every daemon is started from this one file, so while it is missing nothing can run. This daemon is HELD, not quarantined: the supervisor re-checks every %s and starts it automatically as soon as the file exists again. No manual step is needed after reinstalling.",
			path, formatQuarantineWindow(missingPathHoldDelay))
	case missingWorkspaceReasonID:
		return fmt.Sprintf(
			"the workspace folder %s no longer exists — restore the folder, or remove this server from mcphub if the project is gone. This daemon is HELD, not quarantined: the supervisor re-checks every %s and starts it automatically as soon as the folder exists again.",
			path, formatQuarantineWindow(missingPathHoldDelay))
	case unavailableBinaryReasonID:
		return fmt.Sprintf(
			"the drive holding the mcphub program file is not available right now, so %s cannot be reached — reconnect that drive or network location. Nothing is wrong with the installation itself and reinstalling will not help. This daemon is HELD, not quarantined: the supervisor re-checks every %s and starts it automatically as soon as the drive is back. If that drive is not coming back, reinstall mcphub to a local folder.",
			path, formatQuarantineWindow(missingPathHoldDelay))
	case unavailableWorkspaceReasonID:
		return fmt.Sprintf(
			"the drive holding this server's project folder is not available right now, so %s cannot be reached — reconnect that drive or network location. This daemon is HELD, not quarantined: the supervisor re-checks every %s and starts it automatically as soon as the drive is back.",
			path, formatQuarantineWindow(missingPathHoldDelay))
	default:
		return fmt.Sprintf("a path this daemon requires is missing: %s", path)
	}
}

// preSpawnMissingPathHold is the pre-spawn existence gate. It returns nil to
// let the caller PROCEED to spawn (nothing definitively absent — the healthy
// path and every ambiguous path), or errSpawnHeldMissingPath after HOLDING the
// daemon in backoff with NO crash increment.
//
// It is called on EVERY create-process transition, including
// EvChildExit-at-StExiting: unlike the port gate (whose EvChildExit exclusion
// exists because our own dying child owns the port), whether the binary exists
// is independent of which event drove the spawn.
func (c *supervisorController) preSpawnMissingPathHold(d *api.SupervisorDaemon, ev api.LoopEvent) error {
	if c == nil || d == nil {
		return nil
	}
	stat := c.spawnPathStatFn // nil in production; classifySpawnPath falls back to os.Stat
	// Order matters: the binary is probed first because on this product every
	// daemon IS the mcphub binary, so a missing binary is the fleet-wide case
	// and the one the operator must be told about first.
	probes := [...]struct {
		missingID     string
		unavailableID string
		path          string
	}{
		{missingBinaryReasonID, unavailableBinaryReasonID, d.Command},
		{missingWorkspaceReasonID, unavailableWorkspaceReasonID, d.Workspace},
	}
	for _, p := range probes {
		var reasonID string
		switch classifySpawnPath(stat, p.path) {
		case spawnPathAbsent:
			reasonID = p.missingID
		case spawnPathUnavailable:
			reasonID = p.unavailableID
		default:
			continue // present, or ambiguous → fail open
		}
		c.recordSpawnHold(d, reasonID, p.path)
		return c.holdSpawnInBackoff(d, ev, missingPathHoldDelay, errSpawnHeldMissingPath)
	}
	// Nothing absent. Clear any stale hold marker so a recovered daemon stops
	// reporting a missing path and the NEXT absence emits immediately rather
	// than being swallowed by the dedupe window.
	c.clearSpawnHold(d.TaskName)
	return nil
}

// recordSpawnHold marks the tracker (so the hold reaches supervisor-state.json,
// the IPC status reply, `mcphub status --json` and the GUI Dashboard) and emits
// a bounded event-log row.
//
// The tracker mark happens BEFORE holdSpawnInBackoff so that call's MarkBackoff
// + persist writes both the backoff state and the hold reason in one pass.
func (c *supervisorController) recordSpawnHold(d *api.SupervisorDaemon, reasonID, path string) {
	if c.tracker != nil {
		c.tracker.MarkSpawnHold(d.TaskName, reasonID, path)
	}
	if c.events == nil {
		return
	}
	emit, severity, heldFor, suppressed := c.spawnHoldEmitDecision(d.TaskName, reasonID, path, time.Now().UTC())
	if !emit {
		return
	}
	body := map[string]any{
		"reason_id": reasonID,
		"path":      path,
		"action":    spawnHoldOperatorMessage(reasonID, path),
		"workspace": d.Workspace,
		"command":   d.Command,
		// held_for_seconds is the escalation signal an operator greps for: a
		// hold that survives an upgrade window is a real missing file.
		"held_for_seconds": int(heldFor / time.Second),
		// The crash budget is deliberately untouched; state this in the row so
		// nobody reading the log mistakes a hold for a crash.
		"crash_budget_consumed": false,
	}
	if suppressed > 0 {
		body["suppressed_rows"] = suppressed
	}
	_ = c.events.Emit(api.SupervisorEvent{
		Severity: severity,
		Source:   "lifecycle",
		Event:    "daemon-spawn-held-missing-path",
		TaskName: d.TaskName,
		Body:     body,
	})
}

// spawnHoldEmitDecision bounds the event stream for a held daemon. The FIRST
// observation of a (reason, path) tuple always emits at warn; afterwards a
// rollup emits at most once per missingPathEscalateAfter, at error severity,
// carrying how long the hold has lasted and how many ticks were suppressed.
func (c *supervisorController) spawnHoldEmitDecision(taskName, reasonID, path string, now time.Time) (emit bool, severity string, heldFor time.Duration, suppressed int) {
	if c.spawnHolds == nil {
		// Un-wired controller (direct construction in a test): emit every time
		// rather than silently swallowing rows.
		return true, "warn", 0, 0
	}
	key := canonicalSupervisorTaskName(taskName)
	c.spawnHolds.mu.Lock()
	defer c.spawnHolds.mu.Unlock()
	prev, ok := c.spawnHolds.m[key]
	if !ok || prev.reasonID != reasonID || prev.path != path {
		// New hold, or the cause changed: always tell the operator.
		c.spawnHolds.m[key] = &spawnHoldMarker{reasonID: reasonID, path: path, firstAt: now, lastEmit: now}
		return true, "warn", 0, 0
	}
	held := now.Sub(prev.firstAt)
	if now.Sub(prev.lastEmit) < missingPathEscalateAfter {
		prev.suppdRows++
		return false, "", held, prev.suppdRows
	}
	suppressed = prev.suppdRows
	prev.suppdRows = 0
	prev.lastEmit = now
	return true, "error", held, suppressed
}

// clearSpawnHold drops the hold marker for a task. Called on every gate pass
// (the path came back) so recovery is immediate and complete: the tracker field
// stops reporting a missing path, and the dedupe entry is removed so a future
// absence emits its first row at once instead of inheriting a stale window.
func (c *supervisorController) clearSpawnHold(taskName string) {
	if c == nil {
		return
	}
	if c.spawnHolds != nil {
		key := canonicalSupervisorTaskName(taskName)
		c.spawnHolds.mu.Lock()
		_, had := c.spawnHolds.m[key]
		delete(c.spawnHolds.m, key)
		c.spawnHolds.mu.Unlock()
		_ = had
	}
	if c.tracker == nil {
		return
	}
	// Only persist when something actually changed: the gate runs on every
	// create-process transition, and an unconditional write would add a
	// state-file write per spawn for every healthy daemon.
	if c.tracker.ClearSpawnHold(taskName) && c.statePath != "" {
		_ = persistDaemonRuntimeTracker(c.events, c.tracker, c.statePath, taskName)
	}
}

// FleetWideSpawnHold describes the case where EVERY held daemon is held for the
// SAME missing path. In the incident all 12 daemons failed on one absent
// mcphub.exe; twelve identical red cards saying "binary missing" is worse for
// an operator than one clear statement that the mcphub installation itself is
// broken. Consumers (the GUI Dashboard banner, `mcphub status`) derive this
// from the per-daemon rows rather than from a second backend surface.
type FleetWideSpawnHold struct {
	ReasonID string
	Path     string
	Count    int
	Message  string
}

// DeriveFleetWideSpawnHold returns the fleet-wide hold when at least two
// daemons are held AND every held daemon shares one (reason, path). It returns
// nil otherwise — a single held daemon is adequately described by its own row,
// and a mixed set has no single headline.
func DeriveFleetWideSpawnHold(rows []api.DaemonStatus) *FleetWideSpawnHold {
	var reason, path string
	count := 0
	for _, r := range rows {
		if r.SpawnHoldReason == "" {
			continue
		}
		if count == 0 {
			reason, path = r.SpawnHoldReason, r.SpawnHoldPath
		} else if r.SpawnHoldReason != reason || r.SpawnHoldPath != path {
			return nil // mixed causes: no single headline
		}
		count++
	}
	if count < 2 {
		return nil
	}
	return &FleetWideSpawnHold{
		ReasonID: reason,
		Path:     path,
		Count:    count,
		Message:  spawnHoldOperatorMessage(reason, path),
	}
}
