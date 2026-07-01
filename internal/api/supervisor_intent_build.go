// Package api - Phase D.2 supervisor-intent build helpers.
//
// D.2 introduces a per-workspace fan-out for serena-style dynamic-pool
// manifests: given a kind: workspace-scoped manifest with a non-nil
// DaemonTemplate, and the current workspace registry, materialize one
// SupervisorDaemon descriptor per registered serena workspace.
//
// Plan ref: docs/superpowers/plans/2026-05-20-serena-supervisor-unified.md D.2.
package api

import (
	"crypto/sha256"
	"encoding/hex"
	"strconv"
	"strings"

	"mcp-local-hub/internal/config"
)

// SerenaTaskNamePrefix is the canonical leading-backslash prefix for
// every serena dynamic-pool task name. The 8-hex workspace key is
// appended verbatim to produce a "\mcp-local-hub-serena-<wsKey>"
// supervisor-intent task name.
//
// Plan ref: docs/superpowers/plans/2026-05-20-serena-supervisor-unified.md D.2.
const SerenaTaskNamePrefix = `\mcp-local-hub-serena-`

// SerenaTaskNameForWorkspace returns the canonical leading-backslash
// task name for a per-workspace serena daemon. The 8-hex-chars suffix
// comes from WorkspaceKey(workspacePath) which is the same SHA-256[:8]
// hex hash used everywhere else in the codebase for workspace
// identification. See internal/api/workspace_path.go:167.
//
// Hash is deterministic - same canonical path always yields the same
// task name. Collisions inside one user's registry are not a concern:
// birthday bound for 8-hex (32-bit) collisions sits at ~77k workspaces
// for 50% probability and real users carry <100.
//
// Callers MUST pass a CANONICAL absolute path.
//
// Plan ref: docs/superpowers/plans/2026-05-20-serena-supervisor-unified.md D.2.
func SerenaTaskNameForWorkspace(workspacePath string) string {
	return SerenaTaskNamePrefix + WorkspaceKey(workspacePath)
}

// IsSerenaTaskName reports whether taskName looks like the canonical
// supervisor-intent form for a serena per-workspace daemon. Accepts
// both the bare (no leading backslash) and canonical (leading
// backslash) forms. The suffix MUST be exactly 8 lowercase hex chars
// (the WorkspaceKey shape) - any other suffix shape returns false.
//
// The 8-hex requirement is intentional and load-bearing: D.3 will use
// this predicate to classify supervisor-intent.json entries as
// "serena, owned by registry" vs "serena orphan, prune candidate" vs
// "non-serena, leave alone". A permissive predicate that accepted
// "\mcp-local-hub-serena-foo" would mis-classify hand-edited
// descriptors.
//
// Plan ref: docs/superpowers/plans/2026-05-20-serena-supervisor-unified.md D.2.
func IsSerenaTaskName(taskName string) bool {
	const bareSerenaTaskNamePrefix = "mcp-local-hub-serena-"
	suffix := ""
	switch {
	case strings.HasPrefix(taskName, SerenaTaskNamePrefix):
		suffix = strings.TrimPrefix(taskName, SerenaTaskNamePrefix)
	case strings.HasPrefix(taskName, bareSerenaTaskNamePrefix):
		suffix = strings.TrimPrefix(taskName, bareSerenaTaskNamePrefix)
	default:
		return false
	}
	if len(suffix) != 8 {
		return false
	}
	for i := 0; i < 8; i++ {
		c := suffix[i]
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			return false
		}
	}
	return true
}

