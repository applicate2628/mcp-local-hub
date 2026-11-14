# RX-02 / RX-03 TokenUser SID: native qualification

**Status:** bounded Windows observation qualified in the draft [identity PR #644](https://github.com/applicate2628/mcp-local-hub/pull/644). This is not product admission, a new foreign identity interface or completed migration.

## Contents

1. [Source and run identity](#source-and-run-identity)
2. [Implemented contract](#implemented-contract)
3. [Native results](#native-results)
4. [Frozen consumer compatibility](#frozen-consumer-compatibility)
5. [Review and evidence quality](#review-and-evidence-quality)
6. [Reproduction](#reproduction)
7. [Evidence and disposition](#evidence-and-disposition)
8. [Remaining gates](#remaining-gates)
9. [Sources and terms](#sources-and-terms)

## Source and run identity

**Table 1. Exact inputs.**

| Input | Identity and scope |
|---|---|
| Accepted product reference | master `17bc78fc602efa204a74596409c939a3bf1ddc42`; the Go token-user helper at that snapshot remains the comparison reference [7] |
| Native parent | #625 `c5672f49f65713c7555e8841e46de75b045da5f8` |
| Previous identity head | `8e80fad662ef20bd5cbc2d1ca4808a9494d6f30e`, including creation-time/Job/wait evidence [8] |
| SID implementation | [`3ef24b3473900384d8fc34204d4555a9bb30a5db`](https://github.com/applicate2628/mcp-local-hub/commit/3ef24b3473900384d8fc34204d4555a9bb30a5db): six source/test files, 642 added lines; manifests, lockfiles and dependency versions unchanged |
| Native final run | 2026-11-14, 21:01:17–21:01:32 UTC; previous identity head plus explicit working overlay. Its 13 source hashes were checked against the subsequently committed SID code. The run is not relabelled as having started at the later commit |
| Frozen foreign consumer | #631 `eb4ca1d40244ba1915638b76c3e30b2a684d9b07`, only its existing FFI source/header/manifests combined with the native SID source |
| Consumer final run | 2026-11-14, 21:02:31–21:02:57 UTC; native Git head already `3ef24b34`; 36-file combined source manifest |
| Actual platform/toolchains | Windows NT 10.0.26100.0, AMD64; Rust/Cargo 1.99.1, Go 1.27.4 windows/amd64; installed MSVC linker and installed UCRT64 GCC for cgo; existing cached dependencies with Cargo offline/locked |

The two branches remain siblings of #625. The consumer assembly is an owned temporary qualification copy, not a merge, a new backend or a claim that the foreign interface exports SID. Later documentation-only commits do not change the qualified source files.

## Implemented contract

The additive Windows method is `ProcessView::token_user_sid() -> Result<Vec<u8>, TokenUserError>`. Its value is an independent binary SID copy from the user of the original process's primary token. `TokenUser` is queried, not `TokenOwner`. The helper opens a query-only token from the retained process handle [1]; it never reopens a process by its numeric process identifier and never substitutes the caller's current process.

The same `NativeOwner` continues to own the process, primary thread and Job. A SID contains an observation; it does not confer authority to close, resume, terminate or substitute that process. No raw handle, additional process owner, dependency, worker thread or runtime fault hook is introduced.

### Bounded query and validation

`src/token_user.rs` uses an initialized word-aligned buffer large enough for `TOKEN_USER` plus `SECURITY_MAX_SID_SIZE`, rounded up to a whole machine word. Microsoft's `SE_TOKEN_USER` documentation identifies this as maximum TokenUser storage suitable for stack allocation [3]. The implementation does not allocate an attacker-sized native query buffer or retry unbounded size growth.

The validation order is explicit:

1. Check the returned byte count against the complete `TOKEN_USER` header and owned buffer capacity before reading the header.
2. Check that the embedded SID address is non-null, follows the header, is correctly aligned, and lies inside the returned bytes. Address arithmetic is checked for underflow/overflow.
3. Obtain the fixed SID header length with `GetSidLengthRequired(0)` [4]. It is eight bytes; `size_of::<SID>()` includes a trailing placeholder subauthority and is not the variable-length minimum.
4. Read the subauthority count only within the checked header. Bound it by the SDK maximum of 68 bytes and four bytes per subauthority, then check the entire count-dependent SID extent.
5. Derive the validator pointer from the owned buffer base and the checked offset. Call `IsValidSid` only after the full readable extent is bounded [5]. Invalid SID data becomes `InvalidData`; that API has no extended error information, so the code does not read `GetLastError` for it.
6. Reserve and copy exactly the validated SID bytes into the returned vector. Allocation failure is an observation error and still flows through explicit token cleanup.

This checks the actual native result before copying. The parser tests supply synthetic buffers; they do not claim that Windows normally returns malformed TokenUser data.

### Observation and cleanup are separate outcomes

**Table 2. Return value after a token was acquired.**

| Query/copy outcome | Token close outcome | Public result |
|---|---|---|
| SID copied | Close succeeds | `Ok(Vec<u8>)` |
| Observation fails | Close succeeds | `TokenUserError::Observation` with the original observation error |
| SID copied | Close fails | `TokenUserError::Cleanup` with native operation/code; no successful SID escapes |
| Observation fails | Close fails | `TokenUserError::Cleanup` preserves both the observation error and native close failure |

After acquisition, the ordinary query result is recorded before consuming the sole token owner into its raw handle for one explicit `CloseHandle` attempt. A failing close captures the native error immediately. There is no second `OwnedHandle` left to repeat a potentially ambiguous close on a recycled numeric handle. Unwinding before that normal close path has a best-effort ownership fallback; it is not an explicit cleanup receipt.

A failed token close can leave a native resource unreleased; the API reports that fact and does not invent successful cleanup. Later process settlement does not erase the token error already returned to the caller. Test-only cleanup can safely release a deliberately protected token because the fixture knows exactly why it remained live; that does not authorize retrying arbitrary production close failures.

## Native results

**Table 3. Final native run; all ten runner steps met their expected outcome.**

| Check | Observed result |
|---|---|
| `cargo fmt --all -- --check` | Exit 0 |
| Debug full suite | 28 unit + 41 integration + 2 compile-fail documentation tests = **71 passed**, zero failed/ignored/filtered |
| Release full suite | The same **71 passed**, zero failed/ignored/filtered |
| `cargo clippy --offline --locked --all-targets -- -D warnings` | Exit 0 |
| Six semantic negative controls | Each selected test failed with exit 101, the expected test name and its intended diagnostic; Table 4 |
| Source preservation | The 13 ordinary source/manifests hashes remained unchanged by the copy-only controls and match the committed implementation |

The 41 integration tests comprise ten explicit-context, fifteen lifecycle, five creator-crash and eleven standard-stream cases. This increment adds eight unit test functions and one lifecycle integration function to the previous 62-check suite.

Four parser functions cover minimum and maximum valid SID extents, invalid returned lengths, null/foreign/header-overlap/misaligned/outside addresses, invalid revision, excessive subauthority count and truncated SID bodies.

Four native unit functions use real Windows calls: a restricted retained process denies token opening; a real zero-query-right token denies the query and is closed; protected token close fails after either successful or failed query; and 64 repeated observations do not grow the measured process handle count. The copied SID remains stable after process settlement. The process-opening assertion is inside the admission callback, so a later owner error cannot hide substitution of another process.

The integration child independently obtains its own TokenUser SID with the native size-query pattern and `GetLengthSid`, not the library's copying helper. Its binary bytes equal the parent's observation; the marker does not exist during the suspended admission callback. The real account SID is kept in private fixture files and is not included in the published evidence.

**Table 4. Copy-only negative controls.**

| Deliberate mutation | Required discriminating failure |
|---|---|
| Use `GetCurrentProcess()` instead of the borrowed process handle | `token lookup ignored retained process handle` |
| Ignore failed `CloseHandle` | `protected token close became success or lost its receipt` |
| Discard a query error when close also fails | `token cleanup erased the query error` |
| Change the native close error code by one | `token close changed native error` |
| Replace the native returned length with buffer capacity | Rejection assertion fails on a synthetic malformed returned length |
| Skip `IsValidSid` rejection | Rejection assertion fails on a synthetic invalid SID |

The runner verifies both the named failed test and its diagnostic, not just a nonzero exit. Negative controls intentionally filter out the other 27 unit tests; this is distinct from the unfiltered full suites. They never edit the ordinary source tree.

## Frozen consumer compatibility

The unchanged earlier [consumer runner](../identity-2026-11-14/consumer.ps1) was invoked with explicit `-FfiRef eb4ca1d40244ba1915638b76c3e30b2a684d9b07`. No duplicate runner or new foreign export was needed.

**Table 5. Eight successful consumer steps.**

| Step | Observed result |
|---|---|
| Native child build | Exit 0 |
| Existing Rust ABI tests | **14 passed**, zero failed/ignored/filtered |
| Debug DLL build | Exit 0 |
| Release DLL build | Exit 0 |
| Go with debug DLL, race detector | **49 top-level + 63 nested checks**, zero skipped, one run |
| Go with release DLL, race detector | **49 top-level + 63 nested checks**, zero skipped, one run |
| Go with `GOEXPERIMENT=cgocheck2` | Exit 0 |
| `go vet .` in the experimental Go consumer | Exit 0 |

These calls preserve compatibility of the existing process owner and ABI1 consumer. Its observation interface still carries PID/image only. The run neither exercises a foreign SID API nor establishes production Go identity/capability parity.

## Review and evidence quality

Author review covered the retained-process binding, pointer bounds/alignment/provenance, native error capture, token ownership and every ordinary cleanup branch, absence of new dependencies, compatibility and change scope. A separate read-only code review found no blocking runtime defect. It did identify a weak test assertion: accepting any nonzero close error would not prove preservation of the actual error.

The test was strengthened before the final full run. Its fixture deliberately protects its own token, records the actual native failed-close code, and requires the library's receipt to match it. The `wrong-close-code` control demonstrates that changing the code is detected. This fixture-only reference attempt is separate from the one explicit close attempt in the library.

Close protection alone cannot count how often the helper tries to close a handle: repeated protected calls would all fail. The one-attempt claim is established by the inspected control flow; the runtime fixture establishes the observed result combinations and resource recovery under its artificial protection. No debugger/Application Verifier qualification is inferred.

The first focused run already passed eight unit and one integration function, but predates the strengthened exact-code assertion. It is retained only as history. Final claims in Tables 3–5 use the complete final logs and exact source manifests. The independent evidence review checked all 13 native and 36 combined source hashes, both runner hashes, the consumer archive and all six single-mutation copies. It found no source/count discrepancy; a cross-branch plan link and reproduction-layout wording were corrected. This read-only consistency check is not an independently rerun native qualification or final product approval.

## Reproduction

Use the existing authorized toolchains and cache. Do not install missing dependencies or write new build/cache/temp artifacts outside the approved work area. From an owned clone containing this increment:

```powershell
& <source>\experiments\rust-process-owner\verification\sid-2026-11-14\run.ps1 `
  -TaskRoot '<owned-task-root>' -CargoHome '<existing-offline-cargo-cache>' `
  -Run '<fresh-native-run-name>'

& <source>\experiments\rust-process-owner\verification\identity-2026-11-14\consumer.ps1 `
  -TaskRoot '<owned-task-root>' -CargoHome '<existing-offline-cargo-cache>' `
  -CCompiler '<installed-gcc.exe>' -Run '<fresh-consumer-run-name>' `
  -FfiRef 'eb4ca1d40244ba1915638b76c3e30b2a684d9b07'
```

These runs place the clone at `<owned-task-root>\source`. The native script derives the crate location from its own script path, uses new named evidence/control/fixture directories under TaskRoot, and refuses pre-existing evidence names. Tests are serial to make process handle-count observations meaningful. Only finite owned children are used. The scripts record commands, exit codes and source hashes. The consumer script assembles an isolated combined source copy and uses separate targets/caches; it does not modify either shared branch.

The scripts preserve local fixture files for review. Actual SID bytes must not be uploaded with logs. After verifying outcomes and preserving required evidence, move confirmed disposable copies/builds/fixtures to an owned trash directory without deleting or emptying it.

## Evidence and disposition

- [verification.json](verification.json): plain-array source manifests, final step records, mutation definitions/manifests, environment and consumer inputs/counts.
- [native-final.log](native-final.log) and [consumer-final.log](consumer-final.log): complete final command output, including the six expected failures. The runners replace the owned absolute task root with `<task-root>` while recording output.
- [history.zip](history.zip): original evidence files from these two final runs plus the earlier focused log; no binaries, checkout copies, fixture trees or actual SID markers. The earlier focused log is historical and may contain absolute local tool paths.
- [run.ps1](run.ps1): the native run/negative-control script; the earlier consumer script is reused by relative link above.
- [disposition.json](disposition.json): copy-source hash checks and confirmed trash-first moves, recorded outside trash. Needed source, logs/manifests and current development target remain outside it.
- [SHA256SUMS](SHA256SUMS): SHA-256 integrity hashes for this published evidence package. Hashes identify bytes, not authority or mathematical correctness.

The 13-file native manifest and 36-file combined manifest identify the tested inputs; the six mutation manifests identify the deliberately changed copies. The archive is inspected after creation. No cleanup action touches other sessions' checkout, targets, branches or fixtures.

## Remaining gates

1. Define and qualify an additive, separately versioned foreign observation while preserving existing ABI1 exports/layouts. Specify lengths/version/knownness/error outcomes and real Go call behavior; missing identity facts must not become permissive defaults.
2. Preserve the actual Go capability, context/custody and SID formatting/resource receipt at its existing owner before claiming accepted product admission. Copied bytes and digests are insufficient.
3. Cover the remaining applicable acquisition/setup/allocation/attribute-update failure classes and broader I/O/crash/recovery schedules. This run does not prove exhaustive native failure handling or allocator exhaustion.
4. Native Linux/macOS, additional Windows versions/architectures, real production consumers, repository-wide suites, installation/release/recovery and required independent acceptance remain open. Experimental `go vet .` is not repository-wide vet.
5. No performance gain, full migration completion, merge to master, installation, vendor/model execution or explicitly dispatched bot/CI run is claimed.

The canonical [six-stage residual plan in #623](https://github.com/applicate2628/mcp-local-hub/blob/docs/rust-cross-platform-no-com-20261112/docs/modernization/plans/rust-independent-execution-plan-2026-11-12.md) remains the sole progress index. Completing this SID observation advances stage 1 without closing R01/R03 or the whole stage.

## Sources and terms

1. [Microsoft: OpenProcessToken, retained process and query rights](https://learn.microsoft.com/en-us/windows/win32/api/processthreadsapi/nf-processthreadsapi-openprocesstoken).
2. [Microsoft: GetTokenInformation](https://learn.microsoft.com/en-us/windows/win32/api/securitybaseapi/nf-securitybaseapi-gettokeninformation) and [TOKEN_USER](https://learn.microsoft.com/en-us/windows/win32/api/winnt/ns-winnt-token_user).
3. [Microsoft: SE_TOKEN_USER maximum storage and stack allocation](https://learn.microsoft.com/en-us/windows-hardware/drivers/ddi/ntifs/ns-ntifs-_se_token_user).
4. [Microsoft: GetSidLengthRequired](https://learn.microsoft.com/en-us/windows/win32/api/securitybaseapi/nf-securitybaseapi-getsidlengthrequired).
5. [Microsoft: IsValidSid, no extended error information](https://learn.microsoft.com/en-us/windows/win32/api/securitybaseapi/nf-securitybaseapi-isvalidsid).
6. [Microsoft: GetLengthSid requires a valid SID](https://learn.microsoft.com/en-us/windows/win32/api/securitybaseapi/nf-securitybaseapi-getlengthsid); used by the independent child fixture.
7. [Accepted Go token-user helper at master 17bc78fc](https://github.com/applicate2628/mcp-local-hub/blob/17bc78fc602efa204a74596409c939a3bf1ddc42/internal/process/process_owner_sid_windows.go).
8. [Earlier creation-time/Job/wait qualification](../identity-2026-11-14/RESULTS.md), unchanged historical evidence.

**Terms:** SID — Windows security identifier; TokenUser — primary-token user information; PID — process identifier; Job — Windows process-group resource; handle — operating-system resource descriptor; ABI — application binary interface; FFI — foreign-function interface; DLL — dynamic-link library; cgo — Go calls to C-compatible code; `cgocheck2` — Go's additional cgo pointer checking; UTC — Coordinated Universal Time; SHA-256 — 256-bit Secure Hash Algorithm. A Go capability is the existing restricted authority to act on the original process; a SID-formatting receipt records conversion and associated resource-cleanup outcomes, not merely formatted text.
