# W5 Native Worker Prelude Toolchain Package

Execution role: `$toolchain-engineer`

Accepted design: `7423D56DD33394336A06AB8C515D12F4496B4AC0533F2901BF5A0EE1436756ED`

Accepted W2 package: `7FF74F7205D9F21F4E2EE9842C36773C330B43D0D445265B4B94F72E69D60BC1`

Input W5 REVISE: `3C8A784BB93A41674DBE1A80A95B0F001F0E9EF22A5771B3A74F46ECB1BF4B1C`

## Receiving-side echo

- Implement the native W5 prerequisite only: consume the exact five-handle child tuple, validate a bounded broker prelude, revoke and read back inheritance, and emit a native pre-main receipt before any Python/package/descendant work.
- Preserve the no-CRT custom entry and KERNEL32-only Portable Executable closure, deterministic two-build output, verifier, manifest and default-off package state.
- Do not implement embedded CPython, the application result, Go/W4 composition, live Service Control Manager or CST work, Git index, dependency, commit or publication changes.

## Implemented contract

- `WorkerPreMainBootstrapV1` is now a closed canonical JSON frame containing checksum, correlation, exact Query Performance Counter deadline, the exact five roles, positive distinct pointer-sized capability locators, exact access masks and expected volume/file identities.
- `WorkerPreMainReceiptV1` binds the same correlation, deadline, access masks, identities, exact roles and bootstrap checksum; it asserts both inheritance and capability identity validation and requires `python_initialized=false`.
- The no-CRT native worker clears and reads back stdin/stdout/stderr, verifies current Job membership, emits the startup proof on stderr, then reads one four-byte big-endian length plus at most 2048 canonical bytes from stdin.
- It verifies CRC32, correlation shape, unchanged QPC frequency/deadline, roles, locators, masks, directory type and `FILE_ID_INFO` identity, clears and reads back both capability inherit flags, and emits the bound receipt on stdout.
- The prerequisite exits 78 after the receipt. It does not claim a Python initialization or application result. W6 remains the owner of embedded CPython and application execution.

## Changed paths and hashes

| Path | SHA-256 |
|---|---|
| `native/cst-runtime/mcphub_cst_runtime.c` | `ED33E7C5F3B1FE4A30E986B546B20CF700FFB3228FC8A5AEFEA2673E4C82C792` |
| `native/cst-runtime/build.ps1` | `3286F585BC3AA2A5DBE236A19FFACFAF28C51DBC257B848019B3CB6CCB7EF03B` |
| `native/cst-runtime/mcphub-cst-runtime.exe` | `38D87C50F716E334F89628D4F35604534C3A35BDCF35F378F1D939137BAB89E3` |
| `native/cst-runtime/cst-native-runtime-manifest-v1.json` | `FE1FC96E31A7D5A8C168AA3E69F788F6E933030D7E325142DA10E3B085085443` |
| `src/mcphub_em_mcp/cst_saved_field_broker_worker_protocol.py` | `A771551C2809FC41D5B09323DD2BDF03C2B867931B2EF7053F5442B732A14580` |
| `tests/test_cst_saved_field_w5_worker_pre_main.py` | `87EF6FC48BAC5ADAF2EFF8BBFDC4BE33D7E7D2D19E9FB34871674F021E8A362A` |
| `tests/test_cst_native_runtime_w2.py` | `A9C44207F8967FB1964F75E6F117870C9A531318B4EE43798E1686AD5B5C3C89` |
| `tests/test_cst_saved_field_broker_worker.py` | `104F6096E8AFEDCABEC6033B20ACFAB02422A7B7639EF70DBC713D47E3CC7CF9` |
| `tests/test_cst_saved_field_containment_windows.py` | `DBB2F4887E9C30D676B0A4ADC6850C756607BD329A48167BB63FAC2D17A59055` |

## Verification

| Check | Result |
|---|---|
| RED baseline | `52 passed, 2 failed`: removed Python self-attestation API and ordinary Python unable to emit startup proof. |
| Two clean unsigned native builds | PASS; both SHA-256 `38D87C50F716E334F89628D4F35604534C3A35BDCF35F378F1D939137BAB89E3`. |
| Independent PE plus manifest/image verifier | PASS; only KERNEL32 direct imports, no CRT/delay/TLS/CLR/bound import, required mitigations retained. |
| Exact native Win32 five-handle fixture | PASS; real JOB_LIST plus ordered five-entry HANDLE_LIST, startup proof, bootstrap and receipt exchange; `python_initialized=false`. |
| Focused protocol/native/worker/containment suite | PASS: 66 tests. |
| Scoped Ruff check and format | PASS. |
| Scoped `git diff --check` | PASS. |
| Post-edit CodeGraph MCP blast-radius query | PASS; stale warning referenced only unrelated `internal/daemonrecovery/classifier.go`. It identified the known W6/T15 composition consumer requiring the new receipt fields. |

## Toolchain flag delta

Compiler intrinsic generation changed from `/Oi` to `/Oi-`. The intended and observed effect is that the no-CRT image uses the package-owned byte-copy routine instead of an unresolved CRT `memcpy`; the first link demonstrated the old form's unresolved `memcpy`/`__chkstk`, and the final independent PE verifier confirms no additional import library or DLL appeared. Large fixed bootstrap buffers are composition-root-owned static storage, removing `__chkstk` while preserving a single-threaded pre-Python lifetime.

## Platform matrix and risks

| Cell | Verdict |
|---|---|
| Windows x64, MSVC 19.51, SDK 10.0.26100 | VERIFIED: deterministic build, PE verifier and real Win32 five-handle exchange. |
| Other Windows toolchain/SDK versions | `ASSUMPTION (UNVERIFIED)`: the build discovers candidates; each successor needs the same two-build, verifier and Win32 fixture. |
| Linux/macOS | Not a supported native runtime cell; Python protocol tests remain portable. |

Residual W5 work is intentionally owned by `$backend-engineer`: replace the existing three-handle ordinary-Python containment launch with the exact native five-handle path, create/retain the root capabilities, send this bootstrap after startup proof, validate this receipt before the application frame, and prove cancellation/Job/handle settlement. The post-edit CodeGraph query also found the W6/T15 composition fixture still using the removed diagnostics/self-attestation shape; W5/W6 must update it after the real containment composition exists.

Rollback: restore the prior W2 native source/build/image/manifest and the prior reduced pre-main protocol/tests as one atomic group; W5 remains `REVISE:toolchain-prerequisite` while absent.

Adjacent findings: none outside the accepted W5/W6 handoff.

Gate: PASS

## Terms and Abbreviations

- CRT: C/C++ runtime.
- HANDLE_LIST: Windows explicit inherited-handle allowlist process attribute.
- PE: Portable Executable.
- QPC: Query Performance Counter.
- W2/W5/W6: ordered implementation phases in the accepted shipping plan.
