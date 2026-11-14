# Original-process observation: native qualification

## Contents

1. [Scope and source identity](#scope-and-source-identity)
2. [Contract and decisions](#contract-and-decisions)
3. [Observed results](#observed-results)
4. [Review findings and corrections](#review-findings-and-corrections)
5. [Reproduction and evidence](#reproduction-and-evidence)
6. [Remaining work](#remaining-work)

## Scope and source identity

Code commit: [`628890dec7e9e1f52be487fa4533b4bdfedeb71c`](https://github.com/applicate2628/mcp-local-hub/commit/628890dec7e9e1f52be487fa4533b4bdfedeb71c), five source/test files in the existing native experiment. The tested code was based on [#625](https://github.com/applicate2628/mcp-local-hub/pull/625) at `7ce04f7ef2f96680ed94db1edaf77cf0358421c4`. Publication targets its synchronized head `c5672f49f65713c7555e8841e46de75b045da5f8`, which contains accepted master `17bc78fc602efa204a74596409c939a3bf1ddc42`. Both parent commits have the identical native experiment tree `7b896d4cab2315ded3f122b1aa696f33ae385f62`. No dependency, manifest, production integration, foreign layout or installed configuration change is included.

The final native run names the code commit and hashes all native source/test/manifest bytes. Documentation and evidence are added afterwards; an earlier run with an explicit working overlay is not relabelled as testing a future evidence commit. This is a Windows-native increment, not completed migration R01–R24.

The published Go/Rust consumer used for compatibility is frozen at [#631 `a7a4c2d19c5a572ae771ab9b9a1511ae2cf9a718`](https://github.com/applicate2628/mcp-local-hub/tree/a7a4c2d19c5a572ae771ab9b9a1511ae2cf9a718/experiments/rust-process-owner/ffi). A separate disposable copy contains that exact consumer plus the new native crate. Neither the active #631 checkout nor its DLL/target is modified. The concurrent published head `eb4ca1d40244ba1915638b76c3e30b2a684d9b07` was compared before publication: its 31 changed files under this consumer are all verification material; source, headers, manifests and lockfiles are unchanged. This proves source equality for that comparison, without relabelling the frozen run as a test of a later complete checkout.

## Contract and decisions

The existing owner captures either the original creation time or its native error after successful CreateProcessW, retaining process/thread/Job ownership in both cases. The borrowed ProcessView can then compare a fresh creation-time observation with that binding, query the exact original Job and observe the process's wait state.

**Table 1. Decisions and their implications.**

| Decision | Reason and limit |
|---|---|
| Keep raw unsigned FILETIME units | Exact 100 ns ticks since 1601-01-01 UTC, no date-library dependency or lossy conversion; any Go text formatting belongs at its policy boundary |
| Retain the initial error | A later successful query cannot recreate evidence that was not obtained at creation; failure after process creation must not hide the original owner |
| Borrow the original non-null Job | NULL would answer whether a process belongs to any Job and could accept the wrong grouping |
| Use typed NotSignaled/Signaled | A suspended process is nonsignaled; wait failure and exit code 259 cannot stand in for liveness |
| Add observations to the current view | No new process owner, PID reopening, public raw handles, retry, thread, finalizer or permission |
| Keep observations separate from policy | The methods are not an atomic snapshot and do not implement token-user SID, product capability or a complete admission decision |

The Windows API contracts and primary Microsoft sources are numbered [15–17 in the native README](../../README.md#decision-and-sources). Source comparison used the exact [merged #616 CreatorIdentity contract](https://github.com/applicate2628/mcp-local-hub/blob/17bc78fc602efa204a74596409c939a3bf1ddc42/internal/process/creator_identity.go). A copy of its fields or digest is not its private owner-bound capability. Source search did not identify a production caller of ObserveCreatorIdentity; this observation is not a complete call-graph proof.

## Observed results

Environment: Windows x86_64, installed Rust 1.99.1/MSVC, Go 1.27.4 and the existing GCC for cgo. All builds use the existing offline/locked dependencies. No installation, external provider/model execution, manual CI dispatch or product startup is part of these runs.

**Table 2. Final checks.**

| Check | Result |
|---|---|
| Full native debug and release | 20 unit + 40 integration + 2 compile-fail doctests = **62 checks in each profile**, zero ignored/filtered in the complete suites |
| New ordinary coverage | **8 unit functions + 1 integration function**; repetitions and mutation configurations are not counted as additional independent functions |
| Actual post-create capture failure | Isolated copy calls GetProcessTimes with the retained primary-thread handle at the initial capture point; observed native error **6**, owner returned and explicitly stopped/settled |
| Semantic negative controls | **5** rejected changes: recapture lost binding; query any Job; wait failure converted to a value; signaled root called alive; ownerless return after successful creation |
| Static native checks | Clippy all-targets with warnings denied; cargo fmt; git diff --check |
| Existing foreign consumer | **14 Rust ABI tests**; ordinary Go **49 top-level / 63 nested / 0 skipped** with both debug and release DLL, race detector once per profile; cgocheck2 and go vet passed |

The child independently writes its own PID and creation time; the callback's bytes must match after admission, and no child marker exists during observation. Reduced-right aliases and substituted Job handles are private fixtures on resources owned by these tests, not claims that normal creator handles spontaneously lose rights. Repeated observation keeps the process handle count unchanged for the tested schedule.

The constructor fault and early-return control exist only in disposable source copies. Ordinary runtime bytes are checked unchanged across all controls. The control tests are not production feature flags or exported test hooks. Go race results describe executed Go schedules and are not proof of all Rust/Windows interleavings.

## Review findings and corrections

**Table 3. Concrete findings and disposition.**

| Finding | Condition and consequence | Correction / evidence |
|---|---|---|
| New wait-failure test initially checked only outer admission error | A bad wait_state could return success; the later existing wait_root then returned access-denied, allowing the test to pass for the wrong reason | Assert the error at view.wait_state inside the callback. The first failed-wait negative control exposed this, and the final mutation now fails the intended assertion |
| Stored-error unit test did not exercise constructor failure | Assigning a stored error after a successful create could miss a future ownerless return immediately after creation | Add real failing initial capture in a disposable copy plus the opposite early-return control |
| Runner first accepted any failure of the named negative test | A fixture-acquisition failure in that function could satisfy a name-only oracle | Require the specific assertion diagnostic as well as test failure; ownerless control additionally requires its marker after successful CreateProcessW and native error 6 |

The first ad hoc launch stopped when Windows PowerShell treated compiler stderr as an error. The next run omitted the required fixture-root environment variable: eight unit tests passed, integration stopped before child creation. Both are retained as setup diagnostics, not runtime defects or complete passes. Correct runners configure owned fixture directories and use command exit codes. A missing destination directory also prevented the first runner upload before file creation; it was created before the successful write.

Author self-review traced each new native acquisition/query/error and the callback's one-shot/settlement path, checked additive API compatibility, and verified staged scope and source hashes. Separate read-only architecture and code reviews found no blocking runtime defect, identified the constructor-proof and negative-oracle gaps above, and those gaps were corrected. They did not perform an independent native run or qualify full product security.

The prepublication metadata check also rejected PowerShell array wrappers in the aggregate verification receipt. They were normalized to ordinary JSON arrays; original run manifests, outcomes and source hashes are unchanged.

The final documentation review also corrected the fixture inventory: native unit tests launch their own finite test executable with `--list`, in addition to owner_child and owner_crash_fixture. This prose correction requires no code change or rerun.

Applied methods: Git Workflow for isolated commits and exact publication; API/Interface Design and Architecture Review for the additive ownership boundary; Ponytail Code Simplification to avoid another observer/owner layer; Go Testing and Verification for actual foreign-call assertions; Caveman Investigate First and Verify and Stop for distinct failure causes and bounded proof; Personal Context/Context Engineering for continuation; Go Concurrency Lifecycle and Code Review through the independent read-only reviews. These are concrete review methods, not a count of external model runs.

## Reproduction and evidence

Run from an existing checkout with installed Windows tools. Supply a new writable task directory under the user-approved workspace and the existing Cargo cache; no dependencies are downloaded. The native runner has no cleanup, push or installation action.

```powershell
./verification/identity-2026-11-14/run.ps1 -TaskRoot '<owned-task-root>' -CargoHome '<existing-cargo-cache>' -Run 'new-native-run'
./verification/identity-2026-11-14/consumer.ps1 -TaskRoot '<owned-task-root>' -CargoHome '<existing-cargo-cache>' -CCompiler '<installed-gcc.exe>' -Run 'new-consumer-run'
```

**Table 4. Durable evidence.**

| File | Purpose |
|---|---|
| [native-final.log](native-final.log) | Complete final native suites, native capture failure and all negative assertions |
| [consumer-final.log](consumer-final.log) | Existing ABI and real Go calls with both DLL profiles |
| [verification.json](verification.json) | Exact commands/exits, test counts, source/runner hashes and frozen consumer input |
| [history.zip](history.zip) | All recorded initial/intermediate/final diagnostics and source manifests, normalized only for line endings/trailing whitespace and task/cache path placeholders |
| [SHA256SUMS.txt](SHA256SUMS.txt) | Published evidence file hashes |
| [disposition.json](disposition.json) | Verified source-copy manifests, quiescence and trash-first moves; no trash purge |

The archive keeps historical failures available without expanding the PR into hundreds of log files. Final logs are also plain text for review. No unique source, outcome or recovery depends on the disposable copies or on retaining this PC's build caches. The archive is not an executable runtime payload. Sixteen verified disposable source copies, their two control/consumer build targets and duplicate publication staging were moved in seven directory operations to approved trash. Required sources/results, ordinary native target and original fixture evidence remain outside trash; the trash was not emptied.

## Remaining work

The next owner responsibility is a checked observation of the primary token's **TokenUser SID**, with bounded query/copy and explicit token cleanup outcomes. TokenUser is not TokenOwner. An actual product adapter must then preserve the existing SID formatting receipt and original Go capability; adding fields and a digest alone is insufficient.

After native fields are qualified, add an independently versioned foreign observation while preserving existing ABI1 exports/layouts and explicitly refusing unavailable capability. No fallback from rejected identity admission to PID/image-only admission is authorized by this work. Reconcile actual consumers, context/custody rules and current main before integration.

Native Linux/macOS, broader system/allocation/I/O/crash schedules, equivalent-workload benchmarks, repository-wide/installed acceptance and final independent lifecycle/security review remain open. Existing full-Hub gates are not cleared by these focused tests.

Continuation topology was checked read-only. Initially, #625→#631 and #629→#630 had distinct owner boundaries, and the 19 open PRs inspected had no overlapping changed-file sets relative to their own bases; this does not prove no semantic duplication. At publication, #623 and #625 are synchronized with accepted master, #631 follows updated #625, and #630 has been merged by the concurrent session into #629 (not master). Among the then 20 open PRs no competing native-identity increment was found; #625 explicitly reserves code commit 628890de. This increment changed, closed or rewrote none of those PRs and did not overwrite the concurrent plan.
