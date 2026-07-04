---
status: fixed
severity: low
fixed: 2026-07-03 — branch fix/lsp-notification-detach-202. handleLSPNotification now writes 202 immediately and forwards the notification best-effort in a DETACHED goroutine (cleanupContext-bounded, survives client disconnect); new forwardLSPNotificationDetached logs a hub-mcp.log warn on transport failure / non-2xx but never propagates. Mirrors serena forwardSerenaCancelledUpstream. Test TestLSPRouter_NotificationForwardIsDetached202 (reachable→202+delivered, unreachable→202-not-502; negative-controlled).
filed: 2026-07-03
context: deep-audit finding (multi-agent audit, lsp-router × error-propagation lens; partial verification — session-limit cut the skeptics)
---

# LSP router notification forward returns a transport error status (502/504) or JSON-RPC error envelope to a JSON-RPC notification, violating the documented notifications→202 contract

## Finding

`handleLSPNotification` forwards genuine `notifications/*` (e.g. `notifications/cancelled`, `notifications/progress`) SYNCHRONOUSLY via `forwardLSPToWorkspace`. On a forward failure that function writes a response directly to the client:

- `http.Error(..., 502)` when the upstream proxy is unreachable (`internal/gui/lsp_router.go:812`),
- `http.Error(..., 504)` on `ResponseHeaderTimeout` (`:809`),
- `writeJSONRPCError(w, nil, ...)` at HTTP 200 for empty-URL / build-request failures (`:788`, `:793`).

A JSON-RPC notification must never receive a response, and the documented LSP-router contract is that notifications resolve to **HTTP 202** (CLAUDE.md LSP-router cold-start note: "the notification ... is treated as DELIVERED → HTTP 202"). The sibling **serena** router deliberately avoids exactly this: it writes 202 immediately and forwards `notifications/cancelled` in a **detached** best-effort goroutine (`serena_router.go` ~:2029-2032, "finding 2"), so a transport failure is audited but never propagated to the client. The LSP router lacks that detach.

## Failure scenario

A client with a single-candidate bound session sends `notifications/cancelled` on `/lsp/go/mcp` while the target go LSP proxy daemon is momentarily down (supervisor respawn window / not yet re-bound after a GUI restart). `forwardLSPToWorkspace`'s `httpClient.Do` returns connection-refused → `http.Error(w, "upstream LSP proxy at port N unreachable", 502)`. The client receives HTTP 502 with a plaintext body in response to a JSON-RPC notification instead of the documented 202; a strict streamable-HTTP client can treat a non-2xx on the POST as a transport failure. Secondarily, if the daemon accepts the connection but never sends headers (wedged), the notification POST blocks the client for the full ~150s ResponseHeaderTimeout.

## Suggested fix

Mirror the serena router's pattern: for genuine `notifications/*` on the LSP router, write **202 immediately** and forward in a detached best-effort goroutine (audit any forward failure to `hub-mcp.log`, never propagate a non-202 status or a JSON-RPC error envelope to the client). Requests (`tools/call` etc.) keep their current synchronous error mapping.

## Note

Low severity + partial verification (skeptics did not complete before the session limit). Confirm the exact `handleLSPNotification` classification (which methods are treated as notifications vs requests) and that the 202-detach does not lose delivery guarantees the current sync path provides, before implementing.
