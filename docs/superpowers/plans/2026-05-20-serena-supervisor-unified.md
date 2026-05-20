# Unified Plan: Serena dynamic-pool + Supervisor state-machine wiring (v1 draft)

> **Status**: v1 DRAFT — pending dual review loop (sonnet + codex), per the established convergence pattern that brought servers-matrix plan from v1 (15+ BLOCKERS) → v5 (0 BLOCKERS sonnet APPROVE + codex APPROVE_WITH_CHANGES). This v1 captures the architectural posture and phase breakdown; v2+ will incorporate dual-review findings and tighten symbol catalog + acceptance criteria.
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

### Decision 3: Supervisor state-machine wired into production

**Current bug** (diagnosed via codex deep-diagnostic 2026-05-20, file: `.scratch/codex-prompts/supervisor-serena-bug-20260520-044800.out`): production `supervise_reconcile.go:117` calls `r.spawn(d)` directly without posting `EvStart` to the state machine. `cmd.Wait()` goroutine in `supervise.go:1539-1543` calls `MarkExited` + persist without posting `EvChildExit`. Result: state machine's backoff / quarantine logic is **dead code** in production. PR #229 adds the diagnostic emit but does NOT wire the state machine — that wiring is **Phase A.2 of this plan**.

**Constraint**: Phase A.2 depends on PR #229's `daemon-exited` event being present in production AND the serena crash root cause being known (so we can validate backoff actually fires on that exit). Until both gates are satisfied, A.2 implementation is paused and gated on diagnostic data.

### Decision 4: Handshake-port deferred to v2

**Current**: workspaces.yaml records `serena_port: 9121-9199` from a fixed pool with persistent assignment.

**v2 future**: serena binds `port: 0` → kernel-assigned → publishes via supervisor IPC → mcphub-router discovers dynamically. Eliminates port-collision (orphan-on-fixed-port) class of failures. Docked into [G4 unified hub spec](../specs/2026-05-12-g4-unified-hub-mcp-design-v3.md) for v2 lift.

**Why deferred**: v1 must converge with the existing supervisor + workspace-registry primitives. Handshake adds a new IPC verb + discovery handshake protocol — meaningful complexity that benefits from v1 lessons. Not blocking dynamic-pool v1.

### Decision 5: No-path-args sticky-session — under codex consultation

**Open question**: what's the default routing for a no-path tool-call BEFORE any path-call in the session? Candidates from spec §4 Mode 2:
- **(A) Sticky-session per MCP session ID** — works once first path-call binds workspace
- **(B) Default workspace from workspaces.yaml** — exactly one workspace marked `default: true`
- **(C) Reject — require client to call path-tool first**
- **(D) Aggregate from all daemons** (read-only queries only: `list_memories`, etc.)

Combined design is likely a mix: read-ops → D for unbound sessions + A after binding; write-ops → C + A. Codex consultation in flight at `.scratch/codex-prompts/serena-nopath-sticky-20260520-055000.md` (background task). Plan v2 will integrate the consultation verdict.

---

## Phase A: Supervisor state-machine wiring (depends on PR #229)

### A.1: Catalog audit + verified symbol table for state-machine wiring [no-code]

**Scope**: extend the v5 plan's symbol catalog with state-machine-specific symbols needed for production wiring. Reads-only inventory.

| Concept | Real symbol | Location |
|---|---|---|
| State machine entry point | `api.Transition(state SMState, ev SMEvent, ctx SMContext) (newState SMState, side string, persistBefore bool, matched bool)` | `internal/api/supervisor_state_machine.go:47-164` |
| Event loop FIFO | `api.NewEventLoop` + `api.LoopEvent{Kind: EvX, TaskName: "..."}` + `loop.Post(...)` | `internal/api/supervisor_event_loop.go` (TBD: full file read in v2 to fill in exact API surface) |
| Per-daemon SM state cache | `DaemonRuntimeTracker` (current) — TODO: separate SM-state field from runtime tracker | `internal/cli/supervisor_runtime_tracker.go` |
| Reconciler spawn fan-out | `(*Reconciler).Reconcile(intent, daemonIntent, currentRunning, now)` calls `r.spawn(d)` | `internal/cli/supervise_reconcile.go:117` |
| Production spawn fn | `makeProductionSpawnFnWithStatePath(job, events, tracker, statePath)` | `internal/cli/supervise.go:1498` |
| `cmd.Wait()` goroutine | `internal/cli/supervise.go:1539-1543` (legacy silent) → `:1539-1572` after PR #229 (with `daemon-exited` emit) | (file:line) |
| `MarkSpawned` / `MarkExited` | `DaemonRuntimeTracker.MarkSpawned(taskName, pid, startedAt)` / `MarkExited(taskName)` | `internal/cli/supervisor_runtime_tracker.go:41-90` |

