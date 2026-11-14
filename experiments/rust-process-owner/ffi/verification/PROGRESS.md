# Execution ledger — plan: docs/modernization/plans/rust-independent-execution-plan-2026-11-12.md

## 2026-11-14 — PR-durable evidence and explicit environment calling boundary

Base: `04ca025e49de430de7d8f1a7dcea0ffd2cab33d2`, existing PR #631.
Native parent #625 remains `7ce04f7ef2f96680ed94db1edaf77cf0358421c4`.

Ruling: user requires all necessary results in PR; local-only ledgers are not
recovery. Historical evidence for the native dependency and FFI is imported with
original/published hashes, without rerunning or upgrading its claims.

Ruling: continue in the already isolated clone and approved branch. No active
primary checkout, shared installation, other code branch, dependency version or
root build is changed. Cache-only toolchains; no install, merge, model launch or
explicit CI/bot dispatch. Repository-wide/installed gates remain open.

Next bounded task: transmit an explicit complete environment through the existing
Go/C/Rust creation boundary. Preserve `mh_create1` and its empty-environment
contract. An additive export must reuse the same native creation owner, validate
count/size and preserve pending/denied/late-error ownership. Empty means empty,
not inherit; input buffers live only through the synchronous call; no parent
mutation. Do not introduce an environment overlay or hidden required-key copy.

Work steps: archive/push existing evidence; write observed-child tests and watch
an ignoring stub fail; implement additive ABI and caller marshalling; exercise
invalid names/order/duplicates/lengths, GC, deny, parallel children and empty
compatibility; run native/foreign suites plus negative controls; self-review;
commit implementation and final results separately; verify remote contents.

All active/uncertain source, required evidence and recoverable data stay outside
trash. Only confirmed redundant files may move directly into approved trash;
never empty it automatically.
Archive verification: the initial diff check rejected trailing whitespace and
CRLF in generated JSON. Published text now removes only end-of-line whitespace
and normalizes LF; original-byte hashes remain intact and published hashes were
recomputed. No test result or failure message was changed or upgraded.

Environment task RED: compiled ignoring-environment wrapper failed observed-child
value, validation, bound and invalid-UTF8 assertions; existing native owner was
used and owned test resources were explicitly settled. Raw log and exact stub
semantics are versioned under environment-2026-11-14/.

Implementation check caught an overly broad text replacement matching the
'err' prefix in errors.New. Two malformed return expressions were corrected
after inspecting the exact source lines; this was an editing error, not a
native or ABI failure. Formatting and integration checks are repeated below.

Environment negative qualification: previous release artifact hash matched its
04ca025 evidence and was refused for missing environment export127. Omitting
the native environment caused three normal observed-child tests to fail. Late
create metadata errors retained owners on both old and new paths. All native
mutations were restored; normal DLL was rebuilt. Logs04-07 are versioned.

First committed-runner invocation stopped at the Go formatting gate after
Rust checks passed: the added NewWithEnvironment documentation had not been
normalized by gofmt. Inspected exact gofmt output and formatted the affected
file only. The interrupted run remains in runs/environment-2026-11-14/ with
no COMPLETED marker; it is not counted as a full passing run.

Environment implementation committed as 96cd48c2dfe0bee5291bc3e3c7c235f954c01743. Seven source/test files match
the full successful run's SHA256 manifest. Native parent625 and dependency
versions are unchanged. Evidence and runner are being committed separately;
no implementation result depends on the local-only ledger.

Final verification: implementation 96cd48c2dfe0bee5291bc3e3c7c235f954c01743 matches seven source/test hashes
from runs/environment-2026-11-14-final. All22 runner steps matched their expected
exit status (including the explicit exit7 control);23 Go top-level plus18 nested
cases,11 ABI unit tests and unchanged53-parent checks passed in the recorded
configurations. Failed/negative logs remain versioned, not omitted.

Next unresolved contract: redirected stdio/non-inheritable conversion, full
product policy fields and native failure/cancellation/Unix gates. The current
implementation is not installed or merge-ready; independent review is pending.

Publication check rejected redundant final blank lines in seven generated logs.
The runner now emits one final LF; existing run text was normalized only at EOF
and checksum manifests refreshed. No command/result changed. A fresh complete
run of the final runner is recorded separately to avoid claiming old runner
hashes match the new exporter.

