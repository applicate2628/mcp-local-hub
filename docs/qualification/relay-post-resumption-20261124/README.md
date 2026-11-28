# Relay: resume a POST-origin response without repeating the operation

Qualification date: 24 November 2026 (Moscow time). Owner: #629. Executed source: exact owner `4b6ec39930cb5a0498dba068b99dbf29ba7129b7` plus the five files listed below. This is targeted qualification, not acceptance of an installed Product or the complete repository.

## Contents

1. [Contract and scope](#contract-and-scope)
2. [Executed evidence](#executed-evidence)
3. [Real consumer and failure interpretation](#real-consumer-and-failure-interpretation)
4. [Exact files and review](#exact-files-and-review)
5. [Evidence and remaining work](#evidence-and-remaining-work)

## Contract and scope

An interrupted Server-Sent Events (SSE) response to POST may continue through GET using that original request's endpoint, negotiated protocol version, session and committed event cursor. The operation is never repeated as another POST, and the independently running GET stream cannot lend its cursor or replace those operands. The admitted legacy versions are explicitly `2025-03-26`, `2025-06-18` and `2025-11-25`; an unnegotiated or unknown version, including the different `2026-07-28` transport, does not acquire legacy resumption behavior.

Continuation is permitted only after an accepted complete event establishes a usable cursor. A delivered message without an identifying cursor makes replay unsafe until a genuinely new committed cursor covers it. A partial event cannot commit its pending identifier. Parser errors and output failures are terminal, even if an output error wraps end-of-file. A read interruption can resume; a body-close error cannot. Read and close causes remain inspectable together.

One absolute operation deadline begins before the original POST and also bounds retry delays and continuations. Redirects cannot resend POST. Each response body closes once. Forwarding stops on the matching JSON-RPC 2.0 result or error; a server request using the same identifier and a response with a different identifier do not complete the original operation.

This implements request-local resumption. Interrupted initialization before version negotiation is refused. A failed continuation GET is returned as a failure; it is not retried as an operation. This does not establish durable or exactly-once execution across unrelated processes.

## Executed evidence

Table 1 counts raw Go test events, including root tests, subtests and package events. These counts are not counts of distinct root tests. Tests ran on the already available Windows AMD64 Go 1.27.4 toolchain, offline dependencies, private task state, and `-race -count=1`; no installed Hub or native Scheduler was changed.

| Table 1 — phase | PASS events | FAIL events | SKIP events | Interpretation |
|---|---:|---:|---:|---|
| v1 focused candidate | 25 | 0 | 0 | New contract exercised |
| v1 old-code negative control | 0 | 7 | 0 | Expected failures distinguish absent resumption, lost causes and renewed deadline |
| v2 real SDK consumer | 2 | 0 | 0 | Actual client receives stored server result |
| v2 real SDK on old code | 0 | 2 | 0 | Expected test and package failure: original stream closes without result delivery |
| v2 all Relay/shared receiver regression | 277 | 3 | 0 | Preserved incompatible request/response identifier in an older framing fixture; explained below |
| v3 final all Relay/shared receiver regression | 281 | 0 | 0 | All 74 selected root functions passed, including the real consumer |

The final phase also passed `go build ./...` (22.386 s) and `go vet ./...` (9.698 s). Its race-enabled regression took 35.094 s. Plan SHA-256: `3044d7388e812a1e8c034b731db5b7795f6d32cb9bfb5d74509525ddaf57bd5b`. The exact source inventory is 3,234 files / 54,345,524 bytes, inventory SHA-256 `04ada15ee0c742d75350589dd0cd04ed21184630a2d589090b0f1adad2f4f184`. Full file inventories and unchanged-before/after checks are preserved in the archive.

## Real consumer and failure interpretation

`TestRelayPOSTConsumerSDKResumesStoredResult` uses the pinned Go software development kit (SDK) v1.6.1 client through `IOTransport`, real Relay standard input/output, a loopback SDK Streamable HTTP server and `MemoryEventStore`. The actual SDK closes the POST stream and records the result. A causal gate permits resumed GET only after both original POST return and result storage. The fixture does not manufacture SDK event identifiers or result frames.

The observed invariant is one effect, one tool POST, one request-stream GET continuation, one stored result, one replay and one client delivery. Original session/version and non-main-stream cursor bindings are checked. Relay and client settle; active HTTP handlers reach zero; the server session and event-store session close. The old owner code fails the same real-consumer scenario.

The three unexpected v2 FAIL events all belong to `TestRelayResponseFramingFeedsOneStdioMessage/text/event-stream`, its root and its package. That existing fixture sent request identifier `1` but returned `9007199254740993`. The new completion contract correctly could not report that request complete. The final change makes the fixture request use the same large identifier while preserving its large-integer and escaped-newline framing assertions. A separate `different_response_id` regression requires the mismatched result to leave the original operation incomplete. No production bytes changed between v1 and v3; both subsequent revisions change tests only. Every failed event remains in the evidence.

## Exact files and review

Table 2 binds the published code to the final executed source. SHA-256 is the Secure Hash Algorithm with a 256-bit digest.

| Table 2 — file | SHA-256 |
|---|---|
| `internal/daemon/relay.go` | `2cfdc77a384940bee96492189efccd3cf3af6e0eccceee71c9677b758f114f84` |
| `internal/daemon/relay_post_resume.go` | `be1d1823b1800cef6ac4a42eeedda2473db1c93048cf118d5852a53a0e42cc23` |
| `internal/daemon/relay_post_resume_test.go` | `0f90672cfbdbfc9fb5600837e0cfb283bb6f80fed22e682e05e9a39c6729fcf4` |
| `internal/daemon/relay_post_consumer_test.go` | `39c7fe85f0730150c4d63b023014fab26585c4313c39e93b1dabccfddc508dc0` |
| `internal/daemon/relay_response_contract_test.go` | `3e93f95f822d9d1ead5f2f5c7957c09360936a92986d27f9705e2b8817293445` |

Own full-diff review covered operation identity, parser cursor commitment, request completion, error provenance, body and session ownership, unchanged caller HTTP client, common deadline, shared-parser compatibility, and failure cleanup in the real consumer. No additional actionable defect was found in this reviewed slice. Race execution covers the schedules actually exercised; it does not prove all possible concurrent schedules.

## Evidence and remaining work

[Evidence archive](relay-qualification-20261124-57604a3a.zip) — 1,186,604 bytes, SHA-256 `7853ede2b4a9ec41ad6deed62f34344a85dc83600352c3f5be8e1079d2003107` — contains all six phases, raw event sequences in their published path-redacted view, frozen plans, exact final changed files, preparation receipts and runner sources. [The manifest](evidence-manifest.json) records original and published byte counts and hashes separately. Original unredacted logs and their raw archive remain outside trash in the authorized task directory. Only local path prefixes are redacted; FAIL/SKIP events and causes are retained. A script's recorded original hash refers to its original executable input, not the path-redacted published text.

This evidence does not cover the complete standard/tagged suites, installed Product lifecycle, POSIX or ARM64 execution, or the separately requested consumer memory/latency/cancellation campaign. It is not evidence for the separately issued Source `b426508361858ff2d63a5b994633d4cc367f55bf`, which predates this change. External review of the new published commit remains required. No merge, donor closure, manual continuous integration dispatch or installed-state action was performed.
