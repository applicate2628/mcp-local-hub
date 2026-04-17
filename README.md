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
   └──┬──────┬──────┬──────┬──────┬──────┬──────┬──────┬─────────────────┘
      │      │      │      │      │      │      │      │
      ▼      ▼      ▼      ▼      ▼      ▼      ▼      ▼
   ┌─────┐┌─────┐┌─────┐┌─────┐┌─────┐┌─────┐┌─────┐┌─────┐
   │seren││seren││memor││seq- ││wolf-││god- ││paper││time │
   │claud││codex││y    ││think││ram  ││bolt ││-srch││     │
   │9121 ││9122 ││9123 ││9124 ││9125 ││9126 ││9127 ││9128 │
   └──┬──┘└──┬──┘└──┬──┘└──┬──┘└──┬──┘└──┬──┘└──┬──┘└──┬──┘
      │      │      │      │      │      │      │      │
      │      │      └──────┴──────┴──────┴──────┴──────┴── (shared by all 4 clients)
      │      │
   ┌──┼──────┼──────────┬──────────┐
   ▼  ▼      ▼          ▼          ▼
  Claude   Gemini    Antigravity  Codex CLI
  Code     CLI       (stdio       (HTTP)
  (HTTP)   (HTTP)     relay)
```

The 6 non-serena daemons run as **stdio-bridge** via a native Go stdio-host (`internal/daemon/host.go`): one subprocess per daemon, multiplexed across concurrent HTTP clients via JSON-RPC `id` rewriting and a cached `initialize` response.

Antigravity's Cascade agent rejects loopback-HTTP MCP entries, so `mcp-local-hub` bridges it via a **stdio relay subprocess**: `mcp.exe relay` translates between stdio JSON-RPC and the shared HTTP daemon. Cascade sees a normal stdio command; the daemon stays shared.

## Quick start

```bash
# Build
go build -o mcp.exe ./cmd/mcp

# Install all bindings (2 daemons + 4 client configs)
./mcp.exe install --server serena

# Verify
./mcp.exe status
claude mcp get serena    # shows: Status: ✓ Connected, Type: http
```

Detailed setup, per-client behaviour, and troubleshooting in [INSTALL.md](INSTALL.md).

## Supported clients

| Client | Version tested | Config path | Transport |
|---|---|---|---|
| Claude Code CLI | 2.1.112 | `~/.claude.json` | HTTP (`type: "http"`) |
| Codex CLI | 0.121.0 | `~/.codex/config.toml` | HTTP (streamable_http) |
| Gemini CLI | 0.38.1 | `~/.gemini/settings.json` | HTTP (`type: "http"`) |
| Antigravity IDE | v0.x | `~/.gemini/antigravity/mcp_config.json` | stdio relay → HTTP |

**Antigravity note:** Cascade rejects loopback-HTTP MCP entries, so `mcp-local-hub` writes a **stdio relay** entry instead — `mcp.exe relay --server serena --daemon claude`. Cascade spawns the relay as a normal stdio subprocess; the relay forwards JSON-RPC to the shared HTTP daemon on port 9121. No extra Serena process per Antigravity session.

## Key commands

| Command | What it does |
|---|---|
| `mcp install --server <name>` | Create scheduler tasks for each daemon, write client config entries, start daemons |
| `mcp install --server <name> --daemon <d>` | Install only one daemon + its referencing client bindings |
| `mcp install --server <name> --dry-run` | Print the plan without applying |
| `mcp uninstall --server <name>` | Reverse: delete scheduler tasks, remove client entries (backups retained) |
| `mcp rollback` | Restore the latest `.bak-mcp-local-hub-*` for every client |
| `mcp status` | Show state of all `mcp-local-hub-*` Task Scheduler tasks |
| `mcp restart --server <name> \| --all` | Stop + re-run scheduler tasks |
| `mcp daemon --server <n> --daemon <d>` | Invoked by the scheduler; exec the real server with tee'd logs |
| `mcp relay --server <n> --daemon <d>` | stdio↔HTTP bridge for clients that reject loopback HTTP (e.g. Antigravity) |
| `mcp relay --url <url>` | Direct relay to an arbitrary Streamable HTTP endpoint |
| `mcp secrets {init,set,get,list,delete,edit,migrate}` | Manage age-encrypted vault for API keys etc. |
| `mcp version` | Print build version, commit, and date |

## Current status

**Phase 2 complete** (2026-04-17). 8 MCP daemons consolidated:

- serena (×2 contexts: claude-code, codex)
- memory, sequential-thinking, wolfram, godbolt, paper-search-mcp, time

Plus context7 as direct HTTPS entry (no daemon needed). Native Go stdio-host (`internal/daemon/host.go`) replaces `supergateway` npm dep for stdio→HTTP bridging. All 4 clients (Claude Code, Codex CLI, Gemini CLI, Antigravity) share these daemons. See [docs/phase-2-verification.md](docs/phase-2-verification.md).

**Roadmap (not yet implemented):**

- Phase 3: workspace-scoped daemons + `mcp register`/`unregister` for per-project mcp-language-server instances
- Post-Phase-2: minimal GUI installer (scans client configs, checkboxes for hub routing)
- Phase 4+: Linux/macOS scheduler backends

## Platform support

**Windows 11** is first-class (tested on 10.0.26100). Linux and macOS ship compile-only stubs in `internal/scheduler/` — the build succeeds on `GOOS=linux` and `GOOS=darwin`, but `mcp install` fails immediately with "not yet implemented" on those platforms. Real systemd-user-unit and launchd-agent backends are Phase 4 scope.

## License

Apache License 2.0 — see [LICENSE](LICENSE).

Copyright 2026 Dmitry Denisenko ([@applicate2628](https://github.com/applicate2628))
