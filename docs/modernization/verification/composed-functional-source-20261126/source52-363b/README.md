# Exact functional Source52

## Contents

1. [Identity and scope](#1-identity-and-scope)
2. [Exact change](#2-exact-change)
3. [Reconstruction prerequisites](#3-reconstruction-prerequisites)
4. [Verification and limits](#4-verification-and-limits)
5. [Execution evidence](#5-execution-evidence)
6. [Sources](#6-sources)

## 1. Identity and scope

This capsule preserves the exact successor to Source6d used by the [Source52 A/B attempt][3]. It contains the original raw commit, exact one-file text patch and native export observations, plus a shared complete-tree reconstruction record. The historical incremental bundle is preserved locally and is not an input to this text capsule. It is a source reproduction artifact; it does not establish functional acceptance.

Table 1. Source chain.

| Layer | Commit | Tree |
| --- | --- | --- |
| Original composed Source | `e66f5c6fab7da2a2eff16ff1f0891991ca1a839c` | `4df69e99dca3f808b26e6a5bf7721bdd0ba3a91c` |
| Exact parent Source6d | `6d4fa7b8f9b86079020af125b030a26350cb5bb0` | `277dad42f9e9080d66955fa7469fbc68bc8618e4` |
| Source52 | `52a2d9ef57b7c372b9fa149adf062ad7e503b22d` | `51258ebd9ad3a654418b6ecb9163947734ef0314` |

The complete original composition is described in [the original capsule][1], including components beyond the named PRs. The [parent Source6d capsule][2] adds its exact preceding commits. This capsule adds one successor commit and preserves both predecessors. Later public PR movement does not alter this Source chain.

## 2. Exact change

Source52 adds the original lost-child event-loop cleanup join. The helper cancels and joins the original loop before its dependent event log and temporary fixture cleanup; ten existing call sites use that helper. Test assertions and deadlines are preserved. The public change mapping is recorded [separately][4].

Table 2. Exact single-file mapping.

| Path | Parent blob | Source52 blob |
| --- | --- | --- |
| `internal/cli/supervise_lostchild_f6_f2_test.go` | `607e3dd5ce78970f5af027200718d20c350b37a8` | `8d6414f0e64d6dd02fbacbca357a6edd692aa359` |

The Source52 working-file SHA-256 is `8bd40a8b1d41becc6d21192044883f994516e666951d781a61cc1eda71e65be4`. Review the unchanged [full-index text patch](source6d-to-source52.patch). The proposed failure-only stack diagnostic overlay is not part of this immutable Source commit.

Git object identifiers use the repository's SHA-1 object format. SHA-256 separately binds exported bytes; these are different hash domains. Full artifact pins are in [source.json](source.json).

## 3. Reconstruction prerequisites

Follow the [shared text reconstruction instructions](../text-source-reconstruction-363b/README.md): obtain exact public e66 through [its capsule][1], construct Source6d using its nine-file patch, then apply [source6d-to-source52.patch](source6d-to-source52.patch) to that verified index. The final full tree must be `51258ebd9ad3a654418b6ecb9163947734ef0314`. No private target commit is used as a read-tree input.

The shared [complete e66 inventory](../text-source-reconstruction-363b/e66-tree-files.tsv), the parent capsule's nine exact replacements and Table 2 describe all 3867 target entries. The raw [commit-52.raw](commit-52.raw) retains its exact Git identity, canonical authorship metadata, parent and tree. The optional commit-object recipe preserves the three original raw commit headers; it does not invent missing older history.

The original 955-byte bundle is retained outside this replacement publication. Its historical verification/head text remains here as evidence of the earlier export; no binary bundle or encoded substitute is supplied as a reconstruction input.

## 4. Verification and limits

The later [native text reconstruction](../text-source-reconstruction-363b/evidence.json) completed with 14 successful original commands and 28 original readers closed. It constructed the complete e66→6d→52 index chain, with 3867 entries and 274 directory objects per snapshot. Native write-tree results and independent complete-tree encoders agreed with Table 1 and the exact 9+1 replacement maps. Receipt SHA-256: `f059068dcfefe637e8048bfa6f8bc7b77157c8818b2a038a74cb38d4334a90fd`.

The earlier native export completed with ten successful original commands and twenty original readers closed. The retained parent reaped its original controller, closed its captures and returned exit 0. The parent elapsed time reported by root was 1.68955 seconds. Native export receipt SHA-256: `06c2f3047b2255b2c57fdf1a0339869a045320174e88b9cfe32bb225e94bc078`.

The original native `git bundle verify` and `list-heads` outputs are retained as [verification](bundle-verification.txt) and [head](bundle-heads.txt) data. The prerequisite is precisely Source6d. After transfer, all eight exported files matched their original byte counts and SHA-256 hashes. Recomputing the Git commit object ID from the exact 280 raw bytes reproduced Source52; the recorded parent/tree matched Table 1. Recomputing the two known file blob IDs matched Table 2.

The earlier export checks alone did not constitute full Source reconstruction. The later text run adds complete Git index/tree identity and exact delta application. It did not create a full checkout, reread all unchanged blob contents or repeat the public cold-bootstrap route. The inherited public e66 graph, native text reconstruction and original export retain their distinct scopes; no new runtime qualification follows from them.

Source commit/clean-readback receipt SHA-256 is `7310b496e3d248c41f2035ff0c4aa942c142330f4131fade9b4ba235a422c9a3`. This source identity receipt and the export receipt are distinct from the runtime-test receipt.

## 5. Execution evidence

The [Source52 execution report][3] records:

- `go build ./...`: PASS.
- `go vet ./...`: PASS.
- Focused default controller tests: 47 top-level passes, 49 passes including subtests and one failure; original execution/resources CLOSED.
- Selected orphan-reap cases and stale-generation disown case: PASS.
- `TestSupervisorController_ParoleQueuedStop_PostSelfHonorsStop`: FAIL, observed spawning instead of exiting.
- Tagged controller and full-suite qualification: NOT_RUN.
- Exact runtime blocking instruction: UNDETERMINED.

The capsule does not turn that focused FAIL/CLOSED into PASS. The earlier Source6d result and the older held negative-control UNKNOWN remain unchanged. No installed lifecycle or release readiness is established.

## 6. Sources

1. [Original complete composition and reconstruction][1].
2. [Exact parent Source6d capsule][2].
3. [Source52 A/B execution evidence and remaining work][3].
4. [Public lost-child cleanup change mapping][4].
5. [Machine-readable source/artifact pins](source.json).
6. [Exact raw commit](commit-52.raw), historical [bundle head](bundle-heads.txt) and [native bundle verification](bundle-verification.txt).
7. [Shared text reconstruction](../text-source-reconstruction-363b/README.md), [complete-tree receiving](../text-source-reconstruction-363b/evidence.json) and [full base inventory](../text-source-reconstruction-363b/e66-tree-files.tsv).

[1]: ../README.md
[2]: ../source6d-363b/README.md
[3]: ../../ab-functional-source52-20261126/README.md
[4]: ../../lostchild-loop-cleanup-20261126/README.md
