# Product executable inventory qualification — 22 November 2026

## Contents

1. [Problem and change](#problem-and-change)
2. [Exact sources](#exact-sources)
3. [Observed results](#observed-results)
4. [Review and remaining boundaries](#review-and-remaining-boundaries)
5. [Evidence and references](#evidence-and-references)

## Problem and change

The command-test Product branch #657 still compared absolute executable paths with a culture-sensitive, case-sensitive PowerShell operator. A different spelling of an ancestor directory could reject the correct launcher/Product pair. A culture-ignorable character inside a member name could also evade that comparison. Both outcomes were reproduced by the controlled provider-result cases below.

The production change imports the existing master correction from commit `434f27e476aa02bebdc7e73dbf6e5cd9f4aeb569`: enumerate registry-relative names, normalize directory separators to `/`, and compare the sorted, NUL-separated lists with `StringComparison.Ordinal`. Only `build.ps1` changes: five added lines and three removed lines. The existing filter remains `mcphub*.exe`; this is not an inventory of every possible executable name.

The registered paths remain `mcphub.exe` and `mcphub-product/mcphub.exe`. The former is the console launcher; the latter is the graphical Product. Fixture issuance, artifact hashes, portable validation, the command-test mode, and the pinned resource generator are unchanged. No new dependency is introduced.

## Exact sources

**Table 1 — source identities and executed builder bytes.** SHA-256 is the cryptographic content digest; Git commit identifiers bind the source history.

| Input | Commit or composition | Builder SHA-256 |
|---|---|---|
| Original #657 control | `4fa52b60b719664ede441be339e196a8b9766c50` | `9dd728889a303534e872913832ba89614cfc79b50a4eb1d864e8fe287b22ab7b` |
| Publication candidate | Original #657 plus the inventory correction | `bc55dd0815e95c6dc5a19513f07e6a8b5eab636382a16e9940e28b209dd1b8f2` |
| Current-source builder composition | master `75dbc6406123e22468f168b861d5e0622c9d5769` plus the complete #657 command-test builder delta | `8735f68771d545b814a3c609a8d019ec03adb9f0266fb1fcb3918e9029fb9579` |

The publication candidate keeps the branch's source version `0.4.60`. The composed builder keeps master's source version `0.4.65`. Their only difference is that version scalar. This does not issue or qualify a new Product of either version. The registry bytes, SHA-256 `3442d29fcc99611c299d9c9e6496ed2cfe7f9b5192f8452298fe2dd6e229cd0b`, declare schema 3 and `windows-product-identity-v1`; no version-2 identity or compiled registry-version-4 producer is enabled.

The common base is `a7d92d2852c4f6eb8afb0569ef76a545e38aacf7`. Direct byte comparison established that master changes this builder from the common base only by the inventory correction and source version. The #657 test script remains byte-identical in the composition. Reversing the inventory correction returns the exact original #657 builder.

## Observed results

The isolated Windows PowerShell **5.1.26100.10068** run completed on 22 November 2026, **17:30:58–17:31:01 Moscow time**. The harness decoded the unchanged repository bytes explicitly as UTF-8, parsed them into an abstract syntax tree, and executed only the two inventory assignments and their immediate rejection condition. Its `Get-ChildItem` result was deliberately controlled; the other builder statements were not executed.

**Table 2 — actual process and case outcomes.** PASS means the observed acceptance/rejection matched the case; FAIL remains a failure even when it is an expected negative control.

| Input | Process exit | PASS | FAIL | Failed cases |
|---|---:|---:|---:|---|
| Original control | 1 | 9 | 2 | `ancestor-case-only`, `product-ignorable-character` |
| Publication candidate | 0 | 11 | 0 | None |
| Current-source builder composition | 0 | 11 | 0 | None |

The eleven cases cover the exact set, changed ancestor casing, reversed enumeration, missing launcher, missing Product, extra root and nested executables, wrong launcher and Product case, wrong Product-directory case, and an inserted soft hyphen. All three case runs have empty standard error. Every direct PowerShell child was waited to completion. The source digests, PowerShell image digest and execution-policy observations matched before and after execution.

### Preserved initial failure

The first harness used `Parser.ParseFile`. On this host, Windows PowerShell 5.1 interpreted the UTF-8 builder through Windows-1251; its em dash became a parsing problem before the inventory cases. The initial process exited 1 with no case events and a parser diagnostic. This is preserved as a **harness failure**, not counted as the regression control in Table 2.

Only the harness's decoding/parsing entry changed: strict UTF-8 `ReadAllText`, followed by `Parser.ParseInput`. No builder bytes changed to make the test pass. The original harness and its failed receipt remain in the archive. The successful harness SHA-256 is `993c1e62444d14f7b5c9015d307113ce0a8df19ee267ac4a29260f2d88df3e03`.

The initial shell also inherited PowerShell-7 module paths while executing PowerShell 5.1. Its security-module import failed because of duplicate type data. The isolated child used its own existing `$PSHOME\Modules`; no module was installed or modified. The effective execution policy was already `Unrestricted`, with every other reported scope `Undefined`, and remained identical afterwards. No execution-policy override was passed. A PowerShell-7 alias probe was rejected by the remote tool as unavailable; no PowerShell-7 qualification is claimed.

## Review and remaining boundaries

Own review checked the exact production delta, registry and version invariants, source composition, error preservation, qualification boundary and raw case observations. An independent source review found no blocking defect and verified all 24 prepared manifest entries before execution. The explicit UTF-8 harness adjustment and all final receipts were additionally reviewed by the publishing owner.

This result establishes the inventory comparison behavior for the named inputs and controlled provider results. It does **not** establish native filesystem enumeration, native Portable Executable metadata, full builder execution under PowerShell 5.1, a newly issued Product fixture, installation, or a real process lifecycle. The initial UTF-8 parsing observation remains relevant to any later decision to support direct whole-builder execution on that shell.

The current-source journal, backend-hold and upgrade-abort consumers still need an admissible source-compatible native command-test Product. The historical `0.4.60` fixture does not qualify master `0.4.65`. Serena's publisher and end-to-end installation/Ready/settlement requirements are separate. Full default and tagged Go suites and external review of the newly published head remain open. This PR stays Draft.

## Evidence and references

[results.json](results.json) records the executed source pins, outcomes and archive identity. [raw-evidence.zip](raw-evidence.zip) contains **25 regular files**: 24 indexed files plus `inventory.json`. It retains the three builder inputs, registry, both harness revisions, both runners, initial failed execution, successful controls/candidates and policy observations. Every indexed length and SHA-256 digest was verified after retrieval.

Archive SHA-256: `1e874e5bc0914394848850ddec10afb6cf4ac585265a77a890f0f26a6d232174` (44,134 bytes). Its `evidence-utf8/receipt.json` is the completed execution receipt. Command output is raw and distinguishes process status from case outcomes.

1. [Original #657 source](https://github.com/applicate2628/mcp-local-hub/tree/4fa52b60b719664ede441be339e196a8b9766c50).
2. [Existing inventory correction](https://github.com/applicate2628/mcp-local-hub/commit/434f27e476aa02bebdc7e73dbf6e5cd9f4aeb569).
3. [Current master source](https://github.com/applicate2628/mcp-local-hub/tree/75dbc6406123e22468f168b861d5e0622c9d5769).
4. [Microsoft: Get-ChildItem, PowerShell 5.1](https://learn.microsoft.com/en-us/powershell/module/microsoft.powershell.management/get-childitem?view=powershell-5.1).
5. [Microsoft: StringComparison](https://learn.microsoft.com/en-us/dotnet/api/system.stringcomparison).
