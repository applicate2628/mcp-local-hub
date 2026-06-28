---
status: open
context: backlog
severity: low
---

# Backlog: GUI self-restart may orphan gate-ON client URLs if the hub port drifts

Found 2026-06-28 during the §B GUI-self-restart verification (analyst ac2f747b).

## The edge
The shipped GUI self-restart (POST `/api/gui/restart`, commit `218a790c`) re-execs `mcphub gui`; the new process re-binds the hub aggregate listener. It re-uses the PERSISTED hub port (`hub-mcp.endpoint.json`), so in the common case it re-binds the SAME port and gate-ON client URLs (`http://127.0.0.1:<hubport>/clients/<client>/mcp` + `/g/<group>/mcp`) stay valid.

BUT: the self-restart path does NOT share the `--reset-port` exit-8 gate-ON guard (`internal/cli/gui.go:242-250` via `api.GatedOnClients()`). If a self-restart re-binds a DIFFERENT hub port (e.g. the old port is momentarily still held / taken by another process during the handoff window) while clients are gate-ON, every gated client URL orphans (connection refused) — the same drift class the `--reset-port` guard protects against. Low probability (the persisted port is re-used; drift needs the old port to be unavailable at re-bind), but unguarded on this path.

## Options (when prioritized)
1. Share the `GatedOnClients()` awareness on the self-restart path: after re-bind, if the new hub port != the persisted one AND clients are gate-ON, auto-run the `reconcile-hub-mode` rewrite (or warn loudly).
2. Or accept as a documented known-limit (the common case is same-port; recovery is `mcphub install --reconcile-hub-mode`).

## Why low priority
Same-port re-bind is the norm; the worst-case no-GUI failure is already recovered by `\mcp-local-hub-liveness` (~1 min). This is a rare correctness edge, not a routine failure. The 2 other analyst-flagged residual risks (timing-window dependence; no real-process handoff test) are accepted: the liveness task nets the no-GUI worst case, and the handler's seam-mocked test covers the logic (a real-process integration test is infeasible in the Playwright suite since it kills the test's own GUI).
