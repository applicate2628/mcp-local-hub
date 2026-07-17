# Item 3 recon — GUI self-restart / port-change (analyst memo, 2026-07-17)

Read-only factual map by `$analyst` (opus). Branch master @ 6415c377. Every claim carries file:line in
the source runs; the load-bearing summary is preserved here as the design's evidence base.

## Scope decision (operator, 2026-07-17)
- **Both ports in scope:** the GUI-server port (browser tab) AND the hub-aggregate port (gate-ON client
  URLs + `/g/<group>/mcp` group URLs).
- **Both adjacent findings fold into item 3's design** (the `--port`-defeats-persisted precedence, and
  the liveness-does-not-recover-brick correction).
- The closed backlog `2026-06-28-gui-self-restart-gate-on-port-drift.md` recovery assumption is to be
  corrected regardless.

## The framing fact the roadmap conflates: TWO ports, TWO authority models
- **GUI-server port** — `127.0.0.1:<guiport>` the browser uses. Resolved by `resolveGuiPort`
  (`internal/cli/gui.go:47-55`), persisted as `gui_server.port`, mirrored into the pidport file.
- **Hub-aggregate port** — a SEPARATE socket at `hub-mcp.endpoint.json`.`Port`, baked into every gate-ON
  `…/clients/<client>/mcp` and `/g/<group>/mcp`. Bound value is authoritative (`HubMcpBoundPort`,
  `server.go:955-967`), endpoint file re-persisted to match on each bind (`hub_mcp_bind.go:196-205`).
A self-restart re-execs the same argv, so both re-bind; whether either PORT changes depends on distinct
resolution paths.

## The three confirmed hazard windows (present in code today)

### H1 — zero-GUI-listener BRICK, permanent, not auto-recovered
Self-restart is **kill-old-then-bind-new** and cannot be otherwise for one port (single-flock +
address-in-use forbid two holders). Sequence (`gui_self_restart.go:98-153`, `gui.go:90-111`):
parent spawns detached child (`MCPHUB_GUI_SELF_RESTART_HANDOFF=1`), replies `200 {restarting:true}`,
then after `selfRestartExitDelay=250ms` calls `os.Exit(0)` (releases the flock). Child polls
`AcquireSingleInstanceAt` every `100ms` up to `selfRestartHandoffAcquireDeadline=10s`; on acquire it
binds and only then rewrites pidport with the bound port (`gui.go:697-700`).
**Brick:** if the child's 10s poll fails to acquire (contention, or spawn never reached the poll), it
falls through to the busy/handshake path and exits WITHOUT starting a server — parent already gone →
**zero GUI listeners, no incumbent, no auto-relaunch.**
**Liveness does NOT net it** (correction to closed backlog): `runEnsureAlive`
(`supervise_ensure_alive.go:346-360`) returns no-op if the supervisor is running; a self-restart adopts
the live supervisor (`os.Exit` skips `manager.Stop`), so supervisor stays up → liveness no-ops → dead
GUI is not relaunched. The autostart-GUI relaunch fires only when the supervisor is ALSO dead
(`:393-409`).

### H2 — false success
The `200 {restarting:true}` is emitted on **spawn + exit-intent only** (`gui_self_restart.go:123-132`),
never on "new listener confirmed bound." Frontend immediately shows "reconnects on the same port in a
moment" (`SectionGuiServer.tsx:74`) — an assumption false on a real port change.

### H3 — orphaned browser tab, no auto-navigate
No port-redirect mechanism exists in `frontend/src`. `EventSource` uses relative URLs and auto-reconnects
to the SAME origin (`hooks/useEventSource.ts:43-48`); on a genuine GUI-port change (no-`--port` autostart
launch) the old-port tab retries forever. `actual_port` is consumed only to render the restart badge.

