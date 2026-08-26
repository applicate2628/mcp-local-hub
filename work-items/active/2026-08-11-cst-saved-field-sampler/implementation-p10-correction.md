# P10 AR-C4-01 Implementation Correction Gate

Date: 2026-08-12
Execution role: `$backend-engineer` integration owner
Scope: bounded architect-escalated AR-C4-01 correction only; no P11, target CST, Line10, Git history, publication, hub, fleet, or vendor-process mutation
Candidate base: `ea40f2c31dc46271777f9d796225d25fda305603` (C4, local, unpublished)

## Accepted Inputs

| Input | SHA-256 |
|---|---|
| Plan | `D01FEC17BACBE6AC4C81E98B807C1E6A70EF937406376DC2F1CF6822794323E3` |
| Design | `F6BF0EDB230236683A086465D83D77B90B1F0ADF1382744D7F3A098DDFB16DE5` |
| Decision | `636295710EF42295C01D6D5E7F97667D017C72EDBDF5C13C83F17C9DC42179DA` |
| Architecture implementation review | `A714A6B73D048F0E1D6DD517A9DC734B8082D5D6D1E381B6BFFC9B3A35552462` |
| QA implementation review | `961346E41C4C1EA88490C05CF2D0D04AAC19F0AF2E5FE582A5560AC6D03E1586` |
| Security implementation review | `805AF15F98D68A3E647466BF86C0AE64BE153F633D4835A3B6596E24E479A11B` |
| C4 architecture review | `CBD03DD01F237D0BB01223942CF562361C7610862B07ABE5FCC948AFDAC0B17A` |
| Prior correction artifact | C3-bound predecessor retained in repository history; this artifact supersedes its terminal verdict. |

## Repository Orientation and Preserved Boundaries

The live mutable surface is `servers/electromagnetics-mcp/`, governed by the accepted plan/design and the package `pyproject.toml`. The correction changes only saved-field owners and their tests. It leaves the existing six tool bodies, Go hub/process ownership, dependency manifests, catalogue pin, target workflows, status, ledgers, bugs, decisions, plans, and review artifacts untouched. Unrelated working-tree changes in `internal/api/port_alloc_excluded_windows.go`, `work-items/README.md`, and other work-items were preserved byte-for-byte by this lane.

## Diagnostic Premises and RED Receipts

