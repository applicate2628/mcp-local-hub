# Supported MCP clients

mcp-local-hub writes MCP-server entries into each client's own config file, so
the client connects to hub-managed servers. As of 2026-06-17 it supports **41
clients**. Three are installed by default (`--all` / fresh install); the rest are
opt-in (`mcphub install --server X --client <name>`).

## Default-install (3)

`claude-code` · `codex-cli` · `cursor`

## Opt-in (38)

**Original / Wave-2:** `vscode` · `gemini-cli` · `qwen-cli` · `antigravity` ·
`zed` · `kiro` · `windsurf` · `cline` · `kilocode` · `opencode` · `hermes` ·
`openclaw`

**agent-skills vendor reconciliation (2026-06-17):** `copilot-cli` · `amazon-q` ·
`openhands` · `aider` · `bob` · `codebuddy` · `command-code` · `cortex` ·
`deepagents` · `devin` · `droid` · `firebender` · `iflow-cli` · `junie` ·
`kimi-code-cli` · `kode` · `ona` · `pi` · `qoder` · `qoder-cn` · `roo` ·
`rovodev` · `tabnine-cli` · `warp` · `continue` · `goose`

Config formats span JSON (`mcpServers` object map, some with a `type:http` /
`type:streamable-http` discriminator), TOML (`[mcp]` arrays — openhands), and YAML
(`mcpServers` — continue; an `extensions` key — goose; `mcp-server` list — aider).
Relay-stdio clients (no raw-URL entry; mcphub writes a `mcphub relay` command):
`antigravity`, `zed`, `aider`, `pi`.

## Why not every `skills`-CLI vendor

The `vercel-labs/skills` CLI lists ~71 *skill-install* targets. That set is **not**
1:1 with MCP-config clients — skills go to a `skillsDir`, MCP servers go to a
client's config file. We reconciled all 53 names mcphub didn't already cover:

- **Built (verified writable config):** the 26 reconciliation clients above.
- **N-A — no writable file config:** the tool has MCP but configures it only
  through an in-app UI / paste-JSON panel, or the config file location is
  IDE-managed and undocumented (so an adapter would have to guess a path):
  `augment`, `astrbot`, `codemaker`, `codestudio`, `inference-sh`, `mcpjam`,
  `promptscript`, `replit`, `adal`, `loaf`, `moxby`, `terramind`, `tinycloud`,
  `universal`, `codearts-agent`, `forgecode`, `jazz`, `lingma`, `trae`, `trae-cn`.
- **Deferred — verified MCP-capable + verified path, but niche tool with a
  non-standard config (build on demand):** `amp` (VS Code settings
  `amp.mcpServers`), `zencoder`/`zenflow` (VS Code settings `zencoder.mcpServers`),
  `autohand-code` (`mcp.servers` array), `crush` (`mcp` key), `mux`
  (`servers` .jsonc), `neovate`, `pochi`, `reasonix`, `dexto`, `mistral-vibe`
  (TOML `[mcp_servers]`).

A new client is added in exactly one place — a `clientDescriptor` row in
`internal/clients/clients.go` (the `clientRegistry()` table) plus an adapter file.
`SupportedClientNames`, `AllClients`, `ConfigPathForName`, `DefaultInstallClientNames`,
and (since the §9.2 FAMILY-B refactor) the scan/migrate client set all derive from
that one table, so a new client can't drift out of any of them.
