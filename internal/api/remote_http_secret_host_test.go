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

func TestExpandRemoteHTTPURLSecretsRejectsPlaceholderHostDelimiterInjection(t *testing.T) {
	for _, expandedHost := range []string{
		"host/evil",
		"host@evil",
		"host:443/evil",
		"host#frag",
		"host?q",
		"bad_host",
		"host:+443",
	} {
		t.Run(expandedHost, func(t *testing.T) {
			_, err := expandRemoteHTTPURLSecrets(
				"https://${secret:REMOTE_MCP_HOST}/mcp",
				fakeSecretLookup(map[string]string{"REMOTE_MCP_HOST": expandedHost}),
			)
			if err == nil {
				t.Fatal("expected expanded placeholder host delimiter rejection; got nil")
			}
			if !strings.Contains(strings.ToLower(err.Error()), "expanded remote-http host") {
				t.Fatalf("error = %v, want expanded remote-http host rejection", err)
			}
			if strings.Contains(err.Error(), expandedHost) {
				t.Fatalf("expanded secret host leaked in error: %v", err)
			}
		})
	}
}

func TestExpandRemoteHTTPURLSecretsPreservesPlaceholderHostPathAndQuery(t *testing.T) {
	got, err := expandRemoteHTTPURLSecrets(
		"https://${secret:REMOTE_MCP_HOST}/a/b?c=d",
		fakeSecretLookup(map[string]string{"REMOTE_MCP_HOST": "remote.example"}),
	)
	if err != nil {
		t.Fatalf("expandRemoteHTTPURLSecrets: %v", err)
	}
	if got != "https://remote.example/a/b?c=d" {
		t.Fatalf("expanded URL = %q, want path and query preserved", got)
	}
}

func TestExpandRemoteHTTPURLSecretsExpandsNonHostSecretWithPlaceholderHost(t *testing.T) {
	// bot PR #388 r10 (remote_http_secret_url.go:75): a placeholder host
	// PLUS a second ${secret:...} in the query must both expand without the
	// suffix-comparison tripping a false "expanded URL shape changed".
	got, err := expandRemoteHTTPURLSecrets(
		"https://${secret:REMOTE_MCP_HOST}/mcp?token=${secret:REMOTE_TOKEN}",
		fakeSecretLookup(map[string]string{
			"REMOTE_MCP_HOST": "remote.example",
			"REMOTE_TOKEN":    "abc123",
		}),
	)
	if err != nil {
		t.Fatalf("expandRemoteHTTPURLSecrets with non-host secret: %v", err)
	}
	if got != "https://remote.example/mcp?token=abc123" {
		t.Fatalf("expanded URL = %q, want host + query secret both expanded", got)
	}
}

func TestExpandRemoteHTTPURLSecretsNonHostSecretStillValidatesExpandedHost(t *testing.T) {
	// The non-host secret must not let a private/loopback expanded host slip
	// through: host validation still runs after isolating the authority.
	_, err := expandRemoteHTTPURLSecrets(
		"https://${secret:REMOTE_MCP_HOST}/mcp?token=${secret:REMOTE_TOKEN}",
		fakeSecretLookup(map[string]string{
			"REMOTE_MCP_HOST": "127.0.0.1",
			"REMOTE_TOKEN":    "abc123",
		}),
	)
	if err == nil {
		t.Fatal("expected loopback host rejection even with a non-host secret present")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "loopback") {
		t.Fatalf("error = %v, want loopback rejection", err)
	}
	if strings.Contains(err.Error(), "abc123") {
		t.Fatalf("non-host secret value leaked in error: %v", err)
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
