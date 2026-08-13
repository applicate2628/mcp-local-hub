# W2 Toolchain Package — Native Runtime and Immutable Load-Closure Owner

Execution role: `$toolchain-engineer`

Plan: `484883EDBAD02333162C61FAF78B99AA56C402FD64D5955F0E6B65BDDEC82E14`

Accepted design: `7423D56DD33394336A06AB8C515D12F4496B4AC0533F2901BF5A0EE1436756ED`

Accepted W1 package: `5A5932BF2B585AB7A3808D849E7C8645005404DDB021F0FF426C5850FCBC26DF`

## Receiving-side echo

- Task understood: implement W2 only: one package-owned AMD64 no-CRT custom-entry runtime, an independent PE admission verifier, deterministic two-build manifest, fixed frontend/worker roles, and a fail-closed recursive package-load closure builder.
- Inputs accepted: the exact plan, design and W1 hashes above; W1 native RED test SHA-256 remained `4C9F33DB8E4A9EA9A510FC1F77919B0118B0C1E5A47D45D81185F6893F6E68D6`.
- Output: eight W2 files listed below. No Go launch/composition/receipt path, package dependency manifest, live CST, Service Control Manager, App Control, VHDX, Git index, commit, or publication was changed.
- Preserved boundary: absent `ProvisionedPackageIdentityV1` remains default-off. The native image revokes role handles and exits `native_loader_invalid` (process code 78) without Python, package, vendor, CST, descendant or background work.
- Assumptions: no unverified premise drives the W2 gate. Signing and a real provisioned CPython/System32 closure remain explicitly target-unfulfilled until X1/X2 and provisioning phases.

## Implementation package

| Path | SHA-256 | Owner |
|---|---|---|
| `native/cst-runtime/mcphub_cst_runtime.c` | `53531388ADBB42434460A9806A614E5F24F6F3906FB67A0634012F017CA348D2` | Freestanding custom entry; first-instruction standard-handle revoke/readback, then exact frontend or worker capability tuple, then default-off. |
| `native/cst-runtime/build.ps1` | `35F53893EB9839845ED0788488B2192A44FF9A4BCB27CBA56B87B2DCE89708E4` | Absolute Visual Studio/SDK discovery, two-build deterministic compiler/linker owner, repo-neutral manifest emission and scratch cleanup. |
| `native/cst-runtime/verify.ps1` | `F05159875277DF84749FB6956A651A9F8E213CF708F239788B2148BA2839EC9D` | Manifest/image binding entrypoint. |
| `native/cst-runtime/verify_cst_native_pe.py` | `1155A3BCA6E704AF99A385AD6B60799986730B6ADC8A8F8D0E95AECD0D485DAF` | Independent raw PE parser: AMD64, entry, import names, forbidden directories, load-config flags, sections, relocations and CET extended characteristic. |
| `native/cst-runtime/build_package_load_closure.py` | `D9085B98D09937ED376C21666C166E46CA570FCF50B94E382D286598C071D8FB` | Independent recursive normal/delay import traversal with one-root package resolution, explicit System32 rows, deterministic order and missing/ambiguous rejection. |
| `native/cst-runtime/mcphub-cst-runtime.exe` | `CFCE49ED23D63D5F19F4CEBF386282916331255092A30E77382FA45F483C5E91` | Byte-identical unsigned W2 image from both clean builds. |
| `native/cst-runtime/cst-native-runtime-manifest-v1.json` | `AEC9DE81AEE361C5FF1AEB2C126219781BC891E681D0C4D6402F9C4006C5AC56` | Publication-safe input/toolchain/PE/role/default-off closure receipt. |
| `tests/test_cst_native_runtime_w2.py` | `E17D6D2B8F32F58B8EFF4A944F631799BBCDF139FF3730CA055DC86153769F8C` | PE mutation self-falsifiers, closure traversal failures, manifest provenance and real-child default-off tests. |

## Toolchain and observed flag delta

