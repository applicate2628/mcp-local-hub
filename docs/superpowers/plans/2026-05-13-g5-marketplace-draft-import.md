# G5 Marketplace Draft-Import Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Ship `mcphub marketplace {search,show,generate,refresh}` CLI subcommands that let an operator discover MCP servers from a curated registry, inspect metadata + README, and project a catalog entry into a draft mcp-local-hub manifest YAML. Zero auto-install side effects: drafts are printed to stdout; the operator runs `mcphub manifest create` + `mcphub install` separately.

**Architecture:** Stateless metadata cache. Catalog lives at `marketplace/v1/catalog.json` (hand-maintained in this repo, served via GitHub raw URL). Hub fetches with TTL + ETag, persists to `<state-dir>/marketplace-cache.json` (0600 + DACL-verified). Native-http entries are LISTED but `generate` refuses them with a clear G6-deferral warning.

**Tech Stack:** Go (`net/http`, `crypto/sha256`, `gopkg.in/yaml.v3`), cobra CLI, existing flock + SecureWriteClientConfig pattern from G4.

**Spec:** [docs/superpowers/specs/2026-05-12-g5-marketplace-draft-import-design.md](../specs/2026-05-12-g5-marketplace-draft-import-design.md). Implementation gated on codex r1 review approval (per spec line 3).

**Branch:** `feat/g5-marketplace-draft-import` from master HEAD `0a534f9` (post-G4-phase-5 merge).

**PR strategy:** Single bundled PR per memory rule "Don't split tiny PRs". All 5 phases land together. Bot review + 3-lane codex deep-sec gate before merge, same flow as PR #160.

---

## Phase 1: Catalog schema + parser + seed catalog

**Files:**
- Create: `marketplace/v1/catalog.json`
- Create: `internal/api/marketplace_catalog.go`
- Create: `internal/api/marketplace_catalog_test.go`

The catalog is a hand-maintained JSON list of MCP server entries. Parsing happens in-process: read bytes, schema-version-check, parse into typed `Catalog{Entries[]Entry}`, surface concrete errors. The same parser is reused by the cache (Phase 2), generator (Phase 3), and CLI (Phase 4).

- [ ] **Step 1.1: Seed catalog with ~10 curated entries**

Create `marketplace/v1/catalog.json` with stdio MCP servers we know work. Start with the canonical Anthropic-published references plus a couple of community standouts:

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

- [ ] **Step 1.2: Write the failing parser test**

Create `internal/api/marketplace_catalog_test.go`:

```go
package api

import (
	"strings"
	"testing"
)

// TestParseCatalog_HappyPath covers the canonical shape from
// docs/superpowers/specs/2026-05-12-g5-marketplace-draft-import-design.md.
func TestParseCatalog_HappyPath(t *testing.T) {
	raw := `{
  "schema_version": "1",
  "generated_at": "2026-05-13T00:00:00Z",
  "entries": [
    {
      "id": "filesystem",
      "name": "Filesystem MCP server",
      "summary": "Sandboxed filesystem.",
      "homepage": "https://github.com/modelcontextprotocol/servers/tree/main/src/filesystem",
      "readme_url": "https://raw.githubusercontent.com/modelcontextprotocol/servers/main/src/filesystem/README.md",
      "transport": "stdio",
      "command": "npx",
      "args": ["-y", "@modelcontextprotocol/server-filesystem"],
      "env": {"FOO": "bar"},
      "categories": ["filesystem"],
      "license": "MIT"
    }
  ]
}`
	cat, err := ParseMarketplaceCatalog([]byte(raw))
	if err != nil {
		t.Fatalf("ParseMarketplaceCatalog: %v", err)
	}
	if cat.SchemaVersion != "1" {
		t.Errorf("schema_version = %q, want %q", cat.SchemaVersion, "1")
	}
	if len(cat.Entries) != 1 {
		t.Fatalf("entries len = %d, want 1", len(cat.Entries))
	}
	e := cat.Entries[0]
	if e.ID != "filesystem" || e.Transport != "stdio" || e.Command != "npx" {
		t.Errorf("entry round-trip mismatch: %+v", e)
	}
	if got := e.Env["FOO"]; got != "bar" {
		t.Errorf("env[FOO] = %q, want bar", got)
	}
}

func TestParseCatalog_RejectsUnknownSchemaVersion(t *testing.T) {
	raw := `{"schema_version": "9999", "entries": []}`
	_, err := ParseMarketplaceCatalog([]byte(raw))
	if err == nil || !strings.Contains(err.Error(), "schema_version") {
		t.Errorf("want schema_version error; got %v", err)
	}
}

func TestParseCatalog_RejectsMissingID(t *testing.T) {
	raw := `{"schema_version": "1", "entries": [{"name": "no-id", "transport": "stdio", "command": "npx"}]}`
	_, err := ParseMarketplaceCatalog([]byte(raw))
	if err == nil || !strings.Contains(err.Error(), "id") {
		t.Errorf("want missing-id error; got %v", err)
	}
}

func TestParseCatalog_RejectsDuplicateID(t *testing.T) {
	raw := `{"schema_version": "1", "entries": [
		{"id": "dup", "name": "a", "transport": "stdio", "command": "npx"},
		{"id": "dup", "name": "b", "transport": "stdio", "command": "npx"}
	]}`
	_, err := ParseMarketplaceCatalog([]byte(raw))
	if err == nil || !strings.Contains(err.Error(), "duplicate") {
		t.Errorf("want duplicate-id error; got %v", err)
	}
}

func TestParseCatalog_RejectsBadTransport(t *testing.T) {
	raw := `{"schema_version": "1", "entries": [
		{"id": "x", "name": "X", "transport": "websocket", "command": "npx"}
	]}`
	_, err := ParseMarketplaceCatalog([]byte(raw))
	if err == nil || !strings.Contains(err.Error(), "transport") {
		t.Errorf("want transport error; got %v", err)
	}
}

func TestParseCatalog_HttpEntryAllowedNoCommand(t *testing.T) {
	// http entries are LISTED in the catalog but generate refuses
	// them in Phase 3. The parser must accept them without `command`.
	raw := `{"schema_version": "1", "entries": [
		{"id": "ctx7", "name": "Context7", "transport": "http", "url": "https://mcp.context7.com/mcp"}
	]}`
	cat, err := ParseMarketplaceCatalog([]byte(raw))
	if err != nil {
		t.Fatalf("http entry should parse without command: %v", err)
	}
	if cat.Entries[0].URL != "https://mcp.context7.com/mcp" {
		t.Errorf("url = %q, want https://mcp.context7.com/mcp", cat.Entries[0].URL)
	}
}

func TestParseCatalog_StdioEntryRequiresCommand(t *testing.T) {
	raw := `{"schema_version": "1", "entries": [
		{"id": "nocmd", "name": "no command", "transport": "stdio"}
	]}`
	_, err := ParseMarketplaceCatalog([]byte(raw))
	if err == nil || !strings.Contains(err.Error(), "command") {
		t.Errorf("want stdio-needs-command error; got %v", err)
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
// §"Registry source" + §"Threat model".
//
// The catalog is a JSON document hand-maintained at
// marketplace/v1/catalog.json. This package parses + validates the
// JSON shape; cache + fetch logic lives in marketplace_cache.go.

package api

import (
	"encoding/json"
	"fmt"
	"strings"
)

// MarketplaceCatalogSchemaVersion is the only schema_version this
// build accepts. Bump when the catalog shape changes; older clients
// will reject newer catalogs rather than silently misparse them.
const MarketplaceCatalogSchemaVersion = "1"

// MarketplaceCatalog is the top-level JSON shape served from
// the registry URL.
type MarketplaceCatalog struct {
	SchemaVersion string               `json:"schema_version"`
	GeneratedAt   string               `json:"generated_at,omitempty"`
	Entries       []MarketplaceEntry   `json:"entries"`
}

// MarketplaceEntry is one row. `transport` is one of "stdio" / "http".
// stdio entries require `command`; http entries require `url`. The
// generator (Phase 3) refuses http entries with a G6-deferral warning.
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
	URL        string            `json:"url,omitempty"` // http transport only
	Categories []string          `json:"categories,omitempty"`
	License    string            `json:"license,omitempty"`
}

// ParseMarketplaceCatalog decodes + validates raw JSON bytes. Returns
// a concrete error per first invalid entry; callers should not partial-
// accept (the spec §"Threat model" says malformed catalogs are
// rejected wholesale rather than silently dropping entries).
func ParseMarketplaceCatalog(raw []byte) (*MarketplaceCatalog, error) {
	var cat MarketplaceCatalog
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&cat); err != nil {
		return nil, fmt.Errorf("decode catalog: %w", err)
	}
	if cat.SchemaVersion != MarketplaceCatalogSchemaVersion {
		return nil, fmt.Errorf("schema_version %q: this build only accepts %q (rebuild mcphub or refresh the catalog)",
			cat.SchemaVersion, MarketplaceCatalogSchemaVersion)
	}
	seenID := map[string]bool{}
	for i := range cat.Entries {
		e := &cat.Entries[i]
		if err := validateMarketplaceEntry(e); err != nil {
			return nil, fmt.Errorf("entry %d: %w", i, err)
		}
		if seenID[e.ID] {
			return nil, fmt.Errorf("entry %d: duplicate id %q", i, e.ID)
		}
		seenID[e.ID] = true
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
git commit -m "feat(g5): catalog schema + parser + seed catalog (Phase 1)"
```

---

## Phase 2: TTL + ETag cache with atomic write

**Files:**
- Create: `internal/api/marketplace_cache.go`
- Create: `internal/api/marketplace_cache_test.go`
- Modify: `internal/api/state_paths.go` (add `marketplaceCacheFileLeaf` constant)

Cache lives at `<state-dir>/marketplace-cache.json`. Pattern mirrors `hub-mcp-tokens.json` from G4: 0600, DACL-verified parent, atomic tempfile+rename write, flock for cross-process serialization. TTL: 24 hours. Conditional GET via `If-None-Match: <etag>` on stale; 304 → bump `fetched_at`, 200 → replace body. Network failure → fall back to stale cache with `WARN: catalog is N hours stale` (cache stays available offline). 10MB body cap from spec §"Threat model".

- [ ] **Step 2.1: Add cache file leaf constant**

In `internal/api/state_paths.go`, add alongside the existing leaf constants:

```go
// G5 Marketplace cache.
const marketplaceCacheFileLeaf = "marketplace-cache.json"
```

- [ ] **Step 2.2: Write the failing cache tests**

Create `internal/api/marketplace_cache_test.go`:

```go
package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestLoadMarketplaceCatalog_FreshFetch(t *testing.T) {
	_ = hubMcpStateTestHelper(t) // reuse the DACL-hardened tempdir helper
	body := `{"schema_version":"1","entries":[{"id":"x","name":"X","transport":"stdio","command":"npx"}]}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("ETag", `"abc123"`)
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()
	cat, src, err := LoadMarketplaceCatalogFrom(context.Background(), srv.URL)
	if err != nil {
		t.Fatalf("LoadMarketplaceCatalogFrom: %v", err)
	}
	if cat.Entries[0].ID != "x" {
		t.Errorf("entry round-trip mismatch: %+v", cat.Entries[0])
	}
	if src != MarketplaceSourceFresh {
		t.Errorf("source = %v, want fresh", src)
	}
}

