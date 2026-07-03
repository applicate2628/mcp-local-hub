# LSP lazy-proxy: two pre-existing lifecycle/refcount edges (backlog)

- **status:** fixed
- **severity:** low
- **filed:** 2026-07-02
- **fixed:** 2026-07-03 (branch `fix/lsp-preexisting-lifecycle-edges`, both edges)
- **context:** surfaced by the P2c-unified commission (fable+opus) as PRE-EXISTING (present on master before the unified refactor); deliberately NOT fixed in PR #489 to avoid scope creep.

## Edge 1 — transient Configured-stomp during the reserve→shadow-reset window

`internal/daemon/lazy_proxy.go` reapIdleBackend's post-unlock `PutLifecycle(Configured)` can land after a concurrent cold-path reserve's flock `Starting` write (reserve passed the `reaping` spin before the reap began). The row reads `Configured` for the ~30-45s materialize window → other proxies' cold-start gate / hard cap undercount by one. Self-heals at publish-time reconcile. Master had the same ordering. Fix direction: reset/reconcile the row under the flock inside reserve rather than after return, or gen-guard the idle-reap Configured write.

**Fixed 2026-07-03 (NARROWED, disclosed residual — post-review corrected attribution).** `reapIdleBackend` now routes its terminal Configured write through the single-owner `reconcileRegistryLifecycleLocked(gen)` under `p.mu` instead of the out-of-band `PutLifecycle(Configured)`. Two DISTINCT protections (a fable xhigh pre-bot review corrected the initial misattribution):

- **The gen guard's real production trigger is a concurrent proxy `Stop()` mid-reap** — every other generation bumper (publish, wedge-reap, send-failure) refuses while `p.reaping` is held, so "a materialize republished over the teardown" cannot actually race this call site; a concurrent `Stop()` can, and the stale reap's write then no-ops.
- **The documented reserve race** (a cold-path reserve's flock `Starting` write landing before this reconcile — the original stomp) is mitigated by the reconcile DERIVING `Starting` from `startingSince`: once `markStartingReserved` has run, the reap's write preserves Starting. **RESIDUAL:** a reserve that has written its flock Starting but not yet returned to `markStartingReserved` leaves a microseconds-wide gap where the reconcile still derives Configured — narrowed from the old ~`Stop()`-duration window, NOT fully closed; reachability needs the reserve to stall ≥ IdleBackendTTL between its `reaping`-spin break and its flock write (same exotic class as the original bug); self-heals at publish-time reconcile.

This removes the last non-reconcile RUNNING-lifecycle Configured writer (the pre-concurrency startup seeds remain, gen-0 before Serve) and keeps the `lastWrittenLifecycle` shadow consistent with the registry. `Stop()` still precedes the Configured write. Regression: `TestLazyProxy_IdleReap_StaleConfiguredWriteNoStompsNewerStarting` (constructs the superseded-gen state directly; fails on master with `lifecycle=configured`).

## Edge 2 — warm-path didOpen/didClose rollback-after-delivery (refcount desync)

`handleDocLifecycle`: with `warmed=true` the forward runs under the raw client ctx; a delivered `didOpen` that stalls past the client deadline hits `isClientCancelErr` → `rollbackDocRef` even though the bytes were written to stdin → refcount desync (the next `didOpen` duplicates the upstream open). The delivered⇒keep-refcount logic (round-1 P2c fix) exists ONLY in the `!warmed` probation branch, not the warm path. Fix direction: apply the same delivered⇒keep-refcount classification to the warm notification path.

**Fixed 2026-07-03.** The SendRequest-error block in `handleDocLifecycle` now classifies BOTH delivered shapes as 202 + keep-refcount: `isProbationDeadline` (cold budget, `!warmed`) OR `isClientCancelErr` (client ctx canceled/deadlined after delivery — the warm path). This is sound because SendRPC writes stdin BEFORE awaiting and the endpoint's only context-error surface is that post-write select, so any context cancel/deadline error means the notification was already delivered. The pre-delivery failure path (a non-context send error while the client ctx is live) still rolls back the refcount and tears down. Residual: a client-cancel racing a broken-pipe write failure keeps a phantom refcount, but self-heals via the next real forward's `onSendFailure` → `resetDocRefs`. Regressions: `TestLazyProxy_DocLifecycle_WarmDeliveredThenClientCancel_KeepsRefcount` (fails on master with code 200/-32603, passes now) and `TestLazyProxy_DocLifecycle_WarmPreDeliveryFailure_RollsBackAndTearsDown` (guards the still-rolls-back failure path).

Both were low-severity, self-healing/bounded, and pre-dated the P2c work.
