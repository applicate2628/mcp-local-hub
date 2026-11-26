# Go and Rust stdio consumer evidence

**Consumer comparison NOT_RUN on 26 November 2026.** Two isolated attempts reached closed preparation failures at the original 300-second deadline. A third attempt stopped during original-owner admission before the private Git checkout started. The latest guard missed the reader cleanup deadline and retained the failure even after the original readers were physically received. [Normalized evidence](evidence.json)

## Sources and intended workload

This experiment exercises the private Go source at `3ebe335a2f666a5279d7fafc31a6f4dc3feedff4`, tree `67c13b8fc1317b4fd60e3135a3eb8a376fe01817`, with one test overlay. All 3,508 ordinary Git blob contents were independently checked against the preserved tracked-source map. The complete tree contains 57,144,526 bytes. The Rust dynamic-link library was originally built at `efa73c0e23f7af16b8aa2ccb047b745a58ba4f71`, with source association based on the preserved map of 64 native files. The library digest is `dfa87a10a39e9110cda735ae6a987bfa64b03446529072710723e9b6ea0dca56`. The library remained outside execution during this attempt. [Source, overlay and tool identities](evidence.json)

The planned host order is Go–Rust–Rust–Go, using the private production `NewStdioHost`, original helper subprocess and retained native owner. Each host targets 66 JSON-RPC (JSON Remote Procedure Call) requests across three cases. It exercises healthy service after canceled observations, final reply before normal exit, and cancellation of the original service lifetime. The HTTP handler is called in-process. Session setup uses the existing internal helper and omits the MCP (Model Context Protocol) initialize exchange and network listener. This workload can check consumer behavior and report exploratory timings under the pinned inputs. Product qualification and the decision to adopt Rust have separate acceptance criteria.

The [Go test overlay](testdata/zz_rust_adoption_stdio_consumer_windows_test.go) preserves the exact 10,147 bytes staged on Windows. It is stored under `testdata` and depends on helpers and the production route in the declared private source. The [draft plan](draft-plan.json) preserves the fixed command templates, work deadlines, receipt rules and exact Git checkout changes. Machine paths are represented by their artifact roles. Format, build and test execution of this overlay all remain **NOT_RUN**.

## Closed first attempt

**Table 1. Phase outcomes.**

| Phase | Observed result |
| --- | --- |
| Data-only staging | 20 files independently read back with matching SHA-256 and byte counts. The PowerShell wrapper emitted two unavailable `Get-FileHash` diagnostics. The exact transport receiver exited 0, and independent data readback verified all staged files before the guard. |
| Original-owner guard | Normal command exit 0. Deliberate timeout exit 1, its original Job terminated and closed. Both commands had settled original process, handle and capture receipts. |
| Source preparation | **FAIL**, command exit 1 after 300.216 s on a 300 s work budget. Original Job was terminated, active process count reached 0, and handles and both empty capture streams closed within the cleanup budget. |
| Parent and stop gate | Original parent exit 1 after 301.055 s. The controller was reaped and capture handles closed. No parent diagnostic deadline exceeded. The stop gate forbids new commands in this attempt. |
| Format, compile and comparison | **NOT_RUN**. No successor plans, materialization receipt, compiled image or consumer result were present. |

After closure the partial copy held 615 source files and 21,398,684 bytes. No proxy, staged library or compiled file was present. The final stop gate's SHA-256 is `7bd333c977c3007e11919270ef870dc16ec7e3e03dd6c18cd320a1ea6ad0c513`. The original parent receipt's SHA-256 is `d8f9f444f6961367395737995775ed1ed1dc0908284a617a8d2c19b26a3377ed`. SHA-256 is the 256-bit Secure Hash Algorithm. The phase records preserve the original capture roles and hashes. Live services and Task Scheduler were outside the run. [Original phase and parent pins](evidence.json)

## Follow-up diagnostics