// BuildSupervisorDaemonsForSerena materializes one SupervisorDaemon
// descriptor per registered serena workspace, given a parsed manifest
// with a non-nil DaemonTemplate and the snapshot of serena rows from
// the workspace registry.
//
// §7.1 upgrade-gate precondition (bot PR #246 r2): each row carries a
// RuntimeSpec (materializeSerenaRuntimeSpec below), which an OLD supervisor
// binary's ReadSupervisorIntent (DisallowUnknownFields) cannot read. ANY caller
// that WRITES these rows into supervisor-intent.json must first ensure the
// running supervisor is this binary (or none is running) — otherwise the old
// supervisor rejects the file and split-brains. InstallParsedManifest enforces
// this with the SupervisorRunningUnderStateDir refuse-gate; the cutover phase
// (design §9 Phase 3) upgrades that refuse to an automatic cold-restart drive.
//
// Inputs:
//
//   - m                 - parsed ServerManifest. MUST satisfy
//     m.Kind == config.KindWorkspaceScoped AND m.DaemonTemplate != nil.
//     If either gate fails the function returns nil.
//   - workspaces        - registry snapshot returned by
//     (*Registry).SerenaEntries(). Empty or nil -> nil result (NOT
//     an error; an empty pool is a valid steady state).
//   - manifestHash      - opaque content hash of the parsed manifest.
//     Empty string is fine for unit tests.
//   - mcphubBinaryPath  - absolute path to the mcphub binary that the
//     supervisor will exec for each descriptor. The supervisor uses
//     exec.Command(d.Command, d.Args...) verbatim and does NOT add
//     any `mcphub daemon` wrapper of its own (see internal/cli/
//     supervise.go), so the descriptor MUST already point at the
//     mcphub binary plus the `daemon --server ... --workspace ...`
//     argv. Production callers resolve it via canonicalMcphubPath();
//     tests may pass any non-empty string.
//
// Outputs:
//
//   - one SupervisorDaemon per entry in workspaces, in input order.
//
// Behavior:
//
//   - TaskName follows the canonical form (leading backslash +
//     mcp-local-hub-serena- + 8-hex-chars-of-WorkspaceKey).
//   - Server is m.Name (e.g. "serena").
//   - Daemon is the per-workspace WorkspaceKey (8 lowercase hex chars)
//     so the downstream health/capability lookup at
//     internal/api/health.go (keyed by Server+"/"+Daemon) keeps
//     per-workspace rows distinct. Without this, every per-workspace
//     descriptor would collapse onto a single ("serena", <context>)
//     pair and only the last workspace's port/backend would survive
//     in those views.
//   - Command is mcphubBinaryPath - the supervisor execs mcphub
//     itself, NOT the raw uvx/python interpreter named in m.Command.
//     The `mcphub daemon` subcommand wraps the raw interpreter
//     internally, layering port-binding, health probes, and graceful
//     shutdown that the supervisor cannot replicate from a bare
//     interpreter argv. Spec ref:
//     docs/superpowers/specs/2026-05-20-serena-dynamic-pool.md §6.
//   - Args is the canonical daemon-wrap argv:
//     `daemon serena-proxy --server <m.Name> --workspace
//     <ws.WorkspacePath> --port <ws.Port> --task-name <self>`. The
//     trailing --task-name lets the `serena-proxy` subcommand
//     (internal/cli/daemon_serena.go) look up ITS OWN descriptor in
//     supervisor-intent.json and read the RuntimeSpec below — it no
//     longer re-reads the manifest. The supervisor itself does NOT
//     see the manifest; the descriptor is genuinely self-sufficient
//     (RuntimeSpec carries every launcher input), so a stale
//     supervisor (e.g. restored from a snapshot) can spawn the right
//     child even if the embedded manifest is later edited.
//   - RuntimeSpec is the MATERIALIZED child runtime spec (design §3):
//     ChildCommand = m.Command; ChildArgs = expanded BaseArgs ++
//     ExtraArgsTemplate ++ appended [--context, DaemonTemplate.Context];
//     EnvRefs = clone(m.Env) with secret:KEY verbatim; UpstreamPort =
//     ws.Port + NativeHTTPInternalPortOffset; ExternalPort = ws.Port;
//     WorkspacePath = ws.WorkspacePath. The launcher reads this instead
//     of the manifest.
//   - Env is a CLONE of m.Env (each descriptor owns its own map).
//   - Env values are passed verbatim. Secret-placeholder expansion
//     (`secret:KEY` references resolved against the vault) is the
//     launcher's responsibility (over RuntimeSpec.EnvRefs) - this
//     helper is pure and does NOT consult any vault or runtime state.
//   - Workspace is the canonical absolute path from the registry row.
//   - Port is the per-workspace port persisted on the registry row.
//     The fan-out does NOT re-allocate.
//   - ManifestHash is the supplied manifestHash.
//
// Determinism: same inputs -> same outputs. Order matches the input
// workspaces slice.
//
// Filesystem existence: this helper does NOT consult the filesystem.
// Workspace paths from removed/moved directories are emitted as-is
// (the supervisor would later fail to spawn them because cmd.Dir =
// d.Workspace is set unconditionally before cmd.Start). The CALLER
// (D.3 / install_intent) is responsible for filtering non-existent
// workspace paths before writing the descriptor list to
// supervisor-intent.json, or for accepting the stale-row audit
// trail per the dynamic-pool spec's missing-workspace case.
//
// Plan ref: docs/superpowers/plans/2026-05-20-serena-supervisor-unified.md D.2.
// Spec ref: docs/superpowers/specs/2026-05-20-serena-dynamic-pool.md §6
//
//	(lifecycle / spawn args, port: workspace.serena_port).
func BuildSupervisorDaemonsForSerena(
	m *config.ServerManifest,
	workspaces []WorkspaceEntry,
	manifestHash string,
	mcphubBinaryPath string,
) []SupervisorDaemon {
	if m == nil || m.DaemonTemplate == nil {
		return nil
	}
	if m.Kind != config.KindWorkspaceScoped {
		return nil
	}
	// native-http build-time transport gate (design §3.1). The proxy no
	// longer re-validates transport at runtime (it reads a materialized
	// RuntimeSpec, not the manifest), and a stdio-bridge + daemon_template +
	// kind:workspace-scoped manifest PASSES config.ServerManifest.Validate
	// today — so this is the only thing stopping a non-native-http
	// dynamic-pool manifest from reaching the HTTP-reverse-proxy spawn path.
	// Return nil (the install/migrate caller fails loud via the
	// InstallParsedManifest contract gate, which also enforces native-http).
	if m.Transport != config.TransportNativeHTTP {
		return nil
	}
	// Empty-context gate (bot PR #246 P2). The materializer APPENDS
	// `--context <DaemonTemplate.Context>` to every RuntimeSpec.ChildArgs
	// (buildSerenaChildArgs / design §5). config.ServerManifest.Validate does
	// NOT check Context (it checks port_pool + extra_args_template only —
	// internal/config/manifest.go:404-415), so an absent/blank context would
	// silently materialize `--context ""` and the serena child would launch
	// with an invalid empty context. Return nil here (the install/migrate
	// caller fails loud via the InstallParsedManifest contract gate, which also
	// enforces non-empty context) rather than emitting a broken descriptor.
	if strings.TrimSpace(m.DaemonTemplate.Context) == "" {
		return nil
	}
	// Duplicate-context gate (bot PR #246 r2 P2). buildSerenaChildArgs APPENDS
	// the authoritative `--context <DaemonTemplate.Context>`; a --context token
	// already in base_args / extra_args_template would materialize a SECOND
	// --context flag (child rejects the duplicate, or silently uses the wrong
	// value when the two differ). config.ServerManifest.Validate now rejects this
	// shape, but InstallParsedManifest accepts a PRE-PARSED manifest that may not
	// have been re-validated — so guard here too (defense-in-depth, mirrors the
	// transport + empty-context gates above).
	if config.ArgsContainContextFlag(m.BaseArgs) || config.ArgsContainContextFlag(m.DaemonTemplate.ExtraArgsTemplate) {
		return nil
	}
	if len(workspaces) == 0 {
		return nil
	}

	out := make([]SupervisorDaemon, 0, len(workspaces))
	for _, ws := range workspaces {
		if ws.Language != SerenaLanguageSentinel {
			continue
		}
		if ws.WorkspacePath == "" {
			continue
		}

		// Per-workspace daemon key for the (Server, Daemon) lookup
		// in health.go and friends. Prefer the persisted
		// WorkspaceKey from the registry row; fall back to computing
		// it from the path so the helper stays robust against rows
		// written before the key was persisted.
		daemonKey := ws.WorkspaceKey
		if daemonKey == "" {
			daemonKey = WorkspaceKey(ws.WorkspacePath)
		}

		// Canonical supervisor argv: the supervisor invokes
		// `mcphub daemon serena-proxy --server <name> --workspace
		// <path> --port <port> --task-name <self>`. The trailing
		// --task-name lets the `serena-proxy` subcommand
		// (internal/cli/daemon_serena.go) look up ITS OWN descriptor
		// in supervisor-intent.json and read the materialized
		// RuntimeSpec below — it no longer re-reads the manifest. The
		// supervisor itself execs exec.Command(d.Command, d.Args...)
		// verbatim, so the descriptor is genuinely self-sufficient:
		// every input the launcher needs (child command/args incl.
		// --context + --project, env refs, internal/external ports,
		// workspace) lives on the descriptor, NOT in the manifest.
		taskName := SerenaTaskNameForWorkspace(ws.WorkspacePath)
		args := []string{
			"daemon", "serena-proxy",
			"--server", m.Name,
			"--workspace", ws.WorkspacePath,
			"--port", strconv.Itoa(ws.Port),
			"--task-name", taskName,
		}

		out = append(out, SupervisorDaemon{
			TaskName:     taskName,
			Server:       m.Name,
			Daemon:       daemonKey,
			Command:      mcphubBinaryPath,
			Args:         args,
			Env:          cloneStringMap(m.Env),
			Workspace:    ws.WorkspacePath,
			Port:         ws.Port,
			ManifestHash: manifestHash,
			// P1b: serena-proxy children bind their port only after the
			// language-server subprocess is up (tens of seconds; measured go
			// LSP cold = 46s), so stamp the longer 120s first-bind deadline
			// explicitly on the descriptor. (supervisorStartupBindDeadline also
			// defends pre-field serena rows via isSerenaProxyDescriptor, but the
			// explicit field makes the on-disk descriptor self-describing.)
			StartupBindDeadlineSeconds: 120,
			RuntimeSpec:                materializeSerenaRuntimeSpec(m, ws),
		})
	}
	return out
}

