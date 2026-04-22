package gui

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestIndexHTML_ServedAtRoot verifies that GET "/" returns the Vite-
// generated index.html. Post-D0 the page no longer includes the old
// data-screen nav anchors (routing moved into app.js); the stable
// invariants are the doctype, the <title>, the #app mount point, and
// the bundle references.
func TestIndexHTML_ServedAtRoot(t *testing.T) {
	s := NewServer(Config{})
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("status = %d", rec.Code)
	}
	ct := rec.Header().Get("Content-Type")
	if !strings.HasPrefix(ct, "text/html") {
		t.Errorf("content-type = %q", ct)
	}
	body := strings.ToLower(rec.Body.String())
	for _, want := range []string{
		"<!doctype html>",
		"mcp-local-hub",
		`id="app"`,
		`/assets/app.js`,
		`/assets/style.css`,
	} {
		if !strings.Contains(body, strings.ToLower(want)) {
			t.Errorf("index.html missing %q", want)
		}
	}
}

func TestStaticAsset_StyleCSS(t *testing.T) {
	s := NewServer(Config{})
	req := httptest.NewRequest(http.MethodGet, "/assets/style.css", nil)
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("status = %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/css") {
		t.Errorf("content-type = %q", ct)
	}
}

// TestStaticAsset_AppJS covers the Vite-generated bundle. Before D0 the
// bundle was split across four script tags; now there is exactly one
// entry bundle at /assets/app.js and this test catches any Vite config
// drift that would rename it.
func TestStaticAsset_AppJS(t *testing.T) {
	s := NewServer(Config{})
	req := httptest.NewRequest(http.MethodGet, "/assets/app.js", nil)
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("status = %d", rec.Code)
	}
	ct := rec.Header().Get("Content-Type")
	// Go's FileServer mime-sniffs .js -> text/javascript on most runtimes,
	// or application/javascript on older stdlibs. Accept either.
	if !strings.HasPrefix(ct, "text/javascript") && !strings.HasPrefix(ct, "application/javascript") {
		t.Errorf("content-type = %q, want javascript MIME", ct)
	}
}