| Receipt | Exact command | RED result | Root cause proved |
|---|---|---|---|
| P10 owner/route aggregate | `uv run --frozen --python 3.13 pytest -q tests/test_cst_saved_field_p10_correction.py` | `8 failed, 1 passed` | Incomplete settlement accepted; arbitrary failure identifier accepted; expired budget entered the application; workspace was implicitly pre-created; trusted workspace owner was absent; startup proof was optional; ordinary denial quarantined; normal request emitted no first-instruction proof. |
| Held-handle policy | `uv run --frozen --python 3.13 pytest -q tests/test_cst_saved_field_policy.py -k "one_verified_handle or exact_allowlist"` | `2 failed` | Policy bytes were not consumed from the verified held handle and access control used no exact allowlist parser. |
| Bounded writer | named reader-cancellation guard before production edit | failed: no `write_request` owner and no bounded writer join | Synchronous request `WriteFile` could consume the whole invocation outside settlement ownership. |
| C2 direct owner aggregate | `uv run --frozen --python 3.13 pytest -q tests/test_cst_saved_field_p10_c2_correction.py` | `6 failed, 1 passed`; the strengthened depth oracle then also failed before production correction | Normal calls lacked proof-before-request sequencing; helper read authority input before proof; ambiguous vendor creation reported zero owned resources; failed CRT conversion leaked the native handle; executable identity was not pinned; recursive enumeration entered beyond the admitted depth. |
| Concrete helper capability path | `uv run --frozen --python 3.13 pytest -q tests/test_cst_saved_field_helper.py::test_wired_helper_composition_uses_one_workspace_owner_and_truthful_settlement --tb=short` | failed with `cst_saved_field.activation_failed` | The helper constructed `model.cst/Result/...` while the copied CST result bundle is `model/Result/...`; the capability-relative path owner, not a fallback path reopen, was corrected. |
| Fresh FastMCP composition | `uv run --frozen --python 3.13 pytest -q tests/test_cst_saved_field_composition.py --tb=short` | four initial failures at the installed `ToolManager.add_tool` boundary | The test consumer incorrectly treated an installed `Tool` record as a callback; inspection of the installed 1.29.0 surface corrected the oracle to add `tool.fn`, after which actual off/on composition passed. This was an oracle RED, not a production defect claim. |
| C3 workspace capability | deterministic root/child name-swap plus wide-directory probe | `CREATE_SWAP replacement_child=True original_child=False`; `SETTLE_SWAP replacement_deleted=True original_preserved=True`; wide directory materialized 1000 rows before rejecting `max_entries=3` | Workspace create/delete used path names after capability proof, and Win32 enumeration accumulated all rows before checking the admitted entry budget. |
| C3 absolute deadline | expired invocation plus intentionally blocked request factory | expired invocation entered executable pin; blocked factory remained beyond 20 seconds until the diagnostic runner was terminated | Executable hash and request construction were outside one admission-anchored cancellable 60+10-second owner. The two synthetic helper processes left by that forced diagnostic termination were later identity-checked and removed; the corrected real probe leaves none. |
| C3 real NTFS policy | `.venv\\Scripts\\python.exe -m pytest -q tests/test_cst_saved_field_p10_correction.py::test_saved_field_trusted_root_policy` | `WinError 38` on ordinary directory `FileStreamInfo`; after accepting documented empty-stream EOF, pointer-width ctypes failures surfaced at `OpenProcessToken` and `EqualSid` | Directory stream absence is a valid EOF result, and the security API owner lacked exact `HANDLE`/pointer-width signatures. |
| C3 settlement retry | exact guard `test_saved_field_workspace_factory_transaction` after adding a fail-once capability deleter | RED: `attempts == [1]`, expected `[1, 1]` | `WorkspaceLease` marked itself settled before deletion succeeded, preventing retry and making the receipt untruthful. |

The RED tests exercised the direct production owners. No historical pre-production RED is claimed. Where candidate C already rejected a case, the prior containment artifact's deterministic mutation/revert receipts remain the sensitivity evidence; P10 does not relabel them.

## Corrected Ownership Model

| Owner | Corrected invariant | Falsifying probe |
|---|---|---|
| `WindowsPolicyPlatform` / `WindowsPathIdentityV1` | Policy bytes, root identity, owner and exact effective allowlist are derived from the same held handle. Directory `FileStreamInfo` `ERROR_HANDLE_EOF` is accepted only as an exact empty stream inventory. Current owner, `SYSTEM`, and Administrators are the only allowed identities; the native API signatures retain full pointer width. | Real NTFS empty-stream/ACL guard, same-held-file mutation oracle, and foreign security-identifier access-control oracle. |
| `_WindowsRelativeFileOwner` | Source and destination children are opened component-by-component relative to a held directory handle through `NtCreateFile(OBJECT_ATTRIBUTES.RootDirectory, relative UNICODE_STRING)`. Directory rows stream from bounded `NtQueryDirectoryFile` buffers and budget checks run before each yield/allocation. | Safe Windows root-swap test plus `max_entries=3` streaming oracle proves no path reopen or all-rows materialization. |
| `TrustedWorkspacePolicy` | One injected owner creates and hardens the child relative to the retained root handle, proves it, transfers it, and recursively deletes that exact retained capability. Settlement becomes true only after deletion succeeds. | Root/child replacement swap, real NTFS create/delete, fail-once settlement retry, and factory boundary failure matrix. |
| `AbsoluteInvocationBudget` | One admission monotonic start covers executable open/hash/revalidation, descriptor factory, inventory, copy, launch, bounded write/read, activation, sampling, validation and publication. Request construction runs in the same bounded `CancelSynchronousIo` worker contract and the 70-second cleanup envelope never resets the 60-second success deadline. | Expired-before-pin oracle, all-stage expiry matrix, blocked factory/writer/readers, and no-success-after-expiry checks. |
| `WindowsContainedInvocation` | Every normal call requires first-instruction Job/standard-handle/no-console/no-breakaway proof; mandatory native closes, joins, accounting and receipt values are observed rather than asserted. | Real safe Win32 containment, tuple mutation, first-instruction, timeout, residual-tree and close-failure tests. |
| `CallSettlement` / `SamplerAdmissionGate` | Complete truthful success receipt is mandatory. Ordinary pre-resource denial remains a stable denial; only unproved owned settlement latches quarantine. Validation and publication occur before lease release. | Receipt-field mutation matrix, route matrix, blocked-window oracle and quarantine linearization. |
| MCP boundary | Only a closed failure allowlist and bounded redacted response can cross the boundary; vendor/path/process identifiers are never published. | Canary matrix and path-shaped identifier tests. |
| `_NativeHandleLedger` / `_BoundedIoWorker` | Each pipe/process/thread handle is owned immediately after creation. Blocking reader/writer work is cancelled and joined inside the 60+10-second envelope; a close failure remains owned and therefore quarantines. | Nine partial-allocation prefixes, close-failure retention, and engineered blocked-I/O cancellation. |
| Restart composition root | Missing snapshot, workspace policy, or runner leaves the exact three-tool CST baseline. A complete tuple adds exactly one saved-field tool. | Fresh off/on FastMCP composition: 3 CST off, 4 CST on, 7 total with the unchanged 3 HFSS tools. |

