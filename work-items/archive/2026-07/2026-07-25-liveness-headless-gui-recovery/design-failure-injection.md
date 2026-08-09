## 1. Provenance

- Execution role: external-worker
- Assigned / replaced internal role: architect
- Requested provider: codex
- Resolved provider: Codex CLI
- Requested consultant mode: not-applicable
- Actual execution path: external CLI (Codex CLI)
- Model / profile used: gpt-5.6-sol / xhigh
- Launch flags: `-m gpt-5.6-sol -c model_reasoning_effort="xhigh" -s read-only -C <neutral> --skip-git-repo-check --ephemeral --ignore-rules`
- Run record: started `2026-07-27T05:26:15.2903889+03:00`; finished
  `2026-07-27T05:31:51.6495590+03:00`; exit `0`; prompt/stdout/stderr/exit
  evidence under
  `.scratch/pr589-design-panel-20260727-0522/lane-failure-injection.*`
- Deviation reason: none

## 2. Panel identity

- Panel disposition: INPUT_ONLY
- Lane: failure-injection
- Framing: design backward from deterministic failure injection and process/resource lifetime boundaries, prioritizing provable recovery and audit durability under cancellation, timeout, blocked I/O, and process exit.
- Pinned-input identity: PR589-R4-F5-F7-HEAD-db879c5f
- Scope: one sealed candidate design for F5–F7, using only the supplied factual package.

## 3. Candidate design

The candidate uses three narrow ownership corrections:

| Class | Owning boundary | Proposed seam |
|---|---|---|
| F5 | Phase-I GUI lease lifecycle in `supervise_ensure_alive.go` | Return a tick-local lease disposition from Phase I and consume it only at GUI-owner relaunch gates. |
| F6 | Recovery audit lifecycle shared by recovery coordination and the event owner | Establish a durable, uniquely keyed audit handoff before admitting an irreversible recovery action; use the same key for tracked delivery and later finalization. |
| F7 | The two pre-action diagnostic call sites in `supervise_ensure_alive.go` | Replace unbounded pre-action emission with a shared, tick-bounded diagnostic budget using the existing bounded emission abstraction. |

### F5 — tick-local retained-lease disposition

`runEnsureAliveGUIRecoveryPhase1` currently returns no outcome, while outer liveness continues after its deadline and later evaluates topology (`internal/cli/supervise_ensure_alive.go:516-582,1346-1349`). Phase I therefore becomes the sole owner of a tick-local lease disposition with three semantic states:

- No lease was acquired.
- Lease release was positively confirmed.
- Lease may still be retained.

The state becomes “may still be retained” immediately after successful acquisition. It becomes “release confirmed” only after a positively successful release. A discarded release result, `ReleaseErr`, cancellation while a lease-owning worker remains active, or the Phase-I deadline expiring before ownership is settled leaves it in “may still be retained.” This covers the probe/cancellation, invalid-state, free-record-mismatch, marker-CAS, and release-error paths identified at `internal/cli/supervise_ensure_alive.go:631-660,701-710,730-750`.

The outer tick consumes this disposition as an additional prerequisite only for operations that require becoming the GUI lease owner:

- Running plus `ConfirmedDead` may relaunch only when Phase I proves the lease absent or successfully released (`internal/cli/supervise_ensure_alive.go:1373-1383,948-970`).
- Down plus `ConfirmedDead` uses the same prerequisite (`internal/cli/supervise_ensure_alive.go:1478-1499`).

The disposition does not replace or reinterpret topology. It is a resource-safety gate layered at the owning GUI relaunch seam after topology classification.

It is scoped to one liveness tick. A later tick recomputes lease disposition rather than inheriting a permanent suppression. This avoids converting one uncertain release into an unbounded recovery lockout.

Running plus `Unknown` retains its existing flock-held behavior (`internal/cli/supervise_ensure_alive.go:1118-1130,1384-1401`). Down plus Alive/Unknown continues through standalone supervisor recovery because that path does not require the GUI flock (`internal/cli/supervise_ensure_alive.go:1445-1475`).

Preservation commitments:

- Phase-I cancellation and signal attribution remain unchanged.
- No same-process reacquisition is attempted after an uncertain release.
- The uninterrupted Unknown/unheld timing window and Alive/Unknown classifications are not changed.
- Non-GUI standalone recovery remains available when GUI relaunch is barred.

### F6 — durable audit reservation and single-key handoff

The current pending-blocked branch leaves only a goroutine retaining the audit, while the one-shot command can return and its process can exit (`internal/daemonrecovery/recovery.go:822-825`; `internal/cli/daemon_recover.go:121-134`). A goroutine is therefore not an adequate ownership boundary for process-exit durability.

