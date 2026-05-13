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

**HTTPS enforcement (codex r1 P1 closure):** the `--registry` URL MUST be `https://` (parse and reject otherwise). The HTTP client uses Go's default transport with normal TLS verification (no `InsecureSkipVerify`). HTTPS-to-HTTP downgrade redirects are rejected: a `CheckRedirect` callback verifies every redirect target is `https://`. Tests use `httptest.NewTLSServer` with the server's certificate injected into the test client's `RootCAs`. Compression is explicitly disabled (`DisableCompression: true`) so the 10MB body cap applies to wire bytes (defeats gzip-bomb amplification — codex r1 P2).

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
    Print one entry's full metadata: id, name, summary, transport,
    command/url, args, env, homepage, license, categories, AND the
    entry.readme_url string (not the README body — operator opens
    that URL themselves). codex r1 P1 closure: README body fetch
    is deferred from v0.3.0 — separate fetch + cache discipline
    + UI line-wrap is non-trivial and the URL alone is enough for
    a single-click external open.

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
| Native-http (remote URL) entries | G6 Remote MCP manifests territory; G5 skips with warning. The current manifest schema requires `command` so even hand-authored remote manifests fail until G6 lands. |
| Search by tool name / capability | Catalog entries don't carry capability metadata in v1. Future v0.4.x can add it. |
| README body fetch inside `marketplace show` | codex r1 P1 closure: print `readme_url` only; deferred to v0.4.x. |
| Automatic expansion of sensitive env placeholders | codex r1 P1 closure: catalog-controlled `${env:VAR}` values matching the sensitive-name policy below are LEFT VERBATIM in the draft (operator must consciously edit the draft before `manifest create`). |

## Threat model

| Vector | Mitigation |
|---|---|
| Hostile catalog injects malicious commands | Operator MUST inspect generated YAML before `mcphub manifest create`. `marketplace show` displays README + metadata. No auto-install. |
| MITM on catalog fetch | HTTPS-only (registry URL must be `https://`). Cache validates schema_version + JSON shape; rejects malformed payloads. |
| Stale catalog with revoked entries | TTL + ETag + explicit refresh. Stale cache surfaces `WARN: catalog is N hours stale` so operator can refresh. |
| Catalog file replaced by attacker on the registry host | Out-of-scope for v0.3.0 — registry trust is operator responsibility. v0.4.x signature verification closes this. |
| `${workspaceFolder}` / `${env:VAR}` placeholders in catalog `args` | Reuse G7's existing `vscodeExpander` logic (export as `api.PlaceholderExpander`) — keep ONE expander across G5+G7 so semantics cannot diverge. Surface empty-env warnings to stderr. |
| Catalog injects `${env:VAR}` for a sensitive env name | codex r1 P1 closure: define a sensitive-name policy — names matching `*_TOKEN`, `*_SECRET`, `*_PASSWORD`, `*_KEY`, `*_API_KEY`, `AWS_*`, `AZURE_*`, `GCP_*`, `GITHUB_*` are LEFT VERBATIM in the draft (not expanded). Generator returns a warning surfaced to stderr: `WARN: catalog references ${env:AWS_SECRET_ACCESS_KEY} — left verbatim in draft so the value is never written to the YAML you'll commit; edit before saving`. |
| Disk fill via abusive catalog size | 10MB hard cap on the fetched catalog body (mirrors watchdog log size); reject + log on overflow. `DisableCompression: true` so the cap applies to wire bytes (defeats gzip-bomb amplification — codex r1 P2). |
| Race on cache write | Reuse G4's `writeHubMcpStateFile` helper (codex r1 P1 closure) — atomic tempfile + rename + post-rename DACL re-verify on the parent dir handle (codex r3 P2 closure: this helper does NOT acquire `hub-mcp.lock`; cross-process flock is the caller's responsibility for multi-step transactions. The cache is best-effort metadata so atomic-rename alone is enough — a torn read fails JSON parse and triggers refresh on next load). Read path goes through `VerifyHubMcpStateDACL` + `readHubMcpStateFile`. Cache file leaf joins the existing state-name allowlist via `validateStateFileName`. |
| HTTPS-to-HTTP downgrade via redirect | `CheckRedirect` callback rejects any redirect target whose scheme is not `https://`. |
| Clock rollback making stale cache look fresh | codex r1 P2 closure: on read, if `fetched_at > now` (skew or corrupted timestamp), force revalidation. `MarketplaceCacheAge` is clamped non-negative. |
| Catalog ID collision with reserved manifest name | codex r1 P2 closure: parser validates each `entry.id` via `CheckManifestName` (same gate as `mcphub manifest create <name>`), so an entry named `mcphub-hub`, an entry with whitespace, or an entry colliding with reserved Windows device names is rejected at parse time rather than failing at `manifest create` later. |

## Test surface

**Unit:**

- `marketplace_catalog_test.go`: parse, validate schema_version, reject malformed entries, oversize payload rejection.
- `marketplace_cache_test.go`: fresh fetch, TTL expiry triggers conditional GET, 304 keeps body, 200 replaces, network error falls back to stale cache with WARN.
- `marketplace_generate_test.go`: stdio entry → valid draft YAML; http entry → skip with G6 warning; placeholder expansion mirrors G7.