## Review Finding Disposition

| Finding | Disposition and evidence |
|---|---|
| AR-I-01 | Closed: helper no longer pre-creates a temp directory. One `TrustedWorkspacePolicy` creates and transfers one lease; concrete injected-vendor success traverses the real helper composition. |
| AR-I-02 | Closed: production policy, source, destination, workspace and vendor paths use the shared identity grammar and held-handle-relative owner; name-based authority helpers were removed. |
| AR-I-03 | Closed: policy bytes and root proof come from the same verified handle; child content is opened relative to a retained root handle. |
| AR-I-04 | Closed: `TrustedWorkspacePolicy` is injected from the composition root and is the sole workspace owner. |
| AR-I-05 | Closed: one absolute deadline reaches every phase; request writing is bounded and joined inside settlement. |
| AR-I-06 | Closed: policy/descriptor/input denial is non-quarantining; quarantine is restricted to unproved owned settlement. |
| AR-I-07 | Closed: helper/application settlement receipts derive from actual cleanup and every mandatory field is validated on success and failure. |
| AR-I-08 | Closed: first-instruction proof is emitted and required on every normal call, not only a synthetic branch. |
| AR-I-09 | Closed: vendor identifiers are closed-format values and public failure identifiers are clamped to the safe allowlist. |
| AR-I-10 | Closed: all 32 accepted guard names resolve one-to-one to collected executable tests below. |
| QA-01 | Closed: access-control validation is an exact current-owner/`SYSTEM`/Administrators allowlist and rejects any arbitrary security identifier entry. |
| QA-02 | Closed: `test_saved_field_settlement_blocked_window` deterministically holds activation and proves zero cleanup/settlement until release, then exactly one settlement. |
| QA-03 | Closed for the implementation harness: destination mismatch, path-role, exact/one-over limit, timing, containment and resource matrices are persistent and green. Target-only CST evidence remains P11+. |
| QA-04 | Closed: absent/disabled/malformed/revision-stale/quarantined, hub/bare/direct/gate-off, protocol, denial, failure and success routes have stable assertions. |
| SI-H-01 | Closed: one held-handle identity/access/relative-transfer owner replaces path reopens, Windows `O_NOFOLLOW` fiction and ambient workspace creation. |
| SI-H-02 | Closed: complete observed settlement is mandatory and incomplete/false/native-close failure quarantines without publishing. |
| SI-H-03 | Closed: exact budget tuple is validated and one deadline covers chunked I/O, helper/vendor work and publication. |
| SI-M-01 | Closed: arbitrary helper/vendor identifiers cannot cross the closed public allowlist; canaries are redacted. |
| SI-M-02 | Closed for safe non-CST implementation: every normal helper attests first instruction; parent independently validates it and native close failures are settlement failures. Target CST descendants remain P11. |
| C2-SI-H-01 | Closed for the safe non-CST path: the helper retains the independently re-proved source root and hardened workspace capabilities; Windows enumeration uses `NtQueryDirectoryFile` on held directories, and every nested open/create uses a component-by-component `NtCreateFile` `RootDirectory` walk with reparse rejection. Transfer, destination equality, selected-field copy and generated-header verification consume that owner. |
| C2-SI-H-02 | Closed: enumeration is iterative and checks time, depth, entries and files before descent/allocation; hashing/copying check before and after each chunk; every vendor call and encoding are deadline-bracketed. The 5-second startup, 58-second response, 60-second absolute and 70-second cleanup cutoffs are now consumed rather than merely serialized. |
| C2-SI-M-01 | Closed: `PinnedExecutable` holds a non-reparse, non-writable/non-deletable executable handle, records final path/volume/file/hash/version, and revalidates before and after the exact `CreateProcessW` call. |
| C2-SI-M-02 | Closed: every successful pipe/process/thread allocation enters `_NativeHandleLedger` immediately; failed CRT ownership transfer closes the raw handle; partial-prefix and close-failure tests prove exact cleanup/quarantine state. |
| QA2-01 | Closed for executable safe non-CST evidence: the depth-before-descent oracle, per-chunk and per-vendor deadline traces, exact cutoff propagation, native allocation prefixes, behavioral blocked-I/O cancellation, capability-retention, component-swap and fresh composition tests replace the cited degenerate/static gaps. Target CST timing remains P11/P12. |
| QA2-02 | Closed: `hub` calls `FastMCP.call_tool`, `bare` calls the installed `ToolManager.call_tool`, `direct` calls the installed `Tool.run`, and `gate_off` exercises missing registration. Fresh composition separately proves 3 CST tools off and 4 CST/7 total on. All denial routes prove one safe text item and zero invoker work. |

