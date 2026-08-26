# P00–P06 Containment Implementation

Gate: **PASS**

The P00–P06 slice now owns a default-off restart policy, Windows path identity,
complete manifest-v2 transfer, closed helper protocol, fixed isolated helper entry,
pure reusable core, atomic hidden Win32 Job launch, and fail-latched admission. It
contains no P07 vendor methods, P08 FastMCP registration, P09 history work, live CST
call, Line10 dependency, hub/fleet mutation, commit, index change, or publication.

## Accepted Inputs Revalidated

| Input | SHA-256 | Result |
|---|---|---|
| Plan | `D01FEC17BACBE6AC4C81E98B807C1E6A70EF937406376DC2F1CF6822794323E3` | Exact match |
| Design | `F6BF0EDB230236683A086465D83D77B90B1F0ADF1382744D7F3A098DDFB16DE5` | Exact match |
| Accepted decision | `636295710EF42295C01D6D5E7F97667D017C72EDBDF5C13C83F17C9DC42179DA` | Exact match |
| Architecture review | `D80ACAAA0E86F382DD30191C9042B8126C921E8718D19FBA5B4EB3D838167D20` | Exact `PASS` input |
| Security review | `4A983C6501849F12BBB3F03C26C33B419629624A0A7369BE8938BDEACB23178A` | Exact `PASS` input |

## RED and GREEN Receipts

| Stage | Receipt |
|---|---|
| Baseline | Dependency sync, `85/85` package tests, Ruff check, and Ruff format passed before the slice. |
| Aggregate RED | New P00–P06 tests produced `354 failed, 4 passed, 1 warning in 6.46s`; missing owners and premature P08 registration were named. |
| Historical oracle sensitivity | The original candidate already passed the three QA-oracle tests, so no false pre-production RED is claimed. Disposable mutations, followed by byte-exact restore, made selected-field corruption RED with `DID NOT RAISE`, made foreign identity loss RED at each of five acquisition boundaries, and made early cleanup/settlement RED with trace `['is_live', 'clear', 'activate']`. |
| Transfer correction RED | Missing, extra, path, type, stream, size, hash, and cardinality mismatches were detected, but all eight failures had `workspace_settlement=None`. |
| Transfer correction GREEN | The same eight rows return `authorized_copy_changed`, complete cleanup receipt, zero vendor calls, and removed workspace. |
| Pure-core RED | Static probe found filesystem imports and a concrete P07 vendor adapter in the P05 surface. |
| Pure-core GREEN | Core is deterministic/filesystem-free/vendor-free; concrete vendor methods/module are absent; pure resolver/unit/schema tests pass. |
| Protocol RED | Closed response receipt parsing was absent (`HelperResponseV1.from_wire` missing). |
| Protocol GREEN | Missing/extra receipt fields, malformed/truncated/oversize/second/canary frames, identity drift, exact and one-over request/response/text bounds all fail atomically and redact private data. |
| Containment RED | Failure-mode matrix found no admission-to-invocation integration owner (`ContainedSamplerRunner` absent). |
| Containment GREEN | stderr overflow, nonzero/crash, timeout, residual tree, reader nonjoin, missing signal/exit/reference/active-zero/handle-close, and unexpected parent exception converge on one settlement/quarantine owner. |
| Win32 RED | The real probe first lacked a first-instruction receipt, then exposed ctypes pointer-width and transient Job-accounting defects, and finally showed that API-level breakaway success is not proof of escape under nested Jobs. |
| Win32 GREEN | Parent and child prove exact Job membership, the three-role inherited HANDLE_LIST, no console/window, and either denied breakaway creation or a second process counted in the exact Job. |
| Final focused | P00–P06 focused suite passed `124/124` before the closed response parser; all newly added rows remain included in the final full run. |
| Final full | Package passed `442/442`; the only warning is the pre-existing Pydantic incomplete-forward-reference warning. |
| Static | `ruff check src tests`, `ruff format --check src tests`, and `git diff --check` passed. |
| Cross-platform | WSL imported stdlib-only policy/transfer/containment modules. WSL lacks `pydantic`, so full core import and pytest there remain unavailable rather than claimed green. |
| Safety | Nineteen changed source/test paths passed explicit path publication scanning; dependency/lock diff is empty; no saved-field helper process remained. |
| Protected | `cst.py` and `safety.py` equal accepted pre-candidate parent `2658ac85`; HFSS/jobs/CST-results are unchanged; the sole Go diff is unrelated pre-existing user work. |

