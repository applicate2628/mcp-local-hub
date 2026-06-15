package gui

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"mcp-local-hub/internal/api"
)

// fakeMarketplaceLister is a Server-local test double for the
// marketplaceLister interface. It records the call and returns the
// canned entries/err so the GET /api/marketplace handler is exercised
// without a live network fetch against the real registry.
type fakeMarketplaceLister struct {
	called  bool
	entries []api.MarketplaceEntry
	err     error
}

func (f *fakeMarketplaceLister) MarketplaceEntries(_ context.Context) ([]api.MarketplaceEntry, error) {
	f.called = true
	return f.entries, f.err
}

// fakeMarketplaceRefresher is a Server-local test double for the
// marketplaceRefresher interface backing POST /api/marketplace/refresh.
// It records the call and returns the canned entries/err so the
// force-refresh handler is exercised without a live network fetch.
type fakeMarketplaceRefresher struct {
	called  bool
	entries []api.MarketplaceEntry
	err     error
}

func (f *fakeMarketplaceRefresher) RefreshMarketplaceEntries(_ context.Context) ([]api.MarketplaceEntry, error) {
	f.called = true
	return f.entries, f.err
}

// newMarketplaceTestServer wires only the marketplaceLister subset. Unlike
// the catalog handler (registered inside the shared registerManifestRoutes),
// registerMarketplaceRoutes touches ONLY s.marketplaceLister, so no other
// fakes are needed to avoid a nil-deref.
func newMarketplaceTestServer(m *fakeMarketplaceLister) *Server {
	s := &Server{mux: http.NewServeMux(), marketplaceLister: m}
	registerMarketplaceRoutes(s)
	return s
}

// newMarketplaceRefreshTestServer wires only the marketplaceRefresher subset.
// registerMarketplaceRoutes also wires GET /api/marketplace (which reads
// s.marketplaceLister), so a fake lister is supplied to avoid a nil-deref —
// but the POST /api/marketplace/refresh handler under test touches ONLY
// s.marketplaceRefresher.
func newMarketplaceRefreshTestServer(r *fakeMarketplaceRefresher) *Server {
	s := &Server{mux: http.NewServeMux(), marketplaceLister: &fakeMarketplaceLister{}, marketplaceRefresher: r}
	registerMarketplaceRoutes(s)
	return s
}

func postMarketplaceRefresh(t *testing.T, s *Server, origin string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/marketplace/refresh", nil)
	req.Header.Set("Sec-Fetch-Site", origin)
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)
	return rec
}

func getMarketplace(t *testing.T, s *Server, origin string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/marketplace", nil)
	req.Header.Set("Sec-Fetch-Site", origin)
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)
	return rec
}

func TestMarketplaceHandler_ReturnsEntries(t *testing.T) {
	m := &fakeMarketplaceLister{entries: []api.MarketplaceEntry{
		{
			ID:         "filesystem",
			Name:       "Filesystem",
			Summary:    "Read/write files within allowed roots.",
			Categories: []string{"files", "core"},
			Homepage:   "https://example.com/filesystem",
			// Fields below MUST NOT leak into the read-only browse DTO.
			Transport: "stdio",
			Command:   "npx",
			Args:      []string{"-y", "@modelcontextprotocol/server-filesystem"},
		},
		{ID: "git", Name: "Git", Summary: "Git repository tooling.", Transport: "stdio", Command: "uvx"},
	}}
	s := newMarketplaceTestServer(m)
	rec := getMarketplace(t, s, "same-origin")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%q", rec.Code, rec.Body.String())
	}
	if !m.called {
		t.Error("MarketplaceEntries was not called")
	}
	var body marketplaceListResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(body.Entries) != 2 {
		t.Fatalf("entries len = %d, want 2: %+v", len(body.Entries), body.Entries)
	}
	e0 := body.Entries[0]
	if e0.ID != "filesystem" || e0.Name != "Filesystem" || e0.Summary != "Read/write files within allowed roots." {
		t.Errorf("entry[0] = %+v", e0)
	}
	if len(e0.Categories) != 2 || e0.Categories[0] != "files" {
		t.Errorf("entry[0].Categories = %v", e0.Categories)
	}
	if e0.Homepage != "https://example.com/filesystem" {
		t.Errorf("entry[0].Homepage = %q", e0.Homepage)
	}
	// `transport` IS part of the browse wire shape now (the one-click-install
	// frontend reads it to pick hub vs direct mode), so it must be present.
	if e0.Transport != "stdio" {
		t.Errorf("entry[0].Transport = %q, want stdio", e0.Transport)
	}
	// The HEAVIER install-only fields (command/args/url/env) MUST still NOT
	// appear in the read-only browse wire shape — POST
	// /api/marketplace/install re-loads the full entry server-side instead.
	if strings.Contains(rec.Body.String(), "command") ||
		strings.Contains(rec.Body.String(), "server-filesystem") {
		t.Errorf("read-only DTO leaked heavy install fields: %q", rec.Body.String())
	}
}

