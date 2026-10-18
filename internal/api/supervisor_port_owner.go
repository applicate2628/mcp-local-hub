// Package api — the single owner of "what port + first-bind deadline does this
// supervisor daemon descriptor use". Every port-DECISION path (liveness sweep,
// P1b deadline, squatter classifier, `mcphub daemon recover`, startup
// running-scan, status display, serena-unregister pre-removal force-kill)
// resolves through this file, so a Port=0 legacy
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

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"mcp-local-hub/internal/config"
)

// SupervisorFrozenPort is a serialized original port operand. Its rows carry
// data only; capture completeness, envelope presence and lifecycle admission
// remain responsibilities of the consuming operation.
type SupervisorFrozenPort struct {
	TaskName   string `json:"task_name"`
	Port       int    `json:"port"`
	ArgvSHA256 string `json:"argv_sha256"`
}

var (
	ErrInvalidSupervisorFrozenPort    = errors.New("invalid supervisor frozen port")
	ErrUnsortedSupervisorFrozenPorts  = errors.New("unsorted or duplicate supervisor frozen port")
	errUnresolvedSupervisorDaemonPort = errors.New("daemon managed port or positive exclusion unresolved")
)

// CanonicalSupervisorFrozenPorts returns a nonnil owned copy in strict
// task/port/digest order. It neither validates rows nor proves capture completed.
func CanonicalSupervisorFrozenPorts(rows []SupervisorFrozenPort) []SupervisorFrozenPort {
	canonical := make([]SupervisorFrozenPort, len(rows))
	copy(canonical, rows)
	sort.Slice(canonical, func(i, j int) bool {
		return supervisorFrozenPortLess(canonical[i], canonical[j])
	})
	return canonical
}

func supervisorFrozenPortLess(left, right SupervisorFrozenPort) bool {
	if left.TaskName != right.TaskName {
		return left.TaskName < right.TaskName
	}
	if left.Port != right.Port {
		return left.Port < right.Port
	}
	return left.ArgvSHA256 < right.ArgvSHA256
}

// ValidateSupervisorFrozenPorts checks row values and strict canonical order.
// Nil and empty sets have no rows; the consuming codec owns their admission.
func ValidateSupervisorFrozenPorts(rows []SupervisorFrozenPort) error {
	for i, row := range rows {
		if strings.TrimSpace(row.TaskName) == "" || row.Port < 1 || row.Port > 65535 || !isLowerHex64(row.ArgvSHA256) {
			return ErrInvalidSupervisorFrozenPort
		}
		if i > 0 && !supervisorFrozenPortLess(rows[i-1], row) {
			return ErrUnsortedSupervisorFrozenPorts
		}
	}
	return nil
}

// CaptureSupervisorFrozenPorts computes an owned canonical cohort from one
// original intent snapshot, before the normal reader's legacy filtering.
// A nonnil empty result requires explicit daemons:[] or positive exclusions;
// unresolved input returns an error and no partial cohort. Source namespace and
// lifecycle admission remain the caller's responsibility.
func CaptureSupervisorFrozenPorts(intentSnapshot []byte) ([]SupervisorFrozenPort, error) {
	return captureSupervisorFrozenPorts(intentSnapshot, resolveManifestDaemonPort)
}

func captureSupervisorFrozenPorts(intentSnapshot []byte, resolve manifestDaemonPortResolveFn) ([]SupervisorFrozenPort, error) {
	f, err := decodeOriginalSupervisorIntentFile("original supervisor intent", intentSnapshot)
	if err != nil {
		return nil, err
	}
	return captureSupervisorIntentPorts(f, resolve, false)
}

