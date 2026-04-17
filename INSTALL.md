# Installation

## Prerequisites

1. **Go 1.22+** — `go version` must succeed. Tested on 1.26.2 windows/amd64.
2. **Git for Windows** (includes Git Bash; the CLI expects Unix-style shell for some setup commands).
3. **uvx** — Python package runner, needed by Serena. Install via [uv](https://github.com/astral-sh/uv):
   ```powershell
   powershell -c "irm https://astral.sh/uv/install.ps1 | iex"
   ```
   Then verify: `uvx --version`.
4. **Windows 11** recommended. Windows 10 should work but is untested. Linux/macOS currently fail at `mcp install` — stubs only.
5. **An MCP client or two** (Claude Code, Codex CLI, Gemini CLI, Cursor, Continue…) already installed on the machine.

## Build

```bash
cd d:\dev\mcp-local-hub
go build -o mcp.exe ./cmd/mcp
```

On success: `mcp.exe` appears in the repo root (~12 MB, includes Windows version resource metadata).

Optional: add the repo directory to `PATH` so you can run `mcp` from anywhere. Until then every command is `./mcp.exe ...`.

## First install

Seven servers ship with manifests: `serena`, `memory`, `sequential-thinking`, `wolfram`, `godbolt`, `paper-search-mcp`, `time`. Each is installed independently. Start with Serena (Phase 1 flagship):

```bash
# Preview what would happen (no side effects)
./mcp.exe install --server serena --dry-run

# Apply: creates 3 Task Scheduler tasks, writes 4 client configs, starts both daemons
./mcp.exe install --server serena
```

Expected output:

```
✓ Scheduler task created: mcp-local-hub-serena-claude
✓ Scheduler task created: mcp-local-hub-serena-codex
✓ Scheduler task created: mcp-local-hub-serena-weekly-refresh
  backup: C:\Users\<you>\.claude.json.bak-mcp-local-hub-<timestamp>
✓ claude-code → http://localhost:9121/mcp
  backup: C:\Users\<you>\.codex\config.toml.bak-mcp-local-hub-<timestamp>
✓ codex-cli → http://localhost:9122/mcp
  backup: C:\Users\<you>\.gemini\settings.json.bak-mcp-local-hub-<timestamp>
✓ gemini-cli → http://localhost:9121/mcp
  backup: C:\Users\<you>\.gemini\antigravity\mcp_config.json.bak-mcp-local-hub-<timestamp>
✓ antigravity → relay (mcp.exe relay --server serena --daemon claude)
✓ Started: mcp-local-hub-serena-claude
✓ Started: mcp-local-hub-serena-codex

Install complete.
```

First `✓ Started` triggers `uvx` to download Serena (~30 seconds on a fresh machine). After that Serena processes live on ports 9121 and 9122.

Verify:

```bash
./mcp.exe status        # 3 tasks; -claude and -codex Running, -weekly-refresh Ready
claude mcp get serena   # Status: ✓ Connected, Type: http, URL: http://localhost:9121/mcp
codex mcp get serena    # enabled: true, transport: streamable_http
```

### Partial install (one daemon only)

```bash
./mcp.exe install --server serena --daemon codex
```

Creates only the `codex` daemon (port 9122), applies only the `codex-cli` client binding, skips weekly refresh. Useful when trying it out on a single client first.

## Per-client notes

### Claude Code

Writes to `~/.claude.json` (user scope) — the single-file config at your home directory, not `~/.claude/settings.json` (that file holds UI preferences and is ignored for MCP).

HTTP entry shape:
```json
"mcpServers": {
  "serena": {
    "type": "http",
    "url": "http://localhost:9121/mcp"
  }
}
```

**If you had Claude Code open before `install`, restart the Claude session.** Claude reads the MCP list at session start and caches it; `claude mcp get serena` will show Connected immediately, but the current chat session will not see `serena` tools until you start a fresh one.

### Codex CLI

Writes to `~/.codex/config.toml`:
```toml
[mcp_servers.serena]
startup_timeout_sec = 10.0
url = 'http://localhost:9122/mcp'
```

Same session-cache caveat as Claude: restart the Codex CLI after install for its agent to pick up the new MCP. `codex exec` always starts a fresh session, so experiments via `codex exec "use find_symbol..."` bypass the caching issue.

### Gemini CLI

Writes to `~/.gemini/settings.json`:
```json
"mcpServers": {
  "serena": {
    "url": "http://localhost:9121/mcp",
    "type": "http",
    "timeout": 10000
  }
}
```

Namespace for Serena tools inside a Gemini prompt is `mcp_serena_*` (single underscore), not `mcp__serena__*` (double underscore) as in Claude/Codex. Example:
```bash
gemini -p "use mcp_serena_find_symbol with name_path=main" -m gemini-2.5-flash --yolo
```

### Antigravity (Cascade)

Antigravity's Cascade agent silently drops any `mcp_config.json` entry pointing at a loopback HTTP URL. `mcp-local-hub` works around this by writing a **stdio relay** entry instead:

```json
"serena": {
  "command": "D:\\dev\\mcp-local-hub\\mcp.exe",
  "args": ["relay", "--server", "serena", "--daemon", "claude"],
  "disabled": false
}
```

Cascade spawns `mcp.exe relay` as a normal stdio subprocess. The relay translates JSON-RPC between stdin/stdout and the shared HTTP daemon on port 9121. No extra Serena process per Antigravity session — it shares the same daemon as Claude Code and Gemini CLI.

**After install, restart Antigravity** for Cascade to pick up the new entry:
```powershell
Get-Process -Name Antigravity | Stop-Process -Force
Start-Sleep 3
Start-Process "$env:LOCALAPPDATA\Programs\Antigravity\Antigravity.exe"
```

The relay binary path is absolute (points at `mcp.exe` in the repo root). If you move the binary, re-run `mcp install --server serena` to update the path.

## Per-server notes (beyond serena)

Phase 2 added 6 global daemons. Each has its own manifest in `servers/<name>/manifest.yaml`.

### memory (port 9123)

Runs `npx -y @modelcontextprotocol/server-memory`. Stores data in
`MEMORY_FILE_PATH` (default set to `c:/Users/dima_/OneDrive/Documents/env/Agents/memory.jsonl`
in the manifest — update for your system before install). This is the
critical daemon — previously each client spawned its own memory server,
causing concurrent writes to the same JSONL file (data race). The
shared daemon serializes all writes through one subprocess.

### sequential-thinking (port 9124)

Runs `npx -y @modelcontextprotocol/server-sequential-thinking`. Stateless
reasoning helper. No env needed.

### wolfram (port 9125)

Runs `node C:/Users/dima_/.local/mcp-servers/wolframalpha-llm-mcp/build/index.js`.
Requires the Wolfram LLM MCP server installed separately at that path.
`WOLFRAM_LLM_APP_ID` is stored in the encrypted vault:

```bash
mcp secrets set wolfram_app_id --value <your-app-id>
```

### godbolt (port 9126)

Runs `python C:/Users/dima_/.local/mcp-servers/godbolt-mcp/godbolt_mcp.py`
from the godbolt venv. Requires the godbolt-mcp project installed at that
path. Stateless (API proxy to godbolt.org).

### paper-search-mcp (port 9127)

Runs `uvx --from paper-search-mcp python -m paper_search_mcp.server`.
Requires `uvx`. `PAPER_SEARCH_MCP_UNPAYWALL_EMAIL` is stored in the vault:

```bash
mcp secrets set unpaywall_email --value <your-email>
```

First install may take ~30s as `uvx` downloads `paper-search-mcp`.

### time (port 9128)

Runs `npx -y @mcpcentral/mcp-time`. Trivial, stateless.

### context7 (no daemon)

Available at `https://mcp.context7.com/mcp` as a remote HTTPS endpoint.
Codex CLI, Gemini CLI, and Antigravity typically have it pre-configured.
For Claude Code, add it manually:

```bash
claude mcp add --transport http context7 https://mcp.context7.com/mcp
```

## Uninstall & rollback

```bash
# Remove scheduler tasks and client entries
./mcp.exe uninstall --server serena

# Restore client configs from the latest backup (pre-install state)
./mcp.exe rollback
```

Backups are named `<config>.bak-mcp-local-hub-YYYYMMDD-HHMMSS` and live next to each client config. Uninstall does NOT delete them — keep as long as you want or clean up manually.

`uninstall` does not kill already-running Serena Python processes (Task Scheduler deletes the task metadata, not live children). If a daemon is still bound to 9121/9122 after uninstall:
```powershell
Stop-Process -Name python -Force -ErrorAction SilentlyContinue
# or specifically:
Get-Process | Where-Object { $_.Path -like '*uvx*' } | Stop-Process -Force
```

## Secrets

For servers that need API keys (wolfram, paper-search-mcp, any OAuth-bearer server):

```bash
./bin/mcp.exe secrets init                         # generate .age-key + empty secrets.age
./bin/mcp.exe secrets set WOLFRAM_APP_ID --value AB123...
./bin/mcp.exe secrets list                         # shows keys, not values
./bin/mcp.exe secrets get WOLFRAM_APP_ID           # copies to clipboard by default
./bin/mcp.exe secrets get WOLFRAM_APP_ID --show    # prints to stdout
./bin/mcp.exe secrets edit                         # open decrypted vault in $EDITOR
./bin/mcp.exe secrets migrate --from-client codex-cli   # scan existing config for API keys, interactively import
```

### Where the secret files live

Both files live in the per-user data directory, **independent of where the repo or mcp.exe is installed**:

| OS | Path |
|---|---|
| Windows | `%LOCALAPPDATA%\mcp-local-hub\` — typically `C:\Users\<you>\AppData\Local\mcp-local-hub\` |
| Linux | `$XDG_DATA_HOME/mcp-local-hub/` — default `~/.local/share/mcp-local-hub/` |
| macOS | `~/Library/Application Support/mcp-local-hub/` |

Two files are stored there:

- `.age-key` — private identity file, ~75 bytes. **Never commit, never email in plaintext.** Treat like an SSH private key.
- `secrets.age` — encrypted vault containing your actual secret values. Opaque ciphertext without `.age-key`.

### Transferring to another machine

1. Install `mcp-local-hub` on the new machine (clone + `./build.sh`)
2. Copy both files from the old machine's data dir to the new machine's data dir (path from the table above):
   - Through a password manager (Bitwarden secure notes, 1Password, etc.)
   - Through an encrypted USB stick
   - Through `scp` / `rsync` / `rclone` with a trusted transport
   - Through a **private** GitHub repository (public repos with `.age-key` is a critical leak)
3. Run `./bin/mcp.exe secrets list` on the new machine — should print your keys without error. If it errors with "failed to decrypt", the `.age-key` or `secrets.age` didn't copy correctly.

### Manifest env references use prefixes

- `secret:KEY` — look up in encrypted vault
- `file:KEY` — look up in `config.local.yaml` (gitignored)
- `$VAR` — read OS environment variable
- anything else — literal value

### Backup

Losing `.age-key` means the vault is unreadable — there is no recovery path. Keep at least one copy outside the primary machine (password manager is ideal).

## Troubleshooting

### `port 9121 already in use`

Preflight caught another listener on the port Serena wants. Either:
- Another Serena instance is already running (from a previous manual stdio setup) — kill it: `Get-Process -Name python | Where-Object { $_.Path -like '*uvx*' } | Stop-Process`
- A different local service is using 9121 — change the port in `servers/serena/manifest.yaml` and re-install

### `command "uvx" not found on PATH`

Install `uv` (see Prerequisites). Restart your shell afterwards so `PATH` picks up the new binary.

### `error: create task ...: schtasks /Create: exit status 1`

If the error mentions a specific XML element (`(N,M):ElementName:`), it's a schema violation in `scheduler_windows.go`. Please file an issue with the exact message — both known XML bugs (`RestartInterval` flat, `WeeklyTrigger` direct child) are already fixed, any new one would be a regression or Windows version difference.

### Serena installs but client doesn't see it

- Did you restart the client session? (Claude/Codex/Gemini cache MCP list at start — see per-client notes above.)
- Is port 9121 / 9122 actually listening? `powershell "Get-NetTCPConnection -LocalPort 9121"`
- Does `<client> mcp get serena` report Connected? If yes but tools aren't used, it's a prompt-interpretation issue — try `"use mcp__serena__find_symbol with name=main"` instead of `"use serena find_symbol"`.

### Antigravity's `RefreshMcpServers: loading already in progress` loop

Unrelated to mcp-local-hub — this is a third-party bug in `mcp-language-server` (returns exit 1 on graceful shutdown, blocking Antigravity's refresh cycle). Full kill + restart usually clears it:
```powershell
Get-Process -Name Antigravity | Stop-Process -Force
Start-Sleep 3
Start-Process "$env:LOCALAPPDATA\Programs\Antigravity\Antigravity.exe"
```

### Logs

Daemon output lives in `%LOCALAPPDATA%\mcp-local-hub\logs\<server>-<daemon>.log`. Rotates at 10 MB, keeps the last 5 rotations.

Scheduler view: `%SystemRoot%\System32\Tasks\mcp-local-hub-*` are the XML task definitions; `taskschd.msc` opens the GUI.

## Next steps

- Read [docs/phase-1-verification.md](docs/phase-1-verification.md) for the full live-test matrix and the nine post-plan fixes applied during real testing.
- Read [docs/superpowers/specs/2026-04-16-mcp-local-hub-design.md](docs/superpowers/specs/2026-04-16-mcp-local-hub-design.md) for the architectural rationale (global vs workspace-scoped daemons, port pool allocation, transport choice, secrets handling).
- If you want to add a new MCP server beyond Serena, copy `servers/serena/manifest.yaml` to `servers/<your-server>/manifest.yaml`, adjust fields, then `mcp install --server <your-server>`. Port must be in 9121–9139 (global range) or 9200–9299 (workspace-scoped) and registered in `configs/ports.yaml`.
