# RX-02 / RX-03: isolated native process-owner experiment

**Experimental, not a Hub backend and not a replacement for `internal/process`.** This standalone crate implements bounded slices of the [MCPHub Rust plan](https://github.com/applicate2628/mcp-local-hub/pull/623). Web, Go policy/supervisor, installed binaries, configuration and the release build are unchanged.

## Contents

1. [Process and settlement contract](#process-and-settlement-contract)
2. [Explicit borrowed standard streams](#explicit-borrowed-standard-streams)
3. [Explicit environment and working directory](#explicit-environment-and-working-directory)
4. [Creator-crash qualification](#creator-crash-qualification)
5. [Native failure paths](#native-failure-paths)
6. [Evidence and negative controls](#evidence-and-negative-controls)
7. [Reproduction](#reproduction)
8. [Remaining gates](#remaining-gates)
9. [Decision and sources](#decision-and-sources)

## Process and settlement contract

`NativeOwner` owns the original process/thread handles and an unnamed Windows Job with kill-on-close. `PROC_THREAD_ATTRIBUTE_JOB_LIST` establishes membership at creation; `CREATE_SUSPENDED` prevents normal execution before the single-use admission callback. `ProcessView` is borrowed and observation-only, not a grant to close or replace the original process.

`wait_root` observes the retained handle, including a valid exit code of 259. `finish` distinguishes a pending root, an exited root with active descendants, a settled Job and failed native close attempts. An incomplete result keeps the original owner. Later descendant termination does not overwrite the root's observed exit status. Failed/ambiguous actions are not replayed through a different backend.

`Drop` is a best-effort fallback, **not** proof of wait/reap or successful cleanup. Explicit `finish` reports settlement; real close-failure injection remains unqualified.

## Explicit borrowed standard streams

Windows-only `create_suspended_with_stdio(program, args, BorrowedStdio { stdin, stdout, stderr })` extends the same creation mechanism rather than maintaining another spawn path.

**Table 1. Implemented boundary.**

| Property | Contract |
|---|---|
| Input authority | Exactly three caller-supplied `BorrowedHandle` values, already marked inheritable |
| Native category gate | `FILE_TYPE_DISK` / `FILE_TYPE_PIPE`; null, current-process pseudo handle, other native categories and non-inheritable inputs are rejected before acquiring the process owner. This is not a full locality or protocol detector |
| Parent ownership | No `CloseHandle`, `DuplicateHandle` or inherit-flag mutation of the supplied sources |
| Child inheritance | Explicit `PROC_THREAD_ATTRIBUTE_HANDLE_LIST` with distinct supplied handles only; stdout/stderr may share one handle |
| Existing no-stdio route | Inheritance remains disabled; existing public signature preserved |
| Borrow duration | Source handles live through synchronous creation; on return the child owns its OS copies and the Rust process owner retains no source-handle reference |
| Admission | The child is still suspended in its Job; denying admission cannot consume input or write output |
| Stream completion | The caller still owns its pipe ends. No hidden parent-side writer is retained to suppress EOF |
| Scope | This passes streams, but does not capture, asynchronously drain, interrupt or bound arbitrary caller I/O |

The supplied handle's flags must not change concurrently with creation. Borrowing protects Rust object lifetimes; it does not serialize external raw-handle mutations. Caller-provided access rights and stream direction remain caller obligations. Inherited files can share the underlying file position; this is not an isolated content snapshot.

The `GetFileType` check is only a native category filter, not proof of supported I/O semantics or a security sandbox. Microsoft includes sockets in `FILE_TYPE_PIPE`; remote/device-backed files may also require additional qualification. Only owned local files and anonymous pipes were exercised here. Passing the category check does not qualify an untested handle. The caller must supply a stream compatible with the child; this experiment does not promise a complete socket/network exclusion detector.

Registered attribute values persist until `DeleteProcThreadAttributeList`, not merely until `CreateProcessW` returns. Both backing handle arrays are declared before the native attribute-list owner so reverse local destruction preserves that native lifetime contract.

**No global inheritance claim:** a handle list restricts this child. An unrelated concurrent `CreateProcess` in the same parent that blindly inherits every inheritable handle can still inherit the caller's already-inheritable sources. This experiment neither changes such callers nor adds a broker/global lock pretending to control them. A production caller must satisfy that existing process-wide creation contract.

This deliberately narrow interface is **not drop-in Go parity**: a non-inheritable Go file is rejected rather than silently made inheritable, duplicated, or consumed. Any conversion owner must be qualified separately, including its own acquisition and cleanup failures.

## Explicit environment and working directory

Windows-only `create_suspended_with_context(program, args, LaunchContext { current_dir, environment }, optional_stdio)` supplies per-child arguments to the **same** native creation core. It does not call `SetEnvironmentVariable`, `SetCurrentDirectory`, a shell, or a second process launcher. Existing `create_suspended` and `create_suspended_with_stdio` signatures still inherit the parent environment/directory as before.

**Table 2. Explicit context boundary.**

| Property | Contract and evidence boundary |
|---|---|
| Full environment | A complete supplied ordinary-name block, not an overlay. No implicit copying of parent PATH, SystemRoot, credentials or SDK variables |
| Empty environment | A non-null UTF-16 buffer containing two NUL units; NULL would mean inheritance, not empty |
| Caller ordering | Names must be strictly increasing by Windows case-insensitive ordinal comparison; wrong order and duplicate/case-aliased names are rejected rather than silently sorted or resolved |
| Entry syntax | Nonempty ordinary names, no '=' or NUL in names, no NUL in values. Empty values and '=' inside values are preserved. Hidden per-drive '=X:' entries are not supported by this slice |
| Encoding | `CREATE_UNICODE_ENVIRONMENT`; raw Windows UTF-16 units are preserved, including the exercised unpaired-surrogate value |
| Input budgets | Environment block including separators/terminators: at most 1,048,576 UTF-16 units. Directory including terminator: at most 32,767 units. These are experimental input budgets, not claims about every OS/program limit |
| Working directory | Absolute caller-supplied path, passed directly to CreateProcessW. Missing/inaccessible directories retain native failure; the parent directory is never changed |
| Timing and ownership | Validation/encoding happens before native owner acquisition. Context buffers live through CreateProcessW; afterwards the child has its OS copy. Caller strings are not retained or modified |
| Composition | Optional borrowed stdio uses its existing allowlist/admission/ownership contract; no extra privileges or new stream owner |

The caller must prepare the required, already ordered environment for the intended program. No automatic inheritance fallback is performed on invalid input. This low-level pilot is **not** a complete Go environment overlay/sanitization policy. In particular, Windows or the child runtime may synthesize variables after creation; the guarantee is the block supplied by the library, not a promise that every later process environment contains exactly those entries and no others. Tests examine only explicit fake keys, never dump a user's environment.

A working-directory path is **not** an anchored directory identity, filesystem sandbox, or protection against an external rename/reparse race. Native Windows resolves it. Only owned local drive-absolute directories are tested; UNC/device/other namespaces and additional filesystem behavior are not qualified. Environment buffers are bounded and errors omit their contents; secure erasure of all copies is not claimed.

## Creator-crash qualification

`tests/owner_crash.rs` uses a finite, hidden `owner_crash_fixture` executable to exercise the **existing** native owner in another process. No runtime API or production creation/settlement implementation is changed by this increment.

**Table 3. Distinct observed termination scenarios.**

| Point | Required observation |
|---|---|
| Before admission, root remains suspended | Killing the creator terminates the root without its execution marker ever appearing |
| Admitted root and descendant are alive | Both retained process observations become signaled after creator death |
| Root has exited with code 7; descendant remains alive | Creator death stops the descendant, while the retained root result is still 7 |
| Explicit environment/cwd and borrowed stdio are used | The same crash guarantee survives the inheritance-enabled creation route |
| Normal explicit finish control | Root/descendant settle with the requested code; the Rust Drop witness is present, unlike abrupt death |

The driver kills **only its directly created owner process**, not the owner's Job or descendants. It retains identity-checked process observation handles acquired before that failure, never a Job handle that could keep the Job alive. Completed fixture records contain PID and native creation time; the observer verifies both creation time and the exact fixture executable before retaining cleanup authority. This is a controlled test-fixture protocol, not a new production authorization mechanism based on PID or a path.

A separately created control process stays alive through each measured failure. The owner fixture's stack Drop marker must be absent after forced termination and present after normal finish. Process exit is observed with native handle waits: readiness, return from kill, and elapsed time alone are not completion. The known root/one-descendant outcomes are captured **before** the test's own safety cleanup. The safety cleanup cannot make a failing crash assertion pass. Owned running fixtures have a 30-second fallback lifetime; successful/negative measurements and cleanup finish before it. Suspended-root failures are cleaned through the retained, verified process handle.

Negative control: removing only `JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE` makes all three initial abrupt scenarios fail while the normal `finish` control still passes. A separate repetition also fails the explicit-context/stdio scenario. The production source is restored byte-for-byte before the final suite. Thus the tests distinguish kernel last-Job-handle-close behavior from Rust destructors, explicit `finish`, a watchdog, parent-side EOF, or an accidentally terminated surrounding process set.

**Scope limits:** these are selected points **after CreateProcessW returned successfully**, not proof about creator death inside an in-progress native create, exhaustive crash schedules, system/power loss, hostile breakaway, remote effects or durable recovery. The observer knows the finite fixture membership; this is not a generic census algorithm. No extra Job handle is deliberately kept alive; any retained/duplicated Job handle would change the last-close condition. No Linux/macOS equivalent is inferred.

## Native failure paths

`src/native_failure_tests.rs` is compiled only under `cfg(test)`. It exercises the same owner methods using real failing Windows calls, not a runtime mock layer, replacement backend, configurable failure registry or global hook. Restricted-right duplicates refer to the test's already-owned process/thread/Job and are substituted into private fields only by the tests. That substitution does not occur through the public API and does not claim that normal creator handles spontaneously lose rights.

**Table 4. Nine failure tests and their observable invariants.**

| Failure | Required outcome |
|---|---|
| Invalid native attribute count in the size query | Preserve the actual OS error code rather than convert it to text-only `Other` |
| Missing executable after native preparation | No owner is returned; repeated attempts preserve the native failure and do not accumulate process handles after the separately noted first-call initialization |
| Resume denied by thread access rights | Original root stays suspended and retained; the single admission attempt cannot be repeated |
| Unexpected primary-thread suspend count | Do not report a successful start or repeat admission; the same owner remains stoppable |
| Process wait without SYNCHRONIZE | Return the native error, not Pending/timeout or Complete; retain owner slots |
| Signaled process without exit-code query rights | Do not fabricate or cache a root exit; the original process observation can still recover it |
| Job accounting query denied after root exit | Preserve the known root exit, but do not close resources or assert full settlement |
| Termination denied, then test rights restored | Return the cached first attempt rather than silently reissue the side effect; later execution settlement is distinct from that failed stop attempt |
| Protected native handles cannot close | Report every failed close and the original root result; do not turn execution success into cleanup success |

The close case covers all seven nonempty subsets of thread/process/Job close failure. `HANDLE_FLAG_PROTECT_FROM_CLOSE` is applied only to this fixture's real handles. The library detaches an unsuccessfully closed numeric handle, keeps `CleanupFailed`, and does not treat it as reusable ownership. The fixture independently confirms the known protected handle is still live, removes **its own artificial protection**, and releases it. This test cleanup is not a recommended retry policy for an arbitrary ambiguous production close. The handle count after fixture cleanup must return to its prior value. Repeating `finish` returns the same recorded failure; this does not instrument or count every possible kernel call.

A real defect was reproduced at `AttributeList::new`: the OS returned error 87 in the controlled invalid-count call, while `io::Error::other(format!(...))` discarded its `raw_os_error`. The helper now returns the original error for an unexpected native failure, and separately reports impossible success/empty-buffer shapes. The public create route normally requests one or two attributes; the intentionally invalid count is a private fault stimulus, not an exposed input or a claimed ordinary runtime incidence. The correction applies to the common native-error path, not only error 87.

Negative controls deliberately changed `WAIT_FAILED` into timeout and changed failed cleanup into Complete. Both tests failed for the intended semantic reason, all mutations were restored, and the full crate suite was repeated. No allocator, Job creation/security setup, second attribute initialization/update, or already-blocked native I/O failure is implied to be exhaustively covered. Native close protection is tested under the ordinary non-debugger runner, not arbitrary debugger/Application Verifier exception settings.

## Evidence and negative controls

Native Windows x86_64, Rust 1.99.1, pinned cached `windows-sys = 0.61.2` and `windows-link = 0.2.1`. FileSystem/Pipes/Globalization features use the same dependency; `Cargo.lock` and dependency versions are unchanged.

**39 native integration tests, 12 unit tests (nine native-failure and three encoding/validation), and two compile-fail doctests passed** in the current iteration. The original 13 lifecycle tests still cover at-create suspension/membership, one-shot admission, retained pending owners, descendants, root outcomes, UTF-16 argument quoting and repeated lifecycle handle counts.

**Table 5. Ten original native stdio cases (plus explicit-context composition).**

| Tested behavior | Evidence |
|---|---|
| Binary stdin/stdout/stderr | Exact bytes including NUL/non-UTF-8; no output before admission; parent handles/flags remain usable |
| Aliased stdout/stderr | One underlying handle receives both ordered writes without duplicate-list problems |
| Parent releases its File objects after create | The child continues through its own inherited OS references |
| Admission denied | No child marker or output; parent handles/flags survive termination and settlement |
| Non-inheritable source in each of three slots | Rejected before create, without mutating parent flags |
| Missing executable | First failure preserves all borrowed handles; 24 subsequent failures do not accumulate handles |
| Unrelated inheritable event | Child cannot signal the actual parent event, while all explicit streams still work |
| Anonymous pipes | Child waits until the real writer owner supplies EOF; final bytes arrive; no retained parent writer masks final broken-pipe state |
| Repeated successful stdio lifetimes | Stable handle count after 16 complete lifetimes |
| Pseudo/null/non-file sources | Rejected before child creation; actual event and parent files remain valid |

The second compile-fail example rejects dropping a source `File` while its borrowed streams are still used for creation. The first still rejects retaining `ProcessView` past admission.
The context suite adds ten native cases: Unicode/case-insensitive lookup, empty values and parent-sentinel exclusion; explicit empty environment; unchanged legacy inheritance; source strings released/changed after creation; denied admission; malformed/duplicate/unordered/oversized entries; malformed cwd; native missing-directory error; a non-Unicode UTF-16 value; and two simultaneously live children with separate contexts. An eleventh new native case combines explicit context with borrowed stdin/stdout/stderr. Three unit tests check exact double-NUL framing, total budget boundaries including separators, directory cap and non-echoing errors.

A compiling context-ignoring stub failed eight native cases. Temporarily replacing only the environment pointer with NULL made the empty-environment test observe the fake parent sentinel. Omitting only the native directory pointer made the two-context test observe the parent's directory. Both negative changes were removed, and all tests repeated. These controls distinguish actual native parameter use from merely preparing plausible buffers.

The new test report decoder initially failed a Clippy constant-chunk lint. It now uses the standard-library fixed-size chunk view, with no suppressed warning. No production behavior was changed to satisfy that lint.

The initial stdio-ignoring stub compiled and failed eight behavioral cases. Temporarily removing only the HANDLE_LIST registration made the unrelated-event test observe the real event becoming signaled; after restoration the full suite passed. This checks the protected object, not just a potentially reused numeric handle.

**Handle-count qualification:** a first missing-image failure showed a one-time count rise on the preexisting no-stdio route too. In a fresh diagnostic, the sequence was `66, 74, 74, 74, 74, 74, 74`; the stdio route then stayed at 74 for all six failures. The exact process-global allocation source was not identified. The final failure test verifies source preservation on the first attempt and stable repeated-failure counts after that attempt. It does **not** claim first failure allocates no process-global resources.

`clippy --all-targets -- -D warnings` and formatting checks passed. These are actual scenario results, not proof over all schedules, resource-exhaustion states, native APIs or operating systems. No performance comparison or speedup is claimed.

## Reproduction

Use already available toolchains and cached pinned dependencies. All new build/cache/temp/evidence paths should be in the approved work area; do not install missing packages silently. Set a new owned fixture root for each invocation; tests deliberately retain fixture files for review and trash-first disposition.

```powershell
$env:CARGO_NET_OFFLINE = 'true'
$env:CARGO_TARGET_DIR = '<owned-work-area>\target'
$env:TEMP = '<owned-work-area>\tmp'
$env:TMP = $env:TEMP
$env:MCPHUB_RX02_PARENT_SENTINEL = 'owned-test-parent-only-20261113'
$env:MCPHUB_RX02_FIXTURES = '<new-owned-fixture-directory>'
New-Item -ItemType Directory -Path $env:MCPHUB_RX02_FIXTURES | Out-Null
cargo test --offline --locked -- --test-threads=1
cargo clippy --offline --locked --all-targets -- -D warnings
cargo fmt --all -- --check
```

Tests run serially because process handle counts are not meaningful if unrelated tests concurrently acquire resources. Only the locally built finite `owner_child` and `owner_crash_fixture` programs are launched with no console window. The stdio fixture reads at most 4096 bytes; production streams are not implicitly capped by that test-fixture choice. A bounded owner wait protects the test; a failed assertion still has the owner's Job-close fallback.

## Remaining gates

1. Exact accepted Go-contract comparison, including non-inheritable source conversion, caller environment preparation/sorting, required SDK keys, directory identity requirements and selected-activation identity. No production caller is wired here.
2. Remaining acquisition/setup/allocation/attribute-update failure classes; already-blocked native I/O cancellation; crash timing inside acquisition/native create and broader recovery qualification. The real failure cases and post-create crash points above are exercised, not all native failures/schedules. A successful stdio test is not a complete I/O pump or recovery contract.
3. Reviewed Go/Rust application binary interface (ABI) and real foreign-caller tests. Safe Rust borrowing does not prove Go/Python callers obey it.
4. Native Linux/macOS implementations and tests. The borrowed-handle and explicit-context APIs are intentionally Windows-only; non-Windows strict creation remains explicitly unsupported. No Unix runtime or cross-build qualification is claimed by this iteration.
5. Independent lifecycle/security review and the repository's integration/version/release gates. This branch remains a draft experiment; no installation or merge is included.

There is no restart policy, persisted state, scheduler, sandbox, new permission, COM client, capture daemon or vendor/model execution. The build and tests do not qualify unsupported consoles, sockets, network/device handles or Windows versions beyond the actually exercised environment.

## Decision and sources

**Decision for this experiment:** require pre-authorized inheritable sources instead of toggling caller flags or making temporary inheritable duplicates behind the caller's back. This avoids another cleanup/ownership protocol in the first stdio slice. The tradeoff is an explicit qualification gate for production callers whose sources are non-inheritable. Revisit only when that caller's actual contract is fixed.

**Context decision:** accept a complete, already ordered explicit block rather than adding an environment-merging framework or silently using parent variables. This keeps native mechanism separate from caller policy and makes invalid ordering/duplicates observable. Revisit caller preparation only against its accepted contract; do not fix unknown required variables by copying the entire parent environment.

The underlying creation, admission and settlement code is shared with `create_suspended`; no second process-owner implementation was introduced. Changes are confined to this experimental crate and its tests.

1. [Microsoft: HANDLE_LIST and Job attributes](https://learn.microsoft.com/en-us/windows/win32/api/processthreadsapi/nf-processthreadsapi-updateprocthreadattribute).
2. [Microsoft: inheritance, standard handles and shared kernel objects](https://learn.microsoft.com/en-us/windows/win32/procthread/inheritance).
3. [Rust: BorrowedHandle lifetime and non-consuming use](https://doc.rust-lang.org/std/os/windows/io/struct.BorrowedHandle.html).
4. [Microsoft: GetFileType categories, including socket classification](https://learn.microsoft.com/en-us/windows/win32/api/fileapi/nf-fileapi-getfiletype).
5. [MCPHub foreground/admission reference](https://github.com/applicate2628/mcp-local-hub/blob/53645ec7170c9d56073ab5140dec865b23ce7c21/internal/process/foreground_windows.go).
6. [Microsoft: CreateProcessW environment and current-directory arguments](https://learn.microsoft.com/en-us/windows/win32/api/processthreadsapi/nf-processthreadsapi-createprocessw).
7. [Microsoft: environment-block syntax and ordering](https://learn.microsoft.com/en-us/windows/win32/procthread/changing-environment-variables).
8. [Microsoft: non-linguistic ordinal sorting](https://learn.microsoft.com/en-us/windows/win32/intl/handling-sorting-in-your-applications) and [CompareStringOrdinal](https://learn.microsoft.com/en-us/windows/win32/api/stringapiset/nf-stringapiset-comparestringordinal).
9. [Microsoft: Job lifetime, last-handle-close and membership](https://learn.microsoft.com/en-us/windows/win32/procthread/job-objects).
10. [Microsoft: TerminateProcess is asynchronous; wait for the retained process](https://learn.microsoft.com/en-us/windows/win32/api/processthreadsapi/nf-processthreadsapi-terminateprocess).

**Terms:** stdio — standard input/output/error; EOF — end of file/stream; Job — Windows process-group resource object with its configured policies; handle — operating-system resource descriptor; ABI — application binary interface; FFI — foreign-function interface. No property of one OS mechanism is transferred to another by naming it alike.
11. [Microsoft: DuplicateHandle access masks and same-object identity](https://learn.microsoft.com/en-us/windows/win32/api/handleapi/nf-handleapi-duplicatehandle).
12. [Microsoft: SetHandleInformation and protection from close](https://learn.microsoft.com/en-us/windows/win32/api/handleapi/nf-handleapi-sethandleinformation).
13. [Microsoft: InitializeProcThreadAttributeList size-query contract](https://learn.microsoft.com/en-us/windows/win32/api/processthreadsapi/nf-processthreadsapi-initializeprocthreadattributelist).
14. [Microsoft: Job access rights](https://learn.microsoft.com/en-us/windows/win32/procthread/job-object-security-and-access-rights), [process rights](https://learn.microsoft.com/en-us/windows/win32/procthread/process-security-and-access-rights), and [thread rights](https://learn.microsoft.com/en-us/windows/win32/procthread/thread-security-and-access-rights).