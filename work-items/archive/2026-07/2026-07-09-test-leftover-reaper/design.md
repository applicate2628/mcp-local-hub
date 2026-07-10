# Test-Leftover Reaper — Preview/Diagnostics-Only v1 Design

Date: 2026-07-10
Status: accepted and DELIVERED for v1. Version 1 is preview/diagnostics only. Destructive apply is deferred to v2.
Delivered: v1 shipped in PR #527, merged to master as `436e4f58` (bot PASS, 2 rounds). The implementation is `internal/api/test_leftover_preview.go` (evidence/classification) with the `internal/api/test_leftover_preview_test.go` vocabulary tests; the enum/vocabulary tables below have been reconciled to that merged code const set.

This is the authoritative design for the test-leftover reaper work item. The accepted durable decision is `work-items/decisions/2026-07-10-test-leftover-reaper-preview-only-v1.md`. The three adversarial re-gates are recorded in `security-review.md`: the original gate found nine issues including three P1 live-kill paths, round 2 found one new P1, and round 3 found zero P1 but confirmed one P2 ordering flaw and one P3 ancestry flaw. The destructive predicate converged only by refusing standalone `supervise` processes, which are the main observed leftover population.

## Security Posture And Cited Facts

The ordinary `CleanupOrphans` and `AggressiveCleanup` paths intentionally spare mcphub-family binaries. The own-binary boundary is defined at `internal/api/cleanup.go:32-60` and enforced during default and aggressive candidate handling at `internal/api/cleanup.go:1287-1298` and `internal/api/cleanup.go:1473-1475`. Version 1 must not weaken either path.

The committed GUI integration test proves the go-build-cache leftover class: it invokes `go run` with `test_state_path_env`, the resulting mcphub grandchild can outlive `Cmd.Wait`, and killing the wrapper can leave it alive (`internal/cli/gui_integration_test.go:60-83` and `internal/cli/gui_integration_test.go:98-124`). A GUI spawns `supervise` from its resolved executable with inherited environment (`internal/cli/gui_supervisor_owner.go:128-150` and `internal/cli/gui_supervisor_owner.go:888-900`), deliberately detaches it (`internal/cli/gui_supervisor_owner_windows.go:10-31`), and may add breakaway-from-job behavior (`internal/cli/supervisor_spawn_breakaway_windows.go:12-49`). A restarted GUI may adopt an existing supervisor (`internal/cli/gui_supervisor_owner.go:20-32`, `internal/cli/gui_supervisor_owner.go:109-110`, and `internal/cli/gui.go:674-698`).

The 2026-07-09 field incident was an already-standalone `mcphub-reliability-*.exe supervise` process. A live, adopted supervisor can have the same dead recorded parent process identifier (PPID), image, argument shape, build tag, and redirected state as a true leftover. Version 1 therefore lists standalone `supervise` candidates but never classifies them as safe to kill. Each receives the operator note `manual-reap-only: verify identity out-of-band before killing`.

Version 1 is an evidence command, not an authorization command. It enumerates, classifies, and renders process metadata. It has no apply flag, confirm token, kill owner, tree-reap coordinator, or process-termination step.

## Decision And Scope

Version 1 ships one command:

```text
mcphub cleanup test-leftovers [--min-age-sec <seconds>] [--temp-root <path>]
```

The command is CLI-only and always runs in preview mode. `--min-age-sec` changes display classification only. `--temp-root` adds a strict-canonical classification scope for otherwise recognized reliability-family images; it does not authorize action. `--apply` and `--confirm-token` are not v1 options and must be rejected as unsupported input. No environment variable or hidden configuration may enable apply behavior.

The command stops after rendering the diagnostic result. If an operator chooses to remove a reported process, that action occurs outside this command and outside the v1 contract, after out-of-band verification of executable image path, argument vector, and `StartedAt`.

Decision back-references:

- `work-items/decisions/2026-07-10-test-leftover-reaper-preview-only-v1.md` owns the preview-only v1 scope and v2 deferral.
- `work-items/decisions/2026-07-09-test-leftover-reaper-peb-env-proof-preview-only.md` remains accepted for any future apply-capable Windows path; its Process Environment Block (PEB) contract is deferred with v2.

## Change-Surface Contract

| Field | Contract |
|---|---|
| Intended change surface | Future v1 implementation: one `mcphub cleanup test-leftovers` preview command and one read-only candidate/evidence path that reuses `runProcessSnapshot` plus `parseProcessRows`. |
|  | Add strict path canonicalization for classification display, on-disk buildinfo inspection where available, and focused preview tests. |
|  | This documentation revision changes only this work item and its decision record. |
| Approved extension seam(s) | `internal/api` owns candidate evidence and classification; `internal/process` owns a strict-error file/directory canonical form. |
|  | `internal/cli` owns flag parsing and policy-aware rendering. The CLI passes typed options downward; lower layers do not reread ambient policy. |
| Protected / must-not-touch surfaces | Existing `CleanupOrphans`, `AggressiveCleanup`, their token semantics, `reapOneOrphan`, `orphanTerminateFn`, daemon recovery, scheduler/ticker behavior, GUI cleanup handlers, and the current own-binary guard. |
|  | The v1 lane must not call, wrap, or route into any termination owner. |
| Declared blast radius | Additive, operator-invoked, read-only CLI/API behavior. No unattended sweep, GUI endpoint, scheduler, default cleanup, aggressive cleanup, state mutation, remote-memory requirement, or process termination. |

## V1 Candidate Enumeration And Evidence Flow

1. `internal/api` calls `runProcessSnapshot` (`internal/api/processes.go:184-235`) and passes the returned census directly to `parseProcessRows` (`internal/api/cleanup.go:1251-1275`). It does not use `parseOrphans` because that path intentionally drops own mcphub basenames at `internal/api/cleanup.go:1287-1298`.
2. The classifier considers rows resembling the known test-leftover families: reliability-temp, gui-e2e, go-build-cache, operator-temp-root, and `supervise` argv associated with those image families. The classifier is deliberately broader than a kill predicate because missing or adverse evidence must remain visible to the operator.
3. Every path used for a branch or protected-path display verdict goes through the strict canonicalization contract. Canonicalization failure keeps the row in output, preserves only the path representation allowed by output policy, and labels the result `path-canonicalization-error`.
4. The evidence collector derives age, parent-liveness verdict, normalized argv shape, and pattern class from the census. It may read the target image on disk with `debug/buildinfo.ReadFile` to report the exact `test_state_path_env` tag finding. A read or parse failure becomes diagnostic evidence; it never suppresses the row.
5. Remote process memory is not required for v1. The accepted implementation scope reports `not-collected-v1` for the environment-override field. If an already-available, cheap, bounded, same-user, read-only evidence source is separately accepted during implementation, it may populate this diagnostic field only; failure remains `unavailable` and cannot change candidate inclusion or authorize action. Implementing the deferred PEB reader is not part of v1.
6. The renderer emits one record per candidate and a top-level snapshot verdict. Rendering is the terminal step.

