package pinstatus

import (
	"net/url"
	"strings"
)

// This file is the SINGLE OWNER of credential handling for remote URLs.
//
// A portfile may legitimately carry a credential-bearing remote —
// `vcpkg_from_git(URL https://user:token@host/repo.git ...)` is valid CMake,
// and GITLAB_URL can carry the same. That URL then reaches THREE separate
// emission points in one PortResult (Remote.URL, Evidence.Commands, and the
// gitlab CompareURL, which is derived from Remote.URL) plus a fourth in every
// audited FetchCandidate — and an MCP result is not a local log: it is copied
// into a model transcript, a provider's request log, and whatever the caller
// persists. Patching the emission points individually is how the fourth one
// gets missed, so every string that could carry a credential passes through
// redactURL here, and NOTHING else in this package formats a remote URL.
//
// Redaction alone is not sufficient, which is why hasEmbeddedCredential
// exists alongside it: a credential passed to `git ls-remote` becomes an
// argv entry, visible in the process table to every local account for the
// life of the child. Redacting our own output would not close that channel,
// so the query is refused outright (ReasonRemoteURLCredentialBearing) and
// redaction is the defence-in-depth that guarantees no field can leak the
// value even if a future path forgets the refusal.

// redactedUserinfo replaces the userinfo component. It is deliberately not
// the empty string: a caller comparing two results must still be able to see
// THAT a credential was present, just never what it was.
const redactedUserinfo = "REDACTED"

// emitSafeQueryKeys is the ALLOWLIST of query-parameter names whose VALUE may
// be emitted verbatim. It is EMPTY, and that is the design, not an oversight.
//
// Emission used to be governed by a DENYLIST of secret-shaped key names
// (token, secret, password, key, auth, sig, ...). That is the wrong polarity
// for this side of the file: the default outcome for an unenumerated name was
// "print the value". The set of credential-shaped parameter names is open, so
// the denylist leaked, by construction, every one of these real spellings:
//
//	?code=...        OAuth 2.0 authorization code
//	?jwt=...         a bare JSON Web Token
//	?assertion=...   RFC 7523 JWT bearer assertion
//	?pat=...         Azure DevOps personal access token
//	?session=... / ?sid=...   a session identifier
//	?ticket=...      CAS / Kerberos service ticket
//	?refresh=...     OAuth refresh token
//
// none of which contains "token", "secret", "key" or "auth" as a substring.
//
// The two failure directions are not symmetric, which is what decides the
// polarity:
//
//   - Leaking is IRREVERSIBLE. The file header states the reason: an MCP
//     result "is copied into a model transcript, a provider's request log, and
//     whatever the caller persists".
//   - Over-redacting is RECOVERABLE and shallow. The KEY is always preserved
//     (only the value becomes REDACTED), so the operator still sees the URL's
//     scheme, host, path and exactly which parameters were present, and the
//     portfile they already have carries the original.
//
// And the measured cost of over-redaction on real input is zero: across the
// 2856 portfiles of a current vcpkg checkout, 124 vcpkg_from_git / GITLAB_URL
// fetch-URL lines carry a query string in exactly 0 cases (measured
// 2026-07-27, C:\vcpkg). A git remote URL has no query to lose.
//
// A future parameter that is genuinely safe to print is added here with its
// own justification — a deliberate edit, rather than a name nobody thought to
// put on a ban list.
var emitSafeQueryKeys = map[string]struct{}{}

// argvSecretQueryKeys is a SEPARATE, deliberately different list: the positive
// credential identifier used ONLY by hasEmbeddedCredential, which decides
// whether to REFUSE the git ls-remote query outright.
//
// It is not the emission rule and must not be merged into it, because the two
// answer different questions with opposite costs:
//
//   - Emission asks "is it safe to PRINT this value" — over-answering costs a
//     value the operator can recover from the portfile, so it fails closed.
//   - This asks "does this URL EMBED A CREDENTIAL" — the answer becomes the
//     wire verdict unknown(remote_url_credential_bearing), whose contract in
//     types.go is that the URL "embeds a credential". Applying the emission
//     rule here would make the tool assert that fact about `?depth=1`: a
//     conclusion, not an observation, and exactly the fabricated-verdict class
//     this server exists to eliminate.
//
// So this side stays a positive identifier and therefore stays INCOMPLETE: the
// spellings listed in emitSafeQueryKeys' comment above (code, jwt, assertion,
// pat, session, ticket, refresh) are still handed to `git ls-remote`'s argv,
// where the child's full command line is readable by every local account for
// its lifetime. That residual is REAL and is NOT closed here; closing it means
// refusing every query-bearing remote URL, which needs a reason name that says
// "unclassifiable query parameter" rather than "credential", i.e. a change to
// the closed wire enum. Tracked as
// work-items/bugs/2026-07-27-pinstatus-argv-refusal-is-a-credential-denylist.md.
//
// Matched case-insensitively against the whole key, and as a substring, so
// "access_token" and "X-Api-Key" are both caught.
var argvSecretQueryKeys = []string{
	"token",
	"secret",
	"password",
	"passwd",
	"pwd",
	"key",
	"auth",
	"credential",
	"sig",
	"signature",
}

