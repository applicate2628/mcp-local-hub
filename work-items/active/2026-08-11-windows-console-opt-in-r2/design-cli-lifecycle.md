# Architecture package — CLI lifecycle ownership

## Decision summary

The durable production shape is one `runSupervise`-owned, named runtime group with **two shutdown phases**, not a flat `cancel(); Wait()`:

1. seal registration, mark graceful exit, cancel and join every producer while the FIFO EventLoop remains available to drain already-started posts;
2. cancel and join the EventLoop consumer;
3. only then emit the terminal lifecycle result and let listener/file/log/stderr/lock cleanup complete.

The controller test fixture has the smaller equivalent owner: FIFO barrier → cancel → `loopDone` → event-log close → temporary-root cleanup. Quiesce tests use real helper processes plus a read-only observer over the actual production liveness result; the observer cannot supply or change a liveness verdict.

This package returns **REVISE**, not PASS. Current accepted scope lacks three contracts required to make the chosen production owner safe:

- initial `Reconciler.Reconcile` is not context-aware and can block in a FIFO `EventLoop.Post`, an LSP registry predicate, or direct termination (`internal/cli/supervise_reconcile.go:130-268`);
- maintenance `waitAndRecord` goroutines may survive the existing 30-second drain plus 5-second kill wait and still emit after `Shutdown` returns (`internal/cli/supervise_maintenance_adapter.go:302-415`);
- the required real-child, no-PID-reuse quiesce oracle is evidenced on Windows, but its macOS generation-hold equivalent is not established; current Darwin liveness intentionally lacks the Linux zombie refinement (`internal/cli/supervise_quiesce_posix.go`, `internal/cli/supervise_quiesce_test.go:280-317`).

Implementing a flat runtime group before those contracts are accepted can deadlock shutdown or falsely claim that all writers joined. The exact return target is `$reliability-engineer`, followed by this `$architect` again.

## Accepted evidence and spot-check

- Accepted factual package hash: `reliability-cli-adjacent.md` SHA-256 `4161BEFBAD08F31A80CA579669BF279811FA9978269549DFE360D467064506FC`, rechecked in this session.
- The accepted owner chain remains current: EventLoop launch has cancel but no join (`internal/cli/supervise.go:782-861`); all four terminal branches cancel and immediately return (`internal/cli/supervise.go:1594-1714`).
- The accepted fixture chain remains current: `lostChildParoleController` registers context cancellation and event-log close but owns no `loopDone` (`internal/cli/supervise_lostchild_f6_f2_test.go:252-279`); the stale-generation test returns on intermediate state observation (`:672-708`).
- The accepted quiesce chain remains current: the helper has no ready/release handshake (`internal/cli/supervise_quiesce_test.go:13-51`) and `Drain` classifies the actual `isPIDAlive` results (`internal/cli/supervise_quiesce.go:103-151`).
- Additional architecture inventory found fourteen direct `go` launches in `runSupervise` (`internal/cli/supervise.go:858-1544`) plus nested IPC connection handlers (`:1856-1929`) and quiesce drains (`:2234-2270`). Those nested owners are material to a no-writer-after-return guarantee and are not optional detail.

## Chosen design, pending prerequisite acceptance

### 1. Production owner

`runSupervise` constructs exactly one package-private `supervisorRuntimeGroup` after the event log opens and before the first worker starts. The group is the single owner of registration state, shutdown state, producer context, EventLoop context, named membership, active IPC connections, and the join oracle.

Conceptual internal surface; names are architectural, not implementation code:

| Operation | Contract |
| --- | --- |
| `StartProducer(name, fn)` | Under the group mutex, reject after seal; otherwise add the named member before launch. The wrapper retains the existing panic guard, executes `fn(producerCtx)`, records settlement, then decrements membership. |
| `StartConsumer("event-loop", fn)` | Same pre-launch registration, but uses the separately cancellable EventLoop context and the consumer membership set. Only the EventLoop belongs to this phase. |
| `OwnListener(listener)` | Registers the IPC listener as an unblocker. Shutdown closes it after sealing so `Accept` returns `net.ErrClosed`. |
| `StartConnection(conn, fn)` | Registers an accepted connection before launch and retains it in the active-connection set. Shutdown closes all active connections so the 60-second read deadline is not the join bound. |
| `StartNested(name, fn)` | Used by connection handlers for quiesce drains. It inherits `producerCtx`; a post-seal start is rejected before a goroutine exists. |
| `Shutdown(reason)` | Owner-only and idempotent: seal → cancel producers → close listener/connections → settle accepted maintenance/reconcile resources → join producers → cancel EventLoop → join consumer → return one structured result. No worker calls it. |
| `Outstanding()` | Stable, sorted names and phase for diagnostics/tests; contains no command line, path, environment, or customer data. |

