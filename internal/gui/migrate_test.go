// internal/gui/migrate_test.go
package gui

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type fakeMigrator struct {
	called []string
	err    error
}

func (f *fakeMigrator) Migrate(servers []string) error {
	f.called = servers
	return f.err
}

func TestMigrate_CallsAPIWithServerList(t *testing.T) {
	fm := &fakeMigrator{}
	s := NewServer(Config{})
	s.migrator = fm

	req := httptest.NewRequest(http.MethodPost, "/api/migrate",
		bytes.NewReader([]byte(`{"servers":["memory","wolfram"]}`)))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, body=%q", rec.Code, rec.Body.String())
	}
	if strings.Join(fm.called, ",") != "memory,wolfram" {
		t.Errorf("Migrate called with %v", fm.called)
	}
}

func TestMigrate_BadMethodReturns405(t *testing.T) {
	s := NewServer(Config{})
	req := httptest.NewRequest(http.MethodGet, "/api/migrate", nil)
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("status = %d", rec.Code)
	}
}
