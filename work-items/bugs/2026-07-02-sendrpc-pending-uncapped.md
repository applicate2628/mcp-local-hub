# SendRPC registers pending requests with no maxPendingRequests cap

- **status:** fixed
- **fixed:** 2026-07-03 (branch fix/sendrpc-pending-cap) — SendRPC now enforces the same maxPendingRequests bound handlePOST uses, refused BEFORE the stdin write (pre-delivery, retry-safe). Regression: TestBackendLifecycle_SendRPCPendingCapRefusesPreDelivery (cap-refusal + drain-then-retry halves; negative-controlled).
- **severity:** low
- **filed:** 2026-07-02
- **context:** adjacent-finding (surfaced by the architect probation/delivery design, aa385669)

## Finding

`StdioHost.SendRPC` (`internal/daemon/backend_lifecycle.go:104-106`) registers a pending request in `h.pending` with NO `maxPendingRequests` bound, unlike `handlePOST` (`internal/daemon/host.go:992`) which enforces the cap. The P2c "await-after-delivery" model (decision `2026-07-02-lsp-probation-await-after-delivery.md`) holds first cold requests longer, raising concurrent pending count. A fan-out spike on a cold backend is therefore unbounded on the SendRPC path.

## Not blocking

Pre-existing (not introduced by P2c); the await-after-delivery change makes it marginally more reachable. Fix: apply the same `maxPendingRequests` bound in `SendRPC` that `handlePOST` uses. Low priority — a bound if fan-out grows.