The group is not a generic package dependency. It is local to the supervisor composition root because it owns process-lifetime resources and exit ordering (`runSupervise` is the composition root at `internal/cli/supervise.go:582-1716`). Standard-library synchronization and contexts are sufficient; no dependency is added.

### 2. Shutdown ordering

One owner-side helper replaces the four copied terminal sequences without changing trigger classification:

1. emit the existing trigger event (`supervisor-exit-signal` or `supervisor-exit-ctx`);
2. call `gracefulInProgress.Enter()` exactly as today;
3. seal the runtime group;
4. cancel the producer context and close the IPC listener plus every active connection;
5. perform the accepted maintenance child settlement contract;
6. join every named producer while EventLoop remains alive to drain already-issued blocking posts;
7. cancel and join EventLoop;
8. emit the existing `supervisor-exit` row on the three nil-return graceful paths; retain the current context-cancel row/result on parent cancellation;
9. write the existing stderr graceful banner and return the same nil or `ctx.Err()` result;
10. outer defers close the event log, stderr sink, and singleton lock only after group settlement.

The EventLoop must not be cancelled with its producers. `IntentWatcher`, crash bridge, initial reconcile, liveness, port-gate, and realloc workers can post while settling (`internal/cli/supervise.go:1228-1431`; `supervisor_controller.go:4230-4250,4548-4595`; `supervise_realloc.go:394-409`). Simultaneous cancellation permits the consumer to exit before a blocking producer post and creates a join deadlock.

Join failure has a bounded **detection** deadline, but never an unsafe bounded abandonment. At the deadline, the owner emits one fail-loud event containing sorted outstanding owner names and captures a bounded stack; it does not close sinks or return while a writer is live. A hard production termination policy would change exit codes and requires separate operator approval. Every accepted worker contract must instead provide a finite cancellation bound; that premise is currently missing for Reconcile and maintenance waiters, hence REVISE.

### 3. Startup-error coverage

The group cleanup is registered immediately after group construction, later than event-log/stderr/lock defers so last-in-first-out execution settles workers before those resources close. It covers all errors after the first launch:

| `runSupervise` return | Worker state at return site | Required owner action |
| --- | --- | --- |
| state-dir resolution error (`:592-595`) | no resource group | Owner-exempt; preserve error. |
| singleton lock error (`:603-607`) | no resource group | Owner-exempt; preserve error. |
| event-log open error (`:635-640`) | no worker started | Owner-exempt; preserve error and existing outer cleanup. |
| intent load error (`:947-957`) | EventLoop started | `Shutdown("startup-intent-error")`, then return the original wrapped error. |
| fail-closed collapse error (`:959-969`) | EventLoop started | Same; preserve original error identity. |
| supervisor-state load error (`:985-996`) | EventLoop started | Same. |
| overlay load error (`:999-1012`) | EventLoop started | Same. |
| IPC listener bind error (`:1040-1044`) | EventLoop started; no IPC worker | Same; no listener hook registered. |
| signal (`:1595-1628`) | complete conditional membership | Existing events/result, common graceful shutdown. |
| test exit (`:1629-1659`) | complete conditional membership | Existing events/result, common graceful shutdown. |
| IPC graceful exit (`:1660-1692`) | complete conditional membership | Existing events/result, common graceful shutdown. |
| parent context cancellation (`:1693-1714`) | parent already cancelled | Common shutdown; preserve `ctx.Err()`. |

Returns inside callbacks/closures at `supervise.go:810,1211,1256,1273,1318,1479,1529,1562` are not composition-root returns and do not own shutdown.

## Complete direct-worker inventory

