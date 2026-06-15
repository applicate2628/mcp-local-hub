# Dynamic MCP discovery — see ALL installed servers, flag hub-managed separately

Started: 2026-06-15 · Source: live-GUI UX review (user)

## Vision (user, verbatim intent)
- **"видеть нужно все"** — the GUI must show EVERY installed MCP server across all
  client configs, not just the curated/known set.
- **"managed отдельно оповещать"** — clearly flag which servers are hub-managed
  (routed through mcp-local-hub) vs unmanaged/external, as a separate signal.
- **"maximum dynamic, меньше hardcoded"** — discovery must be dynamic (enumerate
  whatever clients + servers actually exist on the host), NOT gated by a
  hardcoded client list.
- **"наверное нужен какой-то автоскан"** — an auto-scan (re-scan on change / on a
  schedule), not only a manual scan.

## Evidence (captured 2026-06-15, live host)
- Config truth: `~/.claude.json` = **26** MCP servers; `~/.codex/config.toml` =
  **~28** (incl. external non-hub: context7, exa, playwright, mui-mcp, repomix,
  raindrop, stgen-dxf-viewer, qt-docs).
- Live `GET /api/scan` = **32 entries** (12 via-hub, 8 can-migrate, **7 unknown**,
  4 not-installed, 1 per-session). The "unknown" (external/other) bucket shows
  only 7: clangd, codegraph, mui-mcp, raindrop, repomix, stgen-dxf-viewer,
  time-server. MISSING vs configs: context7, exa, playwright, qt-docs.
- Root causes of the gap:
  1. The scan reads a **hardcoded 15-client list** (CORE_CLIENTS + WAVE2_CLIENTS,
     internal/gui/frontend/src/lib/routing.ts + the Go-side client registry) —
     servers in clients outside that list, or in non-standard config paths, are
     invisible.
  2. **remote-http** entries (context7, qt-docs) aren't surfaced as "unmanaged"
     in the unknown bucket the way stdio entries are.
  3. The Migration "unknown" group filters out **dismissed** entries
     (groupMigrationEntries drops dismissedUnknown), so previously-dismissed
     externals vanish from the view.
- **Stale/duplicate entries** confuse the picture: `clangd`, `time-server`,
  `fetch` appear both as hub-managed AND as a separate stale/duplicate row → see
  the fetch-checkbox bug below.

## Linked bugs (same surface)
- **fetch checkbox doesn't uncheck after Apply** (user #4): `fetch` is classified
  `via-hub` (status) AND has a duplicate/stale config entry. After a demigrate
  Apply, the hub-routed binding is removed but the stale direct entry remains, so
  the matrix cell stays checked. The matrix must reconcile per-(server,client)
  against the ACTUAL post-Apply config, deduping hub-routed vs stale-direct.
