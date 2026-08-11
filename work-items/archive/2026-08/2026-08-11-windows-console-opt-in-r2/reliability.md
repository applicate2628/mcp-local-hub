# Reliability design package — broad CLI and GUI lifecycle failures

## Summary

This diagnosis changes the owning boundary for one reported family and narrows the other two to test-lifecycle code. No product code or console contract needs alteration.

| Family | Deterministic finding | Candidate versus immutable `HEAD` | Single owner |
| --- | --- | --- | --- |
| CLI `ReconcileIPC` / `EventLoop` | The five-minute package alarm sampled whichever reconcile sibling had just started. Candidate was in its named test for 0 seconds and `HEAD` for 2 seconds when the package-wide timer expired. With a 12-minute package bound, both complete the entire ReconcileIPC family. | Baseline command-budget defect; not caused by the console candidate. Candidate finishes in 378.750 s and `HEAD` in 361.212 s, then each reports different adjacent failures outside this family. | The broad CLI gate command/budget. `ReconcileIPC`, `EventLoop`, and state-file code are not correction targets for this symptom. |
| GUI broadcaster workers | Four handler-test construction sites create exactly nine persistent workers and never close their `Server` broadcaster: a five-row matrix, two single tests, and a two-row matrix. | Baseline test-lifecycle defect. Candidate, repeat candidate, and `HEAD` all report `drainPersist=9`; relevant sources are byte-identical to `HEAD`. | `newEphemeralServer` is the existing test-lifetime owner; every handler-style `daemon_recover_test.go` server must use it. |
| Audit-lock helper acquisition | One 80 ms context owns process startup, helper test-binary bootstrap, flock acquisition, marker write, timeout, and reap. The parent waits one second for a marker after that same 80 ms context can already have killed the helper. | Baseline test-synchronization defect. It failed once only in candidate broad; candidate repeat, candidate exact, `HEAD` broad, and `HEAD` exact passed. A green rerun does not validate the transient window. | The test's injected `auditLockTerminalRunner`; split acquisition/reap from deadline classification and engineer both conditions. |

The broad packages remain release-red for independent failures. Candidate CLI reports quiesce settlement and a temporary-directory cleanup failure; immutable `HEAD` reports a different temporary-directory cleanup failure. Those are not evidence against the Reconcile diagnosis and need separately admitted owners. The accepted Windows target console result remains 6/6 top-level and 63/63 subtests GREEN (`work-items/archive/2026-08/2026-08-10-windows-console-opt-in/qa.md:48-63`).

## Artifact

### Scope and method

- Active scope and protected target evidence are defined in `work-items/active/2026-08-11-windows-console-opt-in-r2/brief.md:7-18` and `status.md:14-25`.
- The prior five-minute candidate/`HEAD` commands, exact controls, and GUI repeats are recorded at `work-items/archive/2026-08/2026-08-10-windows-console-opt-in/qa.md:29-43`. Prior root language was treated as a hypothesis.
- CodeGraph was queried first. Its CLI status reported an up-to-date 2,040-file index. When the MCP response marked a specific file pending watcher sync, only that file was read directly; no stale response drives this package.
- All current owner files in this package have zero `git diff --name-only HEAD` entries; receipt: `.scratch/windows-console-contract/r2-reliability/relevant-source-diff.txt`.
- No Windows target console suite was repeated.

### Phase 1 — concrete data