| Worker name | Launch / condition | Current stop behavior | Runtime classification | Prerequisite or proof |
| --- | --- | --- | --- | --- |
| `event-loop-run` | `supervise.go:858-861`; unconditional after loop creation | `loopCtx.Done()` (`internal/api/supervisor_event_loop.go:259-283`) | sole consumer member | Cancel only after every producer joins. |
| `stall-dump-worker` | `supervise.go:1072-1075`; `!noIPC` | returns on context, but an in-flight capture performs synchronous file/event writes (`supervise_stall_dump.go:367-430`) | producer member | Verify the capture path has a finite state-write bound. |
| `stall-dump-sentinel-watcher` | `supervise.go:1076-1079`; `!noIPC` | context-select between ticks (`supervise_stall_dump.go:643-688`) | producer member | Existing bounded filesystem operations; join guard proves. |
| `ipc-accept-loop` | `supervise.go:1108-1111`; `!noIPC` | only listener close unblocks Accept (`supervise.go:1856-1919`) | producer member plus listener unblocker | Listener ownership moves into group shutdown. |
| `crash-event-bridge` | `supervise.go:1237-1240`; production spawn only | context-select, then blocking EventLoop post (`supervisor_controller.go:4557-4595`) | producer member | EventLoop stays alive until bridge joins. |
| `port-gate-worker` | `supervise.go:1260-1263`; production spawn only | context-select, but in-flight classification/termination may run up to its internal bound (`supervisor_controller.go:4230-4305`) | producer member | Verify each blocking dependency's finite cancellation/timeout. |
| `ephemeral-range-warmup` | `supervise.go:1292-1295`; production spawn only | no context; documented netsh deadline is 3 seconds (`supervise.go:1286-1295`) | producer member | Named guard proves the installed implementation respects the bound; otherwise REVISE. |
| `realloc-worker` | `supervise.go:1297-1300`; production spawn only | context-select; in-flight allocation/persistence is synchronous (`supervise_realloc.go:394-455`) | producer member | Finite lock/I/O bound must be evidenced. |
| `supervisor-liveness-monitor` | `supervise.go:1320-1323`; production spawn only | context checked between full synchronous sweeps (`supervise_liveness.go:144-202`) | producer member | Sweep dependency bound required; EventLoop remains alive for its posts. |
| `quarantine-parole-monitor` | `supervise.go:1329-1332`; production spawn only | context-select and nonblocking TryPost (`supervisor_controller.go:527-549`) | producer member | Existing stop contract is suitable. |
| `intent-watcher` | `supervise.go:1428-1431`; unconditional | context checked between callbacks; callback performs reads and blocking posts (`supervise_watcher.go:149-176`; `supervise.go:1347-1427`) | producer member | EventLoop remains alive; callback I/O bound must be evidenced. |
| `initial-reconcile` | `supervise.go:1484-1487`; `intent != nil` | **no context**; can call registry predicate, blocking Post, or terminate (`supervise_reconcile.go:130-268`) | producer member, **unsafe today** | Required accepted context/cancellation contract. |
| `maintenance-scheduler` | `supervise.go:1526-1531`; unconditional | context checked between `Tick` calls (`supervise_maintenance_adapter.go:521-544`) | producer member | In-flight `Tick` and spawned waiters need one accepted settlement owner. |
| `supervisor-heartbeat` | `supervise.go:1540-1544`; unconditional | context checked between event-log emits (`supervisor_heartbeat.go:62-115`) | producer member | Event-log emit bound must be evidenced. |

All fourteen are members when started; no direct launch is owner-exempt. The condition table is the authoritative membership inventory and the structural guard consumes it.

## Nested and adjacent resource inventory

| Object / worker | Existing owner and close representation | Required classification |
| --- | --- | --- |
| IPC connection handler | accept loop launches an untracked goroutine; connection owns `Close`, read loop has a 60-second deadline (`supervise.go:1922-2053`) | group member registered before launch; active connection closed on shutdown. |
| Quiesce drain | connection handler launches with `context.Background()` and waits for result (`supervise.go:2234-2270`) | nested group member using producer context plus requested deadline; it still calls the unchanged real `Drain`. |
| Production daemon waiters / crash senders | spawn subsystem starts child waits and posts to `crashCh` (owner chain described at `supervise.go:1140-1159,1228-1240`) | adjacent process-lifecycle owner; exact shutdown settlement is **ASSUMPTION (UNVERIFIED)** and belongs in the prerequisite reliability inventory. |
| Maintenance `waitAndRecord` | `maintenanceSpawner.Start` launches it; child `done` and `Shutdown` represent settlement (`supervise_maintenance_adapter.go:295-415`) | cannot be cleanly exempt because it can emit after Shutdown; accepted owner contract required. |
| Controller `time.AfterFunc` follow-up | default `reapAfterFunc` is documented at `supervise.go:1198-1204` | adjacent conditional worker; cancellation/settlement inventory is **ASSUMPTION (UNVERIFIED)**. |
| IPC listener | deferred `Close` currently unblocks Accept (`supervise.go:1040-1046,1868-1883`) | runtime-owned stop hook, not an outer sink close. |
| Event log | opened before workers and captured by almost every worker (`supervise.go:635-666`) | protected sink; closes only after group join. |
| stderr sink | process-lifetime panic sink (`supervise.go:610-628`) | protected sink; graceful banner after join, release after group. |
| singleton lock / owner sidecar | outer deferred release (`supervise.go:598-608`) | protected ownership token; last resource released. |
| signal subscription | `signal.Notify` plus deferred `signal.Stop` (`supervise.go:1578-1586`) | owner-exempt after select; no writer. |

