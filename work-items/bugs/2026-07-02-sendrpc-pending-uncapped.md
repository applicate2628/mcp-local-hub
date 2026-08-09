# SendRPC registers pending requests with no maxPendingRequests cap

- **status:** fixed
- **fixed-by:** PR #493 (`8de5ed3a`) - `SendRPC` enforces `maxPendingRequests` and returns `ErrTooManyPending`.
- **HEAD reconciliation (2026-07-09):** Verified against master `63b6a008`; see `triage-2026-07-09.md` for code/test evidence.
- **fixed:** 2026-07-03 (branch fix/sendrpc-pending-cap) — SendRPC now enforces the same maxPendingRequests bound handlePOST uses, refused BEFORE the stdin write (pre-delivery, retry-safe). Regression: TestBackendLifecycle_SendRPCPendingCapRefusesPreDelivery (cap-refusal + drain-then-retry halves; negative-controlled).
- **severity:** low
- **filed:** 2026-07-02
- **context:** adjacent-finding (surfaced by the architect probation/delivery design, aa385669)

## Finding

`StdioHost.SendRPC` (`internal/daemon/backend_lifecycle.go:104-106`) registers a pending request in `h.pending` with NO `maxPendingRequests` bound, unlike `handlePOST` (`internal/daemon/host.go:992`) which enforces the cap. The P2c "await-after-delivery" model (decision `2026-07-02-lsp-probation-await-after-delivery.md`) holds first cold requests longer, raising concurrent pending count. A fan-out spike on a cold backend is therefore unbounded on the SendRPC path.

## Not blocking

Pre-existing (not introduced by P2c); the await-after-delivery change makes it marginally more reachable. Fix: apply the same `maxPendingRequests` bound in `SendRPC` that `handlePOST` uses. Low priority — a bound if fan-out grows.

## Caller-layer discrimination (fable pre-bot P1, same branch)

The raw cap refusal would have classified as a backend failure in the #492 error-shape model (non-context error → onSendFailure → teardown), killing every delivered in-flight request on a merely-saturated HEALTHY backend and re-entering the same fan-out cold — a self-inflicted outage loop strictly worse than the unbounded map. Fixed with a THIRD identity class: `ErrTooManyPending` sentinel (host.go), `%w`-wrapped by SendRPC, and an `errors.Is` branch in all three request handlers BEFORE the failure fall-through → retryable 503 + Retry-After, NO teardown (doc-lifecycle also rolls back its optimistic refcount — the refusal is pre-delivery). Caller-layer regression: `TestLazyProxy_ToolsCall_PendingCapSaturation_Retryable503NoTeardown` (negative-controlled: pre-fix 200/-32603 + teardown).
