---
status: proposed
date: 2026-06-24
slug: d1-three-catalog-shapes
---

# D-1 — three catalog shapes for desktop-app MCP servers (S1/S2/S3)

## Context

The desktop-app MCP catalog epic (`work-items/epics/2026-06-23-desktop-app-mcp-catalog.md`)
lists MCP servers for the operator's engineering/creative desktop apps. Those servers
arrive in three fundamentally different runtime shapes, and the hub can supervise/own only
some of them. Cataloging them all under one shape (e.g. forcing an OAuth connector into a
remote-http row) would either fail at install or misrepresent the trust/ownership model.
This decision fixes the taxonomy so each Tier row lands in the shape the hub can actually
honor.

## Decision

Catalog rows fall into exactly three shapes, verified against the hub's own code:

- **S1 — local-stdio daemon.** The hub supervises the process under its OS lifecycle
  primitive (Windows Job Object / Linux `PR_SET_PDEATHSIG` / macOS process-group), applies
  the restart-policy state machine, and exposes it over the per-client routes. This is the
  shape for ALL COM / Python / CLI servers (Mathcad, Excel, Ableton, …) and for the
  coding-agent-as-MCP rows (codex `mcp-server`). Catalog `transport: "stdio"` (or
  `native-http` for an HTTP-speaking local daemon like serena); the hub owns the lifecycle.
- **S2 — remote-http bare-URL.** The hub writes `url` + headers into client configs and fans
  out, with NO process to supervise (context7-style). Bound to `remoteHTTPCapableClients`.
  Catalog `transport: "http"` → projected onto a `remote-http` draft.
- **S3 — client-side OAuth connector.** Vendor-hosted connectors (Autodesk Fusion, SketchUp,
  Blender Lab) that the client (claude.ai / Claude Desktop) connects to directly via
  OAuth 2.1 + PKCE. **The hub can neither supervise NOR write these** — they live entirely
  in the client's connector store. Therefore S3 is a **docs-only reference row**: the
  catalog carries a pointer/description, never a `command`/`args`/`url` the hub would try to
  install.

**S3 OAuth connectors are docs-only and are NOT forced into S2.** An OAuth connector is not
a bare-URL remote-http server: it requires the client's own OAuth flow + connector UI, which
the hub's remote-http path (static `url` + `${secret:}` headers) cannot express. Projecting
an S3 connector onto an S2 remote-http row would produce a manifest that installs a URL the
client cannot authenticate against — a false install. S3 stays a documentation pointer until
(if ever) the hub grows a dedicated **connector-docs transport** that carries a vendor +
connector-name + docs-URL with no install side effect. That transport is **deferred to a
future work-item and is explicitly NOT in this PR** (the Tier-1 first batch is S1 only).

## Consequences

- Tier-1 first batch (this PR) is entirely S1: excel, ableton, codex-mcp-server. (mathcad was
  DROPPED before merge — ${workspaceFolder} freeze for a kind:global daemon, unprobed/absent
  server artifact, license pending; deferred to
  work-items/backlog/2026-06-24-mathcad-mcp-row-deferred.md.)
- S2 already exists and is unchanged (context7/qt-docs remote-http rows in v1, carried into
  v2 verbatim).
- S3 (Tier-5: Fusion / Blender Lab / SketchUp) is parked as docs-only; no schema work lands
  until a connector-docs transport is separately designed and approved.
- The `$architecture-reviewer` promotes proposed → accepted.