## Authority + precedence (as coded, not as roadmapped)
- GUI-server port resolution: `--port` flag WINS → else persisted `gui_server.port` in [1024,65535] →
  else 0 (OS-assigned) (`resolveGuiPort:47-55`). Autostart/tray pass NO `--port`
  (`autostart/windows.go:65-67`), so the shipping self-restart uses the persisted port; a dev
  `--port N` launch re-runs `--port N` and **silently ignores** a changed persisted value.
- Authority today: **bound socket = truth**; pidport = metadata (flock is ownership authority); persisted
  = intent-until-restart with no live reconciliation. This is the INVERSE of the roadmap's "persisted
  port authoritative."

## Hub-port + gate guard gaps
- `--reset-port` exit-8 guard (`gui.go:204-262`) keys ONLY on the `mcphub-hub` client aggregate entry
  (`hub_gate_detect.go:62`). **Group `/g/` URLs are unprotected** — not blocked by the guard, and NO
  reconcile path rewrites group URLs (they are hand-copied from the Groups GUI). Doubly unprotected.
- The listener-rollback reset (`hub_listener.go:658`, `ResetHubPortContext` on initial-start
  reload-handler failure, `preservePortOnReloadHandlerFailure=false`) sets `Port=0` with **no gate-ON
  guard at all** — can orphan gated URLs inside the bind transaction. The auto-restart driver opts OUT
  of this reset (`preserve…=true`, `:176-183`), so the two callers have opposite polarity.

## Reusable from items 1 & 2
- **Item 1:** `HubListenerHealthWatcher` (`hub_listener_health_watcher.go`) TCP-dials the bound HUB port
  every 15s, unresponsive after 3 fails, fires callbacks + events — a listener-liveness primitive item 3
  can reuse to confirm a new HUB listener is accepting connections. **But it probes the hub port only,
  not the GUI-server port, and it is a bare TCP dial (not a wedged-handler probe).** The `hub_health.go`
  `healthy|recovering|needs-reconcile|down` surface + `GET /api/hub/health` + `hub-health` SSE already
  model an instance-id-change → `needs-reconcile` route with `operator_action=reconcile-hub-mode`.
- **No equivalent health watcher exists for the GUI-server listener** — item 3 must build one to "confirm
  the new listener bound" for the GUI port.
- **Item 2:** `daemonrecovery` / `HoldPIDForTermination` are daemon-port tools; low direct overlap. The
  closest "verify before killing a listener holder" precedent is the `--force --kill` three-part identity
  gate (`single_instance.go:876-1012`).

## Known limits the design must accommodate (not eliminate)
1. Same-port handoff has an IRREDUCIBLE sub-second zero-listener window (kill-old-then-bind-new forced by
   single-port). Target = shrink + detect + auto-recover, not eliminate — unless a port-handover /
   SO_REUSEPORT scheme is introduced (does not exist here).
2. `EventSource` cannot cross an origin change → auto-navigate on a GUI-port change needs a client-side
   redirect the frontend lacks.
3. `--port`-flag-wins caps "persisted port authoritative" to no-`--port` launches unless precedence
   changes.
4. Group `/g/` URLs have no reconcile repair path at all.

## Open questions the design MUST answer
1. Which port is authoritative — persisted `gui_server.port`, pidport, or bound socket? Should `--port`
   keep overriding a changed persisted value on a self-restart?
2. How is "new GUI-server listener bound" confirmed, given the parent has exited and there is no
   GUI-server liveness probe (item-1's watcher is hub-only)? Who observes it; how does a failed child
   report to a tab whose listener is gone?
3. Can the same-port window be bind-before-kill at all (single-flock forbids two holders)? If not, is the
   target shrink+detect+recover?
4. What recovers a self-restart brick, given liveness no-ops while the adopted supervisor is alive? A new
   GUI-owner liveness signal independent of supervisor liveness?
5. Auto-navigate mechanism: where does the old-port tab learn the new port (old listener is dead)?
   pidport / a pre-exit redirect page / a fixed known port — pick the source of truth.
6. Hub-port scope: extend the exit-8 guard to group `/g/` URLs and to the un-guarded listener-rollback
   reset; and does item 3 add a reconcile path for group URLs (currently none)?
