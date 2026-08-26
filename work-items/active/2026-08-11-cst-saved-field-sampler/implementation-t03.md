# T03 Backend Implementation — Neutral Contracts, Policy and Budget

Gate: **PASS** — strict RED/GREEN, focused schema/policy/budget tests, Ruff, format and current CodeGraph impact evidence satisfy the T03 boundary.

Execution role: `$backend-engineer` under `$lead`. Scope: T03 only. Baseline: `5ff268dc13b2be9ca9500b5441634f0594538b94`.

## Receiving-side echo and invariant

Accepted design `AFABC3C...`, decision `18307E...`, reviews `475606E...`, `A0F0D2...`, `238059A...`, plan `8DD78E...`, and T02 artifact `AE0C697...` were re-hashed before work. The neutral port owns the one immutable integer QueryPerformanceCounter triple and contains no CST or Windows implementation import. Frontend, broker and broker-worker schemas are closed/canonical/bounded and preserve one correlation/request-hash/budget chain. The independently loaded policy is exact V1, default-off, canonical, manifest-v2 bound, and includes exactly the enrollment/frontend/broker descriptors. No service or CST call is introduced.

| Owner | T03 surface |
|---|---|
| Neutral port | `cst_saved_field_port.py`: immutable `AbsoluteInvocationBudget` plus existing request/vendor/acquisition/batch/failure/receipt and `AuthorizedVendorPathLease` types. |
| Frontend schema | New `cst_saved_field_frontend_protocol.py`: authority-free request/result and locally split daemon/frontend receipts. |
| Broker schemas | Existing broker and broker-worker modules now depend on the neutral budget owner; their canonical closed frame and nested response contracts remain wire-compatible. |
| Policy | `cst_saved_field_policy.py`: accepted V1 schema, exact three descriptors, manifest-v2 identity and verified-file/default-off validation. |

## RED/GREEN and MCP evidence

| Stage | Receipt |
|---|---|
| RED | Focused T03 pytest exited 1 with 3 failures/2 passes: frontend protocol missing, neutral budget missing, and policy still V2. |
| GREEN focused | 29 policy, deadline, frontend, broker and worker protocol tests passed. |
| Affected saved-field surface | All collected saved-field tests passed except the T00 topology scaffold, which remains deliberately RED on future T04+ service/daemon/broker owners and prohibited pre-refactor `cst.py` edges. It is not a T03 regression oracle. |
| Static | Ruff check passed; seven changed Python/test files were already formatted; `git diff --check` passed. |
| CodeGraph | Initial post-change query did not resolve the new Python symbols and was rejected as evidence. The immediate exact-path re-query found current `AbsoluteInvocationBudget` at `cst_saved_field_port.py:11`, its `QpcDeadlineV1` extension edge, two broker-protocol uses and affected tests. No stale/disabled banner appeared. |
|  | A second exact `BrokerChallengeV1` query found the live vendor-isolation/client call chain and the later-phase issued/admitted-tick guard; no T03 scope expansion was made. |

## AC state, wire change and rollback

| AC | State |
|---|---|
| T03-AC01 | PASS: Hub enrollment from T02 plus frontend, broker and worker V1 owners; closed canonical frames reject duplicates, noncanonical/trailing data and over-limit frames. |
| T03-AC02 | PASS: policy absent/invalid/disabled is default-off; V1 requires canonical held-file access, unique IDs, exact descriptors and manifest-v2 identity. |
| T03-AC03 | PASS: `AbsoluteInvocationBudget` owns the exact 60-second triple; protocol `QpcDeadlineV1` is a wire-compatible subtype; cleanup alone derives termination plus 10 seconds. |
| T03-AC04 | PASS: neutral immutable records and `AuthorizedVendorPathLease` contain no CST/Windows implementation import. |
| T03-AC05 | PASS: focused malformed/ceiling/hash/correlation/budget/policy matrices are green. Runtime nonce replay state is implemented by later protocol services, not this value-only phase. |

Wire change: persisted policy schema changes from the unaccepted pre-phase V2 shape to accepted V1 and adds required `endpoints` plus `manifest_schema`; disabled/invalid remains fail-closed. No installed policy exists or was changed. Broker deadline wire keys remain exactly `{qpc_frequency,admitted_tick,deadline_tick}`. Frontend request carries only schema/correlation/challenge/entry/request hash/request; it contains no path, bytes, handle, manifest or policy authority.

Rollback is one T03 group: remove the frontend protocol/T03 tests; revert neutral-budget and policy V1 hunks; restore broker deadline definition and policy tests. No unrelated dirty path belongs to this group.

## Terms and Abbreviations

- CST: Computer Simulation Technology.
- QPC: Windows QueryPerformanceCounter.
- V1: version one of a closed schema.
