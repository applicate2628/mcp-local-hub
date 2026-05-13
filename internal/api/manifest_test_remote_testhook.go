// internal/api/manifest_test_remote_testhook.go — G6 sub-PR 3
// test-injection surface, parallel to marketplace_testhook.go.
//
// Lives in a regular Go file (not _test.go) so cross-package tests in
// internal/cli/manifest_test_remote_test.go can install a TLS-trusting
// client via the `mcp-local-hub/internal/api` import. Production
// paths (ManifestTestRemote, ManifestTestRemoteIn) call
// TestRemoteClientForCmd, which returns the production client unless
// a test has installed an override.

package api

import (
	"net/http"
	"net/http/httptest"
	"sync"
)

var (
	testRemoteTestClientMu sync.Mutex
	testRemoteTestClient   *http.Client
)

// TestRemoteClientForCmd returns the test hook if a test installed
// one, else the production HTTPS-only client. The mutex serializes
// hook reads against parallel installer flips.
func TestRemoteClientForCmd() *http.Client {
	testRemoteTestClientMu.Lock()
	defer testRemoteTestClientMu.Unlock()
	if testRemoteTestClient != nil {
		return testRemoteTestClient
	}
	return newTestRemoteClient()
}

// InstallTestRemoteTestClientForCLI builds a TLS-trusting client for
// the given httptest.NewTLSServer and installs it as the hook for
// the duration of the test. Returns a cleanup closure that restores
// the previous hook. Tests should pass it to t.Cleanup.
//
// The borrowed marketplace builder applies a hard 15s http.Client
// Timeout that would mask the operator-visible --timeout flag (bot
// r2 P2 closure, PR #171). Drop it here so the test hook matches the
// production no-Timeout policy where ctx-deadline is the single
// source of truth.
func InstallTestRemoteTestClientForCLI(srv *httptest.Server) func() {
	testRemoteTestClientMu.Lock()
	defer testRemoteTestClientMu.Unlock()
	prev := testRemoteTestClient
	c := buildTLSTrustingClient(srv)
	c.Timeout = 0
	testRemoteTestClient = c
	return func() {
		testRemoteTestClientMu.Lock()
		defer testRemoteTestClientMu.Unlock()
		testRemoteTestClient = prev
	}
}
