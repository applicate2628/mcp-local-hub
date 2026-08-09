---
id: 2026-07-30-vcpkg-mcp-bounded-public-result-contract
title: Bounded public result and filesystem ingress contract for vcpkg MCP
status: accepted
date: 2026-07-30
updated: 2026-08-01
revision: 5
owner: architect
review-gate: architecture-reviewer
review-result: PASS
review-artifact: ".reports/2026-07/report(architecture-reviewer)-2026-07-30_pr591-corrective-implementation-r3-review.md sha256=63F29A1B7E35923E4197AB94B3D317AA490C73E1F17AE130E39CF78376707366"
lead-acceptance: accepted
accepted-date: 2026-07-30
semantic-body-sha256: D4510321F00C5F057D5F698E31D46706BF877787C873348460B29B1761BEEC85
supersedes: none
superseded-by: none
---

# Bounded Public Result and Filesystem Ingress Contract for vcpkg MCP

Decision ID: `2026-07-30-vcpkg-mcp-bounded-public-result-contract`

Required acceptance: architecture-reviewer `PASS`, then explicit Lead acceptance,
before implementation.

## Context

The vcpkg Model Context Protocol (MCP) server has seven public tools, one shared
JSON serializer, several producer-side limits, and package-owned result shapes.
The long-lived contract must bound both work and exact encoded output, preserve
causal failure identity, report evidence completeness honestly, and keep process
and filesystem ownership in the correct dependency layer.

The current last-failure diagnostic-drop exactness owner already consumes four
upstream evidence domains:

- `directory_entries`;
- `relevant_logs`;
- `log_bytes`;
- `diagnostics`.

Those domains are independently mutable. In particular, exhausting a per-log or
aggregate byte budget can make `log_bytes=limited` while the diagnostic parser
state remains `diagnostics=settled_complete`. Terminal reason
`build_interrupted` does not make that incomplete byte stream complete.

## Relationship to existing decisions

| Decision | State under this revision | Relationship |
|---|---|---|
| `work-items/decisions/2026-07-25-vcpkg-mcp-tool-contracts.md` | `accepted`, current | Foundation. Its read-only, evidence, tri-state, closed-reason, and in-hub ownership contracts remain authoritative. This decision extends, and does not supersede, it. |
| `work-items/decisions/2026-07-29-vcpkg-pinstatus-remote-query-admission.md` | `accepted`, current, unsuperseded | Related but separate. It owns value-bearing-query admission and the package-private approved-URL capability. This revision neither supersedes it nor redefines its accepted capability. |
| `work-items/decisions/2026-07-30-vcpkg-mcp-bounded-public-result-contract.md` | `accepted`, current | Sole accepted owner of the universal bounded-result, bounded-ingress, public causal-core, exactness, dependency-direction, and three-record transfer contract defined here. |

The three decision records form one closed transfer manifest. Their exact
review-source identities are:

| Path | Required relation state | Exact source at revision-5 review |
|---|---|---|
| `work-items/decisions/2026-07-25-vcpkg-mcp-tool-contracts.md` | `accepted`, current foundation | SHA-256 `9B68F91B072CBBBF816B84A08B7BEF4709053E086CA239C7ADE9DE7F45C98B06`; Git blob `7ac411e3c52c9c292f3c0007d709325db0b7ae72` |
| `work-items/decisions/2026-07-29-vcpkg-pinstatus-remote-query-admission.md` | `accepted`, current, unsuperseded | SHA-256 `EB3A8C2DD6E07ADD99D81DF324CB6EF552E5FD360D62EFFBBC84F7C50DCC75D6`; content-derived Git blob `381a3195bbc07a4bc8de8e49270dfb460004b2a4` |
| `work-items/decisions/2026-07-30-vcpkg-mcp-bounded-public-result-contract.md` | `accepted`, current; revision 5 was proposed during review | The revision-5 design and review gate record the exact proposal file SHA-256, Git blob, and `semantic-body-sha256`; Lead acceptance records the resulting accepted file SHA-256 and Git blob |