Direct probes resolved Visual Studio Community 18.8.3, MSVC tools `14.51.36231`, compiler `19.51.36252`, linker/dumpbin `14.51.36252`, and Windows SDK `10.0.26100.0`. `cl`, `link`, and `dumpbin` were not resolved from ambient PATH; build discovery uses `vswhere` and absolute installed paths. The manifest replaces those machine paths with `<MSVC>` and `<WindowsSDK>`.

The compiler/linker delta is `/Zl /GS- /NODEFAULTLIB /ENTRY:mcphub_cst_entry /SUBSYSTEM:WINDOWS /DEPENDENTLOADFLAG:0x800 /DYNAMICBASE /HIGHENTROPYVA /NXCOMPAT /CETCOMPAT /GUARD:CF /BREPRO`. `dumpbin /HEADERS /LOADCONFIG` observed AMD64 PE32+, entry RVA `0x1000`, only KERNEL32 imports, CET compatible, High Entropy VA, Dynamic Base, NX, Control Flow Guard, load-config size `0x148`, Dependent Load Flag `0x0800`, relocations, and zero TLS/delay/CLR/bound-import directories. The independent parser agreed.

## Fresh verification

| Check | Result |
|---|---|
| `build.ps1 -Clean -Unsigned` | PASS; both clean unsigned outputs SHA-256 `CFCE49ED23D63D5F19F4CEBF386282916331255092A30E77382FA45F483C5E91`. |
| `verify.ps1 -Unsigned` | PASS; independent PE and manifest/image binding. |
| W1 native plus W2 test files | PASS: 15 tests. |
| Focused plan keyword selection | PASS: 12 tests. |
| PE self-falsification | PASS: wrong machine, import, TLS, relocation and CET mutations return code 78. |
| Package closure self-falsification | PASS: unresolved and ambiguous package dependencies reject deterministically; synthetic allowed System32 closure is stable. |
| Real frontend/worker absent-receipt children | PASS: both exit 78 with empty stdout/stderr after exact inherited handle inputs. |
| Existing-six oracle (`test_servers.py`, `test_stdio.py`) | PASS: 6 tests; one pre-existing Pydantic forward-reference warning. |
| Ruff check and format | PASS: four scoped Python files. |
| `git diff --check`; `.build` residue check | PASS; no build scratch remains. |
| W0 preservation oracle | Expected RED preserved: 8 missing W3+ production-root/receipt/Go-integration assertions, one sensitivity PASS. |

## Platform matrix, risk and handoff

| Cell | Verdict |
|---|---|
| Windows x64 / MSVC 19.51 / SDK 10.0.26100.0 | VERIFIED for W2 build, independent verification, mutation tests and real default-off child probes. |
| Other Windows toolchain/SDK versions | `ASSUMPTION (UNVERIFIED)`: build discovers them but W2 acceptance pins the observed receipt; provision/review each successor independently. |
| Linux/macOS | Not a supported W2 runtime cell; Python parsers are portable but native image execution is Windows-only. |

Residual risk: a real signed CPython/package/System32 closure, App Control receipt, read-only VHDX continuity, Python initialization and package application work do not exist in W2. The emitted closure state is explicitly `unprovisioned`; treating it as authority must fail closed. W3 may integrate only the exact native image/manifest receipt with the existing Go spawn owner; later provisioning phases must populate and independently verify the real closure before any package load.

Rollback: remove exactly the eight W2 paths above. W1 and existing implementation paths require no restoration.

Adjacent findings: none.

Next owner: `$backend-engineer` for W3, retaining `exec.Cmd` as the sole frontend creator and the exact W2 image/manifest receipt as admission input.

Gate: PASS

## Terms and Abbreviations

- AMD64: 64-bit x86 Windows machine type.
- CET: Control-flow Enforcement Technology.
- CRT: C/C++ runtime.
- PE: Windows Portable Executable.
- SDK: Software Development Kit.
- W2/W3: ordered implementation phases in the accepted plan.
