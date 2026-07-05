# Plan — Phase A.1: STOP supervisor-aware (spec §4, branch fix/stop-supervisor-aware)

Spec: docs/superpowers/specs/2026-06-10-clean-architecture-redesign.md §4.
Base: master 2c7c343 (post-#278). Branch created: fix/stop-supervisor-aware.

## Evidence (verified this session)

- ROOT (spec §4 confirmed in code): `Stop` (install.go:2200) → recordStopIntent →
  stopKillCore (:2217) → killDaemonByPort (taskkill /F /T = NON-clean exit) +
  sch.Stop (no-op for supervisor-owned; no scheduler row). Supervisor reaper sees
  non-clean exit → respawns; 60s IntentWatcher hasn't read the stop yet → churn →
  quarantine. StopAll (:2545) same, and records NO intent at all in its body.
  StopWithOpts (install_intent.go:178) shares stopKillCore.
- Restart is the working mirror: `restartSupervisorOwnedDaemons`
  (restart_supervisor.go:21) reads supervisor-intent, filters (skip maintenance
  rows via isMaintenanceTaskName; match server/daemonFilter with
  ParseManagedTaskName fallback), per-target DialSupervisorIPCRespawn; Restart
  (install.go:2262) combines: supervisor pass first → schedulerBlockedRestartTaskNames
  → legacy loop skips handled names; schedulerUnavailableError tolerated when
  supervisorHandled.
- **DISCOVERY (gap vs spec §4 wording):** the IPC reconcile classifier
  (classifyDriftAction, supervise_reconcile_ipc.go:485) returns NO_OP for
  `!hasSched && intentDesired==stopped` — supervisor-owned daemons have NO
  scheduler row by design, so `reconcile --apply` today never posts the
  terminate-direction EvIntentUpdate for them. smState is already computed
  (lookupControllerSMState :220) but unused by classify. The spec assumed the
  verb already handled terminate; it does not. The orphan-gate's
  "terminate-direction left intact" comment refers to hasSched=true rows only.

## Design

1. **Classifier terminate-direction (supervise_reconcile_ipc.go):** pass smState
   into classifyDriftAction; for `!hasSched && supervisorOwned &&
   intentDesired==stopped && smState is live (StRunning/StSpawning/StBackoff...)`
   → ReconcileActionPostEvIntentUpdate (terminate). Dead/idle SM → no_op
   (quarantine-respected, same as startup reconciler's `isStopped && !running`).
   The existing orphan gate only downgrades the SPAWN direction — unaffected.
   Verify EvIntentUpdate on running+stopped-intent drives StRunning→StExiting→
   StIdle without respawn (api.Transition; applyReconcileDrift refreshes
   daemonIntent cache BEFORE posting — supervise_reconcile_ipc.go:544-576).
2. **stopSupervisorOwnedDaemons (new internal/api/stop_supervisor.go):** mirror
   restartSupervisorOwnedDaemons target selection EXACTLY (skip maintenance,
   server/daemonFilter match, normalize task names). If no targets → (nil,false,nil).
   Else ONE DialSupervisorIPCReconcile(ctx, apply=true) via a
   supervisorStopReconcileFn seam; success → per-target RestartResult rows (one
   reconcile covers all); IPC-unavailable (ErrSupervisorIPCUnavailable) →
   (nil,false,err-or-tolerate?) — DECISION: tolerate as not-handled (fall back to
   kill path) but only when errors.Is(ErrSupervisorIPCUnavailable); other errors
   propagate. Rationale: supervisor down = nothing will respawn; legacy kill is
   then correct and curing.
3. **Wire (mirror Restart's combine-and-skip):**
   - Stop (install.go:2200) + StopWithOpts (install_intent.go:178): after
     recordStopIntent (intent FIRST — the reconcile must read the fresh stop),
     call stopSupervisorOwnedDaemons; stopKillCore skips handled task names
     (new param or map, mirroring schedulerBlockedRestartTaskNames).
   - StopAll (install.go:2545): supervisor pass for ALL intent daemons
     (server="" filter) + legacy loop skip-handled. NOTE: StopAll records no
     daemon-intent today — without recordStopIntent the reconcile sees
     desired=running and won't terminate. DECISION: StopAll must recordStopIntent
     for supervisor-owned targets first (it's the spec's correctness requirement,
     not scope creep — without intent the IPC path cannot stop anything).
4. **Tests (internal/api):** seam-stub DialSupervisorIPCReconcile +
   supervisor-intent fixture (SetDaemonStateRootForTest): Stop on
   supervisor-owned → reconcile-apply called, killDaemonByPort NOT called for
   handled tasks (killByPortFn seam records); IPC-unavailable → falls back to
   kill path; mixed install → both passes; StopAll writes intent then reconciles.
   internal/cli classifier tests: terminate-direction post for supervisor-owned
   running SM + stopped intent; quarantined/idle → no_op; orphan spawn-gate
   untouched.
5. **Live verify after merge+redeploy:** `mcphub stop paper-search` (or GUI stop)
   → daemon stops, stays stopped (no respawn-churn), `mcphub status` shows
   stopped; restart brings it back.

## Post-discovery refinements (verified 2026-06-10 03:50)

- SM table (supervisor_state_machine.go Transition): EvIntentUpdate(stopped)
  stops correctly from EVERY live state — StRunning→StExiting ("issue
  terminate"), StSpawning→queued_action=stop, StBackoffWaiting→StIdle (cancel
  timer), StExiting→clear queued_action (cancels pending respawn);
  StIdle/StQuarantined stay put. So classifier live-set = {StSpawning,
  StRunning, StExiting, StBackoffWaiting}; StIdle/StQuarantined → no_op.
- classifyDriftAction gains an smState param (already computed by the caller
  at supervise_reconcile_ipc.go:220, currently unused by classify). Package-
  private, 1 caller.
- computeIntentDesired (supervise_reconcile_ipc.go:410) honors a stopped
  daemon-intent entry with canonical+bare key fallback — recordStopIntent's
  canonical write is read correctly.
- stopTaskNamesForServer (install_intent.go:540): global servers enumerate
  manifest daemons (`mcp-local-hub-{server}-{daemon}`) — matches supervisor-
  intent task names for migrated globals (paper-search etc.). Workspace-scoped
  servers enumerate LSP registry rows and SKIP @serena sentinel rows by design
  — serena-pool stop is a separate path (idle-shutdown #6 will own it); NOT
  A.1 scope.
- schedulerBlockedRestartTaskNames (install.go:2430) is reusable as the
  skip-set builder (drops only Code==SUPERVISOR_UNAVAILABLE rows).
- DRY: extract the target-selection loop from restartSupervisorOwnedDaemons
  (restart_supervisor.go:36-66) into a shared selectSupervisorOwnedTargets
  helper used by both restart and stop mirrors.
- stopSupervisorOwnedDaemons error polarity: ErrSupervisorIPCUnavailable →
  (nil, false, nil) — supervisor down means nothing respawns, legacy kill path
  is correct and curing. Reachable-but-failed reconcile → per-target error
  rows with handled=true — do NOT fall through to taskkill while a live
  supervisor would observe the non-clean exit and respawn (the exact churn
  this fix kills).

## Sequencing / gates

Implement → build/vet → targeted tests both tag modes → fable CLI review →
bot (quota may be exhausted until ~2026-06-11; user authorized subagent gate for
the PR-278 doc commit — for THIS PR ask again or wait) → merge → redeploy → live
paper-search verify (the original symptom).
