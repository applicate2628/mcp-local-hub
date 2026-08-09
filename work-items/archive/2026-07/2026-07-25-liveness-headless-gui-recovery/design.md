# PR #589 round-4 closure design

## 1. Pinned input and scope check

- Pinned identity: `PR589-R4-F5-F7-HEAD-db879c5f`.
- Evaluated local head: `db879c5f76e479a7ae568f58ae87de864f3d9f28`.
- Protected already-fixed baseline: F1-F4 in local commit `f150be61`.
- Accepted factual input:
  `research-pr589-round4.md` (`PASS`; F1-F4 already fixed, F5-F7 real/open).
- Admitted source owners: `internal/cli/supervise_ensure_alive.go`,
  `internal/api/supervisor_events.go`, and
  `internal/daemonrecovery/recovery.go`, plus focused tests and the owning
  canonical docs.
- Excluded: GUI/tray/supervisor launch, Task Scheduler, real state, unrelated
  review findings, publication, and changes outside this worktree.

## 2. Candidate roster

| Lane | Framing | Execution | Model/profile | Artifact |
| --- | --- | --- | --- | --- |
| Owner seam | Keep each correction in the current lease, event-log, or recovery owner | External Codex command-line interface (CLI), read-only, neutral working directory | `gpt-5.6-sol` / `xhigh` | `design-owner-seam.md` |
| Failure injection | Choose contracts by the failure each one must survive | External Codex CLI, read-only, neutral working directory | `gpt-5.6-sol` / `xhigh` | `design-failure-injection.md` |

Both runs used file-based prompts, explicit model and effort, isolated output
files, and completed with exit `0`.

## 3. Independence check

The lanes received the same pinned evidence packet but distinct framings and
separate prompt/output files. Neither candidate saw the other's output. They
converged independently on:

- a tick-local retained-lease disposition for F5;
- event-owner durability and one logical audit identity for F6;
- changing only the two pre-action detection emits for F7.

Their useful disagreement was F6 timing and F7 delivery posture. The
failure-injection lane required a pre-action durable reservation and selected a
bounded emit; the owner-seam lane kept restart delivery first and selected
lossy non-blocking detection emits.

## 4. Comparison matrix

| Question | Owner-seam lane | Failure-injection lane | Synthesis |
| --- | --- | --- | --- |
| F5 state | `not exposed`, `released`, `may retain` | equivalent three-state result | Accept one typed, monotonic lease lifecycle shared by the GUI probe owner and the Phase-I timeout owner. |
| F5 consumer | gate the two GUI-owner relaunch paths | same | Accept; do not rewrite daemon topology or the 90-second Unknown marker contract. |
| F6 durable carrier | event-owner handoff after respawn | event-owner reservation before destructive action | Select post-respawn handoff because the supplied finding requires return-time process-exit durability while the existing contract reserves respawn delivery after commit (`internal/daemonrecovery/recovery.go:531-551`). |
| F6 identity | action-keyed row/handoff | same | Accept exact normalized event bytes plus a stable content identity owned by the event-log layer. |
| F7 emission | `TryEmit` at two sites | cumulative bounded `EmitWithTimeout` | Select a third resolution: perform the recovery/suppressor decision before the existing blocking observability call. This removes diagnostics from the action-critical path without adding another timed worker. |

## 5. Resolved conflicts and verified assumptions

1. The event owner has no existing outbox, audit journal, or replay mechanism:
   the current code exposes only blocking, try, timeout, and tracked-goroutine
   emission (`internal/api/supervisor_events.go:323-405,614-682`), and the
   current-session control search for `outbox`, pending-audit, audit-journal,
   handoff, and audit replay found no implementation under `internal/`.
   Therefore F6 needs one narrow owner-level extension; a recovery-local spool
   is rejected.
2. `TryEmit` is non-blocking only while acquiring the in-process mutex and
   cross-process flock (`internal/api/supervisor_events.go:387-405,502-505,
   569-577`). After acquisition it performs rotation/open/write/close
   synchronously with no write deadline (`internal/api/supervisor_events.go:
   598-611`). It cannot own F7's full progress guarantee. The supplied finding
   expressly permits moving observability after the recovery decision, which
   is the selected correction.
3. A tracked timeout can hand both locks to an unbounded in-process worker
   (`internal/api/supervisor_events.go:614-682`), and the recovery fallback
   currently returns on an unsettled worker (`internal/daemonrecovery/recovery.go:817-825`).
   A process-local wait is therefore not a durability boundary.
4. The event envelope has no stable top-level identifier
   (`internal/api/supervisor_events.go:129-151`). To avoid a schema change, the
   handoff identity is the digest of the exact normalized JSONL bytes. The
   recovery event timestamp must be fixed before its first tracked attempt so
   the original row and handoff serialize identically; event serialization
   otherwise fills time at marshal time (`internal/api/supervisor_events.go:
   731-776`).
5. `ReleaseErr` is the positive release proof surface used by the existing
   free-lease path (`internal/cli/supervise_ensure_alive.go:678-710`). Every
   other `Lease.Release()` site in the Phase-I class must feed the same outcome
   instead of discarding it.
