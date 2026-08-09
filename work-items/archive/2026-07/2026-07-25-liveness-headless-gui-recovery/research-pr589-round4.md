# Research — PR #589 seven-finding current classification

Evidence baseline: local `HEAD db879c5f76e479a7ae568f58ae87de864f3d9f28`;
remote branch head `2d68690f00ec29328453cdcb2a0ef7097321e1dc`;
local commits ahead are `f150be61` and `db879c5f`.

## Classification

| ID | Classification | Current evidence |
| --- | --- | --- |
| F1 | **ALREADY FIXED** | Commit `f150be61` cancels before emitting at `internal/cli/gui_exit_signal.go:129-138`; `TestObserveGUIExitSignal_CancelsBeforeWaitingOnEmit` passes. |
| F2 | **ALREADY FIXED** | Commit `f150be61` passes `emitErr` into fallback selection at `internal/daemonrecovery/recovery.go:511-516` and refuses reacquisition after direct release failure at `:809-816`; the seven-row outcome matrix passes. |
| F3 | **ALREADY FIXED** | Commit `f150be61` resets the marker for every non-Unknown supervisor-down observation at `internal/cli/supervise_ensure_alive.go:1426-1444`; the Alive-interruption sequence test passes. |
| F4 | **ALREADY FIXED** | Commit `f150be61` maps unsupported `LiveUnreachable` with `PIDAlive=false` to Unknown while preserving supported alive-but-unreachable as Alive at `internal/cli/supervise_ensure_alive.go:434-450`; the seven-row verdict matrix passes. |
| F5 | **REAL, open** | Phase I returns no retained-lease outcome at `internal/cli/supervise_ensure_alive.go:516-582`; a release failure is only logged at `:737-750`, then final liveness continues at `:1346-1349`. |
| F6 | **REAL, open** | A worker still pending after respawn returns without a durable carrier at `internal/daemonrecovery/recovery.go:817-825`; the one-shot caller then returns at `internal/cli/daemon_recover.go:121-134`. |
| F7 | **REAL, open** | `emitLivenessEvent` uses unbounded `Emit` at `internal/cli/supervise_ensure_alive.go:477-504`; pre-action calls at `:1171-1179` and `:923-950` can block the recovery callback. |

No finding is `WRONG`.

## Files & symbols

- Signal observer and lifecycle:
  `internal/cli/gui_exit_signal.go:118-151,219-259`.
- Signal-context production wiring and force-kill consumer:
  `internal/cli/gui.go:471-489,687-705`;
  `internal/gui/single_instance.go:1245-1267,1358-1464`.
- GUI-verdict mapper:
  `internal/cli/supervise_ensure_alive.go:375-450`.
- Phase-I lease classifier:
  `internal/cli/supervise_ensure_alive.go:516-797`.
- Headless recovery and confirmation window:
  `internal/cli/supervise_ensure_alive.go:912-981,1062-1279`.
- Final liveness decisions:
  `internal/cli/supervise_ensure_alive.go:1333-1500`.
- GUI-lease acquisition/release outcomes:
  `internal/gui/single_instance.go:452-521,598-635`.
- Event-log modes:
  `internal/api/supervisor_events.go:177-255,337-405,452-469,539-682`.
- Recovery audit/respawn/fallback:
  `internal/daemonrecovery/recovery.go:469-551,734-844`.
- One-shot caller:
  `internal/cli/daemon_recover.go:101-134`.

## Flows and class sweeps

### F1 — cancellation visibility before diagnostics

| Participant | Classification |
| --- | --- |
| Valid SIGINT/SIGTERM | Fixed by `f150be61`: cancel, then emit (`gui_exit_signal.go:129-138`). |
| Closed signal channel | Correct: no fabricated signal or cancellation (`:120-128`). |
| Context canceled without signal | Correct: no false attribution (`:138-143`). |
| Observer stop | Correct: cancel, join, unregister (`:233-237`). |
| Production RunE wiring | Correct: stop is deferred immediately (`gui.go:471-489`). |
| Force-kill final guard and waits | Correct consumers of prompt cancellation (`single_instance.go:1252-1267,1400-1464`). |
| Tray/self-restart emitters | Excluded: independent triggers, not signal-context consumers (`gui.go:1418-1432`; `gui_self_restart.go:173-193`). |

