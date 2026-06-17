# RESUME — GUI polish + quality initiative (parked 2026-06-17 for reboot)

> Durable resume point. R: is a RAM-disk (workflow outputs there are GONE after
> reboot). Everything needed to continue is here + in git + in the on-disk
> `work-items/ROADMAP.md` (gitignored but on D:, survives reboot).

## Current state
- **HEAD:** `4133dd1` (refactor(status): canonical daemon-state classifier). **All pushed** (origin/master up to date).
- **Deployed:** dev binary `~/.local/bin/mcphub.exe` = 4133dd1, v0.4.8. Fleet healthy (39 connected, 18 Running + 1 Stopped).
- **npm:** v0.4.8 live (latest). Publish = tag-push CI only (see CLAUDE.md "npm publish — CANONICAL pipeline").

## THE PLAN (full): `work-items/ROADMAP.md` → section "🎨 GUI POLISH + FULL-TEST INITIATIVE"
G1-G16, with a REVISED execution order from a 4-lens plan review (wwr9w47v1, convergent). Phases:
- **Phase 0 (DONE + deployed):** G2 capability-badge · G1 matrix hover col/row-scope · G6 sidebar logo · G3 init-verify (no gap). PLUS live-review tweaks: Settings Appearance row-align · row-hover on whole server cell · asset no-cache (deploy-staleness fix) · tray/favicon icon redraw = G6 logo.
- **Phase 1 (DONE):** G9 architecture audit + G16 security-surface audit (read-only). Findings → `work-items/2026-06-17-phase1-audit-findings.md`. **G9 P1 (daemon-state canonical classifier) ALREADY FIXED + deployed (4133dd1)** — golden-test byte-identical, Quarantined enumerated (fail-quiet trap closed).
- **Phase 2 (NEXT):** G5 design-system foundation (shared `.btn` base + style-contract; de-dup the 8+ per-screen button selectors) BEFORE per-button polish. FOLD IN: G12 global error-boundary (NONE exists — white-screen risk), G13 SSE `useEventSource` onerror+reconnect+stale badge, G14 responsive (only 1 @media in 1880 lines), G15 a11y (`<th>` no scope=, contrast audit, modal focus-return), destructive-confirm + toast/banner unification. PLUS audit-fixes: **G9 P2** (client config-path dual-encoded descriptor-vs-adapter → adapter ConfigPath() sole owner), **G16 P2** (no central error-redaction helper — many gui handlers leak raw err.Error()/os.PathError paths; add writeAPIErrorRedacted in scan.go, EXEMPT the backups path-echoes).
- **Phase 3:** G4 functional test (every button — control-inventory checklist) UNIFIED with G11 coverage, as durable Playwright specs AFTER UI structurally final.
- **Phase 4:** G7 perf (post-G2; EXCLUDE the /api/status capability-probe path G2 shrank) · G8 code-clean (informed by G9).

## Key revised-order rules (from plan review — non-negotiable)
1. **G5 (style) BEFORE G4 (test)** — else button-restyle invalidates G4 specs.
2. **G2 BEFORE G7** — G2 deleted probe round-trips G7 would profile.
3. **G9 early + read-only** — its dual-store finding IS Phase E (reconcile, don't fork; G9 ≠ GUI-polish).
4. **G3 hard-downscoped** — 21 clients already have InitEmpty; matrix = fixed 15 cols (routing.ts CORE 7 + WAVE2 8); pochi/zencoder registry-only NOT columns. DONE (no gap).
5. **G4/G5 need a Definition-of-Done** (control-inventory / written style-contract).

## Deferred / parked (NOT in this initiative)
- **E2-final** (delete daemon-intent.json file layer) — ABANDONED as a one-shot (too entangled, lane dup'd methods). E2-core already shipped 0.4.8 (sub-block is sole live stop source). The file-layer tail is a HARMLESS backward-compat read; remove in a future major. See ROADMAP §D + the lengthy E2 analysis. Tray reads the now-stale daemon-intent.json (latent G9-adjacent bug) — repoint to sub-block when E2 tail is done.
- Roadmap §A/B/C/D/E/F (the pre-GUI tail) — all CLOSED/gated/blocked/parked (see ROADMAP CLEAN REMAINDER).

## How to resume (deploy/test discipline)
- **Deploy:** `bash build.sh` → cp bin/mcphub.exe to `~/.local/bin/mcphub.exe.new` → find GUI owner (parent of `mcphub supervise`) → taskkill /F /T → rename-aside (mv old→.old-ts, .new→target) → Start-Process gui --no-browser → until-loop wait supervisor + settle 12s → verify `claude mcp list` (NOT mcphub status). Sweep .old-* to keep 5.
- **State-safe go test:** fresh temp HOME/USERPROFILE/LOCALAPPDATA + `MCPHUB_STATE_DIR_OVERRIDE=<temp>/mcp-local-hub` (must end in mcp-local-hub or TestDaemonStateDir fails) + MCPHUB_E2E_SCHEDULER=none MCPHUB_E2E_SUPERVISOR=none + `-tags=test_state_path_env` + narrow `-run`. NEVER whole-package against live state.
- **Workflow scripts:** NO literal `${VAR}` or backticks inside agent-prompt template literals (both crash the parse — bit me 3× this session). Use single quotes for code spans, reword `${}`.
- **GUI visual bugs:** screenshot via Playwright MCP (navigate http://127.0.0.1:9125/#/... → browser_take_screenshot → Read the png). Browser caches `/assets/*` — the no-cache fix is deployed, but a stale browser needs ONE Ctrl+Shift+R first.
- Pre-existing host-flaky tests (NOT regressions): TestEnrichStatusWithRegistry_SelfPIDIsNotAlive, TestStopForcePIDIdentityRefusalFallsThroughToPortClassifier.

## Immediate next action on resume
Pick: **G9 P2 (config-path single-owner)** OR start **Phase 2** (G5 design-system + the folded UX gaps G12-G15 + G16 redaction helper). Ask the user, or default to Phase 2 since it's the biggest value block.
