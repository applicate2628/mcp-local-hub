---
title: api_surfaces_test.go cleanup races with goroutine spawned by StatusContext/RestartContext
severity: low
found-by: g4-phase3 backend engineer (race detector self-falsification pass)
found-on: 2026-05-12
project: mcp-local-hub
context: adjacent-finding
status: closed
closed-on: 2026-06-14
related-pr: pending Phase 3 (feat/g4-phase3-resolver-sessions-aggregator)
---

# Pre-existing data race in TestStatusContext_RespectsCtxCancellation + RestartContext sibling

## Reproduction

```bash
go test -race -count=1 -run "TestStatusContext_RespectsCtxCancellation|TestRestartContext_RespectsCtxCancellation" ./internal/api/
```

Both tests fail with `WARNING: DATA RACE` reports:

- Read at `api_surfaces.go:464` (inside the goroutine `StatusContext`
  spawns to honor ctx cancellation) vs write at
  `api_surfaces_test.go:104` (the deferred t.Cleanup function that
  restores `getStatusFn`).
- Same shape for `RestartContext` at `api_surfaces.go:502` vs
  `api_surfaces_test.go:112`.

## Why this is pre-existing

The `installTestStatusFn` / `installTestRestartFn` helpers swap a
package-level function pointer, then restore it via `t.Cleanup`.
`StatusContext`/`RestartContext` reads that same pointer from inside
a goroutine spawned under ctx-watch (best-effort cancellation per
CLAUDE.md Watchdog section — "ctx cancellation returns to caller
within ~10ms, underlying op continues until completes").

The race is real but benign in production: the goroutine read happens
before `t.Cleanup` fires (the cancellation goroutine is racing only
with test teardown, not with another live request). It's a test-only
race, NOT a production correctness issue.

## Why filing as adjacent finding

- Phase 3 deliverable (resolver + sessions + aggregator) does not
  touch `api_surfaces.go` or `api_surfaces_test.go`.
- Phase 3's own additions (`hub_mcp_request_id.go`,
  `hub_mcp_resolver.go`, `hub_mcp_session.go`, `hub_mcp_aggregator.go`
  and their tests) are race-clean under `-race` when run with a
  scoped `-run` filter.
- Fixing this race correctly likely requires storing
  `getStatusFn` / `getRestartFn` behind a sync.Mutex or an
  atomic.Pointer rather than a plain function pointer, and reworking
  the goroutine-spawn pattern. Out of scope for the phase.

## Suggested fix

Move the test fn-pointer swap and the production read into
`atomic.Pointer[func(...)]` so the cleanup write happens-before the
goroutine read. Same shape as how Phase 3 uses
`atomic.Pointer[ResolverSnapshot]`.

## Related code

- `internal/api/api_surfaces.go:461-510` — `StatusContext` and
  `RestartContext` goroutine-spawn pattern
- `internal/api/api_surfaces_test.go:104-112` — test helpers
- `internal/api/api_surfaces_test.go:159, 196` — failing test cases

## Closure (2026-06-14)

CLOSED — adversarially re-verified (refute-default skeptic) as FULLY fixed at
HEAD; residual hunted and not found.

FIXED: both `StatusContext` and `RestartContext` snapshot the package
function-pointer into a LOCAL variable BEFORE spawning the ctx-watch goroutine
(`internal/api/api_surfaces.go:119` and `:160`), so the goroutine reads its own
captured local rather than the package-level pointer that `t.Cleanup` restores.
The happens-before hazard the race detector flagged is gone — the test's
fn-pointer restore can no longer race the goroutine read because the goroutine
never touches the package-level pointer. An adversarial sweep of the
`api_surfaces` goroutine-spawn sites confirmed NO other goroutine in that file
reads a package-level function pointer.

Tests at HEAD (confirmed to exist and exercise the fix under `-race`):

- `TestStatusContext_RespectsCtxCancellation` (`internal/api/api_surfaces_test.go:169`).
- `TestRestartContext_RespectsCtxCancellation` (`internal/api/api_surfaces_test.go:207`).

Doc moved to `work-items/bugs/closed/` per repo convention.
