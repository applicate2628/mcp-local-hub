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
	calledServers []string
	calledClients []string
	err           error
}

func (f *fakeMigrator) Migrate(servers, clients []string) error {
	f.calledServers = servers
	f.calledClients = clients
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
	if strings.Join(fm.calledServers, ",") != "memory,wolfram" {
		t.Errorf("Migrate called with servers=%v", fm.calledServers)
	}
	// Clients field is optional; omitting it preserves the original
	// "all bound clients" behavior, which surfaces as an empty slice.
	if len(fm.calledClients) != 0 {
		t.Errorf("Migrate called with clients=%v, want empty", fm.calledClients)
	}
}

// TestMigrate_ForwardsClientsSubset guards the per-cell Apply path: when the
// GUI toggles a single (server, client) cell, the request body carries both
// a servers list and a clients list, and the handler must forward clients
// into the migrator so ClientsInclude narrows the rewrite — otherwise one
// flipped checkbox silently rewrites every client binding for that server.
func TestMigrate_ForwardsClientsSubset(t *testing.T) {
	fm := &fakeMigrator{}
	s := NewServer(Config{})
	s.migrator = fm

	req := httptest.NewRequest(http.MethodPost, "/api/migrate",
		bytes.NewReader([]byte(`{"servers":["memory"],"clients":["claude-code"]}`)))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, body=%q", rec.Code, rec.Body.String())
	}
	if strings.Join(fm.calledServers, ",") != "memory" {
		t.Errorf("Migrate called with servers=%v", fm.calledServers)
	}
	if strings.Join(fm.calledClients, ",") != "claude-code" {
		t.Errorf("Migrate called with clients=%v, want [claude-code]", fm.calledClients)
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
