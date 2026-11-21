# Qualified Go test inventories

## Contents

1. [Results](#results)
2. [Source and scope](#source-and-scope)
3. [Evidence and limits](#evidence-and-limits)

## Results

All four selected compiled inventories completed successfully. ROOT means a top-level test, example, or fuzz-test name. API means application programming interface; CLI means command-line interface.

| Selection | ROOT names | Test / Example / Fuzz | Benchmarks listed, not run | Groups planned, not executed |
|---|---:|---|---:|---:|
| api-normal | 3437 | 3437 / 0 / 0 | 1 | 43 |
| cli-normal | 1960 | 1960 / 0 / 0 | 0 | 25 |
| api-tagged | 3443 | 3443 / 0 / 0 | 1 | 44 |
| cli-tagged | 1963 | 1963 / 0 / 0 | 0 | 25 |

The build tag is `test_state_path_env`. Normal and tagged ordered ROOT lists are identical for API: `false`; for CLI: `false`. Exact differences, if any, are in `inventory-comparison.json`.

## Source and scope

Source commit: `f12932bcca6c288d062f9affd32ea58a7c9eb74f`, tree `79a36a7aca62cd2c08cb6a1a79a1f7d58ad6010c`. Preparation and inventory used a new detached checkout. The clean source and all 3216 tracked-file hashes matched before and after; aggregate SHA-256: `e5e03931462e4e9a981e7ed09637d58b0feb36d67efb38afcdb4cdbe974a33c0`.

The existing Go compiler reported `go version go1.27.4 windows/amd64`. The standalone tool build selected only `./tools/testshards`; each listing also compiled the selected package's test binary. The four plan invocations used explicit `-execute=false`; the underlying operation was `go test -list . -p=1` for API or CLI, with the tag on the two tagged selections. Package initialization and existing `TestMain` setup/cleanup ran. Test, example, fuzz and benchmark bodies and planned groups were not executed. Module/toolchain downloads were disabled. No branch integration or installation occurred.

Preparation: `2026-11-21T09:53:33.213739+00:00` to `2026-11-21T09:53:50.918683+00:00`. Inventory: `2026-11-21T09:54:21.282117+00:00` to `2026-11-21T09:57:01.564544+00:00`. These source receipt times use Coordinated Universal Time (UTC); Moscow time is UTC plus three hours.

## Evidence and limits

`REVIEW.md` explains the source review, prerequisites, environment allowlist and exact execution boundary. The package contains the executed controller, this exporter, both original receipts, every receipt-referenced command stream, all four original lists and plans, and derived source/comparison summaries. SHA-256 means the Secure Hash Algorithm with a 256-bit digest. `PUBLIC-HASHES.json` records every payload size and checksum; all copied payload bytes are unchanged. Generated summaries are identified separately. No redaction was needed for selected evidence.

The full source tree, executable binaries, full source manifests, runtime directories and caches remain outside this public package. The three retained full source manifests were checked against the receipts and recomputed aggregate; their original checksums are in `source-qualification.json`. Source file contents are available at the exact public commit.

All top-level commands returned exit code zero and settled through retained direct handles. The controller does not prove deadline enforcement or settlement for an entire descendant process tree. The original inventory tool clears child `GOFLAGS`; source hashes and cleanliness were rechecked after execution.

These inventories establish compiled top-level names, counts and a diagnostic grouping plan. They establish neither nested subtest counts nor successful test-body execution or durations. They do not replace the unchanged five-minute unsharded gate, qualify the historical failing source, or reconstruct the missing historical Go JSON event trace (JSON is JavaScript Object Notation).