func captureSupervisorIntentPorts(f *SupervisorIntentFile, resolve manifestDaemonPortResolveFn, current bool) ([]SupervisorFrozenPort, error) {
	// JSON null and missing/null daemons all decode to nil; only an explicitly
	// present array can supply complete local capture input.
	if f == nil || f.Daemons == nil {
		return nil, errors.New("original supervisor intent requires a nonnull daemons array")
	}
	rows := make([]SupervisorFrozenPort, 0, len(f.Daemons))
	seenTasks, seenPorts := make(map[string]bool), make(map[int]bool)
	for i, d := range f.Daemons {
		if strings.TrimSpace(d.TaskName) == "" || strings.TrimSpace(d.Command) == "" {
			return nil, fmt.Errorf("original daemon %d requires task_name and command", i)
		}
		if current {
			key := canonicalIntentTaskKey(d.TaskName)
			if seenTasks[key] {
				return nil, errors.New("duplicate current runtime task identity")
			}
			seenTasks[key] = true
			if currentSupervisorPortlessDescriptor(d) {
				continue
			}
			builtinRoute, err := recognizeBuiltinRouteDaemon(d)
			if err != nil {
				return nil, fmt.Errorf("original daemon %d: %w", i, err)
			}
			_, _, ordinary := DescriptorServerDaemon(d)
			if !builtinRoute && (len(d.Args) == 0 || d.Args[0] != "daemon" || (!ordinary && !IsSerenaProxyDescriptor(d) && !IsWorkspaceLSPProxyDescriptor(d))) {
				return nil, errors.New("unknown current runtime descriptor role")
			}
		}
		resolved, err := classifyEffectiveDaemonPort(d, resolve, true)
		if err != nil {
			return nil, fmt.Errorf("original daemon %d: %w", i, err)
		}
		if resolved.portless {
			continue
		}
		if current {
			if seenPorts[resolved.port] {
				return nil, errors.New("duplicate current runtime port identity")
			}
			seenPorts[resolved.port] = true
		}
		argv, err := json.Marshal(append([]string{d.Command}, d.Args...))
		if err != nil {
			return nil, fmt.Errorf("original daemon %d argv: %w", i, err)
		}
		digest := sha256.Sum256(argv)
		rows = append(rows, SupervisorFrozenPort{
			TaskName: d.TaskName, Port: resolved.port, ArgvSHA256: hex.EncodeToString(digest[:]),
		})
	}
	rows = CanonicalSupervisorFrozenPorts(rows)
	if err := ValidateSupervisorFrozenPorts(rows); err != nil {
		return nil, err
	}
	return rows, nil
}

// Positive exclusions reuse the existing one-shot and generated timer owners.
// Port=0 alone never establishes that a descriptor is a maintenance role.
func currentSupervisorPortlessDescriptor(d SupervisorDaemon) bool {
	if d.Port != 0 {
		return false
	}
	if isLegacyOneshotDaemon(d) {
		return true
	}
	for _, kind := range []string{"workspace-weekly-refresh", "server-weekly-refresh"} {
		if generatedSupervisorImageTimer(MaintenanceTimer{Name: d.TaskName, Kind: kind, Server: d.Server, Command: d.Command, Args: d.Args}) {
			return true
		}
	}
	return false
}

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
// Resolution order: the stamped descriptor Port (>0) → the descriptor's own
// `--port` arg (LAUNCH TRUTH) → the manifest port for the resolved (server,daemon).
func effectivePort(d SupervisorDaemon, resolve manifestPortDeadlineResolveFn) (int, bool) {
	resolved, err := classifyEffectiveDaemonPort(d, func(server, daemon string) (daemonPortResolution, error) {
		port, deadlineSecs, ok := resolve(server, daemon)
		if !ok {
			return daemonPortResolution{}, errUnresolvedSupervisorDaemonPort
		}
		return daemonPortResolution{port: port, deadlineSecs: deadlineSecs}, nil
	}, false)
	if err != nil || resolved.port <= 0 {
		return 0, false
	}
	return resolved.port, true
}

type daemonPortResolution struct {
	port         int
	deadlineSecs int
	portless     bool
}

type manifestDaemonPortResolveFn func(server, daemon string) (daemonPortResolution, error)

