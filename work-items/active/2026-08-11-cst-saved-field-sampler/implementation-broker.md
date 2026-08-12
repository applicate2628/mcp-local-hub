# P10-P15 broker implementation

Execution role: `$backend-engineer` integration owner. Accepted design SHA-256:
`6C47670725FFB2E715BD78915131F50E693A9B8975A053AE9DB6C2399FD8C172`.
Accepted plan SHA-256:
`FBB757B98797C90B7C9FD9B4C4998DCB01788241C5A4D39DE62D1532FD3C684E`.

## Outcome

P10-P15 replace the daemon-owned helper topology with one credential-free
daemon-to-broker boundary and one broker-owned, no-console Job worker. The daemon
holds only policy identity, the admission gate, and the pipe client. The broker
owns authorization, nonce consumption, worker containment, nested settlement, and
the capability transfer/vendor transaction. Obsolete helper modules and tests were
deleted rather than retained as compatibility paths. No live Service Control
Manager, CST, hub, or fleet action was performed.

## RED to GREEN receipts

| Phase | RED | GREEN |
|---|---|---|
| P10 | The new topology suite collected with eight deliberate assertion failures: 18 obsolete relations, daemon authority edge, missing broker route/protocols, and the breakaway conjunction. | Broker topology/protocol suites: 8 passed; semantic obsolete-topology scan: zero matches. |
| P11 | Policy schema-v1 rejection, broker protocols, and fixed service provisioning owners were absent. | Policy/protocol/provisioning suite: 22 passed. Policy v2 is required; dry-run provisioning/rollback has zero live SCM calls. |
| P12 | Pipe/auth/nonce/QPC owners were absent. | Pipe, protocol, and absolute-deadline suite: 9 passed. |
| P13 | Worker tests failed for the missing broker worker and first-instruction proof. | Containment, worker, and topology suite: 40 passed; real safe Win32 synthetic worker probe passed. |
| P14 | Vendor-isolation tests failed for the missing lease owner. | Complete transfer/path/vendor/isolation suite: 367 passed. |
| P15 | Named integration failed with `ImportError` for `BrokerWorkerApplication`. | Same named test passed with the exact seven-event daemon-to-broker-to-worker-to-settlement trace; P15 focused suite: 33 passed. |

## Acceptance reconciliation

