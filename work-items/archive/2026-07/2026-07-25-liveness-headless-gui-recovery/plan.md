# Implementation plan — PR #589 round-4 closure

## Accepted inputs and execution boundary

- Pinned source identity: `PR589-R4-F5-F7-HEAD-db879c5f`.
- Accepted factual artifact: `research-pr589-round4.md` (`PASS`).
- Accepted design: `design.md` (`PASS`).
- Accepted specialist constraint: `review-reliability-pr589-round4.md`
  (final re-verification `PASS`).
- F1-F4 are protected local closures in commit `f150be61`; no product change is
  admitted for them.
- F5-F7 are the only implementation findings.
- Integration owner: the main conversation holding `$lead`.
- Implementation owner: `$backend-engineer`.
- Verification owner: `$qa-engineer`.
- Final maintainability/resource-lifetime gate: `$architecture-reviewer`.

No phase may launch the graphical user interface (GUI), tray, supervisor,
daemon, Task Scheduler, or a real process kill. No phase may touch another
worktree, use an unscoped `go test ./...`, or use `git checkout --`,
`git reset --hard`, `git stash`, or `git push`.

## Change-surface contract

| Surface | Allowed change | Must not change |
| --- | --- | --- |
| `internal/gui/single_instance.go` | Typed atomic lifecycle for the GUI owner probe and every acquisition/release transition | Existing Held/Free/Unknown meanings, reservation checks, and single-instance ownership |
| `internal/cli/supervise_ensure_alive.go` | Return and consume the tick-local lifecycle disposition; suppress GUI-owner relaunch when the lease may remain; defer two pre-action diagnostics | F1, F3, F4 behavior; 90-second Unknown marker semantics; standalone supervisor recovery |
| `internal/api/supervisor_events.go` | Normalize-once record type; adjacent pending handoff persistence, replay, exact-line deduplication, and bounded replay | Existing JSONL envelope, active plus `.1` retention, mutex then flock order, public emit-mode semantics |
| `internal/daemonrecovery/recovery.go` | Use one prepared event; persist/finalize it after respawn; return typed audit-durability failure | Destructive identity gate, mandatory respawn reservation, F2 no-reacquire behavior |
| `internal/cli/daemon_recover.go` | Dedicated failure mapping and exit code `7` | Existing exit codes `2` through `6`, command syntax, and success wording |
| Focused tests in the same packages | Deterministic lifecycle, persistence, replay, process-exit, failure-injection, and ordering guards | No production scheduler, GUI, supervisor, live state, or real process termination |
| `docs/supervisor-architecture.md` | Current state-tree, pending-handoff, replay, and exit-7 contract | Historical specifications under `docs/superpowers/` |

`internal/gui/daemon_recover.go`, external HTTP response codes, scheduler code,
and dependency manifests are outside the accepted change surface.

## Phase A — F5 typed GUI-lease lifecycle and relaunch authority

**Owner:** `$backend-engineer`, with `$reliability-engineer` re-review required
if any transition differs from this table.

**Files:** `internal/gui/single_instance.go`,
`internal/gui/single_instance_restart_test.go`,
`internal/cli/supervise_ensure_alive.go`,
`internal/cli/supervise_ensure_alive_test.go`.

### A1. Exact API and state machine

Add these internal exported symbols to `internal/gui` because the `internal/cli`
consumer must share the same atomic owner:

| Symbol | Exact contract |
| --- | --- |
| `GUIOwnerLeaseLifecycleState` | Typed `uint32` state stored only through `atomic.Uint32` |
| `GUIOwnerLeaseLifecycleOpen` | Zero value; no acquisition has been admitted |
| `GUIOwnerLeaseLifecycleClosedBeforeExposure` | Timeout won before acquisition; probe must never touch the flock afterward |
| `GUIOwnerLeaseLifecycleExposed` | Probe won immediately before the flock attempt; this state means the tick may retain the lease |
| `GUIOwnerLeaseLifecycleNotAcquired` | Admitted flock attempt returned busy or error without a lease |
| `GUIOwnerLeaseLifecycleReleased` | An acquired lease returned nil from `ReleaseErr` |
| `GUIOwnerLeaseLifecycleReleaseUnconfirmed` | `ReleaseErr` returned non-nil |
| `GUIOwnerLeaseDisposition` | Two-value consumer result: `NoRetainedLease` or `MayRetainLease` |
| `NewGUIOwnerLeaseLifecycle` | Creates the lifecycle in `Open` |
| `TryExpose` | Single compare-and-swap from `Open` to `Exposed`; false means no acquisition is allowed |
| `CloseBeforeExposure` | Competing compare-and-swap from `Open` to `ClosedBeforeExposure` |
| `PublishNotAcquired` | Only `Exposed` to `NotAcquired` |
| `PublishRelease` | Only `Exposed` to `Released` for nil or `ReleaseUnconfirmed` for non-nil |
| `Disposition` | `MayRetainLease` only for `Exposed` and `ReleaseUnconfirmed`; every other valid state is `NoRetainedLease`; an invalid numeric state fails closed as `MayRetainLease` |