## Acceptance-Criteria Reconciliation

### P00

- **P00-AC01:** The preflight proves exact candidate ownership/unpublished status and records byte-restorable evidence for every unrelated dirty path; no history or index mutation occurs.
  - **PASS** — unpublished candidate/parent and empty index were probed; unrelated tracked bytes have the scratch patch/hash receipt; no history/index mutation occurred.
- **P00-AC02:** Each of the three defects in `2026-08-11-cst-saved-field-core-qa-oracle-gaps` has one persistent non-degenerate RED test with the exact expected failure.
  - **PASS** — persistent named guards exist. Because candidate behavior already passed them, deterministic disposable mutation sensitivity is the truthful RED substitute; exact bytes were restored before GREEN.
- **P00-AC03:** The complete-manifest bug has add, remove, rename, replace, size, hash, and metadata-drift RED cases at enumerate, pre-open, read, copy, source-close, destination-enumerate, and pre-commit boundaries.
  - **PASS** — persistent `7 × 7` source matrix plus eight-class destination matrix cover every named boundary/class.
- **P00-AC04:** The ADS/alias bug has generated coverage for every path producer and consumer, including superscript `¹/²/³`; every negative row asserts zero filesystem/helper/workspace/CST counters where lexical rejection applies.
  - **PASS** — generated role matrix covers ASCII 1–9, superscript 1–3, case/extensions/ADS/trailing/normalization variants and rejects before its sole provider; no helper/workspace/CST route exists in P00–P06.
- **P00-AC05:** Baseline existing-six schemas, errors, package test result, dependency files, and protected-source hashes are recorded for later byte/shape comparison.
  - **PASS** — schema/body hashes, package baseline, dependencies, and protected hashes were captured and rechecked.

### P01

- **P01-AC01:** Absent, empty, `enabled=false`, malformed, manifest-v1, relative, remote, reparse, wrong-owner, or broadly readable policy produces one typed disabled result and no stale-snapshot fallback.
  - **PASS** — all converge on one typed disabled result; there is no reload/watch/stale fallback.
- **P01-AC02:** A valid enabled v1 policy produces one immutable snapshot keyed by its lowercase canonical policy SHA-256; replacing the file does not alter that process snapshot.
  - **PASS** — frozen tuples/read-only mappings and canonical revision are equality-tested.
- **P01-AC03:** Exact and one-over tests pass for file bytes, entries, identifiers, paths, depths, hashes, and duplicate root/project identities.
  - **PASS** — every declared parser/identity ceiling has exact and one-over coverage.
- **P01-AC04:** Operator generation starts `enabled=false`, emits only canonical policy/entry data, performs no daemon/fleet mutation, and validation fails closed on access-control or identity ambiguity.
  - **PASS** — generation is pure/default-off; concrete Windows owner/access/identity proof fails closed.
- **P01-AC05:** No new dependency or lower-layer ambient configuration read appears.
  - **PASS** — dependency/lock diff is empty; lower layers receive typed inputs only.

### P02

- **P02-AC01:** Canonical ordinary `C:\allowed\project.cst` passes without component rewriting; every disallowed lexical form fails before the first filesystem call.
  - **PASS** — the shared predicate preserves the canonical value and provider count remains zero for rejected rows.
- **P02-AC02:** Generated ASCII digits 1–9 and superscript digits 1–3 COM/LPT property matrices remain rejected under mixed case, zero/one/multiple extensions, ADS, trailing variants, and mutated normalization-step order.
  - **PASS** — generated matrices are persistent and GREEN.
- **P02-AC03:** Every path role requires exact unique long-name, final path, volume/file ID, link-count-one, no-reparse, and only `::$DATA`; missing proof fails before child content or CST.
  - **PASS** — all proof tuple fields are conjunctive; every single-field mutation fails closed.