## Reconcile prerequisite contract

A runtime group is unsafe until reliability establishes an accepted cancellation contract for the initial pass. The minimal architectural candidate is additive:

- retain existing `Reconcile(...)` for current callers;
- add an internal context-aware form used by `runSupervise`;
- check cancellation before every descriptor and before every fan-out;
- use a cancellable EventLoop post seam so producer shutdown cannot block after the consumer stops;
- make the production termination callback honor the same context or prove its finite upper bound;
- return a typed cancellation/error result to the composition root instead of swallowing it.

This is not approved implementation yet. It affects `Reconciler`, EventLoop posting, and termination callers beyond the accepted `supervise.go` production surface. `$reliability-engineer` must enumerate callers and produce real cancellation-in-flight evidence before this architect can approve it.

## Quiesce deterministic test seam

### Chosen observation shape

The observer is parameterized over the real production algorithm, not installed as a mutable global and not allowed to return a verdict:

- the core drain implementation accepts a compile-time observer type whose only operation receives the actual ordered alive-PID slice after a complete `aliveTransientPIDs` pass;
- production `Drain` instantiates a zero-sized no-op observer;
- the named test instantiates a channel observer that assigns monotonic generations and copies the observed slice;
- `isPIDAlive`, probe order, 50-millisecond cadence, timeout arithmetic, context semantics, and `QuiesceResult` remain the sole production decision path.

The no-op instantiation is intended to inline away with no residual observer branch, allocation, copy, atomic, or call. That is **ASSUMPTION (UNVERIFIED)** until an object-code/benchmark probe compares the production `Drain` hot path before and after. A runtime global callback is forbidden because it adds mutable process-global state, creates cross-test races, and makes production pay an observer branch.

### Real helper protocol

1. Re-execute the current test binary as two hidden child helpers. Each writes `ready` through an inherited pipe only after bootstrap and blocks on its own release pipe.
2. Parent waits for both ready messages before starting `Drain`; no sleep is readiness.
3. Parent retains the original process-generation handle/descriptor for both helpers.
4. Observer waits for generation N containing both PIDs.
5. Parent releases only the short helper and waits on the retained generation object for exit without discarding the generation hold.
6. Observer waits for generation M>N containing only the long PID, then cancels the drain context.
7. Oracle is exactly `Drained=1`, `StillRunning=[long]`, `InProgress=false`; cleanup reaps/terminates both helpers and joins the observer on every path.

Windows has accepted source and official semantics for a retained process handle. Linux has an in-repo zombie refinement and pidfd capabilities, but the exact test-generation hold is unverified. Darwin has neither the Linux `/proc` zombie refinement nor an accepted generation-hold recipe in this package. The test cannot call `Wait` and then trust a bare PID without reintroducing PID reuse. The reliability prerequisite must resolve all target platforms or explicitly obtain approval for a Windows-only guard plus a separate macOS oracle.

### Named guard

`TestQuiesceHandler_MixedSettlementAfterObservedProbeGeneration`

- RED before implementation: helpers expose no ready/release protocol and `QuiesceHandler` exposes no read-only observation of real probe generations (`supervise_quiesce_test.go:13-51,280-317`; `supervise_quiesce.go:103-151`). The guard must fail on missing causal seams, not on a natural deadline.
- GREEN terminal oracle: generations N and M are observed in order, M contains only the retained long process, result is `(1,[long])`, `InProgress` is false, and no helper/observer remains.
- Forbidden oracle: injected liveness result, sleep, CPU-load injection, repeated retry-to-green, a PID after generation hold was released, or a weaker proxy state.

## Lost-child fixture owner

The fixture becomes the sole owner of EventLoop start and close. It must not return a loop that callers launch ad hoc.

