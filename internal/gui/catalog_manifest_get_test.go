package gui

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"mcp-local-hub/internal/api"
)

// fakeCatalogManifestGetter is the Server-local test double for the
// catalogManifestGetter subset backing GET /api/catalog/manifest (D2 r2).
type fakeCatalogManifestGetter struct {
	called   bool
	seenName string
	out      string
	err      error
}

func (f *fakeCatalogManifestGetter) CatalogManifestGet(name string) (string, error) {
	f.called = true
	f.seenName = name
	return f.out, f.err
}

// newCatalogManifestGetTestServer wires the catalog-manifest-getter subset plus
// the create/validate/get/edit/list/delete/catalog fakes the shared
// registerManifestRoutes references at registration time (so a stray request
// can't nil-deref). Only the /api/catalog/manifest handler is exercised here.
func newCatalogManifestGetTestServer(getter *fakeCatalogManifestGetter) *Server {
	s := &Server{
		mux:                   http.NewServeMux(),
		manifestCreator:       &fakeManifestCreator{},
		manifestValidator:     &fakeManifestValidator{},
		manifestGetter:        &fakeManifestGetter{},
		manifestEditor:        &fakeManifestEditor{},
		manifestLister:        &fakeManifestLister{},
		manifestDeleter:       &fakeManifestDeleter{},
		catalogLister:         &fakeCatalogLister{},
		catalogManifestGetter: getter,
	}
	registerManifestRoutes(s)
	return s
}

func TestCatalogManifestGetHandler_Returns200WithYAML(t *testing.T) {
	getter := &fakeCatalogManifestGetter{
		// The embed YAML carries a `secret:` ref, never a literal — the
		// secret-safe source the prefill relies on.
		out: "name: 'wolfram'\nkind: global\ntransport: stdio-bridge\ncommand: 'node'\n" +
			"env:\n  WOLFRAM_LLM_APP_ID: 'secret:wolfram_app_id'\n",
	}
	s := newCatalogManifestGetTestServer(getter)
	req := httptest.NewRequest(http.MethodGet, "/api/catalog/manifest?name=wolfram", nil)
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%q", rec.Code, rec.Body.String())
	}
	if !getter.called || getter.seenName != "wolfram" {
		t.Fatalf("getter called=%v seenName=%q, want called for wolfram", getter.called, getter.seenName)
	}
	var body catalogManifestGetResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !strings.Contains(body.YAML, "secret:wolfram_app_id") {
		t.Errorf("response yaml missing secret ref: %q", body.YAML)
	}
}

func TestCatalogManifestGetHandler_NotEmbedded_404(t *testing.T) {
	// The membership-gate miss surfaces as ErrManifestNotEmbedded, which the
	// handler maps to a stable, path-free 404 the frontend turns into the
	// name-only "isn't in the catalog" seed.
	getter := &fakeCatalogManifestGetter{err: api.ErrManifestNotEmbedded}
	s := newCatalogManifestGetTestServer(getter)
	req := httptest.NewRequest(http.MethodGet, "/api/catalog/manifest?name=customsrv", nil)
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body=%q", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "CATALOG_MANIFEST_NOT_FOUND") {
		t.Errorf("body=%q missing CATALOG_MANIFEST_NOT_FOUND code", body)
	}
}

func TestCatalogManifestGetHandler_RejectsNonGET(t *testing.T) {
	getter := &fakeCatalogManifestGetter{}
	s := newCatalogManifestGetTestServer(getter)
	req := httptest.NewRequest(http.MethodPost, "/api/catalog/manifest?name=wolfram", nil)
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", rec.Code)
	}
	if got := rec.Header().Get("Allow"); got != "GET" {
		t.Errorf("Allow header = %q, want GET", got)
	}
	if getter.called {
		t.Error("getter must NOT be called on a method-rejected request")
	}
}

func TestCatalogManifestGetHandler_EmptyName_400(t *testing.T) {
	getter := &fakeCatalogManifestGetter{}
	s := newCatalogManifestGetTestServer(getter)
	// Trailing whitespace name trims to empty → 400 before the getter runs.
	req := httptest.NewRequest(http.MethodGet, "/api/catalog/manifest?name=%20%20", nil)
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%q", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "BAD_REQUEST") {
		t.Errorf("body=%q missing BAD_REQUEST code", rec.Body.String())
	}
	if getter.called {
		t.Error("getter must NOT be called for an empty name")
	}
}

func TestCatalogManifestGetHandler_BackendError_500Sanitized(t *testing.T) {
	// A non-ErrManifestNotEmbedded error (e.g. a corrupt-embed *os.PathError)
	// must be sanitized — the client never sees the filesystem path.
	getter := &fakeCatalogManifestGetter{
		err: errors.New("open /home/alice/.local/share/mcphub/servers/wolfram/manifest.yaml: permission denied"),
	}
	s := newCatalogManifestGetTestServer(getter)
	req := httptest.NewRequest(http.MethodGet, "/api/catalog/manifest?name=wolfram", nil)
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	body := rec.Body.String()
	if strings.Contains(body, "/home/alice") || strings.Contains(body, "permission denied") {
		t.Errorf("body leaks filesystem path or raw error: %q", body)
	}
	if !strings.Contains(body, "CATALOG_MANIFEST_GET_FAILED") {
		t.Errorf("body=%q missing CATALOG_MANIFEST_GET_FAILED code", body)
	}
}

func TestCatalogManifestGetHandler_RejectsCrossOrigin(t *testing.T) {
	getter := &fakeCatalogManifestGetter{}
	s := newCatalogManifestGetTestServer(getter)
	req := httptest.NewRequest(http.MethodGet, "/api/catalog/manifest?name=wolfram", nil)
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
	if getter.called {
		t.Error("getter must NOT be called on a CSRF-rejected request")
	}
}
