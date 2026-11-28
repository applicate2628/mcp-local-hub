# Fresh audit and guarded reproduction — 15 November 2026

**Outcome:** self-review and three independent read-only reviews found no confirmed Rust/C/Go implementation defect in the qualified identity increment. One evidence-preservation defect in the reproduction runner was reproduced and fixed. Runtime inputs remain identical to the qualified implementation.

## Contents

1. [Scope and exact sources](#scope-and-exact-sources)
2. [Finding and correction](#finding-and-correction)
3. [Fresh checks and reused qualification](#fresh-checks-and-reused-qualification)
4. [Current branches and publication](#current-branches-and-publication)
5. [Next receiving contract](#next-receiving-contract)
6. [Evidence and limits](#evidence-and-limits)

## Scope and exact sources

The reviewed prepared head was `10f011453239b26076d60609b2c9a68621185fdb`; the runtime implementation is `32812fa5c3793d9af9f3d5770b614f0a96d4d8da`. The native dependency remains #644 `84119b51e089cce9ca42da2fce35a012eb86cb7c`, on #625 `c5672f49f65713c7555e8841e46de75b045da5f8`. The old foreign boundary is pinned at `eb4ca1d40244ba1915638b76c3e30b2a684d9b07`.

The author read the complete new identity producer/decoder/header/tests, changed loader/admission paths and documentation, and the actual native and Go receiving owners. Separate reviews covered Rust/C memory, layout and ownership; Go lifetime, locking, decoding and test oracles; evidence, reproduction, plan coverage and branch topology.

The additional changes in this audit are the guarded reproduction runner and review/publication documentation. No Rust/Go source, header, dependency or qualified binary is changed.

## Finding and correction

**Table 1. Confirmed defect.**

| Location | Condition | Consequence | Minimal correction |
|---|---|---|---|
| Previous `run.ps1`: default TaskRoot and writes before the late compatibility check | Run without a new explicit root, or reuse a root with previous qualification/compatibility output | Original source identity, hashes and command logs could be overwritten before the runner finally refused the existing compatibility source | Require explicit nonempty TaskRoot; normalize and constrain it to an owned subdirectory of D:\\dev\\ChatGPT; reject existing qualification or compatibility output before directory creation, environment changes or command execution |

The existing source checkout and prebuilt child target remain allowed inputs. The original executed runner in [logs.zip](logs.zip) is unchanged; [run.ps1](run.ps1) is the corrected version for future reproduction. This correction does not reclassify the historical execution.

The regression harness uses inert files under an owned task directory. Local PowerShell Git/Cargo function stubs prevent actual compiler, repository or child execution. Its nonzero Cargo boundary is an expected harness stop, not a failing product build.

**Table 2. Six distinct guard oracles.**

| Case | Observed result |
|---|---|
| Old runner with existing qualification evidence | Existing marker overwritten, reproducing the defect before the isolated tool stop |
| Corrected runner with existing qualification | Refused before any Git/Cargo stub call; complete file/directory inventory unchanged |
| Corrected runner with existing compatibility source | Same early refusal and unchanged inventory |
| Corrected runner with compatibility archive but no source | Same early refusal; the archive is preserved |
| Corrected runner with new output paths | Accepts preflight and reaches exactly one isolated Cargo stub call |
| Corrected runner without TaskRoot, noninteractive invocation | Parameter binding exits 1 before script body; no files created |

PowerShell parsing reported zero syntax errors. A separate read-only review confirmed the new guard precedes every write and that the qualification commands/oracles were unchanged.

The first harness attempt itself stopped because script-scoped stub counters were resolved in the invoked script's scope. The expected isolated tool error was already observed. The counters were corrected to task-specific global names, the initial script/log/failure record were retained, and all six oracles were repeated successfully in a new fixture directory. No product defect or successful full qualification is inferred from that harness failure.

## Fresh checks and reused qualification

**Table 3. What was actually checked in this audit.**

| Object | Fresh result |
|---|---|
| Qualified compiler/test inputs | 38/38 length and SHA-256 matches; Git objects equal implementation commit `32812fa5` |
| Native source, tests and Cargo inputs | Equal #644 `84119b51` |
| Product source at the historical baseline | `internal`, `cmd`, `go.mod`, `go.sum` equal `17bc78fc`; not asserted equal to the later master |
| Current package checksums before the runner correction | 14/14 current and 3/3 legacy entries match |
| Current execution archive | All 80 published log entries match the manifest |
| Historical archive | All 624 files match SHA-256, Git blob SHA-1, lengths, modes and immutable `eb4ca1d4` tree; 2,364,014 uncompressed bytes |
| Plan coverage | Six stages; 24 primary R assignments; all 89 detailed acceptance bullets preserved in order and content |
| Guard regression | Six intended oracles matched; no real compiler or product suite executed |
| Owned executables before disposition | Zero executable paths within the owned FFI task root |

The previous Rust 19-per-profile, Go 54 top-level plus 68 nested checks per profile/instrumentation, compatibility checks and 13 identity controls remain the recorded executions in [RESULTS.md](RESULTS.md). They were **not rerun** for a script/documentation-only correction. The earlier native 71 checks per profile remain separate #644 evidence.

The full repository suites remain recorded failures, including API/CLI timeouts and process failures. Build/vet passed on that historical source. No assertion is made that the later master has the same failures or that these failures are harmless; it has not received a new broad run in this audit.

## Current branches and publication

At the fresh branch check on 15 November, current master was `a7d92d2852c4f6eb8afb0569ef76a545e38aacf7`. The qualification baseline remains `17bc78fc602efa204a74596409c939a3bf1ddc42`.

Their [immutable comparison](https://github.com/applicate2628/mcp-local-hub/compare/17bc78fc602efa204a74596409c939a3bf1ddc42...a7d92d2852c4f6eb8afb0569ef76a545e38aacf7) contains 12 commits and seven changed files: CLI/process/VTune/recovery/readiness tests and tray cancellation handling/tests. PRs #633, #634, #638, #640, #641 and #642 were merged by the parallel workstream. The receiving CreatorIdentity/ProvisionalProcess/SID formatter runtime files are unchanged in that range.

The observed open-PR snapshot has 23 drafts: nine Rust/measurement PRs and fourteen protected parallel PRs. The latter are #632, #635–#637, #639, #643 and #645–#652. Counts are timestamped observations, not claims about every later repository state. Historical [pr-audit.json](pr-audit.json) keeps its original 24-PR snapshot.

The user's continuation request, made in response to the draft-publication proposal, was conditional on a fresh audit: “продолжай, но сначала проведи аудит изменений”. This audit and the runner correction satisfy that condition for continuing publication of the prepared work in existing drafts. The exception is limited to draft publication despite the recorded broad-suite failures; it does not satisfy merge, formal review, release or installation gates.

Reuse #631 and #623, with review dependency **#625 → #644 → #631**. No new PR, parent branch rewrite, forced update, merge, ready-for-review transition, explicit bot/CI dispatch or installation belongs to this increment. Current published heads and base are verified from the PRs after publication; the exact source identities above remain historical execution anchors.

## Next receiving contract

Source review confirmed a semantic mismatch that must be resolved **before a product adapter**: `ProvisionalProcess.ObserveCreatorIdentity` is an observation, whereas experimental `AdmitIdentity` consumes the one-shot admission and can resume the child before returning. An adapter that merely stores callback fields and returns allow would add an unrequested effect; returning deny consumes admission. This is a future integration constraint, not a defect in the documented experimental admission API.

The next bounded design therefore requires a separate read-only native identity query through the retained owner and a private owner/custody boundary in `internal/process`. Existing Go creation, original wait errors, SID formatting issuer, context checks and retirement rules remain the reference. No fabricated `exec.Cmd`, Job, snapshot or digest can supply those capabilities.

The canonical plan in #623 records this next contract and its distinguishing checks. Product wiring remains open.

## Evidence and limits

[audit.zip](audit.zip) contains the actual guard harnesses, original baseline runner, both harness attempts, six case logs, guard receipt, branch/source verification records and disposition. Its internal manifest records exact entry lengths and SHA-256 values; [SHA256SUMS](SHA256SUMS) identifies the archive and updated loose files. Original [logs.zip](logs.zip) and [legacy/history.zip](../legacy/history.zip) remain unchanged.

Guard fixtures contain only inert owned test data and are moved directly to owned trash after recording and verification. Required scripts, execution records, source and normal qualified binaries remain outside trash.

Applied methods: Architecture Review, Code Review and Quality, Rust Unsafe Checker, Go Testing and Verification, Source Driven Development and Git Workflow; API and Interface Design and Architecture Decisions for the next receiving contract. Independent read-only review is not a new native run or formal bot approval.

Terms: FFI — foreign-function interface; ABI — application binary interface; DLL — dynamic-link library; SID — Windows security identifier; PR — pull request; SHA-256 — 256-bit Secure Hash Algorithm. A receipt records actual resource observations; a capability is authority tied to the retained owner.
