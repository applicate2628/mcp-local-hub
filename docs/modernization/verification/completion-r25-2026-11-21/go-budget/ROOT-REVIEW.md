# R25 — root review of the conflict validation oracle

## Contents

1. [Defect and decision](#defect-and-decision)
2. [Own review](#own-review)
3. [Actual qualification](#actual-qualification)
4. [Rejected performance change](#rejected-performance-change)

## Defect and decision

In `internal/cli/install_reconcile_mcp_front_v3_test.go`, `TestMCPFrontV3_SerenaConflictClassifierMutationTable` currently accepts any non-nil reader/rollback error for 30 malformed documents. If a file cannot be opened or admitted, or fails JSON decoding, the test can pass without reaching the domain validator it is intended to exercise. The independent `readerCalls == 0` assertion does not exclude this earlier refusal. This is a false-positive risk in the test oracle, not an observed production authorization bypass.

Require the expected semantic validation message from both the real anchored reader and rollback. Preserve all 34 cases, mutations and classifier expectations, all four valid cases, pin-reader non-entry, the real shared journal setup and every original `WriteStateFileAtomic` call. There are no suitable existing typed/sentinel errors for these gates, so the test uses narrow existing domain message fragments. Do not add new production errors or abstraction solely to support this test.

The file is unchanged by current #637 and #658 and is byte-identical to master `a7d92d2852c4f6eb8afb0569ef76a545e38aacf7`. A separate small Draft PR owns this independent test correction; no donor or useful existing PR is closed for numerical consolidation.

Final candidate: SHA-256 `91a326178b2e2f30be40b181dd4853cafe3c1ee3a0b56fb7406441d7c9619ba4`, Git blob `4768fce28e27a73baf5fc9973b015fc706fbfb6c`, 113,546 bytes. The patch changes one existing function: 35 additions and 20 deletions, no import, production or dependency change.

## Own review

The main agent read the complete diff, the original test setup, exact reader/validation/rollback branches and the Windows leaf/parent admission implications; the author and supplementary independent reviews are additional evidence.

- Requirements/correctness: ten case-specific overrides map to earlier binding, identity, generation and operation validation gates; twenty remaining negative cases require the existing authority-shape refusal. Both reader and rollback assertions short-circuit safely on nil error. All 34 names/order and classifier expectations remain intact.
- Security: error assertions cannot pass on unrelated open, permission, missing-file or JSON failures. The original secure durable writer and hardened fixture directories remain. The patch does not relax any production gate, reinterpret a caller digest as authority or issue a qualified publisher receipt.
- Compatibility: both supported test configurations passed against exactly the same clean master source. Production behavior and public API are unchanged. Error wording remains an intentional test dependency until a genuine typed error contract exists.
- Complexity: one optional table field and two direct assertions, no new helper layer or duplicated owner. Existing explicit cases remain readable. The follow-up that removed durability calls was rejected for publication after the measurements below.

No blocking defect remains in this narrowly scoped test change. It does not make the full program or installed Product ready.

## Actual qualification

Source tree `bda8789467feb6795d2ab341479a32b27e8cb489`; clean source plus the one pinned overlay. Scope `invalid-oracle`, manifest `cd167a9cda767b2e57e64d29f689a12819d86ca34052ea89e91cbc3078f17dd9` and frozen wrapper `610269f4e6841d336c4339abafe94751db8386d49c3fa4a2ce3bb103a794984e`.

| Table 1. Check | Observed result |
|---|---|
| Installed gofmt check | Empty output, exit 0. |
| Selected Windows root, default with race detector | 1 root, 34 leaf subtests, 35 test/subtest PASS and 1 package PASS, zero FAIL/SKIP; exit 0. |
| Same selection with `test_state_path_env` and race detector | Same exact terminal oracle, exit 0. |
| `go build -overlay ... ./...` | Exit 0; external command 8.060213 seconds. |
| `go vet -overlay ... ./...` | Exit 0; external command 9.013998 seconds. |

Source and candidate remained unchanged before/after. Publication must read back exact bytes and one parent. No separately executed published-head run is implied. Full default/tagged suites remain NOT_RUN in R25; prior full-suite failures are retained. Direct-child completion is observed, without whole-process-tree settlement/deadline proof.

## Rejected performance change

A preceding candidate replaced 30 durable invalid-input writes with ordinary writes and strengthened the same domain assertions. It passed its selected default repetitions and tagged race check, but did not demonstrate the intended speedup. On the same master snapshot, root durations were 1.99, 2.21 and 1.66 seconds for the original, versus 6.63, 3.24 and 2.89 seconds for that candidate. Package and external-command times measure other boundaries and are not substituted for these root samples.

These six observations do not identify the cause of the difference or establish a general performance regression. They are sufficient to decline the unproven preparation change here. The final patch restores the original writer and makes no speed claim. Both complete earlier outputs, candidate bytes, receipts and the separate 190-phase diagnostic remain evidence outside trash.

Terms: PR — pull request; SHA-256 — 256-bit cryptographic content checksum; API — application programming interface; JSON — JavaScript Object Notation; NOT_RUN — not executed for this qualification.
