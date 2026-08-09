package api

import (
	"context"
	"crypto/tls"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"
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
	_, err := MarketplaceFetchWithClient(context.Background(), client, MarketplaceTestRegistryURL("/catalog.json"), "", nil)
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
	_, err := MarketplaceFetchWithClient(context.Background(), client, MarketplaceTestRegistryURL("/catalog.json"), "", nil)
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
}

func TestMarketplaceFetchTransport_HonorsProxyByDefaultAndDirectFetchOptInDisablesIt(t *testing.T) {
	t.Setenv("HTTPS_PROXY", "http://127.0.0.1:65535")

	proxied := newMarketplaceFetchTransportWithResolver(nil, false)
	if proxied.proxy == nil {
		t.Fatal("marketplace registry fetch transport must honor proxy routing by default")
	}

	direct := newMarketplaceFetchTransportWithResolver(nil, true)
	if direct.proxy != nil {
		t.Fatal("marketplace direct-fetch opt-in must disable proxy routing")
	}
}

func TestOperatorRequestsMarketplaceDirectFetchTruthiness(t *testing.T) {
	for _, tc := range []struct {
		name string
		val  string
		want bool
	}{
		{name: "unset", val: "", want: false},
		{name: "one", val: "1", want: true},
		{name: "true case-insensitive", val: " TrUe ", want: true},
		{name: "false", val: "false", want: false},
		{name: "garbage", val: "yes", want: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(MarketplaceDirectFetchEnv, tc.val)
			if got := operatorRequestsMarketplaceDirectFetch(); got != tc.want {
				t.Fatalf("operatorRequestsMarketplaceDirectFetch(%q) = %v, want %v", tc.val, got, tc.want)
			}
		})
	}
}

func TestMarketplaceFetchTransport_DirectPathInstallsDialGuard(t *testing.T) {
	for _, tc := range []struct {
		name        string
		directFetch bool
	}{
		{name: "default-direct-path", directFetch: false},
		{name: "direct-fetch-opt-in", directFetch: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tr := newMarketplaceFetchTransportWithResolver(nil, tc.directFetch)
			if tr.direct.DialContext == nil {
				t.Fatal("marketplace direct fetch transport must install a guarded DialContext")
			}
			_, err := tr.direct.DialContext(context.Background(), "tcp", "127.0.0.1:443")
			if err == nil {
				t.Fatal("expected dial guard to reject loopback address")
			}
			if !strings.Contains(strings.ToLower(err.Error()), "loopback") {
				t.Fatalf("dial guard error = %v, want loopback rejection", err)
			}
		})
	}
}

func TestMarketplaceFetchTransport_ProxyDialToLoopbackProxyIsNotRejectedByOriginGuard(t *testing.T) {
	var proxyHits atomic.Int32
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		proxyHits.Add(1)
		if r.Method != http.MethodConnect {
			t.Errorf("proxy method = %s, want CONNECT", r.Method)
		}
		http.Error(w, "proxy reached", http.StatusBadGateway)
	}))
	defer proxy.Close()

	proxyURL, err := url.Parse(proxy.URL)
	if err != nil {
		t.Fatalf("parse proxy url: %v", err)
	}
	tr := newMarketplaceFetchTransportWithResolverAndProxy(nil, false, func(*http.Request) (*url.URL, error) {
		return proxyURL, nil
	})
	client := &http.Client{
		Transport:     tr,
		CheckRedirect: rejectUnsafeMarketplaceRedirect,
		Timeout:       time.Second,
	}

	_, err = MarketplaceFetchWithClient(context.Background(), client, "https://marketplace-proxy-target.example.test/catalog.json", "", nil)
	if err == nil {
		t.Fatal("expected proxy CONNECT failure after proxy was reached; got nil")
	}
	if proxyHits.Load() == 0 {
		t.Fatalf("proxy was not reached; error = %v", err)
	}
	lower := strings.ToLower(err.Error())
	if strings.Contains(lower, "loopback") || strings.Contains(lower, "private address") {
		t.Fatalf("origin dial guard rejected the proxy address instead of letting CONNECT reach proxy: %v", err)
	}
}

