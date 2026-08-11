# Bug: Full CLI suite stalls in reconcile lifecycle tests

- id: 2026-08-10-cli-reconcile-broad-timeout
- context: adjacent-finding
- status: open
- severity: high
- area: internal/cli reconcile test lifecycle
- found-by: qa-engineer

`go test -count=1 -timeout 5m ./internal/cli` times out on both the candidate
and immutable `HEAD`. The exact reconcile test passes alone on both. The
specific sibling at timeout changes with package order, while an EventLoop and
state-read or intent-refresh path remain blocked. This is a pre-existing broad
test lifecycle defect and blocks the canonical regression gate.
