## Provenance

- Execution role: external-worker
- Assigned / replaced internal role: architect
- Requested provider: codex
- Resolved provider: Codex CLI
- Requested consultant mode: not-applicable
- Actual execution path: external CLI (Codex CLI)
- Model / profile used: gpt-5.6-sol / xhigh
- Launch flags: `-m gpt-5.6-sol -c model_reasoning_effort="xhigh" -s read-only -C <neutral> --skip-git-repo-check --ephemeral --ignore-rules`
- Run record: started `2026-07-27T05:26:15.2642711+03:00`; finished
  `2026-07-27T05:31:39.5872558+03:00`; exit `0`; prompt/stdout/stderr/exit
  evidence under `.scratch/pr589-design-panel-20260727-0522/lane-owner-seam.*`
- Deviation reason: none

## Panel identity

- Panel disposition: INPUT_ONLY
- Lane: owner-seam
- Framing: minimize blast radius by assigning each open class to its existing resource, process, or observability owner.
- Pinned-input identity: PR589-R4-F5-F7-HEAD-db879c5f
- Protected baseline: F1–F4 at local commit `f150be61`.
- Evaluated head: local `db879c5f`; remote `2d68690f`.

## Candidate design

### Ownership map

| Class | Owner | Seam and proposed contract |
|---|---|---|
| F5 | Phase-I graphical user interface (GUI) lease operation | Return an explicit, tick-local lease disposition to the outer liveness coordinator. The coordinator consumes it once at the common GUI-owner scheduling seam. Phase I currently returns no outcome while the outer tick continues after its deadline, which leaves the scheduler without the information needed to suppress a same-tick relaunch (`internal/cli/supervise_ensure_alive.go:516-582,558-581`). |
| F6 | Supervisor-event/audit owner | Extend the existing tracked-emission abstraction with a process-exit-safe durable-row-or-durable-handoff receipt. The recovery coordinator preserves ordering but does not create a competing audit store. A blocked tracked write currently survives only in its goroutine, while the one-shot command-line interface (CLI) can return and exit (`internal/daemonrecovery/recovery.go:819-825`; `internal/cli/daemon_recover.go:121-134`). |
| F7 | Ensure-alive recovery coordinator | At the two pre-action sites only, use the event owner’s existing lossy `TryEmit` seam. Keep the blocking helper and all other emission classes unchanged. The event owner already exposes blocking, bounded, tracked, and lossy variants (`internal/api/supervisor_events.go:337-405`). |

### F5 — retained GUI-lease outcome propagation

Phase I returns one of three semantic dispositions:

- `not exposed`: the operation conclusively did not acquire the lease;
- `released`: acquisition occurred, and release completion was confirmed;
- `may retain`: the worker was still incomplete at the Phase-I deadline, cancellation or an early exit occurred after possible acquisition, or release returned an error or otherwise lacked confirmation.

`may retain` is the default once acquisition may have occurred. Probe/cancellation, invalid-state, free-record mismatch, marker compare-and-swap (CAS), and error-discarding release paths all feed that disposition unless successful release is positively established (`internal/cli/supervise_ensure_alive.go:631-660,701-710,730-750`). A late worker completion cannot upgrade the already-consumed disposition for the current tick.

The outer liveness coordinator converts the disposition into one tick-local capability: whether a GUI-owner task may be invoked. It checks that capability at one scheduling seam before either ConfirmedDead consumer reaches GUI relaunch:

- running plus ConfirmedDead (`internal/cli/supervise_ensure_alive.go:1373-1383,948-970`);
- down plus ConfirmedDead (`internal/cli/supervise_ensure_alive.go:1478-1499`).

A `may retain` result suppresses only those GUI-owner invocations for that tick. It does not rewrite the topology result to Unknown and does not alter the 90-second Unknown window. Running plus Unknown already observes the flock as held, while down plus Alive/Unknown uses standalone supervisor recovery and does not require the GUI flock (`internal/cli/supervise_ensure_alive.go:1118-1130,1384-1401,1445-1475`).

