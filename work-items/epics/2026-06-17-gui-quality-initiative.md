---
status: active
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
- gui-g14-responsive (active) — breakpoint contract (sidebar→drawer, matrix→scroll); only 1 @media today
- gui-g15-a11y-deepening (active) — th scope=, table caption, WCAG-AA contrast, modal focus-return
- gui-g17-vendor-init-uninstalled (active) — secure parent-dir create so not-installed clients can Initialize on clean install (`work-items/2026-06-18-vendor-init-uninstalled-clients.md`); SECURITY-reviewed lane
- gui-g4-functional-test (active) — control inventory: every button × screen × expected outcome (Playwright)
- gui-g11-coverage-audit (active) — raise coverage on highest-risk branches (unified with G4)
- gui-g10-stale-sweep (active) — worktrees/.scratch/.old + the phantom serena-registration daemon

## Rollup (manual snapshot — derive live via /agents-status)
14 of 19 children closed. Open: G14 responsive, G15 a11y, G17 vendor-init (security lane),
G4+G11 test, G10 sweep. Close the epic only when all 19 are closed AND the dev-variant
GUI is user-solid.