- **"Columns" label unclear** (user #5) — FIXED this session: renamed the matrix
  column-visibility control "Columns (N/15)" → "Clients (N/15)" + clearer popover
  ("Show / hide clients").

## Proposed design (phased — DRAFT, pending user confirm on surface)
- **P1 — dynamic client discovery (backend):** replace the hardcoded 15-client
  enumeration in the scan with dynamic discovery — enumerate every client config
  the host actually has (probe each known adapter's config path AND any extra
  configs found), and surface EVERY server entry (stdio + remote-http) with its
  source client(s). Keep the canonical client registry as a *label/known-set*,
  not a *filter*.
- **P2 — "managed" flag:** every scanned server carries an explicit
  managed-by-hub boolean (routed through a hub daemon / has a hub manifest) so
  the UI can badge it. Dedup hub-routed vs stale-direct duplicates by
  (server,client) so fetch-style ghosts collapse.
- **P3 — surface (UI):** show ALL discovered servers with a clear managed/
  unmanaged badge. CANDIDATE HOMES (confirm with user): (A) extend the Servers
  matrix to list every discovered server row with a "managed" badge + dynamic
  client columns; (B) a dedicated "All MCP" / "Discovered" screen grouped by
  managed/unmanaged; (C) enhance the existing Migration screen groups (rename
  "unknown" → "unmanaged/external", stop hiding dismissed, include remote-http).
- **P4 — auto-scan:** re-scan on client-config change (fsnotify) and/or a light
  poll, so the view stays live without a manual refresh.
- **P5 — fetch dedup fix:** reconcile the matrix against actual post-Apply config
  so a demigrate'd cell unchecks even when a stale direct entry lingers.

## Open decisions (for user)
1. Surface for "see all": A (Servers matrix), B (new screen), or C (enhance
   Migration)? — recommendation pending.
2. Priority vs the rest of §B (Store is done; this is a new top ask).

## Status
DRAFT captured. Columns-label fix done. Awaiting user confirm on P3 surface +
priority before building P1-P5.

## P5 root cause CONFIRMED (fetch checkbox)
Not a stale duplicate (configs have ONE clean `fetch` = http://localhost:9133/mcp).
Root: `internal/api/demigrate.go` — `allowURLBackfill = len(backups) > 0 || sawLegacy`.
A via-hub server added as http-from-start (never migrated from stdio) has NO
backup, so the URL-backfill RemoveEntry fails closed → the entry stays → the
matrix cell stays checked. This is the demigrate-fallback bug
(work-items/bugs/2026-05-15-demigrate-fallback-when-no-pre-hub-form.md, §B
"product decision"). User's product decision (implied by calling it a flaw):
a via-hub entry whose URL is a hub URL (clients.IsHubHTTPURL) is provably
hub-managed → demigrate may remove it without a backup. FIX: extend
allowURLBackfill (or tryMarkerOrBackfillRemove) to treat an IsHubHTTPURL match
as sufficient corroboration. Sensitive demigrate-correctness change → do with
review.

## REFINED root (codegraph scan.go) — 2026-06-16
The missing servers are NOT primarily the hardcoded-15 (ScanFrom DOES read
claude-code + codex-cli — where the user's servers live). The real root is
`internal/api/scan.go:classify()`:
- A non-hub http/sse entry (Transport http/sse, NOT clients.IsHubHTTPURL) hits
  `!hasHub && !hasStdio` → returns **"not-installed"** → migration-grouping.ts
  drops "not-installed" → context7 (mcp.context7.com), qt-docs (qt.io), and any
  external remote MCP VANISH from the view. THIS is "скан не видит все".
- via-hub (http + IsHubHTTPURL) is the only "managed" signal; there's no explicit
  Managed flag on ScanEntry.

REFINED design (surface C = Migration→Discovery):
- **P1 classify:** a non-hub http/sse entry → a new status `external` (unmanaged
  remote), surfaced — NOT "not-installed". stdio-no-manifest stays `unknown`
  (also unmanaged-external). Keep `not-installed` ONLY for manifest-only rows
  with zero client presence.
- **P2 managed flag:** add `Managed bool` to ScanEntry (true = via-hub /
  hub-routed). Thread to the DTO + frontend types.
- **P3 Discovery screen:** rename Migration → Discovery; render ALL groups —
  Managed (via-hub), Migratable (can-migrate), Unmanaged/External (unknown +
  external remote), Per-session; STOP hiding dismissed (show a collapsed
  "dismissed" section). Badge managed-by-hub.
- **P4 auto-scan:** fsnotify on the 15 client config paths → re-scan + SSE push.
- 15-client enumeration stays (it covers the real clients); truly-unknown
  clients (beyond 15) = a later generic-config-discovery follow-up, NOT the
  current gap.

## Linked bug: marketplace offers already-installed server as "fetch-2" (2026-06-16)
`fetch` is BOTH a shipped hub daemon (port 9133, running) AND a marketplace
catalog entry (marketplace/v1/catalog.json). The Catalog.tsx marketplace-browse
section does NOT cross-reference `installedServers` (it has the set from
/api/status but applies it only to the SHIPPED section), so it offers to install
fetch → my Store NAME_CONFLICT 409 → suggested_name "fetch-2". Absurd: fetch is
already installed. SAME managed-awareness gap.
FIX (fold into P2 managed-flag): the marketplace browse must mark an entry whose
id/name is already installed (in installedServers / managed set) as "installed"
(no install affordance, or an "already installed" badge) — never offer re-install
of a running server. Also consider: should the marketplace catalog even list
servers that ship with the hub? (curate the catalog OR rely on the installed-mark
— the installed-mark is the robust fix.)

### DELIVERED 2026-06-16 (commit 2538151, master, pushed+deployed+live-verified)
- Catalog marketplace browse now cross-references installedServers (id OR name);
  an installed entry renders a green "installed" lsp-chip-via-hub badge
  (testid catalog-marketplace-installed-badge-<id>) and the install affordance is
  suppressed (installed branch takes priority over the whole hub/direct/conflict
  block). Live: `fetch` marketplace card shows "installed", zero install buttons —
  fetch-2 absurdity gone.
- Also landed (same commit, user's "описания → tooltip, вкладки ровные" ask):
  inline card prose (shipped catalog-desc-<name> + marketplace summary) moved into
  ⓘ InfoTip tooltips next to each title → compact uniform cards across tabs.
  InfoTip forwards data-testid + mirrors text onto trigger title for coverage.
- Verify: typecheck clean, vitest 560/560 (Catalog 22/22 + 1 new Part-A test),
  go build ./... + vet clean, bundle regenerated, 27/27 MCP Connected post-deploy,
  Playwright live-verified (installed badge + tooltips + uniform cards).