Add `Lifecycle *GUIOwnerLeaseLifecycle` to
`GUIOwnerLeaseProbeRequest` and return the same pointer in
`GUIOwnerLeaseProbeResult`. A nil lifecycle is a pre-acquisition contract error:
return Unknown and do not touch the flock.

The transition inventory is complete and monotonic:

| From | Trigger/owner | To | Required effect |
| --- | --- | --- | --- |
| `Open` | Outer Phase-I deadline expires | `ClosedBeforeExposure` | Return `NoRetainedLease`; a later worker cannot acquire |
| `Open` | Probe immediately before `tryAcquireSingleInstanceLockAt` | `Exposed` | Acquisition may proceed |
| `Exposed` | Flock busy or acquisition error | `NotAcquired` | No same-tick suppression |
| `Exposed` | Internal tentative release succeeds | `Released` | Preserve the existing Held/Unknown result |
| `Exposed` | Internal tentative release fails | `ReleaseUnconfirmed` | Return Unknown and suppress every GUI-owner relaunch this tick |
| `Exposed` | Caller-owned `ReleaseErr` succeeds | `Released` | Preserve the existing Phase-I diagnostic |
| `Exposed` | Caller-owned `ReleaseErr` fails | `ReleaseUnconfirmed` | Preserve Unknown diagnostic and suppress every GUI-owner relaunch this tick |

Every attempted transition from a terminal state is rejected without replacing
the earlier evidence. Tests must cover the competing `Open` transitions under
the race detector-compatible channel barrier; no sleep-based race assertion is
accepted.

### A2. Every caller and release path

`ProbeGUIOwnerLease` calls `TryExpose` immediately before
`tryAcquireSingleInstanceLockAt`. It publishes `NotAcquired` on both busy and
acquisition-error returns. It publishes the result of every release currently
owned by `unknownAfterTentativeLease` and the reservation-found-after-acquire
branch.

`runEnsureAliveGUIRecoveryWithinBudget` must replace every error-discarding
`Lease.Release()` with `ReleaseErr()` plus `PublishRelease` on:

1. context completion after the probe;
2. invalid Held result carrying a lease;
3. invalid Unknown result carrying a lease;
4. invalid/default state carrying a lease.

`runEnsureAliveGUIRecoveryFree` uses its existing `sync.OnceValue` release
owner, but that owner also publishes the same result exactly once for marker
mismatch, cancellation, compare-and-swap, diagnostic, and normal exits.

Change `runEnsureAliveGUIRecovery` to return `GUIOwnerLeaseDisposition`. The
outer timeout first calls `CloseBeforeExposure`, then returns `Disposition`.
Replace the string-only result channel with
`ensureAliveGUIRecoveryResult{Output string, LeaseDisposition
gui.GUIOwnerLeaseDisposition}`. The worker writes its buffered output plus the
shared lifecycle's terminal disposition into that value; the completed-worker
branch returns exactly that disposition.

`runEnsureAlive` computes one immutable
`allowGUIOwnerRelaunch := disposition != MayRetainLease` and threads it through:

1. running supervisor plus ConfirmedDead GUI to
   `runEnsureAliveHeadlessFleet`;
2. elapsed Unknown confirmation to
   `runEnsureAliveGUIOwnerUnknownEscalation`, then to headless recovery;
3. down supervisor plus ConfirmedDead GUI before its direct
   `livenessRelaunchFn` call.

When false, use the existing `ensureAliveHeadlessFleetSuppress` owner with
reason exactly `phase-i-lease-unconfirmed` and detail stating that the current
one-shot process may still own the GUI single-instance flock. Extend that
helper's closed reason vocabulary from two to three values. Down+Alive and
down+Unknown keep the standalone supervisor path because it never acquires the
GUI flock.

### A3. Tests and acceptance

| ID | Observable assertion |
| --- | --- |
| A-AC1 | `TestProbeGUIOwnerLeaseLifecycle_TimeoutBeforeExposurePreventsAcquisition` proves a timeout-winning compare-and-swap causes zero acquisition calls. |
| A-AC2 | `TestProbeGUIOwnerLeaseLifecycle_EveryInternalReleasePublishesDisposition` covers cancellation-after-acquire, marker reread failure/change, clock failure, reservation-after-acquire, and final cancellation; nil release maps to `Released`, non-nil maps to `ReleaseUnconfirmed`. |
| A-AC3 | `TestEnsureAliveGUIRecovery_RetainedLeaseSuppressesEveryGUIOwnerRelaunch` has exactly three subtests: `running-confirmed-dead`, `down-confirmed-dead`, and `elapsed-unknown`; every subtest observes zero `livenessRelaunchFn` calls and the exact suppression reason. |
| A-AC4 | `TestEnsureAliveGUIRecovery_PreAcquisitionUnknownDoesNotSuppressRelaunch` proves validation/read failure before `TryExpose` does not suppress an otherwise-authorized later relaunch. |
| A-AC5 | `TestEnsureAliveGUIRecovery_ClassifierTimeoutRetainsLeaseUntilCASCompletes` is revised to assert zero same-tick GUI-owner relaunch while the worker is `Exposed`; it still proves the later compare-and-swap completes before process cleanup. |
| A-AC6 | Existing Unknown-window, live-handoff, boot-grace, and standalone-relaunch tests retain their original outcomes. |

