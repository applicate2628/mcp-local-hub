# PR 591 R35 consolidated architecture review

## Gate scope and evidence boundary

- Candidate: `.scratch/pr591-bot-r9`, base `51ac193f7e2c1d49404a4609b31de20fd06566da`, published HEAD `c363e5845ebda802932d57b23757d68a98cddcf8`, plus the 14-path unstaged R35 delta.
- Execution role: independent architecture reviewer on a Sol engine, distinct from the Terra implementation and QA lane.
- Mode: read-only review. No candidate source, test, documentation, status, Git index, commit, push, GitHub comment, build, test, or process state was modified. This report is the only written artifact.
- CodeGraph was invoked first. The candidate has no `.codegraph/`; the MCP lookup resolved through the branch-unsafe parent index and surfaced unrelated/pending files. No CodeGraph result is load-bearing. Exact candidate bytes and the upstream analyst/QA artifacts are authoritative.
- Upstream inputs read completely: `review-r35-consolidated-analyst.md` and `review-r35-consolidated-qa.md`.

## Reviewed surfaces

The review is bounded to the following files and contracts actually inspected:

- Governance and lane state: `work-items/active/2026-08-08-finish-open-pr-bot-findings/status.md`, `CONTRIBUTING.md`, and the candidate `README.md` preview/build/install/catalogue/architecture sections.
- CMake include grammar: `internal/cmakegraph/cmakegraph.go`; `internal/cmakegraph/r35_bot_regressions_test.go`.
- Pin-status remote admission and execution capability: `internal/vcpkgmcp/pinstatus/redact.go`; `internal/vcpkgmcp/pinstatus/remote.go`; `internal/vcpkgmcp/pinstatus/r35_bot_regressions_test.go`.
- Contained-process lifecycle: `internal/process/contained_stream.go`; `internal/process/contained_stream_other.go`; `internal/process/contained_stream_wait_deferred_posix.go`; `internal/process/contained_stream_wait_linux.go`; `internal/process/contained_stream_wait_kqueue.go`; `internal/process/contained_stream_wait_posix.go`; `internal/process/contained_stream_windows.go`.
- Contained-process guards: `internal/process/r35_deferred_wait_posix_test.go`; `internal/process/r35_no_console_windows_test.go`; `internal/process/contained_stream_linux_test.go` (the PID-1 zombie fixture); search-only participant inventory in `internal/process/r29_bot_regressions_test.go`.
- Root discovery: `internal/vcpkgmcp/discovery/discovery.go`; `internal/vcpkgmcp/discovery/r35_bot_regressions_test.go`.
- Catalogue/install ownership: `internal/api/install.go`; `internal/api/manifest.go`; kind-definition/search matches in `internal/config/manifest.go`; `configs/ports.yaml`; `docs/architecture-highlights.md`.
- Manifest inventory: the `kind` field in all 16 `servers/*/manifest.yaml` files: `drmemory`, `fetch`, `gdb`, `godbolt`, `lldb`, `mcp-language-server`, `memory`, `oneapi-run`, `paper-search-mcp`, `perftools`, `sequential-thinking`, `serena`, `time`, `vcpkg`, `vtune`, and `wolfram`.
- Contracts: exact CMake option spelling; remote-string admission before process execution; signal-before-reap and exact-one POSIX wait ownership; Windows no-console creation with inherited redirected handles; terminal explicit-root classification; showcase/global-install/embedded-manifest taxonomy; production-owner regression coverage; one-owner/no-layered-fix discipline; typed-return failure idiom.

`PASS` or `verified` below attests only to these surfaces.

## C1-C8 claim verdicts and receiving-side echo

Canonical S4 verdicts are `verified`, `failed`, and `not-verifiable (with reason)`.

