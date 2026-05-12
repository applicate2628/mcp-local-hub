# G5 — Marketplace Draft-Manifest Import Design

**Status:** active design, 2026-05-12. v1 spec for the G5 backlog item ("Browse/import from an MCP registry as a draft-manifest flow"). Effort estimate: ~2-3 days. Implementation gated on codex r1 review approval.

## Goal

Give an operator a CLI (and later GUI) path to discover MCP servers from an external registry, inspect their metadata + README, generate a draft mcp-local-hub manifest, and apply it via the existing `inspect → validate → dry-run → backup → apply` contract. **No auto-install side effects.** The marketplace is a read-only discovery surface; mutation goes through the same `mcphub manifest create` / `mcphub install` paths an operator would use for any other manifest.

## Architecture

The marketplace is a **stateless metadata cache**. Two phases:

1. **Discovery**: an operator queries a registry (initially: the curated awesome-mcp-servers list on GitHub, since the official MCP registry isn't yet stable). The hub fetches a small JSON catalog, parses entries, caches with explicit freshness.
2. **Import**: an operator picks an entry, runs `mcphub marketplace generate <id>` which produces a draft YAML manifest. **The draft is printed to stdout** — no file written, no install run. Operator pipes to a file or pastes into GUI Add server screen.

This mirrors the G7 VS Code workspace import pattern: external config is untrusted; the operator's `mcphub manifest create` (or GUI Save & Install) is the only path that actually writes anything.

## Registry source

**MVP source:** GitHub raw URL of a curated marketplace JSON catalog. v1 ships with a SINGLE registry: `https://raw.githubusercontent.com/applicate2628/mcp-local-hub/master/marketplace/v1/catalog.json`. The catalog file is hand-maintained in this repo. v0.4.x can grow to multi-registry support; for v0.3.0, one source eliminates trust-management complexity.

Catalog JSON shape (v1):

```json
{
  "schema_version": "1",
  "generated_at": "2026-05-12T00:00:00Z",
  "entries": [
    {
      "id": "filesys",
      "name": "Filesystem MCP server",
      "summary": "Read and write files in a sandboxed directory.",
      "homepage": "https://github.com/modelcontextprotocol/server-filesystem",
      "readme_url": "https://raw.githubusercontent.com/modelcontextprotocol/server-filesystem/main/README.md",
      "transport": "stdio",
      "command": "npx",
      "args": ["-y", "@modelcontextprotocol/server-filesystem", "${workspaceFolder}"],
      "env": {},
      "categories": ["filesystem", "io"],
      "license": "MIT"
    }
  ]
}
```

Each entry maps cleanly onto a draft ServerManifest. Native-http entries (with `transport: "http"` + `url`) are LISTED but draft generation refuses them with a clear G6-deferral warning — same pattern as G7's http/sse handling, for the same reason (current manifest schema requires `command`).

## Cache strategy

`<state-dir>/marketplace-cache.json` (0600 + DACL-verified, mirroring `hub-mcp-tokens.json` discipline):

```json
{
  "schema_version": "1",
  "fetched_at": "2026-05-12T03:42:00Z",
  "etag": "\"abc123\"",
  "catalog": { ... full catalog body ... }
}
```

TTL: 24 hours. On every `marketplace search` / `marketplace show` invocation, the cache is checked first. If stale (>24h), conditional GET with `If-None-Match: <etag>` — 304 → bump `fetched_at`, keep cached body; 200 → replace cached body. If the fetch fails for any reason (network, server error, malformed JSON), the **cached stale data is used with a clear `WARN: catalog is N hours stale` message** — never silently auto-install with old metadata, but discovery surfaces stay functional offline.

Explicit invalidation: `mcphub marketplace refresh` forces an unconditional fetch.

## CLI surface

```text
mcphub marketplace search [query]
    List catalog entries whose name/summary/categories match query.
    Empty query → list all. Output: table with id, name, summary,
    transport, categories.

mcphub marketplace show <id>
    Print one entry's full metadata + fetched README (separately
    fetched from entry.readme_url with the same cache discipline).

mcphub marketplace generate <id>
    Print draft mcp-local-hub manifest YAML to stdout. Same shape
    as `mcphub import vscode-workspace` output: name+kind+transport+
    command+base_args+env+daemons[default port: 0 TODO]+client_bindings.
    NO write side effects. Operator pipes to file or pastes into GUI.

mcphub marketplace refresh
    Force re-fetch the catalog (bypass TTL + ETag).
```

Warnings go to stderr (`cmd.ErrOrStderr()` per G7's Codex P2 fix). YAML on stdout, table on stdout (different commands emit different shapes; status text via stderr).

## Out of scope (MVP)

| Feature | Reason / future home |
|---|---|
| GUI Marketplace screen | CLI-first; GUI surface ships in v0.4.x once API + tests stabilize. |
| Multi-registry support | Single curated registry simplifies trust. v0.4.x adds `marketplace add-registry <url>`. |
| Per-entry signatures / provenance verification | Out of scope for v0.3.0; relies on operator trust in the curated catalog. v0.4.x can add SHA-256 pins or sigstore verification. |
| Auto-install / one-click install | Explicitly rejected per ravitemer-mcp-hub-adoption-proposals "Do Not Copy" list. Operator runs `mcphub manifest create` + `mcphub install` as separate explicit steps. |
| Native-http (remote URL) entries | G6 Remote MCP manifests territory; G5 skips with warning. |
| Search by tool name / capability | Catalog entries don't carry capability metadata in v1. Future v0.4.x can add it. |

## Threat model

| Vector | Mitigation |
|---|---|
| Hostile catalog injects malicious commands | Operator MUST inspect generated YAML before `mcphub manifest create`. `marketplace show` displays README + metadata. No auto-install. |
| MITM on catalog fetch | HTTPS-only (registry URL must be `https://`). Cache validates schema_version + JSON shape; rejects malformed payloads. |
| Stale catalog with revoked entries | TTL + ETag + explicit refresh. Stale cache surfaces `WARN: catalog is N hours stale` so operator can refresh. |
| Catalog file replaced by attacker on the registry host | Out-of-scope for v0.3.0 — registry trust is operator responsibility. v0.4.x signature verification closes this. |
| `${workspaceFolder}` / `${env:VAR}` placeholders in catalog `args` | Identical handling to G7 — expand at `marketplace generate` time using current shell env + workspace argument; surface empty-env warnings. |
| Disk fill via abusive catalog size | 10MB hard cap on the fetched catalog body (mirrors watchdog log size); reject + log on overflow. |
| Race on cache write | Atomic write via `O_EXCL` temp + rename, mirroring `hub-mcp-tokens.json` pattern (see G4 v3 spec §"Token + endpoint state hardening"). |

## Test surface

**Unit:**

- `marketplace_catalog_test.go`: parse, validate schema_version, reject malformed entries, oversize payload rejection.
- `marketplace_cache_test.go`: fresh fetch, TTL expiry triggers conditional GET, 304 keeps body, 200 replaces, network error falls back to stale cache with WARN.
- `marketplace_generate_test.go`: stdio entry → valid draft YAML; http entry → skip with G6 warning; placeholder expansion mirrors G7.

**Integration:**

- `marketplace_e2e_test.go`: spin up a fake HTTPS server serving a fixture catalog; run `mcphub marketplace search`, `show`, `generate`, `refresh`; verify offline-fallback path with a paused server.

**Playwright E2E:** none for v0.3.0 (CLI-only surface).

**Manual smoke** (`docs/phase-3b-ii-verification.md` D2.8): run `marketplace search filesystem` → see filesys entry. Run `generate filesys > /tmp/draft.yaml`. Run `mcphub manifest create filesys < /tmp/draft.yaml`. Verify the manifest works end-to-end.

## Acceptance criteria

- `marketplace search` returns matching entries from the curated catalog.
- `marketplace show <id>` prints metadata + README (with same 24h cache + ETag).
- `marketplace generate <id>` prints valid draft YAML to stdout that `mcphub manifest create` accepts.
- Stale cache (>24h) triggers conditional GET; network failure falls back to stale data with WARN.
- Cache file 0600 + DACL-verified on Windows (handle-bound, parent dir check) — same hardening as G4 state files.
- Native-http / SSE entries skipped with G6-deferral warning.
- 10MB payload cap rejects oversized catalogs.
- `marketplace refresh` forces unconditional fetch.

## Files to create / modify

| File | Kind | Purpose |
|---|---|---|
| `marketplace/v1/catalog.json` | new | hand-maintained curated catalog (~10-20 entries to start) |
| `internal/api/marketplace_catalog.go` | new | catalog parser + validator |
| `internal/api/marketplace_cache.go` | new | TTL + ETag cache + atomic write |
| `internal/api/marketplace_generate.go` | new | catalog entry → draft YAML projection (mirrors G7 placeholder expansion + skip http with G6 warning) |
| `internal/api/marketplace_test.go` | new | unit tests |
| `internal/cli/marketplace.go` | new | `mcphub marketplace {search,show,generate,refresh}` subcommands |
| `internal/cli/marketplace_test.go` | new | CLI test covering cmd.ErrOrStderr routing + happy path |
| `internal/cli/root.go` | modify | register `marketplace` subcommand |

## Terms and Abbreviations

- `MCP`: Model Context Protocol; the protocol an MCP server speaks to a client.
- `marketplace`: a curated catalog of MCP servers available for discovery + import.
- `catalog`: the JSON document listing all entries in the marketplace.
- `entry`: one row in the catalog, identifying a single MCP server.
- `draft manifest`: a YAML document projected from a catalog entry — operator must inspect + accept before any side effects.
- `state-dir`: per-user state directory; `%LOCALAPPDATA%\mcp-local-hub\` on Windows.
- `TTL`: time-to-live; catalog cache expires after 24h.
- `ETag`: HTTP entity tag for conditional GET; lets the registry tell us "your cached copy is still current".
- `DACL`: Windows Discretionary Access Control List; per-file ACL entries that grant/deny per-SID access.
- `G6`: backlog item for Remote MCP manifests (http-transport entries) — http entries in the catalog skip with G6-deferral warning until G6 lands.
