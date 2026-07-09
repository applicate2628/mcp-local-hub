---
id: 2026-07-09-e2e-lazy-register-stale-vs-failloud-status-seam
severity: medium
area: internal/e2e/lazy_register_test.go (Status assertion) vs internal/api/health.go (fail-loud IPC status seam)
found-by: $lead, during the pre-release full-suite gate for the 0.4.24 bump
---

- **status:** open
- **not a regression:** reproduces identically at `f7eaa1c8` (the commit before PR #524 and PR #525
  merged), verified by running the single test in a detached worktree at that commit.

## Symptom

`go test ./internal/e2e/ -run TestE2E_LazyRegisterFullLifecycle` fails on Windows:

```
lazy_register_test.go:333: Status: supervisor unreachable — restart the hub
```

The whole `internal/e2e` package is therefore red in the canonical pre-push gate
(`go test -count=1 ./...`).

## Diagnosis — the test is stale, the code is correct

`internal/api/health.go` deliberately fails loud: when the package-level status seam
`SupervisorIPCStatusFn` is non-nil and the supervisor IPC is unreachable, `computeDaemonsSection`
converts `ErrSupervisorIPCUnavailable` into `ErrSupervisorDown` (health.go ~438-440) and refuses to
fall back to the legacy scheduler scan. The comment there records why: the silent fallback painted
migrated daemons — whose `\mcp-local-hub-*` tasks no longer exist — as failed/Restarting even while the
supervisor-owned process served verified traffic. Removing that false negative was the point of v0.6
Workstream B §3.1.

The test predates that contract. It sets `MCPHUB_E2E_SCHEDULER=none` and then calls `a.Status()`, and
its own comment at the failing site still says *"Status is best-effort on non-Windows hosts"* — the
best-effort behavior the fail-loud seam replaced. It asserts the old contract.

`internal/api/health.go` was last touched by #510; neither #524 (`22c91cab`) nor #525 (`5d8ab063`)
touched it, and `internal/e2e/lazy_register_test.go` was last touched by #462.

## Open question to settle before fixing

The isolated single-test run also fails, so `SupervisorIPCStatusFn` is non-nil in that run. Establish
**who wires it** for the `internal/e2e` test binary (a package init, an import side effect, or another
test in the package that sets it without a `t.Cleanup` restore). The seam's own doc-comment states the
contract is "tests can swap it and restore via `t.Cleanup`" — if some test sets it and does not restore,
that is a second defect and the leak would make this failure order-dependent.

## Fix sketch (do not guess — verify the wiring first)

Either:
1. the test stubs `SupervisorIPCStatusFn` with a fake returning the rows it wants, restoring via
   `t.Cleanup`; or
2. the test asserts the fail-loud contract (`errors.Is(err, api.ErrSupervisorDown)`) instead of
   expecting rows, and drops its stale "best-effort" comment; or
3. if a sibling test leaks the seam, fix the leak and re-check whether this test passes with a nil seam
   (nil seam -> legacy scheduler scan -> `MCPHUB_E2E_SCHEDULER=none` -> empty rows, no error).

Whichever is chosen, the stale "Status is best-effort on non-Windows hosts" comment must go, and the
test must state which contract it is pinning.

## Release impact

None for the 0.4.24 bump: the failure is pre-existing and unrelated to the shipped changes. The other
red package, `internal/cli` (`TestForce_*`, `TestGuiResetPortClearsPortKeepsInstanceID`), is the known
host-environmental class — this machine's `R:\Temp` grants `S-1-5-11` (Authenticated Users), so the
owner-only state-file DACL allowlist correctly refuses `gui.pidport`. That one is the environment, not
the code.