This preserves F1 by leaving cancellation-before-emission ordering and joined signal attribution untouched. It preserves F2 because no reacquisition probe is added after an uncertain release. F3 and F4 remain topology contracts rather than being overloaded with lease state.

### F6 — process-exit-safe recovery audit

The event owner receives each committed destructive recovery audit under one opaque action identity and returns a tracked receipt. That receipt represents one logical audit row across the original writer and any durable handoff.

The recovery flow is:

1. Clean-reap and termination-unconfirmed actions enter the same tracked path they use now (`internal/daemonrecovery/recovery.go:493-506`).
2. Fast completion records a durable row before respawn, preserving the current fast path.
3. Audit waiting never decides whether respawn occurs. If the row is not already durable, respawn proceeds first.
4. After respawn, but before a one-shot caller may return, the recovery coordinator asks the same tracked receipt to establish either:
   - confirmation that the row is durable; or
   - an event-owner-managed durable handoff containing the same action identity and complete audit payload.
5. Once either receipt is durable, the recovery function may return without waiting for the original writer.
6. Handoff replay and the original tracked writer use the action identity as an idempotency key. A handoff is retired only after the corresponding row is durably present, producing one durable audit row even if replay races a late original completion.

The owner-contract change is necessary because the current blocked-success path retains the only audit in an in-process goroutine, and the release-failure path cannot safely reacquire the writer under F2 (`internal/daemonrecovery/recovery.go:822-836`). Recovery-local retry logic cannot turn that goroutine into process-exit durability.

No-budget, no-worker, and definite-failure participants use the same post-respawn durable-row-or-handoff finalizer instead of relying solely on a blocking fallback (`internal/daemonrecovery/recovery.go:511-515,545-551,817-844`). Pending success remains complete as soon as its durable row is observed (`internal/daemonrecovery/recovery.go:819-821`). Pending release failure seals through the receipt’s already-owned handoff seam and never reacquires the uncertain primary writer.

The handoff belongs to the supervisor-event owner and uses no GUI, tray, supervisor-control, scheduler, or application-state synchronization path. Its replay is an audit-storage lifecycle concern.

### F7 — non-blocking pre-action observability

The generic `emitLivenessEvent` helper remains blocking because changing it globally would alter phase-I and post-decision behavior beyond F7 (`internal/cli/supervise_ensure_alive.go:477-504`).

Only these two ordering-sensitive call sites use an explicit pre-action best-effort emission:

- Unknown escalation immediately before headless recovery (`internal/cli/supervise_ensure_alive.go:1171-1179`);
- headless detection immediately before suppressor evaluation and possible relaunch (`internal/cli/supervise_ensure_alive.go:923-950`).

Each site attempts the same event through `TryEmit`, then proceeds to its suppressor or recovery callback whether the event was accepted or dropped. There is no retry, wait, or spawned diagnostic goroutine before the callback. Phase-I emissions remain inside their existing budget, and suppress/no-action plus success/failure emissions remain after the decision or callback (`internal/api/supervisor_events.go:337-405` and the supplied F7 site inventory).

## Alternatives and tradeoffs

