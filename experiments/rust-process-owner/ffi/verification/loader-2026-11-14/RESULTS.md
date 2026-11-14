# Native DLL loader: failure and reference-ownership qualification

**Source base:** `65f513d24b39d8ed83dbe80dd15e1c377fc0f76e`.
**Working PR:** [#631](https://github.com/applicate2628/mcp-local-hub/pull/631), dependent on unchanged native-owner [#625](https://github.com/applicate2628/mcp-local-hub/pull/625).

## Contents

1. [Scope and decision](#scope-and-decision)
2. [Native cases and observations](#native-cases-and-observations)
3. [Negative controls and initial failures](#negative-controls-and-initial-failures)
4. [Reproduction](#reproduction)
5. [Review and remaining limits](#review-and-remaining-limits)
6. [References](#references)
7. [Final verification and commit identity](#final-verification-and-commit-identity)

## Scope and decision

This increment qualifies the existing Go/C boundary that loads the Rust dynamic-link library (DLL). It uses real Windows loading/unloading with small, locally compiled benign DLL fixtures. No Rust/native-owner or product implementation is changed merely to manufacture a rewrite. Fixture C code is test data, not a second process engine or a new C/C++ product component.

The previously interrupted `65f513d` publication was confirmed on GitHub and in the clean private clone before work began. It already preserved the prior post-rebuild artifact checks. Those historical hashes were not edited to match later binaries. Parent #625 remains `7ce04f7ef2f96680ed94db1edaf77cf0358421c4`.

The contract distinguishes inability to load an image, refusal of an already loaded incompatible library, independent references held by other callers, and requested release of the caller's own reference. A numeric module observation is not ownership and is never passed to FreeLibrary by the test. The observation is made without increasing the reference count and only under the fixture's serialized lifetime assumptions.

## Native cases and observations

**Table 1. Seven opt-in test functions.**

| Test area | Exact exercised cases |
|---|---|
| Required exports | Individually missing each of nine required exports: version, create, admission, stop, finish, release, checked admission, environment and stdio; native error127, no retained module reference after refusal |
| Version compatibility | Complete export sets reporting ABI versions0,2 or maximum uint32; revision error1306; no retained module reference |
| Native load failures | Missing file126, non-executable image193, DllMain initialization refusal1114, missing dependency126; nil library result and original native error |
| Another caller's ownership | A separately acquired reference remains live after this loader refuses symbols/version; releasing that original reference leaves no extra reference from the failed Open |
| Successful ownership | Two independent Library objects can reference one module; closing either twice must not unload the other reference; the final legitimate close unloads it |
| Unicode path/local dependency | A Unicode DLL path works; an import beside its DLL is loaded and then unloaded with the final owner |
| Dependency search isolation | In a finite owned child, a missing import present only in that child's working directory/PATH is still refused by the explicit DLL-directory/System32 search policy |

The fixtures export correctly declared but inert process-operation functions. Tests call Open/Close, not those fake operations. DllMain performs no I/O, thread/process creation, nested loader operation or external work; the initialization-refusal variant only returns false on attach. The dependency returns a constant version. These fixtures are not claimed to implement a Rust owner.

For actual initialization failures, the test suppresses error dialogs only on its locked test thread and restores the prior mode. The dependency-search case changes working directory and PATH only for its own finite child. The parent environment, global search configuration, registry, system DLLs and installed Hub are untouched. The child executable is the existing test binary, started without a shell and with a watchdog, and is joined.

Post-load reference assertions use valid DLLs. The malformed-image case asserts the load outcome only: a separate failed full-path module lookup is not treated as authoritative proof of module absence. No heap-allocation leak, universal unload guarantee or race-free observation of independently changing third-party modules is inferred from these cases.

## Negative controls and initial failures

**Table 2. Three deliberate defects, applied only to a private copy of the Go/C bridge.**

| Mutation | Required semantic failure |
|---|---|
| Omit FreeLibrary on incompatible-image refusal | The test observes an unexpected retained module reference |
| Ignore version mismatch | An incompatible fixture is accepted instead of returning revision mismatch |
| Replace constrained dependency search by default lookup | The isolated child loads the dependency from its working directory/PATH when refusal was required |

The runner requires both the nonzero exit and the corresponding semantic assertion text. A compilation/setup error alone does not qualify as a successful negative control. The original runtime source and Rust DLLs are not mutated; the erroneous copies execute in finite owned test processes and are never installed.

Two initial failures are preserved rather than reclassified as product defects:

1. [First positive run](../runs/loader-positive-initial/run.log): all tested LoadLibrary errors were correct, but the test then expected a full malformed-image-path GetModuleHandleEx lookup to report module-not-found126. It instead reported bad-image193. The oracle was narrowed to separate load-error preservation from module-reference observations on valid images. The [corrected run](../runs/loader-positive-corrected/steps.json) passed.
2. [First mutation attempt](../runs/loader-negative-leak-module/run.log): a C block comment inside the Go cgo block ended that block prematurely. The semantic marker was absent, so the runner rejected this as an invalid negative experiment. The mutation annotation was changed to a line comment. The subsequent three [reference-leak](../runs/loader-negative-final-leak-module/steps.json), [version](../runs/loader-negative-final-accept-version/steps.json), and [search](../runs/loader-negative-final-default-search/steps.json) controls compiled and failed their intended assertions.

Old run inputs keep their original source hashes. They are not described as tests of later edited bytes. The final positive run uses the unchanged production bridge, with the completed test assertions and runner.

## Reproduction

The [runner](run.ps1), [DLL fixture](fixture.c), [dependency fixture](dependency.c), and opt-in Go test are committed. No unversioned environment helper or local-only source is required. Existing Windows x86_64 Go/cgo and C compiler tools must already be available. The ordinary Rust DLLs are prebuilt by the existing [full experimental runner](../run.ps1); no toolchain, package or dependency is automatically downloaded.

```powershell
# From experiments/rust-process-owner/ffi/verification/loader-2026-11-14
.\run.ps1 -LabRoot 'D:\dev\ChatGPT\your-owned-lab' `
  -CCompiler 'C:\path\to\existing\gcc.exe' -RunName 'new-positive'
.\run.ps1 -LabRoot 'D:\dev\ChatGPT\your-owned-lab' `
  -CCompiler 'C:\path\to\existing\gcc.exe' -RunName 'new-leak-negative' `
  -Mutation leak-module
.\run.ps1 -LabRoot 'D:\dev\ChatGPT\your-owned-lab' `
  -CCompiler 'C:\path\to\existing\gcc.exe' -RunName 'new-version-negative' `
  -Mutation accept-version
.\run.ps1 -LabRoot 'D:\dev\ChatGPT\your-owned-lab' `
  -CCompiler 'C:\path\to\existing\gcc.exe' -RunName 'new-search-negative' `
  -Mutation default-search
```

A new run name is mandatory; existing logs/copies are not overwritten. All writable paths stay below `D:\dev\ChatGPT`. The C fixtures are compiled with warnings as errors and without a C runtime entrypoint, using their declared minimal DllMain. The Go module uses only its existing dependencies. Commands, exits, mutation text, input/fixture hashes and logs are grouped per run rather than stored as one file per compiler invocation.

A completion marker appears only when every expected result matches. Exit7 is an intentional status-capture control. Positive qualification runs use `-race -count=3`, vet, `cgocheck2`, and a distinct JSON-count run; repetition is not counted as a new test. Tests under `ffi_loader_faults` are deliberately selected, not silently skipped normal tests. Ordinary code/normal DLL hashes are checked before and after each isolated loader run; a later ordinary rebuild is a new artifact epoch, not a byte-reproducibility claim.

## Review and remaining limits

The source review retained one loader and one owner. No new runtime registry, fallback, service, dependency or exception-based recovery framework was added. The tiny fixtures reuse the declared ABI header; negative variants are generated only from explicit unique anchors. Failure cleanup releases the loader's reference, never a borrowed GetModuleHandle observation. The test controls have explicit lifetimes and no blanket process cleanup.

Eleven relevant skills were read and applied: Using Superpowers (recover the task), Executing Plans (ledger and scoped gate), Test-Driven Development (tests before instrumentation and distinguishing negatives), Using Git Worktrees (existing private checkout), API and Interface Design (typed refusal versus resource ownership), Architecture Review (trust/lifetime boundaries), Go Testing and Verification (real loader and independent-reference oracles), Go Security Hardening (fixed executable/path/environment scope), Systematic Debugging (separate test-oracle and experiment-construction errors), Ponytail Review (no duplicate runtime mechanism), and Verification Before Completion (fresh full checks and published evidence). This is author self-review, not an independent review or multiple agents.

LoadLibrary may run image initialization before ABI/export checks. Therefore a correct refusal does not mean an untrusted DLL was safe to load. Absolute paths and checksums are not signature/authenticity admission. The tested search policy does not establish guarantees about every preloaded module, side-by-side assembly, known-DLL/API-set redirection or hostile concurrent replacement. The fixture begins without its private dependency preloaded.

Still open: product binary trust/identity and deployment admission, other loader/unload/allocator/panic failures and races, exhaustive memory accounting, all native descriptor and I/O profiles, complete product policies, native Linux/macOS, equivalent-workload benchmarks and independent lifecycle/security review. The current gate supplies no portability or speedup evidence, and does not enable the DLL in the working Hub.

## References

1. [Microsoft LoadLibraryExW](https://learn.microsoft.com/en-us/windows/win32/api/libloaderapi/nf-libloaderapi-loadlibraryexw): image loading and dependency search flags.
2. [Microsoft DllMain](https://learn.microsoft.com/en-us/windows/win32/dlls/dllmain): initialization execution/refusal and loader-lock constraints.
3. [Microsoft GetModuleHandleExW](https://learn.microsoft.com/en-us/windows/win32/api/libloaderapi/nf-libloaderapi-getmodulehandleexw): non-owning observation and concurrency limitations.
4. [Microsoft FreeLibrary](https://learn.microsoft.com/en-us/windows/win32/api/libloaderapi/nf-libloaderapi-freelibrary): per-process module references.
5. [Microsoft SetThreadErrorMode](https://learn.microsoft.com/en-us/windows/win32/api/errhandlingapi/nf-errhandlingapi-setthreaderrormode): thread-scoped error handling.

These sources specify mechanisms; the versioned run logs establish only the explicitly exercised behavior. Necessary results belong to this PR; disposable binaries and caches can be rebuilt.

## Final verification and commit identity

Qualification source commit: `f2645b1fa994e8fdde0b5d4327f0220ba38cd742`.
The [final loader run](../runs/loader-final/steps.json) passed all24 expected
steps. Its [count record](../runs/loader-final/counts.json) reports7 top-level
functions,20 nested cases,0 skips. The loader suite ran with the race detector
three times, then vet and cgocheck2. The nine-export and three-version loops
also repeat their own refusal checks; those repetitions are not new functions.

The [ordinary regression run](../runs/loader-ordinary-final/steps.json) passed
all22 expected steps:47 normal Go functions/31 nested cases/0 skips, both Rust
DLL profiles with Go race x3, cgocheck2 and vet;14 Rust ABI tests and53 native
owner checks in debug/release, with Clippy and format checks. These are the full
experimental suites, not all tests or installed acceptance of the Hub.

Final loader and ordinary source manifests match the committed qualification
bytes. They describe the actual overlay on the starting head, not a fictional
clean checkout of the later evidence commit. Earlier runs keep their earlier
input identities. The ordinary regression runner may rebuild ordinary DLLs;
a per-run pre-rebuild hash is not applied to later artifacts. Binary-identical
builds and performance improvements are not claimed.

### Disposable-copy cleanup

Seven completed fixture/copy directories were checked against their recorded
fixture/source/mutation manifests and moved directly into approved trash. Their
generated import libraries are reproducible from the recorded C build. The
[movement record](disposable-copies.json) lists each verified scope. An initial
preflight stopped because PowerShell's default JSON decoding mismatched a Unicode
path; [the diagnostic record](cleanup-preflight.txt) confirms that explicit UTF-8
matched both the name and unchanged SHA256 before any movement. No historical
hash or fixture was altered to bypass the check. Active source/normal targets,
cache and all necessary evidence remain outside trash; trash was not emptied.
