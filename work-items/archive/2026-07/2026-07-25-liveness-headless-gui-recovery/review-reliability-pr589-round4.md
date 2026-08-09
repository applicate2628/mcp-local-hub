# Reliability review — PR #589 round 4 design

## Scope and evidence

Reviewed the accepted `design.md` against
`research-pr589-round4.md` and current source at
`db879c5f76e479a7ae568f58ae87de864f3d9f28`. This is a design gate only.
No product code was changed and no package test, graphical user interface
(GUI), tray, supervisor, scheduler, or live-state operation was run.
Codegraph was connected but this worktree has no `.codegraph/` index; its own
tool contract says indexing is the user's decision, so source was inspected
read-only instead.

## Target reliability objectives

| Objective | Service-level indicator and measurement point | Window and threshold | Error-budget consequence |
| --- | --- | --- | --- |
| F5 retained-lease safety | Same-tick GUI-owner relaunch callbacks observed after a Phase-I result that cannot prove lease release; measured at the injected relaunch seam | Every ensure-alive invocation; zero such callbacks | Any violation blocks implementation acceptance |
| F6 committed-audit survival | Committed destructive recoveries returning without either the exact retained log row or an atomically persisted pending handoff; measured at `ExecuteWithDependencies` return | Every committed recovery; zero unaudited returns | Any violation blocks implementation acceptance |
| F6 retained-row cardinality | Retained exact rows for one normalized committed event across active log plus `.1`; measured after late completion and replay | Every replay race; exactly one retained row | Any duplicate blocks implementation acceptance |
| F7 recovery progress | Time from a pre-action detection site to suppressor/relaunch callback under injected event-log contention or write stall; measured at the callback seam | Every invocation; no more than the explicit diagnostic budget | Any overrun blocks implementation acceptance |

These are correctness objectives rather than fleet availability percentages:
the scheduled action has no production metrics or paging surface in the
accepted design.

## Failure-mode and participant review

