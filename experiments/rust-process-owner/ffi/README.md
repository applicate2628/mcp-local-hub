# Go ↔ Rust owner: experimental C ABI

**Status:** isolated Windows x86_64 calling-side experiment. This is not a Hub backend, a stable product SDK, a sandbox, or an automatic replacement for Go process ownership. It depends on the unchanged [native-owner experiment](../README.md) from PR #625; the foreign-boundary PR targets that branch so the native implementation is not duplicated in its review diff.

## Contents

1. [Scope and ownership](#scope-and-ownership)
2. [ABI and outcomes](#abi-and-outcomes)
3. [Checked original-process admission](#checked-original-process-admission)
4. [Memory, concurrency and failures](#memory-concurrency-and-failures)
5. [Evidence](#evidence)
6. [Reproduction](#reproduction)
7. [Remaining gates and references](#remaining-gates-and-references)

## Scope and ownership

**One actual creation engine:** Go caller → small C dynamic-call shim → this Rust DLL → the existing `NativeOwner` → Windows. The shim does not spawn, supervise or close process handles. Rust owns its allocation and the original Job/process/thread resources. Go owns the library lifetime and the single opaque owner reference, and serializes each owner's calls.

The first slice creates a suspended child with an absolute executable, an explicit absolute working directory, a **complete empty environment**, and **no redirected standard streams**. This deliberate restriction avoids claiming complete parity with the larger Rust context/stdio API or current Go launch policy. No parent environment/cwd mutation or implicit inheritance fallback is added.

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

Exports: `mh_create1`, `mh_admit1`, `mh_stop1`, `mh_finish1`, `mh_release1`, and additive `mh_admit_checked1`. The ABI-1 structure layouts stay unchanged. This updated Go loader requires the checked-admission export and refuses older DLLs without it; it never silently falls back to unchecked admission. Symbols and numeric values in the header are the source of truth for this experiment, not a promise to keep them as the future product API.

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

Bounds are explicit experimental budgets: no more than 64 argument views, no embedded NUL, at most 32,766 UTF-16 units per string and in the combined executable/cwd/argument input. Finite wait accepts 0..60,000 milliseconds. These are not claims about universal OS or SDK limits. Environment/stdio marshalling, streaming buffers and arbitrary long-lived asynchronous calls are not part of this ABI slice.

## Checked original-process admission

`Owner.AdmitChecked(func(ProcessObservation) error)` runs policy synchronously **inside** the existing `NativeOwner::admit_and_resume` closure. The child has not been resumed by this owner yet. Rust calls `ProcessView.process_id()` and `ProcessView.image_path()` using the original retained process handle, not `OpenProcess(PID)` or a stored create-path string. Only the resulting PID and UTF-16 image name cross the callback; the Go handler copies the image into Go-owned memory before policy/GC/retention.

**Table 3. Checked admission semantics.**

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

`Finish` is synchronous and bounded, with the owner's lock held. It cannot be interrupted by a Go context while already inside native wait; concurrent stop waits its turn. A product cancellation adapter must select polling/budgets or a separately qualified design rather than claim this is cancellable asynchronous I/O.

**Late result errors do not erase ownership facts.** If native creation transfers a non-null owner but response metadata is invalid, `New` returns **both that owner and an error**. The caller must retain any non-null owner even on error; it may not retry the operation. If release clears the slot but its response is malformed, Go still updates its live-owner count and returns the response error. This avoids both losing a live child and pretending an already deallocated object remains live. The protocol assumes a returned non-null pointer is real native ownership; arbitrary corrupt pointers are outside a safe in-process FFI contract.

There is no Go finalizer. Garbage collection cannot report native settlement, retry ambiguous cleanup or safely decide to kill a child. Explicit caller cleanup is required. `Release` frees the native wrapper; it does not reinterpret `CleanupFailed` as successful OS cleanup. Keep the terminal `Finish` outcome.

The DLL uses non-unwinding `extern "C"` and abort panic profiles. **Unexpected panic, allocation abort or native corruption may terminate the Go host.** No Rust panic travels through C into Go; Go policy panic is contained by the checked callback handler. Neither fact provides crash isolation or recovery from arbitrary native corruption. Expected errors are structured values. Deliberate panic/allocator-failure testing is not included in this iteration.

The C shim loads only the caller-supplied absolute DLL path with `LOAD_LIBRARY_SEARCH_DLL_LOAD_DIR | LOAD_LIBRARY_SEARCH_SYSTEM32`, without changing process-global DLL search settings. Absolute path and constrained dependency search do not prove trust, prevent concurrent replacement, or validate a signature/hash. Tests use the locally built DLL in the owned workspace; product binary admission, load/unload fault recovery and all transitive-dependency qualification remain separate.

## Evidence

**Table 4. Independent verification layers.**

| Layer | What it establishes | What it does not establish |
|---|---|---|
| 15 normal Go integration test functions (including 7 checked-admission functions) | Real cgo/C-shim/Rust-DLL calls; pre-admission absence of effects, copied Unicode args, pending ownership, deny/stop, native codes, repeated finish, release/unload order, concurrent Go callers, surviving descendant, empty env/cwd, budgets, original image evidence and one-shot policy/panic/cancellation handling | Whole Hub integration, native Unix, raw foreign-pointer safety, or Rust/C race freedom |
| 7 Rust ABI unit tests | C layout constants, initialized output/short-buffer protection, bad versions, structural input rejection, exact pending/native-error and seven cleanup-mask encodings | Actual OS cleanup failure transmitted through the foreign caller; those native failures were separately tested in the underlying crate |
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

## Reproduction

Prerequisites: existing Windows x86_64 Rust/MSVC target, Go with cgo, and a working local C compiler. No tools, crates or Go modules are downloaded by this procedure. The new lockfile resolves only the path dependency and the same pinned Windows crates already used by the owner. The Go module uses the standard library only.

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

Before any product use: extend the checked observation to all fields and policy required by the actual product admission contract (for example token/creation-time/image validation where required); define non-inheritable stdio conversion and custom environment marshalling; reconcile current Go/selected-activation contracts; native Unix backends; full blocking I/O/cancel/drain/join; foreign cleanup-failure and DLL/allocator/panic fault tests; real workload comparison; independent lifecycle/security review; packaging, binary admission and installed acceptance. No second supervisor, C++ GUI or vendor SDK rewrite is introduced.

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

Terms: ABI — application binary interface; FFI — foreign function interface; DLL — dynamic-link library; cgo — Go's C interoperability tool; UTF-16 — 16-bit Unicode encoding; PID — process identifier; OS — operating system. Input lengths are UTF-16 code units, not bytes or Unicode character counts. Timeouts are milliseconds. No formula or platform guarantee is inferred from a passing compile alone.
