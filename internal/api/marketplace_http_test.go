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

// TestMarketplaceHTTPClient_RejectsCredentialHeaders pins codex r5
// lane 1 P2 closure: MarketplaceFetchWithClient must refuse extra
// headers that carry credentials (Authorization, Cookie,
// Proxy-Authorization). The threat model is an unauthenticated GET
// against a public registry, so any such header would leak operator
// credentials to whatever URL --registry points at.
func TestMarketplaceHTTPClient_RejectsCredentialHeaders(t *testing.T) {
	// The server records what headers it actually received so the
	// test can also prove the rejection happened CLIENT-side (before
	// the request reached the wire). If the test ever sees one of
	// these headers in srv-received headers, it has regressed.
	var seenForbidden []string
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for _, k := range []string{"Authorization", "Cookie", "Proxy-Authorization"} {
			if r.Header.Get(k) != "" {
				seenForbidden = append(seenForbidden, k)
			}
		}
		_, _ = w.Write([]byte(`{"schema_version":"1","entries":[]}`))
	}))
	defer srv.Close()
	client := injectTLSTestClient(srv)
	for _, hdr := range []string{"Authorization", "Cookie", "Proxy-Authorization", "authorization", "COOKIE"} {
		_, err := MarketplaceFetchWithClient(context.Background(), client, srv.URL, "", map[string]string{
			hdr: "leaked-credential-value",
		})
		if err == nil {
			t.Errorf("header %q: expected rejection; got nil", hdr)
			continue
		}
		if !strings.Contains(err.Error(), "credential-bearing header") {
			t.Errorf("header %q: error %v missing 'credential-bearing header' text", hdr, err)
		}
	}
	if len(seenForbidden) != 0 {
		t.Errorf("server received forbidden headers despite client-side rejection: %v", seenForbidden)
	}
}

// TestMarketplaceHTTPClient_RejectsEmbeddedCredentials pins codex
// r6 P1 closure (PR #163): a registry URL like
// `https://user:pass@host/catalog.json` must be rejected at the lib
// layer. Go's net/http auto-emits an Authorization header from
// url.URL.User on the outbound request, which would bypass the
// forbiddenMarketplaceHeaders denylist exercised by
// TestMarketplaceHTTPClient_RejectsCredentialHeaders.
func TestMarketplaceHTTPClient_RejectsEmbeddedCredentials(t *testing.T) {
	// Server must never receive the request — if the rejection
	// regressed, the test fails loudly via the seenAuth check.
	var seenAuth string
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenAuth = r.Header.Get("Authorization")
		_, _ = w.Write([]byte(`{"schema_version":"1","entries":[]}`))
	}))
	defer srv.Close()
	// Splice the userinfo into the httptest URL (https://<host>:<port>).
	// httptest URLs are always scheme://host[:port]/path, so the
	// "https://user:pass@<rest>" rewrite is safe.
	rewritten := strings.Replace(srv.URL, "https://", "https://attacker:hunter2@", 1)
	client := injectTLSTestClient(srv)
	_, err := MarketplaceFetchWithClient(context.Background(), client, rewritten, "", nil)
	if err == nil {
		t.Fatalf("expected rejection of url with embedded credentials; got nil (url=%q)", rewritten)
	}
	if !strings.Contains(err.Error(), "must not embed credentials") {
		t.Errorf("error missing 'must not embed credentials' text: %v", err)
	}
	if seenAuth != "" {
		t.Errorf("server received Authorization header despite rejection: %q (rejection bypassed)", seenAuth)
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