### F2 — tracked/direct audit outcomes

| Outcome | Classification |
| --- | --- |
| Immediate success | Correct: no fallback. |
| No budget/no attempt | Correct for F2: one post-respawn fallback. |
| Lock-acquisition timeout without worker | Correct for F2: one fallback. |
| Direct settled write failure | Correct: one fresh fallback. |
| Direct release failure | Fixed by `f150be61`: no reacquisition (`recovery.go:809-816`). |
| Pending success | Correct: no second write (`:819-821`). |
| Pending still unsettled | Correct for F2 idempotency; F6 remains open (`:822-825`). |
| Pending release failure | Correct for F2: no second write (`:826-836`). |
| Pending definite failure | Correct: one fresh fallback (`:838-843`). |

### F3 — uninterrupted Unknown+unheld window

| Participant | Classification |
| --- | --- |
| Unknown plus held/error flock | Correct: resets window (`supervise_ensure_alive.go:1118-1130`). |
| First Unknown+unheld | Correct: arms marker (`:1133-1144`). |
| Pre-90-second Unknown+unheld | Correct: preserves marker, no escalation (`:1147-1150`). |
| Elapsed Unknown+unheld | Correct: consumes marker before recovery (`:1153-1179`). |
| Running Alive/ConfirmedDead | Correct: reset (`:1373-1383,1403-1411`). |
| Running Unknown | Correct: routes to confirmation owner (`:1384-1401`). |
| Down Alive/ConfirmedDead | Fixed by generalized non-Unknown reset (`:1426-1444`). |
| Down Unknown | Correct: marker remains; recovery is GUI-independent (`:1438-1475`). |
| Reset failure before escalation | Correct: refuses escalation (`:1164-1169`). |

### F4 — identity capability versus liveness fact

| Producer/consumer state | Classification |
| --- | --- |
| Matching ping with unsupported identity | Correct: Healthy to Alive (`single_instance.go:1049-1077`). |
| macOS unsupported, no ping | Fixed: Unknown (`:1080-1097`; mapper `supervise_ensure_alive.go:440-446`). |
| Windows non-amd64 unsupported, no ping | Fixed: Unknown (`single_instance.go:1100-1116`; mapper `:440-446`). |
| Supported identity says alive, no ping | Preserved as Alive (`single_instance.go:1147-1153`; mapper `:447`). |
| Definitive absent PID | Preserved as ConfirmedDead (`single_instance.go:1140-1144`; mapper `:436-437`). |
| Ambiguous platform error | Preserved as Unknown (`single_instance.go:1119-1137`; mapper `:448-449`). |
| Force-kill consumer | Correct: unsupported identity refuses destructive kill (`single_instance.go:771-809,1180-1249`). |

### F5 — retained GUI-lease propagation

| Participant | Classification |
| --- | --- |
| Tentative release after cancellation/revalidation failure | Open: release failure becomes Unknown but does not reach final decision (`single_instance.go:492-515,614-618`). |
| Reservation found after acquisition | Open: same lost outcome (`single_instance.go:507-511`). |
| Worker context done after probe | Open: error-discarding `Release` (`supervise_ensure_alive.go:631-635`). |
| Invalid Held/Unknown/default carrying a lease | Open: error-discarding `Release` (`:641-660`). |
| Free mismatch | Open propagation: `ReleaseErr` emits Unknown, outer return is void (`:701-710`). |
| Free after marker compare-and-swap | Open named instance: `ReleaseErr` emits Unknown, outer return is void (`:730-750`). |
| Deadline while worker holds lease | Open: final liveness proceeds without outcome (`:558-581`). |
| Running+ConfirmedDead | Affected: can re-fire GUI task (`:1373-1383,948-970`). |
| Running+Unknown | Correct/not affected: retained flock is observed as held (`:1118-1130,1384-1401`). |
| Down+ConfirmedDead | Affected: GUI-owner relaunch (`:1478-1499`). |
| Down Alive/Unknown | Correct/not affected: standalone supervisor path does not acquire GUI flock (`:1445-1475`). |
| `db879c5f` supervisor-lock fix | Excluded: different lock and propagation path (`internal/api/supervisor_lock.go:270-315`). |

