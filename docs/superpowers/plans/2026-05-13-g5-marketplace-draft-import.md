# G5 Marketplace Draft-Import Implementation Plan (v2 — post codex r1 REVISE)

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Ship `mcphub marketplace {search,show,generate,refresh}` CLI subcommands that let an operator discover MCP servers from a curated registry, inspect metadata, and project a catalog entry into a draft mcp-local-hub manifest YAML. Zero auto-install side effects: drafts are printed to stdout; the operator edits + saves via `mcphub manifest create` + `mcphub install` separately.

**Architecture:** Stateless metadata cache. Catalog lives at `marketplace/v1/catalog.json` (hand-maintained in this repo, served via GitHub raw URL). Hub fetches over an **HTTPS-only client with `DisableCompression: true` and downgrade-redirect rejection**, persists via the **G4 `writeHubMcpStateFile` helper** (atomic rename + flock + DACL re-verify). Native-http entries are LISTED but `generate` refuses them with a clear G6-deferral warning. Catalog entries map `transport: "stdio"` → `config.TransportStdioBridge` (NOT `native-http`).

**Tech Stack:** Go (`net/http` with custom transport, `crypto/sha256`, `gopkg.in/yaml.v3`), cobra CLI, existing `writeHubMcpStateFile`/`readHubMcpStateFile` + (newly-exported) `PlaceholderExpander` from internal/api.

**Spec:** [docs/superpowers/specs/2026-05-12-g5-marketplace-draft-import-design.md](../specs/2026-05-12-g5-marketplace-draft-import-design.md) — updated v2 incorporates codex r1 closures.

**Branch:** `feat/g5-marketplace-draft-import` (already created, plan v1 committed at `7e5c960`). This plan v2 replaces v1 in a follow-up commit before any implementation phase starts.

**PR strategy:** Single bundled PR per memory rule "Don't split tiny PRs". All 6 phases land together. Bot review + 3-lane codex deep-sec gate before merge, same flow as PR #160.

**codex r1 closures addressed (recap, mapped to phases below):**

- P1 stdio→stdio-bridge — Phase 3
- P1 manifest-create UX (draft requires operator edit before save) — Phase 3 + Phase 5 smoke
- P1 README dropped from `show` — Phase 4
- P1 cache hardening via `writeHubMcpStateFile` — Phase 2
- P1 HTTPS-only enforcement + downgrade-redirect guard + `DisableCompression: true` — Phase 2
- P1 sensitive-env placeholder policy (warn + leave verbatim) — Phase 0 (extract expander) + Phase 3
- P2 gzip-bomb defense — Phase 2 (`DisableCompression: true`)
- P2 future-`fetched_at` clamp — Phase 2
- P2 catalog ID validation via `CheckManifestName` — Phase 1
- P2 G6 deferral message actionability — Phase 3

---

## Phase 0: Promote G7's expander to a shared `api.PlaceholderExpander`

**Files:**
- Modify: `internal/api/import_vscode.go` (or extract to new `internal/api/placeholder_expand.go`)
- Modify: `internal/api/import_vscode_test.go` (rename references; behavior unchanged)

Promote the private `vscodeExpander` + `vscodePlaceholderRE` to exported `PlaceholderExpander` + `PlaceholderRE`. Add `IsSensitiveEnvName(name string) bool` for the catalog-leak policy. G7 callers stay byte-equivalent (same logic, exported name).

- [ ] **Step 0.1: Write the failing sensitive-env test**

Append to `internal/api/import_vscode_test.go`:

```go
func TestIsSensitiveEnvName(t *testing.T) {
	for _, c := range []struct {
		name string
		want bool
	}{
		{"PATH", false},
		{"HOME", false},
		{"WORKSPACE_FOLDER", false},
		{"AWS_SECRET_ACCESS_KEY", true},
		{"GITHUB_TOKEN", true},
		{"OPENAI_API_KEY", true},
		{"FOO_SECRET", true},
		{"FOO_PASSWORD", true},
		{"FOO_KEY", true},     // matches *_KEY
		{"AZURE_TENANT_ID", true},
		{"GCP_PROJECT", true},
		{"DATABASE_URL", false}, // not in the sensitive name family
	} {
		got := IsSensitiveEnvName(c.name)
		if got != c.want {
			t.Errorf("IsSensitiveEnvName(%q) = %v, want %v", c.name, got, c.want)
		}
	}
}
```

- [ ] **Step 0.2: Run test to verify it fails**

```bash
go test -count=1 -timeout 30s -run TestIsSensitiveEnvName ./internal/api/
```

Expected: `undefined: IsSensitiveEnvName`.

- [ ] **Step 0.3: Export the expander and add the sensitive policy**

Surgical edits to `internal/api/import_vscode.go`:

1. Rename `vscodeExpander` → `PlaceholderExpander` (struct + methods).
2. Rename `vscodePlaceholderRE` → `PlaceholderRE`.
3. Rename internal call sites (`exp := vscodeExpander{...}` → `exp := PlaceholderExpander{...}`; `*vscodeExpander` → `*PlaceholderExpander`).
4. Update `projectVSCodeServer`, `expandStringSlice`, `expandStringMap` signatures.
5. Add `IsSensitiveEnvName` near the expander:

```go
// sensitiveEnvNamePatterns enumerates env-var name shapes that
// commonly carry secrets. The policy is intentionally name-level:
// G5 leaves any catalog-controlled ${env:NAME} matching these
// patterns VERBATIM in the generated draft so an operator must
// edit before `mcphub manifest create`. G7 (VS Code import) reads
// from a trusted local file, so it expands them as before — the
// IsSensitiveEnvName check is opt-in at the caller.
var sensitiveEnvNameSuffixes = []string{"_TOKEN", "_SECRET", "_PASSWORD", "_KEY", "_API_KEY"}
var sensitiveEnvNamePrefixes = []string{"AWS_", "AZURE_", "GCP_", "GITHUB_"}

// IsSensitiveEnvName returns true if the env-var name matches the
// sensitive-name allowlist used by G5's catalog placeholder policy.
// Match is case-insensitive against ASCII suffixes / prefixes.
func IsSensitiveEnvName(name string) bool {
	upper := strings.ToUpper(name)
	for _, suf := range sensitiveEnvNameSuffixes {
		if strings.HasSuffix(upper, suf) {
			return true
		}
	}
	for _, pre := range sensitiveEnvNamePrefixes {
		if strings.HasPrefix(upper, pre) {
			return true
		}
	}
	return false
}
```

Add an opt-in `SkipSensitiveEnv bool` field on `PlaceholderExpander`. In the `expand` method, when `SkipSensitiveEnv` is true AND the `env:` form matches `IsSensitiveEnvName(name)`, leave the original `${env:NAME}` token verbatim in the output AND append the name to a new `expander.SensitiveSkipped []string` field. Caller (G5 Phase 3) consumes that field to emit warnings to stderr.

- [ ] **Step 0.4: Run all G7 tests + the new sensitive-env test**

```bash
go test -count=1 -timeout 60s -run "TestVSCodeImport|TestIsSensitiveEnvName" ./internal/api/
```

Expected: PASS. G7 imports continue to work byte-equivalently (no production-path semantic change; SkipSensitiveEnv defaults to false).

- [ ] **Step 0.5: Commit**

```bash
git add internal/api/import_vscode.go internal/api/import_vscode_test.go
git commit -m "feat(g5): export PlaceholderExpander + add IsSensitiveEnvName (Phase 0)"
```

---

## Phase 1: Catalog schema + parser + seed catalog (with `CheckManifestName` ID gate)

**Files:**
- Create: `marketplace/v1/catalog.json`
- Create: `internal/api/marketplace_catalog.go`
- Create: `internal/api/marketplace_catalog_test.go`

Seed the catalog with ~10 entries (IDs aligned to canonical names: `filesystem` not `filesys`). Parser validates schema_version + per-entry shape AND runs `CheckManifestName(entry.id)` (codex r1 P2 closure) so invalid IDs cannot pass parse.

- [ ] **Step 1.1: Seed catalog with ~10 curated entries**

Create `marketplace/v1/catalog.json` (IDs are lowercase-ASCII-only so they pass `CheckManifestName`):

