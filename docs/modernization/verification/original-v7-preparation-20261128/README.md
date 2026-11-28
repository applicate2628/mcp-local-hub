# Original 727d acceptance: closed preparation and composition

This packet preserves the closed preparation needed by the recovered `727d66753814dc218e26d699e57402db2d13190a` acceptance campaign. The private publisher source is separately bound to `3ebe335a2f666a5279d7fafc31a6f4dc3feedff4`.

The scope is **compiled inputs and DATA composition**. The full acceptance verdict is **NOT_RECEIVED in this packet**. The frozen composition receipt records its preparation-time `NOT_RUN` state. Subsequent full-run observations belong to their own original owner and receiving records.

## Contents

- [1. Closed results](#1-closed-results)
- [2. Concrete parsing defect and correction](#2-concrete-parsing-defect-and-correction)
- [3. Preserved operational inputs](#3-preserved-operational-inputs)
- [4. Exact DATA reconstruction](#4-exact-data-reconstruction)
- [5. Acceptance scope and remaining limits](#5-acceptance-scope-and-remaining-limits)
- [6. Sources](#6-sources)

## 1. Closed results

**Table 1. Original parent outcomes.** Durations are elapsed seconds from the original parent records. A preparation PASS qualifies only the work listed in the second column.

| Phase | Work | Commands | Original result | Parent seconds |
|---|---|---:|---|---:|
| Product v6 | Independent cache copy; original 19-artifact copy; private builder | 3 | FAIL, physically CLOSED | 72.6821214 |
| Private compilation v7 | Date parser probe; fresh independent cache copy; fresh private issuance and two compilations | 3 | PASS, physically CLOSED | 568.6791257 |
| Runtime composition v6 | Verify explicit inputs and render the reviewed literal runtime | 1 | PASS, physically CLOSED | 1.4208165 |

All seven command records have original native completion, joined stdout/stderr readers, closed capture streams and received direct exit. No command terminated its Windows Job. Input maps before and after every command equal the corresponding preserved plan: 3 × 3,544 entries in Product v6, 3 × 3,562 in private v7, and 1 × 107 in composition.

[closed-phase-observations.json](closed-phase-observations.json) contains the command-level results, original parent and execution digests, capture digests and bounds. Original parent bytes, outer completion records, all 14 command capture files, and lossless execution projections are included in [source-bindings.json](source-bindings.json). Standard output and standard error are separate files, including empty captures.

Product v6's original FAIL is preserved. Its first two commands completed successfully. The third command stopped before creating the fresh private source export, compiler images or migration evidence parents.

## 2. Concrete parsing defect and correction

The v6 configuration contained the ISO timestamp `2026-11-28T11:11:34Z`. Under the qualified PowerShell 7.7.0 runtime, the default `ConvertFrom-Json -AsHashtable` conversion produced `System.DateTime`. Its comparison display was `11/28/2026 11:11:34`, so the builder's explicit ISO-text regular expression rejected it.

The v7 DATA-only probe reproduces both sides using the same timestamp:

| Parser mode | Observed type | ISO-text predicate |
|---|---|---|
| Default | `System.DateTime` | false |
| `-DateKind String` | `System.String` | true; exact original literal retained |

The new builder differs from the preserved v6 builder in exactly two lines:

```diff
-#Requires -Version 7.0
+#Requires -Version 7.5
-$cfg=[IO.File]::ReadAllText($BindingsPath) | ConvertFrom-Json -AsHashtable
+$cfg=[IO.File]::ReadAllText($BindingsPath) | ConvertFrom-Json -AsHashtable -DateKind String
```

Microsoft documents `DateKind` from PowerShell 7.5 and the `String` value's retention of timestamp text [4]. The probe itself is an observed local result [1], with no source/cache access or child processes.

The v7 phase used a new output directory and a new independent Go cache copy. It reused the already closed 19-artifact copy through its actual file hashes and original receipt. The private builder then used the still-absent source/image destinations within the original Product v6 input root. The old v6 script, configuration, parent, captures and FAIL were preserved.

## 3. Preserved operational inputs

The 51 frozen files listed in [source-bindings.json](source-bindings.json) include:

- The actual final binding JSON with original SHA256 `b5a15b47e09a4c3476205778b85c1d83a22275ddf664cf0bdcde377e896136ce`, and its sorted runtime configuration with SHA256 `037d4ad93bc93dcf531669eaaa90be32154c1ba34af6f50528eaa03e9f9ef4a2`.
- The concrete Product v6, private v7 and runtime composition plans; both private build configurations; the rendered runtime manifest and preparation record.
- Six root-owned outer PowerShell wrappers for helper, Product v6, private v7, external helper, runtime composition and the full literal launcher.
- The actual private issuer and migration input JSON for both modes, their Go build metadata, private evidence-parent creation records, and the compiled-input receiving record.
- Original closed parents, outer completion records, captures, artifact-copy and cache-copy receipts.

Paths and the original operator SID are represented by explicit named role markers. The public renderer has no embedded role values or fallback values. Its code is the [source capsule renderer](../original-v6-sources-20261128/reconstruct_sources.py) with only the frozen `BINDINGS_SHA256` value changed for this packet. Each substitution and occurrence count is visible in the bindings manifest.

The separately preserved [source capsule](../original-v6-sources-20261128/) contains the v6/v7 builders, date probe, nine runtime templates, private graph receiver and existing ownership machinery. Its [template manifest](../original-v6-sources-20261128/metadata/templates-manifest.json) fixes the nine template inputs and original provenance. Those sources are shared dependencies and are not duplicated here.

The two compiled private images are input artifacts for the original migration child protocol. Their receipt is `COMPILED_INPUTS_ONLY`, with `source_restored=true` and `tests_executed=false`. Their hashes are:

| Mode | Private image SHA256 |
|---|---|
| Default | `5c5cfc94af370194bb047a433912b4cda50bcd4827633aa4c5185d6736364e7c` |
| `test_state_path_env` | `0bd8fd3bc4deeadec72a6659056d23d4f47928214456ed6637a62731ee3d8911` |

These separate images do not replace the incoming Go test executable used by the literal full commands.

## 4. Exact DATA reconstruction

The reconstruction programs only read, verify and write files. They do not import preserved source code, acquire native process ownership, run tests, invoke Git, use the network or change any gate status.

1. Supply an operator-owned JSON containing exactly the role keys in `source-bindings.json`. The roles describe the original work area, installations, artifact area and operator SID. Original values are intentionally absent from this public packet.
2. Run `reconstruct_sources.py --roles-json OPERATOR_ROLES.json --output NEW_DATA_DIRECTORY`. It verifies all published bytes and role occurrence counts, reconstructs all 51 original byte sequences, and requires each complete original length and SHA256 before creating the new output.
3. Run `restore_execution_metadata.py --data-root NEW_DATA_DIRECTORY --output NEW_EXECUTION_DIRECTORY --index-sha256 INDEX_SHA256`, using the index digest from this packet's manifest. It restores each repeated input map from the exact reconstructed plan, then verifies the complete original execution length and SHA256 before writing it.

The execution projections preserve every unique command, native, IO, join, timing, error, FAIL and gate field. Only repeated `step_inputs[*].before` and `after` maps are replaced with an explicit reference to `original_plan.input_files`. The original map order matches the plan order. Serialization is the original UTF-8 `json.dumps(..., ensure_ascii=False, indent=2)` plus a final newline.

All three complete execution files reconstruct byte-for-byte, including Product v6's FAIL. Their original SHA256 values are:

| Phase | Complete original execution SHA256 |
|---|---|
| Product v6 | `0d62de08b4493ffc1a707f939f85355fffb1b08d03bf690439263d5022b04ca6` |
| Private v7 | `b1c4a3d3f5e4ca1a65227f05bb648487cad8dfd43af49927e1cd3e6d1f0e780a` |
| Runtime composition | `eac444852333c7b2891fe2cd856098df14398291b45d755636b4f99f3b1fbd39` |

[data-reconstruction-proof.json](data-reconstruction-proof.json) records the verified round trips. The original private role file is kept outside the publication packet. Reconstructed receipts remain historical DATA; recreating their bytes transfers no process, filesystem or cache authority.

## 5. Acceptance scope and remaining limits

The reviewed full configuration preserves these literal commands and their five-minute test timeouts:

```text
go build ./... && go vet ./... && go test -count=1 -timeout 5m ./...
go test -tags=test_state_path_env -count=1 -timeout 5m ./internal/api/ ./internal/cli/
```

The mandatory receiving inventory is 14 migration roots, 15 preparation records per mode, two canonical roots and the original three API stderr rotation roots. Three CLI stderr scenarios remain an additional explicit set. All names and predicates remain in the linked frozen sources.

The actual incoming Go test image is qualified through the existing compiler resource and companion-bundle route. The complete original source maps and separate private publisher issuance remain bound to their actual commits and directories. The independent Go cache copy uses the received current donor; no authority is inherited from a relocated historical cache.

The external canonical producer is the exact original ext17 generation. It supports its original normal receiving and retained UNKNOWN behavior. The appended receiver's later failed-physical-closure capability is unavailable from that producer; this preparation does not grant it.

These preparation results do not qualify a public PR head, the restored `33eaf8` changes, any concurrent A–D composition, or either full literal command. Final full-run receiving is a separate result.

## 6. Sources

1. [Original parser probe bytes](sources/original-private-compile-v7/date-parser-probe.json), preserved with their original digest in `source-bindings.json`.
2. [Closed phase observations](closed-phase-observations.json), [lossless execution reconstruction index](execution-reconstruction-index.json), and [DATA reconstruction proof](data-reconstruction-proof.json).
3. [Frozen source capsule](../original-v6-sources-20261128/) and [its template manifest](../original-v6-sources-20261128/metadata/templates-manifest.json).
4. Microsoft Learn, [ConvertFrom-Json: DateKind and timestamp conversion](https://learn.microsoft.com/en-us/powershell/module/microsoft.powershell.utility/convertfrom-json?view=powershell-7.6#-datekind), checked 2026-11-28.
