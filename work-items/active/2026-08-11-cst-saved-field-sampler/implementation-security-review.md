# W9 Independent Security Review — Immutable Working Candidate

Review target: commit `bab886092ae0a4148c05f1e057eeedd73731eedf`, tree
`1850fd616f6585b31727c725b37de236c09d527a`, 66 changed paths.

Immutable inputs were re-hashed in this review:

| Artifact | SHA-256 |
|---|---|
| `design.md` | `7423D56DD33394336A06AB8C515D12F4496B4AC0533F2901BF5A0EE1436756ED` |
| accepted authority-containment decision | `8EAD15D041781A05ED192107D997693E27B79175C0D0125BB09F1C0A6DE8696A` |
| `implementation-w7.md` | `1E099E74EDFCF098B042A4D4385EFFC962729FE566A2C5C87CFE835CCEF6C9E1` |
| `implementation-architecture-review.md` | `64A1192479246386A0E6CCC7506B15D9925C3DF63C4EDB2093F58686E85D5188` |
| `implementation-security-constraints.md` | `153328F5DF914EA969B8D6E945A604E81CCE1B556B7112083B7D52CD5EBD2EF8` |

The shared worktree contained unrelated edits. CodeGraph was used first, then its
`status -> sync -> status` sequence reported the index up to date. Later pending-file
notices caused by other shared-worktree activity were not used as immutable evidence.
All terminal source and executable probes below ran from a clean `git archive` of the
exact candidate under `/.scratch/`. This review changed no source, Git index, service,
policy, virtual disk, installed CST state, or live process topology.

## Reviewed surfaces

- Go launch owner: `internal/daemon/host.go`, `launch_capability.go`,
  `launch_capability_windows.go`; direct-launch composition in `internal/cli/daemon.go`
  and its CST contract tests.
- Native barrier: `native/cst-runtime/mcphub_cst_runtime.c`, runtime manifest,
  closure builder and independent PE verifier.
- Fixed endpoint and transport owners: `cst_saved_field_endpoints.py`, daemon service,
  broker client/service, broker-worker protocol and frontend protocol.
- Authority and lifetime owners: `cst_saved_field_containment_windows.py`, broker worker,
  policy, transfer/path identity, vendor isolation and production composition.
- Adversarial tests: native W2; enrollment; frontend; daemon; broker pipe/service;
  containment; path identity; safety transfer; production composition; worker pre-main.

The observed chain remains:

`StdioHost -> exact native frontend -> enrollment/frontend pipe -> daemon admission -> broker pipe -> atomic Job/five-handle worker -> native pre-main -> closed worker application`.

The native composition root clears and reads back the three standard-handle inherit flags
before role handling (`mcphub_cst_runtime.c:27-31,138-149`). The worker path additionally
validates exact five-role bootstrap data, directory identities and access masks, clears both
directory handles, and emits `python_initialized=false` before any package route
(`mcphub_cst_runtime.c:107-120`). The broker creates the worker already in the Job with one
explicit handle list and independently records created-worker identity, pre-request identity,
Job membership, handle roles and settlement events
(`cst_saved_field_containment_windows.py:1384-1396,1700-1764,1900-1955`). Thus the static
native startup frame cannot by itself manufacture kernel containment success.

## Independent adversarial findings

No critical, high, medium or low security finding was found on the reviewed candidate.

The most aggressive falsification was the native-startup receipt boundary. The worker emits
a closed startup frame, but the broker conjoins its Job observation and separately validates
`KernelContainmentEvidenceV1`, exact five inherited roles, native pre-main receipt and complete
all-return settlement before accepting a result. Mutation tests reject missing or altered
facts. This is owner-local evidence composition, not a caller-supplied or literal settlement
shortcut.

Named-pipe squatting, peer spoofing and replay do not gain candidate authority: endpoints are
fixed by one registry (`cst_saved_field_endpoints.py:5-10`); descriptor, local-only,
single-instance, peer-token/service/session/image, impersonation-revert and one-use nonce
matrices deny before protected work. The production service remains default-off, so an
injected synthetic connector or startup proof is test composition only and cannot activate a
live service.