Final published-runner verification: runs/environment-2026-11-14-published
completed on implementation96cd48c with every recorded source/runner hash
matching. The earlier interrupted formatting run and earlier passing run are
both retained with their distinct identities. No generated binary is required
from this PC; exact build instructions and artifact hashes are committed.

## Stdio calling boundary — 2026-11-14

Base37f0f8bb2b0f4ce068aaeb13000f0ccb6ac7b6f5, same isolated631 branch.
Preflight: native BorrowedStdio already owns no input handles and uses the
existing at-create inheritance allowlist/Job. Go files are normally non-inheritable.
Ruling: add one explicit creation entry point combining env and three borrowed
stream handles; old create APIs retain their behavior. Convert Go sources to
three temporary inheritable copies with DuplicateHandle inside RawConn.Control,
not Fd+KeepAlive and not SetHandleInformation on original sources. Original
file objects remain caller-owned. Temporary copies are closed once after create
or acquisition failure; every close error is preserved, and no ambiguous retry
or success claim is made. An already transferred owner is retained with errors.
Three bounded duplicate slots avoid a global registry/new process owner; aliased
stdout/stderr refer to the same native object through separate copies.
Qualification: binary input/output, shared output, no effect before admission,
parent close after create, real pipe EOF without hidden writers, denied/native
failure, allowlist exclusion and partial-copy cleanup. Blocking pumps/cancellation,
foreign kernel close faults and arbitrary concurrent inheriting launchers remain
separate gates. A per-child allowlist does not secure unrelated CreateProcess calls.
All unique commands/results/failures/reproducer notes go under verification/ and
are committed with this increment. No private env script required for final runs.
Native parent625, root product, dependencies and installation remain untouched.
Stdio edit issue: an insertion split a multiline C typedef, caught by the C
compiler before integration. Moved the new declaration after the complete
prototype; no platform/API behavior inferred from this editing failure.

Stdio implementation committed as 4fbbea19e9db7dacc0daef9448cd4c05f2e0c2fa. Six source/test files match the
complete stdio-implementation run: Go33 top-level/31 nested, ABI14 and unchanged
parent53 passed in recorded debug/release configurations. Seven close-failure
masks and partial-copy cleanup passed. Unknown-close recovery, all schedules
and unrelated launcher inheritance are not claimed. Initial source-hash check
encountered unavailable Get-FileHash; the existing .NET SHA256 API completed
the same verification without installing anything. Evidence commit follows.

Stdio qualification extended to ordinary Go os.Pipe after the native synchronous
pipe case; binary input/output and EOF are separately asserted, not inferred.
Controlled removal of stdio makes ordinary child tests fail; suppressing close
errors fails all seven protected-close cases. Late malformed metadata retains
owner/source files, and a DLL lacking the stdio symbol is rejected. Sources were
restored exactly and a complete final runner is being recorded.
Self-review keeps one native process owner; uses stdlib DuplicateHandle and
RawConn.Control; source flags stay unchanged; process outcome and temporary
copy cleanup stay separate. Unknown close failure is not automatic recovery,
and the child allowlist does not protect unrelated blanket-inheritance launches.
15 relevant skills applied: Using Superpowers; Executing Plans; TDD; API and
Interface Design; Architecture Review; Go Concurrency Lifecycle; Go Security
Hardening/process boundary; Go Testing and Verification; Using Git Worktrees
(existing isolated clone); Rust Unsafe Checker; Rust Resource Lifecycle;
Ponytail Review; Systematic Debugging; Git Workflow/Versioning; Verification
Before Completion. Rust skills read through omgskills at5c40d3ad785193231b7d0dbfb8e1eb447e5edd94.
This is author self-review, not independent review or product acceptance.
Final stdio qualification:34 normal Go top-level functions,31 nested cases,
zero skips; complete debug/release DLL race x3, cgocheck2/vet,14 ABI unit tests
and unchanged native53 checks passed with all22 runner exits matched. One
runner step deliberately expects7 to verify failure-code capture. The final
run identifies implementation4fbbea1 plus the later ordinary-Go-pipe test and
qualification overlay by exact hashes, not a fictional clean later commit.
All required final/negative logs, RESULTS, mutation reproduction and artifact
hashes are committed under verification/; no result depends on a local ledger.
Ordinary source handles and input bytes remain caller-owned. Temporary-copy
close failure is reported but not claimed fully recovered. Remaining major
steps: full product identity policy; blocking I/O/cancel/drain/join; broader
handle profiles/inheritance coordination; native Unix and independent review.
## Context-aware settlement and pipe termination - 2026-11-14

