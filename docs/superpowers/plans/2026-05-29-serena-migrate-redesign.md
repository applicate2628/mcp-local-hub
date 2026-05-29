# Plan: Serena dynamic-pool migrate redesign (descriptor-driven proxy)

> **Status**: PLAN — implementation-ready (rev 2), pending O1/O2/O3 resolution (design §9).
> **Rev 2** closes the codex design-review REVISE
> (`.scratch/codex-prompts/migrate-redesign-design-review-20260529-111125.out`): the old Phase 1
> (materializer) and Phase 2 (proxy) are **merged into one atomic phase** because a Phase-1 descriptor
> carrying `--task-name` would die on the proxy's unknown-flag rejection before Phase 2 lands; the
> native-http build/install gate and the descriptor/flag consistency contract land in that same atomic
> phase; the supervisor upgrade/restart gate is added; Phase 6 (E.2) hard-depends on the atomic phase;
> the false "1+2 fixes any already-installed dynamic-pool intent" claim is corrected; O1's blocking
> scope is corrected (it blocks the builder + migrate phases, NOT the atomic defect-fix).
> **Design contract**: [docs/superpowers/specs/2026-05-29-serena-migrate-redesign-descriptor-proxy.md](../specs/2026-05-29-serena-migrate-redesign-descriptor-proxy.md).
> **Parent plan**: [docs/superpowers/plans/2026-05-20-serena-supervisor-unified.md](2026-05-20-serena-supervisor-unified.md)
> (this supersedes §D.3; §E.2 + §G.1/§G.2 depend on this).
> **Builds ON (do not rework)**: PR #244 — `InstallParsedManifest`, two-pass `executeInstallTo`,
> `BuildSupervisorDaemonsForSerena`, the POSIX maintenance reaper.
> **Verified against HEAD**: `4960d61` (branch `feat/serena-d3-install-parsed-manifest`).

## Repo-standard gates (every phase)

Per [CLAUDE.md](../../../CLAUDE.md) "PR review + merge workflow", before every push:

```bash
go build ./... && go vet ./... && go test -count=1 -timeout 5m ./...
go test -tags=test_state_path_env -count=1 -timeout 5m ./internal/api/ ./internal/cli/
```

