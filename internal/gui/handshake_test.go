package gui

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestHandshake_PingOKThenActivate(t *testing.T) {
	activated := make(chan struct{}, 1)
	mux := http.NewServeMux()
	mux.HandleFunc("/api/ping", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true,"pid":111,"version":"t"}`))
	})
	mux.HandleFunc("/api/activate-window", func(w http.ResponseWriter, r *http.Request) {
		activated <- struct{}{}
		w.WriteHeader(http.StatusNoContent)
	})
	ts := httptest.NewServer(mux)
	defer ts.Close()

	port := parseTestServerPort(t, ts.URL)
	dir := t.TempDir()
	pidport := filepath.Join(dir, "gui.pidport")
	if err := os.WriteFile(pidport, []byte(formatPidport(111, port)), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := TryActivateIncumbent(pidport, 2*time.Second); err != nil {
		t.Fatalf("TryActivateIncumbent: %v", err)
	}
	select {
	case <-activated:
	case <-time.After(1 * time.Second):
		t.Fatal("incumbent never received activate")
	}
}

func TestHandshake_ConnectionRefusedReturnsError(t *testing.T) {
	dir := t.TempDir()
	pidport := filepath.Join(dir, "gui.pidport")
	if err := os.WriteFile(pidport, []byte(formatPidport(99999, 1)), 0o600); err != nil {
		t.Fatal(err)
	}
	err := TryActivateIncumbent(pidport, 500*time.Millisecond)
	if err == nil {
		t.Fatal("expected error on unreachable incumbent")
	}
}

// TestHandshake_ReReadsPidportDuringStartupWindow simulates the race the
// round-3 fix introduced: the incumbent writes pidport with port=0 before
// bind, then rewrites it with the OS-assigned port once Server.Start
// resolves the ephemeral port. A second instance that reads pidport only
// once sees port=0 and probes 127.0.0.1:0 for the entire timeout. Polling
// inside the retry loop must catch the RewritePidportPort update and
// reach the now-live incumbent.
func TestHandshake_ReReadsPidportDuringStartupWindow(t *testing.T) {
	dir := t.TempDir()
	pidport := filepath.Join(dir, "gui.pidport")
	// Initial pidport: port=0 (incumbent in pre-bind state).
	if err := os.WriteFile(pidport, []byte(formatPidport(111, 0)), 0o600); err != nil {
		t.Fatal(err)
	}

	// Spin up an httptest server that becomes "live" after a delay.
	activated := make(chan struct{}, 1)
	mux := http.NewServeMux()
	mux.HandleFunc("/api/ping", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true,"pid":111,"version":"t"}`))
	})
	mux.HandleFunc("/api/activate-window", func(w http.ResponseWriter, r *http.Request) {
		activated <- struct{}{}
		w.WriteHeader(http.StatusNoContent)
	})
	ts := httptest.NewServer(mux)
	defer ts.Close()
	port := parseTestServerPort(t, ts.URL)

	// Simulate the incumbent's RewritePidportPort happening 400ms after
	// the second instance starts probing.
	go func() {
		time.Sleep(400 * time.Millisecond)
		_ = os.WriteFile(pidport, []byte(formatPidport(111, port)), 0o600)
	}()

	if err := TryActivateIncumbent(pidport, 3*time.Second); err != nil {
		t.Fatalf("TryActivateIncumbent: %v", err)
	}
	select {
	case <-activated:
	case <-time.After(1 * time.Second):
		t.Fatal("incumbent never received activate (re-read pidport probably missing)")
	}
}

// parseTestServerPort extracts the port from an httptest.Server URL
// (always "http://127.0.0.1:<port>").
func parseTestServerPort(t *testing.T, url string) int {
	t.Helper()
	i := strings.LastIndex(url, ":")
	p, err := strconv.Atoi(url[i+1:])
	if err != nil {
		t.Fatalf("parseTestServerPort %q: %v", url, err)
	}
	return p
}
