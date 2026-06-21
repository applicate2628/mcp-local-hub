package cli

import (
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"mcp-local-hub/internal/gui"
)

func TestResolveInstallGUIPortReturnsZeroForMissingOrStalePidport(t *testing.T) {
	root := t.TempDir()
	t.Setenv("LOCALAPPDATA", root)
	t.Setenv("XDG_STATE_HOME", root)
	t.Setenv("HOME", root)

	if got := resolveInstallGUIPort(); got != 0 {
		t.Fatalf("resolveInstallGUIPort() with no pidport = %d, want 0", got)
	}

	appDir := filepath.Join(root, "mcp-local-hub")
	if err := os.MkdirAll(appDir, 0o700); err != nil {
		t.Fatalf("mkdir app dir: %v", err)
	}
	pidport := filepath.Join(appDir, "gui.pidport")
	if err := gui.WritePidport(pidport, os.Getpid(), 9125); err != nil {
		t.Fatalf("write pidport: %v", err)
	}

	if got := resolveInstallGUIPort(); got != 0 {
		t.Fatalf("resolveInstallGUIPort() with stale pidport = %d, want 0", got)
	}
}

func TestResolveInstallGUIPortReturnsLivePidportPort(t *testing.T) {
	root := t.TempDir()
	t.Setenv("LOCALAPPDATA", root)
	t.Setenv("XDG_STATE_HOME", root)
	t.Setenv("HOME", root)

	appDir := filepath.Join(root, "mcp-local-hub")
	if err := os.MkdirAll(appDir, 0o700); err != nil {
		t.Fatalf("mkdir app dir: %v", err)
	}
	pidport := filepath.Join(appDir, "gui.pidport")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/ping" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "pid": os.Getpid()})
	}))
	defer srv.Close()
	port := installTestPortFromURL(t, srv.URL)
	if err := gui.WritePidport(pidport, os.Getpid(), port); err != nil {
		t.Fatalf("write pidport: %v", err)
	}

	if got := resolveInstallGUIPort(); got != port {
		t.Fatalf("resolveInstallGUIPort() = %d, want %d", got, port)
	}
}

func installTestPortFromURL(t *testing.T, rawURL string) int {
	t.Helper()
	u, err := url.Parse(rawURL)
	if err != nil {
		t.Fatalf("parse test server URL %q: %v", rawURL, err)
	}
	_, rawPort, err := net.SplitHostPort(u.Host)
	if err != nil {
		t.Fatalf("split test server host %q: %v", u.Host, err)
	}
	port, err := strconv.Atoi(rawPort)
	if err != nil {
		t.Fatalf("parse test server port %q: %v", rawPort, err)
	}
	return port
}
