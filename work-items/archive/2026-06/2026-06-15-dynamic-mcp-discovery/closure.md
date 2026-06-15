# Closure — Dynamic MCP discovery (see ALL installed servers, flag managed separately)

Closed: 2026-06-16

## Outcome — DELIVERED

The user's UX-burst vision ("видеть нужно все, managed отдельно оповещать,
maximum dynamic, наверное нужен автоскан") is delivered across five shipped +
deployed + live-verified commits:

- **Discovery view + external classify (1df59a4):** `classify()` now returns a new
  `external` status for client-present non-hub remote HTTP/SSE entries (was dropped
  as `not-installed` → invisible). `ScanEntry.Managed` bool (= `Status=="via-hub"`,
  single owner). Migration screen → DiscoveryScreen rendering Managed-by-hub /
  Ready-to-migrate / Unmanaged-External / Per-session / collapsed Dismissed groups.
  Live: externals context7/qt-docs/exa now surface.
- **fetch checkbox / demigrate hub-URL relax (269bd15) [P5]:** a via-hub hub-URL
  entry with no backup is now demigrate-removable (IsHubHTTPURL corroboration);
  restore-wins + non-hub fail-closed preserved.
- **"Columns" → "Clients" matrix control rename (a2d13f3).**
- **Marketplace mark-installed + card descriptions → InfoTip tooltips (2538151):**
  an already-installed server (e.g. shipped `fetch`) shows an "installed" badge
  instead of offering install → fetch-2; inline card prose moved to ⓘ tooltips for
  uniform compact cards.
- **Auto-scan [P4] (3e03188):** light `useAutoScan` poll-while-mounted (10s,
  visibility-paused) + shared `ScanRefreshControls` ("Rescan now" + "updated Ns
  ago") on Discovery + Servers. Servers matrix PAUSES auto-refresh + disables
  Rescan while dirty (3 independent no-clobber defenses) — live-verified.

## Verification
- typecheck clean; vitest 577/577; go build ./... + go vet ./internal/gui/ clean.
- Deployed each step (build.sh → rename-aside swap → supervisor+GUI restart);
  27–28/28 MCP Connected post-deploy.
- Playwright live-verified: Discovery externals + managed badges; marketplace
  installed badge (no fetch-2); uniform tooltip cards; auto-scan controls +
  dirty-pause no-clobber on the Servers matrix.

## Residual risk / deferred
- **P1 — dynamic client discovery beyond the hardcoded ~17-client enumeration**
  (enumerate ANY client config on the host, not just the known set) is the one
  unbuilt P-item. It was NOT the real "scan misses MCP" root (that was `classify()`
  dropping remote-http, now fixed); P1 is a genuine future enhancement. → ROADMAP.
- **fsnotify-instant auto-scan** (backend watcher pushing updates even to an
  already-open screen) was the rejected heavier alternative; the light poll covers
  the real case. Upgrade path if poll-while-open proves insufficient. → ROADMAP.

## Archive location
work-items/archive/2026-06/2026-06-15-dynamic-mcp-discovery/