**Diff-invisible invariants copied from the design:** pre-acquisition Unknown is
not a retained-lease claim; the Unknown confirmation window remains
uninterrupted and single-owned; standalone supervisor recovery remains
GUI-independent; one retained-lease suppression costs only one scheduled tick.

**Named regression guards:** the five exact tests in A-AC1 through A-AC5 plus
the existing `TestEnsureAlive_HeadlessFleet_UnknownEscalatesAfterConfirmationWindow`,
`TestEnsureAlive_HeadlessFleet_LiveHandoffSuppresses`, and
`TestEnsureAlive_FreeLock_UnknownGUIOwnerState_UsesStandaloneRelaunch`.

**Revert unit:** Phase A is standalone. Revert all four Phase-A files together;
do not retain the CLI consumer without the GUI lifecycle owner.

## Phase B — F6 event-owner pending handoff

**Owner:** `$backend-engineer`; this shared API phase requires
`$architecture-reviewer` review before integration.

**Files:** `internal/api/supervisor_events.go`,
`internal/api/supervisor_events_test.go`.

### B1. Exact API, layout, and normalization

Add these symbols:

| Symbol | Exact contract |
| --- | --- |
| `PreparedSupervisorEvent` | Immutable exported value with unexported normalized JSONL bytes and SHA-256 digest |
| `PrepareSupervisorEvent` | Calls the existing envelope defaulting/truncation owner exactly once and returns bytes including one terminal newline |
| `EmitPreparedWithTimeoutTracked` | Uses the prepared bytes without remarshal or timestamp regeneration |
| `PersistPending` | Atomically establishes the prepared record's durable handoff |
| `TryReplayPending` | Non-blocking mutex/flock attempt; contention leaves handoffs intact and is not an error |

Existing `Emit`, `TryEmit`, `EmitWithTimeout`, and
`EmitWithTimeoutTracked` keep their signatures. Each prepares once, then enters
the common prepared-byte path.

The exact layout is:

```text
<state-dir>/supervisor-events.log.pending/
  <64-lowercase-hex-sha256-of-exact-jsonl-bytes>.jsonl
```

The pending directory is `l.path + ".pending"`. Each final file contains
exactly one normalized JSONL record, including its terminal newline. The
maximum valid pending-file length is exactly
`supervisorEventMaxBytes + 1` bytes. One replay pass processes at most
`64` final `.jsonl` files. The directory may retain more than 64 carriers;
later emits drain later batches. Temporary names begin with `.tmp-` and are
never treated as replayable carriers.

### B2. Secure atomic persistence

`PersistPending` performs this exact sequence:

1. Validate the prepared value: non-empty, one terminal newline, no embedded
   second line, length at most `supervisorEventMaxBytes + 1`, and digest equal
   to the bytes.
2. Create the adjacent pending directory with mode `0700`; apply `Chmod(0700)`
   after creation where supported.
3. If the final digest path exists, read at most
   `supervisorEventMaxBytes + 2` bytes. Exact content equality is success.
   Any difference is a digest/content collision error; neither file is
   overwritten or deleted.
4. Create a same-directory `.tmp-*` file with exclusive creation and mode
   `0600`; write all bytes, call `Sync`, close, and propagate every write,
   sync, or close failure.
5. Use `os.Link(temp, final)` as the atomic no-replace publication step.
   If `os.Link` reports that `final` already exists, re-read and require exact
   content equality. Any other link failure is a durability failure.
6. Remove the temp link. A deferred cleanup attempts the same removal on every
   failure path. The final handoff is never removed by persistence error
   cleanup.

This same-directory hard-link publication is the cross-platform standard-library
no-replace primitive; unsupported filesystems fail closed through
`FailureAuditDurability`. Power-loss durability is not claimed, so no
directory-fsync contract is added.

For deterministic failure injection without package-global races, add one
package-private `supervisorEventPendingIO` field to each
`SupervisorEventLog`. Its callbacks own directory creation/chmod, temp-file
creation, bounded read/directory scan, hard-link publication, rotation,
append-open/write/sync/close, and removal. `OpenSupervisorEventLog` installs
the production operating-system callbacks; same-package tests replace callbacks
only on their own log handle. No exported setter or mutable global is added.

### B3. Replay, exact retained-history proof, and resource lifetime

All four emit modes converge on one new `writeEventBatch` while the existing
in-process mutex and cross-process flock are held. `writeEventBatch` first
calls `replayPendingLocked`, then calls the captured
`supervisorEventWriteFn` for the current prepared event. Preserve
`supervisorEventWriteFn` as the injectable current-row physical-write seam;
its production target remains `writeEventLine`. The timeout mode runs the whole
batch inside its existing tracked worker, so replay I/O remains covered by the
same caller-timeout/worker-ownership contract. Replay itself appends through a
separate package-private `appendSupervisorEventLine(raw, sync bool)` owner:
`writeEventLine` passes `false`, while pending replay passes `true`. This keeps
an injected stalled current write after the replay prefix and prevents test
seams from bypassing the all-mode ordering rule.

