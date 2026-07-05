# Decision: LSP probation — await-after-delivery for response-needed forwards

- **status:** proposed
- **filed:** 2026-07-02
- **context:** LSP lazy-proxy cold-materialize probation (P2c, PR #489); architect design `.reports/2026-07/` (aa385669 design package) resolving a 2-round Codex-bot concurrency edge-mine.
- **gate:** `$architecture-reviewer` promotes proposed→accepted; `$reliability-engineer` reviews the timeout/SLO posture BEFORE predicate-1 implementation.

## Decision

The LSP-proxy probation fast-fail (503 + Retry-After "cold start; retry") applies to a forwarded send **iff the client needs no response from it** (a notification: `textDocument/didOpen`/`didClose`). A send whose response the client needs (`tools/call`, generic `handleForward` requests) is **awaited under the client's own context and is NEVER fast-failed after delivery** — the probation `MaterializeWaitBudget` is removed from `handleToolsCall`/`handleForward`.

## Why

`StdioHost.SendRPC` writes the request to the backend stdin BEFORE selecting on ctx, so a probation-budget `DeadlineExceeded` means the request was already delivered. Returning a 503 then causes the client to retry a **fresh** request (new id, same method+params, forwarded verbatim by the GUI LSP router) → **duplicate execution of a side-effecting tool** (`edit_file`, `rename_symbol`, ...). Round-1 fixed this for notifications (202-delivered); this decision extends the same "delivered ⇒ don't retry" distinction to requests, uniformly, via the response-need predicate. Rejected alternatives (recorded in the architect package): SendRPC Deliver/Await split (fragile method+params re-await dedup), internal readiness-probe gating warmed (no probe that is both non-side-effecting and index-gated), per-tool idempotency allowlist (not uniform; brittle across backends).

## Tradeoff (the thing $reliability-engineer must sign off)

The first cold `tools/call` now holds one connection up to the client/router bound (`serenaUpstreamTimeout = 60s`) instead of 503-ing at 15s. gopls index is ~35-45s (inside 60s), so the common case returns the real result on the first call rather than forcing a client retry. Residual risks (architect §6 self-critique): (1) indexing >60s → router 504 → a 504-retrying client can still duplicate (much smaller window than today's 15s-503; mitigation to consider: proxy awaits just under 60s and returns a NON-retryable JSON-RPC error on its own bound); (2) `reapWedgedProbation` branch A does not skip on in-flight requests — unreachable while the 60s router bound < 5m `ColdStartMaxProbation`, but couples if an operator raises client timeout above 5m (document `ColdStartMaxProbation ≥ client-timeout` or make branch A skip progressing in-flight requests).

## Scope

`internal/daemon/lazy_proxy.go` handler layer only. No wire-shape/schema/client-config change. One existing probation test's contract changes (first cold tools/call no longer 503s). Notifications' 202-delivered behavior is retained verbatim.

## $reliability-engineer verdict: REVISE → 8 MUST-DO constraints (folded into the accepted design)

Direction confirmed net-better, but the naive "remove the budget, await under client ctx" under-specifies the bound. Accepted refinements:

1. Remove ONLY the post-handshake SendRequest bound (`boundedCallCtx`). KEEP the materialize-phase `DoBounded`/`ErrMaterializeInFlight`→503 — it is PRE-delivery (backend not yet written to) and therefore retry-SAFE.
2. Add `ColdRequestHoldCeiling` (≥ worst-case cold index of the SLOWEST fronted backend — rust-analyzer/large-TS/clangd via mcp-language-server ROUTINELY exceed 60s; gopls-only optimism is wrong — and strictly < `ColdStartMaxProbation`). On ceiling expiry return a NON-retryable controlled JSON-RPC error (NO "retry" wording, NO Retry-After — agents auto-retry on retry-text → duplicate). This ceiling also bounds the direct-per-daemon-port client (no-timeout codex client would else hang to the 5m watchdog).
3. Decouple the LSP-forward upstream timeout from `serenaUpstreamTimeout`; size it so the proxy ceiling fires BEFORE the router timeout (client sees the controlled error, never a raw 504).
4. Enforce the timeout-ordering invariant at config load: `ColdStartMaxProbation > LSP-forward-upstream-timeout > ColdRequestHoldCeiling > MaterializeWaitBudget`. Do NOT make `reapWedgedProbation` Branch A skip in-flight requests (no wedged-vs-progressing signal → reintroduces wedge-forever); the invariant is the fix.
5. Slow-forward observability event REQUIRED (not deferred) — the only fleet signal for a 40-60s hold once the 15s-503 is gone.
6. Remove the dead `isProbationDeadline`→503 branch from the request handlers (C6 residue); keep it notification-only.
7. Update the CLAUDE.md 503-warming contract doc (false for the request path now).
8. **SLO (stated):** cold first LSP `tools/call` first-byte p50 ≈ backend cold-index, p99 ≤ `ColdRequestHoldCeiling`, beyond which a controlled non-retryable error — never a silent hang, never a raw 504. Notifications unchanged.
