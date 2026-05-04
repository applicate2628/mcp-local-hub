# Serena (MCP server)

Two daemons, split by Serena context:

| Context | Port | Clients |
|---|---:|---|
| claude | 9121 | Claude Code, Cursor, VS Code, Gemini CLI, Qwen CLI, Antigravity relay |
| codex | 9122 | Codex CLI |

Default install writes `claude-code`, `codex-cli`, and `cursor`. Use
`--clients ...` or `--all-clients` to opt in VS Code, Gemini CLI, Qwen CLI, or
Antigravity.

Upstream: https://github.com/oraios/serena

Install: `mcphub install --server serena`

## Terms and Abbreviations

- `MCP`: Model Context Protocol; the protocol Serena exposes to agent clients.
- `Qwen CLI`: Qwen Code command-line client; opt-in for this manifest.
- `VS Code`: Visual Studio Code; opt-in for this manifest.