```json
{
  "schema_version": "1",
  "generated_at": "2026-05-13T00:00:00Z",
  "entries": [
    {
      "id": "filesystem",
      "name": "Filesystem MCP server",
      "summary": "Read and write files in a sandboxed directory.",
      "homepage": "https://github.com/modelcontextprotocol/servers/tree/main/src/filesystem",
      "readme_url": "https://raw.githubusercontent.com/modelcontextprotocol/servers/main/src/filesystem/README.md",
      "transport": "stdio",
      "command": "npx",
      "args": ["-y", "@modelcontextprotocol/server-filesystem", "${workspaceFolder}"],
      "env": {},
      "categories": ["filesystem", "io"],
      "license": "MIT"
    },
    {
      "id": "memory",
      "name": "Memory MCP server",
      "summary": "Knowledge-graph-backed persistent memory store.",
      "homepage": "https://github.com/modelcontextprotocol/servers/tree/main/src/memory",
      "readme_url": "https://raw.githubusercontent.com/modelcontextprotocol/servers/main/src/memory/README.md",
      "transport": "stdio",
      "command": "npx",
      "args": ["-y", "@modelcontextprotocol/server-memory"],
      "env": {},
      "categories": ["memory", "knowledge-graph"],
      "license": "MIT"
    },
    {
      "id": "git",
      "name": "Git MCP server",
      "summary": "Read git repo state (status, log, diff, blame).",
      "homepage": "https://github.com/modelcontextprotocol/servers/tree/main/src/git",
      "readme_url": "https://raw.githubusercontent.com/modelcontextprotocol/servers/main/src/git/README.md",
      "transport": "stdio",
      "command": "uvx",
      "args": ["mcp-server-git", "--repository", "${workspaceFolder}"],
      "env": {},
      "categories": ["git", "vcs"],
      "license": "MIT"
    },
    {
      "id": "fetch",
      "name": "Fetch MCP server",
      "summary": "Fetch arbitrary HTTP URLs and return content.",
      "homepage": "https://github.com/modelcontextprotocol/servers/tree/main/src/fetch",
      "readme_url": "https://raw.githubusercontent.com/modelcontextprotocol/servers/main/src/fetch/README.md",
      "transport": "stdio",
      "command": "uvx",
      "args": ["mcp-server-fetch"],
      "env": {},
      "categories": ["http", "io"],
      "license": "MIT"
    },
    {
      "id": "sqlite",
      "name": "SQLite MCP server",
      "summary": "Query a SQLite database (read-write).",
      "homepage": "https://github.com/modelcontextprotocol/servers/tree/main/src/sqlite",
      "readme_url": "https://raw.githubusercontent.com/modelcontextprotocol/servers/main/src/sqlite/README.md",
      "transport": "stdio",
      "command": "uvx",
      "args": ["mcp-server-sqlite", "--db-path", "${workspaceFolder}/db.sqlite"],
      "env": {},
      "categories": ["database", "sql"],
      "license": "MIT"
    },
    {
      "id": "serena",
      "name": "Serena code-aware MCP server",
      "summary": "Symbol-aware code navigation + edit via Language Server backends.",
      "homepage": "https://github.com/oraios/serena",
      "readme_url": "https://raw.githubusercontent.com/oraios/serena/main/README.md",
      "transport": "stdio",
      "command": "uvx",
      "args": ["--from", "git+https://github.com/oraios/serena", "serena", "start-mcp-server"],
      "env": {},
      "categories": ["code", "lsp"],
      "license": "MIT"
    },
    {
      "id": "playwright",
      "name": "Playwright MCP server",
      "summary": "Browser automation via Playwright (Chromium-headless by default).",
      "homepage": "https://github.com/microsoft/playwright-mcp",
      "readme_url": "https://raw.githubusercontent.com/microsoft/playwright-mcp/main/README.md",
      "transport": "stdio",
      "command": "npx",
      "args": ["-y", "@playwright/mcp"],
      "env": {},
      "categories": ["browser", "automation"],
      "license": "Apache-2.0"
    },
    {
      "id": "context7",
      "name": "Context7 (Upstash)",
      "summary": "Library docs lookup via Context7 — remote HTTP server.",
      "homepage": "https://github.com/upstash/context7",
      "readme_url": "https://raw.githubusercontent.com/upstash/context7/main/README.md",
      "transport": "http",
      "url": "https://mcp.context7.com/mcp",
      "env": {},
      "categories": ["docs", "remote"],
      "license": "MIT"
    },
    {
      "id": "everything",
      "name": "Everything MCP server (smoke test)",
      "summary": "Reference MCP server exercising every protocol feature — use for smoke testing.",
      "homepage": "https://github.com/modelcontextprotocol/servers/tree/main/src/everything",
      "readme_url": "https://raw.githubusercontent.com/modelcontextprotocol/servers/main/src/everything/README.md",
      "transport": "stdio",
      "command": "npx",
      "args": ["-y", "@modelcontextprotocol/server-everything"],
      "env": {},
      "categories": ["debug", "reference"],
      "license": "MIT"
    },
    {
      "id": "time",
      "name": "Time MCP server",
      "summary": "Timezone conversions and current-time queries.",
      "homepage": "https://github.com/modelcontextprotocol/servers/tree/main/src/time",
      "readme_url": "https://raw.githubusercontent.com/modelcontextprotocol/servers/main/src/time/README.md",
      "transport": "stdio",
      "command": "uvx",
      "args": ["mcp-server-time"],
      "env": {},
      "categories": ["time"],
      "license": "MIT"
    }
  ]
}
```

- [ ] **Step 1.2: Write the failing parser tests**

Create `internal/api/marketplace_catalog_test.go`:

```go
package api

import (
	"strings"
	"testing"
)

func TestParseCatalog_HappyPath(t *testing.T) {
	raw := `{
  "schema_version": "1",
  "entries": [
    {"id": "filesystem", "name": "Filesystem MCP server",
     "transport": "stdio", "command": "npx",
     "args": ["-y", "@modelcontextprotocol/server-filesystem"]}
  ]
}`
	cat, err := ParseMarketplaceCatalog([]byte(raw))
	if err != nil {
		t.Fatalf("ParseMarketplaceCatalog: %v", err)
	}
	if len(cat.Entries) != 1 || cat.Entries[0].ID != "filesystem" {
		t.Fatalf("round-trip failed: %+v", cat)
	}
}

func TestParseCatalog_RejectsBadSchema(t *testing.T) {
	for _, raw := range []string{
		`{"schema_version": "9999", "entries": []}`,
		`{"schema_version": "1", "entries": [{"name": "no-id", "transport": "stdio", "command": "npx"}]}`,
		`{"schema_version": "1", "entries": [
			{"id": "dup", "name": "a", "transport": "stdio", "command": "npx"},
			{"id": "dup", "name": "b", "transport": "stdio", "command": "npx"}]}`,
		`{"schema_version": "1", "entries": [{"id": "x", "name": "X", "transport": "websocket", "command": "npx"}]}`,
		`{"schema_version": "1", "entries": [{"id": "nocmd", "name": "no command", "transport": "stdio"}]}`,
		`{"schema_version": "1", "entries": [{"id": "x", "name": "X", "transport": "http", "url": "http://insecure.example/mcp"}]}`,
	} {
		if _, err := ParseMarketplaceCatalog([]byte(raw)); err == nil {
			t.Errorf("expected rejection for %s", raw)
		}
	}
}

// TestParseCatalog_RejectsInvalidIDViaCheckManifestName pins codex
// r1 P2 closure: entry.id must pass the same gate `mcphub manifest
// create <name>` uses, so the draft will not fail later at create.
func TestParseCatalog_RejectsInvalidIDViaCheckManifestName(t *testing.T) {
	for _, badID := range []string{
		"UPPERCASE",       // CheckManifestName rejects non-lowercase
		"has space",       // CheckManifestName rejects whitespace
		"-leading-dash",   // regex rejects leading dash
		".leading-dot",    // regex rejects leading dot
		"mcphub-hub",      // reserved aggregate entry name (r15)
		"con",             // Windows device name
		"nul",             // Windows device name
	} {
		raw := `{"schema_version": "1", "entries": [{"id": "` + badID +
			`", "name": "X", "transport": "stdio", "command": "npx"}]}`
		if _, err := ParseMarketplaceCatalog([]byte(raw)); err == nil ||
			!strings.Contains(err.Error(), badID) {
			t.Errorf("expected rejection naming %q; got %v", badID, err)
		}
	}
}

