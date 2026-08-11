# Reliability package — adjacent CLI quiesce and teardown failures

## Summary

The three failures are not Reconcile failures. They split into two owner classes.

| Family | Exact owner chain | Candidate versus immutable `HEAD` | Disposition |
| --- | --- | --- | --- |
| Quiesce `drained=0`, both PIDs `still_running` | The test treats `exec.Cmd.Start` as child readiness, then gives a newly-created Windows PowerShell process a fixed two-second lifetime. `Drain` correctly reports both numeric PIDs alive at its final probe. | Candidate broad alone observed the failure. Candidate and `HEAD` named-order controls both pass in about 2.06–2.07 s. Owner files are byte-identical to `HEAD`, and the test does not call the changed no-console process helpers. Indirect candidate attribution remains **ASSUMPTION (UNVERIFIED)** because the real broad failure did not reproduce on `HEAD`. | Test-fixture readiness and observation defect; release-relevant until a condition-driven guard replaces natural process-start timing. No `Drain` arithmetic change is justified. |
| `CallsReaperBeforeReconcileReady` TempDir not empty | `runSupervise` starts EventLoop, IntentWatcher, initial reconcile, maintenance scheduler, and heartbeat workers. Exit cancels `loopCtx` and returns without joining them; the test's `done` channel therefore proves only the main function returned, not that state-directory writers stopped. | Candidate broad failed; exact candidate/`HEAD` sequence passes. Owner source is byte-identical to `HEAD`. The test package installs a no-op reconcile spawn, so changed Windows child-start code is not reached. | Baseline supervisor runtime-lifecycle defect. `runSupervise` owns cancellation **and join** before closing files/locks and returning. |
| `StaleLivenessDisownDroppedByGeneration` TempDir not empty | The fixture starts `go loop.Run(ctrl.ctx)` and registers cancellation but no join. The test returns as soon as `smStates` changes, while the same handler stores state before persistence and side effects; its event-log/state writer can still run during TempDir teardown. | Immutable `HEAD` broad failed; exact candidate/`HEAD` sequence passes. All owner source is byte-identical. No process or console helper is called. | Baseline test-fixture lifecycle defect sharing the EventLoop-join boundary with the full supervisor case. |

The minimal correction boundary is: a deterministic quiesce child/probe fixture, an explicit EventLoop close/join owner in the lost-child fixture, and a single `runSupervise` runtime group that cancels and joins every worker on all returns. The accepted Windows console contract remains 6/6 top-level and 63/63 subtests GREEN and was not rerun (`work-items/active/2026-08-11-windows-console-opt-in-r2/brief.md:7-12`).

## Evidence

### Scope and method

- The active stage and evidence gate are recorded in `work-items/active/2026-08-11-windows-console-opt-in-r2/status.md:8-24`; ordered delivery remains unchanged (`roadmap.md:5-16`).
- The accepted prior package refuted Reconcile attribution and explicitly separated these failures (`reliability.md`, Summary and Phase 1). The accepted GUI implementation changed only GUI test owners (`implementation-f.md`, Summary and Receiving-side echo).
- CodeGraph was queried first against the fresh 2,040-file index. It returned the quiesce and liveness owner chains. Direct reads were limited to the named test bodies and runSupervise/controller ranges omitted by the two allowed explore responses.
- Relevant owner files have zero candidate diff entries. Current receipt: `.scratch/windows-console-contract/r2-cli-adjacent/relevant-source-diff.txt`.
- No broad 12-minute storm, GUI suite, Windows target suite, live process, or fleet mutation was performed.

### Real symptoms