| Claim | Guarantee, single owner, enforcement probe | S4 verdict | Evidence and disposition |
|---|---|---|---|
| C1 | Only exact-case `RESULT_VARIABLE` consumes the following include argument. Owner: `includeArgumentOptional`. Probe: `TestR35IncludeResultVariableRequiresExactCase` through production `Walk`. | `verified` | Exact equality is at `internal/cmakegraph/cmakegraph.go:2026`; the guard reaches the production graph walker at `internal/cmakegraph/r35_bot_regressions_test.go:8-17`. Analyst/QA `fixed` disposition confirmed. |
| C2 | `[user@]host:path` admits an omitted user while relative paths, slash-before-colon paths, and drive-relative paths remain non-remote. Owner: `isSCPLikeRemote`, consumed by the sole capability constructor `approveRemoteURL`. Probe: the two R35 pin-status tests. | `verified` | One classifier is reused at `internal/vcpkgmcp/pinstatus/redact.go:207-218,231-287`; execution still requires `approvedRemoteURL` at `redact.go:143-195` and `remote.go:69-86`. Guards cover production `PinStatus` plus local/drive cases at `r35_bot_regressions_test.go:9-77`. Analyst/QA `fixed` disposition confirmed. |
| C3 | Deferred Linux and BSD-family POSIX waiting observes exit without reaping, signals the process group before the one `cmd.Wait`, and propagates the final wait state. Owner: `posixContainedChild.terminateBy` with exact-one reap in `startContainedLeaderReap`. Probe: ordering unit guard plus the production PID-1 lifecycle fixture. | `not-verifiable (with reason)` | Static order is coherent: Linux `waitid(...WNOWAIT)` at `contained_stream_wait_linux.go:12-23`; BSD-family `kqueue` at `contained_stream_wait_kqueue.go:12-44`; group signal precedes reaper start at `contained_stream_other.go:99-123`; final result returns through `contained_stream.go:411-418,515-526`. The production-lifecycle guard is invalid and fails before its oracle because it bypasses required initialization (F1). The implementation is not disproved, but the diff-invisible lifecycle invariant lacks a valid named guard. |
| C4 | Windows adds `CREATE_NO_WINDOW` without breaking inherited stdout/stderr handles. Owner: `containedWindowsCreationFlags`, used once by `windowsContainedChild.start`. Probe: `TestR35ContainedWindowsUsesNoWindowAndPreservesRedirectedStreams`. | `verified` | Flags are centralized at `internal/process/contained_stream_windows.go:128-160`; handle-list and `STARTF_USESTDHANDLES` wiring remain at lines 73-125. The guard invokes production `RunContainedStream` and checks both streams at `r35_no_console_windows_test.go:15-41`. Analyst/QA `fixed` disposition confirmed. |
| C5 | An explicit regular-file root is terminal invalid input and cannot fall through to env/common-root discovery. Owner: the explicit tier of `DiscoverRoot`. Probe: `TestR35ExplicitRegularFileIsInvalidRoot`. | `verified` | Root-object type is checked before the synthetic child path at `internal/vcpkgmcp/discovery/discovery.go:248-304`; the guard proves one root stat and no child probe at `r35_bot_regressions_test.go:12-34`. Analyst/QA `fixed` disposition confirmed. |
| C6 | Catalogue/docs distinguish the 11-row showcase, 15 global `install --all` targets, and 16 embedded manifests. Owner facts: README table rows; `InstallAllWithOpts` plus manifest kinds; embedded manifest inventory. Static probes: table/config/manifest counts and current prose scan. | `failed` | The candidate has 11 showcase rows, 16 manifests, and exactly one workspace-scoped manifest, so `InstallAllWithOpts` targets 15 (`internal/api/install.go:299-330`). `README.md:106,117,209` instead labels all three as 11; `docs/architecture-highlights.md:15-21` still says 10/three embedded entries; `internal/api/install.go:246-250` still says 10. Analyst/QA documentation finding confirmed as F2. |
| C7 | Regression guards exercise production owners and do not bypass required initialization. Owner: each test fixture's construction seam. Probe: direct inspection plus focused/PID-1 QA results. | `failed` | C1, C2, C4, and C5 guards reach production owners. The PID-1 fixture constructs `&posixContainedChild{cmd: command}` at `internal/process/contained_stream_linux_test.go:315-323`, bypassing `initializePlatformContainedWait` called by production construction at `contained_stream_other.go:46-49`; QA's true PID-1 run fails before `zombie-launched`. F1. |
| C8 | The multi-fix batch has one owner per defect class, no piled neighboring checks, and one failure idiom per layer. Probe: mandatory per-class anti-layering and D1 scans below. | `verified` | No defect class has a second independent logic owner or duplicate termination/failure idiom. C6's prose is a false read model of the catalogue owner, not a second install mechanism. F1 and F2 remain correctness/coverage defects, but neither adds a parallel production fix. |

## Findings

### F1 — PID-1 regression fixture bypasses the deferred-wait lifecycle initializer

