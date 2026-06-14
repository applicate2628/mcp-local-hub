package gui

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"mcp-local-hub/internal/api"
	"mcp-local-hub/internal/config"
)

// fakeManifestCreator and fakeManifestValidator are Server-local test doubles.
// They shadow the real api.API calls via the interfaces injected into Server.
type fakeManifestCreator struct {
	name string
	yaml string
	err  error
}

func (f *fakeManifestCreator) ManifestCreate(name, yaml string) error {
	f.name, f.yaml = name, yaml
	return f.err
}

type fakeManifestValidator struct {
	lastYAML string
	out      []string
}

func (f *fakeManifestValidator) ManifestValidate(yaml string) []string {
	f.lastYAML = yaml
	return f.out
}

func newManifestTestServer(create *fakeManifestCreator, validate *fakeManifestValidator) *Server {
	s := &Server{mux: http.NewServeMux(), manifestCreator: create, manifestValidator: validate}
	registerManifestRoutes(s)
	return s
}

func postJSON(t *testing.T, s *Server, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)
	return rec
}

// ---- /api/manifest/create ----

func TestManifestCreateHandler_RejectsNonPOST(t *testing.T) {
	s := newManifestTestServer(&fakeManifestCreator{}, &fakeManifestValidator{})
	req := httptest.NewRequest(http.MethodGet, "/api/manifest/create", nil)
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", rec.Code)
	}
}

