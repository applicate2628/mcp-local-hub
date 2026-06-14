# mcp-local-hub

Run one copy of each [Model Context Protocol](https://modelcontextprotocol.io) server on your workstation, shared across every MCP client that needs it — instead of each client spawning its own redundant stdio process.

> [!WARNING]
> Preview version: `mcp-local-hub` is actively under development. Interfaces,
> manifests, GUI flows, install/migration behavior, and supported-client wiring
> may still change. Windows 11 is the primary tested path, but not every
> feature, server, client combination, or platform path is fully tested yet; use
> dry-runs and backups before applying changes to important MCP client configs.

## The problem

Every modern coding assistant (Claude Code, Codex CLI, Cursor, VS Code, Gemini CLI, Qwen Code CLI, Antigravity, Continue, ...) speaks MCP, and each client independently `exec`s whatever stdio servers you configure — `uvx serena`, `npx @modelcontextprotocol/server-memory`, `mcp-language-server`, and so on. If you use three assistants side-by-side on the same project, you get **three Serena processes**, **three gopls subprocesses**, **three separate memory stores**. Each per-session spawn re-downloads dependencies, re-indexes your code, and competes for RAM.

## What this tool does

`mcp-local-hub` runs each MCP server **once per OS user**, exposes it as a local HTTP endpoint via [Streamable HTTP transport](https://modelcontextprotocol.io/docs/concepts/transports), and writes the correct client-config entry into each managed MCP client. Clients see a shared daemon instead of their own child process.

```
   ┌─────────────────────────────────────────────────────────────────────┐
   │             OS-level Task Scheduler (Windows schtasks)              │
   │             starts on logon, restarts on failure                    │
   └──┬──────┬──────┬──────┬──────┬──────┬──────┬──────┬──────┬──────┬───┘
      │      │      │      │      │      │      │      │      │      │
      ▼      ▼      ▼      ▼      ▼      ▼      ▼      ▼      ▼      ▼
   ┌─────┐┌─────┐┌─────┐┌─────┐┌─────┐┌─────┐┌─────┐┌─────┐┌─────┐┌─────┐
   │seren││memor││seq- ││wolf-││god- ││paper││time ││gdb  ││lldb ││perf │
   │×2   ││y    ││think││ram  ││bolt ││-srch││     ││     ││     ││tools│
   │/22  ││9123 ││9124 ││9132 ││9126 ││9127 ││9128 ││9129 ││9130 ││9131 │
   └──┬──┘└──┬──┘└──┬──┘└──┬──┘└──┬──┘└──┬──┘└──┬──┘└──┬──┘└──┬──┘└──┬──┘
      │      │      │      │      │      │      │      │      │      │
      └──────┴──────┴──────┴──────┴──────┴──────┴──────┴──────┴──────┘
                                    │
                 shared by default + opt-in MCP clients
                                    │
         ┌───────────┬───────────┬───────────┬───────────┬───────────┐
         ▼           ▼           ▼           ▼           ▼
      Claude      Codex       Cursor      VS Code   Gemini/Qwen/
      Code        CLI         (HTTP)      (HTTP)    Antigravity
      (HTTP)      (HTTP)                            (opt-in)
```

Stdio-only MCP servers (memory, time, sequential-thinking, wolfram, gdb, paper-search-mcp) run behind a native Go **stdio-host** (`internal/daemon/host.go`): one subprocess per daemon, multiplexed across concurrent HTTP clients via JSON-RPC `id` rewriting and a cached `initialize` response. Three servers (**godbolt**, **lldb-bridge**, **perftools**) ship as Go code **embedded directly in the mcphub binary** — no npm/pip dependency, starts instantly.

Antigravity's Cascade agent rejects loopback-HTTP MCP entries, so `mcp-local-hub` bridges it via a **stdio relay subprocess**: `mcphub relay` translates between stdio JSON-RPC and the shared HTTP daemon. Cascade sees a normal stdio command; the daemon stays shared.

## Quick start

```bash
# 1. Build (embeds git commit + build date into the binary)
bash build.sh        # Git Bash / WSL / Linux / macOS
# or on Windows native:
pwsh ./build.ps1

# Plain `go build -o mcphub.exe ./cmd/mcphub` also works for dev
# iteration but leaves version metadata as dev/unknown.

# 2. Install to ~/.local/bin and register on user PATH (idempotent)
./mcphub.exe setup

# 3. Install the MCP servers you want shared
./mcphub.exe install --server serena       # default clients: Claude/Codex/Cursor
./mcphub.exe install --all                 # all 10 servers, default clients

# Optional client targeting
./mcphub.exe install --server serena --clients qwen-cli,vscode
./mcphub.exe install --server serena --all-clients

# 4. Verify
./mcphub.exe status
claude mcp get serena    # shows: Status: ✓ Connected, Type: http
```

Detailed setup, per-client behaviour, and troubleshooting in [INSTALL.md](INSTALL.md).

## Ten shipped servers

| Server | Port | Transport | Notes |
|---|---:|---|---|
| **serena** (×2 daemons) | 9121 / 9122 | native-http (uvx) | Flagship: per-client daemons (claude / codex) for context isolation |
| **memory** | 9123 | stdio-bridge (npx) | Shared JSONL write-serialized across all clients |
| **sequential-thinking** | 9124 | stdio-bridge (npx) | Stateless reasoning helper |
| **wolfram** | 9132 | stdio-bridge (node) | Requires `wolfram_app_id` secret |
| **godbolt** | 9126 | **embedded Go** | Compiler Explorer — compile/execute/disasm via godbolt.org + optimization remarks, llvm-mca, pahole |
| **paper-search-mcp** | 9127 | stdio-bridge (uvx) | Requires `unpaywall_email` secret |
| **time** | 9128 | stdio-bridge (npx) | Trivial stateless |
| **gdb** | 9129 | stdio-bridge (uv run) | Multi-debugger with session management |
| **lldb** | 9130 | **embedded Go bridge** | Auto-spawns `lldb.exe`, HTTP-multiplexes concurrent clients onto single TCP connection |
| **perftools** | 9131 | **embedded Go** | clang-tidy + llvm-objdump + include-what-you-use over real projects; `hyperfine` is **opt-in only** (RCE surface — set `MCP_LOCAL_HUB_ENABLE_UNSAFE_HYPERFINE=1`, see INSTALL) |

Plus **context7** as a direct HTTPS entry (no daemon, no scheduler task).

### Embedded vs external servers

Three servers (`godbolt`, `lldb-bridge`, `perftools`) are implemented as Go packages inside `internal/<name>/` and run as subcommands of the mcphub binary itself — no external runtime dependency. Each also ships as an independent standalone binary via `go build ./cmd/<name>` for users who want just that one server without the full hub.

**Performance-review workflow** combining multiple servers in one chat:

```
# audit real project for perf antipatterns
perftools.clang_tidy(files=["src/hot.cpp"], checks="performance-*")

# sanity-check asm on godbolt with optimization remarks
godbolt.compile_code(source=..., filters={optOutput: true, intel: true})

# statistical bench (requires opt-in: MCP_LOCAL_HUB_ENABLE_UNSAFE_HYPERFINE=1 on
# the perftools daemon — see INSTALL.md "Opting into hyperfine")
perftools.hyperfine(commands=["./old_bin", "./new_bin"], warmup=3)

# verify the LTO-linked final binary keeps the vectorization
perftools.llvm_objdump(binary="./new_bin", project_root=".", function="hot_loop")
```

## Supported clients

Default install targets are `claude-code`, `codex-cli`, and `cursor`.
`vscode`, `gemini-cli`, `qwen-cli`, `antigravity`, `zed`, `kiro`, `windsurf`,
`cline`, `kilocode`, `opencode`, `hermes`, and `openclaw` are opt-in via
`--clients ...` or `--all-clients`, so install does not silently mutate every
assistant installed on the workstation.

| Client | Install mode | Version/status | Config path | Transport |
|---|---|---|---|---|
| Claude Code CLI | Default | 2.1.112 tested | `~/.claude.json` | HTTP (`type: "http"`) |
| Codex CLI | Default | 0.121.0 tested | `~/.codex/config.toml` | HTTP (streamable_http) |
| Cursor | Default | Preview; live smoke pending | `~/.cursor/mcp.json` | HTTP (`type: "http"`) |
| VS Code | Opt-in | Preview; live smoke pending | user-profile `mcp.json` | HTTP (`type: "http"`) |
| Gemini CLI | Opt-in | 0.38.1 tested | `~/.gemini/settings.json` | HTTP (`type: "http"`) |
| Qwen Code CLI | Opt-in | Preview; live smoke pending | `~/.qwen/settings.json` | HTTP (`httpUrl`) |
| Antigravity IDE | Opt-in | v0.x tested | `~/.gemini/antigravity/mcp_config.json` | stdio relay -> HTTP |
| Zed | Opt-in | Preview; built from upstream docs, live smoke pending | `~/.config/zed/settings.json` (`%APPDATA%\Zed\settings.json` on Windows) | stdio relay -> HTTP (`context_servers`) |
| Kiro | Opt-in | Preview; built from upstream docs, live smoke pending | `~/.kiro/settings/mcp.json` | HTTP |
| Windsurf | Opt-in | Preview; built from upstream docs, live smoke pending | `~/.codeium/windsurf/mcp_config.json` | HTTP (`serverUrl`) |
| Cline | Opt-in | Preview; built from upstream docs, live smoke pending | VS Code globalStorage `…/saoudrizwan.claude-dev/settings/cline_mcp_settings.json` | HTTP (`type: "streamableHttp"`) |
| Kilo Code | Opt-in | Preview; built from upstream docs, live smoke pending | VS Code globalStorage `…/kilo-code.kilo-code/settings/mcp_settings.json` | HTTP |
| OpenCode | Opt-in | Preview; built from upstream docs, live smoke pending | `~/.config/opencode/opencode.json` | HTTP |
| Hermes | Opt-in | Preview; built from upstream docs, live smoke pending | `~/.hermes/config.yaml` | HTTP |
| OpenClaw | Opt-in | Preview; built from upstream docs, live smoke pending | `~/.openclaw/openclaw.json` | HTTP |

**Antigravity note:** Cascade rejects loopback-HTTP MCP entries, so `mcp-local-hub` writes a **stdio relay** entry instead — `mcphub.exe relay --server <name> --daemon <d>`. Cascade spawns the relay as a normal stdio subprocess; the relay forwards JSON-RPC to the shared HTTP daemon. No extra server process per Antigravity session.

## CLI surface

### Core operations

| Command | What it does |
|---|---|
| `mcphub setup` | Install binary to `~/.local/bin` and register on user PATH (idempotent) |
| `mcphub install --server <name>` | Create scheduler tasks, write default client configs, start daemons |
| `mcphub install --server <name> --clients <ids>` | Install only the named client bindings |
| `mcphub install --server <name> --all-clients` | Install every client binding declared by the manifest |
| `mcphub install --all` | Bulk install every manifest under `servers/` into default clients |
| `mcphub install --server <n> --dry-run` | Print plan without applying |
| `mcphub uninstall --server <name>` | Remove scheduler tasks + client entries (backups retained) |
| `mcphub status` | Show state of every `mcp-local-hub-*` task (Running / Scheduled / Stopped) with PID, RAM, uptime, next-run |
| `mcphub restart --server <n>` / `--all` | Stop + re-launch one or all daemons |
| `mcphub stop --server <n>` / `--all` | Stop daemons without uninstalling |
| `mcphub version` | Print version, commit, build metadata |

### Discovery & migration

| Command | What it does |
|---|---|
| `mcphub scan` | Classify every MCP entry across managed clients into `via-hub`, `can-migrate`, `unknown`, `per-session`, `not-installed` |
| `mcphub migrate --server <n>` | Rewrite stdio client entries to hub HTTP for a given server |
| `mcphub manifest list` | List every manifest under `servers/*/manifest.yaml` |
| `mcphub manifest show <name>` | Print a manifest's contents |

### Logs, backups, recovery

| Command | What it does |
|---|---|
| `mcphub logs <server> [--tail N]` | Tail daemon's stdout/stderr log |
| `mcphub backups list` | Every `.bak-mcp-local-hub-*` across managed clients |
| `mcphub backups clean` | Prune old timestamped backups, keep N most recent + pristine sentinel |
| `mcphub backups show <file>` | Diff a backup against the live config |
| `mcphub rollback` | Restore the latest backup for every client |
| `mcphub rollback --original` | Restore the pristine pre-hub sentinel |
| `mcphub cleanup --dry-run` | List candidate orphan MCP server processes |

### Scheduler & secrets

| Command | What it does |
|---|---|
| `mcphub scheduler upgrade` | Rewrite every task's `<Command>` to the current canonical `mcphub.exe` path |
| `mcphub scheduler weekly-refresh set "SUN 03:00"` | Install a hub-wide weekly `restart --all` task |
| `mcphub scheduler weekly-refresh disable` | Remove the hub-wide weekly task |
| `mcphub secrets {init,set,get,list,delete,edit,migrate}` | Age-encrypted vault for API keys |
| `mcphub settings {get,set,list}` | GUI preference registry for Phase 3B/3B-II surfaces |

### Transport shims (Hidden; called by scheduler, not by humans)

| Command | What it does |
|---|---|
| `mcphub daemon --server <n> --daemon <d>` | Invoked by the scheduler; exec real server with tee'd logs |
| `mcphub relay --server <n> --daemon <d>` | Stdio↔HTTP bridge (for clients that reject loopback-HTTP) |
| `mcphub relay --url <url>` | Direct relay to an arbitrary Streamable HTTP endpoint |
| `mcphub godbolt` | Embedded godbolt MCP server (also ships as `./cmd/godbolt` standalone) |
| `mcphub lldb-bridge <host:port>` | LLDB TCP↔stdio bridge + auto-spawn (also `./cmd/lldb-bridge`) |
| `mcphub perftools` | Embedded perf-toolbox MCP (also `./cmd/perftools`) |

## Architecture highlights

### PATH-based install model

Scheduler tasks reference `~/.local/bin/mcphub.exe` by absolute path. `mcphub setup` puts the binary there and registers the directory on user PATH (Windows: `HKCU\Environment\Path` + `WM_SETTINGCHANGE` broadcast; Linux/macOS: prints shell-rc line). Moving or rebuilding the binary later only requires re-running `mcphub setup` — scheduler tasks keep pointing at the canonical path and automatically use the new binary.

### go:embed manifests

All 10 server manifests are baked into the binary via `//go:embed */manifest.yaml`. Daemons load their config from the embedded FS, not from disk, so `~/.local/bin/mcphub.exe` works without a sibling `servers/` directory.

### Dual-entry pattern

Embedded Go servers (godbolt, lldb-bridge, perftools) expose a `NewCommand() *cobra.Command` factory that's imported from two places — `cmd/<name>/main.go` (standalone binary) and `internal/cli/root.go` (hub subcommand). Same code path, zero duplication, two shipping shapes.

### Native Go stdio-host with child-exit detection

Stdio-bridge daemons run external stdio servers (npx/uvx/node/python) via a Go host (`internal/daemon/host.go`) that:

1. Spawns one subprocess per daemon (not per HTTP client)
2. Multiplexes concurrent HTTP clients by rewriting JSON-RPC `id` to an internal atomic counter, then routes responses back via a pending-request map
3. Caches the `initialize` response — first client's result is replayed for all subsequent clients with their own `id` substituted
4. Broadcasts server-initiated notifications (no `id`) to all active SSE subscribers via GET /mcp
5. **Detects child-process exit** via a dedicated `cmd.Wait()` goroutine; propagates the signal up so the daemon exits non-zero and Task Scheduler's `RestartOnFailure` (3 retries, 1min spacing) auto-recovers from npx/uvx children that die mid-session

## Supervisor architecture (v0.5.0)

### Overview

v0.5.0 replaces the v0.4.x model of N-scheduled-tasks-per-daemon with a single
long-lived `mcphub supervise` parent process per user that owns every MCP
daemon as a child process under an OS-appropriate lifecycle primitive (Windows
Job Object, Linux `PR_SET_PDEATHSIG` or systemd user service, macOS process
group + kqueue + LaunchAgent). The supervisor observes child exits in real
time, applies a persisted restart-policy state machine with sliding 30-min
failure windows, and exposes a local-only owner-bound IPC for control
commands. The remaining Task Scheduler / autostart entry is the per-user
autostart shim that re-starts the supervisor on logon.

Release scope: **Windows GA**, **Linux beta**, **macOS preview**. POSIX has
no v0.4.x to migrate from (v0.4.x shipped Windows-only) — Linux beta starts
fresh, skipping the migration journal entirely. macOS preview is build-only
Go cross-compile, no automated tests. v0.5.x stabilizes Linux to GA + macOS
CI lane; v0.6 promotes macOS only if a real containment primitive becomes
available.

### New commands

| Command | What it does |
|---|---|
| `mcphub supervise` | The long-lived supervisor process. Idempotent via `supervisor.lock`. Hosts FIFO event loop, reconcile driver, IPC listener, child-exit reaper. |
| `mcphub strict-mode enable` / `disable` | Canonical mutation of `supervisor-intent.strict_mode`. Universal lock order: `migration.lock` BEFORE `--once.lock`. Two-resource atomic write (intent file + autostart shim args) with revert-on-failure. |
| `mcphub strict-mode --recover` | Reconciles after a `STRICT_MODE_REVERT_FAILED` (exit 10) breadcrumb. Prompts operator to drive both intent + shim either to the `intended` value or to `actual_intent_state`. |
| `mcphub autostart enable` / `disable` / `status` | Per-OS autostart shim. Windows: Task Scheduler `LogonTrigger`. Linux managed: systemd user service. Linux unmanaged + macOS: per-OS user-space shim. |
| `mcphub install --upgrade` | Cold-restart upgrade flow. Rename-aside binary replacement (Windows `MoveFileExW`) + atomic rename (POSIX). Issues IPC `quiesce-timers` then `exit{graceful}`; force-kills supervisor with `taskkill /F /T /PID` on timeout; explicitly starts new supervisor. |
| `mcphub install --rollback-to-legacy` | Reverses migration. Translates supervisor-state quarantined entries to `daemon-intent.json` `chronic-failure`; uninstalls supervisor shim; re-registers every v0.4.x `legacy-tasks/<task>.xml` via `schtasks /Create /XML /F`; runs each task and waits up to 60s for the expected port to bind. Unbound ports captured in `rollback-warnings.json`; rollback exits 0 with warnings. |

### State files

All under `<state-dir>` (per-user `%LOCALAPPDATA%\mcp-local-hub\` on Windows;
`$XDG_STATE_HOME/mcp-local-hub` or `~/.local/state/mcp-local-hub` on POSIX):

```text
<state-dir>/
  supervisor-intent.json              # NEW: daemon descriptors + maintenance timers + strict_mode (canonical)
  supervisor-state.json               # NEW: per-daemon runtime state, restart_history (30-min sliding window), transient_pids, maintenance_fired_at
  supervisor-events.log               # NEW: JSONL audit trail (envelope: schema_version, ts, severity, source, event, task_name, body); 16 KB per-entry cap; 10 MB rotation → .log.1
  supervisor.lock                     # NEW: supervisor singleton lock + sidecar with {pid, start_time}
  migration-journal-<UTC-ts>/         # NEW: per-install migration journal (retain 5 newest after `committed`)
  daemon-intent.json                  # preserved exactly (byte-symmetric for rollback)
  managed-entries.json                # preserved exactly
  watchdog-state.json                 # preserved unchanged (v0.4.x watchdog diagnostics)
```

### Migration from v0.4.x

`mcphub install --upgrade` runs a two-phase journaled migration on Windows
v0.4.x hosts:

1. Enumerates every `mcp-local-hub-*` Task Scheduler task via
   `scheduler.EnumerateAllMcphubTasks()` (regardless of Run As).
2. Renders a canonical-template-snapshot via a v0.4.x-pinned template
   renderer (`internal/migration/v04x_template_defaults.go`) and classifies
   each task's XML as default-match, known-deviation, or hard-deviation.
   Hard deviations abort unless `--discard-scheduler-customizations` is set.
3. Resolves each daemon's PID via `lookupProcessIdentity(port)` (PowerShell
   `Get-CimInstance Win32_Process` with `wmic.exe` fallback for pre-24H2
   hosts), then 4-gate ownership check (image basename, CommandLine, start
   time pre-migration.lock, ExecutablePath under install-dir).
4. `taskkill /F /T /PID` each ownership-verified daemon, drops the
   `pre-os-mutating` marker on first kill.
5. Unregisters each legacy task, installs the supervisor autostart shim,
   explicitly starts the supervisor, waits for reconcile-ready via IPC
   `status` within 30s. On timeout: auto-rollback.

Migration is journaled at every state transition (`prepared` →
`pre-os-mutating` → `os-mutating-complete` → `committed`). The journal
retains forward-progress for crash-resume and rollback evidence.

`mcphub install --rollback-to-legacy` reverses migration from any
`committed` or `pre-os-mutating` marker by re-registering preserved
`legacy-tasks/<task>.xml` and translating supervisor quarantined entries
back to `daemon-intent.json` `chronic-failure`. Token-mismatch pre-flight
catches "rollback caller token is less privileged than supervisor process"
early (exit 13).

### Per-OS behavior matrix

| OS / mode | Job Object support | Restart policy | Autostart backend | Cold-start reaper |
|---|---|---|---|---|
| **Windows** | Yes — `JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE` + `PROC_THREAD_ATTRIBUTE_JOB_LIST` at create-time | Supervisor in-process state machine, persisted to `supervisor-state.json` | Task Scheduler `LogonTrigger` (one entry: the autostart shim) | Not needed (Job Object reaps every child on supervisor exit) |
| **Linux managed** | n/a (cgroup-based via systemd) | Supervisor in-process + systemd `Restart=on-failure` for supervisor itself | systemd user service with `KillMode=control-group` | Not needed (cgroup termination is atomic) |
| **Linux unmanaged** | n/a | Supervisor in-process; `PR_SET_PDEATHSIG` direct-child containment (double-fork OUT OF SCOPE) | None (manual `mcphub supervise &`) | Yes — supervisor sweeps stale `mcphub.exe daemon` children on start, 2-3s settling between reaps for TCP TIME_WAIT |
| **macOS managed** | n/a | Supervisor in-process + LaunchAgent `KeepAlive` for supervisor itself | LaunchAgent (restart-after-exit only; NOT containment) | Yes — same as Linux unmanaged |
| **macOS unmanaged** | n/a | Supervisor in-process; process group + kqueue `EVFILT_PROC NOTE_EXIT` observation (NOT containment) | None | Yes — same as Linux unmanaged |

Full design + invariants live in
[`docs/superpowers/specs/2026-05-16-v0.5.0-supervisor-architecture.md`](docs/superpowers/specs/2026-05-16-v0.5.0-supervisor-architecture.md).

## Current status

**Preview / Phase 3B-II hardening** — the core CLI, daemon layer, Windows GUI,
tray, secrets, settings, backup, migration, and workspace-scoped LSP surfaces
are in the tree. Phase 3A and Phase 3B-I are closed; Phase 3B-II has delivered
multiple follow-up slices, but still needs the live/manual smoke matrix and
backlog reconciliation before a release-ready claim.

Delivered and documented:

- 10 shipped servers plus the direct HTTPS `context7` entry.
- 22 user-facing CLI commands across install, migration, logs, backups,
  scheduler, secrets, settings, cleanup, and version surfaces.
- Go rewrites of godbolt and lldb, embedded as dual-entry servers.
- Perftools wrapping clang-tidy, opt-in hyperfine, llvm-objdump, and iwyu.
- PATH-based install model with `mcphub setup`.
- go:embed manifests for filesystem-independent binaries.
- Native stdio-host, child-exit detection, and Task Scheduler restart policy.
- Local-loopback GUI, SSE event bus, dashboard, logs, migration matrix,
  secrets/settings/about screens, Windows tray subprocess, and Playwright/E2E
  infrastructure.
- Workspace-scoped LSP lazy proxies and a Phase 3B-II live/manual smoke
  checklist for tray, console, reboot, daemon kill, and multi-language LSP
  validation.

Phase evidence:

- **Phase 1** — Serena consolidation across the original 4 clients ([docs/phase-1-verification.md](docs/phase-1-verification.md)).
- **Phase 2** — 7 global daemons added, supergateway -> native Go stdio-host ([docs/phase-2-verification.md](docs/phase-2-verification.md)).
- **Phase 3A** — CLI parity and Go-embedded servers ([docs/phase-3a-verification.md](docs/phase-3a-verification.md)).
- **Phase 3B-I** — GUI Installer MVP ([docs/phase-3b-verification.md](docs/phase-3b-verification.md)).
- **Phase 3B-II** — backlog and manual smoke matrix ([docs/superpowers/plans/phase-3b-ii-backlog.md](docs/superpowers/plans/phase-3b-ii-backlog.md), [docs/phase-3b-ii-verification.md](docs/phase-3b-ii-verification.md)).

Forward development proposals:

- Ideas to evaluate from `ravitemer/mcp-hub` are captured in
  [docs/superpowers/plans/2026-05-04-ravitemer-mcp-hub-adoption-proposals.md](docs/superpowers/plans/2026-05-04-ravitemer-mcp-hub-adoption-proposals.md).

Roadmap / remaining work:

- **Phase 3B-II release hardening** — execute the D2/D3 live/manual smoke matrix and reconcile the remaining backlog before tagging.
- **Phase 3C+ candidate work** — optional unified MCP endpoint, richer health/capability status, remote-server manifests, marketplace/import flow, and VS Code workspace/JSON5 import compatibility.
- **Phase 4+** — Linux/macOS scheduler backends (systemd user units + launchd agents).

## Feature & readiness matrix

A surface-by-surface map of what this project actually does today, with explicit honesty about preview-state coverage. See [`CONTRIBUTING.md`](CONTRIBUTING.md) for the promotion rules a row follows.

- **✅ Stable** — fresh automated test coverage OR a recent live-smoke pass for the exact user-visible surface, AND no open critical caveat. Not "production-ready"; "works as advertised in this preview, with evidence."
- **⚠ Preview** — feature is shipped and reachable, but live-smoke coverage is partial, an open caveat exists in `work-items/bugs/` or the backlog, OR the surface may change in incompatible ways. Backups and dry-run before applying.
- **🚧 Roadmap** — feature is acknowledged in the backlog but not yet shipped, OR currently exists only as a cross-compile path with no runtime evidence.

| Surface | Status | Notes |
|---|---|---|
| Run on Windows | ✅ Stable | Tested on Windows 11 (10.0.26100); primary platform |
| Run on Linux | 🚧 Roadmap | Ubuntu CI builds/tests; install/scheduler not implemented |
| Run on macOS | 🚧 Roadmap | darwin cross-build only; scheduler + force-kill probe stubbed |
| Auto-start on logon — Windows | ✅ Stable | Task Scheduler with restart-on-failure |
| Auto-start on logon — Linux | 🚧 Roadmap | systemd user units (F2) + `mcphub setup --server` with `loginctl enable-linger` (F3) tracked in backlog |
| Auto-start on logon — macOS | 🚧 Roadmap | launchd auto-start is not currently tracked in the backlog F-tier; manual launch only |
| Default client install | ⚠ Preview | Claude Code, Codex CLI, Cursor; Cursor live-smoke pending in verification matrix |
| Opt-in client install | ⚠ Preview | VS Code, Gemini-CLI, Qwen-CLI, Antigravity (stdio-relay), Zed (stdio-relay), Kiro, Windsurf, Cline, Kilo Code, OpenCode, Hermes, OpenClaw; built from upstream config docs, live smoke pending |
| GUI dashboard (`mcphub gui`) | ⚠ Preview | Loopback-only; CSRF/DNS-rebind hardened (PR #51); manual GUI browser smoke pending |
| GUI logs viewer (`/api/logs/:server`) | ⚠ Preview | SSE tail follow + filter + ERROR/WARN highlight + Open folder all shipped |
| Workspace-scoped LSP lazy proxies | ⚠ Preview | `mcphub register` + per-language proxy; D3 manual multi-language smoke pending |
| Encrypted secrets vault | ⚠ Preview | age-encrypted; argv-leak removed (PR #128); open cross-process last-write-wins limitation tracked in `work-items/bugs/a3a-vault-concurrent-edit-lww.md` |
| Local manifest authoring (GUI Add server / `mcphub manifest create`) | ⚠ Preview | Form + `Paste YAML` import; YAML smuggling hardened (PR #51) but still surface-may-change before 1.0 |
| Backups, rollback, migration | ⚠ Preview | `backups.keep_n` enforced + per-write timestamped; tracked race in interleaved migrate/demigrate (`work-items/bugs/b1-backup-file-race.md`) |
| Per-server HTTP API (`/mcp` per daemon) | ⚠ Preview | DNS-rebind + Content-Type + body-size guards; GET/SSE server-notification semantics still being reconciled |
| Unified health/status snapshot | 🚧 Roadmap | G2, immediately ahead of preview tag — combines ping/status/version + probes |
| Capability browser (tools/resources/prompts) | 🚧 Roadmap | G3, post preview-tag |
| Marketplace / remote manifests | 🚧 Roadmap | G5/G6/G7 — Phase 3C/3D |

## License

Mozilla Public License Version 2.0 (`MPL-2.0`) — see [LICENSE](LICENSE).

Commercial licenses and commercial versions are available by separate agreement
for teams that need different licensing terms, private distribution, support,
warranty, indemnity, integration, packaging, or proprietary deployment terms.
To discuss a commercial version or commercial license, contact Dmitry Denisenko
through GitHub at [@applicate2628](https://github.com/applicate2628).

Unless and until a separate commercial agreement is signed, use, modification,
and distribution of this repository remain governed by `MPL-2.0`. A commercial
offer does not reduce the rights granted for the public source code under
`MPL-2.0`.

Copyright 2026 Dmitry Denisenko ([@applicate2628](https://github.com/applicate2628))

## Terms and Abbreviations

- `CLI`: Command-Line Interface; commands such as `mcphub install` and
  `mcphub status`.
- `Commercial license`: separate private agreement for different licensing,
  support, distribution, warranty, indemnity, integration, packaging, or
  deployment terms.
- `Cursor`: Cursor editor/agent client; default MCP client target in this
  preview.
- `GUI`: Graphical User Interface; the embedded local web interface and tray
  surface.
- `MCP`: Model Context Protocol; the protocol used by managed clients and
  servers.
- `MPL-2.0`: Mozilla Public License Version 2.0; the open-source license used
  by this repository.
- `Qwen Code CLI`: Qwen command-line agent client; opt-in MCP client target.
- `SSE`: Server-Sent Events; the HTTP event stream used by the GUI.
- `VS Code`: Visual Studio Code; opt-in MCP client target and future
  workspace-import surface.
