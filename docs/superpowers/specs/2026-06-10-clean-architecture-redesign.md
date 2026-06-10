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

### 3.1 Observed bug (2026-06-10): status misreports "Restarting" / hub "down" while daemons serve

`mcphub status` and the GUI Dashboard paint EVERY daemon `Restarting` and the hub "down" while serena (9125/serena/mcp) + fetch (9121) actually serve verified-working MCP traffic — a false negative. Root candidates: (a) the supervisor is genuinely restart-looping (the STOP-bug churn — §4 / Workstream A), or (b) the status snapshot reads a stale / transient `Restarting` label without re-probing actual port-bound + PID-alive liveness. Fix direction: status reflects ACTUAL liveness (port + PID probe), never a stale label; the GUI fails loud ONLY when a daemon is genuinely unreachable. Couples to Workstream B (GUI fail-loud) + §4. Capture supervisor-state.json + supervisor-events.log at repro.

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


---

## Roadmap overview

This roadmap supersedes the original 8-section sketch by adding the two owner-requested workstreams the first pass dropped — **multi-agent/multi-client adapter scaling (§9)** and the **GUI one-click MCP Store (§10)** — plus the full deferred-work inventory (§11) and a unified phased sequencing (§12). Sections §0–§8 above stand unchanged.

| Workstream | Goal | Status | Depends-on |
|---|---|---|---|
| **A — STOP fix** (§4) | Make `Stop`/`StopAll` supervisor-aware (IPC `reconcile --apply`); kill the stop→failed→Quarantine churn | **DONE** (8ab8a42, awaiting deploy) | #278 merged |
| **B — GUI fail-loud** (§5) | Stop surfacing legacy watchdog/serena-unified rows when IPC down; show "supervisor down — restart" | next (independent) | #278 merged |
| **C — drop watchdog task** (§5) | Stop `setup.go` auto-installing `\mcp-local-hub-watchdog`; uninstall on existing hosts | future | A |
| **D — delete watchdog engine** (§5) | Remove `watchdog.go` / `recovery.go` / `watchdog_state.go` | future | C |
| **E — collapse dual-intent** (§5) | One `supervisor-intent.json`; delete `daemon-intent.json` + `install_intent.go`; migrate 4 readers | future | D |
| **F — drop scheduler/migration** (§5) | Global daemons → supervisor-intent; remove `install --rollback-to-legacy`, `internal/migration/`, `migrate-legacy` | future | E |
| **§3 — connection robustness** | serena + LSP router fail loud at connection layer on backend loss (no zombie sessions) | next | folds into A/B |
| **#6 — idle-shutdown** | serena pool daemons sleep after N idle min (GUI-configurable); `IntentReasonIdle` | future | E (unified intent) |
| **#8 — config-centralization** (§2) | `configs/ports.yaml` becomes runtime port owner; daemon ports auto-allocated 9150+; tier-A → GUI | next/future | feeds §10 port alloc |
| **demigrate-serena-router** | demigrate recognizes `/serena/mcp` router shape as mcphub-managed-removable | next | independent |
| **#4 — hash→name display** | Show `serena · <project>` not `serena-<8hex>` in CLI + Dashboard | next | independent (display-only) |
| **§9 — multi-agent/multi-client** | Adapter-registration table; per-client GUI enable/disable; more agents; relay parity | next→future | client-config adapter layer |
| **§10 — GUI MCP Store** | One-button install (generate+manifest-create-to-disk+install); catalog screen; port alloc; progress stream; secret prompts; runtime probes | future | G5 (done); #8 port alloc; §9 adapters |
| **§11 — backlog** | Categorized deferred items (LSP router design, Linux lane, observability, testing, secrets, docs) | mixed | per-item |
| **LSP router design** | First-class spec for the LSP router (modes/lifecycle/fail-loud parity with serena) — currently named, undesigned | future | §3 |
| **G6 remote-MCP** | `transport: remote-http` install path (URL+headers+secrets, no daemon) | future | feeds §10 remote shape |

Legend: #278 = **MERGED** (2c7c343 on master); STOP-fix (A) = **DONE** (8ab8a42, awaiting deploy); **next** = first PR block after STOP deploy; **future** = later phased PR.

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