| Probe | Observed result | Decision |
| --- | --- | --- |
| Prior exact Reconcile candidate / `HEAD` | PASS / PASS; 11.913 s / 12.266 s (`qa.md:38`) | Isolated success alone did not clear the broad alarm. |
| Prior five-minute CLI candidate / `HEAD` | Both package-timeout. The sampled sibling differs (`qa.md:39`, bug record `work-items/bugs/2026-08-10-cli-reconcile-broad-timeout.md:10-14`). | A package timer is not a per-test elapsed-time measurement. |
| Candidate CLI, `go test -count=1 -timeout 12m ./internal/cli` | Exit 1 after package 378.750 s; every ReconcileIPC test completed. Failures: `TestQuiesceHandler_MixedDrainedAndStillRunning` and `TestSuperviseCommand_CallsReaperBeforeReconcileReady`. Receipt: `.scratch/windows-console-contract/r2-reliability/candidate-cli-broad-timeout12m.txt`. | Falsifies a Reconcile/EventLoop five-minute hang. Broad CLI remains red for separate families. |
| Immutable `HEAD`, same 12-minute command | Exit 1 after package 361.212 s; every ReconcileIPC test completed. Failure: `TestSupervisorController_StaleLivenessDisownDroppedByGeneration` temporary-directory cleanup. Receipt: `.scratch/windows-console-contract/r2-reliability/head-cli-broad-timeout12m.txt`. | Confirms the command-budget cause on the immutable control; adjacent failures are order/global-lifecycle participants, not one shared Reconcile root. |
| Prior GUI candidate / repeat / `HEAD` | `drainPersist=9 runDropReporter=0` in all three (`qa.md:40-42`; `work-items/bugs/2026-08-10-gui-broadcaster-workers-leak.md:10-13`). | Baseline, broad-order reproducible. |
| Targeted committed-result matrix | Assertions pass, package oracle reports `drainPersist=5`. Receipt: `.scratch/windows-console-contract/r2-reliability/gui-daemon-recover-committed-matrix.txt`. | Five table rows each create one unowned worker. |
| Targeted tail group and exact members | Group reports 4; exact results are success-safe-fields=1, release-unconfirmed=1, durable-handoff=0, respawn-failure matrix=2. Receipts: `.scratch/windows-console-contract/r2-reliability/gui-daemon-recover-tail-suspects.txt` and the four `gui-TestDaemonRecoverRoute*.txt` files. | `5 + 1 + 1 + 2 = 9`; no unexplained broadcaster worker remains. |
| Prior audit-lock broad and exact controls | First candidate broad fails marker acquisition; identical candidate broad repeat passes; exact candidate and `HEAD` pass; `HEAD` broad passes (`qa.md:40-42`; bug record `work-items/bugs/2026-08-10-audit-lock-helper-broad-flake.md:10-15`). | Timing dependence is observed; the current test does not deterministically create the acquisition window. |

### Phase 2 — hypothesis inventory and falsification

| Hypothesis | Falsification | Verdict |
| --- | --- | --- |
| A1. A Reconcile sibling blocks for five minutes. | Five-minute stacks show the named tests active for only 0–2 seconds; both 12-minute controls complete the full family. | Refuted. |
| A2. A leaked EventLoop or state-file activity makes the Reconcile family nonterminal. | Candidate and immutable `HEAD` both complete that family under the extended package budget. | Refuted for the reported broad timeout. Fixture cleanup still lacks an explicit join and remains a separate unverified hygiene invariant. |
| A3. The console candidate indirectly caused the CLI alarm. | Relevant files are byte-identical to `HEAD`; both candidate and immutable `HEAD` exhibit the five-minute cutoff and complete Reconcile under 12 minutes. | Refuted. |
| B1. `Broadcaster.Close` cannot stop its persist worker. | Existing `Close` closes the channel and joins the worker (`internal/gui/events.go:199-240`); explicitly owned daemon-recovery server sites do not contribute to the exact count. | Refuted as the nine-worker root. |
| B2. Direct handler-test `NewServer` calls omit the broadcaster owner. | Four targeted construction groups reproduce exactly 9. `newEphemeralServer` already disables persistence before exposure and registers cleanup (`internal/gui/test_server_lifecycle_test.go:22-30`). | Verified root. |
| C1. Production audit-lock containment fails to reap. | Current exact candidate/`HEAD` tests pass and `RunStrictlyContained` closes its job, waits for the child, joins all three stream workers, and closes parent files before return (`internal/process/run_strictly_contained.go:109-140,186-257`). | Not established as a production defect. |
| C2. The test gives the helper a deterministic chance to acquire the flock before timeout. | The 80 ms budget is installed before the runner (`internal/gui/audit_lock_terminal_worker_test.go:61-77`); only the helper writes the marker after lock acquisition (`:48-58`); the parent marker poll lasts one second (`:90-99`) but cannot extend the killed child's context. | Refuted; verified test-synchronization root. |

### Family A — CLI owner model

`newReconcileTestFixture` creates isolated state roots, starts one `EventLoop`, and registers only `cancel` as cleanup (`internal/cli/supervise_reconcile_ipc_test.go:60-134`). `EventLoop.Run` returns on context cancellation (`internal/api/supervisor_event_loop.go:259-283`). The synchronous refresh has three bounded modes: inline application when no loop is configured, context-aware post, and a context-aware wait for the completion barrier (`internal/cli/supervisor_controller.go:1128-1230`). A deterministic full/stopped-loop regression already exercises the bounded post path (`internal/cli/supervise_reconcile_ipc_test.go:1754-1830`).

