# Backlog: gate-ON hub-listener HANG has no automatic recovery (B1)

Date filed: 2026-06-16 (user: "да" — file the gate-ON review footguns)
Status: backlog
Source: gate-ON regression review (opus architectural lane) —
`.reports/2026-06/report(main)-2026-06-16_23-30_gate-on-regression-review.md`
Relevance: latent until clients are flipped gate-ON; structural SPOF gap. NOT a
blocker for the dormant-ready state (a hung listener with all clients direct-port
harms nothing).

## Symptom (VERIFIED by review)
Under gate-ON, the hub aggregate on `:<hubport>` (3439) is the single path for all
of a client's aggregated MCP servers (11 for claude-code). The hub listener is a
fire-and-forget goroutine inside the GUI process
(`internal/gui/hub_listener.go:296-318` — `srv.Serve(ln)`; on serve-loop death it
logs `hub-listener-down` and sets `comp.alive.Store(false)`, but NOTHING restarts
it). A HANG (wedged accept loop, a stuck handler goroutine, a deadlock on
`hub-mcp.lock`) with the GUI process still alive leaves all aggregated servers
unreachable with no automatic recovery until the operator manually restarts the GUI.

## Why the existing recovery does NOT cover it
- `\mcp-local-hub-liveness` (`mcphub supervise --ensure-alive`) probes the
  SUPERVISOR LOCK, not the hub listener's responsiveness. A live GUI with a hung
  listener passes the liveness probe.
- Hot-swap (a) self-heal + (b) DaemonRestartWatcher recover daemon RESTARTS only —
  they run downstream of a live hub and do nothing for hub death/hang.
- `comp.alive` is a passive badge consumed only on `/api/settings` reads
  (`hub_listener.go:299-302`); there is no active health-probe-and-restart.

## Fix options (pick at adoption time)
- Add an active hub health-probe to the liveness task (or a dedicated watchdog
  goroutine): periodic loopback GET to `/clients/<any>/mcp`; on timeout, restart
  the listener (ShutdownHubListener + startHubMcpListener) or escalate.
- OR a self-watchdog goroutine in the GUI that pings the listener and re-binds on
  unresponsiveness, with bounded retries + a backoff.
- Minimum: a runbook entry — "if ALL aggregated MCP dies at once under gate-ON,
  restart the GUI" — proportionate for a single-operator host.

## Next steps
- Decide adoption posture first (moot if gate-ON is never used).
- For a single-operator host, accept-with-runbook is proportionate; a fleet/
  multi-user posture warrants the active health-probe.