**Integration:**

- `marketplace_e2e_test.go`: spin up a fake HTTPS server serving a fixture catalog; run `mcphub marketplace search`, `show`, `generate`, `refresh`; verify offline-fallback path with a paused server.

**Playwright E2E:** none for v0.3.0 (CLI-only surface).

**Manual smoke** (`docs/phase-3b-ii-verification.md` D2.8): run `marketplace search filesystem` → see `filesystem` entry. Run `marketplace generate filesystem > /tmp/draft.yaml`. **Operator edits `/tmp/draft.yaml`:** change `name: filesystem` to `name: filesystem-test` (so it doesn't collide with the canonical name once we add the registry), pick a real port for `daemons[0].port` (e.g. `9200`), inspect `command` and `base_args` to confirm no surprises, redact any sensitive env values the generator left verbatim. Then `mcphub manifest create filesystem-test < /tmp/draft.yaml` and `mcphub install --server filesystem-test`. Verify the daemon registers + the configured client surface picks it up.

**Why the operator-edit step is load-bearing (codex r1 P1):** the catalog is external + untrusted. `marketplace generate` is a projection, not a save: it emits a `name:` matching the entry id and `port: 0` precisely so `manifest create` refuses it without an explicit operator-side rename + port pick. This forces inspection-before-save, in line with the spec's "no auto-install side effects" stance.

## Acceptance criteria

- `marketplace search` returns matching entries from the curated catalog.
- `marketplace show <id>` prints metadata (id, name, summary, transport, command/url, args, env, homepage, license, categories, readme_url). README body is intentionally NOT fetched in v0.3.0 (deferred per Out-of-scope).
- `marketplace generate <id>` prints valid draft YAML to stdout. Stdio entries map to `transport: stdio-bridge` (codex r1 P1 closure — NOT `native-http`); http entries refuse with G6-deferral warning. The draft requires operator edit (`name:` rename + port pick + sensitive-env redaction) before `manifest create` accepts it.
- Stale cache (>24h) triggers conditional GET; network failure falls back to stale data with WARN.
- Cache file routed through `writeHubMcpStateFile` (codex r1 P1 closure): 0600 + DACL-verified parent + atomic tempfile + rename + post-rename DACL re-verify (best-effort cache, no cross-process flock — codex r3 P2 closure). Read path goes through `VerifyHubMcpStateDACL` + `readHubMcpStateFile`.
- HTTPS-only enforced: `--registry` URLs not matching `https://` are rejected; redirects to non-HTTPS targets are rejected via `CheckRedirect`.
- Compression disabled on the HTTP client: 10MB cap applies to wire bytes (no gzip-bomb amplification).
- Future `fetched_at` (clock rollback / corrupted cache) forces revalidation; `MarketplaceCacheAge` is non-negative.
- Catalog parser validates each `entry.id` via `CheckManifestName`; rejects entries that wouldn't pass `manifest create`'s gate.
- Sensitive-env placeholders (`*_TOKEN`, `*_SECRET`, `*_PASSWORD`, `*_KEY`, `*_API_KEY`, `AWS_*`, `AZURE_*`, `GCP_*`, `GITHUB_*`) are left verbatim in the draft; generator emits a stderr WARN line per such placeholder.
- Native-http (`transport: "http"`) entries are accepted at parse time but refused by `generate` with a G6-deferral warning that names today's workaround (none — wait for G6 or write a local stdio wrapper). SSE entries are out of scope for v1 — the parser rejects any transport other than `stdio` or `http`, so an SSE entry never reaches the generator.
- 10MB payload cap rejects oversized catalogs.
- `marketplace refresh` forces unconditional fetch.

## Files to create / modify

| File | Kind | Purpose |
|---|---|---|
| `marketplace/v1/catalog.json` | new | hand-maintained curated catalog (~10 entries to start) |
| `internal/api/marketplace_catalog.go` | new | catalog parser + validator (per-entry `CheckManifestName(id)` + transport allowlist) |
| `internal/api/marketplace_cache.go` | new | TTL + ETag cache routed through `writeHubMcpStateFile` / `readHubMcpStateFile` (G4 hardening reuse) |
| `internal/api/marketplace_http.go` | new | HTTPS-only client (parse + reject non-`https://`, `CheckRedirect` downgrade guard, `DisableCompression: true`) + injectable test client |
| `internal/api/marketplace_generate.go` | new | catalog entry → draft YAML projection. Uses `config.TransportStdioBridge`; reuses G7's expander via newly-exported `api.PlaceholderExpander` so sensitive-env policy lives in one place |
| `internal/api/import_vscode.go` | modify | promote private `vscodeExpander` to exported `PlaceholderExpander` (or extract to `internal/api/placeholder_expand.go`) so both G5 and G7 share one expander + sensitive-name allowlist |
| `internal/api/marketplace_test.go` | new | unit tests covering schema, HTTPS enforcement, gzip-bomb rejection, sensitive-env redaction, clock-skew clamp |
| `internal/cli/marketplace.go` | new | `mcphub marketplace {search,show,generate,refresh}` subcommands |
| `internal/cli/marketplace_test.go` | new | CLI test covering cmd.ErrOrStderr routing + happy path + httptest.NewTLSServer + test-client injection |
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
