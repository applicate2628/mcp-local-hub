# Phase G correction design — parent restart single-owner lifecycle

- Date: 2026-07-18
- Role: `$architect`
- Scope: correction of the five confirmed Phase G findings only
- Admission source: direct human Phase G plan plus the 2026-07-18 `$lead` amendment
- Cross-cutting decision registry: none; every decision below is local to this work-item and the already-approved Phase G seams

## Accepted evidence and spot-check result

The load-bearing review citations still match the current tree:

- QA proves a one-millisecond connection refusal while the confirmation context remained live and identifies the single call/probe at `item3-unitB-phaseG-qa.md:63,88`; the bug record traces the same production path at `work-items/bugs/2026-07-18-restart-v3-parent-single-shot-confirmation.md:18-30`. Current source still creates one context and calls `Confirm` once at `internal/gui/gui_restart_protocol.go:244-255`, while the probe returns the first transport error at `internal/gui/gui_restart_protocol.go:495-545`.
- QA proves reservation validation follows hub close and release at `item3-unitB-phaseG-qa.md:89`; the bug record gives the exact expected pre-release placement at `work-items/bugs/2026-07-18-restart-v3-validates-reservation-after-release.md:10-20`. Current source still reserves at `internal/gui/gui_restart_protocol.go:284-297`, closes/releases at `:299-305`, and validates at `:317-324`.
- The architecture review demonstrates the two late-publisher arms at `item3-unitB-phaseG-architecture-review.md:60-75`. Current source publishes initializer output at `internal/gui/server.go:1172-1183`, starts the unjoined restart driver at `:1196-1197`, and the driver publishes replacements at `internal/gui/hub_listener.go:474-544`; restart close still performs only `Swap(nil)` plus shutdown at `internal/gui/server.go:933-934`.
- The architecture review identifies five divergent post-`Begin` cleanup branches at `item3-unitB-phaseG-architecture-review.md:77-88`. Current source still duplicates marker/nonce cleanup at `internal/gui/gui_restart_protocol.go:171-216`, ignores several cleanup errors, and does not close a non-nil child whose `PID()` is zero.
- The architecture review traces v3 process spawn and `os.Exit` ownership to the GUI package at `item3-unitB-phaseG-architecture-review.md:90-98`. Current source still selects GUI-owned `SpawnRestartV3GUI` and `RequestSelfRestartExit` from CLI at `internal/cli/gui.go:95-119`, while the concrete `exec.Cmd`, retained handle, and `os.Exit` live at `internal/gui/gui_self_restart.go:203-319`.
- The authoritative ordering and rollback rules remain `item3-unitB-plan.md:187-208`; the four-state persisted record and single deadline policy remain owned by `internal/gui/gui_restart_record.go:29-59,151-177`.

## Chosen approach

Correct the five classes by strengthening existing owners, not by adding compensating paths:

1. Publish one Server-owned hub lifecycle barrier before the full GUI handler becomes reachable. It owns registration, cancellation, joining, and final bundle take/shutdown for both hub producers. Restart close and both normal `Server` shutdown exits call the same operation.
2. Create one parent-attempt cleanup owner immediately after `Begin`. Every later return settles through that owner; `Start`, the continuation, and rollback code perform no direct nonce, marker, child-handle, listener-recovery, hub-close, lease-release, or exit cleanup.
3. Keep GUI protocol contracts process-agnostic. CLI composition owns the v3 `exec.Cmd`, retained `os.Process` handle, parser-aware argv/environment, and real `os.Exit`. Gate-OFF v1 stays exactly where it is.
4. Make authenticated confirmation a bounded condition wait: only a typed dial/not-listening result retries, under one `RestartDeadlines.Proof` context and one injected wait seam. Identity, authentication, HTTP, and proof-shape failures are terminal on the first observation.
5. Validate the complete `Reserve` result immediately on return. Only a valid `reserved` record permits progress publication, hub shutdown, and lease release.

## Change-Surface Contract