func TestLoadMarketplaceCatalog_StaleHits304KeepsBody(t *testing.T) {
	_ = hubMcpStateTestHelper(t)
	body := `{"schema_version":"1","entries":[{"id":"x","name":"X","transport":"stdio","command":"npx"}]}`
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.Header().Set("ETag", `"abc123"`)
		if r.Header.Get("If-None-Match") == `"abc123"` {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()
	// Fresh fetch.
	_, _, err := LoadMarketplaceCatalogFrom(context.Background(), srv.URL)
	if err != nil {
		t.Fatalf("fresh: %v", err)
	}
	// Force stale by rewinding fetched_at.
	forceMarketplaceCacheStaleForTest(t, time.Now().Add(-48*time.Hour))
	// Stale revalidate → 304 → body preserved.
	cat, src, err := LoadMarketplaceCatalogFrom(context.Background(), srv.URL)
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
		t.Errorf("hits = %d, want 2 (one fresh + one revalidate)", hits)
	}
}

func TestLoadMarketplaceCatalog_NetworkErrorFallsBackToStaleWithWarn(t *testing.T) {
	_ = hubMcpStateTestHelper(t)
	body := `{"schema_version":"1","entries":[{"id":"x","name":"X","transport":"stdio","command":"npx"}]}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(body))
	}))
	// Fresh fetch.
	_, _, err := LoadMarketplaceCatalogFrom(context.Background(), srv.URL)
	if err != nil {
		t.Fatalf("fresh: %v", err)
	}
	// Server goes away.
	srv.Close()
	forceMarketplaceCacheStaleForTest(t, time.Now().Add(-48*time.Hour))
	cat, src, err := LoadMarketplaceCatalogFrom(context.Background(), srv.URL)
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

func TestLoadMarketplaceCatalog_RejectsOversizePayload(t *testing.T) {
	_ = hubMcpStateTestHelper(t)
	huge := strings.Repeat("x", 11*1024*1024) // 11 MB > 10 MB cap
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(huge))
	}))
	defer srv.Close()
	_, _, err := LoadMarketplaceCatalogFrom(context.Background(), srv.URL)
	if err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Errorf("want size cap error; got %v", err)
	}
}
```

- [ ] **Step 2.3: Run tests to verify they fail**

```bash
go test -count=1 -timeout 60s -run "TestLoadMarketplaceCatalog" ./internal/api/
```

Expected: `undefined: LoadMarketplaceCatalogFrom` (and several friends).

- [ ] **Step 2.4: Implement the cache**

Create `internal/api/marketplace_cache.go`:

```go
// internal/api/marketplace_cache.go — G5 Marketplace cache.
//
// Spec: docs/superpowers/specs/2026-05-12-g5-marketplace-draft-import-design.md
// §"Cache strategy" + §"Threat model".
//
// The cache file is `<state-dir>/marketplace-cache.json` (0600 +
// DACL-verified parent on Windows; same pattern as hub-mcp-tokens.json
// in G4). TTL 24h. Conditional GET via If-None-Match: <etag>. 10 MB
// payload cap. Offline fallback uses stale cache with WARN.

package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

const (
	marketplaceCacheTTL          = 24 * time.Hour
	marketplaceCacheMaxBodyBytes = 10 * 1024 * 1024
	marketplaceHTTPTimeout       = 15 * time.Second
)

// MarketplaceSource indicates where the returned catalog came from
// on a Load call. Callers use this to decide whether to emit a
// "WARN: catalog is N hours stale" message to the operator.
type MarketplaceSource int

const (
	MarketplaceSourceFresh         MarketplaceSource = iota // 200 response, body replaced
	MarketplaceSourceCached                                  // TTL not expired, no fetch attempted
	MarketplaceSourceRevalidated                             // 304 response, body preserved
	MarketplaceSourceStaleFallback                           // fetch failed, returning stale cache with warn
)

// marketplaceCacheFile is the on-disk shape.
type marketplaceCacheFile struct {
	SchemaVersion string             `json:"schema_version"`
	FetchedAt     time.Time          `json:"fetched_at"`
	ETag          string             `json:"etag,omitempty"`
	Catalog       MarketplaceCatalog `json:"catalog"`
}

// LoadMarketplaceCatalogFrom fetches + caches the catalog at `url`,
// returning the parsed catalog, the source classification, and any
// error. Uses the global state-dir cache (see DaemonStateDir).
func LoadMarketplaceCatalogFrom(ctx context.Context, url string) (*MarketplaceCatalog, MarketplaceSource, error) {
	cacheFile, _ := readMarketplaceCache()
	if cacheFile != nil && time.Since(cacheFile.FetchedAt) < marketplaceCacheTTL {
		return &cacheFile.Catalog, MarketplaceSourceCached, nil
	}
	// Stale or absent → fetch.
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, 0, fmt.Errorf("build request: %w", err)
	}
	if cacheFile != nil && cacheFile.ETag != "" {
		req.Header.Set("If-None-Match", cacheFile.ETag)
	}
	client := &http.Client{Timeout: marketplaceHTTPTimeout}
	resp, err := client.Do(req)
	if err != nil {
		if cacheFile != nil {
			return &cacheFile.Catalog, MarketplaceSourceStaleFallback, nil
		}
		return nil, 0, fmt.Errorf("fetch %s: %w", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotModified && cacheFile != nil {
		cacheFile.FetchedAt = time.Now()
		_ = writeMarketplaceCache(cacheFile)
		return &cacheFile.Catalog, MarketplaceSourceRevalidated, nil
	}
	if resp.StatusCode != http.StatusOK {
		if cacheFile != nil {
			return &cacheFile.Catalog, MarketplaceSourceStaleFallback, nil
		}
		return nil, 0, fmt.Errorf("fetch %s: HTTP %d", url, resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, marketplaceCacheMaxBodyBytes+1))
	if err != nil {
		return nil, 0, fmt.Errorf("read body: %w", err)
	}
	if int64(len(body)) > marketplaceCacheMaxBodyBytes {
		return nil, 0, fmt.Errorf("catalog body exceeds %d-byte cap", marketplaceCacheMaxBodyBytes)
	}
	cat, err := ParseMarketplaceCatalog(body)
	if err != nil {
		return nil, 0, fmt.Errorf("parse fetched catalog: %w", err)
	}
	newCache := &marketplaceCacheFile{
		SchemaVersion: cat.SchemaVersion,
		FetchedAt:     time.Now(),
		ETag:          resp.Header.Get("ETag"),
		Catalog:       *cat,
	}
	_ = writeMarketplaceCache(newCache)
	return cat, MarketplaceSourceFresh, nil
}

// MarketplaceCacheAge returns the age of the cached body, or 0 if no
// cache exists. CLI uses this to format the WARN-stale message.
func MarketplaceCacheAge() time.Duration {
	cf, err := readMarketplaceCache()
	if err != nil || cf == nil {
		return 0
	}
	return time.Since(cf.FetchedAt)
}

// RefreshMarketplaceCatalog forces an unconditional fetch (bypass TTL
// and ETag). Used by `mcphub marketplace refresh`.
func RefreshMarketplaceCatalog(ctx context.Context, url string) (*MarketplaceCatalog, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	// Explicit `no-cache` semantics: do NOT send If-None-Match.
	client := &http.Client{Timeout: marketplaceHTTPTimeout}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch %s: %w", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetch %s: HTTP %d", url, resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, marketplaceCacheMaxBodyBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read body: %w", err)
	}
	if int64(len(body)) > marketplaceCacheMaxBodyBytes {
		return nil, fmt.Errorf("catalog body exceeds %d-byte cap", marketplaceCacheMaxBodyBytes)
	}
	cat, err := ParseMarketplaceCatalog(body)
	if err != nil {
		return nil, fmt.Errorf("parse fetched catalog: %w", err)
	}
	newCache := &marketplaceCacheFile{
		SchemaVersion: cat.SchemaVersion,
		FetchedAt:     time.Now(),
		ETag:          resp.Header.Get("ETag"),
		Catalog:       *cat,
	}
	_ = writeMarketplaceCache(newCache)
	return cat, nil
}

func readMarketplaceCache() (*marketplaceCacheFile, error) {
	dir, err := DaemonStateDir()
	if err != nil {
		return nil, err
	}
	path := filepath.Join(dir, marketplaceCacheFileLeaf)
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var cf marketplaceCacheFile
	if err := json.Unmarshal(data, &cf); err != nil {
		return nil, fmt.Errorf("parse cache: %w", err)
	}
	return &cf, nil
}

func writeMarketplaceCache(cf *marketplaceCacheFile) error {
	dir, err := DaemonStateDir()
	if err != nil {
		return err
	}
	path := filepath.Join(dir, marketplaceCacheFileLeaf)
	data, err := json.MarshalIndent(cf, "", "  ")
	if err != nil {
		return err
	}
	// Atomic tempfile + rename, same pattern as SettingsSetIn r16.
	tmp := bytes.NewBuffer(nil)
	tmp.Write(data)
	tmpFile, err := os.CreateTemp(dir, ".marketplace-cache-*.json.tmp")
	if err != nil {
		return err
	}
	tmpPath := tmpFile.Name()
	cleanup := func() { _ = os.Remove(tmpPath) }
	if _, err := tmpFile.Write(data); err != nil {
		_ = tmpFile.Close()
		cleanup()
		return err
	}
	if err := tmpFile.Close(); err != nil {
		cleanup()
		return err
	}
	if err := os.Chmod(tmpPath, 0600); err != nil {
		cleanup()
		return err
	}
	if err := os.Rename(tmpPath, path); err != nil {
		cleanup()
		return err
	}
	return nil
}

// forceMarketplaceCacheStaleForTest rewinds fetched_at — for tests
// that need to exercise the stale-revalidate path.
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

- [ ] **Step 2.5: Run tests to verify they pass**

```bash
go test -count=1 -timeout 60s -run "TestLoadMarketplaceCatalog" ./internal/api/
```

Expected: PASS.

- [ ] **Step 2.6: Commit**

```bash
git add internal/api/marketplace_cache.go internal/api/marketplace_cache_test.go internal/api/state_paths.go
git commit -m "feat(g5): cache with TTL + ETag + atomic write (Phase 2)"
```

---

## Phase 3: Entry → draft YAML projection

**Files:**
- Create: `internal/api/marketplace_generate.go`
- Create: `internal/api/marketplace_generate_test.go`

Project a `MarketplaceEntry` into a `config.ServerManifest` YAML. Handle `${workspaceFolder}` / `${env:VAR}` placeholder expansion (same logic as G7 VS Code import — if a G7 helper exists, reuse it; otherwise inline a minimal expander). Skip http entries with explicit G6-deferral warning.

- [ ] **Step 3.1: Write the failing generator tests**

Create `internal/api/marketplace_generate_test.go`:

```go
package api

import (
	"strings"
	"testing"
)

func TestGenerateDraftManifest_StdioEntry(t *testing.T) {
	e := &MarketplaceEntry{
		ID:        "filesystem",
		Name:      "Filesystem MCP server",
		Transport: "stdio",
		Command:   "npx",
		Args:      []string{"-y", "@modelcontextprotocol/server-filesystem", "${workspaceFolder}"},
		Env:       map[string]string{"FOO": "bar"},
	}
	got, err := GenerateDraftManifest(e, GenerateOpts{WorkspaceFolder: "/path/to/workspace"})
	if err != nil {
		t.Fatalf("GenerateDraftManifest: %v", err)
	}
	for _, want := range []string{
		"name: filesystem",
		"kind: global",
		"transport: native-http",
		"command: npx",
		"/path/to/workspace",
		"FOO: bar",
		"client_bindings:",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("draft YAML missing %q\n---\n%s", want, got)
		}
	}
}