The 2026-07-30 record cannot embed its own full-file digest without creating a
self-reference. Its immutable `semantic-body-sha256` therefore covers the exact
bytes from the first Markdown heading through end of file. Acceptance changes
only the frontmatter keys enumerated in the transfer section; the semantic body
and its digest do not change.

## Decision

1. `internal/vcpkgmcp/publicresult/publicresult.go` is the sole dependency-leaf
   owner of the universal 256-KiB indented-JSON result ceiling, additive
   `result_projection` shape, omission-reason vocabulary, structural
   `Projectable` contract, and exact encoded-byte measurement.
2. Every registered vcpkg MCP success result implements `Projectable` and reaches
   the single `vcpkgserver.jsonResult` boundary. A second serializer or an
   unconstrained handler result is prohibited.
3. Each tool package owns semantic projection, field priority, package
   status/reason, and its minimal result. A minimal result retains the causal core
   of every retained failure.
4. `internal/vcpkgmcp/boundedio/boundedio.go` is the sole neutral owner of paged
   file/directory admission and exact closure of handles it acquires. Tools own
   semantic budgets but do not reimplement generic paging/close logic.
5. A directory is returned only when its complete admitted contents reach
   end-of-directory within budget and can be canonically sorted. On
   limit-plus-one overflow, that directory contributes no arbitrary
   enumeration-order prefix; it is omitted as a whole with limited/unknown-total
   metadata.
6. Producer limits bound work and retention before serialization. The
   public-result boundary independently bounds exact encoded bytes. Neither layer
   substitutes for the other.
7. `result_projection` is an indefinite additive wire field. Existing
   complete-case fields, types, meanings, and ordering remain stable.
   Unknown-field-tolerant clients are compatible; strict decoders must admit the
   additive field before upgrade.
8. Domain-specific stable failure vocabularies remain package-owned. For pin
   status, one `PublicFailure` shape and one total projector preserve primary ID,
   ordered secondary cause IDs, safe exit code, and fixed sanitized detail
   through normal and minimal projections.
9. Raw remote text, credentials, paths, environment values, and unmatched stderr
   tokens have no public projection path. Bounded raw stderr may only feed a
   total fixed-category sanitizer.
10. Tool packages depend downward on `publicresult`, `boundedio`, evidence types,
    and approved infrastructure seams. The leaves import no tool/server package.
    `vcpkgserver` remains the composition root; `internal/process` remains the
    only platform process-containment owner.
11. `lastfailure.projectDiagnosticsDroppedExact` is the single exactness
    projector. It consumes the four independently-owned upstream domain states
    `directory_entries`, `relevant_logs`, `log_bytes`, and `diagnostics`; no
    terminal reason or one domain alone may manufacture exactness.

## Diagnostic-drop exactness algebra

Let:

- $D$ be `directory_entries`;
- $R$ be `relevant_logs`;
- $B$ be `log_bytes`;
- $G$ be `diagnostics`;
- $C$ be `settled_complete`;
- $A$ be `not_applicable`.

The public boolean is:

`diagnostics_dropped_exact = all_not_applicable OR active_exact`

where:

`all_not_applicable = (D=A) AND (R=A) AND (B=A) AND (G=A)`

and:

`active_exact = (D=C) AND (R=C) AND (B IN {C,A}) AND (G IN {C,A})`.

Every other combination is `false`. Across the seven terminal domain states,
this yields five exact combinations and 2,396 fail-closed combinations.