6. `GUIOwnerLeaseProbeResult` currently exposes only state, reason, an
   optional caller-owned lease, and the marker record
   (`internal/gui/single_instance.go:105-117`). Internal tentative-acquisition
   paths release before returning and discard the release disposition
   (`internal/gui/single_instance.go:485-521,614-618`). F5 therefore requires
   a typed lifecycle seam in the GUI probe owner; mapping all Unknown outcomes
   to `may retain` would incorrectly suppress relaunch after pre-acquisition
   validation/read failures.

## 6. Coherent final design

### F5 — retained GUI lease

The GUI probe owner and `runEnsureAliveGUIRecovery` share one typed, monotonic
lease lifecycle. The lifecycle atomically arbitrates acquisition against the
outer timeout:

- `open`: no acquisition has been admitted.
- `closed-before-exposure`: the timeout owner closed the gate before the probe
  could attempt acquisition; the probe must not touch the flock afterward.
- `exposed`: the probe won the gate immediately before attempting the flock;
  the timeout owner must treat this as `may retain` until a later positive
  result.
- `not-acquired`: the admitted attempt returned busy/error without obtaining a
  lease.
- `released`: a previously acquired lease returned nil from `ReleaseErr`.
- `release-unconfirmed`: release returned an error.

The probe executes one compare-and-swap from `open` to `exposed` immediately
before `tryAcquireSingleInstanceLockAt`. The timeout path executes the competing
`open` to `closed-before-exposure` transition before returning. Exactly one can
win: if timeout wins, no later acquisition is permitted; if probe wins,
timeout consumes `may retain`. Busy/acquisition error publishes
`not-acquired`. Every internal tentative release publishes `released` or
`release-unconfirmed` in `GUIOwnerLeaseProbeResult`; a Free result publishes
`exposed` and the caller completes the same lifecycle after `ReleaseErr`.
Pre-acquisition Unknown remains safe and does not suppress future ticks.

`runEnsureAliveGUIRecovery` returns this tick-local disposition to
`runEnsureAlive`.

- Every caller-owned lease exit observes `ReleaseErr`. Only nil publishes
  `released`; cancellation, invalid probe state, marker mismatch,
  compare-and-swap paths, and explicit release failure publish
  `release-unconfirmed`.
- The existing Unknown diagnostic remains. The new result additionally removes
  GUI-owner relaunch authority for this tick.
- `runEnsureAlive` evaluates supervisor and GUI topology as today. The
  capability gates every path that can invoke `livenessRelaunchFn` for a GUI
  owner: running+ConfirmedDead, down+ConfirmedDead, and the
  Unknown-confirmation delegation. Down+Alive/Unknown standalone supervisor
  recovery remains eligible because it does not acquire the GUI flock.

This preserves the uninterrupted Unknown-window contract and makes the
availability cost one scheduled tick.

### F6 — committed-audit process lifetime

The supervisor-event owner gains a durable pending-event handoff adjacent to
the primary log.

- The committed recovery event is normalized once, including timestamp, before
  the tracked attempt. Its exact JSONL bytes define the handoff identity.
- Fast tracked success remains durable before respawn and creates no handoff.
- Respawn remains ahead of fallback finalization.
- After respawn, every no-attempt, definite failure, unsettled worker, or
  release-uncertain outcome asks the event owner to persist the exact event
  bytes atomically in its pending directory. The same-directory temporary file
  is fully written, synced, closed, and atomically renamed before
  acknowledgement. An identical digest filename is accepted only after
  full-content equality is verified.
- Handoff persistence failure returns a distinct
  `FailureAuditDurability` operation result after respawn. The CLI prints that
  the recovery action was committed and respawn delivery was attempted or
  accepted, but the audit handoff could not be preserved, and exits with a
  dedicated non-zero code. It is never misreported as a respawn failure or as
  fully audited.
- The finalizer then makes a non-blocking replay attempt. Contention leaves the
  durable handoff intact and permits the one-shot caller to return.
- Every `SupervisorEventLog` emit mode replays pending files after acquiring
  its existing mutex+flock and before rotating or appending the current row.
  Before replay it compares complete JSONL records in the active log and `.1`
  backup. This handles a late original completion without a duplicate.
- After replay append, the handoff is deleted. If the process dies between
  append and delete, the next replay sees the exact row and deletes the
  handoff without appending again.
- A release failure never triggers a second same-process blocking acquisition;
  it only persists the handoff.
- Pending files are capped at the existing event maximum and replay processes a
  fixed maximum count per pass. Digest/content mismatch, malformed/oversize
  input, scan/read/append/sync/rename/delete error, or failed retirement leaves
  the handoff in place and fails the current replay; none authorizes another
  append or silently discards the carrier.

The pending directory is not a second audit system: serialization, replay,
deduplication, and retirement all belong to `SupervisorEventLog`.
The guarantee is process-exit-safe and exactly once within the repository's
retained active-plus-`.1` history; it does not claim power-loss durability or
unbounded historical exactly-once.

