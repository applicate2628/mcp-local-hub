# Unified Plan: Serena dynamic-pool + Supervisor state-machine wiring (v7)

> **Status**: v7 — closes v6 codex review's 6 new BLOCKERs (api.IsActiveStop nonexistent, gracefulInProgress field mismatch, F.3 wiring path wrong, D.3 outer/inner rollback contradiction, --start-after-write doesn't compose, F.4 handler dependency gap) AND absorbs v6 sonnet's 6 IMPORTANTs (per-task event storm, line :228→:238, 8 missing test names, new-package declarations, helper declarations, mcphub reconcile gap) + 1 MINOR (reaper :2030→:2037). v7 also names structural changes that v6 implied but did not specify: `executeInstallTo` Pass A / Pass B split for `--start-after-write`, `HubLocalDeps` attached to `hubSession` (so `AggregateToolsCall` signature stays unchanged), `mcphub reconcile` operator command added to Phase A.3. Pending v7 dual review.
>
> **Convergence history**:
>
> - v1 (commit 5aa683b): initial draft. Sonnet REVISE: 4 BLOCKERS + 5I + 5M.
> - v2 (commit 02abc55): v1 BLOCKERS resolved; codex no-path consult closed Decision 5. Sonnet v2 REVISE: 3 NEW BLOCKERS. Codex v2 REVISE: same + NEW B.4 LoopEvent.
> - v3 (commit 112099a): 4 v2 BLOCKERS resolved. Sonnet v3 REVISE: 4 NEW BLOCKERS (LSP call-sites under-counted, validator types wrong, `executeInstallTo` unexported, `Supervisor`/`smState` missing). Codex v3 REVISE: same 4 + 4 IMPORTANT (D.3 chain incomplete, IntentWatcher not wired, sentinel collision unprevented, path traversal hole). Operational evidence + Phase H added (1fad546+338ae82+2fd5f18) — parallel trajectory.
> - v4 (commit 6f22944): header bumped with resolution intent for all 4 v3 BLOCKERS + 4 IMPORTANTS, but section bodies still v3. Dual review returned REVISE with consensus that section-level prose must be rewritten before v5 can be reviewed against verified evidence. Both reviewers also surfaced specific corrections: (a) LSP call-site catalog was 4-undercounted at 13-actual sites (NOT 6, NOT 8 — see B.1 v5 table), (b) `@`-prefix is NOT currently rejected by the manifest validator, (c) the validator-level rejection alone does not defend the registry write path because `@serena` lives in `WorkspaceEntry.Language` (registry) not `LanguageSpec.Name` (manifest), (d) v3 pseudocode used `Manifest` (real type is `ServerManifest`) + `len(m.PortPool)` (real type is `*PortPool`), (e) v3's `executeInstallTo` reference is to an unexported function.
> - v5 (commits 7589961+56fb528+da75f5e): section bodies rewritten to match the v4 header intent + close the additional reviewer-surfaced corrections. v5 dual review returned REVISE with 7 converged BLOCKERs: A.2 pseudocode used non-existent api types (RestartPolicyState, EvReconcileTick, SMContext fields) and wrong NewIntentWatcher signature; A.1 catalog had 5 stale file:line refs (HEAD drifted from 6f22944 to 56fb528); D.3 atomicity covered only 3 of 6 migration steps; D.1 schema gate missing for non-workspace-scoped manifests; F.4 hub.bind_workspace integration unspecified; B.1 missed register.go:640 (14th call-site); F.3 forward-ref to undefined GetSMState accessor.
> - v6 (commit bd552ee): all 7 v5 BLOCKERs closed against verified code at HEAD `56fb528`. v6 dual review returned divergent verdicts: codex REVISE with 6 new BLOCKERs (fake-API helpers — `api.IsActiveStop` doesn't exist as free function, `gracefulInProgress` field missing from controller; F.3 wiring referenced runSupervise but real hub construction is at `gui/hub_listener.go:182`; D.3 outer/inner rollback contradiction; `--start-after-write` flag doesn't compose with current `executeInstallTo` loop; F.4 `AggregateToolsCall` signature doesn't carry sticky/registry/events). Sonnet APPROVE_WITH_CHANGES with 6 IMPORTANTs (per-task event storm risk, line :228→:238 drift, 8 missing test names, new-package declarations missing, helper declarations missing, `mcphub reconcile` operator command gap) + 1 MINOR (reaper :2030→:2037 range).
> - v7 (this commit): all 6 v6 codex BLOCKERs closed + all 6 v6 sonnet IMPORTANTs absorbed:
>   - **B.1** Registry: 14-site verified call-site catalog (5 LSP-only requiring sentinel filter — incl. `register.go:640` legacy-key fallback added in v6; 9 backend-agnostic safe-include); dual-gate `@`-prefix defense at manifest validator AND registry write path; backend/server ownership matrix; `mcphub unregister --backend` default-LSP-only semantics; `mcphub stop` backend-aware `taskFilter`.
>   - **D.1** Validator: compile-accurate pseudocode against verified types (`ServerManifest`, `*PortPool`, `Languages []LanguageSpec`); v6 adds CROSS-BRANCH gate that rejects `DaemonTemplate != nil` when `Kind != KindWorkspaceScoped` OR `Transport == TransportRemoteHTTP` (closes v5 codex finding of silent acceptance under global/remote-http).
>   - **D.3** Install chain: new exported `api.InstallParsedManifest(ctx, m, opts)` AND v6-introduced **migration-driver rollback stack** wrapping all 6 migration steps (manifest write + registry alloc/save + scheduler tasks + client configs + intent write + daemon spawn); v6 acknowledges existing rollback closures are best-effort and surfaces failures via `rollback-incomplete` audit event + composite error; v6 introduces `--start-after-write` flag to defer daemon-spawn-before-intent-write race (closes v5 codex finding "daemons started before intent write").
>   - **A.2** SupervisorController v6 rewrites pseudocode against **real `api` package surface**: `api.SMState` (not RestartPolicyState), real `api.SMContext{IntentDesired, IntentIsActiveStop, Failures, QueuedAction, GracefulInProgress}` (not invented fields), `api.NewEventLoop(capacity int)` with required arg, `NewIntentWatcher(stateDir, pollInterval, onChange)` real signature, `EvIntentUpdate` (not non-existent EvReconcileTick); new `GetSMState(taskName) (api.SMState, bool)` public accessor exposes per-task SM state for F.3's health gate.
>   - **F.2** JSON-RPC `result`/`error` envelope classification with `classifyReadMemoryResponse` helper; v6 adds envelope-shape gate (`jsonrpc == "2.0"` AND `id` present) so a corrupted upstream returning `{"result":"x"}` cannot spuriously satisfy the 1-success branch.
>   - **F.3** Single-workspace shortcut gated on `api.StRunning` health check via the new `SerenaHealthLookup` interface (api package owns; cli supervisorController implements via GetSMState) — closes v5 sonnet finding "api router cannot directly depend on cli controller" via dependency-inversion seam; 412 with `daemon_state` field on unhealthy.
>   - **F.4** Snapshot-then-release lock pattern; fan-out runs lock-free against value-copy slice; `hub.bind_workspace` is MCP tool on hub-mcp endpoint with v6-specified integration into `AggregateToolsList` (`hub_mcp_aggregator.go:228`) and tools/call dispatch bypass (early-return branch before RouteMap lookup); reserved namespace `mcphub__*`.
>   - **A.1** catalog refreshed against HEAD `56fb528`: 6-param `makeProductionSpawnFnWithStatePath` (not 5-param) at `:1878`, `cmd.Wait()` reaper block at `:1979-2037` (v7 corrected from v6's `:1979-2030`), `MarkSpawned`/`MarkExited` at `:112`/`:149`, `supervisorStateFromRuntimeState` at `:314`, `UNKNOWN_COMMAND` emits at `:1080,1089,1248`; new `SMContext` row added for v5 BLOCKER context.
>   - **v7 closures** (new this iteration):
>     - **A.2**: replaced non-existent `api.IsActiveStop(d, now)` with real `DaemonIntent.IsActiveStop(now) (bool, string)` method form at `internal/api/daemon_intent.go:308`; replaced `c.gracefulInProgress.Load()` with `c.graceful.InProgress()` (real surface at `supervise.go:245`) + added `graceful *gracefulCounter` + `daemonIntent *daemonIntentCache` fields to `supervisorController`; added delta-only `EvIntentUpdate` posting instead of per-task storm (sonnet IMPORTANT B.1).
>     - **F.3**: rewired SerenaHealthLookup integration to real hub construction path at `internal/gui/hub_listener.go:182` (`api.NewHubMcpHandler(store)`); added `WithHubLocalDeps`/`WithSerenaHealthLookup` functional options pattern; named `internal/api/serena_routing/` as NEW package created by this phase.
>     - **D.3**: explicit outer/inner rollback rule (inner runs first; outer pushes only step 2-3 undos — NOT scheduler/client/intent/daemon — to avoid double-undo); concrete structural change to `executeInstallTo` (Pass A creates tasks, Pass B starts them; gated by `startTasks bool` param); named all migration helpers in acceptance criteria (`snapshotManifest`, `restoreManifest`, `snapshotRegistry`, `restoreRegistry`, `allocateSerenaPorts`, `writeNewManifest`).
>     - **F.4**: `HubLocalDeps` attached to `hubSession` (carries sticky+registry+events+health); `AggregateToolsCall` signature UNCHANGED; `HandleCall(ctx, sess *hubSession, clientReqID, paramsRaw)` matches existing dispatcher shape; corrected line ref `:228 → :238`; named `internal/api/hubmcp/` as NEW package.
>     - **A.3**: NEW `mcphub reconcile` operator command for in-place drift cleanup (sonnet IMPORTANT B.6); IPC `reconcile` verb replaces UNKNOWN_COMMAND at `:1080` for this case; dry-run / `--apply` modes.
>     - **F.2**: envelope-shape test additions (`TestClassifyReadMemoryResponse_RejectsMissingJSONRPCVersion` + `_RejectsMissingID`).
>     - **Test contract additions**: 5 new D.1/D.3 negative tests (kind:global, remote-http, rollback failure, audit emit on undo failure, StartAfterWrite deferral, executeInstallTo Pass A/Pass B separation); 4 new A.2 controller/health-gate tests; 5 new F.4 hub-local tool tests.
>
> **For agentic workers / future implementers**: this plan describes work that depends on PR #229 (supervisor `daemon-exited` emit) landing first. Until #229 merges + binary upgraded + serena crash root cause is identified via the new event, implementation of Phase A.2 (state-machine wiring) is **blocked on diagnostic data**. Phases B-F can start in parallel to A.2 once A.1 (catalog + plan ratification) is done.
>
> **Operator-mandated architectural posture**: "N серен = N активных воркспейсов агентов" (1:1 биекция). No global serena daemons. Each registered workspace gets its own long-lived `serena` daemon bootstrapped on `--project <abs-path>` with languages from `.serena/project.yml`. mcphub becomes the router; clients hit a constant mcphub endpoint and the router forwards to the per-workspace daemon based on path-arg.
>
> **Spec dependency**: full design in [docs/superpowers/specs/2026-05-20-serena-dynamic-pool.md](../specs/2026-05-20-serena-dynamic-pool.md). This plan is the implementation contract for that spec.

**Goal**: deliver dynamic-pool architecture for serena MCP — one serena daemon per registered workspace, with mcphub-router path-aware routing + sticky-session for no-path tools + auto-register on miss + workspace registry persistence. Simultaneously wire the v0.5.0 supervisor state machine into production (currently bypassed — see Phase A) so per-daemon crash recovery + backoff + quarantine work as specified.

**Tech stack**: Go 1.22 backend, Preact + TypeScript + Vite frontend (workspace registry UI), Playwright E2E, `gopkg.in/yaml.v3` for workspaces.yaml + manifest schema additions, `github.com/gofrs/flock` for workspace registry RMW.

**Scope boundaries**:
- **In scope**: dynamic-pool daemon spawn, path-aware routing, sticky-session, auto-register, workspace registry, supervisor state-machine production wiring
- **Out of scope** (deferred to v2 / G4 unified hub): handshake / dynamic-port discovery, full unified-hub router with constant client-facing port, multi-workspace symbol search

---

## Architectural posture & decision log

### Decision 1: 1:1 biection (serena daemons : active workspaces)

**Operator constraint**: every active workspace gets its own serena daemon. Trade-off: RAM (~300-500 MB per warm serena daemon) is acceptable on the operator's 3+ GB-free-RAM machine; up to ~6 concurrent workspaces is the realistic ceiling without swapping.

**Why not unified single daemon**: serena's `_active_project` is process-global. Two workspaces sharing one daemon → switch thrashing (kill LSP children for A, spawn for B, then back) at every cross-project tool call. Codex's deep-source review (2026-05-20) confirmed `activate_project` in `claude-code` context is not even exposed as a tool (`single_project: true` in `claude-code.yml` at pinned commit `f0a3a279b7c48d28b9e7e4aea1ed9caed846906b`). One unified daemon would either lock all clients to one project (claude-code preset) or thrash continuously (codex preset).

### Decision 2: Routing middleware as the cohesion layer

**Three modes**:
- **Mode 1 (path-aware)**: tools with `relative_path` / `file_path` / `name_path` → ancestor-walk to `.serena/project.yml` → workspace identified → forward to that daemon. Default for most tools (`find_symbol`, `replace_symbol_body`, `find_referencing_symbols`, `search_for_pattern`, etc.)
- **Mode 2 (sticky-session)**: tools without path-args (`list_memories`, `get_current_config`, `read_memory`, `write_memory`, `delete_memory`, etc.) → mcphub maintains per-MCP-session `client_id → workspace` map; bound at first path-aware call in session. **Pending codex consultation** for default-workspace fallback semantics when no prior path-call exists.
- **Mode 3 (auto-register on miss)**: unknown path not matching any registered workspace → file-extension survey → create `.serena/project.yml` stub → spawn new daemon → register in workspaces.yaml → forward.

### Operational evidence (2026-05-20 live audit)

**Captured on the operator's machine** as load-bearing input to the v4+ review loop. Live `Get-CimInstance Win32_Process` ancestor-chain walk on 2026-05-20 immediately after migrating 8 codex-cli stdio MCP entries to hub HTTP via `mcphub migrate sequential-thinking memory wolfram godbolt lldb paper-search-mcp serena gdb --clients codex-cli`:

- **18 `mcp-language-server.exe` processes** all share IDENTICAL ancestor chain:
  `mcp-language-server.exe < codex.exe < Antigravity.exe < Antigravity.exe < explorer.exe`
- ONE codex.exe alive (operator-confirmed: "там работает всего 1 codex через субагентов")
- 13 codex stdio MCP entries remain UN-migratable per `mcphub scan` (no top-level hub-routable manifest binding): of these, 8 are `mcp-language-server` per-language wrappers (`clangd`, `javascript`, `python`, `rust`, `fortran`, `vscode-css`, `typescript`, `vscode-html`), 1 is `gopls-mcp` (Go, native gopls MCP per `servers/mcp-language-server/manifest.yaml:19-40` — NOT wrapped by `mcp-language-server.exe`), and 4 are unrelated stdio servers (`time-server`, `stgen-dxf-viewer`, `raindrop`, `fetch`)
- `mcphub cleanup --scan-clients` reports **0 orphans** — correctly excluding `child of live codex` per the live-client safety guards at `internal/api/cleanup.go:333-361` (known client ancestor list) + `:916-958` (ancestor walk skip)
- The 18 processes are NOT orphans; they're live-rooted under a live codex but accumulate because each codex internal subagent spawns its own stdio MCP children that do not get reaped on subagent finish

**What this proves**:

1. The 8-server migration to hub HTTP (done today) WILL drop ~38% of per-subagent MCP spawns the next time codex picks up the rewritten config. That's the lower-bound win.
2. The remaining 13 LSP-language entries are the architectural ceiling: until a workspace-scoped hub-routable LSP-bridge exists (PR #222 in flight on `feat/v0.5.x-servers-matrix-revamp`), each codex subagent must spawn its own `mcp-language-server` per language → fleet-multiplier accumulation.
3. **`mcphub cleanup` is not the right primitive** for this class of problem. The safety guard correctly refuses to kill child-of-live-codex processes. The fix is to **eliminate the spawn at config-write time**, not to reap after the fact.
4. The `1 codex × N subagents × M LSP languages = N×M MCP processes` formula is exactly what the unified plan's dynamic-pool architecture and PR #222's LSP-bridge revamp jointly address. This audit is the empirical motivation.

**Direct implications for v4+ scope**:

- Phase D (per-workspace serena spawn) must inherit the same hub-routable pattern from PR #222's LSP-bridge: clients write one stable hub URL, hub fans out to workspace-keyed daemons.
- Phase E (auto-register on miss) becomes more important because the 13 stdio entries above are mostly per-language workspace-scoped LSP servers — auto-register on first hit is the operational path for getting all 13 onto hub routing.
- The plan's no-path-args sticky-session design (Decision 5) addresses the same class of failure: without sticky-session binding, an unbound codex subagent would either silently route writes to the wrong workspace or fail-closed; both are operationally worse than the live "spawn fresh stdio child" behavior we see today.

**Cleanup intervention 2026-05-20 07:50 (operator-requested broader sweep)**:

After the 18-mcp-language-server snapshot above, total process count grew to 280+ as more codex subagents ran. A second ancestor-walk widened the scope to ALL descendants of `codex.exe` whose ancestor chain does NOT contain `mcphub.exe daemon` (operator's correct primitive: kill subagent-spawned, keep hub-managed):

| Process name | Count under codex (mcphub-excluded) | Killed in safe subset | Surviving after kill |
|---|---|---|---|
| `node.exe` (MCP-stdio) | 153 | 94 (matched `mcp-server\.js`, `@playwright/mcp`, `@modelcontextprotocol`) | 98 (mixed: hub-routed + IDE-internal, some natural respawn) |
| `cmd.exe` (shell wrappers) | 118 | 0 (skipped — too risky to blanket-kill) | 32 (lost 86 via parent-death cascade) |
| `gopls.exe` (Go LSP) | 60 | 60 (all — clear LSP backend, mcphub-bridge will respawn on demand) | 4 |
| `chrome.exe` (Playwright) | 17 | 0 (per-session, operator may be viewing) | 53 (grew — Playwright reopened) |
| `mcp-language-server.exe` | 18 | 18 (all) | 0 |
| `conhost.exe` / `pwsh.exe` | 6 | 0 (operator terminals) | unchanged |
| **Total** | **372** | **172 direct + ~86 cascade = ~258 processes reaped** | net **~120** of original |

**Why `mcphub cleanup --scan-clients` returned 0 orphans pre-intervention** (correctly): all 372 are LIVE-rooted under `codex.exe < Antigravity.exe < explorer.exe`. None are orphans by the spec's definition (ancestor process is alive). The accumulation pattern is a per-subagent stdio fan-out, not a leaked-after-parent-died one.

**This implies a Phase F+ (operational hygiene) scope** the unified plan should add — not just hub-routing config rewrites (Phase B-E already cover that) but ALSO:

1. **Aggressive-cleanup CLI mode** that takes the operator's "child of live codex subagent" intent explicitly: `mcphub cleanup --aggressive --client codex-cli --kill-live-rooted-mcp-stdio`. Default stays safe; explicit flag opts into the operator-confirmed sweep. Closes the operational gap where the safety guard correctly refuses but the operator wants to override.
2. **Per-subagent lifecycle integration** with codex CLI (upstream feature request): codex subagents should EITHER reap their stdio MCP children on subagent finish OR (preferred) inherit a single parent stdio MCP set from the codex CLI parent. Until upstream codex adopts one of these, the per-subagent fan-out remains the architectural ceiling.
3. **GUI Servers matrix cleanup action** for the operator's interactive flow — Dashboard already has the "Cleanup orphans" button (commit `5ce805a` on this branch); needs an "Aggressive sweep" sibling per #1.

These three motivate a new **Phase H: Operational hygiene tooling** (deferred but in-scope for v4 review; named H NOT G because the existing Phase G already covers legacy 2-daemon cleanup per §"## Phase G: Cleanup of legacy 2-daemon" below): tooling that complements the hub-routing config changes in Phases B-E. Item 2 (upstream codex feature request) is **External / upstream follow-up; non-blocking for the mcphub PR** — it explains the architectural ceiling but does not gate Phase B-E.

**Canonical cleanup counts** (single normalized block, replaces the inconsistent "154 killed" / "172 direct" / "258 total" framings in prior commit messages):

- Initial snapshot (07:42 UTC, just after migrate): 18 `mcp-language-server.exe` total, all rooted under `codex.exe < Antigravity.exe`
- Growth window (07:42 → 07:50): codex internal subagents respawned; total candidates under codex (mcphub-excluded) reached **372** by widened ancestor walk
- Direct kills via `Stop-Process -Id`: **172** (18 `mcp-language-server.exe` + 60 `gopls.exe` + 94 `node.exe` matching MCP-server cmdline patterns)
- Cascade exits observed post-kill: ~86 `cmd.exe` wrapper processes exited after their wrapped children were killed (NOT parent-death — Windows `Stop-Process` doesn't tree-kill; this is the `cmd.exe /c <child>` wrapper exiting once the wrapped child terminates, observed empirically). Per `internal/api/cleanup.go:730-745` the underlying mechanism is `taskkill /PID /F` without `/T`, confirming no tree-kill.
- Net survivors immediately after: ~120 of original 372 — Playwright `chrome.exe` actually grew to 53 during the sweep (Playwright reopened sessions), `cmd.exe` dropped to 32, `gopls.exe` dropped to 4

**Survivor `gopls.exe` classification** (live-probed via ancestor walk, NOT mcphub-bridge respawn): the 4 survivors are 2 top-level instances under Cursor and Claude IDE extensions + 2 telemetry children of those top-level instances. None root through `mcphub.exe daemon`. These are unrelated to the codex/Antigravity subagent fan-out and were correctly skipped by the kill predicate.

### Decision 3: Supervisor state-machine wired into production

**Current bug** (diagnosed via codex deep-diagnostic 2026-05-20, file: `.scratch/codex-prompts/supervisor-serena-bug-20260520-044800.out`): production `supervise_reconcile.go:117` calls `r.spawn(d)` directly without posting `EvStart` to the state machine. `cmd.Wait()` goroutine in `supervise.go:1539-1543` calls `MarkExited` + persist without posting `EvChildExit`. Result: state machine's backoff / quarantine logic is **dead code** in production. PR #229 adds the diagnostic emit but does NOT wire the state machine — that wiring is **Phase A.2 of this plan**.

**Constraint**: Phase A.2 depends on PR #229's `daemon-exited` event being present in production AND the serena crash root cause being known (so we can validate backoff actually fires on that exit). Until both gates are satisfied, A.2 implementation is paused and gated on diagnostic data.

### Decision 4: Handshake-port deferred to v2

**Current**: workspaces.yaml records `serena_port: 9121-9199` from a fixed pool with persistent assignment.

**v2 future**: serena binds `port: 0` → kernel-assigned → publishes via supervisor IPC → mcphub-router discovers dynamically. Eliminates port-collision (orphan-on-fixed-port) class of failures. Docked into [G4 unified hub spec](../specs/2026-05-12-g4-unified-hub-mcp-design-v3.md) for v2 lift.

**Why deferred**: v1 must converge with the existing supervisor + workspace-registry primitives. Handshake adds a new IPC verb + discovery handshake protocol — meaningful complexity that benefits from v1 lessons. Not blocking dynamic-pool v1.

### Decision 5: No-path-args routing — RESOLVED (codex consult 2026-05-20)

**Verdict** (from codex deep-source review of serena pinned commit `f0a3a279...` at `tools_base.py:337-343` + `memory_tools.py:30-72` + `cli.py:338-368`; MCP Streamable HTTP spec; Python SDK `streamable_http_manager.py:225-240`):

Key facts surfaced by codex:
1. **No-path serena tools are NOT projectless**: per `tools_base.py:337-343`, any tool without `ToolMarkerDoesNotRequireActiveProject` checks `_active_project` before `apply()` and returns `"Error: No active project..."` if `None`. List of affected tools: `list_memories`, `read_memory`, `write_memory`, `delete_memory`, `check_onboarding_performed`, `onboarding`, `get_current_config`.
2. **`--project <abs>` on serena CLI activates project at startup**, so in dynamic-pool every daemon ALREADY has `_active_project` set bootstrap-time. All no-path tools work immediately on the daemon's own project.
3. **`Mcp-Session-Id` header is protocol-stable**: per MCP spec 2025-06-18 §"Session Management", server MAY issue on initialize, client MUST send on subsequent requests, 404 means new session must initialize. Python MCP SDK v1.26.0 FastMCP default `stateless_http=False`, generates `uuid4().hex`, stores `_server_instances[session_id]`, validates header. STABLE across TCP reconnect IF client preserves header; NOT stable across DELETE, session expiry, server restart, or new initialize. **Do NOT use TCP connection ID as anchor** — use `Mcp-Session-Id` header.
4. **Default-workspace fallback for ANY unbound no-path call is unsafe** — silent r/w against the wrong project's state. Exception: if registry has exactly ONE workspace, route there (no ambiguity possible).

**Decision: tool-group strategy table** (Phase F implements this):

| Tool group | Bound session strategy | Unbound (pre-first-path-call) strategy | Notes |
|---|---|---|---|
| `list_memories`, `check_onboarding_performed`, `get_current_config` | sticky daemon | aggregate workspace-keyed results | Don't merge into "native serena shape" without `workspace` key. `get_current_config` returns hub-summary + per-daemon configs as separate fields |
| `read_memory name` | sticky daemon | query all daemons; return only if EXACTLY ONE has the memory, else disambiguation error | `name` not unique per pool. Don't do "first success" — leaks wrong workspace |
| `write_memory`, `delete_memory`, `onboarding` | sticky daemon | **reject** with explicit "no workspace bound; call a path-aware tool first OR use explicit `hub.bind_workspace`" message | Binding sources: first path-aware tool-call, explicit hub `bind_workspace` command (new), or single-workspace-registry shortcut. No default for writes |

**Binding rule** (codex-confirmed): hub maintains `Mcp-Session-Id → workspace` sticky map; first path-aware call sets the mapping for that session. The mapping persists until: (a) session DELETE-d by client, (b) session 404-expired, (c) explicit `hub.bind_workspace` overrides. Optional new IPC verb `hub.bind_workspace <abs-path>` lets a client opt in to explicit binding before any path-call.

---

## Phase A: Supervisor state-machine wiring (depends on PR #229)

### A.1: Catalog audit + verified symbol table for state-machine wiring [no-code]

**Scope**: extend the v5 plan's symbol catalog with state-machine-specific symbols needed for production wiring. Reads-only inventory.

| Concept | Real symbol | Location (verified against HEAD `56fb528`) |
| --- | --- | --- |
| State machine entry point | `api.Transition(state SMState, ev SMEvent, ctx SMContext) (newState SMState, side string, persistBefore bool, matched bool)` | `internal/api/supervisor_state_machine.go:47` |
| State machine states | `api.SMState` (StIdle/StSpawning/StRunning/StExiting/StBackoffWaiting/StQuarantined) | `internal/api/supervisor_state_machine.go:5-14` |
| State machine events | `api.SMEvent` (EvStart/EvHealthOK/EvChildExit/EvTimerDue/EvIntentUpdate/EvManualRestart/EvRequestGraceful/EvQuiesceComplete/EvSupervisorRestart) | `internal/api/supervisor_state_machine.go:16-28` |
| State machine context | `api.SMContext{IntentDesired string, IntentIsActiveStop bool, Failures int, QueuedAction string, GracefulInProgress bool}` — v5 BLOCKER fix: A.2 v5 incorrectly invented `{TaskName, Now, FailuresInWindow}` fields that don't exist; the real `SMContext` carries the intent-resolved policy state, not the runtime-tracker counters | `internal/api/supervisor_state_machine.go:31-37` |
| Event loop FIFO | `api.NewEventLoop(capacity int) *EventLoop` (required `capacity` arg, min 16); `api.LoopEvent{Kind: SMEvent, TaskName: string, Body: map[string]any}`; `(*EventLoop).Post(LoopEvent)`, `(*EventLoop).RegisterHandler(func(LoopEvent))`, `(*EventLoop).Run(ctx)` | `internal/api/supervisor_event_loop.go:6-38` |
| Per-daemon SM state cache | `DaemonRuntimeTracker` (existing) — separates runtime state (PIDs, restart_count) from formal SM state; v6 A.2 introduces `supervisorController.smStates sync.Map` keyed by TaskName for `api.SMState` values | `internal/cli/supervisor_runtime_tracker.go:30` |
| Reconciler spawn fan-out | `func (r *Reconciler) Reconcile(intent *api.SupervisorIntentFile, daemonIntent *api.DaemonIntentFile, currentRunning map[string]bool, now time.Time)` — calls `r.spawn(d)` directly at `:118` (NOT via Transition; bypass documented in Decision 3) | `internal/cli/supervise_reconcile.go:91-129`; spawn call at `:118` |
| Production spawn fn (post-PR #230 sig) | `func makeProductionSpawnFnWithStatePath(job *process.Job, events *api.SupervisorEventLog, tracker *DaemonRuntimeTracker, statePath string, overlay *daemon_env_overlay.Overlay, crashCh chan<- crashEvent) SpawnFunc` — 6 params after PR #230 added `crashCh` for auto-respawn dispatcher | `internal/cli/supervise.go:1878`; production call site at `:637` |
| `cmd.Wait()` exit reaper | `daemon-exited` event emit + `MarkExited` + non-clean-exit `crashCh` send all live in the reaper goroutine; PR #229 emit at `:2005-2011`, MarkExited at `:2012`, crashCh send at `:2019-2026` | `internal/cli/supervise.go:1979-2037` block (goroutine `go func() { ... }()` opens at `:1979`, closes at `:2037`; v6 said `:2030` but the closing brace is at `:2037`) |
| `MarkSpawned` / `MarkExited` | `DaemonRuntimeTracker.MarkSpawned(taskName, pid, startedAt)` / `MarkExited(taskName)` — MarkExited does NOT decrement pid_generation; that field accumulates | `internal/cli/supervisor_runtime_tracker.go:112` (MarkSpawned), `:149` (MarkExited) |
| `supervisorStateFromRuntimeState` | maps runtime tracker state → persisted state field; missing case for `"spawning"` → falls into `default: "idle"` (root cause of state="idle" + pid_generation=35 silent crash loop) | `internal/cli/supervisor_runtime_tracker.go:314` |
| IPC `respawn` handler | `handleRespawn(conn, req, deps)` — exposed via per-task `respawn` IPC verb; `restart`/`reload` verbs return UNKNOWN_COMMAND | `internal/cli/supervise_respawn.go:96-237`; UNKNOWN_COMMAND emits at `internal/cli/supervise.go:1080,1089,1248` |
| IntentWatcher constructor + Run | `NewIntentWatcher(stateDir string, pollInterval time.Duration, onChange func()) *IntentWatcher` (note: ONE `stateDir`, NOT two paths); `(*IntentWatcher).Run(ctx)` polls `supervisor-intent.json` + `daemon-intent.json` mtimes under that dir, fires `onChange` callback when either changes; default poll interval = 60s when pollInterval <= 0 | `internal/cli/supervise_watcher.go:107` (NewIntentWatcher), `:120` (Run) |

**Acceptance criteria**: every state-machine-related symbol the v2 implementer will call appears in this table with verified file:line. Pre-existing symbols from v5 plan's catalog remain valid; this table adds the SM-wiring symbols.

**TBD for v2**: read `supervisor_event_loop.go` end-to-end to populate exact `Post` / `Run` / event-loop lifecycle API. (Codex's deep-diagnostic noted "FIFO event loop" exists but didn't fully trace the production caller — v2 implementer must verify.)

### A.2: Wire state machine into production reconcile + spawn paths (v5 design)

**v3/v4 status**: BLOCKER (sonnet + codex). v3 referenced `s.Supervisor` struct + `s.smState` field as if they existed in production — neither does. Production is `func runSupervise(ctx, noIPC, strictMode) error` at `supervise.go:315` with all state as locals (event log, job, IPC listener, intent watcher, runtime tracker — closures over function-local variables). PR #230 added `runRespawnDispatcher` (`supervise_respawn_dispatcher.go:77`) + `DaemonRuntimeTracker` (`supervisor_runtime_tracker.go:30`) as a lightweight auto-respawn path that does NOT use the formal `api.Transition()` state machine. A.2 must reconcile with these, not replace them blindly.

**v5 design — lightweight `supervisorController` wrapping the existing runtime**:

The PR #230 `runRespawnDispatcher` is a working subset of the formal state machine (StRunning + EvChildExit → StBackoffWaiting → StSpawning | StQuarantined). A.2 promotes the dispatcher's responsibilities into a small struct that also owns the `IntentCache` and routes ALL spawn/respawn through the formal `api.Transition()` SM. The existing `DaemonRuntimeTracker` is REUSED (sliding-window quarantine semantics are already correct); the lightweight dispatcher's body is folded into the controller's event handler.

```go
// supervisorController is the long-lived runtime owner replacing the
// closure-over-locals pattern in runSupervise. It owns:
//   - IntentCache (atomic.Value) for descriptor lookup on EvStart/EvChildExit
//   - eventLoop for serialized side-effect dispatch (replaces the dispatcher goroutine's
//     direct crashCh→spawn path)
//   - DaemonRuntimeTracker (reused as-is from PR #230 — sliding-window quarantine
//     PLUS PID/restart_count entries needed for IPC status)
//   - per-TaskName SM state map (api.SMState values), mirrored to supervisor-state.json
//
// Field count: 5. NOT a god-object — the heavy lifting (spawn, audit emit, IPC,
// reconcile, watcher) stays as free functions or existing types; controller is
// the orchestration glue.
type supervisorController struct {
    intentCache *IntentCache              // atomic.Value snapshot pointer; refreshed on IntentWatcher.onChange
    eventLoop   *api.EventLoop            // serialized dispatch; existing primitive at api/supervisor_event_loop.go:15
    tracker     *DaemonRuntimeTracker     // PR #230; sliding-window + entries map; SOLE consumer of crash-counting methods
    smStates    sync.Map                  // taskName → api.SMState
    events      *api.SupervisorEventLog   // audit emitter
    graceful    *gracefulCounter          // existing surface at supervise.go:206-245; shared with IPC exit{graceful} flow
    daemonIntent *daemonIntentCache       // small wrapper around the parsed daemon-intent.json file; refreshed alongside intentCache on watcher.onChange
}

// GetSMState exposes the controller's per-task SM state so OTHER subsystems
// (notably F.3's single-workspace-shortcut health gate) can read the policy
// state without touching the unexported sync.Map directly. Returns (StIdle,
// false) when no state is tracked for the given task.
func (c *supervisorController) GetSMState(taskName string) (api.SMState, bool) {
    v, ok := c.smStates.Load(taskName)
    if !ok {
        return api.StIdle, false
    }
    s, ok := v.(api.SMState)
    if !ok {
        return api.StIdle, false
    }
    return s, true
}

type IntentCache struct {
    snap atomic.Value // *intentSnapshot
}

type intentSnapshot struct {
    intent       *api.SupervisorIntentFile
    daemonByTask map[string]*api.SupervisorDaemon
}

func (c *IntentCache) Lookup(taskName string) (*api.SupervisorDaemon, bool) {
    s, ok := c.snap.Load().(*intentSnapshot)
    if !ok || s == nil {
        return nil, false
    }
    d, ok := s.daemonByTask[taskName]
    return d, ok
}

// Refresh atomically swaps the cached snapshot. Wired into IntentWatcher.onChange.
func (c *IntentCache) Refresh(intent *api.SupervisorIntentFile) {
    snap := &intentSnapshot{
        intent:       intent,
        daemonByTask: make(map[string]*api.SupervisorDaemon, len(intent.Daemons)),
    }
    for i := range intent.Daemons {
        d := &intent.Daemons[i]
        snap.daemonByTask[d.TaskName] = d
    }
    c.snap.Store(snap)
}

// handleLoopEvent is the single dispatch path for spawn/exit events.
// Replaces both the direct r.spawn(d) call in supervise_reconcile.go:118 AND
// the runRespawnDispatcher goroutine in supervise_respawn_dispatcher.go:77.
//
// Type accuracy (v6 closure of v5 BLOCKERS 1-3): the real api types per
// api/supervisor_state_machine.go are SMState (not RestartPolicyState) and
// SMContext{IntentDesired, IntentIsActiveStop, Failures, QueuedAction,
// GracefulInProgress} (not invented TaskName/Now/FailuresInWindow fields).
// The controller resolves the intent-derived fields from the daemon descriptor
// (intent has Desired + ActiveStopUntil per supervisor_intent.go) and reads
// the rolling failure count from the tracker.
func (c *supervisorController) handleLoopEvent(ev api.LoopEvent) {
    d, ok := c.intentCache.Lookup(ev.TaskName)
    if !ok {
        return // daemon dropped from intent; audit-log orphan event if desired
    }
    var currentState api.SMState
    if v, ok := c.smStates.Load(ev.TaskName); ok {
        if s, ok := v.(api.SMState); ok {
            currentState = s
        }
    }
    now := time.Now().UTC()
    // Resolve the intent's active-stop predicate via the REAL method form.
    // v7 closure of v6 codex finding: there is no `api.IsActiveStop(d, now)`
    // free function; the predicate is `func (i DaemonIntent) IsActiveStop(now
    // time.Time) (bool, string)` at internal/api/daemon_intent.go:308.
    // The second return is the human-readable reason (clock-skew / TTL /
    // user-stop) — only the boolean is fed into SMContext; the reason is
    // available if v7 wants to surface it in audit events later.
    daemonIntent := lookupDaemonIntent(d.TaskName) // resolved from daemon-intent.json snapshot
    activeStop, _ := daemonIntent.IsActiveStop(now)
    smCtx := api.SMContext{
        IntentDesired:      d.Desired,                        // string "running" | "stopped"
        IntentIsActiveStop: activeStop,                        // from DaemonIntent.IsActiveStop(now)
        Failures:           c.tracker.CrashCountInWindow(ev.TaskName, now, respawnFailureWindow),
        QueuedAction:       "",                               // populated from supervisor-state.json on cold start; "" on first observation
        GracefulInProgress: c.graceful.InProgress(),          // real surface: gracefulCounter.InProgress() at supervise.go:245
    }
    newState, side, persistBefore, matched := api.Transition(currentState, ev.Kind, smCtx)
    if !matched {
        return // log + drop; transition is a no-op for this state+kind pair
    }
    if persistBefore {
        c.smStates.Store(ev.TaskName, newState)
        // Persist to supervisor-state.json via existing tracker persist seam.
        // Best-effort — audit-log on failure but do NOT block side effect.
    }
    c.executeSideEffect(side, d)
}
```

**IntentCache concurrency primitive choice** (closes v4 codex Q1 finding):

`atomic.Value` is correct for read-mostly snapshot pointer swap:

- Writes (IntentWatcher.onChange) build a fresh `*intentSnapshot` (including a fresh `daemonByTask` map) and call `c.snap.Store(snap)` — one atomic pointer write.
- Reads (every handleLoopEvent call) call `c.snap.Load().(*intentSnapshot)` once, then operate on a fully-immutable snapshot for the duration of the event.
- The snapshot is NEVER mutated post-publish; readers cannot observe a partial map; writers cannot corrupt a concurrent reader's view.
- RWMutex + CoW would add a per-read mutex acquire (write contention is rare but read contention is per-event); atomic.Value is strictly better for this pattern.

The same primitive is already used in production by `internal/api/health.go`'s `DaemonStatusSnapshot` cache — A.2 reuses the established pattern.

**Replacement points**:

- `supervise_reconcile.go:118` direct `r.spawn(d)` → `eventLoop.Post(LoopEvent{Kind: api.EvStart, TaskName: d.TaskName})`. Descriptor `d` resolved via cache; not duplicated in the event.
- `supervise.go:1916-1917` `cmd.Wait()` goroutine: after the existing `daemon-exited` emit (PR #229), also `eventLoop.Post(LoopEvent{Kind: api.EvChildExit, TaskName: taskName})`. Handler then triggers backoff / quarantine / respawn state transitions through the formal SM.
- `runRespawnDispatcher` (`supervise_respawn_dispatcher.go:77`) — **REMOVED**. Its responsibilities (sliding-window check, backoff timer arm, spawn fire, quarantine audit) are absorbed into `executeSideEffect`. The `crashCh` channel and dispatcher goroutine in `runSupervise` are deleted. Loose ends:
  - `crashEvent` struct moves into `executeSideEffect`'s internal model OR is deleted entirely (the SM's `EvChildExit` event carries exit_code via `LoopEvent.Body`).
  - The existing dispatcher tests (`supervise_respawn_dispatcher_test.go` — 8 tests including `TestRespawnDispatcher_SchedulesRespawnAfterCrash`, `TestRespawnDispatcher_QuarantineAfterThreshold`, `TestRespawnDispatcher_SuppressesOnStopIntent`, `TestRespawnDispatcher_RetriesOnSpawnFailure`, `TestRespawnDispatcher_TracksBackoffAndQuarantineState`) are refactored to drive the controller's `handleLoopEvent` instead of `runRespawnDispatcher` directly; the test contract is preserved (same semantics, different entry point).

**Single-consumer guarantee** (closes v4 codex Q4 finding "Does supervisorController cross with DaemonRuntimeTracker ownership?"):

The controller becomes the SOLE consumer of `DaemonRuntimeTracker`'s crash-counting methods. Direct callers of `tracker.RecordCrashAndCountInWindow` outside the controller's `executeSideEffect` are forbidden (lint guard via grep regression in CI). The tracker's other entries (`entries map[string]DaemonRuntimeEntry` for PID generation + restart count) remain shared with `supervise.go`'s spawn closure — those are write-only-on-spawn-success and read-only by IPC `status` snapshots; no concurrency issue.

**runSupervise refactor** (the function-local-state → controller migration):

```go
// supervise.go:315 — runSupervise builds the controller from existing primitives
// instead of holding raw locals. No behavior change at this boundary; the
// controller is the new owner of state that was previously closure-captured.
//
// API surface (v6 closure of v5 BLOCKERS 4-5): api.NewEventLoop takes a
// REQUIRED capacity int (minimum 16; see supervisor_event_loop.go:20-24).
// NewIntentWatcher takes (stateDir, pollInterval, onChange) per
// supervise_watcher.go:107 — ONE stateDir under which intent files live,
// not two separate file paths.
func runSupervise(ctx context.Context, noIPC bool, strictMode bool) error {
    // ... existing pre-controller setup (lock, intent load, event log, job creation) ...

    const eventLoopCapacity = 256 // generous for the per-daemon event mix; min is 16
    ctrl := &supervisorController{
        intentCache: newIntentCache(),
        eventLoop:   api.NewEventLoop(eventLoopCapacity),
        tracker:     NewDaemonRuntimeTracker(),
        events:      events,
    }
    ctrl.intentCache.Refresh(initialIntent)
    ctrl.eventLoop.RegisterHandler(ctrl.handleLoopEvent)
    go ctrl.eventLoop.Run(ctx) // FIFO consumer; exits on ctx done

    // IntentWatcher wired into runSupervise. The watcher polls every file
    // under stateDir whose mtime it tracks (supervisor-intent.json +
    // daemon-intent.json by design); onChange fires when ANY of them changes.
    watcher := NewIntentWatcher(stateDir, 60*time.Second, func() {
        // Re-read intent + refresh cache atomically. Errors logged via events.
        updated, err := api.LoadSupervisorIntent(filepath.Join(stateDir, "supervisor-intent.json"))
        if err != nil {
            _ = events.Emit(api.SupervisorEvent{
                Severity: "warn", Source: "intent-watcher", Event: "intent-reload-failed",
                Body: map[string]any{"error": err.Error()},
            })
            return
        }
        ctrl.intentCache.Refresh(updated)
        // Post EvIntentUpdate per known task so the SM picks up intent flips
        // (Desired stopped/running, ActiveStopUntil shifts). EvIntentUpdate is
        // the real event constant per supervisor_state_machine.go:23; v5 used
        // a non-existent `EvReconcileTick` value.
        //
        // Burst behavior (closes v6 sonnet IMPORTANT B.1 "per-task storm"):
        // EventLoop.Post is blocking when the channel is full (no select/default
        // at supervisor_event_loop.go:33-35). For 6-workspace deployment with
        // ~3 daemons each = 18 events per watcher tick, against capacity 256
        // this is safe (~7% of capacity). For larger deployments OR rapid
        // back-to-back intent rewrites (e.g. `mcphub install` chain), v7 uses
        // delta-only posting: compare the new intent against the cached snapshot
        // taken under the same atomic.Value swap (intentSnapshot diff) and post
        // ONLY for daemons whose intent fields actually changed.
        delta := diffIntentSnapshots(previous, updated) // returns []string of task names
        for _, taskName := range delta {
            ctrl.eventLoop.Post(api.LoopEvent{Kind: api.EvIntentUpdate, TaskName: taskName})
        }
    })
    go watcher.Run(ctx) // <-- v3 said "wired"; v4 header confirmed; v6 makes it concrete with real API

    // ... rest of runSupervise (IPC listener, reconcile driver, child-exit reaper) ...
}
```

**Gating** (carried from v3, prerequisites unchanged):

1. **PR #229 merged to master** — DONE (merged 2026-05-20 as commit `526bea9`; the daemon-exited emit is now live on master)
2. **Master rebased into `feat/v0.5.x-servers-matrix-revamp`** — DONE (PR #230 was rebased onto post-#229 master)
3. **PR #230 merged to master** — DONE (merged 2026-05-20 as commit `c840664`; auto-respawn dispatcher is the foundation A.2 builds on)
4. **Operator runs `mcphub install --upgrade`** + cold-restart supervisor — DONE
5. **Serena crash root cause identified** — DONE via PR #229's `daemon-exited` event (port-bind conflict; resolved by killing manual wrappers)

A.2 implementation can now proceed. Phases B + C + D + E + F + G + H are NOT blocked on A.2 and can fan out in parallel.

**Acceptance criteria**:

- `runRespawnDispatcher` deleted from `supervise.go` startup AND `supervise_respawn_dispatcher.go`; replaced by controller dispatch
- Reconciler no longer calls `r.spawn` directly; all spawn intent flows through `eventLoop.Post(EvStart)` → `handleLoopEvent` → `executeSideEffect`
- `cmd.Wait()` exit posts `EvChildExit` (in addition to the existing `daemon-exited` audit emit from PR #229)
- `IntentWatcher.Run` is invoked from `runSupervise` (no longer dead code)
- State machine drives transitions visible in `supervisor-state.json`: `idle` → `spawning` → `running` → `backoff-waiting` → `spawning` → `quarantined` per spec
- Restart-policy state fields (`failures_in_window`, `backoff_until`, `quarantine_since`) appear in serialized state
- DaemonRuntimeTracker's crash-counting methods are called only from `executeSideEffect` (regression guard via grep)
- Manual smoke (preserved from v3): kill serena daemon → supervisor respawns within backoff window; kill 10 times → quarantine kicks in

**Test contract**:

- `TestSupervisorController_IntentCacheRefreshOnWatcherEvent` — IntentWatcher fires → cache snapshot updated atomically; concurrent handleLoopEvent reads see consistent old-or-new state, never partial
- `TestSupervisorController_HandleEvChildExit_TransitionsToBackoffWaiting`
- `TestSupervisorController_HandleEvChildExit_TransitionsToQuarantinedAfterThreshold` — replaces dispatcher test of same semantic
- `TestSupervisorController_HandleEvChildExit_SuppressesOnStopIntent` — replaces dispatcher test
- `TestSupervisorController_HandleEvChildExit_RetriesOnSpawnFailure` — replaces dispatcher test
- `TestSupervisorController_PersistedStateMatchesSpec` — verify `supervisor-state.json` field schema matches spec (including `failures_in_window`, `backoff_until`, `quarantine_since`)
- `TestStateMachineWiring_DoesNotDoubleRespawnWithLegacyDispatcher` — regression guard that the old dispatcher entry point is gone and no duplicate respawn fires
- `TestSupervisorController_GetSMState_ReturnsTrackedState` — `GetSMState(taskName)` returns the value previously stored via `smStates.Store` (regression guard for the F.3 health-gate dependency)
- `TestSupervisorController_GetSMState_DefaultsToIdleForUnknownTask` — unknown task returns `(StIdle, false)`; `false` distinguishes "no tracked state yet" from a literal `StIdle` value
- `TestSerenaHealthLookup_InterfaceContract` — verifies that `cli.supervisorController` satisfies `api.serena_routing.SerenaHealthLookup` (compile-time assertion via `var _ api_serena_routing.SerenaHealthLookup = (*supervisorController)(nil)`)
- `TestIntentWatcherOnChange_DeltaOnly_DoesNotEventStorm` — closes v6 sonnet IMPORTANT B.1: simulate intent reload where only 1 of 18 daemons changed; verify exactly 1 `EvIntentUpdate` posted (not 18)

### A.3: Migration — upgrade installed binary + restart supervisor + `mcphub reconcile` (v7 addition)

**Scope**: operator-side migration documentation + smoke checklist after Phase A.2 lands, plus a new operator command `mcphub reconcile` (closes v6 sonnet IMPORTANT B.6) for interactive drift cleanup WITHOUT full supervisor cold-restart.

**`mcphub reconcile` command** (v7 NEW): the supervisor's startup-time reconciler picks up drift (orphan tasks, intent-without-task pairs) automatically on cold restart, but operators on long-running supervisor processes who have surfaced drift via `mcphub status` need a way to trigger a reconcile in-place. `mcphub reconcile` sends an IPC `reconcile` verb to the running supervisor; the supervisor re-reads its intent file, walks the scheduler-registered tasks, computes the drift set, and (with `--apply`) posts `EvIntentUpdate` per drifted task so the SM drives the corrective transitions.

```bash
mcphub reconcile           # dry-run: print drift report, no mutations
mcphub reconcile --apply   # apply: trigger SM transitions to align scheduler state with intent
```

Acceptance criteria for the new command:

- IPC `reconcile` verb implemented in `supervise.go` IPC dispatcher (replaces the existing `UNKNOWN_COMMAND` response at `:1080` for the `reconcile` case specifically; other UNKNOWN_COMMAND verbs remain rejected)
- Dry-run prints structured drift report: per-daemon `{task_name, scheduler_state, intent_desired, action}`
- `--apply` posts `EvIntentUpdate` per drifted task; subsequent SM transitions drive Run/Stop/Delete
- `mcphub reconcile` returns within 30s OR explicit timeout error
- Audit event `mcphub-reconcile-invoked` recorded with body `{dry_run, drift_count, applied_count}`

**Steps**:
1. `mcphub install --upgrade` — replaces binary
2. Supervisor cold-restart (Task Scheduler / systemd / launchd will pick up new binary)
3. Verify state machine fields appear in `supervisor-state.json`
4. Verify serena daemons enter `running` state (if root cause from PR #229 is fixed) OR `backoff-waiting` then `quarantined` (if still crashing — then proceed to root-cause fix)

---

## Phase B: Workspace registry extension

### B.1: Extend existing `Registry` / `WorkspaceEntry` with `@serena` sentinel language tuple (v5 design)

**v3/v4 status**: BLOCKER (both sonnet + codex). Three independent defects converged across reviews:

1. **Call-site catalog drastically undercounted** (4 sites claimed, 14 sites actually iterate `reg.Workspaces` per fresh grep at HEAD `56fb528`)
2. **False validator-rejection claim** ("@ prefix is invalid as an LSP-language name") — manifest validator at `internal/config/manifest.go:347-365` has NO `@`-prefix rejection rule; the sentinel CAN collide if an attacker or buggy manifest names a LanguageSpec `@anything`
3. **Sentinel lives in different struct than the proposed defense** — `@serena` rows write to `WorkspaceEntry.Language` (registry field), NOT `LanguageSpec.Name` (manifest field). Adding `@`-prefix rejection ONLY to manifest validator does not defend the registry write path

**v5 design**: keep `(WorkspaceKey, Language)` as the primary registry key. Serena entries use sentinel `Language: "@serena"` to distinguish from per-LSP-row tuples. Add a dual-gate defense — both the manifest validator AND the registry write path refuse `@`-prefix Language values unless they arrive via the explicit `PutSerena` entry point.

**Verified call-site catalog** (grep `range.*reg\.Workspaces|range.*workspaces|ListByWorkspace` against `internal/`, 2026-05-20 HEAD `6f22944`):

| # | File:line | Operation | @serena handling | Action |
| --- | --- | --- | --- | --- |
| 1a | `register.go:637` | `reg.ListByWorkspace(wsKey)` lookup of existing LSP rows during **unregister** (the call lives in `unregisterWithManifest` at register.go:603-700; the v5 description "during register" was wrong) | LSP-only — must filter sentinel | Add `if e.Language == SerenaLanguageSentinel continue` |
| 1b | `register.go:640` | `reg.ListByWorkspace(legacyWSKey)` legacy-key fallback when `:637` lookup returned zero rows (symlink-canonicalization migration path) | LSP-only — same semantic as 1a | Add same sentinel filter |
| 2 | `register.go:727` | Scan ClientEntries for entry-name collision during `ResolveEntryName` | Backend-agnostic — collision check on string name; serena rows have own naming (`serena-<short_key>`) | NO filter (safe-include) |
| 3 | `register.go:754` | Same collision-helper (`entryNameTakenByOtherWorkspace`) | Same | NO filter (safe-include) |
| 4 | `install.go:657` | Build `byTask` map for lifecycle/last-call enrichment | Backend-agnostic — TaskName is per-task unique | NO filter (safe-include) |
| 5 | `install.go:2124` | Build `byTask` map for status path | Same | NO filter (safe-include) |
| 6 | `install_intent.go:559` | Walk for `mcphub stop --daemon <lang>` task-name collection | LSP-only when `daemonFilter` != "" — for serena `daemonFilter` semantics need re-design (see "stop semantic for serena" note below the table) | Backend-aware filter |
| 7 | `weekly_refresh.go:126` | Iterate to fire weekly-refresh schtasks /Run | Backend-agnostic via `WeeklyRefresh` flag — serena rows default to `WeeklyRefresh=false` and skip | NO filter (lifecycle gate suffices) |
| 8 | `status_enrich.go:69` | Build TaskName→entry map for overlay | Backend-agnostic | NO filter (safe-include) |
| 9 | `membership.go:51` | Build `[WorkspaceKey,Language]` index for weekly-refresh membership API | LSP-only — `@serena` rows MUST NOT appear as a "language" in membership UI | Add filter (LSP-only ownership) |
| 10 | `api_surfaces.go:430` | Build `WorkspaceTasksByKey` + `PortMap` for canonical status snapshot | Backend-agnostic — every workspace-scoped task belongs in the snapshot | NO filter (safe-include) |
| 11 | `legacy_migrate.go:206` | Match legacy task names against registry during migration | Backend-agnostic — task-name match works for both | NO filter |
| 12 | `gui/daemons.go:83` | Render membership table for weekly-refresh GUI panel | LSP-only — same logic as membership.go:51 | Add filter (LSP-only ownership) |
| 13 | `gui/workspaces.go:101` | List all workspaces (display table) | Backend-aware — display column "Backend" reads `e.Backend` field; serena rows show "serena" | NO filter (display surface, no semantic conflation) |

**Sites requiring filter `Language != SerenaLanguageSentinel`** (LSP-only consumers): `register.go:637`, `register.go:640` (legacy-key fallback), `install_intent.go:559` (with backend-aware re-design per the "Stop semantic for serena" block below), `membership.go:51`, `gui/daemons.go:83`. Five sites total — NOT the four sites that v3 named.

**Sites that are safe-include** (already backend-agnostic by TaskName-keyed iteration or lifecycle-flag gating): 9 production sites (the remaining entries in the table above). Test sites are not behavioral; they exercise raw iteration semantics and a regression test guards membership-classification correctness (see test contract below).

**Backend/server ownership matrix**:

| Backend value (`WorkspaceEntry.Backend`) | Language value | Server slug | Owning server's manifest path | Lifecycle owner |
| --- | --- | --- | --- | --- |
| `mcp-language-server` | per-LSP language (e.g. `"go"`, `"typescript"`) | `mcp-language-server` | `servers/mcp-language-server/manifest.yaml` | LSP lazy-proxy task per row |
| `gopls-mcp` | `"go"` (always) | `gopls-mcp` | `servers/gopls-mcp/manifest.yaml` | Go-specific lazy-proxy task |
| `serena` | `"@serena"` sentinel (always) | `serena` | `servers/serena/manifest.yaml` | Per-workspace dynamic-pool task |

Each backend owns exactly one shape of registry row. Cross-backend pollution (e.g. a `serena` backend with `Language="go"`) is rejected at `PutSerena` / `RegisterLSP` entry points.

**`@`-prefix defense** (closes v3 BLOCKER-1):

1. **Manifest validator gate** (`config/manifest.go:347-365`): add rejection `if strings.HasPrefix(l.Name, "@") { return fmt.Errorf("manifest %s: languages[%d].name must not start with '@' (reserved for sentinel rows)", m.Name, i) }`. Catches any manifest that tries to declare an LSP language with the sentinel prefix.

2. **Registry write-path gate** (`workspace_registry.go` — new wrapper around `Put`): add `func (r *Registry) PutLSP(e WorkspaceEntry) error` that refuses `strings.HasPrefix(e.Language, "@")`. Existing `Put` becomes a low-level helper that both `PutLSP` and `PutSerena` call after validation. All current LSP-registration call sites switch to `PutLSP` (mechanical rename; `register.go` is the only writer).

The two gates compose: the manifest validator prevents bad LanguageSpec.Name at install/load time; `PutLSP` prevents bad WorkspaceEntry.Language at register-time even if some future caller skips the manifest path. Together they defend the sentinel uniqueness.

**Scope** (registry field extension):

```go
// Additions to existing WorkspaceEntry struct (workspace_registry.go:31):
type WorkspaceEntry struct {
    // ... existing fields preserved ...
    WorkspaceKey  string            `yaml:"workspace_key"`
    WorkspacePath string            `yaml:"workspace_path"`
    Language      string            `yaml:"language"` // "@serena" sentinel for dynamic-pool rows
    Backend       string            `yaml:"backend"`  // existing: "mcp-language-server"|"gopls-mcp"; new: "serena"
    Port          int               `yaml:"port"`     // serena port lives here too; AllocatedPorts covers
    TaskName      string            `yaml:"task_name"`
    ClientEntries map[string]string `yaml:"client_entries"`
    WeeklyRefresh bool              `yaml:"weekly_refresh"`

    // NEW (only meaningful when Language == SerenaLanguageSentinel):
    RegisteredAt  time.Time `yaml:"registered_at,omitempty"`
    RegisteredVia string    `yaml:"registered_via,omitempty"` // "manual" | "auto-detect" | "migration"
    Languages     []string  `yaml:"languages,omitempty"`      // snapshot of .serena/project.yml at register time
}
```

**Save pipeline note** (sonnet v2 carryover, unchanged in v5): existing `(*Registry).Save()` uses plain `os.WriteFile` + atomic rename (`workspace_registry.go:129-163`), NOT `SecureWriteClientConfig`. The registry lives in the operator's `%LOCALAPPDATA%`-scoped state dir with 0600 file mode. Hardening parity with hub-mcp state files is OUT OF SCOPE for B.1; tracked as a separate follow-up.

**New API on existing `Registry`** (atomic API per call-site type, closes v3 BLOCKER-1 codex finding "RegisterSerena/UnregisterSerena should be the only way @serena rows enter/leave"):

```go
const SerenaLanguageSentinel = "@serena"

// Read paths (filter by sentinel):
func (r *Registry) SerenaEntries() []WorkspaceEntry  // Language == SerenaLanguageSentinel
func (r *Registry) GetSerena(workspaceKey string) (WorkspaceEntry, bool)
func (r *Registry) LSPEntries() []WorkspaceEntry     // Language != SerenaLanguageSentinel
func (r *Registry) ListByWorkspaceLSP(workspaceKey string) []WorkspaceEntry  // LSPEntries filtered by key

// Write paths (the dual-gate defense):
func (r *Registry) PutLSP(e WorkspaceEntry) error    // refuses '@'-prefix Language; calls Put after validation
func (r *Registry) PutSerena(e WorkspaceEntry) error // requires Language == SerenaLanguageSentinel; calls Put after validation
func (r *Registry) RemoveSerena(workspaceKey string)
func (r *Registry) AllocateSerenaPort(pool PortPool) (int, error) // first free port from pool not in AllocatedPorts

// Internal-only helper (existing Put stays exported for UPDATE writes):
// CALL-SITE MIGRATION (verified via `grep 'reg\.Put('` on internal/, 2026-05-20):
//   PROD writers (4 sites total):
//     - register.go:285 (NEW row insert from `mcphub register`) → switch to PutLSP
//     - register.go:450 (composed entry after registration success) → switch to PutLSP
//     - register.go:482 (rollback restore of prior entry) → keep Put (preserves whatever
//                       Language the prior row had, including @serena if a serena row
//                       is being rolled back; @-prefix gate would corrupt rollback)
//     - daemon_workspace.go:187 (UPDATE lifecycle on existing row from `mcphub daemon`)
//                       → keep Put (entry came from registry; Language already validated)
//   TEST writers (8+ sites): test fixtures pre-populate registry state; keep Put.
// Save() is unchanged.
```

**Unregister semantics with `--backend` flag** (closes v3 BLOCKER-1 codex finding "default unregister walks by Language"):

The existing `mcphub unregister <workspace>` command has two interpretations under the @serena coexistence:

- **Default (v5)**: `mcphub unregister <workspace>` unregisters ONLY LSP rows (`Language != "@serena"`); `--backend serena` is required to remove serena rows; `--backend all` removes everything.
- **Rationale**: LSP rows and the serena row have independent lifecycles. An operator may want to disable LSP routing for a workspace while keeping the long-lived serena daemon running, or vice versa. Defaulting to "LSP-only removal" matches the existing semantic (pre-v5 `mcphub unregister` removed ALL workspace entries because there was only one backend; with two backends, the default should be the narrower scope).

```bash
mcphub unregister D:\dev\PaperPane               # removes only LSP rows; serena unchanged
mcphub unregister D:\dev\PaperPane --backend serena  # removes only serena row; LSP rows unchanged
mcphub unregister D:\dev\PaperPane --backend all     # removes everything (legacy behavior)
mcphub unregister D:\dev\PaperPane --backend mcp-language-server  # narrow LSP-only by backend value
```

The CLI surface for `--backend` lives in B.2; B.1 only defines the registry API (`RemoveByBackend(workspaceKey, backendFilter string)`).

**Stop semantic for serena** (closes call-site #6 from the table above — `install_intent.go:559` backend-aware filter):

The existing `mcphub stop <server> --daemon <name>` walks `reg.Workspaces` with `daemonFilter != "" && e.Language != daemonFilter`. For LSP servers this works because `e.Language` IS the daemon identifier (`"go"`, `"typescript"`, etc.). For serena, `e.Language == "@serena"` and the operator-meaningful daemon identifier is the workspace path, not the language sentinel. v5 introduces an explicit `--backend` filter on the stop path that mirrors the unregister CLI:

```bash
mcphub stop serena                               # stop ALL serena daemons (no daemon filter)
mcphub stop serena --workspace D:\dev\PaperPane  # stop serena daemon for one workspace
mcphub stop mcp-language-server --daemon go      # legacy LSP semantic; unchanged
mcphub stop mcp-language-server                  # stop ALL LSP daemons for this server
```

Internally, the `install_intent.go:559` loop gets the backend-aware filter via a new `taskFilter` function that takes both the backend value AND the daemon/language identifier:

```go
// taskFilter returns true if the entry should be included in the stop set.
// daemonFilter == ""    → include all entries for this server (no narrow)
// backend == "serena"   → match by Workspace OR include all serena rows
// backend == LSP value  → match by e.Language (existing semantic)
func taskFilter(e WorkspaceEntry, backend, daemonFilter, workspace string) bool {
    if e.Language == SerenaLanguageSentinel && backend == "serena" {
        return workspace == "" || e.WorkspacePath == workspace
    }
    if e.Language != SerenaLanguageSentinel && backend != "serena" {
        return daemonFilter == "" || e.Language == daemonFilter
    }
    return false // backend/sentinel mismatch
}
```

The full CLI surface (flags + parser changes) lives in B.2; this clause defines only the registry-walk semantic that closes the install_intent.go:559 backend-aware-filter gap.

**Acceptance criteria**:

- Manifest validator rejects `LanguageSpec.Name` with `@`-prefix at `Validate()`
- `PutLSP` rejects `WorkspaceEntry.Language` with `@`-prefix
- `PutSerena` requires `Language == SerenaLanguageSentinel` exactly
- `Registry.Load()` / `Save()` round-trip preserves new optional fields (omitempty pattern, no strict-parse on registry)
- The 4 LSP-only call sites (register.go:637, install_intent.go:559, membership.go:51, gui/daemons.go:83) filter sentinel rows
- `AllocatedPorts()` automatically includes serena ports (no code change required)
- `mcphub unregister <workspace>` default removes only LSP rows

**Test contract**:

- `TestServerManifestValidate_RejectsAtPrefixLanguageName` — `LanguageSpec{Name: "@serena"}` fails Validate
- `TestRegistry_PutLSP_RejectsAtPrefixLanguage` — `PutLSP(WorkspaceEntry{Language: "@anything"})` returns error
- `TestRegistry_PutSerena_RequiresExactSentinel` — `PutSerena` with `Language: "@other"` returns error
- `TestRegistry_SerenaSentinel_RoundTripsNewFields` — Load/Save preserves Languages + RegisteredAt + RegisteredVia
- `TestRegistry_SerenaSentinel_CoexistsWithLSPRows` — same workspace_key with both "@serena" and "go"/"typescript" rows
- `TestRegistry_AllocateSerenaPort_FirstFreeFromPool` / `TestRegistry_AllocateSerenaPort_ExhaustionReturnsError`
- `TestRegistry_LegacyEntryReadAccepted` — older entry without Languages field loads cleanly
- `TestWorkspaceRegistryConsumers_ClassifyByBackend` — regression guard that asserts each of the 4 LSP-only sites filters sentinel rows AND each of the safe-include sites still iterates ALL rows
- `TestRegistry_Unregister_DefaultBackendSemantics` — `Unregister(ws)` removes only LSP; `--backend serena` removes only serena; `--backend all` removes everything

### B.2: `mcphub workspace {register, unregister, list, set-default}` CLI

**Scope**: new cobra subcommands wiring B.1's API to operator surface.

```bash
mcphub workspace register "D:\dev\PaperPane" [--default] [--languages cpp,typescript,markdown]
mcphub workspace unregister "D:\dev\PaperPane"
mcphub workspace list
mcphub workspace set-default "D:\dev\PaperPane"
```

**Behavior**:
- `register`: read `.serena/project.yml` for languages (or use `--languages` override); allocate port from pool; write to workspaces.yaml
- `register` without existing `.serena/project.yml`: error with explicit "run `mcphub workspace bootstrap <path>` first" guidance (B.3)
- `unregister`: remove from workspaces.yaml, leave `.serena/` intact on disk
- `list`: tabular output with path, languages, default flag, port, last spawn-time

**Test contract**:
- `TestWorkspaceRegister_AllocatesPortFromPool`
- `TestWorkspaceRegister_RejectsExistingPath`
- `TestWorkspaceUnregister_RemovesEntryButLeavesDisk`
- `TestWorkspaceList_TabularOutput`

### B.3: `mcphub workspace bootstrap <path>` — `.serena/project.yml` initializer

**Scope**: command that file-extension-surveys a directory + writes `.serena/project.yml` with detected languages. Used by both manual operator flow AND auto-register-on-miss (Phase E.2).

**Acceptance criteria**:
- Survey scans `<path>/**` (bounded depth 5, gitignore-aware, skip `node_modules`/`target`/`dist`/`.git`)
- Detect via extension map: `.cpp/.hpp/.cc → cpp`, `.go → go`, `.ts/.tsx → typescript`, `.py → python`, `.rs → rust`, `.md → markdown`, etc. (extend per current `mcp-language-server` manifest support)
- Write `.serena/project.yml` with `languages: [...]` + `read_only: false` + `excluded_dirs: [...]`
- Refuse to overwrite existing `.serena/project.yml` (require `--force`)

---

## Phase C: Routing middleware in mcphub

### C.1: Path-aware route resolver

**Scope**: new package `internal/api/serena_routing/` with:

```go
type WorkspaceResolver struct {
    workspaces *WorkspacesFile  // loaded once, refreshed on workspaces.yaml mtime change
}

// ResolveByPath returns the workspace entry whose path is an ancestor
// of the given absolute path, or whose path + relative_path resolves
// to an existing file when path is relative.
func (r *WorkspaceResolver) ResolveByPath(path string) (*WorkspaceEntry, error)

// AncestorWalk walks up from path until a `.serena/project.yml` is
// found; returns the workspace directory.
func (r *WorkspaceResolver) AncestorWalk(absPath string) (string, error)
```

**Acceptance criteria**:
- Absolute path: ancestor-walk until `.serena/project.yml` found; if no match — `ErrWorkspaceNotFound` (triggers Mode 3 auto-register caller-side)
- Relative path: for each registered workspace, try `workspace.Path + relative_path`; first existing wins; deterministic order (alphabetic by workspace.Path)
- Returns workspace entry → caller can extract `SerenaPort` for forwarding

**Test contract**:
- `TestResolveByPath_AbsoluteMatch`
- `TestResolveByPath_RelativeMatchFirstWorkspace`
- `TestResolveByPath_NoMatch_ReturnsErrWorkspaceNotFound`
- `TestAncestorWalk_FindsProjectYml`

### C.2: mcphub-router HTTP handler `/serena/mcp` (path-aware)

**Scope**: new GUI handler that:
1. Receives MCP request from client
2. Extracts path-arg from tool body (handles serena's `relative_path` / `name_path` conventions)
3. Calls `WorkspaceResolver.ResolveByPath`
4. Forwards request to `localhost:<workspace.SerenaPort>/mcp`
5. Streams response back

**Acceptance criteria**:
- HTTP POST `/serena/mcp` with MCP tools/call body
- Path extraction handles all path-arg variants in serena tool schema (need to enumerate)
- Forward includes original headers (Content-Type, MCP-Session-Id, etc.)
- Response streamed as SSE or single-shot depending on upstream Content-Type
- On `ErrWorkspaceNotFound` → trigger Mode 3 (Phase E) inline OR return HTTP 503 with explicit "register workspace first" message (TBD per Phase E decision)

**Test contract** (IMPORTANT-4 sonnet v1 fix — expanded error-path coverage):

- `TestSerenaRouter_TwoWorkspaces_PathArgRoutesCorrectly` — happy path: two registered workspaces, path arg under workspace A → request hits daemon A only
- `TestSerenaRouter_WorkspaceNotFound_TriggersMode3OrReturns503` — path doesn't match any registered workspace → either auto-register (Phase E) fires, OR (if E disabled) HTTP 503 with explicit "register workspace first" guidance
- `TestSerenaRouter_UpstreamTimeout_Returns504` — upstream serena daemon not responding within configured timeout (default 60s for tool-call, matches HTTPHost httpClient timeout) → HTTP 504 Gateway Timeout with body `{"error": "upstream serena daemon at port X did not respond within Ys"}`
- `TestSerenaRouter_UpstreamConnectionRefused_Returns502` — upstream port not listening (daemon crashed/not yet up) → HTTP 502 + audit event `serena-upstream-unreachable`
- `TestSerenaRouter_MissingPathArg_RoutesToMode2` — tool body has no `relative_path`/`file_path`/`name_path` → falls through to Phase F (sticky-session or fallback)
- `TestSerenaRouter_MalformedToolBody_Returns400` — body is not valid JSON OR missing `name` field → HTTP 400 with parse error
- `TestSerenaRouter_PreservesMcpSessionIdHeader` — request's `Mcp-Session-Id` header forwarded verbatim to upstream + response header threaded back through
- `TestSerenaRouter_PreservesContentTypeStreaming` — upstream `text/event-stream` response streams chunked back to client without buffering

### C.3: Sticky-session map for no-path tools

**Scope**: extend C.2 handler with per-MCP-session sticky-session binding.

```go
type SessionRouter struct {
    sessions map[string]*WorkspaceEntry  // mcp_session_id → workspace
    mu       sync.RWMutex
}

func (s *SessionRouter) BindSession(sessionID string, ws *WorkspaceEntry)
func (s *SessionRouter) LookupSession(sessionID string) *WorkspaceEntry
```

**Acceptance criteria**:
- On every path-aware call, bind session_id → workspace AFTER successful resolve
- On no-path call: lookup session_id; if bound → forward to that workspace; if not bound → fallback per codex consult decision (D for read-ops, C for write-ops likely)
- Session cleanup: lazy expiration (e.g., TTL 24h since last call) OR on explicit MCP session close (if serena MCP transport surfaces close events)

**Test contract**:
- `TestSessionRouter_BindOnPathCall`
- `TestSessionRouter_NoPathFallback_Aggregate` (for read-ops)
- `TestSessionRouter_NoPathFallback_Reject` (for write-ops)

**Dependency**: codex consultation result for no-path-args semantics (pending — see Decision 5).

---

## Phase D: Per-workspace serena daemon spawn

### D.1: Manifest schema extension — `workspace-scoped` + `daemon_template` validator branch (v5)

**v3/v4 status**: BLOCKER (sonnet + codex). Pseudocode in v3 was uncompilable against the actual types in `internal/config/manifest.go` at HEAD `6f22944`:

- v3 referenced `(m *Manifest)` — actual type is `ServerManifest` (manifest.go:48)
- v3 wrote `len(m.PortPool) > 0` — actual type is `*PortPool` (pointer to struct with `Start int, End int`, manifest.go:58 + 109-112), so `len()` is a compile error
- v3 wrote `len(m.Languages)` — accurate (slice; manifest.go:57) but reviewers flagged consistency
- v3 wrote `len(m.Daemons)` — accurate (slice of `DaemonSpec`; manifest.go:56) but the struct is `DaemonSpec` not `Daemon`
- v3 wrote `containsWorkspacePathToken(m.DaemonTemplate.ExtraArgsTemplate)` — helper undefined; reviewers need the actual signature and semantics

**v5 design**: validator branch on `DaemonTemplate != nil`. Pseudocode below is compile-accurate against the verified types. The new `DaemonTemplate` struct uses `*PortPool` (NOT `[]int`) for consistency with the existing `ServerManifest.PortPool` field shape — operators write `start: 9121, end: 9199` and the same range allocator is reused.

**New Go struct** (added to `internal/config/manifest.go` alongside existing `DaemonSpec`):

```go
// DaemonTemplate describes a per-workspace daemon spawn template for the
// dynamic-pool branch of kind=workspace-scoped. Mutually exclusive with
// the legacy ServerManifest.Daemons list (validator rejects both-present).
type DaemonTemplate struct {
    Context           string    `yaml:"context"`
    PortPool          *PortPool `yaml:"port_pool"`           // reuse existing PortPool{Start,End}
    ExtraArgsTemplate []string  `yaml:"extra_args_template"` // each arg may contain ${workspace.path}
}

// Extension to existing ServerManifest struct (manifest.go:48):
type ServerManifest struct {
    // ... all existing fields preserved (Name, Kind, Transport, Command, BaseArgs,
    //     BaseArgsTemplate, Env, Daemons []DaemonSpec, Languages []LanguageSpec,
    //     PortPool *PortPool, IdleTimeoutMin, ClientBindings, WeeklyRefresh,
    //     URL, Headers, RequiredBinaries) ...

    DaemonTemplate *DaemonTemplate `yaml:"daemon_template,omitempty"` // NEW; mutually exclusive with Daemons
}
```

**Validator branch** (extends existing `func (m *ServerManifest) Validate()` at manifest.go:251):

```go
// containsWorkspacePathTokenInArgs scans each element of args for the
// literal substring "${workspace.path}". Returns true on the first match.
// Substring-match (not exact-equality) so operators can write composite
// args like "--project=${workspace.path}/src". Internal helper, lowercase
// — only the validator uses it.
func containsWorkspacePathTokenInArgs(args []string) bool {
    const tok = "${workspace.path}"
    for _, a := range args {
        if strings.Contains(a, tok) {
            return true
        }
    }
    return false
}

// Extension to existing Validate() at manifest.go:251.
// Inserted into the existing `if m.Kind == KindWorkspaceScoped` block
// at manifest.go:337-366 (replaces lines 337-366 with the dual-branch form).
//
// NEW v6 cross-branch gate (closes v5 BLOCKER D.1 codex finding "daemon_template
// silently accepted under kind=global / transport=remote-http"): reject any
// non-nil DaemonTemplate when the manifest is not legitimately workspace-scoped.
// This rejection lives OUTSIDE the workspace-scoped branch so kind=global +
// daemon_template (which would otherwise return nil at line 367 / 274-314 of
// the remote-http path) fails fast with an explicit error.
func (m *ServerManifest) Validate() error {
    // ... existing global / transport-scope checks preserved (manifest.go:251-336) ...

    if m.DaemonTemplate != nil && m.Kind != KindWorkspaceScoped {
        return fmt.Errorf("manifest %s: daemon_template requires kind=workspace-scoped (got kind=%q); dynamic-pool spawn is incompatible with kind=global", m.Name, m.Kind)
    }
    if m.DaemonTemplate != nil && m.Transport == TransportRemoteHTTP {
        return fmt.Errorf("manifest %s: daemon_template is incompatible with transport=remote-http (no local subprocess to spawn from the template)", m.Name)
    }

    if m.Kind == KindWorkspaceScoped {
        if m.DaemonTemplate != nil {
            // Dynamic-pool branch.
            if m.PortPool != nil {
                return fmt.Errorf("manifest %s: kind=workspace-scoped with daemon_template must NOT set top-level port_pool (move start/end into daemon_template.port_pool)", m.Name)
            }
            if len(m.Languages) > 0 {
                return fmt.Errorf("manifest %s: kind=workspace-scoped with daemon_template rejects top-level languages[] (dynamic-pool serena is multi-language per .serena/project.yml)", m.Name)
            }
            if len(m.Daemons) > 0 {
                return fmt.Errorf("manifest %s: kind=workspace-scoped with daemon_template is mutually exclusive with daemons[] (dynamic-pool migration requires removing the legacy daemons[] block)", m.Name)
            }
            if m.DaemonTemplate.PortPool == nil {
                return fmt.Errorf("manifest %s: daemon_template.port_pool is required (start/end)", m.Name)
            }
            if m.DaemonTemplate.PortPool.Start <= 0 || m.DaemonTemplate.PortPool.End < m.DaemonTemplate.PortPool.Start {
                return fmt.Errorf("manifest %s: daemon_template.port_pool must have start>0 and end>=start (got {%d,%d})", m.Name, m.DaemonTemplate.PortPool.Start, m.DaemonTemplate.PortPool.End)
            }
            if len(m.DaemonTemplate.ExtraArgsTemplate) == 0 {
                return fmt.Errorf("manifest %s: daemon_template.extra_args_template must be non-empty", m.Name)
            }
            if !containsWorkspacePathTokenInArgs(m.DaemonTemplate.ExtraArgsTemplate) {
                return fmt.Errorf("manifest %s: daemon_template.extra_args_template must contain ${workspace.path} token somewhere (else workspace context is lost on spawn)", m.Name)
            }
            return nil
        }
        // Legacy LSP-bridge branch (unchanged — preserves current manifest.go:337-365 behavior).
        if m.PortPool == nil {
            return fmt.Errorf("manifest %s: port_pool is required for kind=workspace-scoped", m.Name)
        }
        if m.PortPool.Start <= 0 || m.PortPool.End < m.PortPool.Start {
            return fmt.Errorf("manifest %s: port_pool must have start>0 and end>=start (got {%d,%d})", m.Name, m.PortPool.Start, m.PortPool.End)
        }
        if len(m.Languages) == 0 {
            return fmt.Errorf("manifest %s: languages[] must be non-empty for kind=workspace-scoped", m.Name)
        }
        for i := range m.Languages {
            // ... existing per-language checks preserved verbatim (manifest.go:347-365) ...
        }
        return nil
    }
    // ... rest of Validate() preserved ...
    return nil
}
```

**Sentinel-prefix rejection on `LanguageSpec.Name`** (B.1 dual-gate defense, lives in the same per-language loop):

```go
for i := range m.Languages {
    l := &m.Languages[i]
    if l.Name == "" {
        return fmt.Errorf("manifest %s: languages[%d].name is required", m.Name, i)
    }
    // NEW (B.1): refuse '@' prefix to keep the @serena sentinel collision-free.
    if strings.HasPrefix(l.Name, "@") {
        return fmt.Errorf("manifest %s: languages[%d].name must not start with '@' (reserved for sentinel rows)", m.Name, i)
    }
    // ... existing backend / transport / lsp_command checks preserved ...
}
```

**Manifest example** (post-D.1, what `servers/serena/manifest.yaml` becomes after migration):

```yaml
name: serena
kind: workspace-scoped        # existing kind value; no new constant needed
transport: native-http
command: uvx
base_args: [...]
env: {PYTHONUNBUFFERED: "1"}
daemon_template:              # NEW optional block
  context: codex
  port_pool:
    start: 9121
    end: 9199
  extra_args_template:
    - --context
    - codex
    - --project
    - "${workspace.path}"
# NOTE: top-level `daemons:` block is INCOMPATIBLE with `daemon_template:` —
# validator rejects both-present (one or the other, not both). This forces
# explicit migration to dynamic-pool. Migration tooling (D.3) drops legacy
# daemons[] when writing the new manifest.
```

**Decision** (rejected: new third kind; accepted: extend existing `workspace-scoped`): serena's dynamic-pool falls under the existing `workspace-scoped` semantic — one daemon per workspace. The change adds a new OPTIONAL `daemon_template` block alongside the existing `daemons:` list. When `daemon_template` is present, reconciler generates one descriptor per registered serena workspace from the template; when only legacy `daemons:` is present, current per-daemon behavior is preserved. The `KindWorkspaceScoped` constant value (`"workspace-scoped"`) is unchanged.

**Acceptance criteria**:

- `daemon_template`-only manifest validates successfully (no `languages[]` / top-level `port_pool` required)
- Both-present (top-level `port_pool` AND `daemon_template`) → reject with explicit "move start/end into daemon_template.port_pool"
- Both-present (`daemons[]` AND `daemon_template`) → reject with explicit "dynamic-pool migration requires removing the legacy daemons[] block"
- `daemon_template.extra_args_template` MUST contain `${workspace.path}` (substring match — composite args like `--project=${workspace.path}/sub` pass)
- `LanguageSpec.Name` rejects `@`-prefix (closes the B.1 sentinel-collision gate at the manifest layer)
- LSP-language manifest with `port_pool` + `languages[]` (no `daemon_template`) continues to validate as before (regression guard for mcp-language-server / gopls-mcp / existing global LSP manifests)
- `dec.KnownFields(true)` strict parse remains intact (new `daemon_template` key has yaml tag with omitempty; new field on existing struct does not break existing manifest YAMLs)

**Test contract**:

- `TestServerManifestValidate_WorkspaceScopedWithDaemonTemplate_Valid`
- `TestServerManifestValidate_WorkspaceScopedWithDaemonTemplate_RejectsTopLevelPortPool`
- `TestServerManifestValidate_WorkspaceScopedWithDaemonTemplate_RejectsTopLevelLanguages`
- `TestServerManifestValidate_WorkspaceScopedWithDaemonTemplate_RejectsDaemonsListBoth`
- `TestServerManifestValidate_DaemonTemplateMissingWorkspacePathToken`
- `TestServerManifestValidate_DaemonTemplateInvalidPortPoolRange`
- `TestServerManifestValidate_RejectsDaemonTemplateForKindGlobal` — v7 cross-branch gate: `Kind: KindGlobal` + `DaemonTemplate != nil` fails before workspace-scoped branch
- `TestServerManifestValidate_RejectsDaemonTemplateForRemoteHTTP` — v7 cross-branch gate: `Transport: TransportRemoteHTTP` + `DaemonTemplate != nil` fails
- `TestServerManifestValidate_RejectsAtPrefixLanguageName` — `LanguageSpec{Name: "@serena"}` fails (B.1 dual-gate)
- `TestServerManifestValidate_LegacyLSPManifest_StillValidates` — regression guard
- `TestContainsWorkspacePathTokenInArgs_SubstringMatch` — composite args like `--project=${workspace.path}/sub` and `${workspace.path}` standalone both return true; bare args without the token return false
- `TestServerManifestParse_DaemonTemplate_StrictKnownFields` — yaml round-trip preserves template; unknown fields fail strict parse

### D.2: Supervisor instance-per-workspace spawn

**Scope**: extend `loadIntentFiles` + `Reconciler.Reconcile` to instantiate N daemon descriptors from workspaces.yaml × manifest.yaml's `daemon_template`.

```go
// For manifest with kind: workspace, generate one SupervisorDaemon per
// entry in workspaces.yaml, with port allocated from manifest.port_pool
// + extra_args template expanded with ${workspace.path}.
```

**Acceptance criteria**:
- supervisor-intent.json contains N descriptors for serena (one per registered workspace)
- Each descriptor has unique task_name (e.g., `\mcp-local-hub-serena-<hash(workspace_path)>`)
- Each descriptor has unique port from pool
- workspaces.yaml mtime change → reconcile re-runs → spawn new instances for added workspaces, terminate instances for removed workspaces

### D.3: Migration from legacy 2-daemon (or unified-intermediate) to dynamic-pool

**Scope**: new `mcphub migrate serena legacy-to-dynamic-pool` subcommand.

**Source-state detection** (IMPORTANT-5 fix from sonnet v1 review): the operator's current `servers/serena/manifest.yaml` has `kind: global` with single `unified` daemon (intermediate). Migration must detect THREE possible source states explicitly:

| Source state | Detection predicate |
|---|---|
| Legacy 2-daemon | `manifest.daemons[]` contains exactly 2 entries named `claude` + `codex`, AND `manifest.daemon_template` is absent |
| Intermediate unified | `manifest.daemons[]` contains exactly 1 entry named `unified`, AND `manifest.daemon_template` is absent |
| Already migrated (target) | `manifest.daemons[]` is absent OR empty, AND `manifest.daemon_template` is present |
| Malformed / partial | anything else (e.g., daemons[] has 3+ entries, OR both daemons[] and daemon_template present) → error with explicit "manual reconciliation required" |

**Behavior**:

1. Detect source state via predicate above; refuse-with-error on malformed/partial
2. If already-migrated: exit 0 (idempotency); no writes
3. Read existing `Registry` (workspace_registry.go) for any pre-registered serena workspaces
4. If no serena workspaces registered: prompt operator to register at least one via `mcphub workspace register <path> --backend serena --languages <list>`; bail out (exit non-zero)
5. Rewrite `manifest.yaml`: drop `daemons[]` block, add `daemon_template` block per D.1 schema
6. For each registered serena workspace, allocate port from `daemon_template.port_pool` via `Registry.AllocateSerenaPort()` (B.1) and write back via `Registry.Save()`
7. **Reload trigger via new exported seam `api.InstallParsedManifest`** (v5 closure of v3 BLOCKER D.3):

   **v3/v4 status**: BLOCKER (sonnet + codex). v3 referenced `api.executeInstallTo(...)` as the in-process install primitive, but that function is UNEXPORTED (`install.go:1634`) — a migrate subcommand outside the `internal/api` package cannot call it. v3 also did not state any atomicity contract: if scheduler-task creation succeeds but `supervisor-intent.json` write fails, the system is half-configured (tasks exist; supervisor reconciler never sees them).

   **v5 design**: introduce `api.InstallParsedManifest` as a new exported sister-entry-point to `api.Install`. It accepts a pre-parsed `*config.ServerManifest` (skipping the embed-FS load step), bypasses `refuseWorkspaceScopedInstall` (workspace-scoped is the whole point), runs `BuildPlanWithOpts` + scheduler-task creation + `WriteSupervisorIntent`, and shares the rollback stack across all three side-effects. Atomicity contract is option A (rollback) — partial failure leaves end-state identical to never-attempted install.

   **Signature**:

   ```go
   // InstallParsedManifest is the workspace-scoped sister to (a *API).Install.
   // Accepts a parsed manifest (caller owns parsing — typically the migrate
   // subcommand that just wrote a new manifest.yaml). Skips refuseWorkspaceScopedInstall
   // gate. All three side effects (scheduler tasks + per-client config + supervisor-intent
   // write) share one rollback stack; on any failure the stack runs and end-state
   // matches never-attempted install.
   //
   // Returns the absolute path of supervisor-intent.json that was written, for the
   // caller to log.
   func (a *API) InstallParsedManifest(
       ctx context.Context,
       m *config.ServerManifest,
       opts InstallParsedManifestOpts,
   ) (intentPath string, err error)

   type InstallParsedManifestOpts struct {
       Writer            io.Writer
       ClientsInclude    []string
       IncludeAllClients bool
       Workspaces        []WorkspaceEntry // pre-loaded snapshot of registered serena workspaces
       DryRun            bool
   }
   ```

   **Atomicity contract** (the v3 BLOCKER D.3 closure that v4 deferred; v6 expanded to cover the full migration sequence per codex v5 finding):

   The migration sequence has **six** mutating steps (manifest write, registry-port allocation + save, scheduler tasks, per-client configs, intent write, daemon spawn from `executeInstallTo`). v5 only covered steps 3-5 inside `InstallParsedManifest`'s rollback. v6 extends the rollback boundary outward AND acknowledges the limits of best-effort rollback:

   - **Pre-flight gate** (defense layer 1 — fail-fast before any mutation): `WriteSupervisorIntent` is dry-written first via the existing `WriteStateFileAtomic` helper (`supervisor_intent.go:134-136`) against a temp path. If the dry-write fails (disk full, permission denied, parent-dir DACL gate refusal under `MCPHUB_REQUIRE_SINGLE_USER_HOME=1`), the function returns BEFORE any other mutation — end-state = pristine.
   - **Migration-driver rollback stack** (defense layer 2 — owns the WHOLE sequence, not just `InstallParsedManifest`): the `mcphub migrate serena legacy-to-dynamic-pool` driver (the caller, NOT the install seam itself) owns a top-level rollback stack that wraps the entire 6-step sequence. Each step pushes its undo onto the stack BEFORE proceeding:

     ```go
     // Pseudocode for the migrate driver — NOT InstallParsedManifest itself.
     // The driver owns the migration-wide rollback stack; InstallParsedManifest
     // owns the install-time sub-stack (steps 3-5).
     func migrateSerenaDynamicPool(ctx context.Context) (err error) {
         var rollback []func() error // each closure returns its own error
         defer func() {
             if err == nil {
                 return
             }
             // Run undo in reverse; collect rollback errors so failures are
             // surfaced to the operator (not swallowed as v5 implied).
             var rollbackErrs []error
             for i := len(rollback) - 1; i >= 0; i-- {
                 if undoErr := rollback[i](); undoErr != nil {
                     rollbackErrs = append(rollbackErrs, undoErr)
                 }
             }
             if len(rollbackErrs) > 0 {
                 // Best-effort failed; system is in inconsistent state.
                 // Emit audit event + return composite error.
                 events.Emit(api.SupervisorEvent{
                     Severity: "error", Source: "migration",
                     Event: "rollback-incomplete",
                     Body: map[string]any{"errors": rollbackErrs, "primary_error": err.Error()},
                 })
                 err = fmt.Errorf("%w; rollback also failed: %v", err, errors.Join(rollbackErrs...))
             }
         }()

         // Step 1: pre-flight intent dry-write (no rollback push — read-only check)
         if err := preflightIntentWrite(intentPath); err != nil { return err }

         // Step 2: manifest write + rollback push (restore from backup snapshot)
         manifestBackup, err := snapshotManifest("servers/serena/manifest.yaml")
         if err != nil { return err }
         rollback = append(rollback, func() error { return restoreManifest(manifestBackup) })
         if err := writeNewManifest("servers/serena/manifest.yaml", newManifestBody); err != nil { return err }

         // Step 3: registry port allocation + Save + rollback push
         portsBefore := snapshotRegistry(reg)
         if err := allocateSerenaPorts(reg, workspaces); err != nil { return err }
         rollback = append(rollback, func() error { return restoreRegistry(reg, portsBefore) })
         if err := reg.Save(); err != nil { return err }
         rollback = append(rollback, func() error { reg.Workspaces = portsBefore; return reg.Save() })

         // Steps 4-6: InstallParsedManifest owns sub-stack for scheduler tasks +
         // per-client configs + intent write + daemon spawn. Its own rollback
         // runs internally on sub-failure; if it RETURNS an error, the migrate
         // driver's outer rollback also fires (manifest + registry undo).
         if _, err := api.InstallParsedManifest(ctx, newManifest, opts); err != nil { return err }

         // Success — clear rollback so deferred undo is a no-op.
         rollback = nil
         return nil
     }
     ```

   - **Best-effort rollback acknowledged** (defense layer 3 — operator-visible inconsistency): codex v5 finding "existing rollback closures swallow errors at install.go:1696-1704, :1784-1792" is accepted as accurate. Operating-system mutations (scheduler `Delete`, file rename) can fail during undo for reasons orthogonal to the original failure (service stopped, disk re-filled, permission flip). v5's "end-state identical to never-attempted install" overclaim is dropped. v7 contract: **rollback is best-effort; ALL rollback errors are surfaced to the operator via audit event + composite return error**. If rollback is incomplete, the operator sees `rollback-incomplete` in `supervisor-events.log` with the list of failed undo steps and can manually reconcile via `mcphub reconcile` (see Phase A.3 v7 addition below).

   **Outer/inner rollback composition** (closes v6 codex BLOCKER "D.3 outer rollback wraps all 6 mutating steps but pseudocode delegates 4-6 to inner; double-undo risk"):

   The two rollback scopes compose under one explicit rule: **inner stack runs first on sub-failure; if it completes, outer stack runs the remaining undos for steps that the inner stack did NOT own**. Concretely:

   - `InstallParsedManifest` (inner) owns undos for steps 4 (scheduler tasks), 5 (per-client config), and 6 (intent write atomic-rename + daemon spawn). If ANY sub-step fails, the inner stack pops in reverse and runs the undos for steps the function ALREADY completed.
   - `InstallParsedManifest` either RETURNS success (steps 4-6 all committed) OR RETURNS an error with the inner stack ALREADY EXECUTED (steps 4-6 either committed nothing or fully undone).
   - The migrate driver (outer) sees the return:
     - Success → no migrate-driver rollback needed; clear `rollback = nil`.
     - Error → the inner has already handled steps 4-6; the migrate driver's outer stack only runs undos for steps 2 (manifest write) and 3 (registry alloc/save). The outer stack does NOT re-run scheduler/client/intent/daemon undos — those are the inner's responsibility, marked complete by virtue of the function having returned.
   - Pseudocode reflection of this rule:

     ```go
     // Inner pop runs FIRST on InstallParsedManifest's own failure path
     // (no migrate-driver involvement). Migrate driver's outer stack does
     // NOT push scheduler/client/intent/daemon undos — those live inside
     // InstallParsedManifest's own deferred rollback.
     migrateRollback = append(migrateRollback, restoreManifestFn, restoreRegistryFn)
     // NOTE: no scheduler/client/intent/daemon undos pushed to migrateRollback.
     // InstallParsedManifest owns them internally.
     if _, err := api.InstallParsedManifest(ctx, newManifest, opts); err != nil {
         // Inner has already run its sub-stack; outer fires the deferred undo
         // for steps 2+3 (manifest + registry). No risk of double-undo because
         // outer never pushed inner's undos.
         return err
     }
     ```

   - **Daemon-already-started reality** (closes v6 codex BLOCKER "--start-after-write does not compose; current loop starts daemons at install.go:1796-1807"):

     The v6 plan named a `--start-after-write` flag but did not describe the structural change required in `executeInstallTo`. The flag alone cannot defer spawn because the existing loop tightly couples task creation + immediate `sch.Run(...)` start in one iteration. v7 names the structural change concretely:

     **Refactor `executeInstallTo` (`internal/api/install.go:1634`) into two passes**:

     - **Pass A — task creation only**: iterate `p.SchedulerTasks` and call `sch.Create(spec)` for each; collect created task names into a slice; push compensating `sch.Delete(name)` onto rollback. DO NOT call `sch.Run` in this pass.
     - **Intermediate**: per-client config writes (unchanged from current `:1730-1790`).
     - **Intermediate**: `WriteSupervisorIntent` (NEW step explicitly between pass A and pass B for `InstallParsedManifest`; existing for `api.Install` which leaves intent writes to `recordInstallIntentPostSuccess`).
     - **Pass B — task start**: iterate the created task names slice and call `sch.Run(name)`. Pass B is GATED by the `startTasks bool` parameter (NEW): `api.Install` passes `true` (current behavior preserved); `InstallParsedManifest` passes the value of `opts.StartAfterWrite` (default `true`; migrate scenarios pass `false` and the daemons are started later by the reconciler when it picks up the new intent).

     Pseudocode skeleton (replaces a portion of the current install.go:1666-1810 body):

     ```go
     func executeInstallTo(w io.Writer, m *config.ServerManifest, p *Plan, keepN int, startTasks bool) error {
         // ... existing setup (scheduler, workDir, rollback stack) ...
         var createdNames []string
         // Pass A: create only
         for _, t := range p.SchedulerTasks {
             if err := sch.Create(spec); err != nil { /* rollback */ return err }
             createdNames = append(createdNames, t.Name)
             rollback = append(rollback, func() { _ = sch.Delete(t.Name) })
         }
         // ... per-client config writes (unchanged) ...
         // ... WriteSupervisorIntent (new explicit step for InstallParsedManifest path) ...
         if startTasks {
             // Pass B: start
             for _, name := range createdNames {
                 if err := sch.Run(name); err != nil { /* rollback */ return err }
             }
         }
         return nil
     }
     ```

     `api.Install` callers retain `startTasks=true` (no behavior change); `InstallParsedManifest` defaults `opts.StartAfterWrite=true` for direct callers (matching existing semantics) but the migrate driver passes `false` so daemons are started only by the reconciler after the intent flip.
   - **No transient half-states observable to supervisor for the intent file itself**: the supervisor's `IntentWatcher` polls `supervisor-intent.json` mtime. The atomic rename via `WriteStateFileAtomic` means the file either has the OLD content or the NEW content — never partial. Reads racing the rename see one of the two committed states.
   - **Reconcile-on-startup defense** (defense layer 4): the supervisor's reconciler on cold restart re-reads the intent file and compares against scheduler-registered tasks. Tasks-without-intent are surfaced as drift; intent-entries-without-task are reconciled by spawning. This defends against the case where the migrate driver crashes mid-sequence (e.g., OS reboot during step 4) and leaves an inconsistent state that no rollback closure could undo because the process terminated abruptly.

   **IntentWatcher.Run wiring** (v4 header closure carried into v5): `IntentWatcher` is currently defined in `supervise_watcher.go` but its `Run` method is NOT invoked from `runSupervise()`. Phase A.2 wires this. D.3's migration sequence relies on the wiring being live; if A.2 ships first, the migrate command's intent-write triggers a reconcile within the watcher's poll interval. If A.2 has not shipped, migrate prints an operator-facing warning: "supervisor will not auto-reload — run `mcphub supervise` restart to apply manifest changes."

   **Duplication concern** (codex Q4 finding): `api.InstallParsedManifest` and `api.Install` share ~40 lines of plumbing (BuildPlanWithOpts, audit-first emission, executeInstallTo loop). v5 extracts the shared body into an unexported `(a *API) installPlan(ctx, m, plan, opts) error` helper called by both. `api.Install` keeps its global-server entrypoint with `refuseWorkspaceScopedInstall` gate; `api.InstallParsedManifest` keeps its workspace-scoped entrypoint with the pre-loaded workspace snapshot. Both call `installPlan` for the actual mutation work. Net code growth: ~80 lines (struct + new function + tests).

   **v5 migration sequence** (replaces v3 step list at this location):

   1. acquire `Registry.Lock()` for cross-process safety (`workspace_registry.go:169-178`)
   2. read current registry; build `[]WorkspaceEntry` snapshot of serena workspaces (`reg.SerenaEntries()`)
   3. write new `servers/serena/manifest.yaml` with `daemon_template` block (atomic via existing `SecureWriteClientConfig` pipeline)
   4. invoke `api.InstallParsedManifest(ctx, newManifest, InstallParsedManifestOpts{Workspaces: snapshot, ...})` IN-PROCESS — the new exported seam
   5. the seam writes scheduler tasks + per-client config entries + supervisor-intent.json under a single shared rollback stack
   6. `IntentWatcher.detectChange()` (`supervise_watcher.go:193-200`) detects intent mtime change on next poll tick and fires `onChange` → reconciler picks up new descriptors (assuming A.2 wired the watcher into runSupervise)
   7. release Registry lock

   **IntentWatcher default poll** (sonnet v2 MINOR-2 fix, unchanged in v5): `NewIntentWatcher` defaults `pollInterval` to `60 * time.Second` when `pollInterval <= 0` (`supervise_watcher.go:108-110`). **Operator-facing behavior**: migration prints "supervisor will pick up new intent within 60s (next IntentWatcher tick); no manual restart required."

   **Why in-process vs shell-out** (unchanged): shell-out has multiple failure modes (operator's PATH, mcphub binary version mismatch, intent file lock races against another mcphub process). In-process call uses the same Go functions the install command does, with the Registry lock held, so all writes are atomic relative to other registry mutations.

**Acceptance criteria**:

- Idempotent: detection predicate returns "already migrated" if rerun; no writes, exit 0
- Refuses if no serena workspaces registered (clear error: "register at least one workspace before migration")
- Preserves per-workspace `.serena/cache/` directories (no disk write inside workspace dirs)
- Audit event `serena-dynamic-pool-migration` written with body `{source_state, target_workspaces, allocated_ports}`
- Reconciler picks up new descriptors within `intent_watcher_poll_interval` (no IPC required)
- v7-introduced helpers explicitly named (closes v6 sonnet IMPORTANT B.5): `snapshotManifest(path string) ([]byte, error)`, `restoreManifest(backup []byte) error`, `snapshotRegistry(reg *Registry) []WorkspaceEntry`, `restoreRegistry(reg *Registry, snapshot []WorkspaceEntry) error`, `allocateSerenaPorts(reg *Registry, workspaces []WorkspaceEntry) error`, `writeNewManifest(path string, body []byte) error` — all live in new file `internal/cli/migrate_serena.go`
- v7-introduced new packages (closes v6 sonnet IMPORTANT B.4): `internal/api/serena_routing/` (owns `SerenaHealthLookup` interface + `WorkspaceResolver`); `internal/api/hubmcp/` (owns `ToolDescriptor` + `HandleCall` for hub-local tools) — both created as part of this phase
- `--start-after-write` flag explicitly listed on `InstallParsedManifestOpts`: `StartAfterWrite bool` (default true; migrate driver passes false)
- `mcphub reconcile` operator command (closes v6 sonnet IMPORTANT B.6) — see Phase A.3 v7 addition

**Test contract**:

- `TestMigrateSerena_DetectsLegacy2Daemon`
- `TestMigrateSerena_DetectsUnifiedIntermediate`
- `TestMigrateSerena_DetectsAlreadyMigrated_NoOp`
- `TestMigrateSerena_RejectsMalformedManifest`
- `TestMigrateSerena_RejectsEmptyWorkspaceRegistry`
- `TestMigrateSerena_AllocatesPortsForEachWorkspace`
- `TestMigrateSerena_WritesAuditEvent`
- `TestMigrationDriver_RollbackOnIntentWriteFailure_RestoresManifestAndRegistry` — inject failure at intent write step; verify manifest backup restored + registry ports rolled back
- `TestMigrationDriver_RollbackIncompleteAuditEventOnUndoFailure` — inject failure in one undo closure; verify `rollback-incomplete` audit event emitted with the failed-step list AND composite return error includes the rollback error
- `TestInstallParsedManifest_StartAfterWriteFalse_DefersDaemonSpawn` — Pass A creates tasks, intent write succeeds, but `sch.Run` is NOT called inside `executeInstallTo`; daemons start only on the next reconciler tick after watcher fires `EvIntentUpdate`
- `TestInstallParsedManifest_StartAfterWriteTrue_PreservesLegacyBehavior` — when called from `api.Install` path, Pass B runs and daemons start immediately (regression guard for existing global-install semantics)
- `TestExecuteInstallTo_PassAPassB_Separation` — verify the structural change to executeInstallTo: created-tasks slice captured between Pass A and Pass B; `startTasks=false` skips Pass B; rollback on Pass B failure undoes Pass A creates

---

## Phase E: Auto-register on miss

### E.1: File-extension survey helper

**Scope**: function used by both `mcphub workspace bootstrap` (B.3) and auto-register Mode 3 (E.2).

```go
func SurveyLanguages(absPath string, maxDepth int) ([]string, error)
```

**Acceptance criteria**:
- Walks `<path>` to max depth (default 5)
- Respects .gitignore (parse + match)
- Skips heavy dirs: `node_modules`, `target`, `dist`, `.git`, `__pycache__`, `.venv`
- Returns sorted unique languages from extension map (defined in single source — `internal/api/language_detection.go`)
- Returns empty slice (NOT error) if no recognized extensions

### E.2: Auto-register on miss flow

**Scope**: mcphub-router (Phase C.2) extension — when ResolveByPath returns ErrWorkspaceNotFound:

1. Call `SurveyLanguages(path)`
2. If languages detected: create `<path>/.serena/project.yml`, allocate port, write workspaces.yaml entry, spawn daemon, audit event, forward request
3. If no languages: HTTP 422 with explicit "no recognizable code files at <path>" message
4. If spawn fails: HTTP 503 + audit event + revert workspaces.yaml entry

**Acceptance criteria**:
- Atomic: either fully registered + spawned + responding, or fully rolled back
- Audit event `workspace-auto-registered` with body `{path, languages, port, trigger_tool, trigger_path}`
- Daemon ready (HTTP 200 on /mcp) within bounded time (30s default; configurable)

**Test contract**:
- `TestAutoRegister_SuccessPath_FullFlow`
- `TestAutoRegister_NoLanguagesDetected_HTTP422`
- `TestAutoRegister_SpawnFailure_RollsBack`

---

## Phase F: No-path-args routing — concrete strategy per Decision 5

Per Decision 5 (resolved by codex consult 2026-05-20), Phase F implements the tool-group strategy table inline. Three sub-phases F.1-F.3 implement the three rows of the Decision-5 table.

### F.1: Read-only no-path tools — sticky-when-bound, aggregate-when-unbound

**Tools**: `list_memories`, `check_onboarding_performed`, `get_current_config`.

**Bound session** (sticky-session map populated by prior path-aware call):
- Forward to the workspace's serena daemon
- Pass response through unchanged

**Unbound session** (no prior path-aware call in this `Mcp-Session-Id`):
- For each registered serena workspace, issue the same tool-call to that workspace's daemon in parallel (bounded N, default max 8 parallel)
- Build aggregate response with workspace-keyed result map:
  ```json
  {"results": {"D:\\dev\\PaperPane": [...], "D:\\dev\\mcp-local-hub": [...]}}
  ```
- Do NOT flatten into "native serena shape" without `workspace` key (codex constraint: client must see which workspace each result came from)
- Special case: `get_current_config` returns hub-summary (number of workspaces, sticky-state, port allocation) PLUS per-daemon `config:` array

**Acceptance criteria**:
- Sticky path: HTTP 200, single-workspace native response shape
- Unbound aggregate path: HTTP 200, workspace-keyed map with all registered serena workspaces present
- Parallel fan-out respects N-bound concurrency limit
- Single-workspace-registry shortcut: if exactly one registered serena workspace, route to it directly (no aggregate wrapping) — saves clients the need to drill into wrapper

**Test contract**:
- `TestSerenaRouter_ListMemoriesBound_SingleWorkspaceResponse`
- `TestSerenaRouter_ListMemoriesUnbound_AggregateAllWorkspaces`
- `TestSerenaRouter_GetCurrentConfigUnbound_HubSummaryShape`
- `TestSerenaRouter_SingleWorkspaceRegistry_NoAggregateWrapping`

### F.2: `read_memory <name>` — strict disambiguation when unbound (v5)

**Bound session**: sticky-forward to the workspace's serena daemon. Pass response through unchanged (including transport-level JSON-RPC envelope).

**Unbound session**:
- Query all registered serena daemons in parallel
- Collect responses; classify each via the two-layer success predicate below
- Cases:
  - Exactly 1 success: return that workspace's response unchanged + `X-Serena-Workspace: <abs-path>` response header (so client can sticky-bind explicitly going forward)
  - 0 successes: HTTP 404 with body `{"error": "memory '<name>' not found in any registered serena workspace"}`
  - 2+ successes: HTTP 409 Conflict with body `{"error": "memory '<name>' exists in multiple workspaces", "workspaces": ["D:\\dev\\PaperPane", "D:\\dev\\mcp-local-hub"], "guidance": "call a path-aware tool first to bind workspace, or use hub.bind_workspace explicitly"}`
- Codex constraint: do NOT use "first success wins" — that silently leaks the wrong workspace's memory contents

**JSON-RPC + HTTP success predicate** (closes v4 BLOCKER F.2 codex finding): MCP transport is JSON-RPC over HTTP; HTTP 200 can still carry a JSON-RPC error envelope `{"jsonrpc":"2.0","id":..,"error":{"code":-32602,"message":"memory not found"}}`. Naive "HTTP 200" classification would count error responses as success and trigger spurious 409 disambiguation. Use two layers:

```go
// classifyReadMemoryResponse returns (isHit, reason) for one upstream response.
// Order: HTTP-status gate → JSON-RPC envelope-shape gate → error-field gate →
// result-shape gate.
//
// Envelope-shape gate (v6 closure of v5 sonnet finding): the JSON-RPC 2.0 spec
// requires the response to carry `jsonrpc: "2.0"` AND `id: <number|string|null>`.
// A body like `{"result":"x"}` is NOT a valid JSON-RPC response and must not
// count as a hit — it would only arise from a buggy or non-conforming upstream.
// Without this gate, an HTTP 200 + arbitrary `{"result":"x"}` from a corrupted
// or misconfigured serena daemon could spuriously satisfy the 1-success branch.
func classifyReadMemoryResponse(resp *http.Response, body []byte) (bool, string) {
    if resp.StatusCode != http.StatusOK {
        return false, fmt.Sprintf("http-%d", resp.StatusCode)
    }
    var env struct {
        JSONRPC string          `json:"jsonrpc"`
        ID      json.RawMessage `json:"id"`
        Result  json.RawMessage `json:"result,omitempty"`
        Error   *struct {
            Code    int    `json:"code"`
            Message string `json:"message"`
        } `json:"error,omitempty"`
    }
    if err := json.Unmarshal(body, &env); err != nil {
        return false, "malformed-jsonrpc"
    }
    // Envelope shape gate — both fields MUST be present per JSON-RPC 2.0.
    if env.JSONRPC != "2.0" {
        return false, "missing-or-wrong-jsonrpc-version"
    }
    if len(env.ID) == 0 {
        return false, "missing-jsonrpc-id"
    }
    if env.Error != nil {
        // upstream signalled error per JSON-RPC; not a hit. Specific error codes
        // (e.g. -32602 "memory not found") are routed to the 0-successes branch.
        return false, fmt.Sprintf("jsonrpc-error-%d", env.Error.Code)
    }
    if len(env.Result) == 0 || string(env.Result) == "null" {
        return false, "empty-result"
    }
    // Result body is shape-valid; it's a hit.
    return true, ""
}
```

**Special case**: memory name starting with `global/` (per serena convention) — can be de-duped/read-once across the pool because global memories are by-name unique. Acceptance criterion: documented behavior for `global/*` is "read first daemon's response since global memories are by-name unique by serena convention". Defer cross-pool global memory sync to v2.

**Test contract**:

- `TestSerenaRouter_ReadMemoryUnbound_ExactlyOneMatch_Returns200`
- `TestSerenaRouter_ReadMemoryUnbound_ZeroMatches_Returns404`
- `TestSerenaRouter_ReadMemoryUnbound_MultipleMatches_Returns409Disambiguation`
- `TestSerenaRouter_ReadMemoryUnbound_GlobalNamespace_FirstDaemonWins`
- `TestClassifyReadMemoryResponse_JSONRPCErrorCounts_AsZeroHits` — HTTP 200 + `{"error":{"code":-32602,...}}` body must NOT count as success
- `TestClassifyReadMemoryResponse_EmptyResultCountsAsZeroHits` — HTTP 200 + `{"result":null}` must NOT count as success
- `TestClassifyReadMemoryResponse_MalformedJSONRPCCountsAsZero` — non-JSON or missing envelope fields count as misses, not panics
- `TestClassifyReadMemoryResponse_RejectsMissingJSONRPCVersion` — body `{"result":"x"}` (no `jsonrpc:"2.0"` field) is NOT a hit (v7 envelope-shape gate)
- `TestClassifyReadMemoryResponse_RejectsMissingID` — body `{"jsonrpc":"2.0","result":"x"}` (no `id` field) is NOT a hit (v7 envelope-shape gate)

### F.3: `write_memory` / `delete_memory` / `onboarding` — fail-closed unbound (v5)

**Bound session**: sticky-forward to the workspace's serena daemon. Pass response through unchanged.

**Unbound session**:

- Return HTTP 412 Precondition Failed with body:
  ```json
  {
    "error": "no workspace bound for this MCP session",
    "guidance": "call a path-aware serena tool first (find_symbol, search_for_pattern, etc.) OR use 'hub.bind_workspace <abs-path>' to explicitly bind"
  }
  ```
- DO NOT default-route to any workspace — codex constraint: silent writes to wrong project state are unrecoverable corruption
- Exception: single-workspace-registry shortcut — IF and ONLY IF a health gate passes (see below)

**Single-workspace-registry shortcut + health gate** (closes v4 BLOCKER F.3 codex finding "single-workspace shortcut without health check"):

When exactly one registered serena workspace exists, the unbound-write rejection is too coarse — operators with a single project want their `write_memory` calls to route there without ceremony. But routing to a dead daemon produces an opaque connection error, not the actionable 412 + guidance the user needs. The shortcut adds a pre-route health gate:

```go
// shouldUseSingleWorkspaceShortcut returns true iff:
//   1. exactly one serena workspace is registered, AND
//   2. that workspace's daemon is healthy per the supervisor state.
//
// The health predicate uses the SerenaHealthLookup interface — a NARROW seam
// exposed by the cli-layer supervisorController (A.2) for the api/router layer
// to read the per-task api.SMState WITHOUT a direct cli→api dependency reversal.
// The api package defines the interface; the cli package implements it on
// supervisorController via the public GetSMState(taskName) (api.SMState, bool)
// method introduced in A.2 v6. Healthy = api.StRunning. Unhealthy states
// (StBackoffWaiting, StQuarantined, StSpawning, StIdle, StExiting) all return
// false → shortcut declined, fall through to the 412 rejection so the operator
// sees a clear "your serena daemon for D:\dev\Foo is in quarantine — fix that
// before writing" diagnostic instead of an opaque connection timeout.
//
// Cross-package dependency note (closes v5 sonnet finding "api router cannot
// directly depend on cli controller" + v6 codex finding "F.3 wiring gap —
// current hub construction is at internal/gui/hub_listener.go:182 via
// api.NewHubMcpHandler(store), NOT runSupervise"):
//
// The SerenaHealthLookup interface lives in a NEW sub-package
// `internal/api/serena_routing/` (created by Phase C; declared explicitly as
// NEW in the v7 acceptance criteria below). The api package never imports
// internal/cli; cli already imports api packages, so cli→api remains the
// established direction.
//
// Real wiring path (v7 closure of codex finding):
//   1. `runSupervise` in internal/cli/supervise.go:315 constructs the
//      `supervisorController` (A.2). The controller is reachable from the
//      cli package only.
//   2. `mcphub gui` boots the hub-mcp HTTP server in internal/gui/hub_listener.go;
//      at line 182 it calls `api.NewHubMcpHandler(store)`. The hub-mcp handler is
//      shared with the supervisor process via the supervisor IPC `status` seam
//      (already wired in PR for supervisor IPC status — see CLAUDE.md "Supervisor
//      (v0.5.0)" section "GUI Dashboard status is now sourced through the
//      supervisor IPC status seam").
//   3. v7 extends `api.NewHubMcpHandler` to accept an optional
//      `SerenaHealthLookup` parameter:
//        `func NewHubMcpHandler(store *Store, opts ...HubMcpHandlerOpt) *HubMcpHandler`
//        `func WithSerenaHealthLookup(h SerenaHealthLookup) HubMcpHandlerOpt`
//      gui/hub_listener.go passes `WithSerenaHealthLookup(supervisorIPCHealthLookup)`
//      where `supervisorIPCHealthLookup` is a thin adapter that translates
//      `GetSMState(taskName)` into a supervisor IPC `status` round-trip and
//      reads the per-task SM state from the response.
//   4. Inside `mcphub supervise` (the supervisor process itself), the
//      controller's `GetSMState` is wired directly to the in-process hub
//      router instance (also constructed via `NewHubMcpHandler`, this time
//      with the direct controller as the SerenaHealthLookup impl — no IPC
//      hop needed for the in-process case).
type SerenaHealthLookup interface {
    GetSMState(taskName string) (api.SMState, bool)
}

func (r *SerenaRouter) shouldUseSingleWorkspaceShortcut() (*WorkspaceEntry, bool) {
    serena := r.registry.SerenaEntries()
    if len(serena) != 1 {
        return nil, false
    }
    ws := &serena[0]
    if r.health == nil {
        return nil, false // controller not wired — fail closed
    }
    state, ok := r.health.GetSMState(ws.TaskName)
    if !ok || state != api.StRunning {
        return nil, false
    }
    return ws, true
}
```

**Health-failure path**: when the shortcut declines because the single daemon is unhealthy, the 412 response body adds a `daemon_state` field so the operator knows WHY:

```json
{
  "error": "no workspace bound for this MCP session",
  "daemon_state": "quarantined",
  "registered_workspaces": ["D:\\dev\\Foo"],
  "guidance": "the only registered workspace's serena daemon is quarantined; run `mcphub supervise` cold-restart to clear the 30-min window, or use 'hub.bind_workspace D:\\dev\\Foo' to explicitly bind once the daemon recovers"
}
```

**Acceptance criteria**:

- Unbound write with zero-or-multi workspaces → HTTP 412 + explicit guidance message (no silent default)
- Single-workspace shortcut routes only when the daemon's SM state is `StRunning`; any other state → 412 with `daemon_state` populated
- Each rejection emits audit event `serena-write-unbound-rejected` with body `{tool, session_id_hash, registered_workspace_count, daemon_state}` (`daemon_state` empty string when zero-or-multi workspaces)

**Test contract**:

- `TestSerenaRouter_WriteMemoryUnbound_Returns412`
- `TestSerenaRouter_DeleteMemoryUnbound_Returns412`
- `TestSerenaRouter_OnboardingUnbound_Returns412`
- `TestSerenaRouter_WriteMemorySingleWorkspaceShortcut_HealthyReturns200`
- `TestSerenaRouter_WriteMemorySingleWorkspaceShortcut_QuarantinedReturns412WithDaemonState`
- `TestSerenaRouter_WriteMemorySingleWorkspaceShortcut_BackoffWaitingReturns412`
- `TestSerenaRouter_WriteMemoryUnboundEmitsAuditEvent`

### F.4: Sticky-session map implementation (v5)

**Storage**: in-process map `map[string]*WorkspaceEntry` keyed by `Mcp-Session-Id` header value. Protected by `sync.RWMutex`. Lazy expiration: TTL 24h since last call (configurable via `mcphub config sticky-ttl`).

**Atomic snapshot release before fan-out** (closes v4 BLOCKER F.4 codex finding "fan-out lock held across upstream calls"):

The naive implementation holds `mu.RLock()` for the entire fan-out duration in F.1/F.2's unbound branch, which means every concurrent path-aware call (which would `mu.Lock()` to bind a new session) blocks waiting for the fan-out's parallel HTTP calls to return. Under load this serializes the hub. Fix: snapshot the relevant map entries under the RLock, release the lock, then perform the upstream calls against the snapshot:

```go
// resolveBoundWorkspace looks up the session's bound workspace under the lock
// and returns the resolved pointer (or nil). The returned WorkspaceEntry is
// a value copy — readers must NOT retain a pointer into the live map.
func (s *StickyMap) resolveBoundWorkspace(sessionID string) (WorkspaceEntry, bool) {
    s.mu.RLock()
    defer s.mu.RUnlock()
    e, ok := s.m[sessionID]
    if !ok {
        return WorkspaceEntry{}, false
    }
    return *e, true // value copy under the lock
}

// snapshotForFanout takes the unbound-fan-out target set: a value-copy slice
// of ALL serena WorkspaceEntries known to the registry, captured under the
// registry's own lock. After this returns, the caller holds NO lock; fan-out
// HTTP calls operate purely on the value-copy slice.
func (r *SerenaRouter) snapshotForFanout() []WorkspaceEntry {
    return r.registry.SerenaEntries() // already returns a value-copy slice
}

// handleUnboundReadMemory shows the lock release pattern.
func (r *SerenaRouter) handleUnboundReadMemory(req *http.Request, body []byte) (*http.Response, error) {
    // Step 1: snapshot under the lock (RLock released by SerenaEntries return).
    targets := r.snapshotForFanout()
    if len(targets) == 0 {
        return notFoundResponse("no registered serena workspaces"), nil
    }
    // Step 2: fan out HTTP calls against the snapshot. NO LOCK HELD here.
    results := r.fanOutBounded(req.Context(), targets, body, fanoutConcurrency)
    // Step 3: classify and aggregate (lock-free; results is goroutine-local).
    return aggregateReadMemoryResults(results), nil
}
```

The same pattern applies in F.1's aggregate path: registry snapshot → release → bounded parallel fan-out → aggregate.

**Hook points**:

- On every path-aware tool-call response success → `sticky[session_id] = resolved_workspace` (idempotent if already bound to same workspace)
- On HTTP 404 from upstream (session expired per MCP spec) → evict `sticky[session_id]`
- On explicit MCP DELETE on `Mcp-Session-Id` (per MCP spec §"Session Management") → evict
- On `hub.bind_workspace <abs-path>` (new **MCP/HTTP tool**, NOT supervisor IPC) → set `sticky[session_id]` explicitly; refuses if session already bound to different workspace unless `force: true` param

**`hub.bind_workspace` belongs on MCP/HTTP layer, not supervisor IPC** (closes v4 BLOCKER F.4 codex finding "hub.bind_workspace as supervisor IPC questionable" + v5 codex finding "hub-local MCP tool integration missing"):

Supervisor IPC (`supervise.go`'s named-pipe / unix-socket) is for process-lifecycle commands (status, restart, exit, quiesce-timers) — operator-facing, single owner, no session concept. Session binding lives in the hub-mcp HTTP layer where every request already carries `Mcp-Session-Id` and goes through the SerenaRouter. v6 places `hub.bind_workspace` as an MCP tool exposed by the hub itself AND specifies how it integrates with the existing `internal/api/hub_mcp_aggregator.go` daemon-fanout aggregator.

**Integration with hub_mcp_aggregator** (the v5 missing piece):

The existing `AggregateToolsList` (`hub_mcp_aggregator.go:228`) builds a merged `tools/list` response by fanning out to every daemon and namespacing each tool as `<server>__<rawname>`. `tools/call` then loads the RouteMap snapshot and forwards `params.name` to the right daemon. `hub.bind_workspace` is NOT a daemon-backed tool — it is hub-local — so two integration points are required:

1. **tools/list injection**: AggregateToolsList appends a hub-local `namespacedTool` entry AFTER the daemon fan-out completes. The exposed name is `mcphub__bind_workspace` (matching the existing `<server>__<rawname>` convention with `server="mcphub"` as a reserved sentinel). The tool descriptor is constant — no per-session shape change.
2. **tools/call dispatch bypass**: the `tools/call` handler at `internal/api/hub_mcp_aggregator.go` (see line range around `:497-548` per codex's spot-check) currently looks up `params.name` in the RouteMap and forwards. v6 adds an early-return branch: if `params.name == "mcphub__bind_workspace"`, dispatch to a hub-local handler that:
   - reads `Mcp-Session-Id` from the request context (already propagated through the router)
   - resolves the `workspace_path` argument against `registry.SerenaEntries()` (refuses path not in the registered set)
   - calls `stickyMap.Bind(sessionID, ws, force)` under the write lock
   - returns a JSON-RPC `result` with `{"workspace_path": "<abs>", "previously_bound": "<abs|nil>", "rebound": <bool>}`
   - emits `serena-session-bound` or `serena-session-rebound` audit event

```go
// internal/api/hubmcp/bind_workspace.go — new file owned by the hub-mcp package.
const HubLocalToolBindWorkspace = "mcphub__bind_workspace"

// ToolDescriptor returns the MCP tools/list descriptor for hub.bind_workspace.
// Called by AggregateToolsList after the daemon fan-out merges.
func ToolDescriptor() namespacedTool {
    return namespacedTool{
        Exposed: HubLocalToolBindWorkspace,
        Server:  "mcphub",
        RawName: "bind_workspace",
        Schema: jsonSchema(`{
            "type": "object",
            "properties": {
                "workspace_path": {"type": "string", "description": "absolute path of registered serena workspace"},
                "force": {"type": "boolean", "default": false, "description": "rebind even if already bound to a different workspace"}
            },
            "required": ["workspace_path"]
        }`),
        Description: "Bind this MCP session to a specific registered serena workspace for sticky no-path routing.",
    }
}

// HandleCall is the hub-local dispatch entry-point, called by the tools/call
// handler when params.name == HubLocalToolBindWorkspace. Bypasses RouteMap.
func HandleCall(ctx context.Context, sessionID string, params map[string]any, sticky *StickyMap, registry *WorkspaceRegistry, events *api.SupervisorEventLog) (map[string]any, error) {
    wsPath, _ := params["workspace_path"].(string)
    force, _ := params["force"].(bool)
    if wsPath == "" {
        return nil, jsonRPCError(-32602, "workspace_path is required")
    }
    serena := registry.SerenaEntries()
    var match *WorkspaceEntry
    for i := range serena {
        if serena[i].WorkspacePath == wsPath {
            match = &serena[i]
            break
        }
    }
    if match == nil {
        return nil, jsonRPCError(-32602, fmt.Sprintf("workspace_path %q is not registered as a serena workspace; available: %v", wsPath, serenaPaths(serena)))
    }
    prev, rebound, err := sticky.Bind(sessionID, match, force)
    if err != nil {
        // typically: bound to different workspace + force=false
        return nil, jsonRPCError(-32603, err.Error())
    }
    event := "serena-session-bound"
    if rebound {
        event = "serena-session-rebound"
    }
    _ = events.Emit(api.SupervisorEvent{
        Severity: "info", Source: "ipc", Event: event,
        Body: map[string]any{"session_id_hash": hashSessionID(sessionID), "workspace_path": wsPath, "previously_bound": prev},
    })
    return map[string]any{"workspace_path": wsPath, "previously_bound": prev, "rebound": rebound}, nil
}
```

Wire-up locations (the implementer-facing integration points, v7 closure of v6 codex finding "handler dependency gap"):

The existing `AggregateToolsCall(ctx, sess *hubSession, clientReqID, paramsRaw)` (`hub_mcp_aggregator.go:504`) carries only the session — it does NOT have access to the sticky map, the workspace registry, or the audit event log. v7 closes this gap by attaching hub-local-tool dependencies to the `hubSession` itself (the session is constructed once per MCP session and naturally outlives any single tools/call), so `AggregateToolsCall`'s signature is UNCHANGED:

```go
// hubSession (existing type at internal/api/hub_session.go or equivalent)
// gets new fields wired at session construction time. None of the new fields
// participate in concurrency-sensitive paths the existing session already
// owns; they are read-only references to long-lived dependencies.
type hubSession struct {
    // ... existing fields (RouteMap atomic.Pointer, Mcp-Session-Id, etc.) ...

    // NEW v7 hub-local-tool dependencies:
    hubLocal *HubLocalDeps // nil for sessions where hub-local tools are disabled (legacy / test)
}

// HubLocalDeps carries the long-lived references that hub-local tool handlers
// need. Constructed once by NewHubMcpHandler and attached to every session it
// creates. The dispatcher uses an interface seam so tests can substitute fakes.
type HubLocalDeps struct {
    Sticky   *StickyMap
    Registry *WorkspaceRegistry
    Events   *api.SupervisorEventLog
    Health   SerenaHealthLookup // F.3's interface; nil for non-supervisor processes
}
```

Wire-up locations:

- **`NewHubMcpHandler` constructor** (`internal/api/hub_mcp.go` or equivalent; wired from `internal/gui/hub_listener.go:182`): extended to accept optional functional options (`HubMcpHandlerOpt`) including `WithHubLocalDeps(*HubLocalDeps)`. The handler stores the deps and passes them to every `hubSession` it creates. For supervisor-process callers, `HubLocalDeps.Health` is the in-process `supervisorController.GetSMState`; for GUI-process callers (the `mcphub gui` command), `Health` is the IPC-adapter described in F.3.
- **`AggregateToolsList`** (`hub_mcp_aggregator.go:238` — note: the function body STARTS at line 238 per fresh grep on HEAD `bd552ee`; the v6 reference to `:228` pointed at the docstring comment block above the function): after the daemon fan-out builds the merged tool list AND publishes the RouteMap, IF `sess.hubLocal != nil` append `hubmcp.ToolDescriptor()` to the result. The hub-local tool does NOT go into the RouteMap (no daemon to route to).
- **`AggregateToolsCall`** (`hub_mcp_aggregator.go:504`): at function entry, before RouteMap lookup, check `if p.Name == HubLocalToolBindWorkspace && sess.hubLocal != nil { return hubmcp.HandleCall(ctx, sess, clientReqID, paramsRaw) }`. Place the branch AFTER params parsing (the existing `var p struct{Name string}` block at `:527-537`) AND BEFORE the RouteMap snapshot fetch at `:540`. This way:
  - Missing `name` still returns the existing `-32602 Invalid params: missing name` at `:535`.
  - Hub-local dispatch bypasses RouteMap entirely (no `Method not found` race against an empty RouteMap).
  - Daemon-routed tools fall through to the existing `:540-549` lookup unchanged.
- **`HandleCall` signature**: re-grounded to use the existing `*hubSession` shape so it slots in cleanly:

  ```go
  func HandleCall(ctx context.Context, sess *hubSession, clientReqID, paramsRaw json.RawMessage) ([]byte, error) {
      var p map[string]any
      if err := json.Unmarshal(paramsRaw, &p); err != nil {
          return buildJSONRPCError(clientReqID, -32602, "Invalid params: "+err.Error(), nil)
      }
      args, _ := p["arguments"].(map[string]any)
      wsPath, _ := args["workspace_path"].(string)
      force, _ := args["force"].(bool)
      // ... validation + sticky.Bind + audit emit ...
      result := map[string]any{"workspace_path": wsPath, "previously_bound": prev, "rebound": rebound}
      return buildJSONRPCResult(clientReqID, result)
  }
  ```

- **Per-client tool filtering**: if a client uses tool-glob filters (existing `ClientBinding.URLPath` machinery), `mcphub__*` tools are NOT auto-filtered out — they appear in tools/list for every connected client. If an operator wants to hide them on a specific client, they add an explicit deny-glob to that client binding.

**Acceptance criteria**:

- Sticky binding correctly resolves on subsequent no-path calls
- Map lookup is O(1)
- Eviction on 404 from upstream + explicit DELETE + TTL expiry
- `hub.bind_workspace` MCP tool works idempotent (re-bind to same workspace = no-op)
- `hub.bind_workspace` exposed on the hub-mcp endpoint, NOT on supervisor IPC; `mcphub supervise` does not implement a `bind_workspace` IPC verb
- Fan-out paths (F.1 unbound aggregate, F.2 unbound read_memory) hold the sticky-map lock ONLY for the snapshot read; HTTP calls run lock-free against the value-copy snapshot
- Audit event `serena-session-bound` on first bind, `serena-session-rebound` on explicit override, `serena-session-evicted` on eviction

**Test contract**:

- `TestStickySession_BindOnFirstPathCall`
- `TestStickySession_LookupAfterBind_O1`
- `TestStickySession_Evict_OnHTTP404FromUpstream`
- `TestStickySession_Evict_OnExplicitDELETE`
- `TestHubBindWorkspaceTool_Idempotent` — re-bind to same workspace = no-op
- `TestHubBindWorkspaceTool_RejectsRebindWithoutForce`
- `TestHubBindWorkspaceTool_ExposedOnMCPLayerNotSupervisorIPC` — regression guard that supervisor IPC has no `bind_workspace` verb
- `TestFanout_ReleasesLockBeforeUpstreamCalls` — concurrent `bind` calls do not block on an in-flight fan-out's upstream RTT
- `TestAggregateToolsList_AppendsMcphubBindWorkspaceWhenHubLocalDepsPresent` — v7 wire-up: tools/list response includes `mcphub__bind_workspace` ONLY when `sess.hubLocal != nil`
- `TestAggregateToolsList_OmitsHubLocalToolWhenDepsAbsent` — regression guard for legacy/test sessions without HubLocalDeps
- `TestAggregateToolsCall_RoutesHubLocalToolBypassingRouteMap` — v7 dispatch: `params.name == "mcphub__bind_workspace"` returns from HandleCall WITHOUT calling resolveToolsCallRoute (no `-32601 Method not found` race against empty RouteMap)
- `TestAggregateToolsCall_FallsThroughToRouteMapForDaemonTools` — regression guard that daemon-routed tools still go through the existing path
- `TestHubLocalDeps_WiredFromGuiHubListener` — `NewHubMcpHandler(store, WithHubLocalDeps(deps))` actually propagates `deps` to every constructed session

---

## Phase G: Cleanup of legacy 2-daemon

### G.1: Remove legacy `claude` + `codex` (or `unified`) daemon descriptors from `servers/serena/manifest.yaml`

**Scope**: after D.3 migration is verified on operator's machine, remove the legacy `daemons:` block from manifest.yaml entirely. Keep only `daemon_template`.

**Acceptance criteria**:
- Manifest validation requires either `daemons:` (legacy) OR `daemon_template:` (dynamic-pool), not both
- Schema strict-parse rejects both-present
- Existing legacy clients that hardcoded `localhost:9121` are still served via mcphub-router on the constant client-facing endpoint

### G.2: Remove `client_bindings:` from `servers/serena/manifest.yaml`

**Scope**: per spec §3 Часть Б, client_bindings become unused in dynamic-pool (router handles all bindings).

**Acceptance criteria**:
- Migration script generates per-client config rewrites that point to mcphub-router endpoint instead of individual serena ports
- Removed `client_bindings` field from struct OR allowed-but-ignored for backward compat (decide in v2)

---

## Phase H: Operational hygiene tooling (parallel to B-E)

Motivated by the 2026-05-20 operational evidence + cleanup intervention above. Provides operator-visible recovery paths for the per-subagent stdio fan-out class of failure. **Parallel** to B-E, not blocking — the architectural fix (hub-routing config) is in Phases B-E; Phase H is the emergency override when those fail or are slow to roll out.

### H.1: `mcphub cleanup --aggressive` CLI mode

**Scope**: extend `internal/cli/cleanup.go` (current implementation at `cleanup.go:24-33, 121-126` already has `--dry-run`/`--confirm`) with an `--aggressive` flag that opts INTO killing live-rooted MCP-stdio processes that the default safety guard correctly refuses.

**Contract** (per codex v3+audit review IMPORTANT-6 — tighter than initial draft):

- `--aggressive` REQUIRES one of: `--client <name>` (e.g. `codex-cli`) OR `--root-pid <pid>` — no implicit "all live-rooted" mode
- Dry-run preview is MANDATORY when `--aggressive` set: dispatches `--dry-run` automatically, prints candidate list (PID, name, parent chain, match source), THEN waits for explicit positive operator confirmation via second invocation OR `--confirm-aggressive-token <random-token-from-dry-run>`
- DENY-list by default: `cmd.exe`, `chrome.exe`, `conhost.exe`, `pwsh.exe`, `powershell.exe` excluded from kill targets even under `--aggressive` (operator terminals + Playwright sessions). Override via separate `--include-class <name>` flag per excluded name, with stderr warning.
- Existing `mcphub.exe daemon` ancestor exclusion remains (no aggressive mode bypasses hub-managed processes)
- Per-PID match-source must appear in output: which manifest pattern matched, which ancestor walked the gate, why included

**Acceptance criteria**:
- `mcphub cleanup --aggressive` without `--client`/`--root-pid` → exits non-zero with explicit "scope required" message
- `mcphub cleanup --aggressive --client codex-cli` (no token) → prints candidate list + per-PID match-source + a confirmation token; does NOT kill
- `mcphub cleanup --aggressive --client codex-cli --confirm-aggressive-token <token>` → kills only the previewed candidates (token bound to that exact candidate snapshot; stale token → reject + re-run dry-run)
- Killing `cmd.exe`/`chrome.exe`/`conhost.exe`/`pwsh.exe`/`powershell.exe` requires explicit per-class `--include-class`
- Audit event `aggressive-cleanup-executed` with body `{client, root_pid, candidate_count, killed_count, skipped_classes, token_used}`

**Test contract**:
- `TestCleanupAggressive_RejectsWithoutScope`
- `TestCleanupAggressive_DryRunPrintsTokenAndSkipsKills`
- `TestCleanupAggressive_TokenMismatchRejects`
- `TestCleanupAggressive_DenyListExcludesDangerousClassesByDefault`
- `TestCleanupAggressive_IncludeClassFlagOverridesWithWarning`

### H.2: GUI Servers matrix "Aggressive sweep" — advanced modal (NOT sibling button)

**Scope**: extend existing GUI Dashboard cleanup path (`internal/gui/frontend/src/api.ts:379-387` for the safe path with `apply:true`) with a SEPARATE "Aggressive sweep" affordance behind an advanced modal.

**Why modal, not sibling button** (per codex v3+audit review IMPORTANT-7): the existing Dashboard cleanup button calls `apply:true` directly because its safety guard guarantees zero live-rooted kills. Aggressive mode WILL kill live-rooted processes that may disrupt the operator's interactive sessions — same affordance shape is operationally unsafe.

**Modal flow**:

1. "Advanced cleanup" link in Dashboard expand reveals
2. Scope picker: client dropdown (`codex-cli`, `claude-code`, ...) OR root PID input
3. "Preview candidates" button → calls `/api/cleanup/aggressive-preview` (new endpoint, GET) → modal table lists per-PID name, parent chain, match source
4. Class deny-list checkboxes (default-checked, explicit unticking required per dangerous class — `cmd.exe`/`chrome.exe`/`conhost.exe`/`pwsh.exe`)
5. Typed confirmation: "type EXACTLY 'KILL N LIVE-ROOTED PROCESSES' to confirm" where N = candidate count
6. Submit → `/api/cleanup/aggressive-execute` with token from step 3 → results table

**Acceptance criteria**:
- Modal cannot be submitted without typed confirmation matching candidate count exactly
- Token from step-3 preview must match step-6 submit; otherwise reject + force re-preview
- Dangerous-class checkboxes start checked (excluded); operator must explicitly opt-in to override
- Operator-visible warning text: "Live Codex sessions may be disrupted. This action is irreversible."

**Test contract**:
- E2E Playwright: open modal, preview, attempt submit without confirmation → reject; submit with correct confirmation → execute
- Unit: typed-confirmation regex match against candidate count

### H.3 (External / upstream follow-up — non-blocking for mcphub PR)

Upstream codex CLI feature request: per-subagent stdio MCP lifecycle integration. Two options the codex team could adopt:

- (a) Reap per-subagent stdio MCP children on subagent finish (lifecycle ownership)
- (b) Inherit a single parent stdio MCP set from the codex CLI parent (child-of-parent semantics)

Neither is in scope for the mcphub PR — they're explanations of the architectural ceiling that Phase H tooling exists to mitigate. If upstream codex adopts either, Phase H becomes optional.

---

## Out of scope (deferred)

### Handshake / dynamic-port (v2)

Per Decision 4 — daemon binds port 0, kernel assigns, daemon publishes via supervisor IPC, mcphub-router discovers dynamically. Eliminates port-collision failure class. Docks into G4 unified hub spec for v2 lift.

**Why not v1**: meaningful complexity (new IPC verb + discovery protocol) that benefits from v1 lessons. v1 uses persistent port assignment from pool.

### Multi-workspace symbol search (v2)

`find_referencing_symbols` where symbol is in workspace A but refs in workspace B → currently returns only workspace-A refs (out of scope). v2 may extend mcphub-router to query all workspaces + merge results.

### Unified client-facing port (v2 — G4 docking)

v1: clients use serena's existing per-daemon endpoint via router. v2: clients use ONE constant mcphub endpoint for everything (memory, serena, time, etc.) per G4 spec.

---

## Open questions (v2)

**Resolved in v2** (removed from list):
- ~~No-path-args fallback semantics~~ — RESOLVED by codex consult (Decision 5 + Phase F)
- ~~MCP session ID stability~~ — RESOLVED: codex confirmed `Mcp-Session-Id` is protocol-stable across TCP reconnect when client preserves header; use it as sticky anchor (NOT TCP connection ID)
- ~~Migration from operator's current state~~ — RESOLVED in D.3 with three-state detection predicate (legacy/unified/migrated)

**Still open**:

1. **`workspaces.yaml` (registry) hot-reload latency** — when operator adds workspace via `mcphub workspace register`, how long until reconciler picks up the change and spawns the new serena daemon? IntentWatcher polls every 30s by default. Acceptable: 30-60s. If operator wants instant, add explicit `mcphub workspace reload` command that bumps mtime + waits for next poll OR add a new IPC `intent-reload` verb (more scope)
2. **Auto-register `.serena/project.yml` defaults** — `read_only: false`, `excluded_dirs: [...]`, `language_detector_threshold`? Need a single source-of-truth defaults file in `internal/api/language_detection.go`
3. **Port allocation persistence on unregister** — keep port reserved for retention period (e.g., 24h, in case operator re-registers same path) or release immediately?
4. **Cross-workspace memory access** — `read_memory name` in session bound to workspace A, but operator wants to read memory from B → out-of-scope for v1, F.2 disambiguates by error; v2 may add explicit `hub.read_memory_in_workspace <name> <abs-path>`
5. **State-machine wiring (Phase A.2) prerequisites** — pinned to PR #229 merge + binary upgrade + serena crash root cause from new `daemon-exited` events (see Phase A.2 gating §)
6. **Port-pool boundary tuning** — default 9121-9199 = 79 slots. Realistic operator ceiling per Decision 1 = ~6 workspaces. Should default pool be narrower (e.g., 9121-9139 = 19 slots, more conservative)?
7. **`hub.bind_workspace` IPC verb scope** — Phase F.4 introduces this new verb. Should it be available BOTH via the GUI MCP-router endpoint AND via local mcphub IPC for CLI use (`mcphub session bind --session-id X --workspace Y`)? Latter adds CLI scope
8. **`get_current_config` aggregation shape** — F.1 says "hub-summary + per-daemon configs as separate fields". Need exact JSON schema documented in F.1's acceptance criteria before implementation (current text is hand-wave)
9. **Aggregate parallelism bound** — F.1 says "bounded N, default max 8 parallel". Is 8 the right number for typical operator's 3-6 workspace scenario? Tuning candidate
10. **Single-workspace-registry shortcut affects F.3** — if exactly one workspace registered, F.3 routes writes there directly (no fail-closed). Is that the right trade-off vs. always-require-explicit-bind? Codex consult mentioned exception but didn't dive into edge cases

---

## Review history

- **v1** (commit 5aa683b): initial architectural posture + phase breakdown. Sonnet review = REVISE (4 BLOCKERS + 5 IMPORTANT + 5 MINOR). Codex deep-source consultation on no-path-args returned with concrete Q5 strategy table.
- **v2** (commit 02abc55): all 4 sonnet v1 BLOCKERS resolved (B.1 Registry extension instead of parallel type; D.1 reuse `kind: workspace-scoped` + add `daemon_template` block; A.2/A.3 explicit cross-branch gating; D.3 IntentWatcher mtime instead of broken IPC `reload`). 5 IMPORTANT line-number / signature corrections applied to Phase A.1 catalog. Decision 5 closed with concrete codex-driven strategy table; Phase F changed from TBD placeholder to 4 concrete sub-phases (F.1-F.4) with full acceptance criteria and test contracts.
- **v3** (this commit): all 4 sonnet+codex v2 BLOCKERS resolved with concrete architecture:
  - **B.1 Registry identity**: `(WorkspaceKey, Language)` tuple preserved as primary key; serena entries use `Language: "@serena"` sentinel (invalid as LSP language → no collision possible). `AllocatedPorts()` automatically covers serena ports (no SerenaPort field needed). `Languages []string` added as optional field for the multi-language snapshot. 4 existing LSP-only call sites get one-line filter `Language != SerenaLanguageSentinel`. v2's false `SecureWriteClientConfig` claim corrected.
  - **D.1 validator branch**: explicit `if m.DaemonTemplate != nil` branch in `Validate()` skips per-language `port_pool` + `languages[]` checks; requires `daemon_template.port_pool` non-empty + `extra_args_template` references `${workspace.path}`. Mutual-exclusion with legacy `daemons[]` documented with explicit migration guidance.
  - **D.3 install chain**: migration tool invokes `api.BuildPlanWithOpts` + `executeInstallTo` IN-PROCESS (no shell-out) under Registry.Lock; intent regeneration is atomic relative to other registry mutations. IntentWatcher poll default corrected from claimed 30s to actual 60s.
  - **A.2 LoopEvent descriptor lookup**: production SM dispatch handler caches parsed intent + TaskName→Daemon index; refreshed on IntentWatcher.onChange. Handler looks up descriptor by TaskName when processing EvStart/EvChildExit. `LoopEvent` itself stays minimal — descriptor lookup is implementation detail. Concurrency via copy-on-write `atomic.Value` for the snapshot.

  v2 IMPORTANT addressed inline: supervise_reconcile.go spawn line `:117` → `:118` corrected; IntentWatcher default `30s` → `60s` corrected; stale "kind: workspace" in spec §72-94 to be patched in spec follow-up.
- **v4+ (TBD)**: convergence iterations until 0 BLOCKERS per established v1→v5 pattern from servers-matrix plan.

---

## Implementation sequencing notes

**Critical path**:
1. PR #229 (supervisor `daemon-exited` emit) — landed for diagnostic visibility
2. Operator: upgrade installed binary + restart supervisor (manual step)
3. Identify serena crash root cause from new `daemon-exited` events — outside this plan's code scope
4. **A.2 unblocked** (state machine wiring)
5. **Parallel**: B (workspace registry) + C (router) + D (daemon spawn) + E (auto-register) — independent, can fan out to multiple implementers
6. F (sticky-session details) after codex consult
7. D.3 migration script
8. G (legacy cleanup) — gates on operator confirming dynamic-pool stable on their machine

**Estimated effort** (rough, will tighten in v2):
- A.1: 2-4 hours (catalog audit)
- A.2: 8-16 hours (state machine wiring + tests)
- B: 8-12 hours total (registry + CLI + bootstrap)
- C: 12-20 hours total (resolver + router + sticky-session)
- D: 8-12 hours total (manifest schema + spawn + migration script)
- E: 4-8 hours total (survey + auto-register flow)
- F: 4-8 hours (post-consult)
- G: 2-4 hours (cleanup)

**Total v1 ballpark**: 50-90 hours of focused implementation, plus review cycles.

---

## Verification posture

This v1 plan is intentionally a DRAFT pending dual-review. Per the convergence pattern from servers-matrix plan (v1→v5 with 15+ BLOCKERS resolved across cycles):

- Sonnet review should focus on: scope coherence, phase ordering, dependency analysis, test contract completeness
- Codex review should focus on: API symbol verification (catalog accuracy), Go idioms, race conditions in state machine wiring + router, security concerns (workspace registry parent-dir DACL, port pool exhaustion)

v2 will integrate all findings inline + bump version + add convergence row to "Review history".