Existing runtime evidence:
`TestEnsureAliveGUIRecovery_UnconfirmedLeaseReleaseDegradesToUnknown` proves
the local diagnostic downgrade;
`TestEnsureAliveGUIRecovery_ClassifierTimeoutRetainsLeaseUntilCASCompletes`
proves relaunch can be observed while the Phase-I lease remains unreleased.
The exact `ReleaseErr != nil` same-tick suppression guard is missing.

### F6 — committed-audit durability

| Participant | Classification |
| --- | --- |
| Committed clean reap/unconfirmed termination | Both enter the tracked audit path (`recovery.go:493-506`). |
| Fast tracked write | Correct: durable before respawn; guarded by `TestExecuteFastCommittedAuditIsDurableBeforeRespawn`. |
| No budget/no worker | Correct: post-respawn blocking fallback (`recovery.go:511-515,545-551,817-844`). |
| Pending settled before post-respawn peek | Correct: row retained (`:819-821`). |
| Pending still blocked | Open: returns with only in-memory goroutine ownership (`:822-825`). |
| Pending release failure | Correct for no-duplicate F2; durability remains unproven (`:826-836`). |
| Pending definite failure | Correct: blocking fresh attempt (`:838-843`). |
| One-shot CLI caller | Affected: returns immediately after recovery (`daemon_recover.go:121-134`). |
| GUI HTTP caller | Shares worker but normally outlives request; no process-lifetime durability contract (`internal/gui/daemon_recover.go:17-24`). |

`TestExecuteDoesNotHangWhenAuditWorkerNeverSettles` requires recovery to
return; `TestExecuteLateAuditWorkerSettlingAfterReturnProducesExactlyOneRow`
proves only same-process eventual completion. No subprocess/process-exit
survival test exists.

### F7 — liveness emits relative to recovery

| Emit group | Classification |
| --- | --- |
| Generic `emitLivenessEvent` | Open owner: unbounded `logger.Emit` (`supervise_ensure_alive.go:477-504`). |
| Phase-I diagnostics | Correct for progress: inside budgeted worker (`:648,757,769,776,794`; deadline `:576-581`). |
| Unknown reset/arm/consume failures | Correct/not affected: failure branches authorize no recovery (`:1125,1139,1165`). |
| Unknown escalation event | Open sibling: blocking emit precedes headless recovery (`:1171-1179`). |
| Headless detection event | Open named instance: blocking emit precedes suppressors/relaunch (`:923-950`). |
| Suppress event | Correct/not affected: no recovery intended (`:978`). |
| Relaunch success/failure events | Correct/not affected: after callback (`:952,964`). |
| Running Unknown no-action event | Correct/not affected (`:1397`). |
| Down-branch success/failure events | Correct/not affected: after callbacks (`:1460,1471,1485,1495`). |

No exact test wedges the event-log lock and proves either pre-action site still
reaches the recovery callback.

## Contracts

- F1: cancellation is visible before diagnostic blocking; joined attribution
  remains intact.
- F2: synchronous and pending release failures do not cause same-process
  fallback reacquisition.
- F3: one uninterrupted Unknown+unheld window owns escalation.
- F4: unsupported identity cannot assert Alive; supported
  alive-but-unreachable remains Alive.
- F5: Phase I can know or suspect a retained lease, but final liveness cannot
  observe that outcome.
- F6: an unsettled pending audit is process-memory-only after respawn.
- F7: two blocking observability calls precede recovery callbacks.

## Tests & coverage

All executed commands used fresh isolated state directories and
`-tags=test_state_path_env`.