// materializeSerenaRuntimeSpec builds the per-workspace DaemonRuntimeSpec the
// serena-proxy launcher reads instead of re-parsing the manifest. PURE: no
// filesystem, no vault, no runtime state — same inputs yield the same spec.
//
// ChildArgs assembly is the SINGLE mechanism that fixes finding #4 (lost
// --context): expand ${workspace.path} over BaseArgs ++ ExtraArgsTemplate via
// config.ExpandWorkspacePathTokens, then APPEND --context <DaemonTemplate.Context>
// as a separate trailing pair (the template does NOT carry a --context token —
// design §5). ChildArgs does NOT include --port; the launcher appends the
// internal (upstream) port at spawn.
//
// EnvRefs is a CLONE of m.Env with secret:KEY values left VERBATIM (resolved
// in the launcher against the vault — cleartext-free on disk, design §3).
//
// Port math: UpstreamPort = ws.Port + config.NativeHTTPInternalPortOffset;
// ExternalPort = ws.Port; WorkspacePath = ws.WorkspacePath. ExternalPort and
// WorkspacePath mirror the top-level SupervisorDaemon.Port / .Workspace fields
// (the §3.2 consistency contract asserts they agree at proxy startup).
//
// Design ref: docs/superpowers/specs/2026-05-29-serena-migrate-redesign-descriptor-proxy.md §3/§5.
func materializeSerenaRuntimeSpec(m *config.ServerManifest, ws WorkspaceEntry) *DaemonRuntimeSpec {
	return &DaemonRuntimeSpec{
		SpecVersion:   DaemonRuntimeSpecVersion,
		ChildCommand:  m.Command,
		ChildArgs:     buildSerenaChildArgs(m, ws.WorkspacePath),
		EnvRefs:       cloneStringMap(m.Env),
		UpstreamPort:  ws.Port + config.NativeHTTPInternalPortOffset,
		ExternalPort:  ws.Port,
		WorkspacePath: ws.WorkspacePath,
	}
}