- **P02-AC04:** Deterministic swap, hard-link, short-name, case/NFC collision, stream, mapped-drive, and reparse probes cannot supply bytes.
  - **PASS** — held-handle fake matrices cover the identity classes and the target Windows ADS probe is real; no name re-open grants bytes.
- **P02-AC05:** The ADS/alias bug remains open but has GREEN implementation evidence ready for independent QA; it is not closed by the implementer.
  - **PASS** — implementation evidence is GREEN and this lane changed no bug lifecycle record.

### P03

- **P03-AC01:** Canonical rows sort by UTF-8 path bytes and the v2 aggregate is stable across filesystem enumeration order; manifest-v1 is rejected, never migrated.
  - **PASS** — forward/reverse enumeration equality and exact v2 schema rejection are GREEN.
- **P03-AC02:** Every ancillary/project/mesh/selected-field mutation boundary from P00 now returns the named stable failure before CST/session creation; parent source call count remains exactly zero.
  - **PASS** — the boundary matrix suppresses vendor start and no P08 parent source path exists.
- **P03-AC03:** Complete destination mismatch in count, path, type, stream, size, hash, identity uniqueness, or aggregate removes the workspace and never exposes a vendor path.
  - **PASS** — all eight mismatch classes have direct falsifiers, complete settlement, zero vendor calls, and exact-child removal.
- **P03-AC04:** Failure after create, permission, owner/access, resolution, identity, initialization, and immediately before transfer removes the child exactly once, preserves all siblings, and emits a complete `WorkspaceSettlement` with no defaulted producer field.
  - **PASS** — policy owner/access is proven before creation; the factory matrix covers every post-create state it owns and returns a field-complete receipt while preserving siblings.
- **P03-AC05:** Exact-limit transfer succeeds; 33rd depth, 20,001st entry, 10,001st file, per-file over 8 GiB, aggregate over 16 GiB, or expired budget rejects before copying the crossing unit.
  - **PASS** — production constants are exact and lower injectable ceilings prove the same before-crossing-unit branch without allocating multi-GiB fixtures.
- **P03-AC06:** The complete-manifest bug remains open pending independent QA even after every local GREEN probe passes.
  - **PASS** — this lane changed no bug lifecycle record.

### P04

- **P04-AC01:** Exact-limit request/response/stderr frames pass and one-over frames fail atomically without partial parsing or publication.
  - **PASS** — canonical exact/one-over request and response frames, final UTF-8 text, and containment stderr-overflow behavior are covered.
- **P04-AC02:** Invalid length, UTF-8, JSON, schema, correlation, policy/entry, receipt, trailing bytes, second frame, stderr byte, or exit contract maps only to the stable safe protocol failure.
  - **PASS** — framing/identity/closed-receipt matrix and containment exit/stderr matrix converge on `helper_protocol_invalid` without raw bytes.
- **P04-AC03:** Actual FastMCP result construction later receives one already encoded canonical text item; `structuredContent` is absent and no second serializer exists.
  - **PASS** — the protocol owns one capped encoded text; the P08 consumer is intentionally absent, so no competing serializer/structured content exists.
- **P04-AC04:** Canary values injected into every caller/vendor/path/file-ID/security-ID/environment/license/exception/raw-status field are absent from every public and captured diagnostic channel.
  - **PASS** — all available P00–P06 channels are allowlist-built; private causes and malformed-frame canaries never appear. P07-only producer fields do not yet exist.
- **P04-AC05:** Budget values are exactly 60/5/2/10/1 seconds and the declared byte, candidate, metadata, process, and point ceilings; no sub-budget extends the absolute deadline.
  - **PASS** — exact constants are asserted in one protocol owner and the Job process limit consumes that owner.

### P05

- **P05-AC01:** Helper uses fixed module entry, zero variable argv/environment authority, one request, one response, and no CST import in the parent process.
  - **PASS** — isolated fixed module, one read/write/exit contract, and absent P08 parent composition are static- and runtime-tested.
- **P05-AC02:** Import-graph/static probes show application and vendor depend only on `cst_saved_field_port.py`; core/helper/protocol values contain no CST object.
  - **PASS** — pure core has no filesystem/vendor import; concrete vendor module/methods are absent until P07; neutral records contain no CST object.
