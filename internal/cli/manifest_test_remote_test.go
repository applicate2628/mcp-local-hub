package cli

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"mcp-local-hub/internal/api"
)

func writeRemoteManifest(t *testing.T, dir, name, body string) {
	t.Helper()
	sub := filepath.Join(dir, name)
	if err := os.MkdirAll(sub, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sub, "manifest.yaml"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestManifestTestRemoteCmd_PrintsResult(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":"2025-11-25","capabilities":{"tools":{}},"serverInfo":{"name":"test-srv","version":"0.0.1"}}}`))
	}))
	defer srv.Close()
	t.Cleanup(api.InstallTestRemoteTestClientForCLI(srv))
	dir := t.TempDir()
	t.Setenv("MCPHUB_MANIFEST_DIR_OVERRIDE", dir)
	writeRemoteManifest(t, dir, "remote", "name: remote\nkind: global\ntransport: remote-http\nurl: "+srv.URL+"\nclient_bindings:\n  - client: claude-code\n")

	c := newManifestCmdReal()
	var stdout, stderr bytes.Buffer
	c.SetOut(&stdout)
	c.SetErr(&stderr)
	c.SetArgs([]string{"test-remote", "remote"})
	if err := c.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("test-remote: %v\nstderr: %s", err, stderr.String())
	}
	out := stdout.String()
	for _, want := range []string{"remote reachable", "protocolVersion: 2025-11-25", "test-srv 0.0.1", "[tools]"} {
		if !strings.Contains(out, want) {
			t.Errorf("stdout missing %q\n---\n%s", want, out)
		}
	}
}

func TestManifestTestRemoteCmd_UpstreamErrorToStderr(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte("bad token"))
	}))
	defer srv.Close()
	t.Cleanup(api.InstallTestRemoteTestClientForCLI(srv))
	dir := t.TempDir()
	t.Setenv("MCPHUB_MANIFEST_DIR_OVERRIDE", dir)
	writeRemoteManifest(t, dir, "remote", "name: remote\nkind: global\ntransport: remote-http\nurl: "+srv.URL+"\nclient_bindings:\n  - client: claude-code\n")

	c := newManifestCmdReal()
	c.SilenceUsage = true
	c.SilenceErrors = true
	var stdout, stderr bytes.Buffer
	c.SetOut(&stdout)
	c.SetErr(&stderr)
	c.SetArgs([]string{"test-remote", "remote"})
	err := c.ExecuteContext(context.Background())
	if err == nil {
		t.Fatalf("expected error, stdout=%q", stdout.String())
	}
	if !strings.Contains(err.Error(), "401") {
		t.Errorf("error should surface upstream status: %v", err)
	}
	if stdout.Len() > 0 {
		t.Errorf("stdout should be empty on failure, got %q", stdout.String())
	}
}

func TestManifestTestRemoteCmd_TransportGate(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("MCPHUB_MANIFEST_DIR_OVERRIDE", dir)
	writeRemoteManifest(t, dir, "local", "name: local\nkind: global\ntransport: stdio-bridge\ncommand: echo\nbase_args: [\"hi\"]\ndaemons:\n  - name: default\n    port: 9999\nclient_bindings:\n  - client: claude-code\n    daemon: default\n")
	c := newManifestCmdReal()
	c.SilenceUsage = true
	c.SilenceErrors = true
	var stdout, stderr bytes.Buffer
	c.SetOut(&stdout)
	c.SetErr(&stderr)
	c.SetArgs([]string{"test-remote", "local"})
	err := c.ExecuteContext(context.Background())
	if err == nil {
		t.Fatalf("expected transport rejection")
	}
	if !strings.Contains(err.Error(), "remote-http") {
		t.Errorf("error should name expected transport: %v", err)
	}
}
