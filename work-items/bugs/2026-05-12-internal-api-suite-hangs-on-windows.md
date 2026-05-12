---
title: `go test ./internal/api/` hangs >4min on Windows under sweepLoop goroutine accumulation
severity: low
found-by: g4-phase4 pre-push verification
found-on: 2026-05-12
project: mcp-local-hub
context: pre-existing master flake (reproduces on `2943f00` BEFORE Phase 4)
status: open
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
