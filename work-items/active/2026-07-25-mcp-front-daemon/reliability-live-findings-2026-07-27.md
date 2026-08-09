# PR #588 R3 recovery reliability repeat gate

Date: 2026-07-27  
Role: `$reliability-engineer`  
Review round: R2 of the R3 contract  
ADR SHA-256: `6DC6DC4478F05044DBD5E58E08F7DF0E4D4A87AA4161BE20E02B46B93330028F`  
Design SHA-256: `A42A6CDF0BDEEF1502640269E305D1F6D3F09E05F72789810A125FA6CD6C106B`

## Verdict

**PASS — planner-eligible.**

The amended R3 contract closes RR3-01 and RR3-02 without weakening the accepted
same-config dependency, fail-closed uncertainty, independent-progress, lock,
migration, or retirement rules. This is design acceptance, not implementation
evidence.

## Prior findings closure

| Finding | Amended contract | Reliability verdict |
| --- | --- | --- |
| RR3-01: Windows all-component no-follow was an outcome without a race-free mechanism | The API owner opens the root with `windows.NtCreateFile`, `FILE_OPEN_REPARSE_POINT`, and `OBJ_DONT_REPARSE`; opens every child through `OBJECT_ATTRIBUTES.RootDirectory`; inspects every handle for reparse attributes; requires directory intermediates and a regular final file; performs final-handle volume/root checks only as defense-in-depth; reads/hashes that handle once; and closes every handle before mutation (`ADR:220-287`). POSIX uses a root descriptor plus per-component `openat` and `O_NOFOLLOW` (`ADR:252-258`). Path precheck plus absolute open is explicitly forbidden (`ADR:278-283`). | PASS |
| RR3-01: pin read had no named bound owner | The API reader uses the existing `maxStateFileBytes` 1 MiB owner through `io.LimitReader(..., cap+1)` (`ADR:232-236,273-276`). Current source confirms `maxStateFileBytes = 1 << 20` at `internal/api/state_read_caps.go:9-11`. | PASS |
| RR3-02: no-invocation conflict erased older rollback authority | `precondition-conflict` now retains an existing prior `Applied` receipt and never creates a new one (`ADR:168-177,311-321`). First generation remains pinless and authority-free (`ADR:297-310`). Rollback can invert only the retained exact receipt; diverged state performs zero inverse writes (`ADR:313-321`). | PASS |
| RR3-02: terminal conflict had no monotonic forward-retry lifecycle | I10 and forward admission now refuse every byte-identical or changed-plan forward entry while any pending or terminal disposition exists; only explicit rollback may advance/retire it (`ADR:101-109,179-194,323-333`). Automatic retries/backoff are forbidden and each admitted plan operation invokes its adapter at most once (`ADR:207-211`). | PASS |

## Regression sweep

| Reliability surface | Owner and invariant | Verdict |
| --- | --- | --- |
| Same-config multi-entry dependency authorization | `clients.lockingClient` reads target and every dependency, durably prepares, invokes one target mutation, and observes the group under one config lock (`ADR:117-160`). Callers and concrete adapters cannot duplicate the capability. | PASS |
| Uncertainty classification | One CLI classifier is policy-free; `prepared` and post-write conflict block forward and become rollback-local uncertainty. Equality after re-entry never manufactures a receipt (`ADR:165-205`). | PASS |
| Row-only Serena no-write conflict | First generation is the only pinless shape. Prior-owned conflict retains the exact receipt and pin and becomes rollback-pending (`ADR:290-336`). No second authority exists. | PASS |
| Independent rollback progress | Structural pin invalidity is one global pre-write refusal; runtime row/group uncertainty remains local; all independent safe inverses run before aggregate pending return (`ADR:340-356`). | PASS |
| Lock order and resources | Operation lock → one config lock → short journal lock; no operation/second-config acquisition under config lock. Secure-pin handles close before any config lock and every exit path owns cleanup (`ADR:358-374`). | PASS |
| Disposition and retirement | Only rollback advances dispositions; active disposition blocks forward; durable all-terminal re-read is the sole retirement proof (`ADR:101-109,343-350,420-423`). | PASS |
| Compatibility and migration | Version 1, version 2, and malformed/interim version 3 remain read-only refusals with zero client mutation and no silent repair (`ADR:541-558`). | PASS |
| Change surface | Secure readers move to the stable API state-read owner and consume its unchanged cap; CLI owns orchestration but not filesystem primitives or a duplicate cap (`ADR:376-407`). Protected install/route/state-path and `.codegraph*` surfaces remain excluded. | PASS |

