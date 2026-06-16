# CLI reference

Full command reference for `mcphub`. The [README](../README.md#cli-surface)
keeps only the core day-to-day operations; this file is the complete surface.
For install / per-client behaviour / troubleshooting see
[INSTALL.md](../INSTALL.md); for supervisor / lifecycle depth see
[supervisor-architecture.md](supervisor-architecture.md).

## Core operations

| Command | What it does |
|---|---|
| `mcphub setup` | Install binary to `~/.local/bin` and register on user PATH (idempotent) |
| `mcphub setup --trusted-root <abs-path>` | Same as `setup`, plus bless one or more ABSOLUTE workspace paths as LSP trusted roots (repeatable; idempotent) so the GUI LSP router auto-registers language servers under them without a manual GUI bless |
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

## Discovery & migration

| Command | What it does |
|---|---|
| `mcphub scan` | Classify every MCP entry across managed clients into `via-hub`, `can-migrate`, `unknown`, `per-session`, `not-installed` |
| `mcphub migrate --server <n>` | Rewrite stdio client entries to hub HTTP for a given server |
| `mcphub manifest list` | List every manifest under `servers/*/manifest.yaml` |
| `mcphub manifest show <name>` | Print a manifest's contents |

## Logs, backups, recovery

| Command | What it does |
|---|---|
| `mcphub logs <server> [--tail N]` | Tail daemon's stdout/stderr log |
| `mcphub backups list` | Every `.bak-mcp-local-hub-*` across managed clients |
| `mcphub backups clean` | Prune old timestamped backups, keep N most recent + pristine sentinel |
| `mcphub backups show <file>` | Diff a backup against the live config |
| `mcphub rollback` | Restore the latest backup for every client |
| `mcphub rollback --original` | Restore the pristine pre-hub sentinel |
| `mcphub cleanup --dry-run` | List candidate orphan MCP server processes |

## Scheduler & secrets

| Command | What it does |
|---|---|
| `mcphub scheduler upgrade` | Rewrite every task's `<Command>` to the current canonical `mcphub.exe` path |
| `mcphub scheduler weekly-refresh set "SUN 03:00"` | Install a hub-wide weekly `restart --all` task |
| `mcphub scheduler weekly-refresh disable` | Remove the hub-wide weekly task |
| `mcphub secrets {init,set,get,list,delete,edit,migrate}` | Age-encrypted vault for API keys |
| `mcphub settings {get,set,list}` | GUI preference registry for Phase 3B/3B-II surfaces |

## Transport shims (Hidden; called by scheduler, not by humans)

| Command | What it does |
|---|---|
| `mcphub daemon --server <n> --daemon <d>` | Invoked by the scheduler; exec real server with tee'd logs |
| `mcphub relay --server <n> --daemon <d>` | Stdio↔HTTP bridge (for clients that reject loopback-HTTP) |
| `mcphub relay --url <url>` | Direct relay to an arbitrary Streamable HTTP endpoint |
| `mcphub godbolt` | Embedded godbolt MCP server (also ships as `./cmd/godbolt` standalone) |
| `mcphub lldb-bridge <host:port>` | LLDB TCP↔stdio bridge + auto-spawn (also `./cmd/lldb-bridge`) |
| `mcphub perftools` | Embedded perf-toolbox MCP (also `./cmd/perftools`) |
