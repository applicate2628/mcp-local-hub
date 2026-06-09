# Serena dynamic-pool migrate redesign — descriptor-driven proxy

> **Status**: DESIGN — PASS (rev 2). Architecture decided (codex design-consult 2026-05-29,
> `.scratch/codex-prompts/d3-precedence-design-20260529-040309.out`) and formalized here. **Rev 2 closes a
> codex design-review REVISE** (`.scratch/codex-prompts/migrate-redesign-design-review-20260529-111125.out`
> — 3 blockers + 6 major/medium): Phase 1+2 merged atomically (§2.2/§4), native-http build/install gate
> added (§3.1), E.2 auto-register (codex "Phase 6", plan Phase 5) hard-depends on the atomic
> descriptor/proxy phase (§6/plan), `--context` single-append mechanism (§3/§5),
> cross-version upgrade/restart gate (§7.1), nil-spec heal path (§4), context value made O1-agnostic (§4/§9),
> GUI-port live-pidport discovery (§5), descriptor/flag consistency contract (§3.2). The direction
> (descriptor-driven proxy) is unchanged.
> This spec SUPERSEDES the manifest-rewrite approach in the parent serena plan §D.3
> ([docs/superpowers/plans/2026-05-20-serena-supervisor-unified.md:1135-1409](../plans/2026-05-20-serena-supervisor-unified.md)).
> It builds ON the merged PR #244 foundation (`InstallParsedManifest`, two-pass `executeInstallTo`,
> `BuildSupervisorDaemonsForSerena`); it does NOT rework that foundation.
>
> Implementation contract: [docs/superpowers/plans/2026-05-29-serena-migrate-redesign.md](../plans/2026-05-29-serena-migrate-redesign.md).

## 1. Problem (verified against merged code)

The serena per-workspace proxy re-reads the server manifest at spawn and fails the
dynamic-pool checks because the embedded manifest is still the legacy `kind: global`
shape. Concretely, traced at HEAD `4960d61`:

1. The supervisor execs `mcphub daemon serena-proxy --server serena --workspace <path> --port <port>`
   for each registered workspace. The argv carries no template/kind/transport data — only
   server, workspace, port ([internal/api/supervisor_intent_build.go:212-217](../../../internal/api/supervisor_intent_build.go)).
2. `serena-proxy` re-reads the manifest in-process via `api.NewAPI().ManifestGet(serverFlag)`
   ([internal/cli/daemon_serena.go:87](../../../internal/cli/daemon_serena.go)).
3. `ManifestGet` → `loadManifestYAMLEmbedFirst` prefers the **embedded** manifest and only
   falls to disk when the embed read fails
   ([internal/api/manifest_source.go:73-87](../../../internal/api/manifest_source.go)). Serena's
   manifest is embedded, so the embed always wins.
4. The embedded manifest is `kind: global` with the legacy 2-daemon claude/codex shape and no
   `daemon_template` ([servers/serena/manifest.yaml:1-2,28-36](../../../servers/serena/manifest.yaml)).
5. The proxy then fails the hard checks: `m.Kind != KindWorkspaceScoped` →
   error ([internal/cli/daemon_serena.go:98-100](../../../internal/cli/daemon_serena.go)) and
   `m.DaemonTemplate == nil` → error ([internal/cli/daemon_serena.go:101-103](../../../internal/cli/daemon_serena.go)).

**Result on a packaged binary**: a migrate-by-disk-rewrite "succeeds" (writes
`supervisor-intent.json` + an on-disk manifest) but every spawned per-workspace daemon dies at
startup, because the proxy reads the embedded legacy manifest, not the disk rewrite. The disk
rewrite is functionally dead. The same defect blocks E.2 auto-register, which would synthesize the
same workspace-scoped dynamic manifest the proxy must consume.

The proxy's self-sufficiency comment is **currently false**:
[internal/api/supervisor_intent_build.go:136-140](../../../internal/api/supervisor_intent_build.go) claims
"The supervisor itself does NOT see the manifest; the descriptor is intentionally
self-sufficient" — but the proxy depends on a manifest re-read for kind/template/transport/env/command/args.

### What the proxy actually consumes from the manifest (the runtime contract)

Reading [internal/cli/daemon_serena.go:87-136](../../../internal/cli/daemon_serena.go), the proxy uses:

| Manifest field | Runtime use | Build-time or runtime? |
|---|---|---|
| `m.Kind`, `m.DaemonTemplate != nil`, `m.Transport` | validation gates (lines 98-106; transport check at :104-105) | **build-time** (move to descriptor-build / install — see §3.1 transport gate) |
| `m.Name` | identity cross-check (line 95) | runtime (already on descriptor as `Server`) |
| `m.Command` | child command (line 129) | **runtime** (materialize onto descriptor) |
| `m.BaseArgs` ++ `m.DaemonTemplate.ExtraArgsTemplate` | child args, after `${workspace.path}` expansion (lines 121-124) | **runtime** (materialize onto descriptor) |
| `m.Env` | env, after `secret:` resolution (lines 109-111) | **runtime** (materialize raw refs onto descriptor; resolve in proxy) |
| `m.DaemonTemplate.PortPool` | NOT read by proxy | build/register-time only |
| `m.DaemonTemplate.Context` | NOT read by proxy → finding #4 (lost) | runtime input that must materialize to `--context <value>` |
| LSP `m.Languages` | NOT read by proxy | build/register-time only |

This table is the load-bearing input to the descriptor schema in §3.

## 2. Decision: descriptor-driven proxy (codex Option 1, strengthened)

