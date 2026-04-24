package gui

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
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
	create := &fakeManifestCreator{err: errors.New("manifest \"demo\" already exists at ...")}
	s := newManifestTestServer(create, &fakeManifestValidator{})
	rec := postJSON(t, s, "/api/manifest/create", `{"name":"demo","yaml":"name: demo"}`)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "already exists") {
		t.Errorf("body=%q missing backend error text", rec.Body.String())
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
