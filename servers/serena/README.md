# Serena (MCP server)

Three daemons, one per client context:

| Context       | Port | Clients            |
|---------------|------|--------------------|
| claude-code   | 9121 | Claude Code        |
| codex         | 9122 | Codex CLI          |
| antigravity   | 9123 | Antigravity, Gemini CLI |

Upstream: https://github.com/oraios/serena

Install: `mcp install --server serena`
