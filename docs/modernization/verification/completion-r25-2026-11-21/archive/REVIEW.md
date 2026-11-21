# R25 archive scripts — source review

## Contents

1. [Decision](#decision)
2. [Required behavior](#required-behavior)
3. [Corrections during self-review](#corrections-during-self-review)
4. [Verification performed and remaining](#verification-performed-and-remaining)
5. [Completed second attempt and public transfer](#completed-second-attempt-and-public-transfer)

## Decision

Prepared a new packager and an independent standalone public verifier. Root's first actual Windows packaging attempt failed before publication; its bounded correction and five synthetic stat checks are recorded below. Root's second attempt completed, and this reviewer then performed the explicitly delegated transfer of four public artifacts and an independent local verifier run. The 12 earlier synthetic archive cases remain separate artifact checks. Root owns Product/runtime execution and GitHub publication; this reviewer's completed remote actions were limited to the public-artifact transfer. Transport cleanup is separately authorized and receives its own subsequent receipt.

The relevant source patterns were read in `r24-audit/runtime/package_evidence.py` and `verify_public_archive.py`; the R25 owner wrapper and available exact scope manifests were read separately. The implementation does not import or mutate the R24 module's globals. Its packager is approximately 240 lines and its standalone verifier approximately 360 lines; most verifier code is explicit evidence binding, rather than a generic framework. The code is longer than a minimal ZIP writer because a ZIP integrity result must not silently become a qualification PASS.

Architecture Review, Go Testing and Verification, Go Concurrency Lifecycle and Ponytail Code Simplification were applied earlier in this continued R25 task. Their relevant principles here are explicit evidence authority, finite ownership, separate source/runtime claims and avoiding a second execution/capture abstraction. No new provider or quota-consuming review was launched.

## Required behavior

| Area | Review finding |
|---|---|
| Authority | Required external index SHA-256; exact scope-manifest pins; fixed root; frozen owner SHA; wrapper hash bound to plans and receipts. The public verifier never imports archive bytes. |
| Read boundary | Only three named evidence directories are traversed. Other support files are indexed individually. Original Go files are direct manifest-derived reads; missing originals are explicitly checked. No source/cache/runtime/trash traversal. |
| Historical evidence | V1 has only the preparation phase admitted. V2 remains an expected compile failure, with linked build failure events, no test events and a required compiler diagnostic. Later commands/phases do not acquire PASS by omission. |
| Candidate identity | Exact SHA-256, byte count and Git blob for unredacted candidate bytes. Preimages bind to original Git blobs; source-evidence files also bind to declared SHA-256. Changed public originals remain declared limitations. |
| Capture ownership | Final/initial receipts, environment, overlay and every recorded command/observation lifecycle and stdout/stderr capture are retained and checked. Unknown phase files and incomplete execution prefixes are rejected. |
| Test interpretation | The verifier parses Go JSON independently, compares terminal lists with captures and manifests, checks required diagnostic output, and reports roots, subtests and packages separately. Compilation is not labelled test execution. |
| Publication | Unique D: destination, exclusive creates, ZIP CRC/member-digest readback, second complete input read, completion marker written last. A partial new publication is preserved and not treated as complete. |
| Redaction | Two literal byte replacements only. The file map declares original/public size/hash and replacement counts; public logs with unchanged bytes are checked directly against raw capture hashes. |
| Compatibility | Standard-library Python only; Windows packager, platform-independent verifier. No dependency installation, tool invocation, network access, subprocess or owner import in either path. |

## Corrections during self-review

These issues were corrected before any invocation of either script:

1. **Compressed ZIP bound:** the first draft reused a 128 MiB individual-file reader for complete archives, contradicting the 512 MiB total archive bound. Archive readbacks now pass the explicit larger bound; source/evidence members retain the smaller bound.
2. **Incomplete capture suffixes:** the first verifier draft accepted extra files inside an otherwise admitted evidence phase. It now reconstructs the exact required lifecycle/receipt/stream file set, so an unaccounted later command start or missing capture cannot be ignored.
3. **Verification result schema:** a dictionary merge initially allowed the embedded summary schema to replace the standalone verification schema. The verification result now has its own explicit final schema.
4. **Redaction accounting:** the verifier now checks exact replacement-count keys, nonnegative integer counts and agreement with the changed flag.
5. **Optional format directory:** inventory now distinguishes an absent directory from a broken symbolic link before deciding whether to skip it.
6. **Memory bound while collecting:** the aggregate byte bound is checked as files are captured, rather than after an unlimited accumulation.
7. **Direct-command deadline binding:** recorded `direct_timeout_seconds` must match the planned command timeout; exact command arguments alone do not bind that capture-owner setting.

No producer qualification, native safety property, Product admission or full-test result is inferred from these checks. The source preimages authenticate exact archived file contents against manifest Git blobs; the packager does not rerun Git to establish an independent checkout-wide tree identity. That identity remains tied to captured source observations.

## Verification performed and remaining

Performed: source comparison with R24 patterns, direct reading of R25 wrapper/manifest data and retained V2 compiler receipt/output, inspection of the frozen owner's capture filenames in the existing R24 ZIP, Python AST syntax parsing of both new sources, and local file hashing. Root then explicitly authorized local synthetic guard tests. This reviewer never called the packager's `main`; its module was later imported only to call `read_regular` with synthetic path/descriptor observations, as described below.

`synthetic_guard_checks.py` constructs labelled synthetic capture graphs and ZIP bytes entirely in memory. It reads the real unchanged frozen-owner bytes from the existing local R24 archive solely to exercise the fixed hash pin; that owner and every synthetic script/receipt member remain unexecuted. Only the clearly labelled synthetic test report is written. No synthetic capture is placed in the R25 runtime evidence or published as a real execution receipt.

All 12 cases passed against verifier SHA-256 `7a5361a4eb4a1b802d91d8bfd8276e91487650b650ca5d8d005498852116a539`: a valid ZIP with repeated count-3 terminals; rejection of collapsed counts; preservation of a compilation failure and later NOT_RUN; rejection of test activity inside a claimed pre-test compilation failure; modified stdout; extra/missing capture members; changed manifest and index pins; changed source preimage; wrong overlay target; duplicate ZIP member. Mutation cases recompute the ordinary member map where appropriate, so the receipt/source/plan binding must detect them. The positive repeated case independently asserts three root PASS, three subtest PASS and one package PASS. The report is `synthetic-guards-7a5361a4eb4a.json`; these are artifact-consistency results only.

Completed after the initial source review: root's independent code review, the final seven-scope index and explicit support inventory, actual read-only packaging on Windows, byte-exact transfer of its four public artifacts, and the standalone verifier run against both external pins. Root also reported a matching independent local verification. Any future additional scope requires a new reviewed index and publication; it is not covered by this completed archive. The validation still does not authenticate runtime execution or qualify unexecuted phases.

Local files here are Git-backed project review material. No inputs or recovery copies were removed. The scripts intentionally perform no cleanup; root can later move a confirmed unnecessary partial publication to trash without discarding active or required evidence.

### Observed Windows correction after the first packaging attempt

The first root invocation used packager `80029a94fcc3e70c89d1e0ca16662587db3bb683e0d768fcfac2549241622b60` and index `36697e659091c0c99fbf45105eec03a34c017200623afa9ec1161ed7af3ef8f6`; it failed with `Input changed while opening` before a publication was created. Both exact source/index byte sequences and the reported failure were preserved in `first-run-read-failure-v1/` before correction.

An initial index-only root probe showed equal fields and did not justify weakening a guard. Root then inspected the admitted inputs and observed a concrete representation difference in Python 3.14.7: for the already written CMake log, `lstat.st_ctime_ns` was the birth time `1795264895100974000`, while `fstat.st_ctime_ns` was `1795264897640541700`; both APIs agreed on device, inode, length, mtime, birthtime and attributes. The individual path before/after and descriptor opened/after observations were stable, and bytes matched the expected stdout digest. The durable root probe is `D:\dev\ChatGPT\mcp-r25-focused-20261121\inspect_archive_inputs-result.json`. This is a concrete observed API distinction, not evidence of modified log bytes and not a blanket claim about all Python versions. General Python documentation separately notes the evolving Windows `st_ctime` semantics: [Python 3.14 os.stat_result](https://docs.python.org/3.14/library/os.html#os.stat_result).

The minimal revised packager, SHA-256 `bd655c27959379e17da702ebc13d47a475b8f7a2e04e923ef5c78bf034d510e9`, compares the six common fields across APIs, compares full signatures including `ctime` within each API, and returns both signatures for the second complete input read. Every reader mismatch now identifies the path. No `ctime` observation is discarded. The verifier remains unchanged at `7a5361a4eb4a1b802d91d8bfd8276e91487650b650ca5d8d005498852116a539`.

Five focused local cases passed in `synthetic_stat_checks.py`: the observed cross-API `ctime` difference with stable per-API values is accepted; path `ctime` drift is rejected; descriptor `ctime` drift is rejected; common inode mismatch is rejected; both full signatures remain visible when a later individually stable read changes either API's `ctime`. The report is `synthetic-stat-guards-bd655c279593.json`. These are synthetic observations with path operations and `fstat` replaced in memory, not a Windows or Product run. Root then supplied the new external index pin and completed the actual rerun described next.

## Completed second attempt and public transfer

Root's corrected second packager run completed under final index SHA-256 `5143480a3c9be41a678301c49f60ec251e596f4432058591ecf10401016d39b6`, producing the unique D: publication ending `package-20261121T130804Z-a18202cb28444e27b2b0ec2c2f95539d`. The original failed script/index, failure metadata and diagnostic inputs remain preserved; the first attempt has not been relabelled PASS.

The fixed publication's completion marker was independently read through Remote Desktop Commander before transfer. `export_public_transfer.py`, SHA-256 `d593eb16c24b5250dce2b6f9bd84dabc172b41763303e108b26edf859fbf216a`, received a separate source review by `/root/r25_rust_inputs`. Its four-file literal allowlist excludes `raw.zip` from reads; it makes two stable reads, checks completion pins, writes only exclusive new files under a unique D: transfer directory, and limits each Base64 payload to 60,000 characters. A bootstrap verified the exact script hash before executing it once. The durable stdout reported COMPLETE and stderr was empty.

All 75 chunks were fetched and their individual byte counts/digests checked before assembly. The local assembler also checked the externally read public ZIP pin and the completion marker's other public pins. Its output is exactly the four files listed in [README.md, Table 1](README.md#completed-publication-and-independent-verification). The private raw archive was not read or transferred. The completed publication was never opened for writing.

The unchanged standalone verifier ran locally with `-I -B`, external index pin `5143480a…d39b6` and external public ZIP pin `e4da7afcf31d5310df6fcd3d10e13938c01f5b997d7677c9708583c87a6dcf55`; exit status was zero. [public-verification.json](public-verification.json), SHA-256 `09770c54b45d943d01eabe8c4b10f4382dd5f05bc1285580e6e371c1427e4050`, records 2,721 members, seven scopes, eight candidate Go files and 1,038 recomputed raw stdout/stderr digests. Every stream is unredacted. The only changed members are the two packager source copies, each with two declared literal substitutions; comparison to the retained local source preimages confirmed those substitutions exactly. The standalone verifier bytes in the ZIP match the original verifier.

A further independent read of every relevant stdout reconstructed the same 16 root PASS, 360 subtest PASS, 362 leaf PASS and 10 package PASS events, with the single historical compilation/package failure preserved. There were 11 test-command attempts and four build/vet commands. The sidecar manifest/summary and local final index match their ZIP members byte-for-byte. [public-cross-check.json](public-cross-check.json) records these checks; no archive code, Go command or Product was executed.

The evidence result is `BOUNDED_RESULTS_ONLY`; runtime authentication remains `NOT_ESTABLISHED`. Member digests, capture consistency and an externally supplied index do not independently establish the execution environment, full-suite gates or process-tree settlement. Original bytes of the two redacted script copies cannot be authenticated from their public copies alone; the separately retained local preimages supplied the additional redaction check here. Root independently confirmed matching public results and then authorized transport-only cleanup; transfer receipts, scripts, logs and final artifacts remain outside trash.