The parser's `snapErr` remains the single snapshot-completeness owner. The top-level `SnapshotVerdict` is exactly one of `snapshot-complete`, `snapshot-degraded`, or `snapshot-unsupported-platform`. A complete snapshot yields `snapshot-complete`. If `parseProcessRows` returns complete rows plus `snapErr`, v1 may render those rows but must label the run `snapshot-degraded` and must not claim the list is exhaustive. If `runProcessSnapshot` itself returns no usable census, the command returns a visible diagnostic error and no completeness claim. On a non-Windows host with no injected census, the default `wmic`/`powershell` snapshotter is unavailable; v1 returns an empty, non-error no-op preview labeled `snapshot-unsupported-platform` (mirroring `CleanupOrphans` and `ListMatchingProcesses`), never a false "no leftovers" claim.

## V1 Per-Candidate Evidence Contract

| Field | V1 output contract |
|---|---|
| PID | Census process identifier, carried with an `IdentityVerdict` of `identity-available` or, when PID / `StartedAt` / executable path is missing or invalid, `identity-unavailable`; the row is not silently dropped if it can still be identified for display. |
| StartedAt | RFC3339Nano creation time derived through the same `orphanStartedAt` representation used by existing cleanup identity binding (`internal/api/cleanup.go:1717-1725`). Missing data is shown as unavailable. |
| Executable path | Full or redacted according to the established local-output policy. The raw census spelling and strict-canonicalization verdict remain distinguishable; machine-local paths are not copied into tracked docs, tokens, or portable artifacts. |
| Argv shape | A normalized branch-relevant shape: exactly one of `gui`, `gui-e2e`, `reliability-daemon`, `supervise`, or `unrecognized`. Full argv is shown only where the local output policy permits it. |
| Branch / pattern class | One of `reliability-temp`, `gui-e2e`, `go-build-cache`, `operator-temp-root`, `standalone-supervise`, `live-supervise`, `ambiguous-multi-match`, or `unclassified`. A `supervise` argv with a live parent classifies `live-supervise`; with a dead or unproven parent it classifies `standalone-supervise`. This is a display classification, never an allow verdict. |
| Age | Computed age plus two diagnostics: `younger-than-requested-min-age` / `at-or-above-requested-min-age` relative to `--min-age-sec`, and `younger-than-apply-floor` / `at-or-above-apply-floor` relative to the deferred 600-second floor. A zero creation time yields `age-unavailable` for both. No v1 filtering or action depends on any of them. |
| Parent liveness | `parent-alive`, `parent-proven-dead`, or `parent-unproven` from the existing tri-state helper semantics at `internal/api/cleanup.go:1687-1708`. This is evidence, not provenance or authorization. |
| Buildinfo tag | `test-tag-present`, `test-tag-absent`, `unreadable`, `unparsable`, or `not-collected` from the on-disk target image. |
| Environment override | `not-collected-v1` by default; if a separately accepted read-only provider exists, `present` with policy-safe path evidence, `absent`, or `unavailable`. The complete environment is never logged. |
| Hypothetical apply result | `ApplyLifecycle` always carries `apply-deferred-v1`. If collected evidence conclusively fails a deferred v2 gate, `would-refuse` carries exactly one stable label using the vocabulary below; otherwise `would-refuse=not-evaluated-v1`. When the snapshot is degraded, every candidate's `would-refuse` is set to `snapshot-degraded`. |
| Operator note | Every `standalone-supervise` candidate carries `manual-reap-only: verify identity out-of-band before killing`. Other candidates may carry concise remediation tied to their evidence label. |

## V1 Result Verdict Fields

Beyond the per-candidate evidence above, the top-level `TestLeftoverPreview` result and each candidate carry structural verdict fields with fixed value sets:

| Field | Scope | Stable values |
|---|---|---|
| `SnapshotVerdict` | Result | `snapshot-complete`, `snapshot-degraded`, `snapshot-unsupported-platform` |
| `TempRootVerdict` | Result | `not-supplied` (no `--temp-root`), `path-canonical`, `path-canonicalization-error` |
| `ProtectedScopeVerdicts` | Result | Per protected root (`production-state`, `install-path`, `repo-path`): `path-canonical` or `protected-scope-unverified` when that root could not be strict-canonicalized |
| `IdentityVerdict` | Candidate | `identity-available`, `identity-unavailable` |
| `PathVerdict` | Candidate | `path-canonical`, `path-canonicalization-error` |
| `ExecutablePathPolicy` | Candidate | `basename-only` (the local-output policy applied to the rendered executable field) |
| `PathRelations` | Candidate | Zero or more of `operator-temp-root`, `os-temp-root`, `production-state`, `repo-path`, `install-path` (strict-canonical containment hits) |

When any `ProtectedScopeVerdicts` entry is `protected-scope-unverified`, an otherwise-clean candidate's `would-refuse` fails closed to `protected-scope-unverified` rather than the `not-evaluated-v1` fallthrough, because a protected root that cannot be canonicalized cannot prove the candidate is outside it.

## Test-Leftover Signature (Discriminator Table)

