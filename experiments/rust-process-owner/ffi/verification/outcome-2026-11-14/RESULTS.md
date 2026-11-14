# Shared FFI outcome validation

**Implementation:** `4ebca16f53c4fc3ad4b07c47455745ff5fb58951`.
**Starting head:** `085a47fe165bc4be5fcc6ea6d35e2e8dc5c0b449`.
**Delivery:** existing [draft PR #631](https://github.com/applicate2628/mcp-local-hub/pull/631), dependent on unchanged native owner [#625](https://github.com/applicate2628/mcp-local-hub/pull/625).

## Contents

1. [Confirmed defect](#confirmed-defect)
2. [Correction and ownership contract](#correction-and-ownership-contract)
3. [Verification layers](#verification-layers)
4. [Reproduction and durable evidence](#reproduction-and-durable-evidence)
5. [Self-review and selected methods](#self-review-and-selected-methods)
6. [Merged-master delta and remaining gates](#merged-master-delta-and-remaining-gates)

## Confirmed defect

The pre-fix common Go decoder `result` in `go/bridge_windows.go` checked layout only after accepting status zero. Nonzero statuses bypassed that check, and successful statuses did not validate state/flag/cleanup relationships. Consequently a loaded DLL with an ABI1-shaped but inconsistent response could produce a trusted-looking successful `Outcome`.

Three initial cases reproduced the defect through actual Go/cgo/Rust calls with owned finite child processes:

**Table 1. Observed RED results.**

| Serialized return | Pre-fix Go behavior | Consequence |
|---|---|---|
| COMPLETE with unknown observation flag bit4 | Returned that Outcome with nil error | Unknown metadata accepted as successful observation |
| COMPLETE with failed-close slot and error code | Returned that Outcome with nil error | Contradictory completion/cleanup claims not rejected |
| Version2 layout with native-failure status3 and code87 | Returned `CallError` with87 without reporting layout mismatch; exposed Outcome | Native field interpreted using an incompatible declared layout |

See [initial run](../runs/outcome-red/run.log) and the [reproducible old-decoder replay](../runs/outcome-red-reproducible/run.log). The latter retrieves the exact old bridge from commit085a47f into a disposable Go caller copy; it does not silently revert the current worktree.

The controlled DLL changes **only serialized return values after the real owner has settled**. This proves a decoder failure under an inconsistent producer, not that the normal Rust implementation spontaneously produced these responses. A deliberately hostile in-process DLL could bypass such checks entirely; no sandbox, code-trust or memory-corruption protection is claimed.

## Correction and ownership contract

The correction is shared by create, admit, stop, finish and release through their existing `result` path. A small Go value validator checks the fixed ABI1 relationships. The header now documents the canonical zero-initialized fields already produced by the native code. No structure layout, exported function, Rust implementation, native-owner dependency, installer or product integration changes.

**Table 2. Decoder behavior.**

| Condition | Result |
|---|---|
| Header size/version invalid, on any status | `ErrInvalidOutcome`; no trusted returned Outcome |
| Unknown status/state/flag/cleanup-mask bit | Protocol rejection instead of default/fallback interpretation |
| Unflagged root/active data or terminal state without known root/nonzero PID | Rejected as inconsistent observation |
| Terminal state reports active members | Rejected; a root result does not imply whole-operation settlement |
| Cleanup mask, state and individual error codes disagree | Rejected; no masked cleanup failure or invented success |
| Valid CleanupFailed state with one or more of the three close slots | Accepted with exact root exit/code mask; nil call error does not mean cleanup succeeded |
| Successful Finish returns UNKNOWN rather than a settlement | Rejected in the common finite/context finish path |
| Valid layout plus nonzero call status | `CallError` retained; native code interpreted only for native-failure status |
| Unknown layout plus nonzero call status | Preserve separately returned status, but do not trust the native-code field at a potentially wrong offset |

On invalid response data the exposed `Outcome` is zero. This means **no trustworthy observation is available**, not that the call had no effect. `errors.Is` can identify `ErrInvalidOutcome`; `errors.As` can still find the independent call-status error where one exists. No log includes user payloads or arbitrary child output.

A non-null allocation transferred by native create still belongs to the caller even with a response error. A release call that cleared its slot has deallocated the owner even when response metadata is wrong. Those facts remain outside the observation validator; the correction neither discards ownership nor authorizes retry, replay, a hidden kill or early DLL unload.

This intentionally rejects inconsistent experimental ABI1 producers that the old caller tolerated. It does not promise forward-compatible acceptance of unknown semantics; future formats need an explicit compatible contract. It does not reconstruct all native truth from a struct or claim that a self-consistent lie can be detected in-process.

## Verification layers

### Controlled foreign responses

The [final runner](../runs/outcome-final/steps.json) executed **26 variants** with all56 expected steps matched, including one intentional exit7 control for error-code capture:

- **17 invalid return variants**, each exercised by finite `Finish` and `FinishContext`.
- **7 valid serialized CleanupFailed masks**, each exercised by both wait forms, preserving real child exit259 and every selected error code.
- **2 previous allocation-custody regressions**: malformed create metadata and malformed release metadata.

These are 26 fixture configurations, not26 new independent test functions. The opt-in implementation adds two functions under `ffi_outcome_faults`; the two custody regressions reuse existing `ffi_faults` functions. Repeated calls/configurations are not added to ordinary test counts.

Each foreign case verifies the actual finite child's marker, refusal to unload the library while its owner is retained, explicit release of the **same** allocation and safe module close. In the invalid cases both protocol rejection and absence of trusted result data are asserted. A nonzero native call cause is independently checked where applicable. Compilation failure is not an accepted RED result: the runner requires the precise semantic assertion.

### Actual native cleanup failures

The existing protected-close runner was repeated against the **new decoder**: [regression steps](../runs/native-cleanup-outcome-regression/steps.json). It exercised actual Windows CloseHandle failures for all seven nonempty thread/process/Job masks through both Finish forms, debug/release DLLs with race-enabled repetitions, plus `cgocheck2` and vet. Its normal-DLL absence controls also passed.

That is distinct from the synthetic return-value variants above. The test-only original-handle accessor is added only to an owned source copy; ordinary Rust artifacts remain uninstrumented. Recovery is limited to those continuously held, explicitly protected test handles and does not permit retrying arbitrary stale production handle values. The earlier [native-cleanup method](../native-cleanup-2026-11-14/README.md) remains authoritative.

### Complete ordinary experiment

Both [implementation run](../runs/outcome-ordinary-implementation/steps.json) and [final committed-code run](../runs/outcome-ordinary-final/steps.json) completed all22 expected steps. The initial run identifies the implementation overlay on085a47f. The final run identifies code commit4ebca16 with documentation/evidence work still pending; neither is represented as a clean checkout of a future evidence commit.

**Table 3. Fresh ordinary verification.**

| Layer | Result |
|---|---|
| Complete Go experimental module | **49 top-level functions,63 nested cases,0 skips** |
| New normal contract tests | Two functions with17 accepted-value and15 rejected-value subcases |
| Debug and release ordinary DLL | Full Go module with race detector and three repetitions per DLL passed |
| Go pointer/static/format | `cgocheck2`, `go vet`, gofmt checks passed |
| Rust ABI crate | **14 unit tests**, debug/release, Clippy and formatting passed |
| Unchanged native owner | **53 checks**, debug/release, Clippy and formatting passed |

Observed tools were Go1.27.4/windows-amd64 and Rust1.99.1/MSVC with the already installed C compiler. No toolchain, crate, module or dependency was installed/downloaded. Source hashes identify exactly what ran. The full Hub repository, installed product and native Unix backends were not executed by these experiment gates. Go race instrumentation is not a proof of all C/Rust/native schedules.

## Reproduction and durable evidence

The [runner](run.ps1) uses an existing cache and creates a new private parent/FFI source copy under the approved lab. All normal source and ordinary DLL hashes are verified unchanged across each controlled run. Test sources, exact mutation recipes, actual command outputs, expected/actual exits, source identities, ordinary artifact identities and limitations are committed here. Reports are grouped per run instead of one record per compiler command.

```powershell
# From experiments/rust-process-owner/ffi; all paths below are examples.
.\verification\outcome-2026-11-14\run.ps1 `
    -LabRoot 'D:\dev\ChatGPT\your-owned-lab' `
    -CCompiler 'C:\path\to\existing\gcc.exe' -RunName 'new-final'

# Exact old decoder in an isolated copy; expect semantic test failures.
.\verification\outcome-2026-11-14\run.ps1 `
    -LabRoot 'D:\dev\ChatGPT\your-owned-lab' `
    -CCompiler 'C:\path\to\existing\gcc.exe' -RunName 'new-red' -Phase red `
    -Cases @('unknown-flags','cleanup-on-complete','bad-layout-native')
```

The ordinary `owner_child` must have been built by the existing offline runner first. Existing report/work directories are never overwritten. No automatic branch mutation, push, install or cleanup is performed. Raw source mutations apply to disposable copied Rust/Go only, not a live installation. Binaries and compiler caches are rebuildable and intentionally not committed.

The initial setup command had a PowerShell quoted-pipeline parse error before any writes. A later read-only diagnostic regex command also had a quoting error before execution. These were corrected tool-command mistakes, not source/runtime failures. The initial response-defect RED and every subsequent result are preserved; there is no suppressed failed product test in the current completed suites.

Artifact hashes identify individual builds, not byte-for-byte reproducible toolchains. A later ordinary rebuild may have a different binary hash; historical hashes are not rewritten to match it. Compare each run against its own source/artifact manifest.

## Self-review and selected methods

**Table 4. Eleven skills actually read and applied.**

| Skill | Concrete use |
|---|---|
| Using Superpowers | Select the current unfinished boundary rather than repeat loader/cancellation work |
| Executing Plans | Recover exact remote head and committed ledger; keep required evidence in PR |
| Test-Driven Development | Reproduce contradictory responses with real foreign calls before changing the decoder |
| API and Interface Design | One shared validity contract; observation errors do not erase ownership transfer |
| Architecture Review | Compare against merged-master identity contract and keep native-owner responsibilities separate |
| Using Git Worktrees | Verify the existing private clone/branch; never change active main |
| Systematic Debugging | Trace the failure to status-before-layout decoding rather than special-casing error87 |
| Go Testing and Verification | Separate pure value tests, controlled foreign returns and actual kernel-close evidence |
| Go Security Hardening | Use only owned finite fixtures; reject malformed observations without a hostile-DLL safety claim |
| Ponytail Review | One small validator, reused decoder/dispatcher; no registry, new supervisor or dependency |
| Verification Before Completion | Fresh complete experiments, exact source checks, separate code/evidence commits |

Confirmed finding: `go/bridge_windows.go::result` on085a47f skipped layout validation on errors and accepted contradictory result values on success. Required precondition is an incompatible/defective serialized response from a loaded producer; consequence is an untrustworthy completion or native-cause interpretation. The minimal correction validates the shared boundary rather than adding a separate condition to every caller.

Own review also verified: valid exit0/259 remains supported; state UNKNOWN stays legal for operations that do not report settlement, but not successful Finish; unknown metadata yields no trusted observation; valid native errors and allocation facts remain observable; valid cleanup masks are not collapsed; ordinary Rust/parent code and dependencies are unchanged. Missing raw-pointer safety, hostile-code isolation, every kernel-failure class and full admission parity are **limitations**, not defects solved by these tests.

No independent reviewer/agent or paid model ran. A successful compiler/test result is not proof of mathematical properties, total leak freedom, cross-platform readiness or the safety of loading untrusted code.

## Merged-master delta and remaining gates

[PR #616](https://github.com/applicate2628/mcp-local-hub/pull/616) is now merged at17bc78fc, but its source delivery does not clear its remaining full-suite/native/installed gates. The immutable [admission comparison](ADMISSION-PARITY.md) records the actual gap: the accepted Go snapshot requires original birth, owner SID/validated formatting, retained-Job membership and root wait/liveness evidence in addition to PID/image. The Rust callback still exposes only the latter two fields. No default values or weaker policy were invented.

Next work must extend the original native capability and versioned observation where the actual consumer requires it, then reconcile the experiment family with merged master under review. This iteration does not rebase #625/#631 or install the Rust library in the working Hub.

Other gates remain: remaining descriptor/pump/backpressure/recovery profiles, global inheritance coordination, other allocator/panic/loader/kernel failure schedules, native Linux/macOS implementation and testing, equivalent-workload measurements, independent lifecycle/security review and binary/version/install acceptance. The prior native-cleanup and loader qualifications remain their own bounded evidence, not universally complete assurances.

Primary references: [Go errors.Is/As/Join](https://pkg.go.dev/errors), [cgo interoperation rules](https://go.dev/wiki/cgo), and the repository's [ABI1 header](../../include/owner_v1.h). The header and matching Rust implementation define the value contract; general documentation does not prove a particular native outcome.

No merge, release, installation, active-primary-repository/configuration change, external provider/model launch or explicit workflow/bot dispatch occurred. Every necessary result and recovery instruction belongs to this PR rather than the continued survival of a local machine.

### Completed-copy retirement

After all tests completed, five disposable qualification source/artifact copies
were checked against their exact manifests and moved directly to approved trash.
The ordinary source, normal targets/cache and every necessary result remained
outside trash. No deletion or automatic emptying occurred. The [preflight](cleanup-preflight.json),
[receipt](cleanup-receipt.json) and [retirement script](retire-copies.ps1) are saved.
A PowerShell JSON-array wrapper and then an undefined text-encoding variable
caused two preflight failures before any movement; the [diagnostic](cleanup-preflight-diagnostic.log)
records both. Explicit record selection and UTF-8 construction then verified the
original hashes rather than bypassing them. This does not change code or test outcomes.
