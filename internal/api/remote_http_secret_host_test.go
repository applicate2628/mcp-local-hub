package api

import (
	"context"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"mcp-local-hub/internal/config"
	"mcp-local-hub/internal/secrets"
)

func seedDefaultSecretForTest(t *testing.T, key, value string) {
	t.Helper()
	root := t.TempDir()
	t.Setenv("LOCALAPPDATA", root)
	t.Setenv("XDG_DATA_HOME", root)

	dataDir := secrets.UserDataDir()
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		t.Fatalf("create test secret dir: %v", err)
	}
	keyPath := secrets.DefaultKeyPath()
	vaultPath := secrets.DefaultVaultPath()
	if err := os.MkdirAll(filepath.Dir(keyPath), 0o700); err != nil {
		t.Fatalf("create key parent: %v", err)
	}
	if err := secrets.InitVault(keyPath, vaultPath); err != nil {
		t.Fatalf("init test vault: %v", err)
	}
	v, err := secrets.OpenVault(keyPath, vaultPath)
	if err != nil {
		t.Fatalf("open test vault: %v", err)
	}
	if err := v.Set(key, value); err != nil {
		t.Fatalf("set test secret %q: %v", key, err)
	}
}

func TestBuildPlanWithOpts_RemoteHTTPSecretPlaceholderHostRejectsLoopbackExpansion(t *testing.T) {
	seedDefaultSecretForTest(t, "REMOTE_MCP_HOST", "127.0.0.1")

	m := &config.ServerManifest{
		Name:      "secret-host",
		Kind:      config.KindGlobal,
		Transport: config.TransportRemoteHTTP,
		URL:       "https://${secret:REMOTE_MCP_HOST}/mcp",
		ClientBindings: []config.ClientBinding{
			{Client: "claude-code"},
		},
	}

	_, err := BuildPlanWithOpts(m, BuildPlanOpts{IncludeAllClients: true})
	if err == nil {
		t.Fatal("expected expanded placeholder host loopback rejection; got nil")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "loopback") {
		t.Fatalf("error = %v, want loopback rejection", err)
	}
	if strings.Contains(err.Error(), "127.0.0.1") {
		t.Fatalf("expanded secret host leaked in error: %v", err)
	}
	if !strings.Contains(err.Error(), "${secret:REMOTE_MCP_HOST}") {
		t.Fatalf("error should reference placeholder host, got %v", err)
	}
}

func TestManifestTestRemote_SecretPlaceholderHostRejectsLoopbackExpansionBeforeDial(t *testing.T) {
	seedDefaultSecretForTest(t, "REMOTE_MCP_HOST", "127.0.0.1")

	dir := t.TempDir()
	writeManifest(t, dir, "remote", "name: remote\nkind: global\ntransport: remote-http\nurl: https://${secret:REMOTE_MCP_HOST}/mcp\nclient_bindings:\n  - client: claude-code\n")
	var transportCalled bool
	client := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		transportCalled = true
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(`{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":"2025-11-25"}}`)),
			Request:    req,
		}, nil
	})}

	_, err := NewAPI().manifestTestRemoteWithClient(context.Background(), dir, "remote", client)
	if err == nil {
		t.Fatal("expected expanded placeholder host loopback rejection; got nil")
	}
	if transportCalled {
		t.Fatal("test-remote transport was called before rejecting the expanded loopback host")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "loopback") {
		t.Fatalf("error = %v, want loopback rejection", err)
	}
}
