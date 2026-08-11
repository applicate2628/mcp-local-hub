# Reliability prerequisites — CLI lifecycle ownership

## Decision summary

The six architecture prerequisites are now bounded. They do **not** support a reliability `PASS` for the proposed two-phase supervisor runtime owner:

- Reconcile has a viable smallest context contract, but no deterministic cancellation-in-flight RED receipt exists.
- Transitive writers are classified, but several have no finite settlement bound.
- The two-frame inter-process communication (IPC) contract requires a final frame after `accepted`; immediate connection close is incompatible.
- A join timeout cannot authorize return beneath live writers. The only invariant-preserving bounded escape is composition-root process termination, whose exit-code and restart owners are not approved.
- The process-generation oracle is supportable on Windows and Linux. Native macOS remains a narrow release blocker for this lifecycle guard.
- Both named RED guards still lack required causal seams; the missing seams are specified below.

The functionality-first recommendation is **Policy B: park the baseline CLI lifecycle defects and explicitly waive the broad CLI gate for this console-only release**. It preserves the current hub availability contract and avoids introducing an unproved fatal-exit/restart path. This is a release waiver, not a lifecycle fix, and requires explicit operator approval.

The terminal gate is **REVISE** until the operator chooses Policy A or B. Policy A additionally requires an approved exit-code owner, restart owner, and native macOS process-generation proof or explicit platform narrowing.

## Reliability objectives

| ID | Service-level indicator (SLI) | Measurement point | Window | Threshold | Error-budget consequence |
| --- | --- | --- | --- | --- | --- |
| HUB-AVAIL-1 | Successful installed-hub client handshake and live request | client side, against the restarted installed process | first 10 minutes after install/restart, sampled every 5 seconds | 100% success; zero failed samples | one failed sample exhausts the release error budget and aborts installation rollout or triggers rollback |
| HUB-CONSOLE-1 | Visible console-window count during ordinary startup and operation without exact leading `--debug-console` | Windows desktop/process-window enumerator | every ordinary launch plus the same 10-minute post-restart window | exactly 0 visible console windows | one visible console exhausts the release error budget and blocks publication/install acceptance |
| CLI-SETTLE-1 | Owned producer count and protected-sink writer count after `runSupervise` returns | supervisor composition-root lifecycle probe | every graceful, cancellation, startup-error, and test terminal path | exactly 0 producers and 0 writers before protected resources close | any live owner is a lifecycle release failure; Policy B may waive this objective only for immutable candidate/HEAD-parity baseline defects and may not claim it as met |
| IPC-FINAL-1 | Requests that received `accepted` and then receive exactly one terminal result frame | IPC client on the same connection | every accepted in-flight request during shutdown | 100%; no silent EOF after `accepted` | one missing or duplicate terminal frame blocks Policy A and is a regression under Policy B |

## Architect prerequisite resolution

### 1. Initial Reconcile cancellation contract

**Status: REVISE — contract bounded, causal RED receipt missing.**

Accepted inventory records one production caller in `runSupervise` and 18 direct test callers. The current Reconcile path is context-free across its registry predicate, blocking EventLoop post, spawn callback, and terminate callback. `PostCtx` already exists as the cancellable EventLoop seam.

Smallest compatible contract:

1. Preserve the existing context-free `Reconcile` entry for current direct callers.
2. Add one internal context-aware entry used by the production initial-reconcile worker.
3. Check the context before each descriptor/fan-out and use `PostCtx` for every blocking post.
4. Thread the same context through the registry predicate and spawn/terminate callbacks, or prove a finite bound for each callback before admitting it.
5. Return typed cancellation to `runSupervise`; do not swallow it and do not terminate below the composition root.

The missing falsifying seam is a deterministic test that blocks a known Reconcile dependency, confirms entry into that dependency, cancels the producer context, and observes typed cancellation while EventLoop remains available. The accepted evidence contains no executed RED receipt for that seam.

### 2. Transitive writer ownership

**Status: REVISE — ownership class is explicit; finite settlement is not proved.**

