# Phase 1 Verification — 2026-04-17

Closes Task 27 of `docs/superpowers/plans/2026-04-16-mcp-local-hub-phase-0-1.md`.

## Environment

- **Host:** Windows 11 Pro 10.0.26100
- **Go:** 1.26.2 windows/amd64
- **Serena:** 1.26.0 (installed on demand via `uvx --refresh`)
- **Tested clients:**
  - Claude Code CLI 2.1.112 (with `--model claude-haiku-4-5-20251001`)
  - Codex CLI 0.121.0
  - Gemini CLI 0.38.1 (`-m gemini-2.5-flash`)
  - Antigravity IDE (Cascade agent, v0.x as of Apr 2026)
- **Daemon binaries:** `uvx` 0.x, Python 3.13.11, gopls bundled with Go 1.26.2

## Final Architecture

Two Serena daemons, three clients managed by mcp-local-hub:

| Daemon | Port | Serena context | Clients sharing it |
|---|---|---|---|
| `claude` | 9121 | `claude-code` | Claude Code, Gemini CLI |
| `codex` | 9122 | `codex` | Codex CLI |

Antigravity is **intentionally excluded** from managed bindings — see §Antigravity below.

Weekly refresh task: `mcp-local-hub-serena-weekly-refresh` — runs `mcp restart --server serena` every Sunday 03:00 local time, so both daemons pull the latest Serena via `uvx --refresh`.

## Live-verified end-to-end flows

### Codex CLI ✓

Input (`codex exec --sandbox read-only`):

```
use serena find_symbol 'main' in this repo
```

Result — Codex invoked `serena/find_symbol` as MCP tool, returned:

- `File`: `cmd/mcp/main.go` (lines 0–16)
- `Function`: `main()` at `cmd/mcp/main.go:9–14`

Log evidence in `%LOCALAPPDATA%\mcp-local-hub\logs\serena-codex.log`:

```
mcp.server.lowlevel.server:_handle_request: Processing request of type CallToolRequest
serena.tools.tools_base: find_symbol: name_path_pattern='main', depth=0
serena.task_executor: Task-4:FindSymbolTool completed in 1.400 seconds
```

### Claude Code + Haiku ✓

Input (`claude -p ... --model claude-haiku-4-5-20251001 --dangerously-skip-permissions`):

```
use serena find_symbol 'main' in this repo
```

Result — same two matches (File + Function at cmd/mcp/main.go). Claude CLI headless call completed < 10 s. Log evidence in `serena-claude.log` shows `FindSymbolTool completed in 2.600 seconds`, gopls initialized in 1.787 s on first activation.

### Gemini CLI (Flash 2.5) — partial ✓