### Final C3 review disposition

| Finding | Final correction and evidence |
|---|---|
| AR-C3-01 / C3-SI-H-01 | Closed: path `os.mkdir`/`shutil.rmtree` is no longer authoritative for the Windows production workspace. `WindowsPolicyPlatform.create_restricted_child` uses the held root as `NtCreateFile.RootDirectory`, hardens and proves the returned handle, and `delete_restricted_tree` enumerates/opens/deletes only below that retained handle. Root and child replacement swaps are behaviorally falsified; real NTFS create/delete passes. |
| AR-C3-02 / C3-SI-H-02 | Closed: the runner passes its one admission start into descriptor construction; `WindowsContainedInvocation` checks before executable pin/hash and after revalidation; the native kernel owns request construction in a bounded cancellable worker under the same `absolute_deadline` and `cleanup_deadline`. Win32 directory enumeration streams from a fixed buffer with checks before each yielded row. |
| QA3 exact guards 1/4/5/6/10/13/18/24/25/29/30/32 | Closed with executable bodies: helper and parent expiry, six budget categories, four acquisition stages, 49 source mutations plus eight destination mismatch classes, installed existing-six malformed shapes, multi-role identity mutations, both frame/factory quarantine routes, eleven settlement mutations, real NTFS ACL/capability create/delete, false-proof pre-request behavior, eleven lexical roles, five workspace factory stages, and failed-settlement retry. |
| Runtime residue | Closed: the intentionally terminated pre-fix blocking diagnostic left a two-process synthetic helper chain. Exact executable/command-line/parent identities were verified before terminating only those two PIDs. The corrected real Win32 helper probe followed immediately by a process query returned `NO_HELPER_ORPHANS`. |

## P00-P09 Acceptance Criteria Reconciliation