// redactURL returns raw with any embedded userinfo and any secret-shaped
// query-parameter value replaced. It is total: a URL that does not parse is
// still scrubbed of BOTH channels (see redactUnparsable), because failing to
// parse must never mean failing to redact.
func redactURL(raw string) string {
	if raw == "" {
		return ""
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		return redactUnparsable(raw)
	}
	changed := false
	if parsed.User != nil {
		parsed.User = url.User(redactedUserinfo)
		changed = true
	}
	if parsed.RawQuery != "" {
		if q, qChanged := redactQuery(parsed.RawQuery); qChanged {
			parsed.RawQuery = q
			changed = true
		}
	}
	if !changed {
		// Return the ORIGINAL spelling rather than url.String()'s
		// re-encoding: an unchanged URL must round-trip byte-for-byte so a
		// caller can paste it, and so tests compare against what the
		// portfile actually wrote.
		return raw
	}
	return parsed.String()
}

// redactQuery replaces the value of every parameter whose key is not on the
// emitSafeQueryKeys allowlist. It parses manually rather than via
// url.ParseQuery so that a malformed query string is still scrubbed instead
// of silently passed through whole.
//
// A parameter with no "=" carries no value and is left alone; so is one whose
// value is already empty, because redacting nothing into "REDACTED" would
// invent a secret that was not there.
func redactQuery(rawQuery string) (string, bool) {
	parts := strings.Split(rawQuery, "&")
	changed := false
	for i, part := range parts {
		key, value, hasValue := strings.Cut(part, "=")
		if !hasValue || value == "" || isEmitSafeQueryKey(key) {
			continue
		}
		parts[i] = key + "=" + redactedUserinfo
		changed = true
	}
	if !changed {
		return rawQuery, false
	}
	return strings.Join(parts, "&"), true
}

// isEmitSafeQueryKey reports whether this parameter's VALUE may be printed.
func isEmitSafeQueryKey(key string) bool {
	_, ok := emitSafeQueryKeys[strings.ToLower(strings.TrimSpace(key))]
	return ok
}

// queryCarriesArgvSecret reports whether a raw query string holds a parameter
// this package positively identifies as a credential. See argvSecretQueryKeys
// for why this is a separate — and knowingly incomplete — predicate rather
// than the emission rule above.
func queryCarriesArgvSecret(rawQuery string) bool {
	for _, part := range strings.Split(rawQuery, "&") {
		key, value, hasValue := strings.Cut(part, "=")
		if !hasValue || value == "" {
			continue
		}
		lower := strings.ToLower(key)
		for _, candidate := range argvSecretQueryKeys {
			if strings.Contains(lower, candidate) {
				return true
			}
		}
	}
	return false
}