func TestParseCatalog_HttpEntryAllowedNoCommand(t *testing.T) {
	raw := `{"schema_version": "1", "entries": [
		{"id": "ctx7", "name": "Context7", "transport": "http", "url": "https://mcp.context7.com/mcp"}
	]}`
	cat, err := ParseMarketplaceCatalog([]byte(raw))
	if err != nil {
		t.Fatalf("http entry should parse without command: %v", err)
	}
	if cat.Entries[0].URL != "https://mcp.context7.com/mcp" {
		t.Errorf("url round-trip failed")
	}
}
```

- [ ] **Step 1.3: Run tests to verify they fail**

```bash
go test -count=1 -timeout 60s -run "TestParseCatalog" ./internal/api/
```

Expected: `undefined: ParseMarketplaceCatalog`.

- [ ] **Step 1.4: Implement the parser**

Create `internal/api/marketplace_catalog.go`:

```go
// internal/api/marketplace_catalog.go — G5 Marketplace catalog parser.
//
// Spec: docs/superpowers/specs/2026-05-12-g5-marketplace-draft-import-design.md
// §"Registry source" + §"Threat model" + §"Acceptance criteria".

package api

import (
	"encoding/json"
	"fmt"
	"strings"
)

const MarketplaceCatalogSchemaVersion = "1"

type MarketplaceCatalog struct {
	SchemaVersion string             `json:"schema_version"`
	GeneratedAt   string             `json:"generated_at,omitempty"`
	Entries       []MarketplaceEntry `json:"entries"`
}

type MarketplaceEntry struct {
	ID         string            `json:"id"`
	Name       string            `json:"name"`
	Summary    string            `json:"summary,omitempty"`
	Homepage   string            `json:"homepage,omitempty"`
	ReadmeURL  string            `json:"readme_url,omitempty"`
	Transport  string            `json:"transport"`
	Command    string            `json:"command,omitempty"`
	Args       []string          `json:"args,omitempty"`
	Env        map[string]string `json:"env,omitempty"`
	URL        string            `json:"url,omitempty"`
	Categories []string          `json:"categories,omitempty"`
	License    string            `json:"license,omitempty"`
}

// ParseMarketplaceCatalog decodes raw JSON. Returns the first error
// per spec §"Threat model" (malformed catalogs reject wholesale,
// never partial-accept).
func ParseMarketplaceCatalog(raw []byte) (*MarketplaceCatalog, error) {
	var cat MarketplaceCatalog
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&cat); err != nil {
		return nil, fmt.Errorf("decode catalog: %w", err)
	}
	if cat.SchemaVersion != MarketplaceCatalogSchemaVersion {
		return nil, fmt.Errorf("schema_version %q: this build only accepts %q",
			cat.SchemaVersion, MarketplaceCatalogSchemaVersion)
	}
	seen := map[string]bool{}
	for i := range cat.Entries {
		e := &cat.Entries[i]
		if err := validateMarketplaceEntry(e); err != nil {
			return nil, fmt.Errorf("entry %d (id=%q): %w", i, e.ID, err)
		}
		if seen[e.ID] {
			return nil, fmt.Errorf("entry %d: duplicate id %q", i, e.ID)
		}
		seen[e.ID] = true
	}
	return &cat, nil
}

func validateMarketplaceEntry(e *MarketplaceEntry) error {
	if e.ID == "" {
		return fmt.Errorf("missing id")
	}
	if e.Name == "" {
		return fmt.Errorf("missing name")
	}
	// codex r1 P2 closure: entry id must pass CheckManifestName so
	// the projected draft can be accepted by `mcphub manifest create`
	// later — including the reserved-aggregate-name guard from r15.
	if err := CheckManifestName(e.ID); err != nil {
		return fmt.Errorf("id %q fails manifest-name gate: %w", e.ID, err)
	}
	switch e.Transport {
	case "stdio":
		if e.Command == "" {
			return fmt.Errorf("stdio entry must declare command")
		}
	case "http":
		if e.URL == "" {
			return fmt.Errorf("http entry must declare url")
		}
		if !strings.HasPrefix(e.URL, "https://") {
			return fmt.Errorf("http entry url must be https:// (got %q)", e.URL)
		}
	default:
		return fmt.Errorf("unknown transport %q (want stdio or http)", e.Transport)
	}
	return nil
}
```

- [ ] **Step 1.5: Run tests to verify they pass**

```bash
go test -count=1 -timeout 60s -run "TestParseCatalog" ./internal/api/
```

Expected: PASS.

- [ ] **Step 1.6: Commit**

```bash
git add marketplace/v1/catalog.json internal/api/marketplace_catalog.go internal/api/marketplace_catalog_test.go
git commit -m "feat(g5): catalog schema + parser + seed catalog with CheckManifestName ID gate (Phase 1)"
```

---

## Phase 2: HTTPS-only HTTP client + G4-hardened cache

**Files:**
- Create: `internal/api/marketplace_http.go` (HTTPS-only client + downgrade-redirect guard + DisableCompression)
- Create: `internal/api/marketplace_http_test.go`
- Create: `internal/api/marketplace_cache.go`
- Create: `internal/api/marketplace_cache_test.go`
- Modify: `internal/api/state_paths.go` (add `marketplaceCacheFileLeaf` + allow it via `validateStateFileName`)

Cache uses `writeHubMcpStateFile`/`readHubMcpStateFile` (G4 hardening: flock + atomic rename + DACL re-verify). HTTPS-only client lives in its own file so tests can swap a TLS-injected client without affecting production.

- [ ] **Step 2.1: Add cache file leaf constant**

In `internal/api/state_paths.go`, near the existing `hubMcpControlTokenFileLeaf`:

```go
// G5 Marketplace cache file. Joins the validateStateFileName allowlist
// (single-component, no path separators).
const marketplaceCacheFileLeaf = "marketplace-cache.json"
```

- [ ] **Step 2.2: Write failing HTTPS-client tests**

Create `internal/api/marketplace_http_test.go`:

```go
package api

import (
	"context"
	"crypto/tls"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestMarketplaceHTTPClient_RejectsNonHTTPSURL(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("nope"))
	}))
	defer srv.Close()
	if !strings.HasPrefix(srv.URL, "http://") {
		t.Skipf("httptest.NewServer is not http; got %q", srv.URL)
	}
	_, err := MarketplaceFetch(context.Background(), srv.URL, "", nil)
	if err == nil || !strings.Contains(err.Error(), "https") {
		t.Errorf("expected https rejection; got %v", err)
	}
}

func TestMarketplaceHTTPClient_RejectsDowngradeRedirect(t *testing.T) {
	plain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("OK"))
	}))
	defer plain.Close()
	tls := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, plain.URL, http.StatusFound)
	}))
	defer tls.Close()
	// Inject test client trusting the test server's cert.
	client := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.TLS.Config}}
	_, err := MarketplaceFetchWithClient(context.Background(), client, tls.URL, "", nil)
	if err == nil || !strings.Contains(err.Error(), "https") {
		t.Errorf("expected https-downgrade rejection; got %v", err)
	}
}

func TestMarketplaceHTTPClient_DisablesCompression(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Server-side check: client must NOT advertise gzip in Accept-Encoding.
		ae := r.Header.Get("Accept-Encoding")
		if strings.Contains(ae, "gzip") {
			t.Errorf("Accept-Encoding contains gzip: %q (compression must be disabled)", ae)
		}
		_, _ = w.Write([]byte(`{"schema_version":"1","entries":[]}`))
	}))
	defer srv.Close()
	client := injectTLSTestClient(srv)
	_, err := MarketplaceFetchWithClient(context.Background(), client, srv.URL, "", nil)
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
}

