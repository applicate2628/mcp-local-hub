---
status: closed
---

# Epic: GUI quality initiative (debug + polish the dev-variant GUI)

Opened 2026-06-17 (user live-review drive). Groups the G1-G17 GUI work-items under
one initiative. Detail/plan lives in `work-items/ROADMAP.md` → "GUI POLISH +
FULL-TEST INITIATIVE" (revised phase order from the 4-lens plan review).

## Goal
Make the current dev-variant GUI solid, consistent, and reviewed: every screen +
control works, the design is unified, resilience gaps (error-boundary, SSE, a11y,
responsive) are closed, the architecture/security audit findings are fixed, and the
whole surface is functionally tested. This is the prerequisite the user named for
the FUTURE clean-install / end-user productization track
(`work-items/2026-06-18-clean-install-ux-vision.md` — a SEPARATE future epic, NOT a
child here).

## Children
(each marked `(active|closed)`; closed = shipped + deployed + reviewed)

- gui-g1-matrix-hover-scope (closed) — hover points only at click targets + col/row scope preview
- gui-g2-capability-badge (closed) — method-not-found → "unsupported" not red error (+ #371 narrowed)
- gui-g3-vendor-init-verify (closed) — 15 matrix columns InitEmpty verified (no gap for installed clients)
- gui-g5-button-design-system (closed) — shared .btn base + STYLE-CONTRACT.md + 3-screen proof
- gui-g6-logo-and-tray-icon (closed) — sidebar logo + tray/favicon redrawn to match (hub + 3 satellites)
- gui-g7-perf-status-memo (closed) — per-server manifest port memo on the /api/status hot path
- gui-g8-static-analysis-clean (closed) — scannererr/writestring/S1009/dead-branch fixes; go vet clean
- gui-g9p1-daemon-state-classifier (closed) — one canonical DaemonDisplayState owner; Quarantined trap closed
- gui-g9p2-config-path-single-owner (closed) — adapter ConfigPath() sole owner + reconcile test
- gui-g9p3-state-writers-hardened (closed) — 4 writers through WriteStateFileBytesAtomic (+ Save deadlock fix)
- gui-g12-error-boundary (closed) — top-level ErrorBoundary; route-identity key (codex P3 fix)
- gui-g13-sse-resilience (closed) — useEventSource onerror + live/reconnecting badge
- gui-g16-error-redaction (closed) — writeAPIErrorRedacted central helper; home-path leaks closed
- gui-live-tweaks (closed) — Settings row-align, whole-server-cell row-hover, asset no-cache
- gui-g14-responsive (closed) — sidebar→topbar @≤768px verified@700px; matrix horiz-scroll; codex P3 sticky-border fix
- gui-g15-a11y-deepening (closed) — th scope= + table caption + WCAG-AA --success-color token + modal focus-return
- gui-g17-vendor-init-uninstalled (closed) — secure handle-relative parent-dir create (POSIX fd-relative + Windows NtCreateFile RootDirectory=held-handle); codex MEDIUM Windows-TOCTOU found+fixed; deterministic junction-swap regression test; deployed 62bbf48
- gui-g4-functional-test (closed) — full e2e suite repaired 19-red→129-green (fixture state-isolation hole fixed: binary was untagged → leaked real %LOCALAPPDATA% state, so empty-home contracts never actually tested) + live-drive of all 10 screens (0 console errors); 2 real bugs found+fixed (deploy-stale-serve, split-brain fleet) + Settings deep-link scroll re-scroll fix
- gui-g11-coverage-audit (closed) — high-risk branches gained NEW tests via each G-item (G9-P1 classifier golden tests, G17 junction-swap + strict-prefix-DACL regression, Settings scroll regression) + the e2e VALIDITY fix (the suite was silently reading real state → not testing its empty-home claims). Formal coverage-% measurement NOT run as a separate pass — optional follow-up.
- gui-g10-stale-sweep (closed) — 3 phantom worktree-LSP daemons deregistered; worktree-prune + .scratch + .old sweep

## Rollup
19 of 19 children closed. Epic CLOSED 2026-06-18.

## Closure

Closed 2026-06-18. All 19 children delivered, deployed (HEAD 8df85be, pushed), and
reviewed. Headline outcomes: shared .btn design-system + STYLE-CONTRACT; one canonical
DaemonDisplayState classifier (fail-quiet Quarantined trap closed); top-level
ErrorBoundary + SSE live/reconnecting badge; WCAG-AA a11y (th scope/caption/contrast
token) + responsive sidebar→topbar @≤768px; G17 secure handle-relative parent-dir create
(codex-found MEDIUM Windows-TOCTOU fixed with NtCreateFile RootDirectory=held-handle +
deterministic junction-swap regression test); central error-redaction; perf status-memo;
go-vet clean.

Real defects surfaced + fixed during verification (not just planned scope): (1) deploy
procedure served STALE bundles (gui-only kill left the old web server running) → canonical
full-reset deploy procedure documented; (2) repeated deploys left orphan daemons → supervisor
split-brain (Dashboard all-Quarantined while MCP worked via orphans) → clean-reset recovery;
(3) e2e suite was silently reading the developer's REAL state-dir (binary untagged) so its
empty-home contracts tested nothing → fixture isolation fixed; (4) Settings deep-link
one-shot scroll landed short below async-grown sections → ResizeObserver re-scroll.

Residual (optional follow-ups, non-blocking): formal coverage-% measurement; 2 autocomplete
a11y hints on password inputs (Secrets/Add-server); the broader clean-install/end-user
productization track (`work-items/2026-06-18-clean-install-ux-vision.md`) is a SEPARATE
future epic the user explicitly deferred. Deploy discipline: see
feedback_deploy_kill_gui_process_directly memory (full-reset + verify-served + zero-quarantine).
