// internal/api/marketplace_http.go — G5 HTTPS-only HTTP client.
//
// Spec: docs/superpowers/specs/2026-05-12-g5-marketplace-draft-import-design.md
// §"Registry source" + §"Threat model".
//
// codex r1 P1 closures: enforce https://-only, reject downgrade
// redirects, disable compression (10MB cap applies to wire bytes,
// not decompressed bytes — defeats gzip-bomb amplification).

package api

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"sort"
	"strings"
	"syscall"
	"time"

	"mcp-local-hub/internal/urlredact"
)

const (
	marketplaceHTTPTimeout        = 15 * time.Second
	marketplaceCacheMaxBodyBytes  = 10 * 1024 * 1024
	marketplaceFetchDialTimeout   = 30 * time.Second
	marketplaceFetchDialKeepAlive = 30 * time.Second
)

// MarketplaceDirectFetchEnv is the operator opt-in for the airtight
// marketplace registry fetch path. When set to "1" or "true", marketplace
// fetches bypass environment proxies so the dial-time guard validates the
// origin address directly. The default honors the cloned stdlib proxy settings
// for corporate proxy-only hosts.
const MarketplaceDirectFetchEnv = "MCPHUB_MARKETPLACE_DIRECT_FETCH"

func operatorRequestsMarketplaceDirectFetch() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(MarketplaceDirectFetchEnv))) {
	case "1", "true":
		return true
	}
	return false
}

// rejectUnsafeMarketplaceRedirect refuses redirect targets outside the
// marketplace registry URL policy. Used by both production and test clients.
func rejectUnsafeMarketplaceRedirect(req *http.Request, via []*http.Request) error {
	if len(via) >= 10 {
		return errors.New("too many redirects")
	}
	if err := validateMarketplacePublicHTTPSParsedURL(req.URL); err != nil {
		redactedTarget := urlredact.MarketplaceURLForError(req.URL.String())
		if req.URL.User != nil {
			safeURL := *req.URL
			safeURL.User = nil
			req.URL = &safeURL
		}
		return fmt.Errorf("refusing redirect from %s to unsafe marketplace URL %s: %w", urlredact.MarketplaceURLForError(via[len(via)-1].URL.String()), redactedTarget, err)
	}
	return nil
}

// newMarketplaceTransport clones http.DefaultTransport and only
// overrides DisableCompression so we keep stdlib defaults that
// matter in real environments: ProxyFromEnvironment (so HTTP_PROXY /
// HTTPS_PROXY / NO_PROXY work), the default DialContext (keep-alive,
// dual-stack), idle connection pooling, TLS handshake and
// expect-continue timeouts. Tests substitute a TLS-trusting
// transport via injectTLSTestClient.
//
// codex r6 P2 closure (PR #163): the previous &http.Transport{
// DisableCompression: true} dropped all stdlib defaults, so
// marketplace fetches bypassed proxy resolution and could fail in
// proxied environments where the rest of the binary already works.
func newMarketplaceTransport() *http.Transport {
	if base, ok := http.DefaultTransport.(*http.Transport); ok {
		t := base.Clone()
		t.DisableCompression = true
		return t
	}
	// Fallback path: http.DefaultTransport is documented as
	// *http.Transport; the type assertion is here for forward-
	// compatibility, not because the assertion is expected to fail.
	return &http.Transport{
		DisableCompression: true,
	}
}

// newMarketplaceFetchTransport is the registry-fetch-only transport.
// It preserves the cloned stdlib TLS/idle-connection defaults from
// newMarketplaceTransport, then applies the resolved-IP dial guard only
// when the effective request is direct. On a proxy-selected request, Go
// dials the proxy address and sends CONNECT; validating that proxy IP would
// reject normal loopback/RFC1918 corporate proxies instead of the origin.
func newMarketplaceFetchTransport() http.RoundTripper {
	return newMarketplaceFetchTransportWithResolver(nil, operatorRequestsMarketplaceDirectFetch())
}

type marketplaceFetchProxyContextKey struct{}

type marketplaceFetchTransport struct {
	direct  *http.Transport
	proxied *http.Transport
	proxy   func(*http.Request) (*url.URL, error)
}

func newMarketplaceFetchTransportWithResolver(resolver *net.Resolver, directFetch bool) *marketplaceFetchTransport {
	return newMarketplaceFetchTransportWithResolverAndProxy(resolver, directFetch, nil)
}

