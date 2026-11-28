# Exact functional Source 6d4fa7b8

## Contents

1. [Identity and scope](#identity-and-scope)
2. [Changes and applicability](#changes-and-applicability)
3. [Reconstruction](#reconstruction)
4. [Verification and limits](#verification-and-limits)
5. [Sources](#sources)

## Identity and scope

This text capsule records the exact composed Source used by the historical A/B acceptance attempt. It preserves the original `e66f5c6f` composition, both original raw commit payloads and exact text deltas. It is a source-reproduction artifact; test acceptance is reported separately in [the A/B progress evidence][2].

**Table 1 — Source identities.**

| Layer | Commit | Tree |
| --- | --- | --- |
| Inherited composition | `e66f5c6fab7da2a2eff16ff1f0891991ca1a839c` | `4df69e99dca3f808b26e6a5bf7721bdd0ba3a91c` |
| Liveness, Abort, initial lazy status | `5ed87ff759405efa739229b7307b5c2f9579c8fc` | `75c703052e518b8f286f3aa4f079f630de951441` |
| Status order, private expected row, orphan-loop join | `6d4fa7b8f9b86079020af125b030a26350cb5bb0` | `277dad42f9e9080d66955fa7469fbc68bc8618e4` |

The inherited composition binds #660 at `25508f8bfe5950daca3df1b0faf338d77913640a` and #669 at `ec2248643d875390c41b519b85424a5ca0ce90d1`. Their source provenance and the other P02/donor components are recorded in [the parent capsule][1]. This composition also contains those other components; the two PR numbers alone do not describe its entire Source. Later movement of a public PR head does not change these bindings.

## Changes and applicability

Nine paths differ from the inherited composition, as listed in Table 2. The public #660 update contains the three liveness/setup paths. The public #664 update applies scoped Abort, status and test-harness changes to that PR's own current preimages. Public #664 does not have the composed Source's existing `pid_generation` row field, so the composed expected-map correction is included here without being copied into the public #664 test.

**Table 2 — Exact Git blob mapping.**

| Path | Inherited blob | Source 6d blob |
| --- | --- | --- |
| `internal/api/liveness_task.go` | `dfb417d4f0f51eee221f0f42cd115cb066ddf4db` | `6c852d6eb172b51708f2d5618163b8ebc4138ff2` |
| `internal/api/liveness_task_test.go` | `d1452e90aea536762e0589632f8a86b196e81ccf` | `c9ac7dbd7f3f561cb192177cfa9158c13126a38d` |
| `internal/cli/setup_watchdog_test.go` | `c735b55bf72873ce26ae288ea42ae4476594e227` | `1801bb7925ea115839ac72b1d8d8807ac5e59045` |
| `internal/cli/supervise_status.go` | `81412548e82d81fee31c9902fe5590906cab82a0` | `51400b4337b5f74be9cf87189336006b6329efb7` |
| `internal/cli/supervise_status_process_test.go` | `5d739d3e0b5fcc4ca880b432d5a3c8c5e4a2d18f` | `511d48417f2bfc823dacfb1e7c5c6c8a1ad748fe` |
| `internal/cli/supervise_status_snapshot_test.go` | `31472aa8bc73de0312efb717277ce5983114cf87` | `63657630d1c9a1c5a78fc8c2acad629ea45dad93` |
| `internal/cli/supervisor_controller_orphan_reap_test.go` | `14abb540847502c65473ed2a75446e34a73d16d3` | `fd6a9e2e0c4b1b80ebc60143408559ae2208fef0` |
| `internal/cli/supervisor_launch_windows.go` | `b268dd4f3b557fbeeae076503cfec5a9d71aead3` | `feed8526b12e65b11faf715a5c837e712902c4f3` |
| `internal/cli/supervisor_launch_windows_test.go` | `367cc534b73f43ecaed1c3d634754016e56ad2c7` | `1857ca9fb83c6ad1353bd3c67ab18514342c30a9` |

The full SHA-256 working-file pins, module pins, raw Git commit bytes and export receipt binding are in [source.json](source.json). Git object identifiers in Tables 1–2 use the repository's SHA-1 object format; SHA-256 separately binds artifact bytes. These are different hash domains.

## Reconstruction

Use the [shared text reconstruction instructions](../text-source-reconstruction-363b/README.md). They start with the exact public e66 object graph from [the parent capsule][1], select a fresh index and apply the unchanged [nine-file full-index text patch](e66-to-source6d.patch). The resulting full tree must be `277dad42f9e9080d66955fa7469fbc68bc8618e4`. The patch preserves its original line-ending bytes; no inferred normalization is permitted.

The shared [complete base inventory](../text-source-reconstruction-363b/e66-tree-files.tsv), followed by Table 2, specifies every mode/blob/path entry in the target. Exact [commit-5ed.raw](commit-5ed.raw) and [commit-6d.raw](commit-6d.raw) retain their original timestamps, authorship, trees and parent fields.

The optional intermediate 5ed recipe additionally uses [status5ed-to6d.patch](status5ed-to6d.patch) in reverse and restores the two original e66 test preimages. The 524-byte patch and intermediate status blob were checked offline in both directions; the full 5ed tree model was computed from the received 6d index. That is not a claim that a separate native 5ed blob restoration was run.

The original incremental bundle is preserved outside this replacement publication. Its verification/head text below records the historical export. It is no longer an active capsule input, and no bundle payload is repackaged or encoded here.

## Verification and limits

The later [native text reconstruction](../text-source-reconstruction-363b/evidence.json) completed with 14 successful original commands and 28 original readers closed. Complete e66 and 6d indexes each contained 3867 entries and 274 directory objects. Native write-tree results, independent native tree encoding and two independent local receiving calculations agreed with Table 1. Exact receipt SHA-256: `f059068dcfefe637e8048bfa6f8bc7b77157c8818b2a038a74cb38d4334a90fd`. This establishes full Git tree identity and exact text delta application, without a new cold bootstrap or reread of every unchanged blob.

The earlier native export completed with 11 successful original commands and 22 original readers closed. The retained parent reaped its original controller and closed its capture handles. `git bundle verify` accepted the exact prerequisite, and `git bundle list-heads` returned the Source commit in Table 1. The export receipt SHA-256 is `72936aae5ebac8ba7ef5f6d6cbc4a6235956202a23d464ed2aa3f6b75df91864`.

After transfer, every exported file's SHA-256 and byte count was checked. Recomputing Git object IDs from the exact `commit-5ed.raw` and `commit-6d.raw` byte strings reproduced both commit IDs. The nine blob mappings were read from the native Git object store. Native formatting/commit receipt SHA-256: `f75c328af2d5d072fc538619d41593d63a8b62e8480ba2660481ae8f1eaa99ea`.

This evidence establishes source identity and export closure. It does not establish complete functional acceptance, full-suite PASS, native lifecycle qualification, installed lifecycle behavior, Unix/ARM64 support, or release readiness. Earlier V1/FAIL/UNKNOWN results retain their original meanings. In particular, the separate negative-control batch remains FAIL/HOLD with physical UNKNOWN; this independent capsule does not settle or replay that held run.

## Sources

1. [Original exact composition and reconstruction][1].
2. [A/B changes, execution evidence and remaining work][2].
3. [Exact machine-readable Source and artifact manifest](source.json).
4. Historical [native bundle verification output](bundle-verification.txt) and [bundle head](bundle-heads.txt); the original binary is not included.
5. [Text reconstruction and scope](../text-source-reconstruction-363b/README.md), [receiving evidence](../text-source-reconstruction-363b/evidence.json) and [complete base inventory](../text-source-reconstruction-363b/e66-tree-files.tsv).

[1]: ../README.md
[2]: ../../ab-functional-source6d-20261126/README.md
