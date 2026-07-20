# Bug: internal/daemon has load-sensitive wall-clock tests that fail under parallel-agent machine load

- id: 2026-07-20-daemon-suite-load-sensitive-flakes
- context: adjacent-finding
- status: open
- severity: low
- area: internal/daemon (host_test.go, lazy_proxy_test.go)
- found-by: qa-engineer

## Reproduction

On a loaded host (operator fleet + multiple agents running), `go test -count=1 ./internal/daemon/` intermittently fails on wall-clock-budget tests. Observed 2026-07-20 during QA of `fix/serena-readiness-timeout-inversion` (worktree `agent-aeedb65694c7c5af4`, raw outputs preserved under `.scratch/qa-daemon-suite*.out`):

- `TestHostStopUnblocksSSE` (`host_test.go:668`) — "Stop did not unblock SSE handler quickly: took 4.26s / 4.76s / 6.19s" against a 4 s budget the test's own comment already calls "a defensive ceiling ... under heavy parallel test load".
- `TestLazyProxy_ColdForwardHeldEvent_FiresBeyondBudget` (`lazy_proxy_test.go:3012`) — "condition never met within deadline" (full-suite run only; 3/3 green isolated).
- `TestLazyProxy_DocLifecycle_ProbationDeadlineDeliversOnceNoReplay` (`lazy_proxy_test.go:2466`) — cold didOpen returned the 503 "cold start in progress" branch instead of 202-delivered: the MaterializeWaitBudget window expired under load (full-suite run only; 5/5 green isolated).

## Expected vs actual

Expected: budget tests pass regardless of ambient machine load (or engineer their window deterministically via an injection seam). Actual: budgets are real wall-clock ceilings racing ambient CPU contention.

## Not a regression of the serena readiness fix

Base-oracle evidence: with the fix's 4 changed files reverted to master `d8ab4777`, `TestHostStopUnblocksSSE` failed 4/5 consecutive runs (4.8–7.9 s) on the same host (`.scratch/qa-daemon-sse-base.out`). The failing files are untouched by that diff.

## Suggested direction (not implemented)

Per the race-window assertion discipline, these budgets should be engineered deterministically (injection seam for the stop/materialize latency) rather than widened again; widening the ceiling has already been done once for `TestHostStopUnblocksSSE` (2 s → 4 s) and load has outrun it.