| Class | Alternative | Disposition and tradeoff |
|---|---|---|
| F5 | Re-probe or reacquire the lease before relaunch | Rejected. A failed or uncertain release cannot safely be followed by same-process reacquisition under F2 (`internal/daemonrecovery/recovery.go:826-836`). It also converts ownership knowledge into another race. |
|  | Force the final topology to Unknown whenever Phase I times out | Rejected. Lease eligibility and daemon liveness are distinct. This would disturb the uninterrupted Unknown window and the supported/unsupported liveness classification preserved by F3–F4 (`internal/cli/supervise_ensure_alive.go:1118-1130,1384-1401,1445-1475`). |
|  | Wait for every Phase-I worker before continuing | Rejected. The outer tick intentionally continues at its deadline, and current tests cover a worker that still owns the lease at that point (`internal/cli/supervise_ensure_alive.go:558-581`). |
| F6 | Wait for the tracked writer before respawn | Rejected. Audit input/output (I/O) would preempt respawn; the current design explicitly allows pending paths beyond respawn (`internal/daemonrecovery/recovery.go:817-825`). |
|  | Keep the existing goroutine and return | Rejected. It is same-process eventual completion, not durability across the one-shot CLI exit (`internal/daemonrecovery/recovery.go:822-825`; `internal/cli/daemon_recover.go:121-134`). |
|  | Add a recovery-local spool | Rejected as the primary design. It would split serialization, replay, and exactly-once ownership between recovery and the existing event owner. The tracked abstraction already owns the pending event lifecycle (`internal/api/supervisor_events.go:337-405`). |
|  | Write a durable intent synchronously before respawn | Not selected. It simplifies crash recovery but puts durable audit latency directly in the respawn-critical path, contrary to the required ordering. |
| F7 | Replace all blocking emissions with `TryEmit` | Rejected. Phase-I emissions and post-action outcome emissions are outside F7, and a global change would silently weaken their observability contracts. |
|  | Use `EmitWithTimeout` at the two sites | Viable fallback if bounded delivery opportunity is required. It preserves more events under brief contention but deliberately adds the configured timeout to recovery latency (`internal/api/supervisor_events.go:337-405`). |
|  | Launch blocking `Emit` in an untracked goroutine | Rejected. It converts lock contention into retained goroutines and does not give deterministic process-exit or resource-lifecycle behavior. |

## Participant dispositions

| Class | Participant | Disposition |
|---|---|---|
| F5 | `runEnsureAliveGUIRecoveryPhase1` | Returns the authoritative lease disposition instead of returning no outcome (`internal/cli/supervise_ensure_alive.go:516-582`). |
|  | Probe/cancellation | `may retain` whenever possible acquisition preceded cancellation; F1 emission ordering remains unchanged (`internal/cli/supervise_ensure_alive.go:631-660`). |
|  | Invalid state | Captures release result; absent confirmed release becomes `may retain` (`internal/cli/supervise_ensure_alive.go:631-660`). |
|  | Free-record mismatch | Same conservative disposition; no reacquisition attempt (`internal/cli/supervise_ensure_alive.go:631-660`). |
|  | Marker-CAS path | A post-CAS `ReleaseErr` produces `may retain` and blocks same-tick GUI relaunch (`internal/cli/supervise_ensure_alive.go:701-710,730-750`). |
|  | Error-discarding releases | Converted into observed release outcomes feeding the single disposition (`internal/cli/supervise_ensure_alive.go:631-660`). |
|  | Existing `ReleaseErr` diagnostics | Unknown diagnostic emission remains; the new disposition additionally carries scheduling significance (`internal/cli/supervise_ensure_alive.go:701-710,730-750`). |
|  | Phase-I deadline | Missing completion is consumed as `may retain`; no late same-tick upgrade (`internal/cli/supervise_ensure_alive.go:558-581`). |
|  | Final topology evaluation | Continues unchanged; lease capability is applied only after topology is known (`internal/cli/supervise_ensure_alive.go:1346-1349`). |
|  | Running plus ConfirmedDead | GUI relaunch is gated by the tick-local capability (`internal/cli/supervise_ensure_alive.go:1373-1383,948-970`). |
|  | Down plus ConfirmedDead | Receives the same gate (`internal/cli/supervise_ensure_alive.go:1478-1499`). |
|  | Running plus Unknown | Unchanged because it already observes the flock as held (`internal/cli/supervise_ensure_alive.go:1118-1130,1384-1401`). |
|  | Down plus Alive/Unknown | Unchanged standalone supervisor recovery; it does not need the GUI flock (`internal/cli/supervise_ensure_alive.go:1445-1475`). |
| F6 | Clean-reap event | Uses one action identity and tracked receipt (`internal/daemonrecovery/recovery.go:493-506`). |
|  | Termination-unconfirmed event | Uses the same identity, receipt, and durability rules (`internal/daemonrecovery/recovery.go:493-506`). |
|  | Fast tracked write | Existing durable-before-respawn behavior remains. |
|  | No-budget/no-worker | Respawn first, then establish a durable row or handoff (`internal/daemonrecovery/recovery.go:511-515,817-844`). |
|  | Definite tracked failure | Same post-respawn finalizer; failure does not authorize dropping the audit (`internal/daemonrecovery/recovery.go:545-551,817-844`). |
|  | Pending success | Durable row receipt completes the obligation (`internal/daemonrecovery/recovery.go:819-821`). |
|  | Pending blocked writer | Durable handoff replaces reliance on the goroutine surviving process exit (`internal/daemonrecovery/recovery.go:822-825`). |
|  | Pending release failure | Receipt-owned handoff; no same-process reacquisition (`internal/daemonrecovery/recovery.go:826-836`). |
|  | One-shot CLI | Returns only after durable-row or durable-handoff acknowledgement (`internal/cli/daemon_recover.go:121-134`). |
| F7 | Blocking `emitLivenessEvent` | Retained for all existing non-F7 callers (`internal/cli/supervise_ensure_alive.go:477-504`). |
|  | Unknown-escalation pre-action site | Uses `TryEmit`; recovery callback proceeds after the attempt (`internal/cli/supervise_ensure_alive.go:1171-1179`). |
|  | Headless-detection pre-action site | Uses `TryEmit`; suppressor evaluation and relaunch decision proceed after the attempt (`internal/cli/supervise_ensure_alive.go:923-950`). |
|  | Phase-I emissions | Excluded and unchanged. |
|  | Suppress/no-action emissions | Excluded because they occur after the decision. |
|  | Success/failure emissions | Excluded because they occur after the callback. |
|  | Blocking `Emit` | Remains the default observability contract outside the two pre-action sites (`internal/api/supervisor_events.go:337-405`). |
|  | `EmitWithTimeout` | Retained as the bounded-delivery alternative, not selected for the minimal F7 path (`internal/api/supervisor_events.go:337-405`). |
|  | `EmitWithTimeoutTracked` | Remains the F6 tracked foundation, extended only for process-exit-safe ownership (`internal/api/supervisor_events.go:337-405`). |
|  | `TryEmit` | Selected existing seam for the two F7 sites (`internal/api/supervisor_events.go:337-405`). |

