---
status: accepted
date: 2026-06-20
slug: serena-backend-loss-event-delegates-to-reconcile
---

# Event-driven accelerators DELEGATE to the bot-hardened reconcile owner; they never re-derive its decision logic

## Context

PR #384 added a dedicated `daemon-backend-lost` SSE event so a serena pool-daemon
backend loss tears down stale router sessions INSTANTLY instead of waiting for the 30s
`ReconcileSerenaBackendLossViaIPC` floor. The first implementation made the event
subscriber RE-DERIVE the teardown decision — its own event→workspace mapping, its own
stale-PID guard, its own teardown call. The Codex bot found **4 real defects** in that
re-derivation, each a case the existing reconcile path already handles correctly
(bot-hardened over many prior rounds):

1. stale-restart `prev.PID` loss across the `StalePID`-window (no event ever fired for the
   crash→quarantine path the feature was meant to accelerate);
2. an over-aggressive `live != prev_pid` stale guard that rejected the CURRENT
   PID-change event (A→B) as if it were a delayed old event;
3. an **idle-stop session-loss REGRESSION** — the subscriber bypassed
   `serenaTaskHasActiveIdleStop`, so a normal idle shutdown would drop client sessions
   instead of preserving them for transparent wake;
4. a mapping `daemonKey != "" → continue` dead-end that skipped the port fallback for
   legacy/default-named rows.

The `$architect` review (2026-06-20) identified the root cause: `ReconcileSerenaBackendLossViaIPC`
already owns ALL of this — the idle-stop grace, the `serenaBackendLastPID` pid-generation
snapshot (seeded at bind, independent of the poller delta row), the StalePID carry-forward,
the path-mapping with fallback. The subscriber duplicated that decision logic and got each
edge wrong.

## Decision

An event-driven accelerator for an existing reconcile/floor mechanism MUST be a **trigger
that wakes the single bot-hardened decision owner**, not a parallel re-implementation of
its decision logic. Concretely for #384: the `daemon-backend-lost` event subscriber is
reduced to a cheap `server=="serena"` pre-filter that fires a coalesced
`triggerSerenaBackendLossReconcile()`; the existing 30s ticker driver gains that trigger as
a second wake source in its `select`. `ReconcileSerenaBackendLossViaIPC` (and
`terminateSerenaSessionsForWorkspace`, `serenaTaskHasActiveIdleStop`, the forward-failure
floor) remain the only teardown decision path. The event collapses the latency from 30s to
~event-latency while reusing the one correct owner; the 30s tick remains the always-on
floor. The poller emits `daemon-backend-lost` only as a coarse wake trigger with
`{server, daemon, port, state}`; it does not send a prior PID, track prior PID for the
event, or make teardown decisions.

## Consequences

- All 4 bot edges dissolve by construction (the re-derived logic is deleted, ~50 net lines
  removed); the idle-stop regression cannot recur because there is no second teardown path.
- The poller is only a coarse wake source. It may wake the reconcile owner on directly
  observed live-PID changes, confirmed-dead rows, or removed serena rows, but the event body
  carries no prior-generation evidence.
- The GUI-started-after-daemon case, where this GUI instance never observed the old live
  PID, is handled by the 30s reconcile floor plus the
  forward-failure floor. `serenaBackendLastPID` is seeded at session bind, so the floor still
  detects a later A->B PID change; this is an accepted speed fallback, not a correctness
  fallback.
- General rule (applies beyond serena): when adding a fast-path signal for a hardened
  reconcile/floor, wire the signal to TRIGGER that floor's existing per-item decision, do
  not author a parallel decision path. A parallel path re-opens every edge the floor already
  closed.

Referenced from PR #384. Promotion to `accepted` is the architecture-reviewer's call.

## Superseding follow-up: prev-PID hint removed

The baseline-absent prev-PID hint is removed. It was a latency optimization, not a
correctness requirement, and it created a separate edge class around contamination, idle
grace, stale clearing, baseline absence, and races. The accepted owner model is now:

| Surface | Current owner |
|---|---|
| Known-prior daemon generation | `serenaBackendLastPID`, seeded only when missing at bind time (`internal/gui/serena_router_session.go:762-798`). |
| Baseline-present restart/death teardown | `ReconcileSerenaBackendLossViaIPC` compares the current supervisor PID against `serenaBackendLastPID`; no event hint participates. |
| Baseline-absent stale daemon session | The next forwarded request reaches the live replacement daemon with the cached old daemon session id. A live native HTTP MCP daemon returns HTTP 404 for that unknown session; the relay records this as the repo's 404 session-loss contract (`internal/daemon/relay.go:250`), and the router's `doErr==nil` path copies the upstream status and body to the client (`internal/gui/serena_router.go:1107-1163`). The client re-handshakes. |
| Fresh daemon sessions | Native HTTP initialize is not replayed from cache; each initialize reaches upstream and mints a distinct session (`internal/daemon/http_host.go:452-464`). |
| Event acceleration | `daemon-backend-lost` remains a coarse wake trigger only; the body is `{server, daemon, port, state}` and carries no prior PID. |

Therefore the baseline-absent first healthy observation simply establishes the current PID
as the baseline. It must not mark backend loss without an accepted prior-generation owner.

## Terms and Abbreviations

- GUI: graphical user interface.
- IPC: inter-process communication.
- PID: process identifier.
- SSE: server-sent events.