### F7 — pre-action detection observability

Keep the existing blocking helper, but remove both detection calls from the
action-critical path:

- After the Unknown marker is durably consumed, defer
  `gui-owner-unknown-escalated-to-recovery`, then invoke headless recovery.
- At entry to headless recovery, defer `gui-headless-fleet-detected`, then
  evaluate suppressors and invoke the relaunch callback if authorized.

The deferred diagnostic may delay function/process return, but it cannot delay
the already-completed suppressor or relaunch decision. Phase-I, no-action,
suppressed, and post-callback outcome events remain unchanged. No new worker,
retry, timeout, or lossy event contract is introduced.

## 7. Change-surface contract

| Surface | Allowed change | Preserved contract |
| --- | --- | --- |
| `internal/gui/single_instance.go` | add the typed acquisition/release lifecycle to the GUI probe result/request and publish every internal release outcome | pre-acquisition validation semantics, Held/Free/Unknown classification, single-instance ownership |
| `internal/cli/supervise_ensure_alive.go` | arbitrate timeout versus acquisition; return/consume one tick-local lease disposition; defer the two pre-action detection emits | F1-F4 behavior, Unknown marker ownership, topology classification, standalone supervisor recovery |
| `internal/api/supervisor_events.go` | pending-handoff persist/replay/dedupe owned by the event log | blocking/try/timeout/tracked contracts, one mutex+flock writer order, JSONL format |
| `internal/daemonrecovery/recovery.go` | normalize committed event; replace post-respawn in-memory-only fallback with event-owner finalization; return distinct audit-durability failure | destructive identity gate, mandatory respawn reservation, no duplicate/reacquire on release uncertainty |
| `internal/cli/daemon_recover.go` | map the distinct post-respawn audit-durability failure to explicit wording and a dedicated non-zero exit code | existing failure mapping and CLI syntax |
| focused tests | deterministic release, timeout, process-exit/handoff, replay, and wedged-lock probes | no production process, GUI, tray, supervisor, scheduler, or live state |

No dependency, external wire shape, scheduler contract, or public CLI syntax is
changed. The new audit-durability exit code is an intentional additive
operational-contract change.

## 8. Falsifying probes

| Class | Probe that must fail without the correction |
| --- | --- |
| F5 | Race timeout against every boundary before acquisition, after acquisition, and during release; the lifecycle compare-and-swap must prevent post-timeout acquisition or return `may retain`. Drive running+dead, down+dead, and elapsed Unknown recovery; only `may retain` suppresses same-tick GUI relaunch. |
| F6 | Block the tracked writer, complete respawn, persist the handoff, terminate the helper process, and in a new process prove the handoff survives and replays to exactly one retained row. Mutations removing persistence, all-mode replay, full-content verification, or retain-on-error must fail. |
| F7 | Hold the event-log flock and separately stall the write after acquisition at each detection site. The suppressor/relaunch callback must be observed before the diagnostic attempt. Moving either deferred emit back before the callback must fail. |

## 9. Numbered claims

1. **Guarantee:** no GUI-owner relaunch occurs in a tick that may still own the
   Phase-I GUI lease, while pre-acquisition failures do not suppress relaunch.
   **Single owner:** typed acquisition/release lifecycle spanning
   `ProbeGUIOwnerLease` and `runEnsureAliveGUIRecovery`.
   **Enforcement probe:** race-engineered pre/post-acquisition plus
   running/down/Unknown callback matrix.
2. **Guarantee:** every committed clean-reap or termination-unconfirmed event
   has either its canonical row or a process-exit-safe event-owner handoff
   before recovery returns. **Single owner:** `SupervisorEventLog` durable
   handoff lifecycle. **Enforcement probe:** subprocess-exit persistence plus
   replay.
3. **Guarantee:** one logical committed action produces one retained audit row
   even when the original tracked writer completes late.
   **Single owner:** exact-byte identity and replay deduplication under the
   event-log flock. **Enforcement probe:** late-write/replay race cardinality.
4. **Guarantee:** the two detection rows cannot block the recovery/suppressor
   decision. **Single owner:** action-before-observability ordering at the two
   detection sites. **Enforcement probe:** flock-contention and stalled-write
   callback-before-diagnostic tests at both sites.
5. **Guarantee:** F1-F4 local closures remain unchanged.
   **Single owner:** their existing code and regression tests.
   **Enforcement probe:** the four protected focused tests plus final diff
   review.

## 10. Decision registry

No new cross-work-item architecture decision is required. The durable handoff
is a narrow extension of the existing supervisor-event storage lifecycle, not
a new system-wide audit policy.

## Gate

**PASS** — the design resolves the candidate conflicts, assigns one owner per
guarantee, covers every participant in the three supplied open classes, and
names a falsifying probe for each.

## Terms and Abbreviations

- CAS: compare-and-swap of the restart handoff marker.
- CLI: command-line interface.
- F5-F7: the fifth through seventh supplied PR #589 findings.
- JSONL: one JSON object per line.
- `may retain`: this tick cannot prove that its GUI single-instance lease was
  released.
