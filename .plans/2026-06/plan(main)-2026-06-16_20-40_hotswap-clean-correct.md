# Hot-swap (zero-downtime config) — clean+correct implementation plan

Date: 2026-06-16. Design+decision: `work-items/decisions/2026-06-16-hot-swap-zero-downtime-config.md`.
Operator constraint: hot-swap must NET-IMPROVE stability. Decision: "делаем чисто и правильно."

## Decided architecture (settled)

- **slice 2 = (b) event-driven session invalidation (PRIMARY) + (a) failure-driven self-heal (BACKSTOP)**. NO timer. (c) proactive-handoff REJECTED on ownership grounds.
- **slice 3** = hot-swap-eligibility classifier (pure function, observation-only).
- **slice 4** = gate-ON migration (opt-in, AFTER slice 2) — delivers the literal goal (config UPDATE doesn't drop the client connection).
- **slice 1** (readiness probe) = DEFERRED (no consumer once slice 5 dropped).
- **slice 5** (blue/green) = DROPPED (violates api.Transition single-live-child invariant; a UX problem not a stability fault; 2+4 already deliver the goal).

Cache-coherence framing: the hub's per-daemon MCP session is a CACHE of a resource (the daemon) the SUPERVISOR owns. Invalidate via the resource owner's lifecycle events (b), with a failure-driven backstop for lost events (a). No timer in either.

## Phases (ordered; ship (a) before (b) — (a) works standalone, (b) is the clean primary added on top, (a) stays as backstop)

### Phase 0 — Test-first foundation (MANDATORY, council gate, zero risk)
- Scope: `internal/api/hub_mcp_aggregator_test.go` (new/extended).
- Codegraph confirmed NO covering tests for `dispatchToolsCall` / `resolveToolsCallRoute` / `postToolsCall` / `postInitialize`. Add them using `httptest` daemon stubs: success, transport-fail (closed listener), HTTP-4xx, missing-session, duplicate-id (-32600). This is the regression baseline BEFORE any retry code.
- Acceptance: the dispatch path's CURRENT behavior is pinned. Pure-additive (no prod change).
- Verify: `go test -run 'TestDispatchToolsCall|TestResolveToolsCallRoute|TestPostToolsCall|TestPostInitialize' ./internal/api/` (state-safe — these stub the daemon over httptest; no real daemons, no state dir).

### Phase 1 — Error-class split (no behavior change)
- Scope: `doDaemonPost` + `postToolsCall` (hub_mcp_aggregator.go).
- Introduce a typed error `daemonHTTPError{code int; body []byte}` for the HTTP≥400 branch (:782); transport errors stay the raw `client.Do` error. `postToolsCall` propagates the distinction (today it returns a bare err, collapsing both).
- Add `isRetriableTransportFailure(err) bool` = connection-refused / connection-reset / dial-failure ONLY (via `errors.As(*net.OpError)` / `syscall.ECONNREFUSED`/`ECONNRESET`). EXPLICITLY excludes: HTTP≥400 (daemon received it → non-idempotent side effect may have run), and TIMEOUT (ambiguous — request may have landed).
- Tests: HTTP-4xx → not retriable; conn-refused → retriable; timeout → not retriable; success → no err.
- Acceptance: Phase-0 tests stay green (pure error-typing, no behavior change).

### Phase 2 — (a) BACKSTOP: failure-driven self-heal (the first user-visible win; ships standalone, no event-bus dep)
- Scope: `dispatchToolsCall` (hub_mcp_aggregator.go:633) + a per-daemonKey singleflight helper.
- On `isRetriableTransportFailure`: re-resolve the daemon port from `LoadResolverSnapshot()`, re-run `postInitialize` under per-daemonKey **singleflight** (anti init-storm), refresh `sess.InitSuccesses[daemonKey]` + `sess.DaemonProtoVer[daemonKey]` under `sess.mu`, then retry `postToolsCall` ONCE in-place (hard per-call counter, NOT dispatch re-entry → avoids the -32600 duplicate-id self-collision; allocate a fresh daemonReqID + in-flight row for the retry since the daemonSID changed).
- NO timer. Retry budget stays inside the existing `PerCallWallClockCap`.
- Tests: transport-fail→reinit→retry-succeeds (httptest daemon that 503s once then recovers); HTTP-4xx→NO retry (assert tool not double-called via a hit-counter stub); concurrent calls→singleflight coalesces to ONE re-init.
- Gate: LIVE-VERIFY with operator present (serves ALL MCP calls). Smoke: restart a daemon (e.g. `time`), issue a tool call, confirm it self-heals on the retry.

### Phase 3 — slice 3: eligibility classifier (pure, observation-only)
- Scope: `supervise_reconcile_ipc.go` (classifyDriftAction) + `supervisor_ipc_types.go` (DriftEntry — ADDITIVE optional field).
- Pure function: old vs new descriptor → {hot-swappable-behind-front | needs-hard-restart}. Surface in DriftEntry WITHOUT changing Action. Operator/GUI can SEE "this update will blip" before any behavior change.
- Tests: classifier unit table.
- Gate: additive optional wire field only (no IPC-client break).

### Phase 4 — (b) PRIMARY: event-driven invalidation (the clean architecture)
- PREREQ: scope the event-bus daemon-lifecycle event. Event-bus is PARTIAL (`events.go` + GUI `/api/events` SSE). Determine the supervisor→hub channel (supervisor IPC, or supervisor-events.log tail, or extend the planned fsnotify event-bus). The supervisor already posts `EvHealthOK` (StSpawning→StRunning) — surface a `daemon-restarted`/`daemon-ready{task,port}` event from there.
- Scope: supervisor (publish on restart + ready) + hub (subscribe: invalidate `InitSuccesses[daemonKey]` on daemon-down/restart; proactively re-init on daemon-ready).
- Tests: published event → hub invalidates → re-inits; lost event → (a) backstop still self-heals.
- Gate: (b) is the clean primary; (a) remains the lost-event backstop. Right info-flow (authority publishes → observer converges); clean ownership (supervisor=lifecycle, hub=session) per the repo's State-synchronization-ownership rule.

### Phase 5 — slice 4: gate-ON migration (opt-in; delivers the literal goal)
- Scope: existing `install_hub_reconcile.go` + an opt-in setting.
- Migrate a daemon to gate-ON so the client faces the STABLE hub port (client→hub never drops; only the hub's upstream session goes stale, healed by (a)+(b)). One restart blip per daemon at migration time (scheduled/announced); gate migration to StRunning/StIdle daemons only.
- Tradeoff to state explicitly: the hub becomes a shared SPOF for gate-ON daemons — net-positive only because hub uptime >> daemon uptime under the single-owner supervisor.
- Gate: opt-in; NEVER flip the default fleet-wide this round.

## Cross-phase invariants / gates

- NO timer anywhere (event/client-driven cadence; a backoff timer is a race-window anti-pattern).
- Retry ONLY on transport conn-refused/reset — never HTTP, never timeout (double-execution hazard: MCP tools/call has no idempotency key).
- `api.Transition` single-live-child invariant UNTOUCHED (slice 5 dropped; Conc-F4 invariant test already guards the off-loop posters).
- Test-first: Phase 0 lands before any Phase-2/4 retry code.
- Slice 2 live-verified with the operator present.
- All internal/api test runs: state-safe (the aggregator tests use httptest stubs — no real daemons/state; the few that touch state get state-dir + LOCALAPPDATA isolation + a state backup).

## Scope EXCLUDED this round
- slice 5 blue/green / any api.Transition spawn-before-exit.
- slice 1 readiness probe (deferred until a consumer exists).
- (c) proactive supervisor→hub session handoff (ownership violation).
- serena + other stateful daemons EXCLUDED from hot-swap eligibility (explicit restart only — restart drops LSP/index state).

## Verification (each phase)
`go build ./... && go vet ./...` + the phase's `-run` tests (state-safe). Slice-2/4: live daemon-restart smoke with the operator. Leak-check before any push.