| Exactness class | `directory_entries` | `relevant_logs` | `log_bytes` | `diagnostics` | Exact |
|---|---|---|---|---|---|
| Entire diagnostic universe conclusively inapplicable | `not_applicable` | `not_applicable` | `not_applicable` | `not_applicable` | `true` |
| Active universe, bytes and diagnostics scanned | `settled_complete` | `settled_complete` | `settled_complete` | `settled_complete` | `true` |
| Active universe, no applicable diagnostic bytes | `settled_complete` | `settled_complete` | `not_applicable` | `not_applicable` | `true` |
| Active universe, bytes complete and diagnostic parser inapplicable | `settled_complete` | `settled_complete` | `settled_complete` | `not_applicable` | `true` |
| Active universe, byte input inapplicable and diagnostic accounting complete | `settled_complete` | `settled_complete` | `not_applicable` | `settled_complete` | `true` |
| Any other single-axis or cross-axis combination | any other combination |  |  |  | `false` |

The reachable counterexample
`directory_entries=settled_complete`,
`relevant_logs=settled_complete`, `log_bytes=limited`,
`diagnostics=settled_complete` is therefore `false`, including when terminal
output remains `unknown(build_interrupted)`.

The existing nine-domain by seven-state orthogonal completeness matrix remains
separate. It proves that terminal status/reason does not overwrite any one
evidence axis. The exactness guard then exhaustively evaluates the four-axis
algebra above across all $7^4=2,401$ combinations, with explicit single-axis
mutations from every true seed and relevant multi-axis producer combinations.

## Canonical sources and co-variation contract

The canonical identity is the repository-relative path and accepted Git blob,
not a workstation directory or worktree name.

| Exact path | Owner representation | Must co-vary when this contract changes |
|---|---|---|
| `work-items/decisions/2026-07-30-vcpkg-mcp-bounded-public-result-contract.md` | This long-lived decision, acceptance state, canonical-path registry, exactness algebra, supersession | Always; sole durable authority |
| `work-items/decisions/2026-07-25-vcpkg-mcp-tool-contracts.md` | Accepted foundational tool behavior and tri-state/evidence contract | Add/reference this accepted extension without duplicating its constants or vocabulary |
| `work-items/decisions/2026-07-29-vcpkg-pinstatus-remote-query-admission.md` | Separate accepted URL-admission capability | Relationship state only; do not supersede or copy its semantics into this decision |
| `internal/vcpkgmcp/vcpkgserver/tools.go` | Runtime MCP tool descriptions and input JSON Schema maps for all seven tools | Update descriptions and schema-linked limits/fields for `result_projection`, bounded behavior, and package-owned failure output |
| `internal/vcpkgmcp/vcpkgserver/helpers.go` | Sole external indented-JSON serialization boundary | Replace unconstrained `any` with the `Projectable` path and exact ceiling enforcement |
| `internal/vcpkgmcp/publicresult/publicresult.go` | Planned source owner of `MaxEncodedBytes`, `Projectable`, `result_projection`, omission vocabulary, exact marshal measurement | Source of truth for universal output constants and structural schema |
| `internal/vcpkgmcp/boundedio/boundedio.go` | Planned source owner of paged file/directory admission, limit-plus-one, whole-directory omission, and acquired-handle close | Source of truth for generic bounded-I/O semantics |
| `internal/vcpkgmcp/discovery/discovery.go` | `vcpkg_discover_root` output schema/projector | Additive result projection and semantic field priority |
| `internal/vcpkgmcp/lastfailure/types.go` | `vcpkg_last_failure` output schema/projector | Additive result projection and legacy field compatibility |
| `internal/vcpkgmcp/lastfailure/limits.go` | Nine evidence axes, resource metadata, and four-input diagnostic exactness projector | Exactness algebra, 9×7 independence, and fail-closed result |
| `internal/vcpkgmcp/portresolution/portresolution.go` | `vcpkg_port_resolution` output schema/projector | Additive result projection and semantic field priority |
| `internal/vcpkgmcp/pinstatus/types.go` | Batch/row output schemas and `PublicFailure` | Additive result projection, causal core, and batch/row ownership |
| `internal/vcpkgmcp/patchesapply/patchesapply.go` | `vcpkg_patches_apply` output schema/projector | Additive result projection and semantic field priority |
| `internal/vcpkgmcp/cmaketrace/cmaketrace.go` | `vcpkg_cmake_trace` output schema/projector | Additive result projection and semantic field priority |
| `internal/vcpkgmcp/cmakewrap/tool.go` | `cmake_include_graph` output schema/projector | Additive result projection and semantic field priority |
| `servers/vcpkg/README.md` | Canonical human-facing vcpkg MCP tool/result documentation | Document universal output ceiling, `result_projection`, bounded-ingress behavior, causal failure shape, and four-input exactness meaning |
| `README.md` | Top-level catalog entry linking to `servers/vcpkg/README.md` | No duplicate schema text; keep the link and read-only/tri-state summary accurate |
| `internal/vcpkgmcp/vcpkgserver/tools_test.go` | Existing tool-registration/source-shape gate | Enumerate all seven registrations and reject a serializer bypass |
| `internal/vcpkgmcp/vcpkgserver/public_contract_decision_test.go` | Repository-level three-record decision/co-variation guard | Assert all three exact records independently: accepted/current 2026-07-25 foundation, accepted/current/unsuperseded 2026-07-29 capability, accepted/current 2026-07-30 contract with reviewed semantic body; validate exact paths/blobs/constants/vocabularies and reject duplicate identities |
| `internal/vcpkgmcp/publicresult/publicresult_test.go` | Planned encoded-boundary/schema guard | N-1/N/N+1, normal/minimal, valid JSON, compatibility |
| `internal/vcpkgmcp/boundedio/boundedio_test.go` | Planned paging/lifetime/determinism guard | Request bounds, limit-plus-one, no tail/reopen, exact close, cancellation |
| `internal/vcpkgmcp/lastfailure/projection_test.go` | Existing completeness/causality guard | 9×7 matrix plus exhaustive four-axis exactness algebra and reachable producer cross-products |

