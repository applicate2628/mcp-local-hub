# Backlog: hub adversarial-rotation follow-ups (restart-path capture + latch staleness)

Filed: 2026-07-18 by $lead, from the fable hidden-bug/arbiter lane on PR #562
(`feat/hub-initial-bind-adversarial-rotate`, the Option-B adversarial InstanceID
rotation for the gate-ON hub aggregate listener). All four are OUTSIDE PR #562's
admitted scope (#561 = INITIAL hub bind only); fable's in-diff verdict was PASS.
Decision context: `decisions/2026-07-18-hub-initial-bind-adversarial-token-rotation.md`.

## F1 (P2) — restart-path capture has NO rotation (the initial-bind fix's mirror gap)

`internal/gui/hub_listener.go` (~476-520). PR #562 rotates the hub InstanceID when
a FOREIGN process holds the persisted port at INITIAL bind. But on a MID-RUN
listener death (unresponsive / serve-exit restart), a foreign process that grabs
the persisted port makes `startFn` fail with WSAEADDRINUSE/WSAEACCES, which
`isHubListenerSamePortRebindPendingErr` treats as a benign TIME_WAIT rebind-pending:
30s backoffs up to ~6 min, then exhausted→Down — **never an owner probe, never a
rotation, never needs-reconcile.** Gated clients keep POSTing
`X-Mcphub-Hub-Token` + `X-Mcphub-Instance-Id` to the foreign holder for the whole
window — the exact harvest class #562 rotates for, entered through a different door.

Concrete scenario: attacker watches the hub port, waits for a GUI crash/hang-restart,
binds in the shutdown→rebind gap, harvests for 6+ minutes with zero rotation.

Fix direction: after `samePortRebindWait` exceeds the max (or after N pending
rounds), run the same `hubInitialBindPortNeedsInstanceIDRotationFn` owner probe and
rotate-once — unify the initial-bind and restart-path adversarial handling behind one
predicate (arch C1 single-owner). Bounded by the same accepted multi-tenant residual
(`MCPHUB_REQUIRE_SINGLE_USER_HOME` + owner-only DACL) as the initial-bind path.

## F2 (P3) — successful reconcile never clears the RUNNING GUI's in-process latch

`internal/gui/hub_health.go:57-62` + `internal/cli/install.go:582-587`. `--reconcile-hub-mode`
clears the DURABLE `reconcile_pending` on disk, but the running GUI's in-process
`reconcilePending` latch has no clear path except a process restart. Gate-ON reconcile
requires the hub live (so the GUI is always up), so the operator performs the exact
prescribed action and the needs-reconcile badge stays stuck until GUI restart — the UI
lies stale while disk is correct. (Pre-existing: the latch was already process-scoped
before #562 made it durable.) Fix direction: GUI re-reads `HubEndpoint.ReconcilePending`
on a health-probe tick, or the reconcile CLI pings an internal SSE-triggering endpoint.

## F3 (P3) — vacuous-success clear on an empty reconcile plan

`internal/cli/install.go:559-587`. An empty plan (zero installed-manifest signals, e.g.
a wiped `supervisor-intent.json`) applies nothing, `Failed==0`, and STILL clears the
durable marker while gated clients may hold the stale InstanceID. Mitigated by the
visible `Plan: 0 op(s)` output; combine-with-state-loss edge only. Fix option: skip the
clear when gate-ON and the plan contains zero AddReplace hub entries.

## Relation

F2/F3/F4-doc were folded into #562 where cheap (F4 doc landed in-PR). F1 is the one
substantive security follow-up — it makes the adversarial-rotation defense complete
across BOTH bind paths. Consider promoting F1 to an active item under the Phase-0
productization initiative (it is the same "honest/robust hub" theme as items 1-3).
