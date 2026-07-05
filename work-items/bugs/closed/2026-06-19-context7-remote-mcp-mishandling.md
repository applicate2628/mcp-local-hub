---
status: closed
---

# context7 (remote MCP) mishandled by the hub

> **Closed 2026-07-05 (triage).** The main path WORKS: the GUI marketplace
> installs remote-HTTP entries (context7) — `internal/gui/marketplace_install.go`
> handles the remote-http draft (no daemons block, name re-marshal). Residual is a
> CLI-only parity gap: `mcphub marketplace generate <http-entry>` still emits the
> G6-deferral refusal (`internal/cli/marketplace.go:287`) instead of a remote
> draft. LOW-priority follow-up (CLI users have the documented workaround: add the
> remote server directly in their client). Filed for awareness; not blocking.

Operator: "context7 обычно устанавливается вендорами нативно, хаб его криво
подхватывает или неправильно инсталлит."

## What context7 IS (diagnosis)

A **remote HTTP** MCP server hosted by Upstash at `https://mcp.context7.com/mcp`
(marketplace catalog: `transport: "http"`, `url: mcp.context7.com/mcp`, no
env, no local command). It is NOT a local stdio server — mcphub's model is
local daemons it spawns + supervises. A remote HTTP server cannot be a hub
daemon until **G6 (Remote MCP manifests, v0.4.x)** ships.

context7 can also be wired in a client as STDIO: `command: npx, args: [-y,
@upstash/context7-mcp]` (a vendor npm package), which IS local but is the
vendor's package, not a hub-managed server (no `servers/context7/manifest.yaml`
exists — context7 is catalog-only).

## Two candidate symptoms (need the operator's exact repro to fix precisely)

**(A) Catalog lists it but install refuses ("неправильно инсталлит").**
The marketplace catalog SHOWS context7, but `mcphub marketplace generate
context7` REFUSES `transport: http` with the documented G6-deferral error
(CLAUDE.md "Marketplace"). So it appears installable but isn't — confusing UX.
Fix direction: mark remote/http catalog entries clearly as "remote — add it in
your client directly; the hub can't proxy remote MCPs until G6", and disable
the GUI Install affordance for them (link to the vendor instead).

**(B) Scan mis-handles a vendor-configured context7 ("криво подхватывает").**
- Remote variant (`url: mcp.context7.com`): scan classifies it `external`
  (read-only, Unmanaged group) — `scan.go:1480`. This looks CORRECT; verify
  the GUI surfaces it read-only and never offers to migrate/overwrite it.
- Stdio variant (`npx @upstash/context7-mcp`): `hasStdio=true` and
  `manifestNames["context7"]=false` (catalog-only, no installed manifest) →
  classified `"unknown"` (`scan.go:1472`). If the GUI then offers to
  migrate/install an `"unknown"` stdio entry, it would try to hub-manage a
  vendor package with no manifest — the mishandling. Fix direction: an
  `"unknown"` (catalog-known but no local manifest) stdio entry should surface
  read-only as external/vendor-owned, not as a migrate candidate.

## Need from operator (the precise symptom)

1. How is context7 configured in your client — remote `url` or stdio `npx`?
2. What exactly goes wrong — install button errors, scan shows it wrong, the
   hub overwrites/breaks the vendor entry, or it just won't connect?
3. Which client (Cursor / VS Code / Claude / codex)?

Then the fix targets (A) the catalog/install affordance for remote entries, or
(B) the scan classification of catalog-known-but-manifest-less entries.

## 2026-06-19 CORRECTION — fix (A) does NOT exist: the GUI install WORKS

Code-verified in `internal/gui/marketplace_install.go` + `Catalog.tsx`: the
Catalog renders a TWO-TIER install for a `transport: http` entry and BOTH tiers
handle context7 correctly:
- **"Add to hub"** (`mode=hub`, `handleMarketplaceHubInstall`) drafts a
  remote-http manifest (NO daemons block, no port — the hub proxies to
  `mcp.context7.com`). The G6 remote-MCP-manifest path IS implemented for the
  GUI hub-install (only the `marketplace generate` CLI still refuses http).
- **"Install directly"** (`mode=direct`, http-only) writes the remote URL
  straight into the operator-selected client configs (no hub).

So the original fix-(A) framing — "the catalog offers a broken remote install"
— is WRONG; the GUI install works both ways. Residual context7 items:
1. CLI `mcphub marketplace generate context7` still refuses http (G6-deferral)
   — INCONSISTENT with the working GUI; a small CLI fix (point at the GUI/
   direct path) or lift the refusal now that hub-install supports remote.
2. Fix (B) scan: a stdio `npx @upstash/context7-mcp` → "unknown" → offers
   "Create-manifest" for a vendor server (the "криво подхватывает"). Deferred
   (needs catalog-name awareness; non-corrupting — dismissible).
3. A possible RUNTIME hub-proxy issue (auth/headers to context7) — needs the
   operator's exact repro to confirm; the install paths themselves are sound.

Net: install is not broken; the residual is CLI/GUI consistency + the scan
polish + (maybe) a runtime proxy detail pending repro.

## 2026-06-19 refinement (operator: "почини оба")

- **(A) is the CLEAR, real bug — prioritize.** The marketplace catalog ships
  context7 (`transport: http`) and the GUI Catalog renders it, but
  `marketplace generate` / install REFUSES http with the G6-deferral error, so
  the affordance is a dead end. Fix: in the Catalog GUI + CLI, render remote
  (http) entries with a "Remote MCP — add it in your client directly; hub
  proxying lands in G6" note and DISABLE the Install button (link the vendor
  README instead). Backend already refuses; make the refusal a clear,
  up-front, non-actionable state rather than a click-then-error.

- **(B) is messier + arguably already acceptable.** Making the scan recognize a
  stdio `npx @upstash/context7-mcp` as catalog-known would need the scan to load
  the marketplace catalog — which is FETCHED + cached over HTTP, NOT embedded
  (`internal/api/marketplace_cache.go`), so wiring it into the offline,
  hot-path scan classification adds a network/cache dependency. Meanwhile the
  current handling (stdio-no-manifest → `"unknown"` → optional "Create-manifest"
  + Dismiss) is OPT-IN and dismissible: the hub does NOT auto-manage or
  overwrite the vendor entry; the operator parks it with Dismiss. So (B) is a
  polish item (don't offer create-manifest for a catalog-known vendor server),
  NOT a correctness bug, and is gated on either embedding a catalog name-set or
  a lighter "known-vendor-names" list. Defer (B) behind (A).