| Participant | Required owner class | Current bound / conflict | Falsifying probe |
| --- | --- | --- | --- |
| EventLoop consumer | runtime-group consumer; cancelled only after producers join | context stop exists | announced producer posts during cancellation; producer joins before `loopDone` |
| direct supervisor workers | named runtime-group producers | some synchronous dependencies have no accepted bound | structural membership guard plus injected blocked dependency |
| daemon child `cmd.Wait` / crash sender | independently joined daemon-lifecycle owner, with its join handle observed by the composition root | `cmd.Wait` has no finite cancellation bound in the collected evidence | retained child blocks; shutdown cannot report settlement before the daemon owner closes its join handle |
| maintenance scheduler | named runtime-group producer | scheduler cancellation exists | cancel during `Tick`; scheduler member must settle |
| maintenance `waitAndRecord` | independently joined maintenance owner; `Shutdown` must expose complete settlement, not merely request it | existing 30-second drain plus 5-second kill wait can still return `still_running`; writer may emit later | force child beyond both windows; protected log/root must remain open until waiter settlement or Policy A termination |
| controller `time.AfterFunc` and respawn callbacks | independently joined controller timer registry, sealed and drained before producer completion | callback can outlive cancellation and block posting | arm callback behind a barrier; seal/cancel must prevent or join its post |
| IPC connection handlers | nested runtime-group producers, registered before launch | connection read can be unblocked by close | accepted connection remains counted until its final-frame path terminates |
| quiesce drains | nested runtime-group producers using producer context | current launch uses `context.Background()` | accepted drain is cancelled by group context and completes its existing result path |
| `SupervisorEventLog.Emit` | protected sink operation owned by each joined caller; sink closes after all callers | file-lock acquisition/write has no finite bound in the collected evidence | hold the lock across shutdown; Policy A must terminate or settlement remains unbounded |
| IPC final `conn.Write` | connection-handler operation | no accepted write deadline | peer stops reading; handler must hit the approved write deadline and terminate before close |

No flat `cancel(); Wait()` satisfies this inventory. The EventLoop must remain alive while producers and independently joined owners settle.

### 3. Compatible IPC and quiesce shutdown result

**Status: contract defined; implementation remains gated.**

The existing wire contract is two frames on one connection: an immediate `accepted` frame followed by a required terminal result frame. Therefore shutdown ordering for an accepted request is:

1. seal new runtime registration;
2. cancel the registered quiesce drain through the producer context;
3. obtain the unchanged production `Drain` result;
4. write exactly one existing terminal-result frame on the same connection under an approved finite write deadline;
5. remove the connection and drain memberships, then close the connection idempotently.

Shutdown must not close an accepted connection before step 4. No new JSON fields, result arithmetic, liveness verdict, retry, or alternate success frame is admitted. If the peer does not read before the write deadline, the handler records the existing write failure, closes the connection, and settles; it does not retry and risk a duplicate terminal frame. A deadline value is an operational-contract choice still owned by the IPC owner and planner.

### 4. Finite settlement or fail-loud termination

**Status: REVISE — finite settlement is disproved for the current inventory; owner policy required.**

The collected evidence cannot prove finite settlement for event-log lock/write, daemon `cmd.Wait`, maintenance `waitAndRecord`, controller timers/respawn posts, or the IPC final write. A timeout followed by return would close logs, temporary roots, and locks beneath live writers and is forbidden.

| Policy | Owner behavior | Operational consequences | Restart consequences | Approval state |
| --- | --- | --- | --- | --- |
| **A — bounded fatal settlement handoff** | At the approved settlement deadline, `runSupervise` records the stable outstanding-owner set and bounded stack through a non-blocking/bounded diagnostic path. | The composition root then performs a non-zero fatal process termination; no leaf terminates. Operating-system process teardown is the terminal ownership boundary, so `runSupervise` never returns beneath live writers. | All in-flight requests fail and the hub is unavailable until an external service/task owner restarts it. If no restart owner or restart budget exists, the hub remains down. | **REVISE**: stable exit code, deadline, diagnostic fallback, external restart owner, restart attempts/backoff, and restart drill are not approved. |
|  | A preserves the no-live-writer-after-return invariant by preventing return. | It changes exit status, monitoring severity, and graceful-shutdown semantics; it is an external operational-contract change. | Repeated stuck shutdown can create a crash/restart loop unless the external owner has bounded backoff and an operator-visible terminal state. |  |
| **B — park baseline lifecycle defects for this console release** | Do not land the proposed CLI runtime-owner redesign. Treat candidate/HEAD-parity broad CLI failures as a documented baseline waiver; retain focused console and unaffected contract gates. | No new fatal exit or graceful-shutdown behavior is introduced. The console release may proceed after all remaining console, platform, QA, publication, install, and live probes pass. | Existing hub restart/availability behavior is preserved; no unproved restart dependency is added. | **Operator decision required** because this waives the work-item's broad CLI gate. |
|  | The lifecycle defect remains open and may not be described as fixed. | Residual risk: shutdown may return while a writer or waiter remains; event-log/temp-root cleanup can race; broad quiesce/lifecycle tests remain order-dependent. | A later dedicated lifecycle delivery must re-enter architecture with Policy A or finite member contracts and native macOS proof. |  |

