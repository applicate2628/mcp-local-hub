# Remaining roadmap — CONSOLIDATED, with the hot-swap plan included (2026-06-16)

Single assembled view of ALL remaining work after the 2026-06-16 session.
Canonical item list: `work-items/ROADMAP.md` §B. Hot-swap detail:
`.plans/2026-06/plan(main)-2026-06-16_20-40_hotswap-clean-correct.md` +
`work-items/decisions/2026-06-16-hot-swap-zero-downtime-config.md`.

## 0. Done this session (so the remainder below is accurate)

Pushed + (binary) deployed at 7668bd9: Conc-F5/F4/F7 deep-sec (review-loop), `.old-*`
count-cap (+1 GB freed), §9.2 Wave-2 drift bug, gdb `debugger_*` aliases (user-reported
"unknown tool debugger_start" — fixed+live), lldb no-output documented. Closed PRs
#345/#346. Found ALREADY-DONE (roadmap stale): marketplace-refresh POST, g3-redaction,
g4 MINOR-5. Hot-swap fully designed + stability-council-validated + planned.

---

## 1. KEYSTONE — Hot-swap (zero-downtime config). PLAN INCLUDED.

Decision: slice 2 = **(b) event-driven session invalidation (PRIMARY) + (a) failure-driven
self-heal (BACKSTOP)**, NO timer, (c) proactive-handoff REJECTED (ownership), slice 5
blue/green DROPPED (violates api.Transition single-live-child invariant). Stability bar:
must NET-IMPROVE stability — verified by council (reliability + consultant).

**Phases (ship (a) before (b); (a) standalone, (b) clean primary on top, (a) stays backstop):**
- **P0 — Test-first** (zero risk): covering tests for dispatchToolsCall/resolveToolsCallRoute/
  postToolsCall/postInitialize (NONE exist) via httptest daemon stubs. Mandatory gate.
- **P1 — Error-class split** (no behavior change): typed `daemonHTTPError`; `isRetriableTransportFailure`
  = conn-refused/reset ONLY (never HTTP-4xx → double-exec hazard; never timeout → ambiguous).
- **P2 — (a) backstop** (first user-visible win, no event-bus dep): in-place transport-only retry
  at dispatchToolsCall:633 + per-daemonKey singleflight + InitSuccesses refresh under sess.mu.
  No timer. LIVE-VERIFY with operator.
- **P3 — slice 3 classifier** (pure, observation-only): old-vs-new descriptor → blip? additive
  DriftEntry field, never changes Action.
- **P4 — (b) primary** (clean architecture): supervisor publishes daemon-restarted/ready on the
  event-bus; hub subscribes → invalidate InitSuccesses + re-init. **DEPENDS ON the event-bus
  completion item (Tier B below) — this phase IS partly that work.**
- **P5 — slice 4 gate-ON** (opt-in): client faces stable hub port → connection never drops;
  delivers the literal "config UPDATE doesn't drop the client" goal. One migration blip/daemon.

Excluded: blue/green, readiness probe (no consumer), (c) handoff, serena (stateful → explicit
restart only).

---

## 2. Remaining work — prioritized tiers

### Tier A — high-value, actionable now (Windows, no new HW)
- **Launch face** (TOP adoption lever): before/after screenshot (9→3 RAM) + install-clip GIF +
  README→docs/ restructure. Mostly asset/manual; S each.
- **g4 MINOR-6** — DACL EXPLICIT_ACCESS dedup (~5 Windows test sites + the prod 3-SID allowlist
  triple at secure_write_windows.go:531-554 ↔ hub_mcp_state_dacl_windows.go:318-331). Windows-only,
  **security-boundary → route through the security gate.** S.
- **§9.2 FAMILY-A table** (prevention; the drift bug is already fixed): collapse the clients.go
  triple (SupportedClientNames/AllClients/ConfigPathForName) into ONE registry table +
  DefaultScanOpts(). M. (FAMILY-B ScanOpts struct→map = larger follow-up, ~20 test files.)
- **Catalog +8 servers** — gdb/lldb/mcp-language-server/godbolt/perftools/paper-search + memory/
  serena/time. NOTE design nuance: native-Go bridges (gdb/lldb/perftools) aren't npx packages —
  decide bundled-vs-external catalog representation first. S.
- **GUI features** (each M unless noted): Dashboard expanded-card metrics; Servers matrix row
  drawer + RAM/Uptime/Status cols + per-row Stop/Restart; Settings per-client enable + tray-enable
  toggle (S); Secrets shell-out to `mcphub secrets edit` (S); Add/Edit cwd + env-picker (S);
  BackupsList restore/delete + /api/backups/content + /api/rollback; C4 toasts (S); Settings
  architecture refactor (shared SettingsCard/Row); **GUI self-restart endpoint** (re-exec + lock
  handoff so "Restart now" restarts the GUI listener, not just the supervisor); folder picker (S);
  Status color-blind shape + density registry (PARTIAL).
- **Store residual slices**: POST /api/marketplace/refresh FRONTEND button (XS, backend done);
  direct-mode stdio writer (per-client stdio adapter); Store secret-gating (S4).

### Tier B — architecture / supervisor hardening (some are hot-swap prerequisites)
- **Event-bus completion + fsnotify** (daemon-failed/install-progress/install-done/scan-result/
  client-config-changed). PARTIAL/L. **HOT-SWAP P4 (b) DEPENDS ON THIS** — pull forward; the
  daemon-restarted/ready event is the hot-swap (b) trigger.