The five-minute command supplied one timer to the entire package. It expired after roughly 300 seconds, so the stack named the test executing at package exhaustion, not the activity that consumed the preceding budget. The 12-minute candidate and `HEAD` controls are the minimal falsification of the real symptom.

#### CLI defect-class inventory

All named `TestReconcileIPC_*` participants share some combination of fixture EventLoop, state roots, registry/intent refresh, scheduler seams, timers, environment hooks, and cleanup. None is fixed by this package; all are **not affected** for the false five-minute-hang attribution. The two stack-named siblings are explicitly identified below.

| Source / participants | Shared mechanism | Classification |
| --- | --- | --- |
| `supervise_reconcile_ipc_test.go`: `DryRunReturnsDriftWithoutMutating`, `ApplyPostsEvIntentUpdate`, `SchedulerUnavailableTreatsSupervisorOwnedRowsAsMissing`, `ApplyExcludesOrphanedLSPDescriptor`, `ApplyTerminatesRunningOrphanedLSPDescriptor`, `ApplyTerminatesBackoffOrphanedLSPDescriptor`, `LSPRegistryReadFailureLeavesDescriptorAlone`, `ApplyRefreshesCacheBeforePosting` (`internal/cli/supervise_reconcile_ipc_test.go:201-758`) | Core fixture, EventLoop, state files, intent cache, scheduler fakes. | Not affected by console candidate; potential fixture-lifecycle participants only. All complete in both 12-minute controls. |
| `supervise_reconcile_ipc_test.go`: `ApplyRefreshesCacheFromSubBlockDespiteCorruptDaemonIntent`, `ApplyRefreshesCacheFromSubBlockDespiteDaemonReadError`, `ApplyPreservesDaemonIntentCacheOnMissingSupervisorIntent`, `HandlerTimeoutCancelsSchedulerList`, `HandlerTimeout`, `NoDriftReturnsEmptyArray`, `MissingTaskSpawnsViaIntentUpdate`, `SupervisorOwnedMissingTaskAppliesStart` (`:759-1329`) | State-read error paths, handler timers/cancellation, intent refresh. | Not affected; all complete in both controls. |
| `supervise_reconcile_ipc_test.go`: `OrphanSchedulerTaskFlaggedNeedsManualReview`, `AuditEventEmitted`, `TimeoutErrors`, `DaemonIntentStopOverridesDefault`, `ExpiredUserStopClassifiesDesiredRunning`, `R8_BareKeyDescriptorDriftResolvesViaLookupCanonical` (`:1330-1514,1845`) | Audit sink, timeouts, stop state, registry lookup. | Not affected; all complete in both controls. |
| `supervise_reconcile_stop_test.go`: `ResidualStoppedSchedulerStoppedIntentTerminatesLiveDaemon`, `SupervisorOwnedStoppedIntentTerminatesLiveDaemon`, `SupervisorOwnedStoppedIntentQuarantinedStaysNoOp`, `QuarantinedBystanderNotRevivedOnStop`, `BackoffBystanderNotRespawnedOnApply`, `RunningBystanderNotRepostedOnApply`, `ReadsStopFromSupervisorIntentSubBlock`, `RegularGlobalDaemonDispatchedThroughSM` (`internal/cli/supervise_reconcile_stop_test.go:101-592`) | Stop/registry/intent refresh, EventLoop, scheduler state. | Not affected. `BackoffBystander...` and `ReadsStop...` were incidental `HEAD`/candidate five-minute stack owners; they complete in 12-minute controls. |
| `supervise_reconcile_serena_repair_test.go`: `ApplyRepairsSerenaIntentFromRegistryBeforeDrift`, `DryRunDoesNotRepairSerenaIntentFromRegistry`, `SerenaRepairLockSkipsRemainOK` (`internal/cli/supervise_reconcile_serena_repair_test.go:84-304`) | Registry repair and repair lock. | Not affected; potential global lock/order participant for package duration, not observed as a terminal Reconcile failure. |
| `reconcile_serena_repair_failure_test.go`: `ApplyRepairFailureIsReportedNotSwallowed`, `DryRunPreviewFailureIsReported` (`internal/cli/reconcile_serena_repair_failure_test.go:89-138`) | Registry repair failure/error return. | Not affected; completes in both controls. |
| `supervise_target_settlement_test.go`: `TargetSettlementIsReturnedForExactGeneration` (`internal/cli/supervise_target_settlement_test.go:364`) | Settlement generation and refresh. | Not affected; completes in both controls. |
| Fixture cleanup `t.Cleanup(cancel)` | Cancels but does not join EventLoop (`internal/cli/supervise_reconcile_ipc_test.go:134`). | Potential participant in a distinct resource-hygiene defect. Not the cause of the reported alarm; explicit join proof is unverified. |
| Package test command | One package-wide timeout for hundreds of tests. | **Root owner** for the reported five-minute Reconcile attribution. Fixed operationally by using a measured broad bound; no source fix exists yet. |

