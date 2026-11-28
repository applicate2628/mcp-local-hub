# Scheduler upgrade snapshot admission — 22 November 2026

## Contents

1. [Defect and correction](#1-defect-and-correction)
2. [Exact source and observed checks](#2-exact-source-and-observed-checks)
3. [Review and remaining contracts](#3-review-and-remaining-contracts)

## 1. Defect and correction

The per-server `SchedulerUpgrade` path discarded errors from `ExportXML` and
continued to `Delete`. If subsequent creation failed, there was no original
Extensible Markup Language (XML) definition to restore. Both that path and the
workspace lazy-proxy path also deleted after observing an absent task, allowing
a newly appearing task to be removed before `Create` could refuse the conflict.

One small admission helper now requires either a successful nonempty copied
snapshot or `ErrTaskNotFound` with no returned bytes. Other errors, partial bytes
with an error, and empty successful exports refuse mutation of that task. A
confirmed absence proceeds directly to `Create`; the existing creation contract
refuses an occupied name. Refusal for one server task does not prevent the next
healthy task from being upgraded.

Only `internal/api/scheduler_mgmt.go` and one regression file change. The public
signatures/result format and the separate weekly-refresh transaction remain
unchanged. No new dependency or production test hook was introduced.

## 2. Exact source and observed checks

The publication parent is [b575b2c](https://github.com/applicate2628/mcp-local-hub/commit/b575b2c1c44c03bd14da281f8b81f50c6aec4bb9).
Its `scheduler_mgmt.go` bytes are identical to current master
`75dbc6406123e22468f168b861d5e0622c9d5769`.

Execution used the separately preserved master-based source
`858812988833a84368c60fba1540ae601155fa71`, tree
`dba685c090952a93a80a1e82403c4646ad8aaed1`, plus the previously qualified Scheduler
composition and these candidates: 41 Go overlay mappings for the negative
control, 42 for the candidate. The supporting API TestMain is the safe #665
composition; reconciled BackendRunHold behavior is preserved. This is **not a
standalone qualification of the older #660 branch**.

The full 3281-file physical source inventory, all 50 admitted input files,
overlay maps, exact commands, tool hashes and raw event logs are preserved in
[raw-evidence.zip](raw-evidence.zip). [results.json](results.json) includes the
independent verification of every payload and the exact terminal-event sets.
The previous composition is documented in the
[Stop-error report](https://github.com/applicate2628/mcp-local-hub/blob/b575b2c1c44c03bd14da281f8b81f50c6aec4bb9/docs/modernization/verification/scheduler-stop-errors-20261122/README.md).

**Table 1 — observed Windows/amd64 results, existing Go 1.27.4.** Tests used the
race detector, count 1 and precise admitted selectors.

| Check | Result |
|---|---|
| Old production + new regression | 17 failing and 9 passing leaf cases; two groups/root/package also fail; exact expected Go exit 1 |
| Candidate regression | 26 leaf + two group + one root PASS; package PASS; no FAIL/SKIP |
| Eight adjacent roots | 12 test/subtest PASS + package PASS; no FAIL/SKIP |
| `gofmt` | No byte change; empty subsequent check, exit 0 |
| Full `go build ./...` with candidate overlay | Exit 0 |
| Full `go vet ./...` with candidate overlay | Exit 0 |

The two branches each cover 13 cases: denied/unavailable/canceled/unsupported
export, partial data, nil/empty successful export, contradictory absence,
existing task, direct/wrapped absence, a creation conflict, and denied deletion.
The per-server cases also require the healthy sibling to finish successfully.
The checks assert final task bytes and reported outcome, not only call counts.

The fixture uses an in-memory Scheduler, private manifest/registry/state paths,
and a non-executable canonical-path stub. Unexpected Run/Stop/Status fail the
test. It starts no process, listener, native task or installed Product. The
adjacent selector deliberately excludes `TestSchedulerUpgradeNoopWhenEmpty`,
whose current fixture can reach the native scheduler. Exact JSON event sets
were compared for equality; no extra or skipped tests were accepted.

## 3. Review and remaining contracts

Root own review and independent source review covered the two callers, existing
platform contracts, helper reuse, fixture and TestMain isolation, error paths,
compatibility and complexity. No blocking defect remained in this bounded
correction. External review of the new Git head remains pending; keep Draft.

**The whole replacement transaction is not fixed.** After failed `Create`, the
two existing rollback sites still discard `ImportXML` errors. Windows import
uses forced replacement (`/F`) and may overwrite a competing task. The next
correction needs preservation of both failures, retained recovery material and
creation-only restoration through the original owner. A successful snapshot
also does not establish atomic ownership of the task generation until Delete.

Cancellation here means propagation of a returned cancellation error, not
cancellation of a live Component Object Model (COM) operation. Actual Windows
owner identity/rights, bounded native Run/RunOwned/Stop/Delete, recovery under
concurrency, full default/tagged Go suites and installed-product qualification
remain open. All direct children were waited and no timeout occurred; full
process-tree settlement is not established by this runner. No dependency
downloads, installation, native scheduler mutation, manual CI or paid/quota
review was performed.