| AC | P10 disposition |
|---|---|
| P00-AC01 | Preserved: base candidate ownership/unpublished state verified; no index/history/publication mutation. |
| P00-AC02 | Preserved: all three historical defect oracles remain persistent; mutation sensitivity, not fabricated historical RED, is recorded. |
| P00-AC03 | GREEN: manifest add/remove/rename/replace/type/stream/size/hash/cardinality and acquisition-boundary matrix. |
| P00-AC04 | GREEN: generated ADS/device/alias coverage spans every path producer and role. |
| P00-AC05 | GREEN: existing-six contracts, full package, dependency/protected baselines preserved. |
| P01-AC01 | GREEN: absent/empty/disabled/malformed/version/path/hash/ACL cases fail default-off. |
| P01-AC02 | GREEN: valid enabled snapshot is immutable and keyed by policy identity/revision. |
| P01-AC03 | GREEN: exact and one-over bytes/entries/identifiers/limits remain tested. |
| P01-AC04 | GREEN: provisioning remains canonical default-off and restart-loaded. |
| P01-AC05 | GREEN: no lower-layer environment read or dependency addition. |
| P02-AC01 | GREEN: ordinary drive-colon canonical path is admitted. |
| P02-AC02 | GREEN: generated ASCII 1-9 and superscript 1-3 COM/LPT aliases reject. |
| P02-AC03 | GREEN: every path role requires unique long/final/volume/file/default-stream identity. |
| P02-AC04 | GREEN: swap/hard-link/short-name/case/NFC/stream/device/mapped/reparse matrix rejects. |
| P02-AC05 | GREEN implementation evidence retained; independent bug closure remains reviewer-owned. |
| P03-AC01 | GREEN: canonical UTF-8 path-byte ordering and stable aggregate. |
| P03-AC02 | GREEN: every source/project/mesh/field mutation boundary rejects before success. |
| P03-AC03 | GREEN: exhaustive destination equality matrix covers count/path/type/stream/size/hash/cardinality. |
| P03-AC04 | GREEN: create/permission/owner/access/resolution/identity/copy/verify/transfer failures settle the exact child. |
| P03-AC05 | GREEN: exact/one-over depth, entry, role and byte ceilings. |
| P03-AC06 | GREEN implementation evidence retained; independent QA closure remains reviewer-owned. |
| P04-AC01 | GREEN: exact/one-over request, response and stderr frames. |
| P04-AC02 | GREEN: malformed/truncated/oversize/schema/correlation/revision/receipt cases reject. |
| P04-AC03 | GREEN: FastMCP receives one pre-encoded `TextContent`; `structuredContent` is absent. |
| P04-AC04 | GREEN: caller/vendor/path/file/security/stderr canaries absent from public output. |
| P04-AC05 | GREEN: exact 60/5/2/10/1-second and byte/record limits remain closed. |
| P05-AC01 | GREEN: fixed module entry, fixed application/cwd/environment/argv contract. |
| P05-AC02 | GREEN: application and concrete adapter depend inward through the neutral port. |
| P05-AC03 | GREEN: resolver missing/ambiguous/exact/permutation table. |
| P05-AC04 | GREEN: only m/mm, coordinate order preserved, one exact scale. |
| P05-AC05 | GREEN: every acquisition/workspace/containment receipt field is consumed. |
| P05-AC06 | GREEN: forbidden solve/remesh/save/history/job/import edges absent. |
| P06-AC01 | GREEN safe Win32: first instruction proves exact Job, exact standard handles, no breakaway/window. |
| P06-AC02 | GREEN: every launch tuple/handle/lifetime mutation fails closed. |
| P06-AC03 | GREEN safe non-CST: stage fakes advance one monotonic budget, file/vendor loops check before and after units, and the Win32 parent enforces 5/58/60/70-second cutoffs. Target CST blocking remains P11/P12. |
| P06-AC04 | GREEN: timeout/crash/stderr/malformed/normal-residual routes terminate and settle before exposure. |
| P06-AC05 | GREEN: one-active/one-waiter latch/revision/lease/quarantine schedule. |
| P06-AC06 | GREEN: helper signal/reference close/active-zero/readers/writer/Job settlement is mandatory. |
| P06-AC07 | GREEN: no dependency/lock/Go hub or process-owner delta from this lane. |
| P07-AC01 | GREEN: complete raw-record type/length/finite/enum/path/hash/pairing validation. |
| P07-AC02 | GREEN fake trace: Result3D/frequency/save/generated-header/clean/register/select/sample/settle order. Claim 7 target truth remains P12. |
| P07-AC03 | GREEN: all pre/post-transfer failures roll back exactly once with receipts. |
| P07-AC04 | GREEN: raise-before-handle creates nothing; direct handle rollback closes exactly once. |
| P07-AC05 | GREEN: project/mesh/selected-field post-snapshot mutations reject. |
| P07-AC06 | GREEN: selected-field copy corruption rejects at `copy_field`; independent bug closure remains reviewer-owned. |
| P08-AC01 | GREEN: fresh missing/incomplete composition produces the exact baseline 3 CST/6 existing tools. |
| P08-AC02 | GREEN: fresh complete restart composition produces 4 CST/7 total tools. |
| P08-AC03 | GREEN: unknown input and non-literal `allow_solve` reject before entry. |
| P08-AC04 | GREEN: lexical denial is stable `not_authorized`, one text item and zero invoker work through actual FastMCP, manager and direct-tool routes; gate-off has no registration. |
| P08-AC05 | GREEN against installed FastMCP boundary: one `TextContent`, no structured duplicate, exact cap. |
| P08-AC06 | GREEN: README/catalogue counts and policy-on/off semantics remain truthful. |
| P09-AC01 | GREEN: all P00 oracles pass for their intended cause. |
| P09-AC02 | GREEN deterministic corruption, five-boundary foreign identity and blocked-window settlement oracles. |
| P09-AC03 | GREEN: 546 package tests, Ruff check/format across 45 files, safe Win32 and WSL import/compile. |
| P09-AC04 | GREEN: Claims 1-34 and all 32 exact guard names map one-to-one here. |
| P09-AC05 | Superseded operationally: base C3 remains local/unpublished; Lead must create corrected C4 after this PASS. No commit/index mutation in this lane. |
| P09-AC06 | Preserved: bug-record closure remains independent QA/security/Lead work, not implementation self-approval. |