Then: sweep stray `mcphub.exe` (identity-gated — only kill `mcphub.exe gui...` rows or test-binary
children; never blanket-kill the operator's running daemons, per the MEMORY.md KOSYAK), push, open PR,
drive the Codex bot to PASS, deep-security review, merge.

### Cross-platform test constraints (load-bearing)

- **POSIX `scheduler.New()` returns "not implemented"** — `internal/scheduler/scheduler_linux.go` +
  `scheduler_darwin.go`. Any test that reaches the real scheduler 500s / errors on Linux/macOS. The GUI
  E2E job is pinned `windows-latest` for this reason (CLAUDE.md "GUI E2E tests" §). Keep new unit tests
  scheduler-free, or gate scheduler-touching tests behind Windows. PR #244 r6 hit this exact gotcha.
- **POSIX-tagged tests run via `GOOS=linux`/WSL** — the supervisor/state-file tests use build tags;
  run the POSIX matrix under WSL or `GOOS=linux go test`. The `test_state_path_env` tag enables the
  env-fallback state-dir seam (`internal/api/state_paths_envfallback.go`) so tests can redirect the
  state dir; release builds exclude it.
- **`InstallParsedManifest` tests** use the `MCPHUB_MANIFEST_DIR_OVERRIDE` seam
  ([internal/api/manifest_source.go:18-20](../../../internal/api/manifest_source.go)) to inject a hermetic
  workspace-scoped manifest and `t.TempDir()` workspaces with `WorkspaceKey`/`SerenaLanguageSentinel`
  rows (precedent: `internal/api/install_parsed_manifest_test.go`). New tests follow that pattern.
- **The known flaky `cli supervise IPC` full-suite tests** (`work-items/bugs/2026-05-29-cli-supervise-ipc-tests-flaky-in-full-suite.md`)
  are pre-existing and unrelated; do not let them mask a real regression — run the affected new tests in
  isolation to confirm green.

---

## Phase 0 — Resolve open questions (no code)

**Scope**: get user/codex decisions on design §9 O1/O2/O3.
**Files**: none (decision capture in the design doc + this plan).
**Dependencies**: none. Blocks: **O3** the atomic Phase 1 (the `--task-name` flag shape); **O1** Phase 2
(shared builder default context) and Phase 4 (migrate, which materializes `--context <value>`); **O2**
Phase 3 (client-reconcile client set — its GUI-port half is already resolved in design §5).
**Acceptance**:
- **O1** (single context value) decided and validated against Serena `--context` behavior AND reconciled
  with the in-flight HEAD `servers/serena/manifest.yaml` (HEAD = `codex`; working tree mid-edit;
  `ide-assistant` is NOT parent-plan-sourced — design §9 O1). Recorded in the design doc.
- **O2** (client set for the router rewrite) decided (installed-clients × legacy-serena-bindings).
- **O3** (`--task-name` lookup vs inline `--spec`) confirmed.

**Note on O1 scope**: O1 does NOT block the atomic Phase 1 defect-fix — Phase 1 tests use a placeholder
context value (the materializer/proxy are value-agnostic). O1 must close before Phase 2 (builder default)
and Phase 4 (migrate).
**Shippable**: N/A (gate).

---

## Phase 1 — Atomic: descriptor runtime spec + materializer + proxy reads it + native-http gate + consistency contract

> **This is the merged old-Phase-1 + old-Phase-2, plus the native-http gate and the descriptor/flag
> consistency contract. It is ONE PR.** It MUST be atomic: the materializer appends `--task-name` to the
> wrapper `Args`, and the proxy today rejects unknown flags (it parses only `--port`/`--workspace`/`--server`
> — [internal/cli/daemon_serena.go:176-178](../../../internal/cli/daemon_serena.go)). A Phase-1-only
> descriptor with `--task-name`, spawned before the proxy learns the flag, would die on startup. Merging
> also avoids shipping the materializer inert and avoids landing an unread `runtime_spec` field that an old
> proxy would not consume. **This is the phase that actually fixes the verified defect.**

**Scope**:
1. Add `DaemonRuntimeSpec` to `SupervisorDaemon`; make `BuildSupervisorDaemonsForSerena` materialize it
   (child command/args incl. `--project` + appended `--context`, raw env refs, internal+external port,
   workspace) AND append `--task-name <self>` to the wrapper `Args`.
2. Add the **native-http build/install gate** (design §3.1) at both points: `BuildSupervisorDaemonsForSerena`
   and `InstallParsedManifest`'s contract gate.
3. Rewrite `serena-proxy` startup to read its `RuntimeSpec` by `--task-name`, drop the `ManifestGet`
   re-read and the kind/template/transport manifest gates, add the `--task-name` flag, the fail-loud
   nil/unsupported-spec guard, and the **descriptor/flag consistency assertion** (design §3.2).

**Files**:
- `internal/api/supervisor_intent.go` — add `DaemonRuntimeSpec` struct + `RuntimeSpec *DaemonRuntimeSpec`
  field on `SupervisorDaemon` (additive, `omitempty`).
- `internal/api/supervisor_intent_build.go` — `BuildSupervisorDaemonsForSerena`:
  - **transport gate**: return nil (or have the install/migrate caller fail loud) when
    `m.Transport != config.TransportNativeHTTP` — it already owns the kind/template guards
    ([internal/api/supervisor_intent_build.go:172-180](../../../internal/api/supervisor_intent_build.go)).
  - materialize `RuntimeSpec`: expand `${workspace.path}` over `BaseArgs ++ ExtraArgsTemplate` via
    `config.ExpandWorkspacePathTokens` ([internal/config/manifest_workspace.go:38-45](../../../internal/config/manifest_workspace.go)),
    then **append** `--context <DaemonTemplate.Context>` (NOT a template token — design §5); compute
    `UpstreamPort = ws.Port + config.NativeHTTPInternalPortOffset` (=10000,
    [internal/config/manifest.go:36](../../../internal/config/manifest.go)); clone env.
  - append `--task-name <SerenaTaskNameForWorkspace(ws.WorkspacePath)>` to the wrapper `Args` (lines
    212-217) so the proxy can find its own descriptor.
  - update the now-true self-sufficiency comment (lines 136-140).
- `internal/api/install_parsed_manifest.go` — add `transport == native-http` to the contract gate
  alongside the existing kind + `daemon_template` check
  ([internal/api/install_parsed_manifest.go:116-118](../../../internal/api/install_parsed_manifest.go)).
  This is **additive admission tightening only** — the write/rollback/deferred-start shape is unchanged.
- `internal/cli/daemon_serena.go` — replace lines 87-136 (the `ManifestGet`→parse→gates→childArgs block)
  with: descriptor load by `--task-name` from `supervisor-intent.json`; the §3.2 consistency assertion
  (`--task-name`/`--workspace`/`--port` vs `RuntimeSpec`/top-level fields); `RuntimeSpec`
  nil/unsupported-version fail-loud guard (NO manifest fallback); `secret:` resolution over
  `spec.EnvRefs`; `childArgs = spec.ChildArgs ++ [--port, UpstreamPort]`;
  `daemon.NewHTTPHost{Command: spec.ChildCommand, ...}`. Add the `--task-name` flag.
- tests: `internal/api/supervisor_intent_build_test.go`, `internal/api/install_parsed_manifest_test.go`,
  `internal/cli/daemon_serena_test.go` — see test strategy.

**Dependencies**: Phase 0 (**O3** descriptor-delivery flag shape). A placeholder context value suffices
for tests, so **O1 is NOT required here**.

**Acceptance criteria**:
- `RuntimeSpec` is a pointer with `omitempty`; an existing `supervisor-intent.json` (no `runtime_spec`)
  decodes through a new-binary `ReadSupervisorIntent` (`DisallowUnknownFields`,
  [internal/api/supervisor_intent.go:107](../../../internal/api/supervisor_intent.go)) with nil spec, no
  error.
- For a workspace-scoped native-http manifest with `DaemonTemplate.Context == <X>` and
  `ExtraArgsTemplate == ["--project", "${workspace.path}"]`, the materialized `ChildArgs` contains BOTH
  `--project <canonical-workspace>` AND a trailing `--context <X>` (finding #4 closed, value-agnostic).
  `ChildArgs` does NOT include `--port` (the proxy appends the internal port). The wrapper `Args` end with
  `--task-name <SerenaTaskNameForWorkspace>`.
- `RuntimeSpec.EnvRefs` carries `secret:KEY` verbatim (unresolved); `ChildCommand == m.Command`;
  `UpstreamPort == ExternalPort + 10000`; `ExternalPort == ws.Port`; `WorkspacePath == ws.WorkspacePath`.
- A build-time invariant asserts `RuntimeSpec.ExternalPort == SupervisorDaemon.Port` and
  `RuntimeSpec.WorkspacePath == SupervisorDaemon.Workspace`.
- **native-http gate**: `BuildSupervisorDaemonsForSerena` returns nil (or caller fails loud) for a
  non-native-http `daemon_template` manifest; `InstallParsedManifest` rejects a `stdio-bridge` (or
  remote-http) + `daemon_template` + `kind: workspace-scoped` manifest BEFORE any mutation — note such a
  manifest passes `ServerManifest.Validate` today ([internal/config/manifest.go:294](../../../internal/config/manifest.go),
  daemon_template only rejected for kind!=workspace-scoped / remote-http at
  [:306-311](../../../internal/config/manifest.go)), so this gate is the only enforcer once the proxy's
  runtime check is removed.
- `internal/cli/daemon_serena.go` contains NO `ManifestGet`, NO `ParseManifest`, NO
  `m.Kind`/`m.DaemonTemplate`/`m.Transport` *manifest* gate (claim #1).
- The proxy spawns the upstream child from `spec.ChildCommand` + `spec.ChildArgs ++ --port <UpstreamPort>`;
  the resulting argv carries `--context` and `--project` (verified end-to-end against a hermetic
  descriptor).
- **fail-loud guard**: a nil `RuntimeSpec`, an unsupported `SpecVersion`, OR a §3.2 consistency mismatch
  makes the proxy exit non-zero with an operator-actionable launch-failure log ("reinstall the serena
  dynamic pool" / naming the disagreeing fields) — NO silent fallback to a manifest read.
- **consistency contract**: with a descriptor whose `--port` argv disagrees with `RuntimeSpec.ExternalPort`
  (or `--workspace` disagrees with `RuntimeSpec.WorkspacePath`), the proxy fails loud.
- `secret:` env resolution behavior is unchanged (now over `spec.EnvRefs`).
- The reverse-proxy + shutdown semantics (external→internal, `ChildExited`, ctx cancel) are byte-for-byte
  the same downstream of `NewHTTPHost`.
- `BuildSupervisorDaemonsForSerena` still returns nil for zero workspaces / non-workspace-scoped manifest
  (no regression to its existing early-return guards).

**Test strategy** (unit; scheduler-free, pure where possible):
- `TestBuildSerenaDaemons_MaterializesRuntimeSpec_AppendsContextAndProject` — `ChildArgs` ends with
  `--context <X>`, contains `--project <canonical>`, no `--port`.
- `TestBuildSerenaDaemons_AppendsTaskNameToWrapperArgs` — wrapper `Args` end with `--task-name <canonical
  serena task name>`.
- `TestBuildSerenaDaemons_RuntimeSpecEnvRefsUnresolved` — `secret:` left verbatim (deep-sec: no secret in
  `ChildArgs`).
- `TestBuildSerenaDaemons_RuntimeSpecPortMath` — `UpstreamPort = ExternalPort + NativeHTTPInternalPortOffset`.
- `TestBuildSerenaDaemons_NonNativeHTTP_ReturnsNilOrFailsLoud` — the transport gate.
- `TestInstallParsedManifest_RejectsStdioBridgeDaemonTemplate` — the install-time transport gate; assert
  rejection BEFORE any state write (no `supervisor-intent.json` mutation).
- `TestSupervisorIntent_RoundTrip_NilRuntimeSpecLegacyFile` — decode a pre-RuntimeSpec JSON fixture; nil
  spec, no `DisallowUnknownFields` error; re-marshal omits `runtime_spec`.
- `TestSupervisorIntent_RoundTrip_WithRuntimeSpec` — marshal→decode→equal.
- `TestSerenaProxy_LoadsRuntimeSpecByTaskName` — seed a hermetic `supervisor-intent.json` (env-fallback
  state dir under `test_state_path_env`); assert the proxy resolves its descriptor + spec.
- `TestSerenaProxy_BuildsChildArgsFromSpec_AppendsInternalPort` — final argv == `spec.ChildArgs ++ [--port,
  UpstreamPort]`; `--context`+`--project` present.
- `TestSerenaProxy_NilRuntimeSpec_FailsLoud` — descriptor with nil spec → non-zero exit + launch-failure
  log; assert NO manifest read occurred (e.g. `MCPHUB_MANIFEST_DIR_OVERRIDE` pointing at an empty dir that
  would error if read).
- `TestSerenaProxy_UnsupportedSpecVersion_FailsLoud`.
- `TestSerenaProxy_ArgvSpecMismatch_FailsLoud` — `--port`/`--workspace` argv disagreeing with `RuntimeSpec`
  → fail loud (the §3.2 contract).
- Extract the descriptor→argv assembly + the consistency assertion into small pure helpers so they are
  testable without spawning `daemon.HTTPHost` or binding a port. Keep tests scheduler-free.

**Quality gates**: build+vet+test clean on Windows AND `GOOS=linux`. Bot PASS. Deep-sec: (a) no secret
leak into `ChildArgs` (secrets must be in `EnvRefs` only); (b) the fail-loud guard must be airtight — NO
path where a missing/inconsistent spec silently re-reads the embedded legacy manifest; (c) the native-http
gate cannot be bypassed by a manifest that slips past `Validate`. **Manual smoke on a packaged binary
against ≥1 registered workspace** (the original defect's repro env) — confirm the per-workspace daemon now
starts and the upstream child receives `--context`/`--project`.

**Shippable**: **Independently shippable, single atomic PR** — it both adds the materializer/spec and makes
the proxy consume it, so nothing is ever inert and no descriptor carries an unread `--task-name`. **HIGHEST
RISK** (see §Highest-risk phase). After this PR, the verified defect is fixed for any **newly installed**
dynamic-pool intent; pre-existing nil-spec rows are healed only via reinstall behind the §7.1 upgrade gate
(see Phase 4 + the corrected migration claim below).

---

## Phase 2 — Shared dynamic-pool builder/service (break register↔migrate cycle)

**Scope**: introduce a single owner of serena default port-pool + template policy, consumed by
`workspace register`, the redesigned migrate (Phase 4), and E.2 (Phase 5). Breaks finding #3's cycle.

**Files**:
- `internal/api/serena_dynamic_pool.go` (new) — a service/function returning the effective serena
  `config.DaemonTemplate` (port_pool, context, extra_args_template): from the embedded manifest if it
  already declares `daemon_template`, else from a built-in dynamic-pool default. The default's
  `extra_args_template` is `[--project, ${workspace.path}]` (NO `--context` token — design §5); the
  default's `Context` holds the **O1 value**. Also exposes a "build the in-memory dynamic-pool
  `*config.ServerManifest`" helper for migrate/E.2 (always `transport: native-http`).
- `internal/cli/workspace_cmd.go` — replace `serenaPortPool` (lines 73-81, which fails closed on missing
  `daemon_template.port_pool`, called at [:184-192](../../../internal/cli/workspace_cmd.go)) + the
  `loadSerenaManifestForCLI` embed read (lines 50-66) with a call to the new service. `register` no longer
  fails-closed on the legacy `kind: global` embed.
- tests in `internal/api/serena_dynamic_pool_test.go` (new) + `internal/cli/workspace_cmd_test.go` (extend).

**Dependencies**: Phase 0 (**O1** context value — the builder bakes it into the default template).
Independent of Phase 1 at the code level (different files), but logically Phase 4 (migrate) depends on
BOTH Phase 1 (materializer) AND Phase 2 (builder). Can be developed in parallel with Phase 1 once O1 closes.

**Acceptance criteria**:
- A single function/service answers "effective serena `DaemonTemplate`"; `workspace register`, migrate, and
  E.2 all call it (claim #6). No consumer re-implements the embed-first `serenaPortPool` fail-closed read.
- `mcphub workspace register <path>` succeeds against the *current* embedded `kind: global` manifest (the
  cycle is broken): it allocates a port from the service's effective port-pool, not from the absent
  `daemon_template.port_pool`.
- The built-in default template carries the resolved O1 context and `extra_args_template: [--project,
  ${workspace.path}]` (so the synthesized manifest passes `ServerManifest.Validate`
  [internal/config/manifest.go:287](../../../internal/config/manifest.go) — `${workspace.path}` token
  present per the D.1 validator).
- The "build in-memory dynamic-pool manifest" helper produces a manifest that passes `Validate()` as
  `kind: workspace-scoped` + `transport: native-http` + `daemon_template` (no top-level
  `port_pool`/`languages`/`daemons`).

**Test strategy** (unit; pure where possible):
- `TestSerenaDynamicPool_EffectiveTemplate_FromBuiltinDefault_WhenEmbedIsGlobal` — with the current
  embedded `kind: global` serena manifest, the service returns the built-in default template (no
  fail-closed).
- `TestSerenaDynamicPool_EffectiveTemplate_PrefersEmbedDaemonTemplate_WhenPresent`.
- `TestSerenaDynamicPool_BuildInMemoryManifest_PassesValidate_NativeHTTP` — also asserts transport.
- `TestWorkspaceRegister_SucceedsAgainstLegacyGlobalEmbed` — the cycle-break regression guard; uses the
  `MCPHUB_MANIFEST_DIR_OVERRIDE`/`loadSerenaManifestForCLI` seam to inject the legacy `kind: global` shape
  and asserts register allocates a port. Scheduler-free.

**Quality gates**: build+vet+test clean (Windows + Linux). Bot PASS. Confirm the `serenaPortPool` removal
does not break existing workspace_cmd tests that exercise the test-injected `daemon_template.port_pool`
path — those tests migrate to the service.

**Shippable**: **Independently shippable PR** (changes `workspace register`'s policy source + adds a
service; does not require Phase 1/3/4/5). **Blocked by O1** (the default-template context value). Medium
risk (touches the operator-facing `register` command).

---

## Phase 3 — Client-reconcile to `/serena/mcp` (live-pidport URL discovery)

**Scope**: add an explicit dynamic-pool client-reconcile path that rewrites each managed client's serena
entry to the constant router URL `http://127.0.0.1:<gui-port>/serena/mcp` (relay form for Antigravity)
BEFORE legacy `localhost:9121` endpoints are removed. Closes finding #5 + parent plan §G.2.

**Files**:
- `internal/api/serena_client_reconcile.go` (new) — the reconcile function: discover the GUI port from the
  **live pidport** + readiness ping (design §5), rewrite/insert the serena entry to the router URL for the
  in-scope client set, record the managed-entries marker (`RecordManagedEntry` precedent
  [internal/api/migrate.go:175](../../../internal/api/migrate.go)), and optionally stamp the serena
  registry row's `ClientEntries` ([internal/api/workspace_registry.go:56](../../../internal/api/workspace_registry.go)).
  Antigravity keeps the relay triple ([internal/clients/clients.go:23-35](../../../internal/clients/clients.go))
  but points upstream at the router.
- `internal/clients/*` — no new adapter; reuse `MCPEntry`/`AddEntry`/`BackupKeep`. Antigravity relay
  upstream string changes from `9121` to the router endpoint.
- tests in `internal/api/serena_client_reconcile_test.go` (new).

**Dependencies**: Phase 0 (**O2** client set; the GUI-port half is resolved in design §5). Independent of
Phases 1/2 at the code level. Phase 4 (migrate) calls this. Can be developed in parallel with Phases 1/2
once O2 closes.

**Acceptance criteria**:
- **GUI-port discovery is live-pidport + readiness ping, fail-closed.** The reconcile reads the bound port
  from the pidport file (`ReadPidport` — [internal/gui/single_instance.go:92-110](../../../internal/gui/single_instance.go))
  — NOT from the persisted setting, because the actual bound port is written only AFTER startup
  ([internal/cli/gui.go:413-417](../../../internal/cli/gui.go) writes `s.Port()`) and the resolution order
  is flag>setting>auto ([internal/cli/gui.go:35-52](../../../internal/cli/gui.go)). It pings the bound port
  to confirm the GUI is live (G4 precedent: [internal/cli/install.go:348-374](../../../internal/cli/install.go)).
  If the pidport is absent/stale or the ping fails, the reconcile fails closed with "start the GUI first" —
  it does NOT write a guessed URL.
- For each in-scope client, the serena entry URL becomes the constant router endpoint
  `http://127.0.0.1:<live-gui-port>/serena/mcp` (NOT a per-workspace port); one entry per client,
  workspace-agnostic.
- The G4 hub resolver and `manifestHasScheduledDaemon` are UNCHANGED (claim #8) — serena routing flows only
  through the registry-driven `/serena/mcp` router, never the G4 binding topology.
- Legacy `localhost:9121` removal is ordered AFTER a successful router rewrite for that client (claim #9):
  a per-client rewrite failure leaves that client on the still-functional legacy endpoint, surfaced in the
  `MigrateReport.Failed`-shaped report.
- Antigravity's relay entry points its upstream at the router endpoint, preserving the stdio-relay shape.
- The managed-entries marker records each rewritten (client, "serena") tuple (demigrate symmetry).

**Test strategy** (unit; the adapters already have hermetic test fixtures per `internal/clients/*_test.go`):
- `TestSerenaClientReconcile_DiscoversPortFromLivePidport_FailsClosedWhenAbsent` — assert the URL port
  comes from a seeded pidport; assert fail-closed (no rewrite) when the pidport is missing or the ping
  fails (inject a non-listening port).
- `TestSerenaClientReconcile_RewritesToRouterURL_PerClient` — URL == router endpoint for each client shape
  (claude-code/codex-cli/cursor/vscode/gemini-cli/qwen-cli).
- `TestSerenaClientReconcile_Antigravity_RelayUpstreamIsRouter`.
- `TestSerenaClientReconcile_RecordsManagedEntryMarker`.
- `TestSerenaClientReconcile_LegacyEndpointRemovedOnlyAfterRewriteSuccess` — inject a rewrite failure for
  one client; assert that client keeps the legacy entry and the failure is reported.
- `TestSerenaClientReconcile_DoesNotTouchG4Resolver` — assert `BuildResolverSnapshotFromManifests` /
  `manifestHasScheduledDaemon` are not invoked (structurally, the reconcile lives in its own file and does
  not import the resolver path).
- Scheduler-free (client config writes go through the adapters' hermetic temp-home fixtures).

**Quality gates**: build+vet+test clean (Windows + Linux). Bot PASS. Deep-sec: the router URL is loopback;
confirm no remote/plaintext surface and that the live-pidport discovery cannot be spoofed into writing an
attacker-chosen URL (the readiness ping + loopback-only address bound this). Manual smoke: rewrite a
client, hit a serena tool with a path-arg, confirm the router forwards to the right workspace daemon.

**Shippable**: **Independently shippable PR** as a *new, unwired* reconcile function (inert until Phase 4
calls it; could be exposed behind an experimental flag for manual testing). **Blocked by O2** (client set).
Medium-high risk (touches client configs — the operator's load-bearing artifact). Order legacy-removal
carefully.

---

## Phase 4 — Re-add `mcphub migrate serena legacy-to-dynamic-pool` (+ supervisor upgrade/restart gate)

**Scope**: re-add the operator-facing migrate subcommand (removed in `a7dcbcd` — verified
"remove not-operator-complete migrate command; keep install foundation"), now functional end-to-end: it
builds the in-memory dynamic-pool manifest (Phase 2 builder), calls `InstallParsedManifest` (merged seam,
materializes `RuntimeSpec` via Phase 1), runs client-reconcile (Phase 3), and **drives the supervisor
upgrade/restart gate** (design §7.1) — without rewriting the disk manifest.

**Files**:
- `internal/cli/migrate_serena.go` (new — re-introduced) — the subcommand + the driver rollback stack
  (parent plan §D.3 outer/inner composition). Source-state detection per the parent plan §D.3 table.
- `internal/cli/migrate.go` or the command tree wiring — register the `serena legacy-to-dynamic-pool`
  subcommand (distinct from the generic `mcphub migrate <server>` at
  [internal/cli/migrate.go:15](../../../internal/cli/migrate.go)).
- **Upgrade/restart wiring** — the migrate driver invokes the existing cold-restart upgrade flow
  ([internal/cli/install_upgrade.go](../../../internal/cli/install_upgrade.go): IPC `quiesce-timers` → IPC
  `exit{graceful}` → force-kill fallback → start-new-supervisor) after the intent write + client-reconcile,
  so no old supervisor reads the new `runtime_spec` intent (design §7.1). It does NOT hand-roll a `taskkill`.
- tests in `internal/cli/migrate_serena_test.go` (new).

**Dependencies**: **Phase 1 (materializer + proxy) AND Phase 2 (builder) AND Phase 3 (client-reconcile)** —
all hard. Order: 1, 2, 3 → 4.

**Acceptance criteria**:
- The command does NOT write the disk manifest (the whole defect's root cause is avoided): it builds the
  manifest in memory and calls `InstallParsedManifest`.
- Source-state detection handles legacy-2-daemon / intermediate-unified / already-migrated (idempotent
  exit-0) / malformed (error) per the parent plan §D.3 table.
- Migration proceeds with an empty registry (claim #7): zero workspaces → installs the dynamic-pool intent
  with zero daemon rows, exits success.
- The driver rollback stack restores the registry on failure; `InstallParsedManifest`'s internal rollback
  owns the intent/scheduler/client sub-steps (no double-undo — parent plan §D.3 composition).
- Client-reconcile (Phase 3) runs before legacy `9121` removal.
- **Supervisor upgrade/restart gate (its own acceptance, design §7.1)**: after a `runtime_spec`-introducing
  migrate, no supervisor older than the new binary is left reading the intent (the cold-restart flow's
  `exit{graceful}` + force-kill fallback reaped the prior supervisor, the new one is started and
  reconcile-ready). If the prior supervisor cannot be quiesced/exited, the migrate **fails loud** rather
  than committing an intent a stuck old supervisor will ignore. The new supervisor re-materializes any
  pre-existing nil-spec serena rows BEFORE spawning.
- After a successful run on a packaged binary with ≥1 registered workspace, the (new-binary) supervisor
  reconciler spawns the per-workspace proxy from its `RuntimeSpec` and the daemon starts (the original
  defect is gone).

**Test strategy** (unit + manual):
- `TestMigrateSerena_AlreadyMigrated_Idempotent`.
- `TestMigrateSerena_Malformed_Errors`.
- `TestMigrateSerena_EmptyRegistry_InstallsZeroWorkspaceIntent` — claim #7 regression guard.
- `TestMigrateSerena_CallsInstallParsedManifest_NotDiskWrite` — assert the disk manifest is NOT rewritten
  (the verified-defect regression guard). Use the `MCPHUB_MANIFEST_DIR_OVERRIDE` seam + assert the on-disk
  manifest bytes are unchanged.
- `TestMigrateSerena_RollbackRestoresRegistry_OnInstallFailure`.
- `TestMigrateSerena_DrivesSupervisorRestart` — assert the cold-restart seam is invoked after intent write
  (mock the `install_upgrade` IPC deps — `QuiesceTimers`/`ExitGraceful`/start — per
  `internal/cli/install_upgrade_test.go` precedent); assert fail-loud when the prior supervisor cannot be
  exited.
- `TestMigrateSerena_NilSpecRowsHealedBeforeSpawn` — seed a pre-existing nil-spec serena descriptor; after
  migrate, assert the row carries a fresh `RuntimeSpec` (re-materialized via `InstallParsedManifest`,
  [internal/api/install_parsed_manifest.go:319-346](../../../internal/api/install_parsed_manifest.go)
  wholesale row replacement) and that the supervisor never spawns from the nil-spec row.
- **Avoid the scheduler**: a workspace-scoped manifest yields zero scheduler tasks, and
  `InstallParsedManifest` sets `SkipSchedulerPrune` for `DaemonTemplate` manifests
  ([internal/api/install_parsed_manifest.go:260-279](../../../internal/api/install_parsed_manifest.go)), so
  the migrate path is largely scheduler-free — but the test must run under `test_state_path_env` with a
  temp state dir (it writes `supervisor-intent.json`). Confirm Windows (cli job) + `GOOS=linux` both pass.
- Manual smoke (the load-bearing acceptance): run on the operator's machine against a real registered
  workspace, packaged binary, confirm supervisor restart + daemon start + router routing end-to-end.

**Quality gates**: build+vet+test clean (Windows + Linux). Bot PASS. Deep-sec on (a) the rollback
composition (no double-undo, no half-migrated state on partial failure); (b) the upgrade-gate fail-loud
path (no committed intent left for a stuck old supervisor). The migrate-serena IPC-adjacent full-suite
flake (`work-items/bugs/2026-05-29-...`) is pre-existing — confirm new tests green in isolation.

**Shippable**: **Must land after 1+2+3.** As the operator-facing entry point, it ships only once the
runtime (Phase 1), the cycle-break (Phase 2), and the client-reconcile (Phase 3) are merged. Medium risk
(mostly orchestration of already-tested pieces + the restart wiring).

---

## Phase 5 — E.2 auto-register builds on Phases 1+2 (Phase 1 is a HARD dependency)

**Scope**: wire auto-register-on-miss (parent plan §E.2) onto the descriptor + builder foundation: when
the `/serena/mcp` router's `ResolveByPath` returns `ErrWorkspaceNotFound`, survey languages, register the
workspace, synthesize the per-workspace descriptor in-memory via the builder + `InstallParsedManifest`, and
forward the request.

**Files**:
- `internal/gui/serena_router.go` or a sibling — the auto-register-on-miss branch (currently the router
  returns 503 `phase_e_status: deferred` [internal/gui/serena_router.go:196-221](../../../internal/gui/serena_router.go)).
- `internal/api/serena_dynamic_pool.go` (Phase 2) — reuse the in-memory manifest builder.
- `internal/api/install_parsed_manifest.go` — consumed as-is (the `Workspaces` snapshot now includes the
  newly auto-registered workspace).
- tests + (deferred) Playwright per the parent plan §E.2 test contract.

**Dependencies**: **Phase 1 (materializer + proxy) AND Phase 2 (builder)** — BOTH hard.
- **Phase 1 is a HARD dependency, not "ideally"** (codex Blocker 3): Phase 5's acceptance includes
  "supervisor spawns the daemon → forward". Without Phase 1, the spawned `serena-proxy` still re-reads the
  embedded legacy manifest and fails the kind/template gates
  ([internal/cli/daemon_serena.go:87-105](../../../internal/cli/daemon_serena.go)) — so the spawned daemon
  never comes up and the forward fails. The spawn→forward acceptance is unreachable without Phase 1.
- Order: 1, 2 → 5. Independent of Phases 3/4 at the code level.

**Acceptance criteria** (per parent plan §E.2):
- Unknown path → survey → register → synthesize descriptor (with `RuntimeSpec`) → `InstallParsedManifest` →
  **supervisor spawns the daemon (requires Phase 1) → forward**. Atomic: fully
  registered+spawned+responding, or fully rolled back.
- No-languages-detected → HTTP 422 with explicit message; spawn-failure → HTTP 503 + audit event + registry
  revert.
- Audit event `workspace-auto-registered` with `{path, languages, port, trigger_tool, trigger_path}`.
- Reuses the shared builder (claim #6) — no private embed read.

**Test strategy**:
- `TestAutoRegister_SuccessPath_FullFlow` (per parent plan; mock the spawn/forward where the real serena
  binary is unavailable, but the descriptor synthesized MUST carry a `RuntimeSpec` — assert it).
- `TestAutoRegister_NoLanguagesDetected_HTTP422`.
- `TestAutoRegister_SpawnFailure_RollsBack`.
- The router tests live in `internal/gui` and already use hermetic seams (`serenaRouterTestSeam`); the
  spawn is the hard part — gate the real-spawn assertion behind Windows + a real binary, unit-test the
  decision/rollback logic cross-platform.

**Quality gates**: build+vet+test clean (Windows + Linux). Bot PASS. The router auto-register touches the
live request path — deep-sec on the atomicity (no half-registered workspace on a failed spawn) and on the
auto-register-as-DoS angle (an attacker hitting many bogus paths should not exhaust ports — the survey/422
gate + the registry's port pool bound this).

**Shippable**: **Independently shippable PR after 1+2** (Phase 1 hard, not optional). Medium risk (live
request path + spawn). Can ship after or before the migrate command — it is an orthogonal consumer of the
same foundation, but it CANNOT ship before Phase 1.

---

## Dependency graph + ordering

```
Phase 0 (decisions) ─┬─[O3]─► Phase 1 (ATOMIC: descriptor+materializer+proxy+native-http gate+consistency)  [HIGHEST RISK]
                     │                                  │
                     │                                  ├───────────────────────────────┐
                     ├─[O1]─► Phase 2 (shared builder) ─┼──► Phase 4 (re-add migrate) ◄──┤ (also needs Phase 3)
                     │                                  │                                │
                     │                                  └──► Phase 5 (E.2 auto-register)  │
                     └─[O2]─► Phase 3 (client-reconcile) ──────────────────────────────► Phase 4
```

- **Critical path to the defect fix**: Phase 1 (ONE atomic PR — descriptor + proxy + gates). After it, the
  verified defect is fixed for any **newly installed** dynamic-pool intent. Pre-existing nil-spec rows are
  healed via reinstall behind the §7.1 upgrade gate (Phase 4), NOT automatically.
- **Critical path to operator-complete migrate**: 1, 2, 3 → 4.
- **Phase 5 (E.2) hard-depends on Phase 1** (spawn→forward needs the descriptor-reading proxy) AND Phase 2.
- **Parallelizable**: Phases 2 and 3 are independent of each other and of Phase 1 (different files); they
  can be developed concurrently once Phase 0 closes O1/O2.

## Shippable-PR summary

| Phase | Scope (one line) | Shippable as | Depends on | Blocking decision |
|---|---|---|---|---|
| 0 | Resolve O1/O2/O3 decisions | N/A (gate) | — | — |
| 1 | **ATOMIC** `DaemonRuntimeSpec` + materializer + proxy-reads-it + native-http gate + consistency contract — **fixes the defect** | Independent atomic PR | P0 | **O3** |
| 2 | Shared dynamic-pool builder; unblocks `workspace register` | Independent PR | P0 | **O1** |
| 3 | Client-reconcile to `/serena/mcp` (live-pidport URL; new, unwired) | Independent PR (inert until P4) | P0 | **O2** |
| 4 | Re-add `migrate serena legacy-to-dynamic-pool` + supervisor restart gate | Must land after 1+2+3 | P1, P2, P3 | O1 (via P2) |
| 5 | E.2 auto-register on the foundation | Independent PR after 1+2 | **P1 (hard), P2** | O1 (via P2) |

Note: there is no longer a standalone "proxy" phase that ships inert or that depends on a separate
"materializer" phase — the codex Blocker-1 atomicity is resolved by the merge. No phase is marked
"independently shippable, inert until …" for the descriptor/proxy pair anymore.

## Highest-risk phase

**Phase 1 (the atomic descriptor + materializer + proxy + gates)** — the merged old-1+2.

Why it is the highest risk:
- It is the phase that **actually changes runtime daemon startup** for every per-workspace serena daemon.
  Phases 2/3 are additive or operator-command policy; Phase 1 rewrites the live spawn path.
- The change removes the manifest read that has been the proxy's source of truth since the dynamic-pool
  branch existed. A wrong `RuntimeSpec` materialization surfaces ONLY at proxy spawn time — the failure
  mode is "daemon dies at startup", the same symptom as the original defect, so a regression here is easy
  to mistake for the original bug.
- It is **hard to fully E2E without a real serena binary** (the upstream child is `uvx … serena …`); the
  test strategy mitigates by extracting pure descriptor→argv + consistency-assertion helpers and asserting
  the fail-loud guard, but the final acceptance is a manual smoke on a packaged binary against the
  operator's real workspace — the exact environment where the original defect manifested (packaged binary,
  embedded manifest).
- The fail-loud-on-nil/inconsistent-spec boundary is security/correctness-load-bearing: any code path that
  silently falls back to a manifest read on a missing/inconsistent spec would re-introduce the embed-shadow
  defect under a new disguise.
- The native-http gate is the ONLY enforcer of native-http once the proxy's runtime check is removed (a
  `stdio-bridge` + `daemon_template` manifest passes `Validate` today — design §3.1); a gap here lets a
  non-native-http manifest reach the HTTP-reverse-proxy spawn path.

Mitigations: keep the descriptor→argv assembly + consistency assertion as pure, unit-tested helpers;
require the packaged-binary manual smoke as an explicit acceptance gate; deep-sec review focused on the
fail-loud guard's airtightness AND the native-http gate's completeness at both build/install points.

## Notes on the captured design vs merged code (for the implementer)

- The parent plan §D.3 describes a `--start-after-write` knob and a step that **rewrites the disk manifest**.
  Neither survived into the merged `InstallParsedManifest`: there is **no `StartAfterWrite` field** (the
  deferred-start is structural — [internal/api/install_parsed_manifest.go:39-46](../../../internal/api/install_parsed_manifest.go)),
  and the seam does **not** write the disk manifest. This redesign aligns with the merged reality (descriptor
  materialization, no disk rewrite); do NOT re-introduce the disk-manifest rewrite.
- The codex design-consult output references some now-stale paths (`internal/cli/migrate_serena.go:217/402`,
  `install_parsed_manifest.go:30` StartAfterWrite). Those refer to the removed migrate command and the
  removed knob; they are resolved by removal. Use the `file:line` citations in the design doc (verified at
  HEAD `4960d61`), not the consult output's line numbers.
- **The `runtime_spec` cross-version hazard**: `ReadSupervisorIntent` uses `DisallowUnknownFields`
  ([internal/api/supervisor_intent.go:107](../../../internal/api/supervisor_intent.go)). An OLD supervisor
  reading a NEW `runtime_spec` file fails the decode and keeps its stale cache
  ([internal/cli/supervise.go:760-778](../../../internal/cli/supervise.go)); the GUI adopts an
  already-running supervisor with no version check
  ([internal/cli/gui_supervisor_owner.go:93-97](../../../internal/cli/gui_supervisor_owner.go)). Phase 4's
  upgrade/restart gate (design §7.1) is mandatory for any `runtime_spec`-introducing install/migrate; do NOT
  ship the migrate without it.
- **Legacy nil-spec serena-proxy rows during a binary upgrade (bot PR #246 P1)**: an existing
  `supervisor-intent.json` written by a PRE-redesign `BuildSupervisorDaemonsForSerena` carries serena-proxy
  rows that end at `--port` with NO `--task-name` and NO `runtime_spec`. After a binary upgrade the supervisor
  keeps executing those old rows until a re-install re-materializes the intent, and the redesigned proxy's
  fail-loud-on-nil-spec would make each such row exit immediately and churn through restart backoff/quarantine.
  **Phase 1 guarantees only a CLEAN skip + a clear, operator-actionable signal**: the supervisor spawn path
  (`makeProductionSpawnFnWithStatePath` in [internal/cli/supervise.go](../../../internal/cli/supervise.go))
  SKIPS a nil-`RuntimeSpec` serena-proxy descriptor (identified by its `daemon serena-proxy` argv) and emits a
  `severity: warn, event: legacy-serena-descriptor-skipped` supervisor event naming the task, instead of
  exec'ing a doomed proxy. The proxy keeps its own nil-spec fail-loud as defense-in-depth. The **SMOOTH
  auto-migration — re-materializing legacy rows on `install --upgrade` so they spawn normally — is the cutover
  phase's upgrade-gate responsibility (design 7.1 / Phase 4), NOT this phase**; Phase 1 only removes the
  doomed-spawn churn and makes the required action visible. (Same bot round also added two related guards: the
  build/install-time empty-`daemon_template.context` reject, and the `MCPHUB_SUPERVISOR_INTENT_PATH` control
  channel the supervisor injects so the proxy's intent-path lookup is immune to the serena child's
  HOME/XDG overlay env.)
- The current embedded `servers/serena/manifest.yaml` is `kind: global`. **HEAD** has a single `unified`
  daemon on `--context codex` for all clients; the **working tree is mid-edit** on that file (reverted to
  the two-daemon split). Migrating that embed to the dynamic-pool shape (parent plan §G.1) is decoupled from
  this redesign and can follow later — runtime no longer reads it. The single context value is **O1** (NOT
  `ide-assistant` by default — that value is not parent-plan-sourced; HEAD mandates `codex`).

## Gate

- Each phase has explicit file scope, dependencies, acceptance criteria, test strategy, and quality gates: PASS.
- The three codex blockers are resolved: Phase 1+2 merged atomically (no inert descriptor / unknown-flag
  death); native-http gate placed at `BuildSupervisorDaemonsForSerena` + `InstallParsedManifest` (Phase 1);
  Phase 5 (E.2) HARD-depends on Phase 1 for spawn→forward.
- The majors are reflected: `--context` single-append mechanism (Phase 1/2), cross-version upgrade/restart
  gate (Phase 4 + design §7.1), nil-spec heal path + corrected claim (Phase 4), O1 made context-agnostic
  with corrected blocking scope (Phase 0/2/4), O2 GUI-port live-pidport discovery (Phase 3). The medium
  (descriptor/flag consistency) is in Phase 1.
- Cross-platform constraints (POSIX scheduler "not implemented"; POSIX-tagged tests via `GOOS=linux`/WSL;
  `MCPHUB_MANIFEST_DIR_OVERRIDE`/`test_state_path_env` seams; the pre-existing IPC full-suite flake) are
  named: PASS.
- Shippable-vs-must-land-together is marked; highest-risk phase flagged with mitigations: PASS.
- No implementation code (phases describe scope + acceptance, not code): PASS.

**GATE DECISION: PASS** — implementation-ready. The atomic Phase 1 defect-fix needs only O3 (+ a placeholder
context value for tests). O1 must close before Phase 2 + Phase 4; O2 before Phase 3.

## Terms and Abbreviations

- **descriptor / `DaemonRuntimeSpec` / RuntimeSpec / materialize / fan-out / `InstallParsedManifest` /
  router / dynamic pool / embed-first / G4 hub resolver / workspace registry / `secret:KEY`**: see the design
  doc's [Terms and Abbreviations](../specs/2026-05-29-serena-migrate-redesign-descriptor-proxy.md).
- **atomic Phase 1**: the single PR that adds the runtime spec + materializer AND makes the proxy read it AND
  adds the native-http gate + consistency contract — merged because a half-landed pair would die on the
  proxy's unknown-`--task-name`-flag rejection.
- **native-http gate**: the build/install-time `transport == native-http` validation (design §3.1) that
  replaces the proxy's removed runtime transport check.
- **upgrade/restart gate**: the §7.1 requirement that a `runtime_spec`-introducing install/migrate drives the
  cold-restart upgrade flow so no old supervisor reads the new intent.
- **cold-restart upgrade flow**: the existing `mcphub install --upgrade` machinery
  ([internal/cli/install_upgrade.go](../../../internal/cli/install_upgrade.go)) — IPC `quiesce-timers` →
  `exit{graceful}` → force-kill fallback → start-new-supervisor.
- **live pidport**: the `<state-dir>` pidport file the GUI writes with its actual bound port after startup
  (`ReadPidport`, [internal/gui/single_instance.go:92-110](../../../internal/gui/single_instance.go)); the
  authoritative source for the router URL port.
- **`test_state_path_env`**: build tag enabling the env-fallback state-dir seam
  (`internal/api/state_paths_envfallback.go`) so tests redirect the state dir; excluded from release builds.
- **`MCPHUB_MANIFEST_DIR_OVERRIDE`**: test-only env var that bypasses the embed FS so tests get hermetic
  on-disk manifests ([internal/api/manifest_source.go:18-20](../../../internal/api/manifest_source.go)).
- **`GOOS=linux` / WSL**: how the POSIX-tagged supervisor/state-file tests are exercised on a Windows dev box.
- **deep-sec**: the parallel Codex `xhigh` security-review pass run after bot PASS, per CLAUDE.md Step 5.
