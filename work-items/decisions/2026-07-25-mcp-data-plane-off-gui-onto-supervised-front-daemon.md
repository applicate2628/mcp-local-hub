---
title: The MCP data plane (serena + LSP routing) is owned by a supervisor-managed front daemon, not the GUI
status: proposed
date: 2026-07-25
owner: lead (main conversation)
supersedes: none
relates-to:
  - work-items/active/2026-07-25-mcp-front-daemon/
  - work-items/decisions/2026-07-21-gui-death-instrumentation-not-the-sink.md
  - work-items/bugs/2026-07-20-gui-exits-silently-at-logon-leaving-headless-fleet.md
---

## The question

Claude Code's MCP config points at `http://127.0.0.1:<P>/serena/mcp` and
`/lsp/<lang>/mcp`, both mounted on the **GUI process** listener (port `P`, default
9125). When the GUI dies (silent exit-0 at logon, panic, taskkill), those routes
vanish and Claude Code gets `Subprocess initialization did not complete within
60000ms`. The per-daemon backends are supervisor-managed and survive; only the
GUI-hosted routers die. Operator goal: **Claude / MCP must keep working even when
the GUI process is down.**

## Decision

**Adopt Option C — a dedicated, supervisor-managed "MCP front" daemon that owns a
stable MCP port and hosts the serena+LSP router data plane.** Run the already-
accepted Option-A diagnostics (Part A exit-reason + Part B headless-GUI
observation, decision 2026-07-21) concurrently as the near-term mitigation +
death-rate reducer. Reject Option B (host the router inside `mcphub supervise`).

Operator go-ahead: 2026-07-25 ("да").

## Why C

- **A (robust GUI) cannot meet "survives entirely":** even a perfect exit-fix +
  headless relaunch leaves a detect(~1 liveness tick ~60s)+coldstart window that
  intermittently exceeds Claude Code's 60000ms budget. A is a frequency-reducer +
  diagnostic, not a fix. Also the GUI exit root cause is still UNVERIFIED, so
  fixing it now violates the pre-fix gate — Part A names the caller first.
- **B (router in supervisor) is a self-reap hazard:** the serena auto-register
  cutover reaps+restarts the supervisor (`internal/cli/gui.go:1040-1058`); hosting
  the router there makes that a self-reap, and fuses the data plane into the very
  singleton the liveness task recovers.
- **C's key enabler (architect-verified):** the router HANDLER is thin and
  on-disk-backed — zero references to GUI-process-only `Server` state
  (Broadcaster/tray/hubHealth/hubMcpComp/restartCoordinator/settings/events). Its
  only `Server` coupling is the port-bound same-origin guard (`csrf.go`), trivially
  parameterizable. So the extraction is cheap and reversible.

## Increment 1 (this work-item) — small, verifiable, reversible, CONTRACT-NEUTRAL

Prove the serena+LSP data plane can be served by a supervisor-managed always-on
process that survives GUI death — WITHOUT touching port `P`, the single-instance
lock, RestartV3, or any client config.

1. Extract the router data plane into a neutral package `internal/mcproute`
   (handlers + port-parameterized host/origin guard + transport), taking an
   explicit deps struct instead of `*gui.Server`.
2. `internal/gui` keeps a THIN ADAPTER mounting it on `s.mux` — every existing GUI
   serena/LSP test stays green (the behavior-preserving safety net).
3. New `mcphub route` subcommand: constructs the router deps READ-ONLY (no
   cutover primitives, no idle/prune/reconcile tickers), binds a SECONDARY port
   `Q`, serves `/serena/mcp` + `/lsp/<lang>/mcp`.
4. New supervisor-managed daemon descriptor so reconcile spawns `mcphub route` as
   a Job-Object child (may run standalone first to prove survival, then wire).
5. North-star probe: GUI up → `curl P/serena/mcp` == `curl Q/serena/mcp` (200).
   Kill the GUI PID → `curl Q/serena/mcp` still 200; `curl P/serena/mcp` refused.

## Non-negotiable constraints (implementer)

- Do NOT touch port `P` ownership, the `gui.pidport` single-instance lock, or the
  RestartV3 coordinator/handoff.
- The front daemon is READ-ONLY on the registry + supervisor-intent (sole writers
  unchanged). It must NOT reap/install/start the supervisor (cutover stays in GUI).
- Preserve: the DNS-rebind/same-origin guard admitting Claude Code's no-Origin
  POSTs; the two-separate-registry-handles data-race fix (`gui.go:1027-1035`); the
  LSP notification→202 detach contract; sticky-session bind/lookup/unbind order.
- Console/detach: `mcphub route` as a supervised child inherits
  `MCPHUB_NO_CONSOLE_ATTACH=1` via `composeChildEnv`; verify by probe.

## Deferred to Increment 2 (separate, contract-gated — NOT this item)

Flip port ownership so the front daemon binds `P` and the GUI web UI moves to its
own port. This is the persisted-contract change (client-config URLs) and owns the
RestartV3/single-instance-lock rework. security-sensitive/combined-critical shaped.

## Open questions to probe before Increment 2 (neither blocks Increment 1)

1. Is the persisted GUI port fixed per install (default 9125) or ephemeral? The
   "stable URL" premise depends on it (`internal/cli/gui.go:159`, `setup.go`).
2. Does any operator client point at the hub aggregate (`/clients`, gate-ON)
   instead of `/serena/mcp` + `/lsp/*` directly? If yes, the aggregate listener
   must also survive (scope expansion).

## Also concurrently (separate accepted work-item, decision 2026-07-21)

Part A (exit-reason attribution at GUI cancel/exit sites) + Part B (hoist the
headless-GUI presence observation above `supervise_ensure_alive.go:685`). Reduce
GUI-death frequency + supply the verified exit caller. Independent of Increment 1.
