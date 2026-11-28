# Original full-v6: closed failing acceptance, 2026-11-28

**Classification: FAIL/CLOSED.** Both requested literal acceptance commands were attempted on exact Test Source `727d66753814dc218e26d699e57402db2d13190a`, with the preserved native Product Source `3ebe335a2f666a5279d7fafc31a6f4dc3feedff4`. The build and vet commands exited zero. Both test commands failed. Every original invocation was physically received as closed; that observation does not change the failing qualification.

This is an evidence package for the existing recovery PR. It does not modify production code, tests, a hook, policy, approval state or the canonical report. The full original receipt retains every collected top-level and subtest outcome, the Source/input hashes, the command records, and the 65 package receiving references. Twenty original evidence files can be reconstructed exactly. The five newly included failing package receipts and both of their raw streams preserve the diagnostics below. The separately published [default API package](../closed-api-failure-20261128/README.md) supplies the sixth failing package's full raw evidence.

## Contents

1. [Scope and primary evidence](#1-scope-and-primary-evidence)
2. [Literal command and package results](#2-literal-command-and-package-results)
3. [Acceptance roots and missing results](#3-acceptance-roots-and-missing-results)
4. [Observed failures](#4-observed-failures)
5. [CLI counting discrepancy](#5-cli-counting-discrepancy)
6. [Original native closure and input observations](#6-original-native-closure-and-input-observations)
7. [Exact reconstruction and DATA verification](#7-exact-reconstruction-and-data-verification)
8. [Disclosure, preservation and limits](#8-disclosure-preservation-and-limits)
9. [Sources](#9-sources)

## 1. Scope and primary evidence

Table 1 identifies the observed source and original closed receipts. A SHA-256 digest identifies file bytes. A Git commit identifies the retained source snapshot; it is not a test result.

| Table 1 — item | Identity |
|---|---|
| Test Source | `727d66753814dc218e26d699e57402db2d13190a` |
| Test Source tree | `d959ed0a4f8ab02947aec7569f3c0535a3cff533`, 3,509 tracked files |
| Native Product Source | `3ebe335a2f666a5279d7fafc31a6f4dc3feedff4` |
| Full receipt, 8,488,794 bytes | `6cd606a5ec4e6e1a3e74d4b8324786cc98dffe8cd587f4a41c89a751ea7f0c4a` |
| Original parent, 2,035 bytes | `062ab6f5a124d5e3b2338355e53c8bb6e7a9f15e26c99e3d5765cac7044fc794` |
| Original outer process receipt, 546 bytes | `53bf598d463b4ab04ffed5f91c7e3d9fabe03e43448346ef35edefcd8ccea006` |
| Root's independent receiving audit, 4,560 bytes | `f02bf04c80d0e84a5989297f742662ea874bbf7743c78ab3da947305004849d5` |

The complete original receipt is preserved in [its transparent template](templates/full/receipt.json.template), with byte identities in [source-bindings.json](source-bindings.json). The immutable Source reconstruction is described in [the original Source727 capsule](../original-source727d-recovery-20261128/README.md). This failure does not qualify another Source, including the recovered CLI commits, their successor, or newer A–D work in the existing PRs. [S1, S2]

## 2. Literal command and package results

The two requested command strings remain:

```text
go build ./... && go vet ./... && go test -count=1 -timeout 5m ./...
go test -tags=test_state_path_env -count=1 -timeout 5m ./internal/api/ ./internal/cli/
```

Table 2 reports the original command records, without changing the five-minute package timeout. Seconds come from the original command receipts; they are not new measurements.

| Table 2 — original command | Exit | Recorded seconds | Result |
|---|---:|---:|---|
| `go build ./...` | 0 | 116.7113142 | PASS |
| `go vet ./...` | 0 | 123.3890215 | PASS |
| Default `go test -count=1 -timeout 5m ./...` | 1 | 1224.826 | FAIL |
| Tagged API/CLI command | 1 | 313.641 | FAIL |

The default phase received all 64 expected package test invocations. Ten additional packages had no test files. The tagged phase expected two test invocations and received only API: CLI failed to compile before its test invocation could begin. The tagged CLI package failure is present in the Go command stream even though there is no CLI test inventory or package-test receipt for that mode. [S1, S2, S3]

| Table 3 — outcome ledger | PASS | SKIP | FAIL | No terminal result |
|---|---:|---:|---:|---:|
| Default, top-level tests across all packages | 9,746 | 194 | 7 | 389 |
| Default, all observed terminal records including subtests | 17,286 | 265 | 8 | — |
| Tagged API, top-level tests | 3,460 | 65 | 4 | 0 |
| Tagged API, all terminal records including subtests | 6,194 | 83 | 4 | — |
| Default CLI, top-level inventory of 1,988 tests | 1,585 | 14 | 0 | 389 |

The top-level default inventory contains 10,336 tests, of which 9,947 have a terminal result. The 389 missing terminal results are all in default CLI. Fifteen of those tests had a start event: the fourteen migration roots and `TestRunSupervisorHeartbeat_StopsOnDone`. “NOT_RUN” is the historical ledger label for absence of a terminal result; it does **not** mean every such test lacked a start event. The other 374 missing tests had no start event. The CLI package exited 2 on its original five-minute test alarm. [S1, S4]

The default full-terminal FAIL total is eight because one process test has both a failing subtest and a failing top-level parent. Table 3 counts those separately only in the all-terminal row. The full per-package mappings are in [full-failure-evidence.json](full-failure-evidence.json); the original complete names and outcomes remain in S1.

## 3. Acceptance roots and missing results

Table 4 keeps the original requested categories separate. The exact names are copied from S1 into `required_acceptance_roots` in [the structured projection](full-failure-evidence.json).

| Table 4 — acceptance category | Default mode | Tagged mode |
|---|---|---|
| Fourteen original Serena migration roots | Started/paused; no terminal result | No CLI test invocation: build failure |
| Two canonical native CLI roots | No terminal result; no canonical requests or attempted cases | No CLI test invocation: build failure |
| Three original API stderr rotation/threshold roots | All three PASS | All three PASS |
| Three additional CLI stderr panic/error-exit roots | All three PASS | No CLI test invocation: build failure |
| Previously remaining original expected-refusal control | Still NOT_RUN; original exact test name/argv unresolved | Not satisfied by this run |

The two canonical CLI roots are `TestCanonicalGUIRuntimeRecoveryNativeIndependentReplay` and `TestCanonicalSuccessorNativeControlRetainsOriginalFiles`. The closed canonical controller reports zero requests and both cases `attempted=false`. Its complete physical closure does not imply canonical qualification. The default CLI stdout ends after the start of `TestRunSupervisorHeartbeat_StopsOnDone`; its stderr records the original timeout and stack. [S4]

The API stderr roots are `TestRotateSupervisorStderrSinkIfOversize_AtBoundary`, `TestRotateSupervisorStderrSinkIfOversize_ReplacesExistingBackup` and `TestSupervisorStderrSinkOversize_MatchesRotationThreshold`. The additional CLI roots are `TestSupervisorStderrSink_CapturesRuntimePanic`, `TestSupervisorStderrSink_CapturesMainGoroutinePanic` and `TestSupervisorStderrSink_RecordsErrorExit`. Passing these individual roots does not turn either full command into PASS. [S1, S4, S8]

The newly fixed DWORD prestart refusal and namespace controls are separate evidence. They are not substituted for the unresolved originally requested remaining control. Historical full-v8 UNKNOWN, full-v9 FAIL/UNKNOWN, and the previously published historical 717 aggregation retain their prior classifications. [S9]

## 4. Observed failures

Table 5 records direct observations. A diagnostic establishes the observed failure; it does not by itself establish every underlying cause.

| Table 5 — package/mode | Direct observation | Primary evidence |
|---|---|---|
| API/default | Two local Git submodule fixtures refused the file transport; compact Serena attempt reported contained process start failure; diagnostics refused a missing remaining original work interval | Separate default API capsule, S8 |
| CLI/default | Original `panic: test timed out after 5m0s`; `TestRunSupervisorHeartbeat_StopsOnDone` was running at the alarm; root exit 2 | S4 |
| oneapi/default | `TestDetectRootFound`: `POSIX DetectRoot returned ok=true with no probe candidates`, location `oneapi_test.go:46` | S5 |
| daemon/default | `TestCstDirectFrontendCrossesGoCapabilityAndFixedLocalPipe`: test frontend builder exited 1 because its Windows `ProgramFiles(x86)` toolchain location was unavailable, `cst_direct_frontend_e2e_windows_test.go:53`; builder line 7 | S6 |
| process/default | `TestRunStrictlyContained_WindowsTimeoutProofWindowFastAndSlow/fast_original_handle_signal`: `positive fact=<nil> want=true`; timeout-proof member wait exceeded 50 ms with zero processes still in the job; `TerminateProcess: Access is denied.`, location `run_strictly_contained_windows_test.go:127` | S7 |
| API/tagged | Same two local Git failures and compact Serena attempt failure; additionally `TestCanonicalManifestDirIgnoresExecutableContextAndExistence` failed after 18.22 s with child exit 1 and no diagnostic output, location `manifest_canonical_root_test.go:56` | S10 |
| CLI/tagged | `internal/cli/attach_client_real_supervisor_windows_test.go:109:24: assignment mismatch: 3 variables but startSuperviseForIPCTest returns 4 values` | S3 |

The process test's `expired_before_observation` sibling passed in 0.19 s. The failed fast sibling took 0.18 s, and its top-level parent failed in 0.37 s. This evidence does not prove that changing an arbitrary timeout would be a valid repair. The original assertions and limits are retained. [S7]

The tagged API diagnostics test passed after the earlier default run. This observation is distinct from its default-mode failure and does not erase it. The tagged canonical-manifest failure contains no nested native error or useful child output; a specific path or budget cause cannot be asserted from that excerpt alone. The previous default API capsule records its own timing limits and distinguishes the long-path hypothesis from the observed native start error. [S8, S10]

Original raw stderr contains the CLI timeout stack and retains native numeric identifiers. The final line-anchored test name is an observation at the alarm, not proof that this test consumed all five minutes.

## 5. CLI counting discrepancy

The original CLI stdout at physical line 5,587 contains this sequence, with the Go test frame byte shown explicitly:

```text
Branch [A/B]: <0x16>--- PASS: TestStrictModeRecover_OrdinaryIntentFailureStopsBeforeShim (0.02s)
```

The prompt did not end with a newline. A regular expression anchored only to the physical beginning of a line therefore counts 1,584 top-level PASS records after removing the frame byte. A parser that recognizes the `0x16` test-frame boundary counts the additional PASS and matches the original coverage ledger: **1,585 PASS**. No test was rerun and no terminal record was invented. The exact raw stdout is 516,257 bytes, SHA-256 `d680bab54bba9cd6043a1f46359076d650a08d9061d550ceee54bff8019f787f`. [S4]

The DATA parser independently matched all 10,461 terminal records in the five newly preserved raw stdout files to S1: CLI 2,533; oneapi 19; daemon 423; process 1,205; tagged API 6,281. There were no extra, missing or duplicate terminal records in these comparisons. The already published default API parser separately matched its 6,275 records. These checks verify recorded data; they do not execute tests or grant acceptance. [S8, S11]

## 6. Original native closure and input observations

Root's independent receiving audit checked actual original files after the full parent closed. Table 6 reports the audit's observations, not a fresh execution of that audit by this capsule's author. Its exact source script and result are preserved. [S2]

| Table 6 — root's observed check | Count/result |
|---|---:|
| Original commands physically received | 283 |
| Commands inside package adapters | 272 |
| Actual stdout/stderr capture hashes checked | 566 |
| Package receipts checked | 65 |
| Original owner receipts checked | 130 |
| Actual retained Source files hashed | 3,509 |
| Actual input files hashed | 115 |
| Temporary API resource returned to its prior absent state | 1 |
| Original source, inputs, Product and private migration exports unchanged | true |
| Original invocations closed / commands quiescent | true |

The outer receipt records the same original process object, successful wait and disposal, exit 1, no new job/tree wait and no wait errors. The original parent records received terminal exit, closed captures, physical completion, exit 1 and no parent deadline expiry. The main receipt retains the original invocation issuer's joined worker and no publication error. The CLI controller retained the original child/readers until closure; its root exit is 2 and termination was not requested. [S1, S2, S4]

These closure observations make the failed run inspectable. They do not confer a PASS cache lineage or authorize deleting its primary evidence. This capsule issues no cache reuse grant. The exact source/resource restoration observations remain distinct from the old full-v9 source/cache post-closure NOT_RUN history. [S2, S9]

## 7. Exact reconstruction and DATA verification

[source-bindings.json](source-bindings.json) declares every original role path, byte count, original SHA-256, public template SHA-256 and counted substitution. Its SHA-256 is `4cdec1bc4436a72f7e75df04ee78c78082c3dc9f07fc24d6911267bc83cb6af9`.

The twenty originals total 14,916,637 bytes. The public templates replace host directory prefixes and the actual owner SID with explicit role markers. There is no encoded path, embedded role value, fallback, operator name, default host path or network retrieval. The operator must supply the original approved role values in a private JSON file. Different values fail the exact original byte hash.

The reused [reconstructor](reconstruct_sources.py) differs from the previously published API reconstructor only in its binding hash. It validates every reconstructed file before creating a new output directory. It refuses existing output paths and never imports or executes an artifact. For an operator-supplied approved directory and role file:

```text
python -I -S -B reconstruct_sources.py --roles-json <private-role-file.json> --output <new-restored-directory>
python -I -S -B verify_closed_full.py --restored <new-restored-directory> --summary <new-summary.json>
```

These are reproduction instructions, not commands run during this preservation work. The [new DATA verifier](verify_closed_full.py) checks all twenty original hashes, the frozen FAIL/CLOSED classification, the command/package counts, the recorded root-audit cardinalities and the five raw outcome ledgers. It checks the CLI framing discrepancy and the original timeout diagnostic. It does not rerun the original 566-capture audit, source, a test, a build or a native owner. It writes `EXACT_CLOSED_FAILURE_DATA_VERIFIED`, never product PASS.

The daemon stdout contained seven CRLF sequences and one lone CR. The text read API normalized them. Root supplied the exact original eight CR byte offsets by a separate read-only byte observation. Restoring those positions, including the lone CR at offset 6,121, reproduced the complete 72,767-byte stream and its original hash `90aa7ecb897894d931c33aeeefca70c992f8e51539d7bd00a3334ad8dd822515`. Those original bytes are retained, including control framing and line endings. The evidence therefore does not rely on silently normalized text. [S6, S11]

`.gitattributes` uses `* -text` so Git does not transform these bytes. The excerpt of tagged CLI build failure consists of the last six original JSONL records; its own digest, source-stream digest, source size and line interval are in [excerpt provenance](excerpts/provenance.json). It is explicitly an excerpt, not the entire 5,866,860-byte tagged command stream.

## 8. Disclosure, preservation and limits

Real operator paths and the actual owner SID are absent from the public package. Original native PIDs, native birth values, file IDs, closed-run nonces and handle observations remain exact. These closed sandbox identifiers are useful for matching the original evidence and are not replaced with hundreds of private roles.

Six remaining Windows-style literal operands are source-defined test fixtures: one language-server binary subtest path, three cross-platform application glob subtests, one marker-path subtest, and one synthetic CLI shim command. They are not observations of those installed products or actual work directories. Synthetic localhost/private-address rejection URLs and local test ports also remain as test data. [S1, S4, S12]

The original full receipt preserves all collected outcome ledgers. This package is not an archive of every one of the 566 raw captures or all 130 separate owner files. It preserves their frozen references and root's actual all-file receiving audit, plus the full raw failure streams and applicable original package proofs. The default API originals are a pinned public dependency. Do not discard the complete closed run solely because this capsule was published; any later retention decision must check remaining unique primary evidence and current dependencies.

No source, native command, build, test, cache copy, Git command, index edit, commit, push or cleanup was executed by the capsule author while preparing this package. Read-only byte/JSON comparisons and candidate-file writes were the only new work. Root performed the separately recorded original run, independent receiving and byte observation. Historical failures and unknown scopes remain immutable; acceptance of the successor Source requires its own two complete literal commands and remaining requested checks.

## 9. Sources

- **S1.** [Full original receipt template](templates/full/receipt.json.template), [original parent](templates/full/parent.json.template), [outer receipt](exact/full/outer-original.json), and [source bindings](source-bindings.json).
- **S2.** [Root's independent audit result](templates/root-receiving/result.json.template) and [exact audit source template](templates/root-receiving/full-v6-root-audit-v1.ps1.template).
- **S3.** [Tagged CLI compiler failure](excerpts/tagged-cli-build-failure.jsonl), [excerpt provenance](excerpts/provenance.json).
- **S4.** [Default CLI receipt](templates/default-cli/receipt.json.template), [raw stdout](templates/default-cli/suite-owner.stdout.txt.template), [raw stderr](templates/default-cli/suite-owner.stderr.txt.template).
- **S5.** [Default oneapi receipt](templates/default-oneapi/receipt.json.template), [raw stdout](templates/default-oneapi/suite-owner.stdout.txt.template), [raw stderr](exact/default-oneapi/suite-owner.stderr.txt).
- **S6.** [Default daemon receipt](templates/default-daemon/receipt.json.template), [raw stdout](templates/default-daemon/suite-owner.stdout.txt.template), [raw stderr](exact/default-daemon/suite-owner.stderr.txt).
- **S7.** [Default process receipt](templates/default-process/receipt.json.template), [raw stdout](exact/default-process/suite-owner.stdout.txt), [raw stderr](exact/default-process/suite-owner.stderr.txt).
- **S8.** [Published default API failure capsule](../closed-api-failure-20261128/README.md), artifact manifest SHA-256 `1f1e4cf51a4a4548c492c2737028b81b38233c047d8c89790d9fc7f917155117`.
- **S9.** [Historical source recovery](../original-source727d-recovery-20261128/README.md), [historical CLI 717 recovery](../historical-cli717-recovery-20261128/README.md), and unchanged historical sections of the canonical [integration report](../../../../.plans/2026-11/open-pr-integration-20261117.md).
- **S10.** [Tagged API receipt](templates/state-api/receipt.json.template), [raw stdout](templates/state-api/suite-owner.stdout.txt.template), [raw stderr](templates/state-api/suite-owner.stderr.txt.template).
- **S11.** [DATA preparation proof](data-reconstruction-proof.json) and [DATA verifier](verify_closed_full.py).
- **S12.** [Disclosure review](disclosure-review.json) and complete [artifact manifest](artifact-manifest.json).
