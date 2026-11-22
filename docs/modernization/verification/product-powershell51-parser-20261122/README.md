# Windows PowerShell 5.1 Product script parsing — 22 November 2026

## Contents

1. [Defect and minimal correction](#defect-and-minimal-correction)
2. [Exact sources](#exact-sources)
3. [Observed native parser results](#observed-native-parser-results)
4. [Preserved launcher failure](#preserved-launcher-failure)
5. [Evidence and remaining boundaries](#evidence-and-remaining-boundaries)

## Defect and minimal correction

The published [builder at `477f6e3f66eca482e2267351af3535c62db8ebb2`](https://github.com/applicate2628/mcp-local-hub/blob/477f6e3f66eca482e2267351af3535c62db8ebb2/build.ps1) declares `#Requires -Version 5.1`. Its only non-ASCII character is U+2014, an em dash in the missing-output diagnostic on line 617. The source is UTF-8 without a byte-order mark (BOM).

Windows PowerShell reads BOM-less script files using its legacy system code page. On the observed code-page-1251 host, the em dash's UTF-8 bytes `E2 80 94` decode to `U+0432 U+0402 U+201D`. The final character is a smart quotation mark, which PowerShell treats as a quote. This breaks parsing before any builder statement executes. Microsoft's primary documentation describes [the minimum-version declaration](https://learn.microsoft.com/en-us/powershell/module/microsoft.powershell.core/about/about_requires?view=powershell-5.1), [script encoding](https://learn.microsoft.com/en-us/powershell/module/microsoft.powershell.core/about/about_character_encoding?view=powershell-5.1), and [smart-quote handling](https://learn.microsoft.com/en-us/powershell/module/microsoft.powershell.core/about/about_quoting_rules?view=powershell-5.1).

The production correction replaces that one em dash with the ASCII hyphen `-`. The diff adds one line and removes one line; every other byte is preserved. Both corrected builders are entirely ASCII. The registry-relative inventory guard remains byte-identical, SHA-256 `9db765887cb33f71ff0b146d684068b0f68b110c2e33ee0f937ad011897257a5`.

## Exact sources

**Table 1 — inputs parsed by the native engine.** SHA-256 identifies the exact file bytes.

| Input | Source and version | Builder SHA-256 |
|---|---|---|
| Published control | PR #657 at `477f6e3f66eca482e2267351af3535c62db8ebb2`; version 0.4.60 | `bc55dd0815e95c6dc5a19513f07e6a8b5eab636382a16e9940e28b209dd1b8f2` |
| Publication candidate | Published control plus the single-character correction; version 0.4.60 | `b52b2676ce02d61d6549091c224dfb65c7bbf747ee1698094a3e1a266c474d90` |
| Current-source builder composition | master `75dbc6406123e22468f168b861d5e0622c9d5769` plus the complete #657 command-test builder delta and this correction; version 0.4.65 | `e811e4549672ec4cc3ea72e59b3fa6f0235191137e41007a3600edf0f6354fa8` |

The two corrected builders differ only in their existing source-version scalar. No version was changed to imply qualification of a new Product. Reversing the punctuation correction restores each respective input exactly. The published control's Git blob is `b624db22af0582295081b483234914b484ca5add`; the candidate's is `0027986c32d8266f626f633c9e7776661570d75d`.

## Observed native parser results

The execution ran on **Windows PowerShell Desktop 5.1.26100.10068**, native default code page **1251**, culture **ru-RU**, on 22 November 2026, **17:52:35–17:52:38 Moscow time (UTC+03:00)**. The ASCII-only harness called `System.Management.Automation.Language.Parser.ParseFile` directly. It did not force source decoding and did not invoke the parsed script. UTF-8 was selected only for the harness's structured output.

**Table 2 — actual process and parser outcomes.**

| Input | Process exit | Parser errors | Meaning |
|---|---:|---:|---|
| Published control | 1 | 3 | Genuine native parse failure, retained as the negative control |
| Publication candidate | 0 | 0 | Native file parsing succeeded |
| Current-source builder composition | 0 | 0 | Native file parsing succeeded |

The control reported `TerminatorExpectedAtEndOfString` once and `MissingEndCurlyBrace` twice. The recorded extents begin on lines 630, 616 and 614 respectively; these are parser diagnostics following the malformed quoting, not three independent defects. Full identifiers, messages and extents are preserved in the raw output and [results.json](results.json).

The three source files and harness matched their admitted hashes before and after execution. The native PowerShell image hash remained `95ce2acbc6cd9d7f4a22759d39a8d40fc2a53ceeecf068ef856849f0d2076961`. Execution policy was already `Unrestricted` in CurrentUser, with all other reported scopes `Undefined`; the complete before/after observations matched. No execution-policy override or mutation was used. Existing native PowerShell modules were selected through `PSMODULEPATH`; temporary, profile and module-analysis paths were inside the new owned D-drive directory.

All **five direct PowerShell children**—two policy observations and three parser checks—were waited to completion. Standard error was empty for every child, and no timeout occurred. The runner completed successfully because the control's exit 1 and the candidates' exits 0 matched their declared distinct outcomes. It does not turn the control's parse failure into a successful parse.

## Preserved launcher failure

An earlier read-only preparation command failed in CMD because quoting joined the executable path and arguments incorrectly. Python, PowerShell and the parser harness did not start in that attempt; a subsequent filesystem observation confirmed that the new task directory did not yet exist. The coordinator authorized the corrected invocation using the known paths without unnecessary quotes. This transport failure is retained separately in `transport/launcher-failure.json`; it is not counted as the parser negative control in Table 2.

## Evidence and remaining boundaries

[raw-evidence.zip](raw-evidence.zip) contains **22 regular files: 21 indexed payload files plus `inventory.json`**. It preserves the exact three builders, ASCII parser harness, runner and exporter, all five commands' raw streams, the completed receipt, exact patch, pre-execution provenance and the earlier launcher failure. The 17 files retrieved from the device were verified byte-for-byte; four local provenance files were added without changing those raw files. Every indexed length and SHA-256 was checked.

Archive: **43,049 bytes**, SHA-256 **`464b2f1ced2a63c8d1e4b8eab9d2a9d4c844b726723352628be159b7350c9f8c`**. The completed `evidence/receipt.json` is 12,820 bytes, SHA-256 `33c550cae82a82f847181c684644b12d4d89bd258b66476cbb6dcfa66db78b52`. [results.json](results.json) records the source identities, process outcomes and claim limits. The archived `source-manifest.json` and `minimal-plan.json` retain their original pre-execution `NOT_RUN` status; the completed receipt records the later execution.

This result establishes native file parsing for these exact inputs on the recorded host. **No builder statement, Go, Node, native Product, installation or Task Scheduler operation was executed.** A new Product was not issued. The prior inventory qualification remains tied to its own source hashes; no new inventory comparison or full Go suite is claimed here. Complete operating-system process-tree settlement was not independently observed beyond waiting for the five direct children. Full builder and Product acceptance, the integrated Serena lifecycle and external review of the eventual published head remain open.