| Discriminator | Cited evidence | V1 preview classification | Deferred v2 apply gate |
|---|---|---|---|
| Snapshot completeness | `runProcessSnapshot` is at `internal/api/processes.go:184-235`. | Render completeness; degraded rows remain diagnostics. | Fail apply closed as `snapshot-degraded`. |
|  | `parseProcessRows` owns `snapErr` at `internal/api/cleanup.go:1251-1275`. | Do not claim a degraded snapshot is exhaustive. |  |
|  | Existing callers propagate `snapErr` at `internal/api/cleanup.go:1284`, `internal/api/cleanup.go:1420`, `internal/api/cleanup.go:1469`, `internal/api/cleanup.go:1557`, and `internal/api/cleanup.go:1623-1630`. |  |  |
| Parent liveness | `orphanParentProvenDead` is tri-state at `internal/api/cleanup.go:1687-1708`. | Display alive / proven-dead / unproven. | Mandatory negative guard for top-level candidates. |
|  |  | Standalone `supervise` remains listed regardless. | Never sufficient provenance for `supervise`. |
| Target buildinfo tag | Reliability tests use `test_state_path_env` at `internal/cli/daemon_reliability_test.go:67-82`. | Report the on-disk finding when cheap and available. | Exact tag is mandatory. |
|  | GUI e2e setup uses the tag at `internal/gui/e2e/global-setup.ts:74-99`. |  | Failure is `not-test-tagged`. |
| Environment override | The test variant consumes the override at `internal/api/state_paths_envfallback.go:53-75`. | `not-collected-v1` by default; optional evidence is non-authorizing. | The deferred PEB reader and `envProofGate` are mandatory. |
|  | The census has no environment field at `internal/api/processes.go:184-235`. |  |  |
| Production state | `daemonStateDirReadOnly` owns the production root at `internal/api/state_paths_prod.go:53-69`. | Display strict-canonical relation evidence or `path-canonicalization-error`. | Equality or descendants refuse `production-state`. |
|  | `rootContains` owns equality/containment at `internal/api/lsp_trusted_roots.go:214-254`. | Do not hide the row. |  |
| Reliability-temp image | Reliability tests build `mcphub-reliability-*` at `internal/cli/daemon_reliability_test.go:49-82`. | Display family and argv-shape match, including `supervise`. | Requires exact family, path, argv, and common gates. |
|  |  |  | Standalone `supervise` is refused. |
| GUI e2e image | Global setup writes the fixture at `internal/gui/e2e/global-setup.ts:13-40`. | Display fixture equality, GUI argv shape, and cheap marker evidence. | Requires exact fixture, GUI argv, and both markers. |
|  | It tags the fixture at `internal/gui/e2e/global-setup.ts:74-99`. |  |  |
|  |  |  | No marker admits `supervise`. |
| Go-build-cache image | `go run` leaves a GUI grandchild in `internal/cli/gui_integration_test.go:98-124`. | Display strict component containment and GUI or `supervise` argv. | Requires exact branch gates. |
|  |  |  | `supervise` requires a corrected v2 tree contract. |
| Supervise topology | GUI spawn is at `internal/cli/gui_supervisor_owner.go:128-150`; adoption is at `internal/cli/gui.go:674-698`. | A `supervise` argv with a live parent lists `live-supervise`; with a dead or unproven parent it lists `standalone-supervise` plus the manual-reap-only note. Both are hints only. | Standalone rows refuse `supervise-not-tree-reachable`. |
|  |  | Never call it safe or tree-confirmed. | Tree authorization is blocked by round-3 P2/P3. |
| Operator temp root | No committed source authorizes the considered `f1-cli-verify` prefix. | A strict-canonical root scopes display only. | Root must be token-bound and outside production. |
|  |  | It cannot broaden the basename family. |  |
| Installed and repo paths | Install locations are at `internal/api/install.go:39-64` and `internal/cli/setup.go:99-115`. | Display the strict-canonical relation or failure. | Refuse `install-path` / `repo-path` before admission. |
|  | Own binaries are protected at `internal/api/cleanup.go:32-60`. |  |  |
| Age | Existing cleanup floor is 600 seconds at `internal/api/cleanup.go:141-145`. | Display age relative to 600 seconds; do not filter or act. | Effective apply age can never be below 600 seconds. |
| Identity | Existing identity binding uses PID, executable path, `StartedAt`, and live command line at `internal/api/cleanup.go:1749-1786`. | Display available fields and evidence gaps. | Fresh `{PID, StartedAt}` and command-line revalidation are mandatory. |

## Fail-Closed Predicate: V1 Display Versus Deferred V2 Authorization

Version 1 deliberately has no positive authorization predicate. No record may be named `allowed`, `apply-eligible`, `safe-to-kill`, or equivalent. The only v1 inclusion question is whether a census row resembles an admitted diagnostic pattern strongly enough to help an operator investigate it.

The v1 display predicate is:

1. reuse `runProcessSnapshot` plus `parseProcessRows`;
2. retain every row matching a known image family, a known branch path, a supplied display-only temp root, or `supervise` argv associated with those families;
3. evaluate each available evidence field independently;
4. keep the candidate visible when an evidence read, strict canonicalization, parent probe, buildinfo read, or classification fails;
5. render `apply-deferred-v1` plus a conclusive hypothetical `would-refuse` label when one is known; and
6. stop after rendering.

The deferred v2 predicate is separate: every common gate, exactly one positive branch, every negative guard, scope binding, identity proof, audit-intent requirement, and corrected tree rule must pass before a kill owner can be reached. Those contracts appear only under `Deferred: Destructive Apply (v2)`.

## V1 Diagnostic And Refusal Vocabulary

The refusal vocabulary is reused in v1 only as diagnostic labels describing what a hypothetical v2 apply would refuse. A v1 label has no authorizing complement: absence of a known refusal never means permission.

The rows split by whether baseline v1 code actually emits the label. The "emitted" rows are the exact `would-refuse` and lifecycle set produced by `testLeftoverWouldRefuse` plus the bulk degraded-snapshot assignment; the reserved row is the shared vocabulary a hypothetical v2 apply would emit but for which baseline v1 has no evidence path.

| Class | Stable values | V1 meaning |
|---|---|---|
| V1 lifecycle (emitted) | `apply-deferred-v1`, `not-evaluated-v1`, `manual-reap-only` | No apply exists; `not-evaluated-v1` is the `would-refuse` fallthrough when no conclusive refusal is known; `manual-reap-only` is the standalone-`supervise` operator note. |
| Topology / branch (emitted) | `supervise-not-tree-reachable`, `argv-not-in-branch`, `requires-explicit-temp-root`, `ambiguous-family-classification` | `would-refuse` labels v1 emits when current evidence conclusively establishes the condition. `ambiguous-family-classification` fails closed when one path matched more than one preview family. |
| Provenance (emitted) | `not-test-tagged`, `guard-evaluation-error` | `would-refuse` labels from the on-disk buildinfo finding (`test-tag-absent` → `not-test-tagged`; `unreadable` / `unparsable` / `not-collected` → `guard-evaluation-error`). `not-collected-v1` is environment evidence status, not an inferred refusal. |
| Safety guards (emitted) | `install-path`, `repo-path`, `production-state`, `path-canonicalization-error`, `parent-alive-or-unproven`, `min-age-below-apply-floor`, `protected-scope-unverified`, `snapshot-degraded`, `identity-unavailable` | Diagnostic `would-refuse` labels; the candidate remains visible. `snapshot-degraded` is bulk-assigned to every candidate on a degraded snapshot. |
| Reserved for deferred v2 (NOT emitted by baseline v1) | `basename-not-in-branch`, `e2e-markers-absent`, `env-read-error`, `env-override-absent`, `unsupported-arch`, `command-line-mismatch`, `token-mismatch`, `identity-filter-excludes-recycled-pid`, `audit-intent-unavailable`, `reaped`, `refused(<exact reason>)`, `terminate-unconfirmed` | Part of the shared refusal/outcome vocabulary, but baseline v1 has no env-read, e2e-marker, command-line, token, audit, or termination evidence path, so it never emits them. See `Deferred Refusal And Outcome Vocabulary`. V1 must never emit `reaped` or `terminate-unconfirmed`. |

