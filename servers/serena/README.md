# Serena (MCP server)

Serena clients use the live-probed hub router endpoint
`http://127.0.0.1:<router-port>/serena/mcp`. A workspace proxy port is internal
to supervision and must never be configured as a client URL.

Current Serena 1.7 project files use `language_servers`. Existing `languages`
and singular `language` files remain compatible; migrate only those keys without
replacing other settings with `mcphub workspace bootstrap <path>
--migrate-serena-schema`.

One unified daemon on `--context codex`, port 9121, serving every client —
Claude Code, Codex CLI, Cursor, VS Code, Gemini CLI, Qwen CLI, and the
Antigravity relay (post-2026-05-20 review replaced the old claude/codex
split; see manifest.yaml for the rationale).

Default install writes `claude-code` and `codex-cli`. Use
`--clients ...` or `--all-clients` to opt in Cursor, VS Code, Gemini CLI, Qwen
CLI, or Antigravity.

Upstream: https://github.com/oraios/serena

Install: `mcphub install --server serena`

## Terms and Abbreviations

- `MCP`: Model Context Protocol; the protocol Serena exposes to agent clients.
- `Qwen CLI`: Qwen Code command-line client; opt-in for this manifest.
- `VS Code`: Visual Studio Code; opt-in for this manifest.