// classifyEffectiveDaemonPort owns the precedence for both legacy Boolean
// lookup and complete capture. Strict qualification only affects capture;
// legacy callers retain their existing stamped/first-valid-argv behavior.
func classifyEffectiveDaemonPort(d SupervisorDaemon, resolve manifestDaemonPortResolveFn, complete bool) (daemonPortResolution, error) {
	if complete {
		if d.Port < 0 || d.Port > 65535 {
			return daemonPortResolution{}, ErrInvalidSupervisorFrozenPort
		}
		if _, _, source := ResolveDescriptorMatchIdentity(d); source == IdentityCorruptGlobalArgv {
			return daemonPortResolution{}, errors.New("corrupt global daemon identity")
		}
		if (IsSerenaProxyDescriptor(d) || IsWorkspaceLSPProxyDescriptor(d)) && d.Server != "" && DescriptorServerName(d) == "" {
			return daemonPortResolution{}, errors.New("conflicting proxy server identity")
		}
	}
	if d.Port > 0 {
		if complete {
			if _, _, err := descriptorArgPort(d, true); err != nil {
				return daemonPortResolution{}, err
			}
		}
		return daemonPortResolution{port: d.Port}, nil
	}
	// The descriptor's own `--port` arg is the launch truth for a proxy row: a
	// workspace-proxy (`daemon workspace-proxy --port 94xx …`) or serena-proxy
	// (`daemon serena-proxy --port 915x …`) binds exactly that port, and its
	// dynamic daemon name (lsp-<ws>-<lang> / <workspace-hash>) is not declared in
	// any manifest — so the manifest fallback below would wrongly report it
	// portless and every port protection would stay off (bot PR #505 r3). A global
	// daemon carries no `--port` arg, so this is a no-op there and it falls to the
	// manifest as before.
	p, _, err := descriptorArgPort(d, complete)
	if err != nil {
		return daemonPortResolution{}, err
	}
	if p > 0 {
		return daemonPortResolution{port: p}, nil
	}
	if complete && (IsSerenaProxyDescriptor(d) || IsWorkspaceLSPProxyDescriptor(d)) {
		return daemonPortResolution{}, errors.New("proxy has no resolved port argument")
	}
	server, daemon, idOK := DescriptorServerDaemon(d)
	if !idOK {
		return daemonPortResolution{}, errUnresolvedSupervisorDaemonPort
	}
	resolved, err := resolve(server, daemon)
	if err != nil {
		return daemonPortResolution{}, err
	}
	if resolved.port > 0 {
		if complete && resolved.port > 65535 {
			return daemonPortResolution{}, ErrInvalidSupervisorFrozenPort
		}
		return resolved, nil
	}
	if complete && resolved.port == 0 && resolved.portless {
		return resolved, nil
	}
	return daemonPortResolution{}, errUnresolvedSupervisorDaemonPort
}

// IsSerenaProxyDescriptor / IsWorkspaceLSPProxyDescriptor classify a descriptor's
// LAUNCH shape from ARGV ALONE (`daemon serena-proxy …` / `daemon workspace-proxy
// …`), independent of the persisted Server field. The `daemon <kind>` subcommand
// handler decides identity at spawn — a workspace-proxy row spawned with those
// args IS an LSP proxy whether or not its Server field was ever stamped — so a
// FIELDLESS legacy row (Server=="" — the shape F5's identity-heal used to fix)
// still classifies. These are the SINGLE owner of "which proxy shape is this",
// shared by descriptorArgPort's port-scoping (below) and the cli squatter/reconcile
// classifiers (isSerenaProxyDescriptor / isLSPWorkspaceProxyDescriptor re-export
// them). Single-owning the 2-token argv check makes it structurally impossible for
// the port-resolve side and the port-protect side to disagree about a shape — the
// exact argv-vs-field drift bot PR #505 r5 (F3) found when the classifier keyed on
// d.Server while the resolver keyed on argv.
func IsSerenaProxyDescriptor(d SupervisorDaemon) bool {
	return len(d.Args) >= 2 && d.Args[0] == "daemon" && d.Args[1] == "serena-proxy"
}

// IsWorkspaceLSPProxyDescriptor — see IsSerenaProxyDescriptor. Only
// mcp-language-server emits the `daemon workspace-proxy` subcommand, so the argv
// shape uniquely identifies an LSP workspace proxy; the Server field is redundant
// for a well-formed row and wrong for the fieldless legacy shape.
func IsWorkspaceLSPProxyDescriptor(d SupervisorDaemon) bool {
	return len(d.Args) >= 2 && d.Args[0] == "daemon" && d.Args[1] == "workspace-proxy"
}