| Observation | Current evidence | Meaning |
| --- | --- | --- |
| Candidate broad quiesce | `TestQuiesceHandler_MixedDrainedAndStillRunning` fails after 2.04 s: expected `drained=1`, observed `drained=0`, `still_running=[73636 53940]`. `.scratch/windows-console-contract/r2-reliability/candidate-cli-broad-timeout12m.txt:546-547`. | The final liveness probe saw both actual test PIDs alive. It does not evidence wrong subtraction. |
| Candidate broad full-supervisor teardown | `TestSuperviseCommand_CallsReaperBeforeReconcileReady` assertions finish, then `testing` fails `TempDir RemoveAll` because `hardened-parent` is not empty. `.scratch/windows-console-contract/r2-reliability/candidate-cli-broad-timeout12m.txt:586-587`. | Failure occurs in framework cleanup after the test body, not in reaper ordering assertions. |
| Immutable `HEAD` broad controller teardown | `TestSupervisorController_StaleLivenessDisownDroppedByGeneration` assertions finish, then the same TempDir cleanup class fails. `.scratch/windows-console-contract/r2-reliability/head-cli-broad-timeout12m.txt:529-530`. | A second unchanged EventLoop owner reaches the same cleanup class under broad order/load. |
| Candidate named-order control | One package process, each named test once: stale-liveness 0.02 s PASS, quiesce 2.06 s PASS, reaper 0.06 s PASS; package 2.176 s, exit 0. `.scratch/windows-console-contract/r2-cli-adjacent/candidate-named-order.txt`. | Exact/three-test PASS confirms broad-order dependence; it does not clear broad failures. |
| Immutable `HEAD` matching control | Same order and arguments: 0.02 s / 2.07 s / 0.07 s, package 2.197 s, exit 0. `.scratch/windows-console-contract/r2-cli-adjacent/head-named-order.txt`. | The isolated behavior is equivalent between candidate and `HEAD`. |

Exact command for both trees:

`go test -v -count=1 -timeout 2m ./internal/cli -run '^(TestQuiesceHandler_MixedDrainedAndStillRunning|TestSuperviseCommand_CallsReaperBeforeReconcileReady|TestSupervisorController_StaleLivenessDisownDroppedByGeneration)$'`

### Hypothesis inventory and falsification

| Hypothesis | Falsification probe | Verdict |
| --- | --- | --- |
| Q1. `Drain` subtracts incorrectly. | Its result is `initialCount - len(stillRunning)` on cancellation/deadline (`internal/cli/supervise_quiesce.go:103-136`). The observed two live entries from an initial two mathematically require zero drained. | Refuted. |
| Q2. The short child is ready and guaranteed to exit about 200 ms after `cmd.Start`. | On Windows the helper launches a new PowerShell and only then executes `Start-Sleep -Milliseconds 200`; the comments themselves record 100–150 ms startup as an ordinary unloaded observation (`internal/cli/supervise_quiesce_test.go:13-34`). There is no ready or exit condition before the two-second Drain starts (`:283-317`). | Refuted. Test fixture owns the result. |
| Q3. A recycled PID can affect the result. | Production state contains PID and `StartedAt`, but `aliveTransientPIDs` calls only `isPIDAlive(t.PID)` (`internal/cli/supervise_quiesce.go:139-151`). Windows opens whichever process currently owns that numeric PID (`internal/cli/supervise_quiesce_windows.go:34-48`). | Potential participant, not evidenced in the captured failure because both original commands remained unreaped until after Drain. Deterministic guard must remove this ambient window. |
| Q4. Candidate no-console process changes directly delay the helper. | The test calls `exec.Command("powershell", ...)` directly (`internal/cli/supervise_quiesce_test.go:21-34`), not `internal/process` launch helpers; all quiesce owner files have zero diff versus `HEAD`. | Direct coupling refuted. Indirect broad-load coupling remains **ASSUMPTION (UNVERIFIED)**. |
| T1. `cmd.Execute` return means full supervisor teardown completed. | `runSupervise` starts workers (`internal/cli/supervise.go:782-861,1347-1431,1484-1544`), exit paths cancel and immediately return (`:1594-1714`), and no worker join precedes return. | Refuted. Full-supervisor lifecycle root verified by source. |
| T2. The stale-liveness test observes handler completion. | It observes only `ctrl.GetSMState != StRunning` and returns (`internal/cli/supervise_lostchild_f6_f2_test.go:699-708`). `handleLoopEvent` stores the new state before persistence and side effect (`internal/cli/supervisor_controller.go:3245-3267,3345-3356`). | Refuted. State transition is an intermediate condition, not handler settlement. |
| T3. Fixture cancellation proves EventLoop exit. | `lostChildParoleController` registers `cancel` but no done/join (`internal/cli/supervise_lostchild_f6_f2_test.go:252-279`); the named test starts `go loop.Run(ctrl.ctx)` without retaining completion (`:681-683`). | Refuted. Test-fixture root verified by source. |
| T4. No-console changes indirectly created TempDir writers. | Stale-liveness has no child process. In full-supervisor tests, package TestMain installs a no-op `reconcileSpawnFn` specifically to prevent real child/start-with-job execution (`internal/cli/settings_registry_test.go:368-395`). Relevant lifecycle files have zero diff. | Direct and reachable indirect console-spawn path refuted. Broad scheduling determines which baseline owner loses teardown, not whether the join contract exists. |

