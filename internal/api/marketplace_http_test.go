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
	rewritten := strings.Replace(MarketplaceTestRegistryURL("/catalog.json"), "https://", "https://attacker:hunter2@", 1)
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
	}))
	tr.Proxy = nil
	tr.TLSClientConfig = &tls.Config{InsecureSkipVerify: true, MinVersion: tls.VersionTLS12}
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
