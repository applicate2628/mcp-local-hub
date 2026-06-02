# Serena lifecycle invariants

Load-bearing invariants for the serena dynamic-pool auto-register / supervisor
lifecycle. They are documented here (rather than only inline) because a future edit
can silently violate them, and the violation is not local — it surfaces as a
session/liveness race or a registry/intent split-brain that the incremental review
rounds on PR #253 repeatedly surfaced (7 bot rounds + the consultant strategic
review clustered almost entirely in this area).

## 1. Snapshot-to-action authority (the cross-cutting one)

> **An observation of ephemeral state carries authority to act only at the instant
> it was taken. By the time you act on it (T+δ), it may be stale. Convert the
> snapshot into a lease, re-check it immediately before the action, or back the
> action with non-fatal convergence/recovery — never treat a stale read as durable
> permission.**

Every serena lifecycle bug in this subsystem has been an instance of sampling
ephemeral state at T and acting at T+δ as if the sample still held:

| Snapshot (T) | Action (T+δ) | Staleness risk | Mitigation in code |
|---|---|---|---|
| supervisor liveness probe (`autoRegisterSupervisorRunningFn` / `SupervisorRunningUnderStateDir`) | reap / install / IPC reconcile | supervisor exits between probe and reconcile | **r7:** an UNAVAILABLE reconcile (`ErrSupervisorIPCUnavailable`) is treated as "supervisor gone → START it", not ignored (`serena_auto_register.go`, post-commit block) |
| router session peek (`peekVersionState`) | forward / detached register | session DELETE/idle-sweep between peek and act | **rounds 2/4/5:** the session watcher cancels the detached register ctx on mid-flight termination; the helper re-checks `ctx.Err()` at its mutation boundaries |
| registry row Save (a durable claim) | install commit (the convergence the supervisor acts on) | a process crash between Save and commit releases the held flock, leaving a registry row with no intent daemon | **crash-repair:** `RepairOrphanSerenaWorkspaces` (`serena_crash_repair.go`) re-converges the two at GUI startup (live-add re-install + reconcile) |

When adding a new lifecycle step that reads ephemeral state, classify it against this
table. If you sample-then-act across a window, you owe one of: an immediate re-check,
a lease/singleton that fails closed (e.g. `supervisor.lock`), or non-fatal convergence
that reconciles the divergence later (the IntentWatcher 60s poll, the startup repair).

## 2. The pre/post-commit ctx boundary (auto-register)

`AutoRegisterSerenaWorkspace` has a **point of no return** — the supervisor-intent
write. Two contexts gate it, and the boundary is load-bearing:

- The **request ctx** (cancellable; the router's detached `WithoutCancel(r.Context())`
  + 45s, watched for session death) gates the **pre-commit** phase ONLY: the 5b abort,
  the 7c session gate, and the install's own commit-point `ctx.Err()` hook. A session
  terminated here aborts cleanly (rollback; recovery-restart if a reap already ran).
- `commitCtx = context.WithoutCancel(ctx) + 60s` drives the **must-complete** steps:
  reap, post-commit start, the failPreCommit recovery-restart, the live-add reconcile.
  A session cancel must NOT abort these — a reaped-but-not-restarted supervisor leaves
  the host with NO supervisor running.

**Invariant: a step that must complete after the reap goes on `commitCtx`; a step
that should honor a session termination goes on the request ctx. Placing a step on
the wrong context reintroduces either a half-cutover (no supervisor) or a
terminated-session-still-mutates bug.** When you add a step, decide which side of the
commit it is on and pick the ctx accordingly.

## 3. Registration-strict vs routing-lenient session asymmetry

The two serena session gates treat a non-empty `routerSessionAbsent` id differently,
**by design**:

- The **routing** gate (`serena_router.go` authoritative path-bearing gate) treats
  absent as a TRUE-legacy / path-only caller and forwards to an EXISTING workspace
  (PR #249). Leniency is acceptable because routing is a lower-durability read.
- The **registration** gate (`attemptSerenaAutoRegister`) REJECTS a non-empty absent
  id (minted-then-DELETEd or never-minted-here). Registration mutates the registry +
  supervisor intent — higher-trust side effects — and per MCP a valid in-session
  tool-call carries an initialized (live) or empty session id, so an absent id is
  revoked/unknown.

**Invariant: routing MAY treat absent as legacy; registration MUST NOT. Do not
"harmonize" the two gates — the asymmetry is intentional (mutate vs read).** It is
pinned by `TestSerenaRouter_AutoRegister_AbsentSession_RejectsBeforeRegister` (the
register path) alongside the routing-path legacy tests; changing either gate must keep
both passing.

## Terms and Abbreviations

- **snapshot-to-action authority** — the principle that a read of ephemeral state
  authorizes an action only at the instant of the read; see §1.
- **commitCtx** — `context.WithoutCancel(requestCtx)` + timeout, used for the
  must-complete cutover steps; see §2.
- **orphan row** — a serena registry row with no matching supervisor-intent daemon,
  left by a crash between the registry Save and the install commit; see §1 row 3.
- **introduce-crash** — a crash during the very first serena introduce, before the
  intent gained any `runtime_spec`; not live-add-repairable (deferred to `mcphub
  migrate`).
- **PR #249 / #253** — the serena routing (#249) and auto-register-on-miss (#253)
  pull requests these invariants emerged from.