| Participant | Design |
| --- | --- |
| hardened temp root | remains test framework-owned and registered first. |
| event log | opened by fixture; no independent cleanup that can race the loop. |
| context/cancel | fixture-owned. |
| EventLoop | fixture starts it exactly once after handlers can be registered through the fixture seam, or exposes one idempotent `Start` method; no caller uses raw `go loop.Run`. |
| `loopDone` | closed only by the Run wrapper after `Run` returns. |
| FIFO barrier | existing `evControllerBarrier` / `evReapBarrier` alias and acknowledgement key (`supervisor_controller.go:728-754,2846-2850`). |
| close | idempotent cleanup: post barrier → wait acknowledgement → cancel → wait `loopDone` → close event log. Root cleanup follows by cleanup registration order. |
| non-loop tests | fixture records not-started and closes the event log without posting a barrier. |

Every caller in `supervise_lostchild_f6_f2_test.go` must consume the fixture owner instead of launching the loop. `supervise_prespawn_binary_gate_test.go:65-91` already demonstrates a local cancel+join owner; it must be reconciled to the single fixture owner rather than retain a second loop lifetime. That sibling is in the declared test blast radius even though the original symptom was in stale-liveness.

The stale-generation test may still observe its state transition for its semantic assertion, but fixture close uses the FIFO barrier as the terminal settlement oracle. State observation never substitutes for handler completion.

### Named guard

`TestSupervisorLifecycle_OrderSequenceJoinsBeforeTempRootRemoval`

- RED before implementation: the lost-child fixture has no `loopDone`, and `runSupervise` returns after cancel without waiting for an announced worker (`supervise_lostchild_f6_f2_test.go:252-279,672-708`; `supervise.go:1594-1714`).
- GREEN controller oracle: prior FIFO handler work acknowledges the existing barrier, cancel is delivered, `loopDone` closes, event log closes, and the exact root can be removed and recreated immediately.
- GREEN production oracle after prerequisites: each terminal branch blocks until named active membership is zero; EventLoop settles after producers; protected resources remain open until then.
- The one-second test watchdog diagnoses a stuck owner by name and stack. It is not permission to abandon the owner or retry root removal.

## All-return-path membership guard

Add a focused abstract-syntax-tree guard beside the lifecycle tests. Its authoritative table contains the fourteen launch names and their condition class from this design. The guard must prove:

1. every direct `go` statement within `runSupervise` is inside the runtime group's named start wrapper;
2. the name is a unique compile-time string present in the authoritative table;
3. every conditional table row has exactly one matching launch;
4. EventLoop is the only consumer-phase member;
5. no member body calls `Shutdown`, `Wait`, or the group's join method;
6. `acceptIPCConnections` cannot use a raw `go` for a connection handler and `handleQuiesceTimers` cannot use a raw `go` for its drain;
7. every composition-root return after the first worker launch is dominated by the single shutdown owner.

This is a structural owner guard, not a source-line-count or regex assertion. A mutation that replaces one registered launch with raw `go` must make it RED.

## Change-Surface Contract

`{ intended change surface: test-only quiesce helpers/observer guard; lost-child fixture lifecycle and its direct prespawn caller; one supervisor composition-root runtime owner plus only prerequisite-approved cancellation adapters; approved extension seams: QuiesceHandler compile-time result observer, lostChild fixture Start/Close owner, runSupervise named two-phase runtime group, ipcDispatchDeps runtime registration, prerequisite context-aware Reconcile entry; protected / must-not-touch surfaces: GUI source/tests, Drain liveness/arithmetic/cadence/wire result, state and intent schemas, state-machine transitions/generation/persistence, reaper/readiness meaning and order, daemon spawn policy, console/PE/installer/npm behavior, exact leading --debug-console grammar and default hidden-console composition; declared blast radius: internal/cli supervisor lifecycle and named tests only, plus a separately accepted Reconciler/maintenance cancellation contract if reliability proves it necessary }`

### Allowed surface after prerequisite PASS

- `internal/cli/supervise.go`
- one focused runtime-group source/test pair in `internal/cli/`, classified under the supervisor lifecycle owner
- `internal/cli/supervise_quiesce.go` and platform test-helper files only for the non-authoritative observer and real helper protocol
- `internal/cli/supervise_quiesce_test.go`
- `internal/cli/supervise_lostchild_f6_f2_test.go`
- `internal/cli/supervise_prespawn_binary_gate_test.go` only to remove the duplicate fixture lifetime
- `internal/cli/supervise_reconcile.go` and maintenance owner files only after the returned reliability package explicitly admits their contract deltas

### Forbidden surface