#### Named CLI regression guard

`TestReconcileIPC_EventLoopAndStateFileLifecycle_OrderSequence` is the owner-level guard proposal. It must run an alternating multi-scenario sequence covering the two formerly stack-named stop tests plus the full/stopped-loop refresh barrier. Each scenario creates a fixture, explicitly closes it, waits on a fixture-owned `loopDone`, then proves its state root can be removed and recreated before the next scenario. Expected result: every loop joins inside a 1-second test watchdog, every state root is removable, and the subsequent scenario completes. A goroutine-count delta is not the oracle.

Execution: **ASSUMPTION (UNVERIFIED)** because read-only Phase 1 cannot add the fixture `Close`/`loopDone` seam or guard. Resolving step: add the RED guard at `newReconcileTestFixture`, observe failure caused by the absent join, add only fixture-owned join cleanup, then run the named guard and `go test -count=1 -timeout 12m ./internal/cli`. This is reliability hardening, not a correction for the refuted five-minute Reconcile hang.

### Family B — GUI broadcaster owner model

`NewServer` always creates one broadcaster (`internal/gui/server.go:924-988`). A persistable `Publish` lazily starts one `drainPersist` worker (`internal/gui/events.go:350-397`). `Close` is idempotent, closes both worker inputs, and bounds both joins (`internal/gui/events.go:199-240`). Production `Server` shutdown owns `Close` on activation failure, cancellation, and serve failure (`internal/gui/server.go:1202,1232,1393,1411`). The leak is confined to handler tests that construct a server but never enter production `Start` and never register the test cleanup owner.

#### Broadcaster participant inventory

| Participant / path | Creation, registration, drain, persist, shutdown | Classification |
| --- | --- | --- |
| `NewBroadcaster` / `ensurePersistDrain` / `Publish` | Constructor creates channels; first persistable publish starts exactly one worker (`internal/gui/events.go:141-159,350-397`). | Fixed mechanism; not defective for the nine-worker count. |
| `drainPersist` | Ranges the persist channel and closes `persistDoneCh` (`internal/gui/events.go:243-248`). | Worker observed in all nine leaked stacks; symptom, not owner. |
| Drop reporter | Starts only after a dropped event; broad oracle reports zero (`internal/gui/events.go:377-395`; bug record lines 10-11). | Not affected. |
| `Broadcaster.Close` | `sync.Once`; normal close, never-started close, persist timeout, reporter timeout, and repeated/re-entry calls are bounded (`internal/gui/events.go:199-240`). | Not affected as root. A timeout can deliberately return before a blocked worker exits; package oracle exposes that degradation. |
| `newEphemeralBroadcaster` / `newEphemeralServer` | Disable disk persistence before exposure; register `t.Cleanup(Close)` (`internal/gui/test_server_lifecycle_test.go:11-30`). | Existing single test owner; required implementation boundary. |
| Package oracle | Counts `drainPersist` and `runDropReporter` only after test cleanup (`internal/gui/test_server_lifecycle_test.go:33-52`; invoked by `internal/gui/main_test.go:246`). | Correct detection owner; preserve unchanged. |
| `daemon_recover_test.go` lines 81, 110, 486 | Direct `NewServer` plus explicit event cleanup (`internal/gui/daemon_recover_test.go:80-112,485-488`). | Fixed/owned already. Converting to the helper is optional consistency, not required to remove the nine workers. |
| `TestDaemonRecoverRouteCommittedFlagPreservesHTTPMatrix` line 248 | Five server instances; persistent route events; no cleanup. | Verified leak source: 5. |
| `TestDaemonRecoverRouteSuccessReturnsOnlySafeAcceptedFields` line 433 | One server; persistent route event; no cleanup. | Verified leak source: 1. |
| `TestDaemonRecoverRouteReleaseUnconfirmedIsAWarningOnSuccessNotAFailure` line 540 | One server; persistent route event; no cleanup. | Verified leak source: 1. |
| `TestDaemonRecoverRouteRespawnFailureReportsCommittedTerminationWithoutProcessDetail` line 627 | Two table instances; persistent route events; no cleanup. | Verified leak source: 2. |
| Same-origin, explicit-confirmation, known-contract, durable-handoff sites | Direct unowned servers at `internal/gui/daemon_recover_test.go:274,301,404,570`; targeted known-contract and durable-handoff probes produce zero current drain workers. | Potential participant. Must use the owner helper because a future publish would leak; current zero is not ownership proof. |
| Production `Server.Start` and continuation | Activation failure and both terminal serve branches call `events.Close` (`internal/gui/server.go:1202,1232,1393,1411`). | Not affected. |

