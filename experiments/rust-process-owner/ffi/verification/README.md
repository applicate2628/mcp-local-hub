# Verification evidence

The existing PR #631 preserves its complete earlier evidence in [history.zip](legacy/history.zip). The archive contains **all 624 tracked files** under this directory at immutable source commit `eb4ca1d40244ba1915638b76c3e30b2a684d9b07`, with their original repository-relative paths and exact committed bytes. Packaging these records is not a new test run or renewed acceptance.

## Contents

1. [Current qualification](#current-qualification)
2. [Historical milestones](#historical-milestones)
3. [Archive identity and recovery](#archive-identity-and-recovery)
4. [Meaning and limits](#meaning-and-limits)

## Current qualification

[Versioned identity observations, 15 November 2026](identity-2026-11-15/RESULTS.md) records the current source identity, commands, measured outcomes, negative controls and remaining product-integration limits. The canonical residual plan remains [the six-stage execution plan](https://github.com/applicate2628/mcp-local-hub/pull/623). This directory is evidence, not a second work queue.

## Historical milestones

**Table 1. Immutable reports and reproduction records.**

| Milestone | Preserved report/index |
|---|---|
| Initial native streams, context, creator crash and native failures; first Go/Rust calls and checked admission | [Original bundle index](https://github.com/applicate2628/mcp-local-hub/blob/eb4ca1d40244ba1915638b76c3e30b2a684d9b07/experiments/rust-process-owner/ffi/verification/history/index.json) and [original evidence README](https://github.com/applicate2628/mcp-local-hub/blob/eb4ca1d40244ba1915638b76c3e30b2a684d9b07/experiments/rust-process-owner/ffi/verification/README.md) |
| Explicit child environment | [Contract, results and limits](https://github.com/applicate2628/mcp-local-hub/blob/eb4ca1d40244ba1915638b76c3e30b2a684d9b07/experiments/rust-process-owner/ffi/verification/environment-2026-11-14/RESULTS.md) |
| Explicit standard-stream calling boundary | [Results](https://github.com/applicate2628/mcp-local-hub/blob/eb4ca1d40244ba1915638b76c3e30b2a684d9b07/experiments/rust-process-owner/ffi/verification/stdio-2026-11-14/RESULTS.md), [negative controls](https://github.com/applicate2628/mcp-local-hub/blob/eb4ca1d40244ba1915638b76c3e30b2a684d9b07/experiments/rust-process-owner/ffi/verification/stdio-2026-11-14/MUTATIONS.md) |
| Context-aware observation and pipe completion | [Results](https://github.com/applicate2628/mcp-local-hub/blob/eb4ca1d40244ba1915638b76c3e30b2a684d9b07/experiments/rust-process-owner/ffi/verification/cancellation-2026-11-14/RESULTS.md), [negative controls](https://github.com/applicate2628/mcp-local-hub/blob/eb4ca1d40244ba1915638b76c3e30b2a684d9b07/experiments/rust-process-owner/ffi/verification/cancellation-2026-11-14/MUTATIONS.md) |
| Actual native cleanup failures across the foreign interface | [Results](https://github.com/applicate2628/mcp-local-hub/blob/eb4ca1d40244ba1915638b76c3e30b2a684d9b07/experiments/rust-process-owner/ffi/verification/native-cleanup-2026-11-14/RESULTS.md), [post-rebuild artifact readback](https://github.com/applicate2628/mcp-local-hub/blob/eb4ca1d40244ba1915638b76c3e30b2a684d9b07/experiments/rust-process-owner/ffi/verification/native-cleanup-2026-11-14/ARTIFACT_READBACK.md) |
| Loader refusal and module-reference lifetime | [Results and reproduction](https://github.com/applicate2628/mcp-local-hub/blob/eb4ca1d40244ba1915638b76c3e30b2a684d9b07/experiments/rust-process-owner/ffi/verification/loader-2026-11-14/RESULTS.md) |
| Shared outcome decoder validation | [Results](https://github.com/applicate2628/mcp-local-hub/blob/eb4ca1d40244ba1915638b76c3e30b2a684d9b07/experiments/rust-process-owner/ffi/verification/outcome-2026-11-14/RESULTS.md), [remaining product-admission gap](https://github.com/applicate2628/mcp-local-hub/blob/eb4ca1d40244ba1915638b76c3e30b2a684d9b07/experiments/rust-process-owner/ffi/verification/outcome-2026-11-14/ADMISSION-PARITY.md) |
| Synchronization with accepted master | [Source identities and complete experiment checks](https://github.com/applicate2628/mcp-local-hub/blob/eb4ca1d40244ba1915638b76c3e30b2a684d9b07/experiments/rust-process-owner/ffi/verification/runs/master-convergence-2026-11-14/RESULTS.md) |

The [historical progress log](https://github.com/applicate2628/mcp-local-hub/blob/eb4ca1d40244ba1915638b76c3e30b2a684d9b07/experiments/rust-process-owner/ffi/verification/PROGRESS.md) preserves the original order, failures, corrections and source identities. It is a historical record, not a second current task queue. The archive includes the complete per-run logs, manifests, scripts and checkpoints linked from these reports.

## Archive identity and recovery

**Table 2. Byte-preservation checks.**

| Property | Value |
|---|---|
| Source commit | `eb4ca1d40244ba1915638b76c3e30b2a684d9b07` |
| Source prefix | `experiments/rust-process-owner/ffi/verification/` |
| Archive | `history.zip`, 660,113 bytes |
| Archive SHA-256 | `1df805b8d50ef8e10eaa9404efbee341ba85daeea3c94e3c4a6dd7a572ee342e` |
| Verified tracked files | 624 of 624; 42 additional directory entries |
| Uncompressed file bytes | 2,364,014 |
| Identity check | Every decompressed file exactly matches its immutable Git blob identifier; its SHA-256, length, mode and path are recorded in [manifest.tsv](legacy/manifest.tsv) |
| Package checks | [receipt.json](legacy/receipt.json), [SHA256SUMS](legacy/SHA256SUMS) |

The archive was created directly with `git archive` from the immutable tree. Every entry name was checked for traversal, absolute paths, backslashes, drive/stream separators and duplicates. No active checkout content was substituted. Verification read the ZIP entries in memory; it did not extract files or change the source tree.

To recover the historical evidence, verify the archive checksum and extract into a **new owned recovery directory**. It expands with the repository prefix shown in Table 2. Relative paths between the archived evidence files are preserved. Links and scripts needing source files outside this prefix require the matching immutable repository checkout; the archive contains evidence, not the complete repository. Do not extract over a current working tree or overwrite newer reports.

The same archive content can be regenerated from an existing clone that contains the commit:

```powershell
git archive --format=zip --output='<new-owned-path>\history.zip' `
  eb4ca1d40244ba1915638b76c3e30b2a684d9b07 `
  -- experiments/rust-process-owner/ffi/verification/
```

The output path must be new. No package installation, network download or runtime execution is needed for archive creation.

## Meaning and limits

The preserved bytes are the previously **committed** evidence. Earlier source records explain any original log normalization and retain original/published hashes separately; this archive does not reconstruct unpublished unsanitized originals. Successful runs, initial setup/oracle failures and intentionally rejected implementations retain their original classifications.

These records distinguish native process settlement, root exit, token/handle cleanup, I/O completion and loader ownership. They do not establish full product admission, native Unix coverage, exhaustive failures, installed/release acceptance, a sandbox for an untrusted library or a performance improvement. Artifact and source hashes identify bytes; they do not confer process authority or prove correctness.

New work must keep its actual commands, source identity, results and limitations in the current PR before completion is reported. Required recovery material must remain available independently of a temporary machine.

**Terms:** FFI — foreign-function interface; ABI — application binary interface; I/O — input/output; SHA-256 — 256-bit Secure Hash Algorithm; Git blob — Git's file-content object, identified here by its SHA-1 hash.