// buildSerenaChildArgs assembles the upstream child argv from a serena
// dynamic-pool manifest + a workspace path. It is the one place the
// --context append (finding #4) and the ${workspace.path} expansion live, so
// the materializer and any future caller share a single, unit-testable rule.
//
//	childArgs = ExpandWorkspacePathTokens(BaseArgs ++ ExtraArgsTemplate, wsPath)
//	            ++ ["--context", DaemonTemplate.Context]
//
// --context is APPENDED (not a template token): config.ExpandWorkspacePathTokens
// only resolves ${workspace.path}, so a ${context} token would emit a literal
// invalid argument (design §5). Callers MUST pass a non-nil DaemonTemplate
// manifest (BuildSupervisorDaemonsForSerena guards that upstream).
func buildSerenaChildArgs(m *config.ServerManifest, workspacePath string) []string {
	templateArgs := make([]string, 0, len(m.BaseArgs)+len(m.DaemonTemplate.ExtraArgsTemplate))
	templateArgs = append(templateArgs, m.BaseArgs...)
	templateArgs = append(templateArgs, m.DaemonTemplate.ExtraArgsTemplate...)
	childArgs := config.ExpandWorkspacePathTokens(templateArgs, workspacePath)
	return append(childArgs, "--context", m.DaemonTemplate.Context)
}

// cloneStringMap returns a shallow copy of m, or nil when m is nil.
// Used by BuildSupervisorDaemonsForSerena so each per-workspace
// descriptor owns an independent env map.
func cloneStringMap(m map[string]string) map[string]string {
	if m == nil {
		return nil
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// hashWorkspacePathForTest exposes the suffix-only hash helper for the
// fan-out tests so a deterministic vs. collision assertion can compare
// against the same byte sequence WorkspaceKey produces. Internal, NOT
// part of the public API surface.
func hashWorkspacePathForTest(path string) string {
	sum := sha256.Sum256([]byte(path))
	return hex.EncodeToString(sum[:])[:8]
}