Base0bcb5f0a1044f81dc3d28019ebfc353cc0ef10c7 verified clean on own631 clone.
Ruling: stdio/non-inheritable conversion is already committed; do not repeat it.
This increment separates canceling a wait from canceling the owned operation.
Add FinishContext(ctx) as a non-destructive observation loop over existing
Finish(0), with cancelable acquisition of the existing Go owner mutex. No extra
waiter goroutine or native thread, no hidden Stop/Release/file-close/retry.
A canceled observer keeps its last known outcome and cause; no fabricated
terminal state. Actual native terminal/error results remain visible. A native
call already entered is not force-interrupted, no hard latency bound claimed.

Rejected alternatives: background goroutine around Finish(60000) would survive
the request; automatic kill-on-context would replace product policy; replacing
all mutexes/native waits is an unrelated rewrite. Timed TryLock probing checks
no owner state without acquiring it and yields between attempts.

Qualify real caller-owned pipe read/write termination separately: effects before
safety cleanup, original handles only, bounded payloads and joined goroutines.
Normal EOF must preserve final bytes; a blocked I/O owner is not proof that the
process stopped or that cancellation of observation interrupted I/O.
Parent625 library/API/manifest/lock and product files remain unchanged. All
sources, negative/positive logs, exact commands and remaining limits go to PR.
Scoped complete experimental suites are gates; no blanket Hub tests, installation,
external agents/models, merge or workflow dispatch. Existing caches only.
Native I/O first run exposed two test assumptions, not a library defect:
PeekNamedPipe can report a whole queued Write while that operation is still
pending, and this Windows os.Pipe rejects SetReadDeadline. Tests now observe
completion separately and use their actual endpoint owner's Close+join as the
failure cleanup, not a presumed deadline feature. Original failing log retained.
Local installed Go source confirmed pipe-specific CancelIoEx+join in Close;
this is then exercised natively, not generalized to all File/Reader/Writer types.
Read-only source lookup initially had a PowerShell quoting/path typo; corrected
with go env GOROOT and the actual file_windows.go, no installed source changed.
Cancelable observation implementation and thirteen new selected tests passed.
Negative controls replacing TryLock with blocking Lock, adding implicit Stop,
and erasing the last observed outcome each failed the intended tests. Sources
were restored exactly; the same selected native/race suite passed again.
Self-review: keep a single native owner and one result mapper; no added worker,
native ABI change, scheduler, finalizer or automatic failure policy. The 10ms
timer is a polling choice, not a latency/throughput guarantee. Actual terminal
and cleanup-failed state remains distinct; cancellation does not erase effects.
README now distinguishes native settlement, root exit, I/O operation completion
and drain/EOF. Two legacy control characters in README arrow text were normalized
to ASCII; no historical evidence log was rewritten or test input changed.Full cancellation implementation run completed:47 Go top-level functions,
31 nested cases,0 skips; debug/release DLL race x3, cgocheck2/vet, unchanged
14 ABI unit tests and53 native-parent tests in debug/release, Clippy/fmt all
matched expected exits. All22 runner steps matched, including deliberate exit7
control. Runner source manifest matches current implementation bytes.
Code commit contains only four Go files (shared locked mapper, context observer,
and two test files). Documentation/raw evidence is committed separately; working
source was an explicit overlay on0bcb5f0a, not a fictional clean future commit.
Implementation commit: e973ee2eabb638b6a0372066b6381484ce015dc9
Final negative-runner self-review strengthened implicit-stop injection from a
single context-select branch to every cancellation return. This removes a
scheduler-choice dependency in reproducing the wrong behavior; actual code and
passed full-suite source bytes are unchanged. Earlier negative1 evidence remains.Final committed-code full runner completed on e973ee2:47 top-level Go tests,
31 nested cases,0 skips;14 ABI unit tests;53 native-parent checks; debug/release,
race x3, cgocheck2/vet/Clippy/fmt and all22 expected exits passed. Code/runner
hashes independently matched. Stronger negative-final run detected all three
wrong behaviors, restored code bytes and passed13 selected native/race cases.
Skills13 actually applied and their concrete contribution are recorded in
cancellation-2026-11-14/RESULTS.md. This is self-review, not independent review.
No product/parent source, dependency or installation changed; native Unix and
complete product I/O policy remain open. Required results are staged for PR,
not left as references to an unversioned local folder.Prepublication evidence-only check caught CRLF/trailing whitespace in two new
records. Reproduced failure and original/normalized hashes preserved; normalized
line endings/trailing whitespace only. No source changes or reclassified tests.
A second evidence-only check caught ConvertTo-Json CRLF in the newly generated
normalization manifest. Repeated failure preserved; normalized through the same
text writer. This did not change code, test values or source qualifications.