The recovery attempt receives one stable audit identity. Before an irreversible destructive action is admitted, the event owner durably reserves a handoff containing:

- the stable audit identity;
- the recovery-attempt identity;
- sufficient event payload to finalize the appropriate committed outcome;
- an explicit unresolved/committed-delivery state;
- ownership metadata preventing two delivery agents from finalizing independently.

The reservation is not the final audit row unless the owning audit contract already supports such an outbox representation. It is durable evidence that exactly one keyed record remains owed.

Control flow:

1. Establish the durable reservation under a bounded admission budget.
2. If no reservation can be established, do not admit a new irreversible destructive action. Recovery returns an explicit audit-admission failure rather than silently performing an unauditable destructive commit.
3. Once reserved, perform the destructive recovery action.
4. After the action commits, start respawn without waiting for event-log completion.
5. Attempt the existing tracked write using the reservation’s audit identity.
6. On confirmed durable success, atomically mark the handoff discharged or remove it.
7. On timeout, process return, cancellation, or release failure, retain the handoff. A later owner drains it using the same identity.
8. Sink insertion or finalization is idempotent by that identity; concurrent or replayed delivery can produce one logical audit row only.

This moves durability ahead of the irreversible boundary, while keeping post-commit respawn independent of audit I/O. Fast tracked writes retain their current durable-before-respawn behavior (`internal/daemonrecovery/recovery.go:493-506`). Pending success remains a normal discharge (`internal/daemonrecovery/recovery.go:819-821`). No-budget, no-worker, definite-failure, and blocked cases converge on the same durable handoff rather than relying on a post-respawn blocking fallback (`internal/daemonrecovery/recovery.go:511-515,545-551,817-844`).

Pending release failure does not trigger same-process reacquisition. The handoff remains for a later process or independently owned drainer, preserving F2 (`internal/daemonrecovery/recovery.go:826-836`).

The owning event contract must change if it cannot currently provide durable reservation, stable identity, and idempotent finalization. Recovery coordination alone cannot make a goroutine survive process exit, as demonstrated by the combination of `internal/daemonrecovery/recovery.go:822-825` and `internal/cli/daemon_recover.go:121-134`.

### F7 — one cumulative pre-action diagnostic budget

`emitLivenessEvent` currently calls unbounded blocking `Emit` (`internal/cli/supervise_ensure_alive.go:477-504`). Only the two open pre-action sites switch policy:

- Unknown escalation before headless recovery (`internal/cli/supervise_ensure_alive.go:1171-1179`).
- Headless detection before suppressor evaluation and relaunch (`internal/cli/supervise_ensure_alive.go:923-950`).

At the start of the relevant tick, establish one cumulative pre-action diagnostic deadline. Each of these sites calls the existing bounded `EmitWithTimeout` with only the remaining diagnostic budget. Event success, timeout, or failure is recorded as an observability result but never becomes a prerequisite for suppressor evaluation or the recovery callback. Exhausted budget means the diagnostic attempt is skipped and recovery proceeds.

A cumulative deadline prevents several bounded attempts in one tick from stacking into an effectively unbounded delay. The event owner already exposes bounded and tracked emission operations, so F7 needs no new event-owner primitive (`internal/api/supervisor_events.go:337-405`).

Phase-I emissions remain inside their existing budget. Suppression, no-action, success, and failure emissions remain after the decision or callback and are outside this correction, as identified by the pinned package.

## 4. Alternatives and tradeoffs

