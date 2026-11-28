# A/B functional progress — Source6d, 26 November 2026

**Source6d is committed and its clean source identity was received. Its focused default command passed; the focused tagged command failed during test-fixture cleanup. The original batch resources closed, and the batch remains `FAIL`. Overall functional acceptance is not established.** This additive update preserves the earlier failures, physical-receiving results and held invocation.

## Contents

1. [Exact Source and public changes](#exact-source-and-public-changes)
2. [A: liveness creation and calendar checks](#a-liveness-creation-and-calendar-checks)
3. [B: accepted control proof and status snapshot](#b-accepted-control-proof-and-status-snapshot)
4. [Orphan harness cleanup](#orphan-harness-cleanup)
5. [Historical results and the retained HOLD](#historical-results-and-the-retained-hold)
6. [Future coordinator preflight](#future-coordinator-preflight)
7. [Verification status and next acceptance](#verification-status-and-next-acceptance)
8. [Evidence and references](#evidence-and-references)
9. [Terms and abbreviations](#terms-and-abbreviations)

## Exact Source and public changes

**Table 1. Source identity and publication preimages.**

| Item | Exact identity |
| --- | --- |
| Current composed Source commit | `6d4fa7b8f9b86079020af125b030a26350cb5bb0` |
| Source tree | `277dad42f9e9080d66955fa7469fbc68bc8618e4` |
| Parent | `5ed87ff759405efa739229b7307b5c2f9579c8fc` |
| Clean native commit root receipt SHA-256 | `f75c328af2d5d072fc538619d41593d63a8b62e8480ba2660481ae8f1eaa99ea` |
| Public #660 preimage head | `25508f8bfe5950daca3df1b0faf338d77913640a` |
| Public #664 preimage head | `0e8bec723af3f4a52f158e29d2447836ae887374` |

The [Source6d capsule](../composed-functional-source-20261126/source6d-363b/README.md) binds the composed source separately. [evidence.json](evidence.json) records the public file preimages/postimages and the limits of this progress record. Clean commit receiving proves source identity and source-state readback; it does not prove successful execution of the changed program.

The public #664 change contains five files: [supervisor_launch_windows.go](../../../../internal/cli/supervisor_launch_windows.go), [supervisor_launch_windows_test.go](../../../../internal/cli/supervisor_launch_windows_test.go), [supervise_status.go](../../../../internal/cli/supervise_status.go), [supervise_status_snapshot_test.go](../../../../internal/cli/supervise_status_snapshot_test.go), and [supervisor_controller_orphan_reap_test.go](../../../../internal/cli/supervisor_controller_orphan_reap_test.go).

Source6d also contains a private-composition expectation correction adding the already emitted `pid_generation: 1` field to an exact status-map assertion. That correction is not included in the public #664 delta: its public status preimage does not emit that field. The public status producer retains that boundary. The public snapshot-test file retains its existing CRLF line endings; its Go text matches the reviewed LF candidate.

## A: liveness creation and calendar checks

The corresponding three-file change in existing PR #660 makes the liveness task owner create only after task-not-found with zero exported bytes, through the existing create-if-absent capability. Matching definitions remain unchanged. Drift, corruption, conflict and uncertain post-create results preserve occupants instead of forcing replacement or deletion. The typed calendar comparator now checks `StartBoundary` and `Enabled` with field-specific diagnostics.

The tests adapt the existing liveness/setup fixtures to those outcomes, including absence with bytes, a backend without the creator capability, create errors whose diagnostic readback matches, post-create readback errors and non-destructive restoration. Explicit uninstall remains its separate operation. This advances the identified liveness gap; it is not acceptance of every legacy cleanup, port-stop, dry-run, autostart, refresh or autorun-OFF scenario.

## B: accepted control proof and status snapshot

### Abort reuses the accepted original binding

`ownedSupervisorLaunch.Abort` previously repeated control observation within its cleanup budget, including process-role discovery. The change uses the already accepted private proof and validates its issuer, original creator object, process identity, peer and expected dependencies. It then uses the same strict peer communication and original wait/namespace-settlement path. Original deadlines remain unchanged.

The existing stalled-creator test adds an accepted-control case with a foreign current sidecar. Its fake observer refuses re-observation, and its strict sender checks the exact original operands. Concurrent Abort calls still exercise one accepted exit request and the original wait. That synthetic fixture does not prove native supervisor admission; the native happy case remains a separate acceptance gate.

### Status loads one needed snapshot before process observation

`supervisorStatusDaemons` loads the port-owner snapshot only when a running row or pending-stop diagnostic needs it. The request-local loader memoizes both data and error. Idle and stopped requests avoid the query; running rows and pending-stop diagnostics share one result.

The first running, non-pending row loads the snapshot before its potentially blocking `PIDStatus` call. This preserves the ordering exercised by the concurrent-request test: a later request cannot supply its newer port map to the earlier blocked request. It does not make all tracker and port changes atomic. Identity checks, RAM projection, error handling, the nil-port-owner fallback and existing coalescer policy remain unchanged.

The added snapshot tests cover inactive rows with direct/coalesced probes and a mixed pending-stop/running request, including the preserved diagnostic owner. Existing concurrent-request assertions remain intact.

## Orphan harness cleanup

The test harness starts a real event loop whose synchronous handler can still use event-log and state files after cancellation is requested. Its previous cleanup only cancelled the context before temporary-directory removal; it did not join that original loop.

The original loop goroutine now closes a completion channel when it returns. Cleanup cancels and waits for that channel before the earlier event-log and temporary-directory callbacks run. At the unchanged five-second barrier failure, a bounded 1 MiB snapshot of the existing goroutine stacks is written before the unchanged panic, making the original handler's location available before cleanup joins it.

This corrects the proven cleanup ordering defect. The earlier handler timeout's cause remains **`UNDETERMINED`**. A handler blocked in an operation that does not observe cancellation can still delay the join until the existing outer test timeout intervenes. No new observer, longer barrier timeout or production-controller change is introduced.

## Historical results and the retained HOLD

**Table 2. Distinct historical outcomes.**

| Scope | Original result | Separate receiving / meaning |
| --- | --- | --- |
| Native happy qualification on Sourcee66 | `FAIL` | Later control/status changes require new qualification. |
| e66 run-return negative probe | Original command `FAIL` | Separate physical receipt `CLOSED`; original qualification remains failed. |
| e66 stdout-reader negative probe | Original command `FAIL`; original native result retains `UNKNOWN` | Separate physical receipt `CLOSED`; no promotion of the frozen native result. |
| Earlier focused orphan test | Barrier timeout `FAIL` | Initial handler cause remains `UNDETERMINED`; cleanup ordering was separately corrected. The original run retains its own source binding. |
| Source5ed focused status tests | Existing exact-map and concurrent-request assertions failed | Source6d contains the identified expectation/order corrections; its received results are in Tables 4 and 5. |
| Later Source5ed negative-check batch | Batch `FAIL/HOLD` | Three preceding expected-negative checks and restoration steps were individually received. The final verification did not complete acceptance; separate physical receiving remains `UNKNOWN`. |

The last batch reused a verification step name, so its stdout/stderr capture destinations already existed. The original reader workers failed exclusive capture creation and retained that error. The final read-only verifier was reaped, its native process scope closed, and the original reader workers joined with their pipe-close calls returned. Those facts did not establish EOF or capture-file-close facts: the capture files had not been acquired by those workers. The existing receiver therefore kept `UNKNOWN` and `new_commands_permitted=false`.

The held coordinator has no supported release branch after its single unsuccessful physical-receiving attempt: it retains the same owner and waits. No capture read, retry, termination, reclassification or substitute release is claimed. The final verifier's exit status and the earlier successful source restoration are not a successful final batch verification.

The new Source was prepared independently from the previously committed immutable parent in an isolated worktree. Its admitted semantic corrections do not consume the held captures or settle the old invocation. The HOLD remains scoped to its original run and dependent resources; new Source verification requires its own original owners and fresh evidence.

## Future coordinator preflight

The separately reviewed future ordinary coordinator checks capture-destination uniqueness before creating a controller or invoking the guard. It includes the implicit `head` command, constructs stdout/stderr destinations, normalizes Windows absolute paths and case, and rejects duplicate destinations or empty/non-string names. It changes no owner or receiver semantics and does not alter the running held coordinator.

The [seven-line preflight patch](capture-preflight.patch) is a standalone helper delta. Its exact before/after source digests and pure-check results are in [evidence.json](evidence.json). It is not a new recovery API for the held owner.

**Table 3. Pure checks of the actual inserted syntax tree.**

| Synthetic names | Result |
| --- | --- |
| `one`, `two` | Accept |
| Repeated `verify-positive` | Reject |
| `HEAD` | Reject collision with implicit `head` |
| `One`, `one` | Reject case-equivalent destinations |
| Empty name | Reject |
| `one/../head` | Reject normalized collision with implicit `head` |

The author and independent reviewer exercised the inserted block with the Windows path model, without target-program imports or native processes. A syntax-tree comparison found no changes outside the added block. These checks establish the listed uniqueness behavior, not general path confinement or all Windows filesystem alias cases.

## Verification status and next acceptance

**Table 4. Exact Source6d status at this evidence boundary.**

| Check | Status |
| --- | --- |
| Native source commit/tree and clean readback | Received; receipt digest in Table 1 |
| Public preimage matching and exact patch preparation | Completed as artifact checks |
| Own-diff and independent static review of the changed contracts | Completed; no blocking finding in the reviewed deltas |
| Source6d default focused status command | `PASS`; original command/resources closed |
| Source6d tagged focused liveness/control/status command | `FAIL`; API package passed, CLI package failed during fixture cleanup; original command/resources closed |
| Dedicated Source6d orphan regression | `NOT_RUN` |
| `go build ./...` / `go vet ./...` on Source6d | `NOT_RUN` |
| Source6d native happy qualification | `NOT_RUN` |
| `go test -count=1 -timeout 5m ./...` on Source6d | `NOT_RUN` |
| `go test -tags=test_state_path_env -count=1 -timeout 5m ./internal/api/ ./internal/cli/` on Source6d | `NOT_RUN` |

**Table 5. Received Source6d focused commands. Counts distinguish top-level passed tests from all passed tests including subtests.**

| Command | Exit | Passed top-level / including subtests | Other test results | Command elapsed seconds |
| --- | ---: | ---: | --- | ---: |
| Default focused status | 0 | 40 / 88 | No skip/failure reported | 152.53037440002663 |
| Tagged focused liveness/control/status | 1 | 151 / 315 | One top-level skip and one top-level failure | 156.57040990004316 |

The tagged API package passed and the CLI package failed. The single failed test was `TestSupervisorController_StaleLivenessDisownDroppedByGeneration`; its retained diagnostic reports `TempDir RemoveAll` cleanup with a nonempty directory. This record does not claim that its cause was fixed in Source6d. The batch receipt SHA-256 is `fc4e69767c640a4584e914213fbea35731d992e67975ca05c5a8bcfd076a700a`. The root received all three original commands and six readers closed, with a parent duration of 309.6139396 seconds. This failed batch has no new operational HOLD. The older held negative-check batch in Table 2 remains separate and unchanged.

New results belong in an additive exact-source evidence update after their original execution and resource settlement are received. Static review, source commit receiving and the focused results do not enable the full runtime adapter or substitute for actual P1/P2 Product bindings, native control admission, the mandatory full commands or the wider A functional criteria. Calendar normalization and the unresolved initial orphan timeout remain explicit limits. Any later cleanup fix and successor source are outside this immutable Source6d record.

## Evidence and references

[evidence.json](evidence.json) contains the exact source/publication/file pins, review provenance, preserved outcomes and `NOT_RUN` ledger. Runtime statements about the later held batch are attributed to the root's safe-metadata receiving; failed raw captures are excluded. Machine paths, account names and actual process identifiers are not published.

1. [Source6d capsule](../composed-functional-source-20261126/source6d-363b/README.md), assembled separately under the existing Source owner.
2. [Earlier exact Sourcee66 composition](https://github.com/applicate2628/mcp-local-hub/blob/10a68908390128a7df9e61193c89bac106661506/docs/modernization/verification/composed-functional-source-20261126/README.md).
3. [Exact e66 native negative-probe report](history/e66-negative-probes/README.md) and [evidence](history/e66-negative-probes/evidence.json), preserved without alteration. The [earlier receiver-development report](https://github.com/applicate2628/mcp-local-hub/blob/8b1d27290812717736691fc9745e0da9509da9f6/docs/modernization/verification/failed-owner-receiving-20261126/README.md) remains separate; it predates these native probes.
4. Existing [PR #660](https://github.com/applicate2628/mcp-local-hub/pull/660), [PR #664](https://github.com/applicate2628/mcp-local-hub/pull/664), and requested composition component [PR #669](https://github.com/applicate2628/mcp-local-hub/pull/669).
5. The five changed source files linked above and [machine-readable evidence](evidence.json).

## Terms and abbreviations

- **Source:** exact committed program source revision.
- **PR:** pull request.
- **PID:** process identifier; accepted identity also binds the process generation.
- **RAM:** resident memory reported by the existing process observation.
- **EOF:** end of file, observed by the original output-pipe reader.
- **HOLD:** retained owner state whose required completion evidence has not been accepted.
- **P1/P2:** separate native-test and command-test Product roles, each requiring its own build identity and fixture.
- **Static review:** source inspection without executing the target program.
