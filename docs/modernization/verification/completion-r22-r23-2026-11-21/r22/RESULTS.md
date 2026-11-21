# R22 results and retained failures

## Contents

- [1. Command and event evidence](#1-command-and-event-evidence)
- [2. Consumer and native observations](#2-consumer-and-native-observations)
- [3. Review boundaries](#3-review-boundaries)

## 1. Command and event evidence

Table 1. Counts below are terminal event occurrences, including repeated tests; test events and package events are separated. Zero test events for build, vet or formatting commands does not mean tests were run. `complete` is the retained phase state and can mean an expected failing control was correctly observed. Exact test names and command arguments are in execution-summary.json and original retained receipts.

| Phase | Exit code(s) | Test pass/fail/skip | Package pass/fail/skip | Retained outcome |
|---|---|---|---|---|
| cmake-adjacent | 0 | 6/0/0 | 1/0/0 | complete |
| cmake-build-vet | 0, 0 | 0/0/0 | 0/0/0 | complete |
| cmake-candidate | 0 | 14/0/0 | 1/0/0 | complete |
| cmake-consumer-baseline | 0 | 4/0/0 | 1/0/0 | complete |
| cmake-consumer-benchmark | 0 | 0/0/0 | 1/0/0 | complete |
| cmake-consumer-candidate | 0 | 4/0/0 | 1/0/0 | complete |
| cmake-consumer-prepare | 0, 0, 0, 0 | 0/0/0 | 0/0/0 | complete |
| cmake-control | 1 | 1/6/0 | 0/1/0 | complete |
| cmake-prepare | 0, 0, 0, 0 | 0/0/0 | 0/0/0 | complete |
| diagnostics-build-vet | 0, 0 | 0/0/0 | 0/0/0 | complete |
| diagnostics-candidate | 0 | 6/0/0 | 1/0/0 | complete |
| diagnostics-control | 1 | 0/3/0 | 0/1/0 | complete |
| diagnostics-prepare | 0, 0, 0, 0 | 0/0/0 | 0/0/0 | complete |
| lsp-benchmark-admit | 0 | 0/0/0 | 0/0/0 | complete |
| lsp-benchmark-baseline | 0 | 0/0/0 | 1/0/0 | complete |
| lsp-benchmark-candidate | 0 | 0/0/0 | 1/0/0 | complete |
| native-raw-roundtrip | 0, 0, 0 | 0/0/0 | 0/0/0 | complete |
| pending-ram-adjacent | 0 | 16/0/0 | 1/0/0 | complete |
| pending-ram-build-vet | 0, 0 | 0/0/0 | 0/0/0 | complete |
| pending-ram-candidate | 0 | 56/0/0 | 1/0/0 | complete |
| pending-ram-control | 1 | 0/25/0 | 0/1/0 | complete |
| pending-ram-prepare | 0, 0, 0, 0 | 0/0/0 | 0/0/0 | complete |
| relay-retry-candidate | 1 | 1/1/0 | 0/1/0 | failed |
| relay-retry-control | 1 | 0/1/0 | 0/1/0 | complete |
| relay-retry-prepare | 0, 0, 0, 0 | 0/0/0 | 0/0/0 | complete |
| relay-retry-v2-adjacent | 0 | 4/0/0 | 1/0/0 | complete |
| relay-retry-v2-admit | 0 | 0/0/0 | 0/0/0 | complete |
| relay-retry-v2-build-vet | 0, 0 | 0/0/0 | 0/0/0 | complete |
| relay-retry-v2-candidate | 0 | 2/0/0 | 1/0/0 | complete |
| relay-retry-v2-control | 1 | 0/1/0 | 0/1/0 | complete |
| scheduler-rollback-adjacent-snapshot | 0 | 15/0/0 | 1/0/0 | complete |
| scheduler-rollback-build-vet | 0, 0 | 0/0/0 | 0/0/0 | complete |
| scheduler-rollback-candidate-backend | 0 | 8/0/0 | 1/0/0 | complete |
| scheduler-rollback-candidate-core | 0 | 84/0/0 | 1/0/0 | complete |
| scheduler-rollback-control | 1 | 6/36/0 | 0/1/0 | complete |
| scheduler-rollback-prepare | 0, 0, 0, 0 | 0/0/0 | 0/0/0 | complete |

Relay v1 candidate exit 1, race diagnostic and mixed repeated test outcomes are retained. Relay v2 has separate control, candidate, adjacent and build/vet evidence. The scheduler negative control was rechecked with the pinned read-only validator: 6 passing test events, 36 failing test events and one failing package event, including exact assertion prefixes. No tests were rerun for this revalidation.

## 2. Consumer and native observations

For the three CMake cases selected-32, selected-1000 and last-marker, baseline discarded argument-reference counts were 33,275, 31,339 and 33,265; candidate counts were all zero. Candidate discarded nonzero-record and string-byte-reference sums were also zero. All three baseline/candidate public JSON byte pairs were equal. These are retained references inspected by the test, not unique heap, resident-set size (RSS), peak-memory or concurrency measurements. Timings are single observations without acceptance thresholds.

The native roundtrip was revalidated from its durable observations, stdout, private manifest/final records, seed bytes and four raw-export files. Recorded calls and results reconcile to two registrations and two deletions with separate absence checks. Raw XML is not published. The count basis is harness call/result observation, not system-call tracing. Exact source identity and digest-only private evidence references are in execution-summary.json.

## 3. Review boundaries

The packager checks immutable input identities, flat evidence inventories, per-command start/launch/final receipts, original stream bytes, Go events, frozen negative/positive oracles, candidate Git blobs, overlay maps, source snapshots, CMake byte pairs, scheduler assertion diagnostics and native durable records. All selected inputs are read again before output creation; outputs use exclusive creation in a previously absent directory. No product repository, prior evidence or existing file is rewritten.

Publication correspondence uses GitHub file blob identifiers at exact supplied commits. It does not establish that those complete commits were executed. The private-to-public boundary is explicit in MANIFEST.json. Process-tree settlement, broad program gates, installed product behavior and formal external approval remain outside these results.