Source/workspace substitution is denied through no-follow handle identity, exact access/share
masks, reparse/hard-link/stream/reserved-device rejection, manifest equality and post-open
revalidation. Image and manifest files are held and rechecked immediately before process
start. No pathname, wrapper, shell, PATH, `uvx`/`npx`, ordinary-Python or alternate broker
fallback was found. One replacement literal scan match was the legitimate vendor-owned
`_settlement(...)` constructor; it is not a hardcoded success route. The first wildcard-based
scan was invalid on Windows and is not evidence.

## Phase security checklist

| Surface | Result | Evidence |
|---|---|---|
| Untrusted input crossing a trust boundary | found | Closed frame schemas reject unknown fields and oversize/truncated frames; caller data selects no path, process or endpoint. Path traversal/substitution matrices and fixed-pipe tests completed with exit 0. No server-side request forgery surface exists because the route has no caller-selected network target. |
| Authorization/object substitution | found | Entry, revision, correlation, capability digest, peer identity, policy and request hash substitutions deny before broker/source work; replay tests completed with exit 0. |
| New or updated dependency | not-applicable | The 66-path candidate changes no dependency manifest or lockfile. Native imports are independently allowlisted and verified as KERNEL32-only. |
| New config or flag | found | Positive provision/package/policy receipts are required to enable work; absent, disabled, malformed or target-unsettled state keeps the seventh tool absent with zero protected work. |
| Agent- or large-language-model-facing surface | not-applicable | Runtime accepts a closed MCP tool request, not model instructions, code, prompts or generated commands; no untrusted-content execution route exists. |

## Fresh exact-snapshot evidence

| Probe | Terminal result |
|---|---|
| CodeGraph MCP owner/call-path queries after freshness sequence | Core launch, containment and receipt owners returned without candidate-path stale evidence after sync; later shared-worktree pending notices were excluded. |
| Exact commit/tree and `git show --check` | Commit/tree matched the review target; 66 paths; check exited 0. |
| Focused Go launch/admission suite | `go test ./internal/daemon ./internal/cli -run 'Test.*(CstDirect|LaunchCapability|Enrollment|HandleList|StdioHost)' -count=1`: daemon 7.287 s, CLI 0.030 s, both exit 0. |
| Native image and manifest | `pwsh ./verify.ps1 -Unsigned`: exit 0; image SHA-256 `38D87C50F716E334F89628D4F35604534C3A35BDCF35F378F1D939137BAB89E3`. |
| Independent adversarial Python selection | 382 collected cases across 12 native/enrollment/frontend/daemon/broker/containment/path/transfer/composition/pre-main files; all completed with exit 0 under CPython 3.13.11. |
| Forbidden weak-route scan | Replacement directory-root scan found no unavailable transport, fake/caller receipt, shell launch, dynamic loader/Python initialization, `uvx`/`npx`, or literal settlement-success route. One legitimate vendor `_settlement` constructor was inspected. |

## Exact 18 S4 claim reconciliation

Canonical verdicts are `verified`, `failed`, or `not-verifiable (with reason)`.

