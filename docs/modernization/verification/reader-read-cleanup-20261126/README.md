# #627: native partial-read failure with cleanup errors

The new test-only change passes on the existing Windows/amd64 toolchain:
**21 tests passed, 0 failed, 0 ignored**. It adds three combinations to the existing
failure matrix. The production reader, public API, Cargo inputs and dependencies
are unchanged.

## Contents

1. [Contract and change](#contract-and-change)
2. [Native result](#native-result)
3. [Original ownership and exact bytes](#original-ownership-and-exact-bytes)
4. [Scope and remaining work](#scope-and-remaining-work)
5. [Previous PR description](#previous-pr-description)
6. [Sources](#sources)

## Contract and change

The reviewed base is `d910767902f7e1385b97e12afdc675b0c9d27b99` [1]. Its metadata
and cleanup matrix already covers 25 subcases and has a preserved 20-test native
pass [2]. This change reuses that matrix and the existing native read-failure
fixture. Source review found no new production defect in metadata-error propagation.

The existing `LockedByte` guard is moved to the enclosing test module so both tests
can use the same fixture mechanism. Its independent file handle acquires a byte
lock at offset 4096 before the reader's second call. The original reader handle
remains intact. Windows denies the locked range to a separate handle, including
one in the same process [3]. The recorded read requests are `[4096, 2]`: the first
chunk is read before the lock, and the second call produces a genuine
`ReadFile` / `ERROR_LOCK_VIOLATION` failure.

The existing per-invocation `ReadOperations` fixture then performs real native
closes on the original handles and substitutes selected cleanup status values.
It observes acquired/closed handles and checks that every acquisition is consumed
once, with at most two live reader handles. No new fault framework is introduced.

**Table 1. New subcases, all passed.**

| Cleanup status after the genuine read failure | Required result |
|---|---|
| Final file only | Primary `UnsafeObjectOrIo / read-final / Win32(ERROR_LOCK_VIOLATION)`; final cleanup code |
| Retained parent only | Same primary; parent cleanup code |
| Both | Same primary; cleanup codes in final → parent order |

Every subcase returns an error, so the partial byte buffer is not returned as a
successful result. The independently owned fixture lock is released before result
assertions. The existing genuine-read-error test remains the zero-cleanup-error
control.

## Native result

Execution used an isolated copy of the eight crate files under
`D:\dev\ChatGPT\functional-route-20261126\composition-p02\.scratch\reader627-review-v1`.
The containing checkout was `b6f82b3f531bb3eec197049167cdc5971b44e2b0`; this is the
coordinator's checkout identity, not a claim that the Rust delta was committed
there. Only the copied crate and its private verification inputs were written.

**Table 2. Commands and observed results.**

| Command | Result | Command seconds |
|---|---|---:|
| `rustfmt --edition 2021` on the four Rust files | PASS; no byte changes | 0.094 |
| `rustfmt --check --edition 2021` on the same files | PASS | 0.082 |
| `cargo check --offline --locked --all-targets --target x86_64-pc-windows-msvc` | PASS | 17.610 |
| `cargo test --offline --locked --lib --target x86_64-pc-windows-msvc -- --test-threads=1` | **21 PASS / 0 FAIL / 0 ignored** | 38.024 |

Cargo received an explicit manifest path to the copied crate. The test command's
time includes compilation; libtest itself reported 0.35 s. The whole coordinator
run took 61.191 s, completing on **26 ноября 2026 в 14:18:05 МСК (UTC+03:00)**. The exact 21 test names,
source hashes, executable pins and command arguments are recorded in
[evidence.json](evidence.json). Original test output is
[rust-test.stdout.txt](rust-test.stdout.txt).

The same installed Rust/MSVC/SDK files were freshly hashed against the preserved
pins. The 13 dependency packages came from the previously closed successful
`mcp-reader-matrix-v3-20261125-5bc9fbb9` vendor directory. Its package manifests and
all listed contents were verified before reuse. Vendor access was read-only;
the new Cargo home, build target, temporary files and fixtures were under the new
D: scope. No download, installation, VM or WSL instance was created.

## Original ownership and exact bytes

The run reused the existing `run_wave_commands.py` coordinator, SHA-256
`0997ef9baef811f0748fe764488a23ff556f75cc36ca43dae3523ceb8aa91bbd`, and ordinary
original Job guard, SHA-256
`56d6ccee190472cbdacd5f0ec5f7636df3487f27ec208e0776c20c12a22b72f2`.
The data-only [native_snapshot.py.txt](native_snapshot.py.txt) admits existing
inputs and records source bytes; it starts no process and is not another runner.

All seven commands, including checkout identity, admission and final snapshot,
report the original process reaped, original Job empty, native handles closed,
original readers joined, stream EOF and pipe/capture close returned. None timed
out or terminated its Job. The parent reports its original controller reaped and
both controller capture handles closed. Both controller captures were empty.

**Table 3. Original evidence files.**

| File | SHA-256 |
|---|---|
| [receipt.json](receipt.json) | `ce8222af795db9ee9c6ac9102d59ba47a46dbf99c27cd636d770f68efcbdb40f` |
| [root-receiving.json](root-receiving.json) | `29be556e00d635972f65b17a1ede6057ec0869ea3157c876334ddf9a3e49788c` |
| [admission.json](admission.json) | `8b22370dd5418fe929dd59c25243be220fe0c3e676944113eb86e00b36366349` |
| [source-after.json](source-after.json) | `535861d4f9c6833458ae78e1f07fd03b168df8b3a07d76c52d2363ae1a0f02d8` |

The original coordinator receipts retain CRLF bytes. The scoped `.gitattributes`
marks only `receipt.json` and `root-receiving.json` as `-text` to preserve those
exact bytes in Git. Original UTC timestamps remain in machine-readable evidence.
Readback was accepted only
after matching their native SHA-256. Captures were read after original closure
and matched both native hashes and byte counts. All eight post-run source files
match the candidate; the formatter changed none of them. Only `read_tests.rs`
and `failure_tests.rs` differ from the PR base.

Completed fixture data were moved to the user's trash after verification. Source,
captures, receipts and the build cache were retained; no deletion was performed.
Older held process scopes and their captures were not accessed or reclassified.

## Scope and remaining work

The read failure is genuine. The cleanup failures are substituted statuses after
successful native closes, as in the existing matrix. Consequently,
`native_metadata_or_close_failure_qualified` remains **false**. This does not
qualify a genuine failing `GetFileInformationByHandle` or `CloseHandle` call.

This isolated Rust regression does not add a Go adapter or qualify a product
consumer. Existing reader contract questions and product acceptance conditions
remain unchanged. No PR was merged or replaced as part of this check.

## Previous PR description

[previous-pr-description.md](previous-pr-description.md) preserves the complete
PR body read at head `d910767902f7e1385b97e12afdc675b0c9d27b99`, including the
20-test history, earlier native ReadFile result and unresolved acceptance conditions.

## Sources

1. [Reviewed PR base d910767](https://github.com/applicate2628/mcp-local-hub/commit/d910767902f7e1385b97e12afdc675b0c9d27b99).
2. [Existing metadata/cleanup matrix and saved native result](../reader-failure-matrix-2026-11-25/README.md).
3. [Microsoft: LockFile](https://learn.microsoft.com/en-us/windows/win32/api/fileapi/nf-fileapi-lockfile).

**Job** means the Windows process-containment object. **Reader** here means the
crate's file-reading operation, while stdout/stderr readers belong to the command
guard. **EOF** is end of stream. **SHA-256** identifies exact file bytes. **MSVC** is
the Microsoft C/C++ toolchain used by this Rust target; **SDK** is the Windows
Software Development Kit. **CAS** means compare-and-swap of the expected PR head.