`{ intended change surface: internal/gui/gui_restart_protocol.go, internal/gui/gui_self_restart.go, internal/gui/server.go, internal/cli/gui.go, and their adjacent existing *_test.go files; approved extension seam(s): Server-owned hub lifecycle stop barrier, RestartCoordinatorDependencies process-neutral Spawn/Confirm/Exit and wait injection, RestartParentChild retained-handle contract, CLI restartV3ParentRuntime composition, existing HandoffMarkerStore and GUIListenerOwner operations; protected / must-not-touch surfaces: Phase H/J, frontend and generated assets, RestartV3Enabled default-OFF owner, gui-restart.json schema, child Phase F acquisition/activation/Commit path, Unit A port guard, supervisor/autostart ownership, production files outside the four authorized files; declared blast radius: behavioral correction inside the gated parent path plus consolidation of existing Server hub shutdown used by normal shutdown, with no new package edge, external endpoint shape, persisted state, or default-on behavior }`

Shared mutable-state writer-owner: `Server` is the only writer-owner for hub lifecycle stop state and the final `hubMcpComp` take. Downstream settled event: the lifecycle's closed `settled` channel means both producer goroutines have returned and the final bundle has been atomically taken for shutdown; successful Phase G release is forbidden before that event.

## Detailed contracts

### 1. Server-owned hub lifecycle barrier

Add one lifecycle object as Server state in `internal/gui/server.go`; do not add a new production file. Its contract is:

- `continueWithGUIListener` constructs and publishes the lifecycle object before `close(ready)` and before `GUIListenerOwner.ServeFull` (`internal/gui/server.go:1046-1070`). This closes the immediate-request race: once the restart endpoint is reachable, it always sees the lifecycle for that Server run.
- The lifecycle owns one derived context/cancel function, a mutex-protected `{started, stopping}` registration state, completion accounting for exactly two producers, and one `settled` channel. Initializer and restart-driver registrations are added under the mutex before either goroutine launches. A stop that wins before start marks `stopping`, cancels, and settles with zero producers; a start that observes `stopping` launches nothing.
- The initializer and `runHubListenerRestartDriver` each close only their own completion arm. The lifecycle closes `settled` only after both arms return. The existing producer-side canceled-context CAS take-back remains defense inside each producer (`internal/gui/server.go:1179-1183`, `internal/gui/hub_listener.go:540-544`); the lifecycle join is the owner-level proof that no producer can publish afterward.
- One idempotent Server operation, `stopHubLifecycle(ctx) error` conceptually, performs: latch stopping → cancel → wait for `settled` → atomically `Swap(nil)` the final bundle → call existing `ShutdownHubListener` with the same caller context. Restart close and both `ctx.Done`/listener-error normal shutdown branches call this operation. No other Server shutdown path directly cancels these producers or swaps/shuts the final hub bundle.
- Phase G supplies one five-second outer context. The same context covers producer join and `ShutdownHubListener`; the latter already force-closes active connections when graceful shutdown reaches its deadline (`internal/gui/hub_listener.go:1071-1133`). There is no nested independent hub deadline.
- If producer join consumes the deadline, the operation atomically takes and force-closes the currently published bundle but returns typed failure `gui-restart-hub-producer-join-timeout`. It does not report the lifecycle settled. The coordinator cannot take the success release path; the cleanup owner enters terminal rollback-failure handling. Thus a successful handoff never releases while a late CAS publisher exists.
- Normal Server shutdown uses the same result: it returns a shutdown error after bounded force-close instead of running a second cancel/wait/swap stack. Process termination remains the final cleanup for a producer that violated cancellation.

Exact race outcomes:

