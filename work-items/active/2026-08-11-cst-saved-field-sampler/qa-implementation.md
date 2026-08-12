# C6 Independent P17 Implementation QA

Date: 2026-08-12
Execution role: `$qa-engineer`
Immutable candidate: `5ff268dc13b2be9ca9500b5441634f0594538b94` (C6, local and unpublished)
Candidate parent: `2658ac85a0e1ee88b01f920af94c2664201e7a1c`
Accepted plan SHA-256: `FBB757B98797C90B7C9FD9B4C4998DCB01788241C5A4D39DE62D1532FD3C684E`
Implementation input SHA-256: `457892D6316A109784C6FE3C28481346098CD4FE8A8AB8C95FFCF0437BE33620`

## Scope and immutable binding

This is the independent P17 QA gate for exact C6. The lane verified P10-P16,
the 23 design-named guards, old-topology removal, route/protocol/authentication/
replay/QPC/quarantine matrices, worker/Job/breakaway/resource settlement, vendor
capabilities/output sealing, existing-six compatibility, Win32 safe synthetic
behavior, WSL imports, publication safety, history/index/protected/dependency
surfaces, orphans, and foreign CST preservation.

No source, test, Git history, index, Service Control Manager (SCM), CST, hub, or
fleet state was changed. The sole canonical write is this report. Raw evidence is
under `/.scratch/cst-c6-qa/`. C6 remained exact before the report write; the package
tree and index were clean, no remote contained C6, and no dependency or protected
surface changed.

## Criterion-quality precheck

Before execution, each criterion was tested against the behavior it could wrongly
admit. In particular, these were rejected as sufficient evidence: a fake
in-process route standing in for production wiring; a structurally valid but
authority-rebased Query Performance Counter (QPC) triple; a complete settlement
assembled from constants instead of kernel/transport receipts; a pre/post
revalidation mock that never swaps inside the external vendor call; collected-only
tests; or zero old-name text without one current owner replacing the old route.

## Blocking findings

### QA-C6-01 — production broker/vendor route is not wired

Severity: **high**. The production composition creates `WindowsBrokerClient` only
with `UnavailableBrokerTransport` (`cst.py:669-677`); its startup proof is always
false (`cst_saved_field_broker_client_windows.py:171-187`). Therefore a valid
restart-loaded policy cannot make the production sampler available. The passing
P15 route constructs `InProcessBrokerTransport`, `SavedFieldBrokerService`, and a
synthetic transaction entirely inside the test
(`test_cst_saved_field_integration.py:237-340`).

The capability-continuity owner is likewise not production-composed:
`IsolatedVendorPathLease` has only test instantiations, and no concrete production
`VendorPathPlatform` exists. The C6 vendor fix tests a fake platform that returns
opaque strings and toggles `swap` only at revalidation
(`test_cst_saved_field_vendor_isolation_windows.py:6-77`); it does not traverse the
real broker worker transaction or the external vendor read/write window. The safe
falsifier printed `WIRED_TRANSPORT_TYPE UnavailableBrokerTransport` and
`WIRED_STARTUP_READY False`.

Expected: one package-owned production daemon-to-broker transport, broker entry,
worker transaction, and concrete retained-capability vendor platform must be wired;
valid policy plus a real current startup proof may add the sampler, while absent or
failed proof remains default-off. Actual: only the unavailable production branch
and an in-process test branch exist.

Affected criteria/guards: P14-AC09, P14-AC10, P15-AC06, P15-AC07, P16-AC09;
In-server authority, Vendor-byte capability-continuity, Atomic-containment,
Sole-Job-handle, Settlement-order, and Publication guards.

### QA-C6-02 — broker accepts and propagates a replaced QPC authority triple

Severity: **high**. `SavedFieldBrokerService.issue_challenge` checks the original
frequency/admitted tick, but `NonceLedger` stores no original deadline triple and
`exchange` never equality-binds the request deadline to the challenge
(`cst_saved_field_vendor_isolation_windows.py:209-238,263-288`). A fresh executable
falsifier issued a challenge for `QpcDeadlineV1(100,10,6010)`, then submitted the
same nonce with the independently valid `QpcDeadlineV1(999,777,60717)`.