Replay reads at most 65 directory entries, filters final digest-form `.jsonl`
names, sorts the selected names lexically, and processes at most 64. For each:

1. Read no more than `supervisorEventMaxBytes + 2`.
2. Require the exact one-line normalized format and recompute SHA-256; filename
   and content must match.
3. Scan both `l.path` and `l.path + ".1"` for complete newline-terminated
   records and compare the entire raw record, including newline. A trailing
   incomplete retained-log fragment is not a match; read errors and retained
   records above the event cap fail replay.
4. If an exact retained row exists, remove the pending file without appending.
5. Otherwise run the existing rotation check, append the prepared row, `Sync`
   and close the active log, and only then remove the pending file.

If the process exits after append but before removal, the next replay proves the
exact row across active plus `.1` and retires the handoff without a second
append. If the original tracked writer is late, it still owns the event-log
flock; replay cannot pass it, so replay sees the original row after that flock
is released.

Digest/content mismatch, malformed or oversized input, scan/read/rotation/open/
append/sync/close/remove error, and failed retirement all stop the current
replay and retain the final handoff. No error authorizes a second append.
`TryReplayPending` always releases its mutex/flock on success, error, and panic;
contention performs no I/O and returns. `.tmp-*` files are ignored because an
active or process-abandoned persistence operation may own them; same-process
deferred cleanup is the only automatic temp deletion.

### B4. Tests and acceptance

| ID | Observable assertion |
| --- | --- |
| B-AC1 | `TestSupervisorEvent_PrepareOncePreservesTimestampAndBytes` proves tracked emit and handoff use byte-identical timestamped JSONL. |
| B-AC2 | `TestSupervisorEventPending_PersistAtomicCollisionAndBounds` proves mode `0700`/`0600` where observable, exact digest filename, `16 KiB + 1` maximum, exact-content idempotency, collision refusal, and temp cleanup. |
| B-AC3 | `TestSupervisorEventPending_ReplaysBeforeEveryEmitMode` covers blocking, try, timeout, and tracked-timeout modes and observes pending row order before the current row. |
| B-AC4 | `TestSupervisorEventPending_ExactActiveAndBackupDedupe` has active, `.1`, absent, partial-tail, and content-collision rows; exact active or `.1` produces one retained row and retirement. |
| B-AC5 | `TestSupervisorEventPending_LateWriterConcurrentReplayAndRotationExactlyOnce` engineers the stalled-writer window and rotation threshold; active plus `.1` contains exactly one matching row. |
| B-AC6 | `TestSupervisorEventPending_ConcurrentReplayExactlyOnce` runs two processes/handles against one carrier and observes one retained row plus retirement. |
| B-AC7 | `TestSupervisorEventPending_RetainsOnEveryFailure` injects scan, read, rotate, open, append, sync, close, and remove failures; every subtest retains the final carrier and returns non-nil. |
| B-AC8 | `TestSupervisorEventPending_ReplayBatchIsCappedAt64` creates 65 valid carriers and proves exactly 64 are processed in one pass. |
| B-AC9 | Existing emit timeout, pending-worker, rotation, release-failure, and concurrent-emit tests retain their original outcomes. |

**Diff-invisible invariants copied from the design:** all writers keep mutex then
flock order; release failure never triggers reacquisition; exact-once is
limited to retained active plus `.1` history; no power-loss or unbounded-history
claim is introduced.

**Named regression guards:** B-AC1 through B-AC8 plus existing
`TestSupervisorEventLog_EmitWithTimeoutTrackedPendingObservesLateWrite`,
`TestSupervisorEventLog_EmitReportsFlockReleaseFailure`, and
`TestSupervisorEvent_Rotation`.

**Revert unit:** Phases B and C are one atomic revert group. Once recovery
persists `PreparedSupervisorEvent`, Phase B cannot be reverted independently.

## Phase C — F6 daemon-recovery finalization and CLI contract

**Owner:** `$backend-engineer`.

**Files:** `internal/daemonrecovery/recovery.go`,
`internal/daemonrecovery/recovery_test.go`,
`internal/cli/daemon_recover.go`,
`internal/cli/daemon_recover_test.go`.

### C1. Recovery ownership and outcome ordering

Add `FailureAuditDurability = "audit_durability_failed"` to `FailureKind`.
Immediately after constructing either committed recovery event, set its
timestamp from the existing `postKillStarted.UTC()` sample and call
`api.PrepareSupervisorEvent`. Preparation failure is retained as the committed
audit finalizer's error; it must not preempt the mandatory respawn.

Replace the committed branch's queued `func()` with one typed finalizer that
owns exactly one prepared value, the tracked pending handle, and the original
emit error. Preserve the no-kill `already_exited` best-effort closure
independently.

After `deps.Respawn` returns and before success/error classification:

| Tracked outcome | Finalizer action |
| --- | --- |
| Fast success | No handoff; return nil |
| Pending completed with nil | No handoff; return nil |
| Pending still unsettled | `PersistPending(prepared)`, then `TryReplayPending` |
| Direct or pending release failure | `PersistPending(prepared)` only; do not reacquire the possibly-retained event-log flock |
| No attempt, preparation/open/marshal/lock failure, or definite worker failure | `PersistPending(prepared)`, then `TryReplayPending` |