- Severity: blocking.
- Defect class: regression-guard contract / resource-lifecycle initialization.
- Violated claim/law: C7 production-owner guard; C3 diff-invisible signal-before-reap invariant; D4 resource-lifetime ownership.
- Fix-class: `design-decision`.
- WHAT: the fixture replaces `defaultContainedStreamDependencies().newChild` with a direct `posixContainedChild` literal at `internal/process/contained_stream_linux_test.go:315-323`. Production construction initializes `waitObserved` and `reapDone` at `internal/process/contained_stream_other.go:46-49` and `contained_stream_wait_deferred_posix.go:10-13`. `wait` rejects the nil state at `contained_stream_wait_deferred_posix.go:15-24`, so the fixture cannot reach the `zombie-launched` handshake at `contained_stream_linux_test.go:336-344`.
- Failure scenario: the true Linux PID-1 execution reports `zombie fixture start: context deadline exceeded`; therefore it does not test group signal before reaping, descendant disappearance, or final wait-state propagation.
- Upstream disposition: analyst finding `confirmed`; QA finding `confirmed` by a real PID-1 namespace run. The weaker non-PID-1 command was correctly treated as a skip/non-evidence.
- Required acceptance: restore production-equivalent initialization and re-run the exact PID-1 guard to a terminal pass that reaches the zombie handshake, returns without cleanup timeout, and proves both process identities disappear.
- ADVISORY HOW (non-binding): owning seam is the fixture's `deps.newChild`. One candidate is to construct through `newPlatformContainedChild`, type-assert the POSIX child, and replace only the classifier test seam. A smaller alternative is to call `initializePlatformContainedWait` immediately after the literal construction; it is simpler but can drift again if production construction gains another invariant. Falsifying guard: the exact WSL `go test -exec unshare ... TestRunContainedStreamPOSIX_ZombieOnlyGroupDoesNotFalseTimeout` shape used by QA, plus the focused process package.

The `design-decision` classification is mandatory because the finding is concurrency/resource-lifetime-sensitive and the direct-init versus production-constructor alternatives have different future-drift ownership.

### F2 — Catalogue prose conflates three different cardinalities

- Severity: blocking.
- Defect class: public documentation / operational-contract drift.
- Violated claim/law: C6 current-state coherence; catalogue receiving-side echo.
- Fix-class: `inline-sufficient`.
- WHAT: the current owner enumerates 16 embedded manifests and skips only the one `kind: workspace-scoped` manifest, yielding 15 global `install --all` targets (`internal/api/install.go:299-330`). The candidate README calls the command, table, and embedded set 11 at `README.md:106,117,209`. The table itself has 11 rows. Dependent current prose remains stale at `docs/architecture-highlights.md:17,21` and `internal/api/install.go:246-250`.
- Failure scenario: operators are told that `install --all` covers 11 servers when it attempts 15; architecture prose also understates the embedded manifest set and omits the hub-only vcpkg entry pattern.
- Upstream disposition: analyst and QA findings both `confirmed`; the static inventory independently reproduced 11 showcase rows, 15 global targets, and 16 manifests.
- Required acceptance: update all current prose in the admitted surface to state the three distinct sets without treating any count as another, and remove the stale 10/three-entry wording.
- ADVISORY HOW (non-binding): owning seam is catalogue/read-model prose. One candidate is to rename the README heading to an explicitly curated/showcase set, describe `install --all` as the 15 global manifests, and describe the embedded FS as 16 manifests including one workspace-scoped entry; mirror the embedded-entry distinction in `docs/architecture-highlights.md` and correct the `Install` comment. A material alternative is to avoid hard-coded cardinalities for command behavior and link to the enumerator, trading immediate specificity for lower drift. Falsifying guard: count README showcase rows, enumerate all embedded manifests by kind, assert `InstallAllWithOpts` skips only workspace-scoped manifests, and scan current docs/comments for contradictory 10/11 install-or-manifest claims.

`inline-sufficient` applies because the behavior owner and all incorrect read models are evidenced, the requested 11/15/16 taxonomy resolves intent, and the bounded documentation correction has one distinguishing static guard; no code or contract redesign is required.

## Mandatory anti-layering audit

| Defect class | Owner and participant audit | Verdict |
|---|---|---|
| CMake include option case | `includeArgumentOptional` owns option spelling and argument consumption. The regression goes through `Walk`; no adapter/backend duplicate was added. | `CLEAN-SINGLE-OWNER` |
| SCP-like remote admission | `isSCPLikeRemote` is the one shape classifier; `approveRemoteURL` remains the sole capability constructor. `transportArgument` calls back into that same owner at the untrusted-manifest-to-process-execution boundary rather than carrying a copied predicate. | `JUSTIFIED-DEPTH` |
| POSIX signal-before-reap | `posixContainedChild.terminateBy` owns signal/settlement order; `startContainedLeaderReap` plus `sync.Once` owns exact-one reaping; Linux and BSD-family files supply only platform exit observers. `reapBy` retrieves through the same owner and contains no second wait implementation. | `CLEAN-SINGLE-OWNER` |
| Windows no-console creation | `containedWindowsCreationFlags` owns the flag set; `windowsContainedChild.start` owns one `CreateProcess` call and the existing redirected-handle contract. | `CLEAN-SINGLE-OWNER` |
| Explicit-root classification | `DiscoverRoot`'s terminal explicit tier owns root-object classification; the later binary probe checks a different object/invariant and is not a layered recheck. | `CLEAN-SINGLE-OWNER` |
| Catalogue cardinality | `InstallAllWithOpts` plus manifest kinds owns install participation; the 11-row README showcase is a separate curated view. Incorrect prose is F2, not a parallel installation owner. | `CLEAN-SINGLE-OWNER` |

