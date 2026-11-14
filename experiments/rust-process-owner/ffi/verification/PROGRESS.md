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