// redactUnparsable is the fail-closed path for a string url.Parse rejected.
//
// It must scrub EVERY channel the parse path scrubs, because "the URL did not
// parse" is not a reason to emit a secret — it is the case where we understand
// LEAST and must therefore redact MOST. url.Parse rejects a URL for reasons
// that have nothing to do with the query (an invalid %-escape in the path, a
// control character, a space in the host, a bad escape in the fragment), so a
// credential-bearing query survives that rejection intact. Scrubbing only the
// userinfo here — which is what this function used to do — meant
// "https://host/re%zzpo?access_token=SECRET" was returned VERBATIM.
//
// It cannot reason about structure, so it applies the crude rules that are
// still sound on any RFC-3986-shaped string:
//
//   - fragment: everything from the first '#' is split off untouched ('#'
//     terminates the query, so a '?' after it is not a query delimiter);
//   - query: everything after the first '?' in the remainder goes through the
//     SAME redactQuery the parse path uses — one owner for the secret-key rule;
//   - userinfo: everything between the scheme separator and the LAST '@'
//     before the first '/' of the path is userinfo, and is dropped.
//
// Unlike the previous version this never early-returns the raw string on a
// missing scheme or a missing '@': those only mean there is no userinfo to
// drop, not that there is no query to scrub.
func redactUnparsable(raw string) string {
	body, query, hasQuery, fragment := splitURLish(raw)

	body = redactUnparsableUserinfo(body)
	if hasQuery {
		if scrubbed, changed := redactQuery(query); changed {
			query = scrubbed
		}
	}

	out := body
	if hasQuery {
		out += "?" + query
	}
	return out + fragment
}

// splitURLish is the single owner of the crude RFC-3986-shaped split used by
// every unparsable-URL path: '#' terminates the query (so a '?' after it is
// not a query delimiter), and the first '?' in the remainder starts it.
//
// It exists so redaction and the argv-refusal predicate ask their two
// different questions about the SAME decomposition. Before it, the refusal
// path asked its question as `redactUnparsable(raw) != raw`, which silently
// bound the wire verdict to whatever the redaction rule happened to be — so
// making redaction total would have flipped every query-bearing URL into
// unknown(remote_url_credential_bearing) with no edit to the refusal at all.
func splitURLish(raw string) (body, query string, hasQuery bool, fragment string) {
	body, fragment = raw, ""
	if hash := strings.IndexByte(raw, '#'); hash >= 0 {
		body, fragment = raw[:hash], raw[hash:]
	}
	if q := strings.IndexByte(body, '?'); q >= 0 {
		query, hasQuery = body[q+1:], true
		body = body[:q]
	}
	return body, query, hasQuery, fragment
}

// redactUnparsableUserinfo drops the userinfo component of an unparsable URL
// whose query and fragment have already been split off by redactUnparsable.
func redactUnparsableUserinfo(body string) string {
	schemeEnd := strings.Index(body, "://")
	if schemeEnd < 0 {
		return body
	}
	rest := body[schemeEnd+3:]
	authority := rest
	tail := ""
	if authorityEnd := strings.IndexByte(rest, '/'); authorityEnd >= 0 {
		authority = rest[:authorityEnd]
		tail = rest[authorityEnd:]
	}
	at := strings.LastIndex(authority, "@")
	if at < 0 {
		return body
	}
	return body[:schemeEnd+3] + redactedUserinfo + "@" + authority[at+1:] + tail
}

// hasEmbeddedCredential reports whether raw carries a credential this package
// refuses to hand to a child process (see the file comment: argv is visible
// process-wide). Userinfo counts even when it is a bare username, because
// "https://token@host/..." is exactly how personal-access tokens are written.
func hasEmbeddedCredential(raw string) bool {
	if raw == "" {
		return false
	}
	if parsed, err := url.Parse(raw); err == nil {
		if parsed.User != nil {
			return true
		}
		return queryCarriesArgvSecret(parsed.RawQuery)
	}
	// Unparsable: fall back to the same crude authority+query decomposition
	// redaction uses (splitURLish), so a URL we cannot model is still examined
	// rather than trusted. The QUESTION asked of it is this file's own
	// positive credential predicate — deliberately not "did redaction change
	// anything", which would make this wire verdict a shadow of the emission
	// rule (see splitURLish).
	body, query, _, _ := splitURLish(raw)
	if redactUnparsableUserinfo(body) != body {
		return true
	}
	return queryCarriesArgvSecret(query)
}

// redactRemote returns a copy of r whose URL is safe to emit.
func redactRemote(r Remote) Remote {
	r.URL = redactURL(r.URL)
	return r
}

// redactCandidates returns a copy of candidates whose every Remote.URL is
// safe to emit. Candidates include NON-selected fetch calls, which is exactly
// why they need the same treatment: a credential can sit on a call this run
// never queried.
func redactCandidates(candidates []FetchCandidate) []FetchCandidate {
	if len(candidates) == 0 {
		return candidates
	}
	out := make([]FetchCandidate, len(candidates))
	for i, c := range candidates {
		c.Remote = redactRemote(c.Remote)
		out[i] = c
	}
	return out
}