`PendingSupervisorEventEmit.Wait(0)` is called at most once. A successful
`PersistPending` is the process-exit-safe acknowledgement. `TryReplayPending`
is best-effort after that acknowledgement: contention or replay error leaves
the carrier and does not turn the committed recovery into a durability
failure. Only failure to establish or validate the durable carrier returns
`FailureAuditDurability`.

Finalize the audit before post-commit notifications so a panicking notification
cannot preempt the carrier. Respawn remains before every potentially blocking
or filesystem finalization step.

Failure precedence is exact: if handoff persistence fails, return
`OperationError{Kind: FailureAuditDurability}` even if respawn also failed.
Preserve the actual `RespawnResult` in `OperationError.Respawn`; join a
non-nil respawn call error into `Cause`. This prevents an audit-durability
failure from being mislabeled as a respawn failure while retaining both facts.
If durability succeeds, apply the existing respawn classification unchanged.

### C2. CLI exit behavior

Add `daemonRecoverExitAuditDurability = 7`. Extend the exit-code comment and
`TestDaemonRecoverHermeticExitContract`.

`printRecoverError` handles `FailureAuditDurability` before the generic default:

- when `OperationError.Respawn.Success` is true, stderr states that process
  termination was committed and forced respawn was accepted, but the audit
  record/handoff could not be preserved;
- otherwise stderr states that process termination was committed, forced
  respawn was attempted but not accepted (including the bounded code/message
  when present), and the audit record/handoff could not be preserved;
- both forms return exit `7`;
- neither form prints the ordinary success line or claims that the respawn
  itself caused the audit failure.

### C3. Tests and acceptance

| ID | Observable assertion |
| --- | --- |
| C-AC1 | `TestExecutePendingCommittedAuditSurvivesProcessExitAndReplaysOnce` starts the current hermetic test binary as a helper, stalls the tracked writer, observes respawn then recovery return, exits the helper, and uses a new process/handle to replay exactly one retained row. |
| C-AC2 | `TestExecuteAuditHandoffPersistenceFailureReturnsFailureAuditDurabilityAfterRespawn` proves respawn was called once before the typed error and that no success is reported. |
| C-AC3 | `TestExecuteAuditDurabilityFailurePreservesRespawnFailureFact` injects both failures and proves `Kind`, `Cause`, and `Respawn` retain both facts. |
| C-AC4 | `TestQueueIdempotentAuditFallbackOutcomeMatrix` is replaced or revised into the finalizer matrix covering fast success, no attempt, acquisition timeout, definite write failure, direct release failure, pending success, pending timeout, pending release failure, and pending definite failure. |
| C-AC5 | `TestExecuteFastCommittedAuditIsDurableBeforeRespawn` remains green and proves the fast path creates no pending file. |
| C-AC6 | `TestExecuteDoesNotHangWhenAuditWorkerNeverSettles` remains bounded and now proves the pending carrier exists before return. |
| C-AC7 | `TestDaemonRecoverAuditDurabilityFailureExit7PreservesCommittedRespawnWording` covers accepted and failed respawn variants, exact exit `7`, stderr facts, and absence of the success line. |
| C-AC8 | Existing exit codes `2` through `6`, F2 release-failure behavior, port wait, detached respawn reservation, and notification ordering remain unchanged. |

The helper subprocess inherits the current test's fresh
`MCPHUB_STATE_DIR_OVERRIDE`, carries `-tags=test_state_path_env` because the
package test binary was built by the tagged `go test`, uses only injected
recovery dependencies, and must not invoke any production process/scheduler
surface.

**Diff-invisible invariants copied from the design:** destructive commit never
waits on audit before respawn; a release-uncertain writer is never reacquired
in the same process; one logical event retains one row; a recovery return never
leaves only an in-memory goroutine as the audit carrier.

**Named regression guards:** C-AC1 through C-AC7 plus existing
`TestExecuteBlockingPostCommitAuditCannotPreemptRespawn`,
`TestExecuteRespawnFailureReturnsCommittedReapFact`, and
`TestExecutePanickingPostCommitNotifyCannotPreemptRespawn`.

**Revert unit:** atomic with Phase B.

## Phase D — F7 action-before-observability ordering

**Owner:** `$backend-engineer`.

**Files:** `internal/cli/supervise_ensure_alive.go`,
`internal/cli/supervise_ensure_alive_test.go`.

At `runEnsureAliveHeadlessFleet`, construct the detection body and register one
deferred `gui-headless-fleet-detected` emit before evaluating suppressors.
Remove the immediate emit. The function then evaluates live-handoff and
boot-grace suppressors and, when authorized, calls `livenessRelaunchFn`; the
deferred diagnostic runs only while returning.

At `runEnsureAliveGUIOwnerUnknownEscalation`, after the confirmation marker is
successfully consumed, register one deferred
`gui-owner-unknown-escalated-to-recovery` emit, then call
`runEnsureAliveHeadlessFleet`. Remove the immediate emit. The nested headless
detection defer completes before the outer escalation defer; the relaunch or
suppressor decision completes before both.

