# Decision: hot-swap (zero-downtime config apply) — design + staged plan

- **status:** ACCEPTED 2026-06-16 — operator chose "делаем чисто и правильно". Final design: slice 2 = **(b) event-driven session invalidation (PRIMARY) + (a) failure-driven self-heal (BACKSTOP)**, NO timer, (c) proactive-handoff REJECTED on ownership grounds. Ship order per the stability council: test-first → error-class split → (a) backstop → (b) event-driven. Slice 3 (eligibility classifier) alongside; slice 4 (gate-ON) opt-in after; slice 1 (readiness) deferred; slice 5 (blue/green) DROPPED.

## FINAL DECISION — slice 2 architecture (operator-approved)

Cache-coherence done right: the hub's per-daemon MCP session is a CACHE of a
resource (the daemon) the SUPERVISOR owns. The cache must be invalidated by the
RESOURCE OWNER's lifecycle events, with a failure-driven backstop for lost
events. No timer in either path.

- **(b) PRIMARY — event-driven invalidation.** The supervisor (daemon-lifecycle
  authority) publishes a `daemon-restarted` / `daemon-ready` event on the
  project's planned event-bus (ROADMAP §Backlog "Event-bus + fsnotify"). The hub
  (MCP-session owner) subscribes: invalidate the cached `InitSuccesses[daemonKey]`
  on daemon-down/restart, re-initialize on daemon-ready. Right info-flow
  direction (authority publishes → observer converges), clean ownership
  (supervisor = process lifecycle, hub = MCP session), matches the repo's own
  State-synchronization-ownership rule. NOT bolted-on plumbing — extends the
  event-bus the project already plans to build.
- **(a) BACKSTOP — failure-driven self-heal.** On a TRANSPORT-level failure
  (connection refused/reset at doDaemonPost — request demonstrably never landed →
  safe to retry), the hub re-resolves the port + re-inits + retries the call
  IN-PLACE, ONCE. This is the defense against LOST events (events are not
  guaranteed delivered; a cache owner must self-heal). Hard per-call retry
  counter (not re-entry → avoids -32600 self-collision); per-daemonKey
  singleflight (anti init-storm); retry ONLY on transport class, NEVER on
  HTTP>=400 (daemon received it → non-idempotent double-execution hazard).
- **(c) REJECTED** — proactive session handoff (supervisor hands the hub a ready
  MCP session) is UX-cleanest (zero blip) but ownership-DIRTY: it makes the
  supervisor perform MCP session management (initialize / Mcp-Session-Id), which
  is the hub's responsibility. Crosses the lifecycle/protocol boundary; not built.
- **NO TIMER anywhere** — (b) triggers on the real lifecycle event; (a) triggers
  on the actual call failure; the cadence is event/client-driven, never a guessed
  backoff (which would be a race-window anti-pattern with cross-machine variance).

Implementation order (council test-first gate is mandatory):
1. **Covering tests** for dispatchToolsCall / resolveToolsCallRoute / postToolsCall /
   postInitialize (codegraph: NONE exist) — do not edit the hot path blind.
2. **Error-class split** — make doDaemonPost/postToolsCall distinguish transport
   failure from HTTP>=400 so (a) can retry only the safe class.
3. **(a) backstop** — in-place transport-only retry + singleflight.
4. **(b) primary** — event-bus daemon-lifecycle event + hub subscription +
   invalidation. (Prereq: the event-bus is PARTIAL per ROADMAP — scope its
   daemon-lifecycle event as part of this.)
5. Verify live with the operator present (serves ALL MCP calls).


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

## OPEN QUESTIONS — ✅ RESOLVED by operator 2026-06-17

1. **Scope** → **serena/LSP (Slice 2) NOW + global daemons gate-ON as opt-in (Slice 4).**
   Migrate memory/time/sequential-thinking behind the hub (opt-in, one announced
   restart-blip per daemon) so their config-update also stops dropping the client.
2. **Latency tolerance** → **brief first-call latency OK (Option B foundation);
   blue/green (Slice 5) stays DROPPED.** Operator leaned minimal-latency AND
   flagged a FUTURE VPS-hosted-hub requirement. VPS REINFORCES this: (a) network
   RTT dominates the sub-second local reconnect → zero-latency's benefit becomes
   noise over the wire; (b) a remote stable hub-endpoint with daemon restarts
   invisible behind it IS the correct VPS architecture (reinforces hub-front
   slices 2/4); (c) blue/green on a VPS is worse — ephemeral ports + 2-live-children
   on a remote host = firewall/port-mgmt cost for no benefit; (d) a VPS hub is a
   SPOF for MORE clients → hub must be rock-solid → MORE reason not to add
   blue/green risk to api.Transition. Decision: minimal-latency, hub-front.
3. **Serena** → **INCLUDE in auto-reinit (heal the crash-respawn case).** Accept
   that in-progress LSP indexing may reset on restart (it re-indexes); the autonomous
   crash-respawn heal (today a hard -32000) is the bigger win.
4. **Port-allocation policy** → **MOOT** (Slice 5 dropped; gate-ON daemons hold the
   stable hub port in client config, no ephemeral ports needed).

**Implementation order (per the Stability Council verdict + these answers):**
Slice 3 (eligibility classifier, pure fn) → Slice 2 (hub auto-reinit, CORRECTED:
transport-only retry + per-daemonKey singleflight + test-first + bounded backoff)
→ Slice 4 (gate-ON migration, opt-in). Slice 5 NEVER this round. Slice 1 deferred.