Actual receipt: `MUTATED_QPC_ACCEPTED True`; the worker received and the broker
returned the mutated triple. Expected: the broker rejects before authorization or
worker execution with the stable protocol failure. Existing QPC tests mutate a
response/constructor surface but do not falsify this receiver-side authority swap.

Affected criteria/guards: P10-AC17, P12-AC13, P12-AC14, P15-AC07, P16-AC09;
Protocol drift, Finite-budget, Contained-duration, and In-server authority guards.

### QA-C6-03 — broker fabricates a complete outer settlement

Severity: **high**. `SavedFieldBrokerService.exchange` sets worker-signaled,
worker-exit-recorded, worker-reference-closed, Job-active-zero, readers-joined, and
pipe-closed fields to literal `True` rather than consuming an owning containment/
transport receipt (`cst_saved_field_vendor_isolation_windows.py:291-319`). The
in-process cancel paths also return literal-complete receipts
(`cst_saved_field_broker_client_windows.py:185-187`;
`cst_saved_field_vendor_isolation_windows.py:324-342`).

The fresh falsifier supplied only a synthetic complete worker application receipt;
without any process, Job, reader, or pipe owner it obtained
`HARDCODED_OUTER_SETTLEMENT_COMPLETE True` and all six outer lifecycle fields true.
Expected: every lifecycle field originates in its single owning runtime receipt;
missing or unproved settlement suppresses success and quarantines.

Affected criteria/guards: P10-AC18, P12-AC15, P13-AC07 through P13-AC10,
P14-AC11, P15-AC07, P15-AC10, P16-AC09; Settlement-order, Sole-Job-handle,
Quarantine linearization, Contained-duration, and Foreign-process guards.

## Receiving-side echo

| Enumerated participant | Classification | Fresh evidence |
|---|---|---|
| C5 same-name vendor replacement | **not fixed in production** | C6 adds a lease abstraction and fake share-mode/revalidation tests, but no production platform or worker transaction instantiates it. |
| Copied payload open | **not fixed in production** | `activate_and_sample` receives a lease, but production has no concrete lease/platform route; external-path swap is not exercised by C6 tests. |
| Generated header create/seal | **not fixed in production** | Fake `prepare_output`/`seal_output` records shares; no production implementation or external write-window falsifier exists. |
| Clean payload/header and ResultTree registration | **not fixed in production** | Fake locators traverse unit tests only; production route remains unavailable. |
| C5 successful-breakaway truth | **fixed on safe synthetic route** | Exact truth table rejects created breakaway, and real safe Win32 probe either rejects/settles created escape or accepts only denied=true/created=false. |
| Worker/Job/resource siblings | **partially verified** | Worker containment tests pass, but broker outer settlement is fabricated rather than sourced from that owner. |
| Policy/auth/replay/QPC siblings | **QPC not fixed** | Authentication and nonce replay tests pass; receiver-side replacement of the whole valid QPC triple is accepted. |

## P10-P16 acceptance reconciliation

