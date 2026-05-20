# Unified Plan: Serena dynamic-pool + Supervisor state-machine wiring (v5)

> **Status**: v5 — section bodies propagated to match the v4 header summary. v4 header captured the resolution intent for 4 v3+v4 BLOCKERS but section-level prose (B.1, D.1, D.3, A.2) still carried v3 text; v5 rewrites each section against verified code surfaces at HEAD `6f22944` so reviewers can validate the resolutions against actual file:line evidence. PR #229 + PR #230 (daemon-exited emit + auto-respawn dispatcher) MERGED to master 2026-05-20 (commits 526bea9 + c840664). Pending v5 dual review.
>
> **Convergence history**:
>
> - v1 (commit 5aa683b): initial draft. Sonnet REVISE: 4 BLOCKERS + 5I + 5M.
> - v2 (commit 02abc55): v1 BLOCKERS resolved; codex no-path consult closed Decision 5. Sonnet v2 REVISE: 3 NEW BLOCKERS. Codex v2 REVISE: same + NEW B.4 LoopEvent.
> - v3 (commit 112099a): 4 v2 BLOCKERS resolved. Sonnet v3 REVISE: 4 NEW BLOCKERS (LSP call-sites under-counted, validator types wrong, `executeInstallTo` unexported, `Supervisor`/`smState` missing). Codex v3 REVISE: same 4 + 4 IMPORTANT (D.3 chain incomplete, IntentWatcher not wired, sentinel collision unprevented, path traversal hole). Operational evidence + Phase H added (1fad546+338ae82+2fd5f18) — parallel trajectory.
> - v4 (commit 6f22944): header bumped with resolution intent for all 4 v3 BLOCKERS + 4 IMPORTANTS, but section bodies still v3. Dual review returned REVISE with consensus that section-level prose must be rewritten before v5 can be reviewed against verified evidence. Both reviewers also surfaced specific corrections: (a) LSP call-site catalog was 4-undercounted at 13-actual sites (NOT 6, NOT 8 — see B.1 v5 table), (b) `@`-prefix is NOT currently rejected by the manifest validator, (c) the validator-level rejection alone does not defend the registry write path because `@serena` lives in `WorkspaceEntry.Language` (registry) not `LanguageSpec.Name` (manifest), (d) v3 pseudocode used `Manifest` (real type is `ServerManifest`) + `len(m.PortPool)` (real type is `*PortPool`), (e) v3's `executeInstallTo` reference is to an unexported function.
> - v5 (this commit): section bodies rewritten to match the v4 header intent + close the additional reviewer-surfaced corrections:
>   - **B.1** Registry: 13-site verified call-site catalog (4 LSP-only requiring sentinel filter; 9 backend-agnostic safe-include); dual-gate `@`-prefix defense at manifest validator (`config/manifest.go:347-365`) AND registry write path (`PutLSP` wrapper); backend/server ownership matrix; `mcphub unregister --backend {serena|all|<name>}` default-LSP-only semantics.
>   - **D.1** Validator: compile-accurate pseudocode against verified types (`ServerManifest` at manifest.go:48, `*PortPool` with `.Start`/`.End` at manifest.go:58+109-112, `Languages []LanguageSpec`); `DaemonTemplate.PortPool *PortPool` (NOT `[]int`) for consistency; `containsWorkspacePathTokenInArgs` as private helper with substring-match (composite args like `--project=${workspace.path}/sub` accepted); B.1 dual-gate `@`-prefix rejection lives in the same per-language loop.
>   - **D.3** Install chain: new exported `api.InstallParsedManifest(ctx, m, opts)` defined with full atomicity contract (pre-flight `WriteSupervisorIntent` dry-write gate + shared rollback stack across scheduler-tasks + per-client config + intent write); duplication-with-Install resolved via extracted `installPlan` helper; net code growth ~80 lines.
>   - **A.2** SupervisorController: lightweight struct reconciles with existing PR #230 `runRespawnDispatcher` (DELETED) + `DaemonRuntimeTracker` (REUSED as sole consumer); `atomic.Value` for IntentCache (justified vs RWMutex+CoW); `runRespawnDispatcher` body absorbed into `executeSideEffect`; existing 8 dispatcher tests refactored to drive controller entry point; IntentWatcher.Run wiring made concrete in runSupervise.
>   - **F.2** JSON-RPC `result`/`error` envelope classification (not HTTP status alone) with `classifyReadMemoryResponse` helper.
>   - **F.3** Single-workspace shortcut gated on `RestartPolicyState == StRunning` health check; 412 with `daemon_state` field on unhealthy.
>   - **F.4** Snapshot-then-release lock pattern documented; fan-out runs lock-free against value-copy slice; `hub.bind_workspace` moved from supervisor IPC to MCP/HTTP tool on hub-mcp endpoint.
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

