# Serena wrapper closure and bind refusal — 22 November 2026

## Contents

1. [Defect and correction](#1-defect-and-correction)
2. [Exact qualification input](#2-exact-qualification-input)
3. [Observed verification](#3-observed-verification)
4. [Limits and remaining lifecycle work](#4-limits-and-remaining-lifecycle-work)

## 1. Defect and correction

The prepared Serena wrapper joined an operational `ListenAndServe` failure into
its resource-close proof. An occupied local port therefore invalidated that
proof even after the server worker returned its result and closed its channel,
shutdown completed and receiving stayed within the original cleanup deadline.
`ReceiveWrapperServerClose` then refused finalization. After successful upstream
startup the consumer is already current/confirmed; the missing result is the
terminal start disposition and closure receipt, not necessarily a still-pending
row.

The same prepared path returned the raw bind failure without the existing
`bindRefusedExit` classifier. The supervisor uses reserved exit code 3 to select
a new dynamic port; a generic failure did not carry that contract.

The existing wrapper scope now separates a received operational Serve error
from proof of completed resource receiving. All identity, exact-deadline,
shutdown and worker-completion checks remain. A small result-policy method on
that same scope preserves every original cause and maps the received Serve
error through the existing classifier only after shutdown, native stop and
consumer-close receiving all succeed. It also handles a Serve frame received
after service cancellation or child exit wins the initial select. Unsettled
cleanup does not become a reallocation-safe exit.

The startup-failure cleanup branch is unchanged. No new application programming
interface (API), owner, dependency or runtime test hook was added. Publication
changes two production files and one focused Windows test file.

## 2. Exact qualification input

The publication parent is [2defe61](https://github.com/applicate2628/mcp-local-hub/commit/2defe618259f7db4144824d19927b032300d160d).
Both affected original wrapper files have identical Git blobs in that parent,
master `75dbc6406123e22468f168b861d5e0622c9d5769`, and the supplied heads of
#643, #650 and #654.

The physical test source is preserved master-based commit
`858812988833a84368c60fba1540ae601155fa71`, tree
`dba685c090952a93a80a1e82403c4646ad8aaed1`, with all 3281 files pinned. Each Go
overlay contains exactly 15 mappings:

- The two existing #655 finalization source/test files at the publication parent.
- Ten coherent command-line interface (CLI) TestMain/test files from
  #666 `09de8e98d1aa6f122215dbe869bc6f7d0ce083f5`.
- These three candidate files; a negative control substitutes one close-policy
  file at the same mapping.

This is **composition qualification, not a standalone test of the older #655
branch**, and does not qualify the entire four-PR Serena composition. Exact
source inventories, all 20 input/control/overlay files, commands, tool hashes,
raw events, formatter preimages and direct-child receipts are in
[raw-evidence.zip](raw-evidence.zip). [results.json](results.json) records the
independent exact-event and payload verification.

## 3. Observed verification

Tests ran on Windows/amd64 with existing Go 1.27.4, race detection, count 1 and
precise selectors. The regression uses only its own loopback address and a fake
native-stop callback. An explicit test-owned completion signal ensures that the
real Serve attempt has returned and closed its channel before shutdown starts;
the two late-receive cases leave the result frame unread. No sleep-based ordering
is used.

**Table 1 — exact terminal events, including the package event.**

| Check | PASS | FAIL | SKIP | Go exit |
|---|---:|---:|---:|---:|
| Restore old Serve-error inclusion in proof | 23 | 5 | 0 | 1 |
| Restore unclassified final Serve error | 20 | 8 | 0 | 1 |
| Candidate: four new roots and 23 leaves | 28 | 0 | 0 | 0 |
| Seven adjacent roots | 8 | 0 | 0 | 0 |

The first control fails the three real bind schedules, their root and package.
The second additionally fails the two result-policy bind-code cases and their
root. These are **one-statement behavioral controls on the candidate**, not
complete builds of the old head. Both controls retain the same input owners and
the same tests.

The 23 leaves cover three bind-receiving schedules, nine result-policy cases,
eight refusal conditions for the close proof and three malformed worker-result
protocols. Ordinary and clean results, shutdown/stop/receive failures, both
Windows bind-refusal codes, original error preservation, missing/duplicate
frames and a frame without final channel closure are checked. Full terminal
event multisets were compared for equality; no extra or skipped tests were
accepted.

**Table 2 — additional checks on the candidate composition.**

| Check | Result |
|---|---|
| Native `gofmt` | Only two test-struct lines gained alignment spaces; production/control bytes unchanged |
| `gofmt -l` | Empty output, exit 0 |
| Full `go build ./...` with candidate overlay | Exit 0 |
| Full `go vet ./...` with candidate overlay | Exit 0 |
| Source, inputs and tool pins before/after | Unchanged |

Root own review found and corrected an initial test scheduling race before
execution. Independent source review then checked the final error policy,
upstream receiver chain, preservation of proof requirements and fixture bounds.
The final formatting delta was read back and inspected. External review of the
new Git head remains pending; keep Draft.

## 4. Limits and remaining lifecycle work

The fixture qualifies wrapper receipt and result policy. It supplies no native
child, prepared package, Ready publication or consumer-intent acceptance proof.
It does **not** demonstrate an actual terminal consumer-intent write, a running
supervisor's port reallocation, installation, cancellation/recovery of real
Serena, or installed Product behavior. Those integrated checks remain required
on an admitted environment and exact accepting source.

Full default/tagged Go suites and other supported-platform execution were not
run. All original direct children were waited; no timeout occurred. The runner
does not establish complete process-tree containment. Dependency downloads,
system scheduler mutations, installations, manual continuous-integration (CI)
runs and external quota review were not performed.
