# mcp-local-hub

Run one copy of each [Model Context Protocol](https://modelcontextprotocol.io) server on your workstation, shared across every MCP client that needs it — instead of each client spawning its own redundant stdio process.

## The problem

Every modern coding assistant (Claude Code, Codex CLI, Gemini CLI, Antigravity, Cursor, Continue, …) speaks MCP, and each client independently `exec`s whatever stdio servers you configure — `uvx serena`, `npx @modelcontextprotocol/server-memory`, `mcp-language-server`, and so on. If you use three assistants side-by-side on the same project, you get **three Serena processes**, **three gopls subprocesses**, **three separate memory stores**. Each per-session spawn re-downloads dependencies, re-indexes your code, and competes for RAM.

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
   │seren││memor││seq- ││wolf-││god- ││paper││time │ │gdb  ││lldb ││perf │
   │×2   ││y    ││think││ram  ││bolt ││-srch││     │ │     ││     ││tools│
   │/22  ││9123 ││9124 ││9125 ││9126 ││9127 ││9128 │ │9129 ││9130 ││9131 │
   └──┬──┘└──┬──┘└──┬──┘└──┬──┘└──┬──┘└──┬──┘└──┬──┘ └──┬──┘└──┬──┘└──┬──┘
      │      │      │      │      │      │      │       │      │      │
      └──────┴──────┴──────┴──────┴──────┴──────┴───────┴──────┴──────┘
                                    │
                      shared by all 4 MCP clients
                                    │
                    ┌───────────────┼───────────────┐
                    ▼       ▼       ▼       ▼       ▼
                 Claude  Gemini  Antigravity  Codex CLI
                 Code    CLI     (stdio       (HTTP)
                 (HTTP)  (HTTP)   relay)
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
./mcphub.exe install --server serena       # Phase 1 flagship
./mcphub.exe install --all                 # or all 10 at once

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
| **wolfram** | 9125 | stdio-bridge (node) | Requires `wolfram_app_id` secret |
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
perftools.llvm_objdump(binary="./new_bin", function="hot_loop")
```

## Supported clients

| Client | Version tested | Config path | Transport |
|---|---|---|---|
| Claude Code CLI | 2.1.112 | `~/.claude.json` | HTTP (`type: "http"`) |
| Codex CLI | 0.121.0 | `~/.codex/config.toml` | HTTP (streamable_http) |
| Gemini CLI | 0.38.1 | `~/.gemini/settings.json` | HTTP (`type: "http"`) |
| Antigravity IDE | v0.x | `~/.gemini/antigravity/mcp_config.json` | stdio relay → HTTP |

**Antigravity note:** Cascade rejects loopback-HTTP MCP entries, so `mcp-local-hub` writes a **stdio relay** entry instead — `mcphub.exe relay --server <name> --daemon <d>`. Cascade spawns the relay as a normal stdio subprocess; the relay forwards JSON-RPC to the shared HTTP daemon. No extra server process per Antigravity session.

## CLI surface

### Core operations

| Command | What it does |
|---|---|
| `mcphub setup` | Install binary to `~/.local/bin` and register on user PATH (idempotent) |
| `mcphub install --server <name>` | Create scheduler tasks, write client configs, start daemons |
| `mcphub install --all` | Bulk install every manifest under `servers/` |
| `mcphub install --server <n> --dry-run` | Print plan without applying |
| `mcphub uninstall --server <name>` | Remove scheduler tasks + client entries (backups retained) |
| `mcphub status` | Show state of every `mcp-local-hub-*` task (Running / Scheduled / Stopped) with PID, RAM, uptime, next-run |
| `mcphub restart --server <n>` / `--all` | Stop + re-launch one or all daemons |
| `mcphub stop --server <n>` / `--all` | Stop daemons without uninstalling |
| `mcphub version` | Print version, commit, build metadata |

### Discovery & migration

| Command | What it does |
|---|---|
| `mcphub scan` | Classify every MCP entry across all 4 clients into `via-hub`, `can-migrate`, `unknown`, `per-session`, `not-installed` |
| `mcphub migrate --server <n>` | Rewrite stdio client entries to hub HTTP for a given server |
| `mcphub manifest list` | List every manifest under `servers/*/manifest.yaml` |
| `mcphub manifest show <name>` | Print a manifest's contents |

### Logs, backups, recovery

| Command | What it does |
|---|---|
| `mcphub logs <server> [--tail N]` | Tail daemon's stdout/stderr log |
| `mcphub backups list` | Every `.bak-mcp-local-hub-*` across all 4 clients |
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
| `mcphub settings {get,set,list}` | GUI preferences (theme/shell/default-home — forward-compat for Phase 3B) |

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

## Current status

**Phase 3A complete** — full CLI parity delivered plus the session additions documented in [docs/phase-3a-verification.md](docs/phase-3a-verification.md):

- 10 shipped servers (was 8 after Phase 2)
- 22 user-facing CLI commands
- Go rewrite of godbolt and lldb, embedded as dual-entry servers
- New perftools server wrapping clang-tidy/hyperfine/llvm-objdump/iwyu
- PATH-based install model with `mcphub setup`
- go:embed manifests for filesystem-independent binary
- stdio-child-exit detection integrated with Task Scheduler restart policy

**Earlier phases:**

- **Phase 1** — Serena consolidation across 4 clients ([docs/phase-1-verification.md](docs/phase-1-verification.md))
- **Phase 2** — 7 global daemons added, supergateway → native Go stdio-host ([docs/phase-2-verification.md](docs/phase-2-verification.md))
- **Phase 3A** — CLI parity (scan/migrate/manifest/backups/scheduler/settings) and Go-embedded servers ([docs/phase-3a-verification.md](docs/phase-3a-verification.md))

**Roadmap (not yet started):**

- **Phase 3B — GUI layer** (spec at `docs/superpowers/specs/2026-04-17-phase-3-gui-installer-design.md`) — HTTP + SSE + embedded web UI + system tray + unified "servers × clients" migration matrix
- **Phase 4+** — Linux/macOS scheduler backends (systemd user units + launchd agents)

## Platform support

**Windows 11** is first-class (tested on 10.0.26100). Linux and macOS cross-compile but `mcphub install` fails with "not yet implemented" — the scheduler backend for those platforms is Phase 4 scope. The embedded stdio-bridge / godbolt / perftools servers themselves run fine on Linux and macOS; you just can't yet wire them up as persistent daemons through the OS scheduler.

## License

Apache License 2.0 — see [LICENSE](LICENSE).

Copyright 2026 Dmitry Denisenko ([@applicate2628](https://github.com/applicate2628))