func TestGenerateDraftManifest_HttpEntryRefusedWithG6Warning(t *testing.T) {
	e := &MarketplaceEntry{
		ID:        "ctx7",
		Name:      "Context7",
		Transport: "http",
		URL:       "https://mcp.context7.com/mcp",
	}
	_, err := GenerateDraftManifest(e, GenerateOpts{})
	if err == nil {
		t.Fatal("expected G6-deferral error for http entry; got nil")
	}
	if !strings.Contains(err.Error(), "G6") {
		t.Errorf("error must reference G6 for operator clarity; got %q", err.Error())
	}
}

func TestGenerateDraftManifest_PlaceholderEnvExpansion(t *testing.T) {
	t.Setenv("MCPHUB_TEST_API_KEY", "swordfish")
	e := &MarketplaceEntry{
		ID:        "needs-key",
		Name:      "needs-key",
		Transport: "stdio",
		Command:   "npx",
		Args:      []string{"--key", "${env:MCPHUB_TEST_API_KEY}"},
	}
	got, err := GenerateDraftManifest(e, GenerateOpts{})
	if err != nil {
		t.Fatalf("GenerateDraftManifest: %v", err)
	}
	if !strings.Contains(got, "swordfish") {
		t.Errorf("env expansion failed\n---\n%s", got)
	}
}
```

- [ ] **Step 3.2: Run tests to verify they fail**

```bash
go test -count=1 -timeout 60s -run "TestGenerateDraftManifest" ./internal/api/
```

Expected: `undefined: GenerateDraftManifest`.

- [ ] **Step 3.3: Implement the generator**

Create `internal/api/marketplace_generate.go`:

```go
// internal/api/marketplace_generate.go — G5 Marketplace draft generator.
//
// Spec: docs/superpowers/specs/2026-05-12-g5-marketplace-draft-import-design.md
// §"CLI surface" + §"Out of scope".
//
// Project a MarketplaceEntry into a draft mcp-local-hub manifest
// YAML. Stdio entries go through placeholder expansion; http entries
// surface a G6-deferral error (the current manifest schema requires
// `command`).