- §11.1/11.5 LSP router first-class spec (modes/lifecycle/LazyProxy singleflight/own session-store). L.
- §11.4 didOpen/didClose per-client refcount in lazy_proxy.go (hidden multi-agent bug). M.
- Phase E — collapse dual-intent → ONE supervisor-intent.json; delete daemon-intent.json. PARTIAL/L.
- §3 serena+LSP fail-loud through routerSessionStore. PARTIAL/M.
- §11.3 --strict-job-protection flag + auto-remediation backoff + metrics + alerting. M.
- §11.3 disjoint GUI-vs-daemon port ranges (DM-2) + manifest port-planning validation gate. S.
- §11.4 Phase H aggressive-cleanup CLI/GUI surface + per-subagent reap. M.
- §11.3 tray-menu hang after long uptime / state-event flood. PARTIAL/M.

### Tier C — competitor adoptions (after hot-swap)
- **tool-result compression** (LOW effort) — transparent size-policy on the daemon HTTP response
  path (truncate-with-marker / dedup), opt-in. Token savings.
- **groups / namespaces + per-server tool visibility** (KEYSTONE for multi-tenant + §D tier) —
  expose a named subset of servers to a named endpoint. Generalizes per-client targeting.
- **smart routing** (after G4 unified-endpoint) — semantic/vector tool discovery. Needs embeddings.
  Lower urgency (per-client URL targeting already avoids the bloat).
- **hub-endpoint auth** (§D team-tier only) — scoped bearer/OAuth on the GUI/HTTP surface; only
  when the hub is SHARED (LAN/team). Solo loopback doesn't need it.

### Tier D — platform / verification
- **Linux — UNBLOCKED via WSL** (operator: "для Линукс есть wsl"): oneAPI runtime-verify (POSIX
  path compiles; needs a real Linux+oneAPI run); autostart live-verify on systemd; `setup --server`
  headless flag (PARTIAL); LD_LIBRARY_PATH DLLDirs→lib fallback. S-M each.
- **macOS — BLOCKED** (operator: "macos мы не можем проверить" — no HW): process probe (libproc +
  KERN_PROCARGS2) + `--force --kill` recovery. SHELVED.
- Manual smokes D2.1-D2.6 (tray variants, AttachConsole, lock-recovery-through-reboot, task-manager-
  kill, --force, watchdog-removal) + D3 multi-language LSP. Manual, S-M.
- Client live-smokes — Cursor / VS Code / Gemini-CLI / Qwen-CLI / GUI-dashboard (WSL/real-client). S.
- MSYS / toolchain installers — auto-detect gdb/lldb/clang + manual fallback (operator: "продумаем
  отдельно").

### Tier E — bugs / test-infra / hygiene
- **demigrate-fallback always-succeed residual** — PRODUCT DECISION (accept-and-close vs operator
  force-remove). Needs operator.
- g3-populated-e2e coverage (M).
- **Lane-B CI/test-infra** — eventloop-data-race · api-tests-flock-contention · install-test-port-9128
  · tests-leak-state(gui) · cli-supervise-statedir(gui follow-up) · vitest localStorage polyfill
  (happy-dom 20.9 broken).
- §5 supervisor residual — `/api/supervisor/restart` configureDetached has no ERROR_ACCESS_DENIED
  flagless-retry.
- **PATH-shadow** — npm shim `mcphub` (092e030 stale) shadows `~/.local/bin` (live). NEEDS-DESIGN:
  (a) npm postinstall symlink to ~/.local/bin, (b) install --upgrade syncs both, (c) document +
  `mcphub version` warns on mismatch. Needs operator pick.
- publication-safety: genericize `dima_` in test fixtures + lldb/bridge.go comment — LOW urgency
  (source already clean per 393d878; only docs/CLAUDE.md retain it, out of the named scope).

### §D — COMMERCIAL / PAID TIER (PARKED — "сперва адопшн")
fleet console · central policies/GitOps · team vault+RBAC · SSO/roles · audit/compliance ·
observability at scale · private registry · air-gapped · SLA. Plus NOT-STARTED-CORE: §10 store
productization (XL) · §9 vendor breadth (L) · G4 unified-endpoint/opt-in-Hub Phase-4 HTTP (XL).

---

## 3. Dependencies & recommended sequencing

1. **Hot-swap P0→P2** ((a) backstop) — no dependency, high-value, ships first. The stability win.
2. **Event-bus completion (Tier B)** — pull forward BECAUSE hot-swap P4 (b) needs it; doing P4 IS
   doing the daemon-lifecycle slice of the event-bus.
3. **Hot-swap P3 (classifier) + P4 (b) + P5 (gate-ON)** — after the event-bus slice.
4. **Tier A** (launch face, g4 MINOR-6 via security gate, §9.2 table, GUI features) — parallelizable
   with hot-swap; launch face is the top adoption lever.
5. **Tier C** (compression / groups-namespaces) — after hot-swap; groups/namespaces is the next
   keystone (multi-tenant + §D depend on it).
6. **Tier D Linux (WSL)** — now unblocked; macOS stays shelved.
7. **Tier E** — product decisions (demigrate, PATH-shadow) need operator; the rest is hygiene.

## 4. Gates (apply across all code work)
NO timer (hot-swap); retry transport-only; api.Transition single-live-child UNTOUCHED; test-first
on untested hot paths; security-boundary work (g4 MINOR-6) through the security gate; state-safe
test runs (state-dir + LOCALAPPDATA isolation + backup); leak-check before push; live-verify
request-path changes with the operator; human review before push (repo PUBLIC).