## Reliability objectives

These release gates are measured at the CLI/wrapper boundary.

| Objective | Service-level indicator and measurement point | Window | Threshold | Burn consequence |
| --- | --- | --- | --- | --- |
| No unauthorized mutation | Wrapper mutation count after any target, dependency, row-authority, or pin precondition fails | Every invocation and release test run | `0` | One event exhausts the budget and blocks publication |
| Route preservation | Injected dependency races ending without canonical or required legacy route, measured by API integration harness | Every injected race case | `0` | One event exhausts the budget |
| Secure pin authority | Inverses consuming bytes other than the single bounded final-handle read, measured at API-reader/CLI-inverse seam | Every platform matrix case | `0` | One event exhausts the budget |
| Durable recoverability | Retirements without an all-terminal durable re-read, measured at CLI retirement owner | Every rollback invocation | `0` | One event exhausts the budget |
| Independent progress | Eligible independent groups restored after another group becomes pending, measured from durable dispositions | Every mixed-failure case | `100%` | Any suppressed safe group blocks release |
| Diagnosability | Failed/pending rows carrying stable discriminator and row/group identity at return and in the durable report | Every failure path | `100%` before command completion | Missing identity or discriminator blocks release |

## Retry, idempotency, degradation, and recovery

- Maximum automatic attempts: **one** adapter invocation per admitted row.
  There is no hidden retry loop, backoff, or jitter.
- Operator command re-entry is the only retry and must pass durable
  classification plus forward admission.
- Idempotency identity is exact row key, generation, and operation. The
  settled/committed event is the durable row write containing an exact applied
  receipt or same-call no-invocation result (`ADR:199-211`).
- Forward degrades to zero-write refusal on structural invalidity, uncertainty,
  or any active disposition.
- Rollback localizes runtime uncertainty to one Serena row or LSP group,
  completes independent safe work, retains the report, and returns aggregate
  pending state.
- A crash after mutation but before receipt remains explicit uncertainty;
  manual resolution is safer than inferred causation.
- Operation/config/journal locks and all root/intermediate/final handles have
  explicit cleanup on success, failure, cancellation, and size/hash refusal.

## Critical failure detection

The design review is `analysis-only`: implementation has not yet been admitted,
so none of these injection probes has executed. The top failure mode remains
analysis-only solely because the platform reader does not yet exist.

| Failure mode | Detection signal | Detection latency | Response | Verification state |
| --- | --- | --- | --- | --- |
| Reparse/link or component swap | `serena-pin-open-unsafe`, row key, zero mutation count | Before first inverse | No page; fail local command and block release on test failure | analysis-only; root/intermediate/final/swap matrix required |
| Pin exceeds 1 MiB | `serena-pin-too-large` | During pre-write bounded read | No page; global zero-write refusal and close all handles | analysis-only |
| Prior receipt lost or replaced by no-write conflict | Receipt/attempt/disposition transition assertion | Same transition or next rollback classification | No page; retain report, zero synthesized ownership | analysis-only |
| Active disposition receives forward replay | `forward-recovery-disposition-active` | Admission before generation/plan mutation | No page; preserve report and client bytes exactly | analysis-only |
| LSP dependency changes | `dependency-precondition-conflict` | Same wrapper call before target mutation | No page; keep group pending and continue independent groups | analysis-only |
| Post-invocation ownership unknown | `forward-ownership-unknown` | Same-call readback | No page; stop forward and retain report | analysis-only |
| Nonterminal durable re-read | `rollback-recovery-active` | Before retirement | No page; keep report and return aggregate pending | analysis-only |

## Rollout and rollback readiness