Do not change `emitLivenessEvent`, add a goroutine, add a timeout, or change
Phase-I, failure, suppression, success, or no-action event sites.

| ID | Observable assertion |
| --- | --- |
| D-AC1 | `TestEnsureAliveHeadlessFleet_DetectionWriteCannotBlockRelaunch` blocks `supervisorEventWriteFn` only for `gui-headless-fleet-detected` and observes the relaunch callback before releasing the write. |
| D-AC2 | `TestEnsureAliveHeadlessFleet_DetectionWriteCannotBlockSuppressor` blocks the same row and observes the exact live-handoff/boot-grace suppression output before release. |
| D-AC3 | `TestEnsureAliveUnknownEscalation_DetectionWriteCannotBlockRecovery` blocks `gui-owner-unknown-escalated-to-recovery` and observes the delegated headless relaunch or suppressor decision first. |
| D-AC4 | The same three tests run with a real contended event-log flock and finish their action assertions before the flock is released. |
| D-AC5 | Existing event-presence tests still find exactly one of each detection row after the blocked write is released. |

**Diff-invisible invariants copied from the design:** the existing blocking emit
contract and durable row opportunity remain; only two detection rows move;
diagnostics may delay function return but cannot delay the decision; no lossy
event or unbounded worker is introduced.

**Named regression guards:** D-AC1 through D-AC5 plus existing
`TestEnsureAlive_HeadlessFleet_RelaunchesGUI` and
`TestEnsureAlive_HeadlessFleet_UnknownEscalatesAfterConfirmationWindow`.

**Revert unit:** Phase D is standalone.

## Phase E — canonical documentation, integration, and gates

**Owners:** `$backend-engineer` for docs; `$qa-engineer` for fresh evidence;
`$architecture-reviewer` for final gate; `$lead` for commit.

Update `docs/supervisor-architecture.md` in the same change:

1. Add `supervisor-events.log.pending/` to the state tree with the exact
   digest filename, one-record JSONL format, `16 KiB + 1` file cap, and 64-file
   replay-pass cap.
2. State replay placement before rotation/current append under the existing
   event-log mutex and flock, exact active plus `.1` deduplication, and
   retain-on-error behavior.
3. State the bounded guarantee: process-exit-safe carrier and exactly one row
   only within active plus `.1` retained history; no power-loss or unbounded
   historical guarantee.
4. Add `mcphub daemon recover` exit `7`: destructive action committed,
   respawn attempted/accepted as separately reported, but audit durability
   could not be established.

| ID | Observable assertion |
| --- | --- |
| E-AC1 | Every F5-F7 named guard passes with fresh output under the safe command matrix. |
| E-AC2 | Every F1-F4 protected guard passes unchanged. |
| E-AC3 | Each reversible mutation below makes its named guard fail, the inverse patch restores the exact pre-mutation SHA-256, and the guard passes afterward. |
| E-AC4 | Full touched-package tests pass separately for `internal/gui`, `internal/api`, `internal/daemonrecovery`, and `internal/cli`; no unscoped test command is run. |
| E-AC5 | Tagged `go build ./...` and tagged `go vet ./...` exit `0`, each with a new isolated state directory. |
| E-AC6 | `gofmt` changes only admitted Go files; `git diff --check` exits `0`; final diff contains no machine-local path, secret, raw transcript, or `.scratch/` artifact. |
| E-AC7 | Architecture review returns `PASS` on owner boundaries, lock/resource lifetime, no fix layering, and the active plus `.1` exact-once limit. |

**Diff-invisible invariants copied from the design:** F1-F4 code and tests remain
unchanged; the API/GUI external wire shape is not broadened; all temporary
resources are closed/removed on success and failure; no test reaches production
state.

**Named regression guards:** all A-D guards and the four protected F1-F4 tests
listed below.

**Revert unit:** documentation reverts with its owning implementation. Evidence
reports are append-only summaries and are not used to roll product behavior
back.

## Reversible mutation proof

For every mutation:

1. Record `Get-FileHash -Algorithm SHA256` for every target file.
2. Use the `apply_patch` tool for the one named mutation only.
3. Run only the named test through `Invoke-PR589Isolated`.
4. Require a non-zero exit and a failure message at the intended assertion.
   A compile error, timeout, panic unrelated to the assertion, or empty output
   is not accepted evidence.
5. Use a second `apply_patch` invocation containing the exact inverse hunk.
   No Git restore command is permitted.
6. Recompute SHA-256 and require exact equality, then rerun the test and require
   exit `0`.

| Finding | Mutation | Falsifying test |
| --- | --- | --- |
| F5 | In `runEnsureAlive`, temporarily replace the computed tick-local `allowGUIOwnerRelaunch` with an unconditional authorized value. This removes only propagation, not the lifecycle producer. | `TestEnsureAliveGUIRecovery_RetainedLeaseSuppressesEveryGUIOwnerRelaunch` must fail because at least one of its three callback counts becomes `1`, not `0`. |
| F6 | In the unsettled-pending finalizer arm, temporarily return success before `PersistPending`. Leave replay and all API helpers unchanged. | `TestExecutePendingCommittedAuditSurvivesProcessExitAndReplaysOnce` must fail because the child exits with neither a retained row nor a pending carrier. |
| F7-a | At `runEnsureAliveHeadlessFleet`, temporarily replace the deferred detection call with the same immediate call at its former pre-suppressor position. | `TestEnsureAliveHeadlessFleet_DetectionWriteCannotBlockRelaunch` must fail its callback-before-diagnostic assertion. |
| F7-b | Restore F7-a first. At `runEnsureAliveGUIOwnerUnknownEscalation`, temporarily replace only the deferred escalation call with its former immediate call. | `TestEnsureAliveUnknownEscalation_DetectionWriteCannotBlockRecovery` must fail its delegated-decision-before-diagnostic assertion. |

