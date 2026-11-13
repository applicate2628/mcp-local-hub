# PR #616: unified CARBON remediation and continuation plan

Original review baseline: `e3c7cbabc97d218a86599b93218edabd33dee159`.
PR base: `76d11b33bd37e30125525ce8e0e582122f29e91b`.
PR #616 head before continuation: `2ca927ff9f759fca9d7cb96b514fcfd93341d558`.
Published continuation implementation: [`814a2b47`](https://github.com/applicate2628/mcp-local-hub/commit/814a2b47c6b2d2dfdcbca4d324fb792ff83d3a90).
The original five CARBON findings were reproduced on Windows before implementation.
Follow-up bot finding B27 moves the self-contained execution-attribution correction C07 and its producer stage from #620 into #616; the production bytes remain identical to #620.
This record preserves F01–F08 and adds confirmed findings from implementation, independent review and native execution.

Pre-merge audit refreshed on 2026-11-13 against published head
`53645ec7170c9d56073ab5140dec865b23ce7c21`. That head already incorporates
#618–#622. The current correction completes C15 and addresses C18–C21 below;
all five production defects were reproduced on the published source before correction.
The full Windows run also exposed incomplete sandbox setup and outdated or
wall-clock-dependent fixture assumptions (C09, C11, C12, C22 and C23).
The source audit used Go correctness/security/lifetime review, Software Architect,
Ponytail and Caveman verification guidance, with independent reviews of each
correction. The earlier CARBON review remains historical; this pass does not
claim a new complete CARBON workflow.

## Contents

1. [Outcome and boundaries](#outcome-and-boundaries)
2. [Original corrections F01–F08](#original-corrections)
3. [Confirmed continuation findings](#confirmed-continuation-findings)
4. [Recovery and ownership decisions](#recovery-and-ownership-decisions)
5. [Delivery plan](#delivery-plan)
6. [Verification record](#verification-record)
7. [Remaining gates and next actions](#remaining-gates-and-next-actions)
8. [Bot review disposition](#bot-review-disposition)
9. [Terms and abbreviations](#terms-and-abbreviations)

## Outcome and boundaries

Correct confirmed defects while preserving native file/process ownership, journal readback,
rollback identity, process generation, concurrent work and the launcher/Product layout.
Use existing owners and bounded operations; add no generic recovery or subprocess layer.
Keep production corrections distinct from fixture evidence. Preserve the existing
integration in #616 instead of reopening duplicate pull requests (Table 3).

All execution uses isolated temporary state and owned fixture processes. No installed service,
scheduled task, visible GUI or paid/external model was run. Native GUI-subsystem test binaries
are test processes, not a claim that the interactive installed product was exercised.
The full repository and release are **not certified green** by these scoped results.

## Original corrections

Table 1. Preserved original findings, corrections and acceptance boundaries.

| ID / kind | Source and triggering condition | Consequence and required correction | Acceptance |
| --- | --- | --- | --- |
| F01 / production | `internal/cli/windows_product_pair.go`, `WindowsProductTxn.Prepare`; first complete-set journal publication or uncertain write. | Missing native directory identity makes the first checkpoint unrecoverable. Include the received identity in the first journal; preserve stages after uncertain publication and settle only unjournaled owned resources. | First-write crash checkpoints recover; before/after-write errors preserve ownership; ambiguous old journals cannot authorize deletion. |
| F02 / production | Prior-runtime recovery callback in `internal/cli/install_migration_wiring_windows.go`; setup/canonicalize recovery. | These modes do not produce an Upgrade prior-runtime projection. Apply the mode-specific contract instead of requiring that projection. | Real cold rollback reaches settlement for both modes; invalid Upgrade projection still fails. |
| F03 / production | Same prior-runtime callback; prior supervisor was live but is currently absent. | Current absence alone can falsely report restoration. Decide from saved prior state and restore the owned supervisor through readiness. | Absent prior remains absent; live prior reaches owned start/readiness; missing original owner or readiness failure refuses success. |
| F04 / production | `internal/binaryadmission/windows_canonical_entry_set.go` and complete-set identity readers; oversized identity-v1 metadata. | Unbounded input can allocate excessive memory. Enforce one 64 KiB limit before reading the complete metadata. | Limit/limit+1, whitespace, native/build/prior readers covered; executable payload size remains unrestricted by this metadata limit. |
| F05 / fixture | `internal/cbuild/handlers_test.go`; configured-cache fixture uses a noncanonical key on Windows. | Stale-mapping test misses the real cache entry. Use `projectPathKey` for insertion and lookup. | Existing stale-mapping scenario passes without changing production normalization. |
| F06 / production | Replayed supervisor readiness in `internal/cli/install_migration_wiring_windows.go`; foreign pipe server echoes the expected hello. | Hello text alone does not prove the peer. Bind strict status/readiness to the actual native server on the same connection. | Foreign server refused before a status request; actual peer succeeds. |
| F07 / production | GUI prior-runtime replay in the same wiring file; crash after independent prior-supervisor restart, before GUI startup. | Replay may fail or interfere with an already recovered supervisor. Observe its native image, generation, user, role, fleet and peer without claiming creator authority. | Continue GUI recovery without starting/stopping that supervisor; preserve it on later failure and retain historical journal generation. |
| F08 / fixture | `internal/cli/install_migration_wiring_windows_test.go`; setup/canonicalize command fence tests omit the required build binding. | Tests fail before reaching the intended fence. Supply the binding and assert the current diagnostic. | Both command adapters refuse before recovery or mutation while the upgrade fence is held. |

F06 was introduced in the new replay branch and corrected during independent review.
F07 is an inherited availability defect confirmed in the base and reviewed head by its reachable crash sequence.
F08 adapts a test missed by the complete-set command change. These attributions are not interchangeable.

## Confirmed continuation findings

Table 2. Confirmed continuation defects and fixture mismatches; hypotheses are not listed as defects.

| ID / kind | Source | Condition → consequence | Minimal correction / required evidence |
| --- | --- | --- | --- |
| C01 / production | `internal/cli/supervisor_launch_windows.go`, `install_migration_wiring_windows.go`: `BindControl`, `WaitSupervisorReady` | The original Product remains held while supervisor control/readiness cold-opens its path → sharing violation or lost operation lifetime. | Borrow the held Product observation and operation context through control/readiness; retain native peer and generation checks. Qualified replay and actual command fixtures pass (Table 4). |
| C02 / production | `internal/cli/windows_product_pair.go`: `inspectWindowsProductPreflight` | Complete-set Stage/Prior already hold native files; preflight calls cold `AdmitProduct` and pathname snapshot → forward command fails before supervisor settlement. | Inspect the same originals; validate all four bindings, full launcher/Product/identity disposition and prior runtime preimage. Refuse closed/cancelled owners; do not release or reopen them to bypass sharing. |
| C03 / production | `internal/gui/projects_test.go`: `TestProjectsScan_NoWrite`; client readers under `internal/clients` | Read-only project inspection takes mutating config-lock paths → `.cursor/mcp.json.lock` / `.mcp.json.lock` appear. Reproduced on base and candidate. | Nonmutating client-config reads with native/path boundary checks; preserve concurrent writers' ownership. Independent [PR #618](https://github.com/applicate2628/mcp-local-hub/pull/618). |
| C04 / production and static correctness | `internal/process/proc_thread_attribute_observed*.go`, `start_with_job_windows.go` | Native allocation addresses are represented as Go pointers → four `unsafe.Pointer` vet diagnostics at the native/Go boundary. | Keep opaque native addresses as `uintptr` through allocation/use/free; assert ABI size/offsets. Vet passes; amd64 native tests and 386/arm64 compilation are separate evidence. |
| C05 / production | `internal/process/process_owner_sid_windows.go` | An exited process remains open through another retained handle; `OpenProcess` and token query can still succeed → the owner gate can accept an already exited process. | After one `OpenProcess`, query zero-time liveness and owner on that same handle with synchronization access; refuse an exited process. |
| C06 / production | `internal/process/contained_stream*.go`, `provisional.go` | Work cancellation, process failure and later cleanup timeout share ambiguous error evidence → callers can misattribute execution or lose the cleanup sentinel. | Typed `ContainedStageContext`; preserve primary cause and `ErrCleanupTimeout` independently. Original owner, one waiter and one terminal deadline remain authoritative. |
| C07 / production | `internal/kimi/runner.go`: `classifyRunFailure` | Cleanup/receipt handling outlasts the work deadline after an actual success or exit failure → late context reads incorrectly turn the retained result into TIMEOUT/CANCELLED. | Classify from typed primary Stage/Cause once; preserve it through cleanup faults. Only unstructured legacy evidence uses a context snapshot. Deterministic regressions and local fixture runs pass. Follow-up B27 is reproduced and corrected in #616 itself; success, actual cancellation and cleanup failure retain separate outcomes. |
| C08 / production | `internal/daemon/stdio_launch_environment.go`, `stdio_launch_preparation.go` | Windows drive-current-directory entry such as `=C:=C:\work` is split as an empty key → launch environment loses drive state. | Parse and merge the real `=C:` key under the existing canonical normalization; preserve unset/profile semantics. |
| C09 / fixtures | API adopt/project-plan/secrets fixtures; GUI adopt/OpenCode extractor fixtures; CLI config-isolation/settings fixtures | Missing sandbox roots or inherited broad DACLs fail before the intended scenario; rehardened existing paths violate the helper's create contract. | Create required roots in order and protect the exact parent before sandbox setup; share the correctly owned secrets fixture. The November 13 follow-up adds five missing root creations in API/GUI callers. CLI already creates its root through `HardenedDir` and needs no duplicate creation. Keep adverse-ACL tests adverse. |
| C10 / fixtures | `internal/e2e/scan_readiness_fixture/driver/main_windows_test.go`; CLI port/restart fixtures | An allocated port is assumed above 10,000, or a request uses `127.0.0.1` rather than the configured HTTP Host → transport/auth boundary refuses the fixture. | Use a valid logical port with transport mapped to the actual ephemeral listener and send the configured Host; do not relax host authorization. |
| C11 / fixtures | Daemon backend/readiness/Python fixtures; `internal/sonnet/output_lock_test.go`; Kimi/CLI/perftools fixtures | Legacy `StdioHost.job` assertion ignores `original`; real filesystem latency consumes a 10/50 ms readiness budget or Sonnet's 1 s pre-settlement budget; child cleanup is armed too late; external schema/command expectations changed. | Assert actual original ownership and settlement; synchronize the tested boundary. Use standard-library `testing/synctest` for self-contained short-budget scenarios, retaining actual deadlines and result assertions. Sonnet now also asserts the context is live before output settlement. File I/O remains real and the outer test timeout remains required. Preserve negative abort ordering and supported command/schema contracts. |
| C12 / fixture qualification | `tools/test-recovery-namespace`, `scripts/test-recovery-namespace`; `tools/test-job-entry`; `internal/process/jobobject_windows_test.go` | Scalar claims or renewed terminal deadlines cannot prove native custody; ambient helper variables or mutable ancestry could escape the fixture scope. Inherited `GOTMPDIR` also sent `testing.TempDir` outside the private namespace even though TEMP/TMP were redirected. | Real native handles/identity/ACL/reparse observations; pinned source and private roots; reject ambient fixture sentinels; bounded original-owner lifecycle. Override `GOTMPDIR` with that same private temp directory. Retain one actual closure admission/deadline and READY→GRANT→ACK evidence; no fabricated proof or security-policy change. |
| C13 / production | `internal/api/serena_parsed_preparation.go`; `internal/cli/migrate_serena.go` | Migration invokes parsed installation under the quiet lock through a fresh API without a preacquired prepared invocation → the live-workspace path refuses with `SERENA_PARSED_PREACQUIRED_INVOCATION_REQUIRED`. | Prepare with the same owner before the quiet lock; retain immutable parsed input/request and use `WithConsumerInvocation` for the exact consumer descriptors. |
| C14 / production | `internal/api/runtime_product_interlock.go`, `admission_check.go`, `install.go`, `install_parsed_manifest.go` | Quiet-lock admission still expects live supervisor control, or a local child context is cancelled before observed close → valid held installation is refused. | Verify the actual quiet lock and native Product image; thread that resolver and the live bounded invocation context through install and observed close. Never substitute lock text for process authority. |
| C15 / production | `internal/cli/migrate_serena.go`: prepared install result handling; `internal/api/install_parsed_manifest.go` | Installation commits, then invocation close, lease release or a borrow's final cancellation check fails → the caller can roll back registry/reconcile state while the new intent remains durable. | Preserve the API's nonempty committed `intentPath` through later errors; classify it as postcommit without rearming rollback. The November 13 regression uses a real parsed-install write and observed lease, plus a precommit negative control; it does not qualify positive package preparation. |
| C16 / production | `internal/api/install_parsed_manifest.go`; `serena_parsed_scope_test.go` | One/all workspaces disappear before the initial guard or final live filter → publication can use a descriptor set different from the prepared invocation, including an empty set. | Validate incoming invocation even for empty selection; compare the final materialized descriptors exactly before merge/publication. Tests preserve prior/sibling state. Positive full preparation remains an open gate. |
| C17 / fixture | `internal/process/job_options_windows_test.go`: `awaitJobFixtureJSON` | Native integration observed `ERROR_SHARING_VIOLATION` while polling an atomically published readiness frame → the fixture fails before testing creator closure. | Retry only that transient native error and missing-file errors within the existing absolute deadline; keep malformed frames and other errors fail-fast. Native held-file release/expiry/malformed regressions pass (1 / 3); qualified Job rerun passes (2 / 6). |
| C18 / production | `internal/cli/install_migration_wiring_windows.go`: successor launch composition; `internal/binaryadmission/windows_canonical_entry_successor_runtime_windows.go` | The promoted successor runs while Stage/Prior still hold DELETE access; plain path hashing in control/readiness fails with a sharing violation. | Borrow the exact composed successor from promoted stage and byte-identical prior members. Validate the complete launcher/Product/identity set, retain that view through launch/control/readiness, and invalidate it on owner close. Actual native tests cover absent, identical and different-identity prior sets. |
| C19 / production | `internal/binaryadmission/windows_canonical_entry_stage_windows.go`: Stage and original-member close | A reduced-access duplicate still references the write-capable creation FileObject → Windows refuses to execute the newly promoted Product. | Create the original with READ/DELETE only; use a separate transient writer and then a separate read guard. Recheck original identity, extent and full hash before readiness. Close both retained objects before absence readback; preserve busy or changed partial files. |
| C20 / production | `internal/cli/windows_canonical_entry_recovery_windows.go`: pending validation and cold rollback | Setup/canonicalize legitimately accept a partial byte-identical prior set, but unconditional `RuntimeProduct` rejects it as an incomplete executable runtime. | Require the runtime projection only for Upgrade, matching the journal producer and existing callback. Twelve native cases cover two modes, three missing members and validation/rollback; Upgrade projection refusal stays enforced. |
| C21 / production, inherited | `internal/api/adopt.go`: repeat receiver verification | A repeated adopt detects an inconsistent intent, but generic error return finds the nested receiver `AdoptStageError` first → the public stage becomes `intent-task-verify` instead of `existing-state-inconsistent`. | Wrap that repeat-only failure with the already selected stage and `committed_unverified` state; retain the receiver error through `Unwrap`. Existing repeat/fresh-path regressions pass without changing their expectations. The same defect is present on base `76d11b33`. |
| C22 / fixtures | `internal/api/adopted_entries_test.go`; `internal/e2e/lazy_register_test.go` | The unknown-version test supplies supported version 4; the lifecycle test expects deletion of a client entry for which the registry stores no durable ownership proof. | Use maximum supported version + 1 for unknown-version rejection. Check the entire preserved client map and its exact typed preservation warning after unregister, while retaining registry/task/proxy cleanup assertions. Do not relax the production preservation rule. |
| C23 / fixtures | CLI upgrade wiring, no-client install and zero-workspace Serena tests | Old callers omit the complete-set build binding or create an inert canonical file without selecting it, so execution stops at an earlier input/runtime boundary. A synthetic parser-only PE cannot represent the executing Product for the unreadable-intent regression. | Supply the existing explicit build input for the pre-cancelled fence test. Select the existing restore-bound private runtime seam only for no-client dry-run and two zero-workspace cases. Require a real qualified executing CLI test Product for unreadable intent, with runner refusal and unchanged prior/intent bytes. Keep live-workspace preparation and native negative tests unchanged. |

The copied-mutex warning in `stdio_launch_preparation_test.go` and stale process source-contract
file count were test defects, corrected without weakening ownership. Static/native failures that
were unchanged on the base are not attributed to PR #616 merely because the broader run exposed them.

## Recovery and ownership decisions

1. Every first durable checkpoint must carry its received native directory identity.
   Failed/uncertain publication cannot make a later `Prepare` succeed or delete journal-referenced stages.
   Cache a preparation failure and leave `prepared == nil` after closing preparation owners.
2. Old journals with `PayloadDirectory.Original == nil` cannot prove an existing occupant belongs
   to the transaction, even if the directory was previously absent. Retain the journal and diagnose;
   do not invent identity or delete a possible foreign occupant.
3. Restore runtime from saved prior state. File restoration alone does not restore a previously live
   supervisor. A recovered independent supervisor is observed, never reconstructed as a new owned child.
4. Path/hash/disposition checks complement retained original authority. They do not authorize
   replacing that authority with a path reopen or an early close to let the next operation succeed.
   Compatible guard/probe opens remain subordinate to the continuously held original identity.
5. Identity-v1 has one 65,536-byte limit and explicit oversize failure, never truncation.
   The original normal/padded observations were 333/1,048,909 bytes; the limit does not apply to executables.
6. Preserve execution cause separately from cleanup outcome. Do not renew the admitted terminal
   deadline, repeat the original waiter, or let receipt/cleanup timing rewrite an earlier result.
7. Fixture repair must preserve the tested adverse condition and authority boundary. A hardlink fallback
   is suitable only for a shared-file-identity assertion; it does not prove symlink privilege or semantics.
8. A ready staged image must retain no writable FileObject. The READ/DELETE original
   continuously prevents foreign rename/delete; writer-close → read-guard acquisition is
   not an atomic write-exclusion handoff. A live writer prevents guard acquisition;
   subsequent checks reject differences in identity, extent or retained bytes.
   Partial cleanup receives the same guard and compares its recorded preimage before deletion.

## Delivery plan

Table 3. Delivery disposition at the 2026-11-13 snapshot. Earlier delivery
commits are retained in #616; additional audit commits were then unpublished.
Only #616 remains the merge target for this work.

| Delivery | Included work | State / link |
| --- | --- | --- |
| Existing recovery PR | F01–F08, C01–C23, B01–B28 and the incorporated delivery commits. | At the November 13 20:12 UTC snapshot, [PR #616](https://github.com/applicate2628/mcp-local-hub/pull/616) is open at `53645ec7`; the additional audit corrections below are prepared for publication and a fresh review. |
| Client read-only | C03, isolated from recovery and process custody. | [PR #618](https://github.com/applicate2628/mcp-local-hub/pull/618) closed as incorporated; commit `93cf926a` retained. |
| Daemon launch / lifecycle | C08 and daemon-specific fixture corrections. | [PR #619](https://github.com/applicate2628/mcp-local-hub/pull/619) closed as incorporated; commit `e30e492d` retained. |
| Native process / Kimi | C04–C07, C17, native Job qualification and process/runner regressions. | [PR #620](https://github.com/applicate2628/mcp-local-hub/pull/620) merged into the #616 branch, not master; commit `62a7b0dc` retained. |
| Portable fixtures / namespace | C09–C12 outside the daemon/native delivery and recovery-namespace harness. | [PR #621](https://github.com/applicate2628/mcp-local-hub/pull/621) closed as incorporated; commit `64b0eddb` retained. |
| Serena invocation | C13–C16, then caller binding and minimum-compatible tools/original-context corrections. | [PR #622](https://github.com/applicate2628/mcp-local-hub/pull/622) closed as incorporated; `9034a661`, `92bc059c` and `53645ec7` retained. Positive prepared-install qualification remains open. |

Do not reopen or separately merge the incorporated duplicates. The user requested
merge after audit; publication/merge still require the repository gates below.
Own review follows each substantive change and independent review supplements it.
For each executed November 13 correction set, SHA-256 manifests bind the local
source to the formatted Windows source. The initial four production corrections
covered 16 Go files; later fixture and repeat-stage corrections are recorded
separately. Historical source-composition checks are not runtime qualification
of the combined repository.

The additional source is preserved on `fix/pr616-final-audit-20261113` in eight
reviewable commits, ending at `94a75f65f3167d497e3f5382883b5ffe5e11b5d8`:

- `0d5bac86`: C15 committed Serena result and its regression.
- `4264b10d`: C18/C19 executable originals, successor composition and native regressions.
- `23883d32`: C20 mode-specific partial-prior recovery and its native regression.
- `a1b54a22`: C21 repeat-adopt error classification.
- `c99eeb69`: C09/C22 sandbox, schema and client-preservation fixtures.
- `8cb42ae1`: C11 deterministic deadline fixtures.
- `ff8022cc`: C12 private `GOTMPDIR`.
- `94a75f65`: C23 explicit CLI fixture prerequisites.

A verified Git bundle preserves these eight commits on the Windows audit machine
and requires the already-present prerequisite `53645ec7`. It does not contain a
standalone copy of the repository's earlier history. This is a local transfer,
not a GitHub push.

### Publication continuation — November 14, 2026 (Europe/Moscow)

After reviewing the concrete outstanding checks, the user authorized publishing
the reviewed corrections and requesting a fresh bot review. This is a bounded
exception to the complete pre-push test gate for this correction series; the
remaining merge gates are still open. It does not authorize GitHub Actions,
compiler/package installation, elevation, installed-state changes or release.

The publication check covers the eight code commits and the committed report:
30 Go files plus this Markdown plan, with no generated binaries, dependency,
workflow or hook changes. The final executed code remains `94a75f65`; subsequent
documentation commits do not change it. Confirm the actual remote head after
publication and request review for that head. The `53645ec7` bot PASS remains
historical; do not transfer it to the newly published revision.

## Verification record

Table 4. Actual recorded executions. Counts are top-level tests / subtests; overlapping selections must not be summed.

| Scope / candidate | Observed result | What it establishes and limits |
| --- | --- | --- |
| November 13 baseline `53645ec7` | `go build ./...` PASS; canonical `npm test` PASS, 16 tests and zero skips. | A direct Node invocation initially lacked npm's runner environment and failed one test; the actual npm entry point passed. An isolated real tagged Product was built offline from the existing approved cache. |
| C15 before/after | Baseline: late cancellation and package-release cases FAIL; precommit cancellation PASS. Corrected: all 3 PASS. | Real durable intent bytes, registry allocation, reconcile rollback, lock release before start and retained error causes checked. No positive Serena package preparation is claimed. |
| C20 before/after | Baseline: all 12 native partial-prior cases FAIL. Corrected: all 12 PASS. Combined C15/C20 and existing affected guards: 5 / 19 PASS, zero skips. | Actual producer checkpoint and cold reacquisition; validation preserves files/journal. At the prepared checkpoint, rollback preserves the exact partial preimage and cleans uncommitted stages. Invalid Upgrade projection and unknown companions still fail closed. |
| C18/C19 native successor before/after | Baseline: 3 failures—new Product cannot spawn; retained identical Product fails control hashing. Corrected: 1 / 3 PASS, zero skips. | Actual isolated supervisor Start/BindControl/Ready while original files stay held, wrong-hash refusal, creator-only cleanup and closed-owner refusal. The child Product is the real tagged `53645ec7` fixture; the receiver uses all four corrections. |
| November 13 binary admission | Full package: 52 / 73 PASS; 3 existing skips. | Includes the new busy/changed/unchanged partial cleanup and compatible WRITE-denial probes before/after promotion. Symbolic-link privilege, POSIX executable mode and unavailable 8.3 short-name cases remain skipped. |
| November 13 independent native replay | 1 / 6 PASS, zero skips. | Two real supervisor generations, same native peer, retained image/fleet, replay without acquiring creator authority, cleanup after cancellation and closed-owner refusal. GUI observation/reservation/start remain headless substitutions; no Task Scheduler qualification. |
| November 13 combined correction | `go build ./...` and `go vet ./...` PASS after all four corrections. | All 16 changed Go files match the executed Windows source. Full untagged/tagged repository gates are recorded separately and are not inferred from these successes. |
| November 13 full untagged attempt | FAIL: 4,604 top-level / 3,411 subtests passed; 53 packages passed; 73 failure events and 107 skips. | Includes package/parent failure events, so these are not 73 independent defects. Failures included the fixture defects above, missing MSVC, missing qualified Job entry and the API package's 5-minute aggregate timeout. The outer script accidentally retained an empty actor sentinel, so CLI exited before TestMain; this attempt provides no CLI coverage. |
| November 13 full tagged API/CLI attempt | FAIL: 340 / 278 passed; 19 failure events and 4 skips. | API reached its 5-minute aggregate timeout. CLI again exited before TestMain because of the external script's empty actor sentinel. The corrected child environment removes the Env-provider entries and verifies absence; no actor admission guard was changed. |
| Clean-environment full tagged CLI attempt | FAIL: 829 / 608 PASS before the 5-minute package deadline; 9 skips. | 47 failure events comprise 41 leaf cases, 5 repeated parent events and 1 package failure. Serena leaf causes: 7 refused symbolic-link creations, 30 runtime Product preflight refusals and 1 `SERENA_BUILD_SOURCE_INVALID`. Three other leaf failures concern the CLI fixture prerequisites corrected by C23; unreadable-intent native verification is recorded separately below. No stale empty actor sentinel remained in this run. |
| Sonnet B28 fixture follow-up | Full Sonnet package: 58 / 34 PASS; 2 existing skips. | The test proves its context is live before output settlement, then observes the original one-second deadline inside settlement. Completed/nonzero/cleanup-failed results, streams and restart assertions remain intact. No real electromagnetic solver was run. |
| Adopt/API/GUI root follow-up | Initial broader selection: 132 / 167 PASS with 3 skips; API still failed the then-unfixed repeat-stage, omitted-root and schema-version cases. | The GUI portion passed. This mixed intermediate run is not presented as a complete passing API result. |
| C21 and additional API fixture corrections | Tagged focused selection: 13 / 15 PASS, zero skips. | Covers repeat/fresh receiver stages, supported/unknown schema, port/preview roots, and real public-CLI receiving. `TestPromoteAndAbortIdempotent` and `TestAttachExisting_RollbackConflictRetainsRecovery`, which were active at the full-run deadline, also pass individually in this selection. |
| OpenCode extractor root | 1 top-level PASS, zero skips. | The intentional XDG path override and the original extractor assertions remain unchanged. |
| Daemon readiness fixture follow-up | 11 / 11 PASS, zero skips. | Same readiness/epoch/result assertions and 50/10 ms deadlines; native disk writes no longer consume the synthetic timing premise. |
| Daemon cold-forward fixture follow-up | 4 top-level PASS, zero skips. | Three cases first failed in the clean focused baseline; the concurrent-call case failed under the full-run load. Original 40/50/800 ms budgets, one delivery/no replay, held-event timing, warm no-event and eight-caller singleflight assertions are unchanged. The concurrent case tests the documented gate-wait budget, not end-to-end disk/HTTP latency. |
| E2E unregister fixture follow-up | 1 top-level PASS, zero skips. | Full client-map byte preservation, exact typed warning, registry/task cleanup and proxy stop are all checked. |
| C23 ordinary CLI fixture follow-up | 6 / 9 PASS, zero skips; zero-workspace matrix separately 1 / 1 PASS. | Covers flag/check/dry-run behavior, pre-cancelled fence, actual zero-workspace intent/continuation, and the autostart rollback test active at the full-run deadline. The latter passes independently; no deadlock is inferred from the aggregate timeout. These tests do not qualify a live prepared Serena package. |
| C23 unreadable-intent native command | 1 top-level PASS, zero skips, 12.55 s; original test process exit 0. | Actual qualified executing CLI test Product built from `94a75f65`, original native admission/staging, exact unreadable-intent cause, zero upgrade-runner calls and byte-identical prior Product/intent. The selected test has a 90-second deadline. This closes the remaining C23 fixture execution; the explicit test Product has no publisher-source qualification. |
| Final combined source, all 30 Go files | `go build ./...` and `go vet ./...` PASS at 20:11:27 and 20:11:32 UTC; `gofmt` clean. | All 30 local file hashes matched both Windows source trees: 60 SHA-256 comparisons. No executable under the owned audit root remained running at 20:11:32 UTC. This final verification covers the eight code commits through `94a75f65`; the following plan-only commit changes no executable source. |
| Recovery namespace GOTMPDIR correction | 3 / 6 PASS; launcher exit 0. | Actual retained-directory and failure-path measurements both report 148 handles before and 148 after. The earlier 147→148 failure is retained as an unsuccessful observation, not retroactively attributed to a proven leak. WrongOwner assignment was actually refused with Win32 1307 and remains UNVERIFIED. |
| Qualified Job rerun on current source | Both selected top-level tests and the required native subtest PASS; controller exit 0. | Exact source/binary binding, original root-only membership, READY→GRANT→ACK, stdout/stderr EOF, zero final active members and closed original handles were observed. The controller did not require a terminal action. This does not make the ordinary unqualified `go test ./...` invocation pass. |
| Original F01–F08 candidate | Full `internal/binaryadmission`, `internal/cbuild`, `cmd/mcphub`, `cmd/mcphub-launcher`, `tools/windows-product-validate` passed; affected CLI 77 / 102, zero skips. | Earlier scoped baseline, including preparation/rollback/peer/replay/fence regressions. This is not a fresh full-suite result for every continuation draft. |
| Real built complete-set transaction | 1 / 2 PASS. | Actual isolated Product/launcher transaction fixture is now available and executed; the old missing-fixture gap is closed for this scenario. |
| Independent native replay | 2 / 6 PASS. | Actual native supervisor identity/replay/settlement. GUI health observation, reservation and `StartGUI` use headless substitutions, so this establishes continuation to the GUI-start boundary, not actual GUI startup/health or Task Scheduler integration. |
| Qualified GUI test Product commands | Final separate command run: 8 / 11 PASS, zero skips, 138.6 s; first-journal 1 / 3 PASS. | Real built GUI-subsystem test Product and original owners. An earlier combined selection hit its 180 s bound during added preflight. The command selection then completed separately with exit 0; preflight also passed separately. |
| Retained-original preflight rerun | 1 / 11 PASS in 32.3 s. | Separate bounded rerun covers create/replace/identical, repeated held access and path/context/closed-owner refusal, with actual sharing guard. |
| Client read-only draft #618 | Both native client-lock and GUI project-scan selections PASS, including `TestProjectsScan_NoWrite`. | `TestEntryRemoval`, `TestConfigObservation`, `TestConfigLock_`, `TestWithConfigLock_` and `TestProjectsScan_` selectors; published bytes match the Windows-tested candidate. No whole-package GUI claim. |
| Standalone process core, before integration | 12 / 22 PASS, zero skips. | Focused native custody and contained-run selection. |
| Qualified Job controller | 5 / 32 PASS. | Controller/protocol refusal and ownership contracts. |
| Standalone actual native Job fixtures | 2 / 6 PASS. | Original root-only membership, actual READY→GRANT→ACK, native lifetime closure and original-owner cleanup; no extra console member accepted. |
| Standalone Kimi local fixtures, before integration | 5 / 12 PASS, zero skips. | Local doubles, retained primary cause and stubborn-child cleanup; no paid model execution. |
| Follow-up B27 on #616 | Same new consumer regression: baseline `8c4cdaeb` fails 1 / 2 with the reported false TIMEOUT/CANCELLED; corrected candidate passes 1 / 2. | The wrapper completed successfully before cleanup crossed the work deadline or parent cancellation. A separate deterministic producer test confirms the recorded success survives cleanup and closes each resource once. |
| Follow-up B27 regression scope | Kimi selection: 21 / 57 PASS; contained-process selection: 21 / 8 PASS; `go vet ./internal/kimi` PASS. | That historical Kimi selection excluded the older `TestRunWithContainedStreamSettlesStubbornChild` fixture; its ownership repair was then in #620 and was subsequently incorporated into #616. This is not a fresh full-package Kimi claim or full-process vet claim for #616. No model/provider was run. |
| Integrated #620 on #616 | Full Kimi: 21 / 55 PASS; process selection: 30 / 24 PASS; controller: 5 / 32 PASS; final process/Kimi `go vet` PASS. | Preserves both receipt role/size/cancellation contracts and retained execution causes; covers suspended-start/admission/cancellation ordering. No paid provider run. |
| Integrated readiness-sharing regression | 1 / 3 PASS. | Actual Windows share-denying handle: release succeeds, retained handle reaches the original deadline, malformed JSON fails immediately. An earlier qualified integration run exposed this sharing conflict and failed; C17 fixes it. |
| Integrated actual native Job rerun | 2 / 6 PASS, zero skips; controller exit 0. | Final source includes C17 and the three conflict resolutions; native source/binary binding, original ownership, EOF and terminal settlement retained. |
| Static analysis / other Windows architectures | `go vet` for process/daemon/Kimi PASS; 386 and arm64 compile-only PASS. | Clears the affected vet diagnostics and compile-time ABI assertions. Native execution evidence is Windows/amd64 only. |
| Daemon final 15-source candidate | Full selection excluding CST: 274 / 79 PASS; 2 optional skips. | Exact final source verified, including ownership and deterministic lazy-notification fixture. CST is a separate blocked execution below. |
| CST native frontend | Actually attempted; blocked because MSVC x64 is missing; `vswhere` returned `[]`. | No frontend integration PASS; no compiler installation or policy change. This is not hidden among the two optional daemon skips. |
| Portable CLI final selections | 84 / 105 PASS; remaining simple selection 8 / 2 PASS. | Selected repaired fixture contracts; not the entire CLI package or the old positive Serena migration set. |
| Recovery namespace harness | 3 actual tests ran PASS; ordinary-token WrongOwner assignment refused → that branch UNVERIFIED. | Real snapshot/junction/retained-directory/handle evidence; no privilege elevation. Passing surrounding tests do not qualify the refused owner-change branch. |
| Secrets final selection | 13 / 4 PASS; 1 existing Windows chmod skip. | Correct common private-root setup and targeted recovery behavior; chmod-negative capability remains explicitly skipped. |
| Hyperfine final selection | 2 / 9 PASS. | Supported raw external-tool schemas/commands after targeted follow-up. |
| Scan fixture driver / environment overlay | Driver 6 PASS; overlay 1 PASS. | Specific port/transport and overlay regressions. |
| Earlier API adopt / perftools selections | Earlier passing subsets: adopt 12 / 12; perftools 47 / 26 with 4 optional skips; broader executions required follow-up corrections/checks. | Historical selections, not a single final all-pass run of either complete package. Use the final targeted rows above where applicable. |
| Serena frozen 10-file candidate | Focused API/CLI tests: 9 / 12 PASS, zero skips; `go vet ./internal/api ./internal/cli` PASS. Own and two independent source reviews completed. | Do not promote static review or pure descriptor tests to positive prepared-install evidence. See open gate below. |

The early verification used Windows/amd64, Go 1.27.3 and local caches with `GOTOOLCHAIN=local`.
A later explicitly approved `goversioninfo` 1.5.0 download populated an isolated build cache only;
the earlier blanket “nothing downloaded” statement no longer describes the complete continuation.
No MSVC was installed. Real Product fixture metadata permits an empty source epoch only in explicit
test mode; this does not qualify release provenance or release-build metadata.
The first external invocation of the new GUI-subsystem test Product did not
collect an exit status or test output because the shell invocation did not wait
for that subsystem. It is not counted as a test result. After confirming no owned
executable remained, the corrected hidden launcher waited on its original process,
captured stdout/stderr and exit 0, and confirmed the one required PASS with zero skips.

Reproduction entry points are the focused regressions named above,
`tools/test-job-entry/README.md` and
`scripts/test-recovery-namespace/README.md`, now incorporated in #616 (Table 3). They bind source/helper bytes and original native identity, use private
roots and bounded hidden processes, and retain honest unsupported/refused outcomes.
Raw machine-local logs stay out of published plans because they contain private temporary paths.

## Remaining gates and next actions

1. **Completed production correction scope:** C15 and C18–C21 were reproduced,
   corrected, independently reviewed and executed on Windows. The previously open
   independent native replay gate passes within its explicit headless GUI boundary.
   C09/C11/C12/C22/C23 fixture results are separated from production qualification
   in Table 4; none replaces the complete repository gates.
2. **Twenty-nine live-workspace Serena cases do not reach their intended oracles.**
   In the clean CLI run, 28 fail at runtime Product admission (`expected 2, actual 3`)
   and one reaches `SERENA_BUILD_SOURCE_INVALID`. Their unpinned
   `git+https://example/serena` fixture is not a valid prepared-package source.
   Selecting an inert runtime path would only move the first group to the next
   boundary. Two truly zero-workspace cases were repaired and executed separately;
   that path requires no package preparation.
3. **Full positive Serena preparation needs a supported isolated publisher fixture.**
   `build.ps1` expressly forbids combining `TestStateIsolatedProduct` with
   `SourceSnapshotPath`. Preparation checks the current process's issued source
   epoch and native Product image, so an external Product path or ordinary test
   executable cannot supply that authority. The ordinary source-marked Product has
   no sanctioned private-state/autostart test driver: production state resolution
   uses Windows Known Folder, and the corresponding test hooks reject non-test
   processes. The November 14 source review further confirms that even a marked
   Go test Product plus `SetDaemonStateRootForTest` would not isolate preparation:
   `BuildSerenaPreparationInputs` calls `ResolveDaemonStateDirObserved`, whose
   Windows implementation obtains the native Known Folder for the current token
   directly. It bypasses both the ordinary test root override and
   `test_state_path_env` before passing namespace evidence to preparation and
   `AcquireSerenaOwnedTree`. A complete integration driver must preserve both
   provenance and isolation; do not manually assign source marks or relax these
   guards. Qualify a supported observed-namespace fixture or an isolated Windows
   profile/VM before attempting this positive path on a developer machine.
   Its read-only acceptance must establish the same private allowed root for the
   observed preparation namespace and the ordinary API/CLI state resolvers.
   One qualified positive Product run would not make the 29 existing Go tests
   pass: a supported test-publisher/namespace contract or a deliberate transfer
   of their integration oracles remains a separate test-architecture decision.
   It also needs a real pinned HTTPS source, UV/Python probes, installed-tool receipt,
   package metadata and exported-file identity. The installer uses a fresh owned
   attempt/cache; there is no existing verified offline artifact-import fixture.
   Existing UV/Python availability was not inferred either way. Negative owner
   tests, protocol doubles and C15's actual committed-result test cannot qualify
   unlocked preparation → same-owner consumer → quiet-lock install → observed close.
4. **Required native prerequisites remain unavailable.** The CST frontend test
   actually failed because MSVC x64 is absent (`vswhere` returned `[]`). Seven
   mandatory Serena consent cases actually failed at symbolic-link creation because
   the ordinary token lacks the required privilege; their consent assertions were
   not reached. No compiler install, elevation, Developer Mode or policy change
   was performed. In the passing namespace run, actual foreign-owner assignment
   was refused with Win32 1307, so that separate WrongOwner branch remains UNVERIFIED.
5. **The normal `CLAUDE.md` pre-push gates remain unsatisfied.** The recorded
   untagged and tagged attempts are FAIL, including the aggregate 5-minute API/CLI
   deadlines. Isolated tests active at those deadlines subsequently passed; this
   does not make the full commands pass. Qualified namespace and Job controllers
   bind their exact test binary and original owner; those proofs cannot simply be
   transplanted into an ordinary `go test ./...` invocation. Keep this workflow
   mismatch explicit rather than weakening native tests or editing the gate to
   authorize this push.
6. **Publication and merge:** publish the reviewed correction in #616 under the
   bounded November 14 user exception recorded above. Require a new bot PASS on
   the resulting exact
   head; the PASS on `53645ec7` cannot cover these new changes. An exception limited
   to publishing fixes does not clear the remaining merge gates. Once those gates
   are satisfied, merge #616 using the repository's ordinary squash workflow,
   without reopening duplicates or bypassing protection. Do not trigger GitHub
   Actions or a paid provider run.
7. **No release certification:** native GUI startup/health, installed-version
   acceptance, vendor solvers, optional skipped capabilities and full positive
   Serena preparation are not established by the scoped results in Table 4.

## Bot review disposition

Table 5. The original 26 inline threads plus follow-ups B27 and B28. The fresh
November 13 snapshot has 28 resolved / 0 unresolved. Published head `53645ec7`
has the bot's [no-major-issues receipt](https://github.com/applicate2628/mcp-local-hub/pull/616#issuecomment-6856683088)
and no inline findings anchored to that head. This is historical review for the
new correction, not its future bot PASS. The code audit covers 27 addressed
defect/fixture claims and one false panic claim; resolution alone is not proof.

The named regressions below were inspected during this final audit. Their presence is not a claim of a new execution; actual executions and limits are in Table 4. CMake serialization is within one server instance, UNC key tests do not qualify a live network share, and recovery composition tests do not substitute for native replay evidence.

| Thread / source | Condition or affected boundary | Current disposition | Inspected regression / limit |
| --- | --- | --- | --- |
| [B01](https://github.com/applicate2628/mcp-local-hub/pull/616#discussion_r4176558972) — `internal/sonnet/server.go` | Sonnet destination-copy error | False positive of the reported nil panic: `(*os.File).Close` explicitly returns `ErrInvalid` for nil; `errors.Join` retains the original open error. | Official [Go implementation](https://go.dev/src/os/file_posix.go); no extra guard required. |
| [B02](https://github.com/applicate2628/mcp-local-hub/pull/616#discussion_r4176558974) — `internal/sonnet/server.go` | Sonnet Start/Close handoff | Recheck `closed` under the final lock; preparation remains in flight and retains the store lease until settled. | `TestStartRejectsPostCopyHandoffAfterClose` |
| [B03](https://github.com/applicate2628/mcp-local-hub/pull/616#discussion_r4302428866) — `internal/sonnet/server.go` | Sonnet idempotency payload | Persist and compare canonical request digest across replay, concurrent admission and restart; reject changed root/file/timeout. | `TestIdempotencyRequestBindingSurvivesRestart` |
| [B04](https://github.com/applicate2628/mcp-local-hub/pull/616#discussion_r4302428868) — `internal/sonnet/server.go` | Sonnet explicit timeout field | Persist `TimedOut=true` with the timed-out terminal state. | `TestTimedOutFlagPersistsWithTerminalState` |
| [B05](https://github.com/applicate2628/mcp-local-hub/pull/616#discussion_r4302428870) — `internal/kimi/receipt.go` | Kimi primary outcome consistency | Require nested exit/status/gate to equal the top-level outcome. | `TestProviderReceiptRequiresMatchingPrimaryOutcome` |
| [B06](https://github.com/applicate2628/mcp-local-hub/pull/616#discussion_r4304598352) — `internal/cbuild/parse_output.go` | CMake preset/cache binding | Require a settled exact preset binding and revalidate its source/cache paths; no arbitrary cache discovery. | `TestCmakeCleanRejectsUnboundPreset`; `TestCmakeCleanRevalidatesSettledCache` |
| [B07](https://github.com/applicate2628/mcp-local-hub/pull/616#discussion_r4304787175) — `internal/sonnet/server.go` | Sonnet output settlement lock | Hash output and summarize logs outside the global mutex while retaining the in-flight worker/lease. | `TestOutputSettlementDoesNotHoldServerMutex` |
| [B08](https://github.com/applicate2628/mcp-local-hub/pull/616#discussion_r4304787179) — `internal/cbuild/parse_output.go` | CMake configure/clean serialization | Hold the cancellable project operation lock through the complete operation, including different presets sharing a tree. | `TestMutatingToolsWaitForProjectOperation`; `TestConfigureInvalidatesBindingsBeforeUnsettledResult` |
| [B09](https://github.com/applicate2628/mcp-local-hub/pull/616#discussion_r4304787183) — `internal/cbuild/handlers.go` | CTest partial capture | Propagate truncation and always expose partial/output-truncated flags separately from process success. | `TestCTestReportsCaptureTruncation` |
| [B10](https://github.com/applicate2628/mcp-local-hub/pull/616#discussion_r4305536031) — `internal/cbuild/handlers.go` | CMake build serialization | Build uses the same project lock and remaining operation budget. | `TestMutatingToolsWaitForProjectOperation/build` |
| [B11](https://github.com/applicate2628/mcp-local-hub/pull/616#discussion_r4305536036) — `internal/sonnet/server.go` | Sonnet recordless workspace reaping | Require the store lease, owned run namespace and old content throughout; preserve recent/irregular/corrupt-record trees. | `TestStartupReapsOnlyExpiredOwnedRecordlessWorkspaces`; `TestRecordlessSweepPreservesCorruptSnapshot` |
| [B12](https://github.com/applicate2628/mcp-local-hub/pull/616#discussion_r4305536040) — `internal/kimi/receipt.go` | Kimi assigned/execution role | Bind the receipt to the requested role and permitted execution lane. | `TestProviderReceiptBindsRequestedRoleAndExecutionLane` |
| [B13](https://github.com/applicate2628/mcp-local-hub/pull/616#discussion_r4305599810) — `internal/api/adopt.go` | Project adoption repeat identity | Compare canonical project keys while retaining fingerprint, manifest and unique-target checks. | `TestProjectAdoptRecordMatchesCanonicalProjectIdentity`; `TestProjectAdoptExactRepeatIsVerificationOnly` |
| [B14](https://github.com/applicate2628/mcp-local-hub/pull/616#discussion_r4305689966) — `windows-product-artifacts.json` | Windows waiting entry point | Root console launcher waits for the held nested GUI Product; public Node shim uses synchronous spawning and one bin owner. | `TestLauncherUsesHeldProductAndBorrowedStandards`; npm child argv/stdout/stderr/exit test |
| [B15](https://github.com/applicate2628/mcp-local-hub/pull/616#discussion_r4305689968) — `internal/kimi/receipt.go` | Kimi failed gate | A non-PASS gate is an error even with exit zero/completed status. | `TestProviderReceiptFailedGateIsErrorEvenWithSuccessfulExit` |
| [B16](https://github.com/applicate2628/mcp-local-hub/pull/616#discussion_r4305689970) — `internal/sonnet/server.go` | Sonnet Windows path identity | Case-fold Windows request identity keys; preserve actual paths and POSIX case sensitivity. | `TestRequestHashPathCaseFollowsPlatformIdentity` |
| [B17](https://github.com/applicate2628/mcp-local-hub/pull/616#discussion_r4305710924) — `internal/cbuild/handlers.go` | vcpkg mutation serialization | Install shares the project operation lock and budget with CMake operations. | `TestMutatingToolsWaitForProjectOperation/vcpkg_install` |
| [B18](https://github.com/applicate2628/mcp-local-hub/pull/616#discussion_r4305710928) — `internal/sonnet/server.go` | Sonnet replay before discovery | Read and verify durable replay before locating a product needed only for a new execution. | `TestReplayDoesNotRequireProductDiscovery` |
| [B19](https://github.com/applicate2628/mcp-local-hub/pull/616#discussion_r4307024306) — `internal/kimi/runner.go` | Kimi cancellation versus stale receipt | Attributed timeout/cancellation blocks receipt intake, even when a syntactically successful receipt already exists. C07 separately prevents later cleanup timing from changing attribution. | `TestRunWithDependenciesMapsTimeoutCancelAndCleanupFailure`; retained-cause regressions now in #616, with the same production attribution as #620 |
| [B20](https://github.com/applicate2628/mcp-local-hub/pull/616#discussion_r4307024308) — `internal/kimi/runner.go` | Kimi bounded receipt intake | Enforce an 8 MiB limit with regular-file/same-object checks and a limit+1 reader; preserve close failures. | `TestReadReceiptFileSizeBound`; concurrent growth is bounded at the reader, not claimed as a separate executed test |
| [B21](https://github.com/applicate2628/mcp-local-hub/pull/616#discussion_r4307341108) — `internal/cbuild/parse_output.go` | CMake Windows/UNC key case | Use one case-normalized project key for binding insertion, lookup, invalidation and operation locking. | `TestProjectPathKeyCasePolicy`; `TestConfiguredBindingUsesNormalizedSourceKey` |
| [B22](https://github.com/applicate2628/mcp-local-hub/pull/616#discussion_r4307549501) — `internal/cbuild/handlers.go` | CMake purge deadline | Keep the internal operation deadline through validation and check before deletion; report late completion honestly. | `TestPurgeRefusesExpiredBudgetAfterBindingWait`; synchronous deletion itself is not interruptible |
| [B23](https://github.com/applicate2628/mcp-local-hub/pull/616#discussion_r4532426054) — `npm/packages/win32-x64/package.json` | npm nested Product payload test | Expect launcher, nested Product, root identity and README, without a second platform bin owner. | `Windows platform payload has the launcher, nested product, and identity manifest with no bin owner` |
| [B24](https://github.com/applicate2628/mcp-local-hub/pull/616#discussion_r4536597930) — `internal/cli/canonicalize_product_windows.go` | Fresh canonical parent directory | Native Stage opens/creates missing ordinary parents with retained handles and refuses redirected parents. | `TestCanonicalEntryStageCloseBeforeTakeCleansOnlyOriginalStages`; `TestCanonicalEntryStageRefusesRedirectedInstallationDirectory` |
| [B25](https://github.com/applicate2628/mcp-local-hub/pull/616#discussion_r4536597935) — `windows-product-artifacts.json` | Explicit complete/pending recovery layout | Use package-root sidecar only after adjacent ENOENT; validate complete-set targets and wire nested Product/runtime requests for both policies. | `TestRecoveryExecutorIdentityUsesPackageRoot`; `TestRecoveryCommandsForwardCompleteSet` |
| [B26](https://github.com/applicate2628/mcp-local-hub/pull/616#discussion_r4536597945) — `internal/cbuild/handlers.go` | CTest channel snapshot | Keep exact domain/text/structured channel comparisons with both new serialized flags and raw tail. | `TestExecutionResultsRetainRawTailInMCPChannels` |
| [B27](https://github.com/applicate2628/mcp-local-hub/pull/616#discussion_r4540002778) — `internal/kimi/runner.go` | Successful work followed by cleanup crossing the work deadline or parent cancellation | Classify the contained runner's retained outcome once; nil remains success. Distinguish actual execution cancellation with `ContainedStageContext`, preserve cleanup causes independently and continue rejecting stale receipts from interrupted work. | `TestRunWithDependenciesPreservesCompletedOutcome`: fails both cases before the fix and passes after; `TestRunContainedStream_SuccessBeforeContextExpiresDuringCleanup` confirms the producer contract; current source composes cleanly with #620. |
| [B28](https://github.com/applicate2628/mcp-local-hub/pull/616#discussion_r4543557544) — `internal/sonnet/server.go` | Output hashing/log settlement crosses the work deadline after EM already completed | Capture the execution outcome before postprocessing; preserve exit code, reportability and typed failure across output settlement and restart. Already incorporated as `6875fe6f`. | `TestOutputSettlementDeadlinePreservesExecutionOutcome`, covering completed, nonzero and cleanup-failed outcomes; own source audit confirms the retained classification precedes `beforeOutput`. |

## Terms and abbreviations

- PR: pull request, a proposed source merge; PASS means the named executed scope passed.
- Product: nested GUI executable `mcphub-product/mcphub.exe`; launcher: root waiting CLI executable `mcphub.exe`.
- GUI / CLI / API: graphical user interface / command-line interface / application programming interface.
- Native ownership: retained Windows handles and verified original object identity; a pathname or PID alone is insufficient.
- PID / FILETIME: process identifier / Windows creation-time representation; together identify the observed process generation.
- Job: Windows process-containment object; READY/GRANT/ACK: fixture readiness, controller authorization and acknowledgement.
- ABI: application binary interface, including native structure sizes, member offsets and pointer-width representation.
- DACL: Windows discretionary access-control list; reparse point: filesystem redirection metadata observed without following its target.
- SHA-256: 256-bit cryptographic hash used to bind bytes; EOF: end of file/stream, required for completed output draining.
- Checkpoint / cold rollback: durable intermediate journal state / restoration in a new process after interruption.
- Fixture / top-level test / subtest: isolated test state and owned processes / outer Go test / named nested test.
- KiB: 1,024 bytes; MSVC: Microsoft Visual C++ compiler toolchain; CST: the project's CST native frontend integration.
- Quiet lock: retained supervisor-exclusion lock; observed close: completion recorded while its owning operation context is still valid.
- Postcommit failure: failure after changes were applied; cleanup error does not itself authorize rollback.
- UNVERIFIED: the required native condition was not established; neither a pass nor fabricated evidence for that branch.
