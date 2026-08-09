# PR #588 external architecture R2 reconciliation

Date: 2026-07-27  
Owner: `$lead`  
Outcome: **REVISE — return to architecture**

## Execution integrity

| Lane | Complete artifact | Exit oracle | Gate | Evidence status |
| --- | --- | --- | --- | --- |
| Claim verification | `.scratch/external-reviews/r2-claim.out`, SHA-256 `1F7DBAF2CC49F367491814D8ADDC4FB759FDA1676CB2BE7E88C79AF98927EDE0` | Missing: the Windows `.cmd` parent exited before the still-running Node/Codex child, so its post-command exit write never ran | `REVISE` | Substantive findings accepted after main-session source verification; not a counted final gate |
| Adversarial | `.scratch/external-reviews/r2-adversarial.out`, SHA-256 `763FD04D7C3D59C914DB0C8E06B7F4260191C0B6F7A6C8C6CFE155280ABAFA9F` | Missing for the same transport reason | `REVISE` | Substantive findings accepted after main-session source verification; not a counted final gate |

Both artifacts end in an explicit `Gate: REVISE` and contain no
authentication, quota, truncation, timeout, or silent-result failure. The
claim artifact's word `unauthorized` is its own reported clean path count, not
a provider error (`r2-claim.out:31`).

## Verified findings

| ID | Severity | Verified root cause | Required correction class |
| --- | --- | --- | --- |
| AR2-01 | High | Forward legacy removal compares only its own target after `canonicalReady` was established by an earlier operation (`internal/api/lsp_client_router.go:277-317`). Rollback decides legacy readiness before a later canonical inverse whose conditional matcher covers only canonical (`internal/api/lsp_client_router_snapshot.go:313-390,455-475`). | Wrapper-owned target mutation authorized by same-lock predicates over every route dependency; no additional point-in-time check |
| AR2-02 | High | The effective-receipt owner treats `prepared` and `conflict` as uncertain, but re-entry settlement handles only `prepared` (`internal/cli/install_reconcile_mcp_front.go:282-313`). Same-call third-state readback persists `conflict` (`:1441-1449`), after which a forward retry can replace `ActivePlan`/generation. | One uncertainty classifier; forward rejects both states, rollback leaves both pending and continues independent rows/groups |
| AR2-03 | High | Pre-write pin validation hashes the path (`internal/cli/install_reconcile_mcp_front.go:1082-1124`), then Serena rollback independently reopens it (`internal/api/serena_client_reconcile.go:734-740`). Lexical containment does not reject a symlink/reparse target. | Open/validate each pin once through a no-follow contained-file owner, retain exact verified bytes, and pass that buffer to the inverse |
| AR2-04 | Medium | First-generation Serena precondition conflict has no row because the wrapper rejects before `BeforeMutation`; `finishAttempt` returns success when the row is absent (`internal/cli/install_reconcile_mcp_front.go:1386-1393`). | Row-only durable no-write outcome that owns no inverse/pin and cannot be mistaken for mutation authority |
| AR2-05 | Medium | Rollback calls settlement and returns on one uncertain row before pin validation or processing any independent safe row (`internal/cli/install_reconcile_mcp_front.go:700-707`). | Separate classification from caller policy: forward blocks; rollback persists uncertainty and continues independent groups |
| AR2-06 | Medium | Guards omit: full F2 no-invocation table; durable-conflict plan replacement; malformed pin matrix; command-owner version-1/version-2 byte/no-write refusal; dependency edits between group operations; pin swap/link cases (`internal/cli/install_reconcile_mcp_front_v3_test.go:148-183,253-263,295-327`; `internal/api/lsp_client_router_plan_test.go:80-238`). | Deterministic real-owner falsifiers for every omitted participant |

## Class-completeness sweep

| Class | Participants requiring R3 disposition |
| --- | --- |
| Multi-entry authorization | Forward canonical readiness and every legacy removal; rollback every legacy readiness result and the dependent canonical add/remove |
| Uncertainty | `prepared`, post-write `conflict`, forward admission, rollback classification, plan/generation replacement, retirement |
| Pin authority | Path containment, link/reparse rejection, single read, checksum, retained bytes, Serena inverse consumer, zero writes on any invalid pin |
| No-write Serena conflict | First generation, later generation, row validation, pin optionality, plan result, rollback/retirement non-authority |
| Independent rollback progress | Serena rows, unrelated LSP groups, uncertain rows, disposition durability, terminal retirement |
| Compatibility refusal | Version 1 and version 2 through both forward and rollback owners, byte identity, zero adapter/state writes |

No finding changes the already-closed original C1-C10 classification. Protected
`internal/cli/install.go` and `internal/cli/route.go` remain outside the
production diff.

## Concurrent worktree event

During the external runs, another user/Claude session created commit
`31b9ca94` and started CodeGraph initialization across four worktrees. Process
command lines identify that separate Claude session. This lead session did not
create, amend, reset, or remove that commit or the `.codegraph*` artifacts.
R3 must be a new correction atop `31b9ca94`; no history rewrite is authorized.

## Next gate

The architect must revise the version-3 ADR/design for AR2-01 through AR2-06.
Reliability must re-review the changed persisted-state/concurrency contract,
then the planner may admit an exact R3 implementation surface.

## Terms and Abbreviations

- **CAS:** compare-and-set.
- **LSP:** Language Server Protocol.
- **MCP:** Model Context Protocol.
- **R3:** the correction round after the two external R2 reviews.
- **TOCTOU:** time-of-check/time-of-use.

Gate: **REVISE — RETURN(architect)**
