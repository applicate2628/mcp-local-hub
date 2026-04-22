package gui

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type fakeLogs struct {
	body      string
	err       error
	gotServer string
	gotDaemon string
}

func (f *fakeLogs) Logs(server, daemon string, tail int) (string, error) {
	f.gotServer = server
	f.gotDaemon = daemon
	return f.body, f.err
}

func TestLogs_GetReturnsText(t *testing.T) {
	s := NewServer(Config{})
	s.logs = &fakeLogs{body: "line1\nline2\n"}
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

// TestLogs_DaemonQueryParamForwarded confirms that ?daemon= on
// /api/logs/:server reaches the logsProvider. Multi-daemon servers
// (serena: claude + codex) depend on this — without it they get the
// adapter's default="default" fallback and see empty logs.
func TestLogs_DaemonQueryParamForwarded(t *testing.T) {
	fl := &fakeLogs{body: "x"}
	s := NewServer(Config{})
	s.logs = fl
	req := httptest.NewRequest(http.MethodGet, "/api/logs/serena?tail=10&daemon=claude", nil)
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("status = %d", rec.Code)
	}
	if fl.gotDaemon != "claude" {
		t.Errorf("daemon param not forwarded: got %q want claude", fl.gotDaemon)
	}
}
