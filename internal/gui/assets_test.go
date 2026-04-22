package gui

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

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
	body := rec.Body.String()
	for _, want := range []string{"<!doctype html>", "mcp-local-hub", "data-screen=\"servers\"", "data-screen=\"dashboard\"", "data-screen=\"logs\""} {
		if !strings.Contains(strings.ToLower(body), strings.ToLower(want)) {
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