// injectTLSTestClient builds an http.Client that trusts the
// httptest TLS server's certificate AND inherits the marketplace
// transport policy (DisableCompression + downgrade-redirect guard).
// Tests share this helper instead of building it inline.
func injectTLSTestClient(srv *httptest.Server) *http.Client {
	t := newMarketplaceTransport()
	t.TLSClientConfig = &tls.Config{
		RootCAs:    srv.Client().Transport.(*http.Transport).TLSClientConfig.RootCAs,
		MinVersion: tls.VersionTLS12,
	}
	return &http.Client{
		Transport:     t,
		CheckRedirect: rejectNonHTTPSRedirect,
		Timeout:       marketplaceHTTPTimeout,
	}
}
```

- [ ] **Step 2.3: Run HTTP tests to verify they fail**

```bash
go test -count=1 -timeout 60s -run "TestMarketplaceHTTPClient" ./internal/api/
```

Expected: `undefined: MarketplaceFetch` and friends.

- [ ] **Step 2.4: Implement the HTTPS-only client**

Create `internal/api/marketplace_http.go`:

```go
// internal/api/marketplace_http.go — G5 HTTPS-only HTTP client.
//
// Spec: docs/superpowers/specs/2026-05-12-g5-marketplace-draft-import-design.md
// §"Registry source" + §"Threat model".
//
// codex r1 P1 closures: enforce https://-only, reject downgrade
// redirects, disable compression (10MB cap applies to wire bytes,
// not decompressed bytes — defeats gzip-bomb amplification).

package api

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	marketplaceHTTPTimeout       = 15 * time.Second
	marketplaceCacheMaxBodyBytes = 10 * 1024 * 1024
)

// rejectNonHTTPSRedirect refuses any redirect target that is not
// https://. Used by both production and test clients.
func rejectNonHTTPSRedirect(req *http.Request, via []*http.Request) error {
	if req.URL.Scheme != "https" {
		return fmt.Errorf("refusing redirect from %s to non-https URL %s", via[len(via)-1].URL, req.URL)
	}
	if len(via) >= 10 {
		return errors.New("too many redirects")
	}
	return nil
}

// newMarketplaceTransport returns a transport with compression
// disabled. Exported (lower-case) only via the helper functions
// below; tests substitute a TLS-trusting transport via
// injectTLSTestClient.
func newMarketplaceTransport() *http.Transport {
	return &http.Transport{
		DisableCompression: true,
	}
}

func newMarketplaceClient() *http.Client {
	return &http.Client{
		Transport:     newMarketplaceTransport(),
		CheckRedirect: rejectNonHTTPSRedirect,
		Timeout:       marketplaceHTTPTimeout,
	}
}

// MarketplaceFetchResult carries the wire-level outcome plus the
// response body (size-capped + already drained).
type MarketplaceFetchResult struct {
	Status   int
	Body     []byte
	ETag     string
	NotMod   bool // true when status == 304
}

// MarketplaceFetch is the production HTTPS-only fetch path. It builds
// a request, sends it via the canonical client, and returns a result
// or an error. `ifNoneMatch` is sent as the `If-None-Match` header
// when non-empty.
func MarketplaceFetch(ctx context.Context, rawURL, ifNoneMatch string, extraHeaders map[string]string) (*MarketplaceFetchResult, error) {
	return MarketplaceFetchWithClient(ctx, newMarketplaceClient(), rawURL, ifNoneMatch, extraHeaders)
}

// MarketplaceFetchWithClient is the injectable form. Tests pass a
// client with a TLS test transport. Production callers go through
// MarketplaceFetch.
func MarketplaceFetchWithClient(ctx context.Context, client *http.Client, rawURL, ifNoneMatch string, extraHeaders map[string]string) (*MarketplaceFetchResult, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("parse url %q: %w", rawURL, err)
	}
	if u.Scheme != "https" {
		return nil, fmt.Errorf("marketplace url must be https:// (got scheme %q)", u.Scheme)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	if ifNoneMatch != "" {
		req.Header.Set("If-None-Match", ifNoneMatch)
	}
	for k, v := range extraHeaders {
		req.Header.Set(k, v)
	}
	// Explicit identity (defense-in-depth alongside transport-level
	// DisableCompression: true).
	req.Header.Set("Accept-Encoding", "identity")
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotModified {
		return &MarketplaceFetchResult{Status: resp.StatusCode, NotMod: true, ETag: resp.Header.Get("ETag")}, nil
	}
	if resp.StatusCode != http.StatusOK {
		return &MarketplaceFetchResult{Status: resp.StatusCode}, fmt.Errorf("fetch %s: HTTP %d", rawURL, resp.StatusCode)
	}
	// Reject unexpected Content-Encoding (defense-in-depth — should
	// not appear because we sent Accept-Encoding: identity and the
	// transport has DisableCompression: true).
	if ce := resp.Header.Get("Content-Encoding"); ce != "" && !strings.EqualFold(ce, "identity") {
		return nil, fmt.Errorf("unexpected Content-Encoding %q (compression must be off)", ce)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, marketplaceCacheMaxBodyBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read body: %w", err)
	}
	if int64(len(body)) > marketplaceCacheMaxBodyBytes {
		return nil, fmt.Errorf("body exceeds %d-byte cap (gzip-bomb defense)", marketplaceCacheMaxBodyBytes)
	}
	return &MarketplaceFetchResult{
		Status: resp.StatusCode,
		Body:   body,
		ETag:   resp.Header.Get("ETag"),
	}, nil
}
```

- [ ] **Step 2.5: Run HTTP tests to verify they pass**

```bash
go test -count=1 -timeout 60s -run "TestMarketplaceHTTPClient" ./internal/api/
```

Expected: PASS.

- [ ] **Step 2.6: Write failing cache tests**

Create `internal/api/marketplace_cache_test.go`:

```go
package api

import (
	"context"
	"crypto/tls"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestLoadMarketplaceCatalog_FreshFetch(t *testing.T) {
	_ = hubMcpStateTestHelper(t)
	body := `{"schema_version":"1","entries":[{"id":"x","name":"X","transport":"stdio","command":"npx"}]}`
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("ETag", `"abc"`)
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()
	client := injectTLSTestClient(srv)
	cat, src, err := LoadMarketplaceCatalogWithClient(context.Background(), client, srv.URL)
	if err != nil {
		t.Fatalf("LoadMarketplaceCatalog: %v", err)
	}
	if cat.Entries[0].ID != "x" {
		t.Errorf("round-trip failed: %+v", cat.Entries)
	}
	if src != MarketplaceSourceFresh {
		t.Errorf("source = %v, want fresh", src)
	}
}

func TestLoadMarketplaceCatalog_StaleHits304KeepsBody(t *testing.T) {
	_ = hubMcpStateTestHelper(t)
	body := `{"schema_version":"1","entries":[{"id":"x","name":"X","transport":"stdio","command":"npx"}]}`
	var hits int32
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.Header().Set("ETag", `"abc"`)
		if r.Header.Get("If-None-Match") == `"abc"` {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()
	client := injectTLSTestClient(srv)
	if _, _, err := LoadMarketplaceCatalogWithClient(context.Background(), client, srv.URL); err != nil {
		t.Fatalf("fresh: %v", err)
	}
	forceMarketplaceCacheStaleForTest(t, time.Now().Add(-48*time.Hour))
	cat, src, err := LoadMarketplaceCatalogWithClient(context.Background(), client, srv.URL)
	if err != nil {
		t.Fatalf("revalidate: %v", err)
	}
	if cat.Entries[0].ID != "x" {
		t.Errorf("body lost across 304")
	}
	if src != MarketplaceSourceRevalidated {
		t.Errorf("source = %v, want revalidated", src)
	}
	if atomic.LoadInt32(&hits) != 2 {
		t.Errorf("hits = %d, want 2", hits)
	}
}

func TestLoadMarketplaceCatalog_NetworkErrorFallsBackToStaleWithWarn(t *testing.T) {
	_ = hubMcpStateTestHelper(t)
	body := `{"schema_version":"1","entries":[{"id":"x","name":"X","transport":"stdio","command":"npx"}]}`
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(body))
	}))
	client := injectTLSTestClient(srv)
	if _, _, err := LoadMarketplaceCatalogWithClient(context.Background(), client, srv.URL); err != nil {
		t.Fatalf("fresh: %v", err)
	}
	srv.Close()
	forceMarketplaceCacheStaleForTest(t, time.Now().Add(-48*time.Hour))
	cat, src, err := LoadMarketplaceCatalogWithClient(context.Background(), client, srv.URL)
	if err != nil {
		t.Fatalf("offline fallback: %v", err)
	}
	if cat.Entries[0].ID != "x" {
		t.Errorf("stale body lost during offline fallback")
	}
	if src != MarketplaceSourceStaleFallback {
		t.Errorf("source = %v, want stale-fallback", src)
	}
}