func TestMarketplaceHandler_EmptyIsNonNullArray_200(t *testing.T) {
	// A populated-but-empty catalog (or first-run) is 200 {"entries":[]}.
	s := newMarketplaceTestServer(&fakeMarketplaceLister{entries: nil})
	rec := getMarketplace(t, s, "same-origin")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `"entries":[]`) {
		t.Errorf("empty marketplace must serialize as []: %q", rec.Body.String())
	}
}

func TestMarketplaceHandler_FetchError_BestEffortEmpty200(t *testing.T) {
	// Best-effort contract: a fetch/cache failure NEVER 500s. The handler
	// logs the error and returns 200 {"entries":[]} so a transient
	// registry outage cannot break the Catalog screen.
	m := &fakeMarketplaceLister{err: errors.New("fetch catalog: dial tcp: i/o timeout")}
	s := newMarketplaceTestServer(m)
	rec := getMarketplace(t, s, "same-origin")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (best-effort), body=%q", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"entries":[]`) {
		t.Errorf("fetch error must degrade to empty []: %q", rec.Body.String())
	}
	// The raw error MUST NOT leak into the body (it's logged server-side).
	if strings.Contains(rec.Body.String(), "i/o timeout") {
		t.Errorf("body leaks fetch error: %q", rec.Body.String())
	}
}

func TestMarketplaceHandler_NilCategoriesNormalizedToArray(t *testing.T) {
	// An entry with no categories must serialize as [] (never null) so the
	// frontend can map without a guard.
	m := &fakeMarketplaceLister{entries: []api.MarketplaceEntry{
		{ID: "time", Name: "Time", Summary: "Clock + timezone helpers.", Categories: nil},
	}}
	s := newMarketplaceTestServer(m)
	rec := getMarketplace(t, s, "same-origin")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%q", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"categories":[]`) {
		t.Errorf("nil categories must serialize as []: %q", rec.Body.String())
	}
}

func TestMarketplaceHandler_RejectsNonGET(t *testing.T) {
	s := newMarketplaceTestServer(&fakeMarketplaceLister{})
	req := httptest.NewRequest(http.MethodPost, "/api/marketplace", nil)
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", rec.Code)
	}
	if got := rec.Header().Get("Allow"); got != "GET" {
		t.Errorf("Allow header = %q, want GET", got)
	}
}

func TestMarketplaceHandler_RejectsCrossOrigin(t *testing.T) {
	s := newMarketplaceTestServer(&fakeMarketplaceLister{})
	rec := getMarketplace(t, s, "cross-site")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
}

