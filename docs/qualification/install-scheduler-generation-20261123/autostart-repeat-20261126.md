# Windows autostart repeat Enable — scoped evidence, 2026-11-26

> Historical `c0b0cc22` checkpoint. The later real-registration failure, minimal correction, and native PASS are recorded in [the native roundtrip follow-up](autostart-native-roundtrip-20261126.md). The results and limits below retain their original scope.

**Selected functional checks and final metadata: PASS / RECEIVED. Historical v1 and remainder wrappers remain FAIL.** The v1 timeout and the remainder profile-receiving failure are retained separately from passing test results.

Repository: `applicate2628/mcp-local-hub`; existing PR #660. Source base / code parent: `b14cdeba95b2a69815d1677ee3f0dc236e8f1c79`. Full derived terminal events and exact command filters are in [the JSON evidence](autostart-repeat-20261126.json).

Code commit: `c0b0cc224b12260aad1268c50305b7c8cc265b5e`; tree: `81cbaa83e1f25e95051506f942bdf684b5cc6b9b`. The nine files below are the exact tested bytes. The documentation commit uses this code commit as its parent. The current PR #660 description records the published documentation head and its completed readback.

Contents: [Contract](#implemented-contract) · [v1 results](#table-1-received-v1-results) · [Remainder](#table-2-received-remainder-results) · [Final receiving](#table-3-final-read-only-receiving) · [Source bytes](#table-4-reviewed-source-bytes-used-before-positive-execution) · [Integrity](#capture-integrity-and-receiving) · [API remainder](#additional-api-functional-remainder) · [Limits](#remaining-status-and-claim-limits)

## Implemented contract

- A matching existing definition plus typed `Ready`, `Running`, or `Queued` succeeds without `Create`, `Delete`, `ImportXML`, `Run`, or `Stop`.
- Different/disabled definitions and Disabled/Unknown/missing/error runtime observations retain the observed XML and refuse replacement.
- Creation still requires typed absence with no XML; the failed-create observation path and saved-OFF projection remain intact.
- Definition comparison reuses the existing Windows writer and current-user resolution. Equality grants no task-generation ownership.
- Caller correction: the backend returns the direct `ObservedTaskReplacementRefusal`; its generalized `Cause` preserves errors while the existing API direct type assertion retains XML.
- Unknown XML fields/attributes and action order remain significant. Idle Duration/WaitTimeout normalization is limited to the current `RunOnlyIfIdle=false` writer; `DeleteExpiredTaskAfter` is not normalized away.

## Table 1. Received v1 results

| Scope | Observed terminal results | Outcome |
| --- | --- | --- |
| Scheduler + autostart, default build | 20 root PASS, 57 subtest PASS, 2 package PASS; no FAIL/SKIP | PASS |
| Scheduler + autostart, `test_state_path_env` | 20 root PASS, 57 subtest PASS, 2 package PASS; no FAIL/SKIP | PASS |
| Previous-Enable negative control | 1 root FAIL, 3 subtest FAIL, 1 package FAIL; exit 1 expected | Regression detected |
| API refusal finalizer, default | `TestRetainAutostartTaskReplacementRefusal` PASS; API package PASS | PASS |
| CLI saved OFF, default | `TestStrictModeWindowsExplicitAutorunOffSurvivesMutationAndRecovery` PASS, 71.39 s; mutation/recovery subtests PASS | PASS |
| CLI explicit Enable, default | `projected` subtest PASS, 12.65 s; `projection-failed` and parent root have no terminal event | PARTIAL |
| Entire default boundary command | API package PASS; CLI package FAIL at its 90-second Go test alarm | FAIL |

The negative control changes only `internal/autostart/windows.go` relative to the positive input set. All three repeat-Enable cases fail under the old occupied-task refusal, then pass with the candidate restored.

The v1 CLI stack records `FlushFileBuffers` at `internal/api/secure_write_windows.go:330` when the scoped Go alarm fires. This observation does not establish a product defect or a successful remaining test. v1 is not relabelled PASS.

## Table 2. Received remainder results

| Scope | Observed terminal results | Outcome |
| --- | --- | --- |
| Previously unreceived default CLI tail, project `5m` deadline, traced | 2 root PASS, 4 subtest PASS, 1 package PASS; package 0.585 s | PASS |
| Full tagged API + CLI boundary scope, project `5m` deadline | 4 root PASS, 7 subtest PASS, 2 package PASS; API 3.555 s, CLI 48.098 s | PASS |
| Remainder wrapper after the tests | Profile-generation command exited 0, measured captures, and closed; binary stdout was incorrectly decoded as UTF-8 | FAIL |

The default boundary evidence combines received v1 successes with only its previously unreceived tail. The first failing CLI package/campaign is not relabelled. The traced tail did not reproduce the prior slow behavior; the latency cause remains unknown.

The remainder wrapper recorded `UnicodeDecodeError` while receiving binary output from `go tool trace -pprof=syscall`, after both test commands passed. The final read-only commands successfully read that existing profile and received the diff check; no tests were repeated and the remainder wrapper remains FAIL.

## Table 3. Final read-only receiving

| Scope | Observed result | Outcome |
| --- | --- | --- |
| Existing binary profile, `go tool pprof -top -unit=ms` | Exit 0; input is the retained remainder profile | PASS |
| `git diff --check` | Exit 0; empty stdout, exact SHA-256 verified | PASS |
| Source status and pre-command hashes | Exactly the nine reviewed paths; all nine hashes equal tested inputs | PASS |

The profile reports 217.55 ms total syscall delay; `golang.org/x/sys/windows.FlushFileBuffers` accounts for 154.48 ms **cumulative** (71.01%), with zero flat delay. This is not test wall-clock time. It covers the successful 0.585-second default tail, not the earlier 71.39-second saved-OFF root; the earlier latency cause remains **UNKNOWN**.

## Table 4. Reviewed source bytes used before positive execution

These hashes come from `receipt-v1.json` → `step_inputs[autostart-scheduler-default]`, after candidate restoration and native gofmt. The same nine hashes precede all positive test commands and the final read-only steps; the current checkout matches all nine.

| Repository path | SHA-256 |
| --- | --- |
| `internal/api/scheduler_replacement.go` | `257f6840bc7bb6279ea9753dfaae494bc486f55e142578820eb6e7d8497eb731` |
| `internal/autostart/autostart.go` | `4cbcda2e6fa67fa64e1a4f49f59706662b036c7607b8d9b6269d0480c8657649` |
| `internal/autostart/windows.go` | `1332a7bceee2259f0db8c501f854431f5d7f9d8874076d13e15b3e6405ed9e3e` |
| `internal/autostart/windows_test.go` | `ded4e1af9ac6b8fd11e36b0748b57e7048460144403422d8b1b5c2fd87093d18` |
| `internal/cli/autostart.go` | `0bfb9347472de56e671b69d9df5fe03aba7d49ce7beaae092326958450d30cf0` |
| `internal/scheduler/task_xml_match.go` | `1ae2bce75f354fb75c3c04021a31e8853da65507d83b6d42bb0496b4724a851d` |
| `internal/scheduler/task_xml_match_test.go` | `30464d1dc9d53792a8f166a1acd26750ef7eb55e1acd61bba4d6df31933ce12d` |
| `internal/scheduler/task_xml_match_windows.go` | `03e0a1329036795ee64d6bb179c85c28975d12e91ebdcbfb2b38ddddb01886b4` |
| `internal/scheduler/task_xml_match_windows_test.go` | `7be3d01fecef08c2e196256c21a1c42b000722974449c1e5efd5c71824a06fac` |

## Capture integrity and receiving

The eleven received stdout captures and boundary stderr were hashed as raw bytes **before** interpretation or event counting; all match their receipt SHA-256. Nonempty text captures retain LF endings. The empty diff capture was rematerialized as zero bytes after transport added one LF; native bytes were unchanged. Raw logs, host paths, process/user identifiers, and credentials are omitted.

The receipt itself crossed text transport as CRLF → LF. Its received-LF SHA-256 is `4620b6b796cef162aae48122538f15b7f8f0783164c4ad1eebb6b9ccbab7a700`. Converting LF → CRLF only in memory reproduces `2007a76947c261e02c299cd9fddc977988fa9a90b6bbb0903e98c9ecf43e346d`, exactly the original hash in `root-receiving.json`; the received file was not rewritten.

The remainder receipt has the same labelled CRLF → LF transport conversion: received-LF SHA-256 `6e3bbd8361cd319558f670bbba82436fde9e454ea15c83e30fc5cd2914692662` reconstructs original `268b31033a467fe8e2598eb28101e39b8992a6db8e577fc9a723355a6574ed2b`, matching `remainder-root-receiving.json`.

The final receipt also crossed CRLF → LF: received SHA-256 `6dd0c9efe8b74c8f7ea61b2677a1baa2fef3bc0ee594a053988a20f330370f79` reconstructs original `d7599a0151ae24533898080ebf418ededbead785ffdb5d23a9ca7b2adebbcdb4`, matching `finalize-root-receiving.json`.

All 19 native commands / 38 original readers across v1, remainder, finalization, and the API remainder completed receiving: commands `CLOSED`, direct processes reaped, zero active children, handles closed, readers joined. The four outer controllers exited 1, 1, 0, and 0; all were reaped and closed their capture handles. Finalization took 7.635616 s and ran zero tests. Cleanup completion does not change either historical FAIL.

## Additional API functional remainder

Default and `test_state_path_env` each received **12 root PASS, 8 subtest PASS, 1 package PASS, no FAIL/SKIP**. The campaign passed in 20.312742 s; 3 commands / 6 readers closed. Both raw stdout hashes and all 26 source pins recorded before each run match; exact filters, terminal events, hashes, and labelled receipt transport are in the JSON.

This covers the late `phase_f` assertions, parsed-manifest diagnostic, and previously unrun adjacent install/supervisor-stop tests. No code changed for this remainder. Historical `api-final-v2` stays **FAIL / UNKNOWN**; real system lifecycle is **NOT RUN**.

## Remaining status and claim limits

- Previously unreceived default tail and tagged boundary tests — **RECEIVED / PASS**.
- Read-only profile interpretation and final diff-check receiving — **RECEIVED / PASS**; the prior latency cause remains **UNKNOWN**.
- Real Task Scheduler Create/registration-export roundtrip and actual logon smoke — **NOT RUN**.
- Full repository suite — **NOT RUN**. External review — **PENDING**. Publication and exact-head readback are recorded in the current PR #660 description.

These received results cover native Windows execution of scoped tests with scheduler doubles. Live task creation and logon behavior remain unqualified.
