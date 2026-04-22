// internal/gui/servers_test.go
package gui

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

type fakeRestart struct {
	called string
	err    error
}

func (f *fakeRestart) Restart(server string) error {
	f.called = server
	return f.err
}

func TestRestartServer_InvokesAPI(t *testing.T) {
	fr := &fakeRestart{}
	s := NewServer(Config{})
	s.restart = fr
	req := httptest.NewRequest(http.MethodPost, "/api/servers/memory/restart", nil)
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d body=%q", rec.Code, rec.Body.String())
	}
	if fr.called != "memory" {
		t.Errorf("Restart called with %q, want memory", fr.called)
	}
}
