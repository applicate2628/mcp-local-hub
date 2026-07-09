---
id: 2026-07-09-e2e-lazy-register-stale-vs-failloud-status-seam
severity: medium
area: internal/e2e/lazy_register_test.go (Status assertion) vs internal/api/health.go (fail-loud IPC status seam)
found-by: $lead, during the pre-release full-suite gate for the 0.4.24 bump
---

- **status:** resolved
- **not a regression:** reproduces identically at `f7eaa1c8` (the commit before PR #524 and PR #525
  merged), verified by running the single test in a detached worktree at that commit.

## Symptom

`go test ./internal/e2e/ -run TestE2E_LazyRegisterFullLifecycle` fails on Windows:

```
lazy_register_test.go:333: Status: supervisor unreachable — restart the hub
```

The whole `internal/e2e` package was therefore red in the canonical pre-push gate
(`go test -count=1 ./...`).

## Diagnosis — the test is stale, the code is correct

`internal/api/health.go` deliberately fails loud: when the package-level status seam
`SupervisorIPCStatusFn` is non-nil and the supervisor IPC is unreachable, `computeDaemonsSection`
converts `ErrSupervisorIPCUnavailable` into `ErrSupervisorDown` (health.go ~438-440) and refuses to
fall back to the legacy scheduler scan. The comment there records why: the silent fallback painted
migrated daemons — whose `\mcp-local-hub-*` tasks no longer exist — as failed/Restarting even while the
supervisor-owned process served verified traffic. Removing that false negative was the point of v0.6
Workstream B §3.1.

The test predates the current Status contract. It sets `MCPHUB_E2E_SCHEDULER=none` and then calls
`a.Status()`, and its own comment at the failing site still says *"Status is best-effort on non-Windows
hosts"* — the best-effort behavior that was removed. `API.Status` uses its separate, always-wired
`statusInternalDialFn` route in `internal/api/install.go`, so the absent supervisor IPC correctly
returns `ErrSupervisorDown`.

`internal/api/health.go` was last touched by #510; neither #524 (`22c91cab`) nor #525 (`5d8ab063`)
touched it, and `internal/e2e/lazy_register_test.go` was last touched by #462.

## Verified seam wiring and resolution

The isolated e2e test logs `api.SupervisorIPCStatusFn == nil` at the failing `a.Status()` call. The
package has one test, imports neither `internal/cli` nor `internal/gui`, and has no assignment to the
health seam; there is no test-order-dependent leak. The original failure did not prove that the health
seam was wired because `API.Status` does not read it.

`TestE2E_LazyRegisterFullLifecycle` now asserts
`errors.Is(err, api.ErrSupervisorDown)` and documents that Status must fail loud when its IPC-backed
daemon-state source is unavailable. The lifecycle transition remains verified directly through the
registry before this assertion. `internal/api/health.go` remains unchanged.

## Release impact

None for the 0.4.24 bump: the failure is pre-existing and unrelated to the shipped changes. The other
red package, `internal/cli` (`TestForce_*`, `TestGuiResetPortClearsPortKeepsInstanceID`), is the known
host-environmental class — this machine's `R:\Temp` grants `S-1-5-11` (Authenticated Users), so the
owner-only state-file DACL allowlist correctly refuses `gui.pidport`. That one is the environment, not
the code.