**Recommendation:** choose **B** for this console-only release because the user priority is a working, always-running hub and the collected evidence does not establish any external restart owner for A. This recommendation does not grant the waiver; only the operator can change the broad release gate.

### 5. No-PID-reuse real-child oracle

| Platform | Gate | Accepted mechanism | Falsifying probe |
| --- | --- | --- | --- |
| Windows | **PASS for design prerequisite** | retain the original process handle; Windows does not reuse that process identifier while the handle remains open; the production probe observes the signaled exit | real hidden helper protocol observes both PIDs, releases one, waits on retained handle, then observes only the long-lived PID without releasing the generation hold |
| Linux | **PASS for design prerequisite** | retain the unreaped child generation; use the existing `/proc/<pid>/stat` zombie refinement for production liveness; pidfd polling may observe readiness without supplying a verdict | same mixed-settlement sequence while the exited child remains unreaped; production liveness excludes the zombie and result remains `Drained=1`, `StillRunning=[long]` |
| native macOS | **REVISE / narrow BLOCKED** | `kqueue` `NOTE_EXIT` can observe exit, but the collected evidence does not prove a post-reap generation hold compatible with the current `Signal(0)` production liveness semantics | native test must prove generation identity through both observation generations without changing production liveness; otherwise obtain explicit platform narrowing |

Windows/Linux support does not convert the native macOS lane to PASS. Under Policy A the lifecycle implementation remains blocked on native macOS unless the operator explicitly narrows the platform contract. Under Policy B this new lifecycle guard is deferred, while the separate native macOS console release verification remains mandatory.

### 6. Named RED seams

**Status: prerequisite precisely bounded; executed RED receipts remain missing.**

| Guard | Missing causal seam | Required RED oracle |
| --- | --- | --- |
| `TestQuiesceHandler_MixedSettlementAfterObservedProbeGeneration` | helper has no causal ready/release protocol; production probe generations have no read-only observer; native macOS lacks an accepted retained-generation adapter | fail because generation N containing both PIDs or later generation M containing only the long PID cannot be causally observed; never fail because a natural two-second window was too short |
| `TestSupervisorLifecycle_OrderSequenceJoinsBeforeTempRootRemoval` | production has no named runtime membership/injected announced producer; lost-child fixture has no owned `loopDone` plus FIFO barrier close sequence | fail because `runSupervise` can return or fixture cleanup can begin before the announced producer/EventLoop settles; root-removal failure is supporting evidence, not the causal oracle |

The existing broad failures are symptoms and do not substitute for these deterministic RED receipts.

## Failure modes, detection, and recovery

| Critical failure mode | Detection signal | Expected latency | Page decision | Verification state | Recovery |
| --- | --- | --- | --- | --- | --- |
| producer or independent owner fails to settle | proposed stable join-timeout event with phase and sorted owner names; bounded stack | approved settlement deadline | page under A; release finding under B | **analysis-only** — deterministic stuck-owner injection not yet executed | A terminates at composition root; B retains current behavior and keeps defect open |
| fatal exit is not restarted | client HUB-AVAIL-1 failure after fatal exit | first failed 5-second sample | page immediately | **analysis-only** — no restart owner/drill accepted | rollback A; restore last known installed build through platform owner |
| accepted IPC request loses final frame | client observes EOF/deadline after `accepted`; handler write-failure event | IPC write deadline | no page for isolated request; release abort on first occurrence in gate | **analysis-only** — final-write-deadline injection missing | close connection after one failed write; no retry/duplicate frame |
| event-log emit blocks shutdown | outstanding owner plus no terminal lifecycle marker | settlement deadline | page under A | **analysis-only** — top failure is justified analysis-only because the collected evidence forbids new probes in this synthesis lane | A terminates; B records residual baseline risk |
| PID reuse invalidates mixed-settlement oracle | platform guard reports generation mismatch | test-local watchdog | no page; block platform gate | Windows/Linux design PASS; native macOS REVISE | retain generation object or narrow platform explicitly |
| visible console appears in ordinary hub use | HUB-CONSOLE-1 enumerator detects one visible console | next 5-second sample | no page; immediate release rollback | existing Windows focused baseline accepted; installed live probe still pending | rollback installed candidate and restore prior working hub |

## Retry, idempotency, and re-entry

