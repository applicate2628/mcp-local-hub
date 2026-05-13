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
// Two overrides over the borrowed marketplace builder:
//   - Timeout=0: the operator-visible --timeout flag wraps the
//     request in ctx, which is the single source of truth — a hard
//     client cap would silently override it (bot r2 P2, PR #171).
//   - CheckRedirect=rejectAllRedirects: test-remote sends manifest-
//     defined headers (Authorization, X-API-Key) and Go forwards
//     non-sensitive custom headers across host changes, so any
//     followed redirect could leak credentials (bot r5 P1, PR #171).
//
// Test hook mirrors production redirect policy so test assertions
// match the real refuse-and-surface behavior.
func InstallTestRemoteTestClientForCLI(srv *httptest.Server) func() {
	testRemoteTestClientMu.Lock()
	defer testRemoteTestClientMu.Unlock()
	prev := testRemoteTestClient
	c := buildTLSTrustingClient(srv)
	c.Timeout = 0
	c.CheckRedirect = rejectAllRedirects
	testRemoteTestClient = c
	return func() {
		testRemoteTestClientMu.Lock()
		defer testRemoteTestClientMu.Unlock()
		testRemoteTestClient = prev
	}
}
