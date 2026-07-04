// Package api — supervisor-side legacy-descriptor port self-heal primitive.
//
// BackfillIntentDaemonPorts fixes a residual class the supervisor lost-child
// audit (2026-07-04) confirmed: supervisor-intent.json descriptors written by a
// pre-port-field install carry Port=0 for a daemon whose server manifest
// declares a real port (e.g. memory@9123, time@9128, gdb@9129). Port=0
// STRUCTURALLY disables every port-based protection the supervisor owns:
//
//   - the liveness sweep's port-bound check (supervise_liveness.go: `d.Port<=0`
//     is an early return "healthy"),
//   - the P1b first-bind deadline,
//   - the P2a `port_owner_mismatch` sweep-reap of a lost-child squatter,
//   - the reap inside `mcphub daemon recover` (daemon_recover.go: `desc.Port<=0`
//     is a silent skip).
//
// The install fan-out already stamps Port from the manifest
// (supervisorDaemonsFromPlan → `Port: d.Port`); only LEGACY rows written before
// the field existed carry 0, and the install merge does not backfill them. This
// primitive is the supervisor's own startup self-heal: the SUPERVISOR calls it
// ONCE at startup, BEFORE it loads the intent for its first reconcile pass, so
// every existing host is repaired on its next supervisor restart WITHOUT an
// operator-run migration. It only ever RAISES a 0 to the manifest port; it never
// rewrites a non-zero port, a Command/Args/Env, or a runtime_spec descriptor.
package api

import (
	"errors"
	"fmt"
	"os"

	"github.com/gofrs/flock"
)

// resolveManifestPortAndDeadlineFn resolves (port, startupBindDeadlineSeconds,
// ok) for a (server, daemon) pair from the embed-first manifest store. Package
// var so tests inject a hermetic resolver without seeding the embedded manifest
// FS — mirrors the resolveManifestDaemonPortFn seam in
// internal/cli/supervise_status.go.
var resolveManifestPortAndDeadlineFn = resolveManifestPortAndDeadline

// resolveManifestPortAndDeadline reads the (embed-first) server manifest and
// returns the named daemon's Port + StartupBindDeadlineSeconds. Returns
// (0, 0, false) on any error (missing manifest, daemon-name mismatch) so the
// caller treats a non-ok result as "not authoritative — leave the descriptor
// untouched" rather than clobbering it with 0. Reuses the canonical findDaemon
// (install.go) so the manifest daemon-name match is single-owned.
func resolveManifestPortAndDeadline(server, daemon string) (port int, deadlineSecs int, ok bool) {
	m, err := loadManifestForServer("", server)
	if err != nil || m == nil {
		return 0, 0, false
	}
	d, found := findDaemon(m, daemon)
	if !found {
		return 0, 0, false
	}
	return d.Port, d.StartupBindDeadlineSeconds, true
}

// IntentPortBackfill is the structured outcome of BackfillIntentDaemonPorts,
// returned so the CLI caller (which owns the supervisor-events.log handle) emits
// the audit events without the api package taking an I/O dependency on the log.
type IntentPortBackfill struct {
	// Applied is one entry per descriptor whose Port (and, when the manifest
	// declares one, StartupBindDeadlineSeconds) was backfilled from the manifest.
	Applied []IntentPortBackfillRow
	// UnresolvedPortZero lists task names whose descriptor Port is 0 but whose
	// manifest did not resolve a port>0 (missing manifest, daemon-name mismatch,
	// or a manifest that also declares port 0). Left untouched; surfaced as a
	// warn so a daemon stuck with every port protection disabled is visible
	// instead of silently unfixable.
	UnresolvedPortZero []string
	// Contended is true when the intent flock was held by another writer; the
	// backfill was skipped (non-fatal) and retries on the next supervisor start.
	Contended bool
}

// IntentPortBackfillRow describes a single backfilled descriptor.
type IntentPortBackfillRow struct {
	TaskName         string
	Server           string
	Daemon           string
	Port             int
	BindDeadlineSecs int
}