func newMarketplaceFetchTransportWithResolverAndProxy(resolver *net.Resolver, directFetch bool, proxyOverride func(*http.Request) (*url.URL, error)) *marketplaceFetchTransport {
	base := newMarketplaceTransport()
	proxy := base.Proxy
	if proxyOverride != nil {
		proxy = proxyOverride
	}
	if directFetch {
		proxy = nil
	} else if proxy != nil {
		proxy = marketplaceProxyWithResidualWarning(proxy)
	}
	direct := base.Clone()
	direct.Proxy = nil
	configureMarketplaceFetchDialer(direct, resolver, true)

	proxied := base.Clone()
	configureMarketplaceFetchDialer(proxied, resolver, false)
	proxied.Proxy = func(req *http.Request) (*url.URL, error) {
		if proxyURL, ok := req.Context().Value(marketplaceFetchProxyContextKey{}).(*url.URL); ok {
			return proxyURL, nil
		}
		if proxy == nil {
			return nil, nil
		}
		return proxy(req)
	}

	return &marketplaceFetchTransport{
		direct:  direct,
		proxied: proxied,
		proxy:   proxy,
	}
}

func configureMarketplaceFetchDialer(t *http.Transport, resolver *net.Resolver, guardResolvedAddr bool) {
	d := &net.Dialer{
		Timeout:   marketplaceFetchDialTimeout,
		KeepAlive: marketplaceFetchDialKeepAlive,
		Resolver:  resolver,
	}
	if guardResolvedAddr {
		d.ControlContext = marketplaceFetchDialControlContext
	}
	t.Dial = nil
	t.DialContext = d.DialContext
	t.DialTLS = nil
	t.DialTLSContext = nil
}

func (t *marketplaceFetchTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if t.proxy == nil {
		return t.direct.RoundTrip(req)
	}
	proxyURL, err := t.proxy(req)
	if err != nil {
		return nil, err
	}
	if proxyURL == nil {
		return t.direct.RoundTrip(req)
	}
	req = req.WithContext(context.WithValue(req.Context(), marketplaceFetchProxyContextKey{}, proxyURL))
	return t.proxied.RoundTrip(req)
}

func (t *marketplaceFetchTransport) CloseIdleConnections() {
	t.direct.CloseIdleConnections()
	t.proxied.CloseIdleConnections()
}

func marketplaceProxyWithResidualWarning(proxy func(*http.Request) (*url.URL, error)) func(*http.Request) (*url.URL, error) {
	return func(req *http.Request) (*url.URL, error) {
		proxyURL, err := proxy(req)
		if err == nil && proxyURL != nil {
			_ = LogHubMcpEvent("warn", "marketplace-proxy-ssrf-residual-accepted", map[string]any{
				"registry_host":    req.URL.Host,
				"proxy_host":       proxyURL.Host,
				"residual":         "dns-rebind-ssrf-via-proxy",
				"direct_fetch_env": MarketplaceDirectFetchEnv,
				"mitigation":       MarketplaceDirectFetchEnv + "=1 bypasses environment proxies so the dial guard validates the registry origin address directly",
			})
		}
		return proxyURL, err
	}
}

func marketplaceFetchDialControlContext(_ context.Context, _ string, address string, _ syscall.RawConn) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return fmt.Errorf("marketplace fetch resolved dial address %q is not host:port: %w", address, err)
	}
	if i := strings.LastIndexByte(host, '%'); i >= 0 {
		host = host[:i]
	}
	addr, err := netip.ParseAddr(host)
	if err != nil {
		return fmt.Errorf("marketplace fetch resolved dial address %q is not an IP address: %w", address, err)
	}
	return rejectMarketplaceLocalOrPrivateAddr(fmt.Sprintf("marketplace fetch resolved address %q", address), addr)
}

func newMarketplaceClient() *http.Client {
	return &http.Client{
		Transport:     newMarketplaceFetchTransport(),
		CheckRedirect: rejectUnsafeMarketplaceRedirect,
		Timeout:       marketplaceHTTPTimeout,
	}
}

// MarketplaceFetchResult carries the wire-level outcome plus the
// response body (size-capped + already drained).
type MarketplaceFetchResult struct {
	Status int
	Body   []byte
	ETag   string
	NotMod bool // true when status == 304
}

