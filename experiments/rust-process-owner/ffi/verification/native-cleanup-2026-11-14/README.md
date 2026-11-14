# Actual native cleanup failure across the Go/Rust boundary

This qualification belongs to PR #631 and starts from `a59a867d0d8a44d156ab783c78f91a02fe4d83e5`. It exercises the existing native implementation; it is **not a new production recovery API**. Source, commands and results are durable in Git, not dependent on a local PC folder.

## Contents

1. [What is being proved](#what-is-being-proved)
2. [How the native failure is induced](#how-the-native-failure-is-induced)
3. [Observation and recovery authority](#observation-and-recovery-authority)
4. [Reproduction and negative controls](#reproduction-and-negative-controls)
5. [Limits and sources](#limits-and-sources)

## What is being proved

Previously the native owner had its own kernel-failure tests, while the foreign function interface (FFI) separately tested the binary encoding. Those two facts did not establish the complete Windows failure -> Rust settlement -> dynamic-link library (DLL) -> Go path.

**Table 1. End-to-end contract.**

| Observation | Required result |
|---|---|
| Real CloseHandle failure on selected original resources | Go receives CleanupFailed, not Complete or Pending |
| Each selected resource fails | Every failure bit and its real Windows error code survives |
| Child has already exited with code259 | The code is retained as a known exit result, not confused with the Windows STILL_ACTIVE numeric sentinel |
| Finish is repeated | The recorded result stays identical |
| FinishContext is used instead of Finish | The same terminal cleanup failure is preserved |
| Release is called after failed cleanup | Rust wrapper allocation can be freed, but no claim of successful native-resource cleanup follows |
| The test subsequently reclaims its known protected resources | Historical failure is not retroactively rewritten as success |
| Ordinary DLL is loaded | No qualification-only handle accessor is exported |

The primary matrix covers every nonempty subset of thread/process/Job failures: masks1 through7, through both Finish and FinishContext. A second function checks the cached outcome after explicit test-only recovery. A third function checks the absence of the fixture export in ordinary debug/release artifacts. They are opt-in tests under build tag `ffi_native_faults`, not silently skipped normal tests.

## How the native failure is induced

The [runner](run.ps1) copies only the existing native and FFI crate inputs into a fresh owned directory below the approved lab. It appends [owner_slots.rs](owner_slots.rs) to the copied native Windows module and [ffi_slots.rs](ffi_slots.rs) to the copied DLL module. These additions are read-only accessors: they copy three original slot values to a pinned caller buffer. They do not create, duplicate, close, substitute or intercept resources and do not replace the normal close/finish/result code.

The ordinary repository implementation, ABI header, dependency locks and normal DLLs are left unchanged; their hashes are checked again after the run. There is no runtime environment toggle, Cargo feature, global fault registry or production test export. Tests demand the explicitly selected instrumented DLL; a separate control requires missing-symbol error127 from normal artifacts.

Windows `SetHandleInformation` applies `HANDLE_FLAG_PROTECT_FROM_CLOSE` to exact original handles belonging to this fixture. The test first observes a real failing CloseHandle and records its native error, then invokes the actual library cleanup and compares the received errors. It does not hardcode a special error6 success path. The tested Windows host reported code6 for this protection mechanism; that is an observed result, not a claim that every possible close failure has the same code.

Normal control flow is unchanged. The child is finite, hidden, locally built, and exits before native cleanup. No installed Hub, vendor program, user project, network operation, shell script or external model is launched.

## Observation and recovery authority

The raw handle values are exposed **only in this instrumented test artifact**. The test holds the Go owner mutex while copying the slots and pins the output array. It does not reopen a process by PID, scan the process list, synthesize an OwnedHandle alias, or publish these pointers in the product API.

A protected original cannot be closed by CloseHandle while that protection remains set. The fixture verifies that fact before its recovery. It then checks native slot ownership rather than trusting decoded status alone:

- If the owner still holds the original slot, the test only clears the protection it added; normal owner cleanup remains responsible.
- If native finish has detached the slot after a failed close, the test may clear protection and close that **known-live original** once.
- If protection or slot identity is unexpectedly lost, it reports a failure and refuses a potentially stale numeric close.

This exceptional knowledge belongs to the deliberately constructed fixture. It does **not** authorize production code to retry arbitrary failed closes, and it does not promise that arbitrary OS errors are recoverable. Assertions observe the actual result before test recovery. Cleanup cannot turn an incorrect result into a passed assertion.

After recovery the test makes no further syscall through the old handle values. It only asks the owner for its cached terminal outcome and releases the wrapper. No global handle-leak metric or proof of every concurrent schedule is inferred from this experiment.

## Reproduction and negative controls

Prerequisites are the already installed Windows x86_64 Rust/MSVC and Go/cgo toolchains, a working local C compiler, and the existing Cargo cache. The normal DLLs and finite `owner_child` must first be built by the existing [full runner](../run.ps1). Nothing is downloaded or installed. All writable output/cache/temporary paths remain under `D:\dev\ChatGPT`.

From this directory:

```powershell
.\run.ps1 -LabRoot 'D:\dev\ChatGPT\your-owned-lab' `
    -CCompiler 'C:\path\to\existing\gcc.exe' -RunName 'my-positive'
.\run.ps1 -LabRoot 'D:\dev\ChatGPT\your-owned-lab' `
    -CCompiler 'C:\path\to\existing\gcc.exe' -RunName 'my-failure-negative' `
    -Mutation erase-failure
.\run.ps1 -LabRoot 'D:\dev\ChatGPT\your-owned-lab' `
    -CCompiler 'C:\path\to\existing\gcc.exe' -RunName 'my-mask-negative' `
    -Mutation erase-mask
```

Each run refuses existing output names. Commands, actual/expected exits, source hashes, instrumented-copy hashes, ordinary artifact hashes and logs are recorded in `verification/runs/native-cleanup-<RunName>/`. A completion marker is written only if all expected results match; the explicit exit7 control tests failure-code recording.

**Table 2. Deliberately wrong encoding controls, applied only in the disposable copy.**

| Mutation | Intended test failure |
|---|---|
| Replace the cleanup state's assignment by Complete | Each induced native failure is falsely labelled success and the Go assertion fails |
| Remove the cleanup-mask bits while preserving individual error codes | Loss of the failure set is detected independently from state/code preservation |

The normal CloseHandle implementation is never stubbed, even in these negative controls. Code1 from the selected negative Go test command is expected; source compilation or setup failure is not counted as a successful control. Inspect the recorded outcome assertions, not only the exit code. The final positive run uses a newly copied unmodified encoding again.

## Limits and sources

Qualified scope: these original Windows thread/process/Job resources, all seven protected-close combinations, the exercised finite child and both current observation APIs. Not newly qualified: arbitrary kernel failure classes, descriptor races/interference, all dynamic-loader or allocator/panic failures, power loss, product recovery policy, other platforms, real-workload speedup, complete deployment or independent review. The test DLL is intentionally unsafe for product use and is not installed.

References:

1. [Microsoft SetHandleInformation](https://learn.microsoft.com/en-us/windows/win32/api/handleapi/nf-handleapi-sethandleinformation): documented protection bit and object types.
2. [Microsoft CloseHandle](https://learn.microsoft.com/en-us/windows/win32/api/handleapi/nf-handleapi-closehandle): failure reporting, lifetime and repeated-close considerations.
3. [Rust IntoRawHandle](https://doc.rust-lang.org/std/os/windows/io/trait.IntoRawHandle.html): raw-handle ownership transfer at the existing close boundary.
4. [Current FFI contract](../../README.md), [native owner](../../../README.md), and [overall plan #623](https://github.com/applicate2628/mcp-local-hub/pull/623).

An API reference explains the mechanism; only the committed run evidence supports the specific exercised results.