// BackfillIntentDaemonPorts performs the locked read-modify-write described in
// the package comment. It is idempotent (a second run finds every port already
// non-zero and writes nothing) and non-fatal by contract: the caller must treat
// a returned error as a warn, never a startup abort — the daemons still run,
// they just keep the pre-backfill port=0 protection gap until a clean run.
//
// stateDir is the supervisor's already-resolved state root, threaded in by the
// caller (NOT re-resolved here) — the same discipline RepairSerenaIntentFromRegistry
// uses so the backfilled file is the exact one loadIntentFiles reads next.
func BackfillIntentDaemonPorts(stateDir string) (IntentPortBackfill, error) {
	var res IntentPortBackfill
	if stateDir == "" {
		return res, fmt.Errorf("intent port backfill: empty state dir (caller must thread the supervisor's resolved state root)")
	}
	intentPath := joinStateFilePath(stateDir, supervisorIntentFileLeaf)

	// Intent flock, non-blocking. A hung intent writer must not stall supervisor
	// startup; skip on contention — the holder commits its own intent and the
	// next startup re-scans. Mirrors RepairSerenaIntentFromRegistry step 3.
	intentLock := flock.New(intentPath + supervisorIntentLockSuffix)
	locked, err := intentLock.TryLock()
	if err != nil {
		return res, fmt.Errorf("intent port backfill: try-lock %s: %w", intentPath+supervisorIntentLockSuffix, err)
	}
	if !locked {
		res.Contended = true
		return res, nil
	}
	defer func() { _ = intentLock.Unlock() }()

	intent, rerr := ReadSupervisorIntent(intentPath)
	if rerr != nil {
		if errors.Is(rerr, os.ErrNotExist) {
			return res, nil // no intent file yet — nothing to backfill
		}
		return res, fmt.Errorf("intent port backfill: read %s: %w", intentPath, rerr)
	}
	if intent == nil || len(intent.Daemons) == 0 {
		return res, nil
	}

	changed := false
	for i := range intent.Daemons {
		d := &intent.Daemons[i]
		// Only legacy/global daemons with a missing port. A runtime_spec daemon
		// (serena workspace proxy) carries its port inside the spec and the install
		// fan-out already stamps its descriptor Port; its manifest is a dynamic-pool
		// TEMPLATE with no per-workspace port, so a lookup keyed on the
		// workspace-hash daemon name would not resolve anyway. Skip it explicitly so
		// F5 can never clobber a serena descriptor.
		if d.Port > 0 || d.RuntimeSpec != nil {
			continue
		}
		// Maintenance-timer / non-manifest descriptors (e.g. the
		// workspace-weekly-refresh row, args ["workspace-weekly-refresh"]) carry
		// empty Server/Daemon and are portless BY DESIGN — they can never match a
		// manifest daemon. Skip them SILENTLY (no UnresolvedPortZero, no warn):
		// otherwise F5 would emit an "every port-based protection stays disabled"
		// warn for a non-listening timer on every supervisor startup, forever.
		if d.Server == "" || d.Daemon == "" {
			continue
		}
		port, deadlineSecs, ok := resolveManifestPortAndDeadlineFn(d.Server, d.Daemon)
		if !ok || port <= 0 {
			res.UnresolvedPortZero = append(res.UnresolvedPortZero, d.TaskName)
			continue
		}
		d.Port = port
		// Match the install fan-out (supervisorDaemonsFromPlan carries both fields):
		// stamp the manifest's first-bind deadline too, so the backfilled row is
		// byte-identical to what a fresh install would write. Additive — a manifest
		// deadline of 0 leaves the descriptor's 0 (default resolution) unchanged.
		if deadlineSecs > 0 {
			d.StartupBindDeadlineSeconds = deadlineSecs
		}
		changed = true
		res.Applied = append(res.Applied, IntentPortBackfillRow{
			TaskName:         d.TaskName,
			Server:           d.Server,
			Daemon:           d.Daemon,
			Port:             d.Port,
			BindDeadlineSecs: d.StartupBindDeadlineSeconds,
		})
	}

	if !changed {
		return res, nil
	}
	// Write under the held intent flock (writeSupervisorIntentLockHeld assumes the
	// lock at intentPath+".lock" is held, which it is). This rewrites the whole
	// SupervisorIntentFile, so it preserves Stops / maintenance timers / every
	// other row read above verbatim.
	if werr := writeSupervisorIntentLockHeld(intentPath, intent); werr != nil {
		// The write did NOT land — the in-memory mutations were never persisted, so
		// Applied would be a lie to the caller. Zero it (and UnresolvedPortZero) so a
		// caller that emits events on a non-nil-error return can never announce a
		// backfill that did not commit. The current caller discards res on error;
		// this keeps the contract honest for any future one.
		res.Applied = nil
		res.UnresolvedPortZero = nil
		return res, fmt.Errorf("intent port backfill: write %s: %w", intentPath, werr)
	}
	return res, nil
}
