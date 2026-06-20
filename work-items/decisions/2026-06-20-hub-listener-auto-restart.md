---
status: proposed
date: 2026-06-20
owner: architect
supersedes-backlog: work-items/backlog/closed/2026-06-16-hub-listener-hang-no-recovery.md (auto-recovery half)
---

# Decision: gate-ON hub-listener auto-restart on sustained unresponsiveness

## Context

The gate-ON hub aggregate listener is a fire-and-forget `http.Server` goroutine
inside the GUI process (`internal/gui/hub_listener.go:351-373`). It is the SINGLE
socket for all hub-routed MCP under gate-ON: `/clients/<c>/mcp`, `/g/<group>/mcp`.
A HANG (wedged accept loop, stuck handler, lock deadlock) with the GUI alive takes
EVERY hub-routed server down at once; `\mcp-local-hub-liveness` probes the
supervisor lock, not this listener, so it stays green. Today recovery is manual
(restart the GUI). The observability half shipped:
`api.HubListenerHealthWatcher` (`internal/api/hub_listener_health_watcher.go`)
TCP-dials the bound port on a 15 s cadence and emits
`hub-listener-unresponsive` (warn) after 3 consecutive failed dials. It does NOT
restart. This decision designs the deferred auto-restart half.

## The load-bearing facts (verified)

1. **Re-binding preserves the port + identity for free.**
   `BindHubMcpListener` (`internal/api/hub_mcp_bind.go:157`) binds to
   `fmt.Sprintf("127.0.0.1:%d", ep.Port)` where `ep.Port` is read from the
   persisted `hub-mcp.endpoint.json` (`loadHubEndpointLocked`, bind.go:129).
   `ensureHubEndpointLocked` (`hub_mcp_instance.go:87-128`) re-persists the SAME
   `InstanceID` (only generates when missing; refuses on blank-but-present, line
   104-113) and the SAME `Port` (line 114: `if ephemeralPort > 0 { ep.Port = ... }`
   — the OS hands back the same port because the listener requested it explicitly).
   **Therefore a second `startHubMcpListener` call on the same state re-binds the
   IDENTICAL port and serves the IDENTICAL InstanceID.** Gate-ON client URLs are
   NOT orphaned. This is the property the `--reset-port` B2 guard
   (`hub_gate_detect.go`) exists to protect; auto-restart preserves it by
   construction (it never touches `ResetHubPort`).

2. **`startHubMcpListener` / `ShutdownHubListener` are already idempotent,
   ctx-bounded, and self-contained.** `ShutdownHubListener` (hub_listener.go:389)
   is a no-op on nil, drains under a 5 s budget, force-`Close()`s on drain
   failure, closes the session store, removes the control-token file.
   `startHubMcpListener` re-creates a FRESH `HubSessionStore`, `HubMcpHandler`,
   `InternalReloadHandler`, re-publishes the resolver snapshot
   (`publishResolverSnapshotForHubBind`), and re-spawns the per-listener
   `DaemonRestartWatcher` + `HubListenerHealthWatcher`. All hub state that must
   reset (sessions, route maps, per-port stale markers) lives INSIDE the bundle
   and is rebuilt; all state that must persist (port, InstanceID, tokens,
   groups.yaml) lives in flock-guarded state files read fresh on re-bind.

3. **The Server already owns an atomic bundle slot with a CAS ownership
   protocol.** `Server.hubMcpComp atomic.Pointer[HubListenerComponents]`
   (`server.go:586`); the start/shutdown handoff already uses
   `CompareAndSwap` / `Swap(nil)` (server.go:994-1003, 1039, 1079). The restart
   driver composes onto this exact primitive.

4. **The B2 `--reset-port` guard and `reconcile-hub-mode` are orthogonal.**
   `--reset-port` refuses (exit 8) while any client is gate-ON precisely because
   it CHANGES the port. Auto-restart never changes the port, so it neither trips
   nor needs that guard, and never needs `install --reconcile-hub-mode` (which
   rewrites client URLs to a NEW port).

## Decision

Add a **single restart-driver goroutine, owned by `Server`, gated on the
Server-start ctx**, that the health watcher signals once on the transition into
sustained-unresponsive. The driver tears down the current bundle and re-creates
it in place via the EXISTING `ShutdownHubListener` + `startHubMcpListener`,
re-binding the SAME persisted port, under a bounded restart budget with backoff.
No new bind path, no new state file, no port mutation.