## V1 Components, Ownership, And Dependency Direction

| Component | Single owner | V1 contract |
|---|---|---|
| Snapshot adapter | Existing `runProcessSnapshot` plus `parseProcessRows` | Produces typed rows and the sole completeness verdict. No parallel parser is introduced. |
| Candidate evidence classifier | `internal/api` | Computes pattern class and independent diagnostic findings from typed rows and typed options. It returns data; it has no process handle, state mutation, or termination dependency. |
| Strict path canonicalizer | `internal/process` | Returns one strict canonical form or an explicit error for files/directories. In v1 the result is used only for classification display and safe relation evidence. |
| On-disk buildinfo reader | `internal/api` adapter around `debug/buildinfo.ReadFile` | Reads only the target image file and returns a typed finding. |
| Environment evidence provider | Not required in baseline v1 | Baseline output is `not-collected-v1`. Any separately accepted provider is read-only, bounded, non-authorizing, and replaceable without classifier changes. |
| CLI adapter and renderer | `internal/cli` | Parses flags once, calls the evidence API, applies redaction/full-path policy, renders records, and exits. |

Dependencies point from the CLI adapter to the API evidence contract and from the API to read-only process/path utilities. No dependency points from the v1 evidence path into `reapOneOrphan`, `orphanTerminateFn`, the aggressive token owner, or a future apply coordinator.

## V1 Failure Modes And Observability

- Snapshot acquisition failure is visible and nonzero; no empty-success result claims that no leftovers exist.
- A degraded parse is labeled `snapshot-degraded`; any complete rows remain visible without an exhaustive claim.
- Per-candidate evidence failure is attached to that candidate and does not suppress it.
- Strict canonicalization failure is reported without treating an unresolved path as outside a protected root.
- Output applies the established redacted-or-full-per-policy path handling. The complete target environment is never read or logged by baseline v1.
- The command emits no process-mutation intent or outcome event because it performs no mutation. Normal command diagnostics are sufficient; adding a destructive audit namespace in v1 would falsely imply an apply lifecycle.

## Manual Operator Handoff (Outside V1)

For a candidate the operator decides to remove, the safe procedure remains the one used in the 2026-07-09 incident:

1. verify the live image path out of band;
2. verify the live argv, including the exact `supervise` shape;
3. verify `StartedAt` still matches the previewed identity; and
4. use an operating-system process tool outside `mcphub cleanup test-leftovers`.

The v1 command does not perform, script, or confirm step 4.

## Alternatives Considered

| Alternative | Decision | Trade-off |
|---|---|---|
| Ship the previously designed destructive apply as v1. | Deferred. | It safely handles only a fraction of observed leftovers, refuses the main standalone-`supervise` class, and still has confirmed P2/P3 blockers. |
| Hide standalone `supervise` because it cannot be auto-authorized. | Rejected. | It would hide the actual field population and defeat the diagnostic value of v1. |
| Fold mcphub-family handling into `CleanupOrphans` or `AggressiveCleanup`. | Rejected. | It would weaken the existing own-binary boundary and broaden destructive behavior. |
| Ship a separate preview/diagnostics command and leave removal manual. | Chosen. | It provides actionable evidence for the real leftover class with no automated live-kill risk and preserves a future v2 contract. |

## V1 Architectural Claims

1. `{ guarantee: mcphub cleanup test-leftovers v1 never terminates or mutates a process; single-owner: the preview-only CLI/API boundary defined by decision 2026-07-10-test-leftover-reaper-preview-only-v1; enforcement-probe: CLI tests reject --apply and --confirm-token, evidence-path tests inject a fail-on-call termination seam, and repository review finds no v1 call edge to reapOneOrphan or orphanTerminateFn. }`
2. `{ guarantee: Existing CleanupOrphans and AggressiveCleanup behavior remains unchanged; single-owner: the separate test-leftover evidence API seam; enforcement-probe: the implementation diff leaves existing handlers and own-binary checks at internal/api/cleanup.go:1287-1298 and internal/api/cleanup.go:1473-1475 unchanged, and existing cleanup tests pass. }`
3. `{ guarantee: Standalone supervise candidates are visible and never described as auto-authorized; single-owner: the candidate evidence classifier; enforcement-probe: a 2026-07-09-shaped mcphub-reliability-*.exe supervise fixture renders standalone-supervise, apply-deferred-v1, would-refuse=supervise-not-tree-reachable, and the exact manual-reap-only note. }`
4. `{ guarantee: Snapshot completeness has one owner; single-owner: parseProcessRows and its snapErr return at internal/api/cleanup.go:1251-1275; enforcement-probe: a truncated census renders snapshot-degraded and never claims an exhaustive empty or complete result. }`
5. `{ guarantee: Path aliases cannot silently change display classification; single-owner: strict path canonicalizer in internal/process plus rootContains in internal/api; enforcement-probe: case, trailing-separator, junction, 8.3, UNC, and prefix-collision fixtures either converge to the same class or render path-canonicalization-error. }`
6. `{ guarantee: Missing diagnostic evidence keeps a candidate visible and never becomes implicit permission; single-owner: candidate evidence classifier; enforcement-probe: buildinfo read failure, parent-probe error, path error, and not-collected-v1 environment fixtures each remain in output with apply-deferred-v1. }`
7. `{ guarantee: Baseline v1 neither requires nor implements cross-process memory reading; single-owner: the v1 evidence-provider registry; enforcement-probe: the accepted v1 implementation has no PEB reader dependency and emits not-collected-v1 for environment override. }`
8. `{ guarantee: No unattended or GUI surface can reach the v1 lane; single-owner: the CLI composition root; enforcement-probe: route review finds only the explicit cleanup subcommand and no ticker, GUI handler, recovery, or background-watcher registration. }`

## V1 Test Plan

