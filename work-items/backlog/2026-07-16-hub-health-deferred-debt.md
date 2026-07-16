# Hub-health surface — deferred debt (from the PR #555 review panel)

Filed: 2026-07-16
Source: the 5-lane ultracode review panel (Sol architecture / Terra concurrency / Terra frontend /
Terra test-rigor / fable arbiter) on `feat/hub-health-honest-surface` @ 57c6284f, plus 2 Codex-bot
rounds. Everything BLOCKING was fixed in the PR; these four items were explicitly scoped out with the
arbiter's agreement.

## 1. The tracker is driven by the stringly log-emit wrapper, not typed lifecycle transitions

`internal/gui/hub_health.go` — `hubHealthEmitWrapper` wraps the restart driver's `emitFn` and switches
on event NAME STRINGS (`"hub-listener-restart-failed"`, …) to drive tracker state. Health correctness is
therefore coupled to a LOGGING seam: transition ownership is split between the lifecycle owner and the
log emitter, and any caller that supplies a custom `emitFn` silently bypasses health updates.

**Right shape:** the lifecycle owner (the restart driver) calls TYPED tracker methods directly
(`onRestartFailed()`, `onInstanceIDChanged()`, …); logging stays purely observational.

**Why deferred:** no wrong state is produced today — the wrapper delegates unconditionally, the only
`emitFn` in production is the real log emitter, and the arbiter found no regression in the
`hub-mcp.log` stream. The refactor is a pure-structure change with no behavioral gain, so it does not
belong in the same PR as the correctness fixes. (Sol architecture lane finding 4.)

## 2. A queued unresponsive signal survives a watcher recovery

`internal/gui/hub_listener.go` (~251) — the watcher's unresponsive signal is a BARE token on
`hubRestartCh`. If the listener recovers before the driver drains it, the driver can still swap out and
stop a recovered, serving listener.

**Right shape:** carry a component/outage generation on the signal and discard or revalidate it
immediately before taking ownership.

**Why deferred:** PRE-EXISTING restart-driver semantics — not introduced by the hub-health surface (that
change only added a health publish alongside the existing signal). Fixing it means touching the
battle-tested restart handoff, which is its own scoped change. (Terra concurrency lane finding 2.)

## 3. `hub-listener-restarted` with an unverifiable instance id maps to `healthy`

`internal/gui/hub_listener.go` (~370-372) — when the endpoint read fails, `instanceIDChanged` is
undeterminable, so the event falls through to `restarted` → `healthy`, while installed clients may in
fact be orphaned.

**Why deferred:** bounded by genuinely unknowable information; the read error is preserved in the event's
log fields. Surfacing an "unknown" health state would need a fifth state and its own UX. (fable arbiter
residual.)

## 4. No reconcile-ack signal — `needs-reconcile` is process-scoped

`reconcilePending` never clears without a GUI restart, because nothing observes the operator actually
running `mcphub install --reconcile-hub-mode`.

**Arbiter call:** ACCEPTABLE for v1 — with no ack signal, never-clearing is the FAIL-SAFE polarity (a
false "action needed" beats a false "healthy"). The PR mitigates the confusion by stating in the banner
that the notice clears when the hub GUI restarts.

**Right shape (follow-up):** have `install --reconcile-hub-mode` drop an ack (state file bump or an IPC
call) that the GUI observes to clear `reconcilePending` live.

## Not deferred — explicitly rejected as not-worth-code

`serveHubMcpListener`'s stale `registered == nil` read racing a subsequent successful restart's
`markHealthy` (a false `down`): the window is nanoseconds against the ≥5s backoff+bind that separates
the two events. The arbiter overrode the concurrency lane's call here; no code change.