#### Named GUI broadcaster regression guard

`TestDaemonRecoverHandlers_OwnBroadcasterLifecycle` must cover all eight currently unowned direct-handler sites in one package process, with the existing `assertNoBroadcasterWorkers` oracle after cleanup. Expected result: `drainPersist=0 runDropReporter=0`; route responses and in-memory events remain unchanged. Observed pre-fix result: exact component counts 5, 1, 1, 0, and 2, totaling 9. Minimal implementation: replace the eight direct unowned `NewServer` calls at `daemon_recover_test.go:248,274,301,404,433,540,570,627` with `newEphemeralServer(t, Config{...})`. Do not edit production broadcaster/server code.

### Family C — audit-lock owner model

`terminalizeBounded` creates one context for store-mutex acquisition and the injected runner, forwards only the remaining milliseconds, maps timeout/containment errors to safe failure IDs, and always releases `storeMu` through `defer` (`internal/gui/audit_lock_state.go:1441-1495`). `RunStrictlyContained` owns its job, child, input writer, two output drains, and parent file endpoints on validation, pipe, start, cancel/timeout, child-exit, and cleanup-error paths (`internal/process/run_strictly_contained.go:89-257`).

The test's condition is inverted. It sets an 80 ms outer budget, then starts a full copy of the Go test binary with that context. The child can only publish `entered` after acquiring the flock (`internal/gui/audit_lock_terminal_worker_test.go:48-58,63-81`). Under broad load, deadline cancellation can reap it before publication; the one-second parent poll then waits for an event that can no longer occur (`:83-103`).

#### Audit-lock participant inventory

| Participant / return path | Ownership and cleanup | Classification |
| --- | --- | --- |
| Mutation-epoch precheck | Returns stale before lock or child (`internal/gui/audit_lock_state.go:1441-1444`). | Not affected. |
| Store-mutex retry timer | Context bounds retries; timeout publishes uncertain settlement/failure (`:1445-1456`). | Not affected; timer is context-owned. |
| Remaining allowance | Nonpositive allowance returns uncertain without child (`:1457-1463`). | Not affected. |
| Injected `auditLockTerminalRunner` | Receives the same context; sole process-launch seam (`:1464-1473,1547-1561`). | **Single deterministic test owner.** |
| Runner error/timeout/containment | Publishes one safe failure ID and uncertain receipt (`:1474-1495`). | Already covered by injected result/failure tests (`internal/gui/audit_lock_terminal_worker_test.go:247-303,350-395`). |
| Protocol result branches | Durable, stale, uncertain/rejected, invalid/default all classify before return (`internal/gui/audit_lock_state.go:1497-1537`). | Not affected. |
| Strict runner invalid/job/pipe/start failures | No child or all opened files/jobs are closed before return (`internal/process/run_strictly_contained.go:89-202`). | Not affected by broad acquisition failure. |
| Strict runner child exit | Wait, input writer, stdout/stderr drains, job, and files join before return (`:205-257`). | Not affected. |
| Strict runner cancellation/timeout | Terminates job/process group/process, closes job, waits for child, then joins all streams (`:223-257`). | Containment owner; source analysis verifies explicit cleanup. |
| Blocking helper | Acquires flock, writes marker, then sleeps; acquisition/write failures exit 2/3 (`internal/gui/audit_lock_terminal_worker_test.go:48-60`). | Correct condition publisher. |
| Parent test poll | Polls marker for 1 second after launching a child whose lifetime is only 80 ms (`:63-103`). | **Root test defect.** Natural scheduling controls success. |
| Post-return probes | Requires child dead, `storeMu` acquirable, and fresh flock acquirable (`:105-125`). | Correct cleanup oracle; preserve. |
| Retry/re-entry | Each terminalization call has a new context; route settlement validates epoch and receipt identity before durable publication (`internal/gui/audit_lock_state.go:1441-1537`). | Not implicated in the failed acquisition window. |