| ID | Scenario | Required assertion |
|---|---|---|
| V1-T1 | Complete synthetic census with reliability-temp, gui-e2e, go-build-cache, operator-root, and unrelated rows. | Every recognized candidate renders once with PID, `StartedAt`, path-policy result, argv shape, pattern class, age, and parent verdict. |
|  |  | Evidence fields and `apply-deferred-v1` render; unrelated rows do not appear. |
| V1-T2 | Standalone `mcphub-reliability-*.exe supervise` matching the 2026-07-09 incident shape. | It renders `standalone-supervise`, `would-refuse=supervise-not-tree-reachable`, and `manual-reap-only: verify identity out-of-band before killing`. |
| V1-T3 | Candidate with missing StartedAt, buildinfo failure, parent-probe failure, or environment `not-collected-v1`. | The row remains visible with the exact evidence status; no missing field becomes an allow verdict. |
| V1-T4 | `parseProcessRows` returns rows plus `snapErr`, and `runProcessSnapshot` returns a total acquisition error. | The first renders rows plus `snapshot-degraded` without an exhaustive claim; the second is visible and nonzero. |
| V1-T5 | Production/install/repo aliases, broken reparse points, prefix collisions, and a supplied operator temp root. | Strict canonical results drive display only; failures render `path-canonicalization-error`. |
|  |  | The temp root does not broaden basename families. |
| V1-T6 | Tagged, untagged, unreadable, and unparsable on-disk images. | Buildinfo findings are exact and non-authorizing. |
| V1-T7 | Redacted and full local-output policies. | Executable paths and argv obey policy; portable output contains no unintended machine-local path. |
| V1-T8 | `--apply`, `--confirm-token`, hidden environment toggles, and direct evidence-API invocation. | Destructive flags are rejected, no hidden toggle exists, and a fail-on-call `orphanTerminateFn` seam remains untouched. |
| V1-T9 | Existing default and aggressive cleanup suites. | Their behavior and own-binary protections remain unchanged. |
| V1-T10 | Baseline environment evidence and any separately accepted optional read-only provider failure. | Baseline is `not-collected-v1`; optional failure is `unavailable`; both remain visible and never affect inclusion. |

## V1 Gate

Gate decision: **PASS**. The preview/diagnostics-only v1 design is accepted for implementation. It provides evidence for the observed standalone-`supervise` population while keeping every destructive mechanism out of the v1 command.

## Deferred: Destructive Apply (v2)

Everything in this section is preserved as the specification for a possible future apply lane. It is not part of v1, must not be implemented behind v1 flags or hidden configuration, and cannot be wired to a kill owner until all three admission conditions are met:

1. round-3 P2 is resolved with deterministic proof that the GUI's armed respawn loop cannot create a replacement `supervise` between descendant reap, confirm-gone, and GUI reap;
2. round-3 P3 is resolved with PID-recycle-safe ancestry whose every edge is identity- and time-bound, not a bare PPID chain; and
3. a demonstrated value case shows that the safely automatable subset justifies the coordination and maintenance cost despite excluding standalone `supervise`.

Deferred apply syntax:

```text
mcphub cleanup test-leftovers --confirm-token <token> [--min-age-sec <seconds>] [--temp-root <path>]
```

The deferred CLI parses all flags once into typed options. Lower layers do not reread ambient policy. The effective apply age is `max(requestedMinAgeSec, 600)`. A preview temp root and its strict-canonical value must be bound into the confirm token; omission, substitution, or scope drift is `token-mismatch` with zero termination calls.

### Deferred Components And Ownership

| Component | Owns | Deferred contract |
|---|---|---|
| `parseProcessRows` | Snapshot-completeness detection. | Apply calls `runProcessSnapshot` plus `parseProcessRows`; any `snapErr` refuses `snapshot-degraded` before action. |
| Destructive test-leftover predicate | Candidate authorization and exclusive branch. | Returns allow or `refused(reason)`; unavailable/throwing guards become `guard-evaluation-error`. |
| `parentDeathGate` | Top-level parent-liveness proof. | Accepts only `orphanParentProvenDead`; it never authorizes standalone `supervise`. |
| `buildInfoTagGate` | Test-tag provenance from the target image. | Requires exact `test_state_path_env` from `debug/buildinfo.ReadFile(ExecutablePath)`. |
| PEB environment reader | Exact remote-byte acquisition and validated UTF-16 environment block. | Apply-capable only on `windows && amd64` under the reader contract below. |
| `envProofGate` | Target environment and redirected-state proof. | Only a validated map with a non-production override may progress. Absence and ambiguity are distinct refusals. |
| Strict path canonicalizer | One strict-error Windows form. | Every destructive path operand must be absolute, resolved, normalized, and case-folded; no unresolved fallback. |
| Production/install/repo guards | Protected-path exclusion. | Use strict canonical forms plus `rootContains`; equality and containment are explicit. |
| Branch classifier | Exclusive top-level admission. | Exactly one of reliability-temp, gui-e2e, operator-temp-root, or go-build-cache; no branch admits standalone `supervise`. |
| `TestLeftoverConfirmToken` | Preview/apply scope and candidate binding. | Single owner in `internal/api`; no CLI or GUI copy derives a parallel token. |
| Tree coordinator | GUI-to-supervise membership, respawn quiescence, PID-recycle-safe ancestry, and ordering. | **BLOCKED:** it must resolve round-3 P2/P3 before the preserved tree-reap contract can be enabled. |
| `reapOneOrphan` | Identity-gated termination. | Sole per-candidate kill owner; no parallel terminate sequence. |
| Local audit writer | Durable intent and outcome evidence. | Writes local owner-DACL'd events; intent must precede every irreversible action. |

### PEB Environment Reader Contract

The deferred reader contract is mandatory and fail-closed. Its public error/ambiguous refusal remains `env-read-error`, while local diagnostics and tests carry one distinct `envReadFailureStage` from: `open-process`, `classify-architecture`, `query-basic-information`, `query-wow64-information`, `validate-layout`, `read-peb`, `read-process-parameters`, `read-environment-metadata`, `validate-environment-metadata`, `read-environment-block`, `reread-environment-metadata`, or `parse-environment`. Unsupported reader/target directions use `unsupported-arch`. No error/ambiguous result may become key absence or a parsed map.

