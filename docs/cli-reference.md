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

## Supervisor lifecycle

| Command | Short | Long |
|---|---|---|
| `mcphub migrate-legacy [--dry-run\|--yes\|--json]` | Detect + migrate disabled mcp-language-server entries into managed registry | Scan every installed MCP client config (Codex + Claude Code) for |
| | | disabled entries whose command is mcp-language-server. For each unique |
| | | workspace, emit one 'mcphub register' — which allocates ports, creates |
| | | scheduler tasks, and writes new client entries for ALL manifest |
| | | languages — and THEN delete the original disabled entries. |
| | | Lazy-mode note: one 'register' call covers every manifest language at |
| | | once, so migration dedupes the detected rows by workspace and emits |
| | | exactly one register per unique workspace (not one per language). |
| | | Interactive by default: prompts per workspace. --yes skips every prompt. |
| | | --dry-run prints the plan without changing any state. |
| | | Examples: |
| | | mcphub migrate-legacy --dry-run    # preview |
| | | mcphub migrate-legacy              # interactive |
| | | mcphub migrate-legacy --yes        # non-interactive |
| | | See also: register, workspaces. |
| `mcphub autostart {enable\|disable\|status} [--strict-mode]` | Manage supervisor autostart at logon | mcphub autostart installs (or removes) an OS-native shim for the |
| | | current platform. |
| | | - Windows managed: Task Scheduler entry `\mcp-local-hub-supervisor` |
| | | with a LogonTrigger; it runs `mcphub gui [--strict-mode]` at |
| | | current-user sign-in. |
| | | - Linux managed: systemd user service |
| | | `~/.config/systemd/user/mcphub-supervisor.service`; it runs |
| | | `mcphub supervise [--strict-mode]` via `systemctl --user enable --now`. |
| | | - macOS managed: LaunchAgent plist |
| | | `~/Library/LaunchAgents/com.applicate2628.mcphub-supervisor.plist`; |
| | | it runs `mcphub supervise [--strict-mode]` via `launchctl bootstrap`. |
| | | - Linux/macOS unmanaged: no autostart backend; run `mcphub supervise` |
| | | manually or enable the managed backend. |
| | | `status` prints one of: absent, enabled-running, enabled-stopped, drifted, |
| | | stale-residue. Drifted means the on-disk shim's args or binary path |
| | | disagree with what `mcphub autostart enable [--strict-mode]` would |
| | | write today; re-run `mcphub autostart enable` to reconcile. |
| `mcphub strict-mode {enable\|disable\|--recover}` | Atomically toggle supervisor strict-mode (intent + autostart shim) | mcphub strict-mode mutates the supervisor's strict-mode policy by |
| | | writing supervisor-intent.json AND the autostart shim's argv in a |
| | | single atomic operation. If either write fails, the other is reverted |
| | | so the two resources never drift. |
| | | enable     — set strict_mode=true; shim launches with --strict-mode. |
| | | disable    — set strict_mode=false; shim launches without the flag. |
| | | --recover  — read the breadcrumb left by a torn previous run and |
| | | reconcile both resources interactively. |
| | | Exit codes: |
| | | 9   STRICT_MODE_BUSY          — migration.lock or --once.lock held by |
| | | another holder; wait and retry. |
| | | 10  STRICT_MODE_REVERT_FAILED — both shim write AND intent revert |
| | | failed; breadcrumb written to |
| | | <state-dir>/strict-mode-mutation-incomplete.json |
| | | and operator must run --recover. |

## Exit codes

`cmd/mcphub` exits 0 on success. On ordinary command errors it prints
`error: ...` and exits 1. The table below is the complete set of mcphub-owned
typed exit codes that the main binary preserves from command code.

| Code | Meaning |
|---|---|
| 0 | success |
| 1 | generic |
| 2 | `mcphub gui --force` diagnostic-only path: printed stale-instance details; no kill attempted |
| 3 | `mcphub gui --force --kill`: race lost or recorded PID already gone |
| | `mcphub gui --reset-port`: refused because another GUI is running |
| 4 | `mcphub gui --force --kill`: pidport malformed/unrecoverable or kill attempt failed |
| 6 | Non-interactive shell requires `--yes`: `mcphub gui --force --kill`, `mcphub gui --reset-port`, `mcphub hub-mcp regenerate-token --client <name>`, or `mcphub hub-mcp regenerate-instance-id` |
| 7 | `mcphub gui --force --kill`: identity gate refused to kill the recorded PID |
| 8 | setup-state-path-rejected |
| | `mcphub gui --reset-port`: refused while hub-aggregate clients are gate-ON |
| 9 | STRICT_MODE_BUSY |
| 10 | STRICT_MODE_REVERT_FAILED |
| 11 | setup elevated override audit required but failed |
| 12 | setup supervisor-liveness task install failed |

## Logs, backups, recovery

| Command | What it does |
|---|---|
| `mcphub logs <server> [--tail N]` | Tail daemon's stdout/stderr log |
| `mcphub backups list` | Every `.bak-mcp-local-hub-*` across managed clients |
| `mcphub backups clean` | Prune old timestamped backups, keep N most recent + pristine sentinel |
| `mcphub backups show <file>` | Diff a backup against the live config |
| `mcphub rollback` | Restore the latest backup for every client |
| `mcphub rollback --original` | Restore the pristine pre-hub sentinel |
| `mcphub cleanup --dry-run` | List candidate orphan MCP server processes (safe sweep) |
| `mcphub cleanup aggressive --client <name>` | Preview live-rooted MCP-stdio processes under a client (codex/claude/...) + print a confirmation token |
| `mcphub cleanup aggressive --client <name> --confirm-aggressive-token <token>` | Kill the previewed live-rooted candidates (token-bound to that exact set) |
| `mcphub cleanup aggressive --root-pid <pid>` | Same, scoped to descendants of an explicit process id |

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