#### Named deterministic audit-lock guards

1. `TestAuditLockTerminalWorkerCancellationAfterAcquisitionReapsBeforeReturn`: give terminalization a multi-second safety bound. In the injected runner, derive a cancellable child context, start the same contained helper, wait conditionally for `entered`, cancel only after the marker proves the flock is held, join the watcher, then preserve the existing child-dead, `storeMu`, and fresh-flock assertions. Expected: a contained timeout/cancellation result, uncertain receipt, dead PID, unlocked mutex and flock.
2. `TestAuditLockTerminalizationDeadlineClassifiesWithoutProcessStartup`: inject an in-process runner that blocks on `ctx.Done()` and returns `StrictRunTimeout`. Expected: the configured short outer deadline maps once to the timeout failure ID and uncertain receipt, with `storeMu` released. This separates deadline semantics from Windows process-start timing.

Execution: **ASSUMPTION (UNVERIFIED)** because the deterministic guards do not exist and Phase 1 forbids edits. Resolving step: write both RED guards at the existing runner seam in `internal/gui/audit_lock_terminal_worker_test.go`, observe the acquisition guard fail until cancellation is marker-gated, then run both exact guards and the 12-minute GUI package command. No production seam or production source edit is required.

### All-return-path resource matrix

| Resource | Normal | Error | Cancellation / timeout | Package teardown / retry | Verdict |
| --- | --- | --- | --- | --- | --- |
| Reconcile EventLoop | `Run` selects until context end. | Fixture construction errors occur before loop start. | Cleanup calls `cancel`; handler post/wait uses context. | No explicit loop join before next test. | **ASSUMPTION (UNVERIFIED)** for join; exact resolving guard is `TestReconcileIPC_EventLoopAndStateFileLifecycle_OrderSequence`. |
| Reconcile state files / audit log | Per-test state roots and environment are isolated. | Reads/writes return typed errors through the handler. | A package alarm terminates the process, not one test transaction. | Root-removability after loop join is not asserted. | **ASSUMPTION (UNVERIFIED)**; same guard must prove root removal/re-entry. |
| Reconcile timers/global hooks | Handler contexts and test environment cleanup bound local hooks. | Timeout paths have named tests. | Context cancellation propagates. | 12-minute candidate/`HEAD` controls prove family completion, not zero residual resources. | VERIFIED for reported symptom; unverified for zero-residue hardening. |
| Broadcaster persist worker | Channel close drains then closes done. | Persistence is best-effort; Close still joins within bound. | Close returns after `closeDrainTimeout` if storage stalls. | `sync.Once` makes re-entry safe; package oracle reports residue. | VERIFIED mechanism; missing handler-test owner is the defect. |
| Broadcaster reporter | Stop channel requests final report and join. | Blocking audit write is bounded by Close wait. | Timeout returns and package oracle exposes survivor. | Not spawned in observed family. | VERIFIED by source; observed count zero. |
| Audit contained process/job/streams | Child and three stream goroutines join. | Validation, pipe, start, wait, and cleanup errors retain typed cause. | Job/process tree terminated before wait; streams join afterward. | Per-call ownership; no package-global process. | VERIFIED by source owner chain; deterministic acquisition injection remains unverified. |
| Audit flock/store mutex/timer/watcher | Helper marker proves flock; `defer` releases mutex. | Failed acquisition exits; runner error publishes uncertainty. | Current timer can cancel before marker. Proposed watcher requires explicit cancel/join on every path. | Existing probes assert re-entry. | Current race is VERIFIED; proposed guard execution is **ASSUMPTION (UNVERIFIED)**. |

### Reliability service-level objectives