```text
go test ... -run '^(TestObserveGUIExitSignal_CancelsBeforeWaitingOnEmit|TestEnsureAlive_SupervisorDownTickResetsUnknownConfirmationMarker|TestClassifyGUIOwnerVerdict_Matrix|TestEnsureAliveGUIRecovery_UnconfirmedLeaseReleaseDegradesToUnknown)$' ./internal/cli/
ok   mcp-local-hub/internal/cli 0.108s

go test ... -run '^TestEnsureAliveGUIRecovery_ClassifierTimeoutRetainsLeaseUntilCASCompletes$' ./internal/cli/
ok   mcp-local-hub/internal/cli 0.128s

go test ... -run '^(TestQueueIdempotentAuditFallbackOutcomeMatrix|TestExecuteDoesNotHangWhenAuditWorkerNeverSettles|TestExecuteLateAuditWorkerSettlingAfterReturnProducesExactlyOneRow)$' ./internal/daemonrecovery/
ok   mcp-local-hub/internal/daemonrecovery 0.359s
```

F1–F4 have current passing guards. F5 lacks an exact end-to-end release-error
suppression guard; F6 lacks a process-exit durability guard; F7 lacks a wedged
log/pre-action progress guard.

## Similar implementations

- The event-log owner exposes blocking `Emit`, bounded `EmitWithTimeout`,
  tracked `EmitWithTimeoutTracked`, and lossy `TryEmit` with distinct contracts
  at `internal/api/supervisor_events.go:337-405`.
- Phase I already bounds blocking work behind an outer deadline at
  `internal/cli/supervise_ensure_alive.go:558-581`.
- `SupervisorRunningUnderStateDir` reports tentative supervisor-lock release
  failure through an existing error channel at
  `internal/api/supervisor_lock.go:270-315`; this is the same resource-lifetime
  class as F5 but a different lock.

## Constraints

- No GUI, tray, supervisor, daemon, scheduler, installed binary, real loopback
  probe, process kill, broad test, commit, push, stash, reset, or checkout
  restore was used.
- Go language-server initialization timed out; read-only Git/source inspection
  was the approved fallback.

## Change risks

- Signal attribution has four successive correction commits; join, causal
  single registration, and cancel-before-emit are separate invariants.
- GUI-owner recovery has repeated fixes; F5 remains outside the last two local
  closures.
- Audit fallback has three successive ownership changes. F2 no-reacquire does
  not imply F6 process-exit durability.
- F5 intersects an existing test that deliberately lets outer liveness
  continue while Phase I still owns a lease during a blocked compare-and-swap.
- F7 is a two-site class; changing only the named headless detection site leaves
  Unknown escalation open.

## Unresolved questions

1. F3 lacks a direct supervisor-down+ConfirmedDead regression row, although the
   generalized predicate covers it statically.
2. F5 lacks the exact full-run `ReleaseErr != nil` suppression test.
3. F6 has no durable carrier or process-exit test for an unsettled worker.
4. F7 has no injectable wedged-log test at either pre-action site.

## Research admission gates

| Gate | Result |
| --- | --- |
| Regression risk | PASS: sibling behavior and conflicting existing tests are enumerated. |
| Metric alignment | PASS: F5 measures relaunch, F6 process-exit durability, F7 callback progress. |
| Known limits | PASS: uncancellable writes, process exit, persistent release failure, and Phase-I deadline are explicit. |
| Bounded falsification | PASS: each open class has one deterministic missing guard. |

## Adjacent findings

None. Release-error siblings and the second pre-action emit are members of the
supplied defect classes.

## Searched and excluded

- `db879c5f` supervisor-lock release: different lock.
- Tray/self-restart emitters: independent F1 triggers.
- Post-action liveness events: cannot block an already-invoked action.
- Prior reports/bug notes: search hypotheses only, not authority.
- Further widening stopped after event-log ownership and GUI-lease
  producer/caller sweeps changed no classification.

## Gate

**PASS** — all seven findings are classified; each defect class has a current
participant inventory; the four local closures map to `f150be61`; the three
open findings have explicit missing falsification guards.