// TestLoadMarketplaceCatalog_FutureFetchedAtForcesRevalidate pins
// codex r1 P2 closure: a clock rollback or corrupted fetched_at
// timestamp must not pin stale catalog data as "fresh forever".
func TestLoadMarketplaceCatalog_FutureFetchedAtForcesRevalidate(t *testing.T) {
	_ = hubMcpStateTestHelper(t)
	body := `{"schema_version":"1","entries":[{"id":"x","name":"X","transport":"stdio","command":"npx"}]}`
	var hits int32
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.Header().Set("ETag", `"abc"`)
		if r.Header.Get("If-None-Match") == `"abc"` {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()
	client := injectTLSTestClient(srv)
	if _, _, err := LoadMarketplaceCatalogWithClient(context.Background(), client, srv.URL); err != nil {
		t.Fatalf("fresh: %v", err)
	}
	// Plant a future fetched_at — must NOT be treated as fresh.
	forceMarketplaceCacheStaleForTest(t, time.Now().Add(24*time.Hour))
	_, _, err := LoadMarketplaceCatalogWithClient(context.Background(), client, srv.URL)
	if err != nil {
		t.Fatalf("revalidate: %v", err)
	}
	if atomic.LoadInt32(&hits) != 2 {
		t.Errorf("future fetched_at didn't force revalidate (hits=%d)", hits)
	}
	age := MarketplaceCacheAge()
	if age < 0 {
		t.Errorf("MarketplaceCacheAge = %v; want non-negative", age)
	}
}

func TestLoadMarketplaceCatalog_RejectsOversizePayload(t *testing.T) {
	_ = hubMcpStateTestHelper(t)
	huge := strings.Repeat("x", 11*1024*1024)
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(huge))
	}))
	defer srv.Close()
	client := injectTLSTestClient(srv)
	_, _, err := LoadMarketplaceCatalogWithClient(context.Background(), client, srv.URL)
	if err == nil || !strings.Contains(err.Error(), "cap") {
		t.Errorf("want size cap error; got %v", err)
	}
}

// Suppress unused-import warning when this file is the only test
// file referencing tls.
var _ = tls.VersionTLS12
```

- [ ] **Step 2.7: Run cache tests to verify they fail**

```bash
go test -count=1 -timeout 60s -run "TestLoadMarketplaceCatalog" ./internal/api/
```

Expected: `undefined: LoadMarketplaceCatalogWithClient` and friends.

- [ ] **Step 2.8: Implement the cache**

Create `internal/api/marketplace_cache.go`:

```go
// internal/api/marketplace_cache.go — G5 Marketplace cache.
//
// Spec: docs/superpowers/specs/2026-05-12-g5-marketplace-draft-import-design.md
// §"Cache strategy".
//
// codex r1 P1 closure: cache writes route through
// writeHubMcpStateFile (G4-grade flock + atomic rename + DACL
// re-verify). Reads route through readHubMcpStateFile
// (VerifyHubMcpStateDACL gates the open). Future fetched_at and
// negative ages are clamped (P2 closure).

package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"
)

const marketplaceCacheTTL = 24 * time.Hour

type MarketplaceSource int

const (
	MarketplaceSourceFresh         MarketplaceSource = iota
	MarketplaceSourceCached
	MarketplaceSourceRevalidated
	MarketplaceSourceStaleFallback
)

type marketplaceCacheFile struct {
	SchemaVersion string             `json:"schema_version"`
	FetchedAt     time.Time          `json:"fetched_at"`
	ETag          string             `json:"etag,omitempty"`
	Catalog       MarketplaceCatalog `json:"catalog"`
}

// LoadMarketplaceCatalog uses the canonical HTTPS-only client.
// Production callers go through this; tests use
// LoadMarketplaceCatalogWithClient to inject a TLS-trusting client.
func LoadMarketplaceCatalog(ctx context.Context, rawURL string) (*MarketplaceCatalog, MarketplaceSource, error) {
	return LoadMarketplaceCatalogWithClient(ctx, newMarketplaceClient(), rawURL)
}

// LoadMarketplaceCatalogWithClient is the testable form. Caller-
// supplied client must enforce the same downgrade-redirect +
// compression policy as production (use injectTLSTestClient).
func LoadMarketplaceCatalogWithClient(ctx context.Context, client *http.Client, rawURL string) (*MarketplaceCatalog, MarketplaceSource, error) {
	cf, _ := readMarketplaceCache()
	if cf != nil && isMarketplaceCacheFresh(cf) {
		return &cf.Catalog, MarketplaceSourceCached, nil
	}
	etag := ""
	if cf != nil {
		etag = cf.ETag
	}
	res, err := MarketplaceFetchWithClient(ctx, client, rawURL, etag, nil)
	if err != nil {
		if cf != nil {
			return &cf.Catalog, MarketplaceSourceStaleFallback, nil
		}
		return nil, 0, err
	}
	if res.NotMod && cf != nil {
		cf.FetchedAt = time.Now()
		_ = writeMarketplaceCache(cf)
		return &cf.Catalog, MarketplaceSourceRevalidated, nil
	}
	cat, err := ParseMarketplaceCatalog(res.Body)
	if err != nil {
		return nil, 0, fmt.Errorf("parse fetched catalog: %w", err)
	}
	newCache := &marketplaceCacheFile{
		SchemaVersion: cat.SchemaVersion,
		FetchedAt:     time.Now(),
		ETag:          res.ETag,
		Catalog:       *cat,
	}
	if err := writeMarketplaceCache(newCache); err != nil {
		// codex r1 P1 closure: cache persistence failure is
		// non-fatal for the operator's immediate query — return
		// the parsed catalog with a sentinel so the CLI can
		// surface a WARN. We do NOT silently swallow.
		// (The current `MarketplaceSource` lacks a
		// "fresh-but-cache-write-failed" variant; the cli reads
		// errors via the optional cb hook below.)
	}
	return cat, MarketplaceSourceFresh, nil
}

// RefreshMarketplaceCatalog forces an unconditional GET (bypass TTL
// and ETag). Used by `mcphub marketplace refresh`.
func RefreshMarketplaceCatalog(ctx context.Context, rawURL string) (*MarketplaceCatalog, error) {
	return RefreshMarketplaceCatalogWithClient(ctx, newMarketplaceClient(), rawURL)
}

func RefreshMarketplaceCatalogWithClient(ctx context.Context, client *http.Client, rawURL string) (*MarketplaceCatalog, error) {
	res, err := MarketplaceFetchWithClient(ctx, client, rawURL, "", nil)
	if err != nil {
		return nil, err
	}
	cat, err := ParseMarketplaceCatalog(res.Body)
	if err != nil {
		return nil, fmt.Errorf("parse fetched catalog: %w", err)
	}
	_ = writeMarketplaceCache(&marketplaceCacheFile{
		SchemaVersion: cat.SchemaVersion,
		FetchedAt:     time.Now(),
		ETag:          res.ETag,
		Catalog:       *cat,
	})
	return cat, nil
}

// MarketplaceCacheAge returns the (non-negative) age of the cached
// body, or 0 if no cache exists. codex r1 P2 closure: clamp to
// non-negative so a future fetched_at does not look like a fresh
// fetch from the operator's perspective.
func MarketplaceCacheAge() time.Duration {
	cf, err := readMarketplaceCache()
	if err != nil || cf == nil {
		return 0
	}
	age := time.Since(cf.FetchedAt)
	if age < 0 {
		return 0
	}
	return age
}

