---
status: closed
context: backlog
severity: low
---

# Backlog: GUI self-restart may orphan gate-ON client URLs if the hub port drifts

## Closure (2026-06-29) — NOT-A-BUG

Verified against live code: the hypothesized "silent different-port rebind"
is not current behavior. The self-restart bind uses the persisted `ep.Port`
directly (`internal/api/hub_mcp_bind.go:157` —
`bindAddr := fmt.Sprintf("127.0.0.1:%d", ep.Port)`) and only takes a fresh
OS-assigned port when the persisted record had none (port 0, first-start
path; `loadHubEndpointLocked` at `:129` yields a zero `ep` on a missing
file). The auto-restart path explicitly OPTS OUT of the port-reset rollback
(`internal/gui/hub_listener.go:606-611` —
`preservePortOnReloadHandlerFailure`), precisely so a reload-handler failure
re-binds the SAME persisted port and does not orphan gate-ON `/clients/` and
`/g/` URLs; the driver emits `hub-listener-restart-failed` and retries
against the same port. So the same-port re-bind invariant the original edge
worried about IS enforced; there is no path that silently re-binds a
different port and orphans gated URLs while preserving the persisted port
file. Closed as not-a-bug; the rare genuine port-drift case (old port held
at re-bind) is already netted by `\mcp-local-hub-liveness` (~1 min) and
recoverable via `mcphub install --reconcile-hub-mode`, as the doc itself
noted under "Why low priority".

Found 2026-06-28 during the §B GUI-self-restart verification (analyst ac2f747b).

## The edge
The shipped GUI self-restart (POST `/api/gui/restart`, commit `218a790c`) re-execs `mcphub gui`; the new process re-binds the hub aggregate listener. It re-uses the PERSISTED hub port (`hub-mcp.endpoint.json`), so in the common case it re-binds the SAME port and gate-ON client URLs (`http://127.0.0.1:<hubport>/clients/<client>/mcp` + `/g/<group>/mcp`) stay valid.

BUT: the self-restart path does NOT share the `--reset-port` exit-8 gate-ON guard (`internal/cli/gui.go:242-250` via `api.GatedOnClients()`). If a self-restart re-binds a DIFFERENT hub port (e.g. the old port is momentarily still held / taken by another process during the handoff window) while clients are gate-ON, every gated client URL orphans (connection refused) — the same drift class the `--reset-port` guard protects against. Low probability (the persisted port is re-used; drift needs the old port to be unavailable at re-bind), but unguarded on this path.

## Options (when prioritized)
1. Share the `GatedOnClients()` awareness on the self-restart path: after re-bind, if the new hub port != the persisted one AND clients are gate-ON, auto-run the `reconcile-hub-mode` rewrite (or warn loudly).
2. Or accept as a documented known-limit (the common case is same-port; recovery is `mcphub install --reconcile-hub-mode`).

## Why low priority
Same-port re-bind is the norm; the worst-case no-GUI failure is already recovered by `\mcp-local-hub-liveness` (~1 min). This is a rare correctness edge, not a routine failure. The 2 other analyst-flagged residual risks (timing-window dependence; no real-process handoff test) are accepted: the liveness task nets the no-GUI worst case, and the handler's seam-mocked test covers the logic (a real-process integration test is infeasible in the Playwright suite since it kills the test's own GUI).

> **CORRECTION 2026-07-17 (item-3 recon, verified in code):** the claim "the worst-case no-GUI failure
> is already recovered by `\mcp-local-hub-liveness` (~1 min)" is **FALSE**. `runEnsureAlive`
> (`internal/cli/supervise_ensure_alive.go:346-360`) returns a no-op when the supervisor is running, and
> a GUI self-restart ADOPTS the live supervisor (`os.Exit` in `gui_self_restart.go:39-44` skips
> `manager.Stop`, so the supervisor survives the handoff). The autostart-GUI relaunch fires only in the
> `running=false AND no live GUI owner` branch (`:393-409`) — i.e. only when the supervisor is ALSO dead.
> So a self-restart that bricks the GUI (child failed to acquire, parent exited, supervisor still alive)
> is NOT auto-recovered; it needs manual relaunch. This edge is therefore higher-risk than this doc
> assessed, and is folded into **Phase-0 item 3**'s design (see
> `active/2026-07-16-productization-gui-solidify/item3-restart-recon.md`).
