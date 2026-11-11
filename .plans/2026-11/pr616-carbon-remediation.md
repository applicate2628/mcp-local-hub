# PR #616: unified CARBON remediation and continuation plan

Original review baseline: `e3c7cbabc97d218a86599b93218edabd33dee159`.
PR base and independent continuation-draft base: `76d11b33bd37e30125525ce8e0e582122f29e91b`; native PR #620 depends on #616.
PR #616 head before continuation: `2ca927ff9f759fca9d7cb96b514fcfd93341d558`.
Published continuation implementation: [`814a2b47`](https://github.com/applicate2628/mcp-local-hub/commit/814a2b47c6b2d2dfdcbca4d324fb792ff83d3a90).
The original five CARBON findings were reproduced on Windows before implementation.
This record preserves F01–F08 and adds confirmed findings from implementation, independent review and native execution.

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
Separate production corrections from fixture changes and independent pull requests (Table 3).

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
| C07 / production | `internal/kimi/runner.go`: `classifyRunFailure` | Cleanup/receipt handling outlasts the work deadline after an actual exit failure → late context reads incorrectly turn RUN_FAILED into TIMEOUT/CANCELLED. | Classify from typed primary Stage/Cause once; preserve it through cleanup faults. Only unstructured legacy evidence uses a context snapshot. Deterministic regressions and local fixture runs pass. |
| C08 / production | `internal/daemon/stdio_launch_environment.go`, `stdio_launch_preparation.go` | Windows drive-current-directory entry such as `=C:=C:\work` is split as an empty key → launch environment loses drive state. | Parse and merge the real `=C:` key under the existing canonical normalization; preserve unset/profile semantics. |
| C09 / fixtures | `internal/api/adopt_test.go`, `secrets_test.go`, `secrets_init_recovery_test.go`; `internal/cli/client_config_env_isolation_test.go`, `settings_registry_test.go` | Missing sandbox roots or inherited broad DACLs fail before the intended scenario; rehardened existing paths violate the helper's create contract. | Create required roots in order and protect the exact parent before sandbox setup; share the correctly owned secrets fixture. Keep adverse-ACL tests adverse. |
| C10 / fixtures | `internal/e2e/scan_readiness_fixture/driver/main_windows_test.go`; CLI port/restart fixtures | An allocated port is assumed above 10,000, or a request uses `127.0.0.1` rather than the configured HTTP Host → transport/auth boundary refuses the fixture. | Use a valid logical port with transport mapped to the actual ephemeral listener and send the configured Host; do not relax host authorization. |
| C11 / fixtures | `internal/daemon/backend_lifecycle_test.go`, `lazy_proxy_readiness_test.go`, Python fixtures; `internal/kimi/runner_test.go`; CLI expectations; `internal/perftools/handlers_test.go` | Legacy `StdioHost.job` assertion ignores `original`; 10 ms notification budget races cold start; child cleanup is armed too late; external Hyperfine schema or command expectations changed. | Assert actual original owner and terminal settlement; synchronize materialization before the tested notification; hide children and arm bounded cleanup before fatal paths; validate supported raw schema and current command contract. Preserve negative abort ordering. |
| C12 / fixture qualification | `tools/test-recovery-namespace`, `scripts/test-recovery-namespace`; `tools/test-job-entry`; `internal/process/jobobject_windows_test.go` | Scalar claims or renewed terminal deadlines cannot prove native custody; ambient helper variables or mutable ancestry could escape the fixture scope. | Real native handles/identity/ACL/reparse observations; pinned source and private roots; reject ambient fixture sentinels; bounded original-owner lifecycle. One actual closure admission/deadline and READY→GRANT→ACK evidence. No fabricated proof or security-policy change. |
| C13 / production | `internal/api/serena_parsed_preparation.go`; `internal/cli/migrate_serena.go` | Migration invokes parsed installation under the quiet lock through a fresh API without a preacquired prepared invocation → the live-workspace path refuses with `SERENA_PARSED_PREACQUIRED_INVOCATION_REQUIRED`. | Prepare with the same owner before the quiet lock; retain immutable parsed input/request and use `WithConsumerInvocation` for the exact consumer descriptors. |
| C14 / production | `internal/api/runtime_product_interlock.go`, `admission_check.go`, `install.go`, `install_parsed_manifest.go` | Quiet-lock admission still expects live supervisor control, or a local child context is cancelled before observed close → valid held installation is refused. | Verify the actual quiet lock and native Product image; thread that resolver and the live bounded invocation context through install and observed close. Never substitute lock text for process authority. |
| C15 / production | `internal/cli/migrate_serena.go`: prepared install result handling | Installation commits but invocation close fails → conflated errors can trigger rollback of the committed result. | Preserve separate install and cleanup causes; treat observed-close failure after application as postcommit failure without rearming rollback. |
| C16 / production | `internal/api/install_parsed_manifest.go`; `serena_parsed_scope_test.go` | One/all workspaces disappear before the initial guard or final live filter → publication can use a descriptor set different from the prepared invocation, including an empty set. | Validate incoming invocation even for empty selection; compare the final materialized descriptors exactly before merge/publication. Tests preserve prior/sibling state. Positive full preparation remains an open gate. |
| C17 / fixture | `internal/process/job_options_windows_test.go`: `awaitJobFixtureJSON` | Native integration observed `ERROR_SHARING_VIOLATION` while polling an atomically published readiness frame → the fixture fails before testing creator closure. | Retry only that transient native error and missing-file errors within the existing absolute deadline; keep malformed frames and other errors fail-fast. Native held-file release/expiry/malformed regressions pass (1 / 3); qualified Job rerun passes (2 / 6). |

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
4. Path/hash/disposition checks complement retained original authority. They do not authorize a cold
   reopen of locked Stage/Prior members or an early close to let the next operation succeed.
5. Identity-v1 has one 65,536-byte limit and explicit oversize failure, never truncation.
   The original normal/padded observations were 333/1,048,909 bytes; the limit does not apply to executables.
6. Preserve execution cause separately from cleanup outcome. Do not renew the admitted terminal
   deadline, repeat the original waiter, or let receipt/cleanup timing rewrite an earlier result.
7. Fixture repair must preserve the tested adverse condition and authority boundary. A hardlink fallback
   is suitable only for a shared-file-identity assertion; it does not prove symlink privilege or semantics.

## Delivery plan

Table 3. Delivery boundaries. PRs #618, #619, #621 and #622 start from master `76d11b33`.
PR #620 depends on #616 because their Kimi/Windows-startup corrections touch the same contract;
the integrated delta preserves both sets of fixes instead of duplicating the main PR.

| Delivery | Included work | State / link |
| --- | --- | --- |
| Existing recovery PR | F01–F08, original-runtime control/readiness C01, retained complete-set preflight C02 and real Product regressions. | [PR #616](https://github.com/applicate2628/mcp-local-hub/pull/616); continuation implementation `814a2b47` is published. |
| Client read-only | C03, isolated from recovery and process custody. | [PR #618](https://github.com/applicate2628/mcp-local-hub/pull/618). |
| Daemon launch / lifecycle | C08 and daemon-specific fixture corrections, exact 15-file candidate. | Published [PR #619](https://github.com/applicate2628/mcp-local-hub/pull/619), commit `e30e492db6e07c04ee8c508ae66109b73c48d6d1`. |
| Native process / Kimi | C04–C07, C17, native Job qualification and associated process/runner regressions. | [PR #620](https://github.com/applicate2628/mcp-local-hub/pull/620), based on #616; integrated commit `62a7b0dc1028fe0545bcf678b50598d4be59a22a`, 22-file delta. |
| Portable fixtures / namespace | C09–C12 outside the daemon/native delivery, portable contracts and recovery-namespace harness. | [Fixture draft](https://github.com/applicate2628/mcp-local-hub/pull/621); supported/refused namespace branches reported separately. |
| Serena invocation | C13–C16 and focused context/ownership/scope tests; frozen 10-file candidate. | [Serena draft](https://github.com/applicate2628/mcp-local-hub/pull/622); positive prepared-install qualification remains open. |

Keep each source change in its responsible delivery; do not duplicate daemon fixes in the native PR.
Published Git tree hashes were compared with the isolated local indexes and the Windows-tested source bytes. Existing line endings follow each delivery base: three existing Windows files retain CRLF; the integrated `start_with_job_windows.go` keeps #616's LF convention so its actual change remains five lines.
Own review follows each substantive change; independent review supplements it. No merge or release is authorized by this plan.

The combined source integration was checked with `git merge-tree`: #616 → #618 → #619 → integrated #620 → #621 → #622 merges without conflicts. The resulting source tree is `358fbb631b709d8ff6ecd30495b7814f661ab14d` before this plan-only update. This is a source-composition check, not a runtime test of the whole combined repository.

## Verification record

Table 4. Actual recorded executions. Counts are top-level tests / subtests; overlapping selections must not be summed.

| Scope / candidate | Observed result | What it establishes and limits |
| --- | --- | --- |
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

Reproduction entry points are the focused regressions named above,
`tools/test-job-entry/README.md` in the native draft and
`scripts/test-recovery-namespace/README.md` in the fixture draft (Table 3). They bind source/helper bytes and original native identity, use private
roots and bounded hidden processes, and retain honest unsupported/refused outcomes.
Raw machine-local logs stay out of published plans because they contain private temporary paths.

## Remaining gates and next actions

1. **Serena positive preparation/install remains unqualified.** The focused 9 / 12 result is recorded above. Obtain a real prepared-package fixture for unlocked preparation →
   same-owner consumer invocation → quiet-lock install → observed close/postcommit handling.
   The old 21 positive migration fixtures are not qualified by metadata projection or negative-owner
   tests; keep this gap visible until those scenarios execute against the actual contract.
2. **CST frontend remains blocked by MSVC x64.** Preserve the actual failed prerequisite observation.
   Re-run only with an available authorized toolchain; do not install one as part of this continuation.
3. **WrongOwner remains UNVERIFIED on the current ordinary token.** One native owner-change attempt
   was refused. Retain that outcome; do not elevate, adjust privileges or weaken namespace policy.
4. **Publication evidence.** Preserve the coherent PR boundaries in Table 3 and the actual test limits.
   Original line endings follow each delivery base to avoid unrelated diff noise; the affected
   native daemon tests (10 / 7) passed again after restoring CRLF. Integrated process evidence is
   recorded separately from the earlier standalone qualified Job result.
   The earlier combined Product timeout remains recorded alongside the final successful separate runs.
5. **No whole-repository or release certification.** Earlier full-suite failures were attributed and
   addressed in the scopes above; no final all-package run is claimed. Optional skips, blocked native
   prerequisites, compile-only architectures and unqualified positive fixtures remain explicit.

## Bot review disposition

Table 5. All 26 inline bot threads, re-read against the published #616 source. GitHub reports 26 resolved / 0 unresolved. The code audit confirms 25 addressed defect/fixture claims and one false panic claim; resolved status alone is not used as proof.

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
| [B19](https://github.com/applicate2628/mcp-local-hub/pull/616#discussion_r4307024306) — `internal/kimi/runner.go` | Kimi cancellation versus stale receipt | Attributed timeout/cancellation blocks receipt intake, even when a syntactically successful receipt already exists. C07 separately prevents later cleanup timing from changing attribution. | `TestRunWithDependenciesMapsTimeoutCancelAndCleanupFailure`; integrated retained-cause regressions in #620 |
| [B20](https://github.com/applicate2628/mcp-local-hub/pull/616#discussion_r4307024308) — `internal/kimi/runner.go` | Kimi bounded receipt intake | Enforce an 8 MiB limit with regular-file/same-object checks and a limit+1 reader; preserve close failures. | `TestReadReceiptFileSizeBound`; concurrent growth is bounded at the reader, not claimed as a separate executed test |
| [B21](https://github.com/applicate2628/mcp-local-hub/pull/616#discussion_r4307341108) — `internal/cbuild/parse_output.go` | CMake Windows/UNC key case | Use one case-normalized project key for binding insertion, lookup, invalidation and operation locking. | `TestProjectPathKeyCasePolicy`; `TestConfiguredBindingUsesNormalizedSourceKey` |
| [B22](https://github.com/applicate2628/mcp-local-hub/pull/616#discussion_r4307549501) — `internal/cbuild/handlers.go` | CMake purge deadline | Keep the internal operation deadline through validation and check before deletion; report late completion honestly. | `TestPurgeRefusesExpiredBudgetAfterBindingWait`; synchronous deletion itself is not interruptible |
| [B23](https://github.com/applicate2628/mcp-local-hub/pull/616#discussion_r4532426054) — `npm/packages/win32-x64/package.json` | npm nested Product payload test | Expect launcher, nested Product, root identity and README, without a second platform bin owner. | `Windows platform payload has the launcher, nested product, and identity manifest with no bin owner` |
| [B24](https://github.com/applicate2628/mcp-local-hub/pull/616#discussion_r4536597930) — `internal/cli/canonicalize_product_windows.go` | Fresh canonical parent directory | Native Stage opens/creates missing ordinary parents with retained handles and refuses redirected parents. | `TestCanonicalEntryStageCloseBeforeTakeCleansOnlyOriginalStages`; `TestCanonicalEntryStageRefusesRedirectedInstallationDirectory` |
| [B25](https://github.com/applicate2628/mcp-local-hub/pull/616#discussion_r4536597935) — `windows-product-artifacts.json` | Explicit complete/pending recovery layout | Use package-root sidecar only after adjacent ENOENT; validate complete-set targets and wire nested Product/runtime requests for both policies. | `TestRecoveryExecutorIdentityUsesPackageRoot`; `TestRecoveryCommandsForwardCompleteSet` |
| [B26](https://github.com/applicate2628/mcp-local-hub/pull/616#discussion_r4536597945) — `internal/cbuild/handlers.go` | CTest channel snapshot | Keep exact domain/text/structured channel comparisons with both new serialized flags and raw tail. | `TestExecutionResultsRetainRawTailInMCPChannels` |

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
