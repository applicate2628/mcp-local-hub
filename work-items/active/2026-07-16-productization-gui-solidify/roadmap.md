# Phase-0 roadmap — GUI/hub solidify (make the rough surfaces honest + self-recovering)

Source: 16-agent discovery workflow (2026-07-16, run `wf_ed129aa1-bd2`) — 84 findings across
15 GUI surfaces (16 P1 / 38 P2 / 30 P3) + a live GUI walkthrough (Dashboard + Servers: dense
dev-panel, jargon, 198-toggle matrix; zero JS errors → the roughness is product-fit + honesty +
recovery, not broken code). Full findings: the workflow result JSON.

## Five dominant roughness themes

1. **Gate-ON hub-aggregate failure is invisible + CLI-only recoverable.** A hung/dead/
   restart-exhausted hub listener or an instance_id change silently kills ALL aggregated MCP
   traffic while the Dashboard paints every daemon green and Groups still advertises a live
   copy-paste URL. The health watcher already emits `hub-listener-unresponsive/-down/-restart-
   exhausted/-instance-id-changed` — but only to `hub-mcp.log`; no frontend consumes them.
   (This is exactly the "partial" the operator hit 2026-07-16.)
2. **Quarantined / lost-child daemon recovery dead-ends in the GUI.** The visible Restart calls a
   non-force respawn the supervisor refuses for a quarantined daemon; the refusal is only
   `console.error`'d. Real recovery (force respawn + verified-own squatter reap) lives only in the
   CLI (`mcphub daemon recover`); no `/api/daemon/recover` route. Every non-Running state paints
   the same red.
3. **Error surfaces are dead-ends that also leak host internals.** Failures blank whole screens
   with no Retry; many 500s render raw Go/OS errors carrying absolute `%LOCALAPPDATA%` paths, AD
   usernames, DACL/env jargon. `writeAPIErrorRedacted` exists but these sites bypass it
   (open backlog `2026-07-15-gui-error-redaction-broad-audit`).
4. **Status is dishonest.** Stale one-shot snapshots shown as live; "Save & Install" always says
   "next logon" regardless of outcome; Migration blinks a false "No MCP servers found" every poll.
5. **Destructive fleet/credential/config actions fire on one click, no confirm/undo**, and the GUI
   self-restart can brick itself to zero listeners while reporting success.

## Phase-0 items (ranked)

1. **[M] Honest hub-aggregate health on Dashboard + Groups + one-click "Re-reconcile hub URLs"**
   ← theme 1. Consume the existing watcher/restart-driver states into an SSE badge; `connection.available`
   consults the unresponsive signal; a Re-reconcile button bound to the existing `reconcile-hub-mode`.
2. **[M] GUI recovery for quarantined/lost-child daemons** — inline refusal reason + a first-class
   "Recover" action + a new `/api/daemon/recover` route mirroring the CLI flow; truthful state colors
   from the buckets already in `status.ts`.
3. **[M] GUI self-restart / port-change confirms the new listener bound before killing the old**
   (no zero-listener brick; auto-navigate to the new-port URL; persisted port authoritative).
4. **[L] Fail-soft error surfaces + finish the redaction audit** — one shared Retry component;
   route raw 500s through `writeAPIErrorRedacted` (closes the broad-audit backlog + per-handler tests).
5. **[L] Truthful install/migrate/snapshot status** — thread the real daemon-start outcome into
   `/api/install`; render Migration rows from the scan (no blink); relative-age freshness on
   snapshot surfaces.
6. **[M] Confirmation + undo guardrails on destructive actions** — Stop all / Restart supervisor /
   secret Delete / Dismiss / workspace toggle behind ConfirmModal or an undo-window; un-dismiss route.

## FIRST MILESTONE (in progress)

**Honest hub-aggregate health surface on Dashboard + Groups** — the observability slice of item 1,
deliberately EXCLUDING the risky hung-listener auto-restart (that needs Server-lifecycle surgery →
deferred follow-on). Highest-weighted defect + autonomous-safe (backend already emits the states;
work = consume into an honest badge + bind the existing reconcile command). Additive, reversible,
unit-testable by forcing the listener unresponsive.

First step: inventory the exact states emitted by `internal/api/hub_listener_health_watcher.go` +
`internal/gui/hub_listener.go` + the SSE plumbing in `internal/gui/events.go`; define the single
hub-health SSE event shape (state: healthy | recovering | down | needs-reconcile + operator_action)
the Dashboard + Groups subscribe to; confirm `groups.go` `connection.available` keys only on
`HubMcpEndpointActive` and must also consult the unresponsive signal.