| Acceptance criterion | Evidence and disposition |
|---|---|
| P10-AC15 | Persistent guard tests collect and exercise topology, deadline, protocol, containment, and redaction invariants. PASS. |
| P10-AC16 | Pre-removal inventory was 18; the exact semantic scan now returns no match. PASS. |
| P10-AC17 | Both versioned protocols equality-bind correlation, policy revision, request SHA-256, and the unchanged QPC triple. PASS. |
| P10-AC18 | The owning matrix is: validation/authorization before child = daemon/broker gate; pre-child/cancel/disconnect/daemon-death = broker transport; timeout/broker-death/worker-crash/reader-stall = broker Job; normal = worker receipt then broker receipt; cleanup failure = broker quarantine receipt. PASS. |
| P10-AC19 | Claims 1-34 map to the named guards below and current focused/full tests; Claim 7 target CST activation and Claim 15 Line10 remain explicit target-only residuals. The 18 security claims map to protocol, pipe-auth, nonce, capability, containment, settlement, redaction, and publication guards. PASS within P10-P15 scope. |
| P10-AC20 | Before production edits `tests/test_servers.py tests/test_stdio.py` passed 6; after integration they remain green in the P15/full suites. PASS. |
| P11-AC11 | Missing/invalid/disabled leaves no sampler; v1 is rejected and v2 is restart-loaded; daemon and broker receive one revision. PASS. |
| P11-AC12 | Closed canonical bounded v1 broker and private worker schemas reject unknown, missing, duplicate, trailing, oversized, and noncanonical values. PASS. |
| P11-AC13 | Fixed credential-free virtual service identities, pinned images, session 0, service SID type, and protected DACL are typed values. PASS; live provisioning is later. |
| P11-AC14 | Data-only provision/rollback receipts enumerate the fixed operations and report zero live SCM calls. PASS. |
| P11-AC15 | Rollback receipt orders daemon stop, broker settlement, broker stop, signaled/absence proof, policy disable, delete, ACL revoke, restart. PASS as dry-run contract. |
| P11-AC16 | Dependency diff is empty; daemon request schema contains no source locator/bytes/handle/token/SID/license authority. PASS. |
| P12-AC11 | Single-instance local overlapped pipe descriptor, remote denial, exact narrow DACL and high-integrity SACL are asserted. PASS as safe synthetic contract; live descriptor readback is P18. |
| P12-AC12 | SCM/token/service SID/logon/session/integrity/privilege/image proof precedes parsing; impersonation reverts in `finally`; failed revert quarantines. PASS. |
| P12-AC13 | One 256-bit five-second nonce is consume-before-authorize and replay/expiry fail. Correlation/policy/entry/manifest/request hash are closed fields. PASS. |
| P12-AC14 | One daemon-created integer QPC triple remains unchanged; mutation and expiry tests fail closed. PASS. |
| P12-AC15 | Typed cancel receipt requires worker settled, Job active zero, pipe closed, and zero owners before release; incomplete settlement latches admission quarantine. PASS for fake transport. |
| P12-AC16 | Protocol frames and public text have exact caps and closed safe identifiers. Source/tests publication scan is clean. PASS. |
| P13-AC06 | Exact pinned non-shell `CreateProcessW`, fixed `-I -s -E -m` worker, fixed cwd, JOB_LIST, HANDLE_LIST, three std handles, and `CREATE_NO_WINDOW` remain covered. PASS. |
| P13-AC07 | Broker containment owns Job/process/thread/stdio/readers/watchdog; worker proof precedes request read; daemon imports no containment/worker/source module. PASS. |
| P13-AC08 | Existing termination-state tests cover timeout/residual/nonzero/protocol/exception/reader/cancel and ordered settlement. PASS. |
| P13-AC09 | Breakaway truth is denied=true and created=false only; this host permits the synthetic breakaway attempt, so the child is terminated/waited/recorded/closed and startup fails closed. PASS. |
| P13-AC10 | Sole Job ownership and foreign-process preservation tests remain green. No worker orphan remained after the real synthetic probe. PASS. |
| P13-AC11 | Obsolete helper source/tests were deleted and the exact scan returns zero. PASS. |
| P14-AC07 | Stable-handle manifest copy and exhaustive destination equality matrix remain green. PASS. |
| P14-AC08 | Shared Windows grammar/identity tests cover path roles, aliases, streams, hard links, reparse, remote/mapped, and swaps. PASS. |
| P14-AC09 | Vendor lease asserts exact read/input and ancestor shares; no reopen fallback; project/header never cross the daemon request. PASS. |
| P14-AC10 | Output preparation is distinct from share-zero seal, SHA-256 validation, and retained read capability. PASS. |
| P14-AC11 | Typed source/vendor/session/workspace receipts are required; no hard-coded successful settlement is accepted at protocol validation. PASS. |
| P14-AC12 | Acquisition/rollback matrices remain green; no process snapshot/set-difference authority was introduced. PASS. |
| P15-AC06 | Default-off composition preserves six total tools; enabled v2 plus broker startup proof adds exactly the sampler. PASS. |
| P15-AC07 | Named FastMCP integration records exactly challenge, broker authorization, worker start, worker authorization, transfer/vendor settlement, worker settlement, response settlement. PASS. |
| P15-AC08 | Actual result remains one bounded text item with no structured duplicate; full boundary tests are green. PASS. |
| P15-AC09 | Closed safe failure allowlist and protocol constructors reject arbitrary helper/vendor identifiers; validation/canary tests are green. PASS. |
| P15-AC10 | One daemon admission owner gates active/waiting calls and latches quarantine on incomplete nested settlement; deterministic waiter/future-call tests are green. PASS. |
| P15-AC11 | Full 508 tests pass; Ruff check and format pass; diff/protected/dependency checks pass. PASS. |

## Guard ledger

