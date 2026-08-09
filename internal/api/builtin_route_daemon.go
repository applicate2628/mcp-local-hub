// internal/api/builtin_route_daemon.go
//
// Increment 1b of the MCP front-daemon decision
// (work-items/decisions/2026-07-25-mcp-data-plane-off-gui-onto-supervised-
// front-daemon.md) + work-items/decisions/2026-07-25-supervisor-builtin-
// singleton-daemon.md: the supervisor auto-spawns and manages the already-
// built `mcphub route` front daemon (internal/cli/route.go) as an ordinary
// SupervisorDaemon descriptor.
//
// `mcphub route` needs ZERO new spawn machinery: it already has the exact
// shape (Command+Args+Port, uniform restart) reconcile expects of any
// global daemon, and its argv (`route --port <N>`, Args[0] == "route") does
// not match either of the two argv-only proxy-exclusion predicates
// (IsSerenaProxyDescriptor / IsWorkspaceLSPProxyDescriptor, both require
// Args[0] == "daemon" — supervisor_port_owner.go), so it spawns through the
// unmodified reconcile decision logic and the unmodified production spawn
// closure. The ONLY missing piece is durability: an in-memory-only row is
// dropped by the next IntentWatcher re-read (supervisor_controller.go's 60s
// intentCache swap only keeps what's on disk), so this file's job is to
// PERSIST the reserved row — nothing here touches spawn, reconcile, restart,
// or env composition.
//
// Deliberately pure + no internal/cli import: the port is resolved by the
// caller (internal/cli's DefaultRouteDaemonPort stays the single owner) and
// injected down as a parameter, per the repo's config-injected-from-the-top
// convention.
package api

import (
	"fmt"
	"reflect"
	"strconv"
)

// BuiltinRouteTaskName is the reserved, canonical (leading-backslash)
// supervisor-intent task name for the built-in `mcphub route` front daemon.
// Canonical form matches every other SupervisorDaemon.TaskName in
// supervisor-intent.json (see the struct's own doc comment).
const BuiltinRouteTaskName = `\mcp-local-hub-route-front`

// BuiltinRouteServer and BuiltinRouteDaemonName are the reserved (Server,
// Daemon) identity pair stamped on the built-in route daemon's descriptor.
// "route" is not a manifest/catalog server name, on BOTH halves of the
// namespace: no SHIPPED manifest claims it (mechanically re-verified by
// TestBuiltinRouteDaemon_ReservedServerNameNotClaimedByAnyShippedManifest in
// internal/api/builtin_route_daemon_test.go), and checkManifestName
// (internal/api/manifest.go) REJECTS it, so no operator/dev manifest can
// claim it either. Reserving it means buildMergedSupervisorIntent's
// per-server ownership scan (which only claims rows matching a REAL installed
// server name, or a blank-Server row whose task name carries that server's
// prefix) never claims or drops this row during any OTHER server's
// install/uninstall.
const (
	BuiltinRouteServer     = "route"
	BuiltinRouteDaemonName = "front"
)

// BuildBuiltinRouteDaemon returns the canonical SupervisorDaemon descriptor
// for the `mcphub route` front daemon. command is the supervisor's own
// resolved binary path (internal/cli.canonicalMcphubPath() — the same
// binary re-execs itself as `mcphub route`, exactly like every other
// supervisor-spawned `mcphub daemon ...` descriptor); port is
// cli.DefaultRouteDaemonPort. RuntimeSpec is deliberately nil: the route
// daemon is a global (non-workspace-scoped) descriptor spawned via the
// generic `mcphub route --port <N>` argv, not a serena/LSP proxy that needs
// a materialized runtime spec.
func BuildBuiltinRouteDaemon(command string, port int) SupervisorDaemon {
	return SupervisorDaemon{
		TaskName: BuiltinRouteTaskName,
		Server:   BuiltinRouteServer,
		Daemon:   BuiltinRouteDaemonName,
		Command:  command,
		Args:     []string{"route", "--port", strconv.Itoa(port)},
		Port:     port,
	}
}

// ErrBuiltinRouteTaskNameCollision is returned by EnsureBuiltinRouteDaemon
// when an existing supervisor-intent row carries the reserved
// BuiltinRouteTaskName canonical key but its Server/Daemon identity does NOT
// match the reserved (BuiltinRouteServer, BuiltinRouteDaemonName) pair (P2-4,
// adversarial review of Increment 1). Such a row was never written by this
// seeder — it is a foreign row squatting on the reserved name (an operator
// hand-edit, a stale migration artifact, or a bug elsewhere) — and must never
// be silently overwritten or silently treated as "already canonical".
var ErrBuiltinRouteTaskNameCollision = fmt.Errorf("supervisor-intent row collides with the reserved built-in route daemon task name %q but its server/daemon identity does not match the reserved (%q, %q) pair; refusing to overwrite a foreign row", BuiltinRouteTaskName, BuiltinRouteServer, BuiltinRouteDaemonName)