| Class | Alternative | Benefit | Rejection or tradeoff |
|---|---|---|---|
| F5 | Re-probe or reacquire the flock before GUI relaunch | Appears to provide a fresh answer. | An uncertain release cannot be made safe through same-process reacquisition; the existing F2 branch explicitly excludes that response (`internal/daemonrecovery/recovery.go:826-836`). It also introduces a race between the probe and relaunch. |
| F5 | Suppress every recovery operation when Phase I is uncertain | Simple fail-closed rule. | Overbroad: down Alive/Unknown uses standalone recovery and does not need the GUI flock (`internal/cli/supervise_ensure_alive.go:1445-1475`). |
| F5 | Wait until the classifier or release worker finishes | Produces a final ownership answer. | Can make the liveness tick wait indefinitely; outer processing currently proceeds after the Phase-I deadline (`internal/cli/supervise_ensure_alive.go:558-581`). |
| F6 | Keep the detached goroutine alive after CLI return | Small local change. | Process exit destroys the only remaining owner (`internal/daemonrecovery/recovery.go:822-825`; `internal/cli/daemon_recover.go:121-134`). |
| F6 | Block the CLI until the tracked writer completes | Avoids a new persistence contract. | A wedged writer makes one-shot recovery unbounded and can delay completion after respawn (`internal/daemonrecovery/recovery.go:817-844`). |
| F6 | Write the audit synchronously after the destructive action and before respawn | Straightforward ordering. | Audit I/O can preempt respawn, violating the required recovery ordering. |
| F6 | Spawn a helper process to retain the write | Survives parent return in some environments. | It still lacks durable ownership across helper failure or machine exit, complicates cancellation and process cleanup, and does not by itself enforce exactly-once delivery. |
| F6 | Durable reservation before destructive admission | Provides a process-independent recovery point and permits idempotent replay. | Adds a durable outbox/reservation contract and can deny a destructive action when audit durability is unavailable. That availability tradeoff must be explicit. |
| F7 | Use lossy `TryEmit` at both sites | Minimal recovery latency and no timeout accounting. | The supplied facts establish that `TryEmit` exists and is lossy, but do not explicitly establish its behavior while the event-log lock is wedged (`internal/api/supervisor_events.go:337-405`). |
| F7 | Reorder both events after their callbacks | Removes pre-action blocking entirely. | Changes event ordering semantics and weakens pre-decision observability. |
| F7 | Use a fresh timeout independently at each site | Uses the existing bounded API. | Multiple attempts can accumulate; one shared deadline gives a provable per-tick upper bound. |

## 5. Participant dispositions

| Class | Participant | Disposition |
|---|---|---|
| F5 | `runEnsureAliveGUIRecoveryPhase1` | Becomes the owner and producer of the tick-local lease disposition; its existing deadline remains (`internal/cli/supervise_ensure_alive.go:516-582`). |
| F5 | Probe/cancellation path | If acquisition occurred and release is not positively confirmed, returns “may retain”; cancellation semantics otherwise remain unchanged (`internal/cli/supervise_ensure_alive.go:631-660`). |
| F5 | Invalid-state path | Same lease accounting; no topology reinterpretation (`internal/cli/supervise_ensure_alive.go:631-660`). |
| F5 | Free-record mismatch | Same lease accounting; a discarded release result can no longer be treated as proof of release (`internal/cli/supervise_ensure_alive.go:631-660`). |
| F5 | Marker-CAS path | A successful CAS does not prove lease release; release outcome determines the disposition (`internal/cli/supervise_ensure_alive.go:631-660`). |
| F5 | `ReleaseErr` handling | Continues to emit Unknown where currently required and additionally leaves the lease disposition uncertain (`internal/cli/supervise_ensure_alive.go:701-710,730-750`). |
| F5 | Phase-I deadline/outer liveness | Outer liveness still proceeds, carrying the returned disposition to final topology evaluation (`internal/cli/supervise_ensure_alive.go:558-581,1346-1349`). |
| F5 | Running plus `ConfirmedDead` | GUI relaunch receives the lease-safety prerequisite (`internal/cli/supervise_ensure_alive.go:1373-1383,948-970`). |
| F5 | Down plus `ConfirmedDead` | GUI relaunch receives the same prerequisite (`internal/cli/supervise_ensure_alive.go:1478-1499`). |
| F5 | Running plus `Unknown` | Unchanged; existing held-flock observation remains authoritative for this path (`internal/cli/supervise_ensure_alive.go:1118-1130,1384-1401`). |
| F5 | Down Alive/Unknown | Unchanged standalone recovery; explicitly excluded from the GUI lease gate (`internal/cli/supervise_ensure_alive.go:1445-1475`). |
| F5 | Existing diagnostic-downgrade test | Retained and extended to assert the returned lease disposition. |
| F5 | Existing timeout-worker/relaunch test | Reversed into a regression guard: an outstanding lease-owning worker bars same-tick GUI relaunch. |
| F6 | Committed clean-reap event | Uses one pre-established audit identity and reservation (`internal/daemonrecovery/recovery.go:493-506`). |
| F6 | Termination-unconfirmed event | Uses the same lifecycle and exactly-once rule (`internal/daemonrecovery/recovery.go:493-506`). |
| F6 | Fast tracked writer | Unchanged fast path; confirmed durable success discharges the handoff before normal continuation. |
| F6 | No-budget/no-worker path | Retains the durable handoff; does not depend on an unowned blocking fallback (`internal/daemonrecovery/recovery.go:511-515,817-844`). |
| F6 | Definite writer failure | Retains the handoff for replay under the same audit identity (`internal/daemonrecovery/recovery.go:545-551,817-844`). |
| F6 | Pending success | Confirms the single row and discharges the handoff (`internal/daemonrecovery/recovery.go:819-821`). |
| F6 | Pending still blocked | Process-local goroutine ceases to be the sole owner; the durable handoff remains (`internal/daemonrecovery/recovery.go:822-825`). |
| F6 | Pending release failure | No same-process reacquisition; ownership transfers only through the durable handoff (`internal/daemonrecovery/recovery.go:826-836`). |
| F6 | One-shot CLI return | Allowed after respawn and verification that either the row is durable or the durable handoff remains (`internal/cli/daemon_recover.go:121-134`). |
| F6 | Existing same-process tests | Retained as fast-path and eventual-drain coverage but no longer accepted as process-exit evidence. |
| F7 | `emitLivenessEvent` | Its general blocking behavior remains for callers not in the two open pre-action positions (`internal/cli/supervise_ensure_alive.go:477-504`). |
| F7 | Unknown escalation emit | Uses the remaining tick diagnostic budget; callback proceeds after success, timeout, or error (`internal/cli/supervise_ensure_alive.go:1171-1179`). |
| F7 | Headless-detection emit | Uses the same cumulative budget before suppressor/relaunch processing (`internal/cli/supervise_ensure_alive.go:923-950`). |
| F7 | Phase-I emits | Excluded; already governed by the existing Phase-I budget. |
| F7 | Suppress/no-action and post-callback emits | Excluded because they do not gate the open recovery decision. |
| F7 | Event owner | Existing `EmitWithTimeout` is sufficient; tracked and lossy alternatives remain unchanged (`internal/api/supervisor_events.go:337-405`). |