There is no separate checked-in JSON Schema file for these seven tools at the
current candidate. Input JSON Schema is constructed in `tools.go`; output schema
is represented by the package Go result types listed above.

## Decision transfer and acceptance

### Closed three-record manifest

Before source implementation, the candidate branch must contain this exact
relation:

| Record | Required candidate status | Required candidate bytes |
|---|---|---|
| 2026-07-25 tool-contract foundation | `accepted`, current | Exact SHA-256 and Git blob from the review-source table above |
| 2026-07-29 remote-query admission | `accepted`, current, unsuperseded | Exact SHA-256 and Git blob from the review-source table above |
| 2026-07-30 bounded-result contract | `accepted`, current | Exact accepted SHA-256/Git blob recorded by Lead acceptance; Markdown semantic body byte-identical to the reviewed proposal and matching `semantic-body-sha256` |

The repository-relative paths, full content hashes, Git blob identities, parsed
statuses, relation states, and semantic-body identity together form the transfer
manifest. A matching basename, title, or status alone is insufficient.

### Review and metadata-only promotion

1. Revision 5 is materialized as `proposed` in the main orchestration worktree so
   the architecture reviewer can review an exact proposal SHA-256, Git blob, and
   semantic-body SHA-256.
2. The architecture reviewer gates that exact proposed body. A `PASS` does not
   itself mutate the decision.
3. On reviewer PASS, Lead may change only these frontmatter values:
   - `status: proposed -> accepted`;
   - `updated`;
   - `review-result: pending -> PASS`;
   - `review-artifact: pending -> <repo-relative review path plus SHA-256>`;
   - `lead-acceptance: pending -> accepted`;
   - `accepted-date: pending -> <YYYY-MM-DD>`.
4. `revision`, `owner`, `semantic-body-sha256`, decision relations, every
   Markdown-body byte, and every other frontmatter field remain unchanged.
5. Lead records the exact resulting accepted file SHA-256 and Git blob in the
   acceptance handoff. Any body or non-allowlisted metadata change invalidates
   the review and requires renewed architecture review.

