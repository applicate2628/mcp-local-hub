# Settings redesign — declutter + on-demand help tooltips

Template: full-delivery (UI). Orchestrator: main conversation.
Started: 2026-06-15. Status: ACTIVE (in progress).

## Goal

User: "в settings всё очень налеплено, нужно навести порядок с дизайном, а
длинные подсказки сделать всплывающими при hover или при нажатии — но не
лепить так всё." Declutter the Settings screen and move long inline help text
into on-demand ⓘ popovers.

## Decisions

- **Stack:** user authorized shadcn / magicui / Tailwind. Took **Tailwind v4**
  (Vite plugin, preflight OMITTED so the other 6 screens + 618 tests are
  untouched; app tokens re-exported via @theme). Popover written **natively**
  (Preact + ARIA `InfoTip`), NOT Radix/Framer — those need preact/compat shims
  with real portal/ref risk; a tooltip is simple enough to own.
- Prototype-first: redesigned Daemons section, showed user, user said
  "подкрути стиль сначала" (refine before rollout).

## Done

- Tailwind v4 wired (vite.config + selective import, no preflight).
- `InfoTip.tsx` — native accessible popover (hover + focus + click, Esc,
  click-outside).
- `SectionDaemons.tsx` redesigned: card + grouped rows + ⓘ tooltips +
  `.field-ctl` inputs. 515/515 tests green, typecheck clean.

## Open tasks (in priority order)

1. **[DONE] Tooltip flash bug** — decoupled X-centering (static `.infotip-anchor`)
   from the animation (opacity + translateY only) + `animation-fill-mode: both`
   so frame 0 is invisible during the w-max width-settle. (verify visually on deploy)
2. **[DONE] Cramped sub-sections** — WeeklyMembershipTable got real table CSS
   (cell padding, header underline, row dividers, small-caps header);
   DaemonEnvSettings rebuilt ("скомканный" → `.daemon-env-body/-kv/-field`, task
   name on its own muted line, balanced KV grid, spaced actions). 6 env tests green.
3. **[DONE] Style refinements** — capped `.settings-body` to 880px (label↔control
   proximity). (dark theme: pending final verify)
4. **[in progress — ultracode Workflow] Roll out** the card+InfoTip style to the
   other 6 sections (Appearance, GUI server, Backups, Trusted Roots, Maintenance,
   Advanced). Run `settings-redesign-rollout` workflow: 6 parallel frontend-engineer
   agents (own file each) + a typecheck/test verify phase.
5. Rebuild bundle, run full tests + e2e, verify dark theme, deploy, live-verify.

## [DONE] Dark-theme card elevation fix

