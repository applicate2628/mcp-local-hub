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
