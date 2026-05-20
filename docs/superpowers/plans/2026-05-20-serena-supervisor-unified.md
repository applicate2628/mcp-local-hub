# Unified Plan: Serena dynamic-pool + Supervisor state-machine wiring (v3)

> **Status**: v3 — addresses sonnet v2 REVISE (3 NEW BLOCKERS + 1 partial + 5I + 5M) and codex v2 REVISE (4 BLOCKERS + 6I + 3M). Sonnet and codex converged on D.1 validator + D.3 install chain + B.1 Registry; codex added B.4 LoopEvent design. v3 resolves all 4 converged BLOCKERS with concrete architecture. Pending v3 dual review.
>
> **Convergence history**:
>
> - v1 (commit 5aa683b): initial draft. Sonnet REVISE: 4 BLOCKERS + 5I + 5M.
> - v2 (commit 02abc55): v1 BLOCKERS resolved; codex no-path consult closed Decision 5. Sonnet v2 REVISE: 3 NEW BLOCKERS (validator collision, false SecureWriteClientConfig claim, manifest→intent→watcher chain broken). Codex v2 REVISE: same + NEW B.4 (LoopEvent missing descriptor for A.2 dispatch).
> - v3 (this commit): all 4 converged BLOCKERS resolved — Registry `@serena` sentinel language tuple (B.1), validator branch on `daemon_template` presence (D.1), explicit `mcphub install` migration step regenerating supervisor-intent.json from manifest × workspaces.yaml (D.3), event-loop handler descriptor lookup by TaskName via cached intent (A.2). MINORs addressed inline.
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
- 13 codex stdio MCP entries remain UN-migratable per `mcphub scan` (no top-level manifest exists for them): `clangd`, `javascript`, `python`, `rust`, `fortran`, `time-server`, `vscode-css`, `go`, `typescript`, `vscode-html`, `stgen-dxf-viewer`, `raindrop`, `fetch`
- `mcphub cleanup --scan-clients` reports **0 orphans** — correctly excluding `child of live codex` per the safety guard at `internal/cli/overlay_prune_orphans.go`
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

These three add **Phase G** to the unified plan (deferred but in-scope for v4 review): operational hygiene tooling that complements the hub-routing config changes in Phases B-E.

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

### A.2: Wire state machine into production reconcile + spawn paths (v3 design)

**v2 BLOCKER (codex)**: v2 said "replace direct spawn with eventLoop.Post()" but `LoopEvent` (`supervisor_event_loop.go:6-10`) carries only `Kind`, `TaskName`, `Body map[string]any` — no `SupervisorDaemon` descriptor needed for spawn. Posting `EvStart{TaskName: "..."}` loses the descriptor (command, args, env, port, workspace) that `spawn()` needs to call exec.

**v3 design — descriptor lookup pattern**: production handler caches the parsed `SupervisorIntentFile` (loaded once per reconcile cycle, refreshed on `IntentWatcher.onChange`), and the SM dispatch handler looks up the descriptor by `TaskName` when processing `EvStart` / `EvChildExit` events. `LoopEvent` stays minimal; the descriptor cache is an implementation detail of the handler.

```go
// New production handler — registered via EventLoop.RegisterHandler in supervise.go.
//
// IntentCache holds the parsed intent + a TaskName→Daemon index, refreshed
// when IntentWatcher.onChange fires. Concurrency: copy-on-write via atomic.Value
// so the handler always reads a coherent snapshot.
type IntentCache struct {
    snap atomic.Value // *intentSnapshot
}

type intentSnapshot struct {
    intent       *api.SupervisorIntentFile
    daemonByTask map[string]*api.SupervisorDaemon
}

func (c *IntentCache) Lookup(taskName string) (*api.SupervisorDaemon, bool) {
    s := c.snap.Load().(*intentSnapshot)
    d, ok := s.daemonByTask[taskName]
    return d, ok
}

// Production SM dispatch handler. Called from EventLoop.RegisterHandler.
// Owns: state machine transition + side-effect execution (spawn, arm timer, persist).
func (s *Supervisor) handleLoopEvent(e api.LoopEvent) {
    d, ok := s.intentCache.Lookup(e.TaskName)
    if !ok {
        // Daemon dropped from intent; ignore. Audit-log the orphan event for diagnosis.
        return
    }
    currentState := s.smState.Get(e.TaskName)
    ctx := s.smContextFor(e.TaskName, d)
    newState, side, persistBefore, matched := api.Transition(currentState, e.Kind, ctx)
    if !matched {
        return // log + drop
    }
    if persistBefore {
        s.smState.Set(e.TaskName, newState)
        s.persistState()
    }
    s.executeSideEffect(side, d) // spawn / arm-backoff-timer / terminate / etc.
}
```