This is the smallest correct seam: the watcher KEEPS its sole responsibility
(detect + emit), the driver OWNS the restart decision, and the re-create reuses
the two lifecycle functions that already exist and are already hardened.

### Change-Surface Contract

- **Intended change surface:**
  - `internal/api/hub_listener_health_watcher.go` — add a one-shot
    `onUnresponsive func()` callback seam fired on the transition INTO unresponsive
    (alongside the existing `emit`). Pure additive field + one call site at
    watcher.go:163. The watcher does NOT gain restart logic.
  - `internal/gui/hub_listener.go` — add `restartHubListener(ctx, s)` driver +
    wire the watcher's `onUnresponsive` to a `Server`-owned trigger channel at
    the `NewHubListenerHealthWatcher(...)` call site (line 349). The driver calls
    the existing `ShutdownHubListener` + `startHubMcpListener` and re-CAS-es the
    bundle into `s.hubMcpComp`.
  - `internal/gui/server.go` — add `hubRestartCh chan struct{}` (+ a
    `hubRestartGen atomic.Int64` loop-guard counter) to the `Server` struct;
    spawn the restart-driver goroutine in `Server.Start` on the SAME ctx that
    owns the hub-init goroutine, AFTER the hub-init goroutine.
- **Approved extension seam(s):** the watcher's new `onUnresponsive` callback
  field; the `Server.hubRestartCh` trigger channel; the existing
  `s.hubMcpComp` atomic CAS/Swap slot (consumed, not redefined); the existing
  `ShutdownHubListener` + `startHubMcpListener` functions (called, not modified).
- **Protected / must-not-touch surfaces:**
  - `BindHubMcpListener` and the entire `hub_mcp_bind.go` step sequence — NOT
    edited. The re-bind goes through it unchanged so port + InstanceID
    preservation is inherited, not re-implemented.
  - `ResetHubPort` / `ResetHubPortContext` / `hub_gate_detect.go` /
    `install_hub_reconcile.go` — NOT touched. Auto-restart must never call a
    port-reset path.
  - `hub_mcp_handler.go`, the 7-check auth gate, the aggregator, the JSON-RPC
    dispatch — NOT touched (the fresh handler is constructed by
    `startHubMcpListener` exactly as today).
  - The gui-server listener (`s.srv`) and its Start/Shutdown branches — NOT
    touched. The hub restart is independent of the gui-server lifecycle.
- **Declared blast radius:** 3 files, all under `internal/{api,gui}`. No new
  package, no new state file, no schema change, no client-facing contract change,
  no change to the gui-server listener. Behavioral change is confined to gate-ON
  hosts experiencing a sustained hub-listener outage.

## Two failure modes — round-trip probe decision (mode b)

The backlog names (a) a wedged ACCEPT loop / dead socket (TCP dial FAILS) and
(b) a HANDLER deadlock (TCP dial SUCCEEDS, a real authed round-trip hangs).

**Decision: v1 triggers ONLY on the TCP-dial signal (mode a). Do NOT add a
full authed round-trip probe in v1.** Rationale:

- The shipped watcher's dial already catches mode (a) AND the most common mode
  (b) variant: a wedged accept loop fills the kernel backlog so even
  `net.Dial` eventually fails — the dial is not purely a socket-existence check.
- A full authed round-trip probe is heavyweight and SELF-HAZARDOUS: it needs a
  valid hub token + InstanceID + a synthesized JSON-RPC `initialize`, allocates a
  real session against the same session cap it is trying to protect, and — most
  importantly — **a probe that exercises the handler path can itself wedge on the
  same deadlock**, turning the health prober into a second hung goroutine. The
  backlog flagged exactly this ("could itself wedge").
- Restarting on a false mode-(b) signal is more expensive than restarting on a
  true mode-(a) signal (it drops live sessions), so the bar for the trigger must
  stay high. The dial's 3-consecutive-failure threshold is the right bar for v1.

A handler-deadlock-specific probe (a real round-trip with its OWN short timeout,
running in a goroutine the driver can abandon) is a **named v2 follow-up**, filed
as backlog, NOT in scope here. v1 closes the dominant, deterministically-detectable
outage class.

## Trigger → restart seam (smallest correct)

