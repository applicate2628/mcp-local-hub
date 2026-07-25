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
	"slices"
	"strconv"
)

// BuiltinRouteTaskName is the reserved, canonical (leading-backslash)
// supervisor-intent task name for the built-in `mcphub route` front daemon.
// Canonical form matches every other SupervisorDaemon.TaskName in
// supervisor-intent.json (see the struct's own doc comment).
const BuiltinRouteTaskName = `\mcp-local-hub-route-front`

// BuiltinRouteServer and BuiltinRouteDaemonName are the reserved (Server,
// Daemon) identity pair stamped on the built-in route daemon's descriptor.
// "route" is not a manifest/catalog server name (mechanically re-verified by
// TestBuiltinRouteDaemon_ReservedServerNameNotClaimedByAnyShippedManifest in
// internal/api/builtin_route_daemon_test.go): reserving it here means
// buildMergedSupervisorIntent's per-server ownership scan (which only claims
// rows matching a REAL installed server name, or a blank-Server row whose
// task name carries that server's prefix) never claims or drops this row
// during any OTHER server's install/uninstall.
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

// EnsureBuiltinRouteDaemon UPSERTs the built-in route daemon row into
// f.Daemons, keyed by the reserved BuiltinRouteTaskName:
//
//   - absent -> appends the canonical row, returns changed=true ("added").
//   - present but its Command/Args/Port drifted from the canonical shape
//     (e.g. a binary relocation, or a future DefaultRouteDaemonPort bump) ->
//     REPLACES the row wholesale with the canonical one, returns
//     changed=true ("replaced").
//   - present and already canonical -> leaves f untouched, returns
//     changed=false ("unchanged").
//
// f==nil is a no-op (changed=false) — callers only ever pass an already-
// loaded/cloned *SupervisorIntentFile.
//
// This function only edits the in-memory file; it does not persist
// anything. The caller (internal/cli/supervise.go, after loadIntentFiles)
// is responsible for writing the result through the same flocked
// read-modify-write path every other supervisor-intent mutation uses
// (MutateSupervisorIntentIfChanged) — an in-memory-only add would be
// dropped by the next IntentWatcher re-read.
func EnsureBuiltinRouteDaemon(f *SupervisorIntentFile, command string, port int) bool {
	if f == nil {
		return false
	}
	want := BuildBuiltinRouteDaemon(command, port)
	for i := range f.Daemons {
		if canonicalIntentTaskKey(f.Daemons[i].TaskName) != BuiltinRouteTaskName {
			continue
		}
		existing := f.Daemons[i]
		if existing.Command == want.Command && existing.Port == want.Port && slices.Equal(existing.Args, want.Args) {
			return false
		}
		f.Daemons[i] = want
		return true
	}
	f.Daemons = append(f.Daemons, want)
	return true
}