**Acceptance criteria**: every state-machine-related symbol the v2 implementer will call appears in this table with verified file:line. Pre-existing symbols from v5 plan's catalog remain valid; this table adds the SM-wiring symbols.

**TBD for v2**: read `supervisor_event_loop.go` end-to-end to populate exact `Post` / `Run` / event-loop lifecycle API. (Codex's deep-diagnostic noted "FIFO event loop" exists but didn't fully trace the production caller — v2 implementer must verify.)

### A.2: Wire state machine into production reconcile + spawn paths

**Scope**: replace `r.spawn(d)` direct call in `supervise_reconcile.go:117` with `eventLoop.Post(LoopEvent{Kind: api.EvStart, TaskName: d.TaskName})`. Replace silent `MarkExited` in `supervise.go:1539-1572` (post-PR #229) with `eventLoop.Post(LoopEvent{Kind: api.EvChildExit, TaskName: taskName})`. Add production handler in supervise.go (currently absent per codex finding at `supervise.go:436-447`) that consumes loop events + calls `api.Transition` + executes side effects (spawn / arm-backoff-timer / persist).

**Gating**: blocked on (1) PR #229 merged + bin upgrade, (2) `daemon-exited` events visible in operator's `supervisor-events.log`, (3) serena crash root-cause identified via those events. Implementation cannot proceed until validation data exists.

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

### B.1: workspaces.yaml schema + load/save API

**Scope**: new file `internal/api/workspaces.go` with:

```go
type WorkspacesFile struct {
    Version    int
    Workspaces []WorkspaceEntry
}

type WorkspaceEntry struct {
    Path          string    // abs-path on disk
    Languages     []string  // snapshot of .serena/project.yml at register time
    Default       bool      // exactly one workspace can be default
    RegisteredAt  time.Time
    RegisteredVia string    // "manual" | "auto-detect" | "migration"
    SerenaPort    int       // mcphub-allocated; persisted
}

func ReadWorkspaces(path string) (*WorkspacesFile, error)
func WriteWorkspaces(path string, ws *WorkspacesFile) error  // flock RMW via api.SecureWriteClientConfig
func ValidateWorkspaces(ws *WorkspacesFile) error
```

**Acceptance criteria**:
- YAML schema match spec §5
- Exactly one workspace has `default: true` (validator rejects 0 or N>1)
- Each `Path` exists on disk AND contains `.serena/project.yml`
- `SerenaPort` unique within file AND within registered port_pool range (configurable, default 9121-9199)
- `Languages` non-empty
- Atomic write via existing `SecureWriteClientConfig` pipeline (parent-dir DACL + flock)

**Test contract**:
- `TestWorkspacesFile_Load_Valid` — round-trip via WriteWorkspaces → ReadWorkspaces
- `TestWorkspacesFile_Validate_RejectsMultipleDefaults`
- `TestWorkspacesFile_Validate_RejectsMissingProjectYml`
- `TestWorkspacesFile_Validate_RejectsPortOutsideRange`

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

**Test contract**: integration test using two registered workspaces + a sample serena tool call → verify correct upstream daemon hit.

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

### D.1: Manifest schema extension — `kind: workspace` + `daemon_template`

**Scope**: extend `internal/config/manifest.go` to support new manifest kind:

```yaml
name: serena
kind: workspace           # new: was 'global'
transport: native-http
command: uvx
base_args: [...]
env: {PYTHONUNBUFFERED: "1"}
daemon_template:
  context: codex
  port_pool: [9121, 9122, ..., 9199]
  extra_args_template:
    - --context
    - codex
    - --project
    - "${workspace.path}"
```

**Acceptance criteria**:
- New `kind: workspace` value (in addition to existing `global` + `workspace` LSP-bridge-style)
- `daemon_template` struct with `context`, `port_pool`, `extra_args_template`
- `${workspace.path}` token expanded at spawn time per workspaces.yaml entry
- Schema validation rejects malformed templates (e.g., no `${workspace.path}` in args when `kind: workspace`)
- `dec.KnownFields(true)` strict parse — every new YAML key has a Go struct field with yaml tag

**Test contract**:
- `TestManifest_KindWorkspace_DaemonTemplate_Parsing`
- `TestManifest_KindWorkspace_RejectsMissingPortPool`
- `TestManifest_KindWorkspace_TokenExpansionAtSpawn`

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

**Behavior**:
1. Survey existing serena daemons in supervisor-intent.json: `claude` (legacy), `codex` (legacy), `unified` (intermediate)
2. Read workspaces.yaml (if empty, prompt operator to register at least one workspace + bail out)
3. For each registered workspace, generate new descriptor via D.2's template expansion
4. Replace legacy descriptors with new per-workspace descriptors in supervisor-intent.json
5. Trigger supervisor reload (IPC `reload` or restart)

**Acceptance criteria**:
- Idempotent (re-running on already-migrated state is no-op)
- Refuses if workspaces.yaml is empty (clear error message)
- Preserves per-workspace `.serena/cache/` directories (no data loss)
- Audit event `serena-dynamic-pool-migration` written to events log

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

## Phase F: No-path-args sticky-session (pending codex consultation)

**This phase finalized after codex consult result lands**. Current placeholder:

### F.1: Read-only no-path aggregate (Mode D)

For `list_memories`, `get_current_config`, `check_onboarding_performed`: when session not yet bound, aggregate response from all daemons.

### F.2: Write/delete no-path reject (Mode C)

For `write_memory`, `delete_memory`, `onboarding`: when session not yet bound, return HTTP 412 Precondition Failed with explicit "no workspace bound; call a path-aware tool first" message.

### F.3: Sticky bind on first path-call (Mode A)

Subsequent no-path calls after a path-call → forward to bound workspace (Phase C.3).

**Acceptance criteria**: TBD post-consult. Tests TBD.

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

## Open questions

1. **No-path-args fallback semantics** — pending codex consultation (Decision 5)
2. **MCP session ID stability** across reconnects — does serena's Streamable HTTP transport surface `mcp-session-id` header that survives reconnect? If session_id changes per reconnect, sticky-session map needs different anchor (e.g., client TCP connection ID OR a `client-state.yaml` persisted across sessions)
3. **`workspaces.yaml` hot-reload latency** — when operator adds workspace via CLI, how long until first request hits the new daemon? Acceptable: 5-10s. Below that → need explicit `mcphub workspace reload` IPC trigger
4. **Auto-register `.serena/project.yml` defaults** — `read_only: false`, `excluded_dirs: [...]`, what `language_detector_threshold`?
5. **Port allocation persistence on unregister** — keep port reserved for retention period or release immediately?
6. **State-machine wiring (Phase A.2) blocked on** — PR #229 merge + binary upgrade + serena crash root cause from new `daemon-exited` event
7. **Cross-workspace memory access** — `read_memory name` in session bound to workspace A, but operator wants to read memory from B → out-of-scope or special-case?
8. **Migration from operator's current state** — operator currently has `unified` intermediate (committed in this branch); migration G.1 needs to handle BOTH legacy 2-daemon (claude+codex) AND unified intermediate cases

---

## Review history

- **v1 (this draft)**: initial architectural posture + phase breakdown. Pending dual review.
- **v2 (TBD)**: incorporate sonnet + codex review of v1.
- **v3+ (TBD)**: convergence iterations until 0 BLOCKERS per established v1→v5 pattern from servers-matrix plan.

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