| SLO | Service-level indicator and measurement | Window / threshold | Error-budget consequence |
| --- | --- | --- | --- |
| CLI broad gate terminality | Wall duration and terminal package result from `go test -count=1 -timeout 12m ./internal/cli`; attribute a timeout only with per-test elapsed evidence. | Every release candidate and immutable control when attribution is disputed. Zero package timeout alarms; 12-minute safety bound. Current measured maxima: 378.750 s candidate, 361.212 s `HEAD`. | Any package alarm exhausts the release budget and blocks; collect package profile/order evidence instead of naming the sampled active test. |
| Test broadcaster ownership | `assertNoBroadcasterWorkers` count after all test cleanups. | Every `internal/gui` package run; exactly `drainPersist=0` and `runDropReporter=0`. | Any survivor exhausts the budget and blocks the package. No rerun-to-green. |
| Audit containment after acquired-lock cancellation | Marker proves acquired flock; terminal return proves dead PID, unlocked `storeMu`, and fresh flock. | Every exact guard and GUI broad run; zero survivors and all probes complete inside a one-second post-cancel watchdog. | Any survivor, missing marker, or unreleased lock exhausts the budget and blocks. Missing marker is synchronization failure, not containment evidence. |

### Degradation, recovery, observability, and rollback

| Concern | Required behavior |
| --- | --- |
| CLI degradation | A broad command exceeding its measured budget fails loud with package timeout. It must not convert the currently sampled goroutine into a root-cause label. Recovery is a bounded rerun with a larger evidence bound only when classification is open, followed by a separately owned defect for the actual terminal failures. |
| Broadcaster degradation | A blocked disk/audit persistence worker may outlive the bounded `Close` wait; the package oracle exposes it. Handler tests avoid the unrelated persistence side effect through `newEphemeralServer` while preserving routes and in-memory event delivery (`internal/gui/test_server_lifecycle_test.go:22-30`). |
| Audit degradation | Terminal worker failure remains fail-loud as an uncertain settlement plus stable safe failure ID (`internal/gui/audit_lock_state.go:1474-1495`). No raw stream content enters the event. |
| Detection latency | CLI: at package bound. Broadcaster: package `TestMain` after cleanup. Audit: exact guard within marker safety bound plus one-second post-cancel watchdog. These are continuous-integration failures; no operator page is warranted because the changes are test-only. |
| Recovery | Stop/reap/join resources at their owner; prove fresh state-root/flock acquisition. Never retry a failed audit operation as durable success. Never hide a broadcaster survivor by closing it in `TestMain`. |
| Observability | Preserve `TEST_BROADCASTER_LIFECYCLE_LEAK drainPersist=<n> runDropReporter=<n>`, strict-run failure kinds, audit stable failure IDs, package elapsed time, and the exact active-test elapsed shown by Go timeout stacks. |
| Rollout | Backend owner writes RED guard(s), applies only the test-owner correction, runs exact guards, targeted groups, then 12-minute broad CLI/GUI gates. Abort on any worker/PID/lock residue, route-response change, timeout, or change to the exact `--debug-console` grammar/default no-console behavior. |
| Rollback | Changes are test-only and must be one local implementation commit. Because the working tree is user-dirty, rollback is the exact inverse patch for that commit, never a broad reset of user changes. Drill status: **ASSUMPTION (UNVERIFIED)**; resolving step is apply the inverse patch in isolated disposable evidence state, run exact guards, then restore the implementation patch. |

### Minimal downstream implementation surface

| Owner | Allowed surface | Forbidden expansion |
| --- | --- | --- |
| CLI Reconcile family | No product/source correction. QA command uses the measured 12-minute broad bound. Optional RED-first fixture hardening is limited to `newReconcileTestFixture` and its named owner-level guard. | No changes to `ReconcileIPC`, EventLoop production logic, state-file locking, exact leading `--debug-console`, or console creation flags. |
| GUI broadcaster | `internal/gui/daemon_recover_test.go`: replace eight unowned constructors with `newEphemeralServer`. Existing helper/oracle may be referenced unchanged. | No production `events.go`, `server.go`, persistence semantics, or TestMain cleanup that hides leaks. |
| Audit lock | `internal/gui/audit_lock_terminal_worker_test.go`: split into deterministic acquisition/reap and timeout-classification guards using the existing runner injection. | No production audit protocol, timeout, flock, containment, event, or receipt behavior changes. |
| Adjacent CLI failures | Separate admission and owner diagnosis for quiesce and temporary-directory cleanup families. | Do not fold them into the Reconcile or console task without their own evidence chain. |