// DescriptorHasGlobalDaemonArgv reports whether d's argv is a GLOBAL
// `mcphub daemon --server X --daemon Y` launch — the ONLY shape whose argv
// ASSERTS the authoritative (server, daemon) identity DescriptorServerDaemon
// parses. It is the discriminator for the round-6 leak class: when
// DescriptorServerDaemon returns ok=false AND this returns true, the argv is a
// PARTIAL/corrupt global (a `daemon --server X` with no --daemon, or a field/argv
// mismatch) whose launch truth CONTRADICTS the ambiguous task-name split — so a
// task-name fallback is UNSAFE and the consumer must fail closed. The proxy shapes
// are daemon-shaped but legitimately carry no --daemon identity (their daemon is a
// dynamic workspace key), so they are EXCLUDED: a fieldless proxy row MUST stay
// recoverable by its task name, which its own owner paths (DescriptorServerName,
// descriptorArgPort, Is*ProxyDescriptor) depend on. It is the owner-package home of
// cli's isGlobalDaemonDescriptor (which re-exports it), so "is a global daemon
// argv" is single-owned and the port-resolve, squatter-classify, and match-select
// sides can never drift on the shape (PR #505 r5 F3 lesson).
func DescriptorHasGlobalDaemonArgv(d SupervisorDaemon) bool {
	return len(d.Args) >= 1 && d.Args[0] == "daemon" &&
		!IsSerenaProxyDescriptor(d) && !IsWorkspaceLSPProxyDescriptor(d)
}

// descriptorArgPort owns proxy argv parsing. Legacy lookup keeps the first
// positive value; complete capture distinguishes absent, malformed and duplicate
// operands and validates the selected port range.
func descriptorArgPort(d SupervisorDaemon, complete bool) (port int, present bool, err error) {
	// ONLY the proxy shapes carry an authoritative `--port` arg. Scoping the
	// recovery to `daemon serena-proxy` / `daemon workspace-proxy` (via the shared
	// argv predicates) prevents a corrupt or non-proxy global descriptor with a
	// stray `--port` token from being treated as the protected daemon port — which
	// would let stop-force fall through to the by-port kill path against a DIFFERENT
	// mcphub listener instead of treating the unresolved descriptor as portless
	// (bot PR #505 r4). A global daemon (`daemon --server X --daemon Y`) never
	// carries `--port` anyway. Using the SAME predicates the cli classifier uses
	// keeps the resolve side and the protect side provably in lock-step (r5 F3).
	if !IsSerenaProxyDescriptor(d) && !IsWorkspaceLSPProxyDescriptor(d) {
		return 0, false, nil
	}
	for i := 2; i < len(d.Args); i++ {
		if complete && strings.HasPrefix(d.Args[i], "--port=") {
			return 0, true, errors.New("unsupported proxy port argument shape")
		}
		if d.Args[i] == "--port" {
			if complete && present {
				return 0, true, errors.New("duplicate proxy port argument")
			}
			present = true
			if i+1 >= len(d.Args) {
				if complete {
					return 0, true, errors.New("proxy port argument has no value")
				}
				continue
			}
			p, parseErr := strconv.Atoi(d.Args[i+1])
			if complete && (parseErr != nil || p < 1 || p > 65535) {
				return 0, true, errors.New("invalid proxy port argument")
			}
			if parseErr == nil && p > 0 {
				port = p
				if !complete {
					return port, true, nil
				}
			}
		}
	}
	return port, present, nil
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
	}
	// §4b: serena by SERVER identity → 120s, even when the FULL (server,daemon)
	// identity did not resolve. A serena-proxy row carries `--server serena` but
	// no `--daemon`, so DescriptorServerDaemon returns ok=false for that fieldless
	// shape — yet the old isSerenaProxyDescriptor arm gave it 120s. Recovering the
	// server alone (descriptorServerName) covers EVERY serena shape — unified,
	// workspace-hash proxy, and fieldless serena-proxy — so none drops to 60s and
	// gets restarted mid-cold-start (bot PR #505).
	if DescriptorServerName(d) == SerenaServerName {
		return serenaStartupBindDeadlineSeconds
	}
	return defaultStartupBindDeadlineSeconds
}