## Actual native cleanup failures across FFI - 2026-11-14

Base a59a867d0d8a44d156ab783c78f91a02fe4d83e5 verified clean on private631 clone.
Next gate: actual OS CloseHandle failure through the existing DLL into Go,
not merely separately passing native tests and ABI mapping unit tests.
Ruling: use a disposable COPY of the native/FFI source, append a read-only
qualification export that exposes the original owner slots only to opt-in tests.
Normal library/ABI/Go runtime and parent625 stay unchanged; no fault feature,
registry, env-trigger or exported test API in ordinary artifacts.
The test owns the fixtures and temporarily sets PROTECT_FROM_CLOSE on known
original thread/process/Job handles. Native close/settlement code stays exact.
It records the real OS error, verifies all seven nonempty masks, preservation of
root result259, cached results/no retry, Finish and FinishContext, and Release.
Test recovery alone can unprotect and close these known-live protected handles;
it does not grant production a stale-numeric-handle retry. If slots were not
consumed, recovery only restores flags and leaves cleanup to the actual owner.
Run normal-artifact absence control, instrumented-positive tests, wrong-mapping
negative controls, then full ordinary suites. No product/installation activity,
new dependencies, external models, independent review or native Unix claim.
Required code, logs, source identity, commands and limits will be committed in PR.Native-close positive qualification passed on actual protected handles through
Finish and FinishContext for all7 masks; root259 and each real OS error code
survived. A separate case reclaimed known resources in the fixture and verified
that the recorded failure remained failure, not retroactive success.
Normal debug/release artifacts lack the qualification export. Two copy-only
mutations (CleanupFailed->Complete and dropped cleanup mask) each failed the
expected native outcome assertions, not just compilation. No runtime bug was
found by this gate; existing native/FFI behavior is characterized, not rewritten.
One harness preparation error attempted to recreate existing tmp; failure and
runner hash retained. Only empty owned aborted copy moved to trash, not deleted.
Self-review keeps accessor read-only and disposable, test cleanup distinguishes
still-owned slots from detached known-live protected handles, no stale retry.
Native cleanup qualification implementation committed: c3fca608507d2c5a655e583116f06950c6d8dd4c
Final instrumented run and complete ordinary suites passed; corresponding source hashes matched before commit. Evidence/docs commit follows. Ordinary runtime unchanged.

Final native-close evidence:3 opt-in functions (2 behavioral with14 matrix
subcases +1 normal-DLL control), both profiles race x3, cgocheck2/tagged vet,
and fixture Clippy passed. All47 normal Go functions/31 nested/0 skips,14 ABI
unit tests and53 parent checks passed in full ordinary debug/release runs.
Runtime code/ABI/dependencies unchanged. This qualifies protected-close masks,
not every kernel failure or general recovery.11 selected/read/applied skills
and concrete self-review are recorded in native-cleanup-2026-11-14/RESULTS.md.
Publication will include every positive/negative/preparation record and exact
copy-only runner; no required result depends on this machine remaining available.

Completed instrumented source copies matched their recorded manifests; four
copies and their unused test-only target moved directly to approved trash.
Normal target/Cargo cache and active source remain; necessary results are under
verification/ and source control. Movements confirmed, trash not emptied.

Post-publication artifact check initially compared current normal DLLs against
pre-ordinary-rebuild hashes and detected a difference. Source/lock identity was
unchanged; the later ordinary runner had rebuilt both DLLs. Historical hashes
remain intact. ARTIFACT_READBACK records both build epochs without claiming
binary reproducibility. verify-artifacts.ps1 tested the current DLLs without
rebuilding: debug/release export-absence controls and full normal Go race x1
passed, each hash unchanged through the check. Extra evidence-only commit follows.