- GUI, process console composition, Portable Executable flags, npm launchers, installers, fleet state, or hub configuration
- `QuiesceResult`, JSON shape, polling interval, `isPIDAlive` verdict, `TransientPID` schema, or PID-reuse production hardening
- state-machine transitions, generation checks, persistence semantics, reaper order, or `reconcile_ready` meaning
- per-test sleeps, root-removal retries, `TestMain` leak hiding, duplicate done channels, a global mutable liveness hook, or a second runtime owner
- new dependency

## Preserved and changed behavior

| Surface | Preserved | Changed |
| --- | --- | --- |
| CLI result | normal signal/test/IPC exits return nil; parent cancellation returns `ctx.Err()`; startup errors retain their wrapped cause | shutdown does not return before owned workers settle; a verified lifecycle failure may add a typed joined cause only after policy approval. |
| events | existing trigger and exit event IDs/bodies remain | terminal `supervisor-exit` becomes a truthful post-join marker; a new stable join-timeout diagnostic is proposed and requires registry review. |
| readiness/reaper | cold-start reaper still precedes EventLoop and readiness; readiness still means scheduled | none. |
| IPC | hello/request/result wire shapes and idle deadline remain | listener/connections are explicitly closed during shutdown; accepted in-flight drain cancellation must retain its final frame contract. |
| quiesce | real liveness, arithmetic, order, timeout, result shape | test-only observation and causal helper coordination. |
| lost-child state machine | transition/generation/persistence semantics | fixture settlement becomes explicit. |
| console | exact leading `--debug-console` opt-in and ordinary hidden-console behavior | none directly or indirectly. |

No external contract or persisted-state/schema migration is designed. Internal lifecycle timing changes: graceful exit waits for verified bounded settlement. That operational timing contract cannot be approved until reliability supplies bounds for every member.

## Dependency direction and ownership graph

```text
runSupervise (composition root)
  owns supervisorRuntimeGroup
    producer phase
      IPC accept -> registered connection -> registered quiesce drain
      diagnostics / watcher / reconcile / controllers / scheduler / heartbeat
    consumer phase
      EventLoop.Run
  owns protected resources
    listener unblocker -> event log -> stderr sink -> singleton lock

test fixture
  owns EventLoop + barrier + cancel + loopDone + event log + root order

QuiesceHandler
  owns real liveness decisions
  accepts only a non-authoritative compile-time observer in tests
```

No worker owns or calls the composition-root shutdown. Producers depend on the stable EventLoop posting surface; EventLoop does not depend on the runtime group. Test support remains test-specific and parameterized over the production result.

## Diff-invisible invariants and named regression guards

| Invariant | Status | Named regression guard and terminal result |
| --- | --- | --- |
| Every worker is registered before it can outlive the owner. | **ASSUMPTION (UNVERIFIED)** | all-return-path AST membership guard detects any raw or missing launch; mutation proof must be RED. |
| Producers settle before EventLoop cancellation. | **ASSUMPTION (UNVERIFIED)** | `TestSupervisorLifecycle_OrderSequenceJoinsBeforeTempRootRemoval`: announced producer posts during cancellation, settles, then loopDone closes. |
| Every terminal path cancels and joins before protected resources close. | **ASSUMPTION (UNVERIFIED)** | same named guard covers startup error, signal, test, IPC, parent cancellation; active count zero before close probes. |
| No worker calls Wait/Shutdown from inside itself. | **ASSUMPTION (UNVERIFIED)** | AST guard rejects member-body owner re-entry; race guard remains green. |
| Join failure is visible without abandoning writers. | **ASSUMPTION (UNVERIFIED)** | injected stuck worker produces one stable outstanding-owner event/stack by watchdog deadline and proves sinks remain open; policy for eventual termination remains prerequisite. |
| Quiesce observes actual production probe generations and retains process generation. | **ASSUMPTION (UNVERIFIED)** | `TestQuiesceHandler_MixedSettlementAfterObservedProbeGeneration` exact real-child oracle on every supported OS. |
| Fixture close order is barrier → cancel → loopDone → log/root cleanup. | **ASSUMPTION (UNVERIFIED)** | controller phase of lifecycle order test; immediate remove/recreate succeeds. |
| No state-machine or wire semantics change. | enforceable after diff | existing quiesce, lost-child, reconcile, IPC contract suites remain green without assertion edits. |
| No console behavior changes. | protected accepted baseline | scoped diff excludes console surfaces; accepted 6/6 top-level and 63/63 baseline remains valid unless a relevant source byte changes. |

## Failure, degradation, observability, and recovery