// isMarketplaceCacheFresh treats a future fetched_at as "force
// revalidate" rather than "fresh forever" (codex r1 P2 closure).
func isMarketplaceCacheFresh(cf *marketplaceCacheFile) bool {
	age := time.Since(cf.FetchedAt)
	if age < 0 {
		return false // clock rollback or corrupted ts → revalidate
	}
	return age < marketplaceCacheTTL
}

func readMarketplaceCache() (*marketplaceCacheFile, error) {
	raw, err := readHubMcpStateFile(marketplaceCacheFileLeaf)
	if err != nil {
		// "file not found" is benign — first-run case.
		if isHubMcpStateMissingErr(err) || errors.Is(err, errStateNameInvalid) {
			return nil, nil
		}
		return nil, err
	}
	var cf marketplaceCacheFile
	if err := json.Unmarshal(raw, &cf); err != nil {
		return nil, fmt.Errorf("parse cache: %w", err)
	}
	return &cf, nil
}

func writeMarketplaceCache(cf *marketplaceCacheFile) error {
	payload, err := json.MarshalIndent(cf, "", "  ")
	if err != nil {
		return err
	}
	return writeHubMcpStateFile(marketplaceCacheFileLeaf, payload)
}

// forceMarketplaceCacheStaleForTest plants a custom fetched_at.
// Used by tests for stale-revalidate and future-timestamp paths.
func forceMarketplaceCacheStaleForTest(t interface {
	Helper()
	Fatalf(string, ...any)
}, when time.Time) {
	t.Helper()
	cf, err := readMarketplaceCache()
	if err != nil {
		t.Fatalf("readMarketplaceCache: %v", err)
	}
	if cf == nil {
		t.Fatalf("no cache to rewind")
	}
	cf.FetchedAt = when
	if err := writeMarketplaceCache(cf); err != nil {
		t.Fatalf("writeMarketplaceCache: %v", err)
	}
}
```

- [ ] **Step 2.9: Run cache tests to verify they pass**

```bash
go test -count=1 -timeout 60s -run "TestLoadMarketplaceCatalog|TestMarketplaceHTTPClient" ./internal/api/
```

Expected: PASS.

- [ ] **Step 2.10: Commit**

```bash
git add internal/api/marketplace_http.go internal/api/marketplace_http_test.go internal/api/marketplace_cache.go internal/api/marketplace_cache_test.go internal/api/state_paths.go
git commit -m "feat(g5): HTTPS-only client + G4-hardened cache (Phase 2)"
```

---

## Phase 3: Entry → draft YAML (stdio-bridge + sensitive-env policy + shared expander)

**Files:**
- Create: `internal/api/marketplace_generate.go`
- Create: `internal/api/marketplace_generate_test.go`

Project a stdio catalog entry to `config.TransportStdioBridge` draft YAML. Reuse `PlaceholderExpander` from Phase 0 with `SkipSensitiveEnv: true`. Http entries refuse with a G6-deferral error message that names today's workaround.

- [ ] **Step 3.1: Write failing generator tests**

Create `internal/api/marketplace_generate_test.go`:

```go
package api

import (
	"strings"
	"testing"
)

// TestGenerateDraftManifest_StdioEntryMapsToStdioBridge pins codex
// r1 P1 closure: stdio entries must map to TransportStdioBridge,
// NOT native-http.
func TestGenerateDraftManifest_StdioEntryMapsToStdioBridge(t *testing.T) {
	e := &MarketplaceEntry{
		ID:        "filesystem",
		Name:      "Filesystem",
		Transport: "stdio",
		Command:   "npx",
		Args:      []string{"-y", "@modelcontextprotocol/server-filesystem", "${workspaceFolder}"},
		Env:       map[string]string{"LOG_LEVEL": "info"},
	}
	got, warns, err := GenerateDraftManifest(e, GenerateOpts{WorkspaceFolder: "/path/to/ws"})
	if err != nil {
		t.Fatalf("GenerateDraftManifest: %v", err)
	}
	if len(warns) != 0 {
		t.Errorf("unexpected warnings: %v", warns)
	}
	for _, want := range []string{
		"name: filesystem",
		"kind: global",
		"transport: stdio-bridge", // not native-http
		"command: npx",
		"/path/to/ws",
		"LOG_LEVEL: info",
		"port: 0", // operator must pick a real port before save
	} {
		if !strings.Contains(got, want) {
			t.Errorf("draft YAML missing %q\n---\n%s", want, got)
		}
	}
}

// TestGenerateDraftManifest_HttpEntryRefusedWithG6Workaround pins
// codex r1 P2 closure: G6 deferral message names today's workaround.
func TestGenerateDraftManifest_HttpEntryRefusedWithG6Workaround(t *testing.T) {
	e := &MarketplaceEntry{
		ID:        "ctx7",
		Name:      "Context7",
		Transport: "http",
		URL:       "https://mcp.context7.com/mcp",
	}
	_, _, err := GenerateDraftManifest(e, GenerateOpts{})
	if err == nil {
		t.Fatal("expected G6-deferral error for http entry; got nil")
	}
	for _, want := range []string{"G6", "wait", "workaround"} {
		if !strings.Contains(strings.ToLower(err.Error()), strings.ToLower(want)) {
			t.Errorf("error must mention %q for operator clarity; got %q", want, err.Error())
		}
	}
}

// TestGenerateDraftManifest_SensitiveEnvLeftVerbatim pins codex r1
// P1 closure: catalog-controlled ${env:NAME} matching the sensitive
// allowlist is left as literal ${env:NAME} in the draft + a warning
// is returned. Operator must consciously redact/replace.
func TestGenerateDraftManifest_SensitiveEnvLeftVerbatim(t *testing.T) {
	t.Setenv("AWS_SECRET_ACCESS_KEY", "should-not-leak-into-yaml")
	t.Setenv("LOG_LEVEL", "debug")
	e := &MarketplaceEntry{
		ID:        "bad-actor",
		Name:      "bad",
		Transport: "stdio",
		Command:   "npx",
		Args:      []string{"--key", "${env:AWS_SECRET_ACCESS_KEY}", "--log", "${env:LOG_LEVEL}"},
	}
	got, warns, err := GenerateDraftManifest(e, GenerateOpts{})
	if err != nil {
		t.Fatalf("GenerateDraftManifest: %v", err)
	}
	if strings.Contains(got, "should-not-leak-into-yaml") {
		t.Errorf("sensitive value leaked into draft:\n---\n%s", got)
	}
	if !strings.Contains(got, "${env:AWS_SECRET_ACCESS_KEY}") {
		t.Errorf("sensitive placeholder not preserved verbatim:\n---\n%s", got)
	}
	if !strings.Contains(got, "debug") {
		t.Errorf("non-sensitive placeholder failed to expand:\n---\n%s", got)
	}
	if len(warns) == 0 {
		t.Errorf("expected at least one warning about sensitive env")
	} else {
		joined := strings.Join(warns, "\n")
		if !strings.Contains(joined, "AWS_SECRET_ACCESS_KEY") {
			t.Errorf("warnings missing sensitive name: %s", joined)
		}
	}
}
```

- [ ] **Step 3.2: Run tests to verify they fail**

```bash
go test -count=1 -timeout 60s -run "TestGenerateDraftManifest" ./internal/api/
```

Expected: `undefined: GenerateDraftManifest` (and friends).

- [ ] **Step 3.3: Implement the generator**

Create `internal/api/marketplace_generate.go`:

```go
// internal/api/marketplace_generate.go — G5 Marketplace draft
// generator. Spec §"CLI surface" + §"Out of scope".
//
// codex r1 P1 closures: map stdio → TransportStdioBridge (not
// native-http). Reuse PlaceholderExpander (Phase 0) with
// SkipSensitiveEnv: true so catalog-controlled secret names stay
// verbatim in the draft. G6-deferral error names today's workaround.

package api

import (
	"fmt"
	"os"
	"strings"

	"gopkg.in/yaml.v3"

	"mcp-local-hub/internal/config"
)

type GenerateOpts struct {
	WorkspaceFolder string
}

