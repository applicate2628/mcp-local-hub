# Auto-reaper covers client-direct MCP servers (ScanClientConfigs in the automatic path)

Template: full-delivery (security-sensitive) · Lead: main conversation · Opened: 2026-07-19
Bug: work-items/bugs/2026-07-19-auto-reaper-misses-client-direct-mcp-servers.md
Cross-ref: recurrence of work-items/bugs/2026-07-04-npx-stdio-mcp-orphan-accumulation-bypasses-hub.md
Operator directive (2026-07-04, verbatim): "хаб ОБЯЗАН ВБИРАТЬ ТАКИЕ СЛУЧАИ В СЕБЯ".

## Goal (Rank-2 per $analyst)
Wire the EXISTING A6 `ScanClientConfigs=true` into the AUTOMATIC orphan-reaper path
(`internal/gui/cleanup.go:143` CleanupOpts, driven by the 5-min ticker `gui_cleanup_ticker.go:88`).
Reuses the shipped config-absence gate + identity-verified kill (`TerminatePIDWithIdentity`) + 600s age
floor — SCOPE change, not new authority. Makes the automatic sweep reap TRUE (dead-parent) orphans of ANY
client-direct MCP server (codegraph/chrome-devtools/next-devtools) without requiring adoption.

## Security-sensitive (mandatory security-reviewer gate)
`internal/api/cleanup.go` has 4 hardening rounds in one month (#511/#520/#521/#522). Any change re-runs the
security commission. The reaper KILLS processes it did not spawn → false-kill risk. Must preserve: the
config-absence gate (a signature still referenced by any client config → SPARE), identity kill, 600s age
floor, generic-token/bare-name suppression (scan.go:980-1007).

## Scope boundary
Rank-2 (auto A6, dead-parent orphans) ONLY. NOT Rank-3 (auto-aggressive live-rooted dups — separate item,
needs a safe dup-detector + its own security gate). NOT the IPC-poll-flood item (different surface).

## Stage log
| Stage | Owner | Status |
|---|---|---|
| Prioritization | $product-manager (via incident) | admitted P1/HIGH |
| Analysis | $analyst | PASS (memo in bug doc: gap = ScanClientConfigs=false in auto path) |
| Design | $architect | dispatched |

## Design — $architect PASS with CRUCIAL efficacy correction (2026-07-19)
**Rank-2 (ScanClientConfigs flip) is INERT for the incident's felt lag.** The config-absence gate spares by
config-SIGNATURE match (not parent liveness), and there is a nomination⊆spare symmetry: any process nominated
because a present config entry exists is SPARED by that same config's inclusive scan (`cleanup.go:642 strict
⊆ :788 inclusive`). So the fix reaps ONLY config-ABSENT orphans (migrated-out/deleted/uninstalled entries).
The incident's codegraph/chrome-devtools/next-devtools are STILL CONFIGURED → nominated then spared
(`ReapVerdictSparedConfigReferenced`). Rank-2 closes a DIFFERENT real gap (config-absent dead-parent orphans,
e.g. post-migrate mcp-language-server leftovers) but does NOT clear the operator's ~300 config-present dups.

**Security: LOW risk.** The same symmetry proves NO over-kill of a currently-configured server; the fix only
widens nomination into the config-absent set the 4-round-hardened gate (#511/#520/#521/#522) filters fail-closed.
Precise auto-kill predicate: "Windows, dead-parent, config-ABSENT (absent from every readable client config),
>600s, PID-identity-verified at kill". Degraded/unreadable config OR truncated snapshot → spare ALL (fail-closed).

**Change (additive, minimal):** add `scan_clients bool` to `cleanupRequest` (internal/gui/cleanup.go:41-54),
set `ScanClientConfigs: req.ScanClients` (gui/cleanup.go:143), auto-ticker body → `{"apply":true,"scan_clients":true}`
(gui_cleanup_ticker.go:126). Zero backend-logic edits. Rejected the unconditional flip (changes manual button UX).

**Phase 1 = EFFICACY PROBE (evidence gate):** `mcphub cleanup --scan-clients --dry-run` on the accumulated host →
confirm codegraph/chrome-devtools/next-devtools show "skip: still referenced by a client config" (Rank-2 inert
for them, expected). Ship Rank-2 WITHOUT promising the felt lag disappears. Mandatory $security-reviewer gate.

**The operator's felt lag needs Rank-1 (adopt → dedup into hub, today) OR Rank-3 (auto-aggressive, separate
security-gated item) — NOT Rank-2.**

Stage: Design PASS → Phase-1 efficacy probe → implement → MANDATORY security-reviewer → PR.