**Replacement points**:

- `supervise_reconcile.go:118` direct `r.spawn(d)` → `eventLoop.Post(LoopEvent{Kind: api.EvStart, TaskName: d.TaskName})`. Descriptor `d` is reachable via cache; not duplicated in the event.
- `supervise.go:1916-1917` `cmd.Wait()` goroutine: after the daemon-exited emit (now in PR #229 — landed pre-A.2), also `eventLoop.Post(LoopEvent{Kind: api.EvChildExit, TaskName: taskName})`. Handler then triggers backoff / quarantine / respawn state transitions.

**Gating** (unchanged from v2): A.2 starts only after PR #229 + its master-merge + binary upgrade + serena root-cause identification.

**Gating** (BLOCKER-3 fix from sonnet v1 review): the `daemon-exited` event does NOT currently exist on `feat/v0.5.x-servers-matrix-revamp` branch — it lives in PR #229 (`fix/supervisor-child-exit-emit` branch, HEAD `e9adb88` as of 2026-05-20T05:50Z). Explicit prerequisites for Phase A.2 work to begin:

1. **PR #229 merged to master** (currently in bot-review loop, HEAD `e9adb88` after revert of 3 P2 findings — see CLAUDE.md PR workflow Step 4-7)
2. **Master rebased into `feat/v0.5.x-servers-matrix-revamp`** so this branch carries the `daemon-exited` emit
3. **Operator runs `mcphub install --upgrade`** + cold-restart supervisor (binary on disk must be the post-#229 build)
4. **`daemon-exited` events visible** in operator's `supervisor-events.log` for serena daemons
5. **Serena crash root cause identified** from the new event's `exit_code` + `wait_err` fields

Implementation of A.2 cannot proceed until prerequisites 1-5 are satisfied. Phases B + C + D + E + F (workspace registry + routing + spawn + auto-register + sticky-session) are NOT blocked on A.2 and can fan out in parallel.

**Acceptance criteria**:
- Reconciler no longer calls `r.spawn` directly; all spawn intent goes through event loop
- `cmd.Wait()` exit posts `EvChildExit` (in addition to emitting `daemon-exited` from PR #229)
- State machine drives state transitions visible in `supervisor-state.json`: `idle` → `spawning` → `running` → `backoff-waiting` → `spawning` → `quarantined` per spec
- Restart-policy state (failures in 30-min sliding window, backoff_until, quarantine_since) fields appear in serialized state
- Manual smoke: kill serena daemon → supervisor respawns within backoff window; kill 10 times → quarantine kicks in

**Test contract**:
- `TestSupervisor_StateMachine_RestartOnExit` — spawn daemon, kill it, verify state machine fires EvChildExit → backoff timer → respawn within bounded delay
- `TestSupervisor_StateMachine_QuarantineAfterN` — repeatedly kill daemon 10 times, verify state transitions to `quarantined` and stops respawning
- `TestSupervisor_StateMachine_PersistedStateMatches` — verify `supervisor-state.json` field schema matches spec (including `failures`, `backoff_until`, `quarantine_since`)

### A.3: Migration — upgrade installed binary + restart supervisor

**Scope**: operator-side migration documentation + smoke checklist after Phase A.2 lands.

**Steps**:
1. `mcphub install --upgrade` — replaces binary
2. Supervisor cold-restart (Task Scheduler / systemd / launchd will pick up new binary)
3. Verify state machine fields appear in `supervisor-state.json`
4. Verify serena daemons enter `running` state (if root cause from PR #229 is fixed) OR `backoff-waiting` then `quarantined` (if still crashing — then proceed to root-cause fix)

---

## Phase B: Workspace registry extension

### B.1: Extend existing `Registry` / `WorkspaceEntry` with `@serena` sentinel language tuple (v3 design)

**v2 BLOCKER (sonnet + codex)**: v2 proposed adding `Languages []string` + `Default bool` + `SerenaPort int` as parallel fields, but the existing primary key `(WorkspaceKey, Language)` (`workspace_registry.go:31, 180-198`) is the load-bearing identity tuple used by `Put`/`Get`/`Remove`/`ListByWorkspace`. Serena entries need single-row-per-workspace (not per-language), so the schema needed an explicit decision on identity. v2 also falsely claimed `Save()` uses `SecureWriteClientConfig`; actual implementation is plain `os.WriteFile` + atomic rename (`workspace_registry.go:129-163`).

**v3 design**: keep `(WorkspaceKey, Language)` as the primary key. Serena entries use sentinel `Language: "@serena"` to distinguish from per-LSP-row tuples. This preserves all existing `Put`/`Get`/`Remove`/`AllocatedPorts`/`ListByWorkspace` semantics without code changes to existing call sites; per-LSP-language rows and serena-pool rows coexist as different tuples within the same workspace_key. The `@` prefix is invalid as an LSP-language name (LSP language IDs are alphanumeric per the spec), so the sentinel cannot collide with a real language.

**Scope**: extend the EXISTING `internal/api/workspace_registry.go` `WorkspaceEntry` struct with new fields needed by serena dynamic-pool. All new fields use `omitempty` yaml tag for backward compat with installed clients that have older entries.

```go
// Additions to existing WorkspaceEntry struct (workspace_registry.go:31):
type WorkspaceEntry struct {
    // ... existing fields preserved ...
    WorkspaceKey  string            `yaml:"workspace_key"`
    WorkspacePath string            `yaml:"workspace_path"`
    Language      string            `yaml:"language"` // "@serena" sentinel for dynamic-pool rows
    Backend       string            `yaml:"backend"`  // existing: "mcp-language-server"|"gopls-mcp"; new: "serena"
    Port          int               `yaml:"port"`     // SAME field — serena port also goes here; AllocatedPorts already covers
    TaskName      string            `yaml:"task_name"`
    ClientEntries map[string]string `yaml:"client_entries"`
    WeeklyRefresh bool              `yaml:"weekly_refresh"`

    // NEW fields for serena dynamic-pool (only meaningful when Language == "@serena"):
    RegisteredAt  time.Time `yaml:"registered_at,omitempty"`   // when first added
    RegisteredVia string    `yaml:"registered_via,omitempty"`  // "manual" | "auto-detect" | "migration"
    Languages     []string  `yaml:"languages,omitempty"`       // snapshot of .serena/project.yml at register time; distinct from existing Language single-string field
}
```

**Why `@serena` sentinel** (codex v2 BLOCKER-1 resolution):

- preserves `(WorkspaceKey, Language)` primary-key uniqueness without adding a new identity tuple field
- existing `AllocatedPorts()` (`workspace_registry.go:213-221`) already iterates ALL entries and includes their `Port` — serena ports are picked up automatically
- existing `ListByWorkspace(workspaceKey)` (`workspace_registry.go:224-232`) returns ALL entries including the `@serena` row; callers that need only LSP rows filter on `Language != "@serena"` (one-line change in each LSP-only call site, listed in implementer's catalog)
- existing `Remove(workspaceKey, language)` takes language verbatim — `Remove(ws, "@serena")` removes only the serena row; LSP rows untouched
- migration script (Phase D.3) handles per-workspace conversion: for each registered workspace, ensure exactly one `(WorkspaceKey, "@serena")` row exists with port allocated from `daemon_template.port_pool`

**Save pipeline correction** (sonnet v2 NEW B.2): the existing `(*Registry).Save()` uses plain `os.WriteFile` + atomic rename (`workspace_registry.go:129-163`), NOT `SecureWriteClientConfig`. v3 does NOT change that — the registry is not a client-config file and lives entirely in the operator's local `%LOCALAPPDATA%`-scoped state dir with 0600 file mode. Hardening parity with hub-mcp state files is OUT OF SCOPE for v1 dynamic-pool; if needed, a follow-up can route Registry writes through `SecureWriteClientConfig` as a separate PR.

**New API on existing `Registry`** (added to workspace_registry.go, not a new file):

```go
// All operate on the existing Registry singleton. Convention: any
// helper named *Serena* operates only on entries with Language == "@serena".
const SerenaLanguageSentinel = "@serena"

func (r *Registry) SerenaEntries() []WorkspaceEntry  // filter Language == SerenaLanguageSentinel
func (r *Registry) GetSerena(workspaceKey string) (WorkspaceEntry, bool)  // == r.Get(workspaceKey, SerenaLanguageSentinel)
func (r *Registry) PutSerena(e WorkspaceEntry)  // requires e.Language == SerenaLanguageSentinel; calls r.Put(e)
func (r *Registry) RemoveSerena(workspaceKey string)  // == r.Remove(workspaceKey, SerenaLanguageSentinel)
func (r *Registry) AllocateSerenaPort(pool []int) (int, error)  // first free port from pool not in AllocatedPorts(), persisted via Save()
```

**LSP-only call-site update** (the one breaking-change ripple to existing code): callers that currently iterate `Registry.Workspaces` assuming every entry is per-LSP-language must filter `Language != SerenaLanguageSentinel`. Grep-verified call sites: `internal/api/register.go:243` (registration loop), `internal/api/register.go:285` (lookup), `internal/api/install.go:656` (port-collision check), `internal/api/port_alloc_test.go` (test). Each gets a one-line filter; documented in v3 implementer's catalog with exact patch hint.

**Acceptance criteria**:

- Existing `Registry.Load()` / `Save()` round-trip preserves new optional fields (yaml.v3 omitempty pattern, no strict-parse on registry)
- `Language == SerenaLanguageSentinel` rows have non-empty `Languages` slice + non-zero `Port` (validated at register time, NOT in `Save`)
- Existing LSP-language rows (e.g. `Language: "go"`) are unchanged in behavior
- All 4 existing LSP-only call sites filter `Language != SerenaLanguageSentinel` to avoid double-counting serena rows as LSP rows
- AllocatedPorts() automatically includes serena ports (no code change required)

**Test contract**:

- `TestRegistry_SerenaSentinel_RoundTripsNewFields` — Load/Save round-trip preserves Languages + RegisteredAt + RegisteredVia
- `TestRegistry_SerenaSentinel_CoexistsWithLSPRows` — same workspace_key with both "@serena" and "go"/"typescript" entries
- `TestRegistry_AllocateSerenaPort_FirstFreeFromPool`
- `TestRegistry_AllocateSerenaPort_ExhaustionReturnsError`
- `TestRegistry_LegacyEntryReadAccepted` — older entry without Languages field loads cleanly
- `TestRegistry_LSPOnlyCallSites_FilterSerena` — regression guard that the 4 LSP-only call sites do filter the sentinel

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

### D.1: Manifest schema extension — `workspace-scoped` + `daemon_template` validator branch (v3)

**v2 BLOCKER (sonnet + codex)**: v2 reused `kind: workspace-scoped` to avoid the `kind: workspace` collision, but v2 did NOT specify how the validator at `manifest.go:337-365` (which currently requires `port_pool` + `languages[]` for ALL `workspace-scoped` manifests) splits between (a) legacy per-language LSP manifests (mcp-language-server / gopls-mcp) and (b) new dynamic-pool serena manifests with `daemon_template` and no per-language LSP backends. Without an explicit branch, a daemon-template-only serena manifest fails `Validate()` immediately at line 344.

**v3 design**: validator gets explicit branch on `DaemonTemplate != nil`:

```go
// Existing manifest.go:337-365 — extend with a daemon-template branch.
func (m *Manifest) Validate() error {
    // ... existing global / non-workspace-scoped paths unchanged ...
    if m.Kind == KindWorkspaceScoped {
        if m.DaemonTemplate != nil {
            // Dynamic-pool branch: no per-language LSP rows.
            if len(m.PortPool) > 0 || len(m.Languages) > 0 {
                return fmt.Errorf("kind=workspace-scoped with daemon_template must NOT set top-level port_pool or languages[] (move them into daemon_template)")
            }
            if len(m.Daemons) > 0 {
                return fmt.Errorf("kind=workspace-scoped with daemon_template is mutually exclusive with daemons[]")
            }
            if len(m.DaemonTemplate.PortPool) == 0 {
                return fmt.Errorf("daemon_template.port_pool must be non-empty")
            }
            if !containsWorkspacePathToken(m.DaemonTemplate.ExtraArgsTemplate) {
                return fmt.Errorf("daemon_template.extra_args_template must reference ${workspace.path}")
            }
            // No languages[] check — serena daemon is multi-language per .serena/project.yml
            return nil
        }
        // Legacy LSP-bridge branch (existing behavior, unchanged):
        if len(m.PortPool) == 0 {
            return fmt.Errorf("port_pool[] must be non-empty for kind=workspace-scoped")
        }
        if len(m.Languages) == 0 {
            return fmt.Errorf("languages[] must be non-empty for kind=workspace-scoped")
        }
        return nil
    }
    // ...
}
```

**Acceptance criteria**:

- `daemon_template`-only manifest validates successfully (no `languages[]` / `port_pool` at top level required)
- LSP-language manifest with neither `daemon_template` nor empty `languages[]` continues to validate as before (regression guard)
- Both-present is rejected with explicit migration guidance message
- `daemon_template.extra_args_template` MUST contain `${workspace.path}` token (else workspace context is lost on spawn)

**Test contract**:

- `TestManifest_WorkspaceScopedWithDaemonTemplate_Valid`
- `TestManifest_WorkspaceScopedWithDaemonTemplate_RejectsLegacyPortPool` — both `port_pool` (top-level) AND `daemon_template` → reject
- `TestManifest_WorkspaceScopedWithDaemonTemplate_RejectsMissingWorkspacePathToken`
- `TestManifest_WorkspaceScopedLegacyLSP_StillValidates` — regression guard, no change in behavior
- `TestManifest_WorkspaceScoped_RejectsDaemonsListAndTemplateBoth`

**Decision** (rejected: new third kind; accepted: extend existing `workspace-scoped`): serena's dynamic-pool falls under the existing `workspace-scoped` semantic — one daemon per workspace. The change is to add a new OPTIONAL `daemon_template` block alongside the existing `daemons:` list. When `daemon_template` is present (regardless of `kind`), reconciler generates one descriptor per registered serena workspace from the template; when only legacy `daemons:` is present, current per-daemon behavior is preserved.

**Manifest example** (post-D.1):

```yaml
name: serena
kind: workspace-scoped        # existing kind value; no new constant needed
transport: native-http
command: uvx
base_args: [...]
env: {PYTHONUNBUFFERED: "1"}
daemon_template:              # NEW optional block
  context: codex
  port_pool: [9121, 9122, ..., 9199]
  extra_args_template:
    - --context
    - codex
    - --project
    - "${workspace.path}"
# Legacy `daemons:` block is INCOMPATIBLE with `daemon_template:` — schema
# validator rejects both-present (one or the other, not both). This forces
# explicit migration to dynamic-pool.
```

**New Go struct** (added to `internal/config/manifest.go` alongside existing `Daemon` struct):

```go
type DaemonTemplate struct {
    Context           string   `yaml:"context"`
    PortPool          []int    `yaml:"port_pool"`
    ExtraArgsTemplate []string `yaml:"extra_args_template"`
}

type Manifest struct {
    // ... existing fields ...
    Daemons        []Daemon        `yaml:"daemons,omitempty"`         // legacy per-daemon list
    DaemonTemplate *DaemonTemplate `yaml:"daemon_template,omitempty"` // NEW dynamic-pool template; mutually exclusive with Daemons
}
```

**Acceptance criteria**:
- `dec.KnownFields(true)` strict parse remains intact (every new YAML key has yaml tag with omitempty)
- Validator: at most one of `daemons` OR `daemon_template` present (both-present → reject with explicit "dynamic-pool migration requires removing legacy daemons[]")
- `${workspace.path}` token expanded at spawn time per registered serena workspace
- `port_pool` non-empty, all entries in valid TCP-port range, no duplicates
- `extra_args_template` non-empty AND contains `${workspace.path}` token somewhere (else workspace info is lost on spawn)

**Test contract**:
- `TestManifest_DaemonTemplate_Parsing` — yaml round-trip preserves template
- `TestManifest_DaemonTemplate_RejectsBothPresent` — manifest with both daemons and daemon_template fails strict-parse
- `TestManifest_DaemonTemplate_RejectsEmptyPortPool`
- `TestManifest_DaemonTemplate_RejectsTemplateWithoutWorkspacePath`
- `TestManifest_DaemonTemplate_TokenExpansionAtSpawn` — `${workspace.path}` substituted with concrete absolute path

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
7. **Reload trigger** (v3 fix of sonnet v2 NEW B.3 + codex v2 B.3 — converged finding): the existing IPC `restart`/`reload` cases return `UNKNOWN_COMMAND` (`supervise.go:1050-1062`) and `IntentWatcher` polls only `supervisor-intent.json` and `daemon-intent.json` (`supervise_watcher.go:53-60`), NOT `manifest.yaml` or `workspaces.yaml`. The "manifest write → mcphub install → intent regenerated → watcher fires" chain has a missing link: `mcphub install` is NOT auto-triggered when `manifest.yaml` changes. The migration tool MUST explicitly invoke install as an in-process step.

   **v3 migration sequence**:
   1. acquire `Registry.Lock()` for cross-process safety (`workspace_registry.go:169-178`)
   2. write new `servers/serena/manifest.yaml` with `daemon_template` block (atomic via existing `SecureWriteClientConfig` pipeline)
   3. invoke `api.BuildPlanWithOpts(...)` + `api.executeInstallTo(...)` IN-PROCESS (no separate `mcphub install` shell-out) — these are the existing install primitives that regenerate `supervisor-intent.json` from manifest × workspaces.yaml (`internal/api/install.go:1109-1205, 1630-1810`; v3 implementer must verify exact entry-point signatures)
   4. write atomic `supervisor-intent.json` via existing install pipeline; this bumps the file's mtime
   5. `IntentWatcher.detectChange()` (`supervise_watcher.go:193-200`) detects mtime change on next poll tick and fires `onChange` → reconciler picks up new descriptors
   6. release Registry lock

   **IntentWatcher default poll** (sonnet v2 MINOR-2 fix): `NewIntentWatcher` defaults `pollInterval` to `60 * time.Second` when `pollInterval <= 0` (`supervise_watcher.go:108-110`), NOT 30s. **Operator-facing behavior**: migration prints "supervisor will pick up new intent within 60s (next IntentWatcher tick); no manual restart required."

   **Why in-process install vs shell-out**: shell-out has multiple failure modes (operator's PATH, mcphub binary version mismatch, intent file lock races against another mcphub process). In-process call uses the SAME Go function the install command does, with the Registry lock held, so the supervisor-intent.json write is atomic relative to other registry mutations.

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

### F.2: `read_memory <name>` — strict disambiguation when unbound

**Bound session**: sticky-forward to the workspace's serena daemon. Pass response through unchanged.

**Unbound session**:
- Query all registered serena daemons in parallel
- Collect responses; count how many returned a successful read (HTTP 200 + non-empty body)
- Cases:
  - Exactly 1 success: return that workspace's response unchanged + `X-Serena-Workspace: <abs-path>` response header (so client can sticky-bind explicitly going forward)
  - 0 successes: HTTP 404 with body `{"error": "memory '<name>' not found in any registered serena workspace"}`
  - 2+ successes: HTTP 409 Conflict with body `{"error": "memory '<name>' exists in multiple workspaces", "workspaces": ["D:\\dev\\PaperPane", "D:\\dev\\mcp-local-hub"], "guidance": "call a path-aware tool first to bind workspace, or use hub.bind_workspace explicitly"}`
- Codex constraint: do NOT use "first success wins" — that silently leaks the wrong workspace's memory contents

**Special case**: memory name starting with `global/` (per serena convention) — can be de-duped/read-once across the pool because global memories are by-name unique. Acceptance criterion: documented behavior for `global/*` is "read first daemon's response since global memories are by-name unique by serena convention". Defer cross-pool global memory sync to v2.

**Test contract**:
- `TestSerenaRouter_ReadMemoryUnbound_ExactlyOneMatch_Returns200`
- `TestSerenaRouter_ReadMemoryUnbound_ZeroMatches_Returns404`
- `TestSerenaRouter_ReadMemoryUnbound_MultipleMatches_Returns409Disambiguation`
- `TestSerenaRouter_ReadMemoryUnbound_GlobalNamespace_FirstDaemonWins`

### F.3: `write_memory` / `delete_memory` / `onboarding` — fail-closed unbound

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
- Exception: single-workspace-registry shortcut (if exactly one registered serena workspace, route there directly) — only safe path for "default behavior"

**Acceptance criteria**:
- Unbound write → HTTP 412 + explicit guidance message (no silent default)
- Single-workspace-registry shortcut works for both bound and unbound
- Each rejection emits audit event `serena-write-unbound-rejected` with body `{tool, session_id_hash, registered_workspace_count}`

**Test contract**:
- `TestSerenaRouter_WriteMemoryUnbound_Returns412`
- `TestSerenaRouter_DeleteMemoryUnbound_Returns412`
- `TestSerenaRouter_OnboardingUnbound_Returns412`
- `TestSerenaRouter_WriteMemorySingleWorkspaceShortcut_Returns200`
- `TestSerenaRouter_WriteMemoryUnboundEmitsAuditEvent`

### F.4: Sticky-session map implementation

**Storage**: in-process map `map[string]*WorkspaceEntry` keyed by `Mcp-Session-Id` header value. Protected by `sync.RWMutex`. Lazy expiration: TTL 24h since last call (configurable via `mcphub config sticky-ttl`).

**Hook points**:
- On every path-aware tool-call response success → `sticky[session_id] = resolved_workspace` (idempotent if already bound to same workspace)
- On HTTP 404 from upstream (session expired per MCP spec) → evict `sticky[session_id]`
- On explicit MCP DELETE on `Mcp-Session-Id` (per MCP spec §"Session Management") → evict
- On `hub.bind_workspace <abs-path>` (new IPC verb) → set `sticky[session_id]` explicitly; refuses if session already bound to different workspace unless `--force`

**Acceptance criteria**:
- Sticky binding correctly resolves on subsequent no-path calls
- Map lookup is O(1)
- Eviction on 404 from upstream + explicit DELETE + TTL expiry
- `hub.bind_workspace` IPC verb works idempotent (re-bind to same workspace = no-op)
- Audit event `serena-session-bound` on first bind, `serena-session-rebound` on explicit override, `serena-session-evicted` on eviction

**Test contract**:
- `TestStickySession_BindOnFirstPathCall`
- `TestStickySession_LookupAfterBind_O1`
- `TestStickySession_Evict_OnHTTP404FromUpstream`
- `TestStickySession_Evict_OnExplicitDELETE`
- `TestStickySession_HubBindWorkspace_Idempotent`
- `TestStickySession_HubBindWorkspace_RejectsRebindWithoutForce`

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