| Race | Required outcome |
| --- | --- |
| Restart stop wins before producer registration | Registration observes `stopping`; neither producer launches; final take is nil; release may continue. |
| Stop cancels while initializer/restart driver is before CAS | Producer observes cancellation, closes any produced bundle, returns; join completes before final take/release. |
| Producer CAS publishes concurrently with stop | Either final `Swap(nil)` takes it after join, or the producer's canceled-context CAS takes and shuts it; join proves no later publication. |
| Producer ignores cancellation past five seconds | Stop returns the typed join-timeout failure; success release is prohibited and terminal rollback releases/exits. |

### 2. One post-`Begin` cleanup owner

Create a `parentRestartAttempt`-equivalent owner in `gui_restart_protocol.go` immediately after a successful `Begin`. It records marker generation, nonce bytes and possible pathname residue, optional retained child, authentication state, listener mode, hub-stop state, the concrete `parentLeaseReleased` boolean, and response-commit state. It has one settlement entry point with three modes: healthy pre-release rollback, terminal pre-release rollback, and successful release. Helpers may perform one resource operation, but only settlement decides which helpers run and what outcome follows.

Resource rules:

| Resource/state | Single owner and rule |
| --- | --- |
| Nonce bytes | Attempt owner zeros exactly once on every settlement. |
| Nonce path | Mark `mayExist` before the atomic write call, so write failure also attempts unlink. `IsNotExist` is clean; every other unlink failure prevents a healthy 200 return. |
| Child handle | CLI adapter transfers ownership on any non-nil return, including `err != nil` and `PID()==0`. Unauthenticated/PID-zero cleanup calls `CloseWithoutTerminate` exactly once; only a successfully authenticated exact child may receive `TerminateAndClose(ctx)`. |
| Listener admission/bind | Attempt owner records whether grace/close occurred. Healthy rollback restores full admission; same-port restoration uses `BindForRecovery` then `ServeFull` with zero reacquire. Untouched full listener is already proved healthy. |
| Marker | Healthy settlement calls `ClearAfterProvedPreReleaseRollback` last, after nonce/child/listener restoration succeeds. Any cleanup or clear failure calls `Interrupt`; interrupt-write failure emits the existing `gui-restart-interrupted-marker-write-failed`. |
| Lease | The concrete attempt boolean, never marker phase, gates rollback. Healthy rollback never calls `Release`. Success and terminal rollback each call the injected `releaseOnceLease` once. |
| Hub | Only the Server lifecycle stop operation closes it. Success requires settled success; terminal rollback attempts the same bounded stop before release. |
| Exit | Attempt owner returns/uses only the injected `Exit`; CLI supplies the real terminating primitive. |

All cleanup runs under one fresh bounded `context.Background()`-derived `Rollback` context, not the already-canceled request/Server context. This lets context cancellation execute the same bounded child/listener/marker cleanup instead of converting every cleanup call into immediate `context.Canceled`.

Settlement matrix:

| Trigger | Child authority | Required settlement |
| --- | --- | --- |
| Nonce creation/length/write failure | none | Zero/unlink as applicable, clear marker; return ordinary spawn error only when residue absence is proved. |
| Spawn returns error, nil child, or non-nil PID-zero child | unauthenticated | Close any non-nil handle without termination, unlink/zero nonce, clear marker; ordinary 200 only on complete cleanup. |
| Transport confirmation deadline, authentication/identity/protocol failure | terminate only if exact proof had already succeeded | Restore listener/admission as needed, close/terminate by authority, clear marker on proved restoration. |
| Invalid `Reserve` result | authenticated exact child | Pre-release rollback before progress/hub/release. |
| Context cancellation | current authority state | Same bounded settlement path; cancellation is a cause, not permission to skip cleanup. |
| Any nonce/handle/listener/marker cleanup failure | current authority state | Write `interrupted`, bounded hub cleanup, then terminal release exactly once and CLI exit; never keep a healthy flock holder with stale nonterminal state. |
| Success after valid reserve and hub settled | authenticated exact child | Release once; after release only close retained handle without termination, fixed grace/listener-local cleanup, result delivery, and CLI exit. Handle-close failure is diagnostic only and cannot re-enter protocol. |

#### Endpoint response-order contract

