# Reproducing the standard-stream negative controls

These are intentional faults in an isolated, owned test checkout, not supported
runtime modes. Never mutate an installed/shared DLL or a live Hub process.
Base implementation: `4fbbea19e9db7dacc0daef9448cd4c05f2e0c2fa`.
Restore the exact source bytes in a finally block after each experiment, rebuild
the normal DLL, then rerun all normal checks. The committed runner does not
apply these mutations or install any dependency.

## Prerequisites and normal verification

Follow [the ABI README](../../README.md) and [the offline runner](../run.ps1).
The fixture is the locally built parent crate's `owner_child.exe`. Required
absolute DLL/child paths and caches must refer to that checkout's artifacts.
Use only existing Rust/Go/C toolchains; disable automatic toolchain/module/crate
fetches. All writable files must stay in the approved work area.

From the `ffi` directory, with CARGO_TARGET_DIR and the Go environment set as in
the README:

```powershell
cargo build --manifest-path ../Cargo.toml --offline --locked --bin owner_child
cargo build --offline --locked
$env:MCPHUB_RX02_FFI_DLL = Join-Path $env:CARGO_TARGET_DIR 'debug/mcphub_owner_ffi_spike.dll'
$env:MCPHUB_RX02_FFI_CHILD = Join-Path $env:CARGO_TARGET_DIR 'debug/owner_child.exe'
Push-Location go
try { go test -race . -count=3 -timeout=90s } finally { Pop-Location }
```

Before a mutation, save the target file bytes, confirm the named source anchor
occurs exactly once, and ensure no other build/test process is using the same
artifact path. Capturing bytes only in process memory is sufficient for a
synchronous finally restoration; Git is the durable reference for recovery.
Preserve command, source identity, actual exit code and bounded sanitized output.
A failed assertion is different from an unrelated compilation/load failure.

## 1. Ignore the requested stream tuple

In `src/lib.rs`, in the single call to
`NativeOwner::create_suspended_with_context`, replace its `stdio` argument with
`None`. Do not change the constructor, fixture or test expectations. Build the
DLL and run from `go`:

```powershell
go test . -run '^TestStdio(CopiesNonInheritableSourcesAndPreservesOwnership|SharedOutputUsesOneUnderlyingFilePosition)$' -count=1 -timeout=30s
```

Expected exit: **1**, with behavior assertions failing. Recorded tests observed
pending children rather than the intended stream completion. The test's own
owner cleanup then stops/settles them; cleanup is not used to turn pending into
success. See [build](09-ignore-stdio-build.log) and [failure](10-ignore-stdio-negative.log).
The unused-variable warning in this deliberately broken DLL is not a warning in
the restored implementation.

## 2. Hide temporary-copy close failures

In `go/stdio_windows.go`, replace the unique final
`return errors.Join(failures...)` in `stdioCopies.close` with `return nil`.
This mutation is in Go, so no DLL rebuild is needed. Run:

```powershell
go test . -run '^TestStdioTemporaryCloseFailuresRemainSeparateAndAreNotRetried$' -count=1 -timeout=30s
```

Expected exit: **1**. Each of the seven nonempty protection masks must detect
hidden failure. The test independently saves its exact duplicated handles,
unprotects only those test-owned still-live objects and closes them after
capturing the result. It never retries arbitrary stale handles in product code.
See [recorded failure](12-hidden-close-negative.log).

## 3. Corrupt only the create-result metadata

In `src/lib.rs`, insert `out.size = 47;` immediately after the unique successful
create `snapshot(&owner, out);` statement. The actual owner is still transferred.
Build the DLL, set the selected opt-in case, and run from `go`:

```powershell
$env:MCPHUB_FFI_FAULT='stdio-metadata'
go test -tags ffi_faults . -run '^TestStdioMalformedCreateRetainsOwnerAndSourceFiles$' -count=1 -timeout=30s
```

Expected exit: **0** for this fault-aware test. It must receive a non-nil owner
and an error, preserve the source files, reject DLL unload while the owner is
live, and settle it explicitly. This does not test arbitrary invalid pointers.
See [record](14-metadata-owner-preserved.log).

## 4. Remove only the additive stdio symbol

In `src/lib.rs`, rename `fn mh_create_io1(` to `fn mh_absent_create_io1(`.
Build the owned DLL; do not change the C/Go caller. Run:

```powershell
$env:MCPHUB_FFI_FAULT='stdio-no-symbol'
go test -tags ffi_faults . -run '^TestStdioMissingExportIsNotSilentlyIgnored$' -count=1 -timeout=30s
```

Expected exit: **0** for the fault-aware test and native missing-symbol code127
inside its assertion. No fallback to a create API without stdio is permitted.
This deliberately renamed artifact is not presented as a historical released
binary. See [record](16-missing-export-refused.log).

## Restoration and durable evidence

Restore every modified file byte-for-byte, clear MCPHUB_FFI_FAULT and rebuild
normal debug/release artifacts. Run the committed verification script with a
new RunName. Verify source manifests match the files to be committed; no
mutation hook or altered DLL belongs in the PR or installation. Commit tests,
this explanation, failures, final output and hashes, not the rebuildable DLLs.

The [restored focused run](18-restored-stdio-race.log) and
[full final run](../runs/stdio-2026-11-14-final/COMPLETED.txt) followed these controls.
The actual Go-pipe scenario was added before the full final run; it is distinct
from the earlier raw synchronous Windows-pipe scenario.