| Criterion | Verdict and fresh evidence |
|---|---|
| P10-AC15 | **REVISE** — topology tests collect and mutation tests exist, but the current named-guard set lacks falsifiers for production route binding, broker-side valid-QPC replacement, and receipt provenance. |
| P10-AC16 | **PASS** — exact semantic scan reports zero obsolete helper topology; pre-removal inventory 18 is recorded in the accepted implementation artifact. |
| P10-AC17 | **REVISE** — schemas carry equal-looking fields, but broker accepts a different valid QPC triple after challenge. |
| P10-AC18 | **REVISE** — return-path ownership is claimed, but the broker manufactures outer process/Job/reader/pipe settlement bits. |
| P10-AC19 | **REVISE** — Claims 7/15 remain target-only as required, but current implementation claims dependent on route/QPC/settlement owners are not executable truths. |
| P10-AC20 | **PASS** — existing-six fixtures and `tests/test_servers.py tests/test_stdio.py` remain green inside P15/full suites. |
| P11-AC11 | **PASS within default-off behavior** — invalid/missing/disabled policy stays at six; immutable v2 policy tests pass. Valid production policy still cannot start the sampler because QA-C6-01 blocks P15 availability. |
| P11-AC12 | **PASS** — closed, canonical, bounded broker/worker protocol suites pass. |
| P11-AC13 | **PASS as data contract only** — fixed service identities/DACL values pass; live SCM remains prohibited P18 work. |
| P11-AC14 | **PASS as dry-run only** — receipts enumerate operations with zero live SCM calls. |
| P11-AC15 | **REVISE** — rollback data ordering exists, but runtime broker/Job/pipe absence cannot be proved through a missing production route and fabricated receipt. |
| P11-AC16 | **PASS** — no dependency delta and neutral schemas exclude caller path/bytes/handle/token/SID/license authority. |
| P12-AC11 | **PASS as synthetic descriptor contract** — exact pipe descriptor fixture passes; live descriptor readback remains P18. |
| P12-AC12 | **PASS synthetic** — impersonation precedes parse, revert runs in `finally`, and failed revert quarantines. |
| P12-AC13 | **REVISE** — 256-bit one-use/expiry tests pass, but challenge does not bind the request's original QPC triple. |
| P12-AC14 | **REVISE** — executable falsifier proves valid replacement triple reaches worker. |
| P12-AC15 | **REVISE** — cancel/outer settlement may be literal-complete without owning runtime evidence. |
| P12-AC16 | **PASS synthetic** — framing/redaction ceilings pass; no target log claim is made. |
| P13-AC06 | **PASS** — exact `CreateProcessW` tuple and mutation matrix pass. |
| P13-AC07 | **REVISE** — worker safe probe owns real resources, but broker response does not consume its containment receipt. |
| P13-AC08 | **REVISE** — termination tests pass at containment owner; all-return broker aggregation is not receipt-backed. |
| P13-AC09 | **PASS safe synthetic** — created breakaway fails startup after exact settlement; denied=true/created=false alone completes. |
| P13-AC10 | **REVISE** — synthetic foreign preservation passes, but no production broker owns the Job and outer receipt is fabricated. |
| P13-AC11 | **PASS** — source/test scan has zero old helper aliases/resources/relations. |
| P14-AC07 | **PASS** — complete manifest and exact destination matrices pass. |
| P14-AC08 | **PASS** — grammar/identity/alias/ADS/hard-link/reparse/swap matrices pass. |
| P14-AC09 | **REVISE** — share-mode abstraction exists only behind test platforms; no production retained-capability platform is wired. |
| P14-AC10 | **REVISE** — fake output prepare/share-zero seal passes, but no production vendor write/read window exercises it. |
| P14-AC11 | **REVISE** — worker typed receipts exist; broker outer receipt has hard-coded success defaults. |
| P14-AC12 | **PASS for synthetic acquisition** — partial-prefix/zero-created/rollback and foreign-preservation tests pass. |
| P15-AC06 | **REVISE** — default-off baseline is exact; production can never observe a ready broker because only unavailable transport is composed. |
| P15-AC07 | **REVISE** — seven-event trace is an entirely in-process fake and does not traverse production transport, containment, or vendor platform. |
| P15-AC08 | **PASS** — actual FastMCP result boundary is one bounded text item and no structured duplicate. |
| P15-AC09 | **PASS synthetic** — stable failures and canary redaction tests pass. |
| P15-AC10 | **REVISE** — local admission quarantine tests pass; hard-coded transport/broker settlement can falsely avoid quarantine. |
| P15-AC11 | **PASS for checks, REVISE overall** — 508/508, Ruff, format, diff, existing-six, dependencies, and protected surfaces pass, but behavior findings remain. |
| P16-AC06 | **PASS** — one unpublished 31-file candidate; publication scan clean; index/package clean; no remote contains it. |
| P16-AC07 | **PASS** — C6 parent is accepted baseline and old topology scan is zero. |
| P16-AC08 | **PASS binding** — C6, accepted input hashes, diff, dependency, checks, and target-only Claims 7/15 are recorded. |
| P16-AC09 | **REVISE** — green suites do not reconcile production route, QPC binding, or receipt provenance. |

## Named-guard ledger

