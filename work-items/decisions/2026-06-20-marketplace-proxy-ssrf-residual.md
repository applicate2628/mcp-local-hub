---
status: accepted
date: 2026-06-20
slug: marketplace-proxy-ssrf-residual
deciders: architect (opus) + lead
pr: 388
---

# Marketplace registry fetch honors the proxy by default; airtight direct-fetch is an opt-in

## Context

The marketplace catalog FETCH (`internal/api/marketplace_http.go`) pulls a JSON catalog from a
registry URL (default: the project's own GitHub raw URL; operator may override via `--registry`).
A multi-round SSRF hardening added a dial-time IP guard (`marketplaceFetchDialControlContext` →
`rejectMarketplaceLocalOrPrivateAddr`) that rejects a connection whose RESOLVED IP is
loopback/private/link-local/CGNAT/etc. Round 4 set `t.Proxy = nil` so the fetch goes DIRECT and the
dial-time check validates the actual origin — SSRF-airtight, but it BREAKS the marketplace on
corporate hosts where outbound HTTPS is only reachable via `HTTPS_PROXY`.

## Decision (architect, option C — repo-consistent polarity)

**Honor the proxy by DEFAULT; the airtight `Proxy=nil` is an explicit opt-in.**

- Remove the unconditional `t.Proxy = nil`; inherit `ProxyFromEnvironment`. This matches the repo's
  invariant polarity — every existing gate (`MCPHUB_REQUIRE_SINGLE_USER_HOME`,
  `MCPHUB_ALLOW_UNHARDENED_CLIENT_WRITE`, `MCPHUB_STRICT_JOB_PROTECTION`) defaults OPERABLE and makes
  HARDENING the opt-in. (Rejected option D — a `*_ALLOW_PROXY` default-off gate — because it inverts
  that polarity: it would make the feature broken-on-corp by default.)
- Add a default-OFF env gate `MCPHUB_MARKETPLACE_DIRECT_FETCH=1` that RESTORES `Proxy=nil` for an
  operator who wants the airtight direct-IP guarantee and needs no proxy. The var opts INTO
  hardening (inverse of D). One const + one bool resolver, resolved once in the upper layer, threaded
  as a bool — the transport builder reads no ambient env (mirrors the secure-write seam).
- A proxied fetch emits one `warn` event (hub-mcp event-log channel) naming the residual + the opt-in.

## SSRF floor that stays unconditional (why honoring the proxy is acceptable)

The static URL + redirect host check (`validateMarketplacePublicHTTPSParsedURL` +
`rejectUnsafeMarketplaceRedirect`) ALREADY runs on every fetch and every redirect Location — it
blocks a literal loopback/private host in the registry URL on BOTH the proxy and direct paths. The
dial-time resolved-IP guard, however, is applied ONLY on the DIRECT transport
(`configureMarketplaceFetchDialer(direct, resolver, true)`), where it validates the resolved ORIGIN
IP; on the proxied transport it is intentionally NOT applied
(`configureMarketplaceFetchDialer(proxied, resolver, false)`) because it would reject a normal
corporate proxy's own loopback/RFC1918 address rather than the origin. So on the proxy path the
static URL + redirect host check is the authoritative SSRF floor; honoring the proxy does not remove
that floor — it only means the dial-time origin-IP check is unavailable on the proxy path (the
accepted residual below).

## Accepted residual

With the proxy honored (default), the dial-time IP guard validates the proxy's address, not the
origin — so a registry the operator points at could DNS-rebind so the proxy connects to an internal
host the static check could not pre-classify. Accepted because: (a) only reachable by an operator
who chose a non-default registry; (b) credential headers are stripped (blind SSRF, no exfil);
(c) the static URL+redirect check still blocks literal-private targets on the proxy path; (d) the
operator who cannot tolerate it has the one-env-var `MCPHUB_MARKETPLACE_DIRECT_FETCH` opt-in;
(e) the alternative (`Proxy=nil` default) is a fully-dead feature on the corp host class this repo
otherwise supports everywhere.

## Protected surfaces (unchanged)

`rejectMarketplaceLocalOrPrivateAddr/Host`, `validateMarketplacePublicHTTPSParsedURL`,
`rejectUnsafeMarketplaceRedirect`, `marketplaceFetchDialControlContext`, the credential-header
stripping, the 10 MB cap. Full architect package in the PR #388 review thread / this session's
`.reports`.