Mutation output goes under `.scratch/pr589-round4-evidence/` and is never
staged. The lead records command, expected failing assertion, exit code, inverse
hash match, and restored passing output in the final report.

## Safe command matrix

Define this PowerShell helper in the current shell; it creates a fresh
worktree-local state directory per invocation, captures output, restores the
prior environment value, validates the cleanup target, and removes only that
fresh directory:

```powershell
function Invoke-PR589Isolated {
    param([string]$Name, [scriptblock]$Command)
    $repo = (Resolve-Path '.').Path
    $scratch = Join-Path $repo '.scratch'
    $evidence = Join-Path $scratch 'pr589-round4-evidence'
    New-Item -ItemType Directory -Force -Path $evidence | Out-Null
    $state = Join-Path $scratch ("pr589-state-{0}-{1}" -f $Name, [guid]::NewGuid().ToString('N'))
    New-Item -ItemType Directory -Path $state | Out-Null
    $old = $env:MCPHUB_STATE_DIR_OVERRIDE
    try {
        $env:MCPHUB_STATE_DIR_OVERRIDE = $state
        & $Command 2>&1 | Tee-Object -FilePath (Join-Path $evidence ($Name + '.txt'))
        $code = $LASTEXITCODE
        if ($code -ne 0) { throw "command failed with exit $code" }
    } finally {
        if ($null -eq $old) {
            Remove-Item Env:MCPHUB_STATE_DIR_OVERRIDE -ErrorAction SilentlyContinue
        } else {
            $env:MCPHUB_STATE_DIR_OVERRIDE = $old
        }
        $resolved = [IO.Path]::GetFullPath($state)
        $prefix = [IO.Path]::GetFullPath((Join-Path $scratch 'pr589-state-'))
        if (-not $resolved.StartsWith($prefix, [StringComparison]::OrdinalIgnoreCase)) {
            throw "refusing cleanup outside the PR589 scratch prefix: $resolved"
        }
        Remove-Item -LiteralPath $resolved -Recurse -Force
    }
}
```

Run in this order. Every command that can compile or execute
`internal/api` or `internal/cli` carries the required tag and fresh override.

| Gate | Exact command passed to `Invoke-PR589Isolated` |
| --- | --- |
| F5 GUI lifecycle | `go test -tags=test_state_path_env -count=1 -timeout 10m -run '^(TestProbeGUIOwnerLeaseLifecycle_TimeoutBeforeExposurePreventsAcquisition|TestProbeGUIOwnerLeaseLifecycle_EveryInternalReleasePublishesDisposition)$' ./internal/gui/` |
| F5 CLI propagation | `go test -tags=test_state_path_env -count=1 -timeout 10m -run '^(TestEnsureAliveGUIRecovery_RetainedLeaseSuppressesEveryGUIOwnerRelaunch|TestEnsureAliveGUIRecovery_PreAcquisitionUnknownDoesNotSuppressRelaunch|TestEnsureAliveGUIRecovery_ClassifierTimeoutRetainsLeaseUntilCASCompletes)$' ./internal/cli/` |
| F6 event owner | `go test -tags=test_state_path_env -count=1 -timeout 10m -run '^TestSupervisorEventPending_|^TestSupervisorEvent_PrepareOncePreservesTimestampAndBytes$' ./internal/api/` |
| F6 recovery | `go test -tags=test_state_path_env -count=1 -timeout 10m -run '^(TestExecutePendingCommittedAuditSurvivesProcessExitAndReplaysOnce|TestExecuteAuditHandoffPersistenceFailureReturnsFailureAuditDurabilityAfterRespawn|TestExecuteAuditDurabilityFailurePreservesRespawnFailureFact|TestQueueIdempotentAuditFallbackOutcomeMatrix|TestExecuteFastCommittedAuditIsDurableBeforeRespawn|TestExecuteDoesNotHangWhenAuditWorkerNeverSettles)$' ./internal/daemonrecovery/` |
| F6 CLI | `go test -tags=test_state_path_env -count=1 -timeout 10m -run '^(TestDaemonRecoverAuditDurabilityFailureExit7PreservesCommittedRespawnWording|TestDaemonRecoverHermeticExitContract|TestDaemonRecoverRound4HermeticExitContract)$' ./internal/cli/` |
| F7 ordering | `go test -tags=test_state_path_env -count=1 -timeout 10m -run '^(TestEnsureAliveHeadlessFleet_DetectionWriteCannotBlockRelaunch|TestEnsureAliveHeadlessFleet_DetectionWriteCannotBlockSuppressor|TestEnsureAliveUnknownEscalation_DetectionWriteCannotBlockRecovery)$' ./internal/cli/` |
| F1-F4 protected CLI | `go test -tags=test_state_path_env -count=1 -timeout 10m -run '^(TestObserveGUIExitSignal_CancelsBeforeWaitingOnEmit|TestEnsureAlive_SupervisorDownTickResetsUnknownConfirmationMarker|TestClassifyGUIOwnerVerdict_Matrix)$' ./internal/cli/` |
| F2 protected recovery | `go test -tags=test_state_path_env -count=1 -timeout 10m -run '^TestQueueIdempotentAuditFallbackOutcomeMatrix$' ./internal/daemonrecovery/` |
| Full touched GUI package | `go test -tags=test_state_path_env -count=1 -timeout 10m ./internal/gui/` |
| Full touched API package | `go test -tags=test_state_path_env -count=1 -timeout 10m ./internal/api/` |
| Full touched recovery package | `go test -tags=test_state_path_env -count=1 -timeout 10m ./internal/daemonrecovery/` |
| Full touched CLI package | `go test -tags=test_state_path_env -count=1 -timeout 10m ./internal/cli/` |
| Repository build | `go build -tags=test_state_path_env ./...` |
| Repository vet | `go vet -tags=test_state_path_env ./...` |

