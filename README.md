# mcp-local-hub

**One managed local hub for every MCP server you use â€” `N` duplicate processes per server collapse to `1` shared daemon.**

Run one copy of each [Model Context Protocol](https://modelcontextprotocol.io) server on your workstation, shared across every MCP client that needs it â€” instead of each client spawning its own redundant stdio process. Install the binary (`mcphub`) once with `npm`, point your clients at the hub, and stop paying for the same server `N` times.

![mcp-local-hub â€” the GUI dashboard managing the live MCP fleet, and the Catalog with one-click install/uninstall](docs/assets/hero-gui.gif)

<!-- TODO(hero): the GIF above is the GUI tour (the "after" â€” one managed fleet). A short terminal clip of the npm install + the before/after process-count drop can be stitched in front; recording scenario in .scratch/gif/approach-A-terminal-scenario.md. -->

> [!WARNING]
> Preview version: `mcp-local-hub` is actively under development. Interfaces,
> manifests, GUI flows, install/migration behavior, and supported-client wiring
> may still change. Windows 11 is the primary tested path, but not every
> feature, server, client combination, or platform path is fully tested yet; use
> dry-runs and backups before applying changes to important MCP client configs.

## Install (npm)

The fastest path â€” the npm distribution is generally available. The npm
**package** is `mcp-local-hub`; the **command** it installs is `mcphub`.

```bash
# 1. Install the binary globally (the command it installs is `mcphub`)
npm install -g mcp-local-hub

# 2. Put mcphub on your PATH and register state (idempotent)
mcphub setup

# 3. Open the GUI â€” install servers with one click, see every daemon's health
mcphub gui
```

```bash
# Or run once without installing:
npx mcp-local-hub version
```

The meta package ships **no `postinstall` download script** (the top npm
supply-chain attack vector) â€” npm installs only the platform binary that
matches your `os`/`cpu` via an optional dependency, and a tiny Node shim execs
it. Building from source is still supported for dev iteration â€” see
[Building from source](#building-from-source) below.

Detailed setup, per-client behaviour, and troubleshooting in [INSTALL.md](INSTALL.md).

## Why mcphub â€” the problem and the cure

**The problem.** Every modern coding assistant (Claude Code, Codex CLI, Cursor, VS Code, Gemini CLI, Qwen Code CLI, Antigravity, Continue, ...) speaks MCP, and each client independently `exec`s whatever stdio servers you configure â€” `uvx serena`, `npx @modelcontextprotocol/server-memory`, `mcp-language-server`, and so on. Run three assistants side-by-side on the same project and you get **three Serena processes**, **three gopls subprocesses**, **three separate memory stores**. Scale that across the editors, agents, and CLIs a working developer keeps open and you get **dozens of duplicate `node`/`python` processes** â€” each per-session spawn re-downloads dependencies, re-indexes your code, and eats RAM, all to do the same work the process next to it is already doing.

**The cure.** `mcp-local-hub` is one managed local hub: install once, and every client routes through it. Each MCP server runs **once per OS user**, supervised, restarted on failure, and shared â€” so the process tail compresses from **`N` duplicate procs â†’ `1` managed daemon each**.

## What this tool does

`mcp-local-hub` runs each MCP server **once per OS user**, exposes it as a local HTTP endpoint via [Streamable HTTP transport](https://modelcontextprotocol.io/docs/concepts/transports), and writes the correct client-config entry into each managed MCP client. Clients see a shared daemon instead of their own child process.

```
  MCP clients                      mcphub                  shared daemons
  (Claude Code,                                            (one per server,
   Codex, Cursor,        â”Œâ”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”           per OS user)
   VS Code, Gemini,      â”‚   mcphub supervise   â”‚         â”Œâ”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”
   Qwen, â€¦)              â”‚  â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€  â”‚   â”Œâ”€â”€â”€â”€â–¶â”‚ serena Ã—2    â”‚
                         â”‚  supervisor: owns,    â”‚   â”‚     â”œâ”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”¤
   â”Œâ”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”  HTTP    â”‚  restarts, shares     â”‚   â”œâ”€â”€â”€â”€â–¶â”‚ memory       â”‚
   â”‚ client A â”‚ â”€â”€â”€â”€â”€â”€â”€â–¶ â”‚  every daemon         â”‚ â”€â”€â”¤     â”œâ”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”¤
   â”œâ”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”¤  HTTP    â”‚                       â”‚   â”œâ”€â”€â”€â”€â–¶â”‚ godbolt â€¦    â”‚
   â”‚ client B â”‚ â”€â”€â”€â”€â”€â”€â”€â–¶ â”‚  (HTTP front +        â”‚   â”‚     â”œâ”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”¤
   â”œâ”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”¤  HTTP    â”‚   stdio-host /        â”‚   â””â”€â”€â”€â”€â–¶â”‚ +7 more      â”‚
   â”‚ client C â”‚ â”€â”€â”€â”€â”€â”€â”€â–¶ â”‚   embedded Go)        â”‚         â””â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”˜
   â””â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”˜          â””â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”˜
```

Each MCP server runs **once per OS user**; every client shares the same
daemon over loopback HTTP instead of spawning its own copy. See
[docs/supervisor-architecture.md](docs/supervisor-architecture.md) for the
full lifecycle, state-file layout, migration, and per-OS behavior.

Stdio-only MCP servers (memory, time, sequential-thinking, wolfram, gdb, paper-search-mcp) run behind a native Go **stdio-host** (`internal/daemon/host.go`): one subprocess per daemon, multiplexed across concurrent HTTP clients via JSON-RPC `id` rewriting and a cached `initialize` response. Three servers (**godbolt**, **lldb-bridge**, **perftools**) ship as Go code **embedded directly in the mcphub binary** â€” no npm/pip dependency, starts instantly.

Antigravity's Cascade agent rejects loopback-HTTP MCP entries, so `mcp-local-hub` bridges it via a **stdio relay subprocess**: `mcphub relay` translates between stdio JSON-RPC and the shared HTTP daemon. Cascade sees a normal stdio command; the daemon stays shared.

## Building from source

The npm install above is the fastest path. To build from source instead â€” for
dev iteration or to embed your own version metadata:

```bash
# 1. Build (embeds git commit + build date into the binary)
bash build.sh        # Git Bash / WSL / Linux / macOS
# or on Windows native:
pwsh ./build.ps1

# Plain `go build -o mcphub.exe ./cmd/mcphub` also works for dev
# iteration but leaves version metadata as dev/unknown.

# 2. Install to ~/.local/bin and register on user PATH (idempotent)
./mcphub.exe setup
```

Whether you installed via npm or built from source, the rest is identical â€” use
the CLI to install the servers you want shared and verify they connect:

```bash
# Install the MCP servers you want shared
mcphub install --server serena       # default clients: Claude/Codex/Cursor
mcphub install --all                 # all 10 servers, default clients

# Materialize one server in supervisor intent without touching client configs
mcphub install --server fetch --no-client-config

# Optional client targeting
mcphub install --server serena --clients qwen-cli,vscode
mcphub install --server serena --all-clients

# Verify
mcphub status
claude mcp get serena    # shows: Status: âœ“ Connected, Type: http
```

## Ten shipped servers

| Server | Port | Transport | Notes |
|---|---:|---|---|
| **serena** (Ã—2 daemons) | 9121 / 9122 | native-http (uvx) | Flagship: per-client daemons (claude / codex) for context isolation |
| **memory** | 9123 | stdio-bridge (npx) | Shared JSONL write-serialized across all clients |
| **sequential-thinking** | 9124 | stdio-bridge (npx) | Stateless reasoning helper |
| **wolfram** | 9132 | stdio-bridge (node) | Requires `wolfram_app_id` secret |
| **godbolt** | 9126 | **embedded Go** | Compiler Explorer â€” compile/execute/disasm via godbolt.org + optimization remarks, llvm-mca, pahole |
| **paper-search-mcp** | 9127 | stdio-bridge (uvx) | Requires `unpaywall_email` secret |
| **time** | 9128 | stdio-bridge (npx) | Trivial stateless |
| **gdb** | 9129 | stdio-bridge (uv run) | Multi-debugger with session management |
| **lldb** | 9130 | **embedded Go bridge** | Auto-spawns `lldb.exe`, HTTP-multiplexes concurrent clients onto single TCP connection |
| **perftools** | 9131 | **embedded Go** | clang-tidy + llvm-objdump + include-what-you-use over real projects; `hyperfine` is **opt-in only** (RCE surface â€” set `MCP_LOCAL_HUB_ENABLE_UNSAFE_HYPERFINE=1`, see INSTALL) |

Plus **context7** as a direct HTTPS entry (no daemon, no scheduler task).

### Embedded vs external servers

Three servers (`godbolt`, `lldb-bridge`, `perftools`) are implemented as Go packages inside `internal/<name>/` and run as subcommands of the mcphub binary itself â€” no external runtime dependency. Each also ships as an independent standalone binary via `go build ./cmd/<name>` for users who want just that one server without the full hub.

**Performance-review workflow** combining multiple servers in one chat:

```
# audit real project for perf antipatterns
perftools.clang_tidy(files=["src/hot.cpp"], checks="performance-*")

# sanity-check asm on godbolt with optimization remarks
godbolt.compile_code(source=..., filters={optOutput: true, intel: true})

# statistical bench (requires opt-in: MCP_LOCAL_HUB_ENABLE_UNSAFE_HYPERFINE=1 on
# the perftools daemon â€” see INSTALL.md "Opting into hyperfine")
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
| Cline | Opt-in | Preview; built from upstream docs, live smoke pending | VS Code globalStorage `â€¦/saoudrizwan.claude-dev/settings/cline_mcp_settings.json` | HTTP (`type: "streamableHttp"`) |
| Kilo Code | Opt-in | Preview; built from upstream docs, live smoke pending | VS Code globalStorage `â€¦/kilo-code.kilo-code/settings/mcp_settings.json` | HTTP |
| OpenCode | Opt-in | Preview; built from upstream docs, live smoke pending | `~/.config/opencode/opencode.json` | HTTP |
| Hermes | Opt-in | Preview; built from upstream docs, live smoke pending | `~/.hermes/config.yaml` | HTTP |
| OpenClaw | Opt-in | Preview; built from upstream docs, live smoke pending | `~/.openclaw/openclaw.json` | HTTP |

**Antigravity note:** Cascade rejects loopback-HTTP MCP entries, so `mcp-local-hub` writes a **stdio relay** entry instead â€” `mcphub.exe relay --server <name> --daemon <d>`. Cascade spawns the relay as a normal stdio subprocess; the relay forwards JSON-RPC to the shared HTTP daemon. No extra server process per Antigravity session.

## CLI surface

The commands you reach for day to day:

| Command | What it does |
|---|---|
| `mcphub setup` | Install binary to `~/.local/bin` and register on user PATH (idempotent) |
| `mcphub install --server <name>` | Create scheduler tasks, write default client configs, start daemons |
| `mcphub install --server <name> --no-client-config` | Materialize only that server's daemon/supervisor intent; do not read or write MCP client configs |
| `mcphub install --all` | Bulk install every manifest under `servers/` into default clients |
| `mcphub status` | Show state of every `mcp-local-hub-*` task (Running / Scheduled / Stopped) with PID, RAM, uptime, next-run |
| `mcphub restart --server <n>` / `--all` | Stop + re-launch one or all daemons |
| `mcphub scan` | Classify every MCP entry across managed clients (`via-hub`, `can-migrate`, `unknown`, `per-session`, `not-installed`) |
| `mcphub migrate --server <n>` | Rewrite stdio client entries to hub HTTP for a given server |
| `mcphub secrets {init,set,get,list,â€¦}` | Age-encrypted vault for API keys |
| `mcphub rollback` | Restore the latest client-config backup for every client |
| `mcphub version` | Print version, commit, build metadata |

The full command surface â€” install flags, discovery/migration, logs/backups/recovery,
scheduler/secrets, and the hidden transport shims â€” is in
[docs/cli-reference.md](docs/cli-reference.md).

## Architecture highlights

- **PATH-based install model** â€” scheduler tasks reference `~/.local/bin/mcphub.exe` by absolute path; `mcphub setup` puts the binary there and registers it on user PATH.
- **First-run onboarding** â€” `mcphub setup --trusted-root` blesses LSP trusted roots up front; the GUI shows a dismissable welcome banner until the first server is installed.
- **go:embed manifests** â€” all 10 server manifests are baked into the binary, so the binary runs without a sibling `servers/` directory.
- **Dual-entry pattern** â€” embedded Go servers expose a `NewCommand()` factory imported by both the standalone binary and the hub subcommand; one code path, two shipping shapes.
- **Native Go stdio-host with child-exit detection** â€” one subprocess per daemon, multiplexed across concurrent HTTP clients, with child-exit detection feeding Task Scheduler's restart policy.

See [docs/architecture-highlights.md](docs/architecture-highlights.md) for the full prose on each highlight, and [docs/supervisor-architecture.md](docs/supervisor-architecture.md) for supervisor / lifecycle ×}¶ç«h‘éì¶»§q«^uÉÉ½É˜ ‰É•…É•ÍÁ½¹Í”è€•Üˆ°•ÉÈ¤(%ô(%¥˜€…É•ÍÀ¹=,ì($%É•ÑÕÉ¸™…±Í”°¹¥°(%ô(%É•ÍÕ±Ğ°|€èôÉ•ÍÀ¹I•ÍÕ±Ğ¸¡µ…ÁmÍÑÉ¥¹u…¹ä¤(%¥˜É•ÍÕ±Ğ€ôô¹¥°ì($%É•ÑÕÉ¸™…±Í”°¹¥°(%ô(%¥˜Ø°½¬€èôÉ•ÍÕ±Ñl‰É•½¹¥±•}É•…‘ä‰t¸¡‰½½°¤ì½¬ì($%É•ÑÕÉ¸Ø°¹¥°(%ô(%É•ÑÕÉ¸™…±Í”°¹¥°)ô((¼¼€´´´´´´´´´´´´´´´´´´´´´´´´´´´´´´´´´´´´´´´´´´´´´´´´´´´´´´´´´´´´´´´´´´´´´´´´´´´(¼¼1½Üµ±•Ù•°%A™É…µ”$½<¸(¼¼€´´´´´´´´´´´´´´´´´´´´´´´´´´´´´´´´´´´´´´´´´´´´´´´´´´´´´´´´´´´´´´´´´´´´´´´´´´´((¼¼Í­¥Á!•±±½É…µ”É•…‘Ì€¬‘¥Í…É‘ÌÑ¡”ÍÕÁ•ÉÙ¥Í½ÈÌ¡•±±¼™É…µ”…Ğ(¼¼½¹¹•Ñ¥½¸ÍÑ…ÉĞ¸)™Õ¹ŒÍ­¥Á!•±±½É…µ”¡½¹¸¹•Ğ¹½¹¸¤•ÉÉ½Èì(%Ù…È‰Õ˜lĞÀäÙu‰åÑ”(%™½È¤€èô€Àì¤€ğ€ĞÀäØì¤¬¬ì($%¸°•ÉÈ€èô½¹¸¹I•…¡‰Õ™m¤€è¤¬Åt¤($%¥˜•ÉÈ€„ô¹¥°ì($$%É•ÑÕÉ¸•ÉÈ($%ô($%¥˜¸€ôô€Àì($$%½¹Ñ¥¹Õ”($%ô($%¥˜‰Õ™m¥t€ôô€q¸œì($$%É•ÑÕÉ¸¹¥°($%ô(%ô(%É•ÑÕÉ¸™µĞ¹ÉÉ½É˜ ‰¡•±±¼™É…µ”•á••‘•€Ğ-¥ˆ¤)ô((¼¼Ù•É¥™å!•±±½É…µ”É•…‘ÌÑ¡”¡•±±¼™É…µ”…¹Ù…±¥‘…Ñ•Ì¥Ğ……¥¹ÍĞÑ¡”(¼¼•áÁ•Ñ•ÍÕÁ•ÉÙ¥Í½È¹±½¬½İ¹•È¸5¥Íµ…Ñ É•ÑÕÉ¹Ì…¸•ÉÉ½ÈìÑ¡”(¼¼…±±•È±½Í•ÌÑ¡”½¹¹•Ñ¥½¸¸)™Õ¹ŒÙ•É¥™å!•±±½É…µ”¡½¹¸¹•Ğ¹½¹¸°•áÁ•Ñ•…Á¤¹MÕÁ•ÉÙ¥Í½É1½­=İ¹•È¤•ÉÉ½Èì(%±¥¹”°•ÉÈ€èôÉ•…‘1¥¹”¡½¹¸°€ĞÀäØ¤(%¥˜•ÉÈ€„ô¹¥°ì($%É•ÑÕÉ¸•ÉÈ(%ô(%Ù…È•¹ØÍÑÉÕĞì($%!•±±¼…Á¤¹%A!•±±¼©Í½¸è‰¡•±±¼‰€(%ô(%¥˜•ÉÈ€èô©Í½¸¹U¹µ…ÉÍ¡…°¡±¥¹”°€™•¹Ø¤ì•ÉÈ€„ô¹¥°ì($%É•ÑÕÉ¸™µĞ¹ÉÉ½É˜ ‰‘•½‘”¡•±±¼è€•Ü€¡É…Üô•Ä¤ˆ°•ÉÈ°ÍÑÉ¥¹œ¡±¥¹”¤¤(%ô(%¥˜•áÁ•Ñ•¹A%€ôô€À€˜˜•áÁ•Ñ•¹MÑ…ÉÑ•‘Ğ€ôô€ˆˆì($$¼¼9¼½İ¹•ÈÍ¥‘•…ÈÑ¼½µÁ…É”……¥¹ÍĞƒŠP‰•ÍĞµ•™™½ÉĞ°…•ÁĞ¸($%É•ÑÕÉ¸¹¥°(%ô(%¥˜€……Á¤¹Y…±¥‘…Ñ•!…¹‘Í¡…­”¡•¹Ø¹!•±±¼°•áÁ•Ñ•¤ì($%É•ÑÕÉ¸™µĞ¹ÉÉ½É˜ ‰¡•±±¼µ¥Íµ…Ñ è½ĞÁ¥ô•ÍÑ…ÉÑ•‘}…Ğô•Ì•áÁ•Ñ•Á¥ô•ÍÑ…ÉÑ•‘}…Ğô•Ìˆ°($$%•¹Ø¹!•±±¼¹A%°•¹Ø¹!•±±¼¹MÑ…ÉÑ•‘Ğ°•áÁ•Ñ•¹A%°•áÁ•Ñ•¹MÑ…ÉÑ•‘Ğ¤(%ô(%É•ÑÕÉ¸¹¥°)ô((¼¼İÉ¥Ñ•É…µ”)M=8µ•¹½‘•ÌÉ•Ä€¬…ÁÁ•¹‘Ì„ÑÉ…¥±¥¹œ¹•İ±¥¹”€¡Ñ¡”(¼¼ÍÕÁ•ÉÙ¥Í½ÈÌ™É…µ”‘•±¥µ¥Ñ•ÈÁ•ÈÍÁ•Œƒ
œ‰]¥É”™½Éµ…Ğˆ¤¸)™Õ¹ŒİÉ¥Ñ•É…µ”¡½¹¸¹•Ğ¹½¹¸°É•Ä…Á¤¹%AI•ÅÕ•ÍĞ¤•ÉÉ½Èì(%É…Ü°•ÉÈ€èô©Í½¸¹5…ÉÍ¡…°¡É•Ä¤(%¥˜•ÉÈ€„ô¹¥°ì($%É•ÑÕÉ¸™µĞ¹ÉÉ½É˜ ‰µ…ÉÍ¡…°É•ÅÕ•ÍĞè€•Üˆ°•ÉÈ¤(%ô(%É…Ü€ô…ÁÁ•¹¡É…Ü°€q¸œ¤(%¥˜|°•ÉÈ€èô½¹¸¹]É¥Ñ”¡É…Ü¤ì•ÉÈ€„ô¹¥°ì($%É•ÑÕÉ¸•ÉÈ(%ô(%É•ÑÕÉ¸¹¥°)ô((¼¼É•…‘É…µ”É•…‘Ì½¹”)M=8±¥¹”™É½´½¹¸…¹‘•½‘•Ì¥Ğ¥¹Ñ¼…¸(¼¼%AI•ÍÁ½¹Í”¸)™Õ¹ŒÉ•…‘É…µ”¡½¹¸¹•Ğ¹½¹¸¤€¡…Á¤¹%AI•ÍÁ½¹Í”°•ÉÉ½È¤ì(%±¥¹”°•ÉÈ€èôÉ•…‘1¥¹”¡½¹¸°€ÄØÌàĞ¤(%¥˜•ÉÈ€„ô¹¥°ì($%É•ÑÕÉ¸…Á¤¹%AI•ÍÁ½¹Í•íô°•ÉÈ(%ô(%Ù…ÈÉ•ÍÀ…Á¤¹%AI•ÍÁ½¹Í”(%¥˜•ÉÈ€èô©Í½¸¹U¹µ…ÉÍ¡…°¡±¥¹”°€™É•ÍÀ¤ì•ÉÈ€„ô¹¥°ì($%É•ÑÕÉ¸…Á¤¹%AI•ÍÁ½¹Í•íô°™µĞ¹ÉÉ½É˜ ‰‘•½‘”É•ÍÁ½¹Í”è€•Ü€¡É…Üô•Ä¤ˆ°•ÉÈ°ÍÑÉ¥¹œ¡±¥¹”¤¤(%ô(%É•ÑÕÉ¸É•ÍÀ°¹¥°)ô((¼¼É•…‘1¥¹”É•…‘Ì‰åÑ•ÌÕ¹Ñ¥°€q¸œ½Èµ…à¥Ì¡¥Ğ¸I•ÑÕÉ¹ÌÑ¡”±¥¹”(¼¼]%Q!=UPÑ¡”ÑÉ…¥±¥¹œ¹•İ±¥¹”¸I•ÑÕÉ¹Ì•ÉÉ½È½¸µ…à•á••‘•¸)™Õ¹ŒÉ•…‘1¥¹”¡½¹¸¹•Ğ¹½¹¸°µ…à¥¹Ğ¤€¡mu‰åÑ”°•ÉÉ½È¤ì(%‰Õ˜€èôµ…­”¡mu‰åÑ”°€À°€ÈÔØ¤(%™½È¤€èô€Àì¤€ğµ…àì¤¬¬ì($%Ù…ÈˆlÅu‰åÑ”($%¸°•ÉÈ€èô½¹¸¹I•…¡‰lét¤($%¥˜•ÉÈ€„ô¹¥°ì($$%É•ÑÕÉ¸¹¥°°•ÉÈ($%ô($%¥˜¸€ôô€Àì($$%½¹Ñ¥¹Õ”($%ô($%¥˜‰lÁt€ôô€q¸œì($$%É•ÑÕÉ¸‰Õ˜°¹¥°($%ô($%‰Õ˜€ô…ÁÁ•¹¡‰Õ˜°‰lÁt¤(%ô(%É•ÑÕÉ¸‰Õ˜°™µĞ¹ÉÉ½É˜ ‰±¥¹”•á••‘•€•‰åÑ•Ìˆ°µ…à¤)ô((¼¼€´´´´´´´´´´´´´´´´´´´´´´´´´´´´´´´´´´´´´´´´´´´´´´´´´´´´´´´´´´´´´´´´´´´´´´´´´´´(¼¼5¥ÍŒ¡•±Á•ÉÌ¸(¼¼€´´´´´´´´´´´´´´´´´´´´´´´´´´´´´´´´´´´´´´´´´´´´´´´´´´´´´´´´´´´´´´´´´´´´´´´´´´´((¼¼MµÄÉ•µ½Ù•Ñ¡”UMI95µ‰…Í•ÍÕÁ•ÉÙ¥Í•%AA¥Á•A…Ñ €¬ÕÉÉ•¹Ñ]¥¹‘½İÍUÍ•É¹…µ”(¼¼¡•±Á•ÉÌ¸Ù•Éä%A‘¥…°¥¸Ñ¡”ÕÁÉ…‘”½µ¥É…Ñ”™±½Ü¹½ÜÕÍ•ÌÑ¡”M%µ‰…Í•(¼¼…¹½¹¥…°É•Í½±Ù•È…Á¤¹MÕÁ•ÉÙ¥Í½É%A‘‘É•ÍÌ€¡Ñ¡”Á…Ñ Ñ¡”ÍÕÁ•ÉÙ¥Í½È1%MQ9L(¼¼½¸¤°±½Í¥¹œÑ¡”AH€ŒÈÄÈÈÌM%µ½¹Í¥ÍÑ•¹äÁÉ½Á……Ñ¥½¸…À¸((¼¼É•…‘AÉ•5¥É…Ñ¥½¹MÑÉ¥Ñ5½‘”É•…‘ÌÍÑÉ¥Ñ}µ½‘”™É½´ÍÕÁ•ÉÙ¥Í½Èµ¥¹Ñ•¹Ğ¹©Í½¸(¼¼¥˜ÁÉ•Í•¹Ğ¸I•ÑÕÉ¹Ì™…±Í”İ¡•¸Ñ¡”™¥±”¥Ìµ¥ÍÍ¥¹œ¸)™Õ¹ŒÉ•…‘AÉ•5¥É…Ñ¥½¹MÑÉ¥Ñ5½‘”¡ÍÑ…Ñ•¥ÈÍÑÉ¥¹œ¤€¡‰½½°°•ÉÉ½È¤ì(%Á…Ñ €èô™¥±•Á…Ñ ¹)½¥¸¡ÍÑ…Ñ•¥È°€‰ÍÕÁ•ÉÙ¥Í½Èµ¥¹Ñ•¹Ğ¹©Í½¸ˆ¤(%¥¹Ñ•¹Ğ°•ÉÈ€èô…Á¤¹I•…‘MÕÁ•ÉÙ¥Í½É%¹Ñ•¹Ğ¡Á…Ñ ¤(%¥˜•ÉÈ€„ô¹¥°ì($%¥˜½Ì¹%Í9½Ñá¥ÍĞ¡•ÉÈ¤ñğ•ÉÉ½ÉÌ¹%Ì¡•ÉÈ°½Ì¹ÉÉ9½Ñá¥ÍĞ¤ì($$%É•ÑÕÉ¸™…±Í”°¹¥°($%ô($$¼¼MÕÉ™…”½Ñ¡•È•ÉÉ½ÉÌÍ¼Ñ¡”…±±•È…¸‘•¥‘”İ¡•Ñ¡•ÈÑ¼($$¼¼…‰½ÉĞ½ÈÁÉ½••İ¥Ñ ÍÑÉ¥Ñ}µ½‘”õ™…±Í”¸($%É•ÑÕÉ¸™…±Í”°•ÉÈ(%ô(%É•ÑÕÉ¸¥¹Ñ•¹Ğ¹MÑÉ¥Ñ5½‘”°¹¥°)ô((¼¼‘ÕÉAÑÈÉ•ÑÕÉ¹Ì„Á½¥¹Ñ•ÈÑ¼¸İ¥¹¥¼¹¥…±A¥Á”Ñ…­•Ì„€©Ñ¥µ”¹ÕÉ…Ñ¥½¸(¼¼€¡¹¥°€ô¹¼Ñ¥µ•½ÕĞ¤ìÑ¡”¡•±Á•ÈÍ…Ù•ÌÑ¡”…±±Í¥Ñ”™É½´„±½…°Ù…È¸)™Õ¹Œ‘ÕÉAÑÈ¡Ñ¥µ”¹ÕÉ…Ñ¥½¸¤€©Ñ¥µ”¹ÕÉ…Ñ¥½¸ì(%É•ÑÕÉ¸€™)ô