No class is `PILED`. F1 is a bypassed initializer in a guard, not a neighboring production patch; F2 is an inaccurate read model, not duplicate behavior logic.

### D1 failure-idiom probe

- No `os.Exit`, `log.Fatal`, `panic`, `syscall.Exit`, or `runtime.Goexit` idiom appears in the changed production surfaces.
- CMake parsing returns its established `(value, valid)`/reason shapes; pin-status returns `Reason` plus a private capability; discovery returns `Result`; process leaves return errors or `containedWaitResult`; `RunContainedStream` is the composition owner that projects typed `ContainedRunError` stages.
- Verdict: uniform per layer; no duplicate failure idiom for one defect class.

## Architecture, coupling, and blast radius

- The five code fixes remain at their owning seams. There is no new upward dependency, sibling-private import, cyclic edge, ambient configuration read, public schema change, shared mutable global, or scenario-specific backend fork in the reviewed delta.
- C1, C2, C4, and C5 are cohesive local corrections with production-owner guards. The POSIX implementation centralizes the shared deferred state machine while keeping Linux `waitid` and BSD-family `kqueue` as thin platform observers.
- The confirmed blast radius of F1 is the Linux PID-1 integration oracle and confidence in the shared Linux/BSD-family lifecycle, not evidence of a production failure. The confirmed blast radius of F2 is operator-facing installation/catalogue accuracy and dependent current architecture prose; runtime enumeration remains single-owned and unchanged.
- The 290-path cumulative PR was not re-reviewed wholesale in this lane. This verdict covers the R35 owners, callers, invariants, tests, catalogue sources, and dependent current prose listed above.

## Required fixes before merge

1. Repair the PID-1 fixture's construction seam so it carries all production deferred-wait initialization, then re-run the true PID-1 lifecycle guard and affected process checks to terminal PASS.
2. Reconcile current catalogue/read-model prose to the explicit 11 showcase / 15 global install targets / 16 embedded manifests taxonomy, including the dependent stale architecture prose and `Install` comment, then re-run the static cardinality/contradiction guard.

No other architecture fix is required by this review.

## Residual risk and limits

- Darwin/BSD `kqueue` early-exit behavior, signal-before-reap ordering, and final exit-state propagation remain target-runtime `UNVERIFIED`. Static ownership and build-tag structure are coherent, but this lane ran no target-runtime probe. This is retained as an explicit evidence gap, not promoted to a third confirmed blocker.
- QA's operator-stopped full suite remains `UNVERIFIED`; partial output is not completion evidence. The focused four-package PASS supports C1/C2/C4/C5 only.
- The valid Linux PID-1 guard is the immediate falsifier for the shared lifecycle. Passing it will not by itself establish every BSD-family runtime, so any merge decision must retain the platform evidence boundary explicitly unless target evidence is added.

## Gate decision

**REVISE.** C1, C2, C4, C5, and C8 are verified. C3 is not verifiable because its production-lifecycle regression guard bypasses required initialization; C7 therefore fails. C6 fails because the candidate conflates the 11-row showcase, 15 global install targets, and 16 embedded manifests. The anti-layering audit contains no `PILED` class and D1 is uniform. Exactly F1 and F2 require correction and re-verification before this architecture gate can pass.

## Terms and Abbreviations

- BSD: Berkeley Software Distribution family; here Darwin, DragonFly BSD, FreeBSD, NetBSD, and OpenBSD build targets.
- D1/D4: architecture hygiene laws for returned failure values and resource-lifetime ownership.
- MCP: Model Context Protocol.
- PGID/PID: process-group identifier / process identifier.
- R35: the thirty-fifth bot-review correction round for PR #591.
- S4: per-claim review verdict stage using `verified`, `failed`, or `not-verifiable (with reason)`.
- `PASS`: the reviewed scope satisfies the gate with sufficient evidence.
- `REVISE`: confirmed correction and re-verification are required.
- `UNVERIFIED`: the named evidence was not obtained; it is not a clean result.