The shipped (embedded) manifest stays the **catalog / default input**. The workspace registry +
`supervisor-intent.json` become the **operator runtime source of truth**. The serena-proxy starts
from a **materialized runtime descriptor** carried on the `SupervisorDaemon` descriptor, NOT from
`ManifestGet`. It drops the `ManifestGet` re-read and the kind/template/transport checks; those
move to descriptor-build time (`BuildSupervisorDaemonsForSerena`) and install time (`Preflight`,
`InstallParsedManifest`'s contract gate).

### 2.1 Trust / precedence model

Three layers, kept strictly separate (this is the spine of the whole redesign):

- **Shipped catalog (embedded manifest)** — the default template input. Read at *build/register/migrate
  time only* to seed the descriptor and the port pool. Never read at proxy runtime.
- **Operator runtime state** — `supervisor-intent.json` (materialized descriptors) + the workspace
  registry (`workspaces.yaml`, serena rows) + the `/serena/mcp` router's live read of the registry.
  This is what spawns daemons and routes clients at runtime.
- **No third manifest store** — rejected (see §2.3). The descriptor IS the runtime spec; we do not
  recreate manifest precedence under another name.

Direction of authority at each lifecycle stage:

```
register / migrate / E.2 (build time):   embedded manifest  ──►  descriptor + registry
supervisor reconcile (runtime):          supervisor-intent.json (descriptor)  ──►  spawn proxy
serena-proxy spawn (runtime):            its own descriptor  ──►  child command/args/env (NO manifest read)
/serena/mcp routing (runtime):           workspace registry  ──►  upstream port
```

The embedded manifest can be migrated to the dynamic-pool shape later (parent plan §G.1) without
changing runtime behavior, because runtime no longer reads it — that decouples the catalog migration
from the runtime migration and shrinks the blast radius of both.

### 2.2 Mechanism

1. Extend `SupervisorDaemon` ([internal/api/supervisor_intent.go:25-35](../../../internal/api/supervisor_intent.go))
   with an optional, additive `RuntimeSpec` sub-struct (§3). It carries the **materialized** child
   command, final child args (incl. `--project <workspace>` AND `--context <value>`), raw env refs,
   internal+upstream ports, and the workspace path.
2. `BuildSupervisorDaemonsForSerena` materializes the `RuntimeSpec` per workspace. The `ChildArgs`
   assembly is a single mechanism (§5): expand `${workspace.path}` over `BaseArgs ++
   ExtraArgsTemplate`, then **append** `--context <DaemonTemplate.Context>` as a separate trailing pair
   (the materializer appends it; the `extra_args_template` does NOT carry a `--context` token — see §5).
   It also computes the internal port (`port + NativeHTTPInternalPortOffset`) and clones the raw env
   map. The kind/template gates already live there as early-return guards
   ([internal/api/supervisor_intent_build.go:172-180](../../../internal/api/supervisor_intent_build.go)) —
   they stay; the transport gate is ADDED there as a build-time validation (§3.1) because it has no
   guard today.
3. `serena-proxy` reads `RuntimeSpec` off its descriptor instead of `ManifestGet`. It drops the
   manifest re-read and the kind/template/transport checks. It still resolves `secret:` refs against
   the vault and drives the `daemon.HTTPHost` lifecycle exactly as today.
4. The proxy loads its descriptor by `--task-name` (the canonical `SerenaTaskNameForWorkspace`), reading
   `supervisor-intent.json` from the state dir. This keeps the supervisor argv small and the descriptor
   the single runtime input. (Alternative delivery — inline `--spec` JSON on argv — is rejected in §2.3.)

The self-sufficiency comment at `supervisor_intent_build.go:136-140` becomes **true** after this change.

### 2.3 Rejected alternatives

| Alternative | Verdict | Why rejected |
|---|---|---|
| **#2 Disk-wins manifest precedence** — `loadManifestYAMLEmbedFirst` prefers disk for serena (or a marker) | Reject | Fights the documented embed-first source-of-truth design ([manifest_source.go:30-39](../../../internal/api/manifest_source.go)). Blast radius is *every embedded server*: a stale or corrupt disk manifest would shadow the shipped default for all 10+ servers, not just serena. Too much risk for one server's defect. |
| **#3 Separate dynamic-manifest store** — operator-state file the proxy + reconcile read as a parallel manifest | Reject as a manifest store | Creates a *third* source of truth and a new read path. We already have operator runtime state (intent + registry); use it for runtime *descriptors*, not a parallel manifest. |
| **Inline `--spec` JSON on supervisor argv** (instead of `--task-name` lookup) | Reject | argv length/quoting fragility on Windows; the supervisor execs `cmd.Args` verbatim and a multi-KB JSON arg invites truncation/escaping bugs. The descriptor already lives in `supervisor-intent.json`; a `--task-name` lookup reuses the existing read path (`ReadSupervisorIntent`). |
| **Proxy keeps reading manifest but via a disk-only seam for serena** | Reject | Same split-brain risk as #2, narrower. Still couples runtime to a file that build-time already materialized into the descriptor; defeats the self-sufficiency goal. |

## 3. The `SupervisorDaemon` runtime-spec extension

Additive, backward-compatible **for new code reading old files**. `RuntimeSpec` is a pointer with
`omitempty` so existing intent files round-trip unchanged through `ReadSupervisorIntent`'s
`DisallowUnknownFields` decoder
([internal/api/supervisor_intent.go:106-114](../../../internal/api/supervisor_intent.go), the
`dec.DisallowUnknownFields()` call at :107): old files simply have a nil `RuntimeSpec`; a new
supervisor reading them re-materializes on next install. This is the same additive-field discipline
`Lifecycle`/`LastMaterializedAt` use on `WorkspaceEntry`
([internal/api/workspace_registry.go:60-70](../../../internal/api/workspace_registry.go)).

**The reverse direction is NOT automatically safe and needs an explicit upgrade gate.** Because
`ReadSupervisorIntent` calls `DisallowUnknownFields`, an *old* (pre-RuntimeSpec) supervisor binary
reading a *new* `runtime_spec`-bearing intent file fails the decode. That failure is not benign:

- The supervisor's `IntentWatcher` callback treats a re-read error as warn-only and **keeps its stale
  in-memory cache** ([internal/cli/supervise.go:760-778](../../../internal/cli/supervise.go) — the
  comment explicitly says a transient read failure "should NOT clear the cached snapshot"). So an old
  supervisor that was already running would silently ignore the new intent and keep spawning the old
  topology.
- The GUI's `ensureSupervisorRunning` **adopts an already-running supervisor with no version check** —
  `probeSupervisor` returning OK short-circuits to `spawned: false`
  ([internal/cli/gui_supervisor_owner.go:93-97](../../../internal/cli/gui_supervisor_owner.go)), so a
  freshly-installed binary will not by itself replace a stale long-lived supervisor.

Therefore "old supervisors never coexist with new intent files" is NOT a free property of the additive
schema — it must be *enforced* by restarting the supervisor as part of the install/migrate that first
writes a `runtime_spec`. §7 specifies that upgrade/restart gate and ties it to the existing
cold-restart upgrade flow.

```go
// SupervisorDaemon (extended). Existing fields unchanged.
type SupervisorDaemon struct {
    TaskName     string            `json:"task_name"`
    Server       string            `json:"server"`
    Daemon       string            `json:"daemon"`
    Command      string            `json:"command"`        // the WRAPPER cmd: mcphub binary
    Args         []string          `json:"args"`           // `daemon serena-proxy --server ... --workspace ... --port ... --task-name <self>`
    Env          map[string]string `json:"env,omitempty"`
    Workspace    string            `json:"workspace,omitempty"`
    Port         int               `json:"port"`
    ManifestHash string            `json:"manifest_hash"`

    // RuntimeSpec is the materialized child runtime spec for daemons whose
    // launcher (e.g. serena-proxy) must NOT re-read the manifest at spawn.
    // nil for legacy/global daemons that the supervisor spawns via the
    // generic `mcphub daemon --server --daemon` path. Additive + omitempty.
    RuntimeSpec *DaemonRuntimeSpec `json:"runtime_spec,omitempty"`
}

// DaemonRuntimeSpec carries everything the launcher needs to spawn the
// upstream child WITHOUT reading the server manifest. Build-time concerns
// (port_pool, languages, kind) are intentionally absent — they do not belong
// at runtime.
type DaemonRuntimeSpec struct {
    SpecVersion  int               `json:"spec_version"`             // 1; bump on incompatible field changes
    ChildCommand string            `json:"child_command"`            // materialized m.Command (e.g. "uvx")
    ChildArgs    []string          `json:"child_args"`               // FULLY materialized: base_args ++ expanded extra_args_template
                                                                     // ++ [--context, <DaemonTemplate.Context>] appended by the materializer.
                                                                     // extra_args_template supplies --project ${workspace.path}; the
                                                                     // materializer appends --context (it is NOT a template token — §5).
                                                                     // Does NOT include --port; the launcher appends the internal port.
    EnvRefs      map[string]string `json:"env_refs,omitempty"`       // raw env incl. unresolved secret:KEY (resolved in launcher)
    UpstreamPort int               `json:"upstream_port"`            // internal port the child binds (external Port + NativeHTTPInternalPortOffset)
    ExternalPort int               `json:"external_port"`            // the client-facing port the proxy binds (== SupervisorDaemon.Port)
    WorkspacePath string           `json:"workspace_path"`           // canonical absolute path (== SupervisorDaemon.Workspace)
}
```

Design rules:

- **`ChildArgs` is fully materialized at build time** — `${workspace.path}` is already expanded, and
  `--context <value>` is already appended. The proxy does NO token expansion and NO manifest read. This
  is where finding #4 is fixed (single mechanism, §5): the materializer (a) expands `${workspace.path}`
  over `BaseArgs ++ ExtraArgsTemplate` via the existing `ExpandWorkspacePathTokens`
  ([internal/config/manifest_workspace.go:38-45](../../../internal/config/manifest_workspace.go), which
  only replaces the `${workspace.path}` token) and (b) appends `--context <DaemonTemplate.Context>` as a
  separate trailing pair. The `extra_args_template` does NOT contain a `--context ${context}` token —
  there is no `${context}` expansion surface, and adding one is explicitly rejected (§5) in favor of the
  append.
- **Secret refs stay unresolved on disk** — `EnvRefs` carries `secret:KEY` verbatim; the proxy resolves
  against the vault at spawn (preserving the cleartext-free-on-disk invariant the manifest already
  honors). This matches `BuildSupervisorDaemonsForSerena`'s existing "Env values are passed verbatim …
  secret-placeholder expansion is the caller's responsibility" contract
  ([internal/api/supervisor_intent_build.go:141-145](../../../internal/api/supervisor_intent_build.go)).
- **No build-time concerns** — `port_pool`, `languages`, `kind` are absent. They are register/migrate-time
  inputs, not runtime.
- **`Port` redundancy is intentional** — `ExternalPort`/`WorkspacePath` mirror the existing
  `SupervisorDaemon.Port`/`Workspace`. The launcher reads them from `RuntimeSpec` so the runtime contract
  is self-contained in one struct; the top-level fields stay for the supervisor's own reconcile/status
  bookkeeping (which is RuntimeSpec-agnostic). A build-time invariant check asserts they agree.

### 3.1 The native-http transport gate (must NOT be lost)

`RuntimeSpec` intentionally omits `transport` (it carries `ChildCommand`/`ChildArgs`/ports — the proxy
no longer branches on transport at runtime). That omission is safe **only if a build/install-time gate
enforces native-http**, because the proxy currently rejects a non-native-http manifest at runtime
([internal/cli/daemon_serena.go:104-105](../../../internal/cli/daemon_serena.go)) and Phase 2 removes
that runtime check. Today **nothing else enforces it**:

- The fan-out gates only nil/template/kind/empty-workspaces
  ([internal/api/supervisor_intent_build.go:172-180](../../../internal/api/supervisor_intent_build.go)) —
  no transport check.
- `InstallParsedManifest`'s contract gate checks only kind + `daemon_template`
  ([internal/api/install_parsed_manifest.go:116-118](../../../internal/api/install_parsed_manifest.go)) —
  no transport check.
- `ServerManifest.Validate` accepts native-http, stdio-bridge, AND remote-http at
  [internal/config/manifest.go:294](../../../internal/config/manifest.go); it rejects `daemon_template`
  only when `kind != workspace-scoped` or `transport == remote-http`
  ([internal/config/manifest.go:306-311](../../../internal/config/manifest.go)). **A `stdio-bridge` +
  `daemon_template` + `kind: workspace-scoped` manifest passes `Validate` today** — so the runtime check
  in the proxy was the only thing stopping a non-native-http dynamic-pool manifest from reaching the
  HTTP-reverse-proxy spawn path.

**Resolution — an explicit `transport == native-http` build/install gate that lands in the same atomic
phase as (and before) the runtime-check removal.** The gate's exact placement:

1. **`BuildSupervisorDaemonsForSerena`** (the materializer) returns nil — or, preferably, the
   install/migrate caller fails loud — when `m.Transport != config.TransportNativeHTTP`. This is the
   nearest owner: it is the single point where a `daemon_template` manifest is turned into proxy
   descriptors, and it already owns the kind/template guards
   ([internal/api/supervisor_intent_build.go:172-180](../../../internal/api/supervisor_intent_build.go)).
2. **`InstallParsedManifest`'s contract gate** adds `transport == native-http` alongside the existing
   kind + `daemon_template` check
   ([internal/api/install_parsed_manifest.go:116-118](../../../internal/api/install_parsed_manifest.go))
   so a non-native-http dynamic-pool manifest is rejected *before any mutation*, with the same
   fail-loud operator message style as the existing gate.

Both points get tests (plan Phase 1+2). The proxy may keep a single cheap belt-and-suspenders assertion
that `RuntimeSpec` was produced by the native-http path (the descriptor is well-formed for native-http
by construction), but the *authoritative* gate is build/install-time per the `Ownership /
extension-seam hygiene` rule — runtime no longer re-validates transport.

### 3.2 Descriptor/flag consistency contract (proxy fails on disagreement)

The supervisor execs each descriptor's argv verbatim — `exec.Command(d.Command, d.Args...)`
([internal/cli/supervise.go:2149](../../../internal/cli/supervise.go)). The proxy's argv therefore
carries `--server`, `--workspace`, `--port`, and (new) `--task-name`, while the looked-up `RuntimeSpec`
independently carries `WorkspacePath` and `ExternalPort`. Today the proxy uses `--workspace` for
canonicalization + log identity ([internal/cli/daemon_serena.go:71-80](../../../internal/cli/daemon_serena.go))
and `--port` for the listen address ([internal/cli/daemon_serena.go:146-149](../../../internal/cli/daemon_serena.go)).
If the argv and the `RuntimeSpec` disagree (e.g. a hand-edited descriptor, a half-applied migration, or
a stale row), the proxy could listen on one port while spawning a child materialized for another.

**Contract**: at startup, after loading its descriptor by `--task-name`, the proxy asserts:

- `--task-name` resolves to exactly one descriptor whose `TaskName` equals the flag (no match → fail
  loud);
- `CanonicalWorkspacePath(--workspace) == RuntimeSpec.WorkspacePath` (and `== SupervisorDaemon.Workspace`);
- `--port == RuntimeSpec.ExternalPort` (and `== SupervisorDaemon.Port`).

Any mismatch is a fail-loud non-zero exit with a launch-failure log naming the disagreeing fields — NOT
a silent reconcile to one side. This is the runtime mirror of the build-time invariant in §3 that
asserts `RuntimeSpec.ExternalPort == SupervisorDaemon.Port` and
`RuntimeSpec.WorkspacePath == SupervisorDaemon.Workspace`; together they make the argv, the top-level
descriptor fields, and the `RuntimeSpec` a single self-consistent unit at both ends.

## 4. The proxy's new startup path

Replaces [internal/cli/daemon_serena.go:87-136](../../../internal/cli/daemon_serena.go) (the
`ManifestGet` → parse → kind/template/transport checks → build childArgs block). New flow:

```
1. Validate flags (--port, --workspace, --server, --task-name).
2. Canonicalize workspace; compute wsKey; open per-workspace log (UNCHANGED).
3. Load own descriptor:
     intent := ReadSupervisorIntent(<state-dir>/supervisor-intent.json)
     d := intent.findByTaskName(--task-name)         // NEW lookup
     if d == nil || d.RuntimeSpec == nil:
         return error "no runtime spec for task <name>; reinstall serena dynamic pool"
   3.5 Consistency assertion (§3.2): fail loud if any disagree —
       d.TaskName == --task-name
       Canonical(--workspace) == d.RuntimeSpec.WorkspacePath == d.Workspace
       --port            == d.RuntimeSpec.ExternalPort  == d.Port
4. spec := d.RuntimeSpec; if spec.SpecVersion unsupported: fail loud (no manifest fallback)
5. Resolve env: resolver.ResolveMap(spec.EnvRefs)   // secret:KEY → vault (UNCHANGED mechanism)
6. childArgs := append(clone(spec.ChildArgs), "--port", itoa(spec.UpstreamPort))
7. daemon.NewHTTPHost{Command: spec.ChildCommand, Args: childArgs, Env: env,
                      UpstreamPort: spec.UpstreamPort, LogPath: logPath}   // UNCHANGED downstream
8. ListenAndServe external proxy on spec.ExternalPort; shutdown semantics UNCHANGED.
```

What is removed: the `ManifestGet` call, `ParseManifest`, the `m.Kind`/`m.DaemonTemplate`/`m.Transport`
gates, the `BaseArgs ++ ExtraArgsTemplate` assembly, and the `ExpandWorkspacePathTokens` call. All of
that moved to build time.

What stays: flag validation, workspace canonicalization, per-workspace logging, `secret:` resolution
(now over `spec.EnvRefs`), the `daemon.HTTPHost` lifecycle, and the external→internal reverse proxy.

**Defense at the boundary (nil / unsupported / inconsistent spec)**: if `RuntimeSpec` is nil,
`SpecVersion` is unsupported, or the §3.2 consistency assertion fails, the proxy fails loud (non-zero
exit + launch-failure log) rather than silently falling back to a manifest read. **A manifest fallback
is explicitly forbidden** — it would re-introduce the embed-shadow defect this whole redesign exists to
kill (the proxy would re-read the embedded legacy `kind: global` manifest and either crash on the gates
or, worse, silently spawn the wrong child). The operator-actionable error is "reinstall the serena
dynamic pool". This preserves the fail-loud `STATUS_FAILED` posture the project already prefers over
silent legacy fallback (CLAUDE.md "Supervisor" §: "an unreachable or mismatched supervisor fails loud as
`STATUS_FAILED` instead of silently returning the deleted v0.4.x task view").

**Why a nil spec can occur, and the heal path (finding — nil-RuntimeSpec migration).** A nil
`RuntimeSpec` on a serena descriptor is *not* purely theoretical even though the current repo is mostly
greenfield (`mcphub workspace register` fails closed on the missing `daemon_template.port_pool` today —
[internal/cli/workspace_cmd.go:73-81](../../../internal/cli/workspace_cmd.go), called at
[:184-192](../../../internal/cli/workspace_cmd.go) — so few real installs hold serena rows). The nil
case arises for any descriptor **written before `RuntimeSpec` existed**, e.g. a row left by a prior,
since-removed migrate attempt (the migrate command was removed in `a7dcbcd`). The heal is a
**re-materialize via `InstallParsedManifest`**, NOT a runtime manifest fallback:

- The proxy NEVER heals itself by reading the manifest. It fails loud and stops (above).
- The heal runs at install/migrate time: `InstallParsedManifest` re-materializes every serena row with a
  fresh `RuntimeSpec`, and `buildMergedSupervisorIntent` replaces this server's rows wholesale
  ([internal/api/install_parsed_manifest.go:319-346](../../../internal/api/install_parsed_manifest.go)),
  so a single reinstall converts every nil-spec serena row to a spec-bearing one.
- **Sequencing requirement**: the upgrade/restart gate (§7) must drive that re-materialize (or a normal
  reinstall) *before* the supervisor is asked to spawn from any nil-spec row. The §7 cold-restart flow
  is the place this is enforced — the new supervisor reconciles from the freshly-materialized intent, so
  it never spawns a proxy against a nil-spec descriptor in the first place.

## 5. Resolving the related findings (one coherent owner)

### Finding #3 — register↔migrate bootstrap cycle

**The cycle (verified)**: `mcphub workspace register` reads the serena manifest embed-first
([internal/cli/workspace_cmd.go:55-66](../../../internal/cli/workspace_cmd.go)) and `serenaPortPool`
fails closed if `daemon_template.port_pool` is absent
([internal/cli/workspace_cmd.go:73-81](../../../internal/cli/workspace_cmd.go)). The embedded manifest is
`kind: global` with no `daemon_template`, so register cannot allocate a port. Meanwhile the (removed)
migrate command refused an empty registry — circular: you cannot register without the migrated
manifest, and you cannot migrate without a registered workspace.

**Resolution — a shared dynamic-pool builder/service** that owns the **default port-pool + template
policy** for three consumers: `workspace register`, the redesigned migrate, and E.2 auto-register. The
service answers "what is the effective serena `DaemonTemplate` (port_pool, context, extra_args_template)?"
from a single owner instead of three call sites each re-reading the embed-first manifest and each failing
the same way.

- The service's default template is derived from the **embedded manifest if it already declares
  `daemon_template`**, else from a **built-in dynamic-pool default** baked into the service:
  - `port_pool` — the default range.
  - `context` — a single value held in `DaemonTemplate.Context` (the materializer appends it as
    `--context <value>`; it is NOT a token in `extra_args_template`). **The value itself is the pending
    O1 user decision (§9) — this design is context-value-agnostic and does not pick it.** The current
    embedded manifest's value is the natural seed (HEAD declares `context: codex`; the working tree is
    mid-edit on that file — §9 O1).
  - `extra_args_template: [--project, ${workspace.path}]` — `--project` only. **No `--context` token**
    here: the only token the expansion resolves is `${workspace.path}`
    ([internal/config/manifest_workspace.go:38-45](../../../internal/config/manifest_workspace.go)), so a
    `${context}` token would emit a literal invalid argument. `--context` is appended by the materializer
    from `DaemonTemplate.Context` (§3 / finding #4).

  This breaks the cycle: register no longer fails-closed on the legacy `kind: global` embed because the
  service supplies the dynamic-pool template policy.
- **Migration may establish an empty dynamic-pool state** — zero registered workspaces is a
  documented-valid steady state. `InstallParsedManifest` already accepts an empty non-nil `Workspaces`
  slice ([internal/api/install_parsed_manifest.go:88-92,169-171](../../../internal/api/install_parsed_manifest.go))
  and `BuildSupervisorDaemonsForSerena` returns nil for zero workspaces
  ([internal/api/supervisor_intent_build.go:178-180](../../../internal/api/supervisor_intent_build.go)).
  The redesigned migrate therefore does not refuse an empty registry; it installs the dynamic-pool
  manifest with zero daemon rows, and `register` (now unblocked) populates the pool afterward.

This is a `Mechanism inventory before new paths` extension: the owner of port-pool/template policy is
the new service; register/migrate/E.2 consume it instead of each owning a private embed read.

### Finding #4 — `--context` loss

**Verified** (against HEAD and the working tree — they disagree, see O1): `DaemonTemplate.Context`
exists as a field ([internal/config/manifest.go:109](../../../internal/config/manifest.go)) but is **read
by nothing** — the proxy builds args from `BaseArgs ++ ExtraArgsTemplate` only
([internal/cli/daemon_serena.go:121-124](../../../internal/cli/daemon_serena.go)) and
`BuildSupervisorDaemonsForSerena` never references `Context`. The field is decorative today. (Context
values seen in the wild: **HEAD** `servers/serena/manifest.yaml` is a *single* `unified` daemon on
`--context codex` for all clients, with a committed rationale block; the **working tree** is mid-edit and
currently shows the older two-daemon `--context claude-code` / `--context codex` split. The value is
unsettled — O1.)

**Resolution (structural, value-agnostic)**: the materializer **appends** `--context
<DaemonTemplate.Context>` to the descriptor's `ChildArgs` (§3 — separate trailing pair, not a template
token). `Context` becomes load-bearing runtime input, not decoration. The dynamic-pool model uses ONE
daemon per workspace and therefore ONE context value per workspace (the router `/serena/mcp` fronts all
clients regardless of their former per-client context binding, so per-client context divergence is
structurally unreachable — §9 O1 (a)). **This design does NOT pick the value** — `ide-assistant`,
`codex`, or anything else is an O1 user decision; the structure renders whatever `DaemonTemplate.Context`
holds.

> **Open question O1 (see §9)**: the single per-workspace context value is unresolved. HEAD's committed
> manifest mandates `codex`; the working tree is mid-edit on that file; an earlier draft of this design
> asserted `ide-assistant` but that value is **not** sourced from the parent plan (the parent plan's
> §Decision 1 argues the 1:1 daemon:workspace mapping and discusses `claude-code` vs `codex`, and its
> example uses `context: codex` — it never names `ide-assistant`). The value must be validated against
> Serena's actual `--context` behavior and reconciled with the in-flight HEAD manifest before it is baked
> into the shared builder's default. Structural design is context-agnostic; only the value is open.

### Finding #5 — client routing

**Verified**: dynamic serena drops `client_bindings`, but clients still point at the removed
`localhost:9121` global daemon. The `/serena/mcp` router exists and is registry-driven
([internal/gui/serena_router.go:145-147,380-385](../../../internal/gui/serena_router.go)), wired at GUI
boot ([internal/cli/gui.go:303-324](../../../internal/cli/gui.go)). But:

- The **hub G4 resolver** builds bindings only from `m.ClientBindings` + `m.Daemons`
  ([internal/api/hub_mcp_resolver.go:133-161](../../../internal/api/hub_mcp_resolver.go)); a template-only
  serena manifest (no `Daemons`, no `ClientBindings`) yields zero bindings.
- The **hub reconcile filter** `manifestHasScheduledDaemon` iterates `m.Daemons` only
  ([internal/cli/install.go:520-523](../../../internal/cli/install.go)); template-only manifests are
  skipped — never pulled into the installed set.
- The **generic migrate** (`mcphub migrate <server>`, `MigrateFrom`) iterates `m.ClientBindings` and
  resolves the port via `m.Daemons` ([internal/api/migrate.go:97-120](../../../internal/api/migrate.go));
  a template-only serena manifest produces zero rewrites. The existing generic path cannot route clients
  to `/serena/mcp`.

**Critical architectural distinction**: the `/serena/mcp` router and the G4 hub resolver
(`/clients/<id>/mcp`) are **two different routing surfaces**. The serena router resolves the workspace by
tool path-arg against the live registry; it does NOT use the G4 binding topology. Therefore the fix is
NOT to teach the G4 resolver about template-only manifests (that would conflate the two surfaces).

**Resolution — an explicit dynamic-pool client-reconcile path** that rewrites each managed client's
serena entry to the constant router URL `http://127.0.0.1:<gui-port>/serena/mcp` (relay form for
Antigravity), BEFORE legacy `localhost:9121` endpoints are removed. Properties:

- Target URL is the constant GUI-server router endpoint, not a per-daemon port. One entry per client,
  workspace-agnostic (the router picks the workspace per request).
- It records the rewritten entry in the managed-entries marker (same `RecordManagedEntry` discipline
  `MigrateFrom` uses at [internal/api/migrate.go:175-182](../../../internal/api/migrate.go)) so a later
  demigrate can distinguish mcphub-installed from operator-owned entries, and (optionally) in the serena
  registry row's `ClientEntries map[string]string`
  ([internal/api/workspace_registry.go:56](../../../internal/api/workspace_registry.go)) for symmetry with
  the LSP rows.
- Antigravity keeps its stdio-relay shape (relay → router), reusing the existing
  `RelayServer`/`RelayDaemon`/`RelayExePath` triple
  ([internal/clients/clients.go:23-35](../../../internal/clients/clients.go)). The relay's upstream becomes
  the router endpoint instead of `9121`.
- **GUI-port discovery — live pidport + readiness ping, fail-closed (resolves O2's port half).** The
  `/serena/mcp` router lives on the **GUI server**, not the G4 hub listener
  ([internal/gui/serena_router.go:145-147](../../../internal/gui/serena_router.go)). The GUI's bound port
  is `flag > setting > auto(0)` ([internal/cli/gui.go:35-52](../../../internal/cli/gui.go)) and the
  *actual* bound port is written to the pidport file only AFTER the listener is up
  ([internal/cli/gui.go:413-417](../../../internal/cli/gui.go) writes `s.Port()`). A persisted setting
  alone is therefore WRONG for `--port 0` (auto) or an explicit-flag launch that differs from the
  setting. The reconcile MUST derive the URL port from the **live pidport file**
  (`ReadPidport` — [internal/gui/single_instance.go:92-110](../../../internal/gui/single_instance.go))
  and confirm the GUI is actually serving with a readiness ping, exactly mirroring the G4 reconcile
  fail-closed-on-stale-endpoint precedent
  ([internal/cli/install.go:348-374](../../../internal/cli/install.go), which fails closed unless a live
  probe of the bound port succeeds). If the pidport is absent/stale or the ping fails, the reconcile
  fails closed with an operator-actionable "start the GUI first" message — it does NOT fall back to the
  persisted setting and it does NOT write a guessed URL.

This reconcile is the implementation of parent-plan §G.2 acceptance criterion
([…serena-supervisor-unified.md:2018-2019](../plans/2026-05-20-serena-supervisor-unified.md)) — "Migration
script generates per-client config rewrites that point to mcphub-router endpoint instead of individual
serena ports."

> **Open question O2 (see §9)**: which client adapters are in scope for the serena router rewrite (the
> legacy manifest bound claude-code, codex-cli, cursor, vscode, gemini-cli, qwen-cli, antigravity). The
> client set should be derived from the operator's installed clients × the legacy serena bindings, not
> hard-coded. The GUI-port discovery mechanism is no longer open — it is **resolved above** (live pidport
> + readiness ping, fail-closed); O2's remaining open part is only the *client set*.

## 6. How the redesigned `mcphub migrate serena legacy-to-dynamic-pool` sits on the foundation

The command is RE-ADDED on top of the descriptor work (it was removed in `a7dcbcd` as
"not-operator-complete"). It no longer rewrites the disk manifest. Sequence (driver owns a rollback
stack per the parent plan's §D.3 outer/inner composition, which `InstallParsedManifest` already
implements internally):

1. **Source-state detect** (parent plan §D.3 table): legacy 2-daemon / intermediate unified / already-migrated
   / malformed. Idempotent exit-0 on already-migrated.
2. **Build the dynamic-pool manifest in memory** via the shared dynamic-pool builder (finding #3 owner) —
   NOT written to disk. It is `kind: workspace-scoped` with a `daemon_template` carrying the resolved
   port_pool, context, and `extra_args_template`.
3. **Register/preserve workspaces**: read the registry snapshot. Migration may proceed with zero
   workspaces (finding #3); existing serena rows are preserved.
4. **`InstallParsedManifest(ctx, dynamicManifest, opts)`** — the merged seam fans out one descriptor per
   workspace (now with `RuntimeSpec` materialized), writes `supervisor-intent.json` under the folded flock,
   and DEFERS daemon starts to the supervisor reconciler
   ([internal/api/install_parsed_manifest.go:39-95](../../../internal/api/install_parsed_manifest.go)). No
   change to the seam's shape.
5. **Client-reconcile** (finding #5): rewrite client entries to `/serena/mcp` BEFORE removing the legacy
   `localhost:9121` endpoints.
6. **Supervisor upgrade/restart gate** (§7.1): ensure no *old* supervisor binary keeps running against
   the new `runtime_spec`-bearing intent. This is mandatory whenever the install/migrate is the first to
   write a `runtime_spec` — it ties into the existing cold-restart upgrade flow.
7. **Verify**: the (current-binary) supervisor reconciler picks up the new intent and spawns each
   per-workspace proxy from its `RuntimeSpec` (no manifest read), the proxy binds its external port, the
   router resolves workspaces from the registry, clients reach the daemon through the router.

The migrate command becomes the *orchestrator* of (2)→(6); each piece is independently testable and
several land as separate PRs (see the plan).

## 7. Migration + rollback safety

- **Intent-file forward migration (new code reading old files)**: existing `supervisor-intent.json`
  files (pre-RuntimeSpec) have nil `RuntimeSpec` on serena rows. The first `InstallParsedManifest` run by
  the *new* binary re-materializes every serena row with a `RuntimeSpec`, and
  `buildMergedSupervisorIntent` replaces this server's rows wholesale
  ([internal/api/install_parsed_manifest.go:319-346](../../../internal/api/install_parsed_manifest.go)) so
  the row-level heal is complete. Until that runs, a serena proxy spawned from a nil-spec descriptor fails
  loud (§4 boundary defense) — it does not silently fall back to the legacy manifest read.
- **Old supervisor reading new files is NOT auto-safe — see §7.1 (and the reap→write→start interlock in
  §7.1.1)**. The row-level heal above presumes the *new* binary is the one reading and reconciling the
  intent. The precise hazard is the **decoder vintage of the currently-RUNNING supervisor process**, not
  binary identity on disk: an *old-decoder* process still running would fail the `DisallowUnknownFields`
  decode and keep its stale cache (§3) — so a restart/version gate is required, not optional, and "restart,
  not re-link" is what clears it (the serena reap-path is a SAME-binary cutover; see §7.1.1's
  decoder-vintage note). §7.1 is the load-bearing addition; §7.1.1 closes the concurrency hazard inside the
  migrate's own reap→write→start window.

### 7.1 Supervisor upgrade/restart gate (correctness-load-bearing)

**The problem this gate closes.** `ReadSupervisorIntent` uses `DisallowUnknownFields`
([internal/api/supervisor_intent.go:107](../../../internal/api/supervisor_intent.go)). Once an
install/migrate writes a `runtime_spec`-bearing intent file, any *old* supervisor binary still running
will: (1) fail the decode in its `IntentWatcher` callback and **keep its stale in-memory snapshot**
because the callback treats read errors as warn-only
([internal/cli/supervise.go:760-778](../../../internal/cli/supervise.go)); and (2) not be replaced by a
freshly-installed binary, because the GUI's `ensureSupervisorRunning` **adopts an already-running
supervisor with no version check** ([internal/cli/gui_supervisor_owner.go:93-97](../../../internal/cli/gui_supervisor_owner.go)).
The net effect without a gate: the operator runs `migrate`, the new intent lands, but the live old
supervisor ignores it and keeps spawning the legacy topology — the migration silently no-ops at runtime.
"Old supervisors never coexist with new intent files" is therefore a property that must be **enforced**,
not assumed.

**The gate — restart the supervisor as part of the install/migrate that first writes a `runtime_spec`,
via the existing cold-restart upgrade flow.** mcphub already ships exactly this machinery: `mcphub
install --upgrade` runs the cold-restart upgrade flow (CLAUDE.md "Supervisor" §"Cold-restart upgrade
flow"), implemented in [internal/cli/install_upgrade.go](../../../internal/cli/install_upgrade.go) as
**IPC `quiesce-timers` → IPC `exit{graceful}` → force-kill fallback → explicitly start the new
supervisor**, which then reconciles from the (now `runtime_spec`-bearing) intent. The redesign does NOT
invent a new restart path; it requires that the migrate/install path drive this existing flow:

- The migrate command, after writing the new intent (step 4) and reconciling clients (step 5),
  **triggers the cold-restart of the supervisor** (the `quiesce → exit → start-new` sequence in
  `install_upgrade.go`) so the binary that next reads the intent is the new one. If the operator runs
  `migrate` via the normal `mcphub install --upgrade` lifecycle the restart is already in the flow; if
  `migrate` is invoked standalone, it must invoke the same restart seam (not a bespoke `taskkill`).
- A bare in-place binary swap that does NOT restart a *currently-running* old supervisor is unsafe and is
  explicitly out of contract for the `runtime_spec`-introducing install.

**Acceptance criteria for the gate** (its own, separate from the row-level heal):

1. After a `runtime_spec`-introducing migrate/install, no supervisor process older than the new binary is
   left reading the intent: the cold-restart flow's `exit{graceful}` (with force-kill fallback) has
   reaped the prior supervisor and the new one is started and reconcile-ready.
2. If the prior supervisor cannot be quiesced/exited (IPC unreachable, force-kill fails), the
   migrate/install **fails loud** with operator guidance (the existing cold-restart flow already surfaces
   force-kill failures) rather than committing a new intent that a stuck old supervisor will ignore —
   AND no foreign supervisor can start in the reap→write window, AND no concurrent serena auto-register
   reap can force-kill the migrate's lock-holding process (the reap→write→start interlock of §7.1.1
   enforces both).
3. The new supervisor, on cold start, reconciles from the `runtime_spec`-bearing intent and re-materializes
   any nil-spec serena rows (row-level heal) BEFORE spawning — so it never spawns a proxy against a
   nil-spec descriptor (§4 sequencing requirement).
4. Byte-symmetric v0.4.x rollback is unaffected (the gate only touches the v0.5.0 supervisor lifecycle and
   `supervisor-intent.json`, not `daemon-intent.json` / `managed-entries.json` / `watchdog-state.json`).

#### 7.1.1 Reap→write→start window interlock

The gate above closes the *steady-state* hazard (an old-decoder supervisor ignoring new intent). It does
NOT by itself close the *concurrency* hazard inside the migrate's own reap→write→start sequence:
`mcphub migrate serena legacy-to-dynamic-pool` reaps the supervisor (kills the old PID), THEN writes the
new `runtime_spec`-bearing intent, THEN starts the successor. In the gap after the reap but before the
write, a *foreign* supervisor can start — the GUI `ensureSupervisorRunning`, the schedulerless
`registerEnsureSupervisorRunning`, or `POST /api/supervisor/restart` — and if it wins, the gate's
point-in-time probe sees a supervisor running and refuses the migrate's spec-bearing write, leaving the
migrate to fail mid-flight while a freshly-started supervisor holds the lock but has not yet bound its IPC
pipe.

**Mechanism — the lock IS the critical-section mutex (lock-as-mutex).** The migrate HOLDS
`supervisor.lock` across the whole reap→write→start window. While it is held, no other actor can ACQUIRE
it, and every supervisor-starter acquires it before it can run (`api.AcquireSupervisorLock` in
`runSupervise`, [internal/cli/supervise.go:373](../../../internal/cli/supervise.go)) — so a held lock
provably excludes every foreign START for the whole critical section. This is not a new coordination
channel: `supervisor.lock` already IS the single-supervisor singleton mutex; the migrate borrows it as the
critical-section mutex it already is. A racing duplicate-spawn fails its own acquire and its `runSupervise`
returns an error, so the spawned child exits (the singleton-exit property).

**Typed-token gate bypass (not a bool).** Windows byte-range locks are per-HANDLE: the §7.1 gate's
`SupervisorRunningUnderStateDir` probe opens a SECOND flock handle and genuinely misreads the migrate's
OWN held lock as a foreign supervisor (`ERROR_LOCK_VIOLATION` → "running"). So the gate must be told "the
caller already holds the lock; skip the probe." That signal is a **typed capability token**, not a boolean
flag — a `bool SupervisorLockHeldByCaller` would be a zero-cost escape hatch any future caller could set
`true` WITHOUT holding the lock, silently re-arming the split-brain the fail-closed gate exists to prevent.
Instead:

- `InstallParsedManifest` carries `opts.SupervisorLockBypass` of opaque type `InstallParsedManifestBypass`
  ([internal/api/supervisor_lock.go](../../../internal/api/supervisor_lock.go)). Its single field is
  UNEXPORTED (`lk *SupervisorLock`), so no code outside package `api` can forge a non-nil token; the zero
  value (`lk == nil`) is "no bypass" and is the default for every existing call site.
- The token is mintable ONLY by `(*SupervisorLock).AllowSpecBearingWriteBypass()` — i.e. the ONLY way to
  obtain a non-nil token is to already hold a real `*SupervisorLock` returned by `AcquireSupervisorLock`.
- The gate verifies IDENTITY at use time, not truthiness: it skips the probe ONLY when the token's `lk` is
  non-nil AND the lock is still held (`lk.fl != nil` — `Release()` nils `fl`) AND the lock's `path`
  resolves to the gate's own `filepath.Join(stateDir, "supervisor.lock")`. A token whose lock leaf does
  NOT match the gate's `stateDir` is REJECTED (treated as no-bypass → the probe runs → fail-closed), which
  folds the path-consistency check INTO the gate itself. A verified bypass emits the info event
  `spec-bearing-write-allowed-under-caller-lock` (carrying the matched lock path); a forged, mismatched, or
  already-released token never bypasses.

Net: a caller that does not hold the matching lock CANNOT obtain a non-nil bypass, so the invariant is
enforced by the type system plus an identity check, not by a code comment.

**The reap-complete→acquire gap is covered by the acquire-FAIL fail-loud branch.** The interlock is
acquired AFTER the reap (the reap reads `supervisor.lock.owner.json` and must still name the OLD supervisor
when it kills it). In the tiny window between reap-complete and the migrate's own acquire, a foreign
supervisor could start and win the lock. If it does, the migrate's acquire FAILS — and that is the CORRECT
loud-and-retryable outcome, not a bug: the acquire is post-reap-PRE-write, so the new intent is NOT yet
committed and the legacy serena state is untouched. The migrate fails loud ("a supervisor — or another
serena cutover — started during the migrate window and now holds supervisor.lock; the new dynamic-pool
intent was NOT written and legacy serena is untouched; wait for it to settle and re-run the migrate") and
the operator re-runs. No split-brain.

**The release→child-acquire hand-off window is benign.** The migrate RELEASES the lock immediately before
spawning the successor, because the child must `AcquireSupervisorLock` itself — a locked byte-range is not
granted to a detached child on Windows. The residual window between that release and the child's acquire is
benign on every branch: (a) the intent is already committed (the §7.1 WRITE-blocking property is past), so
any winner reads the new `runtime_spec` correctly; (b) the singleton makes a racing duplicate-spawn exit
([internal/cli/supervise.go:373](../../../internal/cli/supervise.go)); (c) an OLD-decoder winner fails its
`DisallowUnknownFields` decode at cold start, the process exits, releases the lock, and NOTHING re-spawns
it (the GUI exit-monitor only logs — `startExitMonitor` in
[internal/cli/gui_supervisor_owner.go](../../../internal/cli/gui_supervisor_owner.go)); and (d) the
migrate's `waitReconcileReadyViaIPC` retries 200 ms × 30 s
([internal/cli/migrate_serena_restart_windows.go](../../../internal/cli/migrate_serena_restart_windows.go)),
so the pre-bind pipe race is tolerated. When the window actually materialized-but-resolved, the migrate
emits the info event `supervisor-interlock-handoff-window` (phase `reconcile-ready-retry` or
`duplicate-spawn-exit`) so an operator can tell a known-benign window apart from a recurrence of the
original 30 s-IPC-timeout bug.

**Auto-register Starter-A extension (the second reaping flow).** The held lock prevents ACQUIRE, not KILL,
so a *second* flow that reaps-then-starts is NOT automatically covered: the serena auto-register cutover
(`AutoRegisterSerenaWorkspace`, [internal/api/serena_auto_register.go](../../../internal/api/serena_auto_register.go))
force-kills the supervisor by PID read from `supervisor.lock.owner.json` on its INTRODUCE branch. But once
the migrate acquires the interlock it OVERWRITES that sidecar with its OWN CLI-process PID — so a concurrent
auto-register INTRODUCE firing inside the migrate's held-lock window would read the sidecar, see the
migrate's PID, and `taskkill /F /T` the MIGRATE mid-cutover. Therefore the auto-register INTRODUCE cutover
acquires the SAME `supervisor.lock` BEFORE its own reap (via the same interlock seam, with identical
acquire→write-with-bypass-token→release lifetime). If the migrate holds the lock, auto-register's acquire
fails BEFORE it reaps anything — so it can NEVER force-kill the migrate's lock-holding PID; it defers
pre-commit, rolls back its registry row, emits the distinct info event
`serena-auto-register-deferred-on-interlock` (NOT a misleading "supervisor running" refusal — there is no
supervisor, a CLI holds the lock), and returns an honest error the router maps to 503 → the client retries.
Symmetrically, if auto-register holds the lock, the migrate's acquire-FAIL fail-loud branch fires. The two
reaping flows are now mutually exclusive and neither can kill the other's lock-holding process. (The
auto-register LIVE-ADD branch does NOT reap and is already §7.1-safe via `HasRuntimeSpecRow()`; it is
unchanged and relies on the registry flock the migrate also takes for its cross-process serialization.)

Why `supervisor.lock` and not a broader `migration.lock`: `migration.lock` is a DIFFERENT leaf and neither
the serena migrate driver nor the v5-upgrade path (`runV5UpgradeWindows`) takes it, so it is not a usable
broader exclusion here; routing both reaping flows onto it would also force it into the generic
`RunInstallUpgrade` contract that serves every v0.5.x upgrade. `supervisor.lock` is the ONE lock every
supervisor-START already contends on, so extending the few REAP flows to take it too is the minimal change
that closes the kill-race without inventing a new lock or widening an unrelated contract.

**Decoder-vintage cross-reference.** The "old supervisor reading new files is NOT auto-safe" note above
(§7 bullet on the reverse-direction hazard) uses *binary*-flavored phrasing, but the hazard the §7.1 gate
and this interlock guard is precisely the **decoder vintage of the currently-RUNNING supervisor process**,
not binary identity on disk. The serena reap-path is a SAME-binary cutover (the rename-aside binary swap is
skipped because the binary is unchanged); a supervisor process launched from a PRIOR image keeps running
the OLD `ReadSupervisorIntent` decoder (its `DisallowUnknownFields` will reject `runtime_spec`) until it is
restarted — restart, not re-link, is what clears it. The interlock's job is to guarantee no process running
an old decoder can observe the new intent and to keep the two reaping flows from killing each other across
the cutover.

The remaining migration/rollback safety properties:

- **No byte-symmetry break for the v0.4.x rollback**: `RuntimeSpec` lives only on `SupervisorDaemon` inside
  `supervisor-intent.json` (a v0.5.0 file). The v0.4.x rollback path
  (CLAUDE.md "Supervisor" §, `daemon-intent.json`/`managed-entries.json`/`watchdog-state.json` byte-symmetry)
  is untouched.
- **Migrate rollback**: the driver's rollback stack restores the registry snapshot and (if a disk manifest
  was ever touched — it is not in this design) the manifest. Client-reconcile failures surface per-client
  (the `MigrateReport.Failed` shape) and are retryable; the legacy endpoints are removed only after the
  router rewrite succeeds, so a partial failure leaves clients on the still-functional legacy endpoint
  rather than a dead URL.
- **Demigrate**: the managed-entries marker lets a future demigrate restore the pre-mcphub client entry,
  identical to the existing migrate/demigrate contract.

## 8. Observability + security-by-design

**Observability**:

- Materialization emits to `supervisor-events.log` (the canonical supervisor audit channel) on each
  install fan-out — one row per workspace descriptor, reusing the existing
  `emitStaleWorkspaceSkippedEvent` channel discipline
  ([internal/api/install_parsed_manifest.go:519-547](../../../internal/api/install_parsed_manifest.go)).
- The proxy logs descriptor load + spec version at startup to the per-workspace log
  ([internal/cli/daemon_serena.go:80](../../../internal/cli/daemon_serena.go) naming), so a spec-version
  mismatch is diagnosable from the daemon log.
- Client-reconcile emits per-(client, server) rows to the hub-mcp event log (the `MigrateFrom` precedent),
  and the marker-write-failure soft-warn is preserved.

**Security-by-design**:

- Secrets stay unresolved on disk (`EnvRefs` carries `secret:KEY` verbatim); resolution is in-process in
  the proxy against the vault. The descriptor file inherits the hardened state-file write pipeline
  (`WriteStateFileAtomic` + the corp-policy DACL posture) that all supervisor state files already use
  (CLAUDE.md "Hardened state-file writes"). No new cleartext surface.
- The router rewrite targets a loopback URL (`127.0.0.1:<gui-port>`); no remote endpoint, no new network
  surface. Same-origin enforcement on `/serena/mcp` is unchanged
  ([internal/gui/serena_router.go:146](../../../internal/gui/serena_router.go) `requireSameOrigin`).
- The proxy's fail-loud-on-nil-spec posture prevents a hand-edited or stale descriptor from silently
  spawning an unintended child.

## 9. Open questions (need a user/codex decision before implementation)

- **O1 — serena single context value (BLOCKING for Phase 3 + the migrate phase; NOT for the atomic 1+2
  defect-fix phase).**
  - **(a) It MUST be a single value.** Per-client context is structurally unreachable in the `/serena/mcp`
    model: the router handler resolves the workspace by tool path-arg against the live registry and has
    NO client identity ([internal/gui/serena_router.go:145-147](../../../internal/gui/serena_router.go) —
    one `/serena/mcp` route, same-origin only, no per-client branch). One daemon per workspace ⇒ one
    `--context` per workspace ⇒ one context for all clients of that workspace. The legacy per-daemon
    `--context claude-code` / `--context codex` split cannot be reproduced and is not needed.
  - **(b) The value is unsettled and needs validation + reconciliation.** Candidates seen in-repo:
    **HEAD** `servers/serena/manifest.yaml` declares a *single* `unified` daemon on **`--context codex`**
    for all clients, with a committed rationale block (the working tree is mid-edit on that same file,
    currently reverted to the older two-daemon split); an earlier draft of this design asserted
    **`ide-assistant`**, but that value is **not** sourced from the parent plan (the parent plan's
    §Decision 1 argues the 1:1 daemon:workspace mapping and discusses `claude-code` vs `codex`; its
    example uses `context: codex`; it never names `ide-assistant`). The chosen value must be validated
    against Serena's actual `--context` behavior (which preset exposes `activate_project` /
    `search_for_pattern` for the multi-project dynamic-pool model) AND reconciled with the in-flight HEAD
    manifest edit before it is baked into the shared builder's default template.
  - **(c) Blocking scope.** O1 blocks **Phase 3** (the shared builder's default-template value) and the
    **migrate phase** (which materializes descriptors carrying `--context <value>`). It does NOT block the
    **atomic Phase 1+2 defect-fix** — that phase's tests can use any placeholder context value (the
    materializer/proxy are value-agnostic; the test asserts that *whatever* `DaemonTemplate.Context` holds
    is appended verbatim as `--context <value>`).
- **O2 — client set for the router rewrite (the GUI-port half is RESOLVED).** Which installed clients get
  rewritten to `/serena/mcp` — derive from the operator's installed clients × the legacy serena bindings
  (claude-code, codex-cli, cursor, vscode, gemini-cli, qwen-cli, antigravity), NOT hard-coded. **The
  GUI-port discovery mechanism is no longer open**: §5 resolves it as live-pidport + readiness-ping,
  fail-closed ("start the GUI first"), mirroring the G4 reconcile precedent
  ([internal/cli/install.go:348-374](../../../internal/cli/install.go)). O2 blocks **Phase 4**
  (client-reconcile) only.
- **O3 — descriptor delivery**: this design picks `--task-name` lookup over inline `--spec` JSON (§2.3).
  Confirm before implementation, since it touches the supervisor argv shape. Blocks the atomic Phase 1+2
  (the `--task-name` flag is added there).

## 10. Claims (falsifiable guarantees this design makes)

This is the primary input to `architecture-reviewer`.

1. **The proxy stops reading the manifest at runtime.** After the atomic Phase 1+2,
   `internal/cli/daemon_serena.go` contains no `ManifestGet`/`ParseManifest` call and no
   `m.Kind`/`m.DaemonTemplate`/`m.Transport` *manifest* gate; the proxy's only runtime inputs are its
   descriptor's `RuntimeSpec` and the vault. (The proxy may keep a cheap descriptor-internal consistency
   assertion per §3.2, which reads no manifest.)
2. **`SupervisorDaemon` is extended additively only.** `RuntimeSpec` is a pointer with `omitempty`; existing
   `supervisor-intent.json` files round-trip through a *new-binary* `ReadSupervisorIntent` unchanged (nil
   spec), and no existing `SupervisorDaemon` field is renamed, retyped, or removed. (Old-binary forward
   reads are handled by the §7.1 upgrade/restart gate, not by the schema alone.)
3. **The embed-first manifest precedence is NOT changed.** `loadManifestYAMLEmbedFirst` and
   `manifestDirForTests` are untouched; no disk-wins mode is introduced; the blast radius does not extend to
   any other embedded server.
4. **The two-pass `executeInstallTo` and the deferred-start contract are NOT reworked.** The redesign calls
   `InstallParsedManifest` with the same opts shape and the same deferred-start contract. The only changes
   inside the api package are: (i) `BuildSupervisorDaemonsForSerena` materializes `RuntimeSpec` (and
   appends `--context`); (ii) the `SupervisorDaemon` struct gains `RuntimeSpec`; (iii) the
   `InstallParsedManifest` contract gate gains a `transport == native-http` clause alongside its existing
   kind + `daemon_template` check (§3.1) — an additive validation, not a change to the seam's
   write/rollback/deferred-start shape.
5. **`DaemonTemplate.Context` becomes load-bearing (value-agnostic).** After the materializer change, the
   materialized descriptor's `ChildArgs` contains `--context <DaemonTemplate.Context>` (appended, not a
   template token) for every serena workspace daemon; the field is no longer decorative. The design does
   not fix the *value* (O1).
