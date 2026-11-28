# Versioned foreign identity observation — 15 November 2026

**Result:** the additive identity boundary is implemented and its recorded scoped qualification passes. Both required full-repository test suites failed on the recorded source. A subsequent [fresh audit](AUDIT.md) fixed the reproduction runner's evidence-overwrite risk; the user authorized continuation of draft publication after that audit. No production owner or installed runtime is switched.

## Contents

1. [Source and scope](#source-and-scope)
2. [Contract and decisions](#contract-and-decisions)
3. [Executed qualification](#executed-qualification)
4. [Negative controls](#negative-controls)
5. [Repository gates and publication](#repository-gates-and-publication)
6. [PR audit and compaction](#pr-audit-and-compaction)
7. [Review and methods](#review-and-methods)
8. [Reproduction and evidence](#reproduction-and-evidence)
9. [Remaining work and sources](#remaining-work-and-sources)

## Source and scope

**Table 1. Source identities.**

| Input | Exact identity |
|---|---|
| Accepted master used for the recorded qualification after #616 | `17bc78fc602efa204a74596409c939a3bf1ddc42` |
| Native-owner parent #625 | `c5672f49f65713c7555e8841e46de75b045da5f8` |
| Qualified native identity #644 | `84119b51e089cce9ca42da2fce35a012eb86cb7c`; SID implementation `3ef24b3473900384d8fc34204d4555a9bb30a5db` |
| Frozen previous #631 | `eb4ca1d40244ba1915638b76c3e30b2a684d9b07` |
| Local ordinary merge of #644 into #631 | `aa7f4ea41631cbb29bd86b9e3c8c149e4a9f7675` |
| New implementation commit | `32812fa5c3793d9af9f3d5770b614f0a96d4d8da`, nine source/test/header/README files |

The qualification actually ran on the local merge plus an explicitly recorded working implementation. All **38 compiler/test input hashes** were rechecked against the subsequent implementation commit. [source-identity.json](source-identity.json) preserves that actual starting state; [source-hashes.json](source-hashes.json) identifies the bytes. This is not retrospectively described as a clean run of a future evidence commit.

Native manifests, lockfile, source and tests match #644. Its earlier **71 native checks per debug/release profile**, including real token acquisition/query/protected-close cases, remain [separate native evidence](https://github.com/applicate2628/mcp-local-hub/blob/84119b51e089cce9ca42da2fce35a012eb86cb7c/experiments/rust-process-owner/verification/sid-2026-11-14/RESULTS.md); they were not rerun or relabelled here.

Only `experiments/rust-process-owner/ffi/` changes in this increment. The prepared review chain is **#625 → #644 → #631**; the ordinary local merge changes neither parent and does not accept #644 into master. The frozen remote head above identifies the earlier baseline; current draft publication and the fresh master comparison are recorded in [AUDIT.md](AUDIT.md) and the PR metadata.

## Contract and decisions

The additive export `mh_admit_identity1` and Go `Owner.AdmitIdentity` expose PID, image, creation time, exact original-Job membership, wait state and binary TokenUser SID. The [header](../../include/identity_v1.h), [Rust producer](../../src/identity.rs), [Go decoder](../../go/identity_windows.go) and [README](../../README.md#additive-identity-protocol) define the complete contract.

**Table 2. Chosen boundaries.**

| Decision | Reason and behavior |
|---|---|
| Independently versioned schema | Existing ABI1 exports, `mh_version1()==1` and 48-byte `mh_out` remain unchanged. Identity is 120 bytes; its report is 24 bytes on Windows AMD64 |
| Optional new symbol | Old ABI1 DLLs still load. `ErrIdentityUnsupported` occurs before admission; there is no automatic PID/image or boolean fallback |
| Original retained owner | Every query uses the existing `ProcessView`; no PID reopening, second owner registry or exported process/token handle |
| Explicit known facts | Zero PID/birth, unknown Job/wait and NotSignaled outside the original Job are rejected. Separate observations are not an atomic snapshot |
| Owned copies | Raw UTF-16 image units and validated SID bytes are copied before Go policy; they survive callback return, owner release and library unload |
| Independent token errors | Query and close codes remain separately discoverable. Failed token close exposes no successful SID and never occupies the old thread/process/Job cleanup slots |
| One admission attempt | Query/close failure, denial, invalid callback data and contained panic consume admission; the original owner remains available for explicit stop/finish/release |
| Separate stages and effects | NONE means collection did not start, not retry authority. COMPLETE means collection completed, not policy approval or resume/settlement success |
| Late invalid report | Protocol failure after actual resume preserves the owner and the real child outcome; it does not imply no effect |
| Callback lifetime | Existing owner mutex covers the synchronous callback. A call-scoped integer `cgo.Handle` is deleted after return. Reentry, Goexit, foreign unwinding and retained native pointers are prohibited |

There is no new dependency, lockfile change or production fault hook. These observations do not construct product `CreatorIdentity` or the private `SIDFormatObservation` receipt.

## Executed qualification

Installed environment: **Windows AMD64, Go 1.27.4, Rust/Cargo 1.99.1 with MSVC target, existing GCC for cgo**. Cargo uses existing offline/locked dependencies. Source, caches, targets, temporary data and fixtures belong to the task under `D:\dev\ChatGPT\mcphub-ffi-identity-20261115`.

**Table 3. Fresh affected-boundary results.**

| Check | Observed result |
|---|---|
| Rust FFI tests, debug and release | **19 passed in each profile**, zero failed/ignored/filtered; this FFI crate has zero doctests |
| Rust static checks | fmt and Clippy all targets with `-D warnings`: exit 0 |
| Go with debug DLL | **54 top-level + 68 nested**, zero failures/skips, separately with `-race` and `GOEXPERIMENT=cgocheck2` |
| Go with release DLL | **54 top-level + 68 nested**, zero failures/skips, separately with `-race` and `GOEXPERIMENT=cgocheck2` |
| Go format/static checks | Empty `gofmt -l`; `go vet .`: exit 0 |
| Exact frozen old Go with new debug DLL | **49 top-level + 63 nested**, zero failures/skips, with `-race` |
| New Go with old DLL | One explicit opt-in compatibility test passes; unsupported preserves deliberate subsequent legacy admission |
| Malformed producer cases | All eight required rejections/error mappings observed |
| Weakened consumers | All four copies fail their named test with the intended diagnostic |
| Source integrity | All 38 qualified inputs unchanged after controls and before the implementation commit |

Debug/release identify DLL profiles; the child fixture is the separately built debug `owner_child.exe`. Ordinary tests and opt-in controls are reported separately. Go race/cgo instrumentation is not a proof of every C/Rust/Windows schedule.

Five new ordinary Go functions contain five nested cases. They compare child-reported PID/birth and TokenUser SID independently, check the no-effect-before-policy boundary and copy lifetime, denial, panic including `panic(nil)`, nil policy, stopped/released owners, and owner/library exclusion. Private binary SID markers are not published or printed.

Five new Rust tests cover layout, output preflight, query classification, independent token errors and a real retained-owner sequence: four preflight refusals leave a subsequent one-shot denial available, with explicit termination/settlement afterward.

An initial qualification attempt stopped at Go formatting after strengthening the mutex test. Formatting was corrected and the full affected qualification repeated. That failed preflight remains recorded separately.

## Negative controls

**Table 4. Representative malformed producer responses.**

| Copy-only fault | Required result |
|---|---|
| Unknown identity version | Protocol error before policy, denied admission, no child effect |
| Zero creation time | Missing process fact rejected |
| Unknown Job value | Unknown observation rejected |
| Invalid SID revision | Invalid binary structure rejected |
| Nonzero unused SID storage | Extra nonzero bytes rejected |
| Unknown report version after real resume | Error with one callback; actual successful child result still observed |
| Synthetic query 5 and token close 6 | STATE, zero callbacks, both typed codes and both native error causes retained |
| Synthetic close-only 6 | STATE, zero callbacks, no invented query error |

The two token errors are synthesized **after a successful real token query and close**. They prove foreign error transfer, not spontaneous Windows failures. #644 contains the distinct real native-failure evidence.

Four weakened Go copies remove observation metadata validation, remove report metadata validation, erase the close code or require the optional export. Every named test fails at its intended assertion, not at compilation. The loader case has an unchanged-client old-DLL baseline and preserves numeric missing-symbol error 127 in a copy-only diagnostic, independently of Windows message language.

[controls.json](controls.json) records exact guarded mutations, commands, hashes and outcomes. [controls.ps1](controls.ps1) reproduces the matrix. Eight cases are representative, not exhaustive field/length fuzzing. There are **13 recorded cases: eight producer cases, one compatibility baseline and four semantic negatives**, all accepted by their distinct oracles.

## Repository gates and publication

**Table 5. Required repository commands.**

| Command | Exit and consequence |
|---|---|
| `go build ./...` | 0 |
| `go vet ./...` | 0 |
| `go test -count=1 -timeout 5m ./...` | 1; failures in process/API/CLI packages, with API/CLI package timeouts |
| `go test -tags=test_state_path_env -count=1 -timeout 5m ./internal/api/ ./internal/cli/` | 1 |

[repository-gates.json](repository-gates.json) records every named failed test, package result, command and original log hash. The collection script's own exit 0 means it finished recording these failures; it is not a passing gate.

Product sources and root Go dependency files in this run match historical accepted master `17bc78fc602efa204a74596409c939a3bf1ddc42`. This experiment neither changes those packages nor establishes the root cause of each failure. It does not silently incorporate the active parallel fixes.

The first attempt encountered an empty task-local module cache with `GOPROXY=off`. The corrected run read an already existing local module cache through a **file-only proxy**, materializing locked dependencies into D:; no network package proxy or toolchain installation was used. Both attempts are retained.

**Historical publication gate:** CLAUDE.md, “Step 1 — Pre-push local verification”, requires both full suites to be clean before `git push`, so the initial prepared checkpoint was not pushed. In response to the draft-only publication proposal, the user then instructed continuation after a fresh audit. [AUDIT.md](AUDIT.md) records that review, its runner correction and the limited draft-publication scope. The recorded test failures remain merge/acceptance blockers; no bot PASS, merge or installed acceptance is inferred. Internal independent review does not substitute for required formal review.

## PR audit and compaction

The first audit found 23 open draft PRs: nine Rust/measurement and fourteen protected parallel changes. The later [snapshot](pr-audit.json) has **24 open PRs**, including new parallel #647: nine in this workstream and fifteen outside it. This increment creates no PR.

No confirmed duplicate implementation justified closing a live review. #630 is already incorporated into #629. The prepared #625 → #644 → #631 chain separates native ownership, native identity and its foreign interface. Closing #644 before acceptance would remove a live review boundary.

The old #631 diff contained 648 files, including **624 historical evidence files**. All are preserved with exact committed bytes and original repository paths in [history.zip](../legacy/history.zip). Root independently checked each decompressed file against both immutable Git blob SHA-1 and SHA-256: **624 files, 2,364,014 uncompressed bytes**. The archive is 660,113 bytes, SHA-256 `1df805b8d50ef8e10eaa9404efbee341ba85daeea3c94e3c4a6dd7a572ee342e`.

The [index](../README.md), [manifest](../legacy/manifest.tsv) and [independent receipt](../legacy/compaction.json) retain recovery and historical milestone links. README links to old reports are pinned to the frozen commit. Only after archive verification and a second retained copy, the clean redundant historical checkout was moved directly to owned trash. No permanent deletion or trash purge occurred.

## Review and methods

Author review covered requirements, layout, fact validity, memory bounds/lifetimes, owner retention, errors/panics, one-shot semantics, missing-export compatibility and complexity. A separate read-only reviewer examined Rust/header/Go and tests and found no concrete blocking defect. That reviewer did not execute tests and did not grant formal product acceptance.

A concrete proof limitation was corrected: the concurrent Finish goroutine could run only after policy release. While the callback is held and before a competitor starts, the test now uses `TryLock` to directly verify that the existing owner mutex remains held. All final Go profile runs include this assertion; no specific scheduler interleaving is claimed.

A hypothetical Rust `raw_os_error()==Some(0)` yields inconsistent native-kind/code-zero metadata and is rejected by Go. No replacement code is fabricated and no spontaneous occurrence is claimed.

Applied skills: Architecture Review and API/Interface Design for contracts; Source Driven Development and Rust Resource Lifecycle for original-owner authority; Rust Unsafe Checker for foreign memory and unwind obligations; Go Testing/Verification and Verify and Stop for calibrated proof; Git Workflow and Ponytail Code Simplification for existing-PR reuse and evidence compaction; Code Review and Quality for self/independent review; Documentation and ADRs for reasons, reproduction and limits. Rust skills were read at pinned `5c40d3ad785193231b7d0dbfb8e1eb447e5edd94`. Internal agents assisted bounded subtasks; no external paid-model run or explicit bot/CI dispatch was used.

## Reproduction and evidence

Use a **new owned** task root under D:\dev\ChatGPT with this repository in its `source` child. Frozen commits in Table 1 and already installed tools/cache must be available.

1. Build `owner_child` with the native manifest, `--offline --locked --bin owner_child`, `CARGO_TARGET_DIR=<task root>\target`, and all temporary/cache paths on D:. The preserved focused script records the actual command/environment.
2. Execute [run.ps1](run.ps1) with mandatory `-TaskRoot <new root>`. The corrected reproduction runner rejects existing qualification or compatibility output before any writes. It runs affected checks and assembles exact old-FFI source/DLL for compatibility. The original executed runner remains unchanged in logs.zip; the correction and its six guard controls are recorded in [AUDIT.md](AUDIT.md).
3. Execute [controls.ps1](controls.ps1) with explicit repository `-SourceRoot`, new debug `-NewDll`, child `-Child` and built old `-OldDll`. It never edits original sources.
4. Run [repository-gates.ps1](repository-gates.ps1) for the separate complete gates, adapting only owned paths and existing local cache location.
5. Verify [SHA256SUMS](SHA256SUMS), [log-manifest.json](log-manifest.json), source and artifact hashes.

[qualification.json](qualification.json), [artifacts.json](artifacts.json), [controls.json](controls.json) and [repository-gates.json](repository-gates.json) are the reviewable records. [logs.zip](logs.zip) retains command logs, recorded execution scripts, preflight failures and prior own-PR descriptions without hundreds of changed files.

Published logs are normalized to UTF-8/LF; textual SID strings and user-profile prefixes are replaced by explicit markers. The manifest records original and published lengths/hashes and replacement counts. Raw originals remain in the owned execution directory. Normalized output is not described as original bytes. Private SID fixtures, copied source trees, binaries, caches and targets are excluded.

After the evidence readback, six completed control-copy/target/cache and private identity-fixture directories were moved directly to owned trash. All 494 control-source files matched the recorded original or final mutation hashes; no owned executable remained active. [disposition.json](disposition.json) records the moves. Active source, normal qualified binaries, compatibility inputs, original logs and broad-failure fixtures remain outside trash.

## Remaining work and sources

The next receiving contract must preserve the actual Go `ProvisionalProcess`, context/custody, identity-in-flight/retirement conditions, and private issuer/input binding of `SIDFormatObservation`. A copied snapshot, SID string or digest cannot mint those capabilities; manufacturing an `exec.Cmd`/Job owner around the Rust process is not a valid adapter.

All six residual stages remain open. Native Unix, other acquisition/allocation/kernel/loader schedules, product I/O and launch policy, executable trust, workload benefits, final independent review and installed/release/recovery acceptance remain unproved.

**Sources**

1. [Go runtime/cgo.Handle](https://pkg.go.dev/runtime/cgo#Handle) — integer representation and explicit lifetime.
2. [cgo pointer rules](https://pkg.go.dev/cmd/cgo#hdr-Passing_pointers) — retention and callback obligations.
3. [Rust Nomicon: FFI and unwinding](https://doc.rust-lang.org/nomicon/ffi.html#ffi-and-unwinding) — unwind boundaries.
4. [Accepted CreatorIdentity](https://github.com/applicate2628/mcp-local-hub/blob/17bc78fc602efa204a74596409c939a3bf1ddc42/internal/process/creator_identity.go) — original owner and private receipt.
5. [Native TokenUser qualification](https://github.com/applicate2628/mcp-local-hub/blob/84119b51e089cce9ca42da2fce35a012eb86cb7c/experiments/rust-process-owner/verification/sid-2026-11-14/RESULTS.md) — native errors distinct from synthetic foreign mapping.

**Terms:** FFI — foreign-function interface; ABI — application binary interface; DLL — dynamic-link library; PID — process identifier; SID — Windows security identifier; Job — retained Windows containment object; UTF-16 — 16-bit Unicode encoding units; cgo — Go/C interoperability; SHA-256 — 256-bit Secure Hash Algorithm; capability — authority tied to a real retained owner; receipt — an observation and associated resource outcomes.
