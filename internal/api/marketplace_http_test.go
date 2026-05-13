package api

import (
	"context"
	"crypto/tls"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestMarketplaceHTTPClient_RejectsNonHTTPSURL(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("nope"))
	}))
	defer srv.Close()
	if !strings.HasPrefix(srv.URL, "http://") {
		t.Skipf("httptest.NewServer is not http; got %q", srv.URL)
	}
	_, err := MarketplaceFetch(context.Background(), srv.URL, "", nil)
	if err == nil || !strings.Contains(err.Error(), "https") {
		t.Errorf("expected https rejection; got %v", err)
	}
}

func TestMarketplaceHTTPClient_RejectsDowngradeRedirect(t *testing.T) {
	plain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("OK"))
	}))
	defer plain.Close()
	tlsSrv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, plain.URL, http.StatusFound)
	}))
	defer tlsSrv.Close()
	// Use the shared TLS-injecting helper so CheckRedirect +
	// DisableCompression are inherited from production policy.
	client := injectTLSTestClient(tlsSrv)
	_, err := MarketplaceFetchWithClient(context.Background(), client, tlsSrv.URL, "", nil)
	if err == nil || !strings.Contains(strings.ToLower(err.Error()), "https") {
		t.Errorf("expected https-downgrade rejection; got %v", err)
	}
}

func TestMarketplaceHTTPClient_DisablesCompression(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Server-side check: client must NOT advertise gzip in Accept-Encoding.
		ae := r.Header.Get("Accept-Encoding")
		if strings.Contains(ae, "gzip") {
			t.Errorf("Accept-Encoding contains gzip: %q (compression must be disabled)", ae)
		}
		_, _ = w.Write([]byte(`{"schema_version":"1","entries":[]}`))
	}))
	defer srv.Close()
	client := injectTLSTestClient(srv)
	_, err := MarketplaceFetchWithClient(context.Background(), client, srv.URL, "", nil)
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
}

// injectTLSTestClient builds an http.Client that trusts the
// httptest TLS server's certificate AND inherits the marketplace
// transport policy (DisableCompression + downgrade-redirect guard).
// Tests share this helper instead of building it inline. The body is
// promoted to production code in marketplace_testhook.go as
// buildTLSTrustingClient so cross-package CLI tests can reuse the
// same TLS-trusting shape (codex r3 P1 #1 closure).
func injectTLSTestClient(srv *httptest.Server) *http.Client {
	return buildTLSTrustingClient(srv)
}

// Suppress unused-import warning for crypto/tls — the legacy in-file
// implementation referenced tls.Config directly. The current
// implementation routes through buildTLSTrustingClient in
// marketplace_testhook.go, but we keep the import as documentation
// that this test file is a sibling of the TLS test hook surface.
var _ = tls.VersionTLS12
