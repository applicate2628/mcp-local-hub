# Decision: hot-swap (zero-downtime config apply) — design + staged plan

- **status:** proposed (awaiting operator answers to the open questions below)
- **date:** 2026-06-16
- **driver:** competitor-parity keystone, user-confirmed ("hot-swap точно нужен"). ROADMAP §"Competitor adoptions".
- **research:** read-only investigation agent (workflow wf_eb66ed6b-d5a) mapped the live reconcile path; recommendation cross-checked against the supervisor lifecycle core.

## Problem

Applying a server add/remove/**update** to `supervisor-intent.json` should not
drop the client's MCP connection. Today an UPDATE of a live daemon's
spawn-affecting descriptor (port/command/args/env/workspace/runtime_spec) forces
a **serial kill→wait-for-exit→respawn** blip.

## Current reconcile path (verified, file:line)

- Install writes intent (atomic temp+rename, `supervisor_intent.go:431`) →
  `nudgeSupervisorReconcileAfterGlobalInstall` (`install_parsed_manifest.go:741/959`)
  → IPC `handleReconcile` (`supervise_reconcile_ipc.go:117`).
- `classifyDriftAction` StRunning arm: if `supervisorDescriptorSpawnDrift`
  (`supervise_reconcile_ipc.go:779` + `runtimeSpecSpawnDrift:805`) →
  `reconcileActionPostEvManualRestart`.
- Blip: `api.Transition` StRunning + EvManualRestart → StExiting (terminate) →
  real EvChildExit → StSpawning (create-process). The new process is NOT started
  until the old exit is observed (strictly serial), and it **reuses the same TCP
  port**, which is what forces the serial ordering.

## Restart-blip root cause (two parts)

1. **Same-port reuse** forces terminate-then-respawn serial ordering.
2. **MCP session is bound to the daemon process** — even a connection-preserving
   front (the mcphub-hub aggregate) loses its cached upstream `Mcp-Session-Id`
   on respawn and has **no auto-reinitialize-on-restart logic today**, surfacing
   as a `-32000 tools/call` failure until the next manual init.

## What already hot-applies today (no blip)

- **ADD** a daemon: existing daemons untouched (new one just spawns).
- **Cosmetic-only UPDATE** (manifest_hash, updated_at, identity): `no_op` —
  `supervisorDescriptorSpawnDrift` deliberately ignores these.
- Only a **spawn-input UPDATE of a live daemon** restarts it. That is the gap.

## Recommended approach: Option C (hybrid) — front-resilience first, blue/green last

Rationale: `api.Transition` is the single highest-blast-radius function in the
repo — a bug there kills the whole live fleet (already demonstrated by the
subagent-test-wiped-intent incident). So land the front-only resilience first
(MEDIUM risk, contained to the hub request path, never touches the lifecycle
core), and reserve the genuinely zero-downtime blue/green port swap for a final,
flag-gated, user-reviewed cutover.

### Staged plan (safe foundation → risky cutover)

1. **FOUNDATION (safe, additive):** daemon **readiness probe** as a pure
   function — replace the `process-start == healthy` assumption
   (`supervisor_controller.go:2779` posts EvHealthOK immediately) with an HTTP/MCP
   readiness check used ONLY by the new hot-swap path; leave the existing spawn
   path byte-identical. Gate behind a new code path, not a behavior flip.
2. **FOUNDATION (safe, HIGHEST perceived-downtime win, touches NOTHING in
   supervisor core):** hub aggregator **auto-reinitialize-on-upstream-failure**.
   In `hub_mcp_aggregator.go dispatchToolsCall` (≈585), on connection-refused /
   daemon "no session" from `postToolsCall` (today → -32000 at :635), re-resolve
   the daemon port from the current resolver snapshot, re-run `postInitialize`,
   refresh `sess.InitSuccesses[daemonKey]`, retry the call ONCE. Generalizes the
   `lazy_proxy.go` re-materialize pattern. Purely makes a currently-FAILING case
   succeed — no existing success path changes. Bounded worst case (a retry bug
   = a request-level error, not fleet death).
3. **FOUNDATION (safe, observation-only):** hot-swap-**eligibility classifier**
   (pure function) reporting whether an old→new descriptor change is hot-swappable
   behind the current routing vs needs a hard restart; surface it in the reconcile
   DriftEntry WITHOUT changing the action — so the GUI/operator can SEE "this
   update will blip" before any behavior change ships.
4. **FOUNDATION (semi-safe, opt-in):** migrate global daemons to **gate-ON**
   (mcphub-hub aggregate) so their client-facing endpoint is the stable hub port.
   Existing reviewed mechanism (`install_hub_reconcile.go`); land behind an opt-in
   setting first; do NOT flip the default fleet-wide without user review.
5. **CUTOVER (RISKY — needs user review, flag-gated, separate PR, NEVER bundled
   with foundation):** blue/green port swap. New descriptor-update path that
   allocates a NEW port, spawns the new daemon, waits on the readiness probe,
   atomically repoints the routing front, drains in-flight on the old daemon,
   then terminates the old. Requires a NEW state-machine path (spawn-before-exit)
   — the most dangerous change. Per-daemon opt-in flag default OFF; deterministic
   tests with injected slow-spawn seams; validated live on ONE non-critical daemon
   (e.g. time) first; explicit rollback (flag OFF → serial restart). Do NOT touch
   `api.Transition`'s existing rows — add new rows/states so the OFF path stays
   byte-identical.
6. **SAFETY RAIL across all slices:** every install/reconcile test MUST
   `t.Setenv` the state-dir override (+ LOCALAPPDATA for registry paths). A
   subagent `go test` wiping live `supervisor-intent.json` already killed the
   fleet once. Back up state before any live validation.

## OPEN QUESTIONS (operator decisions gating the design)

1. **Scope:** global stdio-wrapped daemons (memory/time/sequential-thinking) are
   direct-port (gate-OFF) today, so they CANNOT be hot-swapped without first
   migrating behind the hub. Do you want global-daemon hot-swap (requires gate-ON
   migration), or only serena/LSP (already partly fronted)?
2. **Latency tolerance:** is a brief added FIRST-CALL latency during the restart
   window (Option B foundation) acceptable as "hot-swap", or do you require
   literally zero added latency (Option A blue/green)? This decides whether the
   risky `api.Transition` change is in scope at all.
3. **Serena:** it is workspace-session-stateful (sticky router bindings); a
   restart may drop in-progress LSP/indexing state even with the connection
   preserved. Hot-swap serena, or exclude it as inherently stateful?
4. **Port-allocation policy** for blue/green: where do ephemeral new ports come
   from, and must the client-config rewrite stop racing the swap? (Moot for
   gate-ON daemons — client config holds the stable hub port — another reason to
   require gate-ON as a prerequisite.)

## Recommendation for the operator

Approve slices 1-3 (pure-additive foundation, MEDIUM-or-lower risk, mostly
behind new code paths) to land + deploy with you available to verify the hub
request-path change live. Defer slice 4 (gate-ON default) + slice 5 (blue/green
cutover) until the open questions are answered. Slice 2 (hub auto-reinit) is the
single biggest user-perceived win and is ready to implement.