| Class / participant | Failure mode | Current or designed containment | Detection, latency, page decision | Evidence / gate |
| --- | --- | --- | --- | --- |
| F5 outer Phase-I budget | The classifier times out while its worker can acquire or retain the GUI flock | Design returns a tick-local disposition and removes GUI-owner relaunch authority | Required: stdout plus structured Unknown event in the same tick; no page | `internal/cli/supervise_ensure_alive.go:558-581`; **REVISE** because the synchronization that makes the timeout disposition race-free is unspecified |
| F5 GUI probe owner | `ProbeGUIOwnerLease` acquires, then an internal release fails and returns `Unknown` with no lease object | Design says only positive release permits relaunch | Existing result exposes only `State`, `Reason`, `Lease`, and `Record`; no typed acquired/release disposition | `internal/gui/single_instance.go:105-117,485-521,614-618`; **REVISE**: expand the owning result contract or an equivalent typed lifecycle seam; do not parse error text |
| F5 caller-owned Free lease | Cancellation, contract-invalid state, marker mismatch, compare-and-swap outcome, or release failure leaves ownership uncertain | Every lease-bearing exit must call `ReleaseErr`; `may retain` gates GUI-owner relaunch only | Same-tick Unknown event; no page | Current discarded releases at `internal/cli/supervise_ensure_alive.go:631-660`; observed Free release at `:678-750`; design direction is correct |
| F5 running + dead / down + dead / elapsed Unknown | A retained Phase-I lease is followed by a GUI-owner relaunch that can only exit busy | Tick-local capability suppresses all GUI-owner relaunch paths but not standalone supervisor recovery | Suppression event in the same tick; no page | Relaunch paths at `internal/cli/supervise_ensure_alive.go:1373-1394,1478-1499`; **PASS conditional on the typed F5 outcome correction** |
| F6 tracked writer | Timed-out worker continues with process-local lifetime while holding the log mutex and flock | Persist exact handoff after respawn; never start a competing blocking write | Pending handoff present by function return; no page | Worker ownership transfer at `internal/api/supervisor_events.go:614-682`; current process-local return at `internal/daemonrecovery/recovery.go:817-825`; design direction is correct |
| F6 release-failed writer | Row may exist while this process still retains the flock | Persist handoff, do not reacquire; replay after process exit proves the row or appends it | Returned handoff persistence failure immediately; otherwise pending file; no page | Release semantics at `internal/api/supervisor_events.go:183-192,660-682`; design direction is correct |
| F6 pending handoff writer | Partial file, rename collision, disk-full, permission error, or process exit before acknowledgement | Same-directory temp, full write, file sync, close, atomic rename; acknowledge only afterward | Typed error returned after respawn in the same invocation; no page | Required by design but the final failure type and caller-visible wording are unspecified; **REVISE plan detail** |
| F6 replay owner | Two processes replay the same handoff, or a late original finishes around replay | Replay, row proof, append, and retirement run under the existing mutex plus flock | Exact row in active/`.1` is the settled signal; pending file remains on any failure; no page | Serialization owner at `internal/api/supervisor_events.go:482-596`; **PASS conditional on all emit modes entering replay before rotation/current append** |
| F6 rotation owner | Original row moves from active to `.1` before replay | Check exact line identity in both retained generations under the same locks | Exact-line match during replay; no page | One-backup policy at `internal/api/supervisor_events.go:691-709,795-821`; sufficient for the retained-history guarantee |
| F6 pending retirement | Process exits after replay append but before pending-file deletion | Next replay finds the exact row and retries deletion without appending | Pending file remains visible until deletion; no page | Design is idempotent if a delete failure never triggers another append |
| F6 one-shot CLI | Process exits immediately after `ExecuteWithDependencies` returns | Return only after row or process-exit-safe handoff exists | Typed error on handoff persistence failure; otherwise success | One-shot return at `internal/cli/daemon_recover.go:121-134`; current design closes the reported process-exit gap |
| F7 detection helper | Another process holds `supervisor-events.log.lock` | Design selected `TryEmit`, which skips mutex/flock contention | Dropped advisory row has no alert; action must proceed | `internal/api/supervisor_events.go:387-405,502-577`; lock-contention case is contained |
| F7 detection helper after lock acquisition | Filesystem, antivirus, rotation, open, write, or close stalls | `TryEmit` calls the ordinary write synchronously and has no write deadline | No signal before the stall; scheduled action can still be terminated before recovery | `internal/api/supervisor_events.go:598-611`; **REVISE**: `TryEmit` does not establish claim 4 |
| F7 two pre-action callers | Detection logging precedes suppressor evaluation or relaunch | Must use a truly caller-bounded emit or move the recovery decision before logging | Callback reached within explicit bound; no page | `internal/cli/supervise_ensure_alive.go:923-950,1171-1179`; **REVISE** |

All failure modes are `analysis-only` in this gate because the dispatch
explicitly prohibited tests in `internal/api` and `internal/cli`. The top
failure mode, loss of the committed audit at process exit, is analysis-only
because the required falsifier is a subprocess-exit injection; that probe is
mandatory in the implementation plan.

## Resource lifetime and process-exit analysis

### F5 GUI lease

The worker owns every lease obtained by the Phase-I probe. It must publish a
typed, monotonic lifecycle result before the outer function can authorize any
GUI-owner relaunch. A timeout cannot infer safety from a missing result. The
resource is released by `ReleaseErr` on every success, failure, cancellation,
and invalid-contract exit; if release is unconfirmed, operating-system process
exit is the final cleanup boundary and GUI-owner relaunch is suppressed for
that tick. Standalone supervisor recovery remains available because it does
not acquire the GUI flock.

The accepted design does not yet define a race-free owner-to-caller publication
protocol, and the existing probe result loses internal release disposition
(`internal/gui/single_instance.go:112-117,614-618`). The plan must add one
typed owner-level outcome. A conservative rule that maps every probe
`Unknown` to `may retain` is not acceptable: pre-acquisition marker or path
failures can recur every tick and would suppress GUI recovery indefinitely.

### F6 tracked worker and handoff

On timeout, the tracked worker owns the logger mutex and cross-process flock
until its write and release complete (`internal/api/supervisor_events.go:
614-682`). The caller must not close that uncertainty with another blocking
append. A pending file is owned by `SupervisorEventLog` from acknowledged
atomic rename until either:

1. the exact row is proved in active/`.1`, or
2. replay appends the exact row successfully,

