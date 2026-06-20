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
	cat, src, err := LoadMarketplaceCatalogWithClient(context.Background(), client, MarketplaceTestRegistryURL("/catalog.json"))
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
	registryURL := MarketplaceTestRegistryURL("/catalog.json")
	if _, _, err := LoadMarketplaceCatalogWithClient(context.Background(), client, registryURL); err != nil {
		t.Fatalf("fresh: %v", err)
	}
	forceMarketplaceCacheStaleForTest(t, time.Now().Add(-48*time.Hour))
	cat, src, err := LoadMarketplaceCatalogWithClient(context.Background(), client, registryURL)
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
	registryURL := MarketplaceTestRegistryURL("/catalog.json")
	if _, _, err := LoadMarketplaceCatalogWithClient(context.Background(), client, registryURL); err != nil {
		t.Fatalf("fresh: %v", err)
	}
	srv.Close()
	forceMarketplaceCacheStaleForTest(t, time.Now().Add(-48*time.Hour))
	cat, src, err := LoadMarketplaceCatalogWithClient(context.Background(), client, registryURL)
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

// TestReadMarketplaceCache_RejectsTamperedInvalidCatalog pins the
// cache trust boundary: cache JSON can be written or tampered after the
// original fetch validation, so the read path must re-validate the
// embedded catalog before any cached return path can serve it.
func TestReadMarketplaceCache_RejectsTamperedInvalidCatalog(t *testing.T) {
	_ = hubMcpStateTestHelper(t)
	payload := `{"schema_version":"1","fetched_at":"` + time.Now().UTC().Format(time.RFC3339Nano) + `","source_url":"https://registry.example/catalog.json","catalog":{"schema_version":"1","entries":[{"id":"evil","name":"Evil","transport":"http","url":"http://evil.example/mcp"}]}}`
	if err := writeHubMcpStateFile(marketplaceCacheFileLeaf, []byte(payload)); err != nil {
		t.Fatalf("write tampered cache: %v", err)
	}
	cf, err := readMarketplaceCache()
	if err == nil {
		t.Fatalf("expected tampered cache rejection; got cache %+v", cf)
	}
	if cf != nil {
		t.Fatalf("tampered cache returned alongside error: %+v", cf)
	}
	if !strings.Contains(err.Error(), "cache") || !strings.Contains(err.Error(), "https://") {
		t.Fatalf("error should identify cache validation and https invariant; got %v", err)
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
	registryURL := MarketplaceTestRegistryURL("/catalog.json")
	if _, _, err := LoadMarketplaceCatalogWithClient(context.Background(), client, registryURL); err != nil {
		t.Fatalf("fresh: %v", err)
	}
	// Plant a future fetched_at — must NOT be treated as fresh.
	forceMarketplaceCacheStaleForTest(t, time.Now().Add(24*time.Hour))
	_, _, err := LoadMarketplaceCatalogWithClient(context.Background(), client, registryURL)
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

// TestLoadMarketplaceCatalog_RegistryURLSwitchForcesFresh pins codex
// r5 P1 closure: a cache hit from registry A must not be served when
// the operator queries registry B via `--registry`. Both registries
// are fully fresh (under TTL), so the only way "miss" can fire for
// the second request is the SourceURL gate.
func TestLoadMarketplaceCatalog_RegistryURLSwitchForcesFresh(t *testing.T) {
	_ = hubMcpStateTestHelper(t)
	bodyA := `{"schema_version":"1","entries":[{"id":"alpha","name":"Alpha","transport":"stdio","command":"npx"}]}`
	bodyB := `{"schema_version":"1","entries":[{"id":"beta","name":"Beta","transport":"stdio","command":"npx"}]}`
	var hitsA, hitsB int32
	srvA := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hitsA, 1)
		_, _ = w.Write([]byte(bodyA))
	}))
	defer srvA.Close()
	srvB := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hitsB, 1)
		_, _ = w.Write([]byte(bodyB))
	}))
	defer srvB.Close()
	clientA := injectTLSTestClient(srvA)
	clientB := injectTLSTestClient(srvB)
	registryA := MarketplaceTestRegistryURL("/catalog-a.json")
	registryB := MarketplaceTestRegistryURL("/catalog-b.json")
	// Prime with registry A.
	catA, srcA, err := LoadMarketplaceCatalogWithClient(context.Background(), clientA, registryA)
	if err != nil {
		t.Fatalf("primeA: %v", err)
	}
	if catA.Entries[0].ID != "alpha" || srcA != MarketplaceSourceFresh {
		t.Fatalf("primeA result wrong: id=%s src=%v", catA.Entries[0].ID, srcA)
	}
	// Switch to registry B WITHOUT TTL expiry. The cache is fresh
	// by age, so without the SourceURL gate the prior alpha entry
	// would be returned for the beta query.
	catB, srcB, err := LoadMarketplaceCatalogWithClient(context.Background(), clientB, registryB)
	if err != nil {
		t.Fatalf("switchB: %v", err)
	}
	if catB.Entries[0].ID != "beta" {
		t.Fatalf("registry switch served wrong cache: got id=%s, want beta", catB.Entries[0].ID)
	}
	if srcB != MarketplaceSourceFresh {
		t.Errorf("registry switch source = %v; want fresh", srcB)
	}
	if atomic.LoadInt32(&hitsB) != 1 {
		t.Errorf("registry B not contacted: hitsB=%d, want 1", hitsB)
	}
	// Second read against registry A should still serve fresh cache
	// from B's write — but because B overwrote the cache file, the
	// A read will SEE a B-URL entry and treat it as miss. This is
	// the documented "single-file cache, last-write-wins" shape; the
	// regression we care about is that the wrong-URL entry is never
	// served, which is what the catB.Entries[0].ID assertion above
	// proves.
}

func TestLoadMarketplaceCatalog_RejectsOversizePayload(t *testing.T) {
	_ = hubMcpStateTestHelper(t)
	huge := strings.Repeat("x", 11*1024*1024)
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(huge))
	}))
	defer srv.Close()
	client := injectTLSTestClient(srv)
	_, _, err := LoadMarketplaceCatalogWithClient(context.Background(), client, MarketplaceTestRegistryURL("/catalog.json"))
	if err == nil || !strings.Contains(err.Error(), "cap") {
		t.Errorf("want size cap error; got %v", err)
	}
}

// Suppress unused-import warning when this file is the only test
// file referencing tls.
var _ = tls.VersionTLS12
