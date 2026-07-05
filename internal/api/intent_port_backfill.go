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
// operator-run migration. It RAISES a 0 to the manifest port and, when an older
// intent shape left the identity fields blank, heals Server/Daemon from the row's
// own `daemon --server/--daemon` args (so the squatter classifier + recover engage);
// it never rewrites a non-zero port, a populated identity, Command/Args, Env, a
// runtime_spec descriptor, OR ANY serena descriptor. The serena exemption is by
// server identity, not just RuntimeSpec: a legacy-unified serena row is
// RuntimeSpec==nil yet must stay untouched (stamping its manifest port 9121 would
// activate the liveness port-check with the wrong 60s bind deadline — serena
// lifecycle is owned by serena-migrate/build, never F5). See the inline
// `server == SerenaServerName` guard for the full rationale.
package api

import (
	"errors"
	"fmt"
	"os"

	"github.com/gofrs/flock"
)

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
		// workspace-hash daemon name would not resolve anyway. Skip it explicitly.
		if d.Port > 0 || d.RuntimeSpec != nil {
			continue
		}
		// Resolve the (server, daemon) identity — from the struct fields, or
		// recovered from the canonical `daemon --server/--daemon` args when an
		// older intent shape left the fields blank. A row that is NOT a manifest
		// daemon (a maintenance timer / one-shot such as workspace-weekly-refresh,
		// whose args carry no --server/--daemon) is portless BY DESIGN and skipped
		// SILENTLY — never a UnresolvedPortZero warn on every startup. Crucially
		// this does NOT skip a real daemon row just because its Server/Daemon
		// fields are blank: such a row still carries --server/--daemon in its args
		// and gets its port restored (bot PR #504).
		server, daemon, isManifestDaemon := DescriptorServerDaemon(*d)
		if !isManifestDaemon {
			continue
		}
		// NEVER touch a serena descriptor, regardless of shape. The RuntimeSpec!=nil
		// guard above catches a MIGRATED serena (dynamic-pool proxy row), but a
		// LEGACY-unified serena descriptor (`kind: global`, RuntimeSpec==nil, Port=0,
		// args `daemon --server serena --daemon unified`) slips past it — and its
		// serena manifest DOES resolve a real port (9121). Stamping that port turns on
		// the liveness port-check for serena, but the descriptor's args are NOT the
		// `daemon serena-proxy` shape supervisorStartupBindDeadline keys on for the
		// 120s serena deadline (supervise_liveness.go:520), so it would get the 60s
		// default — far too short for serena's cold uvx+SolidLSP start, driving a
		// daemon-bind-timeout restart cycle that Port=0 (port-check disabled) never
		// caused. serena lifecycle is owned by serena-migrate/build (which sets the
		// runtime_spec + 120s deadline), never by F5. Skip serena by server identity
		// so the "F5 never clobbers a serena descriptor" invariant actually holds.
		// Keyed on SerenaServerName (the canonical single-owner string, not a bare
		// literal) and on server IDENTITY (not the daemon name), so it covers every
		// legacy serena shape — the `unified` row AND the older workspace-hash-named
		// nil-RuntimeSpec rows (serena_intent_repair.go) — with no per-daemon case.
		if server == SerenaServerName {
			continue
		}
		port, deadlineSecs, ok := resolveManifestPortAndDeadlineFn(server, daemon)
		if !ok || port <= 0 {
			res.UnresolvedPortZero = append(res.UnresolvedPortZero, d.TaskName)
			continue
		}
		d.Port = port
		// Heal the identity fields too when they were blank (recovered from args
		// above). A row left `Port>0, Server="", Daemon=""` is a state install never
		// produces, and it DEFEATS the very protection F5 restores: the squatter
		// classifier's argv gate (supervise_squatter.go:218) and `mcphub daemon
		// recover` both require Server/Daemon != "" — a genuine own-child would be
		// misclassified foreign and never reaped. The values came from THIS row's
		// own `daemon --server/--daemon` args (what the spawn itself uses), so they
		// cannot be wrong; when the fields were already populated this is a no-op.
		d.Server = server
		d.Daemon = daemon
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
			Server:           server,
			Daemon:           daemon,
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
