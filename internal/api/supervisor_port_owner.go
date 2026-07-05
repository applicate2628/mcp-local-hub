// Package api — the single owner of "what port + first-bind deadline does this
// supervisor daemon descriptor use". Every port-DECISION path (liveness sweep,
// P1b deadline, squatter classifier, `mcphub daemon recover`, startup
// running-scan, status display) resolves through this file, so a Port=0 legacy
// descriptor no longer STRUCTURALLY disables port protections — they resolve the
// manifest port lazily instead. `SupervisorDaemon.Port` STAYS a persisted
// spawn-cache (the spawn contract, the sole source for runtime_spec rows, and a
// reconcile drift-detection input); only ownership of the DECISION moves here.
//
// This replaced two pre-existing manifest resolvers (a startup write-convergence
// pass and supervise_status.go's private read-fallback memo) and the accreted
// deadline special-cases (a server==serena backfill skip and an argv-keyed
// serena-proxy deadline arm), all now deleted — see
// work-items/decisions/2026-07-05-daemon-port-resolution-single-owner.md.
package api

// Port/deadline defaults, homed here as the SINGLE owner of the magic values
// (design §4b): the resolver is the sole authority, so the deadline no longer
// lives as a cli-package constant + an argv-keyed liveness arm.
const (
	// defaultStartupBindDeadlineSeconds bounds a freshly-spawned daemon's first
	// port bind for a global/legacy daemon that declares no explicit deadline.
	defaultStartupBindDeadlineSeconds = 60
	// serenaStartupBindDeadlineSeconds is the longer first-bind deadline for
	// serena (uvx + SolidLSP cold start routinely exceeds 60s). Keyed on SERVER
	// IDENTITY (§4b), so it covers BOTH the legacy-unified `unified` daemon AND
	// the dynamic-pool proxy rows whose Daemon is a per-workspace hash — a
	// manifest-daemon-name lookup misses the hash rows, so identity is the only
	// key that covers both shapes with one rule.
	serenaStartupBindDeadlineSeconds = 120
)

// manifestPortDeadlineResolveFn resolves (port, deadlineSecs, ok) for a
// (server, daemon) pair. The pure entry points pass the direct embed-first
// reader; DaemonPortResolver passes its per-instance memo. Threading it as a
// parameter keeps ONE decision logic (effectivePort / effectiveDeadline) shared
// by the pure and memoized callers.
type manifestPortDeadlineResolveFn func(server, daemon string) (port int, deadlineSecs int, ok bool)

// EffectiveDaemonPort returns the port a descriptor should be checked/protected
// on: d.Port when it is already stamped (>0), else the manifest-declared port
// for the descriptor's resolved (server, daemon). ok=false when neither yields a
// port>0 (a portless maintenance-timer row, a field/argv-mismatched descriptor,
// or a renamed/removed manifest) — the caller then treats the daemon as having
// no port protection rather than fabricating one.
func EffectiveDaemonPort(d SupervisorDaemon) (port int, ok bool) {
	return effectivePort(d, resolveManifestPortAndDeadlineFn)
}

// EffectiveStartupBindDeadlineSeconds returns the first-bind deadline in seconds:
// an explicit descriptor field (>0) wins; else the manifest-declared deadline
// (>0); else serena by SERVER IDENTITY gets serenaStartupBindDeadlineSeconds
// (§4b — covers unified + workspace-hash proxy shapes); else the global default.
// Deliberately INDEPENDENT of the port short-circuit: a port-stamped,
// deadline-zero row still resolves its manifest/identity deadline.
func EffectiveStartupBindDeadlineSeconds(d SupervisorDaemon) int {
	return effectiveDeadline(d, resolveManifestPortAndDeadlineFn)
}

// effectivePort is the shared port-decision logic (pure + memoized callers).
func effectivePort(d SupervisorDaemon, resolve manifestPortDeadlineResolveFn) (int, bool) {
	if d.Port > 0 {
		return d.Port, true
	}
	server, daemon, idOK := DescriptorServerDaemon(d)
	if !idOK {
		return 0, false
	}
	port, _, ok := resolve(server, daemon)
	if !ok || port <= 0 {
		return 0, false
	}
	return port, true
}

// effectiveDeadline is the shared deadline-decision logic (§4b).
func effectiveDeadline(d SupervisorDaemon, resolve manifestPortDeadlineResolveFn) int {
	if d.StartupBindDeadlineSeconds > 0 {
		return d.StartupBindDeadlineSeconds
	}
	server, daemon, idOK := DescriptorServerDaemon(d)
	if idOK {
		if _, deadlineSecs, ok := resolve(server, daemon); ok && deadlineSecs > 0 {
			return deadlineSecs
		}
		// §4b: serena by server identity, so the workspace-hash proxy rows
		// (whose Daemon name misses the manifest) still get 120s, not 60s.
		if server == SerenaServerName {
			return serenaStartupBindDeadlineSeconds
		}
	}
	return defaultStartupBindDeadlineSeconds
}

// DaemonPortResolver memoizes the manifest read so a hot loop (the liveness
// sweep, the status refresh) resolves each (server, daemon) at most ONCE per
// instance — a single Resolve returns both the port and the deadline from one
// cached read, and a repeated Resolve of the same descriptor reuses it (N
// DISTINCT daemons of one server still cost N reads, but never a duplicate for
// the same pair or the intra-Resolve port+deadline double-read). It is a direct
// generalization of supervise_status.go's private newManifestPortResolver,
// extended to carry the deadline alongside the port and to short-circuit on
// d.Port. NOT safe for concurrent use — construct one per sweep/refresh on the
// owning goroutine.
type DaemonPortResolver struct {
	// memo[server][daemon] → the resolved (port, deadline, ok) triple. The
	// server-level map's mere presence records that the manifest was consulted.
	memo map[string]map[string]resolverEntry
}