- **P05-AC03:** Candidate pure resolver tables pass for missing/ambiguous/exact selector/frequency boundary/hash/pass/permuted order without `#NNNN` inference.
  - **PASS** — all named resolver rows are persistent and GREEN.
- **P05-AC04:** Unit conversion admits only `m`/`mm`, keeps input coordinates, applies one exact scale, and returns finite six-component rows in request order.
  - **PASS** — closed units, exact scale, finite/order, and exact-six-zero behavior are GREEN.
- **P05-AC05:** Every supplied acquisition/workspace receipt field is consumed and echoed exactly; missing fields are internal contract failure, never default success.
  - **PASS** — closed `CallSettlement` parsing rejects each missing/extra field; P07 acquisition is explicitly false/absent rather than defaulted success.
- **P05-AC06:** Static forbidden-edge scan finds zero solve/remesh/save/history/job/Line10/VFEM production dependencies.
  - **PASS** — zero matches across the live P00–P06 saved-field production surface.

### P06

- **P06-AC01:** Instrumented child observes itself in the exact Job on its first instruction, inherits exactly three handles, has no console/window, and cannot create a breakaway child.
  - **PASS** — real Win32 synthetic probe proves all four properties without visible console.
- **P06-AC02:** Mutating any launch tuple field, exposing one extra inheritable handle, using a path/current-directory decoy, or removing an attribute fails before helper work and settles every already-created parent resource.
  - **PASS** — every field/tuple/attribute mutation is rejected and the ctypes owner closes its reversed explicit handle ledger on every return/exception.
- **P06-AC03:** Blocking every source/helper/vendor/encode/validate/publish stage terminates the exact Job at its cutoff; no source/vendor/success counter changes after 60 seconds; proved failure returns by 70 seconds maximum.
  - **PASS** — one absolute 60-second deadline plus 10-second cleanup deadline is passed unchanged to the single kernel owner; pre-P07/P08 stages are absent rather than unbounded.
- **P06-AC04:** Timeout, normal exit with residual descendant, nonzero/crash, protocol/stream overflow, shutdown, parent exception, and reader nonjoin use the same termination state machine and never publish helper success.
  - **PASS** — the complete deterministic matrix enters the same classifier/runner; only fully settled normal result returns bytes.
- **P06-AC05:** Deterministic A/B waiter schedule proves one active plus one waiter, exact 1-second expiry, second-waiter rejection, and quarantine/revision drift denial with lexical/source/helper counters zero.
  - **PASS** — condition-variable schedule, second-waiter rejection, one-second cap, revision recheck, and quarantine wake are GREEN.
- **P06-AC06:** Missing helper signal, reference close, active zero, reader join, or required handle close produces `containment_settle_failed` and a one-way quarantine; no in-process clear exists.
  - **PASS** — every missing receipt bit is parameterized and latches current/queued/future admission; no clear API exists.
- **P06-AC07:** Dependency and lock diffs contain no new package, and Go hub/process source diffs are empty.
  - **PASS** — no dependency/lock or slice-owned Go mutation exists; unrelated user Go work is preserved.

## Guard Allocation (22/22)

