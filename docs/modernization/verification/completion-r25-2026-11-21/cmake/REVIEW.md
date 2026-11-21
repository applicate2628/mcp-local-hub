# R25 — independent review of the CMake retention diagnostic

## Contents

1. [Decision and reviewed identity](#decision-and-reviewed-identity)
2. [Workload and source comparison](#workload-and-source-comparison)
3. [Ownership and collection boundaries](#ownership-and-collection-boundaries)
4. [Concurrency and semantic oracle](#concurrency-and-semantic-oracle)
5. [Measurement interpretation](#measurement-interpretation)
6. [Execution evidence still required](#execution-evidence-still-required)
7. [References](#references)
8. [Terms and abbreviations](#terms-and-abbreviations)
9. [Final formatted revision and input identity addendum](#final-formatted-revision-and-input-identity-addendum)

## Decision and reviewed identity

**No blocking source defect was established in the reviewed revision for the requested diagnostic scope.** The helper can support a controlled comparison of whole-process Go heap allocation and retention while result and encoded-output owners are held and then released. This is an independent source review, not compilation or execution evidence. It establishes no memory threshold, ordinary latency, peak resident memory, throughput or statistical significance.

The final reviewed revision is SHA-256 `c73994c1e23f1ebdc00f56abae36a1f987ac88e6f8f711e753c889940ff4c13f`; its bounded changes are recorded in the [addendum](#final-formatted-revision-and-input-identity-addendum). The original review below retains its earlier source identity and line references.

The reviewed file is `candidate/internal/vcpkgmcp/cmaketrace/cmake_retention_diagnostic_test.go` (11,866 bytes), SHA-256 **`e9815112b207377fd9fc6d0d91c0a522611be770dcb2946e19cfa22dd50f94c5`**. It is a diagnostic overlay intended for the existing #623 evidence archive, not a production source change or installed consumer.

The first reviewed revision, SHA-256 `3f1b230918984e969a11249dd4611c01cfa8588b9a4348da046199a0bc923afa`, used one forced garbage collection at each quiescent snapshot. Review identified that internal encoder pools can survive that first collection and age out at the next ownership phase. Root then added `consumerCollectMemory` with two collections before each quiescent snapshot. That was initially a proposed unreviewed revision; the new function and all four call sites were subsequently read at the current hash above. Reversing only those changes in memory exactly reproduced the first hash, confirming the bounded delta. `AfterWork` remains a direct snapshot without a forced collection. Neither revision was run by this reviewer.

The two-cycle change reduces a known source of phase contamination; it does not turn a whole-process reading into an exact per-value size. The resulting limitation is stated in the helper's own comment at lines 189–195 and remains part of the interpretation below.

## Workload and source comparison

The helper uses the retained original R22 oracle and trace read-only. It does not invoke CMake, create an installation or regenerate the inputs. Three existing cases (`selected-32`, `selected-1000`, `last-marker`) run with one and four joined clients, three samples per combination: **18 observation rows per source variant**, 45 measured `Trace` calls and three warmup calls. The full two-variant comparison should therefore contain 36 rows. These are expected counts from source inspection, not completed observations.

**Table 1. Original input identities and their public redacted counterparts.**

| Artifact | Original bytes | Original SHA-256 | Public redacted bytes | Public redacted SHA-256 |
|---|---:|---|---:|---|
| `producer/trace.jsonl` | 11,827,770 | `755212e57a9318e4945f480d4152e3352053002606f97d20dc3c033055b1a7e8` | 11,302,778 | `a79b7793b5f384f885afb60fb513402d4523bcc0779546aa1e199e15a80fd286` |
| `producer/oracle.json` | 891,803 | `75cfff1d98fbc3ebd1e7d2acafc37d56f6999d0b8112de68ceefe80e634fb381` | 858,127 | `277214f9fd1adb30cbc5f06945b944f94299363dcd7db58e2fb35b353741f085` |

These mappings were verified in `completion-public/r22/MANIFEST.json:373–389`; the local `source/` files match the public redacted hashes. They are source/reference material and must not silently replace the original execution inputs. `loadGeneratedConsumerOracle` (lines 66–100) checks the schema, case order, original trace size/hash and fixture source hashes. The oracle's own original hash is an outer execution identity: it must be retained in root's pre/post readback, since self-consistency with values inside an oracle does not independently pin that oracle's bytes.

The source comparison was inspected directly in the existing Git object store:

- Old source: `251fb4f03405ba438662976137afec0d9c27105e`.
- Current #624 source: `81dcf67db013ba2879878c2ee6fe07aa0bb4aec3`.
- The production delta is exactly five added lines in `internal/vcpkgmcp/cmaketrace/cmaketrace.go`: four comment/spacing lines and `clear(parsed.records[len(records):])` after unfiltered indexes and the returned-record limit have been computed. The only other changed file is a 100-line regression test.

The current production file's SHA-256 is `53a704edffffdd7ee7c8de95584f20f46ac7e6cad405446c05b56ed7c5c396b0`, consistent with the retained exact-source manifest. `filterRecords` reuses the parsed slice's backing array; the new clear changes which discarded record references remain reachable while a returned result retains that array. No fixture result, public result budget or production decoding algorithm is changed by the diagnostic helper.

## Ownership and collection boundaries

**Table 2. Owners retained at each snapshot in the current helper.**

| Snapshot | Source | Deliberate liveness and interpretation |
|---|---|---|
| `Before` | lines 221–231 | Empty result/body containers, error slots, interval storage, start channel and oracle already exist. Two collections precede the allocator reading. No consumer worker has started. |
| `AfterWork` | lines 245–248 | All worker writes are joined; results and bodies remain needed for validation. No forced collection is inserted here. The reading can include unreachable work allocations awaiting collection. |
| `ResultsAndWire` | lines 263–265 | Two collections and a reading occur before `KeepAlive(results)` and `KeepAlive(bodies)`. Both ownership graphs are therefore retained through the snapshot. |
| `WireOnly` | lines 266–268 | `results` is set to nil. Two collections and a reading occur before the final `KeepAlive(bodies)`. The wire bodies are deliberately retained while result ownership has been dropped. |
| `Released` | lines 269–274 | `bodies` is set to nil. Two collections precede the reading. The oracle, error slice and start channel remain alive through explicit later uses. The scalar observation and intervals remain live because they are returned. |

The `KeepAlive` positions cover both collections inside the helper, not merely the call to `ReadMemStats`. There is no returned closure, package global or observation field retaining `results` or `bodies`. Worker interval storage contains integer timestamps only. A returned result may still share data with its own derived indexes by design; dropping the result releases that entire result-owned graph, rather than only its visible record prefix. The oracle is a separate stable owner of expected data and is deliberately present in all phases [1].

The result and body slice containers are allocated before `Before` and later dropped along with their contents. Consequently a small negative `Released.HeapAlloc - Before.HeapAlloc` is possible and is not evidence of corruption. The baseline is a process snapshot, not a mathematically exact empty object state. Observation logging from earlier samples can remain live in the test framework; taking a new baseline per sample accounts for that retained history more honestly than treating absolute snapshots from different samples as interchangeable.

No owned worker is intentionally detached. There are no new callbacks or external processes, and the helper does not retain open trace files. File read/close behavior remains owned by the existing `Trace` implementation and the existing hash helper. Normal snapshots occur only after the worker group is joined.

## Concurrency and semantic oracle

The concurrency ownership is coherent at lines 221–260. Each goroutine receives its own index and writes exactly one `results`, `bodies`, `errors` and `intervals` element. No goroutine appends to those slices or changes their headers. The oracle and query are read-only. `joined.Add(1)` occurs before each launch; `Wait` occurs after all launches. The corresponding `Done` calls order every worker's writes before the main goroutine reads results and metrics [2]. No `t.Fatal` is called from an unjoined worker.

Closing the common start channel admits all created workers, but it does not guarantee simultaneous scheduling. The added `[start,end]` intervals describe each actual `Trace` plus public-encoding interval, relative to the same monotonic `begin` time. They allow root to distinguish joined clients from observed overlap. Do not report four overlapping calls solely because `Clients == 4`; inspect the interval intersection. `WorkNanoseconds` also includes goroutine launch, admission and join overhead.

The semantic oracle at lines 103–163 is materially stronger than checking record counts:

- Every worker's full result must equal the independently retained CMake oracle, including indexes, evidence, completeness, version and truncation fields.
- Nonprojected output must decode back to the full expected result, with the required public fields present.
- Projected output must explicitly omit exactly four named collections, preserve verdict/evidence, report zero retained entries and the correct omitted counts, and remain within the public byte cap. Duplicate or unrecognized omission entries are rejected by the delete-on-use map.
- All workers' encoded public bytes must be identical within each sample. A hash is retained for cross-source comparison.

The helper never replaces useful output with an easier empty result to reduce memory. However, cross-source equality is an outer comparison: root must compare the corresponding wire hashes/lengths and recorded semantics from both variants. The helper does not itself join two separately executed variants into one pass/fail decision.

## Measurement interpretation

**Table 3. Valid use of recorded values.**

| Value | Supported interpretation |
|---|---|
| `AfterWork.TotalAlloc - Before.TotalAlloc` and the corresponding `Mallocs` delta | Cumulative heap allocation for the instrumented batch, including worker orchestration and public encoding. These are not retained bytes and not parser-only cost. |
| `ResultsAndWire.HeapAlloc` relative to the same sample's baseline | Whole-process heap allocation remaining after deliberate collection while result and body owners are live. Compare matching case/client cohorts between the two exact sources. |
| `WireOnly` and `Released` | Further process snapshots after dropping the selected ownership graphs. Differences may include runtime bookkeeping, caches and scaffolding, so do not label them exact sizes of `Result` or a byte slice. |
| `HeapInuse`, `HeapReleased`, `StackInuse`, `Sys` | Distinct allocator/span/runtime observations. They are not substitutes for process resident memory, and ordinary collection does not promise that all unused pages have been returned to the operating system. |
| `NumGC` | Recorded collection count. The two-cycle calls should be visible as increases of at least two between successive quiescent phases; autonomous cycles can add more. It is not a count of exclusively forced collections. |

Semantic validation decodes public JSON after `AfterWork` and before the held snapshot. Therefore **later `TotalAlloc` deltas include validator allocations**. Use the `AfterWork` interval for the stated consumer-batch allocation question. The recorded timing ends before validation and collection but remains instrumented batch timing; it does not establish ordinary request latency or a latency distribution.

The first-version concern was concrete: the standard JSON encoder uses a `sync.Pool`, and the pool implementation can move primary entries into a victim cache for one collection before dropping them on a later one [3–4]. Thus one forced collection per phase could attribute cache aging to the release of a result. The current two-cycle helper addresses this known confound under quiescence. This is an implementation-aware diagnostic improvement, not a general guarantee that no runtime owner or transient allocation affects a snapshot.

Three samples per case/client combination are descriptive repeats. Keep their individual values and spread; do not claim a confidence interval, statistical significance, service capacity or a production budget. Record the same toolchain, compiler/build flags, architecture, `GOMAXPROCS`, garbage-collector and memory-limit settings for both variants. A race-instrumented correctness run, if separately chosen, must not be mixed into ordinary-build allocation comparisons.

## Execution evidence still required

This reviewer performed source reads, immutable Git comparisons, local artifact hashes and the in-memory reverse-delta identity check. **No Go compiler, diagnostic, benchmark, CMake process or remote execution was run, and no helper source was modified.** Only this report was written.

Before interpreting a run, root still owns the final formatted helper hash, exact source trees plus overlay, unchanged original oracle/trace/source hashes, environment/build identity, successful semantic checks, all 18 rows per variant, interval readback and complete process settlement. Formatting can change the reviewed byte hash; preserve that relationship explicitly. The source review does not itself satisfy those execution requirements.

No further code change is requested for the diagnostic scope reviewed here. A result claiming exact object ownership, peak memory, resident memory, ordinary latency, a strict performance threshold or population-level significance would exceed this review's accepted interpretation.

## References

1. [Go runtime: KeepAlive and allocator statistics](https://pkg.go.dev/runtime), official documentation viewed as Go 1.27.4 during review.
2. [Go sync: WaitGroup.Done/Wait ordering](https://pkg.go.dev/sync#WaitGroup).
3. [Go standard JSON encoder source](https://go.dev/src/encoding/json/encode.go), `encodeStatePool` and pooled encoder state.
4. [Go sync.Pool source](https://go.dev/src/sync/pool.go), `poolCleanup` primary/victim-cache transitions; exact installed toolchain/build flags remain execution metadata.
5. [Current #624 production source](https://github.com/applicate2628/mcp-local-hub/blob/81dcf67db013ba2879878c2ee6fe07aa0bb4aec3/internal/vcpkgmcp/cmaketrace/cmaketrace.go) and its parent `251fb4f03405ba438662976137afec0d9c27105e`, compared through the existing local Git object store.
6. Local retained evidence: `completion-public/r22/MANIFEST.json`, `completion-download-b1d932ea/package/r22/execution-summary.json`, `r24-audit/root/source-manifest.json`, and `r25-audit/cmake/source/`.

## Terms and abbreviations

- **GC:** garbage collection, Go's process for finding and reclaiming unreachable memory.
- **Heap:** memory used for allocated Go objects; its accounting differs from physical memory resident in the operating system.
- **RSS:** resident set size, a process-level physical-memory measure that this helper does not collect.
- **Oracle:** independent expected values used to reject wrong observable behavior.
- **Wire body:** the encoded public JSON result, before any separate Model Context Protocol transport envelope.
- **SHA-256:** Secure Hash Algorithm with a 256-bit output, identifying exact artifact bytes.
- **PR:** pull request; this review neither modifies production nor authorizes a merge.

Execution role: independent source reviewer, distinct from root as helper author. Applied skills: Go Performance and Diagnostics, Go Concurrency Lifecycle, Go Testing and Verification. Actual path: current conversation read-only tools and one local report write. Model/profile identity: unspecified by runtime. No additional provider or quota-consuming reviewer was launched.

## Final formatted revision and input identity addendum

The final helper was independently read at **12,122 bytes**, SHA-256 **`c73994c1e23f1ebdc00f56abae36a1f987ac88e6f8f711e753c889940ff4c13f`**. Root reports that `gofmt` on the owned D: checkout changed only field alignment and a trailing blank line, producing the intermediate SHA-256 `12eba51da0b471b080e901aba80d700fd2cc7ec71bb65b984048f2f65b1e1b1e` (11,828 bytes). The reviewer did not execute that formatting step. Removing only the two subsequently added input-check blocks from the final source **in memory** independently reproduced that exact intermediate hash.

`loadGeneratedConsumerOracle` now hashes the bytes read from the original oracle and checks the fixed SHA-256 `75cfff1d98fbc3ebd1e7d2acafc37d56f6999d0b8112de68ceefe80e634fb381` **before unmarshalling them**. This closes the earlier source-level gap where the oracle's own identity depended solely on outer execution evidence. Its existing loader then verifies the trace size and hash and the fixture source hashes against the pinned oracle. The final call to the same loader occurs after every case, client cohort and sample, rechecking the oracle, trace and fixture identities. These are initial and final identity checks; they do not provide a continuously held read lock or prove that no intervening modification was possible.

Neither addition changes worker ownership, joining, the semantic result oracle, the public encoding checks, collection phases, `KeepAlive` placement, per-worker intervals or the recorded metrics. The final loader call and its temporary allocations occur after all observations and logged rows, outside every measured `WorkNanoseconds` interval. The initial check occurs before warmup and the first per-sample baseline. The whole-process and descriptive-only interpretation above therefore remains required.

**No new blocking source defect was established at this final hash.** This addendum is source-only: the reviewer has not compiled or run this helper, invoked CMake, executed remotely or changed the helper source. Root still owns execution evidence, successful final input revalidation, exact toolchain/environment identity and the complete matching rows from both source variants. No earlier test result is relabelled as a run of this revision.