| Guard | Result / falsifying probe |
|---|---|
| Existing-wire compatibility | **PASS** — exact six schemas/catalogue and server/stdio tests. |
| Solve-path preservation | **PASS** — protected diff empty and solve tests remain in full suite. |
| Validation-channel | **PASS** — FastMCP malformed/unknown/literal cases remain pre-entry. |
| No-job-edge | **PASS** — old/daemon topology static tests pass. |
| Foreign-process | **REVISE** — containment fake preserves foreign identity, but broker lifecycle truth is hard-coded. |
| Complete-manifest transfer | **PASS** — 367-node P14 includes mutation/destination equality. |
| Trusted-root injection | **PASS** — policy/path owner matrices pass. |
| Workspace-transaction | **PASS** — transactional creation/rollback/sibling tests pass. |
| Neutral-port | **PASS** — neutral application/vendor import direction passes package/static review. |
| Vendor-record | **PASS** — malformed/nonfinite/status/budget matrices pass. |
| Finite-budget | **REVISE** — declared ceilings pass, but broker accepts rebased QPC authority. |
| Settlement-order | **REVISE** — containment order tests pass; broker fabricates outer settlement. |
| Contained-duration | **REVISE** — worker deadlines pass; original broker QPC triple is not preserved. |
| In-server authority | **REVISE** — default-off works, but production ready route is absent and QPC can be replaced. |
| Protocol drift | **REVISE** — closed wire shape passes; semantic equality to the issued QPC authority fails. |
| Atomic-containment | **REVISE** — safe worker proof passes, but production broker route is absent. |
| Sole-Job-handle | **REVISE** — worker owner passes locally; broker reports Job facts without receipt. |
| Quarantine linearization | **REVISE** — admission tests pass; false-complete settlement bypasses the premise. |
| Namespace identity | **PASS** — 261-node Windows path suite and held-root Win32 swap pass. |
| Vendor-byte capability-continuity | **REVISE** — fake lease tests pass; no concrete production platform or external-call swap probe. |
| MCP-boundary budget | **PASS** — exact/one-over and one-text/no-structured result tests. |
| Canary-redaction | **PASS synthetic** — public/protocol/containment canary tests pass. |
| Publication | **REVISE for promotability** — leak scan is clean, but blocking behavior defects prohibit publication. |

## Fresh executions

Deterministic inputs were `PYTHONHASHSEED=0`, `TZ=UTC`, `LC_ALL=C`, redirected
bytecode, explicit clocks/events/ordering, and no parallel pytest scheduling.

| Verbatim command / check | Result | Wall time | Raw receipt |
|---|---|---:|---|
| `uv run --frozen --python 3.13 pytest -q ... test_cst_saved_field_broker_topology.py test_cst_saved_field_broker_protocol.py test_cst_saved_field_broker_worker_protocol.py` | PASS 8/8; 0 failed/error/skip | 0.893 s | `/.scratch/cst-c6-qa/p10.txt`, `p10.xml` |
| P11 exact plan suite | PASS 22/22; 0 failed/error/skip | 0.971 s | `/.scratch/cst-c6-qa/p11.txt`, `p11.xml` |
| P12 exact plan suite | PASS 9/9; 0 failed/error/skip | 0.849 s | `/.scratch/cst-c6-qa/p12.txt`, `p12.xml` |
| P13 exact plan suite | PASS 40/40; 0 failed/error/skip | 1.204 s | `/.scratch/cst-c6-qa/p13.txt`, `p13.xml` |
| P14 exact plan suite | PASS 367/367; 0 failed/error/skip | 7.602 s | `/.scratch/cst-c6-qa/p14.txt`, `p14.xml` |
| P15 exact plan suite | PASS 33/33; 0 failed/error/skip; one pre-existing Pydantic warning | 4.141 s | `/.scratch/cst-c6-qa/p15.txt`, `p15.xml` |
| `uv run --frozen --python 3.13 pytest -q -p no:cacheprovider` | PASS 508/508; 0 failed/error/skip; same warning | 13.414 s | `/.scratch/cst-c6-qa/full.txt`, `full.xml` |
| Focused protocol/auth/replay/QPC suite | PASS 14/14 | 0.766 s | `/.scratch/cst-c6-qa/protocol-auth.txt`, `protocol-auth.xml` |
| Focused composition/route/quarantine suite | PASS 50/50 | 1.971 s | `/.scratch/cst-c6-qa/routes.txt`, `routes.xml` |
| Focused vendor lease/output/settlement suite | PASS 37/37 | 1.293 s | `/.scratch/cst-c6-qa/vendor.txt`, `vendor.xml` |
| Win32 worker first-instruction/breakaway plus held-root name-swap | PASS 2/2 | 0.866 s | `/.scratch/cst-c6-qa/win32.txt`, `win32.xml` |
| Executable production-route/QPC/receipt falsifier | **FAIL as designed:** unavailable production route; changed QPC accepted; fabricated settlement complete | 2.2 s | `/.scratch/cst-c6-qa/production-route-qpc-receipt-falsifier.txt` |
| `uv run --frozen --python 3.13 ruff check src tests` | PASS | 0.123 s | `/.scratch/cst-c6-qa/ruff-check.txt` |
| `uv run --frozen --python 3.13 ruff format --check src tests` | PASS, 53 files | <1 s | `/.scratch/cst-c6-qa/ruff-format.txt` |
| `git diff --check HEAD^ HEAD` and working-tree `git diff --check` | PASS | <1 s | `/.scratch/cst-c6-qa/commit-diff-check.txt`, `diff-check.txt` |
| WSL2 Ubuntu compile/import of protocol/client/worker/isolation modules | PASS, `WSL_IMPORT_OK` | 4.0 s | `/.scratch/cst-c6-qa/wsl.txt` |
| Publication safety `--range origin HEAD` | PASS receipt v2, 31 files, one complete commit, exact C6 tip | 3.3 s | `/.scratch/cst-c6-qa/publication-range.txt` |
| History/index/dependency/protected audit | PASS for candidate: exact C6, empty index/package status, no remote, zero dependency/protected delta | 3.3 s | `/.scratch/cst-c6-qa/state-audit.json` |
| C6 semantic old-topology scan | PASS, zero hits | 1.5 s | `/.scratch/cst-c6-qa/c6-residue.json` |
| Worker orphan and foreign CST probe | PASS: zero broker-worker orphan; CST PIDs 7624/7636/10032 retain 2026-08-09 creation times and were not mutated | 2.8 s | `/.scratch/cst-c6-qa/orphan-audit.json`, `process-after.json` |
| Preflight unrelated-byte inventory | **external drift:** 37/38 match; accepted decision artifact length changed during the shared Lead workflow, outside C6/package/index and outside this lane | 4.5 s | `/.scratch/cst-c6-qa/unrelated-audit.json` |

