// internal/gui/csrf_test.go
package gui

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestRequireSameOrigin_AllowsSecFetchSiteSameOrigin(t *testing.T) {
	s := NewServer(Config{})
	s.migrator = &fakeMigrator{}
	req := httptest.NewRequest(http.MethodPost, "/api/migrate", bytes.NewReader([]byte(`{"servers":[]}`)))
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Errorf("status = %d, want 204 (allowed)", rec.Code)
	}
}

func TestRequireSameOrigin_AllowsSecFetchSiteNone(t *testing.T) {
	// "none" = user-initiated navigation, never cross-origin
	s := NewServer(Config{})
	s.migrator = &fakeMigrator{}
	req := httptest.NewRequest(http.MethodPost, "/api/migrate", bytes.NewReader([]byte(`{"servers":[]}`)))
	req.Header.Set("Sec-Fetch-Site", "none")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Errorf("status = %d, want 204", rec.Code)
	}
}

func TestRequireSameOrigin_BlocksCrossOrigin(t *testing.T) {
	s := NewServer(Config{})
	s.migrator = &fakeMigrator{}
	req := httptest.NewRequest(http.MethodPost, "/api/migrate", bytes.NewReader([]byte(`{"servers":[]}`)))
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403", rec.Code)
	}
}

func TestRequireSameOrigin_BlocksMismatchedOrigin(t *testing.T) {
	s := NewServer(Config{})
	s.migrator = &fakeMigrator{}
	req := httptest.NewRequest(http.MethodPost, "/api/migrate", bytes.NewReader([]byte(`{"servers":[]}`)))
	req.Header.Set("Origin", "http://evil.example.com")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403", rec.Code)
	}
}

func TestRequireSameOrigin_AllowsEmptyOrigin(t *testing.T) {
	// curl / native clients send no Origin header — should be allowed.
	s := NewServer(Config{})
	s.migrator = &fakeMigrator{}
	req := httptest.NewRequest(http.MethodPost, "/api/migrate", bytes.NewReader([]byte(`{"servers":[]}`)))
	req.Header.Set("Content-Type", "application/json")
	// No Sec-Fetch-Site, no Origin
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Errorf("status = %d, want 204 (curl allowed)", rec.Code)
	}
}