## STABILITY COUNCIL VERDICT (2026-06-16) — operator constraint "hot-swap must NET-IMPROVE stability"

Convened a multi-lens council (reliability + consultant completed on opus;
architecture + performance rate-limited by the server — both PRE-REBUTTED in the
completed lanes' dissent sections). The two completed lanes CONVERGED and
REVISED the original recommendation above. Stability-maximizing order:

1. **Slice 3 (eligibility classifier) — SHIP FIRST.** Pure function, observation-
   only in DriftEntry (additive optional field; never feeds Action). Zero new
   failure modes; instrument-before-act. IMPROVES stability.
2. **Slice 2 (hub auto-reinit) — SHIP, but CORRECTED + TEST-FIRST.** It is the
   real stability win (heals NOT just the operator blip but the AUTONOMOUS
   crash-respawn case: today a supervisor-respawned daemon surfaces as a hard
   -32000 until manual re-init). BUT the naive "retry once on any err" is a
   concrete NEW failure mode the design missed:
   - `postToolsCall` (hub_mcp_aggregator.go:987) collapses TWO classes into one
     err: TRANSPORT failure (client.Do at doDaemonPost:777 — connection
     refused/reset, request demonstrably never landed → SAFE to retry) vs
     HTTP>=400 (:782 — daemon RECEIVED the request, a non-idempotent tool's side
     effect may have ALREADY executed → DOUBLE-EXECUTION hazard; MCP tools/call
     has no idempotency key). **Retry ONLY on transport-level failure, NEVER on
     HTTP/app-level.**
   - In-place postToolsCall retry, NOT dispatch re-entry (re-entry self-collides
     at -32600 duplicate-id). Hard per-call retry counter, not recursion.
   - Per-daemonKey singleflight on the re-init so a mass restart cannot trigger
     an init-storm across sessions. Same PerCallWallClockCap budget. Guard the
     new `sess.InitSuccesses` write (today written only by AggregateInitialize
     under sess.mu — slice 2 adds a second writer on the hot path).
   - **ADD covering tests for dispatchToolsCall / resolveToolsCallRoute /
     postInitialize FIRST** (codegraph: none exist). Do not edit an untested hot
     path blind. Verify live with the operator present (serves ALL MCP calls).
   - The retry should span the restart window via a short bounded backoff (the
     daemon-restart-to-ready time: ~sub-second for native-Go bridges, ~1-2s for
     npx/uvx-wrapped), not a single shot.
3. **Slice 4 (gate-ON migration) — OPT-IN, AFTER slice 2.** This is what
   delivers the operator's LITERAL goal (a config UPDATE no longer drops the
   client connection — the client-facing endpoint is the stable hub port).
   Tradeoff: the hub becomes a shared single-point-of-failure for gate-ON
   daemons (correlated outage if the hub restarts) — net-positive ONLY because
   hub uptime >> daemon uptime under the single-owner supervisor; state that
   justification. One mandatory restart blip per daemon at migration time
   (schedule/announce, gate to StRunning/StIdle). Never flip the default
   fleet-wide this round.
4. **Slice 1 (readiness probe) — DEFER.** It improves stability (removes the
   "process-start == healthy" lie) but no path consumes it once slice 5 is
   dropped. Keep as a gated helper for later; must fail-OPEN to the old EvHealthOK
   behavior and inherit a bounded ctx.
5. **Slice 5 (blue/green cutover) — DROP this round.** DISQUALIFIED by the
   net-improve-stability bar. It adds a spawn-before-exit path to api.Transition
   — the repo's highest-blast-radius function, freshly stabilized after the
   "60s-killer" + intent-wipe incidents — violating the single-live-child
   invariant the whole stabilization rests on. The "new states, don't touch old
   rows" isolation is ILLUSORY: the two-live-children state shares SMContext,
   supervisor-state.json persistence, the off-loop reaper/timer (the Conc-F4
   invariant guards exactly this), and the reconcile loop. It trades a benign
   recoverable ~1-2s PLANNED blip for a rare UNRECOVERABLE wedge — and the
   restart blip is a UX/availability problem, NOT a stability fault. Slices 2+4
   already deliver the literal goal. Revisit only if, after 2+4 are live, a
   MEASURED stability gap remains (it won't — the blip becomes a transparent
   sub-second upstream reconnect behind a stable client-facing port).

**Reframe both lanes landed on:** "hot-swap as a headline feature" is the wrong
frame. The operator's real goal — don't drop the client connection on a config
change — is a hub-front + auto-reinit problem (slices 4+2). Stated that way, the
risky cutover machinery (1+5) is unnecessary. **Ship 2/3/opt-in-4; never ship 5
this round.**

Council caveat: architecture + performance lanes were rate-limited (re-runnable
once the server limit clears). Their likely pro-slice-5 feature/latency arguments
were pre-rebutted in the completed lanes' dissent (architectural elegance ≠
stability; shaving sub-second off a rare action ≠ worth highest-blast-radius
risk). Codex + fable external lanes unavailable this session (usage-limit /
Mythos gating).

## Original recommendation (SUPERSEDED by the council verdict above)

Approve slices 1-3, defer slice 4 + slice 5 until the open questions are
answered. Slice 2 was framed as "ready to implement" — the council corrected
that: it needs the transport-only retry + singleflight + test-first guards.
