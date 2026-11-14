# Explicit environment across Go / C / Rust

Implementation commit: `96cd48c2dfe0bee5291bc3e3c7c235f954c01743`.
Base before code: `ddbf21581a96a417c204fa8396b9c69a6ef9caee` (historical evidence only).
Unchanged native dependency: `7ce04f7ef2f96680ed94db1edaf77cf0358421c4`, PR #625.
Owner PR: #631; canonical plan: #623. This is a bounded experiment, not installed Hub code.

## Results

The new `mh_create_env1` / `NewWithEnvironment` supplies the full ordinary
Windows environment through the same native creator. Old `mh_create1`/`New`
still uses an explicit empty block. Nil/empty never means inherit. Native name
ordering, case-alias rejection and actual process creation remain with the
existing `LaunchContext`, not a second environment/spawn policy engine.

C-owned UTF-16 entries are copied before return. Tests change caller input and
run GC before admission, then read values from the real child. Empty values,
Unicode keys/values, '=' inside values, child cwd and parent-global non-mutation
are checked. Counts, full-block terminators and malformed text are bounded;
Go now rejects invalid UTF-8 on the common input boundary rather than silently
substituting text. No secret values appear in errors or reports.

## Verification

The [complete final run](../runs/environment-2026-11-14-published/COMPLETED.txt)
was produced by the committed [runner](../run.ps1), not an unversioned PC script.
Source SHA256s in that run match the seven committed source/test files above.
The final runner ran on the implementation commit above, with documentation and
evidence changes still pending. Its exact source manifest also identifies the
checked runner. An earlier passing run used the archival HEAD plus the same
code overlay; that earlier evidence is preserved, not used to claim a clean HEAD.

| Layer | Observed result |
|---|---|
| Complete Go experimental module | 23 top-level tests, 18 nested cases, zero skips |
| Debug DLL and release DLL | Each full Go suite passed race-enabled count=3 |
| Pointer checks / static Go checks | `GOEXPERIMENT=cgocheck2`, `go vet` passed |
| FFI Rust crate | 11 unit tests passed debug/release; Clippy -Dwarnings and formatting passed |
| Unchanged native parent | All 53 checks passed debug/release; Clippy and formatting passed |
| Runner exit handling | Intentional exit7 was recorded as an expected control, not success0 |

[Per-command exits](../runs/environment-2026-11-14-published/steps.json),
[Go counts](../runs/environment-2026-11-14-published/go-counts.json),
[source hashes](../runs/environment-2026-11-14-published/source-hashes.json) and raw
logs are committed alongside this report. They cover only the selected Windows
experimental components. Go race/cgo checks are not proof of all Rust/C schedules.

## Failures and negative evidence, retained rather than hidden

- [01-red.log](01-red.log): a compiling Go wrapper ignored the environment. Real
  child-value/validation/size tests failed. It also exposed legacy invalid UTF-8
  normalization. These were behavioral failures, not undefined symbols.
- [03-rust-clippy.log](03-rust-clippy.log): the copied-buffer test wrote a changed
  source unit without subsequently reading that source. An explicit assertion
  was added, no warning suppressed. The final Clippy run passed.
- [First runner attempt](../runs/environment-2026-11-14/steps.json) stopped at
  Go formatting after Rust checks. It has no completion marker and is not counted
  as a full passing run. The documentation comment was formatted and all checks
  rerun in a new report directory.
- [05-ignored-env-negative.log](05-ignored-env-negative.log): replacing only the
  native `environment: &environment` with `environment: &[]` made three ordinary
  observed-child tests fail. The source was restored and normal DLL rebuilt.
- [06-metadata-owner-preserved.log](06-metadata-owner-preserved.log): a controlled
  create response with `out.size=47` still transfers an owner. Both legacy and
  new environment callers retained it with an error; no unload or hidden retry.
- [04-old-export.log](04-old-export.log): hash-verified previous checked-admission
  DLL without `mh_create_env1` was rejected with missing-symbol127, not used via
  empty/inherited fallback. [Artifact provenance](04-prior-artifact.txt) records
  its source/hash; the rebuildable binary itself is not stored in Git.

Temporary native mutations were restored before final verification. No fault
hook or alternate process owner was added. The ordinary child inputs are all
synthetic and owned. An editing typo caused by a broad text replacement was
found by gofmt and corrected before any integration run; the ledger records it.

## Review and limits

15 relevant skills were applied: Using Superpowers, Executing Plans, TDD,
Using Git Worktrees/isolated clone, API/Interface Design, Architecture Review,
Go Testing/Verification, Go Security Hardening/process boundary, Go Concurrency
Lifecycle, Rust Unsafe Checker, Rust Resource Lifecycle, Systematic Debugging,
Ponytail Review, Git Workflow/Versioning, Verification Before Completion.
Rust skills were read at `actionbook/rust-skills@5c40d3ad785193231b7d0dbfb8e1eb447e5edd94`.
This is author review and evidence, not independent expert/agent certification.

The review rejected an implicit parent merge, own Unicode sorting scheme,
new environment registry, automatic old-DLL fallback and duplicate creation
implementation. Exact bound calculations include separators and terminal NULs.
Ordinary names must already be strictly Windows-ordinal-ignore-case sorted;
the caller must choose SDK-required entries. Hidden '=X:' names, arbitrary
non-Unicode Go strings, redirected stdio and non-inheritable-handle conversion
are not implemented. The raw C UTF-16 value-copy unit test is not evidence that
Go strings can represent every Windows code unit.

Actual foreign kernel cleanup failure, full product admission fields, blocked
I/O cancellation, native Unix, equivalent-workload performance, independent
review and installed/binary admission remain open. Environment/cwd is not a
sandbox; loader/runtime-injected variables and secure erasure are not claimed.

## Reproduce and recover

Use a fresh clone of the PR branch and the committed README/runner. Supply only
existing approved Rust/Go/C tools and caches. Commands are offline/locked and
never install, push, merge, launch vendor/models, or activate the working Hub.
The report directory must be new. Do not use the previous PC's cache or private
ledger as the sole source of commands, conclusions or next actions.

Historical native/FFI evidence is preserved in [history](../history/index.json).
All required source, results, failure explanations and reproduction instructions
for this increment are in the PR. Build artifacts and temporary fixtures are
rebuildable and intentionally excluded. No source or result is promised only at
an absolute path on a computer.