| Guard | Owner/evidence | Result |
|---|---|---|
| Complete-manifest transfer guard | `cst_saved_field_transfer.py`; 49 source-boundary plus 8 destination-class falsifiers | PASS |
| Namespace identity guard | one lexical/held-handle Windows identity owner for every role | PASS |
| Foreign-process guard | five-boundary mutation sensitivity; Job-only kill authority | PASS |
| Settlement-order guard | blocked-observer mutation sensitivity plus conjunctive containment receipt | PASS |
| Existing-wire compatibility guard | existing-six schema/body hashes and three CST tools | PASS |
| In-server authority guard | immutable snapshot/admission owner; P08 routes absent | PASS |
| Trusted-root injection guard | only future composition may read ambient policy | PASS |
| Canary-redaction guard | closed frames and allowlist-built failures | PASS |
| Source-capability guard | stable helper-owned handles; no parent source I/O | PASS |
| Workspace-transaction guard | exact-child lease, complete cleanup receipt, siblings preserved | PASS |
| Finite-budget guard | one declared ceiling owner for path/tree/file/byte/candidate/metadata/point/process/time | PASS |
| Protocol drift guard | canonical v1 frames, exact identity, complete closed settlement | PASS |
| MCP-boundary budget guard | one capped encoded text; P08 publisher absent | PASS |
| Neutral-port guard | neutral records; pure core; concrete vendor absent | PASS |
| No-job-edge guard | zero solve/remesh/save/history/job/Line10/VFEM edge | PASS |
| Component-order and zero semantics guard | ReX/ReY/ReZ/ImX/ImY/ImZ and exact-six-zero tests | PASS |
| Atomic-containment guard | real JOB_LIST/HANDLE_LIST hidden helper and full tuple mutation matrix | PASS |
| Contained-duration guard | absolute 60 seconds plus cleanup-only 10 seconds | PASS |
| Sole-Job-handle guard | non-inheritable parent handle and exact-Job accounting/termination | PASS |
| Quarantine linearization guard | one atomic lease/revision/generation/latch owner | PASS |
| Validation-channel guard | sampler absent before P08; existing validation unchanged | PASS |
| Solve-path preservation guard | existing CST solve bodies equal accepted parent | PASS |

## Changed Paths

Production:

- `servers/electromagnetics-mcp/src/mcphub_em_mcp/cst_saved_field_policy.py`
- `servers/electromagnetics-mcp/src/mcphub_em_mcp/cst_saved_field_transfer.py`
- `servers/electromagnetics-mcp/src/mcphub_em_mcp/cst_saved_field_helper_protocol.py`
- `servers/electromagnetics-mcp/src/mcphub_em_mcp/cst_saved_field_helper.py`
- `servers/electromagnetics-mcp/src/mcphub_em_mcp/cst_saved_field_containment_windows.py`
- `servers/electromagnetics-mcp/src/mcphub_em_mcp/cst_saved_field_port.py`
- `servers/electromagnetics-mcp/src/mcphub_em_mcp/cst_saved_field.py`
- `servers/electromagnetics-mcp/src/mcphub_em_mcp/cst_saved_field_vendor.py` (superseded P07 implementation removed)
- `servers/electromagnetics-mcp/src/mcphub_em_mcp/cst.py` (premature P08 registration removed; accepted-parent bytes)
- `servers/electromagnetics-mcp/src/mcphub_em_mcp/safety.py` (superseded ambient factory removed; accepted-parent bytes)

Tests:

- `servers/electromagnetics-mcp/tests/test_cst_saved_field_policy.py`
- `servers/electromagnetics-mcp/tests/test_cst_saved_field_path_identity_windows.py`
- `servers/electromagnetics-mcp/tests/test_cst_saved_field_transfer.py`
- `servers/electromagnetics-mcp/tests/test_cst_saved_field_helper_protocol.py`
- `servers/electromagnetics-mcp/tests/test_cst_saved_field_helper.py`
- `servers/electromagnetics-mcp/tests/test_cst_saved_field_containment_windows.py`
- `servers/electromagnetics-mcp/tests/test_cst_saved_field_contract.py`
- `servers/electromagnetics-mcp/tests/test_cst_saved_field_integration.py`
- `servers/electromagnetics-mcp/tests/test_cst_saved_field_vendor.py` (superseded P07 tests removed)
- `servers/electromagnetics-mcp/tests/test_servers.py`
- `servers/electromagnetics-mcp/tests/test_stdio.py`

This file is the sole work-item artifact changed by this lane.

## Deferred Outside This Slice

- P07 owns real vendor acquisition, candidate validation, activation, sampling, and
  complete acquisition/session receipts.
- P08 owns FastMCP registration, final one-TextContent publication, catalogue, and
  operator documentation.
- Target CST descendants and Line10 acceptance remain the later P11 target gate; the
  current Win32 probe is deliberately synthetic and starts no CST/vendor process.

## Terms and Abbreviations

- ACL — access control list.
- ADS — alternate data stream.
- CST — CST Studio Suite.
- MCP — Model Context Protocol.
- P00–P06 — accepted plan phases covered by this implementation lane.
- RED/GREEN — failing test before implementation / passing test after implementation.
- WSL — Windows Subsystem for Linux.
