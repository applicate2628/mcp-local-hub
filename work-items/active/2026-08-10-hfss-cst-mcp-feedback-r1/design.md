# Design: HFSS/CST MCP feedback R1

Decision reference: `2026-08-10-stdio-http-session-contract` (proposed).

## Chosen approach

### Shared stdio HTTP transport

`internal/daemon.StdioHost` remains the one owner of the stdio subprocess and becomes the one owner of its HTTP-session registry. A successful initialize response mints one secure session and records the negotiated version. Every later POST, GET, or DELETE gates at this registry before reaching bridge transforms or the subprocess. DELETE removes only the caller's session. The registry is bounded; the oldest session is evicted when the cap is reached.

HTTP failure discriminators:

| Failure | Observable discriminator |
| --- | --- |
| Explicit unsupported protocol version | HTTP 400, `unsupported MCP-Protocol-Version` |
| Missing session on a non-initialize request | HTTP 400, `missing session id` |
| Unknown, evicted, or deleted session | HTTP 404, `unknown session id` |
| Supported version different from the session's negotiated version | HTTP 400, `protocol version does not match session` |
| Session-creation entropy failure | HTTP 500 before returning the initialize result |

### Electromagnetics tool contracts

The Python package uses constrained annotations for exact action enums, local numeric bounds, and CST list cardinalities. A package-local strict FastMCP bootstrap configures its generated argument models to forbid extra keys and fails loudly if the supported MCP 1.x adapter seam is unavailable.

Both solve tools add `preflight`. `start` and `preflight` share one validation function and identical filesystem/settings checks. Only after validation does `start` require `confirm=true` and submit to `JobManager`; `preflight` returns a no-launch validation receipt and has no path to the runner. Existing `status`, `result`, and `cancel` behavior stays unchanged.

### Resource bridge

No change. The current bridge preserves native error envelopes and the restarted runtime reproduces the correct explicit error.

## Alternatives

1. **Omit session IDs and become stateless.** Rejected because the existing hub aggregator intentionally requires daemon session IDs (`internal/api/hub_mcp_aggregator.go:1680-1684`); changing both layers is a wider compatibility break.
2. **Keep one token and only mark it deleted.** Rejected because independent initialize calls would still not be isolated or unique.
3. **Validate unknown Python fields inside each solve function.** Rejected because FastMCP/Pydantic discards extras before the function is called; the owning adapter model must reject them.

## Change-Surface Contract

- **intended change surface:** `internal/daemon/host.go`, its focused tests, the electromagnetics package's FastMCP bootstrap, HFSS/CST solve functions, job action vocabulary, and focused Python contract tests.
- **approved extension seams:** `StdioHost.HTTPHandler` session/version gate; FastMCP argument-model configuration before tool registration; shared solve preflight validators.
- **protected / must-not-touch surfaces:** `HTTPHost` native-HTTP forwarding, hub client-facing session store and aggregator routing, solver runner scripts, project/export formats, manifests until publication, all unrelated daemons' application behavior.
- **declared blast radius:** HTTP lifecycle behavior of every stdio-bridge daemon; tool schema and preflight behavior of HFSS/CST only.

## Stable contracts and migration

- External transport behavior is corrected in one release: compliant initialize→session clients continue unchanged; noncompliant no-initialize clients receive explicit errors. No persisted-state migration exists; adapter restart is rollback.
- Existing action values `start`, `status`, `result`, and `cancel` remain valid. `preflight` is additive.
- Tool names, job IDs, result formats, artifact schemas, and solver launch configuration remain unchanged.

## Diff-invisible invariants

| Invariant | Named regression guard |
| --- | --- |
| Multiple HTTP clients still share one stdio child and cached initialize capability response. | `TestHostInitializeCached` plus a new two-session test: one upstream initialize, two distinct HTTP session IDs. |
| A notification remains HTTP 202 and reaches the child only after session/version gates. | New initialized-notification session test; existing notification tests updated to initialize first. |
| Native-HTTP daemons preserve their upstream-owned sessions and DELETE forwarding. | Existing `internal/daemon/http_host_test.go` suite passes unchanged. |
| Invalid/preflight solve requests never submit a job or spawn a solver. | New HFSS/CST tests inject/inspect the job manager and assert unchanged job count for every failure and preflight row. |
| Missing-resource errors remain causal on both native and synthetic surfaces. | Existing success-mapping unit test plus native and synthetic missing-URI passthrough tests. |

## Claims

1. `{ guarantee: every successful stdio-bridge initialize returns a distinct secure session and a deleted session is unusable; single-owner: StdioHost session registry; enforcement-probe: focused two-initialize/delete/after-delete HTTP test }`.
2. `{ guarantee: an explicit unsupported or session-mismatched MCP protocol version is rejected before subprocess dispatch; single-owner: StdioHost request gate; enforcement-probe: test child write counter remains unchanged and HTTP status is 400 }`.
3. `{ guarantee: session memory is bounded and deterministic under abandoned clients; single-owner: StdioHost registry cap/oldest eviction; enforcement-probe: cap+1 test asserts size cap and first session becomes 404 }`.
4. `{ guarantee: HFSS/CST action schemas publish exact enum values including preflight and descriptions match runtime; single-owner: constrained solve signatures; enforcement-probe: tools/list schema assertions for both servers }`.
5. `{ guarantee: unknown HFSS/CST arguments are rejected before tool execution; single-owner: strict FastMCP argument model; enforcement-probe: extra-field tests assert isError and no job lookup/start }`.
6. `{ guarantee: local numeric and array constraints are published and cross-field validation runs before confirmation; single-owner: constrained annotations plus shared solve validator; enforcement-probe: schema assertions and confirm=false invalid-bound tests }`.
7. `{ guarantee: preflight performs complete validation and can never submit a job; single-owner: HFSS/CST solve dispatch ordering; enforcement-probe: valid preflight receipt with unchanged JobManager inventory }`.
8. `{ guarantee: current missing-resource error behavior is preserved without a speculative patch; single-owner: mapReadResourceResult error passthrough; enforcement-probe: native and synthetic missing-resource tests return the causal error }`.
9. `{ guarantee: native-HTTP forwarding and hub session/routing code are unchanged; single-owner: Change-Surface Contract; enforcement-probe: zero diff in internal/daemon/http_host.go and internal/api plus existing focused suites }`.

## Test strategy

1. Add failing Go transport tests and Python schema/preflight tests before production edits.
2. Run focused normal and race tests for `internal/daemon`; run the full electromagnetics pytest and Ruff suites.
3. Run affected Go sibling suites (`internal/api`, `internal/daemon`) and publication/static checks.
4. Build an isolated `mcphub` binary and launch temporary HFSS/CST daemon endpoints on unused ports; repeat the safe matrix.
5. Only after isolated PASS, upgrade/restart the two live daemons and verify availability. A real solver job remains a separate explicit-approval gate.

## Gate

PASS