## Failure modes and discriminators

| Failure mode | Discriminator | Required outcome |
|---|---|---|
| Phase I completes with confirmed release | Returned lease disposition is `released` | Existing ConfirmedDead GUI relaunch behavior remains eligible. |
| Phase I misses its deadline | No completed disposition is available when the outer tick resumes (`internal/cli/supervise_ensure_alive.go:558-581`) | Consume `may retain`; no same-tick GUI-owner task. |
| Marker CAS succeeds, then release fails | Release result is an error on the marker path (`internal/cli/supervise_ensure_alive.go:701-710,730-750`) | Emit existing Unknown diagnostic and suppress both ConfirmedDead GUI consumers for that tick. |
| Audit row becomes durable before respawn | Tracked receipt reports durable success (`internal/daemonrecovery/recovery.go:819-821`) | No handoff is required; respawn and return normally. |
| Tracked writer remains blocked after respawn | Receipt remains pending (`internal/daemonrecovery/recovery.go:822-825`) | Persist the action-keyed handoff, then allow CLI return without waiting for the writer. |
| Writer ownership release is uncertain | Receipt reports release failure (`internal/daemonrecovery/recovery.go:826-836`) | Seal through the already-owned handoff seam; never reacquire the primary writer. |
| Original audit write completes after handoff | Row and handoff carry the same action identity | Event owner keeps or creates one row and retires the handoff only after row durability. |
| Event-log lock is wedged before recovery | `TryEmit` declines the event at either F7 pre-action site | Proceed immediately to the relevant suppressor or recovery callback. |
| Durable handoff medium is unavailable | Handoff cannot return a durable acknowledgement | Report an audit-durability failure after respawn; do not misreport the recovery as fully audited. This is a residual availability boundary described below. |

## Test strategy

### F5