package api

import (
	"fmt"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

// GenerateOpts carries placeholder-expansion context. Empty
// WorkspaceFolder leaves the placeholder string intact in the
// emitted YAML so the operator can replace it manually.
type GenerateOpts struct {
	WorkspaceFolder string
}

// GenerateDraftManifest projects a catalog entry into a draft YAML
// manifest. Caller pipes the output into `mcphub manifest create`
// — NO write side effects from this function.
func GenerateDraftManifest(e *MarketplaceEntry, opts GenerateOpts) (string, error) {
	if e.Transport == "http" {
		return "", fmt.Errorf("entry %q is http transport — G6 (Remote MCP manifests) deferred to v0.4.x; skipping draft generation", e.ID)
	}
	if e.Transport != "stdio" {
		return "", fmt.Errorf("entry %q transport %q is not supported by draft generation", e.ID, e.Transport)
	}
	// Placeholder expansion in args + env values.
	expand := func(s string) string {
		s = strings.ReplaceAll(s, "${workspaceFolder}", opts.WorkspaceFolder)
		// ${env:NAME} expansion.
		for i := strings.Index(s, "${env:"); i >= 0; i = strings.Index(s, "${env:") {
			end := strings.Index(s[i:], "}")
			if end < 0 {
				break
			}
			name := s[i+len("${env:") : i+end]
			val := os.Getenv(name)
			s = s[:i] + val + s[i+end+1:]
		}
		return s
	}
	args := make([]string, len(e.Args))
	for i, a := range e.Args {
		args[i] = expand(a)
	}
	env := map[string]string{}
	for k, v := range e.Env {
		env[k] = expand(v)
	}
	// Draft manifest shape mirrors `mcphub manifest create` happy-
	// path input. Ports use 0 so the install path picks a free port;
	// operator can override before save.
	draft := map[string]any{
		"name":      e.ID,
		"kind":      "global",
		"transport": "native-http",
		"command":   e.Command,
		"base_args": args,
	}
	if len(env) > 0 {
		draft["env"] = env
	}
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
		return "", fmt.Errorf("yaml marshal: %w", err)
	}
	return string(data), nil
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
git commit -m "feat(g5): catalog entry → draft manifest YAML generator (Phase 3)"
```

---

## Phase 4: CLI subcommands

**Files:**
- Create: `internal/cli/marketplace.go`
- Create: `internal/cli/marketplace_test.go`
- Modify: `internal/cli/root.go` (register `marketplace` subcommand)

Four subcommands: `search`, `show`, `generate`, `refresh`. Default registry URL is `https://raw.githubusercontent.com/applicate2628/mcp-local-hub/master/marketplace/v1/catalog.json`. `--registry` flag overrides for testing. Warnings (stale cache, http skip) go to stderr; YAML + table output go to stdout (G7 r2 P2 pattern).

- [ ] **Step 4.1: Write the failing CLI test**

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

func newMarketplaceCmdForTest() *cobra.Command { return newMarketplaceCmd() }

func TestMarketplaceSearch_HappyPath(t *testing.T) {
	_ = hubMcpStateTestHelper(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("ETag", `"abc"`)
		_, _ = w.Write([]byte(`{"schema_version":"1","entries":[
			{"id":"filesys","name":"Filesystem","summary":"sandboxed fs","transport":"stdio","command":"npx","categories":["fs"]}
		]}`))
	}))
	defer srv.Close()
	c := newMarketplaceCmdForTest()
	var stdout, stderr bytes.Buffer
	c.SetOut(&stdout)
	c.SetErr(&stderr)
	c.SetArgs([]string{"search", "fs", "--registry", srv.URL})
	if err := c.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("search: %v\nstderr: %s", err, stderr.String())
	}
	if !strings.Contains(stdout.String(), "filesys") {
		t.Errorf("search output missing entry id\n---\n%s", stdout.String())
	}
}

