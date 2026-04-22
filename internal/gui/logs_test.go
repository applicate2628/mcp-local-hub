package gui

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type fakeLogs struct {
	body string
	err  error
}

func (f fakeLogs) Logs(server string, tail int) (string, error) {
	return f.body, f.err
}

func TestLogs_GetReturnsText(t *testing.T) {
	s := NewServer(Config{})
	s.logs = fakeLogs{body: "line1\nline2\n"}
	req := httptest.NewRequest(http.MethodGet, "/api/logs/memory?tail=100", nil)
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("status = %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "line1") {
		t.Errorf("body = %q", rec.Body.String())
	}
}
