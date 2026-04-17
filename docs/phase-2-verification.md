# Phase 2 Verification — 2026-04-17

Closes the Phase 2 plan `docs/superpowers/plans/2026-04-17-phase-2-global-daemons.md`.

## Servers

| Server | Port | Transport | Env | Clients |
|---|---:|---|---|---|
| serena (claude) | 9121 | native-http | — | Claude Code, Gemini CLI, Antigravity (relay) |
| serena (codex) | 9122 | native-http | — | Codex CLI |
| memory | 9123 | stdio-bridge (Go host) | MEMORY_FILE_PATH | all 4 |
| sequential-thinking | 9124 | stdio-bridge (Go host) | — | all 4 |
| wolfram | 9125 | stdio-bridge (Go host) | secret:wolfram_app_id | all 4 |
| godbolt | 9126 | stdio-bridge (Go host) | — | all 4 |
| paper-search-mcp | 9127 | stdio-bridge (Go host) | secret:unpaywall_email | all 4 |
| time | 9128 | stdio-bridge (Go host) | — | all 4 |

Plus context7 — `https://mcp.context7.com/mcp` as direct HTTPS entry in Claude Code (no daemon, no scheduler task).

## Architecture change: native Go stdio-host

Replaced the `supergateway` npm dependency with a native Go stdio-host (`internal/daemon/host.go`). The host:

1. **Spawns one stdio subprocess per daemon** — not per HTTP client session.
2. **Multiplexes concurrent HTTP clients** by rewriting JSON-RPC `id` to an internal atomic counter before writing to subprocess stdin. Responses are matched back to the original client by reverse lookup.
3. **Caches the `initialize` response** — the first client's `initialize` is forwarded to the subprocess; subsequent clients get the cached response with their own `id` substituted. Avoids re-initializing a process that's already initialized.
4. **Broadcasts server-initiated notifications** (JSON-RPC without `id`) to all active SSE subscribers via GET /mcp.
5. **Graceful shutdown** — `Stop()` closes a `done` channel that unblocks all POST and SSE handlers within milliseconds, preventing the old 30s-per-handler shutdown stall.

Eliminates the node.js runtime dependency for stdio-bridge daemons.

## Commit trail

| Commit | Description |
|---|---|
| ae630e2 | Reserve ports 9123-9128 + scope doc |
| 8cb5473 | stdio-host subprocess lifecycle (with `os.Environ()` env fix + stdin/goroutine concurrency) |
| 1717f76 | HTTP handler with JSON-RPC id multiplexing |
| 610d47c | initialize cache + SSE + DELETE handlers |
| 23d30a1 | Wire into daemon.go, fix manifest path, shutdown unblock |
| 258d535 | memory daemon (port 9123) |
| da89097 | sequential-thinking daemon (port 9124) |
| 815860a | wolfram daemon (port 9125, APP_ID from vault) |
| d875481 | godbolt daemon (port 9126) |
| a3e6e8a | paper-search-mcp daemon (port 9127) |
| ba265f8 | time daemon (port 9128) |

## Live verification

Task 13 verified end-to-end. All 8 daemon ports respond to MCP `initialize` with valid `result`; `claude mcp list` shows ✓ Connected for all 7 servers (serena, memory, sequential-thinking, wolfram, godbolt, paper-search-mcp, time). Race-detector clean across all tests.

`serverInfo.name` per port:

- 9121, 9122: Serena (v1.26.0)
- 9123: memory-server (v0.6.3)
- 9124: sequential-thinking-server (v0.2.0)
- 9125: wolframalpha-llm (v1.0.0)
- 9126: godbolt-compiler-explorer (v3.2.3)
- 9127: paper_search_server
- 9128: mcp-time (v0.0.3)

## RAM footprint

8 mcp.exe daemon processes total ~71 MB (aggregate). Each inner stdio subprocess (node, python, uvx child) is ~40-70 MB depending on runtime. Before Phase 2: ~60-80 stdio subprocesses across 4 clients totaling ~2 GB. After Phase 2: 7 inner subprocesses + 8 daemons = ~600 MB when measured including inner MCP servers. Net savings: ~1.4 GB from consolidation alone, plus elimination of data-race risk in memory's JSONL store.

## Known follow-ups (not blocking)

- `host.go` does not tee subprocess stderr to `LogPath` (the old supergateway-based path did); stderr goes only to `os.Stderr`, which is captured by the scheduler task's invisible console. Low priority — diagnostics still work via `taskschd.msc` or scheduler Last Run output.
- `initCached` has no invalidation hook if the subprocess crashes and respawns. Phase 3 should address as part of lifecycle supervision.
- `secrets.age` gitignore policy ambiguous: secrets.age is encrypted and theoretically safe to commit, but was not staged during Task 8. Decision deferred.

## Status

**Phase 0 + Phase 1 + post-Phase 1 relay + Phase 2 CLOSED.** 11 commits on master since Phase 1 verification. All 4 clients consuming all 8 daemons (claude-code, codex-cli, gemini-cli via HTTP; antigravity via stdio relay for serena, HTTP for the rest).
