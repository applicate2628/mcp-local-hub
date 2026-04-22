package gui

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
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

// TestHandshake_RetriesOnNonJSONPing pins the R21 P2 fix: a non-JSON
// /api/ping response (e.g. an HTML body from a transient non-mcphub
// listener squatting on the pidport's port during the startup race)
// must NOT be treated as a terminal "not-ok" reply. Before the fix,
// decErr was ignored and body.OK defaulted to false, so the very first
// undecodable response killed the handshake even though a later retry
// could have reached the real mcphub gui incumbent.
//
// This test stages a server that returns garbage HTML for the first two
// ping attempts and valid mcphub JSON on the third, then asserts the
// handshake eventually lands the /api/activate-window call and that
// at least three /api/ping attempts were observed. If the decode-error
// path ever re-regresses to an immediate return, TryActivateIncumbent
// would surface an "incumbent ping replied not-ok" error on the first
// tick and this test would fail fast.
func TestHandshake_RetriesOnNonJSONPing(t *testing.T) {
	dir := t.TempDir()
	pidport := filepath.Join(dir, "gui.pidport")

	var attempts int32
	activated := make(chan struct{}, 1)
	mux := http.NewServeMux()
	mux.HandleFunc("/api/ping", func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&attempts, 1)
		if n < 3 {
			w.Header().Set("Content-Type", "text/html")
			_, _ = w.Write([]byte("<html>not mcphub</html>"))
			return
		}
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
	if err := os.WriteFile(pidport, []byte(formatPidport(111, port)), 0o600); err != nil {
		t.Fatal(err)
	}

	// 3s total budget covers the 250ms retry spacing × ≥3 attempts with
	// comfortable margin on slow CI. A passing run completes as soon as
	// the third ping returns valid JSON and activate-window fires.
	if err := TryActivateIncumbent(pidport, 3*time.Second); err != nil {
		t.Fatalf("TryActivateIncumbent: %v", err)
	}
	select {
	case <-activated:
	case <-time.After(1 * time.Second):
		t.Fatal("incumbent never received activate")
	}
	if got := atomic.LoadInt32(&attempts); got < 3 {
		t.Errorf("expected at least 3 ping attempts (2 non-JSON + 1 JSON); got %d", got)
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
