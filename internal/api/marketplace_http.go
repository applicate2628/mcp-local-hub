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
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	marketplaceHTTPTimeout       = 15 * time.Second
	marketplaceCacheMaxBodyBytes = 10 * 1024 * 1024
)

// rejectNonHTTPSRedirect refuses any redirect target that is not
// https://. Used by both production and test clients.
func rejectNonHTTPSRedirect(req *http.Request, via []*http.Request) error {
	if req.URL.Scheme != "https" {
		return fmt.Errorf("refusing redirect from %s to non-https URL %s", via[len(via)-1].URL, req.URL)
	}
	if len(via) >= 10 {
		return errors.New("too many redirects")
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

func newMarketplaceClient() *http.Client {
	return &http.Client{
		Transport:     newMarketplaceTransport(),
		CheckRedirect: rejectNonHTTPSRedirect,
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

// forbiddenMarketplaceHeaders enumerates auth/cookie headers that a
// caller MUST NOT smuggle into a marketplace fetch. The threat model
// is an unauthenticated GET against a curated public registry; any
// Authorization/Cookie/Proxy-Authorization header carries operator
// credentials that should never be sent to whatever URL the operator
// (or a downstream caller) happens to pass through --registry.
//
// codex deep-sec PR #163 lane 1 P2 closure: prior to this fix
// extraHeaders was set unfiltered, so a future internal caller could
// accidentally leak credentials.
//
// Header names compared lowercase since http.Header canonicalizes on
// set; we check the lowered key before letting it through.
var forbiddenMarketplaceHeaders = map[string]struct{}{
	"authorization":       {},
	"cookie":              {},
	"proxy-authorization": {},
}

// MarketplaceFetchWithClient is the injectable form. Tests pass a
// client with a TLS test transport. Production callers go through
// MarketplaceFetch.
//
// `extraHeaders` is permitted only for non-credentialed metadata
// (e.g. a future User-Agent override). Authorization, Cookie, and
// Proxy-Authorization are dropped with an error before the request
// is built.
func MarketplaceFetchWithClient(ctx context.Context, client *http.Client, rawURL, ifNoneMatch string, extraHeaders map[string]string) (*MarketplaceFetchResult, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("parse url %q: %w", rawURL, err)
	}
	if u.Scheme != "https" {
		return nil, fmt.Errorf("marketplace url must be https:// (got scheme %q)", u.Scheme)
	}
	// codex r6 P1 closure (PR #163): reject URLs that embed
	// credentials (https://user:pass@host/...). Go's net/http
	// auto-emits a Basic Authorization header from url.URL.User on
	// the outbound request, which would bypass the
	// forbiddenMarketplaceHeaders denylist below. The marketplace
	// threat model is an unauthenticated GET against a public
	// registry; allowing URL-embedded credentials would silently
	// leak them to whatever host the operator passed.
	if u.User != nil {
		return nil, fmt.Errorf("marketplace url must not embed credentials (got userinfo before host); registry fetches are unauthenticated")
	}
	// Reject credential-bearing headers BEFORE building the request
	// (defense-in-depth: if a future caller passes Authorization,
	// fail loud, do not silently strip — the caller's intent is
	// either a programming error or a privilege escalation).
	for k := range extraHeaders {
		if _, banned := forbiddenMarketplaceHeaders[strings.ToLower(k)]; banned {
			return nil, fmt.Errorf("refusing to send credential-bearing header %q to marketplace registry — fetches are unauthenticated GETs", k)
		}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
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
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotModified {
		return &MarketplaceFetchResult{Status: resp.StatusCode, NotMod: true, ETag: resp.Header.Get("ETag")}, nil
	}
	if resp.StatusCode != http.StatusOK {
		return &MarketplaceFetchResult{Status: resp.StatusCode}, fmt.Errorf("fetch %s: HTTP %d", rawURL, resp.StatusCode)
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