## Claims 1-34

| Claim | Correction-gate evidence |
|---:|---|
| 1 | Existing six bodies/contracts and inventory remain unchanged; package suite green. |
| 2 | Fresh helper/app state and restart replay green. |
| 3 | Literal-false validation and forbidden solve/job graph green. |
| 4 | Stable source handles only; all vendor writes target committed workspace. |
| 5 | Project/mesh/selected-field pre/post hashes and mutation rejection green. |
| 6 | Exact metadata resolver, no filename inference, green. |
| 7 | **Target-only:** fake order green; installed CST activation remains P12. |
| 8 | Request order and six-component response order green. |
| 9 | Zero is `zero_ambiguous`; signed-zero matrix green. |
| 10 | Closed m/mm transform green. |
| 11 | Exact vendor-returned owned session only; foreign identity never authorizes close. |
| 12 | Complete normalized receipt consumed and emitted after actual settlement. |
| 13 | One bounded machine-neutral redacted text result. |
| 14 | No production Line10/VFEM edge. |
| 15 | **Target-only:** independent native/Line10 comparison remains P14. |
| 16 | One budget, Job, helper and bounded I/O owners; no owned residue in safe Win32 probe. |
| 17 | Acquisition transfer/rollback matrix closes exact resources once. |
| 18 | Neutral application port; no concrete vendor inversion. |
| 19 | One workspace factory owns child until complete lease transfer. |
| 20 | Handle-relative source inventory/copy plus alias/swap/hard-link rejection. |
| 21 | One 60-second absolute deadline covers all work through publication. |
| 22 | Raw vendor records fully validate before allocation/selection/session. |
| 23 | Immutable restart-loaded exact authority; all routes share one gate. |
| 24 | Timeout/residual termination proves signal/exit/close/accounting/join order. |
| 25 | Injected local non-reparse owner-restricted workspace root. |
| 26 | Every copied/generated/vendor path remains under the authorized workspace snapshot. |
| 27 | Exact `CreateProcessW` tuple, Job list, handle list and no-window/no-breakaway proof. |
| 28 | One bounded canonical request/response; correlation/revision/settlement exact. |
| 29 | Fresh helper per call; only atomic gate state crosses calls. |
| 30 | Standard-library `ctypes`; installed `NtCreateFile`/Job API surface; no dependency delta. |
| 31 | Atomic seal/recheck/revoke/quarantine across every route; ordinary denial does not latch. |
| 32 | Parent performs zero source I/O after sealed helper start; deadline covers helper publication. |
| 33 | Complete manifest is copied from stable relative handles and destination equality is exact. |
| 34 | One Windows grammar and one held-handle identity owner cover all path roles. |

