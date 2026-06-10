# Reconcile synchronous dispatch is proxy-only; quarantine-revive on running intent

- **Status:** item (a) CLOSED (no-legacy ownership broadening landed); item (b) reconcile-side bystander revival CLOSED (spawn-direction quarantine-respect gate, PR #279); only the intent-CHANGE path remains as-designed
- **Date:** 2026-06-10
- **Severity:** P3 / design-boundary (no live data-loss; behavior is durable and converges)
- **Context:** adjacent-finding
- **Source:** the opus merge-gate on PR #279 (HEAD eaf7a94) + the fable r4 review of the same branch. The opus gate's honesty finding is FIXED in #279 (the api-side stop/restart paths now report watcher-deferred rows via the typed `DeferredToIntentWatcherCode` per the per-target drift `Action`). The two items below are the parked design boundaries that fix surfaces but does NOT close.
- **Cross-link:** [`docs/superpowers/specs/2026-06-10-clean-architecture-redesign.md`](../../docs/superpowers/specs/2026-06-10-clean-architecture-redesign.md) — Phase B/F.

## Item (a) — synchronous SM dispatch covers ONLY proxy-shaped descriptors

**CLOSED by the "all intent daemons are supervisor-owned" commit on PR #279
(no-legacy directive, spec §0.2):** the
reconcile classifier now broadens supervisor-ownership to ALL intent daemon
rows — `classifyDriftAction` posts `EvIntentUpdate` for a regular `daemon
--server X --daemon Y` descriptor on both the spawn and terminate directions,
so `mcphub stop`/`mcphub restart` of a regular global daemon is synchronously
dispatched through the SM exactly like a proxy. The legacy `sched=missing +
intent=running → needs_manual_review` row died with the `supervisorOwned`
classifier parameter; `isSupervisorOwnedDescriptorForReconcile` was deleted.
The historical description below is retained for context.

`mcphub stop` / `mcphub restart` of a supervisor-owned daemon dispatches the
stop/start intent through ONE `reconcile --apply`
(`stopSupervisorOwnedDaemons` in `internal/api/stop_supervisor.go`;
`restartSupervisorOwnedDaemons` refused-restart branch in
`internal/api/restart_supervisor.go`). The supervisor's reconcile drift
classifier, however, posts `EvIntentUpdate` ONLY for **proxy-shaped**
supervisor-owned descriptors: `isSupervisorOwnedDescriptorForReconcile`
(`internal/cli/supervise_reconcile_ipc.go:549-552`) is true ONLY for
`daemon workspace-proxy` / `daemon serena-proxy` argv.

A **regular global daemon** (`daemon --server X --daemon Y` — memory,
paper-search, time, wolfram, …) is NOT supervisor-owned to that predicate, so
`classifyDriftAction` (`internal/cli/supervise_reconcile_ipc.go:501-531`)
returns `needs_manual_review` on the spawn direction and `no_op` on the
terminate direction (whose `post_ev` gate also requires `supervisorOwned`).
NOTHING is posted; such a daemon's stop/start converges only via the
supervisor's ~60s `IntentWatcher` (intent is durable on disk).

**What #279 fixed (the honesty half):** the api side stops discarding the
`ReconcileResponse` and inspects each target's `DriftEntry.Action`. A
`post_ev_intent_update` entry → plain success row (truly dispatched);
anything else → success-but-deferred row carrying
`api.DeferredToIntentWatcherCode` with empty `Err`
(`internal/api/supervisor_ipc_respawn_client.go`). The result row no longer
FALSELY claims a synchronous SM dispatch that did not happen (the prior
fail-quiet defect: the success row + comments claimed synchronous SM dispatch
for ALL targets, which was false for regular daemons).

**What is PARKED (the behavior half):** making the regular-daemon
synchronous dispatch actually happen requires **broadening the
supervisor-ownership classification** so a regular `--server` descriptor is
classified `supervisorOwned` for reconcile. That change alters the legacy
`sched=missing + intent=running → needs_manual_review` semantics (today that
is the only signal that a scheduler-backed global daemon lost its
`\mcp-local-hub-*` task and needs `install --upgrade`). It therefore belongs
to **Phase B/F** of the redesign spec, NOT this honesty fix:
- **Phase B** (GUI scheduler-fallback → fail-loud) and **Phase F** (move
  fresh-install global daemons off the scheduler-task model onto
  `supervisor-intent.json` reconcile; `mcphub setup` creates zero scheduler
  tasks) together remove the legacy scheduler-task path that the
  `needs_manual_review` row currently protects. Once Phase F lands, a regular
  global daemon IS a supervisor-intent descriptor with no scheduler row by
  design, so broadening the classifier becomes correct rather than a
  semantics regression.

**Guard (now inverted):** `internal/cli`'s
`TestReconcileIPC_RegularGlobalDaemonDispatchedThroughSM` (formerly
`...NotDispatchedThroughSM`) now asserts a regular `daemon --server foo
--daemon default` descriptor yields `post_ev_intent_update` on BOTH
directions (terminate/live-SM and spawn/missing-scheduler), `AppliedCount=1`,
and an `EvIntentUpdate` posted — the broadening this item tracked.

## Item (b) — quarantine-revive on running/absent intent

### (b.1) reconcile-side BYSTANDER revival — CLOSED by PR #279 spawn-direction quarantine-respect gate

The original parked finding warned only about the SM row in the abstract. The
no-legacy broadening on PR #279 (aa1d089) turned it into a **live fleet-wide
bug** by making the `!hasSched` spawn direction unconditional
(`intentDesired==running → post_ev_intent_update`) AND making `reconcile
--apply` fire on EVERY `mcphub stop` / `mcphub restart` (via
`internal/api/stop_supervisor.go` / `restart_supervisor.go`). Because
`handleReconcile`'s drift loop walks ALL supervisor-intent rows — not just the
stop/restart target — a BYSTANDER daemon that is genuinely quarantined
(`StQuarantined`, intent running or absent → `computeIntentDesired` defaults
running) classified `post_ev_intent_update` → `applyReconcileDrift` posted
`EvIntentUpdate(running)` → the SM row at
`internal/api/supervisor_state_machine.go:204-206`
(`StQuarantined + EvIntentUpdate(running) → StSpawning, "reset failures, ..."`)
revived it with the failure window WIPED. Net: stopping or restarting ANY
daemon revived EVERY quarantined bystander, breaking the quarantine contract
("force required") fleet-wide. (Before aa1d089 regular bystanders classified
`needs_manual_review`, so the exposure was proxy-only — the prior parked
description.)

**Fix (PR #279):** spawn-direction quarantine-respect in `classifyDriftAction`
(`internal/cli/supervise_reconcile_ipc.go`): when `intentDesired==running` AND
`smState==api.StQuarantined`, return `needs_manual_review` (NOT post) — so
apply-mode never dispatches `EvIntentUpdate` for the quarantined bystander, and
the drift entry surfaces "quarantined daemon wants running — operator must
force or reset" rather than pretending steady-state. Mirrors the terminate
direction's settled-SM `no_op` (`smStateIsLive` excludes `StQuarantined`) and
the startup reconciler's quarantine-respect. The 60s `IntentWatcher` does NOT
have this hole for bystanders — `diffIntentSnapshots` posts only for CHANGED
intent entries, so an untouched bystander gets no event — therefore the
classifier gate closes the reconcile-side bystander revival **completely**.
Guards: `internal/cli`'s `TestClassifyDriftAction_TerminateDirectionSupervisorOwned`
(new `running quarantined → needs_manual_review` row + live-state spawn rows
unchanged) and `TestReconcileIPC_QuarantinedBystanderNotRevivedOnStop`
(two-row handleReconcile proof: target terminate posted, quarantined bystander
`needs_manual_review`, AppliedCount=1).

### (b.2) intent-CHANGE path — open / by design

What remains is the **intent-CHANGE** path only: when the operator restarts the
quarantined daemon ITSELF, `diffIntentSnapshots` DOES post `EvIntentUpdate` for
that changed entry, and the SM row
`StQuarantined + EvIntentUpdate(running) → reset` revives it. That stays as
designed — a deliberate restart of the quarantined daemon is gated upstream by
the api-side QUARANTINED refusal (the restart caller refuses to write a
running-intent the daemon did not earn; fable N1 force-gate on PR #279), and a
force respawn (`EvManualRestart` via `handleRespawn` force=true) plus
`install --upgrade --reset-failure-windows` are the SANCTIONED un-quarantine
levers. The SM row resetting on a deliberate intent flip is the intended
behavior for those sanctioned paths; only the UNINTENDED bystander revival on
an unrelated stop/restart was a bug, and that is now closed at (b.1).

**Action (parked, narrow):** if a future invariant wants "quarantine survives
even a deliberate restart of the daemon itself", that must be designed at the
SM / `IntentWatcher` post layer (the `StQuarantined + EvIntentUpdate(running)`
transition), not the reconcile classifier — the classifier no longer sees the
intent-CHANGE bystander case because the bystander gate already covers the
untouched-row path. No live bug today: the api-side force-gate caller discipline
covers the restart-caller surface.
