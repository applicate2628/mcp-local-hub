# Bug: native bootstrap load-time DLL code precedes handle revocation

- id: 2026-08-12-cst-native-bootstrap-loadtime-dll-precedes-revocation
- context: 2026-08-11-cst-saved-field-sampler
- status: open
- severity: high
- area: CST native frontend/worker pre-main trust boundary
- found-by: security-reviewer
- fix-class: design-decision

## Reproduction

1. Read `work-items/active/2026-08-11-cst-saved-field-sampler/design.md:1141-1148,1205-1229`: the bundle includes CPython DLLs, the bootstrap validates itself after process start, and the first application-controlled instruction revokes inherited handles.
2. Observe that the design does not constrain the Portable Executable import, delay-import, or thread-local-storage callback tables; does not forbid load-time CPython/extension imports; and does not define a hardened absolute post-revocation DLL load owner.
3. Compare Microsoft's process-initialization behavior: load-time DLL entry points run for `DLL_PROCESS_ATTACH` during process initialization, before the executable entry point: <https://learn.microsoft.com/en-us/windows/win32/dlls/dynamic-link-library-entry-point-function>.
4. Compare Microsoft's DLL-search security guidance: an unresolved non-absolute dependency searches directories and a writable searched directory permits binary planting: <https://learn.microsoft.com/en-us/windows/win32/dlls/dynamic-link-library-security>.

## Expected

No package-extensible or attacker-substitutable code executes while the frontend four handles or worker five handles remain inheritable. The first allowed native code revokes and reads back every required flag before loading embedded CPython or any extension.

## Actual

A load-time DLL, transitive dependency, or TLS callback can execute before the native marker and revocation. It can retain/use a capability or spawn an inheriting descendant. Later self-hash, receipts, Job settlement, or quarantine cannot prove that pre-entry execution did not occur.

## Required design correction

Define and verify a minimal pre-revocation loader closure: statically link the bootstrap or allow only an explicit system/KnownDLL import set; forbid pre-revocation package DLLs and TLS callbacks; verify import/delay-import/TLS tables and transitive dependencies. After revocation, load the held/hash-pinned CPython DLL by absolute path with a closed `LoadLibraryExW` search policy and verify the mapped module identity. Add planted-DLL, import/TLS mutation, missing/transitive dependency, frontend-four and worker-five real-child probes.

Re-review S4 Claims 5, 7, 12, 13, 14, 16 and 18.

## Terms and Abbreviations

- **CPython** — the embedded Python runtime.
- **CST** — Computer Simulation Technology electromagnetic solver suite.
- **DLL** — Dynamic-link library.
- **PE** — Windows Portable Executable image format.
- **TLS callback** — Portable Executable thread-local-storage initialization callback.
