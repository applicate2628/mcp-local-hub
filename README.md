# mcp-local-hub

Run one copy of each [Model Context Protocol](https://modelcontextprotocol.io) server on your workstation, shared across every MCP client that needs it — instead of each client spawning its own redundant stdio process.

## The problem

Every modern coding assistant (Claude Code, Codex CLI, Gemini CLI, Antigravity, Cursor, Continue, …) speaks MCP, and each client independently `exec`s whatever stdio servers you configure — `uvx serena`, `npx @modelcontextprotocol/server-memory`, `mcp-language-server`, and so on. If you use three assistants side-by-side on the same project, you get **three Serena processes**, **three gopls subprocesses**, **three separate memory stores**. Each per-session spawn re-downloads dependencies, re-indexes your code, and competes for RAM.

## What this tool does

`mcp-local-hub` runs each MCP server **once per OS user**, exposes it as a local HTTP endpoint via [Streamable HTTP transport](https://modelcontextprotocol.io/docs/concepts/transports), and writes the correct client-config entry into each managed MCP client. Clients see a shared daemon instead of their own child process.

```
   ┌───────────────────────────────────────────────────────────────┐
   │              OS-level Task Scheduler (Windows schtasks)        │
   │              starts on logon, restarts on failure              │
   └──────┬─────────────────────────────────┬──────────────────────┘
          │                                 │
          ▼                                 ▼
   ┌──────────────────────┐          ┌──────────────────────┐
   │ Serena daemon 9121   │          │ Serena daemon 9122   │
   │ context=claude-code  │          │ context=codex        │
   │ + one shared gopls   │          │ + one shared gopls   │
   └────────┬─────────────┘          └────────┬─────────────┘
            │                                 │
   ┌────────┼─────────┐                       │
   ▼        ▼         ▼                       ▼
  Claude  Gemini   Antigravity*             Codex CLI
  Code    CLI      (stdio, unmanaged)

  * Antigravity's Cascade agent currently rejects loopback-HTTP MCP
    entries; keeps its upstream stdio spawn separately (see INSTALL.md).
```

## Quick start

```bash
# Build
go build -o mcp.exe ./cmd/mcp

# Install all bindings (2 daemons + 3 client configs)
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
| Antigravity IDE | v0.x | — | not managed (see below) |

**Antigravity caveat:** as of April 2026, Antigravity's Cascade agent silently drops `mcp_config.json` entries pointing at loopback HTTP URLs (tested: Gemini-CLI `{url,type,timeout}` schema, Antigravity-native `{serverUrl,disabled}` schema, and hybrid combinations). Remote HTTPS works (e.g. `context7`), localhost does not. `mcp-local-hub` therefore leaves `~/.gemini/antigravity/mcp_config.json` untouched — users keep their existing stdio entry. If Antigravity gains loopback-HTTP support, re-enable by adding an `antigravity` binding to `servers/serena/manifest.yaml`; the adapter code (`internal/clients/antigravity.go`) still uses the canonical `serverUrl` schema.

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
| `mcp secrets {init,set,get,list,delete,edit,migrate}` | Manage age-encrypted vault for API keys etc. |

## Current status

**Phase 1 complete** (2026-04-17). 38 commits on `master`, `go test ./...` all-green, three of four clients live end-to-end verified (Codex, Claude Haiku, Gemini Flash 2.5). See [docs/phase-1-verification.md](docs/phase-1-verification.md) for the full verification matrix including the nine post-plan fixes applied during live testing.

**Roadmap (not yet implemented):**
- Phase 2: memory server + stdio-bridge via [supergateway](https://github.com/supercorp-ai/supergateway)
- Phase 3: workspace-scoped daemons + `mcp register`/`unregister` for per-project mcp-language-server instances
- Phase 4+: additional global daemons (sequential-thinking, wolfram, paper-search-mcp)
- Linux/macOS scheduler backends (currently Windows-first, Linux/macOS compile-only stubs)

## Platform support

**Windows 11** is first-class (tested on 10.0.26100). Linux and macOS ship compile-only stubs in `internal/scheduler/` — the build succeeds on `GOOS=linux` and `GOOS=darwin`, but `mcp install` fails immediately with "not yet implemented" on those platforms. Real systemd-user-unit and launchd-agent backends are Phase 4 scope.

## License

Apache License 2.0 — see [LICENSE](LICENSE).

Copyright 2026 Dmitry Denisenko ([@applicate2628](https://github.com/applicate2628))