The internal coordinator start outcome carries one `sync.Once` response-commit finalizer; this is not an HTTP field.

- On accepted spawn, the continuation may perform reversible confirmation/preparation, but it cannot enter `Reserve`, hub stop, or release until the v3 handler has written the 202 body, called `Flush` when supported, and committed the response latch.
- On a synchronous failure whose cleanup is complete, the handler writes the existing HTTP 200 body and the finalizer is a no-op.
- On a synchronous failure whose cleanup requires terminal release/exit, settlement first writes `interrupted` and performs bounded pre-release cleanup while retaining the lease. The handler then writes/flushes the existing HTTP 200 `{spawned:false,spawned_pid:0,restarting:false,spawn_error}` body and invokes the owner-produced finalizer. That finalizer performs only `Release` once followed immediately by injected CLI `Exit`. A handler `defer` invokes it exactly once even if JSON encoding/flush reports an error.
- No cleanup branch launches an independent release/exit goroutine. This keeps response ordering, release, and exit under one explicit ownership chain.

### 3. GUI protocol / CLI process boundary

`internal/gui/gui_restart_protocol.go` retains only stable process-neutral contracts:

- `RestartParentChild`: `PID`, bounded exact-child `TerminateAndClose`, and idempotent `CloseWithoutTerminate`; its documentation states that a non-nil value transfers one retained-handle obligation even when PID is invalid or spawn also returns an error.
- injected `Spawn(SelfRestartHandoff)`, one-shot authenticated `Confirm`, injected confirmation wait/backoff, and injected `Exit`.
- typed coordinator failures returned to the composition/handler; no `exec.Cmd`, `os.Process`, `os.Exit`, argv parsing, or ambient process policy.

`internal/cli/gui.go` owns the v3 adapter:

- rebuild argv through the existing Unit A parser-aware path, encode the GUI-owned handoff value, replace the one handoff environment key, resolve the current executable, build/start the detached command, retain the concrete process handle, and implement the two handle operations;
- reuse the existing CLI-owned `configureSupervisorDetach` plus `startSupervisorDetachedBreakaway` mechanics rather than cloning platform flag logic;
- inject `selfRestartProcessExitBoundary(exitRequested, func(){ os.Exit(0) })` directly. The wrapper sets the manager-stop skip flag before the composition-root primitive.

`internal/gui/gui_self_restart.go` removes only the v3 retained-process and v3 exit adapters. The v1 `selfRestartSpawnFn`, `selfRestartExitFn`, `spawnSelfRestartGUI`, handler branch, and test seams remain unchanged for gate OFF. There is one gate decision in `guiSelfRestartHandler`; CLI does not duplicate endpoint selection.

Dependency direction remains `internal/cli -> internal/gui`; GUI never imports CLI. No new package or dependency is introduced.

### 4. Authenticated standby confirmation

Keep `ConfirmAuthenticatedStandby` as a one-request proof verifier, but give its failures a typed classification:

- Retryable `restart-standby-transport-not-ready`: `http.Client.Do` failed before any HTTP response because the fixed loopback endpoint could not be dialed, the caller context is still live, and the wrapped error is a dial/connect `net.OpError`. Connection refusal is the primary case.
- Fail-fast: invalid expected identity or nonce; port mismatch; challenge construction; any HTTP response with non-200 status; redirect response; malformed/oversized/unknown-field proof; MAC/identity mismatch; a transport error after connection establishment; caller cancellation.

The coordinator creates exactly one `context.WithTimeout(deps.Context, Deadlines.Proof)` for the whole confirmation loop. Each attempt receives that same context. A retryable result waits through one injected `WaitConfirm(ctx, backoff)` seam (production condition timer; deterministic tests control release). There is no per-attempt or nested deadline. Deadline expiry maps to `gui-restart-standby-proof-timeout`; fail-fast classification preserves its specific ID. The loop never accepts a child-supplied host or URL and recreates the challenge per attempt.

### 5. Reservation validation and release boundary