| Stage | Abort signal, threshold, and observation window | Required drill |
| --- | --- | --- |
| Planning | Any step omits an ADR invariant, owner, or mandatory falsifier; zero omissions per plan review | Map the entire acceptance matrix to implementation and mutation proof |
| Implementation | Any mandatory falsifier does not fail under controlled mutation and pass after restoration; zero tolerated in one QA session | Windows/POSIX secure-read, prior-receipt, disposition-replay, dependency, uncertainty, and retirement drills |
| Local commit | Any scoped tagged test/build/vet failure or missing mutation proof; zero tolerated in final verification | Forward/rollback crash-window and v1/v2 byte-identity drills |
| Publication | Any architecture `REVISE`, leak-check failure, or missing human review | `ASSUMPTION (UNVERIFIED)` until final review and human publication gate |

No production rollout is authorized here. Runtime rollback remains
`ASSUMPTION (UNVERIFIED)` until the R3 command-owner drills execute against the
implementation.

## Numbered reliability claims

1. `{ guarantee: every Windows pin byte consumed by rollback comes from one
   bounded final handle reached through root-anchored component-wise
   no-reparse traversal; single-owner: API Windows beneath-root reader;
   enforcement-probe: root, intermediate, final, component-swap, size, and
   handle-leak matrix observes zero client writes on every refusal }`.
2. `{ guarantee: POSIX pin authority is a root descriptor plus per-component
   openat with O_NOFOLLOW and a regular final fstat; single-owner: API POSIX
   beneath-root reader; enforcement-probe: root/intermediate/final link and
   pathname-swap matrix cannot escape the root or leak a descriptor }`.
3. `{ guarantee: a no-invocation conflict creates no new ownership and cannot
   erase a prior applied receipt; single-owner: CLI classifier and finish
   transition; enforcement-probe: first-generation and prior-receipt matrix
   authorizes only the retained exact compare-and-swap inverse }`.
4. `{ guarantee: pending or terminal rollback disposition cannot be cleared or
   hidden by forward re-entry; single-owner: CLI forward admission;
   enforcement-probe: byte-identical and changed-plan replay preserve
   generation, plan, rows, pins, and adapter counts }`.
5. `{ guarantee: LSP target mutation is authorized by every exact dependency
   under the same config lock; single-owner: lockingClient group mutation;
   enforcement-probe: forward and rollback dependency-boundary edits invoke
   zero unsafe target mutations }`.
6. `{ guarantee: rollback uncertainty blocks only its Serena row or LSP group
   and independent safe inverses complete; single-owner: CLI rollback policy;
   enforcement-probe: mixed uncertain/applied Serena and LSP cases persist
   independent progress before aggregate pending return }`.
7. `{ guarantee: an active report retires only after a durable all-terminal
   re-read; single-owner: CLI retirement gate; enforcement-probe: persistence
   failure and one pending row each preserve the active report }`.

Every claim has one owner and a deterministic falsifier. None adds a fallback,
workaround, compatibility projection, or second authority.

## Residual assumptions

- **ASSUMPTION (UNVERIFIED):** Windows and POSIX root/intermediate/final
  link/reparse, component-swap, size-bound, cancellation, and handle-leak tests
  pass on their target platforms. This is an implementation/QA gate, not an
  architecture premise.
- **ASSUMPTION (UNVERIFIED):** final mutation proofs, scoped tagged tests,
  build, vet, and external architecture review pass. These remain downstream
  gates.

The former pin-size-owner assumption is closed by
`internal/api/state_read_caps.go:9-11`.

## Planner gate

**PASS — RETURN(planner).** Planning may proceed only from ADR SHA-256
`6DC6DC4478F05044DBD5E58E08F7DF0E4D4A87AA4161BE20E02B46B93330028F`
and must map every mandatory falsifier to one implementation step, controlled
defect mutation, restored green run, and owner.

## Terms and Abbreviations

- **ADR** — architecture decision record.
- **CAS** — compare-and-swap.
- **CLI** — command-line interface.
- **FD** — file descriptor.
- **LSP** — Language Server Protocol.
- **R3** — the third correction round.
- **Reparse point** — a Windows filesystem object that can redirect path
  resolution.
- **SLI** — service-level indicator.
- **TOCTOU** — time-of-check/time-of-use.