func TestMarketplaceFetchTransport_DirectDialToPrivateResolvedHostIsRejected(t *testing.T) {
	const rebindHost = "marketplace-private.example.test"
	tr := newMarketplaceFetchTransportWithResolver(marketplaceDNSResolverForTest(t, map[string]net.IP{
		rebindHost: net.IPv4(10, 0, 0, 1),
	}), true)
	client := &http.Client{
		Transport:     tr,
		CheckRedirect: rejectUnsafeMarketplaceRedirect,
		Timeout:       time.Second,
	}

	_, err := MarketplaceFetchWithClient(context.Background(), client, "https://"+rebindHost+"/catalog.json", "", nil)
	if err == nil {
		t.Fatal("expected direct resolved private address rejection; got nil")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "private") {
		t.Fatalf("error = %v, want private-address rejection", err)
	}
}

func TestMarketplaceProxyResidualWarningEmittedWhenProxySelected(t *testing.T) {
	_ = hubMcpStateTestHelper(t)

	proxyURL := &url.URL{Scheme: "http", Host: "proxy.example:8080"}
	proxy := marketplaceProxyWithResidualWarning(func(*http.Request) (*url.URL, error) {
		return proxyURL, nil
	})
	req, err := http.NewRequest(http.MethodGet, "https://registry.example/catalog.json", nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	gotProxy, err := proxy(req)
	if err != nil {
		t.Fatalf("proxy resolver: %v", err)
	}
	if gotProxy.String() != proxyURL.String() {
		t.Fatalf("proxy resolver returned %v, want %v", gotProxy, proxyURL)
	}

	events, err := RecentHubMcpEvents(10)
	if err != nil {
		t.Fatalf("read recent events: %v", err)
	}
	var found map[string]any
	for _, ev := range events {
		if ev["event"] == "marketplace-proxy-ssrf-residual-accepted" {
			found = ev
			break
		}
	}
	if found == nil {
		t.Fatalf("no marketplace proxy residual warning in events: %+v", events)
	}
	if found["level"] != "warn" {
		t.Fatalf("event level = %v, want warn", found["level"])
	}
	if found["direct_fetch_env"] != MarketplaceDirectFetchEnv {
		t.Fatalf("direct_fetch_env = %v, want %s", found["direct_fetch_env"], MarketplaceDirectFetchEnv)
	}
	if found["residual"] != "dns-rebind-ssrf-via-proxy" {
		t.Fatalf("residual = %v, want dns-rebind-ssrf-via-proxy", found["residual"])
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
		_, err := MarketplaceFetchWithClient(context.Background(), client, MarketplaceTestRegistryURL("/catalog.json"), "", map[string]string{
			hdr: "leaked-credential-value",
		})
		if err == nil {
			t.Errorf("header %q: expected rejection; got nil", hdr)
			continue
		}
		if !strings.Contains(err.Error(), "refusing to send header") {
			t.Errorf("header %q: error %v missing 'refusing to send header' text", hdr, err)
		}
	}
	if len(seenForbidden) != 0 {
		t.Errorf("server received forbidden headers despite client-side rejection: %v", seenForbidden)
	}
}

// TestMarketplaceHTTPClient_HeaderFilterIsAnAllowlist pins the POLARITY of
// the extraHeaders filter, which is the property the previous denylist did
// not have.
//
// The old filter enumerated {authorization, cookie, proxy-authorization}
// and forwarded everything else, so the DEFAULT outcome for an
// unenumerated name was "send it to whatever URL --registry points at".
// Every name below is a real credential-bearing header, or a protocol header
// this function sets itself so a caller entry is silently wrong; none of them
// is a hypothetical.
//
// The complement case at the end is what stops the fix from buying
// correctness by refusing everything: the one documented legitimate use
// (a User-Agent override) must still reach the wire.
func TestMarketplaceHTTPClient_HeaderFilterIsAnAllowlist(t *testing.T) {
	var seen []string
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for k := range r.Header {
			seen = append(seen, k+": "+r.Header.Get(k))
		}
		_, _ = w.Write([]byte(`{"schema_version":"1","entries":[]}`))
	}))
	defer srv.Close()
	client := injectTLSTestClient(srv)

	// Each entry: a header the DENYLIST let through, and why it matters.
	refused := map[string]string{
		"PRIVATE-TOKEN":        "GitLab's own credential header — the obvious one for a private catalog host",
		"X-Api-Key":            "the most common vendor API-key spelling",
		"X-Auth-Token":         "vendor bearer-token spelling",
		"X-Amz-Security-Token": "AWS session credential",
		"Authentication-Info":  "RFC 9110 sibling of Authorization",
		"Proxy-Authenticate":   "RFC 9110 sibling of Proxy-Authorization",
		"Cookie2":              "sibling of Cookie",
		// CORRECTION (2026-07-27): this string used to read "overriding it
		// defeats the wire-byte gzip-bomb cap this file installs". False —
		// MarketplaceFetchWithClient's unconditional
		// Set("Accept-Encoding","identity") runs AFTER the extraHeaders loop,
		// so such an entry was always a no-op and the cap was never reachable
		// through it. Measured: sending "gzip" through extraHeaders (with the
		// name temporarily allowlisted) still put "identity" on the wire.
		"Accept-Encoding": "this function sets it unconditionally AFTER the extraHeaders loop, so a caller entry is silently DISCARDED — refuse loudly rather than ignore",
		// The sibling claim is correct and measured the same way: the loop
		// runs AFTER the ifNoneMatch parameter's Set, so a caller entry WINS.
		"If-None-Match": "has its own parameter, and the extraHeaders loop runs AFTER it, so a caller entry silently OVERRIDES the parameter",
	}
	for hdr, why := range refused {
		seen = nil
		_, err := MarketplaceFetchWithClient(context.Background(), client, MarketplaceTestRegistryURL("/catalog.json"), "", map[string]string{
			hdr: "leaked-credential-value",
		})
		if err == nil {
			t.Errorf("header %q was accepted, but the filter must be an ALLOWLIST — %s", hdr, why)
			continue
		}
		if !strings.Contains(err.Error(), "refusing to send header") {
			t.Errorf("header %q: error %v missing 'refusing to send header' text", hdr, err)
		}
		for _, got := range seen {
			if strings.Contains(got, "leaked-credential-value") {
				t.Errorf("header %q reached the wire as %q despite the refusal", hdr, got)
			}
		}
	}

	// Complement: the allowlisted header still works, end to end.
	seen = nil
	if _, err := MarketplaceFetchWithClient(context.Background(), client, MarketplaceTestRegistryURL("/catalog.json"), "", map[string]string{
		"User-Agent": "mcphub-marketplace/test",
	}); err != nil {
		t.Fatalf("the one allowlisted header must still be accepted, or the fix bought correctness by refusing everything: %v", err)
	}
	var sawUA bool
	for _, got := range seen {
		if got == "User-Agent: mcphub-marketplace/test" {
			sawUA = true
		}
	}
	if !sawUA {
		t.Errorf("User-Agent override never reached the server; saw %v", seen)
	}
}