- Inject `ReleaseErr != nil` after marker CAS and run a single tick through running plus ConfirmedDead. Assert the GUI relaunch seam is never invoked (`internal/cli/supervise_ensure_alive.go:701-710,730-750,1373-1383,948-970`).
- Repeat for down plus ConfirmedDead (`internal/cli/supervise_ensure_alive.go:1478-1499`).
- Hold the classifier worker beyond the Phase-I deadline and assert the missing outcome becomes `may retain`, closing the existing relaunch-while-lease-held gap (`internal/cli/supervise_ensure_alive.go:558-581`).
- Prove confirmed release still permits both existing ConfirmedDead consumers.
- Preserve dedicated guards for running plus Unknown and down plus Alive/Unknown (`internal/cli/supervise_ensure_alive.go:1118-1130,1384-1401,1445-1475`).
- Re-run the protected F1–F4 focused tests without changing their assertions.

### F6

- Use a subprocess test whose tracked writer is blocked beyond respawn. Let the one-shot CLI return, terminate that process, then reopen the event owner from a new process and prove either the row or the durable handoff survives (`internal/daemonrecovery/recovery.go:822-825`; `internal/cli/daemon_recover.go:121-134`).
- Replay the handoff and prove exactly one durable row exists.
- Race late completion of the original writer against handoff replay and prove the shared action identity prevents a second row.
- Exercise clean-reap and termination-unconfirmed separately (`internal/daemonrecovery/recovery.go:493-506`).
- Exercise fast success, no-budget/no-worker, definite failure, pending success, pending blocked, and pending release failure (`internal/daemonrecovery/recovery.go:511-515,545-551,817-844`).
- On release failure, instrument primary-writer acquisition and assert no reacquisition occurs.
- Assert respawn is observed before the test permits handoff finalization, proving audit I/O cannot preempt respawn.

### F7

- Wedge the event-log lock before the Unknown-escalation site and assert the headless recovery callback runs within a deterministic test bound (`internal/cli/supervise_ensure_alive.go:1171-1179`).
- Repeat at the headless-detection site and assert suppressor evaluation or the relevant relaunch callback runs within the same bound (`internal/cli/supervise_ensure_alive.go:923-950`).
- Assert no background emission worker remains after either dropped attempt.
- Verify phase-I and post-decision emission tests remain unchanged.

## Risks / assumptions

- **ASSUMPTION (UNVERIFIED):** successful `Release` conclusively proves that the Phase-I worker no longer owns the GUI lease. Resolving probe: inspect the lease implementation’s release contract and exercise successful release followed by an independent acquisition.
- **ASSUMPTION (UNVERIFIED):** both ConfirmedDead consumers can be controlled through one shared GUI-owner scheduling seam without duplicating lease logic. Resolving probe: inspect the complete call paths from `:1373-1383` and `:1478-1499` to `:948-970`; if they do not converge, pass the same tick-local capability to both invocation sites while retaining one disposition owner.
- **ASSUMPTION (UNVERIFIED):** the supervisor-event owner has or can own a durable storage location independent of the primary event-writer lock, with atomic installation and restart discovery. Resolving probe: inspect its construction, persistence configuration, locking boundaries, and startup/open lifecycle. If false, F6 requires an explicitly approved change-surface expansion.
- **ASSUMPTION (UNVERIFIED):** the audit store can enforce idempotency by an opaque action identity without changing externally consumed row shape. Resolving probe: inspect row serialization, uniqueness facilities, and readers against one real stored sample.
- **ASSUMPTION (UNVERIFIED):** durable handoff acknowledgement includes the required flush-to-stable-storage semantics on every supported operating system. Resolving probe: define the durability contract from the actual storage implementation and run process-kill/crash-point tests on each supported platform.
- **ASSUMPTION (UNVERIFIED):** `TryEmit` returns without waiting when the event-log lock is held. The supplied API inventory identifies it as lossy but does not explicitly establish its lock behavior (`internal/api/supervisor_events.go:337-405`). Resolving probe: inspect the implementation and run the two required lock-wedge tests.
- Total failure of the audit volume is outside what an in-process recovery algorithm can convert into a durable record. The design discriminates that condition and refuses to call it audited; the lead must decide whether repository policy requires fail-closed destructive action admission or accepts an explicit post-action durability error.

Gate: RETURN(lead)