Immediately after `MarkerStore.Reserve` returns, validate all success facts used downstream: non-nil record, expected generation/sequence, phase exactly `reserved`, designated child PID/hash, route/ports, and unexpired reservation under injected `Now`. A nil-error malformed result maps to `gui-restart-reservation-invalid` and enters the same pre-release settlement while `parentLeaseReleased == false`.

Only after full validation may the coordinator publish reserved progress, stop the Server hub lifecycle, call `Lease.Release`, and set the local release boolean. After that point it performs no marker/child-readiness decision, wait gate, termination, recovery claim, reacquire, `BindForRecovery`, or activation signal. Permitted actions are only retained-handle close without termination, fixed old-port grace/listener-local close, local result delivery, and CLI exit.

## Failure modes and observable discriminators

| Failure mode | Stable discriminator | Caller/terminal mapping |
| --- | --- | --- |
| Hub producer cannot join inside the five-second owner budget | typed `gui-restart-hub-producer-join-timeout`; existing outer `gui-restart-pre-release-rollback-failed` progress | No success release; terminal rollback cleanup/release/exit. |
| Hub graceful drain expires after producers joined | existing `hub-shutdown-incomplete` | Existing force-close, then successful release if lifecycle is settled. |
| Retryable child not yet listening reaches total Proof deadline | typed `gui-restart-standby-proof-timeout` | Healthy pre-release rollback when restoration/clear proves. |
| Identity/authentication/proof protocol mismatch | typed `gui-restart-standby-identity-invalid`, `gui-restart-standby-authentication-failed`, or `gui-restart-standby-protocol-invalid` | Immediate pre-release rollback; never retried. |
| Reserve returns malformed success | typed `gui-restart-reservation-invalid` | Pre-release rollback; zero reserved progress, hub close, or release. |
| Nonce unlink, retained-handle close, listener restore, or marker clear fails | typed `gui-restart-pre-release-cleanup-failed` with resource field | Interrupt, bounded terminal cleanup, release once, CLI exit. |
| Interrupt marker write also fails | existing `gui-restart-interrupted-marker-write-failed` | Still bounded cleanup, release once, CLI exit. |
| Post-release retained-handle close fails | `gui-restart-child-handle-close-failed` diagnostic | Log only; no protocol re-entry; continue exit. |
| CLI v3 spawn fails | existing `gui-self-restart-v3-spawn-failed`, with typed cause preserved | HTTP 200 non-restarting body after proved cleanup. |

No outbound call gains an unbounded timeout. Confirmation has an explicit no-retry decision for every non-dial failure. Hub and rollback cleanup remain bounded. Authorization remains the existing same-origin rule at `internal/gui/gui_self_restart.go:103-107`; no route becomes public by this correction.

## Anti-layering owner matrix

| Concern | Single owner after correction | Removed/forbidden duplicate |
| --- | --- | --- |
| Hub producer cancel/join/final shutdown | Server hub lifecycle barrier | Direct restart-only `Swap(nil)` and both inline normal-shutdown stacks. |
| Post-`Begin` settlement | Parent attempt cleanup owner | Inline cleanup in nonce/spawn/PID/confirm/reserve branches and a second rollback cleanup layer. |
| Lease release decision | Attempt owner using concrete local release boolean; CLI `releaseOnceLease` supplies idempotence | Marker-derived release state or outer-defer decision logic. |
| V3 OS process and exit | CLI adapter/composition root | GUI-owned `SpawnRestartV3GUI`, retained `exec.Cmd`, and `RequestSelfRestartExit` for v3. |
| Confirmation retry policy | Restart coordinator | Retries inside the one-shot HTTP verifier or CLI. |
| Durable marker transitions | Existing `HandoffMarkerStore` | Caller-written marker files or a second cleanup record. |
| GUI listener admission/recovery | Existing `GUIListenerOwner` | Direct listener bind/handler mutation in coordinator. |

## Intended RED tests and engineered windows