after which deletion retires the handoff. Every read, append, sync, rename,
scan, or delete failure leaves the handoff in place and returns an error; no
failure path may discard it. Process exit releases in-memory goroutines and
operating-system locks but does not remove the renamed handoff, which is the
required durability boundary. Power-loss durability is outside the supplied
finding; if claimed later, parent-directory synchronization must be specified
and verified per supported operating system.

### F7 bounded diagnostics

`TryEmit` has bounded mutex/flock acquisition but an unbounded synchronous
write (`internal/api/supervisor_events.go:598-611`). Therefore it cannot own
the guarantee that detection logging never blocks recovery. Use
`EmitWithTimeout` with one explicit small caller budget, or perform the
recovery decision before observability. If `EmitWithTimeout` is selected, its
worker owns both locks until write completion or process exit and no second
writer may be started for the advisory event.

## Exact-byte identity and active-plus-`.1` deduplication

**Verdict: sufficient for exactly one retained row, but only with the following
enforced preconditions. It is not sufficient by itself for persistence.**

| Scenario | Why the scheme holds | Required enforcement |
| --- | --- | --- |
| Late original completion | Replay cannot pass the same flock while the original owns it; after release, exact-line proof sees the original before appending | Original and handoff must use the same normalized bytes, including a timestamp fixed before the first attempt |
| Release failure | Handoff persists while the current process may retain the flock; process exit releases the operating-system lock, then replay proves or appends | Never reacquire in the release-failed process |
| Concurrent replay | Existing mutex plus flock serializes proof, append, and delete | Every emit mode must replay before its own rotation/current append under those same locks |
| Rotation | The retention model contains exactly active plus `.1`; checking both under the lock covers every retained prior row | Compare complete JSONL records, not substrings; keep handoff on scan/read error |
| Exit after append, before delete | Next replay proves the row and retries retirement | Deletion failure must never authorize another append |
| Repeated persist | Hash filename gives a stable key | If the target exists, verify its full content; never trust digest/filename alone |

The active-plus-`.1` search is sufficient only for the repository's **retained
history**: rotation intentionally deletes older generations
(`internal/api/supervisor_events.go:795-821`). If the old row has aged out of
both retained files, replaying the handoff creates one retained row, not a
duplicate within the retention set. Exact-byte identity depends on
normalizing once; current marshaling fills a missing timestamp on each call
(`internal/api/supervisor_events.go:731-776`), so re-marshaling the event is
not equivalent.

## Retry, re-entry, and idempotency rules

| Operation | Attempts / backoff | Idempotency owner | Settled or committed signal |
| --- | --- | --- | --- |
| F5 scheduled recovery | One decision per tick; next scheduler tick is the retry; no in-tick relaunch retry | Tick-local typed lease disposition | Positive release permits GUI-owner relaunch; `may retain` suppresses it |
| F6 handoff persist | One synchronous persist after respawn; re-entry with the same bytes accepts only an identical existing file | Exact-byte content plus content-derived filename, owned by `SupervisorEventLog` | Atomically renamed and synced pending file |
| F6 replay | At most one opportunistic replay pass per event-log emission; no background loop or invented backoff; later emissions retry | Proof/append/delete transaction under mutex plus flock | Exact row in active/`.1` and handoff removed |
| F6 late original | No independent retry while outcome is unknown | Same pending handle plus exact-byte replay identity | Exact retained row |
| F7 advisory detection | One attempt, no retry | Pre-action diagnostic helper | Callback progress, not event delivery |

## Availability and recovery tradeoffs

- F5 spends at most the current scheduled tick's GUI-owner availability when a
  lease may be retained; standalone supervisor recovery stays enabled. A typed
  distinction is necessary to prevent persistent pre-acquisition failures from
  turning that bounded cost into indefinite suppression.
- F6 keeps respawn ahead of audit finalization, preserving daemon recovery. A
  handoff persistence failure after respawn must return a distinct error that
  says recovery may have succeeded but committed-audit durability failed; it
  must not rewrite the result as a respawn failure.
- F6 replay adds work under the shared event-log lock. It must first check
  whether pending files exist and bound each file to the existing event-size
  cap; malformed or unreadable files remain quarantined/pending and fail the
  emit rather than extending lock hold without bound.