- The watcher fires `onUnresponsive()` ONCE, on the SAME transition where it
  already emits `hub-listener-unresponsive` (watcher.go:161-169). The callback
  does a NON-BLOCKING send on `s.hubRestartCh` (`select { case ch <- struct{}{}:
  default: }`) so the watcher never blocks and a second signal while a restart is
  in flight is coalesced (the buffered-1 channel + non-blocking send is the
  idempotency guard — at most one pending restart request).
- The restart-driver goroutine `select`s on `{ <-ctx.Done(): return; <-s.hubRestartCh:
  restart }`. On a restart request it:
  1. Re-check `ctx.Err()` — if the Server is shutting down, return without
     restarting (do NOT restart during a real shutdown).
  2. Apply the backoff gate (below). If this outage exhausts while a restarted
     listener is still alive, emit `hub-listener-restart-exhausted` (error), stop
     retrying this outage, and keep the driver goroutine alive for future
     `hubRestartCh` signals. If exhaustion happened after shutdown and no listener
     bundle remains, the driver owns a slow no-signal retry timer
     (`hubListenerRestartStableWindow`) because no per-listener watcher exists to
     re-signal it.
  3. `old := s.hubMcpComp.Swap(nil)` — atomically take ownership of the current
     bundle (the same Swap the shutdown path uses). If `old == nil`, the shutdown
     path already took it; return.
  4. `ShutdownHubListener(restartCtx, old)` — drain + close the dead listener
     (5 s budget; force-Close on drain failure is already built in). This frees
     the port so the re-bind can re-acquire it.
  5. `newComp, err := startHubMcpListener(ctx, true, s.api)` — re-bind the SAME
     port via the unchanged `BindHubMcpListener` path; this also re-spawns a FRESH
     health watcher + DaemonRestartWatcher on `ctx`.
  6. On success: `s.hubMcpComp.CompareAndSwap(nil, newComp)` (mirror the start
     protocol; on CAS failure tear `newComp` down — the shutdown path won the
     race). Read `LoadHubEndpoint().InstanceID` before shutdown and after
     re-bind, then emit `hub-listener-restarted` with `port`, `attempt`, and the
     actual `instance_id_preserved` comparison (`info` when true, `warn` when
     unverifiable). A verified ID change is degraded, not healthy: emit
     `hub-listener-restart-instance-id-changed` at `error` with the client impact
     and `operator_action: mcphub install --reconcile-hub-mode`; do not auto-rewrite
     client configs and do not mark `hubRestartLastSuccess`.
  7. On bind failure: emit `hub-listener-restart-failed` (warn), leave
     `s.hubMcpComp == nil` (gate-OFF for this process until the next successful
     pass), and let backoff schedule the next attempt. On Windows,
     `WSAEADDRINUSE` / `WSAEACCES` during same-port re-bind is treated as the
     kernel's temporary `SO_EXCLUSIVEADDRUSE` reservation and uses the separate
     same-port schedule below instead of consuming the normal five-attempt
     outage budget.

**Concurrency safety:** the driver and the start/shutdown paths all rendezvous
ONLY through `s.hubMcpComp` Swap/CAS — the same single atomic the existing code
already uses as the ownership baton. Because step 3 `Swap(nil)` is the same
take-the-bundle-or-take-nothing primitive the ctx.Done shutdown branch uses
(server.go:1039), a real shutdown racing a restart can never double-shutdown or
leave an orphan: whichever path Swaps the non-nil bundle owns its teardown; the
loser gets nil and returns. Step 1's ctx re-check plus step 6's CAS-or-teardown
close the "restart published a live listener after Start returned" window the
same way the hub-init goroutine already does.

## Backoff / loop-guard (don't restart-loop a genuinely broken listener)

A listener that is broken by a stable cause (e.g. the bound port got hijacked by
another process while the GUI was wedged, or a handler bug deadlocks immediately
on every request) must NOT restart-loop forever burning CPU and dropping sessions.

- **Bounded consecutive-restart counter with exponential backoff**:
  `restartBackoff = min(baseBackoff << (attempt-1), maxBackoff)`, e.g.
  `base = 5 s`, `max = 5 min`, `maxConsecutiveRestarts = 5`. After
  `maxConsecutiveRestarts` failed-or-immediately-re-unhealthy restarts, this
  outage is exhausted and `hub-listener-restart-exhausted` (error) is emitted.
  The driver remains alive. If no listener bundle exists, it retries on the
  driver-owned slow timer above; otherwise a later watcher signal after the stable
  window resets the counter and is treated as a fresh outage.