## Exact 32 Guard Allocation

Each name below resolves to exactly one collected test node (parameterized matrices remain one named guard).

| # | Exact guard |
|---:|---|
| 1 | `test_saved_field_absolute_deadline_all_stages` |
| 2 | `test_saved_field_admission_quarantine_linearization` |
| 3 | `test_saved_field_authority_policy_v1` |
| 4 | `test_saved_field_budget_boundaries` |
| 5 | `test_saved_field_cleanup_all_paths` |
| 6 | `test_saved_field_complete_manifest_transfer` |
| 7 | `test_saved_field_component_order_and_zero_semantics` |
| 8 | `test_saved_field_createprocess_tuple` |
| 9 | `test_saved_field_frame_resolution_table` |
| 10 | `test_saved_field_framework_validation_boundary` |
| 11 | `test_saved_field_helper_protocol_v1` |
| 12 | `test_saved_field_helper_reference_order` |
| 13 | `test_saved_field_local_nofollow_boundary` |
| 14 | `test_saved_field_mcp_result_boundary` |
| 15 | `test_saved_field_normal_residual_routes_termination` |
| 16 | `test_saved_field_owned_session_identity` |
| 17 | `test_saved_field_partial_acquisition_transaction` |
| 18 | `test_saved_field_quarantine_all_routes` |
| 19 | `test_saved_field_reader_cancellation` — deterministic blocked worker/cancel/join behavior, not source-text inspection. |
| 20 | `test_saved_field_reserved_device_alias_properties` |
| 21 | `test_saved_field_restart_replay` |
| 22 | `test_saved_field_safe_error_redaction` |
| 23 | `test_saved_field_shutdown_and_restart` |
| 24 | `test_saved_field_timeout_settlement` |
| 25 | `test_saved_field_trusted_root_policy` |
| 26 | `test_saved_field_unit_transform` |
| 27 | `test_saved_field_vendor_call_order` |
| 28 | `test_saved_field_vendor_record_validation` |
| 29 | `test_saved_field_windows_atomic_containment` |
| 30 | `test_saved_field_windows_path_identity_v1` |
| 31 | `test_saved_field_wire_schema_v1` |
| 32 | `test_saved_field_workspace_factory_transaction` |

## AR-C4-01 Correction

| Requirement | Implemented owner and executable evidence |
|---|---|
| Retain authority immediately | `HelperApplicationPort` stores the typed `WorkspaceLease` immediately after the sole policy factory returns. It passes that same lease into `AuthorizedBundleTransfer`; no bare workspace path is retained as cleanup authority. |
| One destructive owner | Helper settlement calls only `AuthorizedWorkspaceSnapshot.settle()` after transfer or `WorkspaceLease.settle()` before transfer commit. The helper has no `shutil` import, path deletion, or path-existence settlement inference. |
| Truthful typed state | `WorkspaceLease.settled` becomes true only after its exact delete owner succeeds. `AuthorizedWorkspaceSnapshot.settled` requires both its own completed transition and the underlying lease state. Transfer failure receipts consume `lease.settled`, never same-name path absence. |
| Retry after cleanup failure | Neither lease nor snapshot marks itself settled before capability deletion succeeds. The persistent fail-once workspace guard proves a later bounded owner call retries the still-retained authority. |
| Exact exceptional falsifier | `test_helper_exception_settles_only_retained_workspace_capability` runs the production helper application path. Transfer exact-settles the owned child, creates a same-name foreign replacement and foreign sibling, then raises. RED deleted the replacement through helper `shutil.rmtree`; GREEN calls the idempotent retained lease owner once total, reports the owned child settled, and preserves both foreign objects. |
| All helper return paths | No-lease failures report no workspace ownership; pre-snapshot failures settle the retained lease; committed paths settle the snapshot wrapping the same lease; cleanup exceptions report `workspace_settled=False`; successful cleanup reports only typed owner state. Existing success, vendor failure, protocol failure, timeout, and settlement matrices remain green. |