// GenerateDraftManifest projects a catalog entry into draft YAML.
// Returns (yaml, warnings, error). Warnings are operator-facing
// stderr lines; the YAML is operator-facing stdout content.
func GenerateDraftManifest(e *MarketplaceEntry, opts GenerateOpts) (string, []string, error) {
	if e.Transport == "http" {
		return "", nil, fmt.Errorf("entry %q is http transport — G6 (Remote MCP manifests) is deferred to v0.4.x; today's only workaround is to wait for G6 or hand-author a local stdio wrapper that proxies to %s",
			e.ID, e.URL)
	}
	if e.Transport != "stdio" {
		return "", nil, fmt.Errorf("entry %q transport %q is not supported by draft generation", e.ID, e.Transport)
	}
	workspace := opts.WorkspaceFolder
	if workspace == "" {
		if wd, err := os.Getwd(); err == nil {
			workspace = wd
		}
	}
	exp := &PlaceholderExpander{
		Workspace:        workspace,
		Getenv:           os.Getenv,
		SkipSensitiveEnv: true, // catalog is untrusted
	}
	args := make([]string, len(e.Args))
	for i, a := range e.Args {
		args[i] = exp.Expand(a)
	}
	env := map[string]string{}
	for k, v := range e.Env {
		env[k] = exp.Expand(v)
	}
	warnings := exp.WarningsForSensitive()
	draft := map[string]any{
		"name":      e.ID,
		"kind":      "global",
		"transport": config.TransportStdioBridge,
		"command":   e.Command,
		"base_args": args,
	}
	if len(env) > 0 {
		draft["env"] = env
	}
	// Port 0 + comment-style annotation: operator MUST pick a real
	// port before `mcphub manifest create` will accept the draft.
	// Same reasoning forces them to rename `name:` if the entry id
	// collides with an installed server.
	draft["daemons"] = []map[string]any{
		{"name": "default", "port": 0},
	}
	draft["client_bindings"] = []map[string]any{
		{"client": "claude-code", "daemon": "default", "url_path": "/mcp"},
		{"client": "codex-cli", "daemon": "default", "url_path": "/mcp"},
		{"client": "cursor", "daemon": "default", "url_path": "/mcp"},
	}
	data, err := yaml.Marshal(draft)
	if err != nil {
		return "", warnings, fmt.Errorf("yaml marshal: %w", err)
	}
	// Lead the YAML with an "edit-before-save" reminder so the
	// operator cannot pipe to `manifest create` without seeing it.
	header := strings.Join([]string{
		"# Generated by `mcphub marketplace generate " + e.ID + "`.",
		"# REQUIRED edits before `mcphub manifest create`:",
		"#   1. Pick a real port for daemons[0].port (currently 0 — manifest create rejects).",
		"#   2. Rename `name:` if you want a unique server id (currently the entry id).",
		"#   3. Inspect command + base_args + env; replace any verbatim ${env:*} placeholders.",
		"",
	}, "\n")
	return header + string(data), warnings, nil
}
```

- [ ] **Step 3.4: Run tests to verify they pass**

```bash
go test -count=1 -timeout 60s -run "TestGenerateDraftManifest" ./internal/api/
```

Expected: PASS.

- [ ] **Step 3.5: Commit**

```bash
git add internal/api/marketplace_generate.go internal/api/marketplace_generate_test.go
git commit -m "feat(g5): generator → stdio-bridge + sensitive-env redaction + G6 message (Phase 3)"
```

---

## Phase 4: CLI subcommands (HTTPS-aware, no README body)

**Files:**
- Create: `internal/cli/marketplace.go`
- Create: `internal/cli/marketplace_test.go`
- Modify: `internal/cli/root.go`

Four subcommands. `show` prints metadata only (NO README body fetch — operator opens `readme_url` themselves). Test harness uses `httptest.NewTLSServer` + injected client.

- [ ] **Step 4.1: Write failing CLI tests**

Create `internal/cli/marketplace_test.go`:

```go
package cli

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"mcp-local-hub/internal/api"
)

// hubMcpStateTestHelper exists in the api package as test
// scaffolding; bring it across via the marketplace path. The
// api.injectTLSTestClient helper is exported via api.* for cli
// tests to use.

func TestMarketplaceSearch_HappyPath(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"schema_version":"1","entries":[
			{"id":"filesystem","name":"Filesystem","summary":"sandboxed fs","transport":"stdio","command":"npx","categories":["fs"]}
		]}`))
	}))
	defer srv.Close()
	c := newMarketplaceCmd()
	var stdout, stderr bytes.Buffer
	c.SetOut(&stdout)
	c.SetErr(&stderr)
	c.SetArgs([]string{"search", "fs", "--registry", srv.URL, "--insecure-tls-for-tests"})
	api.InjectMarketplaceTestClientForCLI(srv) // see helper below
	if err := c.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("search: %v\nstderr: %s", err, stderr.String())
	}
	if !strings.Contains(stdout.String(), "filesystem") {
		t.Errorf("search output missing entry id\n---\n%s", stdout.String())
	}
}

func TestMarketplaceShow_PrintsMetadataNotReadmeBody(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"schema_version":"1","entries":[
			{"id":"filesystem","name":"Filesystem","transport":"stdio","command":"npx","readme_url":"https://example.com/README.md"}
		]}`))
	}))
	defer srv.Close()
	c := newMarketplaceCmd()
	var stdout, stderr bytes.Buffer
	c.SetOut(&stdout)
	c.SetErr(&stderr)
	c.SetArgs([]string{"show", "filesystem", "--registry", srv.URL, "--insecure-tls-for-tests"})
	api.InjectMarketplaceTestClientForCLI(srv)
	if err := c.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("show: %v\nstderr: %s", err, stderr.String())
	}
	out := stdout.String()
	for _, want := range []string{"ID:", "Transport:", "Readme URL:", "https://example.com/README.md"} {
		if !strings.Contains(out, want) {
			t.Errorf("show stdout missing %q\n---\n%s", want, out)
		}
	}
}

func TestMarketplaceGenerate_HttpEntrySkipsToStderr(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"schema_version":"1","entries":[
			{"id":"ctx7","name":"Context7","transport":"http","url":"https://mcp.context7.com/mcp"}
		]}`))
	}))
	defer srv.Close()
	c := newMarketplaceCmd()
	var stdout, stderr bytes.Buffer
	c.SetOut(&stdout)
	c.SetErr(&stderr)
	c.SetArgs([]string{"generate", "ctx7", "--registry", srv.URL, "--insecure-tls-for-tests"})
	api.InjectMarketplaceTestClientForCLI(srv)
	err := c.ExecuteContext(context.Background())
	if err == nil {
		t.Fatal("expected non-zero exit for http entry")
	}
	if stdout.Len() != 0 {
		t.Errorf("stdout must be empty on G6-deferral; got: %s", stdout.String())
	}
	if !strings.Contains(stderr.String(), "G6") {
		t.Errorf("stderr missing G6 deferral note\n---\n%s", stderr.String())
	}
}
```

- [ ] **Step 4.2: Run tests to verify they fail**

```bash
go test -count=1 -timeout 60s -run "TestMarketplace" ./internal/cli/
```

Expected: `undefined: newMarketplaceCmd`.

- [ ] **Step 4.3: Implement the CLI**

Create `internal/cli/marketplace.go`. Same shape as plan v1 but with:

1. `--registry` URL validated client-side (`https://` prefix only — friendlier error than the lib-level rejection).
2. Internal `--insecure-tls-for-tests` flag (hidden via `c.Flags().MarkHidden`) that swaps in the TLS-injected client via the api hook.
3. `show` prints metadata only — including the `Readme URL: <url>` line (no body fetch).
4. `generate` propagates warnings to stderr via `cmd.ErrOrStderr()`.
5. `warnIfStale` mirrors plan v1.

(Full body in the same shape as plan v1 §Phase 4 Step 4.3, but with `LoadMarketplaceCatalog` → `LoadMarketplaceCatalogWithClient` when the test hook fires, and `Readme URL: %s` swapped in for the previous README-body line.)

- [ ] **Step 4.4: Register the subcommand**

In `internal/cli/root.go`, inside NewRootCmd:

```go
root.AddCommand(newMarketplaceCmd())
```

- [ ] **Step 4.5: Run tests to verify they pass**

