# R25 evidence archive: index and execution contract

## Contents

1. [Scope and files](#scope-and-files)
2. [Completed publication and independent verification](#completed-publication-and-independent-verification)
3. [Final index](#final-index)
4. [Root execution](#root-execution)
5. [Result interpretation](#result-interpretation)
6. [Boundaries and retention](#boundaries-and-retention)

## Scope and files

`package_evidence.py` is a read-only evidence collector with new, unique publication outputs. It never invokes Go, Git, CMake, a command shell, an installed tool or a captured owner. It imports only the independently hash-pinned local verifier bytes to share the same evidence checks. It does not import any archive member or copy the R24 packager's mutable globals.

`verify_public_archive.py` is standalone: Python's standard library is sufficient. It opens a ZIP in memory, checks its cyclic redundancy checks (CRC), recomputes file digests and reconstructs the expected outcomes from the externally pinned index. It does not extract members, import their code or execute tests.

Root's first Windows packager invocation stopped before publication on a path/descriptor `ctime` mismatch. The original script/index and failure remain in `first-run-read-failure-v1/`. The revised reader compares common identity fields across the APIs and preserves strict `ctime` checks within each API and across the final reread. Root's second actual invocation completed; the public ZIP was then transferred byte-for-byte and independently verified locally. Separately, five synthetic stat cases and 12 synthetic archive guard cases passed. These unit cases are artifact checks, not Product, Go or native execution results.

## Completed publication and independent verification

The completed publication is `D:\dev\ChatGPT\mcp-r25-focused-20261121\publication\package-20261121T130804Z-a18202cb28444e27b2b0ec2c2f95539d`. Its final externally pinned index is `5143480a3c9be41a678301c49f60ec251e596f4432058591ecf10401016d39b6`. The executed packager is `bd655c27959379e17da702ebc13d47a475b8f7a2e04e923ef5c78bf034d510e9`; the standalone verifier remains `7a5361a4eb4a1b802d91d8bfd8276e91487650b650ca5d8d005498852116a539`.

**Table 1. Exact public transfer outputs, retained under `public/`.**

| File | Bytes | SHA-256 |
|---|---:|---|
| `archives.json` | 920 | `aa13912182114e0b1add6f0774044a81c18ed9e442be63c62166b6f22504f682` |
| `public.zip` | 2,047,483 | `e4da7afcf31d5310df6fcd3d10e13938c01f5b997d7677c9708583c87a6dcf55` |
| `manifest.public.json` | 1,187,225 | `cd49af58398fd05c2abb89f0cd943a16cea66aa1406c8742e228b09d9ec466fd` |
| `summary.public.json` | 11,578 | `a47da3e9ae7c7f446969fbdfcd8c7c85ab78472b05be84dab001ab0b909740c8` |

The independent [public verification result](public-verification.json) reports integrity and evidence consistency PASS, with both external ZIP and index pins matched. There are **2,721 members: 2,719 mapped inputs and two generated metadata members**, 7,052,275 expanded bytes, seven scopes and eight candidate Go files. All **1,038 stdout/stderr streams are unchanged public bytes**, so every raw stream digest was independently recomputed. Sidecar metadata, the final index and the standalone verifier were also compared byte-for-byte with their ZIP members in [public-cross-check.json](public-cross-check.json).

Exactly two files are redacted: the current and preserved V1 packager sources. Each has one plain and one JSON-escaped profile-literal substitution. Their public copies are redacted review evidence and do not have the original packager source digests. Comparison with retained, hash-pinned local preimages independently confirmed those exact four substitutions in [public-redaction-cross-check.json](public-redaction-cross-check.json). This additional local check cannot be reproduced from the redacted public copies alone; `raw.zip` remains private on D: and was neither read nor transferred by this reviewer.

The reconstructed results remain **`BOUNDED_RESULTS_ONLY`**, with `runtime_authentication: NOT_ESTABLISHED`: 11 Go test-command attempts, four build/vet commands, 16 root PASS events, 360 subtest PASS events, 10 package PASS events and one historical package/build failure before tests. A separate raw-stdout recount finds 362 successful leaf executions; roots and subtests total 376 test PASS events. The V1 rejection, V2 compilation failure and `invalid-fixtures/build-vet: NOT_RUN` retain their original classifications. ZIP integrity establishes byte consistency, not independent proof of machine identity, test execution, full qualification or process-tree settlement.

Root separately confirmed a matching independent local verification before authorizing cleanup of the 75 transfer-only Base64 chunks. Transfer scripts, metadata, logs, verification receipts and all four public artifacts remain outside trash. The [transfer receipt](transfer-verification.json) records the exact source and output identities.

## Final index

Root owns `D:\dev\ChatGPT\mcp-r25-focused-20261121\archive-index.json` and supplies its exact Secure Hash Algorithm 256-bit digest (SHA-256) on the command line. The index is not self-pinned. The accompanying `archive-index.example.json` has an intentionally rejected template schema: change it to `r25-archive-index-v1` only after reviewing the final entries and actual outcomes.

The final schema has these fields:

**Table 2. Final index fields.**

| Field | Required meaning |
|---|---|
| `schema` | Exactly `r25-archive-index-v1`. |
| `root` | Exactly `D:\dev\ChatGPT\mcp-r25-focused-20261121`. |
| `files` | Explicit records `{path, sha256}` for `qualify_scope.py`, both archive scripts, and every transfer readback/formatter/decoder that must be retained. No implicit root-directory traversal. Root files must end in `.py`, `.json` or `.txt`; explicit descendants of `transfer-readbacks/` are also allowed. |
| `scopes` | Records `{scope, manifest_sha256, phases}`. The manifest must be the immutable final bytes for that scope, including historical V1/V2. |
| `phases` | Exactly the complete set of phase names in that scope manifest. Each value is an object containing `status`; a historical compile failure also requires `command` and a nonempty `required_output` list. |

Supported phase statuses are deliberately narrow:

**Table 3. Indexed phase outcomes and required evidence.**

| Status | Required evidence |
|---|---|
| `PASS` | Complete final receipt, zero-exit settled commands, exact ordered command membership, original source observations, matching overlay, lifecycle and stream captures, and the manifest's test/diagnostic oracle. |
| `COMPILE_FAILED_BEFORE_TESTS` | Failed/incomplete phase receipt; exactly the executed command prefix ending at the indexed command; that command exits 1 with matching compiler diagnostic, a `build-fail` event and linked `FailedBuild`, one package FAIL, and **no event containing `Test`**. Remaining planned commands are reported `NOT_RUN`. |
| `REJECTED_BEFORE_RUNTIME` | No evidence folder for that phase. The candidate and its earlier preparation evidence remain archived. |
| `NOT_RUN` | No evidence folder for that phase. No success is inferred. |

For V2, use this explicit expected failure:

```json
"qualify": {
  "status": "COMPILE_FAILED_BEFORE_TESTS",
  "command": "native-pins-default",
  "required_output": ["undefined: windows.HANDLE_FLAG_PROTECT_FROM_CLOSE"]
}
```

The example contains the currently available manifest identities and expected final phase policy. It is **not an execution result** and does not admit a pending phase. The direct-seed `invalid-fixtures` scope retains its executed comparison phases while its unexecuted `build-vet` remains `NOT_RUN`; the smaller `invalid-oracle` candidate has its own manifest and phases. Add any further scope only after its exact manifest and intended outcomes are available. Every manifest phase must be represented; extra or missing evidence folders fail verification.

The collector traverses only `evidence/`, `scopes/` and optional `format/`. Explicit root support files come from `files`. It reads the frozen owner only at `D:\dev\ChatGPT\mcp-r17-current-task-recovery-20261121\check_recovery.py`, pinned to `6f69fc636f8ec47b725f07238f7ea2180cee9fb8efbd8e8a7275f4665ea87e61`.

Original source reads are derived solely from a pinned manifest's `files` and `source_evidence`: direct `source-prN/<explicit Go path>` reads, archived under `originals/source-prN/`. Each preimage is checked against its original Git blob; source evidence also has an explicit SHA-256. A `null` original Git blob requires the corresponding original path to remain absent. No source checkout is recursively copied and no Git command is run.

## Root execution

After root reviews and places the exact scripts and final index at the fixed R25 root, use the already available Python interpreter with bytecode writing disabled:

```text
python -B package_evidence.py --index-sha256 <FINAL_INDEX_SHA256>
```

There is no wait/retry loop. A missing receipt or mismatch stops packaging. The collector creates a new `publication/package-<UTC timestamp>-<unique id>/` directory only after validating both raw and redacted evidence interpretations. It writes:

- `raw.zip`: private original bytes; retain on D:.
- `public.zip`: the public counterpart with only the two exact R24 profile-literal substitutions.
- `manifest.public.json`: every input's original/public byte count and digest, changed flag and per-literal replacement counts.
- `summary.public.json`: reconstructed phase outcomes and separate root, subtest and package terminal-event counts.
- `archives.json`: written last, after ZIP/readback checks and a second complete read of every input. It records output byte counts/digests and marks `raw.zip` private.

The independent public check is:

```text
python -B verify_public_archive.py public.zip --index-sha256 <FINAL_INDEX_SHA256> --expected-sha256 <PUBLIC_ZIP_SHA256>
```

The archive digest is optional in the verifier interface; the independently obtained index digest is mandatory. Obtain these pins outside the ZIP. A self-contained archive and file map can be replaced together; internal consistency alone does not authenticate their origin.

## Result interpretation

The generated summary reports `evidence_consistency: PASS` only after the described checks. Its qualification label remains `BOUNDED_RESULTS_ONLY`. Historical compile failure remains the phase and command outcome `COMPILE_FAILED_BEFORE_TESTS`; preparation PASS never becomes a runtime PASS for V1. Unexecuted commands remain `NOT_RUN`.

Counts distinguish `root_*_events`, `subtest_*_events`, and `package_*_events` for PASS/FAIL/SKIP. `build_fail_events` is separate. Repeated roots across independent commands are repeated events; the summary does not call their sum unique test roots. A compile-failed `go test` is a test-command attempt with zero root/subtest events.

For every unchanged public stdout/stderr file, the verifier recomputes the bytes and digest against the original capture. For a changed public file it recomputes the public digest and verifies that the original digest claim matches the receipt, while explicitly retaining the limitation that the raw bytes and exact redaction transformation cannot be recovered from the public ZIP. The same distinction applies to changed source/owner files. The pinned index, scope manifests and overlay authority metadata must be unredacted.

## Boundaries and retention

Symlinks and Windows reparse paths are refused. Cross-API reads compare device, file identity, size, modification time, birth time and attributes. `ctime` is also compared strictly within the path API's before/after pair and within the descriptor API's opened/after pair. Both full signatures are retained for the final complete reread of every input before the completion marker. This accommodates the observed Windows Python representation difference without dropping either API's change-time observation. These are stable capture checks under the owned workspace, not a continuously held cross-file lock. Unique and case-insensitively distinct ZIP member names, regular files, exact membership, CRC and readback hashes are checked. Compressed input ZIPs and total expanded members are bounded at 512 MiB; each member is bounded at 128 MiB and the member count at 50,000.

Neither program traverses `runtime/`, caches, trash, another source tree or other roots. Tool hashes inside original receipts remain historical observations; no installed tool is reread. Original retained CMake fixture trees, installed Product state and external Go fixtures are not copied. This package does not requalify those paths, full test gates or whole-process-tree settlement.

All inputs and existing publication directories remain untouched. A new partial publication is retained without a completion marker if a check fails. It is not silently deleted, moved into trash or presented as complete. Root can later classify it under the project's trash-first policy; unique required evidence and recovery copies remain outside trash.