// TestMarketplaceHTTPClient_RejectsEmbeddedCredentials pins codex
// r6 P1 closure (PR #163): a registry URL like
// `https://user:pass@host/catalog.json` must be rejected at the lib
// layer. Go's net/http auto-emits an Authorization header from
// url.URL.User on the outbound request, which never passes through
// extraHeaders at all and so is not covered by the
// allowedMarketplaceHeaders allowlist exercised by
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
	rewritten := strings.Replace(MarketplaceTestRegistryURL("/catalog.json"), "https://", "https://attacker:hunter2@", 1)
	client := injectTLSTestClient(srv)
	_, err := MarketplaceFetchWithClient(context.Background(), client, rewritten, "", nil)
	if err == nil {
		t.Fatalf("expected rejection of url with embedded credentials; got nil (url=%q)", rewritten)
	}
	if !strings.Contains(err.Error(), "must not embed credentials") {
		t.Errorf("error missing 'must not embed credentials' text: %v", err)
	}
	for _, leaked := range []string{"attacker:hunter2", "attacker@", "hunter2@"} {
		if strings.Contains(err.Error(), leaked) {
			t.Fatalf("credential material leaked in error %q", err.Error())
		}
	}
	if seenAuth != "" {
		t.Errorf("server received Authorization header despite rejection: %q (rejection bypassed)", seenAuth)
	}
}

func TestMarketplaceHTTPClient_RedactsCredentialedRedirectLocation(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "https://redirect-user:redirect-pass@mcp.context7.com/catalog.json", http.StatusFound)
	}))
	defer srv.Close()

	client := injectTLSTestClient(srv)
	_, err := MarketplaceFetchWithClient(context.Background(), client, MarketplaceTestRegistryURL("/catalog.json"), "", nil)
	if err == nil {
		t.Fatal("expected credentialed redirect rejection")
	}
	msg := err.Error()
	for _, leaked := range []string{"redirect-user:redirect-pass", "redirect-user@", "redirect-pass@"} {
		if strings.Contains(msg, leaked) {
			t.Fatalf("credential material leaked in redirect error %q", msg)
		}
	}
	if !strings.Contains(msg, "https://mcp.context7.com/catalog.json") {
		t.Fatalf("redirect error should retain redacted target URL context; got %q", msg)
	}
}