- F7 may lose two advisory detection rows under contention. That is acceptable
  only because suppressor and relaunch outcome rows remain separate. The
  action-progress bound takes priority over detection-row delivery.

## Required plan corrections

1. Expand the F5 change surface to the GUI probe owner, or define an equivalent
   typed lifecycle seam, so pre-acquisition `Unknown`, positively released,
   and possibly retained outcomes cannot be conflated. Specify the race-free
   publication protocol between worker and timeout owner.
2. Replace F7's `TryEmit` selection with a mechanism that bounds the write
   phase as well as lock acquisition (`EmitWithTimeout` with an explicit
   budget), or move each recovery decision before observability. Retain tests
   for both flock contention and injected stalled write.
3. Define F6's typed post-respawn audit-persistence failure and the exact
   command-line message/exit behavior. Preserve the already-delivered respawn
   result separately.
4. Make F6 replay a mandatory prefix of every event-log emit mode under the
   existing mutex plus flock, before rotation/current append. Specify
   exact-line comparison across active and `.1`, full-content verification on
   digest collision, maximum pending-file size/count handling, and
   fail-closed retention on every read/append/delete error.
5. State the F6 guarantee as process-exit-safe and retained-history exactly
   once. Do not claim power-loss durability or unbounded historical
   exactly-once without additional storage and synchronization evidence.

## Falsifying probes

1. F5: block the probe at each boundary before acquisition, after acquisition,
   during internal release, and during caller-owned release. Assert that only
   a typed `may retain` outcome suppresses all three GUI-owner relaunch paths,
   while pre-acquisition `Unknown` does not suppress indefinitely.
2. F5: mutate the worker-to-timeout synchronization so the timeout reads
   `not exposed` while the worker proceeds into the probe; the race-engineered
   test must fail.
3. F6: block the tracked writer, finish respawn, return from recovery, terminate
   the helper process, then use a new process to prove the handoff survives and
   yields exactly one retained row.
4. F6: release the original writer before replay, after handoff creation, and
   around rotation; assert one exact line across active plus `.1`.
5. F6: inject persist, scan, append, sync, rename, and delete failures; the
   pending file must remain or the caller must receive the distinct
   post-respawn audit failure.
6. F6: invoke two replaying processes against one handoff; assert one retained
   row and eventual handoff retirement.
7. F7: hold the flock and separately stall `supervisorEventWriteFn` after lock
   acquisition at both detection sites; the suppressor/relaunch callback must
   be observed before the deferred diagnostic attempt. Moving either
   diagnostic back before the decision must fail.

## Numbered claims

1. `{ guarantee: A tick that may retain the Phase-I GUI lease cannot invoke a GUI-owner relaunch; single-owner: typed Phase-I lease lifecycle result spanning the GUI probe and runEnsureAliveGUIRecovery; enforcement-probe: race-engineered pre/post-acquisition and three-relaunch-path matrix }`.
2. `{ guarantee: Every committed recovery return has either the exact retained audit row or a process-exit-safe handoff; single-owner: SupervisorEventLog pending-handoff lifecycle; enforcement-probe: blocked-writer subprocess-exit persistence test }`.
3. `{ guarantee: One normalized committed event has exactly one row in the active-plus-.1 retained set under late completion, replay, concurrency, and rotation; single-owner: exact-line proof/append/delete transaction under the event-log mutex and flock; enforcement-probe: late-write concurrent-replay rotation cardinality matrix }`.
4. `{ guarantee: Neither pre-action detection site can delay its suppressor or relaunch callback beyond the declared diagnostic budget; single-owner: truly bounded pre-action diagnostic helper or action-before-observability ordering; enforcement-probe: flock-contention and stalled-write callback-deadline tests at both sites }`.
5. `{ guarantee: Replay failure never discards a pending handoff or silently reports a fully audited recovery; single-owner: SupervisorEventLog replay error contract plus daemon-recovery result mapping; enforcement-probe: persist/read/append/sync/rename/delete failure-injection matrix }`.

## Initial gate

**REVISE**

The F6 exact-normalized-byte identity plus active/`.1` deduplication is
sufficient for the explicitly bounded retained-history guarantee when all
emit modes replay under the same locks and full content is verified. The
design is not yet implementable as a reliability-safe package because F5 lacks
a typed, race-free release disposition across the current probe boundary and
F7's selected `TryEmit` still permits an unbounded write-phase stall. The five
plan corrections above are mandatory before implementation.