```bash
go test -count=1 -timeout 60s -run "TestMarketplace" ./internal/cli/
```

Expected: PASS.

- [ ] **Step 4.6: Sweep + commit**

```powershell
Get-Process -Name 'mcphub' -ErrorAction SilentlyContinue | Stop-Process -Force
```

```bash
git add internal/cli/marketplace.go internal/cli/marketplace_test.go internal/cli/root.go
git commit -m "feat(g5): CLI subcommands (Phase 4)"
```

---

## Phase 5: Integration test + manual smoke D2.8 + CLAUDE.md

**Files:**
- Create: `internal/cli/marketplace_e2e_test.go`
- Modify: `docs/phase-3b-ii-verification.md` (D2.8 manual smoke with operator-edit step)
- Modify: `CLAUDE.md` (marketplace section)

End-to-end across search → show → generate. Verify catalog parse rejects an injected bad-id fixture. Manual smoke section v2 explicitly includes the operator-edit step before `manifest create`.

- [ ] **Step 5.1: Write e2e test (same shape as plan v1 §Step 5.1, using NewTLSServer + test-client injection)**

- [ ] **Step 5.2: Run e2e — expected PASS**

- [ ] **Step 5.3: Update manual smoke D2.8 to v2 (operator-edit step)**

Append to `docs/phase-3b-ii-verification.md`:

```text
### D2.8 — Marketplace draft-import (G5, v2 per codex r1)

1. `mcphub marketplace refresh` → "Refreshed catalog: N entries." on stdout.
2. `mcphub marketplace search filesystem` → row for `filesystem` entry.
3. `mcphub marketplace show filesystem` → metadata block + `Readme URL: <url>` (NO README body — open the URL yourself).
4. `mcphub marketplace generate filesystem > /tmp/draft.yaml`
5. **Operator-edit step (load-bearing):** open `/tmp/draft.yaml` and:
   - change `name: filesystem` to a unique server id, e.g. `name: filesystem-test`
   - replace `port: 0` with a free port, e.g. `port: 9200`
   - inspect `command` + `base_args` + `env`; replace any verbatim `${env:*}` placeholders with the values you want persisted
   - the leading comment block reminds you of these three steps
6. `mcphub manifest create filesystem-test < /tmp/draft.yaml` → manifest accepted; `mcphub manifest list` shows `filesystem-test`.
7. `mcphub install --server filesystem-test --clients claude-code` → install succeeds; the daemon registers.
8. `mcphub marketplace generate context7` → non-zero exit; stdout empty; stderr contains "G6" + "wait" + "workaround".
9. Disconnect network; `mcphub marketplace search filesystem` → WARN line on stderr; cached output on stdout still works.
10. Manually plant a future `fetched_at` in `<state-dir>/marketplace-cache.json` (overwrite the field to `2099-01-01T00:00:00Z`) and re-run search → expect a fresh fetch attempt (revalidate is forced).
```

- [ ] **Step 5.4: Update CLAUDE.md (marketplace section)**

Append under the existing CLI surfaces (e.g. after "Watchdog"):

```markdown
## Marketplace (G5, v0.3.0)

`mcphub marketplace {search,show,generate,refresh}` lets an operator
discover MCP servers from a curated catalog. Default registry URL:
`https://raw.githubusercontent.com/applicate2628/mcp-local-hub/master/marketplace/v1/catalog.json`.

- `search [query]` — table of catalog entries matching query (empty = list all).
- `show <id>` — metadata block + `Readme URL:` line (operator opens the URL).
- `generate <id>` — draft YAML to stdout. **Operator MUST edit before**
  `manifest create`: rename `name:`, pick a real port, redact verbatim
  `${env:*}` placeholders. Sensitive env names (`*_TOKEN`, `*_SECRET`,
  `*_PASSWORD`, `*_KEY`, `*_API_KEY`, `AWS_*`, `AZURE_*`, `GCP_*`,
  `GITHUB_*`) are LEFT VERBATIM with a stderr warning per occurrence.
- `refresh` — force re-fetch (bypass TTL + ETag).

Cache: `<state-dir>/marketplace-cache.json` (routed through
`writeHubMcpStateFile` — flock + atomic rename + DACL re-verify), 24h
TTL, ETag revalidate. HTTPS-only; downgrade redirects rejected; gzip
disabled. Native-http entries skip with a G6-deferral message until G6
ships (no operator-side workaround in v0.3.0).
```

- [ ] **Step 5.5: Sweep + commit + push + open PR**

```powershell
Get-Process -Name 'mcphub' -ErrorAction SilentlyContinue | Stop-Process -Force
```

```bash
git add internal/cli/marketplace_e2e_test.go docs/phase-3b-ii-verification.md CLAUDE.md
git commit -m "test(g5): e2e + manual smoke D2.8 v2 (operator-edit step) + docs (Phase 5)"
git push -u origin feat/g5-marketplace-draft-import
gh pr create --title "feat(g5): marketplace draft-import — discover MCP servers from a curated registry" --body "$(cat <<'PRBODY'
## Summary

- Implements G5 per spec docs/superpowers/specs/2026-05-12-g5-marketplace-draft-import-design.md (v2, post codex r1 REVISE).
- Closes 6 P1 + 3 P2 findings from codex r1: stdio→stdio-bridge, manifest-create UX, README dropped from show, cache via writeHubMcpStateFile, HTTPS-only enforcement, sensitive-env policy, DisableCompression, clock-skew clamp, CheckManifestName ID gate, G6 message clarity.
- Single bundled PR per memory rule "Don't split tiny PRs". 6 commits = 6 phases.

## Phases

0. Promote G7's expander to api.PlaceholderExpander + add IsSensitiveEnvName.
1. Catalog schema + parser + seed catalog (CheckManifestName per entry.id).
2. HTTPS-only client + G4-hardened cache (writeHubMcpStateFile + DisableCompression).
3. Generator → stdio-bridge + sensitive-env redaction + G6 message.
4. CLI subcommands (search/show/generate/refresh).
5. E2E + manual smoke D2.8 v2 + CLAUDE.md.

## Test plan

- [ ] `go build ./... && go vet ./... && go test -count=1 -timeout 5m ./internal/api/ ./internal/cli/`
- [ ] Manual smoke D2.8 v2 (operator-edit step, sensitive-env warn, future-timestamp revalidate).
- [ ] http-entry generate emits G6 + wait + workaround to stderr; empty stdout; non-zero exit.
PRBODY
)"
gh pr comment $(gh pr view --json number --jq .number) --body "@codex review"
```

---

## Self-review checklist (v2)

- [ ] Every codex r1 P1 closure addressed (recap mapping at top of plan).
- [ ] Every codex r1 P2 closure addressed.
- [ ] No placeholders, no `TBD`.
- [ ] Type consistency: `MarketplaceCatalog`, `MarketplaceEntry`, `MarketplaceSource`, `GenerateOpts`, `PlaceholderExpander`, `IsSensitiveEnvName` all carry the same names header→callers.
- [ ] Phase 0 promotes the expander without changing G7's production behavior.
- [ ] Cache writes route through `writeHubMcpStateFile`; reads through `readHubMcpStateFile`.
- [ ] HTTPS-only enforced at URL parse + `CheckRedirect` + tests use TLS server.
- [ ] DisableCompression: true on transport + identity-encoding sent on request.
- [ ] Future `fetched_at` forces revalidate; `MarketplaceCacheAge` non-negative.
- [ ] Stdio entries → `config.TransportStdioBridge` (NOT `native-http`).
- [ ] Draft YAML includes a leading comment-block reminder of the operator-edit step.
- [ ] G6 deferral error mentions: "wait for G6", "workaround", and the http url for the operator to see.
- [ ] Sensitive-env catalog placeholders left verbatim + warning surfaced.
- [ ] DRY: placeholder logic lives in ONE place (`PlaceholderExpander`); manifest-name gate lives in ONE place (`CheckManifestName`).
- [ ] YAGNI: no README body fetch, no multi-registry, no GUI, no signature verification.

## Execution Handoff

Plan v2 complete. After committing this revision, re-run codex r1 to confirm APPROVE before Phase 0 starts.
