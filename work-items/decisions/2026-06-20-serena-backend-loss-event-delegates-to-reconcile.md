---
status: proposed
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
floor. The poller keeps a precise per-key `lastSerenaLivePID` tracker and emits
`prev_pid` only from that prior-generation source; it still does not make teardown
decisions.

## Consequences

- All 4 bot edges dissolve by construction (the re-derived logic is deleted, ~50 net lines
  removed); the idle-stop regression cannot recur because there is no second teardown path.
- The poller remains a precise PID-change edge detector: `lastSerenaLivePID` is a per-key
  last-live-PID tracker that supplies the wire-level `prev_pid` for the event. That tracker
  is a source of prior-generation evidence for `ReconcileSerenaBackendLossViaIPC`, not a
  competing owner of the teardown decision.
- The GUI-started-after-daemon case, where `lastSerenaLivePID==0` because the live PID was
  never observed by this GUI poller instance, is handled by the 30s reconcile floor plus the
  forward-failure floor. `serenaBackendLastPID` is seeded at session bind, so the floor still
  detects a later A->B PID change; this is an accepted speed fallback, not a correctness
  fallback.
- General rule (applies beyond serena): when adding a fast-path signal for a hardened
  reconcile/floor, wire the signal to TRIGGER that floor's existing per-item decision, do
  not author a parallel decision path. A parallel path re-opens every edge the floor already
  closed.

Referenced from PR #384. Promotion to `accepted` is the architecture-reviewer's call.

## Follow-up: baseline-absent prev-PID hint

Round 4 kept the same delegation rule while closing the baseline-absent restart gap:
the `daemon-backend-lost` subscriber may record the event's `daemon` workspace key plus
raw `port` and `prev_pid` as an identity-keyed per-port hint, but only
`ReconcileSerenaBackendLossViaIPC` consumes that hint.
The hint is used solely inside the reconcile owner's baseline-absent first-observation
path, after idle-stop, restarting, absent-row, and dead-row cases have already been
classified by the owner. The subscriber still does not map ports to workspaces, inspect
IPC status, or call teardown.
On consume, the hint is usable only when its stored workspace key matches the reconcile
path's current `pathToKey[path]`; a mismatch means the port was reassigned to another
workspace, so the stale hint is deleted without marking backend loss.

The accepted retain/clear lifecycle is:

| Case | Condition | Hint action |
|---|---|---|
| (a) | IPC status read failed | RETAIN wanted-port hints; clear only non-wanted ports because a transient IPC failure leaves the prev-PID as the only old-generation witness. |
| (b) | Successful reconcile has zero tracked workspaces (`knownKeys==0` or `wantPaths==0`) | CLEAR all hints because no bound session can consume them, and a survivor could misfire on a future fresh bind. |
| (c) | Successful reconcile maps the port to a tracked workspace with a prior PID baseline | CLEAR because the stored baseline is the prior-generation source. |
| (d) | Successful reconcile is baseline-absent and falls through to a present healthy PID | CLEAR by consumption in `consumeSerenaBackendPrevPIDHintLocked`; mark backend loss only when the hint's workspace key matches `pathToKey[path]` and `hintPrev != newPID`. |
| (e) | Successful reconcile is baseline-absent and observes restarting (`StalePID!=0`) or idle-stopped without consuming this tick | RETAIN, bounded by router-session liveness for that workspace. |
| (f) | Successful reconcile maps the port to no tracked workspace | CLEAR because no current router-bound workspace can consume that hint. |

Case (e)'s bound is the existing liveness predicate:
`s.serenaRouterSessions.withWorkspaceCount(pathToKey[path]) > 0`. The moment the
workspace has zero router sessions bound, the per-port hint is cleared. This adds no
tick counter and does not add a PID field to `routerSessionBinding`; the session-bound-PID
alternative remains rejected because the bind site cannot obtain the PID without the same
fallible IPC read, which would invert ownership back out of the reconcile owner.

## Terms and Abbreviations

- GUI: graphical user interface.
- IPC: inter-process communication.
- PID: process identifier.
- SSE: server-sent events.