### One-history transfer and staging-copy retirement

1. Before any source implementation, the integration owner transfers all three
   manifest records into candidate branch `feat/vcpkg-mcp` at their exact
   repository-relative paths.
2. The three exact blobs must be present in candidate ancestry before the first
   source implementation commit. A decision-only predecessor commit is preferred,
   but merge, rebase, cherry-pick, or byte-identical copy-and-commit are all
   allowed when they produce the same three final Git blobs and relation states.
   Mechanism identity is not an acceptance proxy; resulting blob identity is.
3. The main orchestration staging worktree must keep the exact tracked accepted
   2026-07-25 foundation. For the staging-only 2026-07-29 and 2026-07-30 files it
   must either resolve each path to the same transferred blob through tracked
   history, or retire that staging copy after its exact blob is safely present in
   candidate history. A retired staging path is absent; it may not remain
   untracked, modified, or divergent.
4. Other foreign worktrees are not mutated by this transfer. They are not
   authorities for this gate; any later branch admitted for implementation must
   independently satisfy the same manifest.
5. Candidate admission produces one immutable
   `decision-transfer-admitted` evidence record containing candidate commit,
   all three paths/statuses/SHA-256/Git blobs, semantic-body digest, main-staging
   resolution, and guard PASS. Until that record exists, implementation is
   blocked.

### Fail-closed repository guard

`TestVcpkgPublicContractDecisionReference` owns one table with three independent
rows. It must reject:

- missing record, wrong exact path, duplicate decision identity, or a second live
  file claiming the same decision;
- 2026-07-25 absent, `proposed`, superseded, body-drifted, or not equal to the
  accepted foundation SHA-256/Git blob;
- 2026-07-29 absent, still `proposed`, dropped, superseded, body-drifted, or not
  equal to the accepted SHA-256/Git blob;
- 2026-07-30 absent, still `proposed`, dropped, superseded without an accepted
  replacement, body-drifted, semantic-body-digest mismatch, non-allowlisted
  metadata drift, or accepted SHA-256/Git-blob mismatch;
- any canonical source path, constant, omission/failure vocabulary, exactness
  algebra, dependency owner, or relationship state drifting from this decision;
- candidate decision paths modified/untracked relative to the admitted commit.

The repo-local test proves the candidate tree. A separate read-only
`PR591DecisionStagingRetirementProbe` enumerates the main orchestration and
candidate worktrees without hardcoded workstation paths. Candidate fails on any
missing, untracked, modified, or divergent manifest path. Main staging fails when
the 2026-07-25 foundation is missing/divergent, or when a present 2026-07-29 or
2026-07-30 staging file is untracked/modified/divergent; absence of either
staging-only file is the valid retired state. Foreign worktrees are reported but
not changed or treated as authorities.

The candidate implementation remains blocked until review PASS, metadata-only
Lead acceptance, exact three-record transfer, staging-copy retirement/resolution,
and both guards pass.

## Compatibility and rollback

- No persisted state or stored-schema migration exists.
- Existing complete under-budget result fields remain unchanged.
- `result_projection` and pin-status `failure` are additive wire fields.
- Oversized, canceled, unreadable, or incomplete calls may newly return explicit
  unknown/resource-limited results instead of unbounded work or false
  completeness.
- Existing `Reason` values remain the compatibility layer while stable failure
  IDs add causal detail.
- Decision transfer adds no runtime or wire migration. The accepted candidate
  ancestry gains the exact accepted foundation, separate accepted remote-query
  record, and accepted bounded-result record as one coherent manifest.
- Binary rollback requires no data rollback, but it reopens boundedness,
  redaction, exactness, and causal-collapse defects and is not a safe production
  remedy.
- Before source implementation, transfer rollback is removal of the unadmitted
  decision-only predecessor from the candidate branch plus restoration of the
  main staging files from their exact reviewed blobs. After source work depends
  on the accepted manifest, reverting only one decision record is prohibited;
  rollback moves the three-record relation and dependent source together.