## Fresh GREEN Receipts

| Gate | Result |
|---|---|
| Full package | `.venv/Scripts/python.exe -m pytest -q` — PASS, 547 collected tests; one pre-existing Pydantic warning only. |
| Formatting/static | `ruff check src tests` — PASS; `ruff format --check src tests` — PASS, 45 files already formatted. |
| Cross-platform | WSL `compileall` and imports of helper/transfer modules — PASS. |
| Win32 | Real first-instruction helper, real NTFS trusted-root child create/delete, held-root name-swap, and the production-helper exceptional capability falsifier passed 4/4. Immediate post-probe query returned `NO_HELPER_ORPHANS`; no visible console was created. |
| Dependency/protected/index | No manifest/lock change; `git diff --cached --name-only` empty; `git diff --check` PASS. The only Go protected-surface diff is the unrelated pre-existing `internal/api/port_alloc_excluded_windows.go`, which this lane did not edit. |
| Publication | The publication-safety scanner passed both changed production modules, the changed helper test, and this final artifact. |
| Resource/foreign | After exact cleanup of the pre-fix forced-diagnostic pair, `Get-CimInstance Win32_Process` found no saved-field helper process after the corrected real probe. Existing `cstd`, `CSTDCMainController_AMD64`, `CSTDCSolverServer_AMD64`, and `mcphub-cst-mcp` identities were observed read-only and were neither opened, stopped nor mutated; all correction execution used synthetic/injected ports. |
| Static anti-layering | Production recursion is iterative; Windows directory listing/open/create/hash/copy uses the retained capability owner. No dependency addition, shell/PATH launch, visible-console path or second workspace factory was introduced. |

The installed Windows SDK declaration for `NtCreateFile` and `OBJECT_ATTRIBUTES.RootDirectory` was checked under the current Windows Kits include tree. The production wrapper uses the retained directory handle as `RootDirectory`, a relative `UNICODE_STRING`, and `FILE_OPEN_REPARSE_POINT`; the root-swap probe falsifies name-reopen behavior.

## Exact Changed Paths From C4

- `servers/electromagnetics-mcp/src/mcphub_em_mcp/cst_saved_field_helper.py`
- `servers/electromagnetics-mcp/src/mcphub_em_mcp/cst_saved_field_transfer.py`
- `servers/electromagnetics-mcp/tests/test_cst_saved_field_helper.py`
- `work-items/active/2026-08-11-cst-saved-field-sampler/implementation-p10-correction.md`

## Residual Target Gates

- Claim 7: installed CST `Result3D`, generated header, ResultTree registration/selection/frequency and owned-session trace remain P12.
- Exact helper plus target CST descendant Job membership, parent-crash kill-on-close and foreign-CST preservation remain P11/P13.
- Claim 15 and Line10 independent native agreement remain P14.
- Corrected immutable candidate C5, independent implementation architecture/QA/security review, manifest pin, build, deployment and hub recovery remain Lead-owned later phases.
- Publication safety is not self-approved here. No commit, index mutation, push, release, live helper, hub, fleet or CST action was performed.

## Gate

**PASS — AR-C4-01 is corrected on immutable candidate C4 and is ready for Lead-created corrected candidate C5 plus independent same-angle review.** Claims 7 and 15 and the explicitly listed target gates remain not-verifiable, not inferred PASS. This implementation gate does not authorize commit, index mutation, push, release, hub restart, fleet mutation, or live CST execution.

## Terms and Abbreviations

- ACL: access-control list.
- ADS: alternate data stream.
- CST: Computer Simulation Technology electromagnetic solver suite.
- DACL: discretionary access-control list.
- FastMCP: Model Context Protocol server framework used by this package.
- Job: Windows Job Object used to contain the helper and its descendants.
- MCP: Model Context Protocol.
- RED/GREEN: failing regression before the production correction / passing regression after it.
- SID: Windows security identifier.
- WSL: Windows Subsystem for Linux.