Dark cards used `bg-app-bg` = `var(--bg)` = #0d1117 = exact page color → flat/invisible.
Added `--card-surface` token (light #fff = unchanged, dark #161b22 elevated = sidebar
surface) + `--color-app-card` in @theme; replaced `bg-app-bg`→`bg-app-card` in all 8
cards + InfoTip popover. Verified numerically (card #161b22 vs page #0d1117, elevated:true)
+ screenshot. typecheck + build clean.

## UX-audit findings (workflow #1 — DONE, 15 findings) — FIX BATCH

P1 (fix now):
- **Backups helperText lies**: "No files are deleted from this screen" but Clean buttons
  DELETE — fix BACKUPS_COPY.helperText + its locked test; also delete dead
  BACKUPS_COPY.cleanTooltip "coming in A4-b" + frozen assertion.
- **Restart-required without button**: gui_server port + hub_endpoint show "Restart required"
  badge, no button → add shared "Restart now" wired to existing POST /restart (Secrets uses it).
  + Advanced autorun → "Restart supervisor now" via POST /api/supervisor/restart (Dashboard uses it).
- **default_home**: free-text path → add placeholder ('e.g. C:\dev or /home/you') + folder picker
  (Browse… via OS dialog, mirror advanced.open_app_data_folder spawn). Reuse for Trusted Roots
  + path-typed daemon-env values.
- **Unimplemented shown active**: appearance.default_home + appearance.shell are "used by future
  launches" with NO consumer → set Deferred:true (disabled + "coming soon" badge, the tray pattern).
P2:
- default_home unclear label (reads as screen, sits next to "default screen") → explicit registry
  display label ("Default home folder"); group path fields apart from navigation enums.
- weekly_schedule + daemon-env value → add placeholders.

## [in progress] Quality-audit (workflow #2) — consistency / theme / formatting / ARCHITECTURE

Covers: template-consistency (did every card get everything), tooltip coverage, theme
compliance light+dark, spacing/fonts, and the abstraction architecture (the card/row/InfoTip
pattern copy-pasted across 7 sections + FieldRenderer inlined in Appearance/GuiServer → propose
shared SettingsCard/SettingsRow/unified-FieldRenderer). Merge with UX-audit → ONE fix batch.

## Settings SHIPPED (ef8d87e, pushed cbcdf28..ef8d87e, deployed + live-verified)

Card redesign + InfoTip + dark elevation + UX-audit fixes (backups copy, defer
default_home/shell, restart buttons w/ honest messaging, placeholders, env rebuild,
duplicate-CSS fix). 28 MCP servers green post-deploy. Live dark screenshot confirmed
elevated cards.

## Settings COMPLETION (in progress — user: "не во всех карточках тултипы, остался текст, кнопки не нормализованы")

User-flagged remaining gaps after the ship:
- **[DONE] Buttons** — added a central `section[data-section] button` style (base outline +
  .btn-primary accent + .btn-danger), normalizing all 29 browser-default buttons. ⓘ excluded.
- **[DONE] loading/error branches** — SectionDaemons loading/error now use the card shell.
- **[in progress — settings-completion workflow] Semantic button classes** (Save/Apply→primary,
  Clean/Force-kill/Stop→danger) + convert remaining long inline help-walls → InfoTip (keep short
  action descriptions visible). 4 parallel frontend-engineers + verify.
- Then: rebuild + test + deploy + screenshot.

## NEXT PHASE — whole-GUI polish (user: "приводи весь GUI в нормальный вид") — see ROADMAP

After Settings is fully done: roll the card/InfoTip/button design language across the other 9
screens (Dashboard, Servers, Catalog, Migration, Add server, Secrets, Logs, Capabilities, About),
plus the **Servers matrix client-column visibility management** (15+ clients → hide/show).
Approach (per user "попорядку по плану параллельно"): Servers + matrix first (highest value),
prototype → then fan-out the rest. mui-mcp connected but NOT used (MUI=React, stack is Preact+Tailwind).

## Recorded as FUTURE design (NOT this work-item) — see ROADMAP.md

- Marketplace two-tier install (direct vs through-hub) + Install button + qt-docs entry.
- Separate MSYS/toolchain installers (auto-detect path + manual fallback).
- Settings architecture refactor (shared SettingsCard/Row/unified FieldRenderer).
- Folder/path picker for TypePath; GUI self-restart endpoint.

## Side ideas captured this session (not part of this work-item)

- Qt docs MCP (`https://qt-docs-mcp.qt.io/mcp`) — add to hub ONLY if the user
  has active Qt/C++ projects; not useful for mcp-local-hub itself (Go+Preact).
  → noted in ROADMAP.
- Expand subagent role catalog from `TheQtCompanyRnD/agent-skills` (~32 agent
  defs). → already appended to ROADMAP.
- Use the project's own CSS language-server MCP (`vscode-css`) for CSS work
  (dogfooding). → ongoing.

## Earlier review follow-ups (from the autoprune/backups-clean fix, pushed cbcdf28)

- Surface per-file `os.Remove` failures into the GUI backups-clean response
  `errors[]` field (signature change across BackupsClean/CleanIn/backupsAPI).
- Registry-Max upper bound on the `?keep_n` query override (consistency).
- Prune Phase 2 — manual GUI Remove button on daemon cards.