The partial copy's creation timestamps cover 299.101 s, with a 154.7 ms median gap between successive files. The materializer checked each path and its ancestors, read and hashed the source, created an exclusive output, and wrote, flushed and synchronized each output file. In a later read-only 25-file sample, the median ancestor check was 0.198 ms. Median file stat and read was 0.072 ms.

A separate same-volume control used 16 synthetic files in sync–buffered–buffered–sync order. With per-file synchronization, `fsync` had a 9.94 ms median and a 271.04 ms maximum. Directory creation and ancestor checks had a 0.275 ms median, and writing and flushing had a 0.126 ms median. The failed preparation did not record timing for individual system calls. The control isolates a cost that can explain the timeout without assigning each historical file delay to an observed call. The small probe made no claim about steady-state runtime performance. [Per-file diagnostic timings](evidence.json)

The partial source files from the first attempt all matched their pinned donor bytes. Their 615-entry inventory was retained with the original failure and captured streams. The verified reproducible copy was moved to trash after its original owner and handles closed. [Partial-source inventory digest](evidence.json)

## Second attempt with buffered source writes

The second isolated attempt kept all exclusive writes, flushes, close and readback, source and donor checks, and original work deadlines. It omitted per-file `fsync` only for the 3,508 reproducible tracked source copies. The overlay, library, proxy files and receipts kept their original write policy. The disposable source copy had no crash-durability claim.

Fresh ownership controls passed. Preparation again reached a **closed FAIL** at the original 300 s deadline. The copy reached 1,736 files and 36,117,817 bytes. The original command exited 1 after 302.947 s including cleanup, and the parent exited 1 after 303.206 s. Its Job reached zero active processes, and original process, handles and readers were received closed. No materialization receipt or successor phases were issued. This result narrows the diagnostic conclusion. The isolated synchronization cost accounted for one expensive operation, and removing that operation alone still left the source-copy route outside budget. The first attempt remains FAIL. [Exact functional delta, stop gate and original parent receipt](evidence.json)

All 1,736 partial source files in this attempt matched their pinned donor bytes. As with the first attempt, the inventory and receipts were retained and the verified reproducible copy was moved to trash after closure. [Second inventory digest](evidence.json)

## Third attempt with a private Git index

The staged materializer uses `git read-tree` with a fresh private `GIT_INDEX_FILE` and `git checkout-index` into an empty source directory. Its check requires the exact 3,508 regular Git blobs, disabled hooks, absence of conversion attributes, unchanged donor metadata and a full hash census after checkout and overlay. Git children would inherit the preparation command's original Job and its single 300-second work budget. Preparation was **NOT_RUN**. [Exact staged changes and command template](draft-plan.json)

The original-owner normal control exited 0. Its intentional timeout control exited 1, and its actual descendant produced the expected readiness marker. The initial timeout control exhausted 2.000499 s of work. The external native settlement returned at 3.494584 s. The original direct child was reaped, its Job had zero active members and its native handles closed.

At the last join observations, approximately 11.99864 s into the timeout command, both original readers were still observed alive. End of file and pipe closure had been observed for both. Capture-file closure and worker completion were unconfirmed. The original deadline result at 12.000193 s remained **UNKNOWN** for reader quiescence and **FAIL** for the gate. The same-owner late receiver then recorded both original workers physically joined, and the parent exited 1 after 28.950 s with its controller and captures closed. No successor plan was issued. [Deadline and late physical closure records](evidence.json)

The last timely observations place the missing evidence at the capture-file close and worker completion boundary. The capture drain calls `writer.close()` and records completion afterward. The record does not separately time capture close or thread scheduling, and the drain has no explicit `fsync` call. A delay while writing the final receipt does not explain the missing reader joins already recorded at the deadline. A fresh controlled admission after the concurrent qualification settles can distinguish transient host contention from a repeated guard problem. The existing gate and the two earlier preparation failures retain their original classifications.