func TestMarketplaceHTTPClient_RejectsLocalAndPrivateRegistryURLsBeforeRequest(t *testing.T) {
	var transportCalled bool
	client := &http.Client{
		Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			transportCalled = true
			return nil, errors.New("transport called")
		}),
	}
	for _, rawURL := range []string{
		"https://localhost/catalog.json",
		"https://localhost./catalog.json",
		"https://LOCALHOST./catalog.json",
		"https://127.0.0.1/catalog.json",
		"https://[::1]/catalog.json",
		"https://0177.0.0.1/catalog.json",
		"https://2130706433/catalog.json",
		"https://0x7f000001/catalog.json",
		"https://0.0.0.0/catalog.json",
		"https://10.0.0.1/catalog.json",
		"https://172.16.0.1/catalog.json",
		"https://192.168.1.1/catalog.json",
		"https://169.254.1.1/catalog.json",
		"https://[fc00::1]/catalog.json",
		"https://[fe80::1]/catalog.json",
	} {
		t.Run(rawURL, func(t *testing.T) {
			transportCalled = false
			_, err := MarketplaceFetchWithClient(context.Background(), client, rawURL, "", nil)
			if err == nil {
				t.Fatalf("expected local/private registry URL rejection for %q", rawURL)
			}
			if transportCalled {
				t.Fatalf("transport was called for rejected registry URL %q", rawURL)
			}
		})
	}
}

func TestMarketplaceHTTPClient_RejectsIDNALocalhostBeforeRequest(t *testing.T) {
	var transportCalled bool
	client := &http.Client{
		Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			transportCalled = true
			return nil, errors.New("transport called")
		}),
	}

	_, err := MarketplaceFetchWithClient(context.Background(), client, "https://ｌｏｃａｌｈｏｓｔ/catalog.json", "", nil)
	if err == nil {
		t.Fatal("expected IDNA-normalized localhost registry URL rejection")
	}
	if transportCalled {
		t.Fatal("transport was called for IDNA-normalized localhost registry URL")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "localhost") {
		t.Fatalf("error = %v, want localhost rejection", err)
	}
}

func TestMarketplaceHTTPClient_RejectsRedirectToLoopback(t *testing.T) {
	for _, target := range []string{
		"https://127.0.0.1/catalog.json",
		"https://localhost./catalog.json",
		"https://LOCALHOST./catalog.json",
	} {
		t.Run(target, func(t *testing.T) {
			var hits int
			srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				hits++
				http.Redirect(w, r, target, http.StatusFound)
			}))
			defer srv.Close()
			client := injectTLSTestClient(srv)
			_, err := MarketplaceFetchWithClient(context.Background(), client, MarketplaceTestRegistryURL("/catalog.json"), "", nil)
			if hits != 1 {
				t.Fatalf("expected initial registry request before redirect rejection; got %d hits", hits)
			}
			if err == nil {
				t.Fatal("expected loopback redirect rejection; got nil")
			}
			if !strings.Contains(strings.ToLower(err.Error()), "loopback") &&
				!strings.Contains(strings.ToLower(err.Error()), "localhost") {
				t.Errorf("error should name loopback/localhost redirect rejection; got %v", err)
			}
		})
	}
}

func TestMarketplaceHTTPClient_RejectsResolvedLoopbackRegistryHost(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		_, _ = w.Write([]byte(`{"schema_version":"1","entries":[]}`))
	}))
	defer srv.Close()

	target, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatalf("parse test server url: %v", err)
	}
	_, port, err := net.SplitHostPort(target.Host)
	if err != nil {
		t.Fatalf("split test server hostport %q: %v", target.Host, err)
	}

	const rebindHost = "marketplace-rebind.example.test"
	tr := newMarketplaceFetchTransportWithResolver(marketplaceDNSResolverForTest(t, map[string]net.IP{
		rebindHost: net.IPv4(127, 0, 0, 1),
	}), true)
	tr.direct.TLSClientConfig = &tls.Config{InsecureSkipVerify: true, MinVersion: tls.VersionTLS12}
	client := &http.Client{
		Transport:     tr,
		CheckRedirect: rejectUnsafeMarketplaceRedirect,
		Timeout:       time.Second,
	}

	_, err = MarketplaceFetchWithClient(context.Background(), client, "https://"+rebindHost+":"+port+"/catalog.json", "", nil)
	if err == nil {
		t.Fatal("expected resolved loopback address rejection; got nil")
	}
	if hits.Load() != 0 {
		t.Fatalf("server received %d request(s); resolved loopback rejection must happen before connect", hits.Load())
	}
	if !strings.Contains(strings.ToLower(err.Error()), "loopback") {
		t.Errorf("error should name resolved loopback rejection; got %v", err)
	}
}