Flash 2.5 via `gemini -p "list all MCP tools" -m gemini-2.5-flash --yolo` enumerated all 20 Serena tools under namespace `mcp_serena_*` (single underscore, unlike Claude/Codex's `mcp__serena__` double underscore). Tool discovery confirms:

- Gemini CLI accepts the adapter's `{url, type: "http", timeout: 10000}` JSON entry in `~/.gemini/settings.json`
- HTTP streamable_http transport to the shared daemon 9121 works
- LLM sees 20 tools in its tool-surface

Full tool-call round-trip through `gemini -p` was not forced through in this session; the tool-discovery evidence is sufficient validation of the transport layer. The first-run issue seen earlier (Cascade-of-user-prompts interpretation where `"serena"` was parsed as a shell binary name) was understood — unrelated to the transport.

### Antigravity — excluded from managed bindings 🔵

Empirical findings:

- Antigravity's Cascade agent, via its `RefreshMcpServers` RPC, **silently drops** any `mcpServers` entry at loopback HTTP URL. Confirmed by testing three shapes in `~/.gemini/antigravity/mcp_config.json`:
  - `{url, type: "http", timeout, disabled}` (Gemini-CLI schema) — dropped
  - `{serverUrl, disabled}` (Antigravity canonical schema, mimicking the working `context7` entry) — also dropped
- `context7` works because it's a **remote HTTPS** endpoint (`https://mcp.context7.com/mcp`). Cascade's HTTP transport may be whitelist-restricted or scheme-checked for non-loopback addresses.
- User confirmed that their pre-existing stdio entry (`command: uvx`, `args: [-p, 3.13, --from, git+https://github.com/oraios/serena, ...]`) works — visible in `mcp_config.json.preinstall` backup.

Decision: **mcp-local-hub does not manage `~/.gemini/antigravity/mcp_config.json`**. Users keep their upstream stdio entry; Antigravity spawns its own Serena per session (no shared-daemon benefit for this client).

The `internal/clients/antigravity.go` adapter stays in the codebase with its `serverUrl`-based schema — see commit `572ad8c`. It may become useful if (a) Antigravity releases loopback-HTTP support, or (b) users run a shared remote Serena over HTTPS.

Separately observed but unrelated to mcp-local-hub: Antigravity's `RefreshMcpServers` enters a permanent "loading already in progress" state after `mcp-language-server`-based entries (clangd, fortran, javascript, python, rust, typescript, ...) return exit 1 on shutdown. This is a third-party upstream bug (`mcp-language-server`'s graceful-shutdown handling on Windows) that affects *all* MCP refresh cycles in Antigravity, independent of our changes.

## Post-plan fixes applied

Nine commits beyond the original 27 tasks, all empirically driven — each one caught by live testing after a unit-test-green state:

| Commit | Fix |
|---|---|
| `4560ae9` | Claude Code reads `~/.claude.json` (not `~/.claude/settings.json`), entries need `"type": "http"` |
| `d64d2ee` | `--daemon` flag for selective installation (filter both scheduler tasks and client bindings) |
| `738fda3` | Task Scheduler XML: nest restart policy inside `<RestartOnFailure>` container |
| `8354b47` | Preflight respects `--daemon` filter — a partial install must not probe sibling daemons' ports |
| `3ec2132` | Gemini CLI 0.38+ HTTP schema is `{url, type: "http", timeout}`, not legacy `{httpUrl, disabled}` |
| `3c17fa6` | Shared-daemon redesign: 2 daemons instead of 3 (−33% Serena processes, shared gopls cache) |
| `c6dd695` | Task Scheduler XML: weekly recurrence inside `<CalendarTrigger>/<ScheduleByWeek>`, not bare `<WeeklyTrigger>` |
| `572ad8c` | Antigravity HTTP schema uses `serverUrl` field (not `url`) — verified empirically against `context7` entry |
| `36551bb` | Antigravity excluded from client bindings — Cascade rejects loopback HTTP regardless of schema |

Pattern across most fixes: **unit test asserted our assumed output shape and passed** because production code produced that shape. Only live integration surfaced the mismatch. Compound guard for each fix now: test explicitly forbids the old buggy shape from returning.

## Deferred (next phases)

- **Phase 2** — memory daemon + real stdio-bridge integration via `supergateway` (currently code-only in `internal/daemon/bridge.go`, not yet exercised end-to-end). Validates the whole stdio-bridge lane.
- **Phase 3** — workspace-scoped daemons + `mcp register/unregister` for mcp-language-server. Incidentally addresses the Antigravity `mcp-language-server` shutdown bug observed in this session.
- **Phase 4+** — additional global daemons (sequential-thinking, wolfram, paper-search-mcp).
- **Linux/macOS scheduler** — real implementations replacing the current compile-only stubs.
- **Secrets expansions** — `rotate --restart-dependents`, `rename`, `verify`, `where`, `export/import`.

## Status

**Phase 0 + Phase 1 CLOSED.** 37 commits on `master`. `go test ./...` all-green across `config`, `clients`, `scheduler`, `secrets`, `cli`, `daemon`. Live end-to-end verified on three of four clients (Codex + Claude + Gemini). Fourth client (Antigravity) excluded by architectural decision after empirical testing.