// DescriptorServerName resolves ONLY the server identity of a descriptor — the
// struct field, else the `--server` arg — without requiring the daemon. Used
// where the decision keys on server alone (the serena first-bind deadline, the
// managed-server signal gate, the server-only stop/restart selector), so a
// serena-proxy row whose `--daemon` is absent still classifies as serena and a
// blank-field row's true server is taken from its args rather than the greedy
// task-name longest-prefix split.
func DescriptorServerName(d SupervisorDaemon) string {
	fieldServer := d.Server
	var argServer string
	if len(d.Args) > 0 && d.Args[0] == "daemon" {
		for i := 0; i+1 < len(d.Args); i++ {
			if d.Args[i] == "--server" {
				argServer = d.Args[i+1]
				break
			}
		}
	}
	// Fail-closed on a field/arg mismatch, the SAME rule DescriptorServerDaemon
	// applies: a struct field that DISAGREES with the launch `--server` arg is a
	// corrupt cache and must not resolve — otherwise a decision keyed on the stale
	// field (restart/stop server selection, the serena deadline) acts on the wrong
	// daemon (bot PR #505 r4). A blank field or an absent arg is not a mismatch.
	if fieldServer != "" && argServer != "" && fieldServer != argServer {
		return ""
	}
	if fieldServer != "" {
		return fieldServer
	}
	return argServer
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
	resolved, err := resolveManifestDaemonPort(server, daemon)
	if err != nil {
		return 0, 0, false
	}
	return resolved.port, resolved.deadlineSecs, true
}

func resolveManifestDaemonPort(server, daemon string) (daemonPortResolution, error) {
	m, err := loadManifestForServer("", server)
	if err != nil {
		return daemonPortResolution{}, fmt.Errorf("resolve manifest %s: %w", server, err)
	}
	if m == nil {
		return daemonPortResolution{}, errors.New("nil daemon manifest")
	}
	d, found := findDaemon(m, daemon)
	if !found {
		return daemonPortResolution{}, fmt.Errorf("manifest %s has no daemon %s", server, daemon)
	}
	return daemonPortResolution{
		port: d.Port, deadlineSecs: d.StartupBindDeadlineSeconds,
		portless: m.Kind == config.KindCompanion && m.Transport == config.TransportProcess && d.Port == 0,
	}, nil
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

// IdentitySource classifies WHY a descriptor's (server, daemon) pair did or did
// not resolve from argv/fields, so a match/select consumer can react correctly
// instead of collapsing every DescriptorServerDaemon miss into a task-name
// fallback (the round-6 leak class).
type IdentitySource int

const (
	// IdentityFromArgvOrField — the authoritative (server, daemon) resolved from
	// the struct fields and/or the `--server`/`--daemon` argv tokens. Use it.
	IdentityFromArgvOrField IdentitySource = iota
	// IdentityCorruptGlobalArgv — a GLOBAL `daemon …` argv is present but
	// DescriptorServerDaemon rejected it (partial: no --daemon; or a field/argv
	// mismatch). The argv is launch truth and CONTRADICTS the ambiguous task-name
	// split, so the consumer MUST fail closed — never fall back to the task name
	// (bot PR #505 r6: install-preflight self-owned-port bypass, status wrong-port
	// probe, restart/stop out-of-scope kill).
	IdentityCorruptGlobalArgv
	// IdentityTaskNameSafe — no GLOBAL daemon argv asserts an identity to
	// contradict (a true task-name-only row, OR a proxy shape whose real identity
	// comes from its own owner paths). The consumer's task-name recovery
	// (ParseManagedTaskName + its own exact-name / longest-installed-prefix
	// disambiguator) is safe here.
	IdentityTaskNameSafe
)

// ResolveDescriptorMatchIdentity folds the round-6 decision into ONE owner call so
// no PAIR consumer (a consumer that matches/selects on a full (server, daemon)
// identity) re-implements the unsafe `DescriptorServerDaemon ok==false →
// task-name fallback` pattern. It returns the CLASSIFICATION, not a forced match:
// match semantics legitimately differ across consumers (exact canonical-name vs
// longest-installed-prefix vs display), so each switches on the source and applies
// its own recovery only for IdentityTaskNameSafe.
//
// SERVER-ONLY consumers (which resolve a server alone and must stay MORE permissive
// — a partial `daemon --server demo` with no --daemon SHOULD still restart under
// `mcphub restart demo`) do NOT use this; they keep DescriptorServerName and gate
// only their downstream longest-prefix fallback on !DescriptorHasGlobalDaemonArgv.
func ResolveDescriptorMatchIdentity(d SupervisorDaemon) (server, daemon string, source IdentitySource) {
	if s, dm, ok := DescriptorServerDaemon(d); ok {
		return s, dm, IdentityFromArgvOrField
	}
	if DescriptorHasGlobalDaemonArgv(d) {
		return "", "", IdentityCorruptGlobalArgv
	}
	return "", "", IdentityTaskNameSafe
}
