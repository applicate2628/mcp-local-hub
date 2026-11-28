# R25 — root review and Windows qualification of PR #648 native pin tests

## Contents

1. [Decision](#decision)
2. [Own review](#own-review)
3. [Executed checks](#executed-checks)
4. [Limits](#limits)

## Decision

Append the single new Windows test file to PR #648 at `fc4dc5c7145e4f4d709b91654ab4bc1fd7abe259`. The reviewed candidate is 9,418 bytes, SHA-256 `d75864f7d0f8517249ae7a68317f94735cf4589327e1398a757ea120ef997f68`, Git blob `0798bba831c055965be0b4326a44b3c4bf97346b`. No production change is justified by these cases. Keep the PR in Draft.

## Own review

The main agent read the full candidate, the exact production finish/close receiver, the native handle-close owner, the contained runner's start/error/settlement path and the pinned x/sys Windows declarations. The supplementary author and independent reviews do not replace this review.

**Requirements and correctness.** The fixture owns an actual file HANDLE. Access probes request write/delete rights without writing or deleting; they must receive the actual Windows sharing violation while the pin is held and succeed after release. Missing native settlement and incomplete input/output settlement cannot close the pin. The complete positive receipt comes from the original contained runner with an empty command; the oracle requires the original Job's no-child observation and all five acquired stream slots to close. A zero-value fabricated completion cannot satisfy this check. The real local preflight remains distinct from an entered contained attempt.

**Safety and lifetime.** Every opened fixture handle has cleanup registered before close protection or fatal assertions. The protected-close scenario changes only a test-owned HANDLE flag. The first native CloseHandle refusal remains sticky after the flag is cleared, including through the tree close receiver. Cleanup closes the still-owned native handle without rewriting that refusal. A successful receiver close prevents cleanup from touching its old numeric handle. File probes close any successfully opened handle. Tests run under the existing isolated TestMain and the root runner's explicit environment; no child executable, installer, scheduler or deployed Product is invoked.

**Compatibility.** Revision 1 incorrectly looked for the private cause in the deliberately safe outer error message; root rejected it before runtime. Revision 2 used the original nil-guarded PrimaryError, but failed real compilation because pinned x/sys v0.47.0 lacks the SDK constant. Revision 3 uses the documented test-local uint32 value 0x00000002; the compiled ABI and native calls now passed. Both rejected candidates and the compiler output remain evidence, not successful runs.

**Complexity.** Two roots and four subtests use existing receivers and the original process runner. There is no new production seam, exported API, dependency, wrapper layer, global mutation or parallel test timing assumption. The synthetic owner state and deliberately degraded negative input/output projection are stated directly in the source.

No blocking defect remains in the reviewed test-only delta. Full acquisition and installed-product acceptance remain outside this test's authority.

## Executed checks

Exact clean Git source observations before/after bind tree `47a6d86501573cb96e45b469365d8f54c18ea22d`; the one candidate overlay is separately hash-pinned. The phase's per-path source SHA-256 map records the new test path as absent. The original implementation/runner files were reviewed from the pinned head; this is not a claim of ten individual per-file runtime hash captures. Installed gofmt accepted the candidate. Scope `pr648v3`, manifest SHA-256 `c1bf5533cf6b94879c18ac9c70ccd006e0ea564c36ab59f46495fe94fdb06996`, wrapper SHA-256 `610269f4e6841d336c4339abafe94751db8386d49c3fa4a2ce3bb103a794984e`.

| Table 1. Check | Observed result |
|---|---|
| Windows selected roots, race detector, default build | Exit 0; 2 roots, 5 leaf cases, 6 test/subtest PASS and 1 package PASS; zero FAIL/SKIP. |
| Same selection with `test_state_path_env` | Exit 0; same exact terminal oracle; zero FAIL/SKIP. |
| `go build` with overlay, `./...` | Exit 0. |
| `go vet` with overlay, `./...` | Exit 0. |

Total: 12 test/subtest PASS, 4 root executions, 10 leaf executions and 2 package PASS; two successful build/vet commands. Source remained clean. Raw phase receipts and output streams remain under `evidence/pr648v3-qualify/` and `evidence/pr648v3-build-vet/` in the R25 evidence archive.

## Limits

These are source-plus-overlay runs. Publication readback must establish that the new commit contains exactly this overlay over the same parent; this is not a separately executed published-head run. The native owner state is synthetic and exercises finish/close custody, not acquisition, begin-use admission, publisher authority, successful child creation or complete Serena installation. The whole default/tagged suites, installed Product and external approval are not established. Direct-child command completion is observed; the command wrapper does not prove settlement or a deadline for the entire process tree.

Terms: PR — pull request; HANDLE — Windows native object handle; SDK — software development kit; ABI — application binary interface; SHA-256 — a 256-bit cryptographic content checksum; I/O — input/output.