1. **Build and target classification.** The apply-capable reader is compiled only for `windows && amd64`; all other builds are preview/refuse-only. The same target handle is classified with `windows.IsWow64Process2` from repository-pinned `golang.org/x/sys v0.46.0` (`go.mod:16`). A failed or unsupported classification is error/ambiguous and spares.
2. **One correctly authorized handle.** Open exactly one non-inheritable handle with `PROCESS_QUERY_INFORMATION | PROCESS_VM_READ`, not `PROCESS_QUERY_LIMITED_INFORMATION`. Close it on every return. Every `OpenProcess`, `NtQueryInformationProcess`, `IsWow64Process2`, or `ReadProcessMemory` failure, including access denial and target exit, is error/ambiguous with its stage and operating-system error retained locally. The existing GUI reader's mask is at `internal/gui/probe_windows.go:131-138` but its identity opens are a different contract.
3. **Exact remote reads.** One `readExact(handle, remoteAddress, dst)` owns every pointer, metadata, and block read. It succeeds only with no API error and `bytesRead == len(dst)`. Zero bases, integer overflow, and short reads are errors. The current GUI reader does not validate every hop (`internal/gui/probe_windows.go:166-207`), so it is not reused unchanged.
4. **Native amd64 chase and coherence.** Use the pinned `windows.NtQueryInformationProcess` binding with `ProcessBasicInformation` and `windows.PROCESS_BASIC_INFORMATION`. Require success, exact `ReturnLength`, nonzero PEB, and layout assertions `sizeof(PBI)=0x30` / `offsetof(PebBaseAddress)=0x08`. Reader version `x/sys-v0.46.0-amd64` pins PEB.ProcessParameters at `0x20`, Environment at `0x80`, and EnvironmentSize at `0x3f0`. Chase through `readExact`; reject zero/odd/overflowing sizes and values over `maxRemoteEnvironmentBytes = 1 MiB`. Reread Environment pointer and size after the block read and require exact equality. The existing x64 offsets are visible at `internal/gui/probe_windows.go:2-11` and `internal/gui/probe_windows.go:162-181` but do not authorize another architecture by analogy.
5. **WOW64 i386 chase.** Use `ProcessWow64Information -> PEB32 -> 32-bit ProcessParameters -> 32-bit Environment` with dedicated `uint32` wire fields and explicit zero-extension. **ASSUMPTION (UNVERIFIED):** customary offsets ProcessParameters=`0x10`, Environment=`0x48`, and EnvironmentSize=`0x290` lack a published Microsoft environment-field contract. This route cannot authorize a kill until a versioned native-layout source, exact offset assertions, and a Windows i386 helper test compare the exact override—including a value containing `=`—with the helper's `os.LookupEnv` result.
6. **Environment parser.** Decode only an exact, bounded, even-sized UTF-16LE block. Reject odd bytes, incomplete code units, unpaired surrogates, incomplete entries, a missing double-NUL, or non-NUL trailing data. Split ordinary entries at the first `=`, require a nonempty name, preserve later `=` characters, and compare `MCPHUB_STATE_DIR_OVERRIDE` with `strings.EqualFold`. Only a fully validated block may return key absent. Never log the full environment map.

### Deferred Strict Path Canonicalization And Relation Contract

`strictPathCanonicalizer` in `internal/process` is the single future canonicalization owner for every destructive file or directory operand. It makes the path absolute, cleans it, fully resolves reparse points/symlinks, expands 8.3 aliases, normalizes UNC/device spellings and trailing separators, and case-folds on Windows. Missing/broken components, permissions, loops, or incomplete resolution return `path-canonicalization-error` with no best-effort fallback.

The owner is a strict-error variant/refactor, not either existing helper unchanged. `api.CanonicalizeTrustedRoot` falls back after failed resolution at `internal/api/lsp_trusted_roots.go:115-152`, and `process.normalizeWindowsExecutablePath` keeps a cleaned unresolved path when `EvalSymlinks` fails at `internal/process/pid_identity_windows.go:145-161`. Once both operands are strict-canonical, `rootContains(root, candidate)` means equality-or-true-descendant and mutual containment means equality (`internal/api/lsp_trusted_roots.go:214-254`).

`productionStateGuard` rejects override or argv state paths equal to or below production. `installPathGuard` rejects the install target and install-directory containment. `repoPathGuard` rejects repo containment except exact GUI-fixture equality. Branches use the same relations; go-build-cache matches a complete directory component, never a full-path prefix. The temp-root and production checks remain independent and conjunctive.

### Deferred Fail-Closed Apply Predicate

A top-level candidate becomes apply-eligible only when every common gate, exactly one positive branch, and every negative guard pass. A `supervise` row is never top-level apply-eligible. There is no common basename shortcut.

#### Positive Common Gates

1. `parseProcessRows` returns no `snapErr` and the row has PID, PPID, ExecutablePath, CommandLine, and `StartedAt`.
2. Age is at least `effectiveApplyMinAgeSec`, never below 600 seconds.
3. The PEB reader returns a parsed environment only through its exact architecture, handle, read, layout, coherence, and parser checks.
4. `envProofGate` finds `MCPHUB_STATE_DIR_OVERRIDE` and strict-canonicalizes a non-production path.
5. `buildInfoTagGate` reads the target image and finds `test_state_path_env`.
6. A top-level candidate's recorded parent is proven dead; this never admits `supervise`.
7. `productionStateGuard` proves every candidate state path is outside production.
8. The candidate has a fresh identity suitable for `reapOneOrphan` and passes one branch.

#### Positive Branch Gates

The order is gui-e2e, go-build-cache, reliability-temp, then operator-temp-root. Before selection, `supervise` argv refuses `supervise-not-tree-reachable`.

1. **reliability-temp:** strict-canonical OS-temp containment, `mcphub-reliability-*` basename, and committed reliability-daemon argv (`internal/cli/daemon_reliability_test.go:49-82` and `internal/cli/daemon_reliability_test.go:154-176`).
2. **gui-e2e:** exact canonical fixture, `gui --no-browser --no-tray --port 0`, and both `MCPHUB_E2E_SCHEDULER=none` and `MCPHUB_E2E_SUPERVISOR=none` (`internal/gui/e2e/fixtures/hub.ts:76-112` and `internal/gui/e2e/fixtures/seeded-hub.ts:88-104`). Markers authorize only the GUI.
3. **operator-temp-root:** supplied, strict-canonical, non-production-intersecting root; contained image remains a reliability-family binary with committed argv. It never permits arbitrary names, `f1-cli-verify`, or `supervise`.
4. **go-build-cache:** a complete `go-build*` component immediately under OS temp or exact `go-build` under LocalAppData, contained `mcphub.exe` image, and GUI argv only.

#### Mandatory Negative Guards

1. Strict-canonical installed target/directory refuses `install-path`.
2. Strict-canonical repo containment refuses `repo-path` except an independently valid exact gui-e2e fixture.
3. Production equality/containment refuses `production-state`.
4. Top-level parent alive, unknown, missing, or probe error refuses `parent-alive-or-unproven`.
5. Missing/unreadable/unparsable/non-matching buildinfo refuses `not-test-tagged`.
6. Production-intersecting temp roots or production overrides refuse `production-state`.
7. Any strict path failure refuses `path-canonicalization-error`.
8. Snapshot, identity, command-line, architecture, environment, age, topology, or guard failures refuse before `reapOneOrphan`.
9. Token drift and changed `{PID, StartedAt}` refuse `token-mismatch` or `identity-filter-excludes-recycled-pid`.
10. Failed durable intent writes refuse `audit-intent-unavailable`.