// MarketplaceFetch is the production HTTPS-only fetch path. It builds
// a request, sends it via the canonical client, and returns a result
// or an error. `ifNoneMatch` is sent as the `If-None-Match` header
// when non-empty.
func MarketplaceFetch(ctx context.Context, rawURL, ifNoneMatch string, extraHeaders map[string]string) (*MarketplaceFetchResult, error) {
	return MarketplaceFetchWithClient(ctx, newMarketplaceClient(), rawURL, ifNoneMatch, extraHeaders)
}

// allowedMarketplaceHeaders is the CLOSED set of extra request headers a
// caller may add to a marketplace fetch. Everything not listed here is
// refused.
//
// This is an ALLOWLIST on purpose. It replaces a denylist of
// {authorization, cookie, proxy-authorization}, which had the wrong
// polarity for a security control: the default outcome of an unenumerated
// name was "send it". The threat model is an unauthenticated GET against
// whatever URL the operator passes through --registry, so every header
// that leaks is a credential handed to a host of the operator's (or a
// downstream caller's) choosing — and the set of credential-bearing header
// names is open, not closed. The denylist missed, by construction, every
// one of:
//
//   - vendor-specific credential headers a private catalog host would
//     plausibly want: GitLab's PRIVATE-TOKEN, X-Api-Key, X-Auth-Token,
//     X-Access-Token, X-Amz-Security-Token, Circle-Token, X-JFrog-Art-Api;
//   - the RFC-standard siblings of the three it did list: Authentication-Info,
//     Proxy-Authenticate, Cookie2;
//   - protocol headers this function already sets ITSELF, where a caller's
//     value is silently wrong: If-None-Match and Accept-Encoding.
//
// Those last two are silently wrong in OPPOSITE directions, and the asymmetry
// is the whole reason to name them. Measured against a live httptest server
// with each name temporarily allowlisted (2026-07-27):
//
//	If-None-Match     the extraHeaders loop runs AFTER the Set that applies the
//	                  ifNoneMatch PARAMETER, so a caller entry WINS. Parameter
//	                  `"param-etag"` + extraHeaders `"caller-etag"` put
//	                  `"caller-etag"` on the wire: the parameter is overridden.
//	Accept-Encoding   the unconditional Set("Accept-Encoding","identity") runs
//	                  AFTER the extraHeaders loop, so a caller entry LOSES.
//	                  extraHeaders `gzip` still put `identity` on the wire: the
//	                  caller's intent is discarded.
//
// CORRECTION (2026-07-27): an earlier revision of this comment, of aed0d7e9's
// message, and of the matching test assertion all claimed that overriding
// Accept-Encoding "defeats the wire-byte gzip-bomb cap this file installs".
// That was never true — the Set below it made such an entry a no-op, so the cap
// was never reachable through this path (and that Set already preceded
// aed0d7e9). The real reason to refuse it is fail-loud over silently-ignored.
// The If-None-Match half of the claim was, and remains, correct.
//
// A per-name reason is not what makes any of these refused, though — that is
// the point of the polarity. Everything outside the map is refused BY DEFAULT;
// the list above records which names the predecessor denylist demonstrably let
// through, not a set of bespoke rules.
//
// Inverting the polarity makes the safe outcome the DEFAULT and makes
// widening the surface a deliberate, reviewable edit to this map, rather
// than a name nobody thought to add to a ban list.
//
// The map is currently minimal by intent: User-Agent is the one use the
// contract ever named ("permitted only for non-credentialed metadata (e.g.
// a future User-Agent override)"), and no in-repo caller passes any header
// at all — both production call sites pass nil (marketplace_cache.go:92,
// marketplace_cache.go:153). A future caller that genuinely needs another
// non-credentialed header adds it here with its own justification.
//
// Header names compared lowercase since http.Header canonicalizes on
// set; we lower the caller's key before looking it up. A name carrying
// stray whitespace or any other non-token byte therefore does not match an
// entry and is refused here, instead of reaching http.Transport and dying
// there with an opaque "invalid header field name".
var allowedMarketplaceHeaders = map[string]struct{}{
	"user-agent": {},
}