- **Rolling restart circuit breaker**: no more than 20 actual start attempts are
  allowed in a 30 minute rolling window, across successful restarts, normal
  failures, same-port retries, and no-signal timer retries. Hitting the rolling
  cap emits terminal `hub-listener-restart-abandoned` (error) and stops the
  driver; operator intervention is required. A genuinely fresh outage after the
  rolling window gets a new budget.
- **Windows same-port reservation backoff**: wrapped `WSAEADDRINUSE` /
  `WSAEACCES` from same-port re-bind is not counted as a normal failed attempt.
  It retries every 30 s under a separate 6 min budget, which outlasts the usual
  Windows TCP `TIME_WAIT`/exclusive-bind reservation without burning the normal
  five-attempt outage budget. If that separate budget expires, the outage emits
  `hub-listener-restart-exhausted` with same-port timeout fields at exactly the
  6 min budget, not one 30 s tick later.
- **"Stable-healthy resets the counter."** A restart counts as SUCCESSFUL only if
  the listener stays reachable for a stability window (e.g. one full health-probe
  interval, 15 s, with no new `onUnresponsive`). The driver tracks the time of the
  last restart; if a new restart request arrives within the stability window of
  the previous restart, the attempt counter INCREMENTS (flapping). If it arrives
  after the listener has been stably healthy, the counter RESETS to 1 (this is a
  fresh outage, not a flap). This is the standard flap-vs-fresh-outage
  discrimination and prevents both restart-loops AND permanent give-up after a
  single later unrelated outage.
- The backoff sleep is `ctx`-bounded (`select { <-ctx.Done(); <-time.After(d) }`)
  so a real shutdown during a backoff wait returns immediately.

## Claims (1:1 review targets for architecture-reviewer)

1. `{ guarantee: the bound hub port is PRESERVED across an auto-restart — gate-ON
   client URLs (/clients/, /g/) are never orphaned; single-owner:
   BindHubMcpListener reading ep.Port from hub-mcp.endpoint.json (hub_mcp_bind.go:157)
   + ensureHubEndpointLocked port preservation (hub_mcp_instance.go:114);
   enforcement-probe: a test that captures the port before a simulated hang +
   restart and asserts HubMcpBoundPort() returns the identical port after, AND a
   grep showing restartHubListener has no call to ResetHubPort/ResetHubPortContext }`
2. `{ guarantee: the InstanceID is PRESERVED across an auto-restart — already-
   installed clients are not 401'd; single-owner: ensureHubEndpointLocked
   (hub_mcp_instance.go:104-113 — generates only when missing, refuses blank);
   enforcement-probe: a test asserting LoadHubEndpoint().InstanceID is unchanged
   across restart + the hub-listener-restarted event carries the actual
   instance_id_preserved comparison and warns on mismatch }`
3. `{ guarantee: auto-restart NEVER fires during a real Server shutdown;
   single-owner: the restart driver's ctx.Done() select arm + the step-1 ctx.Err()
   re-check before Swap; enforcement-probe: a test that cancels the Server ctx then
   signals hubRestartCh and asserts no restart (no new bundle, no hub-listener-restarted
   event) }`
4. `{ guarantee: no double-shutdown / orphaned listener under a shutdown-vs-restart
   race; single-owner: the s.hubMcpComp.Swap(nil) ownership baton shared by the
   restart driver and both Start shutdown branches (server.go:1039/1079);
   enforcement-probe: -race test driving a concurrent ctx-cancel + restart signal,
   asserting exactly one ShutdownHubListener teardown of any given bundle }`
5. `{ guarantee: a genuinely-broken listener cannot restart-loop unboundedly;
   single-owner: the restart driver's maxConsecutiveRestarts + exponential-backoff
   counter with stable-healthy reset; enforcement-probe: a test with an injected
   always-unhealthy startHubMcpListener seam asserting the driver stops after N
   attempts and emits hub-listener-restart-exhausted }`
6. `{ guarantee: the health watcher is NOT given restart logic — it only detects +
   signals; single-owner: the additive onUnresponsive callback seam, fired at the
   same transition as the existing emit; enforcement-probe: grep showing
   hub_listener_health_watcher.go contains no ShutdownHubListener/startHubMcpListener
   call and no hub_mcp_bind import }`
7. `{ guarantee: BindHubMcpListener / the auth gate / the aggregator are not
   modified — the restart reuses the unchanged bind+serve path; single-owner: the
   restart driver calling startHubMcpListener (unchanged); enforcement-probe: git
   diff shows zero changes to hub_mcp_bind.go, hub_mcp_handler.go,
   hub_mcp_aggregator.go }`
