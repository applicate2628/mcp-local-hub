# GUI, OneAPI runner, and process gate triage

**Историческое независимое чтение без запуска кода.** Ведущий исполнитель отдельно выполнил проверки, сверил прямые квитанции и провёл собственное ревью. Текущие результаты — [RESULTS.md](RESULTS.md), итоговая область — [AUDIT.md](AUDIT.md). Итоговые ссылки закреплены опубликованным коммитом `2cb467aac31259ed4fb665ed0a4f277a8f9c31a2`.

## Contents

- [1. Scope and evidence](#1-scope-and-evidence)
- [2. GUI timeout](#2-gui-timeout)
- [3. OneAPI runner failures](#3-oneapi-runner-failures)
- [4. Process qualification prerequisite](#4-process-qualification-prerequisite)
- [5. Candidate correction](#5-candidate-correction)
- [6. Discriminating verification](#6-discriminating-verification)
- [7. Review and limitations](#7-review-and-limitations)

## 1. Scope and evidence

Repository: `applicate2628/mcp-local-hub`. All GitHub source reads were pinned to `92f2d4350bfc22a5fa8099fd1b4ec199f9a19c22`. The task identifies `95f2c6df8d8883f009025d2196480cc54c8101af` as its code-identical parent, with only the plan changed at the current head.

The historical evidence is `r17/entry-env-20261120/report/evidence-redacted.zip`, read without extraction. Relevant member: `evidence/gates/full-test.stdout.log`. Line references below use UTF-8 decoding with replacement for invalid bytes, preserving line order. The archive intentionally retains invalid original byte sequences; their origin is not established.

- Archive SHA-256 (Secure Hash Algorithm, 256 bits): `68b805cd2fd39c7f5883c4ea2637788464d9b3c17f70d629fe2be2c23c779ef0`.
- Redacted stdout member: 203,591 bytes; SHA-256 `5da446c0223d8bcf66fb7d2ce48a5260ea3514059cd11753ebff9a3f94b2d0be`.
- The receipt identifies the original, unredacted stdout as 215,536 bytes and SHA-256 `8dbcfb0d1e23e01e5db481ed9dcc4938f7825f9895580dc274dc9f996bf505dd`. This is a different object, not a mismatch.
- Recorded toolchain: `go1.27.4 windows/amd64`.
- Recorded full gate: `go test -count=1 -timeout 5m ./...`, exit 1, 422.804 seconds.
- Gate source binding: staged candidate tree `8329a7ee95419c8290c13640b3acb50886fdc41c` over base `32b3c4679a6f323c82529a07df9a3a87fc17d6f7`; the published receipt binds that tree to the identified code parent.

This historical independent source review was performed without code execution. The lead executor separately performed runs, checked their direct receipts, and conducted its own review; those outcomes are recorded in [RESULTS.md](RESULTS.md).

## 2. GUI timeout

### Observed outcome

The graphical user interface (GUI) package has **no printed assertion failure** in its output segment (lines 1164–1329). Instead, it reaches the package-wide five-minute timeout:

- Lines 1232–1234: `panic: test timed out after 5m0s`; active test `TestGroupBinding_GlobalScanUnchanged (0s)`.
- Line 1329: package failure after 300.210 seconds.
- Lines 1290–1324: the active test is synchronously flushing an audit log through Windows `FlushFileBuffers`.

**Table 1. Confirmed active source path at the timeout**

| Step | Exact source location | Meaning |
|---|---|---|
| Fixture root | [projects_group_binding_test.go:359–364](https://github.com/applicate2628/mcp-local-hub/blob/92f2d4350bfc22a5fa8099fd1b4ec199f9a19c22/internal/gui/projects_group_binding_test.go#L359-L364) | Replaces the package's hardened root with a plain `t.TempDir()`; calls `seedOneGroup`. |
| Seed write | [projects_group_binding_test.go:48–57](https://github.com/applicate2628/mcp-local-hub/blob/92f2d4350bfc22a5fa8099fd1b4ec199f9a19c22/internal/gui/projects_group_binding_test.go#L48-L57) | Calls `api.WriteGroups` for the fixture. |
| Parent rejection and fallback | [state_file_helper.go:145–164](https://github.com/applicate2628/mcp-local-hub/blob/92f2d4350bfc22a5fa8099fd1b4ec199f9a19c22/internal/api/state_file_helper.go#L145-L164) | In non-strict, eligible-parent mode, invokes the supplied audit callback before the fallback write. |
| Audit event | [hub_mcp_state.go:134–145](https://github.com/applicate2628/mcp-local-hub/blob/92f2d4350bfc22a5fa8099fd1b4ec199f9a19c22/internal/api/hub_mcp_state.go#L134-L145) | Calls `LogHubMcpEvent` for `hub-mcp-state-write-unhardened-parent-fallback`. |
| Synchronous durability | [hub_mcp_log_observation.go:441–463](https://github.com/applicate2628/mcp-local-hub/blob/92f2d4350bfc22a5fa8099fd1b4ec199f9a19c22/internal/api/hub_mcp_log_observation.go#L441-L463) | Writes the event then calls `f.Sync()` at line 458. |

The package-level [GUI TestMain](https://github.com/applicate2628/mcp-local-hub/blob/92f2d4350bfc22a5fa8099fd1b4ec199f9a19c22/internal/gui/main_test.go#L202-L230) already creates and hardens its default API state root. The failing active fixture bypasses that default with its explicit plain temporary root. The observed audit stack proves that a parent gate rejected and the eligible fallback branch was entered at this point.

### What the evidence does not prove

The active test's displayed duration is **0 seconds**, so this stack is not evidence that this particular test deadlocked or occupied the entire five-minute budget. It is a snapshot at the package deadline. Nor does a stack in `FlushFileBuffers` identify the physical storage, endpoint protection, or scheduling cause of elapsed time.

The archive contains neither `hub-mcp.log` nor `supervisor-events.log`. There are zero occurrences of the fallback event name in the GUI stdout segment, because those events are written to an audit file. The gate did not use `-json` or verbose per-test timings. Therefore the number of relaxed-parent warnings and per-test elapsed durations cannot be reconstructed.

The 61 timestamped GUI stdout messages provide only coarse intervals: lines 1181→1182 have a 62-second gap, and 1182→1183 a 192-second gap. These intervals may contain many tests and cannot be assigned to the adjacent log-producing test.

### Bounded next step

Use a fresh task-owned temporary root with a deliberately verified private inherited discretionary access-control list (DACL), or harden only the exact positive fixture parent before replay. Capture per-test JSON events and audit counts. Keep the five-minute original gate result separate from a focused discriminating run. Do not remove log synchronization or weaken parent verification based on this timeout snapshot.

No GUI source candidate is included; the total-runtime cause remains unresolved.

## 3. OneAPI runner failures

**Table 2. All five observed OneAPI failures and exact fixture sites**

| Test | Original fixture location | Failure site / log lines | Confirmed cause before its intended assertion |
|---|---|---|---|
| `TestAsyncRun_PersistsAndReloadsCappedNonUTF8Output` | `async_owner_test.go:535` | :543 / 1337–1338 | Constructor's real state reader rejects ordinary temporary parent. |
| `TestAsyncRun_LoadsLegacyExpandedTextIntoSamePathBase64` | `async_owner_test.go:588` | :604 / 1339–1340 | Initial real secure write rejects ordinary temporary parent. |
| `TestAsyncRun_CloseConcurrentCallersWaitForDeferredWorkerAndRetryLeaseUnlock` | `async_owner_test.go:728` | :739 / 1341–1343 | Constructor's real state reader rejects nested `oneapi-run` parent. |
| `TestAsyncRun_CloseWaitsForEvictedTerminalWorker` | `async_owner_test.go:784` | :797 / 1344–1345 | Same constructor precondition failure. |
| `TestRunInOneAPIEnvTool_StartReturnsAcceptedReceipt` | `async_test.go:19` | :25 / 1346–1347 | Same constructor precondition failure. |

The observed DACL grants access to security identifier (SID) `S-1-5-11`, mask `0x0001013f`, outside the permitted current-user, LocalSystem, and BuiltinAdministrators set. The tests fail before exercising the intended persistence, concurrent close, eviction, or accepted-receipt assertions.

The extra `first unlock fails` on line 1343 is the deliberately injected `asyncTestLease` failure. The constructor joins it to the state-read error during cleanup ([async.go:270–272](https://github.com/applicate2628/mcp-local-hub/blob/92f2d4350bfc22a5fa8099fd1b4ec199f9a19c22/internal/oneapirun/async.go#L270-L272)); it does not demonstrate the intended concurrent-close scenario ran.

### Causal path and missing isolation

1. The failing fixtures omit `Read` and `Write` overrides. [newAsyncRunOwner:235–247](https://github.com/applicate2628/mcp-local-hub/blob/92f2d4350bfc22a5fa8099fd1b4ec199f9a19c22/internal/oneapirun/async.go#L235-L247) therefore selects the real anchored state reader and secure state writer.
2. [ReadStateFileInodeAnchoredWithMaxBytes:35–39](https://github.com/applicate2628/mcp-local-hub/blob/92f2d4350bfc22a5fa8099fd1b4ec199f9a19c22/internal/api/state_read_inode_anchor.go#L35-L39) passes the canonical effective strict-policy predicate.
3. The Windows reader verifies the immediate parent before opening or treating the state leaf as absent ([hub_mcp_state_read_inode_windows.go:110–162](https://github.com/applicate2628/mcp-local-hub/blob/92f2d4350bfc22a5fa8099fd1b4ec199f9a19c22/internal/api/hub_mcp_state_read_inode_windows.go#L110-L162)).
4. Effective strict policy can come from the environment, persisted intent, or an anomalous policy read. [OperatorRequiresSingleUserHome](https://github.com/applicate2628/mcp-local-hub/blob/92f2d4350bfc22a5fa8099fd1b4ec199f9a19c22/internal/api/client_write_init.go#L1031-L1048) consults a process-global cached intent; its [read path](https://github.com/applicate2628/mcp-local-hub/blob/92f2d4350bfc22a5fa8099fd1b4ec199f9a19c22/internal/api/client_write_init.go#L1227-L1290) fails closed for unresolved state paths and anomalous reads.
5. The exact-head `internal/oneapirun` directory contains no `TestMain`. The untagged [production resolver](https://github.com/applicate2628/mcp-local-hub/blob/92f2d4350bfc22a5fa8099fd1b4ec199f9a19c22/internal/api/state_paths_prod.go#L54-L69) uses [Windows KnownFolderPath](https://github.com/applicate2628/mcp-local-hub/blob/92f2d4350bfc22a5fa8099fd1b4ec199f9a19c22/internal/api/state_paths_windows.go#L39-L45), so the harness's `LOCALAPPDATA` and `USERPROFILE` environment redirection does not replace its API policy-state root.

Thus there are two source-confirmed fixture defects: the immediate state-file parents are not hardened, and implicit API policy reads are not isolated from the operator's canonical state directory.

The historical origin of effective `strict=true` is **unknown**. It must not be reported as a leaked environment setting: the harness removes inherited `MCPHUB_*` variables and does not set `MCPHUB_REQUIRE_SINGLE_USER_HOME`. The reader nevertheless prints `MCPHUB_REQUIRE_SINGLE_USER_HOME=1 is set` for any true effective predicate. This diagnostic wording at [lines 159–162](https://github.com/applicate2628/mcp-local-hub/blob/92f2d4350bfc22a5fa8099fd1b4ec199f9a19c22/internal/api/hub_mcp_state_read_inode_windows.go#L159-L162) (also file-DACL lines 262–264) is separately misleading for persisted/anomalous strict policy. Correcting that wording is optional separate work, not a reason to change enforcement.

## 4. Process qualification prerequisite

The two failures at log lines 1352–1356 are:

- `TestJob_BreakawayEnabledDescendantEscapesClose`.
- `TestJobInformationTransportPreparationBoundary/real-native-typed-gc-and-creator`.

Both fail in `requireJobFixtureEntry` with `entry/config: qualified entry configuration is required`.

[loadJobFixtureEntryConfig:735–780](https://github.com/applicate2628/mcp-local-hub/blob/92f2d4350bfc22a5fa8099fd1b4ec199f9a19c22/internal/process/jobobject_windows_test.go#L735-L780) requires the absolute `MCPHUB_JOB_FIXTURE_ENTRY_CONFIG` path, exact executed-binary hash, source companion hashes, and outer Job limits. [receiveJobFixtureEntry](https://github.com/applicate2628/mcp-local-hub/blob/92f2d4350bfc22a5fa8099fd1b4ec199f9a19c22/internal/process/jobobject_windows_test.go#L809-L870) then exchanges correlated READY/GRANT/ACK frames. [requireJobFixtureEntry:911–925](https://github.com/applicate2628/mcp-local-hub/blob/92f2d4350bfc22a5fa8099fd1b4ec199f9a19c22/internal/process/jobobject_windows_test.go#L911-L925) caches the original admission result, so both failures share one missing prerequisite.

The historical runner removes all inherited `MCPHUB_*` variables, supplies standard input from DEVNULL, and launches ordinary `go test`. It does not establish this qualification protocol. The first test refuses at its first source action; the second reaches its native subtest's admission guard. These observations provide no failed native Job-lifetime assertion.

The repository already documents the owning controller in [tools/test-job-entry/README.md](https://github.com/applicate2628/mcp-local-hub/blob/92f2d4350bfc22a5fa8099fd1b4ec199f9a19c22/tools/test-job-entry/README.md). Its exact fresh binary/source binding, GUI-subsystem test executable, private output parent, original process/Job ownership, bounded deadlines, and settlement receipts are prerequisites for a separately authorized native run. A fake configuration file, test skip, generic outer wrapper, or cached old receipt is not an equivalent correction.

No changes are proposed to `internal/process`, the qualification controller, Rust `mh_wait1` / `mh_close1`, or Go `WaitForExit` / `CloseExited`.

## 5. Candidate correction

The three OneAPI test files below are part of the final [candidate-manifest-v2.json](candidate-manifest-v2.json). Original production and baseline references remain pinned to the base commit; candidate links are pinned to the published code commit `2cb467aac31259ed4fb665ed0a4f277a8f9c31a2`.

- **New [main_test.go](https://github.com/applicate2628/mcp-local-hub/blob/2cb467aac31259ed4fb665ed0a4f277a8f9c31a2/internal/oneapirun/main_test.go):** create one owned temporary state root, harden it, install `api.SetDaemonStateRootForTest`, explicitly reset the strict-intent cache before tests, run `m.Run`, restore/reset, and report exact-root cleanup failures.
- **[async_owner_test.go](https://github.com/applicate2628/mcp-local-hub/blob/2cb467aac31259ed4fb665ed0a4f277a8f9c31a2/internal/oneapirun/async_owner_test.go):** replace only the four positive real-I/O state parents identified in Table 2 with existing `apitest.HardenedTempDir` or `apitest.HardenedDir`.
- **[async_test.go](https://github.com/applicate2628/mcp-local-hub/blob/2cb467aac31259ed4fb665ed0a4f277a8f9c31a2/internal/oneapirun/async_test.go):** harden only the accepted-receipt fixture's state parent.

The [existing helper](https://github.com/applicate2628/mcp-local-hub/blob/92f2d4350bfc22a5fa8099fd1b4ec199f9a19c22/internal/api/apitest/hardened_tempdir_windows.go#L25-L110) applies a protected DACL to the exact supplied directory. Its entries use `NO_INHERITANCE`; hardening only an ancestor does not establish the checked child's permissions. The nested `oneapi-run` fixtures therefore call `HardenedDir` on that child itself.

[SetDaemonStateRootForTest:227–242](https://github.com/applicate2628/mcp-local-hub/blob/92f2d4350bfc22a5fa8099fd1b4ec199f9a19c22/internal/api/testhooks.go#L227-L242) resets the Serena stop-read cache only. The separate [ResetStrictModeIntentCacheForTest:245–255](https://github.com/applicate2628/mcp-local-hub/blob/92f2d4350bfc22a5fa8099fd1b4ec199f9a19c22/internal/api/testhooks.go#L245-L255) is necessary for explicit strict-policy cache isolation. No production environment or guard is relaxed.

All five exact-head package test files were inspected for `TestMain`, `os.Exit`, `os.Executable`, self-spawned `exec.Command`, helper selectors, and `t.Parallel`; none require special subprocess dispatch before `m.Run`. Existing tests execute ordinary external commands through their production handler where relevant; the new TestMain does not change those commands' profile, temporary-directory, or output environment variables.

## 6. Discriminating verification

The following records the verification proposal at the source-review stage. Subsequent actual outcomes and direct-receipt bindings are tracked separately in [RESULTS.md](RESULTS.md):

1. **Source and format check:** confirm the baseline blob hashes in the manifest against the pinned head, then run the existing authorized toolchain's formatter on the three candidate files. No local `gofmt` executable was available during this triage.
2. **OneAPI fixture reachability:** run the five tests in Table 2 with JSON events, a fresh task-owned temporary tree, and effective strict mode enabled. Each must reach its original behavioral assertions and pass; no skip or mocked secure read/write substitutes for those five tests.
3. **Policy isolation:** run the package under an isolated test state root before any operator-policy access. Verify the resolved root equals the sandbox and distinguish absent isolated intent from the operator's real intent. An environment-only profile change is not sufficient.
4. **GUI discrimination:** first measure the exact group-binding test with an actual hardened immediate parent and per-test timing. Then measure the GUI package with recorded private-parent controls and preserved audit logging. Only a new complete gate can establish its total runtime meets the existing budget.
5. **Negative controls:** retain tests that intentionally construct an untrusted parent, wrong owner, or reparse point. They must continue to refuse under strict mode. Hardening every hostile fixture would erase the security oracle.
6. **Native process prerequisite:** qualify the documented controller/test-binary pair separately with exact hashes and original ownership receipts. Do not turn the ordinary historical full gate into PASS by excluding or skipping these tests.
7. After focused results settle, run the repository's mandatory build, vet, full, and tagged API/CLI gates using the approved execution boundary. Report each result and all resource-settlement limitations.

## 7. Review and limitations

Own review checked the candidate against the observed failures, compatibility, directory scope, cleanup failure reporting, import availability, package-global lifetime, subprocess roles, and absence of production changes. Both baseline Git blob hashes match the exact-head connector results. The candidate changes only three test files, five real-I/O fixture parent sites, and test-process setup; it leaves all original behavioral assertions intact.

This independent source review is not an execution receipt. Subsequent compilation and test results, verified separately by the lead executor, are recorded in [RESULTS.md](RESULTS.md); manual formatting and source inspection alone do not establish them. The historical GUI timeout's cause remains unresolved: the later passing full-suite series establishes non-reproduction on that series, not a root-cause diagnosis. The source of historical OneAPI effective strict mode remains unknown. Missing original native entry configuration remains a distinct process gate prerequisite.

The proposed test-only patch does not overlap the parallel Rust/Go waiting and handle-close work.