6. **One owner for port-pool/template policy.** After Phase 3, `workspace register`, the redesigned migrate,
   and E.2 read the effective serena `DaemonTemplate` from a single shared builder/service; no consumer
   re-implements the embed-first `serenaPortPool` fail-closed read.
7. **Migration can establish an empty dynamic-pool state.** The redesigned migrate does not refuse a
   zero-workspace registry; it installs the dynamic-pool intent with zero daemon rows and exits success.
8. **The G4 hub resolver is NOT taught about template-only serena manifests.** The serena client routing
   goes exclusively through the `/serena/mcp` registry-driven router; `BuildResolverSnapshotFromManifests`
   and `manifestHasScheduledDaemon` are unchanged by the client-reconcile work.
9. **Client-reconcile precedes legacy-endpoint removal.** No legacy `localhost:9121` serena entry is removed
   from any client config until that client's entry has been successfully rewritten to the router endpoint.
10. **Secrets remain cleartext-free on disk.** `RuntimeSpec.EnvRefs` carries `secret:KEY` verbatim; secret
    resolution happens only in-process in the proxy.
11. **native-http is enforced at build/install time.** After the atomic Phase 1+2, a non-native-http
    dynamic-pool manifest is rejected before any descriptor is materialized — the gate lives in
    `BuildSupervisorDaemonsForSerena` and in `InstallParsedManifest`'s contract gate (§3.1), so removing the
    proxy's runtime transport check does not open a non-native-http spawn path.