### Decision-driving claims

1. **Guarantee:** a package timeout is attributed only after the named test is shown to consume the bound. **Single owner:** broad test command/budget. **Enforcement probe:** candidate and immutable `HEAD` `-timeout 12m` controls plus per-test elapsed in any future timeout stack.
2. **Guarantee:** every handler-test server has exactly one broadcaster lifetime owner before any route can publish. **Single owner:** `newEphemeralServer`. **Enforcement probe:** `TestDaemonRecoverHandlers_OwnBroadcasterLifecycle`, expected zero persist/reporter workers after cleanup.
3. **Guarantee:** audit containment cancellation occurs only after the helper proves it owns the flock when testing post-acquisition reaping. **Single owner:** injected `auditLockTerminalRunner`. **Enforcement probe:** `TestAuditLockTerminalWorkerCancellationAfterAcquisitionReapsBeforeReturn`, expected dead PID and immediately reacquirable mutex/flock.
4. **Guarantee:** short audit deadline classification never depends on process startup speed. **Single owner:** injected in-process runner. **Enforcement probe:** `TestAuditLockTerminalizationDeadlineClassifiesWithoutProcessStartup`, expected one timeout failure ID and uncertain receipt.
5. **Guarantee:** no reliability correction weakens the exact leading `--debug-console` opt-in or default no-visible-console behavior. **Single owner:** existing console composition contract. **Enforcement probe:** accepted 6/6 top-level and 63/63 subtests remain the must-not-break baseline; rerun only if relevant source bytes change (`brief.md:7-12`).

Verification strength: CLI package-budget root is **verified-by-current-environment controls**. GUI broadcaster root is **verified-by-targeted reproduction**. Audit-lock synchronization root is **analysis-only plus historical timing evidence**; deterministic injection remains **ASSUMPTION (UNVERIFIED)** until the named guards execute.

## Risks/Unknowns

| Item | Status | Resolving step |
| --- | --- | --- |
| CLI fixture cancellation has no explicit EventLoop join. | **ASSUMPTION (UNVERIFIED)** as a zero-residue invariant; refuted as the reported five-minute hang. | RED `TestReconcileIPC_EventLoopAndStateFileLifecycle_OrderSequence`, then fixture-owned `Close`/`loopDone` only if RED proves the gap. |
| Audit deterministic acquisition guard does not exist. | **ASSUMPTION (UNVERIFIED)**. | Implement the two named test-only guards at the existing runner seam and execute exact plus broad GUI gates. |
| Broadcaster Close can deliberately time out while a storage call remains blocked. | VERIFIED production degradation in `internal/gui/events.go:223-240`; not observed in the nine-worker family because those tests never call Close. | Preserve package oracle; a distinct production SLO needs an injected blocked storage probe if operational requirements change. |
| Candidate and `HEAD` extended CLI controls fail different adjacent tests. | VERIFIED, unresolved outside this family. | Admit separate quiesce and temporary-directory lifecycle diagnostics; compare each exact real symptom with immutable `HEAD`. |
| Rollback drill | **ASSUMPTION (UNVERIFIED)**. | Exercise inverse patch in disposable isolated evidence state before publication. |

No real external blocker exists for backend implementation. The remaining uncertainty is intentionally converted into deterministic RED-first test work, not permission to patch production code.

## Recommended next role

One `$backend-engineer` owns the test-only implementation. It reads `$superpowers:test-driven-development`, writes the named RED guards first, applies the smallest owner corrections, and verifies targeted symptoms followed by the broad 12-minute package gates. Separate adjacent CLI failures remain distinct successor work; they do not widen this implementation.

## Gate

**PASS** — ready for a single backend implementation owner at the bounded test seams above. PASS does not mean the release broad gates are green; it means the causal package is deterministic enough to implement without guessing. Publication, installation, hub restart, PR work, and live fleet changes remain outside this gate.

## Terms and Abbreviations

- **CLI** — command-line interface.
- **GUI** — graphical user interface.
- **IPC** — inter-process communication.
- **SLO** — service-level objective.
- **SLI** — service-level indicator.
- **flock** — advisory file lock used by the audit occurrence store.
- **RED guard** — a regression test demonstrated to fail before its owner correction.
- **PASS** — this diagnosis is ready for the next bounded role.
- **ASSUMPTION (UNVERIFIED)** — a claim whose exact resolving probe is named but has not yet executed.