| Failure mode | Observable discriminator | Degradation / recovery |
| --- | --- | --- |
| start attempted after seal | proposed stable `supervisor-runtime-start-after-shutdown`, worker name | do not launch; caller returns/cancels its request. No unowned goroutine. |
| producer misses settlement deadline | proposed `supervisor-runtime-join-timeout`, phase=`producer`, sorted owners, bounded stack | keep EventLoop and sinks open; continue safe join or execute separately approved process-termination policy. Never return and close beneath writer. |
| EventLoop misses settlement deadline | same event, phase=`consumer`, owner=`event-loop-run` | producers are already zero; retain sinks/lock and fail loud. |
| listener close fails | proposed `supervisor-runtime-stop-hook-failed`, owner=`ipc-listener` | close all known connections, report exact error, do not claim accept-loop join. |
| connection/quiesce survives cancel | join-timeout names `ipc-connection-handler` or `quiesce-timers-drain` with request identifier only if non-sensitive | keep sinks open; final response/error contract follows prerequisite decision. |
| Reconcile ignores cancellation | join-timeout owner=`initial-reconcile` | current design cannot recover safely; prerequisite blocks implementation. |
| maintenance child waiter survives shutdown | join-timeout owner=`maintenance-wait:<pid>` with PID only | existing still-running policy conflicts with full join; prerequisite blocks implementation. |
| quiesce helper not ready or wrong generation | test failure includes ready states, generations, and actual alive sets | cancel/join helpers; no retry and no timeout extension. |
| fixture barrier or loop join stalls | test failure names phase and captures stack | cancel if possible, join before log/root cleanup; test remains failed. |

Stable event identifiers are an observable contract and require the existing registry owner/reviewer before implementation. Logs contain owner names and PIDs only; they must not capture command lines, environment, absolute paths, or IPC bodies.

## Security and resource safety

- Runtime membership is closed before cancellation, preventing retry/re-entry from creating post-shutdown work.
- Active connection close is idempotent; connection removal occurs exactly once on the handler wrapper's terminal path.
- Quiesce observation is read-only and cannot forge liveness, weakening neither process-identity nor force-kill policy.
- No ambient environment, wall-clock readiness, filesystem ordering, or uncontrolled PID is an acceptance oracle.
- No secret, command line, environment, request body, or machine-local path enters lifecycle diagnostics.
- Panic attribution remains fail-loud through the existing `guardSupervisorGoroutine`; the runtime wrapper must compose with it, not swallow it.

## Alternatives rejected

| Alternative | Decisive rejection driver |
| --- | --- |
| flat shared context plus one WaitGroup | A producer can block posting after EventLoop observes the same cancellation and exits; source has multiple blocking post paths. This can deadlock shutdown. |
| cancel/close files and rely on process exit to kill goroutines | Reproduces the accepted TempDir/event-log writer defect and violates explicit resource ownership. |
| one done channel per worker | Duplicates lifecycle logic and makes conditional/nested launches easy to omit; there is no single auditable membership owner. |
| wait with timeout, return, and close sinks while workers remain | Hides the root and abandons writers. A timeout is a diagnostic threshold, not permission to violate ownership. |
| change `Drain` arithmetic/liveness or inject synthetic verdicts | Accepted evidence shows arithmetic matches actual probe input; synthetic liveness does not test production. |
| sleep for helpers or retry root removal | Natural timing caused the defect; retries only hide the race. |
| use a mutable global observer callback | Adds process-global state, cross-test races, and a production hot-path branch. |
| import `errgroup` or another lifecycle dependency | Standard library suffices; dependency adds no missing cancellation contract. |

## Object-axis record

| Axis | Record |
| --- | --- |
| primary lifecycle objects | `supervisorRuntimeGroup`, producer context, EventLoop context, named member records, active connection set, EventLoop, listener, event log, stderr sink, singleton lock. |
| adjacent conditional resources / close representations | IPC connection `Close`; quiesce result channel/context; maintenance process `done` plus `Shutdown`; daemon wait/crash channel; controller `time.AfterFunc`; signal subscription; test `loopDone`; FIFO barrier acknowledgement; helper process-generation handle. |
| single-writer facts | runtime group alone mutates membership/seal; EventLoop alone consumes controller events; fixture alone starts/closes its loop; QuiesceHandler alone decides liveness. |
| decision facts | fourteen direct launches; IPC and quiesce add nested launches; four terminal branches share cancel-without-join; initial Reconcile and maintenance waiters lack an accepted finite stop contract. |
| evidence | `supervise.go:582-1716,1856-2053,2234-2270`; `supervise_reconcile.go:130-268`; `supervise_maintenance_adapter.go:295-415`; `supervise_lostchild_f6_f2_test.go:252-279,672-708`; accepted reliability package SHA above. |
| C1 single-owner verdict | **REVISE**. Chosen owner is coherent, but a clean guarantee is false until reconcile, maintenance, timer/daemon waiter, and cross-platform process-generation prerequisites are accepted. |

