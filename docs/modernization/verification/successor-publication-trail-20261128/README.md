# Closed source-edit, cache and publication trail — 2026-11-28

This capsule preserves ten source-edit/publication operations and one cache-preparation run: **74 closed native commands and all 148 stdout/stderr captures**, including the failed first edit. It also preserves the root's independent byte comparison, actual DATA replay of the previously published full failure, and two verified moves of disposable copies to trash.

**These records grant no full-suite PASS.** The original Source727 full run remains **FAIL/CLOSED**, and the older full-v8 remains **UNKNOWN**. Historical 717 results retain their existing qualification across revisions. No result here transfers acceptance to final Source59f.

## Contents

1. [Original operations](#1-original-operations)
2. [Preserved failure and Source identities](#2-preserved-failure-and-source-identities)
3. [Closed cache preparation](#3-closed-cache-preparation)
4. [Publication and DATA verification](#4-publication-and-data-verification)
5. [Exact preservation and reconstruction](#5-exact-preservation-and-reconstruction)
6. [Disposition and remaining work](#6-disposition-and-remaining-work)
7. [Sources](#7-sources)

## 1. Original operations

**Table 1. Original classifications and command counts.** Durations are recorded controller/outer times, or the original parent time for warm-cache. They are not test-performance comparisons. [1]

| Operation | Result | Physical state | Commands | Seconds | Meaning |
|---|---|---|---:|---:|---|
| stage-v11 | FAIL | CLOSED | 4 | 0.282401 | Patch applied; controller rejected an undeclared observed-input change. |
| stage-v12 | PASS | CLOSED | 9 | 1.235712 | Checked and staged seven reviewed test-file afterimages. |
| commit-v7 | PASS | CLOSED | 6 | 0.368677 | Created Source04e from recovered Source33. |
| warm-cache | PASS | CLOSED | 4 | 273.338450 | Copied a closed PASS donor and compiled three targets on Source04e. |
| stage-v13 | PASS | CLOSED | 11 | 3.373235 | Applied two migration-fixture Source requirement literals. |
| commit-v8 | PASS | CLOSED | 6 | 0.741892 | Created Source74e. |
| stage-v14 | PASS | CLOSED | 7 | 4.601340 | Staged full-failure/test-fix evidence in the existing documentation checkout. |
| commit-v9 | PASS | CLOSED | 6 | 0.818569 | Created documentation commit 05fbffa3; Git reported 52 changed files. |
| push-v7 | PASS | CLOSED | 4 | 29.024577 | Published that commit through the existing native gate. |
| stage-v15 | PASS | CLOSED | 11 | 1.239046 | Applied the original breakaway-wait assertion correction. |
| commit-v10 | PASS | CLOSED | 6 | 0.367365 | Created final Source59f. |

The receipts preserve native results, original reader joins, pipe/capture closure, timing, input observations and capture hashes. Controller and outer receipts remain separate. Complete identities and primary SHA-256 references are in [event-index.json](event-index.json); every command capture is in [capture-index.json](capture-index.json). [1][2]

## 2. Preserved failure and Source identities

Stage-v11 omitted the existing mutates_inputs declaration on apply-minimal-successor-tests. All four native commands exited 0 and closed. The fourth, git apply, changed the reviewed test files. The controller then recorded the exact error:

~~~text
RuntimeError: test changed observed source inputs
~~~

The controller and original outer process exited 1. Only the first two declared steps were received; later stage-v11 steps are not counted as successful. Stage-v12 began from the actual, independently reviewed afterimages and did not apply the patch again. Later edit plans explicitly declared their input mutation. The original FAIL is retained without reclassification. [1]

The read-only stage-v12 gofmt listing exited 0 and printed internal/cli/supervisor_launch_successor_runtime_windows_test.go. That exact output is preserved. It is not an assertion that every file was formatting-clean; the existing test-fix capsule records the reviewed scope and retained pre-existing spacing. [2][7]

**Table 2. Identities from complete original committed-identity captures.** [2]

| Role | Commit | Tree | Parent |
|---|---|---|---|
| Source04e | 04e5b4eadd6407cbff4e04ba453546d278314bd7 | f68f7584dba7744e7607b07f615758f3ff633f54 | 33eaf8af8fc46cb231f48c70f4775922393245b1 |
| Source74e | 74e7563db244ede5984834233ce8d59072e92c0a | bcf1bd3539a8ba5ab54cff123adbb4bc071276d3 | 04e5b4eadd6407cbff4e04ba453546d278314bd7 |
| Source59f | 59f618ebfbf6d9f52c6994d68586e3c2ebc202a8 | 03e8488a44e67f1ea7e8dc2ef0ece53fee5ae8e1 | 74e7563db244ede5984834233ce8d59072e92c0a |
| Documentation | 05fbffa30f99914c8174966525a70bc9984e7038 | 89f3c5715356ff424d8538c1409cb91e58b1977c | 01b4e915af6439e4c5e7c5c880a6d4885a2dbd3a |

Source and documentation have different branches and roles. A documentation push does not publish or qualify the Source branch by implication. None of the code-edit operations in Table 1 executes the mandatory full test commands. [1][6][7][8]

## 3. Closed cache preparation

The warm producer binds Source04e and its 3,369 tracked files. It copied the original closed PASS donor's Go build/module caches into a fresh private output, then compiled three explicit targets. No cache from the failed Source727 full run was used. Its full plan, historical Source map, parent, execution, outer receipt, copy receipt and root receiving are preserved. [3]

**Table 3. Original warm command durations and scope.** [3]

| Command | Exit | Seconds | Scope |
|---|---:|---:|---|
| Copy closed PASS donor caches | 0 | 150.290637 | Fresh byte-checked Go cache copies |
| Provider working-directory helper, 386 | 0 | 29.012311 | Compilation only |
| Provider working-directory helper, amd64 | 0 | 9.870871 | Compilation only |
| CLI Product with test_state_path_env | 0 | 69.328414 | Compilation only |

Both helpers use GOOS=windows, the selected architecture, CGO_ENABLED=0, the internal/api working directory, and the source's provider-cwd-helper target with the Windows GUI linker option. The CLI uses the repository root, the test_state_path_env tag and the declared CGO/build environment. The exact wrapper and plan contain the literal argv. [3]

Root receiving records eight before/after maps, 3,388 pinned inputs, eight command captures and two empty parent captures checked. The three output hashes and sizes are classified COMPILED_CACHE_INPUT_ONLY. It explicitly records no tests and no full acceptance PASS. The CLI stderr includes a dependency-download notice; there is no measured cache-hit ratio or promised improvement to the five-minute package budget. [2][3]

The bounded Serena production-code review is preserved in [serena-cache-seam-review.md](serena-cache-seam-review.md). It distinguishes reuse of an admissible Ready package in its original namespace from a general cache/seed import. Go cache warming does not prove that real Serena preparation fits the unchanged test deadline. [9]

## 4. Publication and DATA verification

The complete push-v7 output preserves the native gate's V3 and V4 clean results, coverage digests and normal push output for documentation commit 05fbffa3. The original push command/readers, controller and outer process closed with exit 0. No publication hook, policy or approval token was edited by this capsule author. [1][2]

The root also ran the previously published full-failure capsule's DATA verifier. Its preserved result records 20 original files checked and 10,461 frame-aware terminal records matched. FAIL/CLOSED remains unchanged. No historical command was rerun, and no cache qualification was inherited. The earlier 283-command / 566-capture receiving remains explicitly historical. [4]

The root independently compared **all 209 initial trail originals against their actual local bytes: 209 MATCH**. This includes the 15 metadata encodings that originally awaited an independent byte check. Their historical basis labels remain in the original specification; the later MATCH result resolves that pending comparison. The original specification, historical verification script and complete role-only result are preserved alongside that data. [5]

A later role-only report-backup disposition receipt is also included. The final capsule therefore reconstructs **213 originals: 65 metadata/map/plan/script artifacts and 148 command captures**. The actual initial byte-comparison proof covers its stated 209-file scope; it does not silently expand its original inventory.

## 5. Exact preservation and reconstruction

All 148 command captures are losslessly mapped to 43 distinct public payloads. Ninety-six captures are empty; repeated identical nonempty captures share payloads as well. Every original path role, byte count and SHA-256 remains separate in the index and reconstruction bindings. [2]

The push stdout has four carriage-return bytes. Its complete contents were also preserved as an escaped JSON string in the original root audit. That string reconstructs the original 1,928 bytes and recorded SHA-256 exactly; normalized tool output is not treated as the original. The proof records the recovery basis and offsets. [10]

Native numeric identifiers are retained. Host paths use transparent role placeholders, with no encoded value, embedded default or hidden payload. Supply exactly the nine role keys described by source-bindings.json in an operator-owned JSON file.

~~~text
python reconstruct_sources.py --roles-json operator-owned-roles.json --output reconstructed-trail
python verify_closed_trail.py --reconstructed reconstructed-trail --output closed-trail-verification.json
~~~

The renderer checks the manifest pin, every public payload, substitution counts and all reconstructed original hashes before creating a new output. It reuses the existing renderer implementation with only its binding hash changed. The verifier checks original classifications, plans, native/IO closure, all captures, warm maps, the original byte-comparison result and disposition receipts.

These programs process preserved evidence as DATA. They do not import archived controller scripts, run Git or Go, contact a network, read live Source/cache payloads, reissue a capability or replay a historical operation. Preserved historical scripts describe the original operation; they are not automatic execution instructions for a new run.

## 6. Disposition and remaining work

Only two verified duplicate moves are recorded here, totaling **22 files**:

- 21 files in a disposable reconstruction directory: 20 previously published original copies totaling 14,916,637 bytes, plus a repeated reconstruction receipt. Expected bytes/hashes were rechecked at the trash destination; the old directory was absent.
- One 258,462-byte backup of the report already published at 01b4e915. Its exact SHA-256 and size were checked at the trash destination; the old backup was absent. [11]

Both role-only receipts state that no unique data were removed and no permanent deletion occurred. The primary run, Source, caches and UNKNOWN scopes remain preserved.

The complete old full-v6 run remains KEEP. This capsule does **not** archive or classify all 566 old-full captures. Previously published complete raw streams and result/coverage data do not by themselves account for every remaining distinct raw stream. The next bounded DATA audit must retain distinct valuable originals losslessly and document exact duplicates before cleanup.

[resource-disposition.json](resource-disposition.json) records current retention obligations without authorizing further moves. New Product/helper preparation and its continuations are outside the fixed Table 1 trail; their original FAIL, PASS, CLOSED or UNKNOWN records must be preserved separately.

The author checked complete source text, recorded hashes and in-memory literal round trips. The root performed the original-byte comparison described above. The public renderer/verifier are prepared for root review; authoring them is not a claim they were executed. The manifest covers all public files except itself, and .gitattributes preserves byte-level line endings. No Source, Git index/ref, hook, existing report or PR body was changed by this capsule author.

## 7. Sources

1. [Original event index](event-index.json) and exact receipt/plan/root/outer bindings in [source-bindings.json](source-bindings.json).
2. [Complete capture index](capture-index.json), with shared exact or transparent-template payloads.
3. Original-role warm-cache artifacts and preparation scripts in [source-bindings.json](source-bindings.json).
4. [Actual full-failure DATA replay](exact/preservation/full-v6-root-data-verification.json).
5. [Actual original-byte comparison](exact/preservation/root-original-byte-verification.json), its specification and historical verifier in the bindings.
6. [Previously published complete full-v6 failure](../original-full-v6-failure-20261128/README.md).
7. [Seven reviewed Source33 test fixes](../source33-successor-test-fixes-20261128/README.md).
8. [Migration Source requirement literals](../successor14-manifest-pin-20261128/README.md).
9. [Bounded Serena cache review](serena-cache-seam-review.md).
10. [Reconstruction proof](data-reconstruction-proof.json) and [disclosure review](disclosure-review.json).
11. [Reconstructed-copy disposition](exact/preservation/root-reconstruction-trash-receiving.json) and [report-backup disposition](exact/preservation/report-backup-trash-receiving.json).