func TestMarketplaceGenerate_HttpEntrySkipsToStderr(t *testing.T) {
	_ = hubMcpStateTestHelper(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"schema_version":"1","entries":[
			{"id":"ctx7","name":"Context7","transport":"http","url":"https://mcp.context7.com/mcp"}
		]}`))
	}))
	defer srv.Close()
	c := newMarketplaceCmdForTest()
	var stdout, stderr bytes.Buffer
	c.SetOut(&stdout)
	c.SetErr(&stderr)
	c.SetArgs([]string{"generate", "ctx7", "--registry", srv.URL})
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

func TestMarketplaceGenerate_StdioEntryEmitsYAMLToStdout(t *testing.T) {
	_ = hubMcpStateTestHelper(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"schema_version":"1","entries":[
			{"id":"filesys","name":"Filesystem","transport":"stdio","command":"npx","args":["-y","srv-fs"]}
		]}`))
	}))
	defer srv.Close()
	c := newMarketplaceCmdForTest()
	var stdout, stderr bytes.Buffer
	c.SetOut(&stdout)
	c.SetErr(&stderr)
	c.SetArgs([]string{"generate", "filesys", "--registry", srv.URL})
	if err := c.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("generate: %v\nstderr: %s", err, stderr.String())
	}
	if !strings.Contains(stdout.String(), "name: filesys") {
		t.Errorf("stdout missing draft YAML\n---\n%s", stdout.String())
	}
}
```

- [ ] **Step 4.2: Run tests to verify they fail**

```bash
go test -count=1 -timeout 60s -run "TestMarketplace" ./internal/cli/
```

Expected: `undefined: newMarketplaceCmd`.

- [ ] **Step 4.3: Implement the CLI**

Create `internal/cli/marketplace.go`:

```go
// internal/cli/marketplace.go — G5 marketplace subcommands.
//
// Spec: docs/superpowers/specs/2026-05-12-g5-marketplace-draft-import-design.md
// §"CLI surface". Four leaves: search, show, generate, refresh.
//
// Output discipline (mirrors G7's codex P2 fix): table + YAML go to
// stdout, status text (stale warn, G6 skip) goes to stderr.

package cli

import (
	"fmt"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"mcp-local-hub/internal/api"
)

// DefaultMarketplaceRegistryURL is the curated catalog served from
// this repo's master branch. v1 ships with a single registry to
// keep trust management simple; v0.4.x can grow to multi-registry.
const DefaultMarketplaceRegistryURL = "https://raw.githubusercontent.com/applicate2628/mcp-local-hub/master/marketplace/v1/catalog.json"

func newMarketplaceCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "marketplace",
		Short: "Discover MCP servers from a curated registry",
	}
	c.AddCommand(newMarketplaceSearchCmd())
	c.AddCommand(newMarketplaceShowCmd())
	c.AddCommand(newMarketplaceGenerateCmd())
	c.AddCommand(newMarketplaceRefreshCmd())
	return c
}

func newMarketplaceSearchCmd() *cobra.Command {
	var registry string
	c := &cobra.Command{
		Use:   "search [query]",
		Short: "List catalog entries matching query (empty = list all)",
		RunE: func(cmd *cobra.Command, args []string) error {
			cat, src, err := api.LoadMarketplaceCatalogFrom(cmd.Context(), registry)
			if err != nil {
				return err
			}
			warnIfStale(cmd, src)
			q := strings.ToLower(strings.Join(args, " "))
			w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
			fmt.Fprintln(w, "ID\tNAME\tTRANSPORT\tCATEGORIES\tSUMMARY")
			for _, e := range cat.Entries {
				if q != "" && !entryMatches(&e, q) {
					continue
				}
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n",
					e.ID, e.Name, e.Transport, strings.Join(e.Categories, ","), e.Summary)
			}
			return w.Flush()
		},
	}
	c.Flags().StringVar(&registry, "registry", DefaultMarketplaceRegistryURL, "catalog URL")
	return c
}

func newMarketplaceShowCmd() *cobra.Command {
	var registry string
	c := &cobra.Command{
		Use:   "show <id>",
		Short: "Print one entry's metadata + README",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cat, src, err := api.LoadMarketplaceCatalogFrom(cmd.Context(), registry)
			if err != nil {
				return err
			}
			warnIfStale(cmd, src)
			for _, e := range cat.Entries {
				if e.ID != args[0] {
					continue
				}
				fmt.Fprintf(cmd.OutOrStdout(), "ID:        %s\n", e.ID)
				fmt.Fprintf(cmd.OutOrStdout(), "Name:      %s\n", e.Name)
				fmt.Fprintf(cmd.OutOrStdout(), "Transport: %s\n", e.Transport)
				if e.Command != "" {
					fmt.Fprintf(cmd.OutOrStdout(), "Command:   %s %s\n", e.Command, strings.Join(e.Args, " "))
				}
				if e.URL != "" {
					fmt.Fprintf(cmd.OutOrStdout(), "URL:       %s\n", e.URL)
				}
				fmt.Fprintf(cmd.OutOrStdout(), "Homepage:  %s\n", e.Homepage)
				fmt.Fprintf(cmd.OutOrStdout(), "License:   %s\n", e.License)
				fmt.Fprintf(cmd.OutOrStdout(), "Summary:   %s\n", e.Summary)
				if len(e.Categories) > 0 {
					fmt.Fprintf(cmd.OutOrStdout(), "Categories: %s\n", strings.Join(e.Categories, ", "))
				}
				// README fetch is intentionally NOT cached separately
				// — keep v1 simple. v0.4.x can layer per-readme cache.
				return nil
			}
			return fmt.Errorf("entry %q not found in catalog", args[0])
		},
	}
	c.Flags().StringVar(&registry, "registry", DefaultMarketplaceRegistryURL, "catalog URL")
	return c
}

