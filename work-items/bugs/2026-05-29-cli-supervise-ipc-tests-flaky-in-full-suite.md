---
title: cli supervise IPC tests fail in full-package `go test ./internal/cli/` runs but pass in isolation
severity: low
found-by: backend-engineer
found-in-phase: D.3b-2 (migrate serena legacy-to-dynamic-pool)
affected-surface: internal/cli/supervise_test.go
context: adjacent-finding
status: open
related-pr: PR #264 (914d0cf)
---

## Reopened 2026-07-18 — DISTINCT residual mechanism (TempDir cleanup handle-race)

PR #264 fixed the PIPE-CONTENTION mechanism (per-test pipe isolation → the
`sidecar never appeared` poll-timeout no longer fires). A DIFFERENT residual
surfaces on the SAME supervise-IPC test group under full-suite load: the test
FAILS at Go's `t.TempDir` auto-cleanup, not at an IPC assertion:

```
testing.go:1464: TempDir RemoveAll cleanup: unlinkat
  R:\Temp\Test..._IPC_...\001\hardened-parent: The directory is not empty.
```

Observed 2026-07-18 across two full `go test -tags=test_state_path_env
./internal/cli/` runs; a DIFFERENT subset of the six IPC tests failed each run
(run 1: `TestSupervise_IPC_VersionPinning`; run 2:
`TestSuperviseCommand_StatusIPC_UnknownCommand` + `TestSupervise_IPC_QuiesceTimersTwoFrames`).
All pass in isolation. Non-deterministic, load-dependent.

**Root cause (test-harness, NOT production):** each IPC test spawns a real
`mcphub.exe supervise` child under the test's `t.TempDir()`. On Windows a
directory with a live open handle cannot be removed, so when the test returns
and Go runs `RemoveAll` on the TempDir, a supervisor child (or its Job Object /
an open state-file handle under `hardened-parent`) that has not FULLY exited yet
keeps the dir busy → cleanup `unlinkat ... directory not empty`. The IPC logic
itself is correct (isolation passes); this is spawn/exit/handle-release timing.

**Fix direction:** before each IPC test returns, deterministically terminate its
spawned supervisor AND wait for handle release (poll until the child PID is gone
and the state-file handles are closed) before `t.TempDir` cleanup runs — e.g. an
explicit `t.Cleanup` that kills the child and retries the dir removal with a
bounded backoff, or route the spawn through a helper that guarantees reap-before-return.

Unrelated to the hub-reconcile / adopt / de-adopt work; found as a side-effect
while gating PR #562 (hub adversarial-rotation). Severity stays LOW (test-harness
only, no production impact) but it now reds full-cli runs more often than the
intermittent #264-era flake did, so it is worth a harness fix.

## Status

CLOSED — fixed by PR #264 (`914d0cf`, merged 2026-06-03). The full-suite
contention was the in-process supervisors all binding the same per-user-SID
Windows pipe `\\.\pipe\mcphub-supervisor-<SID>`. PR #264 adds a runtime
per-test pipe discriminator (`EnableSupervisorIPCTestPipeIsolation`, installed
by the internal/cli TestMain) deriving a unique pipe leaf from each test's
`MCPHUB_STATE_DIR_OVERRIDE`, so the six named IPC tests no longer collide. The
hook is active in both tagged and untagged test builds. The
`EnableSupervisorIPCTestPipeIsolation` symbol IS compiled into release binaries
(it is an untagged exported function with a POSIX counterpart), but no
production path calls it, so the discriminator var stays nil and
`SupervisorIPCAddress` always returns the per-SID pipe in release. The fixed
condition is "no production caller/assignment," NOT symbol absence (codex
bot #264 P2 r1+r2; wording corrected per #265 r1). Verified 2026-06-03:
`TestSupervise_IPC_VersionPinning` + `TestSuperviseCommand_StatusIPC_ReconcileReady`
pass in both build modes.

The original report's frontmatter `affected-surface` listed
`relay_test.go (TestResolveRelayURL_ResolvesFromEmbeddedManifest)` — now removed
from the metadata. That was a SEPARATE baseline observation, not the
IPC-contention defect, and #264 does not touch it. It is NOT a code bug: the test resolves the `unified` daemon from the
embedded serena manifest, which the committed manifest provides, so it PASSES
on a clean HEAD. It fails ONLY when the working-tree
`servers/serena/manifest.yaml` is dirtied to drop the `unified` daemon (e.g. a
mid-edit revert to the claude+codex 2-daemon layout). Closing this record
therefore hides no code defect — there is nothing separate to track; the relay
line was a dirty-tree artifact, explicitly dropped here (verified 2026-06-03:
passes against committed HEAD, fails only under a local manifest WIP).

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
- ~~internal/cli/relay_test.go — `TestResolveRelayURL_ResolvesFromEmbeddedManifest`~~
  (embedded-manifest read; baseline-only failure tied to the dirty serena
  manifest WIP) — **DROPPED at closure**: not part of this IPC-contention bug
  and not a code defect (passes on clean HEAD); see `## Status`. Removed from
  the frontmatter `affected-surface` too.

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
