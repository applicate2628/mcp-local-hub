package gui

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"mcp-local-hub/internal/api"
)

// fakeMarketplaceLister is a Server-local test double for the
// marketplaceLister interface. It records the call and returns the
// canned entries/err so the GET /api/marketplace handler is exercised
// without a live network fetch against the real registry.
type fakeMarketplaceLister struct {
	called   bool
	entries  []api.MarketplaceEntry
	docsOnly []api.DocsOnlyEntry
	err      error
}

func (f *fakeMarketplaceLister) MarketplaceEntries(_ context.Context) ([]api.MarketplaceEntry, []api.DocsOnlyEntry, error) {
	f.called = true
	return f.entries, f.docsOnly, f.err
}

// fakeMarketplaceRefresher is a Server-local test double for the
// marketplaceRefresher interface backing POST /api/marketplace/refresh.
// It records the call and returns the canned entries/err so the
// force-refresh handler is exercised without a live network fetch.
type fakeMarketplaceRefresher struct {
	called   bool
	entries  []api.MarketplaceEntry
	docsOnly []api.DocsOnlyEntry
	err      error
}

func (f *fakeMarketplaceRefresher) RefreshMarketplaceEntries(_ context.Context) ([]api.MarketplaceEntry, []api.DocsOnlyEntry, error) {
	f.called = true
	return f.entries, f.docsOnly, f.err
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

// TestMarketplaceHandler_ProjectsLiveProbeVerdict is the FINDING-1 (D-3
// mirror-gate) regression: the browse DTO must carry the LIVE host-probe verdict
// (probe_passes) — the same verdict the backend install gate reaches — not just
// the static availability field. Without it, an inert (watch) row whose host app
// is NOW detected stays greyed in the GUI even though the backend would admit the
// install POST, so a now-ready row can never be installed from Catalog.
//
//   - ready/empty row                          → probe_passes=true (installable)
//   - inert row, probe PASSES (present binary)  → probe_passes=true (NOW installable)
//   - inert row, probe FAILS (absent binary)    → probe_passes=false (still greyed)
//
// "go" is guaranteed present in the test toolchain (the same anchor
// availability_probe_test.go uses), so the inert-passing row is deterministic.
func TestMarketplaceHandler_ProjectsLiveProbeVerdict(t *testing.T) {
	m := &fakeMarketplaceLister{entries: []api.MarketplaceEntry{
		// (a) ordinary ready row — no availability, no probe.
		{ID: "ready-row", Name: "Ready", Transport: "stdio", Command: "npx"},
		// (b) inert row whose probe PASSES on this host RIGHT NOW: the host app
		// ("go") is detected, so the backend gate would admit it. The DTO must
		// report probe_passes=true so the GUI exposes the install affordance.
		{
			ID: "now-ready-watch", Name: "Now Ready", Transport: "stdio", Command: "npx",
			Availability: "watch",
			InstallProbe: &api.CatalogAvailabilityProbe{Binaries: []string{"go"}},
		},
		// (c) inert row whose probe FAILS: the host app is absent, so the row
		// stays greyed. probe_passes=false.
		{
			ID: "still-inert-watch", Name: "Still Inert", Transport: "stdio", Command: "npx",
			Availability: "disabled-until-probe",
			InstallProbe: &api.CatalogAvailabilityProbe{Binaries: []string{"definitely-not-on-path-xyz"}},
		},
	}}
	s := newMarketplaceTestServer(m)
	rec := getMarketplace(t, s, "same-origin")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%q", rec.Code, rec.Body.String())
	}
	// The wire body MUST carry the probe_passes key (proves the field exists on
	// the DTO, not just the Go struct).
	if !strings.Contains(rec.Body.String(), `"probe_passes"`) {
		t.Fatalf("DTO body missing probe_passes key: %s", rec.Body.String())
	}
	var body marketplaceListResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	byID := map[string]marketplaceEntry{}
	for _, e := range body.Entries {
		byID[e.ID] = e
	}
	if e := byID["ready-row"]; !e.ProbePasses {
		t.Errorf("ready row: probe_passes=%v, want true (always installable)", e.ProbePasses)
	}
	// The load-bearing claim: a now-ready inert row is reported installable so the
	// GUI (isInert = inert-availability AND NOT probe_passes) shows the buttons.
	nowReady := byID["now-ready-watch"]
	if nowReady.Availability != "watch" {
		t.Errorf("now-ready row availability = %q, want watch", nowReady.Availability)
	}
	if !nowReady.ProbePasses {
		t.Errorf("now-ready inert row: probe_passes=false, want true — a detected host app must be installable from Catalog")
	}
	if e := byID["still-inert-watch"]; e.ProbePasses {
		t.Errorf("still-inert row (absent host app): probe_passes=true, want false")
	}
}

// TestMarketplaceHandler_BrowseDoesNotStatFileProbe is the FINDING-1 (codex
// catalog r4 P2) regression: the browse projection (GET /api/marketplace) must
// NOT os.Stat a catalog-supplied files[] probe while merely serving the list — a
// slow automount or UNC path would otherwise stall opening/refreshing the Catalog
// and touch an external location before the operator chooses install. We prove
// browse is file-presence-INDEPENDENT: a row whose file probe points at a PRESENT
// marker and a row pointing at an ABSENT marker both serialize probe_passes=false
// (browse-unknown / greyed). If the browse path stat'd the file, the present-file
// row would read true and diverge. The real file probe runs only at install
// admission (api.AvailabilityAdmissionEntry), so the now-ready file-probe row is
// still installable once the operator clicks install.
func TestMarketplaceHandler_BrowseDoesNotStatFileProbe(t *testing.T) {
	dir := t.TempDir()
	present := filepath.Join(dir, "installed.marker")
	if err := os.WriteFile(present, []byte("x"), 0o644); err != nil {
		t.Fatalf("write marker: %v", err)
	}
	absent := filepath.Join(dir, "not-installed.marker")

	m := &fakeMarketplaceLister{entries: []api.MarketplaceEntry{
		{
			ID: "file-probe-present", Name: "Present", Transport: "stdio", Command: "npx",
			Availability: "watch",
			InstallProbe: &api.CatalogAvailabilityProbe{Files: []string{present}},
		},
		{
			ID: "file-probe-absent", Name: "Absent", Transport: "stdio", Command: "npx",
			Availability: "watch",
			InstallProbe: &api.CatalogAvailabilityProbe{Files: []string{absent}},
		},
	}}
	s := newMarketplaceTestServer(m)
	rec := getMarketplace(t, s, "same-origin")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%q", rec.Code, rec.Body.String())
	}
	var body marketplaceListResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	byID := map[string]marketplaceEntry{}
	for _, e := range body.Entries {
		byID[e.ID] = e
	}
	// Both file-probe rows are browse-unknown (greyed) regardless of file presence
	// — the browse projection never stat'd either path.
	if byID["file-probe-present"].ProbePasses {
		t.Errorf("file-probe row with a PRESENT marker reported probe_passes=true — the browse projection must not stat the file")
	}
	if byID["file-probe-absent"].ProbePasses {
		t.Errorf("file-probe row with an ABSENT marker reported probe_passes=true; want false")
	}
}