## Failure classification and required correction

| Finding | Classification | Owning correction |
|---|---|---|
| QA-C6-01 | Product regression/incomplete implementation against accepted production topology | `$backend-engineer` integration owner plus platform owner: implement and wire the package-owned transport, broker entry/worker transaction, and concrete capability platform; add a production-composition falsifier and deterministic external-call swap matrix. |
| QA-C6-02 | Protocol/authority regression | Broker protocol owner: bind nonce/challenge to the exact original QPC triple at every receiver; add a valid-triple replacement RED/GREEN test. |
| QA-C6-03 | Settlement correctness regression | Containment/transport owner: return one typed outer receipt from real owners; delete literal-success defaults; mutate every receipt field to prove quarantine. |

Per the dispatch, this lane may update only this QA artifact. The Lead must persist
the three bug-registry records before accepting the REVISE verdict.

## Residual target gates

Live SCM provisioning/readback, installed CST compatibility and descendant proof,
Claim 7, independent native provider qualification, Line10 Claim 15, existing-six
target smoke, release, deployment, and live acceptance remain open. They were not
run and cannot substitute for the three earlier implementation defects.

## Gate

Gate: **REVISE** — exact immutable C6 is not eligible for P17 PASS or publication. Although all 508 package tests, phase suites, Ruff, format, Win32 safe synthetic checks, WSL imports, publication scan, old-topology scan, and candidate-state checks are green, fresh executable falsifiers prove that production composition has no available broker/vendor route, the broker accepts and propagates a replaced valid QPC authority triple, and it fabricates complete outer containment/pipe settlement from literal booleans. These defects invalidate the affected P10-P16 acceptance criteria and guards; correction requires a new immutable candidate and fresh architecture, security-engineer, security-reviewer, and QA gates.

## Terms and Abbreviations

- C6 — sixth local immutable candidate reviewed in this delivery chain.
- CST — Computer Simulation Technology solver environment.
- DACL — Discretionary Access Control List.
- Job — Windows Job Object used for process-tree containment.
- MCP — Model Context Protocol.
- QPC — Query Performance Counter, the Windows monotonic clock.
- SCM — Windows Service Control Manager.
- WSL2 — Windows Subsystem for Linux version 2.
