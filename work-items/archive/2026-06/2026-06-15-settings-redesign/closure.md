# Closure — Settings redesign

Closed: 2026-06-15
Outcome: DELIVERED — shipped, deployed, pushed, live-verified.

## What shipped (pushed to origin/master)

- **ef8d87e** feat(gui): Settings redesign — card layout + on-demand ⓘ tooltips + dark elevation + UX fixes
- **77c5b46** feat(gui): Settings completion — normalize all buttons + card-shell loading/error

(f1987ab "Servers matrix column visibility" was the FIRST step of the follow-on
whole-GUI initiative, not this work-item — tracked in ROADMAP.)

## Result

The Settings screen "налеплено" (cramped, wall-of-help-text) → clean cards across
all 7 sections: header + on-demand ⓘ InfoTip (native Preact/ARIA, hover/focus/click,
Esc/click-outside) replacing the inline help walls; label-left/control-right rows;
`.field-ctl` inputs; small-caps group subheaders; 880px content cap. Dark theme fixed
(cards were `bg=var(--bg)`=page color → flat; added `--card-surface` elevated token).
All ~29 buttons normalized (base outline + .btn-primary accent + .btn-danger red),
loading/error branches carded. UX-audit fixes: backups copy corrected, default_home/
shell deferred, restart buttons with honest messaging, placeholders, env editor rebuilt,
duplicate-CSS bug fixed. 536 unit tests green; light+dark visually verified; 28 MCP green
after each deploy.

Two ultracode workflows drove the rollout (6-section fan-out) + the audits (UX +
quality/architecture), plus a fixes-batch workflow + a completion workflow. Multi-model
+ pr-review-toolkit review gates confirmed regression-clean.

## Residual risk / deferred (all in ROADMAP.md)

- Architecture refactor: the card/row/InfoTip pattern is copy-pasted across 7 sections
  + FieldRenderer was inlined in Appearance/GuiServer — extract shared SettingsCard/
  SettingsRow/unified-FieldRenderer (dedup hygiene; redesign works as-is).
- Folder/path picker for TypePath settings; GUI self-restart endpoint (port/hub changes
  need the GUI process relaunched — no endpoint yet; badges say so honestly).
- happy-dom 20.9.0 non-functional localStorage → a shared vitest Storage polyfill may
  be worth adding (worked around with a test-only shim for the matrix feature).

## Follow-on initiative (NEW, ROADMAP-tracked)

Whole-GUI polish: apply this design language to the other 8 screens (Dashboard, Servers
visual, Catalog, Migration, Add server, Secrets, Logs, Capabilities, About) + the
Servers matrix column-management (already shipped f1987ab). Per-screen prototype→rollout.

## Retrospective

What went well: prototype-first (Daemons) → user-approved direction → fan-out rollout
scaled cleanly; audits caught real defects (dark flatness, lying backups copy, unwired
restart). What to watch: subagent "all green" always re-verified locally (caught nothing
false this time, but the discipline held); the restart-button premise ("POST /restart
exists") was wrong — the implementer correctly refused to wire a 404 and flagged it.
