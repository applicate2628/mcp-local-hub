# Context-aware settlement and native pipe lifecycle

**Implementation commit:** `e973ee2eabb638b6a0372066b6381484ce015dc9`.
**Starting head:** `0bcb5f0a1044f81dc3d28019ebfc353cc0ef10c7`.
**PR:** [#631](https://github.com/applicate2628/mcp-local-hub/pull/631), still a dependent experimental draft on unchanged [#625](https://github.com/applicate2628/mcp-local-hub/pull/625).

## Contents

1. [Result and scope](#result-and-scope)
2. [Contract](#contract)
3. [Verified behavior](#verified-behavior)
4. [Negative controls and corrected assumptions](#negative-controls-and-corrected-assumptions)
5. [Self-review and methods](#self-review-and-methods)
6. [Reproduction and limitations](#reproduction-and-limitations)

## Result and scope

Added `Owner.FinishContext(ctx)` in the Go calling layer and qualified completion of actual caller-owned Windows pipe operations. The method reuses the existing Rust native owner and the existing ABI call/result mapping. It introduces **no native API or dependency changes**, no extra process owner, no asynchronous native-wait thread, no implicit stop/release/stream close and no retry after an ambiguous effect.

The implementation commit has four Go files: the observer, a six-line extraction of the already mutex-protected native dispatcher, and two test files. The native library, Rust ABI implementation/header, root product, installer, dependency manifests/locks and parent PR #625 remain unchanged. Documentation and versioned verification form a separate commit.

This is a bounded addition to the experiment, **not** a complete I/O scheduler, product integration, native Unix backend, measured performance improvement or independent security acceptance.

## Contract

**Table 1. Facts that remain separate.**

| Fact | Meaning |
|---|---|
| Observer context expires | This caller stops waiting; the existing operation and owner are not automatically stopped |
| A native probe returns Pending | The same owner is retained; any known root exit/active count is returned, not replaced with fictional completion |
| Owner mutex is occupied | The observer may cancel without acquiring it; no owner fields are accessed after failed TryLock |
| Native probe returns terminal/error | The actually observed outcome is preserved; a concurrent context cancellation does not erase it |
| An endpoint owner requests interruption | It controls that endpoint and must observe/join the read/write operation; this authority is not inferred from a generic Reader/Closer |
| Root process has exited | Descendants may still exist and retain writers; root exit is not output EOF |
| Native owner has settled | Buffered output may still need reading; settlement does not discard caller-owned bytes or close its readers |
| Error includes custom cancellation cause | Both the standard cancellation/deadline identity and the distinct custom cause remain discoverable |

The observer performs `Finish(0)` only after acquiring the existing mutex and checking its context again. It yields using a reused 10-millisecond timer between probes. This is a chosen polling interval, **not** a worst-case cancellation-latency guarantee. An OS call already entered, scheduling delay or other code's behavior are not made forcibly interruptible.

There is no worker goroutine in the production observer method. Native work remains serialized. A blocked policy/native call started by a different caller remains that caller's responsibility even after this observer returns. No ownership is discarded to make a deadline appear satisfied.

## Verified behavior

**Table 2. Eight new observer test functions.**

| Scenario | Evidence |
|---|---|
| Pre-canceled custom context | No effect or native observation; owner remains usable; original cause and `context.Canceled` both present |
| Nil context | Typed argument rejection, no admission consumption |
| Deadline with suspended root | Pending outcome and retained owner; explicit later stop determines the actual exit code |
| Busy owner mutex | Cancellation returns before the unrelated lock holder releases it |
| Root exited with code7, descendant alive | Known root result and active count survive cancellation; later stop does not overwrite code7 |
| Two observers | One context expires; the other can still observe explicitly requested settlement |
| Already terminal owner | Pre-canceled observation does not consume or mutate cached result; true exit259 stays a known exit code |
| Actual checked-admission callback holding the lock | Observer cancels without resuming or canceling that policy; its eventual permitted process result remains intact |

**Table 3. Five new native I/O test functions.**

| Scenario | Evidence |
|---|---|
| Parent Write pending to a suspended child's pipe | Canceling observation leaves Write pending; only its actual owner's Close interrupts it; byte count/error and operation join are observed |
| Canceled observer followed by normal input/output completion | Later explicit input EOF lets the same child complete naturally; output is exact binary data and not force-closed |
| Root exit with descendant writer | Output reader remains pending until explicit tree stop; root code7 survives; then native EOF is observed |
| Reading final data after native settlement | The previously unread complete bounded output remains available |
| Parent read interrupted by its endpoint owner | The operation reports `os.ErrClosed` and is joined; the child remains pending until explicit Stop |

These tests use finite owned children, real Go pipes, bounded buffers and exact original owner handles. Readiness/available data and operation-completion channels are separately observed. Timers in native tests are watchdogs/deadlines, not stand-ins for a proof of which process or handle completed. Test cleanup cannot make a failed behavior assertion pass.

### Complete experimental suite

The [implementation run](../runs/cancellation-2026-11-14-implementation/steps.json) completed every expected command. It ran on the starting head with the implementation/documentation overlay identified by [source hashes](../runs/cancellation-2026-11-14-implementation/source-hashes.json); those code bytes match `e973ee2`. It is not described as a clean checkout of a future evidence commit.

| Layer | Recorded result |
|---|---|
| Full Go experimental module | **47 top-level test functions, 31 nested cases, 0 skipped** |
| Debug DLL | All normal Go tests with race detector, three repetitions, passed |
| Release DLL | All normal Go tests with race detector, three repetitions, passed |
| Go memory/static/format checks | `cgocheck2`, `go vet` and gofmt checks passed |
| Rust ABI | **14 unit tests** in debug and release, Clippy and formatting passed |
| Unchanged native owner | **53 checks** in debug and release, Clippy and formatting passed |
| Verification runner | All22 steps matched expected exit codes; step00 intentionally expects7 to test exit-code capture |

This is the full experimental module/crate verification, not the entire Hub repository or installed-system acceptance. Go race instrumentation is evidence for executed Go paths, not a proof of every Rust/C/native schedule.

## Negative controls and corrected assumptions

[Exact mutation/reproduction instructions](MUTATIONS.md) and both scripts are committed alongside the raw logs and their input hashes.

The compiling initial stub performed one `Finish(0)` and ignored context. It failed all seven initial observer cases. Three subsequent deliberate regressions were independently detected: blocking instead of cancelable mutex acquisition, implicit Stop on observer cancellation, and erasure of the previously known outcome. Sources were restored exactly; the complete selected thirteen-test set passed again.

The first pipe qualification failed two assumptions of the **test**. The OS could expose the entire queued write through PeekNamedPipe while the call was still pending, so bytes-available is no longer used as a substitute for completion. Also, the tested Windows os.Pipe rejected read deadlines. Tests instead observe the actual completion and the endpoint owner's Close/join. Failed output is preserved in `native-io-initial.log`; it is not presented as two runtime library bugs.

Read-only diagnosis initially included a PowerShell quoting mistake and a guessed source-file name that did not exist; both failed without mutations and were corrected using the installed `go env GOROOT` and `file_windows.go`/`internal/poll/fd_windows.go`. No installed Go source or machine configuration changed. No additional dependency was installed to resolve the test failures.

## Self-review and methods

**Table 4. Thirteen selected, read and applied skills.**

| Skill | Concrete contribution |
|---|---|
| Using Superpowers | Select task-specific methods instead of a fixed ceremonial checklist |
| Executing Plans | Recover state from the live PR and committed ledger; do not repeat already completed stdio work |
| Test-Driven Development | Compiling stub RED, minimal GREEN, independently failing mutation controls |
| Using Git Worktrees | Verify the existing private clone/branch; no mutation of the primary checkout |
| API and Interface Design | Additive observer method; cancellation is not implicit operation authority |
| Architecture Review | Preserve one native owner, distinguish process/output/drain/cleanup, explicit remaining gates |
| Go Concurrency Lifecycle | Every production wait and test goroutine has an owner/termination path; no orphan waiter |
| Go Testing and Verification | Real kernel integration evidence; no deterministic fake-clock claim for external child I/O |
| Go Security Hardening | Only own original process/pipe objects; no PID-only cleanup or closing arbitrary borrowed streams |
| Brainstorming | Continue the already approved experiment; reject background long-wait wrapper and automatic kill policies |
| Systematic Debugging | Investigate native pipe observations and unsupported deadline before correcting tests |
| Ponytail Review | Reuse timer/mutex/native mapper; avoid new scheduler, abstraction layer or dependency |
| Verification Before Completion | Fresh full suites, source-hash checks and separate implementation/evidence commits |

Concrete review conclusions: the native probe helper is shared with existing synchronous calls; unsuccessful TryLock is not permission to inspect state; any already observed native terminal/error result is retained; other callers/streams cannot be interrupted by observer policy; process and I/O finality are tested independently. No unnecessary extra runtime component or speculative configurability was added.

No independent external reviewer or agent ran. Native/Rust panic/allocator abort remains an in-process host-failure risk from the existing FFI contract. Complete kernel cleanup-failure propagation through this new method, hostile/invalid raw pointers, allocation exhaustion, arbitrary blocking sinks and every platform/I/O profile were not newly qualified.

## Reproduction and limitations

Use an owned checkout and existing installed toolchain/cache, with all writable paths below `D:\dev\ChatGPT`. Missing dependencies do not authorize a download. The committed full runner sets environment/paths and records commands/exits/hashes without an uncommitted setup script:

```powershell
# From experiments/rust-process-owner/ffi
.\verification\run.ps1 -LabRoot 'D:\dev\ChatGPT\your-owned-lab' `
    -CCompiler 'C:\path\to\existing\gcc.exe' `
    -RunName 'new-cancellation-full-run'
```

For focused and negative runs, follow [MUTATIONS.md](MUTATIONS.md). Existing log names cannot be overwritten. Compiled artifacts and caches are rebuildable and intentionally not committed; source, command scripts, raw failures/successes, hash manifests, decisions and limitations are durable in this PR.

Still open: complete product admission fields/policy, broad descriptor profiles and global inheritance coordination, actual product pumps/backpressure/drain policy, interrupting native calls already entered, additional foreign kernel/allocator/panic/loader faults, native Linux/macOS backends, workload/performance comparisons, independent lifecycle/security review, packaging and installed acceptance. Neither a pending timeout nor output EOF authorizes repeating a remote side effect.

Primary references used: [Go Mutex/TryLock](https://pkg.go.dev/sync#Mutex.TryLock), [Go context.Cause](https://pkg.go.dev/context#Cause), [CancelIoEx completion semantics](https://learn.microsoft.com/en-us/windows/win32/api/ioapiset/nf-ioapiset-cancelioex), [PeekNamedPipe data observation](https://learn.microsoft.com/en-us/windows/win32/api/namedpipeapi/nf-namedpipeapi-peeknamedpipe). References explain contracts; the actual recorded tests establish only the listed exercised behavior.

No merge, release, installation, active-primary-checkout/configuration change, paid/model/vendor run or explicit workflow/bot dispatch occurred. The working PC is disposable; necessary results and recovery instructions are part of the PR.

### Final committed-code verification

A second complete run is preserved at
[final steps](../runs/cancellation-2026-11-14-final/steps.json),
[final source hashes](../runs/cancellation-2026-11-14-final/source-hashes.json),
[final Go counts](../runs/cancellation-2026-11-14-final/go-counts.json), and
[completion marker](../runs/cancellation-2026-11-14-final/COMPLETED.txt).
It identifies implementation commit `e973ee2eabb638b6a0372066b6381484ce015dc9`
and a documentation/evidence-only overlay. All22 steps matched; the same47/31/0
Go counts,14 ABI tests and53 native-parent checks passed. Every recorded source
hash was independently compared with the files being published.

The final negative-control script was also rerun after strengthening the
implicit-stop mutation to cover every cancellation return rather than one
select branch. All three wrong behaviors were detected; exact source restoration
and the full13-case selected race suite passed again. `negative-final-*` logs
are retained alongside `negative-1-*`. No implementation change followed the
full suite; only evidence/reproduction documentation was completed.
### Publication-format check

The first staged evidence check rejected CRLF/trailing whitespace in the
artifact hash listing and baseline header. The failure was reproduced and
preserved in `publication-format-check.log`. Only line endings and trailing
whitespace were normalized; `publication-normalization.json` records before/after
hashes. Commands, result values and conclusions were not rewritten as successes.
The final staged check was repeated after normalization. Implementation and
passed source manifests were unchanged; this was not a product test failure.
The newly generated normalization manifest also inherited PowerShell JSON's
CRLF output; a second staged check caught it. That failure is preserved in
publication-json-format-check.log. All newly generated metadata is normalized
through the same LF-only writer before the final staged check.