- Runtime shutdown is owner-only and idempotent; repeated calls observe the same sealed state and never start a second settlement sequence.
- Starts after seal are rejected before launch. No retry is allowed inside the runtime group.
- The IPC final frame is attempted once. Retrying could duplicate a terminal result after an ambiguous partial write.
- Connection close and listener close are idempotent and removal from membership occurs exactly once.
- Policy A restart retry/backoff belongs to the external service/task owner. Maximum attempts, jitter, and a settled/recovered event are unknown; this is one reason A remains REVISE.
- Policy B introduces no new restart or mutation retry.

## Rollout and rollback safety

| Stage | Entry condition | Abort signal | Observation window | Rollback/drill state |
| --- | --- | --- | --- | --- |
| policy selection | operator chooses A or B explicitly | no recorded decision | before any lifecycle implementation or gate waiver | not applicable |
| A lifecycle implementation | A owners approved; deterministic RED receipts captured; architect and architecture reviewer PASS | any named owner survives, IPC final frame is missing, or native platform guard fails | full relevant test run on each target | **ASSUMPTION (UNVERIFIED)** — no fatal-settlement/restart drill executed |
| B console release continuation | operator records broad CLI waiver; lifecycle surfaces remain parked | candidate introduces a new failure versus immutable HEAD, any console gate regresses, or unrelated contract assertions change | focused candidate/HEAD comparison plus remaining platform/QA gates | code rollback is removal of the unpublished candidate; installed rollback drill remains pending platform execution |
| install/restart | independent QA and publication safety PASS; human publication approval | first HUB-AVAIL-1 failure or any HUB-CONSOLE-1 failure | first 10 minutes, 5-second sampling | **ASSUMPTION (UNVERIFIED)** until platform owner performs and records installed rollback |

## Recovery readiness

- Policy A cannot be admitted until the external restart owner, stable non-zero exit, bounded backoff, maximum attempts, operator-visible exhausted state, and rollback drill are named and exercised.
- Policy B must preserve the previous install artifact until the new installed hub passes the full 10-minute availability/no-console window.
- Neither policy permits closing protected sinks and returning while an owned writer is live.
- Under B, that invariant is not repaired; instead, the proposed lifecycle change is excluded from this release and the baseline defect remains explicitly open.

## Claims

1. `{ guarantee: an accepted IPC request receives exactly one existing terminal result frame before connection close; single-owner: registered IPC connection handler; enforcement-probe: block peer reads during shutdown and observe one final frame or one bounded write failure with handler settlement }`.
2. `{ guarantee: Windows mixed-settlement observation retains the original process generation and does not decide liveness outside production code; single-owner: Windows real-child test helper owner; enforcement-probe: retained-handle mixed-settlement guard observes both then only the long process }`.
3. `{ guarantee: Linux mixed-settlement observation retains the unreaped child generation while production zombie refinement decides liveness; single-owner: Linux real-child test helper owner; enforcement-probe: pidfd-observed exit plus /proc zombie classification yields Drained=1 and StillRunning=[long] }`.
4. `{ guarantee: no shutdown timeout authorizes runSupervise to return beneath live writers; single-owner: runSupervise composition root; enforcement-probe: injected stuck owner either remains joined with sinks open or triggers approved Policy A process termination }`.
5. `{ guarantee: Policy B changes no CLI lifecycle or restart behavior and does not claim the baseline lifecycle defect as fixed; single-owner: release integration owner; enforcement-probe: scoped diff excludes lifecycle surfaces and closure retains the residual-risk waiver }`.
6. `{ guarantee: native macOS is not reported clean without a retained-generation proof or explicit platform narrowing; single-owner: native macOS release gate; enforcement-probe: native mixed-settlement guard or recorded operator narrowing decision }`.

## Gate

**REVISE → operator decision, then `$architect`.**

- Choose **A** only with approved fatal exit/restart ownership and native macOS proof or explicit narrowing.
- Choose **B** to prioritize current hub functionality and continue the console delivery with a recorded broad-CLI baseline waiver.
- No silent timeout return is permitted under either policy.

## Terms and Abbreviations

- **CLI** — command-line interface.
- **IPC** — inter-process communication.
- **PID** — process identifier.
- **SLI / SLO** — service-level indicator / service-level objective.
- **RED / PASS / REVISE / BLOCKED** — deterministic failing guard / accepted gate / bounded correction required / external decision or evidence required.
- **FIFO** — first in, first out.
- **Policy A** — bounded fatal settlement handoff at the composition root.
- **Policy B** — explicit parking of baseline lifecycle defects for the console-only release.