## Claims for architecture review after REVISE closes

1. `{ guarantee: every direct, conditional, and nested supervisor worker is registered before launch and no start succeeds after shutdown seal; single-owner: supervisorRuntimeGroup membership registry; enforcement-probe: all-return-path AST membership guard plus raw-go mutation proof }`.
2. `{ guarantee: every producer joins while EventLoop can still drain posts, then EventLoop joins before protected resources close; single-owner: supervisorRuntimeGroup two-phase Shutdown; enforcement-probe: TestSupervisorLifecycle_OrderSequenceJoinsBeforeTempRootRemoval on every terminal branch }`.
3. `{ guarantee: no worker invokes group Wait or Shutdown from inside group membership; single-owner: supervisorRuntimeGroup API boundary; enforcement-probe: structural member-body re-entry guard }`.
4. `{ guarantee: quiesce test observation cannot alter liveness and adds no residual production observer branch, call, allocation, or copy; single-owner: compile-time quiesce probe observer instantiation; enforcement-probe: named mixed-settlement guard plus production object-code and allocation comparison }`.
5. `{ guarantee: controller fixtures close only after FIFO settlement and EventLoop return; single-owner: lostChild fixture Close; enforcement-probe: controller phase of TestSupervisorLifecycle_OrderSequenceJoinsBeforeTempRootRemoval }`.
6. `{ guarantee: external IPC, state, readiness, reaper, daemon-spawn, and console contracts remain unchanged; single-owner: runSupervise composition boundary and existing contract owners; enforcement-probe: scoped diff plus existing IPC/reconcile/lost-child/quiesce suites and protected console baseline }`.
7. `{ guarantee: a missed join is reported by stable owner name without abandoning a writer or leaking sensitive data; single-owner: supervisorRuntimeGroup watchdog diagnostics; enforcement-probe: injected stuck-owner test proves event/stack and open sinks at the detection deadline }`.

These are local work-item decisions, not a cross-repository architecture policy; no `work-items/decisions/` record is created at REVISE.

## Required reliability revision

Return exactly to `$reliability-engineer` for one bounded prerequisite artifact that must:

1. reproduce or deterministically inject cancellation during initial Reconcile; enumerate all Reconciler callers, blocking posts, registry reads, spawn/terminate callbacks, and establish the smallest context contract;
2. inventory production daemon waiters, maintenance `waitAndRecord`, controller `time.AfterFunc`, and any other transitive writer that can emit/persist after direct workers settle; classify each as group member or independently joined owner with a falsifying probe;
3. define a compatible in-flight IPC connection/quiesce shutdown result, including final-frame behavior after the immediate accepted frame;
4. prove finite cancellation/settlement bounds for every proposed group member, or name the exact approved fail-loud process termination policy;
5. establish a no-PID-reuse real-child oracle on Windows, Linux, and native macOS without changing production liveness semantics, or obtain an explicit narrower platform decision;
6. provide RED receipts for the two named guards or label the still-missing seams precisely.

After that package passes, return to `$architect` for a bounded revision of this document. Only an independent `$architecture-reviewer` PASS may release planning and RED-first implementation.

## Gate

**REVISE → `$reliability-engineer`** — the two-phase runtime owner, deterministic fixture close, and non-authoritative quiesce observer are the chosen architecture, but implementation is blocked by verified non-context-aware Reconcile and maintenance waiter conflicts plus unverified transitive timer/daemon ownership and macOS generation retention. A flat or partially tracked group would create the exact deadlock/leak class this work is meant to remove.

## Terms and Abbreviations

- **CLI** — command-line interface.
- **IPC** — inter-process communication.
- **FIFO** — first in, first out.
- **PID** — process identifier.
- **C1** — the architecture rule requiring exactly one owner for a cross-cutting invariant.
- **RED / GREEN** — a deterministic failing guard before correction / passing guard after correction.
- **PASS / REVISE** — ready for the next gate / bounded correction required before progression.
- **ASSUMPTION (UNVERIFIED)** — a claim with an explicit resolving probe that has not executed.