func TestMarketplaceFetchDialControlRejectsUnsafeResolvedAddresses(t *testing.T) {
	for _, tc := range []struct {
		name    string
		network string
		address string
		want    string
	}{
		{"ipv4 loopback", "tcp4", "127.0.0.1:443", "loopback"},
		{"ipv6 loopback", "tcp6", "[::1]:443", "loopback"},
		{"ipv4 private", "tcp4", "10.0.0.1:443", "private"},
		{"ipv6 private", "tcp6", "[fc00::1]:443", "private"},
		{"ipv4 link local", "tcp4", "169.254.1.1:443", "link-local"},
		{"ipv6 link local", "tcp6", "[fe80::1%eth0]:443", "link-local"},
		{"ipv4 unspecified", "tcp4", "0.0.0.0:443", "unspecified"},
		{"ipv6 unspecified", "tcp6", "[::]:443", "unspecified"},
		{"ipv4 cgnat", "tcp4", "100.64.0.1:443", "cgnat"},
		{"ipv4 multicast", "tcp4", "224.0.0.1:443", "multicast"},
		{"ipv4 limited broadcast", "tcp4", "255.255.255.255:443", "limited broadcast"},
		{"ipv4 mapped loopback", "tcp6", "[::ffff:127.0.0.1]:443", "loopback"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := marketplaceFetchDialControlContext(context.Background(), tc.network, tc.address, nil)
			if err == nil {
				t.Fatalf("expected rejection for %s", tc.address)
			}
			if !strings.Contains(strings.ToLower(err.Error()), tc.want) {
				t.Fatalf("error %v missing %q", err, tc.want)
			}
		})
	}
}

func TestRejectMarketplaceLocalOrPrivateAddrRejectsSpecialUseAddresses(t *testing.T) {
	for _, tc := range []struct {
		name string
		addr string
		want string
	}{
		{"ipv4 cgnat", "100.64.0.1", "cgnat"},
		{"ipv4 multicast", "224.0.0.1", "multicast"},
		{"ipv4 limited broadcast", "255.255.255.255", "limited broadcast"},
		{"ipv4 this network", "0.1.2.3", "this-network"},
		{"ipv4 ietf protocol assignments", "192.0.0.8", "ietf"},
		{"ipv4 documentation", "192.0.2.1", "documentation"},
		{"ipv4 as112", "192.31.196.1", "as112"},
		{"ipv4 amt", "192.52.193.1", "amt"},
		{"ipv4 6to4 relay anycast", "192.88.99.2", "6to4"},
		{"ipv4 direct delegation as112", "192.175.48.1", "as112"},
		{"ipv4 benchmarking", "198.18.0.1", "benchmarking"},
		{"ipv4 reserved", "240.0.0.1", "reserved"},
		{"ipv4 mapped loopback", "::ffff:127.0.0.1", "loopback"},
		{"ipv6 dummy", "100:0:0:1::1", "dummy"},
		{"ipv6 amt", "2001:3::1", "amt"},
		{"ipv6 as112", "2001:4:112::1", "as112"},
		{"ipv6 orchidv2", "2001:20::1", "orchid"},
		{"ipv6 documentation 3fff", "3fff::1", "documentation"},
		{"ipv6 srv6 sid", "5f00::1", "segment"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			addr := netip.MustParseAddr(tc.addr)
			err := rejectMarketplaceLocalOrPrivateAddr("test address", addr)
			if err == nil {
				t.Fatalf("expected rejection for %s", tc.addr)
			}
			if !strings.Contains(strings.ToLower(err.Error()), tc.want) {
				t.Fatalf("error %v missing %q", err, tc.want)
			}
		})
	}
}