3. **`POST /api/marketplace/install` — the one genuinely-new backend mechanism.** Composes existing pieces in one call: `api.GenerateDraftManifest` (`marketplace_generate.go:29`) → **fill port + name** → `api.ManifestCreate(name, yaml)` (`manifest.go:251`, **writes to disk — §10.0**) → `api.Install(InstallOpts{Server:name})` (`install.go:229`). The last step already exists verbatim in `/api/install` (`install.go:30-32`: `api.NewAPI().Install(InstallOpts{Server:name})`).

4. **Port auto-allocation — the real algorithmic gap, ties to §2/#8.** The draft has `port:0` (`marketplace_generate.go:175-177`) and `Preflight` requires a free port (`install.go:1517-1529`, `portInUse` `:1702`). There is **no production port auto-allocator** — `pickFreeLocalPort` is test-only (`install_test.go:554-559`). The store must scan the **config-driven** canonical band (9150+, owned by `configs/ports.yaml` once #8 promotes it to runtime owner) against `portInUse`, or `net.Listen(":0")`+retain, before manifest-create. **Memory lesson: hardcoding `Port:9200` once killed a live daemon — auto-allocation MUST be a real free-port scan, config-driven, never a literal.** §10 therefore **depends on #8** (config-centralization) for a clean port owner.

5. **Install-progress streaming** — `api.Install` is synchronous and writes human text to `InstallOpts.Writer` (`install.go:230-233`); the GUI `/api/install` discards it (`Writer:nil`→stderr, `install.go:30-32`). For per-step progress (uvx/npx download, spawn, client-config write), wire install output into the existing SSE channel `/api/events` (`internal/gui/events.go:362`) or a streamed response. **"Download" progress = daemon-spawn progress** — uvx/npx fetches the package on the daemon's first exec, so the first-spawn output IS the download progress.

6. **Secret prompts** — when an entry's `env`/headers carry `secret:`/`${env:*}` refs (sensitive-classifier hits, `IsSensitiveEnvName`), the store must **prompt before install** (replacing the CLI's human-edit substitute). The vault UI already exists (`AddSecretModal`, `/api/secrets` `internal/gui/secrets.go:16`); the store detects required secrets from the entry and gates install on their presence, reusing that modal. **Mandatory for remote-http** (§10.4): a missing `${secret:KEY}` fails install before any client-config write.

7. **uvx/npx/pip runtime probe** — `Preflight` already does `exec.LookPath(m.Command)` (`install.go:1499-1501`) and fails loud if missing, but there is **no GUI-facing pre-install probe**. The store should probe the entry's `command` (`uvx`/`npx`/`pipx`) up-front and show "runtime missing — install Node/uv" guidance rather than failing only at install time.

### 10.4 G6 remote-http install shape (the simpler one-click path)