// EnsureBuiltinRouteDaemon UPSERTs the built-in route daemon row into
// f.Daemons, keyed by the reserved BuiltinRouteTaskName:
//
//   - absent -> appends the canonical row, returns changed=true ("added").
//   - present, carries the reserved (Server, Daemon) identity, but the rest
//     of the descriptor (Command/Args/Port/Env/Workspace/...) drifted from
//     the canonical shape (e.g. a binary relocation, or a future
//     DefaultRouteDaemonPort bump) -> REPLACES the row wholesale with the
//     canonical one, returns changed=true ("replaced").
//   - present, carries the reserved identity, and is ALREADY the complete
//     canonical descriptor -> leaves f untouched, returns changed=false
//     ("unchanged").
//   - present but its Server/Daemon does NOT match the reserved identity
//     (P2-4: a foreign row squatting on the reserved task name, detected
//     regardless of whether its Command/Args/Port happen to coincide with
//     the canonical shape) -> returns (false, ErrBuiltinRouteTaskNameCollision)
//     and leaves f completely untouched. The caller must surface this loudly
//     rather than silently clobbering or silently accepting the foreign row.
//   - two or more rows legitimately carry the reserved identity (should not
//     normally happen; a defensive collapse for a prior bug or hand-edit) ->
//     the FIRST is upserted to the canonical descriptor, every OTHER
//     canonical-key row is removed, returns changed=true.
//
// The "already canonical" check compares the COMPLETE descriptor (every
// field of SupervisorDaemon via reflect.DeepEqual), not just Command/Args/
// Port — comparing a subset of fields would silently accept drift in any
// field left out of the comparison (P2-4).
//
// f==nil is a no-op (changed=false, nil) — callers only ever pass an
// already-loaded/cloned *SupervisorIntentFile.
//
// This function only edits the in-memory file; it does not persist
// anything. The caller (internal/cli/supervise.go, after loadIntentFiles)
// is responsible for writing the result through the same flocked
// read-modify-write path every other supervisor-intent mutation uses
// (MutateSupervisorIntentIfChanged) — an in-memory-only add would be
// dropped by the next IntentWatcher re-read.
func EnsureBuiltinRouteDaemon(f *SupervisorIntentFile, command string, port int) (bool, error) {
	if f == nil {
		return false, nil
	}
	want := BuildBuiltinRouteDaemon(command, port)

	matchedIdx := -1
	duplicateIdxs := make([]int, 0, 1)
	for i := range f.Daemons {
		if canonicalIntentTaskKey(f.Daemons[i].TaskName) != BuiltinRouteTaskName {
			continue
		}
		existing := f.Daemons[i]
		if existing.Server != BuiltinRouteServer || existing.Daemon != BuiltinRouteDaemonName {
			// A foreign row squatting on the reserved task name — reject the
			// WHOLE operation loudly rather than overwrite it (wholesale
			// clobber) or accept it unchanged (under-canonicalization). f is
			// left completely untouched.
			return false, fmt.Errorf("%w (found server=%q daemon=%q command=%q)", ErrBuiltinRouteTaskNameCollision, existing.Server, existing.Daemon, existing.Command)
		}
		if matchedIdx == -1 {
			matchedIdx = i
			continue
		}
		duplicateIdxs = append(duplicateIdxs, i)
	}

	if matchedIdx == -1 {
		f.Daemons = append(f.Daemons, want)
		return true, nil
	}

	changed := len(duplicateIdxs) > 0
	if !reflect.DeepEqual(f.Daemons[matchedIdx], want) {
		f.Daemons[matchedIdx] = want
		changed = true
	}
	if len(duplicateIdxs) > 0 {
		drop := make(map[int]struct{}, len(duplicateIdxs))
		for _, idx := range duplicateIdxs {
			drop[idx] = struct{}{}
		}
		kept := make([]SupervisorDaemon, 0, len(f.Daemons)-len(drop))
		for i, d := range f.Daemons {
			if _, isDup := drop[i]; isDup {
				continue
			}
			kept = append(kept, d)
		}
		f.Daemons = kept
	}
	return changed, nil
}
