// internal/api/marketplace_cache.go — G5 Marketplace cache.
//
// Spec: docs/superpowers/specs/2026-05-12-g5-marketplace-draft-import-design.md
// §"Cache strategy".
//
// codex r1 P1 closure: cache writes route through
// writeHubMcpStateFile (G4 SecureWriteClientConfig: atomic tempfile
// + rename + post-rename DACL re-verify; best-effort, no cross-process
// flock). Reads route through readHubMcpStateFile
// (VerifyHubMcpStateDACL gates the open). Future fetched_at and
// negative ages are clamped (P2 closure).
//
// codex r5 P1 closure: cache entries carry the source URL they were
// fetched from. A read with a different rawURL (e.g. operator
// switched `--registry`) is treated as a miss, forcing a fresh GET
// against the requested registry instead of returning a previous
// registry's body. Legacy cache files written before this change
// have an empty SourceURL and therefore always force a refetch on
// first read — graceful migration without an explicit schema bump.

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
	MarketplaceSourceFresh MarketplaceSource = iota
	MarketplaceSourceCached
	MarketplaceSourceRevalidated
	MarketplaceSourceStaleFallback
)

type marketplaceCacheFile struct {
	SchemaVersion string             `json:"schema_version"`
	FetchedAt     time.Time          `json:"fetched_at"`
	ETag          string             `json:"etag,omitempty"`
	SourceURL     string             `json:"source_url,omitempty"`
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
	// codex r5 P1 closure: a cache entry from a different registry
	// URL must not be served for the current rawURL. Drop the
	// reference so the rest of the function treats it as a miss
	// (no ETag, no stale-fallback against the wrong source) and
	// the next successful fetch overwrites the file with the
	// correct SourceURL.
	if cf != nil && cf.SourceURL != rawURL {
		cf = nil
	}
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
		// codex r2 P2 closure: malformed fresh body falls back
		// to stale cache when available (spec §"Cache strategy":
		// "If the fetch fails for any reason ... cached stale
		// data is used with a clear WARN"). The parse error is
		// the same failure surface as a network error for the
		// operator: the registry returned junk, but the local
		// cache is still serviceable.
		if cf != nil {
			_ = LogHubMcpEvent("warn", "marketplace-catalog-parse-failed", map[string]any{
				"err": err.Error(),
			})
			return &cf.Catalog, MarketplaceSourceStaleFallback, nil
		}
		return nil, 0, fmt.Errorf("parse fetched catalog: %w", err)
	}
	newCache := &marketplaceCacheFile{
		SchemaVersion: cat.SchemaVersion,
		FetchedAt:     time.Now(),
		ETag:          res.ETag,
		SourceURL:     rawURL,
		Catalog:       *cat,
	}
	if werr := writeMarketplaceCache(newCache); werr != nil {
		// codex r2 P1 closure: cache persistence failure is
		// non-fatal for the operator's immediate query — the
		// freshly-fetched catalog is still returned. Surface
		// the write error to the hub-mcp event log so an
		// operator running `mcphub hub-mcp status` sees the
		// recent failure. The next successful refresh
		// overwrites the file, so there's no permanent state to
		// reconcile. (Best-effort cache persistence is the
		// documented contract — the on-disk cache is
		// optimization, not authoritative state.)
		_ = LogHubMcpEvent("warn", "marketplace-cache-write-failed", map[string]any{
			"err": werr.Error(),
		})
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
	if werr := writeMarketplaceCache(&marketplaceCacheFile{
		SchemaVersion: cat.SchemaVersion,
		FetchedAt:     time.Now(),
		ETag:          res.ETag,
		SourceURL:     rawURL,
		Catalog:       *cat,
	}); werr != nil {
		// codex r2 P1 closure (Refresh path): same best-effort
		// contract as Load — log + return the fresh catalog.
		_ = LogHubMcpEvent("warn", "marketplace-cache-write-failed", map[string]any{
			"err":  werr.Error(),
			"path": "refresh",
		})
	}
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
