# Supported MCP clients

mcp-local-hub writes MCP-server entries into each client's own config file, so
the client connects to hub-managed servers. It currently supports **47
clients**. Two are installed by default (`--all` / fresh install); the rest are
opt-in (`mcphub install --server X --clients <name>`).

## Default-install (2)

`claude-code` · `codex-cli`

## Opt-in (45)

**Original / Wave-2:** `cursor` · `vscode` · `gemini-cli` · `qwen-cli` · `antigravity` ·
`zed` · `kiro` · `windsurf` · `cline` · `kilocode` · `opencode` · `mimocode` ·
`hermes` · `openclaw`

**agent-skills vendor reconciliation (2026-06-17):** `copilot-cli` · `amazon-q` ·
`openhands` · `aider` · `bob` · `codebuddy` · `command-code` · `cortex` ·
`deepagents` · `devin` · `droid` · `firebender` · `iflow-cli` · `junie` ·
`kimi-code-cli` · `kode` · `ona` · `pi` · `qoder` · `qoder-cn` · `roo` ·
`rovodev` · `tabnine-cli` · `warp` · `continue` · `goose`

**Bespoke-key vendors (2026-06-17):** `neovate` · `crush` · `pochi` · `amp` ·
`zencoder` — config under a NON-standard top-level key, built on the
parameterized `jsonMCPClient` (`serversKey`): `neovate` uses the standard
`mcpServers` (in `~/.neovate/config.json`); `crush` + `pochi` use a top-level
`mcp` key; `amp` + `zencoder` use VS Code User `settings.json` flat dotted keys
(`amp.mcpServers` / `zencoder.mcpServers`).

Config formats span JSON (`mcpServers` object map, some with a `type:http` /
`type:streamable-http` discriminator; a `mcp` object map — crush/pochi; VS Code
settings.json flat dotted keys — amp/zencoder), TOML (`[mcp]` arrays —
openhands), and YAML (`mcpServers` — continue; an `extensions` key — goose;
`mcp-server` list — aider). Relay-stdio clients (no raw-URL entry; mcphub writes
a `mcphub relay` command): `antigravity`, `zed`, `aider`, `pi`, `pochi`,
`zencoder`.

## Why not every `skills`-CLI vendor

The `vercel-labs/skills` CLI lists ~71 *skill-install* targets. That set is **not**
1:1 with MCP-config clients — skills go to a `skillsDir`, MCP servers go to a
client's config file. We reconciled all 53 names mcphub didn't already cover:

- **Built (verified writable config):** the 31 reconciliation clients above —
  26 standard-schema plus the 5 bespoke-key vendors (`neovate`, `crush`,
  `pochi`, `amp`, `zencoder`). `zenflow` is NOT a separate adapter: it shares
  Zencoder's exact config surface (`zencoder.mcpServers` in VS Code
  settings.json), so the `zencoder` adapter covers it.
- **N-A — no writable file config:** the tool has MCP but configures it only
  through an in-app UI / paste-JSON panel, or the config file location is
  IDE-managed and undocumented (so an adapter would have to guess a path):
  `augment`, `astrbot`, `codemaker`, `codestudio`, `inference-sh`, `mcpjam`,
  `promptscript`, `replit`, `adal`, `loaf`, `moxby`, `terramind`, `tinycloud`,
  `universal`, `codearts-agent`, `forgecode`, `jazz`, `lingma`, `trae`, `trae-cn`.
- **N-A — path/key not authoritatively pinnable (never-guess):** `dexto`
  (the MCP block lives in a user-CHOSEN agent `.yml`; there is no fixed
  home-relative config path to target), `reasonix` (project `.reasonix/config.json`
  exists but the top-level JSON key for the MCP block is not confirmed verbatim
  from source — writing under a guessed key would silently no-op).
- **Deferred — verified MCP-capable + verified path, but a non-object-map
  config shape that needs a bespoke parser (build on demand when a user of the
  tool requests it):** `autohand-code` (`~/.autohand/config.json`, a `mcp.servers`
  ARRAY of objects — mirror the openhands array adapter), `mux`
  (`~/.mux/mcp.jsonc`, a `servers` map whose VALUES are bare command STRINGS,
  not objects — diverges from every object-map helper), `mistral-vibe`
  (`~/.vibe/config.toml`, a TOML `[[mcp_servers]]` array of tables — mirror the
  openhands TOML adapter).

A new client is added in exactly one place — a `clientDescriptor` row in
`internal/clients/clients.go` (the `clientRegistry()` table) plus an adapter file.
`SupportedClientNames`, `AllClients`, `ConfigPathForName`, `DefaultInstallClientNames`,
and (since the §9.2 FAMILY-B refactor) the scan/migrate client set all derive from
that one table, so a new client can't drift out of any of them.
