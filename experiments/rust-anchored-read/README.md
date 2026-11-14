# RX-04/read: isolated anchored-file reader experiment

**Reviewable experiment only. Not a replacement for the Hub reader and not connected to installation, state, vault or runtime.** Implements one bounded Windows local-filesystem slice of [the MCPHub Rust work plan](https://github.com/applicate2628/mcp-local-hub/pull/623).

## Contract and ownership

`read_verified(root, components, cap, expected_digest, canceled)` opens a drive-absolute root with a no-reparse NT open, then opens each component relative to the retained parent handle. The final file is read and hashed through the same handle; no pathname is reopened to obtain the returned bytes.

One acquired handle has one owner. The next parent is acquired before its predecessor is closed; at most two handles are retained at once. Final and parent handles are explicitly closed on success and error. Native close failure prevents a successful byte result and is recorded separately from an existing primary failure. A failed numeric close is not retried. Real CloseHandle failure injection is still a qualification gap, not a tested guarantee.

The data cap is positive and at most 1 MiB (`MAX_STATE_BYTES`). A stat check is not sufficient: each read requests at most the remaining capacity plus one sentinel byte. Growth above the cap fails without returning partial data. The SHA-256 digest verifies the bytes actually read, not an earlier path snapshot. It does not establish authorization or trusted provenance.

The current buffer and read chunk use the upstream `zeroize` wrapper and are discarded on failure. This does not prove erasure of all allocator history, transient copies, registers or another process's memory. Logical data length is capped; exact allocator footprint is a separate measurement.

## Important differences from the current Go API

This first slice deliberately accepts only ordinary drive-absolute Windows roots. UNC, verbatim/device namespaces, network filesystems, backup-specific caps and the Go reader's optional empty digest mode are **not qualified**. The digest is already a 32-byte value rather than a hex string. These differences prevent a drop-in replacement claim.

The caller must already have permission to read the selected root. Components are single names, not arbitrary relative paths; empty names, parent traversal, slash/backslash and NUL are rejected. Cancellation is observed between native operations and after the internal test hooks; it does **not** promise interruption of a synchronous ReadFile that is already blocked. There is no Go/Python FFI, writer, journal, lock manager or fallback path.

Outside Windows the function returns `Unsupported` before side effects. That is an explicit unimplemented platform, not Linux/macOS support. Native status codes preserve their NT versus Win32 origin instead of guessing absence from error text.

## Native evidence

Windows 11 build 26100, x86_64, Rust 1.99.1. Dependencies were already cached and used offline: `windows-sys=0.61.2`, `sha2=0.10.9`, `zeroize=1.9.0`; lockfile included. No dependencies were installed or added to the Hub product.

**15 native tests passed without skips**, covering:

- Exact verified bytes, empty files, nested Unicode names and invalid input.
- Cancellation before open and during reading, hash mismatch and excessive size.
- Directory-as-file and missing-file native error classification.
- Reparse points at the root, a root ancestor, intermediate component and final file.
- Replacement with a symlink before open; replacement of a retained parent or final path after open still reads the original object.
- File growth after stat, one-byte over-cap detection, and exact-cap EOF.
- Preservation of injected operation failure and stable handle count over repeated successful and unsuccessful reads.

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

- Exact current Go-contract differential comparison, including its additional namespaces/policies, callers and error taxonomy.
- Performance and whole-process memory comparison; no speedup or reduced resource claim is made here.
- Native failure/close injection and more filesystem/version coverage; stable source identity under concurrent writes is not an atomic file snapshot.
- Reviewed FFI and foreign-caller memory/cancellation behavior before any integration.
- Independent lifecycle/security review and applicable repository gates.
- Native Linux/macOS implementation and tests if admitted. The local analysis environment currently has no Rust toolchain; none was installed.

The inspected Go reader files were unchanged between previously reviewed `92bc059c` and #616 `53645ec7170c9d56073ab5140dec865b23ce7c21`. Integration must re-check the actually accepted owner and transaction changes. This experiment is read-only and never modifies a source file, registry, scheduler task, installed service or user configuration.
