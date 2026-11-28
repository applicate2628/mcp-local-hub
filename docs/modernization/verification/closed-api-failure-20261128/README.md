# Source727 default API: closed failure evidence

This capsule preserves a **FAIL with physical closure** from the default-mode API package on exact Source `727d66753814dc218e26d699e57402db2d13190a`. It contains the complete original API receipt, namespace receipt, stdout and wrapper stderr as transparent templates. It does not claim a successful full run, state-mode run, migration qualification, or qualification of another Source.

## Contents

1. [Scope and exact evidence](#1-scope-and-exact-evidence)
2. [All test outcomes and closure](#2-all-test-outcomes-and-closure)
3. [Four failures and their causes](#3-four-failures-and-their-causes)
4. [Timing and preparation cost](#4-timing-and-preparation-cost)
5. [Reconstruction and independent DATA verification](#5-reconstruction-and-independent-data-verification)
6. [Disclosure and retained limitations](#6-disclosure-and-retained-limitations)
7. [Source references and proposed continuation](#7-source-references-and-proposed-continuation)

## 1. Scope and exact evidence

**Table 1. Scope.**

| Item | Value |
|---|---|
| Package and phase | `mcp-local-hub/internal/api`, default |
| Test Source | `727d66753814dc218e26d699e57402db2d13190a` |
| Test Source tree | `d959ed0a4f8ab02947aec7569f3c0535a3cff533` |
| Product Source | `3ebe335a2f666a5279d7fafc31a6f4dc3feedff4` |
| API role | `literal-original-runs-v6/xe288dd1b2ed2` |
| Namespace role | `literal-original-runs-v6/n14fe6816051b/n` |
| Original outcome | API exit 1, FAIL; native and I/O CLOSED |
| Namespace owner interval | 2026-11-28T11:50:19.1293538Z to 2026-11-28T11:55:18.8653317Z |

The API role is one closed child scope of a larger acceptance run. Its closure does not establish closure or qualification of that larger run. The author read only this closed API scope, the separately restored inactive Source3ebe, and already published immutable source deltas.

**Table 2. Exact original bytes.** These are the original hashes, not hashes of the public templates. Template identities and literal substitution counts are in [source-bindings.json](source-bindings.json).

| Original role | Bytes | Original SHA-256 |
|---|---:|---|
| API receipt | 2,338,457 | `2282b4b297e5d78f90da965383880674dd3f5529cfcfd6b9243cb56145927de8` |
| Namespace receipt | 5,732 | `2fe7149ad8fb358eba3aabae1d7d0d4e11a535b19622b0057f52ff7e034dd98b` |
| Original stdout | 1,207,113 | `664ff94ef54d97d0088b74b0b669edc292589f1911b49798c71192aa79b17541` |
| Wrapper stderr | 341 | `0c954a4a1713930d9b849eaa415d7f195a799745a14df141f7cf71b81d614107` |

The complete receipts retain their original numeric process/file identities and 64-bit native birth values. No JSON reserialization was used to produce their templates. Stdout preserves its original Go framing bytes and all test output. The wrapper replayed stdout byte-for-byte. The native API child stderr was empty; the wrapper's separate 341-byte stderr adds its diagnostic, so its stderr-identity flag is false. [api-failure-evidence.json](api-failure-evidence.json) records that distinction.

The 4,685,811-byte derived test2json output was read after closure and matched SHA-256 `146486690e5b25bd45e34db6f27f6c0df62f4b906636a5027a976f2aa41dfe48`. It is not duplicated here: original stdout plus the primary receipt preserve its useful information, and the included verifier reconstructs every terminal outcome.

## 2. All test outcomes and closure

**Table 3. Complete inventory results.**

| Scope | Total | PASS | SKIP | FAIL |
|---|---:|---:|---:|---:|
| Top-level tests | 3,523 | 3,455 | 64 | 4 |
| Top-level tests and subtests | 6,275 | 6,189 | 82 | 4 |

All 3,523 expected top-level tests have one start and terminal outcome. No expected root is missing, no started test lacks a terminal outcome, and the package terminal is FAIL. The complete 6,275-row outcome array is recoverable from the preserved stdout and is also recorded in the primary receipt. Its canonical compact UTF-8 JSON form is 810,017 bytes, SHA-256 `9c5e0262be78f623ec0f744c721b0b856592796c00e93208526f793513c40001`. It is generated on demand instead of storing another duplicate ledger.

All seven mandatory API checks passed:

- `TestSecretsInit_RecoveryNamespaceBoundary`
- `TestSecretsInit_RecoveryNamespaceRetainedDirectory`
- `TestSecretsInit_RecoveryNamespaceFailurePaths`
- `TestSupervisorPeerStrictControlConnections`
- `TestRotateSupervisorStderrSinkIfOversize_AtBoundary`
- `TestRotateSupervisorStderrSinkIfOversize_ReplacesExistingBackup`
- `TestSupervisorStderrSinkOversize_MatchesRotationThreshold`

The original eight command records are quiescent. The namespace receipt records known root exit 1, all members exited, final active count zero, handles closed and no requested termination. Both original stream workers completed at EOF without limit/error; all five recorded stream handles closed. The binary, bundle and pinned inputs were unchanged. [api-failure-evidence.json](api-failure-evidence.json) links each original command to its stdout/stderr hashes.

There is no panic, timeout stack or goroutine dump in the output. A generic wrapper failure message mentions an incomplete receiver predicate, but the individual original native and I/O fields establish physical closure. A nonzero test result remains FAIL.

## 3. Four failures and their causes

**Table 4. Exact failure sites.** The complete excerpts, stdout line references and original rounded durations are in [api-failure-evidence.json](api-failure-evidence.json).

| Test | Failure site and output |
|---|---|
| `TestIsDeadGitWorktreePath_SubmoduleInsideWorktree_RealGit` | `workspace_prune_test.go:1266`: local `git submodule add` exits 128, `fatal: transport 'file' not allowed` |
| `TestIsDeadGitWorktreePath_SubmoduleInsideRemovedWorktreeNotPruned_RealGit` | `workspace_prune_test.go:1667`: the same refusal |
| `TestPreparedSerenaBuildConstraintCompactAttemptLifecycle` | `serena_prepared_build_constraint_integration_windows_test.go:68`: `run=contained process start failed finish=<nil>` |
| `TestPreparedSerenaFailureDiagnosticsOriginalCommand` | `serena_prepared_diagnostics_windows_test.go:27`: `no remaining original command work interval` |

### Git fixture environment

Both source fixtures explicitly set `protocol.file.allow=always` through `GIT_CONFIG_COUNT/KEY_0/VALUE_0` and inherit the surrounding environment. The preparation owner independently reviewed the frozen full configuration and found `GIT_ALLOW_PROTOCOL=""`, an empty protocol allowlist carried from preparation. This explains the observed file-transport refusals. That configuration attribution is identified separately from the directly observed stderr; its frozen binding hash is recorded in the timing review. No active full configuration was read for this capsule.

### Diagnostics budget

The test computes:

```text
cleanupEnd = min(now + 60 s, packageDeadline - 1 s)
workEnd = cleanupEnd - 45 s
```

Here `now`, `cleanupEnd`, `workEnd` and `packageDeadline` are clock instants; 60, 1 and 45 are durations in seconds. The 45-second cleanup interval comes from the unchanged default preparation policy. The failure checks `now < workEnd` before qualifying or launching the inert command. Therefore this observed refusal proves that at most 46 seconds remained until the package deadline. It is not a child-process failure.

The CompactAttempt fixture instead receives fresh 60-second work and 60-second cleanup intervals; its start failure is a separate issue. Both fixtures select an absolute qualified `COMSPEC` executable. Neither discovers PowerShell, UV or Python through PATH.

### CompactAttempt path risk

The API adapter created a 31-character short-temp path. The namespace owner then overrode its temp environment with a 97-character namespace temp path. The fixture's generation working directory has this shape:

```text
temp / test-name-and-random-suffix / 001 / hardened-parent / 64-hex-identity / generations / new
```

For this 56-character test name and a ten-digit suffix, the Windows path length is:

```text
97 + 1 + 56 + 10 + 4 + 16 + 65 + 12 + 4 = 265 characters
```

The terms count the temp prefix, separators, test name, suffix and remaining path components. Substituting the adapter's 31-character prefix gives 199 characters. For a one-to-ten-digit suffix the long path spans 256–265 characters; the actual suffix was not logged.

This is a concrete mismatch between the short-temp preparation and downstream namespace path. It is a strong explanation to check for the start failure. The native errno was not preserved by the test's fixed-text error formatting, so this capsule does not label any specific Win32 cause as proven. The exact source references are in [timing-cost-review.json](timing-cost-review.json).

## 4. Timing and preparation cost

The converter was invoked without `-t`. Its JSON has **no Time or Elapsed fields**. The following durations come from original Go terminal text, rounded to 0.01 seconds. They do not provide exact per-test start timestamps.

**Table 5. Fifteen largest top-level durations.**

| Test | Seconds | Before Diagnostics |
|---|---:|:---:|
| `TestProbeProviderProcessWorkingDirectoryReadsOwnedWOW64Helper` | 26.39 | Yes |
| `TestSecretsInit_RecoveryNamespaceBoundary` | 19.07 | Yes |
| `TestProviderWorkingDirectoryIdentityOwnsOnlySelectedLiveInstance` | 12.89 | Yes |
| `TestSupervisorIntentLockOwnershipProductionInventory` | 11.71 | No |
| `TestAttachExisting_ConfigLockWaitExpiresAtTenSeconds` | 10.25 | Yes |
| `TestCanonicalManifestDirIgnoresExecutableContextAndExistence` | 9.03 | Yes |
| `TestAttachExisting_ForegroundAdmissionExpiresAtFiveSeconds` | 6.28 | Yes |
| `TestRunDaemonIntentCollapse_LargeLegacyStopsStayUnderIntentCapWithAbsentOnlyWatermarks` | 5.72 | Yes |
| `TestAttachExisting_CommittedRepeatRefusesUnownedRecoveryAndDrift` | 3.71 | Yes |
| `TestAttachExisting_CancelDuringSlowSessionCleanupVetoesWrite` | 3.32 | Yes |
| `TestIntentAudit_CallerStartTimeAgainstIndependentOracle` | 3.26 | Yes |
| `TestWeeklyRefreshWritersSerializeAtTransactionOwner` | 3.01 | No |
| `TestAttachExisting_FreshCommittedPlanApplyRefusesDrift` | 2.86 | Yes |
| `TestProviderAdmissionUnknownMarkerBlocksProbe` | 2.76 | Yes |
| `TestAdoptRepeatRejectsMalformedProvenance` | 2.65 | Yes |

These fifteen roots total 122.91 rounded seconds. Before CompactAttempt, 2,823 sequential roots total 260.95 rounded seconds; CompactAttempt itself takes 0.01 seconds. Before Diagnostics, 2,832 roots total 260.96 rounded seconds; its refusal takes 0.00 seconds. No top-level pause precedes either point. Another 37.24 rounded seconds follow Diagnostics.

The estimated remaining interval is about 39 seconds. It is an estimate from summed rounded test durations, not a recovered timestamp; the source predicate independently proves the upper bound of 46 seconds. All roots total 298.20 rounded seconds, versus approximately 299.736 seconds for the namespace owner interval.

The short CompactAttempt failure does not account for the late Diagnostics execution. Fixing the path and Git environment is necessary for their concrete failures but does not by itself prove sufficient timing margin.

### What can ordinary cache preparation address?

The exact Source727 provider fixture differs from Source3ebe: its already published two-hunk change memoizes helper images by architecture in the TestMain-owned directory. This postimage was independently reconstructed as DATA and matched all 44,798 bytes and SHA-256 `1427099b98f5d851cb8bb2462bfa166ea7687d8ab34cc14142e9344fb39bb4f9`.

- The 26.39-second WOW64 test builds the first 386 helper.
- The following 2.35-second PEB-path test builds the first amd64 helper.
- The 12.89-second identity test then receives the already built amd64 image. Its duration cannot be attributed to a new helper build.
- The 19.07-second namespace boundary test invokes its original PowerShell fixture sixteen times across six subtests. It does not build a Go helper.

The helper build is `go build -ldflags=-H=windowsgui -o <fresh image> ./testdata/provider-cwd-helper`, with `GOOS=windows`, architecture 386 or amd64 and `CGO_ENABLED=0`. It inherits the recorded Go cache and module-cache paths. Successful build output and cache-hit/miss telemetry were not logged, and no separate build phase duration was measured. A cache miss and a promised number of saved seconds are therefore **not established**.

## 5. Reconstruction and independent DATA verification

[source-bindings.json](source-bindings.json) identifies every template and original hash. The renderer is reused from [the source-preservation capsule](../original-v6-sources-20261128/reconstruct_sources.py); its only source change is the pinned bindings hash.

Supply the ten explicit role values in an operator-owned JSON outside the public capsule. There are no original/default host values or encoded substitutes here. Use a new, absent reconstruction directory:

```text
python -I -S -B reconstruct_sources.py --roles-json <operator-roles.json> --output <new-reconstruction-directory>
python -I -S -B verify_closed_api.py --root <new-reconstruction-directory>/closed-api --output <new-derived-data-directory>
```

The renderer validates every reconstructed byte before creating its output. The verifier reads only those four original files. It checks original hashes, standalone/embedded receipt agreement, physical closure, all 3,523 starts and expected roots, every one of 6,275 terminal outcomes, the four FAIL names, mandatory PASS checks and the timing sum. Its optional output contains the canonical complete outcome ledger.

A DATA-verification PASS means that the preserved evidence is internally consistent. The program explicitly retains `historical_test_status=FAIL` and issues no cache, process or full-run qualification. Neither program imports or executes the preserved data or Source.

The author performed in-memory literal reconstruction and ledger comparison. These new Python verifier/renderer files were prepared and reviewed as code; they were not launched by the capsule author. [data-reconstruction-proof.json](data-reconstruction-proof.json) records that distinction.

## 6. Disclosure and retained limitations

The templates replace seven actual path-prefix/form roles, two source-defined synthetic launcher prefixes, and one original account SID. The synthetic prefixes are part of test names, not observed operator installations; that distinction is explicit in the bindings. Original sandbox PIDs, native file IDs and native birth integers remain unchanged for record matching.

The original stdout contains synthetic loopback/private-network URLs in marketplace-rejection subtest names. They are test fixtures, not observed private hosts. Literal control bytes used by Go output framing are preserved. No original host path, actual account SID, credential or encoded host value is embedded in the public package. [disclosure-review.json](disclosure-review.json) describes the bounded scan and its limits.

The namespace test's actual wrong-owner branch remains **UNVERIFIED** because ordinary-token owner assignment was refused. Its aggregate top-level PASS does not erase that explicit limitation. Other historical FAIL/UNKNOWN scopes, the remaining unidentified expected-refusal control, later state-mode results and the parent full-run result are outside this capsule and unchanged.

## 7. Source references and proposed continuation

Primary references:

1. The exact four original receipts/captures in [Table 2](#1-scope-and-exact-evidence), preserved by the bindings.
2. [Original Source727 reconstruction capsule](../original-source727d-recovery-20261128/README.md), including the exact provider-fixture delta.
3. [Public Source3ebe capsule](https://github.com/applicate2628/mcp-local-hub/tree/c80fd6b4e6f9806eb7d94726a979e9e89d46ec75/experiments/rust-process-owner/ffi/verification/stdio-consumer-2026-11-26/source-capsule-3ebe).
4. [Detailed timing, source references and unexecuted preparation proposal](timing-cost-review.json).

A possible continuation is a new owned cache producer after the whole original full run has physically closed. It would copy closed cache data, use the exact Source727 working directory and pinned toolchain/environment, and perform the two ordinary helper builds. Only after both commands and their readers close could its cache data be handed to another run. The tests would still build their own fresh images and launch fresh children.

This proposal has not been executed or qualified here. It does not change the five-minute timeout, test inventory, production code, native owner route or expected failure classification. Preserved cache data never substitutes for original live process or handle custody.
