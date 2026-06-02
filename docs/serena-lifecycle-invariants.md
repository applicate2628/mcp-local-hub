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

| Snapshot (T) | Action (T+δ) | Staleness risk | Mitigation |
|---|---|---|---|
| supervisor liveness probe (`autoRegisterSupervisorRunningFn` / `SupervisorRunningUnderStateDir`) | reap / install / IPC reconcile | supervisor exits between probe and reconcile | **#253 r7:** an UNAVAILABLE reconcile (`ErrSupervisorIPCUnavailable`) is treated as "supervisor gone → START it", not ignored (`serena_auto_register.go`, post-commit block) |
| router session peek (`peekVersionState`) | forward / detached register | session DELETE/idle-sweep between peek and act | **#253 rounds 2/4/5:** the session watcher cancels the detached register ctx on mid-flight termination; the helper re-checks `ctx.Err()` at its mutation boundaries |
| registry row Save (a durable claim) | install commit (the convergence the supervisor acts on) | a process crash between Save and commit releases the held flock, leaving a registry row with no intent daemon | **crash-repair (PLANNED follow-up):** a supervisor-side registry→intent self-heal that re-reads registry + intent under locks, materializes only the MISSING serena rows, writes via a lock-held helper, then reconciles — deliberately NOT a full `InstallParsedManifest` re-install (which replaces all rows and would clobber a concurrent auto-register). See "Planned: crash-repair" below |

When adding a new lifecycle step that reads ephemeral state, classify it against this
table. If you sample-then-act across a window, you owe one of: an immediate re-check,
a lease/singleton that fails closed (e.g. `supervisor.lock`), or non-fatal convergence
that reconciles the divergence later (the IntentWatcher 60s poll, the planned
crash-repair).

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

## Planned: crash-repair (registry↔intent self-heal)

The §1 row-3 split (a registry serena row with no matching supervisor-intent daemon,
left by a crash between the auto-register registry `Save` and its install commit) needs
a convergence step. A first implementation that ran at GUI startup and re-used
`InstallParsedManifest` was abandoned (PR #254, after a Codex architecture review)
because that path is the **wrong seam**:

- `buildMergedSupervisorIntent` REMOVES every existing serena daemon row and re-appends
  from the caller's `Workspaces` snapshot, so a stale snapshot clobbers a concurrent
  auto-register's freshly-added daemon. Avoiding that forced holding
  `serenaAutoRegisterInstallMu` across the whole install — which inherits the install's
  deep blocking-flock tree (registry, intent, preflight, audit, event-log) and stalls
  all `/serena/mcp` auto-registration (and GUI startup) behind any hung lock holder.

The correct design (a follow-up) is a **narrow supervisor-side primitive**, because the
supervisor already owns the intent→daemon lifecycle (startup reconcile, IPC `reconcile`,
the 60s `IntentWatcher`):

1. Try/acquire the registry flock; read the serena rows.
2. Acquire the supervisor-intent lock; re-read the intent under that lock (fresh, not a
   stale snapshot — this is the clobber-safety point).
3. Compute the MISSING serena rows from the two locked snapshots.
4. Live-add: APPEND only the missing rows (never replace-all).
5. First-introduce (intent carries no `runtime_spec`): explicitly support replacing the
   legacy serena rows (the running supervisor is provably this binary) OR keep a
   defer-to-migrate policy — make it explicit, not implicit.
6. Write the intent via a lock-held helper, refresh the controller cache, then reconcile.

Sharp nuance for the implementer: the `IntentWatcher` refreshes the intent cache but
only posts delta events for daemon-intent changes, not newly-added supervisor-intent
rows — so hang the repair before startup reconcile and/or before the IPC `reconcile`
apply, and extend the watcher only if periodic repair is wanted. This new helper becomes
the named **registry→intent materialization owner** and must be tested against a
concurrent auto-register.

## Terms and Abbreviations

- **snapshot-to-action authority** — the principle that a read of ephemeral state
  authorizes an action only at the instant of the read; see §1.
- **commitCtx** — `context.WithoutCancel(requestCtx)` + timeout, used for the
  must-complete cutover steps; see §2.
- **orphan row** — a serena registry row with no matching supervisor-intent daemon,
  left by a crash between the registry Save and the install commit; see §1 row 3.
- **introduce-crash** — a crash during the very first serena introduce, before the
  intent gained any `runtime_spec`.
- **PR #249 / #253 / #254** — the serena routing (#249), auto-register-on-miss (#253),
  and lifecycle-invariants/crash-repair (#254) pull requests these invariants emerged
  from.