Use a unique helper name for every row so output files do not overwrite each
other. Never run `go test ./...`.

## Rollback and residual risk

| Risk | Bound/response |
| --- | --- |
| Lifecycle race | Atomic monotonic transitions; Phase A standalone revert; invalid state fails closed for one tick |
| Retained GUI lease | Suppress only GUI-owner relaunch for the current tick; process exit releases the operating-system flock |
| Unsupported hard-link publication filesystem | Return `FailureAuditDurability` after respawn; retain temp evidence; exit `7`; no fallback overwrite |
| Pending directory accumulates more than one pass | Drain 64 per successful emit; retain all unprocessed carriers; no silent deletion |
| Malformed/corrupt pending carrier wedges replay | Fail current emit and retain carrier for operator diagnosis; no automatic quarantine is admitted in this fix |
| Original audit completes late | Existing flock ordering makes replay observe the original first; exact active plus `.1` comparison prevents a second retained row |
| Row rotates beyond `.1` before replay | Outside the explicit guarantee; no unbounded-history index is introduced |
| Process or host loses unsynced directory metadata | Power-loss durability is outside the accepted guarantee; file data is synced before atomic publication |
| GUI HTTP caller receives the new failure kind | External HTTP mapping is outside the accepted change surface; current redacted internal-error default remains. A distinct public wire code requires separate admission. |
| Deferred diagnostic still blocks process return | Accepted F7 contract is action-before-observability, not bounded return; the callback/suppressor has already executed |

If B or C fails review, revert both as one unit using inverse patches or a new
user-approved local commit; do not use prohibited destructive Git commands.
If A or D fails, revert that phase independently. Do not push any result.

## Final acceptance and commit content

Before commit, the lead runs the Bootstrap checklist with these verified
hypotheses:

1. F5 root cause is loss of the Phase-I lease-release disposition across the
   worker/timeout and `runEnsureAlive` boundary.
2. F6 root cause is an unsettled committed audit whose only carrier is a
   process-local goroutine.
3. F7 root cause is two detection emits ordered before their recovery decision.
4. The change surface is the exact owner set above; unrelated cleanup is
   excluded.
5. Rollback is Phase A standalone, B+C atomic, and D standalone.

Stage only the admitted source/tests, `docs/supervisor-architecture.md`, and
this work item's canonical artifacts/reports. Run the repository publication
safety scan and inspect the staged diff. Create one local commit whose body
names:

- F5: typed lifecycle and all three relaunch paths;
- F6: prepared bytes, process-exit-safe pending handoff, all-mode replay,
  active plus `.1` exact-line deduplication, and exit `7`;
- F7: both action-before-observability sites;
- F1-F4: already fixed by `f150be61`, unchanged and reverified;
- exact mutation evidence and final build/vet/test evidence.

Do not push.

## Recommended role sequence

1. `$backend-engineer` implements Phases A-D and the canonical doc update.
2. `$qa-engineer` executes mutations, narrow guards, full touched packages,
   tagged build, and tagged vet.
3. `$architecture-reviewer` checks owner seams, state lifecycle, lock/resource
   release, exact-once scope, and absence of fix layering.
4. `$lead` reconciles all seven findings, runs publication safety, and creates
   the local commit.

## Gate

**PASS** — every accepted design decision is assigned to an exact symbol,
transition, caller path, persistence/replay step, test, reversible mutation,
safe command, rollback unit, and final gate. No implementation choice remains
open.

## Terms and Abbreviations

- API: application programming interface.
- CAS: compare-and-swap.
- CLI: command-line interface.
- F1-F7: the seven supplied PR #589 findings.
- GUI: graphical user interface.
- JSONL: JavaScript Object Notation Lines, one object per line.
- PID: process identifier.
- SHA-256: 256-bit Secure Hash Algorithm used for content identity.