func TestManifestCreateHandler_RejectsCrossOrigin(t *testing.T) {
	s := newManifestTestServer(&fakeManifestCreator{}, &fakeManifestValidator{})
	req := httptest.NewRequest(http.MethodPost, "/api/manifest/create",
		bytes.NewBufferString(`{"name":"x","yaml":"name: x"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
}

func TestManifestCreateHandler_ForwardsNameAndYAML(t *testing.T) {
	create := &fakeManifestCreator{}
	s := newManifestTestServer(create, &fakeManifestValidator{})
	rec := postJSON(t, s, "/api/manifest/create",
		`{"name":"demo","yaml":"name: demo\nkind: global\n"}`)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, body=%q", rec.Code, rec.Body.String())
	}
	if create.name != "demo" || create.yaml != "name: demo\nkind: global\n" {
		t.Errorf("got name=%q yaml=%q", create.name, create.yaml)
	}
}

func TestManifestCreateHandler_SurfacesCreateError(t *testing.T) {
	// After Codex R1 P2 sanitization, the 500 body contains a stable
	// "internal error ..." message and the MANIFEST_CREATE_FAILED code,
	// NOT the raw backend error text. The real error is logged server-side.
	// TestManifestCreateHandler_500DoesNotLeakErrorDetails covers the leak-
	// prevention invariant specifically.
	create := &fakeManifestCreator{err: errors.New("manifest \"demo\" already exists at ...")}
	s := newManifestTestServer(create, &fakeManifestValidator{})
	rec := postJSON(t, s, "/api/manifest/create", `{"name":"demo","yaml":"name: demo"}`)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "MANIFEST_CREATE_FAILED") {
		t.Errorf("body=%q missing error code", body)
	}
	if !strings.Contains(body, "internal error") {
		t.Errorf("body=%q missing sanitized message", body)
	}
}

func TestManifestCreateHandler_RejectsBadJSON(t *testing.T) {
	s := newManifestTestServer(&fakeManifestCreator{}, &fakeManifestValidator{})
	rec := postJSON(t, s, "/api/manifest/create", `{not-json`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

func TestManifestCreateHandler_RejectsEmptyName(t *testing.T) {
	s := newManifestTestServer(&fakeManifestCreator{}, &fakeManifestValidator{})
	rec := postJSON(t, s, "/api/manifest/create", `{"name":"   ","yaml":"name: x"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

// ---- /api/manifest/validate ----

func TestManifestValidateHandler_RejectsNonPOST(t *testing.T) {
	s := newManifestTestServer(&fakeManifestCreator{}, &fakeManifestValidator{})
	req := httptest.NewRequest(http.MethodGet, "/api/manifest/validate", nil)
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", rec.Code)
	}
}

func TestManifestValidateHandler_ReturnsWarnings(t *testing.T) {
	validate := &fakeManifestValidator{out: []string{"no daemons declared"}}
	s := newManifestTestServer(&fakeManifestCreator{}, validate)
	rec := postJSON(t, s, "/api/manifest/validate", `{"yaml":"name: demo"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var body struct {
		Warnings []string `json:"warnings"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(body.Warnings) != 1 || body.Warnings[0] != "no daemons declared" {
		t.Errorf("warnings = %v", body.Warnings)
	}
	if validate.lastYAML != "name: demo" {
		t.Errorf("validator saw yaml=%q", validate.lastYAML)
	}
}

func TestManifestValidateHandler_EmptyWarningsIsNonNullArray(t *testing.T) {
	validate := &fakeManifestValidator{out: nil}
	s := newManifestTestServer(&fakeManifestCreator{}, validate)
	rec := postJSON(t, s, "/api/manifest/validate", `{"yaml":"name: demo\nkind: global\n"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	// Body must contain "warnings":[] — a JSON null would force frontend
	// to special-case the response.
	if !strings.Contains(rec.Body.String(), `"warnings":[]`) {
		t.Errorf("body=%q missing warnings:[] shape", rec.Body.String())
	}
}

type fakeManifestGetter struct {
	yaml string
	hash string
	err  error
}

func (f *fakeManifestGetter) ManifestGetWithHash(name string) (string, string, error) {
	return f.yaml, f.hash, f.err
}

type fakeManifestEditor struct {
	seenName         string
	seenYAML         string
	seenExpectedHash string
	returnHash       string // injected to assert handler forwards backend's hash into response body
	err              error
}

func (f *fakeManifestEditor) ManifestEditWithHash(name, yaml, expectedHash string) (string, error) {
	f.seenName, f.seenYAML, f.seenExpectedHash = name, yaml, expectedHash
	return f.returnHash, f.err
}

func newManifestTestServerFull(
	create *fakeManifestCreator,
	validate *fakeManifestValidator,
	getter *fakeManifestGetter,
	editor *fakeManifestEditor,
) *Server {
	s := &Server{
		mux:               http.NewServeMux(),
		manifestCreator:   create,
		manifestValidator: validate,
		manifestGetter:    getter,
		manifestEditor:    editor,
	}
	registerManifestRoutes(s)
	return s
}

// ---- /api/manifest/get ----

func TestManifestGetHandler_RejectsNonGET(t *testing.T) {
	s := newManifestTestServerFull(&fakeManifestCreator{}, &fakeManifestValidator{},
		&fakeManifestGetter{yaml: "name: x", hash: "h"}, &fakeManifestEditor{})
	req := httptest.NewRequest(http.MethodPost, "/api/manifest/get?name=x", nil)
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", rec.Code)
	}
}

func TestManifestGetHandler_ReturnsYAMLAndHash(t *testing.T) {
	getter := &fakeManifestGetter{yaml: "name: demo\n", hash: "abc123"}
	s := newManifestTestServerFull(&fakeManifestCreator{}, &fakeManifestValidator{},
		getter, &fakeManifestEditor{})
	req := httptest.NewRequest(http.MethodGet, "/api/manifest/get?name=demo", nil)
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	var body manifestGetResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if body.YAML != "name: demo\n" || body.Hash != "abc123" {
		t.Errorf("body = %+v", body)
	}
}

func TestManifestGetHandler_EmptyName_400(t *testing.T) {
	s := newManifestTestServerFull(&fakeManifestCreator{}, &fakeManifestValidator{},
		&fakeManifestGetter{}, &fakeManifestEditor{})
	req := httptest.NewRequest(http.MethodGet, "/api/manifest/get?name=", nil)
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

// ---- /api/manifest/edit (Appendix P1-3: 200 + {hash} instead of 204) ----

func TestManifestEditHandler_ForwardsNameYAMLAndHash(t *testing.T) {
	editor := &fakeManifestEditor{returnHash: "new-hash-xyz"}
	s := newManifestTestServerFull(&fakeManifestCreator{}, &fakeManifestValidator{},
		&fakeManifestGetter{}, editor)
	rec := postJSON(t, s, "/api/manifest/edit",
		`{"name":"demo","yaml":"name: demo\nkind: global\n","expected_hash":"abc"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%q", rec.Code, rec.Body.String())
	}
	if editor.seenName != "demo" || editor.seenYAML != "name: demo\nkind: global\n" || editor.seenExpectedHash != "abc" {
		t.Errorf("got name=%q yaml=%q hash=%q", editor.seenName, editor.seenYAML, editor.seenExpectedHash)
	}
	var body manifestEditResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if body.Hash != "new-hash-xyz" {
		t.Errorf("body.Hash = %q, want %q", body.Hash, "new-hash-xyz")
	}
}

func TestManifestEditHandler_HashMismatch_Returns409(t *testing.T) {
	editor := &fakeManifestEditor{err: api.ErrManifestHashMismatch}
	s := newManifestTestServerFull(&fakeManifestCreator{}, &fakeManifestValidator{},
		&fakeManifestGetter{}, editor)
	rec := postJSON(t, s, "/api/manifest/edit",
		`{"name":"demo","yaml":"name: demo","expected_hash":"stale"}`)
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "MANIFEST_HASH_MISMATCH") {
		t.Errorf("body missing error code: %q", rec.Body.String())
	}
}

func TestManifestEditHandler_OtherError_Returns500(t *testing.T) {
	editor := &fakeManifestEditor{err: errors.New("disk full")}
	s := newManifestTestServerFull(&fakeManifestCreator{}, &fakeManifestValidator{},
		&fakeManifestGetter{}, editor)
	rec := postJSON(t, s, "/api/manifest/edit",
		`{"name":"demo","yaml":"name: demo","expected_hash":""}`)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d", rec.Code)
	}
}

// Codex R1 (#16 P2): 500 responses must not echo raw err.Error() because
// backend errors on disk-bound calls wrap *os.PathError and leak absolute
// filesystem paths. Verify the three handlers all sanitize before responding.

func TestManifestGetHandler_500DoesNotLeakErrorDetails(t *testing.T) {
	leaky := errors.New("open /home/alice/.secret/manifests/demo/manifest.yaml: permission denied")
	getter := &fakeManifestGetter{err: leaky}
	s := newManifestTestServerFull(&fakeManifestCreator{}, &fakeManifestValidator{},
		getter, &fakeManifestEditor{})
	req := httptest.NewRequest(http.MethodGet, "/api/manifest/get?name=demo", nil)
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	body := rec.Body.String()
	if strings.Contains(body, "/home/alice") || strings.Contains(body, "permission denied") {
		t.Errorf("response body leaks filesystem path or raw error: %q", body)
	}
	if !strings.Contains(body, "internal error") {
		t.Errorf("response body missing generic message: %q", body)
	}
}

func TestManifestEditHandler_500DoesNotLeakErrorDetails(t *testing.T) {
	leaky := errors.New("atomic rename: rename /var/lib/mcphub/servers/demo/manifest.yaml.tmp: no space left on device")
	editor := &fakeManifestEditor{err: leaky}
	s := newManifestTestServerFull(&fakeManifestCreator{}, &fakeManifestValidator{},
		&fakeManifestGetter{}, editor)
	rec := postJSON(t, s, "/api/manifest/edit",
		`{"name":"demo","yaml":"name: demo","expected_hash":""}`)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	body := rec.Body.String()
	if strings.Contains(body, "/var/lib") || strings.Contains(body, "no space left") {
		t.Errorf("response body leaks filesystem path or raw error: %q", body)
	}
	if !strings.Contains(body, "internal error") {
		t.Errorf("response body missing generic message: %q", body)
	}
}

func TestManifestEditHandler_HashMismatch_PassesThrough(t *testing.T) {
	// Hash-mismatch is a legitimate client-facing signal with a generic
	// message; the sanitization must NOT apply to it.
	editor := &fakeManifestEditor{err: api.ErrManifestHashMismatch}
	s := newManifestTestServerFull(&fakeManifestCreator{}, &fakeManifestValidator{},
		&fakeManifestGetter{}, editor)
	rec := postJSON(t, s, "/api/manifest/edit",
		`{"name":"demo","yaml":"name: demo","expected_hash":"stale"}`)
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "MANIFEST_HASH_MISMATCH") {
		t.Errorf("hash-mismatch code missing from body: %q", rec.Body.String())
	}
}

func TestManifestCreateHandler_500DoesNotLeakErrorDetails(t *testing.T) {
	leaky := errors.New("mkdir /etc/secret/servers/demo: permission denied")
	creator := &fakeManifestCreator{err: leaky}
	s := newManifestTestServerFull(creator, &fakeManifestValidator{},
		&fakeManifestGetter{}, &fakeManifestEditor{})
	rec := postJSON(t, s, "/api/manifest/create",
		`{"name":"demo","yaml":"name: demo"}`)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	body := rec.Body.String()
	if strings.Contains(body, "/etc/secret") || strings.Contains(body, "permission denied") {
		t.Errorf("response body leaks filesystem path or raw error: %q", body)
	}
	if !strings.Contains(body, "internal error") {
		t.Errorf("response body missing generic message: %q", body)
	}
}

// Codex R7 (a2b-combined-pr-followups item #3): three coverage gaps on
// /api/manifest/edit that parallel the already-covered get/create handlers.

func TestManifestEditHandler_EmptyName_400(t *testing.T) {
	s := newManifestTestServerFull(&fakeManifestCreator{}, &fakeManifestValidator{},
		&fakeManifestGetter{}, &fakeManifestEditor{})
	rec := postJSON(t, s, "/api/manifest/edit",
		`{"name":"","yaml":"name: demo","expected_hash":""}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "BAD_REQUEST") {
		t.Errorf("body=%q missing BAD_REQUEST code", rec.Body.String())
	}
}

func TestManifestEditHandler_MalformedJSON_400(t *testing.T) {
	s := newManifestTestServerFull(&fakeManifestCreator{}, &fakeManifestValidator{},
		&fakeManifestGetter{}, &fakeManifestEditor{})
	rec := postJSON(t, s, "/api/manifest/edit", `{not-json`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "BAD_REQUEST") {
		t.Errorf("body=%q missing BAD_REQUEST code", rec.Body.String())
	}
}

func TestManifestEditHandler_RejectsNonPOST_405(t *testing.T) {
	s := newManifestTestServerFull(&fakeManifestCreator{}, &fakeManifestValidator{},
		&fakeManifestGetter{}, &fakeManifestEditor{})
	req := httptest.NewRequest(http.MethodGet, "/api/manifest/edit", nil)
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

// ---- GET /api/manifests + DELETE /api/manifest/:name ----

type fakeManifestLister struct {
	called bool
	out    []string
	err    error
}

func (f *fakeManifestLister) ManifestList() ([]string, error) {
	f.called = true
	return f.out, f.err
}

type fakeManifestDeleter struct {
	called   bool
	seenName string
	err      error
}

func (f *fakeManifestDeleter) ManifestDelete(name string) error {
	f.called = true
	f.seenName = name
	return f.err
}

type fakeCatalogLister struct {
	called bool
	out    []config.CatalogFields
	err    error
}

func (f *fakeCatalogLister) CatalogList() ([]config.CatalogFields, error) {
	f.called = true
	return f.out, f.err
}

// newManifestListDeleteTestServer wires only the list + delete subsets plus
// the four create/validate/get/edit fakes the shared registerManifestRoutes
// references at registration time. The create/validate/get/edit handlers are
// never exercised here, but registerManifestRoutes runs all of them, so the
// fields must be non-nil to avoid a nil dereference if a stray request hits.
func newManifestListDeleteTestServer(lister *fakeManifestLister, deleter *fakeManifestDeleter) *Server {
	s := &Server{
		mux:               http.NewServeMux(),
		manifestCreator:   &fakeManifestCreator{},
		manifestValidator: &fakeManifestValidator{},
		manifestGetter:    &fakeManifestGetter{},
		manifestEditor:    &fakeManifestEditor{},
		manifestLister:    lister,
		manifestDeleter:   deleter,
		catalogLister:     &fakeCatalogLister{},
	}
	registerManifestRoutes(s)
	return s
}

// newCatalogTestServer wires the catalog subset plus the create/validate/
// get/edit/list/delete fakes the shared registerManifestRoutes references
// at registration time (so a stray request can't nil-deref). Only the
// /api/catalog handler is exercised here.
func newCatalogTestServer(catalog *fakeCatalogLister) *Server {
	s := &Server{
		mux:               http.NewServeMux(),
		manifestCreator:   &fakeManifestCreator{},
		manifestValidator: &fakeManifestValidator{},
		manifestGetter:    &fakeManifestGetter{},
		manifestEditor:    &fakeManifestEditor{},
		manifestLister:    &fakeManifestLister{},
		manifestDeleter:   &fakeManifestDeleter{},
		catalogLister:     catalog,
	}
	registerManifestRoutes(s)
	return s
}

func TestManifestListHandler_RejectsNonGET(t *testing.T) {
	s := newManifestListDeleteTestServer(&fakeManifestLister{}, &fakeManifestDeleter{})
	req := httptest.NewRequest(http.MethodPost, "/api/manifests", nil)
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

func TestManifestListHandler_RejectsCrossOrigin(t *testing.T) {
	s := newManifestListDeleteTestServer(&fakeManifestLister{}, &fakeManifestDeleter{})
	req := httptest.NewRequest(http.MethodGet, "/api/manifests", nil)
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
}

func TestManifestListHandler_ReturnsNames(t *testing.T) {
	lister := &fakeManifestLister{out: []string{"memory", "time", "serena"}}
	s := newManifestListDeleteTestServer(lister, &fakeManifestDeleter{})
	req := httptest.NewRequest(http.MethodGet, "/api/manifests", nil)
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%q", rec.Code, rec.Body.String())
	}
	var body manifestListResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(body.Manifests) != 3 || body.Manifests[0] != "memory" {
		t.Errorf("manifests = %v", body.Manifests)
	}
}

func TestManifestListHandler_EmptyIsNonNullArray_200(t *testing.T) {
	// Empty set is 200 [] (NOT 404) — "no manifests yet" is a normal state.
	lister := &fakeManifestLister{out: nil}
	s := newManifestListDeleteTestServer(lister, &fakeManifestDeleter{})
	req := httptest.NewRequest(http.MethodGet, "/api/manifests", nil)
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `"manifests":[]`) {
		t.Errorf("empty list must serialize as []: %q", rec.Body.String())
	}
}

func TestManifestListHandler_BackendError_500Sanitized(t *testing.T) {
	lister := &fakeManifestLister{err: errors.New("readdir /home/alice/.local/share/mcphub/servers: permission denied")}
	s := newManifestListDeleteTestServer(lister, &fakeManifestDeleter{})
	req := httptest.NewRequest(http.MethodGet, "/api/manifests", nil)
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
	if !strings.Contains(body, "MANIFEST_LIST_FAILED") {
		t.Errorf("body=%q missing MANIFEST_LIST_FAILED code", body)
	}
}

// ---- GET /api/catalog (§10 v2a — Catalog descriptions) ----

func TestCatalogHandler_ReturnsEnrichedEntries(t *testing.T) {
	catalog := &fakeCatalogLister{out: []config.CatalogFields{
		{Name: "serena", Description: "Semantic code toolkit.", Kind: "global"},
		{Name: "mcp-language-server", Description: "Language-server backend.", Kind: "workspace-scoped"},
	}}
	s := newCatalogTestServer(catalog)
	req := httptest.NewRequest(http.MethodGet, "/api/catalog", nil)
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%q", rec.Code, rec.Body.String())
	}
	if !catalog.called {
		t.Error("CatalogList was not called")
	}
	var body catalogListResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(body.Catalog) != 2 {
		t.Fatalf("catalog len = %d, want 2: %+v", len(body.Catalog), body.Catalog)
	}
	if body.Catalog[0].Name != "serena" || body.Catalog[0].Description != "Semantic code toolkit." || body.Catalog[0].Kind != "global" {
		t.Errorf("entry[0] = %+v", body.Catalog[0])
	}
	if body.Catalog[1].Kind != "workspace-scoped" {
		t.Errorf("entry[1].Kind = %q, want workspace-scoped", body.Catalog[1].Kind)
	}
}

func TestCatalogHandler_EmptyIsNonNullArray_200(t *testing.T) {
	// Empty set is 200 {"catalog":[]} (NOT 404) — "no servers yet" is normal.
	s := newCatalogTestServer(&fakeCatalogLister{out: nil})
	req := httptest.NewRequest(http.MethodGet, "/api/catalog", nil)
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `"catalog":[]`) {
		t.Errorf("empty catalog must serialize as []: %q", rec.Body.String())
	}
}

func TestCatalogHandler_RejectsNonGET(t *testing.T) {
	s := newCatalogTestServer(&fakeCatalogLister{})
	req := httptest.NewRequest(http.MethodPost, "/api/catalog", nil)
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

func TestCatalogHandler_RejectsCrossOrigin(t *testing.T) {
	s := newCatalogTestServer(&fakeCatalogLister{})
	req := httptest.NewRequest(http.MethodGet, "/api/catalog", nil)
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
}

func TestCatalogHandler_BackendError_500Sanitized(t *testing.T) {
	catalog := &fakeCatalogLister{err: errors.New("readdir /home/alice/.local/share/mcphub/servers: permission denied")}
	s := newCatalogTestServer(catalog)
	req := httptest.NewRequest(http.MethodGet, "/api/catalog", nil)
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
	if !strings.Contains(body, "CATALOG_LIST_FAILED") {
		t.Errorf("body=%q missing CATALOG_LIST_FAILED code", body)
	}
}

func TestManifestDeleteHandler_RejectsNonDELETE(t *testing.T) {
	deleter := &fakeManifestDeleter{}
	s := newManifestListDeleteTestServer(&fakeManifestLister{}, deleter)
	req := httptest.NewRequest(http.MethodGet, "/api/manifest/demo", nil)
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", rec.Code)
	}
	if got := rec.Header().Get("Allow"); got != "DELETE" {
		t.Errorf("Allow header = %q, want DELETE", got)
	}
	if deleter.called {
		t.Error("deleter must NOT be called on a method-rejected request")
	}
}

func TestManifestDeleteHandler_RejectsCrossOrigin(t *testing.T) {
	deleter := &fakeManifestDeleter{}
	s := newManifestListDeleteTestServer(&fakeManifestLister{}, deleter)
	req := httptest.NewRequest(http.MethodDelete, "/api/manifest/demo", nil)
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
	if deleter.called {
		t.Error("deleter must NOT be called on a CSRF-rejected request")
	}
}

func TestManifestDeleteHandler_Success_204AndForwardsName(t *testing.T) {
	deleter := &fakeManifestDeleter{}
	s := newManifestListDeleteTestServer(&fakeManifestLister{}, deleter)
	req := httptest.NewRequest(http.MethodDelete, "/api/manifest/demo", nil)
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204, body=%q", rec.Code, rec.Body.String())
	}
	if !deleter.called || deleter.seenName != "demo" {
		t.Fatalf("deleter saw name=%q called=%v, want demo/true", deleter.seenName, deleter.called)
	}
}

func TestManifestDeleteHandler_EmptyName_400(t *testing.T) {
	deleter := &fakeManifestDeleter{}
	s := newManifestListDeleteTestServer(&fakeManifestLister{}, deleter)
	req := httptest.NewRequest(http.MethodDelete, "/api/manifest/", nil)
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	if deleter.called {
		t.Error("deleter must NOT be called when no name is given")
	}
}

func TestManifestDeleteHandler_NotFound_404(t *testing.T) {
	deleter := &fakeManifestDeleter{err: errors.New(`manifest "ghost" does not exist`)}
	s := newManifestListDeleteTestServer(&fakeManifestLister{}, deleter)
	req := httptest.NewRequest(http.MethodDelete, "/api/manifest/ghost", nil)
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404, body=%q", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "MANIFEST_NOT_FOUND") {
		t.Errorf("body=%q missing MANIFEST_NOT_FOUND code", rec.Body.String())
	}
}

func TestManifestDeleteHandler_BackendError_500Sanitized(t *testing.T) {
	deleter := &fakeManifestDeleter{err: errors.New("remove /var/lib/mcphub/servers/demo: directory not empty")}
	s := newManifestListDeleteTestServer(&fakeManifestLister{}, deleter)
	req := httptest.NewRequest(http.MethodDelete, "/api/manifest/demo", nil)
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	body := rec.Body.String()
	if strings.Contains(body, "/var/lib") || strings.Contains(body, "directory not empty") {
		t.Errorf("body leaks filesystem path or raw error: %q", body)
	}
	if !strings.Contains(body, "MANIFEST_DELETE_FAILED") {
		t.Errorf("body=%q missing MANIFEST_DELETE_FAILED code", body)
	}
}

// Coexistence guard: the /api/manifest/ subtree DELETE handler must NOT
// shadow the exact-path create/validate/get/edit handlers. A POST to
// /api/manifest/create still routes to the create handler (which forwards
// to the create fake), not the delete subtree (which would 405 on POST).
func TestManifestDeleteSubtree_DoesNotShadowExactPaths(t *testing.T) {
	create := &fakeManifestCreator{}
	deleter := &fakeManifestDeleter{}
	s := &Server{
		mux:               http.NewServeMux(),
		manifestCreator:   create,
		manifestValidator: &fakeManifestValidator{},
		manifestGetter:    &fakeManifestGetter{},
		manifestEditor:    &fakeManifestEditor{},
		manifestLister:    &fakeManifestLister{},
		manifestDeleter:   deleter,
	}
	registerManifestRoutes(s)
	rec := postJSON(t, s, "/api/manifest/create",
		`{"name":"demo","yaml":"name: demo\nkind: global\n"}`)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("POST /api/manifest/create status = %d, want 204 (exact path must win)", rec.Code)
	}
	if create.name != "demo" {
		t.Error("create handler should have run (recorded name), not the delete subtree")
	}
	if deleter.called {
		t.Error("delete subtree must NOT have intercepted POST /api/manifest/create")
	}
}
