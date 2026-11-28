# PR627: isolated Windows reader qualification

## Contents

- [Observed result](#observed-result)
- [Inputs and scope](#inputs-and-scope)
- [Evidence and verification](#evidence-and-verification)
- [Limits](#limits)

## Observed result

Exact commit **`b827398ef127a1556b7b2bdcdcec7fe03fa6651d`** passed the existing 15 library tests on Windows x64, serially, on 2026-11-21. No source patch was applied. The formatter check passed. This qualifies the `experiments/rust-anchored-read` experiment at this commit, within the limits below.

**Table 1. Recorded phases.** Durations are command wall time except preparation, which uses the complete phase timestamps.

| Phase | UTC interval | Outcome | Duration |
|---|---|---|---:|
| Preparation | 12:54:24.269976–12:54:24.967232 | Completed; 13 cached archives copied and verified | 0.697 s |
| Formatter | 12:54:56.914878–12:54:56.996986 | Exit 0; empty standard output/error | 0.082 s |
| Library tests | 12:55:13.437087–12:55:15.298630 | Exit 0; **15 passed, 0 failed/ignored/measured/filtered** | 1.862 s |

The test executable reports 0.02 s for the tests; Cargo reports 1.76 s for the build. Table 1 includes process startup/settlement and therefore uses a different interval. Evidence: `preparation/receipt.json`, `runs/format-v1/receipt.json`, and `runs/native-debug-v1/{receipt.json,stdout.log,stderr.log}`.

The recorded commands use existing direct tool binaries:

```text
rustfmt --check --edition 2021 src/lib.rs src/windows.rs src/read_tests.rs
cargo test --offline --locked --lib --target x86_64-pc-windows-msvc -- --test-threads=1
```

## Inputs and scope

The reviewed driver and exact seven-file source export ran under `D:\dev\ChatGPT\mcp-r25-reader-20261121`. The source totals 39,826 bytes. `manifest.json` records each file's SHA-256 and Git blob identifier, the commit/tree, the selected tools, and all dependency pins. Its `phase_status: NOT_RUN` fields are the original frozen plan, preserved unchanged; the later receipts record completed execution.

Thirteen existing cached crate archives matched every registry entry in `Cargo.lock`: **3,598,476 archive bytes**, expanded into **797 regular files / 24,348,685 bytes**, plus thirteen generated `.cargo-checksum.json` files. These are preparation/manifest claims about dependency inputs. The public package includes the thirteen checksum manifests, not the archives or expanded files. The frozen driver rechecked actual vendor files before and after the commands. The receipts also record equality of all seven source hashes and eleven selected tool/input hashes before and after each phase.

Only existing inputs were read from the installed cache. Copying, extraction, build output, Cargo/Rustup homes, temporary/profile directories, and fixtures were assigned to the isolated D directory. No dependency download, installation, elevation, or policy change was requested or performed by this workflow. Existing symlink creation capability sufficed. Fixture files and symlink targets remained under the owned fixture directory and were retained for inspection. Parallel Rust work was not modified.

The tests cover exact verified payloads, empty/Unicode files, invalid input, cancellation, checksum and size errors, growth at the size limit, reparse rejection, retained parent/final handles after path changes, selected native-open errors, and repeated handle-count checks. The test named `injected_native_phase_error_preserves_category_and_closes_handles` injects an error through a hook after acquiring the root; it does not force an actual operating-system stat/read/close failure. See the seven source files and the exact fifteen terminal results in `stdout.log`.

## Evidence and verification

The original payload ZIP remains in `private-input/reader627-payload.zip`, SHA-256 `b6160593fafe25adf04615b2c00e4e4db2db3c7731ce5a558b1f33c23d381422`. Only its now-unused transport Base64 file was moved to the owned trash directory. Source, fixtures, archives, vendor files, receipts, and logs remain outside trash.

The evidence packager creates `evidence-v1/private-originals.zip`, `public.zip`, `public-index.json`, then a final `completion.json`. The private ZIP retains the selected original bytes on D. Public contents use exactly two literal replacements: the plain and JSON-escaped user-profile prefix become `<USER_PROFILE>`. All other selected input bytes remain unchanged. The public index records raw/public SHA-256 values, byte sizes, and replacement counts. The source files, driver, verifier, checksum manifests, preparation receipt, and all standard output/error logs must remain byte-identical.

The original bytes of **`manifest.json`, `runs/format-v1/receipt.json`, and `runs/native-debug-v1/receipt.json`** cannot be recomputed or independently checked against their raw SHA-256 values using only the redacted public copies. Their original hashes remain recorded identities; actual originals are retained privately. In addition, the public package omits dependency archive/vendor bytes, installed tools, fixtures, placement metadata, remote inventories, device/account identifiers, and private readbacks. Checksums for omitted objects are claims, not publicly recomputed measurements.

After obtaining the externally published ZIP and index SHA-256 values, use Python 3.11 or newer:

```text
python -I -B reader627_evidence.py verify public.zip public-index.json ZIP_SHA256 INDEX_SHA256
```

Verify mode reads only explicitly supplied files and its own script. It checks both external pins, all 31 ZIP member hashes and cyclic redundancy checks (CRC), safe member names, source SHA-256/Git blobs, the complete thirteen-package lock, checksum-manifest hashes and 797 internal-file hash entries, receipt/source/tool equality, exact commands and scope, and the exact fifteen successful terminal results plus the zero-failure summary. It neither extracts nor executes archive content. It does not download or run Rust. The verifier's summary labels its execution/tool/dependency-byte authentication limits explicitly.

## Limits

The public verifier establishes integrity and internal consistency of supplied evidence. It does **not authenticate that execution occurred**, identify the machine, recompute omitted dependency/tool bytes, or prove the provenance of the declared Git commit from a repository object graph. The selected eleven tool/input pins are not a complete compiler/Windows Software Development Kit (SDK) file closure.

The driver retained and waited for each direct child. Its deadline is 600 seconds; none was reached. It does not provide whole-process-tree deadline enforcement or settlement proof. This result does not qualify the separate Rust process-owner lifecycle (`WaitForExit`/`CloseExited`), a foreign-function interface (FFI), a Go consumer, Product/Serena installation or admission, release/all-target/dependency tests, or unexercised real post-acquisition stat/read/close failure paths. No additional coverage is inferred from the successful names alone.
