# v0.6 Clean-Architecture Redesign — mcp-local-hub

Status: DESIGN (research-mode; no compatibility/migration constraints — see §0).
Author intent captured 2026-06-10 from the project owner.

## §0 Premises (owner-stated, load-bearing)

1. **Raison d'être.** mcp-local-hub exists FOR **serena** (semantic-code MCP) and the **mcp-language-server** (LSP-as-MCP). Process-tail compression serves THAT. serena + the language-server are the #1 health priority; if they break, the bug is in the project — fix mcphub, not "the user's config".
2. **No compatibility, no migration, no old users.** The project is in research mode. There is NOTHING to roll back to. Every v0.4.x compatibility / migration / rollback mechanism is deletable dead weight.
3. **Clean layered architecture.** Proper layers, real abstractions, minimal coupling, no garbage. Bugs (STOP, demigrate, idle, hardcodes) are symptoms of the messy dual-model architecture; the redesign removes the CLASS, not the instances.
4. **Common logic flexible, defaults via GUI.** Policy values (ports ranges, timeouts, thresholds, idle, wake-mode) flow from a single config owner; operator-facing defaults are GUI-settable. True invariants (16KB log cap, protocol magic) stay inline. (= bug #8.)

## §1 Target layered architecture

```
domain/policy        — daemon descriptors, desired-state, restart policy, port policy (pure, no I/O)
supervisor lifecycle — the v0.5.x supervisor: spawn/observe/reconcile/restart (Job Objects)
IPC                  — owner-bound local IPC (status/respawn/reconcile/quiesce/exit/stop)
persistence          — ONE intent file (supervisor-intent.json) + events log; atomic+flock
GUI / HTTP           — /api/*, the hub aggregator, the serena router, the LSP router
client-config adapters — claude-code / codex / cursor / vscode entry read/write (migrate/demigrate)
```
Minimal coupling: each layer depends only downward. No layer reaches into the scheduler/watchdog (deleted). One desired-state owner (no dual-intent).

## §2 Port design (owner question 2026-06-10: "why hardcode ports / can't we auto-pick free?")

- **Hub port (9125) = stable, configurable RENDEZVOUS.** Every client (`~/.claude.json`, `.cursor/mcp.json`, ...) is pinned to `http://127.0.0.1:9125/...`. A random port each boot would break every client. So the hub port MUST be stable — but it is **GUI-configurable** (`gui_server.port`, Settings), and on change the client configs are rewritten.
- **Daemon ports (behind the hub, 9150+) = auto-allocated** from a config-defined range. Clients never see them — only the hub. The serena pool already does this (`AllocatePort`); generalize it so NO per-daemon port is hardcoded.
- **Single owner:** promote `configs/ports.yaml` from a test-only fixture to the RUNTIME port owner (today it is read only by a drift-guard test; real ports are scattered across embedded manifests + Go const ranges — the split that caused the test-Port:9200-killed-live-daemon bug). (= bug #8, config-centralization.)

## §3 Connection robustness (owner principle 2026-06-10: "on failure, better to DROP the connection")

A hub restart / serena-backend failure must make the serena (and LSP) router **fail loud at the connection layer**: close the MCP session so the client sees a clean disconnect and auto-reconnects — NOT leave a zombie "connected but actually dead" state. The 2026-06-10 incident: a hub restart left the Claude Code serena client wedged ("Unable to connect") even though `/serena/mcp` stayed HTTP 200 — backend fine, client zombie, only a full editor restart cleared it. The router owns this: on backend loss / shutdown, terminate upstream + downstream sessions explicitly. (= serena-router-resilience.)

## §4 The STOP bug — root cause + fix (Phase A.1; highest value, ship first)

ROOT: `Restart`/`RestartAll` are supervisor-aware (call `restartSupervisorOwnedDaemons` → IPC respawn); `Stop`/`StopAll`/`stopKillCore` are NOT — they `killDaemonByPort` (taskkill /F = non-clean exit) + `sch.Stop` (no-op for migrated daemons). The supervisor reaper sees the non-clean exit and RESPAWNS (only CLEAN exits drop); the 60s `IntentWatcher` poll hasn't seen the `daemon-intent.json` stop yet → race → repeated stops churn to Quarantine. This is the live paper-search symptom (stop→failed→stuck→won't restart).

FIX: make `Stop`/`StopAll` supervisor-aware exactly like `Restart` — after `recordStopIntent`, issue the existing IPC `reconcile --apply` verb so the supervisor reads the fresh intent, posts `EvIntentUpdate{stopped}`, and the SM drives `StRunning → StExiting → StIdle` (deliberate stop, no respawn). Add `stopSupervisorOwnedDaemons` mirroring `restartSupervisorOwnedDaemons`; wire at `internal/api/install.go` `Stop` (~:2200) + `StopAll` (~:2545). GUI STOP needs NO change (it calls `api.Stop`). One additive PR, no schema change.

## §5 Legacy-removal phases (no compat → all deletable; from §2 of the legacy investigation)

Dependency chain A → C → D → E → F; **B independent**.

- **Phase A** — STOP fix via supervisor IPC (§4). Independent, ship first; fixes the live bug.
- **Phase B** — GUI scheduler-fallback → fail-loud. Stop surfacing legacy watchdog/supervisor/serena-unified rows when IPC is down; show "supervisor down — restart" instead. Independent.
- **Phase C** — Stop auto-installing the `\mcp-local-hub-watchdog` scheduled task (`setup.go`); uninstall it on existing hosts. It actively FIGHTS the supervisor every 5 min — most urgent legacy. Depends on A.
- **Phase D** — Remove the watchdog command + recovery engine (`watchdog.go`, `recovery.go`, `watchdog_state.go`). Depends on C.
- **Phase E** — Collapse dual-intent into ONE `supervisor-intent.json` (desired-state owner); delete `daemon-intent.json` + `install_intent.go` writers; migrate the 4 readers (supervisor controller, tray, restart_supervisor). Depends on D.
- **Phase F** — Move fresh-install global daemons from the scheduler-task model to supervisor-intent; remove `install --rollback-to-legacy`, `internal/migration/`, `migrate-legacy`. Largest. Depends on E.

## §6 Feature designs that ride the clean layers

- **idle-shutdown (#6)** — serena pool daemons sleep after N min no-tool-call (default 30m, GUI-configurable off/15m/30m/1h/2h), wake on next `/serena/mcp` request. Stop = `desired=stopped` on the (unified) intent with a NEW `IntentReasonIdle`; the router clears it + 503-retries on wake. 60s in-supervisor sweeper. Reuses the §4/Phase-E corrected stop-propagation path — do NOT author a second stop path. Open verify: serena `.serena/` cache re-warm cost on cold respawn → drives the default threshold. **wake-mode cold/warm is mcphub-side** (serena has no cache CLI flag — verified) and GUI-settable (warm=keep `.serena/`, cold=clear-before-respawn).
- **config-centralization (#8)** — §2; ports.yaml runtime owner + tier-A values to SettingsRegistry/GUI + a test-port convention (any value reaching killByPortFn/net.Listen uses `pickFreeLocalPort(t)`/Port:0; guard-test greps for live-band literals).
- **demigrate-serena-router bug** — the GUI uncheck-cursor-serena fails because the dynamic-pool migrate rewrote the cursor entry to the `/serena/mcp` router shape, which `liveEntryMatchesManifestBinding` doesn't recognize as mcphub-managed → `RemoveEntry` refuses. Fix: the demigrate must recognize the router shape (marker + `/serena/mcp` URL) as mcphub-managed-removable.
- **hash→name (#4)** — display-only; the `workspace` path is already in `/api/status`; CLI status + GUI Dashboard show `serena · <project>` instead of `serena-<8hex>`.

## §7 Sequencing

#278 (LSP-orphan reconcile guard + migrate-timeout, clean v0.5.x) merges independently. Then Phase A (STOP) → B → C → D → E → F, folding in #6/#8/demigrate/#4 where they touch the same layer. Each phase = one PR through fable → bot → merge → redeploy.

## §8 Terms and Abbreviations

- **rendezvous port** — the single fixed address all MCP clients dial (the hub, 9125).
- **dual-intent** — the current split: `daemon-intent.json` (v0.4.x stop overrides) + `supervisor-intent.json` (v0.5.x descriptors). Collapsed to one in Phase E.
- **zombie connection** — an MCP client that reports "connected" while its transport is actually dead; §3 makes the router fail loud instead.
- **idle-shutdown / wake-mode** — see §6.