## Re-verification

Re-verified the revised `design.md` against the same current source at
`db879c5f76e479a7ae568f58ae87de864f3d9f28`. No implementation or test was
performed.

| Mandatory correction | Revised design evidence | Source compatibility check | Result |
| --- | --- | --- | --- |
| 1. Typed atomic acquisition-versus-timeout lifecycle, including internal releases | The lifecycle has `open`, `closed-before-exposure`, `exposed`, `not-acquired`, `released`, and `release-unconfirmed`; timeout and probe perform competing compare-and-swap transitions, and every internal tentative release publishes its outcome (`design.md:94-127`) | The current result lacks this disposition and internal release helpers collapse it, exactly matching the admitted change surface (`internal/gui/single_instance.go:105-117,485-521,614-618`) | **PASS** — one owner, monotonic transitions, and a race-falsifying probe are specified |
| 2. F7 action before observability | Both detection calls are deferred until after suppressor/relaunch decision execution (`design.md:183-196`) | Current calls precede those decisions at `internal/cli/supervise_ensure_alive.go:923-950,1171-1179`; current helper is blocking at `:477-504` | **PASS** — diagnostics may delay return but cannot delay the decision named by the finding |
| 3. Distinct post-respawn audit-durability failure and CLI exit | `FailureAuditDurability`, explicit committed/respawn wording, and a dedicated non-zero exit are named (`design.md:148-159,205-206`) | Current recovery runs queued audit after respawn but has no returned audit failure (`internal/daemonrecovery/recovery.go:545-571`); current CLI reports generic success/error at `internal/cli/daemon_recover.go:121-134` | **PASS** — failure ownership and operational-contract mapping are explicit |
| 4. Every-mode replay, full-content verification, bounds, retain-on-error | Every event-log emit mode replays under the existing mutex+flock before rotation/current append; complete active/`.1` records are compared; pending size/count are bounded; every listed error retains the handoff (`design.md:160-175`) | All modes converge at `SupervisorEventLog.emit` and rotation/current append are owned below it (`internal/api/supervisor_events.go:476-611,691-709,795-821`) | **PASS** — the planner can place one replay prefix without a bypass path |
| 5. Bounded retained-history/process-exit guarantee | The design expressly limits exactly-once to active plus `.1`, guarantees process-exit survival, and excludes power-loss and unbounded historical claims (`design.md:177-181`) | Current retention is active plus one replacing `.1` backup (`internal/api/supervisor_events.go:795-821`), and the current timeout worker is process-local (`:614-682`) | **PASS** — guarantee matches the actual retention and lifetime boundaries |

### Re-verification notes

- The F5 compare-and-swap protocol closes the race identified by the initial
  gate: timeout can prevent exposure, while any probe that wins exposure is
  conservatively `may retain` until a typed terminal outcome. Pre-acquisition
  validation and marker-read failures remain safe without indefinitely
  suppressing later ticks.
- F7 no longer depends on `TryEmit`. Deferring the existing blocking diagnostic
  is sufficient for the supplied progress invariant because callback/decision
  execution precedes the diagnostic. The narrower guarantee does not claim a
  bounded process return.
- F6 exact normalized bytes plus active/`.1` exact-line proof remains
  sufficient for one retained row. The revision now includes the required
  all-mode serialization, full-content collision check, bounded work, and
  retain-on-error rules; those conditions were the prior gate's qualification.
- All required injections remain implementation/quality-assurance gates. Their
  absence at this design-only stage is recorded as `analysis-only`, not as
  runtime verification.

## Final gate

**PASS**

The five mandatory reliability corrections are now concrete, source-compatible,
and falsifiable enough for planning. This is a design acceptance only; product
behavior remains unverified until the named race, process-exit, replay,
rotation, failure-injection, and action-before-diagnostic tests pass.

## Terms and Abbreviations

- F5-F7: the fifth through seventh supplied PR #589 findings.
- GUI: graphical user interface.
- JSONL: JavaScript Object Notation Lines, one complete JSON object per line.
- SLO: service-level objective.
- `may retain`: the current tick cannot prove that its GUI lease was released.