// sortedAllowedMarketplaceHeaders renders the allowlist for the refusal
// message. Sorted because Go map iteration order is randomized per run, and
// an error string that changes between two identical calls is an ambient
// input an operator (or a test) cannot rely on.
func sortedAllowedMarketplaceHeaders() string {
	names := make([]string, 0, len(allowedMarketplaceHeaders))
	for k := range allowedMarketplaceHeaders {
		names = append(names, k)
	}
	sort.Strings(names)
	return strings.Join(names, ", ")
}

// MarketplaceFetchWithClient is the injectable form. Tests pass a
// client with a TLS test transport. Production callers go through
// MarketplaceFetch.
//
// `extraHeaders` is permitted only for non-credentialed metadata
// (e.g. a User-Agent override). Every name outside
// allowedMarketplaceHeaders — which includes but is not limited to
// Authorization, Cookie and Proxy-Authorization — is refused with an
// error before the request is built.
func MarketplaceFetchWithClient(ctx context.Context, client *http.Client, rawURL, ifNoneMatch string, extraHeaders map[string]string) (*MarketplaceFetchResult, error) {
	u, err := parseMarketplacePublicHTTPSURL(rawURL)
	if err != nil {
		return nil, fmt.Errorf("marketplace url must be public https:// without embedded credentials or unsafe characters (got %q): %w", urlredact.MarketplaceURLForError(rawURL), err)
	}
	// Refuse every header outside the allowlist BEFORE building the request
	// (defense-in-depth: if a future caller passes a credential, fail loud,
	// do not silently strip — the caller's intent is either a programming
	// error or a privilege escalation). Fail-closed: an unrecognized name is
	// refused, never forwarded, because the set of credential-bearing header
	// names is open and this fetch is an unauthenticated GET against an
	// operator-supplied URL.
	for k := range extraHeaders {
		if _, allowed := allowedMarketplaceHeaders[strings.ToLower(k)]; !allowed {
			return nil, fmt.Errorf("refusing to send header %q to marketplace registry — fetches are unauthenticated GETs, so only %s may be added (add the name to allowedMarketplaceHeaders if a non-credentialed header is genuinely needed)", k, sortedAllowedMarketplaceHeaders())
		}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	if ifNoneMatch != "" {
		req.Header.Set("If-None-Match", ifNoneMatch)
	}
	for k, v := range extraHeaders {
		req.Header.Set(k, v)
	}
	// Explicit identity (defense-in-depth alongside transport-level
	// DisableCompression: true).
	req.Header.Set("Accept-Encoding", "identity")
	resp, err := client.Do(req)
	if err != nil {
		return nil, redactMarketplaceHTTPError(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotModified {
		return &MarketplaceFetchResult{Status: resp.StatusCode, NotMod: true, ETag: resp.Header.Get("ETag")}, nil
	}
	if resp.StatusCode != http.StatusOK {
		// rawURL was already accepted by parseMarketplacePublicHTTPSURL
		// (no userinfo), but redact unconditionally so this error site can
		// never become a new credential-leak path if the validator is ever
		// loosened (structural: every URL-in-error goes through urlredact).
		return &MarketplaceFetchResult{Status: resp.StatusCode}, fmt.Errorf("fetch %s: HTTP %d", urlredact.MarketplaceURLForError(rawURL), resp.StatusCode)
	}
	// Reject unexpected Content-Encoding (defense-in-depth — should
	// not appear because we sent Accept-Encoding: identity and the
	// transport has DisableCompression: true).
	if ce := resp.Header.Get("Content-Encoding"); ce != "" && !strings.EqualFold(ce, "identity") {
		return nil, fmt.Errorf("unexpected Content-Encoding %q (compression must be off)", ce)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, marketplaceCacheMaxBodyBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read body: %w", err)
	}
	if int64(len(body)) > marketplaceCacheMaxBodyBytes {
		return nil, fmt.Errorf("body exceeds %d-byte cap (gzip-bomb defense)", marketplaceCacheMaxBodyBytes)
	}
	return &MarketplaceFetchResult{
		Status: resp.StatusCode,
		Body:   body,
		ETag:   resp.Header.Get("ETag"),
	}, nil
}

func redactMarketplaceHTTPError(err error) error {
	// urlredact.ScrubParseError is the single owner of *url.Error
	// credential scrubbing (also used by every url.Parse failure in the
	// marketplace + remote-http validators). net/http wraps transport
	// failures as *url.Error too, so the same primitive covers them.
	return urlredact.ScrubParseError(err)
}