## Loader failure/lifetime qualification - 2026-11-14

Base65f513d24b39d8ed83dbe80dd15e1c377fc0f76e recovered after interrupted response:
remote and private-clone heads match; clone clean; parent625 still7ce04f7.
Artifact-readback65f513d was already pushed; no historical hashes rewritten.
GitHub temporarily reported mergeable=false although the exact base is an
ancestor of this branch; no merge/rebase attempted or merge-readiness inferred.

Ruling: qualify actual library-load failures and reference ownership next, not
reimplement completed process/stdio cleanup. Use tiny locally built benign C
DLLs with declared ABI, no child creation/IO/threads in DllMain. Normal Rust DLL,
public ABI and production loader remain unchanged unless a distinct defect is
reproduced. No new dependency, install, remote model or workflow dispatch.
Expected cases: missing/bad image, missing mandatory exports, unsupported ABI,
initialization refusal, absent dependency, isolated dependency search and owned
module references on success/failure. Absolute path is not authentication, and
LoadLibrary executes image initialization before ABI checks: no untrusted-DLL
sandbox/trust claim. Module observations never become an owned FreeLibrary call.

Tests are opt-in ffi_loader_faults; fixture DLLs and optional mutated Go copies
live only under D:\dev\ChatGPT. Thread-local error-dialog suppression is scoped
and restored; no global loader search/error configuration is changed. Necessary
sources, commands, failures, hashes, conclusions and limitations go into PR631.
Run logs are grouped per qualification run instead of one file per compiler step.
Loader initial run: all tested load refusals returned the expected native code.
One TEST oracle then failed: GetModuleHandleEx(full malformed-image path) reported
bad-image193 rather than the assumed module-not-found126. This was not a dropped
LoadLibrary error or an accepted library. Split load-error preservation from
module-reference assertions. The latter remain explicit for valid loaded DLLs
rejected by symbols/version and for success/independent ownership paths; no
claim about mapping absence is inferred from a failed malformed-image lookup.
Initial failure log/source hashes retained. Scoped thread-error suppression is
also placed on each actual subtest thread instead of its waiting parent.
First loader negative-control attempt was correctly REJECTED by the runner:
exit1 came from compilation, not the required semantic assertion. A block comment
inserted inside the Go cgo block prematurely closed that block. Change only the
copy-mutation annotation to a C line comment; normal source remains unchanged.
Failed log/mutation hash are retained, not counted as a working negative control.
All three valid loader negative controls now compiled and failed their exact
semantic assertions: a retained module reference, accepting unsupported version,
and falling back to CWD/PATH for a missing dependency. The wrapper's normal
implementation remained unchanged throughout; no observed runtime defect.
Self-review placed quiet error handling on actual Unicode/dependency subtest
threads and added local-dependency load/unload observations. The final positive
run records distinct top-level/nested counts, not repetitions as new tests.

Loader qualification code committed: f2645b1fa994e8fdde0b5d4327f0220ba38cd742
Fresh native loader7 top-level/20 nested/0 skipped passed race x3, vet and cgocheck2. Complete ordinary module47/31/0, ABI14 and parent53 debug/release gates passed; source hashes match. No normal runtime change. Documentation/evidence commit follows.

Final loader qualification passed:7 top-level/20 nested/0 skipped, race x3,
vet/cgocheck2; all24 expected loader steps matched. Full ordinary22-step run
passed47/31/0 Go cases, both DLL profiles race x3, ABI14 and parent53 tests,
Clippy and format checks. All tested qualification source hashes matched commit
f2645b1. No runtime/ABI/dependency or product installation changed.
Eleven selected/read skills and their application are recorded in RESULTS.
This is author review and selected native Windows evidence, not independent
acceptance, a safe-untrusted-DLL guarantee, performance or Unix coverage.

Trash preflight initially rejected a Unicode manifest name decoded using the
PowerShell default encoding. Explicit UTF-8 matched its name and unchanged hash;
no movement preceded that correction. Diagnostic record preserved. Seven
verified completed fixture/source copies then moved directly to approved trash;
normal targets/cache/source/results retained, no deletion or trash emptying.

## Outcome boundary validation - 2026-11-14

