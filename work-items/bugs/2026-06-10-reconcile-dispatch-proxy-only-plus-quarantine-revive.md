# Reconcile synchronous dispatch is proxy-only; quarantine-revive on running intent

- **Status:** open / deferred (two parked design items — neither blocking)
- **Date:** 2026-06-10
- **Severity:** P3 / design-boundary (no live data-loss; behavior is durable and converges)
- **Context:** adjacent-finding
- **Source:** the opus merge-gate on PR #279 (HEAD eaf7a94) + the fable r4 review of the same branch. The opus gate's honesty finding is FIXED in #279 (the api-side stop/restart paths now report watcher-deferred rows via the typed `DeferredToIntentWatcherCode` per the per-target drift `Action`). The two items below are the parked design boundaries that fix surfaces but does NOT close.
- **Cross-link:** [`docs/superpowers/specs/2026-06-10-clean-architecture-redesign.md`](../../docs/superpowers/specs/2026-06-10-clean-architecture-redesign.md) — Phase B/F.

## Item (a) — synchronous SM dispatch covers ONLY proxy-shaped descriptors

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

**Guard pinning the parked boundary:** `internal/cli`'s
`TestReconcileIPC_RegularGlobalDaemonNotDispatchedThroughSM` asserts a
regular `daemon --server foo --daemon default` descriptor yields `no_op`
(terminate, live SM) / `needs_manual_review` (spawn, missing scheduler) and
`AppliedCount=0` with no `EvIntentUpdate` posted. A future classifier
broadening cannot silently regress this boundary without flipping that test.

## Item (b) — fable r4 O1: quarantine-revive on running/absent intent (pre-existing SM row)

The spawn direction of `classifyDriftAction` — and equally the
`IntentWatcher`'s `EvIntentUpdate` path — drives a `StQuarantined` daemon back
to `StSpawning` **with failures RESET** when intent is running (or absent →
default running). This is a **pre-existing spec-v13 SM row**, not introduced
by #279: `internal/api/supervisor_state_machine.go:204-206` —
`StQuarantined` + `EvIntentUpdate` with `IntentDesired == "running"` →
`StSpawning, "reset failures, bump pid_generation, create-process"` (the
`EvManualRestart` row at :207-208 does the same).

**Why it matters:** any future gate intended to keep a quarantined daemon
quarantined across a stop/start cycle (e.g. "do not let a plain restart bypass
quarantine") must be designed at the **SM / IntentWatcher level**, NOT at the
reconcile classifier. The reconcile classifier already respects quarantine on
the terminate direction (`smStateIsLive` excludes `StQuarantined`, so a
stopped-intent reconcile of a quarantined daemon is `no_op`) and on the
apply-mode orphan gate, but the spawn direction (running intent) deliberately
revives via this SM row — and the 60s `IntentWatcher` posts the SAME
`EvIntentUpdate(running)` independent of the classifier, so gating only the
classifier would leave the watcher path open. The force-gate work in #279
(fable N1) gates the restart caller's intent WRITE on the typed refusal code
so a quarantined daemon never receives a running-intent it did not earn — but
that is the api-side caller discipline; the SM row itself still revives on any
running intent that reaches it.

**Action (parked):** if a "quarantine survives restart" invariant is desired,
design it as an SM-state-aware gate on the `StQuarantined + EvIntentUpdate
(running)` transition AND on the `IntentWatcher` post path, not as a
classifier tweak. No live bug today (the current force-gate caller discipline
covers the restart-caller surface); recorded so a future gate is not built at
the wrong layer.
