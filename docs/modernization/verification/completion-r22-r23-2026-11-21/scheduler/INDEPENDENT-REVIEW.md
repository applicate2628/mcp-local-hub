# Independent static review: current Scheduler mutation failures

**Result:** no confirmed blocking defect found in the four-file draft examined. This is a separate assistant's source review, not a GitHub `APPROVED` submission or evidence that the tests executed. No source changes, tests, builds, native scheduler operations or publication were performed by this reviewer.

Base: `a63199628c1689b4b77a50abfc7432396a1893f4` (#660).
Reviewed draft patch SHA-256: `eba30c8480099d57d071644afbd0a455136463684aa5ab141fbd78237ab883a8`.

SHA-256 means Secure Hash Algorithm with a 256-bit digest.

## Contents

- [Findings by contract](#findings-by-contract)
- [Boundaries](#boundaries)
- [Reviewed candidate identities](#reviewed-candidate-identities)
- [Retained evidence review](#retained-evidence-review)

## Findings by contract

Table 1. Static contract review.

| Area | Source reviewed | Conclusion |
|---|---|---|
| Initial Delete failure | `install.go:3869–3870`, `install_task_rollback.go:60–74` | Exactly one `ExportXML` observation follows the failed mutation. Only an error-free byte-identical prior definition keeps the ordinary original failure. Absence, changed bytes, empty success and read errors remain explicitly unconfirmed. The pre-delete snapshot reference survives in the original wrapped cause. |
| Fresh Create failure | `install.go:3880–3881`, same helper | Only zero returned bytes plus `errors.Is(..., ErrTaskNotFound)` confirms the prior absence. Other outcomes use the existing incomplete-recovery owner. The helper does not retry, delete, restore or import. |
| Causes and task names | `install.go:3788–3811`, `install_task_rollback.go:32–74` | The original typed mutation cause remains reachable through wrapping/joining. Unexpected read failures remain reachable too. Earlier rollback names are combined with the current incomplete name before constructing the outer error; the existing client rollback error can wrap that result without hiding it. Duplicate/aliased planned task names are rejected before this loop. |
| Adopt and provider retention | Existing `adopt.go:1082–1123`, `install_parsed_manifest.go:1043–1063` at the base | The existing classifier routes the new incomplete type to preservation of manifest, provenance and routed keys, with `committed_unverified`. It also prevents an uncertain scheduler result from becoming a provider unpublished-rollback claim. |
| Compatibility and fixture | `install_scheduler_snapshot_test.go:25–37`; existing no-op scheduler and Windows backend | Production interfaces and dependencies are unchanged. The no-op backend still confirms absence and returns the ordinary failure. The changed fixture exposes actual fake state after any attempted Create, including a rejected Create, instead of replaying its pre-Create snapshot. |
| New test design | `install_task_mutation_failure_test.go` and existing helpers it calls | Eleven observation cases cover the central classification boundary; a combined failure checks names and causes; two outer-adopt traversals call the real install loop. `assertPreservedProvenance` checks exact snapshot bytes, the manifest and the exact routed key set. Side-effect counters check that no inverse mutation occurs. This is a review of assertions, not a test result. |

## Boundaries

The classification observes definition bytes or absence at one point in time. It does not prove atomic object-generation identity, owner/credential/access-control preservation, future stability or completion of a process tree. The additional ExportXML call uses the existing scheduler boundary and does not add cancellation/deadline ownership. The current replacement Create recovery path, pruning and successful mutation paths remain outside this change.

The handoff phrase “any observation error” should be read as unexpected observation errors: the expected ErrTaskNotFound sentinel confirming absence intentionally returns the original failure without adding that sentinel. This is documentation precision, not a blocking code defect.

The static findings above were followed by the separate retained-evidence checks below. Publication correspondence remains separate. This review does not promote earlier broad failures or NOT_RUN gates to success.

## Reviewed candidate identities

Table 2. Original draft bytes used for the static review.

| Repository path | SHA-256 |
|---|---|
| `internal/api/install.go` | `4d1e92fbb7490911b41e20989fa38f2b1854c591c59b8b9bb80f73d6f5054153` |
| `internal/api/install_task_rollback.go` | `41bb89ca491cbf2f10dd7eb70d42cc39d1044eb43a049f134f220e3e9c70d79c` |
| `internal/api/install_scheduler_snapshot_test.go` | `554b66722410d4d7c837d374a471b0384d1fff08290c4b18cd70d8017b05fab1` |
| `internal/api/install_task_mutation_failure_test.go` | `de285f683c0db9e4b46e2a5222cd35439e088ae029ba0c97ffef9139e6264fe1` |

## Retained evidence review

The existing R23 control, candidate, adjacent and build/vet records were independently read after execution. All **220 retained stdout/stderr streams**, including source observations, matched their original byte counts and SHA-256 receipts. Per-command receipts matched the phase records, Go terminal events were reconstructed from actual stdout, all selected base/candidate bytes and overlays matched the frozen manifest, and the retained final source snapshots were clean. Inputs were read again to detect drift. This review executed no Go tests, builds or native actions.

Manifest SHA-256: `9519cb49f5c826077b8cace59c453ae1cb71351c90bd7cd53b61400b1c1651ce`.

Table 3. Observed events, with tests and packages separated.

| Phase | Command exit | Test pass / fail / skip | Package pass / fail / skip | Retained wrapper state |
|---|---|---|---|---|
| control | 1 | 0 / 16 / 0 | 0 / 1 / 0 | complete, expected negative control |
| candidate | 0 | 16 / 0 / 0 | 1 / 0 / 0 | complete |
| adjacent | 0 | 77 / 0 / 1 | 1 / 0 / 0 | **failed**, unchanged |
| build-vet | build 0, vet 0 | no test command | no test command | complete |

The negative control matched the exact frozen selection and each failing leaf had the expected `CURRENT-MUTATION-`, `CURRENT-AND-EARLIER:` or `ADOPT-CURRENT-MUTATION:` assertion family. The candidate passed the 16 selected test events. Race diagnostics were absent from all retained streams.

All **20 admitted adjacent root tests passed**. The sole skip was `TestExecuteInstallToRollbackBackendCompatibility/unsupported_platform`. Its captured output says the native backend is supported on this platform; the unchanged source at `internal/api/install_task_rollback_test.go:395–402` runs this subtest only on Linux or Darwin (macOS). The source's SHA-256 `09cd564df73d0721bf2b56c83c63923110dea14840d8ce754edae063faae5030` and Git blob `39c60a131dbe28fc8e0da2006e72604ed5d3e5b0` were checked. This scenario remains **NOT_RUN_ON_WINDOWS**. The wrapper's original `failed` state and “Qualification selected an unexpected skipped test: adjacent” error are preserved; no neighbouring tests were rerun to replace that result.

Table 4. Original phase and command-output identities.

| Phase | Phase receipt SHA-256 | Main stdout bytes / SHA-256 |
|---|---|---|
| control | `a7374b03e7adf94878fe3d3e8b2dbdd280ab5cedac65b39dad44b7a1c72ffa5e` | 25,647 / `2c6816ad69cf7c92e92522bca0f3f814d16f13463fd0859be8e923fcf489c913` |
| candidate | `133d01b5a913f647c6379b3ec30095554da2372ef47bcdb2c72f14af373c05d8` | 16,321 / `eaee3aa8575a7ef5295aeaacce6cc581b6c08b14c6831cbae40aeb39e7f5f833` |
| adjacent | `a815f5cd1acdea287daff9e365c64cedf519f3b258f59082be2c0dca68d6f45f` | 72,744 / `42a6e8176f97eb22c71ee85abe8d4590c73fd79154242baef3e8fe8c82bbf6ee` |
| build-vet | `af3e9557ba515f1712f8de5b934c0b816b8fd06e077b23ac4d7f411daebb2abd` | build and vet: 0 bytes each |

All five command stderr streams and both build/vet stdout streams were empty, with SHA-256 `e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855`. Exact arguments and all 220 stream identities are in [RETAINED-EVIDENCE-REVIEW.json](RETAINED-EVIDENCE-REVIEW.json).

The formatted new test was read back independently: SHA-256 `7496583080e79d2f531ab85c5c4b4a9b50884f82bd729493df59edfc873a2ed5`. Its three differences from the draft in Table 2 are alignment whitespace after the `name:` field; the product files and existing test retain their Table 2 hashes. The first read-only review helper compared test-root names against an oracle map that also included a package entry; that helper assertion was corrected by explicitly excluding `PACKAGE:` from the root set. Its original source and diagnostic are retained. No product or original execution evidence changed.

The execution source remains `a63199628c1689b4b77a50abfc7432396a1893f4` plus the recorded overlays. These results do not qualify the entirety of a later combined branch head or establish a whole-process-tree deadline/settlement.
