# ADR: Supervisor event-ownership model

**Status:** Proposed (open for review)

**Date:** 2026-05-28

**Driven by:** consultant memo on PR #236 r4 + bot review cascade on
PR #237/#238 (Windows orphan handling found 3 rounds of P2 findings
each rooted in the shared-event-loop + shared-Job-Object design).

**Decision deadline:** before Phase 9 (Maintenance timer scheduler)
adds a new event publisher to the shared queue. The decision shapes
how Phase 9 + Phase 10 (Migration) + Phase 11 (Autostart) integrate
with the supervisor; deferring it past Phase 9 means N more producers
on a model that may not survive the architectural cleanup.

## Context

The v0.5.x supervisor lifecycle has settled around two shared
resources:

- **One `EventLoop` (channel + drainer)** in `runSupervise`, drained
  by a single handler goroutine. All event producers — reconcile
  loop, IPC commands, `crashCh` → SM event translator, daemon-intent
  watcher, backoff timers — post to the same `ch`. Two-channel
  variant landed in PR #236 r4: `selfCh` was added for handler-self-
  posts so the handler is never blocked posting back into a queue it
  is the only drainer of. The main channel is still one-per-supervisor.

- **One `process.Job` Object** in `runSupervise`, passed to every
  spawn closure via `StartWithJob(job, cmd)`. All daemon child
  processes are members of the same Job. `KILL_ON_JOB_CLOSE` on the
  Job means supervisor exit reaps every child — this is the v0.5.0
  reliability primitive that the v0.4.x watchdog could not guarantee.

The PR #236 → #237 → #238 review cascade surfaced six architectural
weaknesses that all root in these shared resources:

1. **Stop-during-spawn intent drop**
   (#236 r4 → r5 fixed) — `StSpawning` lacked an `EvIntentUpdate`
   transition; intent flips during spawn were silently dropped. Fixed
   via SM table extension + `queued_action=stop` mechanism + bounded
   auto-clear. Real fix, but the SM table grew non-trivially for one
   per-task intent concern.

2. **Self-post FIFO race / inline-Post deadlock**
   (#236 r3 → r4 → #237 fixed) — handler self-posting `EvHealthOK`
   on the shared queue could (a) deadlock when buffer full, or (b)
   be overtaken by external `EvIntentUpdate`. Fixed via priority
   `selfCh` and `PostSelf` non-blocking semantics. Real fix, but
   the design needed an entire channel allocated just to avoid
   contention between one handler and itself.

3. **Tracker drift on `StIdle` from non-idle SM state**
   (#236 r4 → #237 fixed) — controller persisted tracker AFTER SM
   transitioned to `StIdle` but BEFORE tracker observed it. Fixed
   by adding `MarkExited` call site in `handleLoopEvent`.

4. **Windows post-create orphan duplicate-spawn risk**
   (#237 → #238 fixed across 6 sub-rounds) — `process.StartWithJob`
   on Windows can return error AFTER `CreateProcess` succeeded (the
   `os.FindProcess` step at `start_with_job_windows.go:181-186`).
   The OS child is alive at `pi.ProcessId` but Go-side handle is
   unavailable. The 6-round fix landed:
   - `process.ErrSpawnPostCreate` sentinel to discriminate the case
   - `process.BestEffortKillByPID` to terminate the orphan PID
   - `OrphanPID` field SEPARATE from `CurrentPID` in tracker entry
     (avoid terminate-path identity-proof conflation)
   - End-to-end persistence through `SupervisorDaemonState` JSON
   - `clearOrphanPIDLocked` on every successful state transition
   - GUI Dashboard surface
   The fix works but is a PID-keyed workaround for a fundamentally
   handle-based problem. A Job Object-based approach would be
   architecturally cleaner (`TerminateJobObject` is the obvious
   primitive — kills the whole tree, handle-keyed, no PID recycling
   race). But it is **unsafe today** because the Job is shared:
   one orphan cleanup would terminate every healthy daemon.

5. **Wrapper-descendants survive PID-kill**
   (#237 → #238 known-debt) — when the orphan is a wrapper (`uvx ->
   python`, `npx -> node`), the wrapper PID's children are alive in
   the Job Object but not in the wrapper process. PID-keyed kill
   leaves them. Job-keyed kill would handle them — same architectural
   blocker as #4.

6. **PID recycling race**
   (#237 → #238 known-debt) — between `StartWithJob` returning the
   orphan PID and `BestEffortKillByPID` calling `OpenProcess(pid)`,
   Windows can recycle the PID. Window is microseconds but real. A
   handle-based kill (Job Object) sidesteps this entirely.

**Pattern:** every one of these is a shared-resource contention or
identity-proof problem. Each individual fix is correct, but the
cumulative LOC + cognitive load + bot-cascade rounds is high. The
architectural alternative — per-task ownership — would eliminate
the class.

## Decision

**Option A — Per-task FSM mailbox (actor model):**

Each daemon owns:
- Its own per-task `mailbox` (buffered channel for that daemon's events)
- Its own SM state goroutine that drains the mailbox
- Its own Job Object handle (separate `process.Job` per spawn)
- Its own `MarkSpawned/MarkExited/...` tracker entry (unchanged)

External producers (reconcile, IPC, watcher, crashCh) route to the
right mailbox by `TaskName`. The supervisor maintains a small
`map[taskName]*daemonActor` and a small fan-out router goroutine that
reads from a small inbound queue and forwards to the right mailbox.

**Option B — Shared loop + per-task Job Object:**

Keep the single EventLoop (the FIFO ordering and `selfCh` priority
work). But each spawn allocates its OWN Job Object instead of reusing
the supervisor-wide one. `TerminateJobObject` becomes safe per-orphan
because the Job is task-scoped.

**Option C — Status quo (defer):**

No architectural change. Each new architectural finding becomes a new
sentinel / new field / new transition. Cumulative cost grows; future
phases (9, 10, 11) inherit the same fragility.

## Recommendation

**Option B for v0.5.x.** **Option A as a v0.6.x candidate.**

### Why B for v0.5.x

- **Smallest blast radius.** Per-task Job Object is a constructor
  change: `process.Job` allocated inside the spawn closure instead
  of in `runSupervise`. The spawn closure already has access to all
  the inputs needed.
- **Closes findings #4 (orphan), #5 (descendants), #6 (PID recycling)
  in one move.** `TerminateJobObject(daemon.Job, 1)` becomes safe
  again — kills only that daemon's tree. The PR #238 `Job.TerminateAll`
  method (already implemented) is then directly useful.
- **Preserves the shared-loop FIFO + selfCh priority design** which is
  working. Findings #1 (stop-during-spawn) and #3 (tracker drift)
  are SM-table issues independent of Job ownership; #2 (self-post
  race) is fixed by `selfCh`. None of these are revisited by Option B.
- **Phase 9-11 compatibility.** Maintenance timer publishers (Phase
  9) keep posting to the shared `EventLoop.ch` as today. Migration
  (Phase 10) and Autostart (Phase 11) integrate via the same
  channel. The shape of the supervisor's external contract does not
  change.
- **Cost.** ~150 LOC: move `process.NewKillOnCloseJob()` from
  `runSupervise` into spawn closure; track per-task Job in
  daemon-runtime tracker; arrange `defer job.Close()` on wait
  goroutine exit; update orphan path in `supervise.go` to call
  `daemonJob.TerminateAll()` (already implemented). Plus ~50 LOC
  tests for the per-task Job lifecycle.

### Why not A for v0.5.x

Option A is the architecturally cleanest model but its blast radius
is large: rewriting the controller's `handleLoopEvent` from "single
handler dispatching by TaskName" to "router fans out to per-task
mailboxes" touches the entire supervisor control plane. It is a
v0.6.x scope, not a v0.5.x in-flight cleanup. Doing it in v0.5.x
risks delaying Phase 9-11 and reopening the bot-cascade pattern this
ADR is meant to terminate.

### Why not C

Each subsequent feature PR will continue to find the same class of
shared-resource problem. PR #236 → #237 → #238 cumulative cost was
~1500 LOC and 11 bot rounds across three PRs. A single Option B
refactor is ~200 LOC and closes the entire class. Cost is bounded;
deferral is unbounded.

## Consequences

### Positive (Option B)

- Closes 3 known-debt items from PR #238 known-debt comment
  (#237/#238 architectural follow-ups) in one focused refactor.
- `process.Job.TerminateAll` becomes a production-callable method
  (currently retained but unused after PR #237 r5.1 revert).
- Phase 9 maintenance-timer publishers do not need new design — they
  post to the same `EventLoop.ch` as today.
- `KILL_ON_JOB_CLOSE` semantics improve: per-daemon Job means a
  hung daemon's children die when that daemon is force-killed,
  without waiting for supervisor exit. Today, only supervisor exit
  reaps non-cooperating descendants.

### Negative (Option B)

- Spawn path becomes slightly more complex: per-spawn Job
  allocation + assignment + cleanup-on-exit needs careful
  lifecycle (defer-style) to avoid Job handle leaks.
- Job handle exhaustion under high churn: each spawn allocates a
  new kernel handle; closed on child exit. Today's shared-Job
  model has exactly one handle per supervisor lifetime. Per-daemon
  Job means N handles concurrently, where N = active daemon count.
  This is bounded by manifest size (typically ≤20), well below
  Windows per-process handle limits (16M); not a practical concern.
- Tests touching the Job Object will need updates: per-task creation
  changes the seam.

### Neutral

- Operator-visible behavior is unchanged. `mcphub status`, GUI
  Dashboard, supervisor-state.json schema all stay the same.
- v0.4.x rollback path is unchanged (legacy watchdog code is
  read-only).

### Future work explicitly NOT in this ADR

- **Per-task FSM mailbox (Option A) for v0.6.x.** Separate ADR if/when
  the v0.5.x scope is closed and Option B's per-task Job has been in
  production for a release cycle. The actor model has stronger
  guarantees but the migration cost is too high in v0.5.x.
- **Cross-platform Job Object equivalent.** Linux cgroups + systemd
  scope or kqueue-based POSIX wrapper-orphan reaping. Tracked as
  F-series follow-up per `process/jobobject_other.go` doc.

## Implementation outline (informative)

Single PR, scoped to v0.5.x:

1. **`process.Job` per spawn.** Move `process.NewKillOnCloseJob()`
   from `runSupervise` into the spawn closure (production path in
   `internal/cli/supervise.go`). Each spawn attempt allocates its
   own Job; `defer daemonJob.Close()` on the wait goroutine.
2. **Tracker tracks per-task Job handle** (in-memory only, not
   persisted — Job handles are process-lifetime). On orphan
   cleanup, `MarkSpawnFailedPreservePID` records `OrphanPID` plus
   the Job handle for the orphan cleanup path.
3. **Orphan branch uses `daemonJob.TerminateAll(5000)`** (the
   method already implemented in PR #237 r5.1, currently unused).
   The shared-Job hazard from PR #238 P1 is removed because Job is
   now task-scoped.
4. **`BestEffortKillByPID` retained** but no longer used by the
   supervisor's orphan path. Kept for any future caller that has
   only a PID; alternative would be deletion (low risk, low value
   either way).
5. **Tests.** Per-task Job creation + cleanup-on-exit + Terminate-
   All-on-orphan-failure regressions. Estimate ~50 LOC test code
   on top of the existing supervisor_controller_test.go scaffold.
6. **Migration safety.** Supervisor restart: no schema change to
   supervisor-state.json (Job handles are process-scoped, not
   persisted). v0.4.x rollback path: untouched.

Expected delta: ~150-200 LOC implementation + ~50 LOC tests. Bot
cascade risk: low (the change is well-scoped and each finding's
mitigation is localized; unlike PR #236-#238 where each finding
exposed adjacent architectural concerns).

## References

- PR #236 (supervisor SM stabilization, merged via `--admin` with
  3 known-debt items): https://github.com/applicate2628/mcp-local-hub/pull/236
- PR #237 (close #236 known-debt, merged via `--admin` with 3
  Windows-edge known-debt items): https://github.com/applicate2628/mcp-local-hub/pull/237
- PR #238 (close #237 known-debt, clean PASS-merge after 6 rounds
  driving to bot PASS): https://github.com/applicate2628/mcp-local-hub/pull/238
- v0.5.0 supervisor architecture spec:
  `docs/superpowers/specs/2026-05-16-v0.5.0-supervisor-architecture.md`
- Consultant memo on PR #236 r4 (cumulative bot cascade pattern):
  inline in PR #236 review thread.
- Kosyak `feedback_kosyak_drive_bot_to_pass_no_admin_escape`
  (operator discipline that motivated this ADR — repeated `--admin`
  merges in #236/#237 were the symptom this ADR addresses
  architecturally).

## Terms and abbreviations

- **ADR:** Architecture Decision Record — durable doc capturing one
  architectural decision and its context.
- **Job Object:** Windows kernel primitive for grouping processes
  into a single lifecycle unit; supports `TerminateJobObject` (kills
  all members), `KILL_ON_JOB_CLOSE` (kills all members when last
  handle closes), and `IsProcessInJob` queries. POSIX has no direct
  equivalent; cgroups + systemd scope is the closest.
- **`StartWithJob`:** the supervisor's spawn helper that attaches
  the child process to a Job Object at `CreateProcess` time via
  `PROC_THREAD_ATTRIBUTE_JOB_LIST`. Avoids the Start-then-Assign
  race where a fast-exiting child could escape the Job between
  `cmd.Start` and `AssignProcessToJobObject`.
- **`ErrSpawnPostCreate`:** sentinel introduced in PR #237 marking
  the Windows-specific case where `CreateProcess` succeeded but
  `os.FindProcess` failed; the OS child is alive but Go has no
  handle to it. Drives the orphan-cleanup path that this ADR
  addresses architecturally.
- **`selfCh`:** the priority channel introduced in PR #236 r4 for
  handler-self-posts; separate from the main `ch` so the handler
  can post back without contending against external producers.
- **`queued_action`:** the per-task pending-side-effect field on
  `SMContext` (e.g. `queued_action=stop` records "stop intent
  arrived during StSpawning, honor on next health-ok/child-exit").
- **Per-task mailbox (actor model):** the alternative architecture
  this ADR rejects for v0.5.x — each daemon owns its own event
  channel + drainer goroutine + Job + state; supervisor routes
  external events to the right mailbox.
- **Cumulative bot cascade:** the pattern where each round of bot
  review finds NEW architectural concerns rather than narrowing on
  the original bug; this ADR is the corrective response to repeated
  cascades on PR #236/#237 known-debt that all rooted in shared-
  resource issues.