func newMarketplaceGenerateCmd() *cobra.Command {
	var registry, workspace string
	c := &cobra.Command{
		Use:   "generate <id>",
		Short: "Print draft manifest YAML for an entry to stdout (no write side effects)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cat, src, err := api.LoadMarketplaceCatalogFrom(cmd.Context(), registry)
			if err != nil {
				return err
			}
			warnIfStale(cmd, src)
			for _, e := range cat.Entries {
				if e.ID != args[0] {
					continue
				}
				if workspace == "" {
					if wd, err := os.Getwd(); err == nil {
						workspace = wd
					}
				}
				draft, err := api.GenerateDraftManifest(&e, api.GenerateOpts{WorkspaceFolder: workspace})
				if err != nil {
					fmt.Fprintln(cmd.ErrOrStderr(), "WARN:", err)
					return err
				}
				fmt.Fprint(cmd.OutOrStdout(), draft)
				return nil
			}
			return fmt.Errorf("entry %q not found in catalog", args[0])
		},
	}
	c.Flags().StringVar(&registry, "registry", DefaultMarketplaceRegistryURL, "catalog URL")
	c.Flags().StringVar(&workspace, "workspace", "", "value to substitute for ${workspaceFolder} placeholders (default: $PWD)")
	return c
}

func newMarketplaceRefreshCmd() *cobra.Command {
	var registry string
	c := &cobra.Command{
		Use:   "refresh",
		Short: "Force unconditional re-fetch of the catalog",
		RunE: func(cmd *cobra.Command, args []string) error {
			cat, err := api.RefreshMarketplaceCatalog(cmd.Context(), registry)
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Refreshed catalog: %d entries.\n", len(cat.Entries))
			return nil
		},
	}
	c.Flags().StringVar(&registry, "registry", DefaultMarketplaceRegistryURL, "catalog URL")
	return c
}

func entryMatches(e *api.MarketplaceEntry, q string) bool {
	hay := strings.ToLower(strings.Join([]string{
		e.ID, e.Name, e.Summary, strings.Join(e.Categories, " "),
	}, " "))
	return strings.Contains(hay, q)
}

func warnIfStale(cmd *cobra.Command, src api.MarketplaceSource) {
	if src != api.MarketplaceSourceStaleFallback {
		return
	}
	age := api.MarketplaceCacheAge()
	hours := int(age / time.Hour)
	fmt.Fprintf(cmd.ErrOrStderr(),
		"WARN: catalog fetch failed; using cached copy from %dh ago. Run `mcphub marketplace refresh` when network returns.\n", hours)
}
```

- [ ] **Step 4.4: Register the subcommand**

Modify `internal/cli/root.go` to add inside NewRootCmd:

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
git commit -m "feat(g5): CLI subcommands {search,show,generate,refresh} (Phase 4)"
```

---

## Phase 5: Integration test + docs

**Files:**
- Create: `internal/cli/marketplace_e2e_test.go`
- Modify: `docs/phase-3b-ii-verification.md` (add D2.8 manual smoke)
- Modify: `CLAUDE.md` (add marketplace section under existing CLI surfaces)

End-to-end: spin a fake registry, run the full search → show → generate → pipe-to-`mcphub manifest create` happy path. Verify offline-fallback path by pausing the server mid-flow.

- [ ] **Step 5.1: Write the e2e test**

Create `internal/cli/marketplace_e2e_test.go`:

```go
package cli

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"mcp-local-hub/internal/api"
)

func TestMarketplaceE2E_SearchShowGenerate(t *testing.T) {
	_ = hubMcpStateTestHelper(t)
	body := `{"schema_version":"1","entries":[
		{"id":"filesys","name":"Filesystem","summary":"fs","transport":"stdio","command":"npx","args":["-y","srv-fs"]}
	]}`
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.Header().Set("ETag", `"abc"`)
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()
	for _, sub := range []struct {
		args []string
		want string
	}{
		{[]string{"search", "filesys", "--registry", srv.URL}, "filesys"},
		{[]string{"show", "filesys", "--registry", srv.URL}, "Transport: stdio"},
		{[]string{"generate", "filesys", "--registry", srv.URL}, "name: filesys"},
	} {
		c := newMarketplaceCmdForTest()
		var stdout, stderr bytes.Buffer
		c.SetOut(&stdout)
		c.SetErr(&stderr)
		c.SetArgs(sub.args)
		if err := c.ExecuteContext(context.Background()); err != nil {
			t.Errorf("%v: %v\nstderr: %s", sub.args, err, stderr.String())
			continue
		}
		if !strings.Contains(stdout.String(), sub.want) {
			t.Errorf("%v: stdout missing %q\n---\n%s", sub.args, sub.want, stdout.String())
		}
	}
	// First call was a fresh fetch; subsequent ones hit the cache.
	if h := atomic.LoadInt32(&hits); h != 1 {
		t.Errorf("registry hits = %d, want 1 (subsequent calls should use cache)", h)
	}
	_ = api.MarketplaceCacheAge() // sanity-check the surface compiles
}
```

