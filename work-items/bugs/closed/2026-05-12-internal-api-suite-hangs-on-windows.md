---
title: `go test ./internal/api/` hangs >4min on Windows under sweepLoop goroutine accumulation
severity: low
found-by: g4-phase4 pre-push verification
found-on: 2026-05-12
project: mcp-local-hub
context: pre-existing master flake (reproduces on `2943f00` BEFORE Phase 4)
status: fixed
fixed-on: 2026-05-13
fixed-by: pre-tag-hygiene branch (post-G6 sub-PR 4 merge)
related-pr: pending Phase 4 (feat/g4-phase4-handler-listener-control)
---

# `go test ./internal/api/` times out on Windows under heavy goroutine accumulation

## Reproduction

On Windows, on master `2943f00` (Phase 3 merged):

```bash
go test -count=1 -timeout 4m -skip 'TestRegister_DefaultAllLanguages|TestMigrateLegacy|TestInstall_AuditFailsErrIdentityOversize_NoSchedulerMutation|TestToolCatalog_GoldenAgainstUpstream' ./internal/api/
```

times out at 240+ seconds. The same suite passes on a Linux CI runner.

Stack trace at timeout shows multiple `HubSessionStore.sweepLoop`
goroutines parked on the sweep ticker (each Phase 3 test that creates
a session store starts one and closes it via `t.Cleanup`, but
goroutine teardown isn't synchronous on Windows the way Linux+Go
expects).

The hanging test varies between runs:
- `TestRegister_NoWeeklyRefreshOpt` (hangs in `WriteDaemonIntent` →
  `parseAndValidateIntent` → `isUTCInstant` on a 1.5 MB on-disk
  intent file)
- `TestUnregister_FullRemovesAllLanguages` (similar path)

## Why this is pre-existing

Reproduced on master at `2943f00` with NO Phase 4 changes applied.
The test suite has slowly accumulated state-leaking tests across
phases; the Windows scheduler's goroutine teardown latency tips it
over the 4-minute boundary.

## Why filing as adjacent finding

- Phase 4 PR adds 5 new test files (handler, internal_reload,
  listener, e2e, gui/hub_listener) all of which call
  `t.Cleanup(store.Close)` on every `NewHubSessionStore` (verified
  by grep). The Phase 4 surface tests themselves PASS in well under
  10s on their own.
- The hang is pre-existing infrastructure noise. Phase 4 is not the
  root cause.
- Linux CI is unaffected (the bot review on PR #157 passed cleanly).

## Suggested fix

Two options:
1. Make `HubSessionStore.Close()` synchronously wait for the
   sweepLoop goroutine to exit (use a done channel inside sweepLoop
   that Close closes-and-waits on). Currently `Close()` calls
   `sweepStop()` (a context cancel) but doesn't block on goroutine
   exit. Goroutines that finished testing accumulate until the test
   binary terminates — on Windows this is slow.
2. Identify and rewrite the test or tests in `register_test.go` /
   `daemon_intent_test.go` that produce a 1.5 MB on-disk intent file
   under state-leak conditions. Reduce the buffer or short-circuit
   `isUTCInstant` for tests.

## Related code

- `internal/api/hub_mcp_session.go:271` — `go s.sweepLoop()` (start)
- `internal/api/hub_mcp_session.go:279-283` — `Close()` (no wait)
- `internal/api/hub_mcp_session.go:439-450` — `sweepLoop` body
- `internal/api/daemon_intent.go:592-656` — `parseAndValidateIntent`
  / `isUTCInstant`
- `internal/api/daemon_intent.go:745+` — `WriteDaemonIntent`

## Workaround for Phase 4 PR

CI runs on Linux (where this doesn't reproduce). Local pre-push
verification on Windows skips the affected tests:

```bash
go test -count=1 -timeout 5m -skip 'TestRegister_DefaultAllLanguages|TestMigrateLegacy|TestInstall_AuditFailsErrIdentityOversize_NoSchedulerMutation|TestToolCatalog_GoldenAgainstUpstream|TestRegister_NoWeeklyRefreshOpt|TestUnregister_FullRemovesAllLanguages' ./internal/api/
```

(Or run targeted Phase 4 test patterns only.)

The Phase 4 implementation is verified independently via `go test
-race ./internal/api/ -run 'TestNewListener|TestHubMcpHandler|TestInternalReload|TestHubMcp|TestHubEndpoint|TestHubListener|TestStartHubMcp'` which completes cleanly in <4 seconds.

## Resolution (2026-05-13)

Both Option 1 and Option 2 from "Suggested fix" were applied:

1. **HubSessionStore.Close synchronously waits for sweepLoop exit.**
   Added `sweepDone chan struct{}` to the struct, `defer close(s.sweepDone)`
   inside sweepLoop, and `<-s.sweepDone` in Close after sweepStop.
   Wrapped in `sync.Once` so Close stays idempotent and only the first
   call blocks. Tests that forget Close still leak goroutines, but
   tests that DO call Close (the vast majority) now release them
   synchronously, eliminating accumulation across the suite.

2. **parseAndValidateIntent: O(N²) → O(N+M).** `isUTCInstant` ignored
   its `taskName` parameter and scanned the entire raw buffer for
   every `"updated_at":"..."` occurrence. Calling it once per task in
   a loop made the parse cost grow with both task count and file
   size. Hoisted the call out of the loop — same defense-in-depth
   against decoder normalization, one scan instead of N.

Full `go test -count=1 -timeout 5m ./internal/api/` now passes in
~35s on Windows (was: timed out at 300s).