G6 (`2026-05-12-g6-remote-mcp-manifests-design.md`) adds `transport: remote-http` — a remote HTTPS endpoint with **no local daemon, no port, no scheduler task** (`:14-18`). It is already partially wired into G5: `generateRemoteHTTPDraft` (`marketplace_generate.go:45-98`) emits `transport: remote-http` + `url` + `client_bindings` for `transport:"http"` catalog entries (e.g. the `context7` seed, `2026-05-13-g5-marketplace-draft-import.md:336-347`). For the store:
- The one-click flow is **simpler** — no uvx/npx download, no port allocation (§10.3#4 skipped), no daemon spawn. Install just expands `${secret:KEY}` headers from the vault at install time and writes URL+headers to client configs (`g6-design:73-81`); Preflight short-circuits for remote-http (`install.go:1480-1496`).
- Secret prompt (§10.3#6) is **mandatory**: a missing secret fails install **before** any client-config write (`g6-design:67-68`); the store pre-collects the token via the vault modal before firing install.
- **Adapter capability matters (ties to §9):** antigravity has **no remote-http support** (relay-tuple only, `g6-design:96-97`) — the store must hide/disable remote-http entries for that client or surface the refuse-with-WARN.
- The store UI must **distinguish the two install shapes**: local-stdio (downloads + spawns a daemon) vs remote-http (writes a URL+token to client configs).

**§10 acceptance:** (1) every store install persists a manifest under `defaultManifestDir()` (the §10.0 regression guard — assert the manifest file exists post-install). (2) port auto-allocation is a config-driven free-port scan, never a literal (guard-test greps for live-band literals — same #8 test convention). (3) install progress streams to `/api/events`. (4) secret-bearing entries gate install on vault presence. (5) runtime-probe surfaces uvx/npx/pip-missing before install. (6) remote-http entries take the daemon-less path and are hidden/disabled for antigravity.

---

## §11 Full backlog (what's left)

Categorized open/deferred inventory. Phase 3B-II GUI, v0.5 supervisor, and serena dynamic-pool are **mostly SHIPPED on master** (PRs through #278); what remains there is polish, residual P2/P3 bugs, and verify-against-HEAD plan-tail items. The single largest open block is the §0–§8 + §9 + §10 redesign itself.

### 11.1 Redesign lane (largest open; §0–§8 + §9 + §10)
A→F legacy-removal, §3 connection-robustness, and feature bugs #4/#6/#8/demigrate — all design-stage. The redesign gaps a full plan must still fill (from the architecture review):
- **No per-phase acceptance criteria / test plan.** §5 names phases but no "done" gate, no per-phase test surface, no E2E updates (the 103-test Playwright suite needs new specs for fail-loud/idle/hash→name). No analogue to the supervisor spec's 15-step implementation outline.
- **LSP router named but undesigned.** §1/§3 demand fail-loud parity for an "LSP router" peer of the serena router, but no spec covers its routing modes, lifecycle, or the per-`(workspace,language)` LazyProxy singleflight. The `didOpen/didClose` no-refcount multi-agent bug (`lazy_proxy.go`, serena-pool §3 Часть Д) is unaddressed.
- **Phase E intent-collapse mechanics undefined.** Research-mode says "no migration," but live hosts (the owner's fleet) DO have a populated `daemon-intent.json` + running serena pool. The roadmap must state how the live machine transitions intent files without the deleted migration engine, and how `IsActiveStop`/TTL/`IntentReason` move to the unified file. **Ordering between #6 (`IntentReasonIdle`) and Phase E (rewriting that file) is unspecified.**
- **§3 fail-loud has no mechanism design.** Which layer holds the session handle; how SSE/HTTP streaming connections tear down; how the router detects backend loss (health-probe vs exit event vs IPC); no reproduction harness for the 2026-06-10 zombie incident → no regression test specified.
- **§2 port-ownership migration scope unbounded.** Need the actual inventory (which embedded manifests, which Go const ranges) and a cutover that doesn't repeat the killed-live-daemon class. Interaction with the serena pool's `AllocatePort` + persisted `workspaces.yaml.serena_port` and the hub-port-change→client-config-rewrite flow is unspecified.
- **macOS/Linux posture post-scheduler.** Phases D/F delete the watchdog/scheduler several cross-platform fallbacks lean on; the redesign never restates the GA/beta/preview matrix or what the autostart shim becomes.
- **No ADR/risk register/rollback story for the redesign itself.** §0 deletes v0.4.x rollback but gives no safety net for the live fleet during A→F (each phase is one PR → redeploy → FULL supervisor restart, interrupting running serena/LSP daemons every phase).

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

**Phase 0 — DONE.** #278 merged to master (2c7c343); the STOP fix (Workstream A) is committed (8ab8a42 on `fix/stop-supervisor-aware`), awaiting deploy. **Gate:** deploy STOP + confirm the §3.1 status-misreport bug clears after a clean supervisor restart.

**Phase 1 — STOP fix (Workstream A, §4).** One additive PR, no schema change: `stopSupervisorOwnedDaemons` mirroring `restartSupervisorOwnedDaemons`; wire at `install.go` Stop (~:2200) + StopAll (~:2545). Ships first — fixes the live paper-search bug. Fold in **#4 hash→name** (display-only, independent) and **demigrate-serena-router** (independent) as small sibling commits or adjacent PRs. **Add the §3 fail-loud mechanism design + regression harness here** (the STOP path and the connection-teardown path both live in the supervisor/router seam).

**Phase 2 — GUI fail-loud (Workstream B, §5).** Independent; can run parallel to Phase 1. Stop surfacing legacy rows when IPC down; show "supervisor down — restart". Pairs naturally with the §3 router fail-loud from Phase 1 (both are "fail loud, no zombie state").

**Phase 3 — config-centralization (#8, §2).** Promote `configs/ports.yaml` to runtime port owner; daemon ports auto-allocated 9150+; tier-A values → SettingsRegistry/GUI; test-port convention (`pickFreeLocalPort(t)`/Port:0 + guard-test). **Ship before §10** — the GUI store's port auto-allocator (§10.3#4) depends on a clean config-driven port owner (the killed-live-daemon class is rooted here). Include the §2 port-ownership inventory (which manifests, which const ranges).

**Phase 4 — legacy removal C→D→E→F (Workstream §5).** Strict chain:
- **C** — drop `setup.go` watchdog-task auto-install + uninstall on existing hosts (most urgent legacy; fights supervisor every 5 min). Depends on A.
- **D** — delete `watchdog.go`/`recovery.go`/`watchdog_state.go`. Depends on C.
- **E** — collapse dual-intent → one `supervisor-intent.json`; delete `daemon-intent.json`+`install_intent.go`; migrate the 4 readers. Depends on D. **Resolve the §11.1 intent-collapse mechanics first** (how the live fleet transitions without the deleted migration engine; where `IsActiveStop`/TTL/`IntentReason` move). **Sequence #6 idle-shutdown's `IntentReasonIdle` AFTER E lands** (it writes the file E is rewriting — do not author it on the soon-deleted dual-intent).
- **F** — global daemons → supervisor-intent; remove `install --rollback-to-legacy`+`internal/migration/`+`migrate-legacy` (largest). Depends on E.

**Phase 5 — idle-shutdown (#6, §6).** Lands AFTER Phase 4-E (needs the unified intent + the §4/Phase-A corrected stop path — do NOT author a second stop path). `desired=stopped`+`IntentReasonIdle`; 60s in-supervisor sweeper; router clears + 503-retries on wake; GUI-configurable threshold + cold/warm wake-mode. Open verify: serena `.serena/` re-warm cost drives the default.

**Phase 6 — multi-agent registration table (§9).** Lands once the client-config adapter layer is stable post-Phase-4 (E/F touch reconcile + intent that the adapter set keys off). Collapse the 4 duplicated canonical-set literals into one registration table; add GUI per-client enable/disable; ship the antigravity relay shape as the documented non-HTTP path. Each new adapter is then one table entry + one ~80-line file + demigrate symmetry tests. Can interleave new-adapter PRs after the table lands.

**Phase 7 — GUI MCP Store (§10).** Depends on Phase 3 (port owner) + Phase 6 (per-client adapter targeting) + the §11.9 `install-progress`/`install-done` events. Sub-sequence: (7a) `GET /api/marketplace`+`POST /api/marketplace/refresh` + Store screen + nav link (thin G5 wrappers); (7b) `POST /api/marketplace/install` orchestrator (generate→port-fill→**manifest-create-to-disk per §10.0**→install) + runtime probe + secret-prompt gate; (7c) install-progress SSE streaming; (7d) **G6 remote-http** install path (§10.4) + antigravity hide/disable. The §10.0 manifest-persistence regression guard is a hard gate on 7b.

**Phase 8 — backlog interleave / future lanes.** GUI screen polish (§11.2), observability event-bus + API-contract reconciliation (§11.9), backups keep-N (§11.9), LSP-router first-class design (§11.1/§11.5), supervisor P3 residuals (§11.3), serena v2 features (§11.4), Linux-server lane F1–F7 (§11.7 — separate strategic lane, not pulled into v0.6 GA), macOS kqueue (§11.3), test-infra leak fixes (§11.10). Each rides whichever earlier phase cleaned the surface it touches.

**Cross-cutting gates for every phase:** (1) per-phase acceptance criteria + test surface (the redesign currently lacks these — §11.1); (2) E2E spec additions where the phase changes observable GUI behavior (fail-loud, idle, hash→name, store); (3) state-file backup before any subagent `go test` (§11.10 fleet-wipe lesson); (4) redeploy = FULL supervisor restart, expect serena/LSP interruption each phase; (5) each phase is reversible only by `git reset` while local — there is no v0.4.x rollback net (§0 premise 2), so the live fleet is the safety surface.


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