Base085a47fe165bc4be5fcc6ea6d35e2e8dc5c0b449 verified on private631 clone.
Live readback:616 merged as17bc78fc602efa204a74596409c939a3bf1ddc42.
CreatorIdentitySnapshot at that exact commit requires birth, owner SID,
Job membership and wait/liveness consistency; the current Rust admission view
still exports only original PID/image. Record the gap; do not invent a weaker
replacement or rebase the experimental family without integration review.
Ruling: qualify and fix the shared outcome decoder before adding more fields.
Current Go result() checks layout only on status0 and accepts contradictory
state/flags/cleanup fields. Hypothesis: inconsistent DLL response can be
returned as successful completion. Not a claim that ordinary Rust emits it,
or that in-process hostile code can be sandboxed. Tests use OWNED processes.
Contract: validate layout on every status; reject unknown enums/flags and
contradictory validity/cleanup data. Invalid metadata yields no trusted Outcome;
retain independent call-status/native code only where its layout is known.
Owner-transfer and cleared-slot release facts remain independent; no retry.
Test return mutations in isolated Rust copies, ordinary source/DLL unchanged.
Positive cleanup codes prove mapping, not actual kernel close failure.
All results committed; no installs/COM/Python/paid models/merge/release/CI dispatch.
Initial task command had a PowerShell quoted-pipeline typo: parser rejected the
whole command before writes. Corrected command executed below, no source defect.Outcome RED confirmed on actual foreign calls: unknown flags and cleanup failure
attached to COMPLETE returned nil error; incompatible layout on native-failure
status bypassed layout validation. Controlled DLLs changed only serialization
after the real child settled. Original native implementation is not claimed to
emit these states; no hostile-DLL sandbox is implied.
Shared decoder now validates all statuses and known ABI1 value relationships,
with a typed protocol sentinel. Invalid observation is not exposed as trusted
state. Layout-known native code and separately returned call status are kept;
unknown layouts do not authorize reading a supposedly native-code field.
Transferred allocation/cleared-slot facts remain independent of decoder errors.
Initial full matrix passed24 variants (17 invalid,7 valid cleanup masks), each
through finite Finish and FinishContext. Tests assert release/unload custody and
real child effects, not just an error string. Valid mask tests are mapping proof,
NOT induced Windows kernel close failures. No native Rust behavior was changed.
Outcome implementation commit: 4ebca16f53c4fc3ad4b07c47455745ff5fb58951. Full ordinary suite and initial24 foreign variants passed before commit; checked source hashes matched. Only five Go/header files, no ordinary Rust/parent625 or dependency change. Qualification records follow in a separate commit.

Final outcome26 variants passed all56 expected runner steps on implementation
4ebca16. Exact-old-decoder replay again failed the three semantic oracles, not
compilation. Both prior allocation handoff regressions passed. Actual protected
native close failures also passed through the new decoder (all7 masks, both
Finish forms/debug/release/race) with unchanged normal DLLs during that runner.
Final ordinary suite49 top-level/63 nested/0 skip, ABI14 and native53 debug/release,
race x3, cgocheck2/vet/Clippy/fmt passed; exact source manifests matched.
Eleven read/applied skills and concrete self-review are recorded in RESULTS.
Current #616 merged-master identity delta is documented without rebase/merge or
invented default fields. Full product admission and Unix remain open.
Five completed disposable copies verified and moved directly to approved trash.
JSON array-wrapping and an undefined encoding variable initially blocked preflight;
no movement before corrected explicit identity checks. Diagnostics preserved.
Normal targets/cache/source and unique results retained; trash not emptied.

## Master convergence - 2026-11-14

Parent 87bf2fe6d612de296d4f30a8a5087adb8ee5cdbc c5672f49f65713c7555e8841e46de75b045da5f8 06d8706a446b456642a13f23cfa58e10c6e3c28f 2c13114eabc7ad6cab416788b6e32faaa8c99935 c028db723295f02909969b57f9d1ee8837a22344 and FFI bb8e316 align to accepted master17bc78fc without
changing experiment source bytes or rewriting history. Full22-step ordinary
experiment suite passed49/63/0 Go,14 ABI and53 parent tests. Results and identities
in runs/master-convergence-2026-11-14; independent acceptance/Unix/product policy
remain open. Branch audit and own-PR coordination belong to PR623.
