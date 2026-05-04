// internal/gui/csrf_test.go
package gui

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRequireSameOrigin_AllowsSecFetchSiteSameOrigin(t *testing.T) {
	s := NewServer(Config{Port: 9081})
	s.migrator = &fakeMigrator{}
	req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:9081/api/migrate", bytes.NewReader([]byte(`{"servers":[]}`)))
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.httpHandler().ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Errorf("status = %d, want 204 (allowed)", rec.Code)
	}
}

func TestRequireSameOrigin_AllowsSecFetchSiteNone(t *testing.T) {
	// "none" = user-initiated navigation, never cross-origin
	s := NewServer(Config{Port: 9081})
	s.migrator = &fakeMigrator{}
	req := httptest.NewRequest(http.MethodPost, "http://localhost:9081/api/migrate", bytes.NewReader([]byte(`{"servers":[]}`)))
	req.Header.Set("Sec-Fetch-Site", "none")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.httpHandler().ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Errorf("status = %d, want 204", rec.Code)
	}
}

func TestRequireSameOrigin_BlocksCrossOrigin(t *testing.T) {
	s := NewServer(Config{Port: 9081})
	s.migrator = &fakeMigrator{}
	req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:9081/api/migrate", bytes.NewReader([]byte(`{"servers":[]}`)))
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.httpHandler().ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403", rec.Code)
	}
}

func TestRequireSameOrigin_BlocksMismatchedOrigin(t *testing.T) {
	s := NewServer(Config{Port: 9081})
	s.migrator = &fakeMigrator{}
	req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:9081/api/migrate", bytes.NewReader([]byte(`{"servers":[]}`)))
	req.Header.Set("Origin", "http://evil.example.com")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.httpHandler().ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403", rec.Code)
	}
}

func TestRequireSameOrigin_BlocksMismatchedOriginEvenWithSameOriginFetchSite(t *testing.T) {
	s := NewServer(Config{Port: 9081})
	s.migrator = &fakeMigrator{}
	req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:9081/api/migrate", bytes.NewReader([]byte(`{"servers":[]}`)))
	req.Header.Set("Origin", "http://evil.example.com")
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.httpHandler().ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403", rec.Code)
	}
}

func TestRequireSameOrigin_AllowsEmptyOrigin(t *testing.T) {
	// curl / native clients send no Origin header — should be allowed.
	s := NewServer(Config{Port: 9081})
	s.migrator = &fakeMigrator{}
	req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:9081/api/migrate", bytes.NewReader([]byte(`{"servers":[]}`)))
	req.Header.Set("Content-Type", "application/json")
	// No Sec-Fetch-Site, no Origin
	rec := httptest.NewRecorder()
	s.httpHandler().ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Errorf("status = %d, want 204 (curl allowed)", rec.Code)
	}
}

func TestRequireAllowedHost_BlocksReadEndpointDNSRebinding(t *testing.T) {
	s := NewServer(Config{Port: 9081})
	req := httptest.NewRequest(http.MethodGet, "http://evil.example.com:9081/api/status", nil)
	rec := httptest.NewRecorder()
	s.httpHandler().ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403", rec.Code)
	}
}

func TestRequireAllowedHost_AllowsLoopbackReadEndpoint(t *testing.T) {
	s := NewServer(Config{Port: 9081})
	req := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:9081/api/ping", nil)
	rec := httptest.NewRecorder()
	s.httpHandler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
}

// TestRequireSameOrigin_RejectsViteOriginWithLoopbackHost is the
// regression guard for the Vite dev workflow. When `vite.config.ts`
// uses `changeOrigin: true` WITHOUT the configure-hook Origin rewrite,
// the proxy rewrites Host to `127.0.0.1:<port>` so requireAllowedHost
// passes, but the Origin header still carries the Vite dev server's
// origin (`http://localhost:5173`). The strict requireSameOrigin must
// reject that case so a misconfigured proxy fails closed instead of
// silently bypassing CSRF.
func TestRequireSameOrigin_RejectsViteOriginWithLoopbackHost(t *testing.T) {
	s := NewServer(Config{Port: 9081})
	s.migrator = &fakeMigrator{}
	req := httptest.NewRequest(http.MethodPost, "/api/migrate", bytes.NewReader([]byte(`{"servers":[]}`)))
	req.Host = fmt.Sprintf("127.0.0.1:%d", 9081)
	req.Header.Set("Origin", "http://localhost:5173")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.httpHandler().ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (Vite Origin without configure hook MUST be rejected)", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "CROSS_ORIGIN") {
		t.Errorf("body = %q, want CROSS_ORIGIN code", body)
	}
}

// TestRequireSameOrigin_AcceptsViteOriginRewrittenByConfigureHook is
// the positive complement of the previous test. When `vite.config.ts`
// also includes the configure hook that rewrites Origin to the
// backend's loopback origin, the strict guard accepts the proxied
// request and the dev workflow keeps working end-to-end.
func TestRequireSameOrigin_AcceptsViteOriginRewrittenByConfigureHook(t *testing.T) {
	s := NewServer(Config{Port: 9081})
	s.migrator = &fakeMigrator{}
	req := httptest.NewRequest(http.MethodPost, "/api/migrate", bytes.NewReader([]byte(`{"servers":[]}`)))
	req.Host = fmt.Sprintf("127.0.0.1:%d", 9081)
	req.Header.Set("Origin", fmt.Sprintf("http://127.0.0.1:%d", 9081))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.httpHandler().ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204 (configure-hook-rewritten Origin must be accepted)", rec.Code)
	}
}
