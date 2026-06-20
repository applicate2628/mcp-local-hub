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
	"net/url"
	"strings"
	"sync"
)

const marketplaceTestRegistryHost = "registry.example.test"

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

// MarketplaceTestRegistryURL returns a non-localhost registry URL that the
// marketplace test client rewrites to the supplied httptest server. Tests use
// this to exercise the production loopback/private-host rejection instead of
// weakening MarketplaceFetchWithClient for local test servers.
func MarketplaceTestRegistryURL(path string) string {
	if path == "" {
		path = "/catalog.json"
	}
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	return "https://" + marketplaceTestRegistryHost + path
}

// buildTLSTrustingClient is the body of injectTLSTestClient promoted
// out of marketplace_http_test.go into production so the cli tests
// can reach it. It keeps the marketplace test policy needed for local
// server rewrites (DisableCompression + CheckRedirect). MinTLS = 1.2
// to keep parity with mainstream Go HTTPS defaults.
func buildTLSTrustingClient(srv *httptest.Server) *http.Client {
	t := newMarketplaceTransport()
	t.TLSClientConfig = &tls.Config{
		RootCAs:    srv.Client().Transport.(*http.Transport).TLSClientConfig.RootCAs,
		MinVersion: tls.VersionTLS12,
	}
	target, _ := url.Parse(srv.URL)
	return &http.Client{
		Transport:     marketplaceTestRewriteTransport{base: t, target: target},
		CheckRedirect: rejectUnsafeMarketplaceRedirect,
		Timeout:       marketplaceHTTPTimeout,
	}
}

type marketplaceTestRewriteTransport struct {
	base   http.RoundTripper
	target *url.URL
}

func (t marketplaceTestRewriteTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if t.target != nil && strings.EqualFold(req.URL.Hostname(), marketplaceTestRegistryHost) {
		clone := req.Clone(req.Context())
		u := *clone.URL
		u.Scheme = t.target.Scheme
		u.Host = t.target.Host
		clone.URL = &u
		clone.Host = t.target.Host
		return t.base.RoundTrip(clone)
	}
	return t.base.RoundTrip(req)
}
