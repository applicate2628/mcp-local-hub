# Architecture highlights

Deep notes on the five architecture highlights summarized in the
[README](../README.md#architecture-highlights). For the supervisor /
lifecycle design, see [supervisor-architecture.md](supervisor-architecture.md).

## PATH-based install model

Scheduler tasks reference `~/.local/bin/mcphub.exe` by absolute path. `mcphub setup` puts the binary there and registers the directory on user PATH (Windows: `HKCU\Environment\Path` + `WM_SETTINGCHANGE` broadcast; Linux/macOS: prints shell-rc line). Moving or rebuilding the binary later only requires re-running `mcphub setup` — scheduler tasks keep pointing at the canonical path and automatically use the new binary.

## First-run onboarding

A fresh install lands on an empty GUI. Two affordances smooth the first run: pass `mcphub setup --trusted-root <abs-path>` (repeatable) to bless your workspaces as LSP trusted roots up front — it writes the same `lsp-trusted-roots.json` store the GUI **Settings → Trusted Roots** panel writes, via the same hardened idempotent append, so the LSP router auto-registers language servers under those paths without a manual GUI bless. Independently, the GUI shows a dismissable **"Welcome to mcp-local-hub"** banner with a one-click link to the Add-server flow whenever no MCP servers are installed yet (it hides automatically once the first server appears).

## go:embed manifests

All 10 server manifests are baked into the binary via `//go:embed */manifest.yaml`. Daemons load their config from the embedded FS, not from disk, so `~/.local/bin/mcphub.exe` works without a sibling `servers/` directory.

## Dual-entry pattern

Embedded Go servers (godbolt, lldb-bridge, perftools) expose a `NewCommand() *cobra.Command` factory that's imported from two places — `cmd/<name>/main.go` (standalone binary) and `internal/cli/root.go` (hub subcommand). Same code path, zero duplication, two shipping shapes.

## Native Go stdio-host with child-exit detection

Stdio-bridge daemons run external stdio servers (npx/uvx/node/python) via a Go host (`internal/daemon/host.go`) that:

1. Spawns one subprocess per daemon (not per HTTP client)
2. Multiplexes concurrent HTTP clients by rewriting JSON-RPC `id` to an internal atomic counter, then routes responses back via a pending-request map
3. Caches the `initialize` response — first client's result is replayed for all subsequent clients with their own `id` substituted
4. Broadcasts server-initiated notifications (no `id`) to all active SSE subscribers via GET /mcp
5. **Detects child-process exit** via a dedicated `cmd.Wait()` goroutine; propagates the signal up so the daemon exits non-zero and Task Scheduler's `RestartOnFailure` (3 retries, 1min spacing) auto-recovers from npx/uvx children that die mid-session