## 6. Failure modes and discriminators

| Failure mode | Required result | Discriminator |
|---|---|---|
| Phase I never acquired the lease | GUI-owner recovery remains eligible subject to existing topology. | Returned disposition is “not acquired”; no release proof is required. |
| Lease acquired and release positively succeeds | GUI-owner recovery may proceed in the same tick. | Release success is observed before Phase-I return. |
| Marker CAS succeeds but release fails | Same-tick running/dead and down/dead GUI relaunches are barred. | Inject `ReleaseErr != nil` after marker CAS (`internal/cli/supervise_ensure_alive.go:701-710,730-750`). |
| Classifier worker still owns the lease at Phase-I deadline | Same-tick GUI relaunch is barred without waiting for worker completion. | Hold the worker beyond `internal/cli/supervise_ensure_alive.go:558-581` and observe both GUI-owner consumers. |
| GUI relaunch is barred but down Alive/Unknown occurs | Standalone supervisor recovery still executes. | Exercise `internal/cli/supervise_ensure_alive.go:1445-1475`. |
| Tracked audit write succeeds quickly | Exactly one durable row exists; no pending handoff remains. | Fast-path completion at `internal/daemonrecovery/recovery.go:493-506,819-821`. |
| Tracked writer remains blocked beyond respawn and CLI return | Exactly one durable handoff survives process exit. | Subprocess exits after `internal/cli/daemon_recover.go:121-134`; parent inspects the injected durable store. |
| Writer succeeds concurrently with handoff drain | Exactly one logical row exists. | Both deliveries use the same stable identity; duplicate insertion must be rejected or coalesced. |
| Writer ownership release fails | No same-process second writer starts. | Exercise `internal/daemonrecovery/recovery.go:826-836` and verify handoff retention. |
| Durable reservation cannot be established | No new destructive action is admitted; failure is explicit and bounded. | Inject reservation timeout/failure before the irreversible action boundary. |
| Pre-action event lock is wedged | Recovery callback executes within the cumulative diagnostic deadline. | Wedge the lock independently at `internal/cli/supervise_ensure_alive.go:1171-1179` and `:923-950`. |
| First pre-action event consumes the diagnostic budget | Any subsequent pre-action diagnostic attempt cannot extend the total bound. | Inject sequential calls against one tick deadline. |
| Bounded event emission returns an error | Recovery decision and callback remain unchanged. | Inject error return from the bounded event owner and verify callback identity/count. |

## 7. Test strategy

### F5

Use deterministic lease and worker fakes:

- Inject `ReleaseErr != nil` after marker CAS.
- Assert Phase I returns “may retain.”
- In the same tick, exercise running plus `ConfirmedDead` and down plus `ConfirmedDead`.
- Assert neither path reaches the GUI relaunch callback (`internal/cli/supervise_ensure_alive.go:1373-1383,1478-1499`).
- Exercise running plus `Unknown` to preserve existing held-flock behavior (`internal/cli/supervise_ensure_alive.go:1118-1130,1384-1401`).
- Exercise down Alive/Unknown and assert standalone recovery still runs (`internal/cli/supervise_ensure_alive.go:1445-1475`).
- Hold the classifier worker past the Phase-I deadline and prove the same tick cannot relaunch while ownership remains unresolved (`internal/cli/supervise_ensure_alive.go:558-581`).
- Retain positive controls for no acquisition and confirmed successful release.

### F6

Use an injected durable audit store and a subprocess boundary:

- Assign a deterministic audit identity.
- Block the tracked writer beyond respawn.
- Verify respawn occurs while the writer remains blocked.
- Let the one-shot CLI return through `internal/cli/daemon_recover.go:121-134`.
- Terminate the subprocess normally without allowing the writer goroutine to complete.
- From the parent process, reopen the injected store and prove exactly one durable row or exactly one unresolved durable handoff remains.
- Start the drainer twice, including concurrent delivery with the original writer where controllable, and prove final cardinality is one.
- Repeat for committed clean-reap and termination-unconfirmed events (`internal/daemonrecovery/recovery.go:493-506`).
- Inject no-budget, no-worker, definite failure, pending success, pending blocked, and release-failure outcomes (`internal/daemonrecovery/recovery.go:511-515,545-551,817-844`).
- Inject durable-reservation failure and prove bounded failure occurs before any destructive commit.

The subprocess test, rather than a goroutine-only test, is the acceptance evidence for process-lifetime durability.

### F7

Use a deterministic event owner whose lock can be held indefinitely and an injected clock/deadline:

- Wedge the event-log lock at Unknown escalation (`internal/cli/supervise_ensure_alive.go:1171-1179`).
- Assert headless recovery callback entry before the configured diagnostic deadline plus deterministic scheduler allowance.
- Wedge the lock at headless detection (`internal/cli/supervise_ensure_alive.go:923-950`).
- Assert suppressor evaluation and the relevant recovery/relaunch callback proceed within the same bound.
- Inject event success, timeout, and explicit failure; callback count and arguments must remain identical.
- Exercise multiple pre-action attempts in one tick and prove elapsed diagnostic delay is cumulative-budget bounded.
- Retain existing Phase-I and post-decision emission tests because their policy is deliberately unchanged.

## 8. Risks / assumptions

- **ASSUMPTION (UNVERIFIED):** the current event/audit store has, or can accept within `internal/api/supervisor_events.go`, a stable record identity and idempotent insertion contract. Resolving probe: inspect the event serialization and persistence owner at `internal/api/supervisor_events.go:337-405`, then run duplicate-key persistence tests against the actual store.
- **ASSUMPTION (UNVERIFIED):** recovery control flow exposes a point before each irreversible clean-reap or termination-unconfirmed action where a durable reservation can be required. Resolving probe: trace every action-commit return path leading to `internal/daemonrecovery/recovery.go:493-506`.
- **ASSUMPTION (UNVERIFIED):** an existing durable outbox or handoff location can be reused without introducing a second persistence owner. Resolving probe: inventory the tracked event writer’s storage, replay, and cleanup mechanisms before adding a new path.
- If no reusable durable handoff exists, the event owner needs a narrow contract extension. Implementing a recovery-local sidecar without owner-level replay and deduplication would create split ownership and is excluded by this candidate.
- A pre-action durable reservation can reduce recovery availability when audit storage is unavailable. This is the explicit price of guaranteeing that no destructive commit becomes unauditable.
- Filesystem durability under total device failure cannot be created by timeout logic alone. The durability claim requires the reservation operation to acknowledge only after the target store’s defined durable-commit point.
- **ASSUMPTION (UNVERIFIED):** the exact cumulative F7 diagnostic budget can be derived from an existing liveness/recovery deadline without weakening F3’s uninterrupted 90-second window. Resolving probe: inventory timing constants and inject the chosen budget into focused clock-controlled tests.
- The supplied package identifies `EmitWithTimeout` as bounded (`internal/api/supervisor_events.go:337-405`). The lock-wedge tests remain necessary to prove that the installed implementation honors that bound at the actual blocked-I/O boundary.
- F5’s conservative lease disposition can skip one GUI recovery opportunity after an ambiguous release. Tick-local scope limits that availability cost while preserving the no-overlap invariant.

## 9. Gate: RETURN(lead)
