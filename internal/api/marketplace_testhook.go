// internal/api/marketplace_testhook.go — G5 test-injection surface.
//
// codex r3 P1 #1 closure: these helpers MUST live in a regular
// Go file (not _test.go) so cross-package test code in
// internal/cli/marketplace_test.go can call them via the
// `mcp-local-hub/internal/api` import. The production paths
// (LoadMarketplaceCatalog, RefreshMarketplaceCatalog) never call
// them; the file only exists for the test-injection surface.

package api

import (
	"crypto/tls"
	"net/http"
	"net/http/httptest"
	"sync"
)

// marketplaceTestClient is the optional test-only client. nil in
// production. Guarded by marketplaceTestClientMu so concurrent test
// invocations don't race the hook pointer (codex r3 P3 #1 closure).
var (
	marketplaceTestClientMu sync.Mutex
	marketplaceTestClient   *http.Client
)

// MarketplaceClientForCmd returns the test hook if set, else the
// production client. CLI subcommands call this to fetch a client.
// Mutex-guarded so any test that flips the hook in parallel is
// observably ordered against readers.
func MarketplaceClientForCmd() *http.Client {
	marketplaceTestClientMu.Lock()
	defer marketplaceTestClientMu.Unlock()
	if marketplaceTestClient != nil {
		return marketplaceTestClient
	}
	return newMarketplaceClient()
}

// InstallMarketplaceTestClientForCLI builds a TLS-trusting client
// for `httptest.NewTLSServer` and installs it as the CLI hook for
// the duration of the test. Returns a cleanup closure that restores
// the previous hook (typically nil). Tests should pass the return
// to `t.Cleanup`.
//
// The function itself is in production code; it just isn't called
// from production code paths. CI builds compile it; the binary
// never invokes it.
func InstallMarketplaceTestClientForCLI(srv *httptest.Server) func() {
	marketplaceTestClientMu.Lock()
	defer marketplaceTestClientMu.Unlock()
	prev := marketplaceTestClient
	marketplaceTestClient = buildTLSTrustingClient(srv)
	return func() {
		marketplaceTestClientMu.Lock()
		defer marketplaceTestClientMu.Unlock()
		marketplaceTestClient = prev
	}
}

// buildTLSTrustingClient is the body of injectTLSTestClient promoted
// out of marketplace_http_test.go into production so the cli tests
// can reach it. Inherits the production transport policy
// (DisableCompression + CheckRedirect). MinTLS = 1.2 to keep parity
// with mainstream Go HTTPS defaults.
func buildTLSTrustingClient(srv *httptest.Server) *http.Client {
	t := newMarketplaceTransport()
	t.TLSClientConfig = &tls.Config{
		RootCAs:    srv.Client().Transport.(*http.Transport).TLSClientConfig.RootCAs,
		MinVersion: tls.VersionTLS12,
	}
	return &http.Client{
		Transport:     t,
		CheckRedirect: rejectNonHTTPSRedirect,
		Timeout:       marketplaceHTTPTimeout,
	}
}
