# Closed stages 21–28 and publication evidence

This capsule preserves **197 exact original artifacts (13,267,093 bytes)** from ten physically closed native events and the bounded DATA checks around them. The events contain **44 original commands, 88 command captures and 20 controller captures**. Repeated contents share 112 public payload files. The original outcomes remain **eight PASS/CLOSED events and two FAIL/CLOSED events**.

The successful focused observations comprise three CLI identity tests in both modes at stage 24 and nine selected CLI tests in both modes at stage 28. These are ordinary `go test` observations. They do not establish either full required command, all migration and canonical-environment checks, or closure of the retained UNKNOWN full59f/CLI owner.

## Contents

1. [Event results and their limits](#1-event-results-and-their-limits)
2. [What is preserved](#2-what-is-preserved)
3. [Exact byte reconstruction](#3-exact-byte-reconstruction)
4. [Root DATA checks and incidental errors](#4-root-data-checks-and-incidental-errors)
5. [Source candidates and provenance](#5-source-candidates-and-provenance)
6. [Publication finding and disclosure](#6-publication-finding-and-disclosure)
7. [Resource disposition and boundaries](#7-resource-disposition-and-boundaries)
8. [Primary references](#8-primary-references)
9. [Publication template revision](#9-publication-template-revision-after-the-original-range-finding)

## 1. Event results and their limits

**Table 1. Original native results.** Complete original receipt hashes, command names, native and input/output closure observations and capture mappings are indexed in [event-index.json](event-index.json). The original files are addressed by their logical role in [source-bindings.json](source-bindings.json).

| Event | Original result | Observed scope |
|---|---|---|
| Stage 21 | FAIL/CLOSED | The three-identity-test attempt stopped during setup because the ordinary runner selected an earlier default module cache while module lookup was disabled. The selected tests were NOT_RUN. |
| Stage 22 | PASS/CLOSED | Staging and checks for the earlier 615-file evidence checkpoint and report. This is publication preparation, not test qualification. |
| Commit 11 | PASS/CLOSED | Created local commit `264a0601b7d027652c12ec88ae4debd6c3bfb69f`, tree `f7fbccfb8cfc5d1c33b986591675d07847af929c`. Successful commit creation does not establish successful publication. |
| Push 8 | FAIL/CLOSED | The original native push exited 1 with `PGG-SCAN-FINDING` at `range-coverage`. This failed attempt remains a failed attempt. |
| Stage 23 | PASS/CLOSED | Prepared the separate baseline control. No full test result is implied. |
| Stage 24 | PASS/CLOSED | Copied caches from the received closed Product-only v2 preparation and observed three CLI identity roots in each of default and tagged modes: six PASS observations. The root DATA audit checked all 3,369 Source59f files. |
| Stage 25 | PASS/CLOSED | Ran the supported canonical publication diagnostic. It identified nine exact occurrences in three files: four machine-path-prefix findings and five password-assignment findings. No publication policy or authority was changed. |
| Stage 26 | PASS/CLOSED | Created the isolated worktree for the reviewed test fixtures. |
| Stage 27 | PASS/CLOSED | Compiled the timing-only namespace owner from the two preserved source inputs. Compilation alone does not establish execution or a namespace timing result. |
| Stage 28 | PASS/CLOSED | Applied the two shared-image test afterimages and the prespawn barrier afterimage to an isolated Source59f baseline. Nine selected roots passed in each mode: 18 PASS observations. The root checked the 3,369-file source set against the baseline with only those exact three test changes. Production edits: zero. |

The baseline commit for stages 24 and 28 is `59f618ebfbf6d9f52c6994d68586e3c2ebc202a8`. Stage 28 has three test afterimages, recorded with full hashes in [evidence.json](evidence.json). It is not byte-identical to the baseline.

For stage 24, Go reported package times of 7.784 seconds in default mode and 17.744 seconds in tagged mode. The corresponding native command intervals were approximately 193.593 and 123.303 seconds; these intervals include work outside the package's reported test duration. Stage 28 reported package times of 8.548 and 5.476 seconds, with native command intervals of approximately 72.869 and 50.015 seconds. The exact individual observations are retained in the original captures and the derived evidence projection. Neither timing pair is a full-suite measurement or a promise of future duration.

## 2. What is preserved

**Table 2. Exact original inventory.**

| Category | Original files |
|---|---:|
| Ten primary receipts, controller roots, original outer-process observations, configurations and launch sources | 50 |
| Additional independent root DATA audits | 8 |
| Original command stdout/stderr captures | 88 |
| Controller stdout/stderr captures, all empty in these events | 20 |
| Source candidates, preparation/receiving scripts, root DATA results and prior publication header/transfer map | 31 |
| **Total** | **197** |

Before capture reads, each event's original control records were checked for known native exit, original process reaping, zero active owned processes, closed native handles, joined original readers, completed and closed pipes/capture streams, matching input/output observations, and completed receiving. The outer records retain the same original process start object, its wait and its disposal. An owner or reader object retained for evidence is not treated as evidence of an open native resource.

The command captures match the exact byte counts and SHA-256 hashes recorded by the original received receipts. Empty captures remain explicit entries. Native process and handle numbers are preserved where present; they are useful correlation data.

The package stores 151 exact original entries and 46 transparent-template entries. Multiple entries can refer to one identical payload. This deduplication does not merge command identities or discard empty results. [source-bindings.json](source-bindings.json) maps every original role to its payload, original byte count/hash, published byte count/hash and exact substitutions.

The prior unpublished checkpoint header and transfer receipt are preserved under the logical roles `publication-v1/checkpoint-report-header-v1.md` and `publication-v1/transfer-receipt.json`. They describe the historical commit11/push8 attempt. They are not installed over the current report by this capsule.

## 3. Exact byte reconstruction

The bindings SHA-256 is:

```text
597b09339d0061817947304d844ad95c7f0cb9b17a2d22f39d9cae3c7bdcbdb3
```

The renderer SHA-256 is:

```text
034723e528b4219f2d255b5aaa8ec6f2e5103fff68701bc8fbfa8afb4c4e01ca
```

The renderer reuses the previously reviewed preservation mechanism with the binding pin changed. It reads artifacts as DATA; it does not import or execute preserved source, launch tests, invoke Git or obtain authority from a historical receipt.

Supply a private JSON object containing exactly the twelve role keys listed in `source-bindings.json`: nine original environment directory values and three source-defined fixture values. Supply each value explicitly; the three fixture values can be obtained from the pinned public source using the exact lexical ranges in `fixture-template-revision.json`. No original value, encoded value or default is embedded in the public capsule.

```text
python reconstruct_sources.py --roles-json operator-roles.json --output reconstructed
```

The destination must be absent and its parent must already exist. The renderer validates every public payload, substitution count, reconstructed original size and original SHA-256 before creating any destination. It then creates files exclusively, reads them back, and emits `reconstruction-receipt.json`. Existing destinations are not overwritten.

The three transparent operations are literal replacement, doubled backslashes for JSON, and quadrupled backslashes for nested JSON. Every marker's role and operation are explicit. There are 44,531 substitutions across the 46 template entries. Newline and carriage-return bytes are part of the original hash; `.gitattributes` disables text conversion for this capsule.

During preparation, all 197 entries were reconstructed in memory and compared with the admitted original bytes. That DATA check is recorded in [author-data-checks.json](author-data-checks.json). It is distinct from the root's subsequent independent native-byte and renderer verification of the saved capsule. No original source or runtime artifact was executed by the preservation author.

## 4. Root DATA checks and incidental errors

The following exact receiving artifacts and their source scripts are included:

- `root-data/source59f-closed-api-diagnostics-root-data-v1.json`: root verification of the companion 96-file capsule and 136 reconstructed originals. The original API result remains **FAIL/CLOSED_PRECHILD**; no suite was started.
- `root-data/root-fixture-v2-verification.json`: root verification of the companion 350-file fixture-template v2 capsule, reconstruction of 333 originals and verification of all 566 historical capture paths. The historical full-v6 result remains **FAIL/CLOSED**.
- `root-data/root-fixture-v2-all566.json`: the complete-capture verification result, SHA-256 `2297a5b987c60244bd1c2c82f7d4253c1301637ee7e4359c8e24a06e75c78a55`.
- `root-data/root-public-fixture-readback-v1.json`: all five referenced public source blobs were fetched independently and matched in full bytes, SHA-256 and Git blob SHA-1. The result contains no fixture values.

Two incidental DATA wrapper errors are retained without converting them into native test failures. The exact focused receiving v2 PowerShell source is preserved; root reported a replacement-induced parser error before it produced DATA output. A separate primary error-output file was not supplied, so this narrow statement is attributed rather than presented as a reconstructed error receipt. Its v3 successor and the successful stage24 audit are separately preserved.

The first fixture-v2 root wrapper completed the 333-original and 566-capture checks but then encountered a case-insensitive PowerShell variable collision in its summary construction. The exact v1 and v2 scripts remain available, and the successful v2 receiving JSON explicitly records the earlier summary error. Historical native outcomes were not changed by either DATA correction.

## 5. Source candidates and provenance

The package includes the exact four prespawn-barrier candidate artifacts, five baseline cache-copy input artifacts and six timing-only namespace candidate artifacts. Their original metadata is preserved, including author-time NOT_RUN or unapplied status. Later actual stage28 application and focused observations are recorded separately in Table 1.

The prespawn barrier changes the completion check in a test helper. Its exact 1,040-byte patch and 32,663-byte afterimage are retained. [source-provenance.json](source-provenance.json) records a DATA replay of the single hunk against the exact [public base file](https://github.com/applicate2628/mcp-local-hub/blob/5ad374eb60cadb8ad56ea167e3fb06edb44bdc18/internal/cli/supervise_prespawn_binary_gate_test.go). The reconstructed afterimage equals SHA-256 `5e3a6517a9fed3a329cea60d12915965720b37c2bb6a865880d3c0c05ed48feb`.

The two shared-image afterimages are supplied by the separate [shared-image candidate capsule](../cli-shared-image-candidate-20261128/README.md), manifest SHA-256 `74ef1f32f87f005ac265e07199f67c2e76931d000936e43107a1c3a02a3484af`. Their exact stage28 applied hashes are in `evidence.json`; the same source files are not duplicated here.

The timing candidate retains its two exact Go afterimages, source delta, manifest, README and DATA checker. It adds timing observations without claiming a new timing result in this package. The two-entry compiled overlay is preserved under `namespace-timing/namespace-timing-overlay.json`. The separately evolving caller and configuration in that directory were outside this collection.

The baseline cache-copy candidate retains both its original and changed source, exact delta, composition and README. The actual stage24 cache handoff and tests are supported by their own closed original records. The presence of a copier or a hash never changes a failed or UNKNOWN donor into a PASS donor.

## 6. Publication finding and disclosure

Push8 remains the original rejected publication attempt. Stage25's canonical diagnosis identified the exact regions; its supported wrapper source and closed native captures are preserved here. The safe locations and fixture-template correction are provided by the [complete historical capture companion](../original-full-v6-complete-captures-20261128/README.md), manifest SHA-256 `e6d697ad37b8bc9a892496d2dad6ad00759bfc367b4a3cd18a70ece88689d861`.

Actual host directories and the operator profile are represented by explicit role markers. The original source afterimage contains 16 source-defined path literals or example occurrences, all unchanged from the byte-identical public base. Seven occurrences reported by the canonical range scan are now explicit fixture-role markers in the published template; nine other source fixture/comment occurrences remain unchanged. Their source locations and hashes are recorded without reproducing the seven replaced values in the provenance summary.

[disclosure-review.json](disclosure-review.json) distinguishes those source fixtures from the host-value check. No secret-token-format finding or actual host-path finding outside the proven public test source was found in this bounded author review. This is not a canonical publication scan result, an allow-list or an approval. The native publication gate and its policy remain unchanged.

## 7. Resource disposition and boundaries

No file or directory was moved or deleted during this preservation work. Original closed event roots, role values and potentially reusable compiler/cache artifacts remain with their owner until exact public readback and a separate proof that each proposed disposable item is unused. A hash-only reference is not a disposable-data proof. See [resource-disposition.json](resource-disposition.json).

The old full-v6 result remains FAIL/CLOSED. The old full-v8 UNKNOWN status remains unchanged. This supplement obtains no closure for retained UNKNOWN full59f/CLI records and includes no UNKNOWN payload. No stage29-or-later run, active Source or cache was read for this package. It claims no full PASS, no new 14+2 acceptance and no completion or identity for the still unresolved original expected-refusal control.

## 8. Primary references

1. [source-bindings.json](source-bindings.json): exact mapping for all 197 originals.
2. [event-index.json](event-index.json): original event and command receipts, closure observations and capture mappings.
3. [evidence.json](evidence.json): derived outcomes, individual focused test observations, primary receiving pins and scope limits.
4. [source-provenance.json](source-provenance.json): public source identity and exact prespawn-barrier DATA replay.
5. [Source59f closed API companion](../source59f-closed-api-diagnostics-20261128/README.md), manifest `606fae7b90c1c18c37adff72aa73410e0c3668fb945ee21f44e196ce0aedeed6`.
6. [Historical full-v6 complete captures](../original-full-v6-complete-captures-20261128/README.md), fixture-template v2 manifest `e6d697ad37b8bc9a892496d2dad6ad00759bfc367b4a3cd18a70ece88689d861`.
7. [Shared-image candidate](../cli-shared-image-candidate-20261128/README.md), manifest `74ef1f32f87f005ac265e07199f67c2e76931d000936e43107a1c3a02a3484af`.

## 9. Publication template revision after the original range finding

The original push11 remains FAIL/CLOSED. The unchanged canonical scanner reported seven machine-path occurrences in the preserved barrier test afterimage. This revision replaces only those seven literal contents with three explicit role markers; it keeps all 197 original byte counts, hashes, event outcomes and contracts unchanged. The prior frozen capsule is retained separately and no Source or runtime input is edited.

[fixture-template-revision.json](fixture-template-revision.json) records the exact original/public hashes and lexical ranges. All three values already exist in the pinned public source blob; the complete 32,425-byte beforeimage was fetched through GitHub and its SHA-256 matched. A reader can obtain each value from the stated source line and zero-based, end-exclusive column range, validate its value hash, and supply it as an explicit role. The public package embeds neither the values nor an encoded/default copy. The shared renderer still verifies every reconstructed original SHA-256 before writing a new destination.

The revised capsule has twelve roles, 151 exact original entries, 46 template entries and 44,531 substitutions. Its 112 payloads include the revised template and 111 byte-identical payloads. Line 26 and all other non-reported source bytes remain unchanged. The renderer code changes only its pinned bindings hash. No new scanner PASS or acceptance result is claimed; the native gate is unchanged and must review the actual proposed range.