8. `{ guarantee: v1 triggers on the TCP-dial signal only; the self-hazardous
   authed-round-trip probe is a named v2 follow-up, not shipped; single-owner: the
   watcher's existing dial-based onUnresponsive transition; enforcement-probe: the
   restart driver has exactly one trigger source (hubRestartCh) and no HTTP-client /
   token / initialize code in the probe path }`

## Test strategy (deterministic hang simulation)

The whole design is built on injectable seams so the hang is simulated, never
raced:

- **Watcher signal:** the watcher already has injectable `dialFn` + `emit`
  (watcher.go:75-79). Add the `onUnresponsive` seam the same way. A test injects a
  `dialFn` that returns an error N times then nil, and asserts `onUnresponsive`
  fires exactly once on the 3rd failure and the recovery info fires once on the
  return — pure, no sockets. (Existing `TestHealthWatcher*` are the template.)
- **Restart driver in isolation:** inject the two lifecycle calls behind function
  fields on the driver (a `shutdownFn func(context.Context, *HubListenerComponents)`
  and a `startFn func(context.Context) (*HubListenerComponents, error)`), defaulting
  to the real `ShutdownHubListener` / `startHubMcpListener`. Tests drive:
  (i) happy restart → fresh bundle CAS'd in, `hub-listener-restarted` emitted;
  (ii) ctx cancelled before signal → no restart;
  (iii) `startFn` always errors → backoff increments, exhausts after N,
  `hub-listener-restart-exhausted` emitted, no-signal timer retries continue, and
  the rolling cap eventually emits terminal `hub-listener-restart-abandoned`;
  (iv) repeated successful restarts inside the stable window exhaust one outage,
  then a later signal after the stable window resets to attempt 1, while repeated
  stable-window-plus-epsilon flaps inside the rolling window eventually abandon;
  (v) -race: concurrent ctx-cancel + signal → exactly one teardown per bundle.
- **Windows same-port reservation coverage:** a Windows-only driver test injects
  wrapped `WSAEADDRINUSE` to prove same-port re-bind failures retry beyond the
  normal five-attempt budget and use the documented 30 s schedule. A real
  socket/TIME_WAIT integration test is not kept in default CI because the
  documented 6 min worst-case wait exceeds the 200 s state-safe race-test budget;
  the schedule proof is the CI-safe enforcement probe. A permanent-reservation
  test asserts timeout at the 6 min budget boundary.
- **End-to-end (1 integration test, Windows-gated like the other hub tests):**
  stand up a real gate-ON listener via the existing hub test harness, capture the
  bound port + InstanceID, replace the live `srv` with a deliberately-wedged
  handler (a handler that blocks, OR simply `srv.Close()` the underlying socket to
  simulate the dead-accept case which the dial detects), signal a restart, and
  assert: (a) `HubMcpBoundPort()` returns the SAME port, (b)
  `LoadHubEndpoint().InstanceID` is unchanged, (c) a real authed
  `/clients/<c>/mcp` round-trip succeeds against the restarted listener.
- **Port-preservation falsification grep:** a guard test (or CI grep) asserting
  `restartHubListener` never references `ResetHubPort`.

## Scope: M

Three files, additive seams onto existing primitives, no new package / state
file / contract. The bulk is the backoff/loop-guard state machine and the
deterministic test scaffolding. Larger than S only because of the
flap-vs-fresh-outage discrimination and the -race ownership test.

## Adjacent findings

None. The investigation stayed within the hub-listener lifecycle.

## v2 follow-ups (named, out of scope)

- Handler-deadlock-specific probe: a real authed `/clients/<c>/mcp` round-trip
  with its own short timeout, run in an abandonable goroutine, to catch mode (b)
  where the accept loop is live but handlers hang. Deferred because it is
  self-hazardous (can wedge) and mode (a) dominates.
- Surfacing restart state on the GUI Dashboard / a badge (the `hub-listener-*`
  events already land in hub-mcp.log; a UI surface is additive).

## Gate

PASS — the design is traceable to the verified bind/lifecycle facts, reuses the
existing hardened lifecycle functions, preserves the port + InstanceID by
construction, composes cleanly with the B2 reset-port guard (orthogonal), and
defines the trigger seam, backoff guard, failure modes, observability, and a
deterministic test strategy. Ready for the planner.
