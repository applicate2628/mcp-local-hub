---
title: cli supervise IPC tests fail in full-package `go test ./internal/cli/` runs but pass in isolation
severity: low
found-by: backend-engineer
found-in-phase: D.3b-2 (migrate serena legacy-to-dynamic-pool)
affected-surface: internal/cli/supervise_test.go, internal/cli/relay_test.go (TestResolveRelayURL_ResolvesFromEmbeddedManifest)
context: adjacent-finding
status: closed
related-pr: PR #264 (914d0cf)
---

## Status

CLOSED — fixed by PR #264 (`914d0cf`, merged 2026-06-03). The full-suite
contention was the in-process supervisors all binding the same per-user-SID
Windows pipe `\\.\pipe\mcphub-supervisor-<SID>`. PR #264 adds a runtime
per-test pipe discriminator (`EnableSupervisorIPCTestPipeIsolation`, installed
by the internal/cli TestMain) deriving a unique pipe leaf from each test's
`MCPHUB_STATE_DIR_OVERRIDE`, so the six named IPC tests no longer collide. The
hook is active in both tagged and untagged test builds yet absent from release
binaries — structurally test-only, since no production path assigns the var
(codex bot #264 P2 r1+r2). Verified 2026-06-03:
`TestSupervise_IPC_VersionPinning` + `TestSuperviseCommand_StatusIPC_ReconcileReady`
pass in both build modes.

## Reproduction

1. `go test -count=1 -timeout 8m ./internal/cli/` from a clean dev tree.
2. Observe these tests FAIL with `supervisor.lock.owner.json never appeared` /
   `sidecar never appeared` after a 3s wait:
   - `TestSuperviseCommand_StatusIPC_ReconcileReady`
   - `TestSuperviseCommand_StatusIPC_UnknownCommand`
   - `TestSupervise_IPC_QuiesceTimersTwoFrames`
   - `TestSupervise_IPC_QuiesceTimersDrainTimeout`
   - `TestSupervise_IPC_ExitGracefulInitiates`
   - `TestSupervise_IPC_VersionPinning`
3. Run any one of them in isolation
   (`go test -run TestSuperviseCommand_StatusIPC_ReconcileReady ./internal/cli/`)
   → PASSES in ~0.06s.

## Expected vs actual

**Expected:** the cli supervise IPC tests are hermetic and pass in both the
full-package run and in isolation.

**Actual:** they pass in isolation but fail in the full-package run. The
failures are a timing/contention artifact: each test spawns a real
`mcphub.exe supervise` child that binds a per-user named pipe and writes a
`supervisor.lock` + `supervisor.lock.owner.json` sidecar. When the full
~100-test cli package runs (including the GUI e2e-style tests and the
new migrate-serena tests, all of which also spawn `mcphub.exe`), the
supervisor child's lock-sidecar write does not appear within the test's
3s poll window, so the assertion times out.

A second, related full-suite-only failure observed at baseline:
`TestResolveRelayURL_ResolvesFromEmbeddedManifest` — sensitive to the
embedded serena manifest read; it failed at baseline (with the
pre-existing dirty `servers/serena/manifest.yaml` WIP in the tree) and is
unrelated to the migrate change.

## Proof it is pre-existing (not caused by D.3b-2)

- Stashed the D.3b-2 changes (`internal/api/manifest_source.go`,
  `internal/cli/migrate.go`, plus the new `internal/cli/migrate_serena*.go`
  moved aside) → ran `go test ./internal/cli/` at baseline → the SAME 6
  supervise IPC tests fail (plus the relay test).
- Each supervise IPC test passes in isolation BOTH with and without the
  D.3b-2 changes.
- The D.3b-2 change touches only the `migrate` command path; it adds no
  supervise/IPC/lock/state-file code.

## Risk

Local dev + CI full-package runs. On CI the cli package E2E job is
Windows-only and the IPC tests already run there; this flakiness could
intermittently red a CI run that executes the whole cli package in one
`go test` invocation. No production-runtime impact (the flakiness is in
the test harness's spawn/poll timing, not in the supervisor IPC code
itself).

## Files involved

- internal/cli/supervise_test.go:241,383,903,979,1050,1226 — the
  `supervisor.lock.owner.json` / sidecar poll-wait assertions.
- internal/cli/relay_test.go — `TestResolveRelayURL_ResolvesFromEmbeddedManifest`
  (embedded-manifest read; baseline-only failure tied to the dirty serena
  manifest WIP).

## Suggested fix

- Serialize the supervise IPC tests (a shared mutex or `-p 1` for that
  group) OR widen the sidecar poll deadline and add a unique per-test
  pipe-name namespace so concurrent supervisor children do not contend.
- Investigate whether the per-user `supervisor.lock` pipe name is
  test-isolated; if multiple test supervisors share one pipe name under
  the same `MCPHUB_STATE_DIR_OVERRIDE`-derived namespace, that is the
  contention root.

## Severity rationale

Low: the underlying supervisor IPC code is correct (tests pass in
isolation); the defect is test-harness spawn/poll contention in
full-package runs. Out of scope for D.3b-2 (migrate command), which is
hermetic and introduced zero new failures.