12. **No old supervisor reads a `runtime_spec` file.** Any migrate/install that first writes a `runtime_spec`
    drives the existing cold-restart upgrade flow (§7.1) so the binary that next reconciles the intent is the
    new one; if the prior supervisor cannot be quiesced/exited, the operation fails loud rather than
    committing an intent a stuck old supervisor would silently ignore.
13. **Argv, top-level descriptor fields, and `RuntimeSpec` are self-consistent.** The proxy fails loud if
    `--task-name`, `--workspace`/`--port`, the top-level `SupervisorDaemon.Workspace`/`Port`, and
    `RuntimeSpec.WorkspacePath`/`ExternalPort` disagree (§3.2) — no silent reconcile to one side.

## 11. Protected surfaces (must remain untouched if this design is followed)

- `internal/api/manifest_source.go` — embed-first precedence (`loadManifestYAMLEmbedFirst`,
  `listManifestNamesEmbedFirst`, `manifestDirForTests`). NO disk-wins mode.
- `internal/api/install_parsed_manifest.go` — the seam SHAPE: the folded flock, the deferred-start
  contract, the read-merge-write rollback, the dry-run preview. These are unchanged. The ONE permitted edit
  is **additive validation**: a `transport == native-http` clause added to the up-front contract gate
  (currently kind + `daemon_template` at
  [internal/api/install_parsed_manifest.go:116-118](../../../internal/api/install_parsed_manifest.go),
  §3.1). This tightens admission only — it rejects a manifest that should never have reached the seam; it
  does not alter the write/rollback/deferred-start behavior for the manifests that pass.
