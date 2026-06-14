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

// newMarketplaceTestServer wires only the marketplaceLister subset. Unlike
// the catalog handler (registered inside the shared registerManifestRoutes),
// registerMarketplaceRoutes touches ONLY s.marketplaceLister, so no other
// fakes are needed to avoid a nil-deref.
func newMarketplaceTestServer(m *fakeMarketplaceLister) *Server {
	s := &Server{mux: http.NewServeMux(), marketplaceLister: m}
	registerMarketplaceRoutes(s)
	return s
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
	// The install-only fields (transport/command/args) MUST NOT appear in
	// the read-only browse wire shape — generation is a CLI flow.
	if strings.Contains(rec.Body.String(), "transport") ||
		strings.Contains(rec.Body.String(), "command") ||
		strings.Contains(rec.Body.String(), "server-filesystem") {
		t.Errorf("read-only DTO leaked install fields: %q", rec.Body.String())
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