| Finding | Intended RED test | Deterministic window / assertions |
| --- | --- | --- |
| Late hub publisher | `TestRestartV3_HubLifecycleBarrierJoinsPublishersBeforeRelease` with `initializer` and `restart-driver` subtests | Inject start functions that signal entry and block immediately before returning a live bundle. Start restart close, prove it has not returned/released, unblock, then assert both producers settled, the returned/final bundle shut exactly once, slot remains nil, and release occurs last. |
| Duplicated cleanup/residue | `TestRestartV3_PostBeginCleanupSingleOwnerMatrix` | Table-drive nonce creation/length/write, spawn error with nil/non-nil child, remove failure, clear failure, context cancellation, listener restore failure, and interrupt-write failure. Assert each acquired resource closes once; ordinary 200 exists only for residue-free cases; terminal cases release/exit once. |
| Healthy cleanup false degrade | `TestRestartV3_HealthyPreAcceptFailuresLeaveNoMarkerAndEnsureAliveDoesNothing` | Use real marker store; after every proved healthy case assert marker absent, drive one real ensure-alive tick, and assert zero emit/mutation and no `gui-restart-live-holder-wedged`. |
| PID-zero retained child | `TestRestartV3_NonNilPIDZeroChildClosesHandleWithoutTerminate` | Return a non-nil fake with PID zero; assert one close-without-terminate, zero terminate, nonce absent/zeroed, marker absent. |
| GUI/CLI D1 boundary | `TestRestartV3_ParentRuntimeOwnsRetainedProcessAndExitInCLI` | Inject CLI process-start/exit seams; prove parser argv/handoff environment reach the CLI adapter, handle closes once, exit flag is set before the composition-root exit, and GUI v3 path exposes only injected contracts. Existing gate-off v1 test remains unchanged. |
| Confirmation transient | `TestRestartV3_ConfirmRetriesTransportNotReadyUntilAuthenticatedWithinProofDeadline` | Confirmer returns typed not-ready twice, injected wait advances/releases attempts, third returns exact proof; assert one Proof context, three calls, and no rollback. |
| Confirmation mismatch | `TestRestartV3_ConfirmAuthenticationMismatchFailsImmediately` | First attempt returns typed authentication mismatch; assert one call, zero waits, and pre-release rollback. Add identity/protocol subcases. |
| Reservation after release | `TestRestartV3_InvalidReserveResultRollsBackBeforeProgressHubCloseOrRelease` | Return nil, wrong phase, wrong generation/sequence/PID/hash, and expired reservation with nil store error; assert zero reserved progress, zero hub stop, zero release, and proved rollback/marker clear. |
| HTTP response outrun | `TestRestartV3_ResponseCommitsBeforeReleaseAndExit` | Block the response-commit latch and make all coordinator seams instant; assert zero reserve/hub/release/exit until fake writer records status+body+flush. Repeat terminal synchronous cleanup failure and assert 200 precedes its release+exit finalizer. |

All GUI/CLI/API tests that touch state paths must use `-tags=test_state_path_env`; none may set `MCPHUB_GUI_SPAWN_TESTS` or spawn a real GUI. Sweep workspace `mcphub.exe` after test execution.

## Diff-invisible invariants