## Quiesce defect-class inventory

| Participant | Success/error/cancel/timeout behavior | Classification |
| --- | --- | --- |
| `spawnShortLivedChild` | `cmd.Start` returns after process creation; PowerShell bootstrap and 200 ms sleep occur later. Start error skips; defer kills, happy path waits only after Drain. `internal/cli/supervise_quiesce_test.go:13-35,283-317`. | **Root test fixture.** No readiness or termination barrier exists before classification. |
| `spawnLongLivedChild` | Starts a 60-second process; tests own kill/wait. `:37-51`. | Fixed/not affected for observed result. It correctly remained live. Early assertion paths kill but do not always `Wait` in the defer, so helper-reap completeness is a potential sibling defect. |
| `QuiesceHandler.Drain` | Snapshots count, probes every 50 ms, returns when none live, context cancels, or deadline expires. `internal/cli/supervise_quiesce.go:90-137`. | Not affected. Arithmetic matches its observed liveness inputs. Fixed wall-clock polling makes fixture timing visible. |
| `aliveTransientPIDs` | Filters the original state slice by the platform probe. `:139-160`. | Not affected for observed two-live result; potential PID-identity boundary. |
| Windows `pidAliveImpl` | Opens PID with `SYNCHRONIZE`, tests handle state, closes handle on all paths. `internal/cli/supervise_quiesce_windows.go:9-48`. | Fixed handle lifecycle. Numeric PID alone cannot exclude reuse. |
| `TransientPID.StartedAt` | Available in state but unused by quiesce liveness (`internal/api/supervisor_state.go:16-23`; `supervise_quiesce.go:145-149`). | Potential participant in production PID-reuse defense; not the captured failure root. Do not widen the current fix without a separate reproduced identity failure. |
| Context cancellation | Returns the most recently probed `stillRunning` snapshot (`supervise_quiesce.go:122-127`). | Fixed contract; guard must cancel only after observing the intended probe generation, not concurrently with it. |
| Deadline | Performs a fresh final probe and classifies (`:116-136`). | Correctly exposed the unready helper. A natural two-second window is not a deterministic oracle. |
| `EmptyState`, `NilState`, `DeadPIDs` siblings | No external child readiness dependency (`internal/cli/supervise_quiesce_test.go:53-97,320-340`). | Not affected. |
| `DrainsTransients` | Uses the same short helper and a five-second natural window (`:99-140`). | Potential same-class participant. |
| `TimeoutWithStillRunning` | Long helper, elapsed-time lower bound, explicit kill/wait (`:142-187`). | Potential wall-clock participant; not the mixed-class root. |
| `ContextCancellation` | Long helper and sleep-driven cancel at 100 ms (`:189-231`). | Potential natural timing participant. |
| `InProgressFlag` | Sleeps 50 ms to guess Drain entry (`:233-278`). | Potential natural timing participant; a condition barrier is required. |

### Named deterministic quiesce guard

`TestQuiesceHandler_MixedSettlementAfterObservedProbeGeneration` is the required owner guard.

