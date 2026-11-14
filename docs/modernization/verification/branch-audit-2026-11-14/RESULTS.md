# Remote-branch audit and safe convergence

**Repository:** applicate2628/mcp-local-hub. **Snapshot:** 2026-11-14.
**Accepted master:** `17bc78fc602efa204a74596409c939a3bf1ddc42`.
**Delivery:** existing plan PR #623, not another competing roadmap.

## Contents

1. [Scope and evidence](#scope-and-evidence)
2. [Duplication and retained branches](#duplication-and-retained-branches)
3. [Convergence decisions](#convergence-decisions)
4. [Relay conflict](#relay-conflict)
5. [Fresh verification](#fresh-verification)
6. [Parallel-session protection](#parallel-session-protection)
7. [Continuation and review](#continuation-and-review)

## Scope and evidence

The initial complete snapshot contains **93 remote branches and19 open PRs**, with637 paginated PR records across states. Full Git history was recovered in the private clone before ancestry comparisons; no shared checkout was changed by fetching.

[Branch records](branches-before.json), [PR metadata](pulls-before.json), [exact refs](refs-before.tsv), [per-PR files/bases](open-pr-analysis-before.json), [pairwise comparisons](overlaps-before.json), [merge predictions](merge-predictions-before.json), and [read-only analyzer](analyze.mjs) are preserved. `BRANCHES.md` provides the human-readable index.

The audit uses exact ancestry, tree identities and before/after blob/mode identities relative to each PR's actual base. Similar names, inherited commits and line counts are not duplicate proof. A clean merge prediction is not test or release readiness. Final publication/readback is recorded separately; a prepared local commit alone does not claim a remote update.

## Duplication and retained branches

**Table 1. Initial classification of all93 heads.**

| Classification | Count | Disposition |
|---|---:|---|
| Default master |1|Do not modify |
| Own open modernization PRs |9|Synchronize/qualify; combine only the cohesive relay pair |
| Protected parallel work |11|Ten open PRs632-641 plus their verification branch; read only |
| Already in master by graph ancestry |12|Historical refs; no deletion |
| Exact head belongs to a merged PR, but not an ancestor of master |49|Historical merged/squashed refs; preserve audit history |
| Closed-PR heads not proved included |10|Retain until useful residue is reviewed |
| No associated PR and inclusion not proved |1|Preserve `feat/mathcad-mcp`, four unique commits |

Among19 open PRs there are **no equal heads, no exact equal change fingerprints, and no overlapping own changed filenames relative to their bases**. This rules out direct duplicate-patch/file fan-out; it is not a proof of semantic non-equivalence of every possible implementation. Eight ancestor pairs are exactly the transitive relations of three intentional stacks:625→631,629→630,632→635→636→637. They are dependencies, not eight duplicated implementations.

The11 uncertain closed/unassociated heads were also checked with `git cherry` against the pinned master; none of their candidate commits was patch-equivalent under that test. [Exact classifications](historical-patch-check.json) remain available. This does not prove that all old proposals are desirable or semantically absent after later rewrites. None was automatically revived, merged or deleted.

## Convergence decisions

All own open PRs623-631 began **53 commits behind** accepted master. Non-rewriting merge commits retain their original histories; no force-push or cherry-picked copies are required.

**Table 2. Review boundaries and qualified actions.**

| PR | Action | Reason |
|---|---|---|
|623|Update the existing plan and audit on accepted master|Single planning source |
|624|Synchronize exact benchmark patch|CMake measurement is independent of process ownership |
|625|Synchronize native owner without changing its source|Keep native lifecycle review separate |
|626|Synchronize baseline/control code|Measurement is not a runtime rewrite |
|627|Synchronize and rerun the reader crate|Filesystem capability is not part of the process-supervisor scope |
|628|Synchronize native observation benchmark|Do not attribute Go repeated-open effects to Rust |
|629 +630|Keep a combined12-file relay fix in629; close630 only after remote containment is verified|One lifecycle fix spans daemon and executable bootstrap |
|631|Follow synchronized625; retain separate FFI review|Foreign allocation/error contracts should not be hidden in a giant native-owner PR |

[Prepared commits](prepared-sync.json) retain their old head and accepted master as parents. `prepare-sync.mjs` verifies that each new-base raw diff has exactly the original paths, modes and before/after blobs. Publication updates only explicitly owned refs after exact-head checks.

FFI follows its parent via `bb8e31648581f603c1a35e65b02eb9c02f1345c2`; evidence is committed as `eb4ca1d40244ba1915638b76c3e30b2a684d9b07`. Before the evidence additions, no byte in `experiments/rust-process-owner` changed during synchronization. The complete rerun is also in631 at `verification/runs/master-convergence-2026-11-14`.

## Relay conflict

The only substantive conflict was `cmd/mcphub/main.go` from630 versus master. Choosing one side wholesale would lose either the immutable canonical-entry build binding or the explicit grant over this executable's inherited stdin.

The resolution starts from exact master blob `10e7aaf4c614bc9f3bad5c1df404ac7471181f1a`: construct the root with `NewRootCmdWithWindowsCanonicalEntryBinding`, bind process-owned relay input to that root, then execute it. [The resolution script](resolve-relay.mjs) checks that reversing only this replacement reproduces the original master bytes. No new policy layer or broad refactor was introduced.

Combined commit **`cd9da98419bb2c5f8c7c0ee621f767ae4bdcc627`** retains both630 head `1326779c` and accepted master as ancestors. Its tree matches the staged tree recorded before [qualification](relay-merged-master/indexed-tree.log). Superseding630 retains its branch, original commits and review history; it is consolidation within an unmerged PR, **not a merge into master**.

Initial PowerShell text capture damaged non-ASCII comments locally. The diff caught this before commit; raw UTF-8 Git output and a round-trip assertion preserved the exact master comments. No damaged source was committed or used for final tests.

## Fresh verification

**Table 3. Actual checks on synchronized source.**

| Area | Executed result |
|---|---|
| Combined relay629 | Daemon relay/line/qualification tests race×3; CLI binding tests race×3; entire main package race×3; selected47 top-level/52 nested/0 skip; product compile and all three affected-package vet checks PASS |
| Native625 + FFI631 | Full experiment:49 Go functions/63 nested/0 skip, both DLL configurations race×3, cgocheck2/vet;14 ABI and53 native-parent checks debug/release, Clippy/fmt PASS |
| CMake624 | Whole cmaketrace/publicresult packages with race and vet PASS |
| Relay baseline626 | Relay/load/line contract selection race×3 PASS; no new benchmark-speed claim |
| Observation628 | Both native-observation tests race×3 and process-package vet PASS |
| Anchored reader627 | Whole crate15 tests,0 ignored; Clippy -Dwarnings and formatting PASS |
| Git integrity | Exact scoped-patch preservation for five independent roots; relay original heads retained; source identities checked |

The first [roots run](roots-verification/steps.json) stopped at reader setup: the process-owner private Cargo cache lacked `sha2`. That failed run is preserved. Thirteen already installed pinned archives were then verified against Cargo.lock and copied into a private D-drive cache, without downloading or modifying the original cache. See [cache receipt](reader-cache-transfer.json) and [successful reader rerun](reader-verification/steps.json). An unavailable `Get-FileHash` stopped the initial hash command before copying; .NET SHA256 performed the actual checks. No package version, toolchain or lockfile changed.

All new writable paths are under `D:\dev\ChatGPT`. No GUI/product deployment, external vendor/model process, whole-Hub acceptance, native Unix execution or performance remeasurement is implied. Historical measurements remain tied to their original source. No workflow/bot was explicitly dispatched.

## Parallel-session protection

PR632-641 and `verify/serena-postmerge-20261114` belong to the active post616 session. Their own changes are separate, and their bases are already current. The Serena stack632→635→636→637 remains deliberately layered; the focused fixture/tray/VTune/recovery work is not folded into a competing integration branch. This audit does not change their refs, bases, states, checks or working trees.

During collection637 and the verification branch advanced. Exact snapshot SHAs were retained and fetched, not overwritten. Final receipts distinguish that legitimate activity from this session's explicit writes.

A separate local checkout `spike/rx02-native-identity-20261114` was also observed at `628890dec7e9e1f52be487fa4533b4bdfedeb71c`, based on625 with uncommitted verification materials. Only Git metadata was inspected. It is treated as **reserved ongoing identity work**: no source/uncommitted evidence/branch was changed or published by this audit, and no duplicate creation/Job/wait implementation was started. Coordination notes are on623 and625. That unpublished work is not counted as accepted qualification.

## Continuation and review

The next work uses converged existing targets, not replacement PRs:

1. Let reserved native-identity work publish and qualify original-handle creation/Job/wait evidence. Extend the existing FFI observation against that accepted capability. Token/SID formatting and actual consumer-policy parity remain explicit gaps; do not synthesize unknown fields.
2. Independently review the combined629 delivery. Focused checks do not authorize merge/install by themselves.
3. Keep624/626/628 measurements separate. Real workloads and equivalent-contract Go/Rust comparison still determine whether more Rust is justified.
4. Continue627 read parity, then bounded write/publish/lock work without duplicating active transaction owners or vendor SDKs.
5. Qualify native Linux/macOS and remaining product/deployment/recovery gates. Passing Windows merges/tests does not prove portability.

The R01-R24 program is not declared complete. This iteration handles branch inventory and convergence, reduces one unnecessary delivery boundary and protects parallel work.

**Table 4. Ten skills read and applied.**

| Skill | Concrete use |
|---|---|
| Using Superpowers | Select current work rather than repeat completed iterations |
| Executing Plans | Exact-head recovery and durable evidence |
| Using Git Worktrees | Isolated documentation/relay/qualification trees |
| Brainstorming | Existing approved plan; reject indiscriminate mega-PR consolidation |
| Architecture Review | Separate native owner, FFI, relay, measurements and live transactions |
| Systematic Debugging | Resolve the actual composition conflict and cache prerequisite |
| Test-Driven Development | Existing behavioral oracles for composition, no new untested runtime mechanism |
| Git Workflow and Versioning | Non-rewriting parents, bounded commits and exact-head publication guards |
| Ponytail Review | Remove one delivery layer, not independent responsibilities |
| Verification Before Completion | Fresh suites and publication readback |

Self-review checked scope, correctness, resource authority, compatibility, text encoding, original-head ancestry and evidence limits. It is not independent lifecycle/security acceptance. No historical remote branch was deleted; no active identity/Serena tree was cleaned or republished. Verified disposable artifacts may move directly to trash after receipts are saved; trash is not emptied. All necessary results and recovery instructions belong to existing PRs rather than the survival of the PC.