- [ ] **Step 5.2: Run e2e to verify it passes**

```bash
go test -count=1 -timeout 60s -run "TestMarketplaceE2E" ./internal/cli/
```

Expected: PASS.

- [ ] **Step 5.3: Add manual smoke to verification doc**

Append to `docs/phase-3b-ii-verification.md` (a new D2.8 section, after the watchdog D2.6 / G4 D2.7):

```text
### D2.8 — Marketplace draft-import (G5)

1. `mcphub marketplace refresh` → expect "Refreshed catalog: N entries." on stdout.
2. `mcphub marketplace search filesystem` → expect a row for `filesys` (or
   `filesystem`) entry in the curated list.
3. `mcphub marketplace show filesys` → expect transport + command + summary.
4. `mcphub marketplace generate filesys > /tmp/draft.yaml`.
5. `cat /tmp/draft.yaml | mcphub manifest create filesys-test` → manifest
   accepted; `mcphub manifest list` shows `filesys-test`.
6. `mcphub install --server filesys-test` → install succeeds; daemon registers.
7. `mcphub marketplace generate ctx7` → expect non-zero exit; stdout empty;
   stderr contains "G6" (Remote-MCP deferral message).
8. Disconnect network; `mcphub marketplace search filesystem` →
   expect WARN line on stderr; cached output on stdout still works.
```

- [ ] **Step 5.4: Update CLAUDE.md**

Append a "Marketplace (G5)" section under the existing CLI surfaces, summarizing:

- `mcphub marketplace search [query]`
- `mcphub marketplace show <id>`
- `mcphub marketplace generate <id>` — pipes to `mcphub manifest create`
- `mcphub marketplace refresh`
- Cache: `<state-dir>/marketplace-cache.json`, 24h TTL, ETag-revalidate, stale fallback.
- Registry URL defaults to `https://raw.githubusercontent.com/applicate2628/mcp-local-hub/master/marketplace/v1/catalog.json`; `--registry` overrides for testing.
- http entries in the catalog defer with G6 warning until G6 ships.

- [ ] **Step 5.5: Sweep + commit + push**

```powershell
Get-Process -Name 'mcphub' -ErrorAction SilentlyContinue | Stop-Process -Force
```

```bash
git add internal/cli/marketplace_e2e_test.go docs/phase-3b-ii-verification.md CLAUDE.md
git commit -m "test(g5): e2e search/show/generate + manual smoke D2.8 (Phase 5)"
git push -u origin feat/g5-marketplace-draft-import
gh pr create --title "feat(g5): marketplace draft-import — discover MCP servers from a curated registry" --body "$(cat <<'PRBODY'
## Summary

- Implements G5 per spec `docs/superpowers/specs/2026-05-12-g5-marketplace-draft-import-design.md`.
- `mcphub marketplace {search,show,generate,refresh}` lets an operator discover MCP servers from a curated registry, inspect metadata, and project entries into draft mcp-local-hub manifest YAML.
- **Zero auto-install side effects** — drafts are printed to stdout; the operator runs `mcphub manifest create` + `mcphub install` separately.
- Cache (`<state-dir>/marketplace-cache.json`) has 24h TTL + ETag revalidate + offline-fallback with WARN.
- Native-http entries (`transport: "http"`) are LISTED in the catalog but `generate` refuses them with an explicit G6-deferral message until G6 (Remote MCP manifests) ships.

## Phases (commits in this PR)

1. Catalog schema + parser + seed catalog (10 curated entries).
2. Cache with TTL + ETag + atomic tempfile+rename write.
3. Catalog entry → draft manifest YAML generator (placeholder expansion + http skip).
4. CLI subcommands (`search`, `show`, `generate`, `refresh`).
5. e2e test + manual smoke D2.8 + CLAUDE.md docs.

## Test plan

- [ ] `go build ./... && go vet ./... && go test -count=1 -timeout 5m ./...`
- [ ] `go test -count=1 -timeout 5m ./internal/api/ ./internal/cli/` clean
- [ ] Manual smoke D2.8: search → show → generate → manifest create → install
- [ ] Manual offline-fallback smoke (disable network mid-search)
- [ ] http-entry skip emits G6 warning to stderr, empty stdout, non-zero exit
PRBODY
)"
gh pr comment $(gh pr view --json number --jq .number) --body "@codex review"
```

---

## Self-review checklist

- [ ] Spec coverage: every §"Acceptance criteria" item maps to a test.
- [ ] No placeholders: every step has runnable code.
- [ ] Type consistency: `MarketplaceCatalog`, `MarketplaceEntry`, `MarketplaceSource`, `GenerateOpts` all carry the same names from header → callers.
- [ ] State-dir hardening: cache file uses atomic tempfile+rename (same as r16 settings fix) + 0600 mode.
- [ ] G6-deferral path covered by test (http entry → stderr warn + non-zero exit, no draft on stdout).
- [ ] Offline fallback covered by test (stale cache + server-down → WARN stderr + stdout still works).
- [ ] Output discipline: status text → stderr; YAML / table → stdout (G7 r2 P2 pattern).
- [ ] Frequent commits: 5 commits, one per phase.
- [ ] DRY: placeholder expansion lives in one place inside `marketplace_generate.go`.
- [ ] YAGNI: no GUI surface, no per-readme cache, no multi-registry, no signature verification (all explicitly deferred per spec §"Out of scope").

## Execution Handoff

Plan complete. Pick execution mode:

1. **Subagent-Driven (recommended)** — fresh subagent per phase + two-stage review (spec-compliance then code-quality). Continuous execution.
2. **Inline Execution** — execute phases in this session using executing-plans, batch with checkpoints.