- `internal/api/hub_mcp_resolver.go` — the G4 binding topology builder. Serena routing does not flow through
  it.
- `internal/cli/install.go:520` `manifestHasScheduledDaemon` — the hub reconcile filter. Unchanged by serena
  client-reconcile.
- The two-pass `executeInstallTo` and the `StartTasks` gate — unchanged.
- v0.4.x rollback byte-symmetry (`daemon-intent.json`, `managed-entries.json`, `watchdog-state.json`).

## Gate

- Traceable to accepted research facts and verified against merged code at HEAD `4960d61`: PASS.
- Alternatives, interfaces, extension seams, dependency direction, blast radius, failure modes,
  observability, and security are explicit: PASS.
- The three codex blockers are resolved: Phase 1+2 are merged atomically and the `--task-name` flag lands
  with the descriptor read (§2.2/§4); the native-http gate is specified at two build/install points (§3.1);
  E.2 auto-register (codex "Phase 6", plan Phase 5) hard-depends on the atomic 1+2 (§6/plan). The
  cross-version upgrade gate (§7.1), the nil-spec heal
  path (§4), the `--context` single-mechanism (§3/§5), the GUI-port live-pidport discovery (§5/O2), and the
  descriptor/flag consistency contract (§3.2) are all specified: PASS.
