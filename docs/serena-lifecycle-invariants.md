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
| registry row Save (a durable claim) | install commit (the convergence the supervisor acts on) | a process crash between Save and commit releases the held flock, leaving a registry row with no intent daemon | **crash-repair (implemented — `RepairSerenaIntentFromRegistry`, PR #256):** a supervisor-side registry→intent self-heal that re-reads registry + intent under locks, APPENDS only the MISSING serena rows, writes via a lock-held helper, then the startup reconcile spawns them — deliberately NOT a full `InstallParsedManifest` re-install (which replaces all rows and would clobber a concurrent auto-register). Runs at supervisor startup only; an orphan created mid-lifetime heals at the next restart or the next new-workspace auto-register. See "Implemented: crash-repair" below |

When adding a new lifecycle step that reads ephemeral state, classify it against this
table. If you sample-then-act across a window, you owe one of: an immediate re-check,
a lease/singleton that fails closed (e.g. `supervisor.lock`), or non-fatal convergence
that reconciles the divergence later (the IntentWatcher 60s poll, the startup
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

## Implemented: crash-repair (registry↔intent self-heal)

The §1 row-3 split (a registry serena row with no matching supervisor-intent daemon,
left by a crash between the auto-register registry `Save` and its install commit) is
healed by `RepairSerenaIntentFromRegistry` (`internal/api/serena_intent_repair.go`,
PR #256), called by the supervisor at startup BEFORE `loadIntentFiles`
(`internal/cli/supervise.go`). A first implementation that ran at GUI startup and
re-used `InstallParsedManifest` was abandoned (PR #254, after a Codex architecture
review) because that path is the **wrong seam**:

- `buildMergedSupervisorIntent` REMOVES every existing serena daemon row and re-appends
  from the caller's `Workspaces` snapshot, so a stale snapshot clobbers a concurrent
  auto-register's freshly-added daemon. Avoiding that forced holding
  `serenaAutoRegisterInstallMu` across the whole install — which inherits the install's
  deep blocking-flock tree (registry, intent, preflight, audit, event-log) and stalls
  all `/serena/mcp` auto-registration (and GUI startup) behind any hung lock holder.

The shipped design is a **narrow supervisor-side primitive**, because the supervisor
already owns the intent→daemon lifecycle (startup reconcile, IPC `reconcile`, the 60s
`IntentWatcher`):

1. Try/acquire the registry flock with a **brief bounded retry** (a non-mutating registry
   reader — the routing cache refresh, `serena_routing/resolver.go` — takes the same
   exclusive flock only momentarily, so a single TryLock-then-skip would forfeit the only
   startup repair pass); HOLD it across the whole repair. Read the serena rows.
2. Acquire the supervisor-intent lock (TryLock, skip-on-contention) while holding the
   registry; re-read the intent under that lock (fresh, not a stale snapshot — the
   clobber-safety point). Deadlock-freedom comes from TryLock on BOTH locks, NOT from
   exclusive ownership (migration / strict-mode / autostart also take the intent lock
   without the registry lock).
3. Classify each serena registry row from the two locked snapshots:
   - **SKIP** a row whose `WorkspaceKey != WorkspaceKey(WorkspacePath)` (hand-edited or
     legacy pre-symlink-resolution row — appending would re-append forever) or whose
     workspace dir is gone (`workspacePathStale`; the install fan-out filters these too,
     since a removed `cmd.Dir` spawn-loops).
   - **DEFER** a row whose intent already has a daemon with the matching task name but
     `RuntimeSpec == nil` (a legacy pre-redesign descriptor the reconciler excludes from
     the spawn set) — appending a duplicate task name or replacing it would break the
     append-only contract; the operator runs migrate to re-materialize it.
   - **APPEND** the MISSING rows (a spawnable spec-bearing daemon truly absent) — never
     replace-all.
4. First-introduce (intent carries no `runtime_spec` at all): defer-to-migrate, because a
   live append cannot introduce the first dynamic-pool row while a possibly-old supervisor
   runs (the §7.1 split-brain hazard). Explicit, emitted as a warn event.
5. Materialize the missing rows via `BuildSupervisorDaemonsForSerena` (copying the mcphub
   binary path from an existing serena daemon's `Command` for consistency), APPEND, and
   write via `writeSupervisorIntentLockHeld`. The caller threads its already-resolved
   `stateDir` in so the repair writes the SAME `supervisor-intent.json` that
   `loadIntentFiles` reads (the registry stays on `DefaultRegistryPath()`, the canonical
   resolver auto-register also uses).

**Scope: startup-only.** The repair runs once at supervisor startup; the first reconcile
then spawns the recovered daemons. An orphan created WHILE the supervisor is already
running is NOT healed until the next supervisor restart OR the next new-workspace
auto-register (whose replace-all install re-materializes every registry row, the orphan
included). The `IntentWatcher` does NOT close this gap — it refreshes the intent cache
but only posts delta events for daemon-intent changes, not newly-added supervisor-intent
rows. Tightening the mid-lifetime window by wiring the repair into
`AutoRegisterSerenaWorkspace`'s existing-row handling, the IPC `reconcile` apply, or a
periodic tick is a tracked follow-up (consultant advisory, PR #256). Deferred and
divergent outcomes are surfaced as warn events to `supervisor-events.log`; GUI/status
surfacing of those states is a follow-up.

## Terms and Abbreviations

- **snapshot-to-action authority** — the principle that a read of ephemeral state
  authorizes an action only at the instant of the read; see §1.
- **commitCtx** — `context.WithoutCancel(requestCtx)` + timeout, used for the
  must-complete cutover steps; see §2.
- **orphan row** — a serena registry row with no matching supervisor-intent daemon,
  left by a crash between the registry Save and the install commit; see §1 row 3.
- **introduce-crash** — a crash during the very first serena introduce, before the
  intent gained any `runtime_spec`.
- **PR #249 / #253 / #254 / #255 / #256** — the serena routing (#249),
  auto-register-on-miss (#253), abandoned GUI-startup crash-repair (#254, closed),
  lifecycle-invariants doc (#255), and the implemented supervisor-side crash-repair
  (#256) pull requests these invariants emerged from.