- Once published, additive field names and initial stable failure identifiers
  remain reserved until a superseding accepted decision defines migration.

## Enforcement probes

- `TestAllRegisteredToolsAreProjectable` covers every handler and rejects a
  second/unconstrained serializer.
- `TestPublicProjectionExactEncodedBoundary` covers exact N-1/N/N+1 indented
  JSON and minimal projection.
- `TestPublicProjectionLegacyCompatibility` removes only additive fields and
  compares complete-case results.
- Bounded-I/O counting readers prove request size, limit-plus-one retention,
  exact close, cancellation, no tail/reopen, and no arbitrary directory prefix.
- `TestRemoteFailureProjectionMatrix` and whole-result secret scans cover normal
  and minimal results.
- `TestProjectionOrthogonalCompletenessMatrix` covers nine axes by seven states.
- `TestDiagnosticsDroppedExactnessMatrix` covers all 2,401 four-axis
  combinations, single-axis mutations, and reachable cross-products, including
  `log_bytes=limited + diagnostics=settled_complete => false`.
- Import-graph guards prevent tool/server imports in the leaves and process
  control in pin status.
- `TestVcpkgPublicContractDecisionReference` enumerates the exact canonical
  paths above and asserts the three manifest rows independently: accepted/current
  2026-07-25 foundation, accepted/current/unsuperseded 2026-07-29 capability,
  and accepted/current 2026-07-30 contract with reviewed semantic
  body. It validates exact hashes/blobs, constants, vocabularies, exactness
  algebra, dependency owners, and duplicate absence.
- `PR591DecisionStagingRetirementProbe` proves candidate/main-staging blob parity,
  clean tracked paths, and one admitted candidate history without mutating any
  worktree.

## Supersession

Any new universal result field, omission reason, encoded-byte ceiling,
directory-prefix policy, diagnostic exactness algebra, public failure
causal-core rule, or dependency-direction exception requires a new accepted
decision that names this record as superseded or partially superseded. Editing
these semantics silently inside one tool is prohibited.

## Acceptance state

The operative state is the frontmatter `status`. No implementation is authorized
until renewed architecture-reviewer `PASS`, metadata-only Lead acceptance, exact
three-record transfer into the candidate branch, staging-copy
retirement/resolution, and both decision guards are recorded. This Markdown body
does not change during promotion.

## Alternatives rejected

1. Per-tool serializers or reflection-based byte truncation: duplicate
   ownership, invalid JSON risk, and causal loss.
2. Whole-directory reads followed by sorting, or paged arbitrary top-N: the
   former defeats bounded ingress; the latter depends on enumeration order.
3. Diagnostic-axis-only exactness: permits false exact counts when an upstream
   directory, log selection, or byte source hides diagnostics.
4. Publishing redacted raw stderr or a generic error string: token redaction is
   not total and generic text collapses causal identity.
5. Keeping independently edited accepted copies in two worktrees: creates
   split-brain governance; one repo-relative path and one accepted blob are the
   authority.
6. Transferring only the new 2026-07-30 blob: leaves the candidate's
   2026-07-25 foundation in a conflicting proposed state and the exact
   2026-07-29 relationship path absent. The complete three-record manifest must
   move before implementation.

## Terms and Abbreviations

- Causal core: status, reason, failure identifier, ordered secondary typed
  causes, safe exit code, and fixed sanitized detail retained for a failure.
- Exactness: proof that a dropped count is the complete count, not a lower
  bound.
- JSON: JavaScript Object Notation.
- MCP: Model Context Protocol.
- Git blob: content-addressed Git object identity for exact file bytes,
  independent of commit or worktree path.
- Limit-plus-one: reading at most one item or byte beyond a configured limit
  solely to prove overflow.
- Semantic body: exact decision bytes from the first Markdown heading through
  end of file; acceptance metadata is excluded.
