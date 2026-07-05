---
status: open
severity: medium
filed: 2026-07-03
context: deep-audit finding (multi-agent audit, hub-aggregator × wire lens, CONFIRMED by 1 verifier before session-limit cut the rest)
---

# Hub tools/list fan-out ignores MCP pagination (result.nextCursor) — a paginating daemon's later tools are silently dropped and unroutable

## Finding

`postToolsList` (`internal/api/hub_mcp_aggregator.go:1618`) sends a hard-coded single request `{"jsonrpc":"2.0","id":2,"method":"tools/list"}` with **no** `params.cursor`, parses only `result.tools` (the response struct at ~:1634-1642 has no `nextCursor` field), and is called exactly once per daemon (only callee is `doDaemonPost` — no pagination loop, no cursor threading).

Per the MCP spec, `ListToolsResult` carries an optional `nextCursor`; a server returning a bounded first page + a cursor is fully spec-compliant. The hub consumes only page 1 and discards the cursor. Because the hub's own client-facing `tools/list` response (`buildToolsListResponse`) also emits no `nextCursor`, the external MCP client (Claude/codex) has no way to request the remaining pages either — so page-2+ tools are permanently invisible AND their names are absent from the session RouteMap, making a later `tools/call` for them return `-32601 "Method not found"`.

## Failure scenario

An operator adds (marketplace/custom manifest) any MCP server whose `tools/list` paginates — e.g. 50 tools + `nextCursor:"p2"` on page 1, the rest on page 2. The hub reads the 50 first-page tools, drops the cursor, merges only those. All page-2+ tools never appear in the aggregated `tools/list` the client sees; any `tools/call` against one of those names → no RouteMap entry → `-32601`. **Silent** (no partialFailures row, no error).

## Suggested fix

In `postToolsList`, loop on `result.nextCursor`: add a `nextCursor` field to the response struct, and while it is non-empty, re-request with `params:{cursor:<nextCursor>}`, accumulating `result.tools` across pages (bound the loop with a sane page cap + the existing per-daemon timeout to avoid a hostile daemon paginating forever). No client-facing `nextCursor` needed as long as the hub fully drains server-side pages before building the aggregated list.

## Note

Verification was partial — the session hit the token limit mid-audit; the correctness-lens verifier CONFIRMED the factual claims (single hard-coded request, no cursor param, no nextCursor field, single callee). Re-confirm reachability (does any currently-cataloged server actually paginate?) before prioritizing — impact is real but conditional on a paginating backend.