| Guard | Current owner and falsifying probe |
|---|---|
| Existing-wire compatibility | CST composition root; existing server/stdio and exact catalogue tests. |
| Solve-path preservation | Existing CST solve tests and protected diff for `jobs.py`/`cst_results.py`. |
| Validation-channel | FastMCP composition tests asserting pre-entry failures and zero authorization. |
| No-job-edge | Topology import scan rejects daemon/worker source and JobManager edges. |
| Foreign-process | Containment/session tests preserve interleaved foreign identity; live CST processes were observed but not touched. |
| Complete-manifest transfer | Transfer destination mismatch and ancillary mutation matrices. |
| Trusted-root injection | Policy/path/workspace owners and root failure matrices. |
| Workspace-transaction | Exclusive workspace lease mutation/rollback tests. |
| Neutral-port | Application/vendor import direction and neutral typed lease/session records. |
| Vendor-record | Candidate/record runtime validation matrices. |
| Finite-budget | Exact QPC triple mutation/expiry tests and existing bounded inventory tests. |
| Settlement-order | Worker then broker nested settlement trace and containment order tests. |
| Contained-duration | One 60-second QPC triple and post-response deadline check; termination-only cleanup remains 10 seconds. |
| In-server authority | Default-off composition plus daemon admission lease and broker policy/nonce authorization. |
| Protocol drift | Closed canonical broker and worker protocol mutation tests. |
| Atomic-containment | CreateProcess tuple tests and safe real Win32 first-instruction probe. |
| Sole-Job-handle | Broker containment owns Job and worker proof; no daemon containment import. |
| Quarantine linearization | Admission waiter/future-call tests and incomplete cancel/response settlement latch. |
| Namespace identity | Shared Windows path identity role/alias/stream/reparse/swap tests. |
| Vendor-byte capability-continuity | Exact share-mode vendor lease and output seal tests. |
| MCP-boundary budget | Actual one-TextContent/no-structured-content/cap tests. |
| Canary-redaction | Closed safe IDs, bounded frames/text, and canary tests across result/diagnostic surfaces. |
| Publication | Source and tests publication-safety scans clean; dependency/protected diffs empty; no push or live deployment. |

Claims 1-34 are allocated 1:1 to their design-named single owners and probes through
the guard ledger above. Claims 7 and 15 remain target gates by accepted design; no
unit or synthetic result is represented as target CST or Line10 evidence.

## Fresh terminal evidence

| Check | Result |
|---|---|
| Full package | 508 passed; one pre-existing Pydantic warning. |
| Ruff | `ruff check src tests`: all checks passed. |
| Format | `ruff format --check src tests`: 53 files already formatted. |
| Win32 | Safe real worker containment probe passed; no visible console and no orphan worker. |
| WSL | Linux import of broker protocol/client/worker/isolation modules: `WSL_IMPORT_OK`. |
| Static | `git diff --check` passed; obsolete-topology search returned no match. |
| Protected/dependencies | No diff in dependency manifests, JobManager, CST result owner, HFSS, or hub process owner. |
| Publication | Source scan clean (101 files); tests scan clean (81 files). A broad package scan remains noisy on pre-existing README/test-fixture literals and is not claimed clean. |
| Processes | Worker query matched only its own inspection shell. Pre-existing CST PIDs 7624, 7636, and 10032 were observed and not mutated. |

## Residual target gates

P16 candidate formation and all Git/index/history work remain with Lead. Live SCM
provisioning/rollback/readback is P18. Target CST activation and descendant proof,
Claim 7, existing-six target smoke, and Line10 Claim 15 are later target gates. The
host's synthetic breakaway creation is correctly fail-closed and therefore does not
claim the target descendant-denial result.

## Gate

**PASS — P10-P15 implementation and safe verification are complete within the
accepted non-live scope.**

## Terms and Abbreviations

- CST: CST Studio Suite.
- DACL: Discretionary Access Control List.
- Job: Windows Job object used for process-tree containment.
- QPC: Query Performance Counter, the Windows monotonic clock.
- SACL: System Access Control List.
- SCM: Service Control Manager.
- WSL: Windows Subsystem for Linux.
