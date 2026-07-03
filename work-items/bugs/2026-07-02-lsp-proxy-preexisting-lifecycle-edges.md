# LSP lazy-proxy: two pre-existing lifecycle/refcount edges (backlog)

- **status:** fixed
- **severity:** low
- **filed:** 2026-07-02
- **fixed:** 2026-07-03 (branch `fix/lsp-preexisting-lifecycle-edges`, both edges)
- **context:** surfaced by the P2c-unified commission (fable+opus) as PRE-EXISTING (present on master before the unified refactor); deliberately NOT fixed in PR #489 to avoid scope creep.

## Edge 1 — transient Configured-stomp during the reserve→shadow-reset window

`internal/daemon/lazy_proxy.go` reapIdleBackend's post-unlock `PutLifecycle(Configured)` can land after a concurrent cold-path reserve's flock `Starting` write (reserve passed the `reaping` spin before the reap began). The row reads `Configured` for the ~30-45s materialize window → other proxies' cold-start gate / hard cap undercount by one. Self-heals at publish-time reconcile. Master had the same ordering. Fix direction: reset/reconcile the row under the flock inside reserve rather than after return, or gen-guard the idle-reap Configured write.

**Fixed 2026-07-03** (option b — gen-guard). `reapIdleBackend` now captures the generation it bumps and routes its terminal Configured write through the single-owner `reconcileRegistryLifecycleLocked(gen)` under `p.mu` instead of the out-of-band `PutLifecycle(Configured)`. Post-teardown the reconcile derives Configured (endpoint nil, not starting) and is gen-guarded: a materialize that republished an endpoint over the teardown bumped the generation again, so the stale reap's write no-ops rather than stomping the newer row back to Configured. This removes the last non-reconcile Configured writer (completing the #489 single-owner discipline) and keeps the `lastWrittenLifecycle` shadow consistent with the registry. `Stop()` still precedes the Configured write. Regression: `TestLazyProxy_IdleReap_StaleConfiguredWriteNoStompsNewerStarting` (fails on master with `lifecycle=configured`, passes with the gen guard).

## Edge 2 — warm-path didOpen/didClose rollback-after-delivery (refcount desync)

`handleDocLifecycle`: with `warmed=true` the forward runs under the raw client ctx; a delivered `didOpen` that stalls past the client deadline hits `isClientCancelErr` → `rollbackDocRef` even though the bytes were written to stdin → refcount desync (the next `didOpen` duplicates the upstream open). The delivered⇒keep-refcount logic (round-1 P2c fix) exists ONLY in the `!warmed` probation branch, not the warm path. Fix direction: apply the same delivered⇒keep-refcount classification to the warm notification path.

**Fixed 2026-07-03.** The SendRequest-error block in `handleDocLifecycle` now classifies BOTH delivered shapes as 202 + keep-refcount: `isProbationDeadline` (cold budget, `!warmed`) OR `isClientCancelErr` (client ctx canceled/deadlined after delivery — the warm path). This is sound because SendRPC writes stdin BEFORE awaiting and the endpoint's only context-error surface is that post-write select, so any context cancel/deadline error means the notification was already delivered. The pre-delivery failure path (a non-context send error while the client ctx is live) still rolls back the refcount and tears down. Residual: a client-cancel racing a broken-pipe write failure keeps a phantom refcount, but self-heals via the next real forward's `onSendFailure` → `resetDocRefs`. Regressions: `TestLazyProxy_DocLifecycle_WarmDeliveredThenClientCancel_KeepsRefcount` (fails on master with code 200/-32603, passes now) and `TestLazyProxy_DocLifecycle_WarmPreDeliveryFailure_RollsBackAndTearsDown` (guards the still-rolls-back failure path).

Both were low-severity, self-healing/bounded, and pre-dated the P2c work.