### Deferred GUI Tree Rule And Round-3 Blockers

No positive branch admits standalone `supervise`. A live adopted supervisor can have the dead PPID of its original GUI, so parent death is not provenance. An already-orphaned supervisor, including the field-incident shape, stays manual-reap-only.

The deferred target contract binds a `supervise` descendant to a confirmed test GUI, independently applies all non-topology gates, reaps and confirms the descendant, then freshly revalidates and reaps the GUI. That descendant-before-GUI contract is preserved, but it is **not implementable until both blockers below are resolved**:

- **P2 respawn ordering blocker.** A GUI-spawned owner arms `runRespawnLoop` at `internal/cli/gui_supervisor_owner.go:329-335`. Unexpected child exit waits from a one-second base and respawns (`internal/cli/gui_supervisor_owner.go:365-460`; constant at `internal/cli/gui_supervisor_owner.go:231`). Reaping the descendant while the GUI is live can therefore create a replacement before multi-second confirm-gone/re-enumeration finishes; killing the GUI afterward can strand the replacement as the same out-of-scope standalone orphan. V2 needs a single owner that quiesces or stops respawn before descendant action and proves the quiesced state holds through GUI exit. No timing assumption is acceptable.
- **P3 ancestry blocker.** A census PPID is recyclable. A bare current-snapshot chain can connect a child to an unrelated process that later acquired an ancestor PID. V2 must bind every edge to `{PID, StartedAt}`, require temporally possible parent-before-child ordering, include every intermediate identity in token and fresh revalidation, and reject any missing, recycled, contradictory, or changed edge. If snapshot data cannot prove those invariants, a stronger operating-system provenance source is required.

The tree coordinator remains blocked until deterministic tests engineer both race windows. There is no post-GUI orphan chase.

### Deferred Confirm Token And Identity Binding

`TestLeftoverConfirmToken` is an `internal/api`-only deterministic owner, analogous to `AggressiveConfirmToken` at `internal/api/cleanup.go:467-490` and `internal/cli/cleanup_aggressive.go:197-200`, but it is a distinct type because its proof material differs.

Token material contains predicate version, PEB reader-contract version, effective age, strict-canonical temp-root hash or no-root sentinel, and sorted candidate records. Each top-level record binds PID, `StartedAt`, canonical image/override hashes, normalized argv hash, branch, buildinfo result, and parent proof. Any future GUI operation also binds every ancestry-edge `{PID, StartedAt}` identity, path/override/argv hashes, independent gates, confirmed GUI identity, branch, quiesced-respawn proof, and final ordering. Raw paths never enter token display or wire output.

Apply takes a fresh `runProcessSnapshot` / `parseProcessRows` snapshot, reconstructs the typed scope, strictly canonicalizes every operand, recomputes the token, filters the exact expected `{PID, StartedAt}` set through `filterToExpectedIdentities` (`internal/api/cleanup.go:1652-1671`), and re-reads command line, environment, buildinfo, parent/tree evidence, and branch data. A same-PID changed-`StartedAt` target refuses `identity-filter-excludes-recycled-pid`.

### Deferred Audit Events And Failure Transparency

The audit sink is the local owner-DACL'd `supervisor-events.log` selected from the caller's hardened state directory, not the target-controlled override. The canonical leaf is at `internal/api/supervisor_events.go:64-68`. A pre-kill event uses durable `Emit`, never lossy `TryEmit` (`internal/api/supervisor_events.go:253-289`).

Before every `reapOneOrphan` call, apply synchronously emits `test-leftover-cleanup-intent` with identity, local path/argv evidence, branch or tree role, environment/buildinfo proof, parent or corrected ancestry proof, token, contract versions, respawn-quiescence proof, and ordering. Failure is `audit-intent-unavailable` and prevents action. Outcomes are `reaped`, `refused(reason)`, or `terminate-unconfirmed`. Fresh re-enumeration plus identity comparison is mandatory; failure to confirm cannot become success.

Local owner-DACL'd entries may retain full paths and argv. API JSON, CLI transport display, and wire-shaped summaries use hashes. Environment maps are never logged.

### Deferred Refusal And Outcome Vocabulary

`env-read-error` remains the public environment refusal with mandatory local `envReadFailureStage`. `path-canonicalization-error` covers strict path failure. `supervise-not-tree-reachable` covers every standalone `supervise` row. The preserved exact values are:

| Class | Exact values |
|---|---|
| Image / branch / topology | `basename-not-in-branch`, `argv-not-in-branch`, `requires-explicit-temp-root`, `ambiguous-family-classification`, `e2e-markers-absent`, `supervise-not-tree-reachable` |
| Provenance / environment | `not-test-tagged`, `env-read-error`, `env-override-absent`, `unsupported-arch` |
| Safety guards | `install-path`, `repo-path`, `production-state`, `protected-scope-unverified`, `path-canonicalization-error`, `parent-alive-or-unproven`, `min-age-below-apply-floor`, `snapshot-degraded`, `identity-unavailable`, `command-line-mismatch`, `guard-evaluation-error` |
| Apply binding | `token-mismatch`, `identity-filter-excludes-recycled-pid`, `audit-intent-unavailable` |
| Outcomes | `reaped`, `refused(<exact reason>)`, `terminate-unconfirmed` |

`ambiguous-family-classification` and `protected-scope-unverified` are emitted by v1 as diagnostics and are carried forward as mandatory v2 refusals: an ambiguous multi-family branch classification cannot resolve to one exclusive positive branch, and a protected root that cannot be strict-canonicalized cannot prove the candidate is outside it. Both fail closed.

### Deferred V2 Claims

