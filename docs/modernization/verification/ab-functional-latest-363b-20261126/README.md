# A/B functional progress: completed evidence on 26 November 2026

**The standalone #660 liveness/setup selection passed in both modes. Composed Source52 build and vet passed, while its focused controller command and the later native HAPPY v3 case remain FAIL/CLOSED. The native case stopped before the third Product launch. Full A/B acceptance is not established.**

This additive report consolidates completed evidence. Earlier documents remain immutable snapshots: their `NOT_RUN` entries describe their original evidence boundary. The later results below update the status view without rewriting those records. Incomplete later activity is outside this report.

## Contents

1. [Exact revisions and current scope](#1-exact-revisions-and-current-scope)
2. [A: standalone liveness and setup](#2-a-standalone-liveness-and-setup)
3. [Source52 build, vet and controller selection](#3-source52-build-vet-and-controller-selection)
4. [Separate Parole diagnostic](#4-separate-parole-diagnostic)
5. [B: actual controller and P1/P2 builds](#5-b-actual-controller-and-p1p2-builds)
6. [B: native HAPPY v3 failure and closure](#6-b-native-happy-v3-failure-and-closure)
7. [Preserved history and remaining acceptance](#7-preserved-history-and-remaining-acceptance)
8. [Sources and terms](#8-sources-and-terms)
9. [Publication correction and additive controls](#9-publication-correction-and-additive-controls)

## 1. Exact revisions and current scope

**Table 1. Revisions actually exercised.**

| Role | Commit | Tree |
|---|---|---|
| Composed Source52 | `52a2d9ef57b7c372b9fa149adf062ad7e503b22d` | `51258ebd9ad3a654418b6ecb9163947734ef0314` |
| Its Source6d parent | `6d4fa7b8f9b86079020af125b030a26350cb5bb0` | `277dad42f9e9080d66955fa7469fbc68bc8618e4` |
| Standalone #660 liveness code | `99c89e9fd12e7bf50a703940c099f568a5c99628` | `f74954deb26871bf2e292b5f2dd8e3bb6162cc90` |

The [Source52 capsule][1] depends on the [Source6d capsule][2]. These pin the composed source separately from the public PR branches and from each execution. Source export, bundle verification and hash agreement do not themselves establish runtime acceptance. The standalone #660 revision has a different parent/tree from the composition, even though its three changed liveness/setup files match Source52 byte for byte. [S1–S3]

**Table 2. Completed result ledger.**

| Scope | Result | Original physical completion |
|---|---|---|
| #660 selected liveness/setup, default | PASS: 29 root tests + 32 subtests; 1 expected platform skip | CLOSED |
| Same #660 selection, `test_state_path_env` | PASS: 29 root tests + 32 subtests; 1 expected platform skip | CLOSED |
| Source52 `go build ./...` | PASS | CLOSED |
| Source52 `go vet ./...` | PASS | CLOSED |
| Source52 default controller selection | 47 root PASS / 49 PASS including subtests; 1 FAIL | CLOSED |
| Source52 plus failure-only Parole diagnostic overlay | FAIL; original loop observed in `FlushFileBuffers` | CLOSED |
| Source52 controller and P1/P2 build preparation | BUILD_ONLY PASS | CLOSED |
| Source52 native HAPPY v3, merged guard named branch | FAIL; qualification false | CLOSED |
| Dependent v3 `run-return` / `stdout-reader` probes | NOT_RUN | No invocation claimed |
| Full default / full tagged API and CLI suites | NOT_RUN | No invocation claimed |

`CLOSED` means the stated original process/reader scope was received complete. It does not turn a failed assertion or incomplete native scenario into PASS. Details and exact receipt pins are retained in [evidence.json](evidence.json). [S1–S5]

## 2. A: standalone liveness and setup

The two standalone Windows/amd64 commands on `99c89e9…` used the same selected API and CLI tests with a substituted Scheduler. Both packages passed in each mode. Each mode produced 29 root test PASS records and 32 subtest PASS records, plus one expected skip of `TestSetup_NonWindowsLivenessNotImplementedContinues`. That skip does not execute the non-Windows branch. The default command took 161.165 seconds; the tagged command took 127.938 seconds. Five original commands and ten original readers closed, including the final clean-source checks. The original receipt is `87fc35a3455c8ce5cae2a4ae134466d3c5b8fee9361e3bc81406065bb83bbe13`. [S3]

The checked behavior is scoped: creation requires typed task absence with zero exported bytes and the existing create-if-absent capability; matching definitions remain unchanged; calendar `StartBoundary` and `Enabled` differences are refused; create/readback/restore failures preserve their errors and existing occupants. A creation receipt or XML equality grants no authority to remove the current task generation. Explicit uninstall remains a separate operation.

These tests did not register actual Windows Scheduler tasks or exercise the fixed production liveness task name. Native calendar normalization, installed lifecycle and the broader legacy cleanup, port-stop, dry-run, strict/recovery autostart, refresh and autorun-OFF scope still require their own acceptance. The earlier Source6d selected API PASS and CLI fixture-cleanup FAIL remain bound to that older composed run. [S2–S3]

The companion #660 evidence is now published in draft [PR #660][6] at `a706b5b2a71ca850136398f7d4a45e624d107151`. All seven scoped code/evidence files were read back byte for byte with their Git blob identifiers, and the PR body readback matched. The tested code revision remains `99c89e9…`; this later publication does not create a new test result. The initial report's prepared-only publication status is preserved by the frozen v2 packet. See section 9.

## 3. Source52 build, vet and controller selection

The [initial Source52 campaign][3] records `go build ./...` PASS in 25.691 seconds and `go vet ./...` PASS in 36.904 seconds. The focused default controller command passed 47 root tests, or 49 PASS records including subtests, and failed `TestSupervisorController_ParoleQueuedStop_PostSelfHonorsStop`. All four original commands and eight original readers closed. Receipt: `5ce22474571013d116634bf2b08043372487b594e577122e341f08eb8ca7b430`. [S1]

The failed assertion observed `spawning` where `exiting` was expected. The test took 14.05 seconds; its existing observation helper had a two-second window. Those durations have different boundaries and do not measure a particular handler or cleanup operation. The selected orphan-reap cases and `TestSupervisorController_StaleLivenessDisownDroppedByGeneration` passed. The Source52 tagged controller selection was not run in this campaign.

The original 14.05-second failure has no stack proving its exact blocking instruction. Its cause remains **UNDETERMINED**. The later diagnostic in section 4 is a separate observation, not a retrospective attribution.

## 4. Separate Parole diagnostic

The [additive Parole diagnostic][4] used immutable Source52 production with a three-line failure-only Go overlay. The test postimage was `3ac226efd68d34c6109aa79b2e36b91d9f4cb16add8bff57416fe026748b3d66`; its original file was `8bd40a8b1d41becc6d21192044883f994516e666951d781a61cc1eda71e65be4`. The existing observation window, expected state and original-loop cleanup remained unchanged. The exact test again failed, in 3.16 seconds; two commands and four readers closed. Receipt: `99124603fad77b49a254559dda80025e1ef7ba5b74f91a40b5990caec92d888f`. [S4]

The bounded, untruncated stack located the original event loop at `windows.FlushFileBuffers`, through `secure_write_windows.go:330`, `state_file_helper.go:115`, `supervisor_state.go:455`, tracker persistence and `supervisor_controller.go:3427`. At that snapshot, fake spawn and the later HealthOK self-event had not been reached. Publishing the `spawning` state therefore did not acknowledge completion of its original handler.

This establishes the instruction at that diagnostic instant. It does not measure the syscall's duration, determine the external cause of its latency, prove a `PostSelf` ordering defect, or identify the blocking instruction in the earlier controller run or the B native case below. At the initial report cutoff, the dispatch-barrier follow-up had no included successful execution result. The later [separate positive/negative controls report][7] preserves both earlier failures and records its own selected default-overlay results; see section 9.

## 5. B: actual controller and P1/P2 builds

The Source52 build preparation completed through its original owner: four commands and eight readers closed, all commands exited 0, and no work/cleanup deadline was exceeded. The parent interval was 242.446293 seconds. Controller compilation took 20.324182 seconds, P1 creation 146.863884 seconds, and P2 creation 71.707767 seconds. Original receipt: `6c030d82340d562d1ab438be3b83d4973a519996f783734069e04c42768c0e04`. [S5]

**Table 3. Exact built execution roles.**

| Role | Actual input | Bytes | SHA-256 |
|---|---|---:|---|
| Original controller | Existing canonical native owner/controller | 6,620,672 | `6204e702dfb1be238793aea9b5c5f2b2f67eb5dea40a2ec0e4d672cd62e6fd01` |
| P1 native Product | `./cmd/mcphub`, `test_state_path_env` | 49,445,888 | `e8dfe4bbcec53fc8db2540706dda88d222263ddcda10dc695b4a986fc8671bd8` |
| P2 command/test Product | Actual `go test -c ./internal/cli`, `test_state_path_env` | 65,721,344 | `93e56542d6c2987a255d8ff85727354fdee8316f3f7518f00aeb706b87c4e546` |

P1 and P2 have separate original build identities, launchers, identity files and fixtures. The actual same-build observations and file pins are recorded in the JSON. P1 is the native/attach test Product; P2 is the command-test image. Neither is an installed/release Product acceptance result or a substitute for the future full-suite default/tagged incoming images. Their build success is not native HAPPY success.

The frozen build-only packet said `source52_happy: NOT_RUN` when it was received. That was correct for its cutoff. The later v3 result below is the current completed native attempt on these exact build outputs.

## 6. B: native HAPPY v3 failure and closure

The selected native case was `TestCanonicalSuccessorNativeControlRetainsOriginalFiles`. The measured route used merged guard `52d0b4bb…`, focused runner `835cc21b…`, its prebound callback and the existing canonical receiver. It exercised the merged guard's named-controller branch; it did not qualify the full adapter's new role wiring or its separate ordinary Windows Job owner. The full exact pins are in the JSON. [S5]

The invocation returned **FAIL/CLOSED**, with qualification false. The original parent returned exit 1 after 128.172464 seconds; the guarded command returned controller exit 2 after 127.262 seconds. The original native case and both stream workers completed under the normal receiver. Capture bytes were admitted through that original normal completion, not through a late physical-access exception. No historical held capture was used. The outer test's timeout/retained-custody diagnostic remains part of its original failed output; subsequent original controller settlement does not convert the case into PASS.

The original observed debug chain contained 43 process objects. Exactly two were Product processes, and both exited 0 without termination. The actor entered the three sequential `absent`, `identical` and `different-identity` subcases, but no third Product creation occurred. There was no root terminal PASS. The two clean Product exits are not standalone subtest acceptance.

**Table 4. Bounded localization of the third scenario.**

| Observation | What it establishes |
|---|---|
| Second Product exited at 16:34:13.2530015 UTC | The second original Product process completed cleanly. |
| Third fixture retained a 10,361,344-byte launcher staging file and a 49,445,888-byte Product staging file | Both files reached their expected extents; this alone is not a hash, Sync, close or seal completion proof. |
| Product staging-file last-write timestamp was 16:34:46.634 UTC; no identity staging file was observed | A post-closure filesystem observation, not an instruction-completion timestamp. |
| Actor work deadline was 16:35:02.3564002 UTC; actor exited 87; original case completion was received by 16:35:09.5648 UTC | The owner received the failed actor and original physical resources within its separate cleanup endpoint. |
| Original ledger has two Product creations and no third | The failure interval precedes the third successor launch. |

The test calls `built.Original.Stage` before transaction preparation, settings/intent publication and successor launch. Stage processes launcher, Product and identity in order. Per member, it copies, calls `Sync`, hashes for cleanup, closes the writer, seals the original guard, and rechecks identity/extent/content before proceeding. The evidence locates the unfinished scenario in that staging/error-cleanup interval before the third launch. **Its exact blocking instruction remains UNDETERMINED.** Sync/`FlushFileBuffers` is one possible operation; hashing, closing, sealing and cleanup have not been excluded. No original actor stack was captured in v3. [S5]

The original closure model remains the fixed-source observed debug chain, not universal process-tree containment. All dependent v3 `run-return` and `stdout-reader` probes remain **NOT_RUN** because HAPPY did not pass. No larger timeout, production durability change, retry, or qualification transfer is claimed here.

## 7. Preserved history and remaining acceptance

**Table 5. Preserved and outstanding scopes.**

| Scope | Status at this completed-evidence boundary |
|---|---|
| Source6d focused campaign | Original FAIL/CLOSED preserved; later Source52 evidence is separate. |
| Initial Source52 controller campaign | Original FAIL/CLOSED preserved; the later Parole overlay does not reclassify its cause. |
| Earlier e66 HAPPY and negative probes | Original failed/UNKNOWN qualifications preserved; their separate physical receipts retain only their stated scope. |
| Older held negative-check batch | Original FAIL/HOLD and separate physical UNKNOWN preserved. No release, capture access or reclassification follows from newer runs. |
| Current Source52 native HAPPY v3 | FAIL/CLOSED; no native functional qualification. |
| Current dependent native negative probes | NOT_RUN. |
| Full standard suite | NOT_RUN. |
| Full tagged API/CLI suite | NOT_RUN. |
| Actual Scheduler calendar normalization and installed lifecycle | Not established by the liveness/setup selection. |
| Full A/B scope, private runtime and installed/release acceptance | Not established by these results. |

Any later corrective test source, diagnostic actor or Product/Test-source split must identify its actual revision and admitted image bytes. Existing Product52 markers, fixtures and failed histories remain bound to their originals. Static review and a prepared diagnostic patch do not supply an execution result. The next evidence must retain its own original owner/reader closure and exact acceptance scope.

## 8. Sources and terms

**Table 6. Numbered evidence sources.**

| ID | Evidence |
|---|---|
| S1 | [Initial Source52 campaign][3] and its [machine-readable evidence](../ab-functional-source52-20261126/evidence.json); original receipt `5ce22474…`. |
| S2 | [Source6d campaign][5], [Source6d capsule][2], [Source52 capsule][1] and preserved history. |
| S3 | Standalone #660 code `99c89e9…`; original receipt `87fc35a3…`; companion evidence publication at `a706b5b…` stated in section 2. This report's JSON includes the sanitized exact source pins, selection, counts and closure predicates. |
| S4 | [Separate Parole diagnostic][4] and its [JSON](../ab-functional-source52-20261126/parole-diagnostic-20261126.json); original receipt `99124603…`, with derivative provenance kept distinct. |
| S5 | This report's [sanitized evidence](evidence.json): exact original build receipt `6c030d82…`, HAPPY receipt `bdc93eb1…`, command receipt `f578f307…`, original controller-result digest `f231c3df…`, and retained input-report hashes. Original machine paths, process identifiers and raw captures are omitted. |

**Source** means an exact committed source revision. **P1/P2** are the distinct native Product and command-test Product roles in section 5. **API** means application programming interface; **CLI** means command-line interface. **EOF** is end of file observed by an original reader. **HOLD** denotes retained original custody whose required receiving facts were not accepted. A test verdict and physical resource completion are separate observations.

## 9. Publication correction and additive controls

The original v2 publication staging step passed: all 42 prepared files were written and read back with their exact byte counts, SHA-256 hashes and Git blob identifiers. Its receipt is `19fa8d5536d166a8a03228beca6d7641a77726d7111524fa55b91e0e3e570b69`. The following standard `git diff --check` returned exit 2 because the publication copy of `internal/cli/supervise_status_snapshot_test.go` contained CRLF line endings. The original batch remains **FAIL/CLOSED**, receipt `0e5c35dfef1314f59c6e5fa72242190b217979b91ec2cd740f5ebd4acbe2ad89`: three original commands and six original readers closed; no HOLD occurred. The planned diff-stat step was not reached. Successful staging did not override the failed formatting gate.

**Table 7. Publication-only correction at this report's cutoff.**

| Item | Exact scope and status |
|---|---|
| Go publication copy | 28,454 bytes, SHA-256 `ab357bcba5ccd283c439a2b496c5bbff2c6d606faa8ca86f2d2668c63dfb0c5e`; 836 CRLF pairs and no lone CR. |
| Prepared normalized copy | 27,618 bytes, SHA-256 `a95b26f2eb3564f3bcefb247064189f9533bb8bfa31e10738f310ef7d5f86298`; replacing only CRLF with LF gives the exact accepted Source52 file hash. |
| Native correction | PREPARED, not executed at this evidence boundary. It must precheck all 42 v2 staged files, modify only this Go copy and the two latest-report files, and read back all 42 resulting files. |
| Formatting and publication | Read-only `gofmt -l`, standard `git diff --check` and diff-stat receiving remain pending for the correction. No index, commit or push result is claimed by this preparation. |

This normalization changes publication line endings only. It does not modify Source52, add a semantic code change, alter a timeout, or relax a test assertion. The frozen v2 package, older reports and source capsules remain unchanged. Literal `.patch` evidence retains its exact bytes; it is data, and any later staged formatting check must name those data exclusions explicitly while still verifying their pinned bytes.

The [separate Parole controls report][7], already included as a sibling document in the reviewed 42-file packet, records a positive default overlay PASS (test 0.27 seconds; package 0.799 seconds) and the intended negative control FAIL after the single `PostSelf` to `TryPost` change (test 0.06 seconds; JSON package 0.584 seconds). Its original five commands and ten readers closed under receipt `f558bde54cb3f313859d856ddf11913fae93f4244a617164577769193aec47f5`. Immutable Source52 remains unchanged. These selected controls preserve the initial Source52 failure, separate diagnostic failure and native HAPPY v3 failure; they do not establish full A/B acceptance.

The publication and formatting facts above are summarized from their original admitted receiving metadata in this report's [JSON](evidence.json). Original machine paths, process identifiers and raw captures are omitted. The correction's future outcome must be received separately rather than inferred from these prepared bytes.

[1]: ../composed-functional-source-20261126/source52-363b/README.md
[2]: ../composed-functional-source-20261126/source6d-363b/README.md
[3]: ../ab-functional-source52-20261126/README.md
[4]: ../ab-functional-source52-20261126/parole-diagnostic-20261126.md
[5]: ../ab-functional-source6d-20261126/README.md
[6]: https://github.com/applicate2628/mcp-local-hub/pull/660
[7]: ../ab-functional-source52-20261126/parole-barrier-controls-20261126.md