- No implementation code (only struct shapes + pseudocode flows): PASS.
- Open questions remaining: **O1 (single context value) BLOCKS Phase 3 + the migrate phase** (not the
  atomic 1+2 defect-fix); **O2 (client set) BLOCKS Phase 4** (its GUI-port half is resolved); **O3
  (`--task-name` delivery) BLOCKS the atomic 1+2** (the flag is added there).

**GATE DECISION: PASS** — proceed to the phased plan. The atomic Phase 1+2 defect-fix needs only O3 (and a
placeholder context value for tests). O1 must close before Phase 3 + migrate; O2 before Phase 4.

## Terms and Abbreviations

- **catalog / embedded manifest**: the `servers/<name>/manifest.yaml` compiled into the binary via
  `//go:embed`; the shipped default, read at build/register/migrate time only under this design.
- **descriptor**: a `SupervisorDaemon` entry in `supervisor-intent.json` — the per-daemon spec the
  supervisor execs.
- **`DaemonRuntimeSpec` / RuntimeSpec**: the new additive sub-struct on `SupervisorDaemon` carrying the
  materialized child command/args/env/ports the launcher needs without reading the manifest.
- **dynamic pool**: the serena architecture of one long-lived daemon per registered workspace (1:1 with
  active workspaces), fronted by the router; opposed to the legacy global 2-daemon model.
