# v0.6 Clean-Architecture Redesign — mcp-local-hub

Status: FULL IMPLEMENTATION PLAN (research-mode; no compatibility/migration constraints — see §0).
This is the **sole authoritative v0.6 plan**, owned by ONE session going forward. The parallel
implementation session is CLOSED. Phase A (STOP fix) is MERGED (PR #279), DEPLOYED, and
LIVE-VERIFIED on the production fleet — see §4 / §12 Phase 1.
Author intent captured 2026-06-10 from the project owner; consolidated with the 3-angle
review-loop synthesis (architect + fable + consultant) — see §15.

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

> **This is its OWN PR (router session-lifecycle), distinct from §3.1 (status display) and from the STOP PR (§15 P1-a + §3.x).** The current `coordinateExpiredRouterSessionUnbind` (`serena_router_session.go:510`) tears down only 2 of the 3 session stores — `routerSessionStore` (the one returning 200 on a dead backend) is left live. The §3 fix needs 3-store teardown + a NEW workspace→session-id reverse index + a regression assert that the `routerSessionStore` entry is gone. Full mechanism design in §3.x; do NOT fold into the STOP PR (different layer).

### 3.1 Observed bug (2026-06-10): status misreports "Restarting" / hub "down" while daemons serve

`mcphub status` and the GUI Dashboard paint EVERY daemon `Restarting` and the hub "down" while serena (9125/serena/mcp) + fetch (9121) actually serve verified-working MCP traffic — a false negative. **This is a STATUS-DISPLAY bug, DISTINCT from the §3 router-zombie bug** (different layer, different fix site — §15 P1-a / SPEC-CORRECTION). Root: (a) the supervisor genuinely restart-looping = the STOP churn, fixed by A (now landed); and (b) the **verified mechanism** (not a "stale label"): `computeDaemonsSection` (`health.go:399-401`) silently FALLS BACK to the legacy scheduler scan on `ErrSupervisorIPCUnavailable`, and `normalizeDaemonState` (`health.go:957-961`) coerces any unknown/blank scheduler state to `failed`. So a migrated daemon whose `\mcp-local-hub-*` task was deleted (or is transient) surfaces as `failed`/`Restarting` even while the supervisor-owned process serves traffic. **Fix = stop falling back to the scheduler view when IPC is down (Workstream B), NOT "re-probe port+PID"** (the IPC status path already reports authoritative PID/port). C/D then remove the stale scheduler data source entirely. Capture supervisor-state.json + supervisor-events.log at repro. **The post-deploy gate #0 smoke (§12) is required here — §3.1 proved the status itself lies.**

## §4 The STOP bug — root cause + fix (Phase A.1) — DONE + DEPLOYED + LIVE-VERIFIED (PR #279)

ROOT: `Restart`/`RestartAll` were supervisor-aware (call `restartSupervisorOwnedDaemons` → IPC respawn); `Stop`/`StopAll`/`stopKillCore` were NOT — they `killDaemonByPort` (taskkill /F = non-clean exit) + `sch.Stop` (no-op for migrated daemons). The supervisor reaper saw the non-clean exit and RESPAWNED (only CLEAN exits drop); the 60s `IntentWatcher` poll had not yet seen the `daemon-intent.json` stop → race → repeated stops churned to Quarantine. This was the live paper-search symptom (stop→failed→stuck→won't restart).

FIX (LANDED): `Stop`/`StopAll` are now supervisor-aware exactly like `Restart` — after `recordStopIntentAs`, the path dispatches the existing IPC `reconcile --apply` verb (via the side-neutral `supervisorReconcileApplyFn` seam) so the supervisor reads the fresh intent, posts `EvIntentUpdate{stopped}`, and the SM drives `StRunning → StExiting → StIdle` (deliberate stop, no respawn). `stopSupervisorOwnedDaemons` mirrors `restartSupervisorOwnedDaemons`; wired at `internal/api/install.go` `Stop` (`stopSupervisorOwnedDaemons` call at `install.go:2226`) + `StopAll` (`install.go:2595`, supervisor pass + the legacy `killByPortFn` fallback at `install.go:2646`). GUI STOP needed NO change (it calls `api.Stop`). One additive PR, no schema change.

**PR #279 result (8 fix-stack commits, 6 review rounds — fable + opus lines; the Codex Cloud bot was quota-limited all day so the owner authorized lane substitution requiring ALL lanes PASS):** the core `8ab8a42` was clean on round 1, but THREE serious bugs surfaced AFTER the first green, all found by the review lanes and fixed in the stack:

1. **Quarantine bypass of the force-gate** (fable r2, `a263443`): the pre-dial running-intent write bypassed the QUARANTINED force-gate via the 60s `IntentWatcher` UpdatedAt-diff → fixed with retry-on-typed-refusal + new code `RESPAWN_REFUSED_INTENT_STOPPED`.
2. **Redial dead vs the real supervisor cache** (fable r3, `eaf7a94`): the SM gate reads the `daemonIntent` CACHE, refreshed only by boot / 60s-watcher / reconcile verb → dispatch via `reconcile --apply` through the side-neutral seam.
3. **Reviving quarantined neighbors** (lead-found `6cc31a3` + fable r5 `bebb760`): the drift loop walks ALL rows; the broadened spawn direction revived `StQuarantined` bystanders with failures reset → quarantine-respect gate added on BOTH spawn arms (all-return-paths discipline).

Squash-merged → master `98ce92f`; branch deleted. **Live verification on the production fleet** (the original symptom): `mcphub stop --server paper-search-mcp` → exit 0, "✓ Stopped", SM state=idle, pid=0, intent desired=stopped reason=user-stop (synchronous, no taskkill); 75 s wait (full IntentWatcher cycle) → state=idle, pid=0, restart_history=0 — NO respawn, NO churn; `mcphub restart --server paper-search-mcp` → exit 0, state=running, pid set. The old stop→respawn→quarantine bug is dead.

> **Operational note (windowsgui stdout swallow).** The windowsgui binary swallows console stdout/stderr under PowerShell `&`; CLI verification MUST use `cmd /c "... > file 2>&1"`. Wrong server-name and bare-arg invocations die on cobra parse BEFORE the audit trail fires (the first two verification attempts wrote no intent-audit entries because the server is `paper-search-mcp` and the flag form is `--server`). This is the §12 gate #0 reason the post-deploy smoke MUST capture output to a file.

## §5 Legacy-removal phases (no compat → all deletable; from §2 of the legacy investigation)

Dependency chain A → C → D → E → F; **B independent**.

- **Phase A** — STOP fix via supervisor IPC (§4). **DONE + DEPLOYED + LIVE-VERIFIED (PR #279, merged 98ce92f).** Fixed the live bug.
- **Phase B** — GUI scheduler-fallback → fail-loud. Stop surfacing legacy watchdog/supervisor/serena-unified rows when IPC is down; show "supervisor down — restart" instead. Independent.
- **§3 fail-loud** — its OWN PR (router session-lifecycle, NOT folded into the STOP PR — §15 P1-a). 3-store teardown + reverse index.
- **Phase C** — Stop auto-installing the `\mcp-local-hub-watchdog` scheduled task (`setup.go:471`); uninstall it on existing hosts. It actively FIGHTS the supervisor every 5 min — most urgent legacy. Depends on A. **Add the lock-liveness recovery task BEFORE C/D delete the watchdog — additive step 3a, NOT `RestartOnFailure` (§15 P1-b).**
- **Phase D** — Delete the watchdog command + recovery engine. **MIGRATE `splitJSONLines` out of `watchdog_log.go` first; KEEP `intent_audit.go`** (both shared with the supervisor — §15 P1-b delete-list traps). SAFE DELETE: `recovery.go`, `watchdog_state.go`, `watchdog_xml_validator.go`, `cli/watchdog.go`, `scheduler_watchdog_xml.go`, the `Install/UninstallWatchdogTask` surface. Depends on C. Gate D on the "kill supervisor mid-session → auto-recovers, zero watchdog code" test. **(C+D ship together as 3b, after 3a observed.)**
- **Phase E** — Collapse dual-intent into ONE `supervisor-intent.json` (desired-state owner); delete `daemon-intent.json` + `install_intent.go` writers; migrate the **5 surviving `IsActiveStop` readers** (supervisor controller, supervise_reconcile, tray, gui_tray_state, api_surfaces — NOT restart_supervisor, which already reads supervisor-intent; see §5.1-E). Depends on D. **Two-step E1/E2 + merge-lock-owner + pre-merge backup (§15 P1-c).**
- **Phase F** — Move fresh-install global daemons from the scheduler-task model to supervisor-intent; remove `install --rollback-to-legacy`, `internal/migration/`, `migrate-legacy`. Largest. Depends on E.

## §6 Feature designs that ride the clean layers

- **idle-shutdown (#6)** — serena pool daemons sleep after N min no-tool-call (default 30m, GUI-configurable off/15m/30m/1h/2h), wake on next `/serena/mcp` request. Stop = `desired=stopped` on the (unified) intent with a NEW `IntentReasonIdle`; the router clears it + 503-retries on wake. 60s in-GUI sweeper (it lives in the GUI process — the only process that observes `/serena/mcp` activity; the supervisor sees the resulting stop/clear via its IntentWatcher + reconcile). Reuses the §4/Phase-E corrected stop-propagation path — do NOT author a second stop path. Open verify: serena `.serena/` cache re-warm cost on cold respawn → drives the default threshold. **wake-mode cold/warm is mcphub-side** (serena has no cache CLI flag — verified) and GUI-settable (warm=keep `.serena/`, cold=clear-before-respawn).
- **config-centralization (#8 — split into #8a/#8b, §15 P2)** — §2; **#8a** (before C): a test-port convention (any value reaching killByPortFn/net.Listen uses `pickFreeLocalPort(t)`/Port:0; guard-test greps for live-band literals **across the test tree too**, since the killed-live-daemon incident was a test literal). **#8b** (after F): ports.yaml runtime owner (reconcile stale data first) + tier-A values to SettingsRegistry/GUI.
- **demigrate-serena-router bug** — the GUI uncheck-cursor-serena fails because the dynamic-pool migrate rewrote the cursor entry to the `/serena/mcp` router shape, which `liveEntryMatchesManifestBinding` doesn't recognize as mcphub-managed → `RemoveEntry` refuses. Fix: the demigrate must recognize the router shape (marker + `/serena/mcp` URL) as mcphub-managed-removable.
- **hash→name (#4)** — display-only; the `workspace` path is already in `/api/status`; CLI status + GUI Dashboard show `serena · <project>` instead of `serena-<8hex>`.

## §7 Sequencing

#278 (LSP-orphan reconcile guard + migrate-timeout, clean v0.5.x) is **MERGED** (2c7c343). Phase A (STOP) is **DONE + DEPLOYED + LIVE-VERIFIED** (PR #279, 98ce92f). The remaining spine is B → §3-fail-loud → C → D → E → F, folding in #6/#8/demigrate/#4 where they touch the same layer, split across the v0.6-core / v0.7-adoption milestones — **see §12 for the authoritative baked-in sequencing** (this §7 is the original short recap; §12 supersedes it). Each phase = one PR through fable → bot → merge → redeploy.

## §8 Terms and Abbreviations

- **rendezvous port** — the single fixed address all MCP clients dial (the hub, 9125).
- **dual-intent** — the current split: `daemon-intent.json` (v0.4.x stop overrides) + `supervisor-intent.json` (v0.5.x descriptors). Collapsed to one in Phase E.
- **zombie connection** — an MCP client that reports "connected" while its transport is actually dead; §3 makes the router fail loud instead.
- **idle-shutdown / wake-mode** — see §6.


---

## Roadmap overview

This roadmap supersedes the original 8-section sketch by adding the two owner-requested workstreams the first pass dropped — **multi-agent/multi-client adapter scaling (§9)** and the **GUI one-click MCP Store (§10)** — plus the full deferred-work inventory (§11) and a unified phased sequencing (§12). Sections §0–§8 above stand unchanged.

| Workstream | Goal | Status | Depends-on |
|---|---|---|---|
| **A — STOP fix** (§4) | Make `Stop`/`StopAll` supervisor-aware (IPC `reconcile --apply`); kill the stop→failed→Quarantine churn | **DONE + DEPLOYED + LIVE-VERIFIED** (PR #279, squash-merged 98ce92f; 8 fix-stack commits, 6 review rounds; deployed + production-verified — §4 / §12 Phase 1) | #278 merged (2c7c343) |
| **B — GUI fail-loud** (§5) | Stop surfacing legacy watchdog/serena-unified rows when IPC down; show "supervisor down — restart" | next (independent) | #278 merged |
| **C — drop watchdog task** (§5) | Stop `setup.go` auto-installing `\mcp-local-hub-watchdog`; uninstall on existing hosts | future | A |
| **D — delete watchdog engine** (§5) | Remove `recovery.go` / `watchdog_state.go` / `cli/watchdog.go` (full list + traps §15 P1-b) | future | C |
| **E — collapse dual-intent** (§5) | One `supervisor-intent.json`; delete `daemon-intent.json` + `install_intent.go`; migrate the **5 surviving `IsActiveStop` readers** (two-step E1/E2 — §15 P1-c) | future (v0.6-core) | D |
| **F — drop scheduler/migration** (§5) | Global daemons → supervisor-intent; remove `install --rollback-to-legacy`, `internal/migration/`, `migrate-legacy` | future (v0.6-core) | E |
| **§3 — connection robustness** | serena + LSP router fail loud at connection layer on backend loss (no zombie sessions) — **its OWN PR, 3-store teardown** (§15 P1-a; NOT folded into the STOP PR) | next | router layer (separate from A/B) |
| **#6 — idle-shutdown** | serena pool daemons sleep after N idle min (GUI-configurable); `IntentReasonIdle` | future | E (unified intent) |
| **#8a — test-port convention** (§2) | guard-grep (test tree too — §15 P2) + `pickFreeLocalPort(t)`/`:0`; the killed-live-daemon class | **DONE** (PR #282/#283; AST guard `internal/api/port_kill_guard_test.go`, helper `install_test.go:559`) | independent (no persistent files) |
| **#8b — config-centralization** (§2) | `configs/ports.yaml` becomes runtime port owner (reconcile stale data first); daemon ports auto-allocated 9150+; tier-A → GUI | future (after F) | feeds §10 port alloc |
| **demigrate-serena-router** | demigrate recognizes `/serena/mcp` router shape as mcphub-managed-removable | next | independent |
| **#4 — hash→name display** | Show `serena · <project>` not `serena-<8hex>` in CLI + Dashboard | next | independent (display-only) |
| **§9 — multi-agent/multi-client** | Adapter-registration table; per-client GUI enable/disable; more agents; relay parity | next→future | client-config adapter layer |
| **§10 — GUI MCP Store** | One-button install (generate+manifest-create-to-disk+install); catalog screen; port alloc; progress stream; secret prompts; runtime probes; **command-confirm gate (§15 P1-d RCE)** | future (v0.7) | G5 (done); #8b port alloc; **NOT §9 table** (hard dep dropped — §15 P2; use per-adapter remote-http capability flag) |
| **§11 — backlog** | Categorized deferred items (LSP router design, Linux lane, observability, testing, secrets, docs) | mixed | per-item |
| **LSP router design** | First-class spec for the LSP router (modes/lifecycle/fail-loud parity with serena) — currently named, undesigned | future | §3 |
| **G6 remote-MCP** | `transport: remote-http` install path (URL+headers+secrets, no daemon) | future | feeds §10 remote shape |

Legend: #278 = **MERGED** (2c7c343 on master); STOP-fix (A) = **DONE + DEPLOYED + LIVE-VERIFIED** (PR #279, merged 98ce92f); **next** = first PR block after STOP deploy; **future** = later phased PR.

**Two-milestone split (consultant, §15):** the workstreams group into **v0.6-core** (A done → B → §3-fail-loud → C → D → E → F + #8a + #4 + demigrate — closes ALL live pains) and **v0.7-adoption** (§9 adapters → §10 store → §13 npm → §14 onboarding + #8b). See §12 for the baked-in sequencing.

---

## §9 Multi-agent / multi-client support

"Agents" = the AI client adapters mcphub installs MCP entries into. The architecture is **already multi-client**: one `clients.Client` interface (`internal/clients/clients.go:52-256`), one `AllClients()` registry (`clients.go:661-673`), one shared daemon served to all clients via per-client transport adapters, and a uniform install/migrate/demigrate/reconcile pipeline. "Support a bunch of agents" is therefore mostly (i) collapsing duplicated canonical-set literals into one registration table, (ii) writing more thin adapters, and (iii) surfacing per-client enable/disable in the GUI — NOT new lifecycle machinery.

### 9.0 Expansion targets (the selected list — owner-confirmed 2026-06-10)

The vendors to add adapters for, **selected with the owner** (transcript L25954 + L25965, then L28280), are **10 client adapters**:

| Vendor | Notes | Config path / format source |
|---|---|---|
| **OpenClaw** | CLI agent | docs.openclaw.ai/cli/mcp |
| **Hermes** | Nous Research agent | hermes-agent.nousresearch.com/docs/reference/mcp-config-reference |
| **OpenCode** | agent | research at build |
| **OpenHands** | agent | research at build |
| **Cline** | VS Code extension | research at build |
| **Aider** | CLI | research at build |
| **Kilo Code** | VS Code agent | research at build |
| **Windsurf** | Codeium IDE | research at build |
| **Zed** | Zed editor | `~/.config/zed/settings.json` -> `context_servers` |
| **Kiro** | Amazon agentic IDE | `.kiro/settings/mcp.json` |

**Ollama -> SKIP** — not a native MCP client; would need a bridge (github.com/jonigl/mcp-client-for-ollama). Deferred, NOT in this scope.

Sizeable feature (10 Go adapters + the 9.2 registration table + README client-version table + per-adapter demigrate/rollback symmetry tests) -> **its own PR** (originally scoped "after #268"; now after the legacy-removal/STOP work). Config formats for the newer vendors are researched at build; OpenClaw + Hermes + Zed + Kiro have known config locations (above).

### 9.1 The canonical client set (7 today)

The build-wide set is declared in **three places that must stay in sync** — this duplication is the central scaling defect:

- `SupportedClientNames()` — `internal/clients/clients.go:593-603`
- `AllClients()` factory list — `internal/clients/clients.go:661-673`
- `serenaReconcileClientSet()` — `internal/api/serena_client_reconcile.go:70-80`

All three list the same 7 in the same order: `claude-code`, `codex-cli`, `cursor`, `vscode`, `gemini-cli`, `qwen-cli`, `antigravity`.

Per-client config path + on-disk shape (`ConfigPathForName`, `clients.go:614-654`, plus each adapter's `AddEntry`):

| Client | Config path | Format | Top-level key | Hub entry shape | Anchor |
|---|---|---|---|---|---|
| `claude-code` | `~/.claude.json` | JSON | `mcpServers` | `{"type":"http","url":...}` | `claude_code.go:110-113` |
| `codex-cli` | `~/.codex/config.toml` | TOML | `[mcp_servers]` | `{url=..., startup_timeout_sec=10.0, http_headers=...}` | `codex_cli.go:101-107` |
| `cursor` | `~/.cursor/mcp.json` | JSON | `mcpServers` | `{"type":"http","url":...}` | `cursor.go:66-69` |
| `vscode` | `%APPDATA%\Code\User\mcp.json` (per-OS, `defaultVSCodeConfigPath` `clients.go:639-654`) | JSON | `servers` (NOT `mcpServers`) | `{"type":"http","url":...}` | `vscode.go:114-117` |
| `gemini-cli` | `~/.gemini/settings.json` | JSON | `mcpServers` | `{"url":..., "type":"http", "timeout":10000}` | `gemini_cli.go:53-57` |
| `qwen-cli` | `~/.qwen/settings.json` | JSON | `mcpServers` | `{"httpUrl":..., "timeout":10000}` (note `httpUrl`) | `qwen_cli.go:69-72` |
| `antigravity` | `~/.gemini/antigravity/mcp_config.json` | JSON | `mcpServers` | **stdio relay** `{"command":"<mcphub.exe>","args":["relay",...],"disabled":false}` | `antigravity.go:89-93` |

The per-client schema quirks are real and load-bearing: VS Code's `servers` vs everyone else's `mcpServers`; qwen's `httpUrl` vs gemini/cursor/codex's `url`; claude/cursor/vscode requiring an explicit `"type":"http"`. The shared base `jsonMCPClient` (`json_mcp.go:13-17`) is parameterized by `urlField` (`"url"` / `"httpUrl"` / `"command"`) and `clientName`, so most variants are one small override (`AddEntry`/`GetEntry`/`Exists`) on top of it — cursor/gemini/qwen are each ~80 lines.

Two narrower default sets gate install aggression:
- `DefaultInstallClientNames()` = `{claude-code, codex-cli, cursor}` (`clients.go:609-611`) — the only clients a plain install touches. vscode/gemini/qwen/antigravity are opt-in so a fresh install does not silently mutate every assistant on the box.

The format-neutral entry passed to every adapter is `MCPEntry` (`clients.go:23-48`): `Name`, `URL`, `Headers`, `Env`, plus the relay-only fields `RelayServer` / `RelayDaemon` / `RelayExePath` / `RelayURL` (URL adapters ignore the relay fields; the antigravity adapter ignores `URL`).

### 9.2 The adapter abstraction + the registration-table refactor (the §9 work)

The `Client` interface (`clients.go:52-256`) core methods: `Name()` (`:54`), `ConfigPath()`/`Exists()` (`:58-63`), `InitEmpty() (created bool, err error)` (`:65-95`, powers the GUI Servers-matrix "Initialize" button), `AddEntry`/`RemoveEntry`/`GetEntry` (`:120-129`, idempotent), plus the backup/restore/demigrate surface (`Backup`/`BackupKeep`/`Restore`/`RestoreEntryFromBackup`/`RestoreEntryFromBackupForRollback`/`BackupContainsEntry`/`BackupEntryIsHubManaged`/`LatestBackupPath`) and the cleanup scanners `AllStdioEntries`/`FindStdioLanguageServerEntries`.

**Adding a NEW agent today touches N hardcoded edit-sites across ≥2 files (this is the defect §9 fixes):**
1. `internal/clients/<newclient>.go` — `New<Client>() (Client, error)` factory + struct (usually embed `jsonMCPClient`, override `AddEntry`/`GetEntry`/`Exists`).
2. `SupportedClientNames()` — `clients.go:593`.
3. `ConfigPathForName()` case — `clients.go:619`.
4. `AllClients()` factory slice — `clients.go:664`.
5. (If default-installed) `DefaultInstallClientNames()` — `clients.go:610`.
6. (If it binds shared daemons) `serenaReconcileClientSet()` — `serena_client_reconcile.go:71` + per-server `client_bindings` in `servers/<server>/manifest.yaml`.

A registry exists (`AllClients`), but the source-of-truth lists are **duplicated literals that can silently drift**. The §9 refactor collapses sites 2–5 into a **single registration table** — `(name → factory → default-config-path → default-install flag)` — so adding a client is one entry. `serenaReconcileClientSet()` already calls its set "fixed (not hard-coded per workstation)" but it is still a literal list (`serena_client_reconcile.go:61-80`); fold it into the same table.

### 9.3 The antigravity stdio-relay pattern (non-HTTP clients)

Antigravity is a Gemini-CLI fork whose Cascade agent **silently drops any loopback-HTTP MCP entry** regardless of schema — only remote HTTPS is accepted (`antigravity.go:9-23`). To keep it on the shared-daemon model, mcphub writes a **stdio entry that spawns `mcphub.exe relay`** as Antigravity's child; the relay bridges stdin/stdout JSON-RPC ↔ the daemon's HTTP endpoint.

The `antigravityClient` embeds `jsonMCPClient` and overrides `AddEntry`/`GetEntry` (`antigravity.go:53-146`). `AddEntry` requires `RelayExePath` (absolute mcphub.exe path) and emits one of two arg shapes (`antigravity.go:72-80`):
- legacy manifest-lookup: `["relay","--server",<s>,"--daemon",<d>]` (resolves port from `servers/<s>/manifest.yaml`)
- dynamic-pool direct: `["relay","--url",<url>]` (the `--url` escape hatch, mutually exclusive with `--server`/`--daemon`).

The relay subcommand is `mcphub relay` (`internal/cli/relay.go` + `internal/daemon/relay.go`; design `docs/superpowers/plans/2026-04-17-post-phase-1-antigravity-relay.md`) — an HTTP↔stdio Streamable-HTTP bridge (POST/GET-SSE/DELETE `/mcp`, `MCP-Session-Id` lifecycle). Net effect: 3 Antigravity windows + Claude + Codex = still only the shared daemon set + one cheap `mcphub relay` subprocess per Antigravity window. The serena dynamic-pool reconcile points the antigravity relay at `/serena/mcp` via `RelayURL` alongside URL clients getting the router URL directly (`serena_client_reconcile.go:257-268`) — the relay pattern composes: same `MCPEntry`, the adapter picks stdio-relay vs direct-URL. **Any future HTTP-incapable agent reuses this exact shape.**

### 9.4 GUI enable/disable per client

The Servers matrix already renders a per-client column with an "Initialize" affordance backed by `InitEmpty()` (`clients.go:65-95`), and install honors a narrower/wider target set via `DefaultInstallClientNames()`. The §9 UX adds **per-client enable/disable checkboxes wired to that target set** so the operator opts each agent in/out without CLI flags — backed by the §9.2 registration table (`default-install` flag becomes operator-overridable in Settings, persisted to the GUI prefs file).

### 9.5 Consistent install / migrate / demigrate / reconcile + hardened writes (inherited free)

The lifecycle is already uniform across all clients: every adapter implements backup→`AddEntry`→`RecordManagedEntry`→`RemoveEntry`, with the demigrate guard `ErrBackupEntryAlreadyMigrated` (`clients.go:479-493`) and the `-original` pristine sentinel (`clients.go:680-726`) giving reversible install. The serena router reconcile iterates all in-scope clients identically and rolls back partial failures per-client via `RestoreSerenaReconcileApplied` (`serena_client_reconcile.go:377-401`). A new adapter inherits all of this by satisfying the interface — **but it MUST correctly implement the demigrate/rollback methods** (its schema-specific `isHubURLShapeEntry` / `isHubRelayShapeEntry` detection), or migration symmetry breaks for that client. Every adapter routes writes through `WriteConfigFile` (`write.go:49`), which production swaps to `api.SecureWriteClientConfig` (`secure_write_client_config.go:76-78`) — handle-relative, TOCTOU-safe, DACL/mode-gated; a new adapter gets this automatically by using `WriteConfigFile`/`EnsureClientConfigStub` rather than `os.WriteFile`, so multi-agent expansion does not weaken the write-security posture.

**§9 acceptance:** (1) registration table replaces the 4 duplicated literal lists; adding a client = one table entry + one adapter file; a drift-guard test asserts the table is the single source. (2) GUI per-client enable/disable persists and gates install/reconcile target set. (3) the antigravity relay shape is documented as the canonical non-HTTP path; any new HTTP-incapable adapter reuses it. (4) each new adapter ships demigrate/rollback symmetry tests (round-trip install→demigrate→install).

---

## §10 GUI MCP Store (one-click install)

Today there is **no GUI store**: the frontend nav has 9 screens (Servers, Migration, Add server, Secrets, Dashboard, Logs, Capabilities, Settings, About — `internal/gui/frontend/src/app.tsx:282-290`) and **no Marketplace/Store screen**; no `/api/marketplace*` route exists in `internal/gui/`. G5 shipped only a **CLI-only, read-only discovery surface with zero install side effects**. §10 builds the GUI store **on the G5 foundation**, adding the one genuinely-new backend mechanism: a server-side `generate → port-fill → manifest-create-to-disk → install` orchestrator.

### 10.0 The "fetch was lost" lesson (load-bearing requirement)

The recent manual `fetch` install was lost because **its generated manifest was never committed into `servers/`** — the daemon and client config existed but the manifest source did not persist, so it vanished on the next reconcile. **The store MUST persist every generated manifest to disk under the canonical manifest dir.** This is feasible because `api.Install` loads manifests **embed-FS-first with a disk fallback to `defaultManifestDir()`** (`internal/api/manifest_source.go:73-87`) — a GUI-written manifest on disk is findable by `Install` without recompiling the binary. The store's `manifest-create` step writes there (`api.ManifestCreate(name, yaml)` → disk, `internal/api/manifest.go:251`); the install then finds it. **No store install may rely on an in-memory-only manifest.**

### 10.1 What G5 already provides (reuse, don't rebuild)

- **CLI leaves** (`internal/cli/marketplace.go:40-290`): `search` (`:71-111`, `entryMatches` `:297-302`), `show` (`:113-211`, README body intentionally NOT fetched `:194-203`), `generate` (`:213-269`, draft YAML to stdout, no writes), `refresh` (`:271-290`). Default registry `marketplace.go:38`.
- **Catalog schema + parser** (`internal/api/marketplace_catalog.go`): `MarketplaceEntry` (`:495-508`: `id,name,summary,homepage,readme_url,transport,command,args,env,url,categories,license`); `ParseMarketplaceCatalog` (`:513-536`, enforces `schema_version=="1"`, rejects dup ids, `DisallowUnknownFields`); `validateMarketplaceEntry` (`:538-567`, gates `entry.id` through `CheckManifestName` — same gate as `manifest create`).
- **HTTPS-only fetch** (`internal/api/marketplace_http.go`): scheme enforcement (`:786-788`), downgrade-redirect guard `rejectNonHTTPSRedirect` (`:733-741`), `DisableCompression` (`:748-750,:801`), 10 MB wire cap (`:819-825`).
- **TTL/ETag cache** (`internal/api/marketplace_cache.go`): 24h TTL (`:1030`), future-`fetched_at` clamp (`:1169-1175`), stale-fallback WARN (`:1068-1073`), at `<state-dir>/marketplace-cache.json` via `writeHubMcpStateFile`.
- **The stdio-bridge wrap** (`internal/api/marketplace_generate.go`): `generateStdioDraft` (`:100-198`) maps `transport:"stdio"` → `config.TransportStdioBridge`, seeds `daemons:[{name:default, port:0}]` + three default `client_bindings`; `http` → `generateRemoteHTTPDraft` (`:45-98`). Secret classifier (`api.IsSensitiveEnvName`) leaves matching `${env:NAME}` **verbatim** with `SkipSensitiveEnv:true` (`marketplace_generate.go:120-125`) because the catalog is untrusted; terminal-escape sanitization (`sanitizeCatalogField` `marketplace.go:328-358`) + YAML-comment-injection rejection (`containsUnsafeYAMLCommentRunes` `marketplace_generate.go:265-272`).

G5 **explicitly rejected one-click install for the CLI** (`2026-05-12-g5-marketplace-draft-import-design.md:103`) and listed the GUI Marketplace screen out-of-scope (`:99`), deferred to v0.4.x. §10 is that deferred screen.

### 10.2 The exact gap to one-click

The current path is CLI + manual + multi-step, and **deliberately un-installable** at the draft stage to force human inspection:
1. `generate <id>` prints YAML with `port:0` and `name:<entry-id>`.
2. Operator **manually edits** — picks a real port, renames, redacts verbatim sensitive env (the human-edit step is load-bearing by design, `g5-design:139-141`).
3. `manifest create <name> < draft.yaml` writes to disk.
4. `install --server <name>` preflights + spawns daemon + writes client configs.

The friction blocking one-click: `port:0` is rejected by Preflight/manifest-create (forces the human editor); and the `generate → edit → create → install` chain has **no single server-side orchestrator** (each is a separate CLI/GUI handler — `/api/manifest/create` `internal/gui/manifest.go:70`, `/api/install` `internal/gui/install.go:44`). The GUI store fills the human-editor gap **server-side** (auto-port, auto-name) and adds the missing orchestrator.

### 10.3 The store pieces

1. **Catalog API endpoint** — new `GET /api/marketplace` + `POST /api/marketplace/refresh` GUI handlers wrapping `api.LoadMarketplaceCatalogWithClient` / `api.RefreshMarketplaceCatalogWithClient` (`marketplace_cache.go`), returning JSON entries to the Store screen. Thin wrapper, low effort.

2. **Store screen + Install button** — new Preact screen parallel to `AddServer.tsx`, catalog entries as cards each with one Install button; new nav link in `app.tsx:282-290`.

3. **`POST /api/marketplace/install` — the one genuinely-new backend mechanism.** Composes existing pieces in one call: `api.GenerateDraftManifest` (`marketplace_generate.go:29`) → **fill port + name** → `api.ManifestCreate(name, yaml)` (`manifest.go:251`, **writes to disk — §10.0**) → `api.Install(InstallOpts{Server:name})` (`install.go:229`). The last step already exists verbatim in `/api/install` (`install.go:30-32`: `api.NewAPI().Install(InstallOpts{Server:name})`). **HARD SECURITY GATE (§15 P1-d):** `Install` execs `m.Command` (`install.go:1499`) from an UNTRUSTED catalog. This one-call flow MUST NOT fire install without first surfacing the resolved `command + args + env` for an explicit per-install confirm (the inspection the CLI's load-bearing human-edit step forced), OR restricting one-click to a signed/curated catalog and routing uncurated entries through the manual `generate→edit→create` path. Auto-firing generate→create→install with no command gate is RCE. **Also reject manifest names that shadow an embed-FS manifest** (§15 P2 / §10.0 — `loadManifestYAMLEmbedFirst` loads embed-first, so a colliding GUI-written name is silently shadowed).

4. **Port auto-allocation — the real algorithmic gap, ties to §2/#8.** The draft has `port:0` (`marketplace_generate.go:175-177`) and `Preflight` requires a free port (`install.go:1517-1529`, `portInUse` `:1702`). There is **no production port auto-allocator** — `pickFreeLocalPort` is test-only (`install_test.go:554-559`). The store MUST use the race-free allocator (§15 P2): `net.Listen(":0")`+retain (hold the listener until the daemon binds it) OR an atomic `AllocatePort`-against-the-live-`Registry`, drawing from the **config-driven** canonical band (9150+, owned by `configs/ports.yaml` once #8b promotes it to runtime owner). The bare `portInUse`-scan-then-create alternative is **FORBIDDEN** — it has a wide TOCTOU window (another install/process grabs the port between scan and bind) = exactly the class #8 exists to kill. **Memory lesson: hardcoding `Port:9200` once killed a live daemon — auto-allocation MUST be a real race-free free-port scan, config-driven, never a literal.** §10 therefore **depends on #8b** (config-centralization runtime owner) for a clean port owner.

5. **Install-progress streaming** — `api.Install` is synchronous and writes human text to `InstallOpts.Writer` (`install.go:230-233`); the GUI `/api/install` discards it (`Writer:nil`→stderr, `install.go:30-32`). For per-step progress (uvx/npx download, spawn, client-config write), wire install output into the existing SSE channel `/api/events` (`internal/gui/events.go:362`) or a streamed response. **"Download" progress = daemon-spawn progress** — uvx/npx fetches the package on the daemon's first exec, so the first-spawn output IS the download progress.

6. **Secret prompts** — when an entry's `env`/headers carry `secret:`/`${env:*}` refs (sensitive-classifier hits, `IsSensitiveEnvName`), the store must **prompt before install** (replacing the CLI's human-edit substitute). The vault UI already exists (`AddSecretModal`, `/api/secrets` `internal/gui/secrets.go:16`); the store detects required secrets from the entry and gates install on their presence, reusing that modal. **Mandatory for remote-http** (§10.4): a missing `${secret:KEY}` fails install before any client-config write.

7. **uvx/npx/pip runtime probe** — `Preflight` already does `exec.LookPath(m.Command)` (`install.go:1499-1501`) and fails loud if missing, but there is **no GUI-facing pre-install probe**. The store should probe the entry's `command` (`uvx`/`npx`/`pipx`) up-front and show "runtime missing — install Node/uv" guidance rather than failing only at install time.

### 10.4 G6 remote-http install shape (the simpler one-click path)

G6 (`2026-05-12-g6-remote-mcp-manifests-design.md`) adds `transport: remote-http` — a remote HTTPS endpoint with **no local daemon, no port, no scheduler task** (`:14-18`). It is already partially wired into G5: `generateRemoteHTTPDraft` (`marketplace_generate.go:45-98`) emits `transport: remote-http` + `url` + `client_bindings` for `transport:"http"` catalog entries (e.g. the `context7` seed, `2026-05-13-g5-marketplace-draft-import.md:336-347`). For the store:
- The one-click flow is **simpler** — no uvx/npx download, no port allocation (§10.3#4 skipped), no daemon spawn. Install just expands `${secret:KEY}` headers from the vault at install time and writes URL+headers to client configs (`g6-design:73-81`); Preflight short-circuits for remote-http (`install.go:1480-1496`).
- Secret prompt (§10.3#6) is **mandatory**: a missing secret fails install **before** any client-config write (`g6-design:67-68`); the store pre-collects the token via the vault modal before firing install.
- **Adapter capability matters (ties to §9):** antigravity has **no remote-http support** (relay-tuple only, `g6-design:96-97`) — the store must hide/disable remote-http entries for that client or surface the refuse-with-WARN.
- The store UI must **distinguish the two install shapes**: local-stdio (downloads + spawns a daemon) vs remote-http (writes a URL+token to client configs).

**§10 acceptance:** (1) every store install persists a manifest under `defaultManifestDir()` (the §10.0 regression guard — assert the manifest file exists post-install). (2) port auto-allocation is a config-driven **race-free** allocation (retain-listener or atomic `AllocatePort`-against-`Registry`), never a literal and never the bare `portInUse`-scan path (guard-test greps for live-band literals across the test tree too — §15 P2 / #8a test convention). (3) install progress streams to `/api/events`. (4) secret-bearing entries gate install on vault presence. (5) runtime-probe surfaces uvx/npx/pip-missing before install. (6) remote-http entries take the daemon-less path and are hidden/disabled for antigravity. **(7) the store NEVER execs a catalog `command` without an explicit per-install confirmation of the resolved command line (§15 P1-d — untrusted-catalog RCE).** **(8) manifest-create rejects names that shadow an embed-FS manifest (§15 P2 / §10.0).**

---

## §11 Full backlog (what's left)

Categorized open/deferred inventory. Phase 3B-II GUI, v0.5 supervisor, and serena dynamic-pool are **mostly SHIPPED on master** (PRs through #278); what remains there is polish, residual P2/P3 bugs, and verify-against-HEAD plan-tail items. The single largest open block is the §0–§8 + §9 + §10 redesign itself.

### 11.1 Redesign lane (largest open; §0–§8 + §9 + §10)
A→F legacy-removal, §3 connection-robustness, and feature bugs #4/#6/#8/demigrate. The redesign gaps the architecture review first flagged here are now **CLOSED by Appendix A (implementation elaboration) + §15 (3-angle review-loop findings)** — each line below points to where it is resolved:

- **Per-phase acceptance criteria / test plan** — RESOLVED in §5.x (per-phase Done-gate / Test-surface / Falsification-test triad, mirroring the v0.5.0 supervisor outline).
- **LSP router named but undesigned** — partial: §3.x covers the LSP-router fail-loud trigger; first-class LSP-router design (modes/lifecycle/LazyProxy singleflight) stays a Phase-8 item, and its fail-loud must inventory its OWN session stores (§15 P2, NOT "mirror serena"). The `didOpen/didClose` no-refcount multi-agent bug (`lazy_proxy.go`) stays a distinct OPEN adjacent finding (Appendix A "Adjacent findings" #1).
- **Phase E intent-collapse mechanics** — RESOLVED in §5.1-E (the one-time in-place merge, the 5 surviving readers, where `IsActiveStop`/TTL/`IntentReason` move) + §12 Phase 4 E1/E2 two-step + §15 concurrency fixes. The #6/E ordering IS specified (E first — §12 Phase 4-tail + §5.1-E).
- **§3 fail-loud mechanism design** — RESOLVED in §3.x (layer holding the session handle, 3-store teardown, backend-loss detection signals, SSE/HTTP teardown, deterministic reproduction harness → regression test) + the §15 P1-a 3-store + reverse-index amendment.
- **§2 port-ownership migration** — RESOLVED in §2.x (the three-owner inventory, the stale-`ports.yaml` finding, the band-authority cutover that cannot repeat the killed-live-daemon class) + §15 #8a/#8b split + test-tree guard-grep.
- **macOS/Linux posture post-scheduler** — partial: §15 P1-b adds the supervisor-liveness-recovery requirement before C/D delete the watchdog; the full GA/beta/preview restatement + what the autostart shim becomes on POSIX stays a Phase-8 / Linux-lane (§11.7) item.
- **ADR/risk register/rollback story** — RESOLVED in §0.x (per-phase reversibility/recovery/deploy-gating register) + §12 Phase 4 pre-merge backup + §15 two-milestone split (defers the persistent-state risk behind the safe A→B→§3→C front).

### 11.2 GUI screen polish (Phase 3B-II gaps; most screens shipped)
- Servers matrix: RAM/Uptime/Status columns + row drawer (manifest preview, lifetime stats, Stop & Restart) — DEFERRED. `Servers.tsx`
- Dashboard expanded card (green/red dot, RAM, Uptime, connected-clients, req/min, RAM sparkline, View-logs link, retry count) — DEFERRED. `Dashboard.tsx`
- Logs regex/substring filter + amber/red highlight + Open-folder — OPEN (Round 1). `Logs.tsx`
- Status shapes (●/○) color-blind affordance — OPEN. Density variables consumed across all screens — OPEN. `app.tsx`/`style.css`
- `appearance.layout` registry + sidebar/tabs switcher; top-tabs alternative — OPEN/DEFERRED. `settings_registry.go`
- SVG sprite icons replacing unicode/emoji — DEFERRED cosmetic.
- About: README/INSTALL/verification docs links — OPEN. `About.tsx`
- Add/Edit: `cwd` in manifest model + file/env-prefix env selectors — DEFERRED. `manifest.go`/`SecretPicker.tsx`
- Servers `unsupported` routing-mapper output — DEFERRED. `routing.ts`

### 11.3 Daemon lifecycle / supervisor residuals (v0.5 mostly shipped)
- STOP→failed→stuck→won't-restart — = §4 Phase A (live paper-search bug).
- Supervisor loses `current_pid` → false quarantine (Layer B adopt-on-port) — OPEN. `bugs/2026-06-09-supervisor-loses-current-pid-false-quarantine.md` (Layer A fixed; Layer B follow-up open).
- Deep-sec P3/LOW residuals (Conc-F4/F5/F7, Reg-F2) — DEFERRED. `bugs/2026-06-09-supervisor-deep-sec-p3-residuals.md`.
- `--strict-job-protection` flag; job-protection auto-remediation / metrics export / alerting — DEFERRED v0.5.x (CLAUDE.md Job Protection runbook).
- Disjoint GUI-vs-daemon port ranges (DM-2 root cause) — OPEN (fixed properly by #8/§2).
- Tray menu hangs after long uptime / state-event flood — OPEN (needs profiling + throttle/decouple).
- api-surfaces status/restart/cleanup race — still-relevant-low. `bugs/2026-05-12-api-surfaces-status-restart-cleanup-race.md`.
- macOS containment / kqueue lifecycle watcher — DEFERRED to v0.6 (v0.5 ships macOS preview, process-group only).

### 11.4 Serena residuals (dynamic-pool shipped #246–#277)
- Serena-unified plan tail items (A.1/A.2/B.1/D.1/D.3/E.1/E.2/F.1–F.4/G.1/G.2/H.1–H.3) — OPEN per TRIAGE; most superseded by shipped PRs — **verify against HEAD**.
- Handshake / dynamic-port discovery (Decision 4 / §7) — DEFERRED to v2.
- Cross-workspace symbol-call (`find_referencing_symbols`) — OUT v1, v2 feature.
- No-path-args sticky-session default-workspace fallback — RESOLVED; broad routing edge stays a verify item.
- `didOpen/didClose` no per-client refcount — OPEN hidden multi-agent bug. `lazy_proxy.go`.
- Phase H operational hygiene tooling (aggressive-cleanup CLI/GUI, upstream codex per-subagent reap) — DEFERRED.
- #278 LSP-orphan reconcile guard + migrate-timeout — **MERGED** (2c7c343 on master).

### 11.5 LSP lane (Servers-matrix revamp; draft, partly shipped)
- LSP router untrusted auto-register hardening — FIXED on `security/lsp-trusted-root-gate` (#272/#273); confirm merged. `bugs/2026-06-09-lsp-router-untrusted-auto-register.md`.
- Servers-matrix LSP-bridge + per-daemon env overlay — core shipped (#266/#268/#274); spec open questions remain (auto-discovery scope for non-LSP gdb/godbolt/perftools `required_binaries`; `lldb` empty required_binaries; `workspaces.yaml` missing/corrupt GUI warning banner).
- Linux/macOS systemd/launchd PATH inheritance for LSP — OUT of scope.

### 11.6 Marketplace + remote-MCP (most shipped; G6 deferred)
- **G6 Remote MCP manifests** (`url+headers+secrets`, context7 first-class) — DEFERRED to v0.4.x; spec exists, impl not done — **feeds §10.4**.
- G4 unified Hub MCP endpoint — SHIPPED (#155–#160); verify.
- G8 config watch / fsnotify dev reload — DROPPED (re-evaluate on demand).
- G2/G3 redaction + populated-e2e-coverage — still-relevant-low. `bugs/2026-05-08-g3-*`.
- Marketplace native-http (`transport:"http"`) generate — gated, refers operator to G6.

### 11.7 Platform / cross-OS (Linux-server lane F1–F7; entirely DEFERRED)
F1 platform-lane refactor (`internal/platform/{windows,linux,darwin}/`); F2 Linux scheduler systemd user units (~3-5d); F3 `mcphub setup --server` (loginctl linger); F4 headless guards ($DISPLAY/$WAYLAND); F5 journald adapter; F6 macOS `--force --kill` probe (`probe_darwin.go` stub); F7 CI Linux build matrix. Linux/macOS desktop tray + browser focus = explicit NON-GOALS.

### 11.8 Secrets
- "Edit vault shells out" (Secrets screen displays command, doesn't spawn) — DEFERRED. `Secrets.tsx`.
- a3a-vault-concurrent-edit-lww — needs-repro. `bugs/a3a-vault-concurrent-edit-lww.md`.
- Per-server MCP env overrides through the hub — SHIPPED (#268); overlay residuals in §11.5.

### 11.9 Observability / event bus
- §3.6 event-bus completion + fsnotify (missing `daemon-failed`, `install-progress`, `install-done`, `scan-result`, `client-config-changed`) — DEFERRED; **`install-progress`/`install-done` are §10.3#5 prerequisites**. `events.go`/`poller.go`.
- §4.3 HTTP API contract reconciliation (missing `/api/install-all`, `DELETE /api/install/:server`, REST `/api/manifests*`, `/api/backups/clean|content`, `/api/rollback`, scheduler endpoints, bulk `PUT /api/settings`) — DEFERRED.
- §3.5 Backups keep-N enforcement on install/migrate (`BackupKeep`/`BackupsClean` exist but paths call `Backup()` without keep-N) — OPEN. `clients.go`/`install.go`/`migrate.go`.
- Tray menu spec gaps + toasts (auto-restart, manual-action-done, Settings on/off) — DEFERRED.

### 11.10 Testing
- D2 live manual smoke (tray rendering, AttachConsole/windowsgui matrix, single-instance recovery through OS reboot, real Task Manager kill) — OPEN, needs Windows desktop. `docs/phase-3b-ii-verification.md`.
- D3 multi-language workspace smoke (`register D:\dev\proj cpp python rust` live clangd/pyright/rust-analyzer) — OPEN.
- Populated-row matrix E2E (needs client-config seed fixture); real migrate/restart E2E; Dashboard SSE cleanup E2E (CDP) — DEFERRED. Linux/macOS E2E — blocked on scheduler test seam.
- Test-infra leaks: `tests-leak-state-into-production-logs` (gui-side `TestRealClientInitializer` still open after #264), `api-tests-flock-contention`, `install-test-port-9128-collision`/`test-portinuse-flake`, `b1-backup-file-race`, `cli-supervise-statedir-override-ungated-in-production` — all in `work-items/bugs/`. **Memory lesson: subagent `go test` once wiped the live `supervisor-intent.json` and killed the fleet — back up state before subagent tests; the `test_state_path_env` tag only ENABLES the override, tests must `t.Setenv` it.**

### 11.11 Docs / governance
- CHANGELOG.md — verify present (G10). G1 README feature/readiness matrix — SHIPPED; needs ongoing accuracy ("Linux scheduler install not ready"). Canonical-source: this redesign spec is now the §9/§10 owner — keep it updated with each phase.

---

## §12 Sequencing

Each phase = **one PR through fable → bot → merge → redeploy** (per the redeploy-always discipline; every redeploy is a FULL supervisor restart that interrupts running serena/LSP daemons — batch where possible). The dependency spine is: #278 → STOP → fail-loud → legacy-removal → unified-intent → scheduler-drop, with multi-agent and the GUI store riding the clean layers, and the backlog interleaved where each item touches the same surface.

### Two milestones (consultant synthesis, §15)

The plan covers **two products**: the live-pain fix (the owner's own fleet) and the adoption surface (new external users). Split them so v0.6-core can ship and stabilize the fleet without waiting on the much larger, lower-urgency adoption work:

- **v0.6-core** — **A (done) → B → §3-fail-loud → C → D → E → F**, plus **#8a** (test-port convention, lands BEFORE C), **#4** (hash→name, display-only), and **demigrate-serena-router**. This closes ALL live pains. The minimal first cut **A → B → §3-fail-loud → C touches NO persistent files** (B is display-only at `health.go`; §3 is router-session lifecycle; C is a one-line `setup.go` call removal) — so the riskiest persistent-state work (E/F) is deferred behind the safe, reversible front of the milestone.
- **v0.7-adoption** — **§9** (10 client adapters) → **§10** (GUI MCP Store) → **§13** (npm delivery) → **§14** (onboarding bundle), plus **#8b** (ports.yaml→runtime owner, lands AFTER F). Larger, lower-urgency, and gated on its own supply-chain / RCE controls (§15 P1s d/e).

The phase numbering below maps onto these milestones; v0.6-core = Phases 1–4 (+ the Phase 4-tail idle-shutdown #6, a v0.6-core item that depends on E), v0.7-adoption = Phases 5–7, and Phase 8 is the cross-milestone backlog-interleave lane (each item rides whichever earlier phase cleaned its surface).

### v0.6-core

**Phase 0 — DONE + DEPLOYED + LIVE-VERIFIED.** #278 merged to master (2c7c343); the STOP fix (Workstream A, PR #279) is **merged (98ce92f), deployed, and live-verified on the production fleet** — see §4. **Gate (now the standing post-deploy gate #0 below):** after the clean supervisor restart, the §3.1 status-misreport bug should be re-checked — note that §3.1 (status display) and §3 (router zombie) are SEPARATE bugs (§15 P1-a / SPEC-CORRECTION); the deploy fixes neither by itself (B fixes §3.1, the router fail-loud PR fixes §3).

**Phase 1 — §3 fail-loud router PR (SEPARATE from the STOP PR — §15 P1-a).** This is a DISTINCT PR from Phase A (which already landed) and from B — it lives in a different layer (the serena/LSP **router**, `internal/gui/serena_router_session.go`, not `install.go`). The §3 zombie fix MUST:
- tear down **all THREE** session stores on backend loss — today `coordinateExpiredRouterSessionUnbind` (`serena_router_session.go:510`) unbinds only `serenaDaemonSessions` + the sticky `sessionRouter` (2 stores), and its own comment assumes the caller already removed the `routerSessionStore` entry. On backend loss there is no such caller, so `routerSessionStore` stays live and `/serena/mcp` keeps returning 200 → the exact 2026-06-10 zombie. The fix needs **3-store teardown + a NEW workspace→session-id reverse index** (so the router can find every session bound to a dead daemon's workspace) + a **regression assert that the `routerSessionStore` entry is gone** after the backend-loss event (not just the daemon/sticky entries).
- use the §3.x mechanism design (child-exit event preferred; IPC-status reconciliation fallback; forward-failure unbind as the always-on floor) and the deterministic reproduction harness in `internal/gui/serena_router_lifecycle_test.go` that drives the REAL exit-event path, not a store delete.
- **#4 hash→name** (display-only, independent) and **demigrate-serena-router** (independent) ride here as small sibling commits or adjacent PRs.

**Phase 2 — GUI fail-loud (Workstream B, §5).** Independent; can run parallel to Phase 1. Stop surfacing legacy rows when IPC down; show "supervisor down — restart" (fix site: `health.go:399-401` IPC→scheduler silent fallback + `gui/server.go:81`). Pairs naturally with the Phase-1 router fail-loud (both are "fail loud, no zombie state") but is a different layer (status display vs session lifecycle). **#8a lands here or just before C:** the test-port convention (any value reaching `killByPortFn`/`net.Listen` uses `pickFreeLocalPort(t)`/`:0`) + a guard-grep that **greps the TEST tree too** — the Port:9200-killed-live-daemon incident was a TEST literal hitting a live daemon, so a non-test-only grep would not have caught it (§15 P2 / fable). #8a is the cheap, no-persistent-file half of #8; the runtime-owner half (#8b) lands after F.

**Phase 3 — legacy removal, split 3a → observe → 3b (Workstream §5; watchdog removal). Full design: §15 P1-b (architect Phase-3 pass).**
- **#8a precedes everything** (DONE — `fix/test-port-guard`): the test-port convention + test-tree guard-grep, so 3a/3b test churn cannot re-introduce a live-band literal.
- **3a — additive supervisor-liveness recovery (§15 P1-b), watchdog UNTOUCHED.** Add a tiny `\mcp-local-hub-liveness` task (~1-min) running `mcphub supervise --ensure-alive`, which probes `SupervisorRunningUnderStateDir` (`supervisor_lock.go:265`, flock-authoritative, fail-closed) and relaunches the owner via `schtasks /Run /TN \mcp-local-hub-supervisor` when no live lock holder. NOT `RestartOnFailure: true` (the Win11 24H2 force-kill bug the watchdog was built around — §15 P1-b). Additive + reversible; proves owner-death recovery on the live fleet while the watchdog net is still present (they target DISJOINT things — owner-relaunch vs daemon-revival — so no fight). Depends on A (deployed). **Observe ≥1 session** before 3b.
- **3b — C + D together (one PR; the watchdog is gone in one redeploy).**
  - **C** — drop `setup.go:471` `InstallWatchdogTask()` auto-install + idempotent uninstall on existing hosts (`autostart/windows.go:116-137` already best-effort-uninstalls `\mcp-local-hub-watchdog` on `autostart enable`; C closes the `mcphub setup` gap). The liveness task install lands at the same site (one-for-one swap).
  - **D** — delete the watchdog engine. **MIGRATE `splitJSONLines` out of `watchdog_log.go` FIRST** (`gui_event_log.go:256` depends on it). **KEEP `intent_audit.go`** (supervisor event log needs `AuditIdentityFieldByteCap`). SAFE DELETE: `recovery.go`, `watchdog_state.go`, `watchdog_xml_validator.go`, `cli/watchdog.go` (the command — ENTIRELY, no read-only stub), `scheduler/scheduler_watchdog_xml.go`, the `Install/UninstallWatchdogTask` API surface + `WatchdogTaskName` (edit the `install.go:432` skip-filter). Removing `recovery.go:299` drops the 6th `IsActiveStop` reader → 5 remain (Phase E). Depends on C. **Gate D: "kill the supervisor mid-session → back within ≈1 min with ZERO watchdog code"** (unit + integration + falsification triad — §15 P1-b).

**Phase 4 — collapse dual-intent E (two-step) + drop scheduler/migration F (Workstream §5).** This is the persistent-state-mutating tail of v0.6-core — the highest-convergence risk (all 3 reviewers flagged Phase E concurrency, §15). Mandatory safety nets baked in:
- **Pre-merge safety (§15 P1-c + consultant):** (i) a `--check`/dry-run merge mode (pure function, run on the LIVE state-dir BEFORE deploying E to prove the merge result); (ii) a **code-baked pre-merge backup** to `<state-dir>/pre-collapse-backup-<ts>/` taken automatically by the merge path itself (not a manual step); (iii) a **single named merge owner** that holds the `daemon-intent.json` flock across the ENTIRE read→merge→write→delete (universal lock order migration.lock→supervisor.lock) — NOT only across the read. Holding only the read lock loses a concurrent old-binary `mcphub stop` that writes `daemon-intent.json` after the read but before the delete (§15 P2 / fable).
- **E1 — merge + repoint readers; `daemon-intent.json` STAYS on disk, unwritten.** All 5 surviving `IsActiveStop` readers (after D removes `recovery.go:299`: `supervisor_controller.go:710`, `supervise_reconcile.go:148`, `tray/state.go:220`, `gui_tray_state.go:165`, `api_surfaces.go:674`) learn to read stops from `supervisor-intent.json`. The file-on-disk in E1 is a FREE recovery point and avoids the multi-process reader-inconsistency window during the redeploy: the 6 `IsActiveStop` surfaces span the supervisor + GUI/tray + status tree, so an old-binary surface still on `daemon-intent.json` after a same-PR delete would read "file absent → no stops" = fail-open un-suppresses a stopped/disabled daemon (§15 P2 / fable + architect + consultant convergence).
- **Observe**, then **E2 — delete `daemon-intent.json` + `install_intent.go` writers** once every reader is confirmed on the new path. Resolve the §11.1 intent-collapse mechanics first (where `IsActiveStop`/TTL/`IntentReason` move — see §5.1-E). Use a `stops:` sub-block inside `supervisor-intent.json` rather than widening `SupervisorDaemon` itself (architect §15: avoids a 31-caller round-trip and keeps descriptors-vs-stop-overrides separated → narrows, not widens, write-contention).
- **F** — global daemons → supervisor-intent; remove `install --rollback-to-legacy`+`internal/migration/`+`migrate-legacy` (largest). Depends on E2.
- **Sequence #6 idle-shutdown's `IntentReasonIdle` AFTER E lands** (it writes the file E is rewriting — do not author it on the soon-deleted dual-intent).
- **#8b lands AFTER F:** promote `configs/ports.yaml` to the **runtime** port owner, but **reconcile its stale data FIRST** — it still lists legacy `serena/unified=9121` and an empty `workspace_scoped`, modeling zero of the dynamic pools (§15 P2 / §2.x SPEC-CORRECTION). Promote the band-authority model (reserved pool ranges, not static ports) per §2.x; C→F stays single-variable so #8b is isolated.

**Phase 4-tail — idle-shutdown (#6, §6).** Lands AFTER Phase 4-E (needs the unified intent + the §4/Phase-A corrected stop path — do NOT author a second stop path). `desired=stopped`+`IntentReasonIdle` on the unified intent; 60s in-GUI sweeper (lives in the GUI process — the only one that observes `/serena/mcp` activity; the supervisor sees the stop/clear via IntentWatcher + reconcile); router clears + 503-retries on wake; GUI-configurable threshold + cold/warm wake-mode. Open verify: serena `.serena/` re-warm cost drives the default.

### v0.7-adoption

**Phase 5 — multi-agent registration table (§9).** Lands once the client-config adapter layer is stable post-Phase-4 (E/F touch reconcile + intent that the adapter set keys off). Collapse the 4 duplicated canonical-set literals into one registration table; add GUI per-client enable/disable; ship the antigravity relay shape as the documented non-HTTP path. Each new adapter is then one table entry + one ~80-line file + demigrate symmetry tests. **Adapter tiers (consultant §15):** Windsurf / Cline / Zed / Kiro are verifiable-first (known config locations); Hermes / OpenClaw / OpenHands / OpenCode are experimental (config researched at build).

**Phase 6 — GUI MCP Store (§10).** Depends on Phase 4 (#8b port owner) + the §11.9 `install-progress`/`install-done` events. The hard dep on the §9 table is **dropped** (architect §15 P2): use a narrow per-adapter remote-http capability flag instead of coupling the whole store to the table refactor. Sub-sequence: (6a) `GET /api/marketplace`+`POST /api/marketplace/refresh` + Store screen + nav link (thin G5 wrappers); (6b) `POST /api/marketplace/install` orchestrator (generate→port-fill→**manifest-create-to-disk per §10.0**→install) + runtime probe + secret-prompt gate; (6c) install-progress SSE streaming; (6d) **G6 remote-http** install path (§10.4) + antigravity hide/disable. **Two hard security gates (§15 P1-d + P2):** (i) the one-click flow MUST surface the resolved `command + args + env` and require an explicit per-install confirm — `Install` execs `m.Command` (`install.go:1499`) from an UNTRUSTED catalog, so auto-firing generate→create→install with no command/args gate is RCE; the manual generate→edit→create path stays for uncurated entries. (ii) port auto-allocation MUST be the retain-listener (`net.Listen(":0")`+hold) or atomic `AllocatePort`-against-the-live-`Registry` path — the bare `portInUse`-scan-then-create alternative has a TOCTOU window (§10.3#4) and is FORBIDDEN. Also reject manifest names that shadow an embed-FS manifest (§15 P2 / §10.0 shadowing). The §10.0 manifest-persistence regression guard is a hard gate on 6b.

**Phase 7 — npm delivery (§13) + onboarding (§14).** **§13 is a hard GATE on F + a clean-Windows smoke (consultant §15).** npm delivery MUST ship its supply-chain envelope, not just feasibility (§15 P1-e): pin every platform package by exact version + `integrity` hash in the shipped lockfile; bundle the binary INSIDE the optionalDependency tarball (NO `postinstall` network fetch — dependency-confusion + arbitrary-fetch surface); publish with npm provenance/signed releases; if a download path is unavoidable, verify a pinned SHA-256 before exec. §14 onboarding (trusted-root proposal at `mcphub setup`, GUI first-run banner, refusal-error guidance, README) follows the owner-stated order onboarding→install-fixes→hash→name→clients→npm.

**Phase 8 — backlog interleave / future lanes.** GUI screen polish (§11.2), observability event-bus + API-contract reconciliation (§11.9), backups keep-N (§11.9), LSP-router first-class design (§11.1/§11.5) — its fail-loud must **inventory its OWN session stores separately** (§15 P2: "mirror serena" would propagate the routerSessionStore defect), supervisor P3 residuals (§11.3), serena v2 features (§11.4), Linux-server lane F1–F7 (§11.7 — separate strategic lane, not pulled into v0.6 GA), macOS kqueue (§11.3), test-infra leak fixes (§11.10). Each rides whichever earlier phase cleaned the surface it touches.

### Cross-cutting gates for every phase

- **(0) Post-deploy serena/LSP smoke-gate — MANDATORY after EVERY redeploy, before resuming work (§15 consultant, "most underrated").** ~20 lines: assert `mcphub status` green + `POST /serena/mcp initialize` succeeds + ONE serena tool-call + ONE LSP query. §3.1 proved the status itself LIES (it can paint healthy daemons `Restarting` via the scheduler fallback), and serena is the #1 health priority across 6+ redeploys — so a status-only check is insufficient; the smoke must exercise a real MCP round-trip. Capture output to a file (`cmd /c "... > file 2>&1"`) because the windowsgui binary swallows console output under PowerShell (§4 operational note).
- **(1)** per-phase acceptance criteria + test surface (see §5.x for the per-phase Done-gate / Test-surface / Falsification-test triad).
- **(2)** E2E spec additions where the phase changes observable GUI behavior (fail-loud, idle, hash→name, store).
- **(3)** state-file backup before any subagent `go test` over `internal/api`/`internal/cli` (§11.10 fleet-wipe lesson — a test that forgets to `t.Setenv` the `test_state_path_env` override wipes the live `supervisor-intent.json`).
- **(4)** redeploy = FULL supervisor restart, expect serena/LSP interruption each phase.
- **(5)** each phase is reversible only by `git reset` while local for A–D; **E/F are NOT fully reversible** (they delete persisted runtime files) — the recovery surface there is the `<state-dir>/pre-collapse-backup-<ts>/` backup, not git. There is no v0.4.x rollback net (§0 premise 2), so the live fleet is the safety surface.


---

## §13 npm-based delivery (install simplification — owner-requested research task)

Recovered from the owner plan (transcript L28299/L28300, dropped by the mid-session compaction). Current install is multi-step + intimidating (`build.sh -> install -> ~/.local/bin -> mcphub setup` + supervisor/PATH). mcphub is a SINGLE Go binary, so it can ship the **standard npm path for native binaries** (the esbuild / sharp / @swc pattern):

- Publish per-platform prebuilt binaries as **`optionalDependencies`** (`mcphub-win32-x64`, `mcphub-linux-x64`, `mcphub-darwin-arm64`, ...); a thin JS shim selects the right one at runtime.
- Result: **`npm install -g mcphub && mcphub setup`** — one command instead of manual build-from-source.
- Alternative: a `postinstall` script that downloads the matching binary from GitHub releases.

Feasibility + design is a **research task**. Big adoption win — drops the build-from-source barrier for the free/open core. Ties to the open-core split in `.dev/mcp-local-hub-plan.md`.


---

## §14 Adoption & onboarding bundle (recovered late-session queue)

Recovered from the owner plan (transcript L29257/L29436/L29441/L31414), dropped by the mid-session compaction. The cohesive **adoption/UX queue** that rides alongside multi-agent (§9) + the store (§10). Owner-stated order (L29441): **onboarding -> install-fixes -> hash->name -> clients(+Zed/Kiro) -> npm**.

### 14.1 Onboarding — trusted-root proposal (the UX hole)

A fresh internet user does NOT know about the trusted-root requirement for serena auto-register — auto-register silently refuses an unknown workspace and the user is stuck with no guidance. Onboarding layers so the user is GUIDED, not left searching:

1. **`mcphub setup` proposes at install** (the main fresh-user path): `--trusted-root <path>` flag (repeatable, non-interactive) + an interactive prompt offering the current / common dev roots.
2. **GUI first-run banner** — prompt to add a trusted root on first launch.
3. **Refusal-error guidance** — when auto-register refuses a workspace, the error tells the user exactly how to add it as a trusted root (not a bare refusal).
4. **README** — document the trusted-root model.

Scoped as a follow-up PR after the GUI trusted-roots PR (#273).

### 14.2 Install-fixes

Simplify + repair the existing install flow (`build.sh -> install -> ~/.local/bin -> mcphub setup` is complex + error-prone). Distinct from but adjacent to §13 npm delivery — install-fixes covers existing-path bugs / ergonomics; §13 adds the npm one-command path.

### 14.3 The rest of the queue (already specced — cross-ref)

- **hash->name (UX-hashes)** — `serena - <project>` and `<language> @ <workspace-basename>` instead of `serena-<hash>` / `lsp-<hash>`, for BOTH serena and LSP daemons. = §6 #4 (display-only).
- **clients (+Zed/Kiro)** — the 10-adapter expansion. = §9 / §9.0.
- **npm** — one-command install. = §13.


---

## §15 Review-loop findings (3-angle: architect + fable + consultant) — must-apply

The v0.6 plan went through one autonomous 3-angle review loop (architecture-reviewer + fable security/correctness + consultant strategy, all MCP-verified against HEAD). This section consolidates every finding as an **actionable amendment**, ordered by priority. The phase definitions in §12 already bake these in; this section is the authoritative finding-by-finding record. **CONVERGENCE (all three reviewers, highest confidence): Phase E concurrency/safety** — architect=merge-lock-owner; fable=merge-loses-concurrent-stop + multi-process-reader-inconsistency; consultant=dry-run + backup + split-E1/E2. Fix E hardest.

### P1 blockers (must land before the phase they gate)

**P1-a — §3 fail-loud is BROKEN as written; ship it as its OWN PR (architect).** `coordinateExpiredRouterSessionUnbind` (`serena_router_session.go:510`) unbinds only **2** stores — `serenaDaemonSessions` + the sticky `sessionRouter` — and its own comment ASSUMES the caller already removed the `routerSessionStore` entry. On backend loss there is no such caller, so `routerSessionStore` stays live and `/serena/mcp` keeps returning HTTP 200 → the exact 2026-06-10 zombie. The §3 fix must:

- do **3-store teardown** (add `routerSessionStore` removal to the backend-loss path);
- add a **NEW workspace→session-id reverse index** so the router can enumerate every session bound to a dead daemon's workspace and tear them all down;
- assert in the regression test that the **`routerSessionStore` entry is gone** after the backend-loss event (not just the daemon/sticky entries).

Ship as its OWN PR — it is a DIFFERENT layer (the GUI serena/LSP router) from the STOP PR (`install.go`) and from B (`health.go`). §3 (router zombie, session-lifecycle) and §3.1 (false "Restarting", status-display) are TWO DISTINCT bugs sharing only the "fail loud, no zombie state" principle — do NOT fold §3 into the STOP PR.

**P1-b — Phase C/D delete the watchdog; add minimal liveness recovery FIRST (fable; design hardened by the architect Phase-3 pass 2026-06-10).** Phase D deletes the watchdog. After D, a mid-session supervisor/GUI-owner death (`taskkill /F mcphub.exe`, OOM, panic, parent Job-Object close) leaves the fleet down until the next logon. **Before deleting the watchdog, add a minimal liveness recovery that is NOT the full watchdog.**

> **TOPOLOGY CORRECTION (architect, verified against HEAD).** The autostart comment (`autostart/windows.go:104-110`) claims the watchdog tick is part of *supervisor* crash recovery — this is INACCURATE. The watchdog (`recovery.go`) restarts **scheduler-task daemons** via the scheduler view; it NEVER restarted the supervisor or the GUI process. The autostart task launches **`mcphub gui`** (`superviseArgs` → `["gui"]`, `windows.go:71-76`), and the GUI's exit-monitor only LOGS on supervisor-child death (`gui_supervisor_owner.go:42-49`) — it does not re-spawn. So the real gap Phase D opens is "**GUI/supervisor-owner death → no relaunch until next logon**," a gap the watchdog ALSO never covered. The liveness recovery therefore adds a genuinely NEW capability (owner-death recovery), it is not a like-for-like watchdog replacement — which strengthens the additive-first split below.

**Mechanism — MANDATE the lock-liveness task; REJECT `RestartOnFailure: true`.** The earlier "EITHER flip autostart `RestartOnFailure: true`+`StopExisting`, OR a tiny restart-if-dead task" framing is resolved: `RestartOnFailure` is the WRONG mechanism because (1) Task Scheduler `RestartOnFailure` does NOT reliably fire on Win11 24H2+ force-kill / End Task — the exact documented failure (`work-items/bugs/2026-05-07-task-scheduler-restartonfailure-not-firing.md`) that the watchdog was BUILT to work around; re-adopting it re-introduces the unreliability. (2) It targets the GUI process, so a supervisor-child-only death (GUI parent survives) never fires it. **USE instead:** a tiny `\mcp-local-hub-liveness` scheduled task (~1-min repetition + LogonTrigger, `IgnoreNew`) running a new minimal `mcphub supervise --ensure-alive` action that: probes the already-shipped, flock-authoritative `SupervisorRunningUnderStateDir(stateDir)` (`supervisor_lock.go:265`, fail-closed on probe error); if running → exit 0; if not running → relaunch the owner (`schtasks /Run /TN \mcp-local-hub-supervisor` → the idempotent adopt-or-spawn in `ensureSupervisorRunning`, `gui_supervisor_owner.go:88`); if probe errors → no-op (undeterminable ≠ dead). The GUI/supervisor singleton locks make the relaunch idempotent — no duplicate supervisor. ~30 lines, no new state files, no recovery state machine. Keep autostart `RestartOnFailure: false` UNCHANGED.

**DELETE-LIST TRAPS (architect + Phase-3a-confirmed; a naive "delete the watchdog files" breaks the build OR re-introduces the gate-poison).** Several symbols in the candidate-delete files are load-bearing elsewhere and must MIGRATE, not delete:
- **MUST MIGRATE — `splitJSONLines`:** `watchdog_log.go` hosts `splitJSONLines` (`:465`), which `gui_event_log.go:256` depends on (its own comment at `:274` says so). Move it to a neutral home (e.g. a new `internal/api/jsonlines.go`) BEFORE deleting `watchdog_log.go`; the rest (`AppendWatchdogLog`/`WatchdogLogEntry`) is watchdog-only and the supervisor uses a SEPARATE `SupervisorEventLog` (`supervisor_events.go:191`).
- **MUST MIGRATE — `isMaintenanceTaskName` (CONFIRMED in Phase 3a):** lives in `recovery.go` (a watchdog-engine delete target) but Phase 3a added the `-liveness` suffix to it, making it load-bearing across 5 callers — the `shouldRemoveGlobalWatchdog` uninstall gate (`setup.go:609`), the supervisor status `is_maintenance` flag (`supervise_status.go:168`), and 3 `daemon_env.go` env-override classifiers. If D deletes it with `recovery.go`, the liveness task loses its maintenance classification and the exact gate-poison Phase 3a fixed RETURNS (the last-server watchdog-teardown gate never fires → both the liveness task AND the watchdog become un-removable). Migrate it out.
- **MUST MIGRATE — `canonicalMcphubPathFn` + `currentWindowsUserFn` (CONFIRMED in Phase 3a):** these path/user resolvers live in the watchdog-side files (`api_surfaces.go` / `watchdog_xml_validator.go`) but Phase 3a's `liveness_task.go` (`InstallLivenessTask`) now consumes them. Migrate, do not delete.
- **MUST KEEP:** `intent_audit.go` — the supervisor's own event log depends on `AuditIdentityFieldByteCap` (`intent_audit.go:98`, referenced by `supervisor_events.go:96,273`). After C+D it has no live production caller but stays as shared infrastructure. Only remove the watchdog-SIDE callers, never the file.

**SAFE DELETE (D):** `recovery.go`, `watchdog_state.go`, `watchdog_xml_validator.go`, `internal/cli/watchdog.go` (the command — delete ENTIRELY, no read-only stub: its `status` data sources are all deleted and §0 premise 2 voids "preserve for legacy installs"), `scheduler/scheduler_watchdog_xml.go`, the `Install/UninstallWatchdogTask`/`*Internal` API surface + `WatchdogTaskName`, the watchdog test seams. Edit (not delete) the `WatchdogTaskName` skip-filter at `install.go:432` (drop the skip).

**Split (architect): #8a → 3a (additive liveness) → observe ≥1 session → 3b (C+D together).** 3a adds the liveness task WITHOUT removing the watchdog (proves the new recovery on the live fleet while the old net is still in place; the two operate on DISJOINT targets — owner-relaunch vs daemon-revival — so they do not fight in the 3a window). 3b removes the watchdog; only then is the **Gate D** test honest: "kill the supervisor mid-session → it is back within N (≈1) min with ZERO watchdog code" (a passing recovery while the watchdog still exists could be the watchdog masking a broken liveness task — a false green). Gate-D test triad: unit (live-lock→no-op, free-lock→relaunch-once, probe-err→no-relaunch fail-closed; `t.Setenv` the state override per the fleet-wipe lesson) + integration (`taskkill /F` the GUI → liveness relaunches + gate-#0 serena/LSP smoke + `go tool nm` zero-watchdog-symbol assertion) + falsification (healthy supervisor → zero relaunches; kill a child daemon only → the supervisor's own sweeper recovers it and liveness does NOT fire — proving owner-death vs daemon-death scoping).

**P1-c — Phase E concurrency is the highest-convergence risk (all 3 reviewers).** Two distinct concurrency defects:

- **The boot-merge holds the `daemon-intent.json` flock only across the READ**, not across read→write-unified→delete (fable). During the redeploy window an old-binary `mcphub stop` (a separate process) still writes `daemon-intent.json` via `recordStopIntentAs`. A stop landing after the merge's read but before its delete is silently LOST (fail-open: a just-stopped daemon comes back running). The "idempotent, delete last" framing covers crash, NOT concurrent write. **Fix:** a single named merge owner holds the flock across the ENTIRE read→merge→write→delete; re-read-under-lock immediately before delete and re-merge any delta, or refuse the delete if the file mtime advanced since the read.
- **Multi-process reader inconsistency in the redeploy window** (all 3). The 6 `IsActiveStop` callers span the supervisor (`supervisor_controller.go:710`, `supervise_reconcile.go:148`) + GUI/tray (`tray/state.go:220`, `gui_tray_state.go:165`) + status (`api_surfaces.go:674`); they are NOT one process and do not all flip to the new binary at the same instant. If the merge deletes `daemon-intent.json` in the SAME PR that adds the new readers, any surface still on the old binary reads "file absent → no stops" = fail-open. **Fix: split E into E1 (merge + repoint readers, `daemon-intent.json` STAYS on disk unwritten — a free recovery point) → observe → E2 (delete file + `install_intent.go` writers).** Add a `--check`/dry-run merge mode (pure fn, run on the live state-dir BEFORE deploy), a code-baked pre-merge backup to `<state-dir>/pre-collapse-backup-<ts>/`, and use a `stops:` sub-block inside `supervisor-intent.json` (one file, two sub-records) rather than widening `SupervisorDaemon` itself (which would force a 31-caller round-trip and re-couple descriptors with stop-overrides).

**P1-d — §10 one-click is RCE (fable).** G5 made the human-edit step "load-bearing by design" BECAUSE the catalog is untrusted — the operator eyeballs `command: uvx, args: [pkg]` before it runs. `MarketplaceEntry.command`/`args` flow through `generateStdioDraft` into the manifest, and `Install` execs `m.Command` (`install.go:1499`). The §10.3#3 server-side auto-fill fires generate→manifest-create→install in one call with NO command/args confirmation gate → a compromised or third-party catalog entry → one click → RCE as the user. **Fix:** the one-click flow MUST surface the resolved `command + args + env` and require an explicit per-install confirm (the inspection the CLI forced manually), OR restrict one-click to a signed/curated catalog and route uncurated entries through the manual `generate→edit→create` path. (The store's `/api/install` IS CSRF-protected via `requireSameOrigin`+`requireAllowedHost` — fable confirmed; the hole is the untrusted catalog command, not CSRF.)

**P1-e — §13 npm is supply-chain (fable).** The two delivery shapes have distinct serious surfaces: (a) per-platform `optionalDependencies` are a **dependency-confusion** target (an unpublished or higher-semver public package resolves the malicious one); (b) the `postinstall`-downloads-from-GitHub-releases path is arbitrary code + network fetch at install time with no integrity check. **Fix (make these the research-task acceptance, not "feasibility only"):** pin every platform package by exact version + `integrity` hash in the shipped lockfile; bundle the binary INSIDE the optionalDependency tarball (no `postinstall` network fetch); publish with npm provenance / signed releases; if a download path is unavoidable, verify a pinned SHA-256 before exec.

### P2 (address in the owning phase)

- **§8a / §8b split (consultant + fable).** Split #8: **#8a** = test-port convention (any value reaching `killByPortFn`/`net.Listen` uses `pickFreeLocalPort(t)`/`:0`) + guard-grep, lands BEFORE C; **#8b** = `ports.yaml`→runtime owner, lands AFTER F. The guard-grep MUST **grep the TEST tree too** — the Port:9200-killed-live-daemon incident was a TEST literal hitting a live daemon (tests run against the real state dir), so the originally-specified non-test-only grep would not have caught the actual incident.
- **§10.3#4 port-alloc TOCTOU (fable).** The "scan the band against `portInUse` **or** `net.Listen(":0")`+retain" alternatives are NOT co-equal: the bare scan-then-create-then-bind path has a wide TOCTOU window (another install/process grabs the port between scan and bind). **Mandate** the retain-listener or atomic `AllocatePort`-against-the-live-`Registry`; FORBID the bare `portInUse`-scan path in §10 acceptance.
- **§10.0 manifest shadowing (architect).** `loadManifestYAMLEmbedFirst` (`manifest_source.go`) loads embed-FS-FIRST, so a GUI-written manifest whose name collides with an embedded one is SHADOWED. The store must REJECT shadowing names at manifest-create.
- **Store→multi-agent-table over-coupling (architect).** Drop the hard dep of the store (§10, now §12 Phase 6) on the §9 registration table (now §12 Phase 5); use a narrow per-adapter remote-http capability flag instead of coupling the whole store to the table refactor. (Original numbering: this was "Phase 7→Phase 6"; the milestone re-sequencing renumbered both.)
- **LSP fail-loud must inventory its OWN stores (architect).** §3.x's "mirror the serena `coordinateExpiredRouterSessionUnbind` pattern" for LSP would propagate the P1-a defect. The LSP-router fail-loud must inventory the LSP session stores SEPARATELY, not copy serena's (already-broken) 2-store unbind.

### P3 (anchor drifts + tense; re-anchor for the implementer)

- `isHubURLShapeEntry` is at `clients.go:522` (the func; the comment is `:513`), **NOT** `:513` as some body text says.
- `StopAll`'s supervisor pass is at the `install.go:2595` area (NOT `:2545`); the legacy `killByPortFn` fallback is at `install.go:2646`.
- The clean-exit DROP branch is at `supervisor_controller.go:665` (`ev.Kind == EvChildExit && currentState == StRunning && supervisorEventIsCleanExit(ev)`), **NOT** `:136-144` (those lines are the `supervisorEventIsCleanExit` helper + const).
- §5.1-E heading "4 daemon-intent readers" lists 5 → it is "**5 surviving** `IsActiveStop` readers" after D removes `recovery.go:299` (the verified pre-D total is **6**, not 4; `restart_supervisor.go` is NOT an `IsActiveStop` reader).
- §4 / Workstream A prose was future-tense for landed-and-deployed work → now past tense (done above).

### Consultant strategic frame (non-blocking but adopted)

- **Two-product split** → the v0.6-core / v0.7-adoption milestones (§12). Minimal first cut A→B→§3→C touches NO persistent files.
- **Phase E blast radius includes the repair tools themselves** (serena/codegraph ARE mcphub clients) → the 3 nets: dry-run merge + code-baked backup + cheap canary (copy state-dir, override, diff). NO full staging environment needed.
- **Isolation is STATE-level, not build-tags** → the E1/E2 split (file-on-disk in E1 = free recovery point); same shape for F.
- **§3-router-fail-loud = a SEPARATE PR, not inside the STOP PR.** Adapter tiers: Windsurf/Cline/Zed/Kiro verifiable-first; Hermes/OpenClaw/OpenHands/OpenCode experimental.
- **Most-underrated missing artifact → the post-deploy serena/LSP smoke-gate (gate #0, §12)** — §3.1 proved the status itself lies; serena is the #1 priority across 6+ redeploys.
- Side note (pack-hygiene, not a plan finding): an external review run reported the bugfix-discipline hook false-positive-blocked its Write.


---

# Appendix A — Implementation elaboration (2026-06-10, architect pass)

Closes the §11.1 gaps with verified file:line anchors. Was the review-loop artifact; its findings now feed §15.

> **NOTE (consolidation 2026-06-10):** the 8 inline **[SPEC-CORRECTION]** markers below recorded where the body was stale at review time. **All have since been APPLIED to the body** (§4 is past tense + DEPLOYED; §5/roadmap say "5 surviving readers"; §3.1 carries the verified `health.go:399-401` mechanism; §3 and §3.1 are split; §2 names the stale-ports.yaml finding; §12 specifies the #6-after-E ordering). The markers are RETAINED as the verified-anchor rationale — each quotes the ORIGINAL stale text it corrected, so where a marker quotes "§5 line 53 says ... 4 readers" that quote is the historical pre-fix wording, not the current body. §15 is the authoritative live findings list; this Appendix is the supporting evidence.

## §5.1 Legacy → bugs traceability

This maps each legacy-removal phase to the concrete bug class it removes, with the live code anchor for both the **defect mechanism** and the **fix site**.

### The dual-model substrate (why these bugs share one root)

mcphub today runs **two** desired-state models simultaneously:

1. **v0.5.x supervisor model** — `supervisor-intent.json` (descriptors) + the pure restart state machine `api.Transition` (`internal/api/supervisor_state_machine.go:47`), driven by the controller (`internal/cli/supervisor_controller.go`). Child exits feed `EvChildExit`; the clean/non-clean distinction is the `clean_exit` body flag (`supervisor_controller.go:144`).
2. **v0.4.x legacy model** — `daemon-intent.json` (3-state stop overrides: `DaemonIntent{Desired,Reason,UpdatedAt}`, `internal/api/daemon_intent.go:219`) + the scheduler/watchdog/recovery engine (`internal/api/recovery.go`, the `\mcp-local-hub-watchdog` task).

Every bug below is an **interference pattern between the two models**. Removing the class (Phases A–F) removes the interference, not the instances.

### (a) STOP → failed → Quarantine churn (live paper-search bug) — fixed by A + E

**Mechanism:** The non-clean exit gate. `api.Transition` routes `StRunning + EvChildExit → StBackoffWaiting "arm timer"` (`supervisor_state_machine.go:125-126`) for any **non-clean** exit; only a clean exit (`clean_exit=true`) observed in `StRunning` is dropped (the drop branch is `supervisor_controller.go:665` — `:136-144` is the `supervisorEventIsCleanExit` helper + const, per §15 P3). The legacy stop path (`StopAll`, `killDaemonByPort`) kills via `taskkill /PID <pid> /F /T` (`internal/api/install.go:2677`) — a non-clean exit — so the reaper respawns before the 60-second `IntentWatcher` poll (`internal/cli/supervise_watcher.go:107-109`, `pollInterval = 60s`) reads the fresh `daemon-intent.json` stop. The race re-stops, climbing the failure counter toward `StQuarantined` (`supervisor_state_machine.go:182-185`, `Failures >= 10`).

**Fix site (A — already implemented):** `Stop`/`StopAll` are now supervisor-aware. `StopAll` records stop intent first (`recordStopIntentAs`, `internal/api/install_intent.go:235`) then calls `stopSupervisorOwnedDaemons` (`install.go:2614`), which drives `EvIntentUpdate{stopped}` → `StRunning → StExiting` (`supervisor_state_machine.go:128-129`) — a deliberate stop with no respawn. The shared target selector is `selectSupervisorOwnedTargets` (`internal/api/restart_supervisor.go:29`), mirroring `restartSupervisorOwnedDaemons` (`restart_supervisor.go:83`).

> **[SPEC-CORRECTION — §4 / Workstream A]** §4 is written in future tense ("FIX: make `Stop`/`StopAll` supervisor-aware … Add `stopSupervisorOwnedDaemons` mirroring `restartSupervisorOwnedDaemons`; wire at `install.go` Stop (~:2200) + StopAll (~:2545)"). **This is already in the tree.** `stopSupervisorOwnedDaemons`, `recordStopIntentAs`, `selectSupervisorOwnedTargets`, and the `stopSchedulerFactory` test seam (`install.go:638`) all exist; `StopAll`'s supervisor pass is at `install.go:2595-2654` (not ~2545), and the comments read "spec §4 Phase A.1". The roadmap table (line 83) correctly marks A **DONE (8ab8a42)**, but §4's prose contradicts it. Update §4 to past tense and re-anchor the line numbers (`StopAll` at `install.go:2595`; the `killByPortFn` legacy fallback at `install.go:2646`).

**Phase E's contribution:** E collapses `daemon-intent.json` into `supervisor-intent.json` so there is no second file the 60s `IntentWatcher` must poll and no window where the supervisor's in-memory `daemonIntentCache` (`supervisor_controller.go:168`) is stale relative to a just-written `daemon-intent.json`. A removes the *symptom* on the live fleet today; E removes the *race window* permanently.

### (b) hub-down / false "Restarting" status (§3.1) — fixed by B + (C/D context)

**Mechanism:** The IPC→scheduler silent fallback. `computeDaemonsSection` (`internal/api/health.go:321`) is the single owner of `/api/status` + `/api/health` daemon rows. When the supervisor IPC seam returns `ErrSupervisorIPCUnavailable`, it **silently falls back to the legacy scheduler scan** (`health.go:399-401`: `if errors.Is(fetchErr, ErrSupervisorIPCUnavailable) { rows, fetchErr = a.StatusContext(parentCtx) }`). `StatusContext` reads the Windows Task Scheduler view (`internal/scheduler/scheduler_windows.go:332` `Status`), whose state vocabulary is normalized by `normalizeDaemonState` (`health.go:947`) — and any unknown/blank scheduler state maps to `"failed"` (`health.go:957-961`). A migrated daemon whose `\mcp-local-hub-*` task was deleted (Phase F's end-state) or is in a transient scheduler state therefore surfaces as `failed`/`Restarting` even while the supervisor-owned process serves traffic on its port.

**Fix site (B):** B makes the GUI/status layer fail loud instead of falling back. Two coupled changes: (1) the GUI's `realHealthBackend.DaemonStatusSnapshot` (`internal/gui/server.go:81`) must surface the IPC error as "supervisor down — restart" rather than rendering the scheduler-derived rows; (2) `computeDaemonsSection`'s `ErrSupervisorIPCUnavailable` branch (`health.go:399-401`) is the production fallback that must be **removed or gated behind a deploy flag** once the watchdog/scheduler tasks no longer exist (post-D/F), because after D/F the scheduler view is *guaranteed* stale.

> **[LANDED — PR #281, Workstream B slice 1]** The IPC-down fallback at `computeDaemonsSection` is **removed** (no longer calls `StatusContext`): on `ErrSupervisorIPCUnavailable` it now sets `rows, fetchErr = nil, ErrSupervisorDown` (`health.go` IPC branch), which propagates to HTTP 500 `STATUS_FAILED` → the Dashboard degraded banner. The SSE `StatusPoller` was a SECOND channel feeding the same Dashboard state; it is now routed through `Server.StatusProvider()` → `DaemonStatusSnapshot` (the same fail-loud snapshot), so a down supervisor emits a `poller-error` event instead of stale scheduler `daemon-state` deltas (PR #281 review P1). Separately, the `normalizeDaemonState` (`health.go`) **wire-enum polarity flipped**: the prior unknown/blank → `"failed"` coercion (`health.go:957-961` in the pre-#281 narrative above) is replaced by unknown/blank → `"unknown"` — the `DaemonRow.State` enum widened 4→5 values (`"running" | "stopped" | "starting" | "failed" | "unknown"`). PR #281 review P2 then narrowed that further so the supervisor's KNOWN degraded/terminal vocabulary is NOT swallowed by `"unknown"`: `"Restarting"/"Backoff"/"Spawning"` → `"starting"` (degraded, recovering) and `"Quarantined"` → `"failed"` (terminal crash-loop give-up); only truly-unrecognized/blank input stays `"unknown"`. The canonical wire-enum is documented in `docs/superpowers/specs/2026-05-07-g2-unified-health-endpoint-design.md:83` and the G2 plan `:183`.

> **[SPEC-CORRECTION — §3.1 root candidate (b)]** §3.1 lists root candidate (b) as "the status snapshot reads a stale/transient Restarting label without re-probing actual port-bound + PID-alive liveness." The verified mechanism is narrower and **more actionable**: the label is not stale-cached at the status layer — it is **freshly computed from the legacy scheduler scan** via the `ErrSupervisorIPCUnavailable` fallback at `health.go:399-401`, then coerced to `failed` by `normalizeDaemonState` (`health.go:957-961`). The fix is not "re-probe port+PID" (the IPC status path already reports authoritative PID/port from `supervisor-state.json`); it is "stop falling back to the scheduler view when IPC is down." This makes (b) a **B-only** fix, with C/D removing the stale scheduler data source entirely. Candidate (a) ("supervisor genuinely restart-looping = the STOP churn") is the **same** root as §5.1(a) above and is fixed by A.

### (c) watchdog FIGHTS the supervisor every 5 min — fixed by C

**Mechanism:** The auto-installed watchdog task. `mcphub setup` calls `a.InstallWatchdogTask()` unconditionally (`internal/cli/setup.go:471`), which imports the `\mcp-local-hub-watchdog` scheduled-task XML (`internal/api/api_surfaces.go:754-771`, `sch.ImportXML(WatchdogTaskName, …)`). That task runs `mcphub watchdog --once` every 5 minutes; the recovery engine (`internal/api/recovery.go:299`) reads `daemon-intent.json` via `IsActiveStop` and restarts any daemon it believes failed — including supervisor-owned daemons the supervisor is deliberately holding in `StExiting`/`StQuarantined`. Two restart authorities, one fleet.

**Fix site (C):** Remove the `a.InstallWatchdogTask()` call at `setup.go:471` and add an idempotent **uninstall** on existing hosts (the API already exposes `UninstallWatchdogTask`, `api_surfaces.go:692`, `sch.Delete(WatchdogTaskName)`). The uninstall-on-upgrade lives in the same `runSetupWatchdog` flow (`setup.go:423`). Depends on A because, until Stop is supervisor-aware, the watchdog is the only thing reviving daemons after the churn — removing it before A would leave genuinely-stuck daemons dead.

### (d) "super-stale code, watchdog + supervisor appeared in dashboard" — fixed by B + D

**Mechanism:** Same fallback as (b), surfacing **legacy task rows**. When IPC is down, the scheduler scan (`health.go:400` → `StatusContext` → `sch.List("mcp-local-hub-")`, e.g. `StopAll`'s use at `install.go:2626`) returns **every** `mcp-local-hub-*` task — including the `\mcp-local-hub-watchdog` maintenance task and any stale per-daemon legacy tasks — which then render as dashboard daemon rows. The supervisor's own rows and the watchdog row both appear because the scheduler prefix match doesn't distinguish maintenance tasks from daemon tasks at that layer (the maintenance filter `isMaintenanceTaskName` lives in the supervisor path, e.g. `restart_supervisor.go:148`, not the scheduler scan).

**Fix site:** B (stop rendering scheduler rows when IPC down) removes the *display*; D (delete `recovery.go`/`watchdog_state.go`/`cli/watchdog.go` + the `InstallWatchdogTask`/`UninstallWatchdogTask` API surface) removes the *source* of the watchdog row. After D, there is no watchdog task to leak into any view.

### Traceability matrix

| Phase | Removes bug class | Defect mechanism anchor | Fix-site anchor |
|---|---|---|---|
| **A** STOP-fix | (a) STOP→Quarantine churn | `supervisor_state_machine.go:125-126` (non-clean→backoff); `install.go:2677` (taskkill /F); `supervise_watcher.go:107` (60s poll) | `install.go:2595-2654` (`StopAll` supervisor pass); `restart_supervisor.go:29` (selector); `install_intent.go:235` (intent-first) — **already landed** |
| **B** GUI fail-loud | (b) false Restarting/down; (d) legacy rows in dashboard | `health.go:399-401` (IPC→scheduler silent fallback); `health.go:957-961` (unknown→failed); `gui/server.go:81` | **LANDED (PR #281 slice 1):** IPC-down fallback removed → `ErrSupervisorDown` → 500 `STATUS_FAILED`; SSE poller routed through `Server.StatusProvider()`→`DaemonStatusSnapshot` (no scheduler fallback, P1); `normalizeDaemonState` enum widened 4→5 (unmapped/blank→`unknown`, P2: `Restarting/Backoff/Spawning`→`starting`, `Quarantined`→`failed`) |
| **C** drop watchdog task | (c) watchdog fights supervisor | `setup.go:471` (`InstallWatchdogTask()` call); `api_surfaces.go:754` (task import) | remove call at `setup.go:471`; add uninstall via `api_surfaces.go:692` |
| **D** delete watchdog engine | (c)/(d) source removal | `recovery.go:299` (`IsActiveStop` restart decision); `api_surfaces.go:722-771` (install API) | delete `recovery.go`/`watchdog_state.go`/`cli/watchdog.go`; remove `Install/UninstallWatchdogTask` |
| **E** collapse dual-intent | (a) race window permanently | `daemon_intent.go:219` (`DaemonIntent`); `supervise_watcher.go:57-59` (`watchedIntentFiles` = both) | unify into `supervisor-intent.json`; migrate 6 readers (see §5.1-E mechanics below) |
| **F** drop scheduler/migration | residual scheduler-view staleness | `install.go:2626` (`sch.List`); `internal/migration/`; `install --rollback-to-legacy` | global daemons → supervisor-intent; delete migration engine |

> **[SPEC-CORRECTION — "4 readers"]** §5 line 53 says Phase E must "migrate the 4 readers (supervisor controller, tray, restart_supervisor)." The verified reader count for `DaemonIntent.IsActiveStop` is **6 call sites**, not 4: `internal/api/recovery.go:299` (watchdog engine — deleted by D, so it drops out of E's scope), `internal/tray/state.go:220`, `internal/cli/gui_tray_state.go:165`, `internal/api/api_surfaces.go:674`, `internal/cli/supervise_reconcile.go:148`, and `internal/cli/supervisor_controller.go:710`. `restart_supervisor.go` does **not** call `IsActiveStop` (it reads `supervisor-intent.json` already). Restate E's reader-migration scope as the 5 surviving `IsActiveStop` callers after D removes `recovery.go`.

---

## §5.x Per-phase acceptance criteria + test plan (mirrors the v0.5.0 supervisor implementation-outline style)

Each phase below has: **Done gate** (the falsifiable PASS condition), **Test surface** (Go unit + Playwright E2E additions, keyed to the current 103-test suite), and a **Falsification test** (the experiment that, if it passes, proves the phase did NOT do its job).

### Phase A — STOP fix (status: landed; this is the gate for re-verification)

- **Done gate:** `mcphub stop <serena daemon>` then `mcphub status` shows the daemon at `stopped` (not `failed`/`Restarting`), and it stays stopped across at least two 60s `IntentWatcher` poll cycles. `mcphub restart <daemon>` revives it.
- **Test surface (Go):** `internal/api/stop_supervisor_test.go` (exists per codegraph blast-radius). Assert: (1) `StopAll` records `Desired=stopped,Reason=user-stop` via `recordStopIntentAs` BEFORE the reconcile; (2) the SM receives `EvIntentUpdate{stopped}` and lands `StExiting`, never `StBackoffWaiting`; (3) the legacy `killByPortFn` path (`install.go:2646`) fires only for non-supervisor tasks.
- **Test surface (E2E):** Servers matrix "Stop all" → daemon row resolves to a **stopped** state and "Run all" revives it. No new spec file needed if the existing servers suite (8 tests) covers the row-state assertion; add one populated-row case once the seed fixture exists (currently deferred per CLAUDE.md "What's NOT covered").
- **Falsification test:** kill a supervisor-owned daemon with `taskkill /F` directly (bypassing `mcphub stop`) and confirm the supervisor **does** respawn it — proving the non-clean→respawn path is intact and A only suppressed *deliberate* stops, not crash recovery. If `taskkill /F` no longer respawns, A over-reached.

### Phase B — GUI fail-loud

- **Done gate:** with the supervisor down (no `supervisor.lock`), `/api/status` returns HTTP 500 `STATUS_FAILED` (it already does on total IPC failure per `health.go:411-430`) and the GUI Dashboard renders "supervisor down — restart" instead of any daemon rows. Zero watchdog/legacy rows ever appear.
- **Test surface (Go):** new test asserting `computeDaemonsSection` does NOT call `StatusContext` on `ErrSupervisorIPCUnavailable` once the fallback is removed (currently `health.go:399-401` *does* call it). Assert `normalizeDaemonState` is no longer reachable from the IPC-down path.
- **Test surface (E2E — NEW spec required):** `internal/gui/e2e/specs/dashboard-fail-loud.spec.ts`. Fixture sets `MCPHUB_E2E_SUPERVISOR=none` (the existing seam, CLAUDE.md GUI E2E section) so no supervisor IPC is reachable; assert the Dashboard shows the fail-loud banner and **no daemon cards**, and the Servers matrix shows the same. This is a genuinely-new observable-behavior surface → adds to the 103-test count.
- **Falsification test:** with the supervisor **up**, confirm normal rows render (B must not break the happy path). With supervisor up but one daemon genuinely `failed`, confirm that one daemon (and only it) shows red — proving B fails loud on real failure, not on transport hiccups.

### Phase 3a — supervisor-liveness recovery (additive; precedes C/D — §15 P1-b)

- **Done gate:** a `\mcp-local-hub-liveness` task (~1-min repetition + LogonTrigger, `IgnoreNew`) is installed and runs `mcphub supervise --ensure-alive`; the watchdog is UNTOUCHED (purely additive). `taskkill /F` the GUI owner → within ≈1 min `SupervisorRunningUnderStateDir` (`supervisor_lock.go:265`) reports running again and the serena pool is back (verified via the gate-#0 smoke, not status alone). Normal operation shows ZERO relaunches.
- **Test surface (Go):** unit-test the `--ensure-alive` action against `SupervisorRunningUnderStateDir` with a `t.Setenv`-overridden temp state dir (per the §11.10 fleet-wipe lesson): live-lock→no-op, free-lock→relaunch-once (recording fake for the `schtasks /Run` step), probe-err→no-relaunch (fail-closed, the guard-precondition check).
- **Falsification test:** with the supervisor healthy, run the action for several ticks → ZERO relaunches (no-op on a live lock; an inverted polarity would relaunch a healthy supervisor). Kill ONLY a child daemon → the supervisor's own liveness sweeper (`supervise_liveness.go`) recovers it and the owner-liveness task does NOT fire (lock still held) — proving owner-death vs daemon-death scoping.
- **Observe ≥1 session** on the live fleet before landing C/D (3b).

### Phase C — drop watchdog task (3b, ships with D)

- **Done gate:** after `mcphub setup` on a clean host, `schtasks /Query /TN \mcp-local-hub-watchdog` returns "not found." On a host that previously had the task, `mcphub setup`/`mcphub install --upgrade` deletes it (`autostart/windows.go:116-137` already best-effort-uninstalls on `autostart enable`; C closes the `mcphub setup` gap). The 3a liveness-task install moves to this same site (one-for-one swap for the removed `InstallWatchdogTask`).
- **Test surface (Go):** extend `internal/cli/setup_watchdog_test.go` (exists) — invert its current assertions: assert `runSetupWatchdog` does **not** call `InstallWatchdogTask` (no `ImportXML` on the injected scheduler) and **does** call `UninstallWatchdogTask` (one `Delete(WatchdogTaskName)`) when the task pre-exists. The existing `schedulerFactoryFn` seam (`api_surfaces.go:78`) records the calls.
- **Falsification test:** start the supervisor, hold a daemon in deliberate `StExiting` (via `mcphub stop`), wait >5 min, confirm the daemon stays stopped (no watchdog revival). If it revives, the watchdog task is still installed/active.

### Phase D — delete watchdog engine (3b, ships with C)

- **Pre-delete MIGRATE (mandatory; a naive delete breaks the build OR re-poisons the uninstall gate — §15 P1-b):** move OUT of the watchdog-engine files (do NOT delete): (1) `splitJSONLines` from `watchdog_log.go` (`gui_event_log.go:256` depends on it); (2) `isMaintenanceTaskName` from `recovery.go` (Phase 3a added the `-liveness` suffix → 5 callers incl. the `shouldRemoveGlobalWatchdog` gate; deleting it re-introduces the gate-poison); (3) `canonicalMcphubPathFn` + `currentWindowsUserFn` (Phase 3a's `liveness_task.go` consumes them). Do NOT touch `intent_audit.go` (the supervisor's `SupervisorEventLog` needs `AuditIdentityFieldByteCap`, `intent_audit.go:98` ← `supervisor_events.go:96,273`) — only remove its watchdog-side callers.
- **Done gate:** `recovery.go`, `watchdog_state.go`, `watchdog_xml_validator.go`, `internal/cli/watchdog.go` (the command — ENTIRELY, no read-only stub), `scheduler/scheduler_watchdog_xml.go`, and the `Install/UninstallWatchdogTask`/`WatchdogTaskName` API surface are gone; the rest of `watchdog_log.go` (post-`splitJSONLines`-migration) is gone; `go build ./...` + `go vet ./...` clean; the `install.go:432` `WatchdogTaskName` skip-filter is edited out; exactly **5** `IsActiveStop` readers remain (after `recovery.go:299` is removed — the verified 6th reader, see §5.1-E).
- **Test surface (Go):** delete `recovery_test.go`, `watchdog_test.go`, `watchdog_state_test.go`, `watchdog_xml_validator_test.go`. The compile-time gate is the clean build + the §5.1-E reader migration. Run the canonical pre-push: `go build ./... && go vet ./... && go test -count=1 ./...` plus the tagged `internal/api/ internal/cli/` run (CLAUDE.md PR Step 1).
- **Gate-D acceptance (the liveness replacement is now the ONLY recovery — honest only with the watchdog deleted):** integration smoke — `taskkill /F` the GUI owner mid-session → the 3a liveness task relaunches it within ≈1 min + the gate-#0 serena/LSP round-trip succeeds + `go tool nm` shows ZERO watchdog symbols in the shipped binary. (A passing recovery while the watchdog still existed could be the watchdog masking a broken liveness task — a false green; that is why C+D delete it before this gate runs.)
- **Falsification test:** `grep` the WHOLE tree (incl `*_test.go`) for `WatchdogTaskName`, `InstallWatchdogTask`, `RecoverStoppedDaemons`, `BuildWatchdogXML`, `AppendWatchdogLog`, `mcphub watchdog --once` outside `docs/`; any live code reference means D is incomplete. Distinguish doc references (update in the same PR — canonical-source-maintenance: the CLAUDE.md "Watchdog" section + `docs/phase-3b-ii-verification.md` D2.6) from live code. (Per memory `feedback_kosyak_surgical_edits_leave_stale_text` — full cross-cutting grep, not a single-section check.)

### Phase E — collapse dual-intent (mechanics in §5.1-E below; criteria here)

- **Done gate:** `daemon-intent.json` and `internal/api/install_intent.go`'s writers are gone; `watchedIntentFiles` (`supervise_watcher.go:57`) lists only `supervisor-intent.json`; all stop/idle/disable intent lives in `supervisor-intent.json`; the live fleet's existing stops survive the in-place merge (serena pool keeps running, paper-search stays stopped if it was).
- **Test surface (Go):** new `internal/api/intent_collapse_test.go`: given a populated `daemon-intent.json` (user-stop on paper-search) + a populated `supervisor-intent.json` (running serena pool), the one-time merge produces a single `supervisor-intent.json` with the paper-search stop preserved (`IsActiveStop` semantics intact: TTL, clock-skew, reason). Reuse the `DaemonIntent.IsActiveStop` table tests (`daemon_intent_test.go:240-303`) against the unified schema.
- **Test surface (E2E — NEW):** `dashboard-idle.spec.ts` is deferred to the Phase 4-tail idle-shutdown (#6); for E itself, no new GUI behavior is observable (it is a persistence-layer change), so no E2E addition — **but** add a guard that the tray/Dashboard still suppress stopped daemons via the migrated `IsActiveStop` readers (`tray/state.go:220`, `gui_tray_state.go:165`).
- **Falsification test:** after the merge, manually re-add a `daemon-intent.json` to the state dir and confirm the supervisor **ignores** it (no longer a watched file). If the supervisor still reacts, E did not fully cut the second reader.

### Phase F — drop scheduler/migration

- **Done gate (as IMPLEMENTED):** `internal/migration/` deleted (the v0.4.x→v0.5.0 forward-migration engine); `mcphub install --rollback-to-legacy` removed (the legacy-demotion flag is gone — only `mcphub install --upgrade`, the cold-restart binary swap, remains); the migration exit codes (`13 ROLLBACK_TOKEN_MISMATCH`, `14 MIGRATION_POWERSHELL_LOCKED`, and the named rollback abort codes) are gone; global daemons (memory/time/wolfram/…) spawn from `supervisor-intent.json` reconcile (via `installPlanCore`'s `superviseGlobal` branch), not from `\mcp-local-hub-*` scheduler tasks; `mcphub setup` creates zero per-daemon scheduler tasks on Windows. **Scope correction:** `mcphub migrate-legacy` is NOT removed — it is a SEPARATE command (M4 Task 14) that converts disabled mcp-language-server client-config entries into managed workspace registrations; it is unrelated to the supervisor forward-migration engine this phase deleted, so it correctly still ships (`internal/cli/migrate_legacy.go`, wired in `root.go`). The control-plane tasks `\mcp-local-hub-supervisor` (autostart) and `\mcp-local-hub-liveness` (3a) PLUS `InstallLivenessTask` are preserved.
- **Test surface (Go):** the migration package's tests (`internal/migration/*_test.go`) are deleted with the package; the fresh-install done-gate tests live in `internal/api/install_parsed_manifest_test.go` (`TestInstallPlanCore_GlobalFreshInstall_WritesSupervisorIntent_NoSchedulerTask`) — they drive `installPlanCore` (the shared owner of the Phase F decision that `Install` (`internal/api/install.go:229`) delegates to) with a global manifest + fake scheduler, assert `supervisor-intent.json` carries the daemon descriptor rows, and assert `sch.Create`/`sch.Run` are NEVER called.
- **Falsification test (Go):** `TestInstallPlanCore_GlobalFreshInstall_NoPerDaemonSchedulerTaskCreated` — fresh global install must create ZERO `\mcp-local-hub-<server>-<daemon>` per-daemon scheduler tasks (the fake scheduler's created-task set is empty). On a clean Windows host the manual equivalent: `schtasks /Query /TN \mcp-local-hub-*` returns nothing per-daemon, yet `mcphub status` shows the global daemons running.

### Feature workstreams

| Workstream | Done gate | Test surface | Falsification test |
|---|---|---|---|
| **#4 hash→name** | CLI status + Dashboard show `serena · <project>` / `<lang> @ <basename>` not `serena-<8hex>` | E2E: NEW assertion in dashboard/servers suite that the rendered label matches `serena · ` + the workspace basename from `/api/status` (the `workspace` field is already in status). Pure display → small E2E delta. | feed a workspace whose path basename collides with the hash prefix; confirm the human name still wins. Confirm a daemon with no `workspace` field falls back gracefully (no empty `serena · `). |
| **#6 idle-shutdown** | serena pool daemon sleeps after N idle min, wakes on next `/serena/mcp`; `IntentReasonIdle` on the **unified** intent; 60s in-GUI sweeper | Go: new `IntentReasonIdle` added to `isKnownIntentReason` (`daemon_intent.go:660`), `IsActiveStop` honors it WITHOUT the user-stop TTL (`daemon_intent.go:320-321` only TTLs `IntentReasonUserStop`); router clears it + 503-retries on wake. E2E NEW `dashboard-idle.spec.ts`: daemon shows "idle (sleeping)" state, next request wakes it. | set idle threshold to 1 min, make a tool call, confirm the daemon is NOT killed mid-call (the sweeper must read last-activity, not wall-clock since spawn). Confirm a `user-disabled` daemon is never woken by an idle wake. |
| **#8a test-port convention** (before C) | guard-grep covers BOTH the non-test AND the test tree (§15 P2); tests use `pickFreeLocalPort(t)`/`:0` | Go: guard-test greps for live-band literals (`9121`/`9123`-`9132`/`9200`-`9299`) reaching `killByPortFn`/`net.Listen` **in BOTH the test tree and the non-test tree**. | introduce a hardcoded `Port: 9200` in a **TEST** path (the actual incident class) AND in a non-test path; the guard-test must fail on BOTH. (Memory lesson: this literal once killed a live daemon — and it was a TEST literal.) |
| **#8b config-centralization** (after F) | `configs/ports.yaml` is the **runtime** port owner (not test-only; stale data reconciled first); daemon ports auto-allocated; tier-A values GUI-settable | Go: a production reader of `ParsePortRegistry` (`config/ports.go:33`) exists and is consulted by `Install`/allocate paths; band-authority model (reserved ranges, not static ports) per §2.x. | promote `ports.yaml` as-is (still listing legacy `serena/unified=9121`, empty `workspace_scoped`); the boot validate must FAIL because it does not model the live dynamic pools. |
| **demigrate-serena-router** | GUI uncheck-cursor-serena succeeds when the entry is the `/serena/mcp` router shape | Go: extend `internal/api/demigrate_test.go` — `liveEntryMatchesManifestBinding` (`managed_entries.go:355`) recognizes `http://127.0.0.1:9125/serena/mcp` as hub-managed. E2E: existing servers-matrix uncheck-via-hub test, but seeded with a router-shape cursor entry. | seed a cursor entry pointing at a *foreign* remote URL; demigrate must STILL refuse it (the widened matcher must not become "remove anything"). |
| **§9 multi-agent table** | the 4 duplicated canonical-set literals collapse to one registration table; adding a client = one entry | Go: drift-guard test asserts the table is the single source for `SupportedClientNames`/`AllClients`/`serenaReconcileClientSet`/`DefaultInstallClientNames`. Per-adapter demigrate symmetry test. | add a client to one literal but not the table; the drift-guard must fail. |
| **§10 GUI store** | every install persists a manifest under `defaultManifestDir()`; port auto-allocated, never literal | Go: post-install assert the manifest file exists on disk (the §10.0 "fetch was lost" regression guard). E2E NEW store screen suite. | install a store entry, then trigger a reconcile, then assert the daemon survives (it would vanish if the manifest were in-memory-only — the exact "fetch was lost" failure). |

---

## §5.1-E Phase E intent-collapse mechanics (the hard one §11.1 flags)

**The problem §11.1 names:** research-mode deleted the migration engine, but the live host has a **populated `daemon-intent.json`** (per-task stop overrides) AND a **populated `supervisor-intent.json`** (running serena pool descriptors). E must merge them into one file **without** the deleted migration machinery.

### What `daemon-intent.json` actually holds (the data to migrate)

`DaemonIntentFile.Tasks` is `map[taskName]DaemonIntent{Desired, Reason, UpdatedAt}` (`daemon_intent.go:219-226`). The behavior to preserve is entirely in `IsActiveStop` (`daemon_intent.go:308-325`):
- `Desired=stopped` is an active stop unless overridden;
- **clock-skew-future** fail-closed (`daemon_intent.go:313-314`);
- **stale bound** (>365d) clears the stop (`daemon_intent.go:317`);
- **TTL** applies **only** to `Reason=user-stop` (`daemon_intent.go:320-321`) — `user-disabled` never expires.

So the merge must carry `Desired`, `Reason`, `UpdatedAt` per task into the unified file's per-daemon record, and the unified-file reader must reproduce `IsActiveStop` exactly (port the pure predicate verbatim — it has no I/O).

### Where `IsActiveStop`/TTL/`IntentReason` move

Move the pure predicate and its constants (`StopIntentTTL`, `ClockSkewFutureTolerance`, `ClockSkewStaleBound`, `IntentReason*`) onto the **`SupervisorDaemon` descriptor** (`supervisor-intent.json`'s per-daemon row). The supervisor already threads `IntentIsActiveStop` into the SM via `SMContext.IntentIsActiveStop` (`supervisor_state_machine.go:33`), and the controller computes it today from `daemonIntentCache.Lookup(...).IsActiveStop(now)` (`supervisor_controller.go:710`). After E, the controller computes it from the unified descriptor's stop fields instead — **the SM input shape does not change**, only the source file. This is why E is low-risk for the supervisor: `api.Transition` is untouched.

### The one-time in-place merge (no migration engine)

Because there is "nothing to roll back to" (§0 premise 2), E does **not** need the journal/rollback machinery in `internal/migration/`. It needs a single idempotent boot-time reconcile:

1. On `mcphub supervise` startup (or `mcphub install --upgrade`), if `daemon-intent.json` exists alongside `supervisor-intent.json`:
   - read both under their existing flocks (`daemon-intent.json` via `ReadDaemonIntent`, `daemon_intent.go:356`; supervisor-intent via `ReadSupervisorIntent`);
   - for each `daemon-intent.json` task with an **active** stop (re-evaluate `IsActiveStop(now)` so expired/stale stops are dropped, not carried), write the stop fields onto the matching `supervisor-intent.json` daemon row (match by canonical leading-backslash `task_name`, `daemon_intent.go:275`);
   - atomic-write the unified `supervisor-intent.json` (temp+rename, same as `WriteDaemonIntent` discipline);
   - delete `daemon-intent.json` and its `.lock`.
2. The merge is **idempotent**: a second boot finds no `daemon-intent.json` and is a no-op. A crash mid-merge leaves `daemon-intent.json` intact (it is deleted last) → the next boot re-runs cleanly. This is the "live fleet is the safety surface" posture (§0 premise 2) — no journal needed because the input file is the recovery point until the rename commits.

> This is **not** a kostyl: the merge names the root cause (two desired-state files) and removes one. It is allowed under §0 premise 2 specifically because there is no compat contract to preserve; the in-place merge is the *minimal* transition, not a workaround that hides a defect.

### The 5 surviving daemon-intent readers to migrate (anchored)

> Using the verified set (see §5.1 [SPEC-CORRECTION "4 readers"]). After Phase D deletes `recovery.go:299`, **5** `IsActiveStop` readers remain, of which these are the ones that read `daemon-intent.json` content and must repoint at the unified file:

1. `internal/cli/supervisor_controller.go:710` — `di.IsActiveStop(now)` feeds `SMContext.IntentIsActiveStop`. Repoint `daemonIntentCache` (`supervisor_controller.go:168`) to read the unified descriptor.
2. `internal/cli/supervise_reconcile.go:148` — `entry.IsActiveStop(now)` in the reconcile spawn/terminate decision. Repoint to the unified per-daemon row.
3. `internal/tray/state.go:220` — tray suppression of stopped daemons. Repoint the tray's intent read.
4. `internal/cli/gui_tray_state.go:165` — GUI tray-state mirror. Same.
5. `internal/api/api_surfaces.go:674` — the `intent.IsActiveStop(now)` helper used by status surfaces. Repoint.

Also migrate the **writers**: `recordStopIntentAs` (`install_intent.go:235`, calls `WriteDaemonIntent`) and `recordRestartIntentForTask` (used by `restart_supervisor.go:114`) must write the unified file. The `IntentWatcher`'s `watchedIntentFiles` (`supervise_watcher.go:57-59`) drops `daemon-intent.json`.

### Ordering constraint with #6 idle-shutdown — sequence #6 AFTER E

`IntentReasonIdle` is a NEW reason value. If #6 lands first, it adds a writer to `daemon-intent.json` (`isKnownIntentReason`, `daemon_intent.go:660`, and the `IsActiveStop` TTL branch at `daemon_intent.go:320-321`) — **the very file E is deleting**. The redesign §12 Phase 4-tail already states this ("Lands AFTER Phase 4-E … do NOT author a second stop path"), and it is correct. Concretely: #6 must (a) add `IntentReasonIdle` to the **unified** schema's known-reason set, and (b) ensure `IsActiveStop` does NOT apply the user-stop TTL to `IntentReasonIdle` (idle daemons sleep indefinitely until a `/serena/mcp` wake clears the reason). Authoring #6 on the dual-intent file first would require re-authoring it on the unified file in E — wasted churn and a window where two reasons live in two files.

> **[SPEC-CORRECTION — §11.1 ordering claim — APPLIED]** §11.1 originally said "Ordering between #6 (`IntentReasonIdle`) and Phase E … is unspecified." §12 Phase 4-tail *does* specify it (E first). This is now resolved: the §11.1 "unspecified" claim has been replaced with a pointer to §12 Phase 4-tail + this §5.1-E section as the canonical resolution.

---

## §3.x Fail-loud mechanism design (zombie-connection regression)

### Which layer holds the MCP session handle

The **serena router** (`internal/gui/serena_router.go`) owns three session stores, all in package `gui`:
1. **router-minted client sessions** — `routerSessionStore` (`serena_router_session.go:101`): maps the client `Mcp-Session-Id` minted at `initialize` to its negotiated protocol version, with 24h idle TTL + LRU cap (`maxRouterSessions = 4096`, `serena_router_session.go:75`).
2. **sticky routing** — the cross-package `sessionRouter` interface (`serena_router.go:39`: `BindSession`/`LookupSession`/`UnbindSession`) maps client session → workspace.
3. **upstream daemon sessions** — `serenaDaemonSessions` (the daemon-side `Mcp-Session-Id`).

The coordinated-unbind owner is `coordinateExpiredRouterSessionUnbind` (`serena_router_session.go:510`): it unbinds an id from the daemon store AND the sticky router together, so a terminated session is gone from all three. The periodic reclaimer is `SweepSerenaSessions` (`serena_router_session.go:473`), wired into the existing `runSessionCleanupTicker` (no new goroutine).

> **This is the layer that must fail loud.** Today the router tears down sessions on **idle TTL** (24h) and on explicit client **DELETE** — but NOT on **backend loss**. That is the zombie gap.

### The zombie mechanism (2026-06-10 incident)

When the hub restarts (or the serena backend daemon dies), the upstream `/serena/mcp` daemon endpoint is replaced/restarted, but the **router's client-side session stores survive** (they live in the long-lived GUI `Server`, not the daemon). The client still holds a `Mcp-Session-Id` the router still considers `routerSessionLive` (`serena_router_session.go:269`), so `/serena/mcp` keeps returning HTTP 200 at the router layer — but every forward to the now-dead/replaced upstream fails or hits a daemon that never saw this session's `initialize`. The client is "connected" (200, live router session) yet effectively dead — exactly the spec's zombie. Only an editor restart mints a fresh session.

### How the router should detect backend loss + tear down

Three detection signals, in increasing fidelity:

1. **Child-exit event (highest fidelity, preferred).** The supervisor already observes child exits via `EvChildExit` (`supervisor_state_machine.go:21`) and the controller's `crashCh` bridge (`supervisor_controller.go:459` "clean exits now flow through crashCh too"). The GUI router subscribes (via the existing `/api/events` SSE bus, `internal/gui/events.go`) to a `daemon-failed`/`daemon-restarted` event for serena-pool daemons. On receipt, the router runs `coordinateExpiredRouterSessionUnbind` (`serena_router_session.go:510`) for **every** session bound to that daemon's workspace — terminating the client session deterministically so the client sees a clean disconnect and re-`initialize`s. (§11.9 already lists `daemon-failed` as a missing event — **this is its first hard consumer**.)
2. **IPC status reconciliation (medium).** On any reconcile tick, if `DialSupervisorIPCStatus` (`internal/api/supervisor_ipc_status_client.go:32`) reports a serena daemon's `pid_generation` advanced (restart) or the daemon absent, sweep that daemon's sessions. This is the fallback when SSE is missed.
3. **Health-probe on forward failure (lowest, but always-on).** When a `/serena/mcp` forward returns a connection error or the daemon's session is unknown upstream, the router does NOT silently retry on the stale session — it unbinds (the three stores) and returns `-32600 "session terminated"` (the same code the expired-on-read path already returns, `serena_router_session.go:461`), forcing the client to re-`initialize`. The router already has the budgets for this: `serenaCleanupTimeout = 5s` (`serena_router_session.go:81`) for detached teardown forwards.

**SSE/HTTP streaming teardown:** the router's forward path already uses detached, short-budget contexts for teardown (`cleanupContext`, `serena_router_session.go:108`) so a hung daemon delays the client by at most 5s, not the 60s `serenaUpstreamTimeout` (`serena_router_session.go:71`). On backend-loss the router must (a) close any in-flight SSE GET stream for the affected sessions, and (b) `UnbindSession` so the next client request is treated as unknown → 503/`-32600` → reconnect. The DELETE teardown path (`unbind`, `serena_router_session.go:376`) is the template; backend-loss is just a new trigger for the same teardown.

### The LSP router (named-but-undesigned, §11.1)

The LSP router (`internal/api/lsp_client_router.go`, the `/lsp/<lang>/mcp` peer) and the per-`(workspace,language)` `LazyProxy` (`internal/daemon/lazy_proxy.go`) must get the **same** fail-loud trigger. `LazyProxy.Stop` (`lazy_proxy.go:200`) already closes the endpoint + HTTP server + lifecycle; the gap is that the **router** doesn't tear down client sessions when the proxy stops. Mirror the serena `coordinateExpiredRouterSessionUnbind` pattern for LSP. The `didOpen/didClose` no-refcount multi-agent bug (§11.1, `lazy_proxy.go`) is a separate but adjacent finding — see Adjacent findings.

### Deterministic reproduction harness → regression test

```
1. Start the hub + a serena pool daemon for workspace W.
2. Client A: POST /serena/mcp initialize → capture Mcp-Session-Id S (routerSessionStore now has S live).
3. Client A: one successful tool call through S (sticky + daemon bindings established).
4. Inject backend loss: kill the serena daemon for W via the supervisor's
   deliberate path (taskkill the daemon PID, OR drive EvChildExit through the
   controller's crashCh bridge, supervisor_controller.go:459).
5. ASSERT (current = zombie): POST /serena/mcp with session S returns 200 and a
   live routerSessionStore entry → FAIL (this is the bug).
6. ASSERT (fixed): within one reconcile tick / SSE event, S is unbound from all
   three stores (routerSessionStore + daemonSessions + sticky), and the next
   request on S returns -32600 "session terminated" → the client re-initializes.
```

The Go test lives in `internal/gui/serena_router_lifecycle_test.go` (the lifecycle suite already exists). It uses the `serenaRouterTestSeam` (`serena_router.go:66`) to inject a fake upstream whose endpoint flips to "dead," then asserts the session sweep fires on the backend-loss event rather than only on the 24h idle TTL. The harness's step 4 must drive the **real** exit event path, not just delete the store entry (per the race-window/end-to-end-channel disciplines in AGENTS.md).

> **[SPEC-CORRECTION — §3 vs §3.1 conflation]** §3 (zombie connection) and §3.1 (false "Restarting") are **two distinct bugs** the spec sometimes treats as one ("Couples to Workstream B"). §3.1 is a **status-display** bug fixed at `health.go:399-401` (B). §3 is a **session-lifecycle** bug fixed in the serena/LSP **router** (`serena_router_session.go`), which is a different layer and a different PR surface. They share the "fail loud, no zombie state" *principle* but not the *fix site*. Keep them as separate workstreams: §3.1 → B; §3 → the router fail-loud design above, folded into Phase 1 per §12.

---

## §2.x Port-ownership migration inventory (the killed-live-daemon class)

### The current split (three owners, one of which is stale)

**Owner 1 — embedded manifests (the de-facto runtime source).** Each `servers/<s>/manifest.yaml` declares its daemon port inline. Verified example: `servers/serena/manifest.yaml:62` declares `port: 9121` for the `unified` daemon. `Install` loads these embed-FS-first (`internal/api/manifest_source.go` per §10.0; `Install` at `install.go:239`), and `findDaemonPort(m, binding.Daemon)` (used by `liveEntryMatchesManifestBinding`, `managed_entries.go:356`) reads the manifest port as truth for client-config URL shape. **This is what actually owns global daemon ports at runtime today.**

**Owner 2 — `configs/ports.yaml` (currently TEST-ONLY).** Parsed by `config.ParsePortRegistry` (`internal/config/ports.go:33`). The **only** reader is the drift-guard test `internal/config/serena_test.go:75-143`, which asserts every embedded-manifest declared port has a matching `ports.yaml` entry. No production code reads it. Its content (read this session, `configs/ports.yaml:1-32`): 10 global entries (serena/unified=9121, memory=9123, sequential-thinking=9124, wolfram=9132, godbolt=9126, paper-search-mcp=9127, time=9128, gdb=9129, lldb=9130, perftools=9131) and `workspace_scoped: []` (empty).

**Owner 3 — `AllocatePort` / `AllocateSerenaPort` (the dynamic-pool runtime owner for workspace-scoped + serena pool).** `AllocatePort(reg *Registry, pool config.PortPool)` (`internal/api/port_alloc.go:40`) returns the lowest free port in the pool that is both unallocated in the workspace `Registry` AND not OS-bound. The serena dynamic pool uses `reg.AllocateSerenaPort(pool)` (`internal/api/serena_auto_register.go:188`) over `EffectiveSerenaPortPool` (`serena_dynamic_pool.go:149`). Allocated serena ports persist to `workspaces.yaml` (the `Registry`, `internal/api/workspace_registry.go:82`).

### The bug class (test-Port:9200-killed-live-daemon)

The split means the **test convention** (pools at `9200-9299`, e.g. `port_alloc_test.go:21`, `e2e/lazy_register_test.go:197`) overlaps the **runtime allocation band**. A test that hardcodes `Port: 9200` and calls `killByPortFn`/`net.Listen` can hit a **live** workspace-scoped daemon the running pool allocated at 9200. The CLAUDE.md memory `feedback_common_logic_flexible_defaults_via_gui` records this exact incident ("Port:9200 literal → killed live daemon").

> **[SPEC-CORRECTION — §2 stale-data finding]** §2 says "promote `configs/ports.yaml` from a test-only fixture to the RUNTIME port owner." Two problems make the promotion non-trivial: **(1)** `configs/ports.yaml` still lists `serena/unified port: 9121` (`configs/ports.yaml:2-4`) — the **legacy** pre-dynamic-pool global. The running architecture uses the serena **dynamic pool** via `AllocateSerenaPort`, and `defaultLegacySerenaPort = 9121` is explicitly the *legacy* constant (`serena_client_reconcile.go:59`). So `ports.yaml` does not model the live serena topology at all. **(2)** `ports.yaml` has `workspace_scoped: []` (empty) — it models zero of the dynamic pools that actually cause the killed-live-daemon collisions. Promoting it as-is would make a stale file the runtime owner. §2 must first **reconcile `ports.yaml` to the dynamic-pool reality** (model the serena pool range and the `9200-9299` workspace band as *reserved bands*, not static ports) before it can be the runtime owner.

### Cutover that does NOT repeat the killed-live-daemon class

1. **`configs/ports.yaml` becomes the band authority, not a port-per-daemon list.** Extend its schema: keep `global` (static global daemon ports, the 10 entries), and make `workspace_scoped` declare **reserved pool ranges** (serena pool band, LSP workspace band) — the ranges `AllocatePort` is allowed to draw from. The existing `PortRegistry.Validate` (`config/ports.go:50`) already detects global-vs-pool overlap (`ports.go:62-67`) and pool-vs-pool overlap (`ports.go:69-78`) — reuse it as the boot-time guard so a global port can never sit inside a pool band.
2. **`AllocatePort` reads the band from the promoted `ports.yaml`** instead of a per-manifest `port_pool` literal. The manifest's `port_pool` becomes a default the config can override (tier-A → GUI per #8). The serena pool's `EffectiveSerenaPortPool` (`serena_dynamic_pool.go:149`) reads the same authority.
3. **Test-port convention (the guard the spec already names, §6/#8a):** any value reaching `killByPortFn` (`install.go:2660`) or `net.Listen` in a test uses `pickFreeLocalPort(t)` / `:0` — never a literal in the live band. A drift-guard test greps **BOTH the non-test tree AND the test tree** for live-band literals (`9121`/`9123`-`9132`/`9200`-`9299`) reaching kill/listen and fails on any match. **The test-tree grep is load-bearing (§15 P2 / fable):** the actual Port:9200-killed-live-daemon incident was a TEST literal hitting a live daemon (tests run against the real state dir), so a non-test-only grep would NOT have caught it. This is the falsification test for #8a.
4. **No port a test allocates can be a band a production daemon uses,** because production draws only from the `ports.yaml`-declared bands and tests draw only from `:0`/`pickFreeLocalPort`. The overlap that caused the incident is structurally impossible once tests stop using fixed live-band literals.

### Interaction with serena pool + persisted state + hub-port-change

- **`AllocatePort` + `workspaces.yaml`:** the serena pool persists allocated ports to the `Registry` (`workspace_registry.go:82`, saved atomically `Save` at `workspace_registry.go:157`). The cutover must keep `AllocatePort` reading the live `Registry` taken-set (`port_alloc.go:40` already does) so a band-config change never re-allocates a port a persisted workspace already holds. Validate at boot: every persisted `serena_port` falls inside the new `ports.yaml` serena band; a persisted port outside the band is a warn (operator changed the band) — fail loud, do not silently re-home.
- **Hub-port-change → client-config-rewrite:** the **hub rendezvous port** (`gui_server.port`, read via `SettingsGet("gui_server.port")` at `lsp_client_router.go:278`, `cli/gui.go:200`) is *separate* from daemon ports and stays stable/GUI-configurable (§2 is correct here). On hub-port change, the install reconcile rewrites every client config (`cli/install.go:370` "rewrites every client config to `http://127.0.0.1:<stale>…`"). The daemon-port band cutover does **not** touch this flow: clients only ever see the hub port + `/serena/mcp` path (the router shape, `clients.go:167`), never a per-daemon port. So #8's band cutover is invisible to clients — only the hub→daemon mapping changes, which is internal.

---

## §0.x Redesign risk register + rollback story

§0 premise 2 deleted the v0.4.x rollback net. Each A→F phase is **one PR → redeploy → FULL supervisor restart** (the CLAUDE.md "redeploy always after merge" discipline + `feedback_always_redeploy_after_merge`), which interrupts the live serena/LSP fleet every phase. There is no compat safety net — **the live fleet is the safety surface**. This register defines the per-phase reversibility, recovery, and deploy-gating.

### Reversibility (git-local) vs recovery (live-fleet)

| Phase | Reversible while local? | Live-fleet recovery if it breaks serena | Pre-deploy gate |
|---|---|---|---|
| **A** (landed) | Yes — `git reset --hard` before push; additive, no schema change (`install.go:2595`) | `mcphub restart serena` revives; worst case `taskkill` the daemon and the supervisor respawns (non-clean→backoff still works) | already in tree; gate is re-verify the §5.1(a) falsification test |
| **B** | Yes — display-only change at `health.go:399-401` + `gui/server.go:81`; no persisted-state change | none needed — B cannot break serena (it changes status *rendering*, not lifecycle). Revert restores the fallback. | **must land BEFORE C/D** — see deploy-gating below |
| **C** | Yes — removing the `setup.go:471` call is a one-line revert; the watchdog task is re-installable via `mcphub watchdog install` | if removing the watchdog leaves a genuinely-stuck daemon, `mcphub restart` or `mcphub watchdog install` (still present until D) re-arms recovery | **gate: A deployed + §3.1 status bug cleared** (so you can SEE whether daemons are actually healthy without the watchdog) |
| **D** | Partially — deletes files; `git reset` restores them while local, but once the watchdog command is gone there is no fallback recovery engine | the supervisor IS the recovery engine after D; recovery is `mcphub restart`/`mcphub supervise` reconcile. If the supervisor itself is broken, recovery is manual `taskkill` + restart supervise | **gate: C deployed and stable ≥1 session** (watchdog uninstalled everywhere first) |
| **E** | Risky — deletes `daemon-intent.json` after the in-place merge. `git reset` restores the *code*, but the **deleted `daemon-intent.json` is gone**. | **Back up the live state dir before deploying E** (per CLAUDE.md memory `feedback_kosyak_subagent_test_wiped_live_supervisor_intent` — state-file wipes kill the fleet). The merge is idempotent and deletes `daemon-intent.json` LAST, so a crash mid-merge is recoverable; but a *bug* in the merge that mis-reads a stop could lose a stop preference. Recovery: re-issue `mcphub stop <daemon>` for any daemon whose stop was lost. | **gate: D deployed; full state-dir backup taken; intent_collapse_test green** |
| **F** | Hard — deletes `internal/migration/` + rollback command. Once `--rollback-to-legacy` is gone, there is no path back to the scheduler model. | the supervisor model is the only model after F. Recovery for a broken fresh-install is `mcphub supervise` reconcile from `supervisor-intent.json`; if that file is bad, restore from the E backup. | **gate: E deployed and stable; serena pool + all 10 globals confirmed healthy under supervisor-only** |

### Deploy-gating (the hard ordering)

**The STOP fix (A) AND the §3.1 status bug (B) MUST clear before C/D remove the scheduler fallback.** Rationale: C/D delete the watchdog and (eventually) the scheduler view. Until §3.1 is fixed (B), `mcphub status` may paint healthy daemons as `failed` via the `health.go:399-401` fallback — so an operator removing the watchdog (C) would be flying blind, unable to tell a genuinely-broken daemon from a status false-negative. Sequence is therefore **A → B → C → D → E → F**, with A+B as a hard gate on C. (§12 already encodes A→B→…; this register makes the *gate* explicit and ties it to the live-fleet observability requirement.)

### State-backup discipline (mandatory, every phase touching the state dir)

Per CLAUDE.md memory `feedback_kosyak_subagent_test_wiped_live_supervisor_intent`: before ANY phase that runs `go test` over `internal/api`/`internal/cli` or that mutates the live state dir (E, F especially), **back up `supervisor-intent.json` + `daemon-intent.json` + `workspaces.yaml`** (jq-filterable copies under `/.scratch/`), because a test that forgets to `t.Setenv` the `test_state_path_env` override can overwrite the live intent and kill the fleet. The `MCPHUB_STATE_DIR_OVERRIDE` seam (`internal/cli/supervise.go:159-163`) is the test redirect; tests must set it, and the redesign's per-phase checklist (§12 cross-cutting gate 3) must enforce the backup.

### Per-phase "is this reversible by git reset" rule

A→D are reversible by `git reset --hard HEAD~N` while local (additive code or file deletions git tracks). **E and F are NOT fully reversible** because they delete persisted runtime files (`daemon-intent.json`) and the rollback command, which `git reset` does not restore on the live host. For E/F the recovery surface is the **state-dir backup**, not git. This is the operational consequence of §0 premise 2 ("nothing to roll back to") and must be stated in the §12 cross-cutting gates.

---

## Adjacent findings

Per the Adjacent findings protocol, issues surfaced during this analysis that are **outside the admitted scope** (elaborating the spec) and are the orchestrator's call to admit:

1. **`didOpen/didClose` no-refcount multi-agent bug (`internal/daemon/lazy_proxy.go`).** §11.1/§11.4 already list it as OPEN. It is *adjacent* to the §3 fail-loud LSP-router work (same file, same lifecycle layer) but is a distinct defect (missing per-client refcount on document open/close). Do NOT fold it into the §3 design; it needs its own fix-design. `context: adjacent-finding, status: open`.

2. **`configs/ports.yaml` is stale for serena (`configs/ports.yaml:2-4` lists legacy `serena/unified=9121`; the running architecture uses the dynamic pool).** This is a latent drift the §2 promotion must fix first, but it is *also* a standalone correctness issue: the drift-guard test (`serena_test.go`) passes because it checks ports.yaml against the embedded manifest (which still declares 9121, `serena/manifest.yaml:62`), so the test cannot catch that the *runtime* no longer uses 9121. Flag for a bug doc if §2 is not admitted soon.

---

## Summary of spec corrections (the review-loop checklist)

1. **§4 / Workstream A** — written future-tense but **already landed**; re-anchor `StopAll` to `install.go:2595` (not ~2545), update to past tense. (roadmap line 83 already says DONE — §4 prose contradicts it.)
2. **§5 line 53 "4 readers"** — verified **6** `IsActiveStop` call sites; after D deletes `recovery.go:299`, **5** remain; `restart_supervisor.go` is NOT an `IsActiveStop` reader.
3. **§3.1 root candidate (b)** — not "stale label"; it is the **`health.go:399-401` IPC→scheduler silent fallback** + `normalizeDaemonState` unknown→failed (`health.go:957-961`). Makes (b) a B-only fix. **(LANDED — PR #281 slice 1:**) the fallback is removed → `ErrSupervisorDown` → 500; the SSE poller is now fail-loud via `Server.StatusProvider()`→`DaemonStatusSnapshot` (review P1); and `normalizeDaemonState` flipped polarity — unmapped/blank → `unknown`, with the KNOWN degraded/terminal supervisor states classified honestly: `Restarting/Backoff/Spawning`→`starting`, `Quarantined`→`failed` (review P2). The `DaemonRow.State` wire-enum is now 5 values; canonical doc `g2-unified-health-endpoint-design.md:83` + plan `:183`.
4. **§3 vs §3.1 conflated** — two distinct bugs/layers: §3.1 = status display (`health.go`, Workstream B); §3 = session lifecycle (`serena_router_session.go`, router fail-loud). Keep separate.
5. **§2 ports.yaml promotion** — `configs/ports.yaml` is **test-only** (sole reader: `serena_test.go`) AND **stale** (legacy `serena/unified=9121`, empty `workspace_scoped`). Must be reconciled to the dynamic-pool reality before it can be the runtime owner.
6. **§11.1 "#6/E ordering unspecified"** — contradicted §12 (which DOES specify E-before-#6, now at Phase 4-tail). Resolved in favor of §12 + §5.1-E.

**Gate decision: PASS** — these six sections are traceable to verified `file:line` facts, the spec's admitted §11.1 gaps are closed, six concrete spec corrections are surfaced for the review loop, and two adjacent findings are filed rather than silently folded in.

**Relevant files (absolute paths):**
- `d:/dev/mcp-local-hub/docs/superpowers/specs/2026-06-10-clean-architecture-redesign.md` (target spec)
- `d:/dev/mcp-local-hub/internal/api/install.go` (Stop/StopAll/killDaemonByPort — `:2595`, `:2646`, `:2669`, `:2677`)
- `d:/dev/mcp-local-hub/internal/api/restart_supervisor.go` (supervisor-owned selector — `:29`, `:83`)
- `d:/dev/mcp-local-hub/internal/api/install_intent.go` (`recordStopIntentAs` — `:235`)
- `d:/dev/mcp-local-hub/internal/api/supervisor_state_machine.go` (`Transition` — `:47`, non-clean→backoff `:125`)
- `d:/dev/mcp-local-hub/internal/cli/supervisor_controller.go` (`clean_exit` gate — `:144`; `IsActiveStop` reader `:710`)
- `d:/dev/mcp-local-hub/internal/api/daemon_intent.go` (`DaemonIntent`/`IsActiveStop` — `:219`, `:308`)
- `d:/dev/mcp-local-hub/internal/cli/supervise_watcher.go` (`watchedIntentFiles` — `:57`)
- `d:/dev/mcp-local-hub/internal/cli/setup.go` (watchdog auto-install — `:471`)
- `d:/dev/mcp-local-hub/internal/api/api_surfaces.go` (`InstallWatchdogTask`/`UninstallWatchdogTask` — `:692`, `:754`)
- `d:/dev/mcp-local-hub/internal/api/health.go` (`computeDaemonsSection` IPC→scheduler fallback — `:399`; `normalizeDaemonState` — `:947`)
- `d:/dev/mcp-local-hub/internal/api/recovery.go` (recovery `IsActiveStop` — `:299`)
- `d:/dev/mcp-local-hub/internal/gui/serena_router.go` + `serena_router_session.go` (session stores + coordinated unbind — `:473`, `:510`)
- `d:/dev/mcp-local-hub/internal/config/ports.go` + `d:/dev/mcp-local-hub/configs/ports.yaml` (port registry parser + stale data)
- `d:/dev/mcp-local-hub/internal/api/port_alloc.go` (`AllocatePort` — `:40`); `serena_dynamic_pool.go` (`EffectiveSerenaPortPool` — `:149`)
- `d:/dev/mcp-local-hub/internal/api/managed_entries.go` (`liveEntryMatchesManifestBinding` — `:355`); `demigrate.go:324`
- `d:/dev/mcp-local-hub/internal/clients/clients.go` (`isHubURLShapeEntry` — func at `:522`, comment at `:513`; legacy→router rewrite note `:167`)
- `d:/dev/mcp-local-hub/servers/serena/manifest.yaml` (static `port: 9121` — `:62`)agentId: a3dfdfbf76c87e445 (use SendMessage with to: 'a3dfdfbf76c87e445' to continue this agent)
<usage>subagent_tokens: 229221
tool_uses: 36
duration_ms: 405354</usage>