| Invariant | Named regression guard and expected result |
| --- | --- |
| Gate OFF retains v1 spawn/exit behavior | `TestGUISelfRestart_SpawnSuccess` and `TestRestartV3ChildSelectionLeavesLegacyGateOffHandoffPathUnchanged`: retained v1 success remains unchanged. |
| Spawn failure remains frontend-readable 2xx | `TestRestartV3_SpawnFailureReturns2xxNonRestartingBody` plus terminal-cleanup response-order subtest: HTTP 200 and existing fields precede any exit. |
| Same-port GUI listener close does not close hub early | `TestRestartV3_SamePort_ClosesOnlyGUIListenerAndKeepsHubEventsAlive`: hub/SSE live until the single lifecycle stop boundary. |
| Same-port rollback retains lease and never reacquires | `TestRestartV3_SamePort_PreReleaseRollbackRetainsLeaseAndRebindsWithoutReacquire`: release/reacquire zero, full handler restored. |
| Unit A parser-aware port/argv behavior | `TestRestartV3_PortArgvMatrix` and `TestGuiResetPort*`: existing matrix passes. |
| Child Phase F activates directly after acquisition | Existing `TestRestartV3_ChildStartupUsesStandbyContinuationAndCommits`: no new parent activation signal or wait. |
| Adopted supervisor fleet survives self-restart | `TestRestartV3_SuccessfulHandoffExitSkipsManagerStop` and `TestArmSupervisorManager_AdoptedOwnerReturnsNilNoLoop`: manager stop remains zero. |
| Release occurs exactly once | Existing G1/G3 plus cleanup matrix: success/terminal count one; healthy rollback zero; outer defer no-op. |
| Post-release protocol is inert | `TestRestartV3_ParentPerformsNoProtocolWriteWaitTerminateOrReclaimAfterRelease`: all forbidden seam counters and reservation-validation counter remain zero after release. |
| Normal shutdown cannot regress while adopting the hub barrier | Existing Server cancellation/listener-error shutdown tests plus the two late-publisher arms: no live bundle and no producer goroutine after return. |

## Contract and persisted-state impact

`no contract/persisted-state change`:

- `POST /api/gui/restart` remains same-origin protected.
- Gate ON success remains HTTP 202 with `{handoff_id,generation,phase:"in-progress",spawned:true,spawned_pid,restarting:true,old_port,target_port?}`.
- Gate ON synchronous failure remains HTTP 200/2xx with `{spawned:false,spawned_pid:0,restarting:false,spawn_error}`.
- Gate OFF retains the v1 body and behavior.
- `gui-restart.json` remains version 3.1 with the same four phases and fields. Cleanup timing changes only whether a proved rollback correctly removes it.

## Alternatives rejected

1. **Add cancel to restart close but do not join producers.** Rejected: current publishers CAS after asynchronous start (`internal/gui/server.go:1172-1183`, `internal/gui/hub_listener.go:474-544`); cancel without join leaves exactly the observed post-`Swap` publication window (`item3-unitB-phaseG-architecture-review.md:60-75`).
2. **Keep branch-local cleanup and add more defers/guards.** Rejected: the current five branches already disagree on marker errors, nonce unlink, and non-nil PID-zero handles (`internal/gui/gui_restart_protocol.go:171-216`; review `item3-unitB-phaseG-architecture-review.md:77-88`). A second cleanup layer would preserve conflicting owners and cannot prove AC-G8.
3. **Leave the v3 `exec.Cmd`/`os.Exit` adapter in GUI and wrap it from CLI.** Rejected: that is the current structure (`internal/cli/gui.go:95-119`, `internal/gui/gui_self_restart.go:203-319`) and the accepted review finds it violates the approved CLI/D1 boundary (`item3-unitB-phaseG-architecture-review.md:90-124`). CLI already owns compatible detached/breakaway mechanics, so no new neutral package is necessary.

## Claims

