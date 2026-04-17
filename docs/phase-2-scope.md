# Phase 2 scope

Adds 6 global MCP daemons + 1 external HTTP client binding (context7).

## Servers consolidated to shared daemons

| Server | Port | Runtime | Env needed |
|---|---:|---|---|
| memory | 9123 | node (npx) | MEMORY_FILE_PATH |
| sequential-thinking | 9124 | node (npx) | — |
| wolfram | 9125 | node | secret:wolfram_app_id → WOLFRAM_LLM_APP_ID |
| godbolt | 9126 | python (venv) | — |
| paper-search-mcp | 9127 | python (uvx) | secret:unpaywall_email → PAPER_SEARCH_MCP_UNPAYWALL_EMAIL |
| time | 9128 | node (npx) | — |

## Direct HTTP (no daemon)

| Server | URL | Reason |
|---|---|---|
| context7 | https://mcp.context7.com/mcp | already remote HTTPS; added to Claude Code only |

## New core component

`internal/daemon/host.go` — native Go stdio-host that replaces supergateway.
Mirror of `internal/daemon/relay.go`: relay = stdio→HTTP (client side),
host = HTTP→stdio (daemon side).
