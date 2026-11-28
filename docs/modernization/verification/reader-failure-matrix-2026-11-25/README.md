# Rust reader failure matrix

The preserved Windows x64 run completed **20 library tests**, with **0 failed, ignored, measured or filtered out**. This qualifies deterministic propagation of metadata and close outcomes on the pinned candidate. The actual Windows calls for the substituted outcomes completed successfully. Native metadata and close failure induction, a Go adapter, and installed Product acceptance remain open. [Normalized evidence](evidence.json)

## Candidate identity and behavior

The run used the seven files of the crate at [#627 `6615d239fe066eaa5fa78c22a5272d6602528274`](https://github.com/applicate2628/mcp-local-hub/tree/6615d239fe066eaa5fa78c22a5272d6602528274/experiments/rust-anchored-read) with two Rust files replaced and one added. All five other crate files, including both Cargo inputs and the public library entry point, match the published base byte for byte. Independent readback matched all eight candidate file sizes and SHA-256 digests. SHA-256 is the 256-bit Secure Hash Algorithm. The source inventory before and after the Windows run records eight files and 57,943 bytes. [Source pins](evidence.json)

**Table 1. Changed paths and executed source digests.**

| Path under `experiments/rust-anchored-read` | SHA-256 |
| --- | --- |
| `src/windows.rs` | `d736e2e0723247d96cb7dc6c02b423adc5fd6761cf3179e9629859b902b8f33f` |
| `src/read_tests.rs` | `7efddd07830aad471cb4903ff8c8346ac54568304241002db618e290c7b648fd` |
| `src/failure_tests.rs` | `5d26c2c818e18a1ceebada0ac00f82f9063498f4b1acfc4c46801f4aa43fb285` |

The Windows reader takes a private per-invocation operations value. The normal entry point binds it to the existing native metadata and single-attempt close functions and forwards the existing hook. Acquisition, ownership transfer, bounded reading, digest verification, primary failure selection and cleanup order follow the previous code. A test can record when each actual handle is acquired or consumed and replace a successful operation's reported outcome with a deterministic error. Test operations stay inside the call. No public API or dependency changes with this delta.

## Matrix and result

**Table 2. New test roots.** The 25 fixtures are internal cases counted from the four test definitions. Cargo reports four new test roots and 16 existing roots.

| Test root | Fixtures | Assertion |
| --- | ---: | --- |
| `native_success_keeps_original_handles_and_verified_bytes` | 1 | Verified bytes and original acquisition and close order |
| `metadata_failure_keeps_primary_and_each_cleanup_combination` | 14 | Error at the root, either intermediate directory, or final file survives every applicable zero, one, or two cleanup errors |
| `close_failure_never_returns_bytes_and_retains_later_cleanup_errors` | 7 | Early parent and final cleanup failures prevent data return and preserve the primary code and subsequent cleanup codes |
| `non_io_primary_failures_survive_both_cleanup_failures` | 3 | Digest mismatch, cancellation and size limit survive both later cleanup errors |

The fixture verifies that every original acquisition has one explicit close, at most two handles are held at once, and no handle remains in its acquisition ledger. The synthetic close status follows a successful native close, so the test leaves no open handle to simulate an error. The previous [actual `ReadFile` range-lock failure qualification](../reader-native-read-2026-11-21/README.md) remains separate.

## Execution and retention

**Table 3. Completed standard commands.** The target was `x86_64-pc-windows-msvc` and Cargo used the pinned offline inputs. Elapsed seconds are command receipt observations.

| Command | Exit | Seconds |
| --- | ---: | ---: |
| `rustfmt --check --edition 2021 src/lib.rs src/windows.rs src/read_tests.rs src/failure_tests.rs` | 0 | 0.115 |
| `cargo check --offline --locked --all-targets --target x86_64-pc-windows-msvc` | 0 | 6.909 |
| `cargo test --offline --locked --lib --target x86_64-pc-windows-msvc -- --test-threads=1` | 0 | 10.965 |

The standard phase ended at 00:05:14 UTC on 25 November 2026. The original processes were reaped, monitored Windows Job instances had zero active processes, and original handles and capture streams closed. The separate ownership guard recorded a successful normal case and an intentional timeout with exit 1. The latter reached the genuine descendant and closed its original Job after termination. Preparation and guard receipts certify their own scopes. The evidence records original capture roles, byte counts and SHA-256, along with command, parent and plan receipt digests. [Receipt and test catalog](evidence.json)

The candidate was uncommitted during the captured run. This publication candidate reproduces the eight executed source files and adds this report and normalized evidence outside the crate. The readback ran no new Rust build or tests. Native metadata and close failures and their combinations still require owned fixtures. The [existing Go reader consumer comparison](https://github.com/applicate2628/mcp-local-hub/blob/2a0657728b45d7dc1c654101c323f883424b7da3/docs/modernization/verification/residual-next-closeout-20261122/C-source-triage/README.md) identifies the separate adapter and Product requirements.