func marketplaceDNSResolverForTest(t *testing.T, records map[string]net.IP) *net.Resolver {
	t.Helper()
	udpConn, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen test dns udp: %v", err)
	}
	t.Cleanup(func() { _ = udpConn.Close() })
	go func() {
		buf := make([]byte, 1500)
		for {
			n, addr, err := udpConn.ReadFrom(buf)
			if err != nil {
				return
			}
			query := append([]byte(nil), buf[:n]...)
			resp := marketplaceDNSResponseForTest(query, records)
			if len(resp) != 0 {
				_, _ = udpConn.WriteTo(resp, addr)
			}
		}
	}()

	tcpLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen test dns tcp: %v", err)
	}
	t.Cleanup(func() { _ = tcpLn.Close() })
	go func() {
		for {
			conn, err := tcpLn.Accept()
			if err != nil {
				return
			}
			go marketplaceServeDNSTCPConnForTest(conn, records)
		}
	}()

	udpAddr := udpConn.LocalAddr().String()
	tcpAddr := tcpLn.Addr().String()
	return &net.Resolver{
		PreferGo: true,
		Dial: func(ctx context.Context, network, address string) (net.Conn, error) {
			var d net.Dialer
			if strings.HasPrefix(network, "tcp") {
				return d.DialContext(ctx, network, tcpAddr)
			}
			return d.DialContext(ctx, network, udpAddr)
		},
	}
}

func marketplaceServeDNSTCPConnForTest(conn net.Conn, records map[string]net.IP) {
	defer conn.Close()
	for {
		var frameLen [2]byte
		if _, err := io.ReadFull(conn, frameLen[:]); err != nil {
			return
		}
		msgLen := int(binary.BigEndian.Uint16(frameLen[:]))
		query := make([]byte, msgLen)
		if _, err := io.ReadFull(conn, query); err != nil {
			return
		}
		resp := marketplaceDNSResponseForTest(query, records)
		if len(resp) == 0 {
			return
		}
		framed := make([]byte, 2, len(resp)+2)
		binary.BigEndian.PutUint16(framed, uint16(len(resp)))
		framed = append(framed, resp...)
		if _, err := conn.Write(framed); err != nil {
			return
		}
	}
}

func marketplaceDNSResponseForTest(query []byte, records map[string]net.IP) []byte {
	if len(query) < 12 {
		return marketplaceDNSHeaderResponseForTest(query, 1)
	}
	off := 12
	var labels []string
	for {
		if off >= len(query) {
			return marketplaceDNSHeaderResponseForTest(query, 1)
		}
		l := int(query[off])
		off++
		if l == 0 {
			break
		}
		if off+l > len(query) {
			return marketplaceDNSHeaderResponseForTest(query, 1)
		}
		labels = append(labels, string(query[off:off+l]))
		off += l
	}
	if off+4 > len(query) {
		return marketplaceDNSHeaderResponseForTest(query, 1)
	}
	qtype := binary.BigEndian.Uint16(query[off : off+2])
	questionEnd := off + 4
	host := strings.ToLower(strings.Join(labels, "."))
	ip := records[host]
	if ip == nil {
		return marketplaceDNSHeaderResponseForTest(query, 3)
	}

	var answer []byte
	switch qtype {
	case 1:
		if ip4 := ip.To4(); ip4 != nil {
			answer = appendDNSAnswerForTest(answer, qtype, ip4)
		}
	case 28:
		if ip16 := ip.To16(); ip16 != nil && ip.To4() == nil {
			answer = appendDNSAnswerForTest(answer, qtype, ip16)
		}
	}

	resp := make([]byte, 12, 12+questionEnd-12+len(answer))
	copy(resp[:2], query[:2])
	binary.BigEndian.PutUint16(resp[2:4], 0x8180)
	binary.BigEndian.PutUint16(resp[4:6], 1)
	if len(answer) != 0 {
		binary.BigEndian.PutUint16(resp[6:8], 1)
	}
	resp = append(resp, query[12:questionEnd]...)
	resp = append(resp, answer...)
	return resp
}

func marketplaceDNSHeaderResponseForTest(query []byte, rcode uint16) []byte {
	resp := make([]byte, 12)
	if len(query) >= 2 {
		copy(resp[:2], query[:2])
	}
	binary.BigEndian.PutUint16(resp[2:4], 0x8180|rcode)
	return resp
}

func appendDNSAnswerForTest(dst []byte, qtype uint16, data []byte) []byte {
	dst = append(dst, 0xc0, 0x0c)
	dst = binary.BigEndian.AppendUint16(dst, qtype)
	dst = binary.BigEndian.AppendUint16(dst, 1)
	dst = binary.BigEndian.AppendUint32(dst, 0)
	dst = binary.BigEndian.AppendUint16(dst, uint16(len(data)))
	dst = append(dst, data...)
	return dst
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

// injectTLSTestClient builds an http.Client that trusts the
// httptest TLS server's certificate and keeps the marketplace test
// policy needed for local server rewrites (DisableCompression +
// downgrade-redirect guard).
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
