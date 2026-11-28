# RX-04/read: isolated anchored-file reader experiment

**Reviewable experiment only. Not a replacement for the Hub reader and not connected to installation, state, vault or runtime.** Implements one bounded Windows local-filesystem slice of [the MCPHub Rust work plan](https://github.com/applicate2628/mcp-local-hub/pull/623).

## Contents

- [Contract and ownership](#contract-and-ownership)
- [Important differences from the current Go API](#important-differences-from-the-current-go-api)
- [Native evidence](#native-evidence)
- [Reproduction](#reproduction)
- [Remaining work](#remaining-work)

## Contract and ownership

`read_verified(root, components, cap, expected_digest, canceled)` opens a drive-absolute root with a no-reparse NT open, then opens each component relative to the retained parent handle. The final file is read and hashed through the same handle; no pathname is reopened to obtain the returned bytes.

One acquired handle has one owner. The next parent is acquired before its predecessor is closed; at most two handles are retained at once. Final and parent handles are explicitly closed on success and error. Native close failure prevents a successful byte result and is recorded separately from an existing primary failure. A failed numeric close is not retried. Real CloseHandle failure injection is still a qualification gap, not a tested guarantee.

The data cap is positive and at most 1 MiB (`MAX_STATE_BYTES`). A stat check is not sufficient: each read requests at most the remaining capacity plus one sentinel byte. Growth above the cap fails without returning partial data. The SHA-256 digest verifies the bytes actually read, not an earlier path snapshot. It does not establish authorization or trusted provenance.

The current buffer and read chunk use the upstream `zeroize` wrapper and are discarded on failure. This does not prove erasure of all allocator history, transient copies, registers or another process's memory. Logical data length is capped; exact allocator footprint is a separate measurement.

## Important differences from the current Go API

**Source comparison completed on 2026-11-18:** Rust `2c13114eabc7ad6cab416788b6e32faaa8c99935` was compared with the accepted Go source at `a7d92d2852c4f6eb8afb0569ef76a545e38aacf7`. The [Windows beneath-root reader][go-reader], [shared error/input contract][go-errors] and [size caps][go-caps] are byte-for-byte unchanged from reviewed #616 `53645ec7170c9d56073ab5140dec865b23ce7c21`. This completes the narrow source comparison; adapter design, product parity and native qualification remain open.

**Table 1. Rust API and current Go reader contracts.** API means application programming interface; MiB means 1,048,576 bytes.

| Boundary | Rust `read_verified` | Current Go contract |
|---|---|---|
| Size and expected hash | Positive caller cap up to 1 MiB; required 32-byte SHA-256 digest | `ReadStateFileBeneathRootNoFollow`: 1 MiB and required 64-digit hexadecimal digest. `ReadClientConfigBackupBeneathRootNoFollow`: 16 MiB and an optional empty digest for an unpinned capture; persisted pins require their digest |
| Cancellation | `AtomicBool` produces the `Canceled` category without a deadline or wrapped cause | `context.Context`; `ctx.Err()` is retained as `StateFileReadError.Cause`, preserving `context.Canceled` versus `context.DeadlineExceeded` through `Unwrap`. This reader does not separately extract `context.Cause(ctx)` |
| Structured errors and absence | Category, operation, NT versus Win32 native code, and separate cleanup failures; no component or Go error projection | `StateFileReadError` also carries the component and unwraps its cause. `errors.Is(err, os.ErrNotExist)` projects recognized original native open-missing outcomes; a general I/O failure is not absence |
| Root paths | Ordinary drive-absolute Windows roots; UNC and verbatim/device roots are rejected | The Windows reader applies `filepath.Clean` and translates UNC (Universal Naming Convention) roots. Equivalent normalization and network-filesystem behavior have not been qualified for Rust |
| Separate inode/policy reader | No access-policy, audit or observation object | [`ReadStateFileInodeAnchored*`][go-inode] and [`ReadStateFileInodeAnchoredObserved`][go-observed] have additional access-policy, audit, absence and resource-observation contracts. They are separate primitives, outside the beneath-root comparison |

The differences in Table 1 affect existing callers. [`prepareOwnedBackendCompensation`][go-inverse] reads `<reference>.inverse.json` with an empty digest and creates a new compensation record only when `errors.Is(err, os.ErrNotExist)` succeeds. An adapter must preserve that distinction between absence and failure. The [command-line interface (CLI) backup-pinning path][go-pin] also reads an unpinned backup with an empty digest, then calculates the digest to retain. No adapter is implemented here.

The caller must already have permission to read the selected root. Components are single names, not arbitrary relative paths; empty names, parent traversal, slash/backslash and NUL are rejected. Cancellation is observed between native operations and after the internal test hooks; neither this Rust slice nor the compared synchronous Go path promises interruption of an already blocked `ReadFile`. There is no Go/Python foreign-function interface (FFI), writer, journal, lock manager or fallback path.

Outside Windows the function returns `Unsupported` before side effects. That is an explicit unimplemented platform, not Linux/macOS support. Native status codes preserve their NT versus Win32 origin instead of guessing absence from error text.

## Native evidence

**Current qualification — 2026-11-21:** the candidate based on `b827398ef127a1556b7b2bdcdcec7fe03fa6651d`, with the additional native-read-error test, passed **16 library tests, 0 failed/ignored/measured/filtered**, serially on Windows x64. The formatter check passed. The [execution record and source identities](../../docs/modernization/verification/reader-native-read-2026-11-21/README.md) distinguish this run from the historical 15-test evidence, retain the first failed formatting attempt, and explain the later documentation-only update. No Clippy rerun or product qualification is claimed.

**Historical evidence, not a rerun for the 2026-11-18 source comparison.** Windows 11 build 26100, x86_64, Rust 1.99.1. Dependencies were already cached and used offline: `windows-sys=0.61.2`, `sha2=0.10.9`, `zeroize=1.9.0`; lockfile included. No dependencies were installed or added to the Hub product.

**15 native tests passed without skips**, covering:

- Exact verified bytes, empty files, nested Unicode names and invalid input.
- Cancellation before open and during reading, hash mismatch and excessive size.
- Directory-as-file and missing-file native error classification.
- Reparse points at the root, a root ancestor, intermediate component and final file.
- Replacement with a symlink before open; replacement of a retained parent or final path after open still reads the original object.
- File growth after stat, one-byte over-cap detection, and exact-cap EOF.
- Preservation of injected operation failure and stable handle count over repeated successful and unsuccessful reads.

The [injected-error test](src/read_tests.rs#L278-L299) returns `Win32(123)` from the first component hook after the root handle is acquired; it does not force a native stat, read or close failure. Actual post-acquisition stat failures, parent/final close failures, and an independent primary error combined with cleanup failures remain qualification gaps. They require explicitly owned native fixtures; neither the historical 15 passes nor the current 16-test run establishes those paths.

The new `native_read_failure_after_partial_read_preserves_error_and_releases_lock` test uses the existing read hook to acquire an exclusive one-byte lock at offset 4096 through an independent fixture handle. After 4096 bytes have been read, the next native `ReadFile` fails with `ERROR_LOCK_VIOLATION` (Win32 code 33), without returning the partial buffer. The test explicitly unlocks the range, preserves the read error separately from cleanup, and requires a subsequent complete verified read. Its local fixture guard also attempts explicit unlocking during unwinding before dropping its owned `File`. [Microsoft documents the separate-handle lock behavior][win-lock] and [exact-range unlocking][win-unlock]. This schedule passed in the current 16-test run. Other read-failure modes and the remaining stat/close/combined-failure cases are not inferred from it.

The symlink tests used the host's existing capability; no privilege elevation or machine settings were performed. Test data are explicitly owned fixtures, not real Hub files or secrets.

The initial nonimplementation failed the expected basic tests. A temporary mutation removing the over-cap sentinel was detected (`[4096,0]` instead of `[4096,1]`); the original source was restored and all tests rerun. `cargo clippy --offline --locked --all-targets -- -D warnings` and `cargo fmt --all -- --check` passed.

These are tested schedules on this Windows build, not a universal filesystem/security proof. The private per-call hook exists only to reproduce deterministic replacement/growth/cancellation schedules; no global or public callback registry is introduced.

## Reproduction

Use an already present compatible toolchain and cache. A missing dependency is a local blocker, not permission to download or install it.

```powershell
$env:CARGO_NET_OFFLINE = 'true'
$env:MCPHUB_RX04_FIXTURES = Join-Path $env:TEMP ('mcphub-read-fixtures-' + [Guid]::NewGuid().ToString('N'))
New-Item -ItemType Directory -Path $env:MCPHUB_RX04_FIXTURES | Out-Null
cargo test --offline --locked -- --test-threads=1
cargo clippy --offline --locked --all-targets -- -D warnings
cargo fmt --all -- --check
```

Tests run serially for meaningful process handle counts. Each run receives a fresh owned fixture root. Fixtures, including test symlinks, are retained for review and trash-first disposition; they never point outside the owned test area and are not automatically deleted. Without existing symlink permission, record an environment failure rather than elevating or pretending the security case passed.

## Remaining work

- Product integration beyond the completed narrow source comparison: preserve caller policies, typed absence, cancellation errors, caps and hash modes; review any required adapter before substitution.
- Performance and whole-process memory comparison; no speedup or reduced resource claim is made here.
- Remaining native stat/close/combined-failure qualification, other read-failure modes, and more filesystem/version coverage; stable source identity under concurrent writes is not an atomic file snapshot.
- Reviewed FFI and foreign-caller memory/cancellation behavior before any integration.
- Product lifecycle/security review and applicable repository gates; source review alone does not qualify product integration.
- Native Linux/macOS implementation and tests if admitted. No platform support is inferred from this source comparison.

Integration must re-check owner and transaction changes accepted after the exact Go checkpoint above. This experiment is read-only and never modifies a source file, registry, scheduler task, installed service or user configuration.

[go-reader]: https://github.com/applicate2628/mcp-local-hub/blob/a7d92d2852c4f6eb8afb0569ef76a545e38aacf7/internal/api/state_read_beneath_root_windows.go
[go-errors]: https://github.com/applicate2628/mcp-local-hub/blob/a7d92d2852c4f6eb8afb0569ef76a545e38aacf7/internal/api/state_read_beneath_root_common.go
[go-caps]: https://github.com/applicate2628/mcp-local-hub/blob/a7d92d2852c4f6eb8afb0569ef76a545e38aacf7/internal/api/state_read_caps.go
[go-inode]: https://github.com/applicate2628/mcp-local-hub/blob/a7d92d2852c4f6eb8afb0569ef76a545e38aacf7/internal/api/state_read_inode_anchor.go
[go-observed]: https://github.com/applicate2628/mcp-local-hub/blob/a7d92d2852c4f6eb8afb0569ef76a545e38aacf7/internal/api/state_read_observation.go
[go-inverse]: https://github.com/applicate2628/mcp-local-hub/blob/a7d92d2852c4f6eb8afb0569ef76a545e38aacf7/internal/api/managed_backend_install.go#L65-L90
[go-pin]: https://github.com/applicate2628/mcp-local-hub/blob/a7d92d2852c4f6eb8afb0569ef76a545e38aacf7/internal/cli/install_reconcile_mcp_front.go#L3464-L3490
[win-lock]: https://learn.microsoft.com/windows/win32/api/fileapi/nf-fileapi-lockfile
[win-unlock]: https://learn.microsoft.com/en-us/windows/win32/api/fileapi/nf-fileapi-unlockfile