- **embed-first precedence**: `loadManifestYAMLEmbedFirst`'s rule that the embedded manifest wins and disk is
  only a fallback when the embed read fails.
- **fan-out**: `BuildSupervisorDaemonsForSerena` materializing one descriptor per registered serena
  workspace.
- **G4 hub resolver**: the unified-hub MCP routing surface (`/clients/<id>/mcp`) whose bindings come from
  `ClientBindings + Daemons`; distinct from the serena router.
- **`InstallParsedManifest`**: the merged (PR #244) workspace-scoped-only in-process install seam that writes
  `supervisor-intent.json` and defers daemon starts to the reconciler.
- **materialize / materializer**: the build-time step that resolves a template (`DaemonTemplate` +
  workspace) into a concrete `RuntimeSpec`.
- **MCP**: Model Context Protocol; the tool/server protocol clients speak.
- **native-http transport**: a server that speaks MCP over a loopback HTTP port; serena's transport.
- **proxy / serena-proxy**: the `mcphub daemon serena-proxy` launcher that runs per workspace; it spawns the
  upstream serena child and reverse-proxies an external port to the child's internal port.
- **router (`/serena/mcp`)**: the path-aware HTTP handler on the GUI server that resolves the target
  workspace from a tool's path-arg (or sticky session) and forwards to that workspace's daemon, reading the
  live workspace registry.
- **`secret:KEY`**: a vault reference placeholder in env values; resolved in-process, never persisted in
  cleartext.
- **supervisor-intent.json**: the v0.5.0 canonical runtime intent file listing every daemon descriptor;
  operator runtime source of truth.
- **workspace registry (workspaces.yaml)**: the per-(workspace, language) registry; serena rows carry
  `Language == @serena` (the sentinel).