| Claim | Verdict | Independent attack, owner and failure oracle |
|---:|---|---|
| 1 | verified | Entry/revision/hash substitutions were exercised against daemon admission and broker reauthorization; those two owners return stable denial before source or worker creation. |
| 2 | verified | Supervisor/frontend PID, token, session and image substitutions fail in the enrollment authenticator before digest enrollment; fabricated frame identity cannot replace kernel-authenticated identity. |
| 3 | verified | CST identity authorization accepts only its closed operation at the supervisor boundary; opcode/identity substitutions perform no generic control action. |
| 4 | verified | The single three-endpoint registry and endpoint descriptor/remote/second-instance/peer/revert tests reject pipe squatting or spoofing before parse/work; live service enforcement remains within Claim 17. |
| 5 | verified | Go admits one direct image and one additional capability handle; native code clears/readbacks all four frontend handles before role/package work. Extra handle, identity and start-failure mutations cancel enrollment. |
| 6 | verified | Enrollment capability and challenge ledgers are independent, one-use and terminal on ACK loss, expiry, cancel, disconnect, exit, shutdown and restart; no stranded authority can be replayed. |
| 7 | verified | Daemon admission binds image, capability, challenge, correlation, request hash, generation, entry and one QPC deadline. Every mutated binding produces zero broker work. |
| 8 | verified | Daemon owns admission/routing and the same-process frontend owns publication; dependency/call-path inspection found no direct frontend-to-broker or daemon-to-CST bypass. |
| 9 | verified | Broker peer authentication, impersonation and proved revert precede nonce/source work; failed revert is quarantine-worthy and peer substitutions are denied. |
| 10 | verified | Broker nonce, correlation, request, policy, manifest and deadline are consumed or cancelled atomically; replay, framing, timeout, disconnect and shutdown tests cannot retry through a fallback. |
| 11 | verified | Broker alone opens least-right source/workspace handles and transfers exact manifest-bound duplicates. Reparse, alias, stream, writer, share/access, identity and post-open drift produce no child or quarantine. |
| 12 | verified | Native worker accepts exactly five roles, clears all five inherit flags, verifies both directory identities and emits a bound pre-main receipt with `python_initialized=false`; absent package admission exits 78 before package code. |
| 13 | verified | The broker-wide inheritance epoch is non-reentrant, the explicit handle list is exact and the Job accounts for descendants; residual sibling/descendant authority or cleanup ambiguity quarantines. |
| 14 | verified | Frontend, daemon transport, broker transport, worker application and kernel containment receipts contain only owner-local observations. Unknown/missing/contradictory bits and incomplete settlement cannot form success. |
| 15 | verified | One unchanged QPC triple originates at daemon admission and crosses broker/worker validation; frequency/tick/deadline mutation or staged delay returns deadline failure followed by bounded settlement. |
| 16 | verified | Existing six remain on their prior contract and the seventh has one native/daemon/broker route. No wrapper, ordinary Python, alternate endpoint or unavailable-transport fallback was found; absence yields exact-six default-off behavior. |
| 17 | not-verifiable (target-only: installed CST, real SCM service identity and descriptor, App Control package admission and disposable-target Job/process trace do not exist in W0-W10) | X3 must prove locked inputs, sealed output, exact hidden non-breakaway Job, ResultTree behavior, full cleanup and preservation of a foreign CST process. Synthetic ports and local process tests cannot promote this claim. |
| 18 | verified | Candidate activation requires positive provision/package/policy evidence and contains no signing, policy deployment, VHDX, CiTool, SCM registration, installed-CST, manifest-pin or publication path. Missing target state leaves the seventh tool absent and performs zero target mutation. |

Matrix total: 17 `verified`, 0 `failed`, 1 `not-verifiable (with reason)`; Claim 17
alone is target-only. Architecture Claims 7 and 15 remain target-only under their separate
34-claim matrix and are not promoted here.

## Residual risk and required boundary

Malicious package/DLL/TLS behavior under enforced App Control, real service discretionary
access control, VHDX namespace continuity, hardware security module ceremony/audit, installed
CST behavior, Line10 comparison, manifest pinning, publication and deployment require X1-X6.
The candidate proves only an executable local, synthetic-authority, default-off working result.
Any source or candidate identity change invalidates this review. No publication-safety
exception is requested or approved.

## Gate

`PASS`

## Terms and Abbreviations

- App Control: Windows application-control policy enforcement.
- CST: Computer Simulation Technology electromagnetic solver.
- HSM: hardware security module.
- MCP: Model Context Protocol.
- PE: Portable Executable.
- QPC: Query Performance Counter monotonic clock.
- S4: the accepted numbered security-claim set.
- SCM: Windows Service Control Manager.
- VHDX: Hyper-V virtual hard disk format.
