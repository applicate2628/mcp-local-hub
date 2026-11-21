# Pull request #627: native ReadFile failure qualification

## Contents

- [Observed result](#observed-result)
- [Tested source identity](#tested-source-identity)
- [Inputs and commands](#inputs-and-commands)
- [Public records and metadata correction](#public-records-and-metadata-correction)
- [Qualification limits](#qualification-limits)

## Observed result

On **2026-11-21 at 14:34 UTC (17:34 Moscow time, UTC+3)**, the Windows x64 library experiment passed **16 tests, with 0 failed, ignored, measured or filtered out**. UTC means Coordinated Universal Time. This fresh run includes the previous 15 tests and one added native-read-failure test. It does not replace the earlier 15-test record for the unmodified base commit. The [captured receipts and output](evidence.json) record the results below.

**Table 1. Recorded attempts.** Times are UTC. Preparation uses phase timestamps; command intervals exclude surrounding receipt work.

| Attempt and phase | Recorded interval | Result |
|---|---|---|
| v1 formatter | 14:29:49.820–14:29:49.949 | Exit 1: multiline formatting required for the new `assert_ne!`; no native tests ran in v1 |
| v2 preparation | 14:30:55.590–14:30:56.799 | Completed; existing archives copied and verified |
| v2 formatter | 14:33:47.952–14:33:48.082 | Exit 0; empty standard output and error |
| v2 library tests | 14:34:04.282–14:34:06.911 | Exit 0; all 16 expected tests passed |

The library command took **2.63 seconds** by its retained-child receipt. Cargo reports 2.51 seconds for compilation; the test executable reports **0.02 seconds** for the tests. These measure different intervals. The v1 formatting failure and logs remain in the evidence; only the candidate test's formatting changed before the fresh v2 attempt.

The added test creates its own 4097-byte file. After the reader buffers 4096 bytes, the existing hook takes an exclusive one-byte lock at offset 4096 through a separate fixture handle and returns success. The next real `ReadFile` fails with `ERROR_LOCK_VIOLATION` (33). Assertions require `UnsafeObjectOrIo`, operation `read-final`, that exact Win32 code, and no cleanup errors. Requests `[4096, 2]` establish the partial-read position. Explicit unlocking precedes result assertions; a subsequent complete verified read succeeds. The local guard also attempts unlocking during unwinding. This qualifies the actual read-error branch for this schedule, rather than another hook-generated error.

## Tested source identity

The executed input was the exact seven-file export of base `b827398ef127a1556b7b2bdcdcec7fe03fa6651d`, plus two explicit overlays: `src/read_tests.rs` and `README.md`. The other five files were unchanged. The base commit and tree in the frozen manifest identify the base, not a commit containing the candidate.

**Table 2. Execution identities.** SHA-256 means the 256-bit Secure Hash Algorithm; the Git blob identifier also includes Git's blob header.

| Object | Identifier |
|---|---|
| Executed and published `src/read_tests.rs`, SHA-256 | `f77765527f80c93677d2ec3cb6991a2fa64c446b50c9c4f563136443603aa6b4` |
| Same test source, Git blob | `d16433d1db44f873e570d4cd65e2ac6f749ebc55` |
| README used during execution, SHA-256 | `1271875e93a004636c42cb18edeef548cda6ead71e5aca0d8d6af4651c785bc4` |
| Exact executed driver, published as `qualification.py`, SHA-256 | `8a6909eff30eea30a98ec0c9b3a659f5cd00a472eea79070f392ec27c1eca4a9` |

The main README was updated **after execution** to report the result, and this evidence directory was added afterward. Therefore, there is no claim that all seven final published files equal the executed seven-file snapshot. The final Rust sources and Cargo inputs remain identical to the successful run, including the test-source identity in Table 2.

## Inputs and commands

**Table 3. Inputs checked by the driver.** These are recorded checks of the execution inputs, not a public copy of every input byte.

| Input group | Recorded check |
|---|---|
| Existing dependency archives | 13 archives matched all registry entries in `Cargo.lock` |
| Expanded dependencies | 797 regular files and 13 generated checksum manifests were checked |
| Experiment source | Seven source hashes were equal before and after the successful phases |
| Selected tools and inputs | Eleven pins were equal before and after the successful phases |

The commands used existing direct binaries, with isolated work, build, temporary and fixture directories under `D:\dev\ChatGPT`:

```text
rustfmt --check --edition 2021 src/lib.rs src/windows.rs src/read_tests.rs
cargo test --offline --locked --lib --target x86_64-pc-windows-msvc -- --test-threads=1
```

No download, installation, elevation or system-policy change was performed. The serial test selection is library-only, in the debug profile. Dependency files were checked before and after the successful v2 commands. The eleven pins do not constitute a complete compiler or Windows Software Development Kit (SDK) file closure.

## Public records and metadata correction

[evidence.json](evidence.json) contains 15 captured records for v1 and v2: manifests, preparation and command receipts, output logs, and driver identities. [qualification.py](qualification.py) is the exact executed v2 driver. The v1 driver is represented losslessly by its unified diff from that file; only its owned D-directory constant differs. The driver is an execution tool with pinned input requirements, not an evidence-only verifier.

**Frozen-plan correction:** `test_policy.exact_passes` remained **15**, an obsolete informational field. The executed driver does not read that field. Its actual oracle compares all 16 expected test names and statuses, and requires the literal zero-failure 16-test summary. The logs and receipt record that successful result. The frozen `phase_status: NOT_RUN` labels are also original plan fields; later receipts record execution. The old broad `not_qualified` wording does not erase the one newly qualified read-failure schedule or qualify the remaining cases.

Five record texts replace personal Windows-profile prefixes with `<USERPROFILE>`. Unredacted logs and the exact final driver can be hashed independently. Raw hashes for redacted records identify retained private originals; their original bytes cannot be recomputed from the public replacement text. The public files omit dependency archives, expanded dependency bytes, tools and native fixtures. Their pins therefore remain recorded input identities, not measurements independently recomputable from this directory alone.

These artifacts establish captured content and internal consistency; they **do not authenticate runtime execution or the trust of the executing host**.

## Qualification limits

Actual post-acquisition stat failures, parent/final close failures, and combinations of an independent primary error with cleanup failures remain open. The new test covers one byte-lock read failure, with no cleanup failure; it does not prove every native read error, buffer erasure, or a per-case process-handle count. The existing repeated-handle-count test also passed, with its own existing scenarios.

There is no production-code change, Clippy rerun, Go consumer or foreign-function interface (FFI) qualification, full repository gate, release/all-target/dependency-test result, product installation/admission, or performance improvement claim. The retained direct child settled, but the driver does not establish a whole-process-tree deadline or settlement guarantee. This execution record is not independent lifecycle or security approval.