| Concept | Real symbol | Location |
|---|---|---|
| State machine entry point | `api.Transition(state SMState, ev SMEvent, ctx SMContext) (newState SMState, side string, persistBefore bool, matched bool)` | `internal/api/supervisor_state_machine.go:47-164` |
| State machine states | `api.SMState` (StIdle/StSpawning/StRunning/StExiting/StBackoffWaiting/StQuarantined) | `internal/api/supervisor_state_machine.go:7-14` |
| State machine events | `api.SMEvent` (EvStart/EvHealthOK/EvChildExit/EvTimerDue/EvIntentUpdate/EvManualRestart/EvRequestGraceful/EvQuiesceComplete/EvSupervisorRestart) | `internal/api/supervisor_state_machine.go:16-28` |
| Event loop FIFO | `api.NewEventLoop` + `api.LoopEvent{Kind: EvX, TaskName: "..."}` + `loop.Post(...)` | `internal/api/supervisor_event_loop.go` (TBD-v3: full file read to fill in exact API surface — v2 implementer must verify before using) |
| Per-daemon SM state cache | `DaemonRuntimeTracker` (current) — separates runtime state from SM state; SM state currently NOT tracked in production (Decision 3) | `internal/cli/supervisor_runtime_tracker.go:41-180` |
| Reconciler spawn fan-out | `func (r *Reconciler) Reconcile(intent *api.SupervisorIntentFile, daemonIntent *api.DaemonIntentFile, currentRunning map[string]bool, now time.Time)` — calls `r.spawn(d)` directly at `:118` (NOT via Transition; bypass documented in Decision 3) | `internal/cli/supervise_reconcile.go:91-129`; spawn call at `:118` |
| Production spawn fn (v5 sig with overlay) | `func makeProductionSpawnFnWithStatePath(job *process.Job, events *api.SupervisorEventLog, tracker *DaemonRuntimeTracker, statePath string, overlay *daemon_env_overlay.Overlay) SpawnFunc` — IMPORTANT-3 sonnet fix: includes `overlay` 5th parameter from servers-matrix Phase 2 wiring | `internal/cli/supervise.go:1838`; production call site at `:628` |
| `cmd.Wait()` goroutine | currently silent `MarkExited` at `internal/cli/supervise.go:1916-1917` — IMPORTANT-2 sonnet fix; after PR #229 (`fix/supervisor-child-exit-emit` branch) emits `daemon-exited` event with pid + exit_code + wait_err before MarkExited | `internal/cli/supervise.go:1910-1920` block |
| `MarkSpawned` / `MarkExited` | `DaemonRuntimeTracker.MarkSpawned(taskName, pid, startedAt)` / `MarkExited(taskName)` — note: MarkExited does NOT decrement pid_generation; that field accumulates | `internal/cli/supervisor_runtime_tracker.go:41-90` |
| `supervisorStateFromRuntimeState` | maps runtime tracker state → persisted state field; missing case for `"spawning"` → falls into `default: "idle"` (root cause of state="idle" + pid_generation=35 silent crash loop) | `internal/cli/supervisor_runtime_tracker.go:243-254` |
| IPC `respawn` handler | `handleRespawn(conn, req, deps)` — exposed via per-task `respawn` IPC verb; `restart`/`reload` verbs return UNKNOWN_COMMAND (BLOCKER-4 context) | `internal/cli/supervise_respawn.go:96-237`; UNKNOWN_COMMAND at `internal/cli/supervise.go:1050-1062` |
| IntentWatcher mtime poll | `(*IntentWatcher).Run(ctx)` polls `supervisor-intent.json` + `daemon-intent.json` mtimes, fires `onChange` callback when either changes; default poll interval set by NewIntentWatcher caller | `internal/cli/supervise_watcher.go:136-170`; constructor at `:107` |

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
//   - DaemonRuntimeTracker (reused as-is from PR #230 — sliding-window quarantine)
//   - persistent SM state map keyed by TaskName, mirrored to supervisor-state.json
//
// Field count: 5. NOT a god-object — the heavy lifting (spawn, audit emit, IPC,
// reconcile, watcher) stays as free functions or existing types; controller is
// the orchestration glue.
type supervisorController struct {
    intentCache *IntentCache              // atomic.Value snapshot pointer; refreshed on IntentWatcher.onChange
    eventLoop   *api.EventLoop            // serialized dispatch; existing primitive
    tracker     *DaemonRuntimeTracker     // PR #230 sliding-window + backoff/quarantine state markers
    smStates    sync.Map                  // taskName → api.RestartPolicyState (the formal SM state)
    events      *api.SupervisorEventLog   // audit emitter
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
func (c *supervisorController) handleLoopEvent(e api.LoopEvent) {
    d, ok := c.intentCache.Lookup(e.TaskName)
    if !ok {
        return // daemon dropped from intent; audit-log orphan event if desired
    }
    current, _ := c.smStates.Load(e.TaskName)
    currentState, _ := current.(api.RestartPolicyState)
    smCtx := api.SMContext{
        TaskName:        e.TaskName,
        Now:             time.Now().UTC(),
        FailuresInWindow: c.tracker.CrashCountInWindow(e.TaskName, time.Now().UTC(), respawnFailureWindow),
    }
    newState, side, persistBefore, matched := api.Transition(currentState, e.Kind, smCtx)
    if !matched {
        return // log + drop; transition is a no-op for this state+kind pair
    }
    if persistBefore {
        c.smStates.Store(e.TaskName, newState)
        // Persist to supervisor-state.json via existing tracker.Persist seam.
        // Best-effort — audit-log on failure.
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
func runSupervise(ctx context.Context, noIPC bool, strictMode bool) error {
    // ... existing pre-controller setup (lock, intent load, event log, job creation) ...

    ctrl := &supervisorController{
        intentCache: newIntentCache(),
        eventLoop:   api.NewEventLoop(),
        tracker:     NewDaemonRuntimeTracker(),
        events:      events,
    }
    ctrl.intentCache.Refresh(initialIntent)
    ctrl.eventLoop.RegisterHandler(ctrl.handleLoopEvent)

    // IntentWatcher wired into runSupervise (the OTHER v4 BLOCKER A.2 closure):
    watcher := NewIntentWatcher(intentPath, daemonIntentPath, func() {
        // Re-read intent + refresh cache atomically. Errors are logged via events.
        if updated, err := api.LoadSupervisorIntent(intentPath); err == nil {
            ctrl.intentCache.Refresh(updated)
            // Post a reconcile-tick event so the controller picks up new descriptors.
            ctrl.eventLoop.Post(api.LoopEvent{Kind: api.EvReconcileTick})
        }
    })
    go watcher.Run(ctx) // <-- v3 said "wired"; v4 header confirmed; v5 makes it concrete

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

### A.3: Migration — upgrade installed binary + restart supervisor

**Scope**: operator-side migration documentation + smoke checklist after Phase A.2 lands.

**Steps**:
1. `mcphub install --upgrade` — replaces binary
2. Supervisor cold-restart (Task Scheduler / systemd / launchd will pick up new binary)
3. Verify state machine fields appear in `supervisor-state.json`
4. Verify serena daemons enter `running` state (if root cause from PR #229 is fixed) OR `backoff-waiting` then `quarantined` (if still crashing — then proceed to root-cause fix)

---

## Phase B: Workspace registry extension

### B.1: Extend existing `Registry` / `WorkspaceEntry` with `@serena` sentinel language tuple (v5 design)

**v3/v4 status**: BLOCKER (both sonnet + codex). Three independent defects converged across reviews:

1. **Call-site catalog drastically undercounted** (4 sites claimed, 13 sites actually iterate `reg.Workspaces`)
2. **False validator-rejection claim** ("@ prefix is invalid as an LSP-language name") — manifest validator at `internal/config/manifest.go:347-365` has NO `@`-prefix rejection rule; the sentinel CAN collide if an attacker or buggy manifest names a LanguageSpec `@anything`
3. **Sentinel lives in different struct than the proposed defense** — `@serena` rows write to `WorkspaceEntry.Language` (registry field), NOT `LanguageSpec.Name` (manifest field). Adding `@`-prefix rejection ONLY to manifest validator does not defend the registry write path

**v5 design**: keep `(WorkspaceKey, Language)` as the primary registry key. Serena entries use sentinel `Language: "@serena"` to distinguish from per-LSP-row tuples. Add a dual-gate defense — both the manifest validator AND the registry write path refuse `@`-prefix Language values unless they arrive via the explicit `PutSerena` entry point.

**Verified call-site catalog** (grep `range.*reg\.Workspaces|range.*workspaces|ListByWorkspace` against `internal/`, 2026-05-20 HEAD `6f22944`):

| # | File:line | Operation | @serena handling | Action |
| --- | --- | --- | --- | --- |
| 1 | `register.go:637` | `reg.ListByWorkspace(wsKey)` lookup of existing LSP rows during register | LSP-only — must filter sentinel | Add `if e.Language == SerenaLanguageSentinel continue` |
| 2 | `register.go:727` | Scan ClientEntries for entry-name collision during `ResolveEntryName` | Backend-agnostic — collision check on string name; serena rows have own naming (`serena-<short_key>`) | NO filter (safe-include) |
| 3 | `register.go:754` | Same collision-helper (`entryNameTakenByOtherWorkspace`) | Same | NO filter (safe-include) |
| 4 | `install.go:657` | Build `byTask` map for lifecycle/last-call enrichment | Backend-agnostic — TaskName is per-task unique | NO filter (safe-include) |
| 5 | `install.go:2124` | Build `byTask` map for status path | Same | NO filter (safe-include) |
| 6 | `install_intent.go:559` | Walk for `mcphub stop --daemon <lang>` task-name collection | LSP-only when `daemonFilter` != "" — for serena `daemonFilter` semantics need re-design | Backend-aware filter (see F.5 below) |
| 7 | `weekly_refresh.go:126` | Iterate to fire weekly-refresh schtasks /Run | Backend-agnostic via `WeeklyRefresh` flag — serena rows default to `WeeklyRefresh=false` and skip | NO filter (lifecycle gate suffices) |
| 8 | `status_enrich.go:69` | Build TaskName→entry map for overlay | Backend-agnostic | NO filter (safe-include) |
| 9 | `membership.go:51` | Build `[WorkspaceKey,Language]` index for weekly-refresh membership API | LSP-only — `@serena` rows MUST NOT appear as a "language" in membership UI | Add filter (LSP-only ownership) |
| 10 | `api_surfaces.go:430` | Build `WorkspaceTasksByKey` + `PortMap` for canonical status snapshot | Backend-agnostic — every workspace-scoped task belongs in the snapshot | NO filter (safe-include) |
| 11 | `legacy_migrate.go:206` | Match legacy task names against registry during migration | Backend-agnostic — task-name match works for both | NO filter |
| 12 | `gui/daemons.go:83` | Render membership table for weekly-refresh GUI panel | LSP-only — same logic as membership.go:51 | Add filter (LSP-only ownership) |
| 13 | `gui/workspaces.go:101` | List all workspaces (display table) | Backend-aware — display column "Backend" reads `e.Backend` field; serena rows show "serena" | NO filter (display surface, no semantic conflation) |

**Sites requiring filter `Language != SerenaLanguageSentinel`** (LSP-only consumers): `register.go:637`, `install_intent.go:559` (with backend-aware re-design per F.5), `membership.go:51`, `gui/daemons.go:83`. Four sites total — NOT the four sites that v3 named.

**Sites that are safe-include** (already backend-agnostic by TaskName-keyed iteration or lifecycle-flag gating): 8 production sites + 4 test sites (test sites are not behavioral; they exercise raw iteration semantics and a regression test guards membership-classification correctness — see test contract below).

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

// Internal-only helper (existing Put becomes unexported or restricted):
// Existing exported (r *Registry) Put(e WorkspaceEntry) callers in register.go switch to PutLSP.
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
func (m *ServerManifest) Validate() error {
    // ... existing global / transport-scope checks preserved (manifest.go:251-336) ...

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

   **Atomicity contract** (the v3 BLOCKER D.3 closure that v4 deferred):

   - **Pre-flight gate**: `WriteSupervisorIntent` is run first against a temporary path (alongside the existing `SecureWriteClientConfig` atomic-rename pattern in `supervisor_intent.go:134`). If the dry-write fails (disk full, permission denied, parent-dir DACL gate refusal), the function returns BEFORE any scheduler task is created — end-state = pristine.
   - **Rollback stack across side-effects**: after the pre-flight succeeds, the function follows the existing `executeInstallTo` pattern (`install.go:1634-1810`) — each scheduler task creation pushes a compensating `Delete()` onto the rollback stack; each per-client config write pushes a restore-from-backup. The final `WriteSupervisorIntent` (now writing to the real path with atomic rename) is the LAST mutating step. If it fails despite the pre-flight (TOCTOU window — disk filled between dry-write and real write), the stack runs in reverse and every scheduler task / client-config write created during this call is undone.
   - **No transient half-states observable to supervisor**: the supervisor's `IntentWatcher` polls `supervisor-intent.json` mtime. The atomic rename means the file either has the OLD content or the NEW content — never partial. If the rollback fires, the file is never renamed at all (rename happens last; rollback unwinds the OS-mutating steps that preceded it).
   - **Documented reconcile path** (defense in depth): even if a future bug introduces a window where scheduler tasks exist but intent has stale content, the reconciler's `buildPruneSetForReconcile` (`install.go:1839`) would not prune the unknown tasks unprompted — operators see them as drift via `mcphub status` and can re-run `mcphub install`.

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

**Test contract**:
- `TestMigrateSerena_DetectsLegacy2Daemon`
- `TestMigrateSerena_DetectsUnifiedIntermediate`
- `TestMigrateSerena_DetectsAlreadyMigrated_NoOp`
- `TestMigrateSerena_RejectsMalformedManifest`
- `TestMigrateSerena_RejectsEmptyWorkspaceRegistry`
- `TestMigrateSerena_AllocatesPortsForEachWorkspace`
- `TestMigrateSerena_WritesAuditEvent`

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
// Order: HTTP-status gate → JSON-RPC envelope gate → result-shape gate.
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
// The health predicate consults the controller's smStates (A.2) for the daemon's
// current RestartPolicyState. Healthy = StRunning. Unhealthy states (StBackoffWaiting,
// StQuarantined, StSpawning, StIdle) all return false → shortcut declined, fall through
// to the 412 rejection so the operator sees a clear "your serena daemon for D:\dev\Foo
// is in quarantine — fix that before writing" diagnostic instead of an opaque connection
// timeout.
func (r *SerenaRouter) shouldUseSingleWorkspaceShortcut() (*WorkspaceEntry, bool) {
    serena := r.registry.SerenaEntries()
    if len(serena) != 1 {
        return nil, false
    }
    ws := &serena[0]
    state, ok := r.controller.GetSMState(ws.TaskName)
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

**`hub.bind_workspace` belongs on MCP/HTTP layer, not supervisor IPC** (closes v4 BLOCKER F.4 codex finding "hub.bind_workspace as supervisor IPC questionable"):

Supervisor IPC (`supervise.go`'s named-pipe / unix-socket) is for process-lifecycle commands (status, restart, exit, quiesce-timers) — operator-facing, single owner, no session concept. Session binding lives in the hub-mcp HTTP layer where every request already carries `Mcp-Session-Id` and goes through the SerenaRouter. v5 places `hub.bind_workspace` as an MCP tool exposed by the hub itself:

```jsonc
// MCP tool exposed by mcphub's own hub-mcp server (NOT a supervisor IPC command).
{
  "name": "hub.bind_workspace",
  "description": "Bind this MCP session to a specific registered serena workspace for sticky no-path routing.",
  "inputSchema": {
    "type": "object",
    "properties": {
      "workspace_path": {"type": "string", "description": "absolute path of registered serena workspace"},
      "force": {"type": "boolean", "default": false, "description": "rebind even if already bound to a different workspace"}
    },
    "required": ["workspace_path"]
  }
}
```

The handler reads `Mcp-Session-Id` from the request, resolves the workspace by path against the registry, and updates the StickyMap under the write lock. Errors return JSON-RPC errors at the MCP layer (no special transport). Tracing: emits the same `serena-session-bound` / `serena-session-rebound` audit events as the implicit bind path.

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