func TestMarketplaceRefreshHandler_InvokesForceRefreshAndReturnsEntries(t *testing.T) {
	r := &fakeMarketplaceRefresher{entries: []api.MarketplaceEntry{
		{
			ID:         "filesystem",
			Name:       "Filesystem",
			Summary:    "Read/write files within allowed roots.",
			Categories: []string{"files", "core"},
			Homepage:   "https://example.com/filesystem",
			// Install-only fields MUST NOT leak into the refreshed browse DTO.
			Transport: "stdio",
			Command:   "npx",
			Args:      []string{"-y", "@modelcontextprotocol/server-filesystem"},
		},
		{ID: "git", Name: "Git", Summary: "Git repository tooling.", Transport: "stdio", Command: "uvx"},
	}}
	s := newMarketplaceRefreshTestServer(r)
	rec := postMarketplaceRefresh(t, s, "same-origin")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%q", rec.Code, rec.Body.String())
	}
	if !r.called {
		t.Error("RefreshMarketplaceEntries (force-refresh path) was not invoked")
	}
	// Identical body shape to GET /api/marketplace.
	var body marketplaceListResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(body.Entries) != 2 {
		t.Fatalf("entries len = %d, want 2: %+v", len(body.Entries), body.Entries)
	}
	e0 := body.Entries[0]
	if e0.ID != "filesystem" || e0.Name != "Filesystem" || e0.Summary != "Read/write files within allowed roots." {
		t.Errorf("entry[0] = %+v", e0)
	}
	if len(e0.Categories) != 2 || e0.Categories[0] != "files" {
		t.Errorf("entry[0].Categories = %v", e0.Categories)
	}
	if e0.Homepage != "https://example.com/filesystem" {
		t.Errorf("entry[0].Homepage = %q", e0.Homepage)
	}
	// `transport` is part of the refreshed wire shape too, matching GET
	// /api/marketplace so one-click install decisions can use either response.
	if e0.Transport != "stdio" {
		t.Errorf("entry[0].Transport = %q, want stdio", e0.Transport)
	}
	// The heavier install-only fields MUST NOT appear in the refreshed wire shape.
	if strings.Contains(rec.Body.String(), "command") ||
		strings.Contains(rec.Body.String(), "server-filesystem") {
		t.Errorf("refresh DTO leaked install fields: %q", rec.Body.String())
	}
}

func TestMarketplaceRefreshHandler_NilCategoriesNormalizedToArray(t *testing.T) {
	r := &fakeMarketplaceRefresher{entries: []api.MarketplaceEntry{
		{ID: "time", Name: "Time", Summary: "Clock + timezone helpers.", Categories: nil},
	}}
	s := newMarketplaceRefreshTestServer(r)
	rec := postMarketplaceRefresh(t, s, "same-origin")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%q", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"categories":[]`) {
		t.Errorf("nil categories must serialize as []: %q", rec.Body.String())
	}
}

func TestMarketplaceRefreshHandler_RefreshError500(t *testing.T) {
	// POST /api/marketplace/refresh is an explicit operator-triggered
	// force-refresh; unlike the best-effort GET, a force-refresh failure
	// is surfaced as an error so the operator knows the re-fetch did not
	// happen (the cache was NOT updated).
	r := &fakeMarketplaceRefresher{err: errors.New("fetch catalog: dial tcp: i/o timeout")}
	s := newMarketplaceRefreshTestServer(r)
	rec := postMarketplaceRefresh(t, s, "same-origin")
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500, body=%q", rec.Code, rec.Body.String())
	}
	// The raw error MUST NOT leak into the body.
	if strings.Contains(rec.Body.String(), "i/o timeout") {
		t.Errorf("body leaks refresh error: %q", rec.Body.String())
	}
}

func TestMarketplaceRefreshHandler_RejectsNonPOST(t *testing.T) {
	s := newMarketplaceRefreshTestServer(&fakeMarketplaceRefresher{})
	req := httptest.NewRequest(http.MethodGet, "/api/marketplace/refresh", nil)
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", rec.Code)
	}
	if got := rec.Header().Get("Allow"); got != "POST" {
		t.Errorf("Allow header = %q, want POST", got)
	}
}

func TestMarketplaceRefreshHandler_RejectsCrossOrigin(t *testing.T) {
	s := newMarketplaceRefreshTestServer(&fakeMarketplaceRefresher{})
	rec := postMarketplaceRefresh(t, s, "cross-site")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
}
