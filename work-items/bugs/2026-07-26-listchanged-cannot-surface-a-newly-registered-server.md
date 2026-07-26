---
title: "A server registered after a client session starts never appears in it — listChanged cannot cover that, so this is a documentation fix, not a code fix"
status: answered-documentation-only
severity: P3
date: 2026-07-26
found-by: operator (field report, driving vcpkg-mcp 0.1.0 against their real vcpkg tree), item I1
answered-by: lead (SDK source probe at the built version)
---

## The observation

`vcpkg-mcp` advertises `"tools": {"listChanged": true}` in its capabilities, yet a server installed
after a client session starts never appears in that session — every call in the operator's field
report had to go through a hand-rolled JSON-RPC client. The question raised was whether to document
"restart the client after install", or to check whether the change-notification path is reachable at
all for a newly-registered server.

## Answer: the path IS reachable and IS implemented. It simply cannot cover this case.

Verified against the installed SDK source at the version this module builds
(`github.com/modelcontextprotocol/go-sdk v1.6.1`), not inferred:

1. **The `true` is not ours.** `listChanged` appears nowhere in `internal/vcpkgmcp/` — no declaration,
   no send site. The SDK sets it by default the moment any tool is registered
   (`mcp/server.go:576` → `caps.Tools = &ToolCapabilities{ListChanged: true}`). For contrast, the
   hub's own serena router advertises the honest `listChanged: false` (confirmed by a live
   `initialize` probe against `127.0.0.1:9125/serena/mcp`).
2. **The capability is NOT a false advertisement.** The SDK genuinely emits
   `notifications/tools/list_changed` — from `AddTool` (`mcp/server.go:282`) and `RemoveTool`
   (`:514`), both via `changeAndNotify`. A server that adds or removes a tool at runtime really does
   notify its connected clients.
3. **But that is a different event.** `listChanged` tells an ALREADY-CONNECTED client that the tool
   list *on that connection* changed. A server that did not exist when the client session started has
   no connection at all — there is no peer to notify and no session to notify it on. No amount of
   work on the notification path can surface a server the client has never dialled.

Surfacing a NEWLY-REGISTERED server is a client-side concern: the client must re-read its server
config and dial the new entry. That is a separate mechanism from `listChanged`, and it is not
something this server can trigger.

## Consequence

Documentation, not code. Note in the install path / README that a client session started before the
server was registered will not see it, and the client must be restarted (or its MCP surface
reloaded).

Do NOT "fix" this by suppressing the SDK's default `listChanged: true` — the capability is truthful
for what it actually covers, and turning it off would break the case it does serve.

## Same class, observed independently

This session's own Claude Code instance had `serena` correctly registered in the client config
(`http://127.0.0.1:9125/serena/mcp`) and the router answering `initialize` normally, yet serena was
absent from the session's tool surface for the entire session. Same cause: the connection was not
established at session start (the fleet was mid-restart during a deploy), and nothing re-dials
mid-session. Worth citing in the documentation as the general behaviour rather than a vcpkg-specific
quirk.
