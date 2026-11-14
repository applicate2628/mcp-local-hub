# Go ↔ Rust owner: experimental C ABI

**Status:** isolated Windows x86_64 calling-side experiment. This is not a Hub backend, a stable product SDK, a sandbox, or an automatic replacement for Go process ownership. It depends on the unchanged [native-owner experiment](../README.md) from PR #625; the foreign-boundary PR targets that branch so the native implementation is not duplicated in its review diff.

## Contents

1. [Scope and ownership](#scope-and-ownership)
2. [ABI and outcomes](#abi-and-outcomes)
3. [Explicit environment](#explicit-environment)
4. [Explicit standard streams](#explicit-standard-streams)
5. [Checked original-process admission](#checked-original-process-admission)
6. [Memory, concurrency and failures](#memory-concurrency-and-failures)
7. [Cancelable observation and pipe settlement](#cancelable-observation-and-pipe-settlement)
8. [Native cleanup across the DLL boundary](#native-cleanup-across-the-dll-boundary)
9. [Loader refusals and module ownership](#loader-refusals-and-module-ownership)
10. [Evidence](#evidence)
11. [Reproduction](#reproduction)
12. [Remaining gates and references](#remaining-gates-and-references)

## Scope and ownership

**One actual creation engine:** Go caller → small C dynamic-call shim → this Rust DLL → the existing `NativeOwner` → Windows. The shim does not spawn, supervise or close process handles. Rust owns its allocation and the original Job/process/thread resources. Go owns the library lifetime and the single opaque owner reference, and serializes each owner's calls.

The boundary creates a suspended child with an absolute executable and explicit absolute working directory. The original `New` / `mh_create1` retains its **complete empty environment** behavior; additive `NewWithEnvironment` / `mh_create_env1` supplies a complete explicit set of ordinary environment entries. Those two paths retain no-redirection behavior. `NewWithStdio` / `mh_create_io1` now explicitly combine the full environment with three standard streams through the same native creation core. This is not full parity with current Go launch policy or a complete I/O pump. No parent environment/cwd mutation or implicit inheritance fallback is added.

Go can supply an allow/deny decision, request stop, observe finite settlement, then explicitly release the terminal owner. The boolean is **not** a process-identity or authorization proof. Raw `ProcessView` and its OS handle are never exported. `AdmitChecked` now invokes Go policy with a copied PID/image-name observation from that original view before native resume; the complete product admission policy and additional identity fields remain separate gates. The returned PID is diagnostic, never authority to reopen/kill a process.

**Table 1. Owner transitions.**

| Situation | Contract |
|---|---|
| Successful create | One opaque Rust allocation is transferred into the caller's initially null slot |
| Denied admission | One-shot decision is consumed; the same suspended owner remains available for explicit stop/finish |
| Pending finish or ordinary error | Owner remains live; no implicit drop, repeat spawn, or alternate backend |
| Root exited but descendant remains | Root code stays known, settlement is pending; the parent result is not overwritten by descendant stop |
| Terminal complete/cleanup-failed result | Native owner records the result; repeated finish returns it without replaying cleanup |
| Release before terminal finish | Refused, pointer unchanged; no hidden stop or attempt to finish |
| Release after terminal finish | Rust frees its own allocation, clears the caller slot; the result of memory release is distinct from the reported native cleanup state |
| DLL unload while any opaque owner exists | Go wrapper rejects unload, including terminal-but-unreleased owners |

## ABI and outcomes

The manually maintained [C header](include/owner_v1.h) is small and verified by both C compile-time assertions and Rust layout tests. `mh_version1` negotiates ABI 1; creation carries a version/reserved field and an exact request byte size. Every operation receives an exact output-buffer size. A mismatch is rejected before native work; no forward-layout guessing or variable-sized Rust types cross the boundary.

Exports: `mh_create1`, `mh_admit1`, `mh_stop1`, `mh_finish1`, `mh_release1`, and additive `mh_admit_checked1` / `mh_create_env1` / `mh_create_io1`. The ABI-1 structure layouts stay unchanged. This updated Go loader requires these additive exports and refuses older DLLs without them; it never silently falls back to unchecked admission. Symbols and numeric values in the header are the source of truth for this experiment, not a promise to keep them as the future product API.

**Table 2. Separate facts in `mh_out`.**

| Field | Interpretation |
|---|---|
| Return status | Call accepted, bad argument/version, native failure, denied decision, invalid state, or release-not-settled |
| `state` | Unknown, pending, complete, or cleanup-failed; status zero alone does **not** mean completed execution or successful cleanup |
| `flags` / `root_exit` | Root exit code is valid only with `MH_ROOT_KNOWN`; 259 is not used as an unknown sentinel |
| `flags` / `active` | Descendant count is meaningful only when `MH_ACTIVE_KNOWN` is set |
| `native_error` | Original OS code for a native-error status, not a later GetLastError from the C/Go shim |
| `cleanup_mask` / `cleanup_codes` | Up to three separate thread/process/Job close failures, preserving the root code |
| `pid` | Original process diagnostic identifier, not a capability |

Bounds are explicit experimental budgets: no more than 64 argument views, no embedded NUL, at most 32,766 UTF-16 units per string and in the combined executable/cwd/argument input. Finite wait accepts 0..60,000 milliseconds. These are not claims about universal OS or SDK limits. The explicit environment has a separate total block budget below; streaming buffers and arbitrary long-lived asynchronous calls remain outside this ABI slice. Explicit stdio is a borrowed native-handle view, not a shared buffer protocol.

### Returned-outcome validation

The common Go decoder now checks ABI1 header size/version on **every** return
status, then checks the defined state/flag/code relationships. It rejects unknown
statuses/states/flags/cleanup slots, unflagged values, terminal states with no
known root or with active members, and cleanup masks inconsistent with their
codes. A successful `Finish` must include a real settlement state, not UNKNOWN.
These checks clarify the existing zero-initialized ABI1 value contract; layouts,
exported functions and the Rust owner are unchanged.

`ErrInvalidOutcome` means the returned `Outcome` is zero because it is **not a
trusted observation**, not because no effect occurred. A separately returned
nonzero call status is preserved. An OS error field is retained only where its
layout is recognized and its status identifies a native failure; an unknown
layout does not authorize interpreting that field. Typed `CallError` and the
protocol failure remain discoverable with `errors.As`/`errors.Is`.

Ownership transfer and an already cleared release slot remain independent of
response validity. Do not retry creation, discard an owner, or infer that a
resource remains allocated solely from an error response. Valid CleanupFailed
reports with any of the seven nonempty thread/process/Job masks are accepted
without turning them into Complete or losing exit259 and the individual codes.

The [outcome report](verification/outcome-2026-11-14/RESULTS.md) distinguishes
17 deliberately inconsistent return variants, seven valid serialized cleanup
reports, two prior handoff regressions, and separately repeated **actual**
Windows close-failure tests. The fixture DLL changes only return values after
real native settlement; it is not evidence that the ordinary Rust producer
emitted those contradictions and not protection from hostile in-process code.

The [merged-master admission comparison](verification/outcome-2026-11-14/ADMISSION-PARITY.md)
records that #616 is now merged. The original PID/image observation still lacks
fields required by the accepted Go creator-identity contract. Those are explicit
remaining gates, not permissions to fill missing identity with defaults.

## Explicit environment

`NewWithEnvironment(program, cwd, args, []EnvironmentEntry{{Name, Value}, ...})`
supplies the complete environment through the same native creation core. The
original request and outcome layouts, `mh_version1`, and `mh_create1` behavior
are unchanged. New C view types (`mh_env_entry` and `mh_environment`) are sized
explicitly and checked on both sides; `mh_create_env1` is an additive symbol.

The following contract is intentional, not inferred from Go map iteration:

- Entries are an ordered slice, already strictly sorted by Windows ordinal
  case-insensitive name comparison. The existing Rust `LaunchContext` validates
  order/uniqueness using the native comparator. No duplicate first/last-wins rule,
  locale-specific sorting, or hidden Go uppercase conversion is added.
- Empty/nil is **empty**, not inherit. The caller explicitly selects any PATH,
  SystemRoot or vendor-required values. No overlay/secret/parent-variable merge
  or fallback is performed, even if a program cannot start with the chosen set.
- Ordinary names must be nonempty and contain neither '=' nor NUL. Values may be
  empty or contain '='. Hidden Windows per-drive `=X:` entries remain unsupported.
- At most **256 entries** and **32,766 UTF-16 units for the full block**, including
  each name/value, '=', entry terminator and final terminator. This is an
  experimental ABI budget, smaller than the native library's general budget,
  not a universal Windows/SDK limitation. Exactly empty has two NUL units.
- Go strings must be valid UTF-8. Invalid UTF-8 is now rejected at the **shared
  string boundary**, including legacy program/cwd/argument input; it is not
  silently replaced with a different character. Raw C UTF-16 input can preserve
  non-Unicode Windows units subject to the native name/shape constraints. The Go
  string adapter does not claim arbitrary non-Unicode Windows-string support.
- C arrays are zero-initialized, and strings are C-owned for the synchronous call.
  Rust checks count/size before copying, copies before native creation returns,
  and retains no Go/C input pointers. Caller mutation after return and Go GC
  cannot alter the child's selected values. Concurrent caller mutation during
  the call is prohibited, as for argv.

`NewWithEnvironment` can return an owner **and** an error after ownership already
transferred; retain every non-null owner exactly as with `New`. Its common
creation path preserves original native errors, checked admission, pending state,
late metadata handling and explicit release. This is not a new process manager.

The loader refuses a DLL without `mh_create_env1` rather than silently dropping
requested entries or switching to inherited/empty mode. Old ABI-1 consumers can
still use the unchanged original exports. Different child environments do not
require temporarily modifying the parent process environment or directory.

An environment block is not a sandbox. Windows and child runtimes may synthesize
additional entries after creation; only fake named values are inspected by tests,
not real secrets. Required SDK policy, environment overlay design, secret-memory
erasure and complete product launch parity are not claimed.

## Explicit standard streams

`NewWithStdio(program, cwd, args, environment, Stdio{Stdin, Stdout, Stderr})`
requires three non-nil caller-owned `*os.File` objects. The complete environment
retains the rules above; nil means empty, not inherit. Source files can start
non-inheritable. Go acquires at most three short-lived inheritable handle copies
with `DuplicateHandle(DUPLICATE_SAME_ACCESS)`, borrowing each source through
`SyscallConn().Control`. It does not race a naked `Fd()` with concurrent Close,
change original inheritance flags, transfer source ownership, or use
`DUPLICATE_CLOSE_SOURCE`.

The additive C entry point accepts `mh_io_context`: an environment view plus
three `uintptr_t` stream handles. Rust creates call-scoped BorrowedHandles and
passes them to **existing** BorrowedStdio/NativeOwner validation and the native
inheritance allowlist. This is not a second creator, process owner or descriptor
registry. Raw C callers must supply real live inheritable file/pipe handles;
null/pseudo-handle checks are not proof that arbitrary numbers are valid handles.

**Table 3. Standard-stream ownership and consequences.**

| Boundary | Contract |
|---|---|
| Original Go file | Remains caller-owned; its flags and lifetime are not silently changed |
| Temporary Go copy | One per slot, held only through synchronous create; close attempted once on success or partial acquisition failure |
| Child inheritance | Existing native explicit allowlist, not blanket inheritance of all parent handles |
| Shared stdout/stderr | Separate handle copies refer to the same underlying kernel object and share its file position; no data snapshot or independent offset is implied |
| After create returns | Child owns its inherited OS references; caller may close original sources before admission |
| Late reporting/copy-close error | A transferred Owner is still returned and must be settled; error does not authorize retry or automatic resume |
| Failed copy close | `StdioCopyError` records operation, stream index and original cause; failure is not proof that the resource was released |

Temporary-copy cleanup does not repeat a failed close using a possibly reused
numeric handle. Unknown or protected-close failures can leave a native copy
unreleased; they are reported, not hidden or claimed automatically recoverable.
This is distinct from the native process owner's thread/process/Job cleanup
outcome. The seven nonempty protected-close combinations are exercised on
**test-owned** copies; that test performs explicit recovery while it still knows
the exact protected objects. It does not grant the production adapter blanket
recovery authority over arbitrary stale handles.

Qualified cases use local regular files, native synchronous anonymous pipes
and ordinary Go `os.Pipe` for a bounded binary transfer. Pipe EOF proves there
is no hidden parent writer in those cases. It does not prove arbitrary overlapped
I/O, sockets, device handles, console support or full-duplex throughput. The
existing native category filter does not certify file access rights or every
FILE_TYPE_PIPE subtype. Caller must supply the right ends and access rights,
arrange I/O bounds/draining and avoid concurrent shared-position file operations.

The allowlist protects **this child**. Temporary copies are inheritable in the
parent during create; an unrelated concurrent native launcher that blindly
inherits all handles is outside this guarantee. No local mutex can silently
claim global coordination with uncooperative launchers. Product integration must
reconcile all launch paths or use a stronger architecture if that threat matters.
There is no shell, inherited-default fallback, I/O worker or finalizer added here.

## Checked original-process admission

`Owner.AdmitChecked(func(ProcessObservation) error)` runs policy synchronously **inside** the existing `NativeOwner::admit_and_resume` closure. The child has not been resumed by this owner yet. Rust calls `ProcessView.process_id()` and `ProcessView.image_path()` using the original retained process handle, not `OpenProcess(PID)` or a stored create-path string. Only the resulting PID and UTF-16 image name cross the callback; the Go handler copies the image into Go-owned memory before policy/GC/retention.

**Table 4. Checked admission semantics.**

| Event | Result |
|---|---|
| Policy returns nil | Existing native admission continues its own pre-resume liveness check and ResumeThread |
| Policy returns an error, including context cancellation | Native admission is rejected; original Go error and native denied status are both observable; the same owner remains for explicit stop/finish |
| Policy panics | Recovered on the Go side before returning through C/Rust; denied with `ErrAdmissionPanic`, without printing the panic payload |
| Nil policy | Rejected without consuming the native admission attempt |
| Admission/stop/release already consumed the valid state | Policy is not invoked; no replay or new process lookup |
| Bad callback metadata or repeated invocation | Rejected; previous policy failure is retained alongside `ErrAdmissionObservation`; policy is never evaluated twice |
| Raw C callback returns something other than 0 or 1 | Rejected with argument status, consuming the attempted admission; no resume |

A temporary `runtime/cgo.Handle` carries a Go-side policy record as an integer token. The DLL does not dereference the token, retain a Go pointer, or save a callback for later. The handle is deleted after the native call returns. The native UTF-16 slice exists only during the callback; policy receives a separate Go slice that it may retain. This is synchronous Windows interoperation, not permission for arbitrary Go code inside a Unix post-fork child.

The owner mutex stays held through policy and native resume/denial, so another owner call cannot race its release. **Policy must return and must not reenter this owner, wait for another operation on it, call `runtime.Goexit`, use longjmp, or let an exception escape through C.** A blocked policy is not made cancellable by this API. There is no guarantee against external actors independently altering a process. Panics in this Go policy are contained, but native/Rust panic or fatal runtime failure still has the fail-stop limitation below.

PID/image name is a bounded observation, **not** a hash/signature, executable-file identity guarantee, process-token/user identity, creation-time attestation or a sandbox. Go supplies the actual decision rules. The old boolean `Admit` remains only as the original low-level experiment; it is not transformed into checked product policy. Product admission parity is not claimed by a successful path comparison.

## Memory, concurrency and failures

Input text and view arrays are allocated on the **C heap**, not pinned long-lived Go objects. Rust validates structural bounds and copies input before creation; the caller frees C buffers before returning `New`. No Go pointer or callback is retained by Rust, and Go never frees Rust memory with C `free`. The raw UTF-16 ABI can represent Windows units; this Go string adapter handles ordinary Unicode strings and does not claim lossless arbitrary non-Unicode Windows path conversion.

Non-null pointers must identify real, aligned, initialized input allocations of the declared length. Output storage must be writable and disjoint. The owner pointer must be live and uniquely operated on, with no concurrent release, aliases or use after release. Checking null/alignment/length does **not** establish arbitrary pointer validity or make malicious native callers safe. There is no speculative global owner registry. The Go wrapper provides a mutex and clears its pointer; raw C callers must obey the same lifetime and exclusivity contract themselves. Go `Owner` and `Library` values must not be copied.

`Finish(milliseconds)` retains its synchronous bounded-wait contract with the owner lock held. An already entered native call is not forcibly interrupted by Go. Additive `FinishContext(ctx)` now offers non-destructive cancellation of observation through zero-timeout native probes; it does not convert arbitrary I/O or policy callbacks into cancelable work. See [the separate observation/operation contract](#cancelable-observation-and-pipe-settlement).

**Late result errors do not erase ownership facts.** If native creation transfers a non-null owner but response metadata is invalid, `New` returns **both that owner and an error**. The caller must retain any non-null owner even on error; it may not retry the operation. If release clears the slot but its response is malformed, Go still updates its live-owner count and returns the response error. This avoids both losing a live child and pretending an already deallocated object remains live. The protocol assumes a returned non-null pointer is real native ownership; arbitrary corrupt pointers are outside a safe in-process FFI contract.

There is no Go finalizer. Garbage collection cannot report native settlement, retry ambiguous cleanup or safely decide to kill a child. Explicit caller cleanup is required. `Release` frees the native wrapper; it does not reinterpret `CleanupFailed` as successful OS cleanup. Keep the terminal `Finish` outcome.

The DLL uses non-unwinding `extern "C"` and abort panic profiles. **Unexpected panic, allocation abort or native corruption may terminate the Go host.** No Rust panic travels through C into Go; Go policy panic is contained by the checked callback handler. Neither fact provides crash isolation or recovery from arbitrary native corruption. Expected errors are structured values. Deliberate panic/allocator-failure testing is not included in this iteration.

The C shim loads only the caller-supplied absolute DLL path with `LOAD_LIBRARY_SEARCH_DLL_LOAD_DIR | LOAD_LIBRARY_SEARCH_SYSTEM32`, without changing process-global DLL search settings. Absolute path and constrained dependency search do not prove trust, prevent concurrent replacement, or validate a signature/hash. Tests use the locally built DLL in the owned workspace; product binary admission, load/unload fault recovery and all transitive-dependency qualification remain separate.

## Cancelable observation and pipe settlement

`Owner.FinishContext(ctx)` waits for native settlement, a native error, or cancellation **of this observer**. It does not call `Stop`, release the owner, close streams or retry a remote effect. The existing Rust owner, ABI and synchronous `Finish(milliseconds)` are unchanged.

**Table 5. Separate cancellation and completion facts.**

| Situation | Result |
|---|---|
| Nil context | Argument error before native observation |
| Context already canceled | Zero outcome plus the cancellation sentinel and custom cause; no native call |
| Owner busy in another native call or policy | Observation can cancel without acquiring that mutex or abandoning the other caller's operation |
| Native state remains pending | Return the last completed observation when cancellation arrives; owner still supports explicit stop/finish/release under its original contract |
| Root exited but descendant still alive | Retain known root code and active count; do not call the entire operation complete |
| Probe observes a native error or terminal result | Return that observed fact even if cancellation arrives concurrently; CleanupFailed is still a distinct terminal state, not success inferred from nil error |
| Another observer remains active | One observer's context cancellation does not cancel or release the owner or the other observer |
| Caller wants to stop the operation | Call the existing explicit `Stop`, then observe settlement; observation cancellation itself has no kill authority |

Implementation uses the existing mutex and the same native operation/result mapping, acquired with `TryLock`. No owner fields are read after a failed lock attempt. A reused timer yields between attempts (10 ms is the current polling interval, **not** a real-time latency guarantee or benchmark). No waiter goroutine is spawned by the method, and the mutex is released between probes. The context is rechecked after acquisition before new native work. A system call already entered, a stalled runtime, or arbitrary policy code are not forcibly interrupted. Fair scheduling and complete native-call cancellation are not claimed.

The returned outcome is this call's last **completed** observation; it can be stale relative to another concurrent caller, and zero means no observation was obtained. It does not add a second authoritative state machine. `Context.Err` and a distinct `Context.Cause` are both preserved. The native library remains loaded because the same owner allocation remains live until explicit terminal release.

### Native pipe qualification, not a new I/O framework

Five integration cases use only owned `os.Pipe` endpoints and the existing finite child fixture. They separately test: pending parent write interrupted by its lawful endpoint owner; canceled observation followed by normal input EOF and complete final output; root exit with a descendant retaining the output writer; reading buffered final bytes after process settlement; and interruption/join of a pending read without implicitly stopping the child.

The child, native process wait, read/write operation and output drain are different owners/outcomes. In the measured scenarios, closing a test-owned pipe endpoint makes its pending Go operation return; the test explicitly joins that goroutine and preserves its byte count and error. The production method does **not** infer permission from an `io.Closer`, close caller streams or create a background copy loop. Tests measure results before their safety cleanup; a cleanup path cannot convert a failed assertion into success.

The first native I/O run exposed two incorrect test assumptions: `PeekNamedPipe` can expose all queued bytes while Write is still pending, and this Windows `os.Pipe` does not support `SetReadDeadline`. The corrected tests observe the actual operation completion channel, and use only the actual endpoint owner's Close/join for failure cleanup. A returned byte count and an error are kept separately. Logs of both the failed and corrected test versions are retained.

This qualifies these Windows pipe profiles and finite payloads, not all files/sockets/consoles/device handles or all native I/O cancellation APIs. There is no universal pump, backpressure policy, hard cancellation deadline, automatic tree shutdown, remote-effect rollback or full product lifecycle admission. Real operations may need caller-specific authority, buffering, drain and cancellation mechanisms.

## Native cleanup across the DLL boundary

The [protected-close qualification](verification/native-cleanup-2026-11-14/RESULTS.md)
now exercises actual Windows CloseHandle failures through the existing Rust DLL
into Go, rather than inferring that path from separate native/unit tests. It
covers all seven nonempty thread/process/Job failure masks through both Finish
and FinishContext, preserves root exit259 and each real error, and checks cached
failure after explicit test recovery. The ordinary runtime and ABI are unchanged.

The [reproducible runner](verification/native-cleanup-2026-11-14/run.ps1) adds a
read-only accessor only in a disposable source copy, not the normal DLL. Tests
verify that normal debug/release artifacts lack that accessor. Test-only recovery
of continuously protected original handles is not general production recovery
or authority to retry unknown/stale numeric handles. See the [method and limits](verification/native-cleanup-2026-11-14/README.md).

Three functions are enabled only under `ffi_native_faults`: two behavioral tests
(with14 matrix subcases) and one ordinary-artifact absence control. They are
executed explicitly, not added to normal test counts or silently skipped. Other
kernel failure classes, loader/allocator/panic paths, Unix and product policy
remain separate gates.

## Loader refusals and module ownership

[Native loader qualification](verification/loader-2026-11-14/RESULTS.md) now
covers individually missing required exports, unsupported ABI versions, native
image/initialization/dependency failures, independent module references, Unicode
paths and the explicit dependency-search policy. Seven opt-in functions with20
subcases run against benign owned DLLs; the ordinary loader was not changed.

Three deliberately wrong bridge copies are rejected: a retained reference after
refusal, acceptance of a wrong version, and unintended CWD/PATH dependency
fallback. This is not a trust or sandbox guarantee: DLL initialization can run
before version/export checks. Binary authenticity and other loader/unload races
remain separate product gates. See the linked report for the corrected initial
test assumption and invalid first mutation attempt, both preserved as failures.

## Evidence

**Table 6. Independent verification layers.**

| Layer | What it establishes | What it does not establish |
|---|---|---|
| 49 normal Go test functions (47 existing functions plus 2 outcome-value contract functions) | Real cgo/C-shim/Rust-DLL calls; pre-admission absence of effects, copied Unicode args, pending ownership, deny/stop, native codes, repeated finish, release/unload order, concurrent Go callers, surviving descendant, empty env/cwd, budgets, original image evidence and one-shot policy/panic/cancellation handling, stream ownership/EOF and temporary-copy failure outcomes, cancelable observation, original-result preservation and owned pipe completion and consistent outcome values | Whole Hub integration, native Unix, raw foreign-pointer safety, or Rust/C race freedom |
| 14 Rust ABI unit tests | C layout constants including environment views, initialized output/short-buffer protection, bad versions/sizes, structural/overflow input rejection, owned raw UTF-16 copies, exact pending/native-error and seven cleanup-mask encodings | Actual OS cleanup failure transmitted through the foreign caller; those native failures were separately tested in the underlying crate |
| Optional `ffi_outcome_faults` and prior handoff regressions | Real foreign calls reject inconsistent serialized outcomes, preserve valid cleanup reports and retain allocation custody after late metadata errors | Native producer spontaneously emitting those contradictions, hostile-native-code isolation or new kernel-failure injection |
| Existing owner suite | Unchanged native owner behavior against its own full tests | Calling-boundary properties by itself |
| Optional `ffi_faults` tests | Ownership-transfer bookkeeping even when an owned test DLL reports malformed create/release metadata | A guarantee that hostile/broken arbitrary native code is safe |

The initial DLL exported callable symbols but always rejected creation. The Go tests compiled, loaded the DLL and failed at the missing behavior. After implementation they passed. Further temporary mutations permitting early release and suppressing native error codes were rejected by the normal Go tests. All mutations were removed before final builds.

Checked-admission tests initially compiled against a rejecting Go stub and failed on missing policy behavior. The successful observation test runs a copied fixture at a Unicode path, deliberately supplies a dot-segment create spelling, and receives the OS image name rather than that string echoed back. The child effect is absent during policy. Go GC inside the callback and after return does not invalidate the retained copied observation.

Controlled DLL variants supplied PID zero, null image, oversized length, duplicate callback and an unknown decision. They were rejected before resume; a duplicate callback initially replaced the first policy error, which a RED test exposed and joining causes corrected. A separate mutation that ignored denial was detected by normal policy rejection/panic/cancellation tests. These are synthetic protocol violations in owned test DLLs, not a claim that the correct library exhibited spontaneous duplication.

Self-review exposed a genuine caller bug: response-validation error after successful creation discarded the returned pointer. A deliberately malformed create-result size reproduced it; preserving the pointer plus error corrected it. The corresponding release experiment also proved that response failure must not discard completed deallocation. A separate classification test found that a native error whose Rust category is InvalidInput could be mistaken for local validation; the create-phase classifier now preserves every supplied native code before considering the broad category. The mapping test uses OS code 87 as one representative, not a special-case branch or evidence of forcing a particular kernel failure through Go. Both red/green runs and exact mutation sources are retained with the task evidence; no runtime fault-injection hook was added.

The optional checks in `go/faults_windows_test.go` are selected with build tag `ffi_faults` and an explicit `MCPHUB_FFI_FAULT`. They intentionally require a mutated test DLL and are excluded from normal runs. The original metadata controls use two exact changes in a disposable owned copy of `src/lib.rs`:

* Create metadata: insert `out.size = 47;` immediately after `snapshot(&owner, out);` in the successful create branch; select `TestCorruptCreateRetainsTransferredOwner`, fault `create-metadata`.
* Release metadata: insert `out.size = 47;` immediately before `drop(Box::from_raw(raw));`; select `TestCorruptReleasePreservesDeallocationFact`, fault `release-metadata`.


Additional optional checks select `TestCheckedAdmissionRejectsMalformedObservation` with `checked-pid`, `checked-null-image`, `checked-length`, or `checked-duplicate`. In the single `match check(pid, image.as_ptr(), image.len(), token)` call, respectively replace pid by 0, the pointer by null, length by 32769, or invoke the same callback once more immediately before the match. No invalid non-null memory is supplied. The unknown-decision check selects `TestCheckedAdmissionRejectsUnknownDecision` with `checked-decision`; call the callback, discard that result, then match on 2. These must reject and consume the attempted admission. The duplicate variant must retain the first policy error without invoking policy again.

`TestCheckedAdmissionMissingExportFailsClosed` with `checked-no-symbol` uses the prior owned DLL, whose artifact hash is recorded before replacement; missing-export code 127 distinguishes this from an unrelated loader failure. It checks refusal rather than silently using the old boolean entry point. There are five optional test functions: the prior two metadata functions and these three new functions; observation variants share one test function, not four independent functions.
Restore the exact source bytes and rebuild the normal DLL after each check. Do not use a mutated DLL for ordinary execution or a shared installation. A negative DLL is not a new supported implementation. The ordinary test assertions, not the temporary mutations, are shipped.

See the [cancelable-observation report](verification/cancellation-2026-11-14/RESULTS.md) and [executable negative controls](verification/cancellation-2026-11-14/mutations.ps1) for the current increment. This includes the initial failing implementation stub and native test assumptions, not only the final success.

### PR-durable results

[Verification index](verification/README.md) and [execution ledger](verification/PROGRESS.md)
are committed in this PR. Historical logs for the native dependency and earlier
FFI stages include successes, negative controls, failures and source hashes;
they are explicitly historical rather than claims about every later head.

The current environment run also records its initially failing tests, precise
negative mutations and correction of a test-only Clippy warning. A copied-buffer
unit test first mutated input without reading the changed local value; adding an
assertion made that test intent explicit instead of suppressing the lint.

The negative environment omission uses the single `LaunchContext` assignment in
`create_impl`: temporarily replace `environment: &environment` with
`environment: &[]`. Three observed-child tests must fail. Restore exact source
and rebuild the normal DLL before ordinary qualification. No runtime hook is
added. `TestEnvironmentCreateRetainsOwnerAfterMetadataFailure` reuses the existing
create-metadata mutation and proves the new path does not drop a transferred
owner. `TestEnvironmentMissingExportDoesNotFallBackToEmpty`, with
`environment-no-symbol`, uses the hash-verified previous DLL and must observe
missing-symbol code127. Neither test is counted among the normal functions.

## Reproduction

Prerequisites: existing Windows x86_64 Rust/MSVC target, Go with cgo, and a working local C compiler. No tools, crates or Go modules are downloaded by this procedure. The new lockfile resolves only the path dependency and the same pinned Windows crates already used by the owner. The Go module uses the standard library only.

The self-contained [verification/run.ps1](verification/run.ps1) sets all writable
paths under the user-approved `D:\dev\ChatGPT`, uses existing tools/cache only,
refuses to overwrite a previous report, and commits no side effects itself:

```powershell
.\verification\run.ps1 -LabRoot 'D:\dev\ChatGPT\your-owned-lab' `
  -CCompiler 'C:\msys64\ucrt64\bin\gcc.exe' -RunName 'new-distinct-run'
```

It records commands/exits, code hashes, tool versions, complete experimental
suites, Go race/cgo checks and the unchanged parent suite in
`verification/runs/<RunName>/`. A deliberate exit7 control validates native exit
propagation; only a completed successful run gets `COMPLETED.txt`. Failed runs
remain visible. No uncommitted environment script from a PC is required.

Use a private checkout, existing caches and `TEMP`, `TMP`, `CARGO_HOME`, `CARGO_TARGET_DIR`, `GOCACHE`, `GOTMPDIR` under the approved work area. Set `GOTOOLCHAIN=local`, `GOPROXY=off`, `GOSUMDB=off`, `CARGO_NET_OFFLINE=true`. Example commands from this `ffi` directory:

```powershell
cargo build --manifest-path ../Cargo.toml --offline --locked --bin owner_child
cargo build --offline --locked
cargo test --offline --locked
cargo clippy --offline --locked --all-targets -- -D warnings
cargo fmt --all -- --check
$env:MCPHUB_RX02_FFI_DLL = Join-Path $env:CARGO_TARGET_DIR 'debug/mcphub_owner_ffi_spike.dll'
$env:MCPHUB_RX02_FFI_CHILD = Join-Path $env:CARGO_TARGET_DIR 'debug/owner_child.exe'
Push-Location go
try {
    go test -race . -count=3 -timeout=90s
    go vet .
    $env:GOEXPERIMENT = 'cgocheck2'
    go test . -count=1 -timeout=90s
} finally { Pop-Location }
```

The owner fixture paths are mandatory; missing binaries do not silently skip tests or trigger installation. Go creates only local owned fixture files and hidden children. The selected native fixture and DLL must match the source being evaluated. Repeat with a release DLL as a separate configuration, not a combined timing comparison. These are correctness checks, not performance measurements.

For a controlled metadata experiment, build only the intended mutated DLL, then run exactly the matching case:

```powershell
$env:MCPHUB_FFI_FAULT='create-metadata' # or release-metadata for its own DLL
Push-Location go
try { go test -tags ffi_faults . -run '^TestCorruptCreateRetainsTransferredOwner$' -count=1 }
finally { Pop-Location }
```

The C compiler checks the shared header; Rust checks its declared layouts. Go race instrumentation checks executed Go accesses, **not** arbitrary C/Rust memory races. `cgocheck2` supplements the memory contract; it is not a proof that every possible foreign pointer or schedule is valid.

## Remaining gates and references

Before any product use: extend the checked observation to all fields and policy required by the actual product admission contract (for example token/creation-time/image validation where required); extend the now-qualified explicit-stream slice to product I/O/cancellation and any remaining descriptor profiles; qualify product-specific required-environment/overlay policy and coordinated inheritance across other launchers; reconcile current Go/selected-activation contracts; native Unix backends; full product I/O policy/cancel/drain/join beyond the finite owned-pipe qualification; other foreign kernel failures beyond protected-close, plus loader/unload failures beyond the selected fixture profiles, allocator and panic fault tests; real workload comparison; independent lifecycle/security review; packaging, binary admission and installed acceptance. No second supervisor, C++ GUI or vendor SDK rewrite is introduced.

The independent PR contains only this boundary; the native-owner parent PR remains unchanged. A small raw ABI does not transfer Rust's borrow checker guarantees to Go or C automatically. This is a checked first calling-side slice, not a complete Rust migration.

References:

1. [Go cgo pointer/lifetime rules](https://pkg.go.dev/cmd/cgo).
2. [Rust FFI and non-unwinding ABI](https://doc.rust-lang.org/nomicon/ffi.html).
3. [Rust ABI and panic behavior](https://doc.rust-lang.org/reference/items/functions.html).
4. [Microsoft LoadLibraryExW search options](https://learn.microsoft.com/en-us/windows/win32/api/libloaderapi/nf-libloaderapi-loadlibraryexw).
5. [Native-owner contracts and remaining gates](../README.md).
6. [MCPHub full work plan, PR #623](https://github.com/applicate2628/mcp-local-hub/pull/623).
7. [Go runtime/cgo.Handle: call-scoped integer tokens and explicit deletion](https://pkg.go.dev/runtime/cgo).
8. [Microsoft QueryFullProcessImageNameW: observed executable name from a process handle](https://learn.microsoft.com/en-us/windows/win32/api/winbase/nf-winbase-queryfullprocessimagenamew).
9. [Microsoft DuplicateHandle: inheritance flag, shared kernel object and file position](https://learn.microsoft.com/en-us/windows/win32/api/handleapi/nf-handleapi-duplicatehandle).
10. [Microsoft process attribute list: explicit handle inheritance](https://learn.microsoft.com/en-us/windows/win32/api/processthreadsapi/nf-processthreadsapi-updateprocthreadattribute).
11. [Go syscall.RawConn: handle validity during Control](https://pkg.go.dev/syscall#RawConn).
12. [Go sync.Mutex.TryLock: successful acquisition versus no ordering from failure](https://pkg.go.dev/sync#Mutex.TryLock).
13. [Go context.Cause and cancellation semantics](https://pkg.go.dev/context#Cause).
14. [Microsoft CancelIoEx: cancellation request is not completion](https://learn.microsoft.com/en-us/windows/win32/api/ioapiset/nf-ioapiset-cancelioex).
15. [Microsoft PeekNamedPipe: available data observation](https://learn.microsoft.com/en-us/windows/win32/api/namedpipeapi/nf-namedpipeapi-peeknamedpipe).

Terms: ABI — application binary interface; FFI — foreign function interface; DLL — dynamic-link library; cgo — Go's C interoperability tool; UTF-16 — 16-bit Unicode encoding; PID — process identifier; OS — operating system. Input lengths are UTF-16 code units, not bytes or Unicode character counts. Timeouts are milliseconds. No formula or platform guarantee is inferred from a passing compile alone.