1. `{ guarantee: Environment presence alone can never authorize apply; single-owner: buildInfoTagGate plus envProofGate; enforcement-probe: untagged and malformed/absent environment fixtures refuse before orphanTerminateFn. }`
2. `{ guarantee: Production/install/repo aliases cannot escape protected roots; single-owner: strictPathCanonicalizer plus rootContains and the three path guards; enforcement-probe: case, separator, junction, 8.3, UNC, device, and prefix-collision fixtures refuse exactly. }`
3. `{ guarantee: Preview/apply scope and candidate identity cannot drift; single-owner: TestLeftoverConfirmToken plus filterToExpectedIdentities; enforcement-probe: changed temp root, candidate material, PID, or StartedAt refuses before action. }`
4. `{ guarantee: Every irreversible action has durable local intent evidence first; single-owner: local audit writer; enforcement-probe: injected intent failure produces audit-intent-unavailable and zero orphanTerminateFn calls. }`
5. `{ guarantee: Termination has one kill owner; single-owner: reapOneOrphan; enforcement-probe: call-graph review finds no parallel terminate path. }`
6. `{ guarantee: A truncated process snapshot cannot authorize apply; single-owner: parseProcessRows snapErr; enforcement-probe: partial top-level and tree fixtures refuse snapshot-degraded with zero calls. }`
7. `{ guarantee: No standalone supervise is automatically reaped; single-owner: exclusive branch classifier; enforcement-probe: matching adopted and field-incident standalone fixtures refuse supervise-not-tree-reachable. }`
8. `{ guarantee: A GUI-tree operation cannot re-manufacture an orphan through respawn; single-owner: future tree coordinator and GUI respawn-quiescence contract; enforcement-probe: deterministic P2 race test proves no spawn from before descendant action through confirmed GUI exit. }`
9. `{ guarantee: No recyclable PPID can establish ancestry; single-owner: future PID-recycle-safe ancestry proof; enforcement-probe: deterministic P3 fixtures recycle direct and intermediate ancestor PIDs and always refuse. }`
10. `{ guarantee: No remote byte sequence becomes kill-authorizing without exact architecture, read, coherence, and UTF-16 validation; single-owner: PEB environment reader; enforcement-probe: amd64/WOW64 helpers pass while every short-read, layout, metadata-drift, parse, access, and exit fault refuses. }`

### Deferred V2 Test Plan

All refusal tests satisfy every non-target gate, alter only the guard under test, assert the exact reason, and assert zero `orphanTerminateFn` calls.

| ID | Preserved scenario | Required assertion |
|---|---|---|
| V2-T1 | Positive reliability-temp top-level fixture. | Exclusive branch passes only with tag, override, dead parent, argv, paths, age, identity, token, and audit intent. |
| V2-T2 | Positive gui-e2e GUI fixture. | Both exact markers and GUI argv are mandatory; markers never admit `supervise`. |
| V2-T3 | Matching standalone/adopted `supervise`. | `supervise-not-tree-reachable` and zero calls. |
| V2-T4 | Positive OS-temp and LocalAppData go-build-cache GUI fixtures. | Strict component containment and GUI argv only; no path-prefix or `supervise` admission. |
| V2-T5 | Otherwise-valid candidate with live/unproven parent. | `parent-alive-or-unproven` and zero calls. |
| V2-T6 | Missing each e2e marker independently. | `e2e-markers-absent` and zero calls. |
| V2-T7 | Untagged target with a present override. | `not-test-tagged`, proving environment is not provenance. |
| V2-T8 | Override or argv state path aliases production. | `production-state` and zero calls. |
| V2-T9 | Installed/repo path aliases. | `install-path` / `repo-path` and zero calls. |
| V2-T10 | Valid key absence and malformed UTF-16 environment blocks. | Only valid absence is `env-override-absent`; malformed blocks are staged `env-read-error`. |
| V2-T11 | Requested age below 600 seconds. | Effective apply floor remains 600 and younger candidates refuse. |
| V2-T12 | Preview temp root omitted/substituted on apply. | `token-mismatch` and zero calls. |
| V2-T13 | Candidate material drifts between preview and apply. | `token-mismatch` and zero calls. |
| V2-T14 | Same PID with changed `StartedAt`. | `identity-filter-excludes-recycled-pid` and zero calls. |
| V2-T15 | Throwing buildinfo, environment, parent, or path guard. | Exact fail-closed reason and zero calls. |
| V2-T16 | Command-line, identity, architecture, or snapshot-completeness failure. | Exact refusal and zero calls. |
| V2-T17 | Durable intent write failure. | `audit-intent-unavailable` before the kill owner. |
| V2-T18 | Intended GUI plus independently gated `supervise` descendant. | Remains blocked until V2-T32 and V2-T33 pass; then exact coordinated ordering and call count are asserted. |
| V2-T19 | Re-enumeration sees recycled/missing identity. | `terminate-unconfirmed`, never fabricated success. |
| V2-T20 | CLI apply parsing/output. | Strict scope, age, token, nonzero mismatch, and per-candidate local audit. |
| V2-T21 | Short read or API error at every PEB hop. | Staged `env-read-error`; never key absence/map. |
| V2-T22 | Native amd64 layout/coherence helper and metadata drift. | Exact helper value on stable data; drift refuses at reread stage. |
| V2-T23 | amd64 reader against WOW64 i386 helper. | Exact present/absent results only after versioned layout source and offset gates. |
| V2-T24 | Access denial, classification/query/read failure, or target exit. | Exact staged `env-read-error` and zero calls. |
| V2-T25 | Production aliases across case, separator, junction, 8.3, UNC, and device forms. | Every equal/descendant alias refuses `production-state`. |
| V2-T26 | Temp roots intersect production and independent production override. | Each conjunctive guard refuses `production-state`. |
| V2-T27 | Installed/repo aliases, exact e2e fixture, and sibling-prefix paths. | Protected aliases refuse; exact fixture may proceed; prefix collision does not contain. |
| V2-T28 | Broken/missing/denied/looping strict canonicalization operand. | `path-canonicalization-error` and zero calls. |
| V2-T29 | Exclusive-branch refusal matrix. | `requires-explicit-temp-root`, `basename-not-in-branch`, or `argv-not-in-branch` without permission union. |
| V2-T30 | Already-orphaned standalone field-incident `supervise`. | `supervise-not-tree-reachable`, zero calls, and manual handoff. |
| V2-T31 | Intended GUI child fails an independent gate or confirmation. | Exact child refusal; zero GUI calls. |
| V2-T32 | GUI-spawned child exit while the real respawn loop is armed with a deterministically enlarged confirm window. | No replacement can spawn from quiescence through confirmed GUI exit. |
|  |  | Inability to prove quiescence refuses the whole operation. |
| V2-T33 | Direct and multi-hop PPID recycling with newer replacement ancestors and changed intermediate identities. | Every impossible, missing, recycled, or changed edge refuses; token and fresh revalidation bind every `{PID, StartedAt}` in the chain. |

### Deferred V2 Gate

Gate decision: **BLOCKED**. Destructive apply is not admitted to v1 and remains deferred until round-3 P2/P3 are resolved and a value case is demonstrated for the safely automatable subset. The preserved contracts above are necessary but not sufficient authorization to implement or ship apply.

## Terms and Abbreviations

- **Apply:** the deferred process-termination mode; no apply mode exists in v1.
- **PEB:** Process Environment Block, a Windows process structure relevant only to the deferred v2 environment-proof contract.
- **PID / PPID:** process identifier / parent process identifier.
- **P1 / P2 / P3:** security-review severity levels, from highest to lower priority in this work item.
- **V1 / V2:** preview/diagnostics-only version 1 / deferred destructive version 2.