1. `{ guarantee: no initializer or restart-driver CAS can publish a hub bundle after successful restart close returns; single-owner: Server hub lifecycle barrier; enforcement-probe: TestRestartV3_HubLifecycleBarrierJoinsPublishersBeforeRelease/initializer and /restart-driver }`
2. `{ guarantee: normal shutdown and Phase G use one hub cancel/join/take/shutdown operation; single-owner: Server.stopHubLifecycle; enforcement-probe: source inventory finds no shutdown-side hubMcpComp.Swap(nil) outside the owner and normal shutdown tests pass }`
3. `{ guarantee: every post-Begin exit settles nonce marker child listener hub lease and exit through one owner; single-owner: parent restart attempt settlement; enforcement-probe: TestRestartV3_PostBeginCleanupSingleOwnerMatrix plus source inventory finds no branch-local cleanup }`
4. `{ guarantee: a healthy pre-release failure leaves no marker and cannot trigger live-holder-wedged; single-owner: attempt healthy settlement plus HandoffMarkerStore.ClearAfterProvedPreReleaseRollback; enforcement-probe: TestRestartV3_HealthyPreAcceptFailuresLeaveNoMarkerAndEnsureAliveDoesNothing }`
5. `{ guarantee: an unauthenticated or PID-zero child is never terminated but every non-nil retained handle is closed once; single-owner: attempt child authority state and CLI retained-process adapter; enforcement-probe: TestRestartV3_NonNilPIDZeroChildClosesHandleWithoutTerminate }`
6. `{ guarantee: v3 reusable GUI protocol contains no exec.Cmd os.Process or os.Exit ownership; single-owner: CLI restartV3ParentRuntime adapter; enforcement-probe: TestRestartV3_ParentRuntimeOwnsRetainedProcessAndExitInCLI plus source grep }`
7. `{ guarantee: only dial/not-listening transport failures retry and all attempts share one Proof deadline; single-owner: RestartCoordinator confirmation loop; enforcement-probe: TestRestartV3_ConfirmRetriesTransportNotReadyUntilAuthenticatedWithinProofDeadline and TestRestartV3_ConfirmAuthenticationMismatchFailsImmediately }`
8. `{ guarantee: malformed Reserve success is rejected before progress hub stop or lease release; single-owner: RestartCoordinator reservation validator; enforcement-probe: TestRestartV3_InvalidReserveResultRollsBackBeforeProgressHubCloseOrRelease }`
9. `{ guarantee: neither successful nor terminal cleanup release can outrun the handler's 202/200 response commit; single-owner: coordinator start response finalizer invoked by guiRestartV3Handler; enforcement-probe: TestRestartV3_ResponseCommitsBeforeReleaseAndExit }`
10. `{ guarantee: the concrete parentLeaseReleased boolean remains the only rollback gate and release is exactly once; single-owner: parent attempt settlement plus CLI releaseOnceLease; enforcement-probe: existing G2 G3 G4 tests and the cleanup matrix }`
11. `{ guarantee: gate-OFF v1 endpoint and process path are unchanged; single-owner: guiSelfRestartHandler gate branch and retained v1 seams; enforcement-probe: TestGUISelfRestart_SpawnSuccess and TestRestartV3ChildSelectionLeavesLegacyGateOffHandoffPathUnchanged }`
12. `{ guarantee: external HTTP and gui-restart.json contracts do not change; single-owner: guiSelfRestartResponse and HandoffMarkerStore schema; enforcement-probe: G5 endpoint tests plus marker codec tests }`

## Residual risk and implementation notes

- Real Windows retained-process behavior remains seam-tested only because the user forbids real GUI spawn. The CLI adapter tests must therefore assert ownership/call order without starting a process.
- A producer that ignores cancellation beyond the five-second barrier forces the terminal exit path; it cannot be treated as a successful handoff. This is intentional fail-closed behavior, not an automatic hub restart redesign.
- The response-commit signal proves local writer ordering, not remote network receipt. The endpoint still preserves the existing best-effort HTTP behavior; no acknowledgement protocol is introduced.

## Gate

**PASS** — the five confirmed classes have one implementable owner each, exact failure mappings, deterministic RED tests, and no parallel lifecycle or cleanup mechanism. The `$backend-engineer` can correct the existing Phase G diff inside the authorized files without redefining architecture.

## Terms and Abbreviations

- AC: Acceptance Criterion.
- CAS: Compare-and-swap.
- CLI: Command-Line Interface.
- D1: Failure returned from reusable layers; process termination owned by the composition root.
- GUI: Graphical User Interface.
- HTTP: Hypertext Transfer Protocol.
- MAC: Message Authentication Code.
- PID: Process Identifier.
- SSE: Server-Sent Events.