type resolverEntry struct {
	port         int
	deadlineSecs int
	ok           bool
}

// NewDaemonPortResolver returns a fresh per-pass memoizing resolver.
func NewDaemonPortResolver() *DaemonPortResolver {
	return &DaemonPortResolver{memo: map[string]map[string]resolverEntry{}}
}

// Resolve returns the effective (port, deadlineSecs, portOK) for a descriptor,
// byte-identical to the pure EffectiveDaemonPort / EffectiveStartupBindDeadlineSeconds
// but reusing one manifest parse per server per instance.
func (r *DaemonPortResolver) Resolve(d SupervisorDaemon) (port int, deadlineSecs int, portOK bool) {
	port, portOK = effectivePort(d, r.memoResolve)
	deadlineSecs = effectiveDeadline(d, r.memoResolve)
	return port, deadlineSecs, portOK
}

// memoResolve is the resolver's cached manifest lookup with the same signature
// as resolveManifestPortAndDeadlineFn.
func (r *DaemonPortResolver) memoResolve(server, daemon string) (int, int, bool) {
	byDaemon, seenServer := r.memo[server]
	if seenServer {
		if e, ok := byDaemon[daemon]; ok {
			return e.port, e.deadlineSecs, e.ok
		}
	} else {
		byDaemon = map[string]resolverEntry{}
		r.memo[server] = byDaemon
	}
	port, deadlineSecs, ok := resolveManifestPortAndDeadlineFn(server, daemon)
	byDaemon[daemon] = resolverEntry{port: port, deadlineSecs: deadlineSecs, ok: ok}
	return port, deadlineSecs, ok
}

// resolveManifestPortAndDeadlineFn resolves (port, startupBindDeadlineSeconds,
// ok) for a (server, daemon) pair from the embed-first manifest store. Package
// var so tests inject a hermetic resolver without seeding the embedded manifest
// FS. MOVED here from intent_port_backfill.go (design Phase 1) so the owner
// package holds the one manifest port+deadline reader.
var resolveManifestPortAndDeadlineFn = resolveManifestPortAndDeadline

// resolveManifestPortAndDeadline reads the (embed-first) server manifest and
// returns the named daemon's Port + StartupBindDeadlineSeconds. Returns
// (0, 0, false) on any error (missing manifest, daemon-name mismatch) so the
// caller treats a non-ok result as "not authoritative — no port protection"
// rather than clobbering it with 0. Reuses the canonical findDaemon (install.go)
// so the manifest daemon-name match is single-owned.
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

// DescriptorServerDaemon resolves the (server, daemon) identity of a supervisor
// descriptor and reports whether it is a manifest-backed `mcphub daemon` row at
// all. It is the SINGLE owner of "what (server, daemon) is this descriptor" —
// every port-decision path resolves identity through it, so a blank-field legacy
// row (Server=="" but args carry `--server X --daemon Y`) is classified
// identically everywhere with no duplicated argv parsing to drift. MOVED here
// from intent_port_backfill.go (design Phase 1).
//
// Identity resolution (fail-closed on a corrupt descriptor):
//   - both struct fields present AND agree with the argv tokens → return them;
//   - a field is blank → recover it from the canonical daemon args
//     ["daemon", "--server", <s>, "--daemon", <d>] the install fan-out writes;
//   - a field is present but DISAGREES with its `--server`/`--daemon` argv token
//     → return ok=false. The argv is the launch-truth (the process spawns from
//     d.Args), so a struct field that contradicts it is a corrupt cache; refuse
//     to resolve a mixed identity rather than stamp a port the process never
//     binds (deep-sec follow-up, PR #504).
//
// Args are the authoritative fallback, NOT the task name: server names contain
// hyphens (paper-search-mcp, sequential-thinking), so splitting a
// "\mcp-local-hub-<server>-<daemon>" task name is ambiguous, whereas --server /
// --daemon are unambiguous tokens. A row that is neither field-populated nor
// daemon-arg-shaped (a maintenance timer / one-shot such as
// workspace-weekly-refresh, args ["workspace-weekly-refresh"]) returns ok=false —
// it is portless by design and must be skipped, not reported unresolvable.
func DescriptorServerDaemon(d SupervisorDaemon) (server, daemon string, ok bool) {
	fieldServer, fieldDaemon := d.Server, d.Daemon

	var argServer, argDaemon string
	if len(d.Args) > 0 && d.Args[0] == "daemon" {
		argVal := func(flag string) string {
			for i := 0; i+1 < len(d.Args); i++ {
				if d.Args[i] == flag {
					return d.Args[i+1]
				}
			}
			return ""
		}
		argServer = argVal("--server")
		argDaemon = argVal("--daemon")
	}

	// Fail-closed mismatch: a populated struct field that DISAGREES with its
	// argv token is a corrupt cache — refuse (ok=false) rather than resolve a
	// mixed identity. A blank field or an absent argv token is not a mismatch.
	if fieldServer != "" && argServer != "" && fieldServer != argServer {
		return "", "", false
	}
	if fieldDaemon != "" && argDaemon != "" && fieldDaemon != argDaemon {
		return "", "", false
	}

	server = fieldServer
	if server == "" {
		server = argServer
	}
	daemon = fieldDaemon
	if daemon == "" {
		daemon = argDaemon
	}
	if server == "" || daemon == "" {
		return "", "", false
	}
	return server, daemon, true
}