1. Re-execute the current test binary as two real helper children. Each helper publishes `ready` only after bootstrap, then blocks on its own `release` condition. The parent waits for both ready conditions before starting Drain.
2. Retain each original `os.Process` handle until settlement. On Windows the process identifier is not reusable until all process handles are closed and the process object is freed; do not call `Wait` on the short child until after Drain classifies it ([Microsoft `PROCESS_INFORMATION`, updated 2024-02-22](https://learn.microsoft.com/en-us/windows/win32/api/processthreadsapi/ns-processthreadsapi-process_information)).
3. Start `Drain` with a long safety context. A narrow test observer reports the monotonically increasing generation and exact result of each real `aliveTransientPIDs` probe without replacing the production probe. Wait for an initial generation containing both original PIDs.
4. Release only the short helper. Wait conditionally until its retained process handle is signaled/exited, then wait for a later Drain probe generation containing only the long helper. Cancel only after that generation.
5. Expect `Drained=1`, `StillRunning=[long]`, `InProgress=false`; then reap the short child, terminate/reap the long child, and join the probe observer on every success/error/cancel/timeout path.

Expected RED trigger: the original test starts its deadline before either child publishes readiness and `QuiesceHandler` exposes no probe-generation observation seam, so it cannot establish steps 1–4. Execution is **ASSUMPTION (UNVERIFIED)** because the read-only stage cannot add the test helper/observer. Resolving command after RED-first implementation:

`go test -v -count=1 -timeout 2m ./internal/cli -run '^TestQuiesceHandler_MixedSettlementAfterObservedProbeGeneration$'`

A bare numeric PID after `Wait`, a synthetic liveness replacement, CPU-load injection, or a sleep is not an accepted oracle.

## Supervisor / TempDir defect-class inventory

### Shared owner chain

The common invariant is “observable state is not terminal settlement.”

- Full supervisor: `loopCancel` is owned by `runSupervise` (`internal/cli/supervise.go:782-785`), but the function returns immediately after cancellation on signal, test, IPC, and parent-context paths (`:1594-1714`).
- Controller fixture: `cancel` is owned by `lostChildParoleController`, but no `loopDone` is owned (`internal/cli/supervise_lostchild_f6_f2_test.go:252-279`).
- Stale-liveness handler: `smStates.Store` precedes persistence and side effects (`internal/cli/supervisor_controller.go:3245-3267,3345-3356`).
- Framework TempDir cleanup is registered before the event-log close and context cancel; Go cleanup runs them in reverse registration order. The effective sequence is cancel → event log close → root removal, with no join between cancel and close (`supervise_lostchild_f6_f2_test.go:252-279`).

### Full-supervisor resources

| Participant | Ownership / all-return paths | Classification |
| --- | --- | --- |
| Singleton lock and owner sidecar | Acquired first; `defer lk.Release` (`internal/cli/supervise.go:598-608`). | Fixed explicit release, but release can precede outstanding worker settlement on return. |
| Stderr sink and event log | Deferred release/close (`:610-640`). | Fixed direct handles; potential ordering victim because workers retain pointers and can emit after the defers begin. |
| EventLoop | Started at `:782-861`; receives cancellation on every bottom exit. | **Potential root writer; no join.** |
| IPC listener/accept and stall watchers | Listener has deferred close; workers use `loopCtx` when IPC is enabled (`:1032-1108`). | Potential same class in production/IPC siblings; not started by `--no-ipc` named test. |
| Crash bridge, port-gate/reallocation workers, warmup, liveness and parole monitors | Production-only when `reconcileSpawnFn == nil` (`:1233-1333`). | Not affected in the named test because TestMain makes the function nonnil (`settings_registry_test.go:368-395`). Potential production join participants. |
| IntentWatcher | Starts unconditionally and uses `loopCtx` (`supervise.go:1335-1431`). | **Potential root writer; cancellation without join.** |
| Initial reconcile | Starts in a goroutine when intent exists (`:1433-1487`); readiness means scheduled, not completed (`:1490-1502`). | **Potential root writer; no context/join.** |
| Maintenance scheduler/spawner | Scheduler uses `loopCtx.Done`; shutdown joins child processes, then returns (`:1504-1576`). | Child settlement fixed; scheduler goroutine itself is not joined. |
| Heartbeat | Uses `loopCtx.Done` (`:1533-1544`). | Potential event-log writer after main return; no join. |
| Timers | EventLoop workers own tickers; exit cancellation is signaled. | Cancellation verified by source; join latency unobserved. |
| Environment override | TestMain resolves `MCPHUB_STATE_DIR_OVERRIDE`; named test sets it to its hardened root (`settings_registry_test.go:399-411`; `supervise_test.go:614-615`). | Fixed isolation; becomes failure target when late writer persists. |
| Test fake reaper | Synchronous callback returns before readiness; no process, timer, file, or goroutine (`supervise_test.go:646-664`). | Not affected. Reaper ordering assertions passed in the broad failure. |
| Test `done` channel | Receives `cmd.Execute` return (`:674-718`). | **Insufficient settlement oracle.** It does not cover runtime workers. |
| Audit ordering read | Reads after main return (`:720-742`). | Correct reaper-vs-ready oracle, not a runtime teardown oracle. |

Full-run siblings sharing the owner include `TestSuperviseCommand_AcquiresLockAndExitsOnSignal`, both status IPC tests, both reaper tests, old-binary sweep tests, maintenance wiring, and reconcile wiring (`internal/cli/supervise_test.go:138-873`; `supervise_maintenance_wiring_test.go:14`; `supervise_reconcile_wiring_test.go:95-430`). Early lock-loser tests do not start the runtime. Malformed-intent tests start the loop before returning on load failure and remain potential cleanup participants. Each sibling must inherit the single runtime-group close path; per-test sleeps/polls are not fixes.

### Direct controller fixture resources

| Participant | Ownership / order | Classification |
| --- | --- | --- |
| Hardened temp root | Created by fixture before other cleanups (`internal/cli/supervise_lostchild_f6_f2_test.go:252-255`). | Failure surface, not root. |
| Supervisor event log | Opened and cleanup-closed (`:256-260`). | Potential late writer target. Close is not a loop join. |
| EventLoop and context | Constructed at `:262`; context cleanup only at `:276-279`; callers launch `go loop.Run`. | **Root fixture boundary: cancel without join.** |
| Stale event handler | Stale generation logs and returns (`internal/cli/supervisor_controller.go:2952-2999`). | Fixed stale-drop logic. |
| Current event handler | Stores non-running SM state before persist/side effect (`:3245-3267,3345-3356`). | **Exact late-writer window observed by test.** |
| State poll | Returns immediately after seeing non-running (`supervise_lostchild_f6_f2_test.go:699-708`). | **Incorrect settlement condition.** |
| EventLoop barrier | `evControllerBarrier` closes a supplied done channel only after prior FIFO handler work completes (`internal/cli/supervisor_controller.go:2846-2850`). | Existing deterministic handler-settlement seam; fixed/available. |
| EventLoop cancellation | `Run` exits on context done after the current synchronous dispatch returns (`internal/api/supervisor_event_loop.go:259-283`). | Fixed cancellation behavior; caller must join. |

Sibling tests that call `lostChildParoleController` and launch its loop are at `supervise_lostchild_f6_f2_test.go:301-708,776-846`, plus `supervise_prespawn_binary_gate_test.go:45`. They are potential participants because the fixture owns no join. The two absorbing-quarantine tests at `supervise_lostchild_f6_f2_test.go:714-775` do not start the loop and are not affected by this resource class.

### Named owner-level order guard

`TestSupervisorLifecycle_OrderSequenceJoinsBeforeTempRootRemoval` covers both broad participants in one package process.

1. Controller phase: start the lost-child EventLoop with an owned `loopDone`. Post stale then current-generation exits. Post the existing `evControllerBarrier` and wait for it, proving all prior handler persistence/logging finished. Cancel, join `loopDone`, close the event log, then remove and recreate the exact state root.
2. Full-supervisor phase: run the reaper-before-ready scenario through a `supervisorRuntimeGroup` that owns every goroutine. Inject one worker that announces start, blocks until runtime cancellation, announces exit, and is counted by the group. Trigger test exit. Require `runSupervise` not to return until active-worker count is zero, then close files/lock and remove/recreate the exact state root.
3. Re-entry phase: repeat controller then full-supervisor phases against fresh roots in the opposite order. Require no live helper, EventLoop, watcher, lock, file handle, timer, or environment override from the first sequence.

Expected RED triggers:

- current lost-child fixture has no `loopDone`, and observing SM state permits teardown before the existing FIFO barrier;
- current `runSupervise` returns after `loopCancel` without waiting for its announced worker.

Execution is **ASSUMPTION (UNVERIFIED)** because the required runtime-group and fixture join seams do not exist and this stage is read-only. Resolving command:

`go test -v -count=1 -timeout 3m ./internal/cli -run '^TestSupervisorLifecycle_OrderSequenceJoinsBeforeTempRootRemoval$'`

The guard must fail on an active owner before attempting root removal; a natural Windows `RemoveAll` collision is supporting evidence, not the deterministic oracle.

## Candidate versus `HEAD` attribution

| Family | Task-caused | Baseline | Still unverified |
| --- | --- | --- | --- |
| Quiesce fixture | No direct no-console call path; relevant files unchanged. | Natural readiness defect exists in both trees. | **Indirect task attribution is unverified** because only candidate broad captured the real symptom and isolated candidate/`HEAD` both pass. Resolving step: execute the deterministic named guard on candidate and immutable `HEAD`; identical RED/green transitions classify baseline. |
| Full-supervisor TempDir | Changed StartWithJob/no-console path is unreachable because TestMain installs no-op spawn. Relevant lifecycle files unchanged. | Cancel-without-join exists in `HEAD`; immutable `HEAD` broad exhibits the same TempDir class at a sibling owner. | Which exact background writer won the candidate cleanup race is unverified without the runtime-group guard. That does not change the single owner. |
| Direct-controller TempDir | Test starts no process and calls no console code. | Fixture cancel-without-join and state-before-persist ordering exist in both trees; `HEAD` broad real symptom confirms. | Exact recreated filename is unobserved because Go reports only the parent directory. Guard resolves by active-owner assertion before removal, not filename guessing. |

## Diff-invisible invariants echo

| Invariant | Verdict | Evidence / resolving step |
| --- | --- | --- |
| Quiesce observes intended process state only after deterministic readiness/termination. | **ASSUMPTION (UNVERIFIED)** | Current helper has no readiness channel (`supervise_quiesce_test.go:13-34`). Resolve with `TestQuiesceHandler_MixedSettlementAfterObservedProbeGeneration`. |
| Quiesce guard has no uncontrolled PID reuse. | **ASSUMPTION (UNVERIFIED)** | Current production probe is numeric-PID-only (`supervise_quiesce.go:139-151`). Guard must retain the original process handle until after the observed probe generation and Drain settlement. |
| Full supervisor joins every helper/daemon/EventLoop/watcher on success. | **ASSUMPTION (UNVERIFIED)** | Cancellation exists; no runtime group wait exists (`supervise.go:782-861,1594-1714`). Resolve with owner-level guard. |
| Same joins occur on error, parent cancellation, IPC exit, signal, and timeout. | **ASSUMPTION (UNVERIFIED)** | Four terminal branches share cancellation but no join. Runtime group must be deferred immediately after creation so later startup errors use it. |
| Direct controller fixture joins its EventLoop before event log/root cleanup. | **ASSUMPTION (UNVERIFIED)** | Only cancel is registered (`supervise_lostchild_f6_f2_test.go:252-279`). Resolve with loopDone cleanup and FIFO barrier. |
| Exact PASS does not clear broad-order failure. | VERIFIED | Both named-order controls pass while accepted candidate/`HEAD` broad receipts contain real failures. |
| Candidate-vs-HEAD uses real failures. | VERIFIED for TempDir class; **ASSUMPTION (UNVERIFIED)** for quiesce indirect attribution | Candidate and `HEAD` broad receipts provide sibling TempDir real symptoms. Only candidate broad contains quiesce. |
| Every diagnostic-owned process/resource is cleaned. | VERIFIED | Both exact commands terminated; the quiesce test's happy path kills/waits both children (`supervise_quiesce_test.go:315-317`); no diagnostic listener/background command remains. |

## Reliability objectives and failure behavior

| SLO | SLI / measurement point | Window and threshold | Error-budget consequence |
| --- | --- | --- | --- |
| Deterministic quiesce classification | Test-side probe generation and returned `QuiesceResult` at handler boundary. | Every exact guard and release CLI suite; 100% of engineered short=false/long=true generations return drained=1 and only long still-running; zero sleep-derived readiness. | One mismatch exhausts budget and blocks. Do not retry-to-green. |
| Supervisor runtime settlement | Runtime-group active count, loopDone, open-owner inventory, and exact state-root remove/recreate at `runSupervise` return. | Every terminal branch and owner-order guard; active count=0 and root immediately removable within a one-second safety watchdog. | Any active owner or root failure exhausts budget and blocks. |
| Broad CLI lifecycle | Terminal package result with 12-minute package bound plus named guard results. | Every release candidate; zero lifecycle/TempDir/quiesce failures. | Any failure blocks; attribution follows the deterministic owner guard, not the last active test. |

| Failure mode | Degradation / recovery | Detection and latency | Page decision / strength |
| --- | --- | --- | --- |
| Child not ready/terminated at classification | Fail the test with probe-generation evidence; cancel and join the helper. Never extend sleep. | Named exact guard, under two minutes. | No page; CI block. Current root is analysis plus real broad symptom; deterministic injection pending. |
| Runtime worker survives supervisor return | Do not close logs/locks or report return until cancel+join completes. A bounded join failure returns a typed lifecycle error with active owner names. | Owner guard at return, under one-second watchdog. | No page in tests; production shutdown alert is analysis-only until runtime group exists. |
| Event handler still writing during TempDir cleanup | FIFO barrier, cancel, join, close, remove in that order. | Owner guard before removal, immediate. | No page; CI block. Real broad symptom verified. |
| Join cannot complete | Fail loud; preserve owner name and goroutine stack. Do not abandon the writer and delete its state root. | Join watchdog and stack capture at one second in tests. | No page in CI; production logging/alert policy requires separate operational approval. |

No mutation is retried. Re-entry uses fresh roots only after prior owners join. The state-machine events retain their existing generation/idempotency rules; this package changes lifetime settlement, not state semantics.

## Rollout, observability, recovery, and rollback

| Stage | Required proof | Abort signal | Rollback |
| --- | --- | --- | --- |
| 1. Quiesce test owner | Named guard RED on missing condition seam, then GREEN without sleep/PID ambiguity; existing quiesce family GREEN. | Any uncontrolled wall-clock readiness, leaked child, wrong generation, or result mismatch. | Revert only the quiesce fixture/seam patch. Drill: **ASSUMPTION (UNVERIFIED)** until backend executes inverse patch and re-runs RED. |
| 2. Direct EventLoop fixture | Owner-order guard RED on active loop; GREEN after barrier+cancel+join; lost-child siblings GREEN. | Active loop/log writer, root removal failure, or state-machine assertion change. | Revert fixture-owner patch only. Drill unverified. |
| 3. `runSupervise` runtime group | RED announced worker survives return; GREEN on signal/test/IPC/context and one post-start error path; full supervisor siblings GREEN. | Any worker count nonzero, join watchdog, lock/log ordering change, or changed exit result. | Revert runtime-group commit only; preserve user dirty bytes. Drill unverified. |
| 4. Broad CLI | `go test -count=1 -timeout 12m ./internal/cli`. | Any package timeout, quiesce mismatch, TempDir cleanup error, live helper, or console-contract source change without targeted rerun. | Stop rollout; use the last stage's inverse patch. No retry-to-green acceptance. |

Observability requirements:

- Guard failure names the outstanding owner (`event-loop`, `intent-watcher`, `initial-reconcile`, `maintenance-scheduler`, `heartbeat`, IPC worker, production worker) and whether cancellation was delivered.
- Quiesce guard records helper readiness, retained original process identity, probe generation, and observed live set; ambient PIDs alone are never the oracle.
- Runtime return records no success until active owner count reaches zero.
- Existing `supervisor-exit`, reaper, reconcile-ready, and state-machine event schemas remain unchanged.

Recovery ordering is fixed: stop new work → cancel owner context → wait for handler/runtime group → close event/file/lock handles → remove test root → restore environment seam. Failure/cancel/timeout all follow the same owner order.

## Minimal downstream implementation surface

| Owner | Allowed surface | Forbidden expansion |
| --- | --- | --- |
| Quiesce test owner | `internal/cli/supervise_quiesce_test.go`; the narrowest package-private probe-observation seam in `supervise_quiesce.go` only if required to expose the real probe generation. Preserve the production `isPIDAlive` decision and use real ready/release helpers. | No Drain arithmetic, polling interval, wire result, maintenance state schema, console policy, general process subsystem, or synthetic liveness replacement. PID-identity production hardening requires separate evidence/admission. |
| Direct controller fixture owner | `lostChildParoleController` and named tests in `internal/cli/supervise_lostchild_f6_f2_test.go`; use existing controller barrier and explicit loopDone. | No state-machine transition/generation/persistence logic change. |
| Full supervisor lifecycle owner | `runSupervise` goroutine launch/terminal-return ownership in `internal/cli/supervise.go`, plus one focused lifecycle test file. One runtime group owns all launched workers. | No reaper ordering, readiness meaning, daemon spawn semantics, state schema, exit event schema, or no-console behavior change. No per-test sleeps/polls layered over missing join. |

One backend owner may implement these as three sequential RED-first commits, but each commit remains independently reversible and verified before the next.

## Claims

1. `{ guarantee: quiesce mixed settlement is classified only after real helpers are ready and one named production-probe generation observes the retained short process exited and long process live; single-owner: QuiesceHandler test readiness/probe-observer fixture; enforcement-probe: TestQuiesceHandler_MixedSettlementAfterObservedProbeGeneration returns drained=1 and only long still-running while retaining original process identity, with no sleep, PID reuse, or synthetic liveness }`.
2. `{ guarantee: a controller test never tears down its state root until every prior FIFO handler completed and EventLoop.Run returned; single-owner: lostChildParoleController close/join path; enforcement-probe: TestSupervisorLifecycle_OrderSequenceJoinsBeforeTempRootRemoval sees the barrier, loopDone, and immediate root remove/recreate }`.
3. `{ guarantee: runSupervise does not return on success, error, signal, IPC exit, parent cancellation, or timeout while any owned runtime worker remains; single-owner: supervisorRuntimeGroup in runSupervise; enforcement-probe: TestSupervisorLifecycle_OrderSequenceJoinsBeforeTempRootRemoval blocks return until an announced worker exits and observes active count zero }`.
4. `{ guarantee: the console candidate is not exonerated merely by untouched owner files; single-owner: candidate-versus-HEAD attribution gate; enforcement-probe: deterministic named guards execute on candidate and immutable HEAD and compare the same engineered real condition }`.
5. `{ guarantee: no lifecycle correction changes exact leading --debug-console opt-in or default no-visible-console behavior; single-owner: existing console composition boundary; enforcement-probe: relevant-source diff excludes console files and accepted 6/6 top-level, 63/63 subtests remain the protected baseline }`.

## Risks / Unknowns

| Unknown | Status | Resolving step |
| --- | --- | --- |
| Candidate-specific trigger for PowerShell exceeding the natural window | **ASSUMPTION (UNVERIFIED)** | Run deterministic quiesce guard on candidate and immutable `HEAD`; do not manufacture CPU load or repeat sleeps. |
| Exact full-supervisor goroutine that recreated the candidate directory entry | **ASSUMPTION (UNVERIFIED)** | Runtime-group guard reports active owner names and prevents all from surviving; do not guess a filename from parent-only RemoveAll error. |
| Production PID reuse in quiesce | Potential risk, not observed root. | Separate identity-reuse injection using `TransientPID.StartedAt`; not part of current minimal fix. |
| Join latency and non-context-aware initial reconcile | **ASSUMPTION (UNVERIFIED)** | Runtime-group RED guard and one cancellation-in-flight injection; if Reconcile cannot stop, route that exact owner for a bounded context contract rather than weakening join. |
| Rollback drills | **ASSUMPTION (UNVERIFIED)** | Backend exercises inverse patch for each local commit in disposable state and reruns its named RED/green probe. |

There is no external blocker. The missing deterministic seams are implementation work with exact resolving guards, not missing product authority.

## Recommended next role

One `$backend-engineer` reads `$superpowers:test-driven-development`, writes both named guards RED first, then applies only the three owner corrections above. It verifies exact guards, sibling quiesce/lost-child/full-supervisor tests, race detector for the named guards, and finally the 12-minute CLI package. Adjacent unrelated CLI failures remain separately classified.

## Gate

**PASS** — ready for one bounded backend owner. PASS means the causal owners and RED seams are explicit; it does not mean the CLI broad release gate is green.

## Terms and Abbreviations

- **CLI** — command-line interface.
- **IPC** — inter-process communication.
- **PID** — process identifier.
- **SLO** — service-level objective.
- **SLI** — service-level indicator.
- **EventLoop** — the supervisor's single-threaded FIFO event dispatcher.
- **FIFO** — first in, first out.
- **RED** — guard fails before the owner correction.
- **PASS** — diagnosis is ready for bounded implementation.
- **ASSUMPTION (UNVERIFIED)** — a claim with an exact resolving step that has not executed.
