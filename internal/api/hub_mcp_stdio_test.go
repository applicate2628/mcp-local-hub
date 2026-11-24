package api

import (
	"bytes"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"mcp-local-hub/internal/config"
)

func TestRegisteredSessionLocalBindingRefusesOrdinaryListener(t *testing.T) {
	hubMcpStateTestHelper(t)
	var calls atomic.Int32
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusTeapot)
	}))
	defer backend.Close()
	_, portText, err := net.SplitHostPort(strings.TrimPrefix(backend.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(portText)
	if err != nil {
		t.Fatal(err)
	}
	raw := fmt.Sprintf("name: private-fixture\nkind: global\ntransport: stdio-bridge\ncommand: fixture\ndaemons:\n  - name: default\n    port: %d\n    mcp_stdio_state: session-local\nclient_bindings:\n  - client: claude-code\n    daemon: default\n    url_path: /mcp\n", port)
	manifest, err := config.ParseManifest(strings.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	prior := LoadResolverSnapshot()
	t.Cleanup(func() { PublishResolverSnapshot(prior) })
	BumpResolverOnManifestChange([]config.ServerManifest{*manifest})
	tokens, err := EnsureHubTokens([]string{"claude-code"})
	if err != nil {
		t.Fatal(err)
	}
	endpoint, err := EnsureHubEndpoint(0, 1234)
	if err != nil {
		t.Fatal(err)
	}
	store := NewHubSessionStore(SessionStoreOpts{})
	defer store.Close()
	handler := NewHubMcpHandler(store)
	handler.SetEndpoint(endpoint)
	request := httptest.NewRequest(http.MethodPost, "http://127.0.0.1/clients/claude-code/mcp", bytes.NewBufferString(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25"}}`))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Mcphub-Hub-Token", tokens.Tokens["claude-code"])
	request.Header.Set("X-Mcphub-Instance-Id", endpoint.InstanceID)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusServiceUnavailable || response.Header().Get("Mcp-Session-Id") != "" {
		t.Fatalf("unsupported private front was admitted: %d %s", response.Code, response.Body.String())
	}
	if calls.Load() != 0 {
		t.Fatal("private descriptor fell through to ordinary TCP")
	}
}
