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

// secretQueryKeys are query-parameter names whose VALUE is redacted. Matched
// case-insensitively against the whole key, and as a substring, so
// "access_token" and "X-Api-Key" are both caught.
var secretQueryKeys = []string{
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
// still scrubbed of anything that looks like userinfo, because failing to
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

// redactQuery replaces the value of every secret-shaped parameter. It parses
// manually rather than via url.ParseQuery so that a malformed query string
// is still scrubbed instead of silently passed through whole.
func redactQuery(rawQuery string) (string, bool) {
	parts := strings.Split(rawQuery, "&")
	changed := false
	for i, part := range parts {
		key, _, hasValue := strings.Cut(part, "=")
		if !hasValue || !isSecretQueryKey(key) {
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

func isSecretQueryKey(key string) bool {
	lower := strings.ToLower(key)
	for _, candidate := range secretQueryKeys {
		if strings.Contains(lower, candidate) {
			return true
		}
	}
	return false
}

// redactUnparsable is the fail-closed path for a string url.Parse rejected.
// It cannot reason about structure, so it applies the crude rule that is
// still sound: everything between the scheme separator and the LAST '@'
// before the first '/' of the path is userinfo, and is dropped.
func redactUnparsable(raw string) string {
	schemeEnd := strings.Index(raw, "://")
	if schemeEnd < 0 {
		return raw
	}
	rest := raw[schemeEnd+3:]
	authorityEnd := strings.IndexAny(rest, "/?#")
	authority := rest
	tail := ""
	if authorityEnd >= 0 {
		authority = rest[:authorityEnd]
		tail = rest[authorityEnd:]
	}
	at := strings.LastIndex(authority, "@")
	if at < 0 {
		return raw
	}
	return raw[:schemeEnd+3] + redactedUserinfo + "@" + authority[at+1:] + tail
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
		if parsed.RawQuery != "" {
			if _, changed := redactQuery(parsed.RawQuery); changed {
				return true
			}
		}
		return false
	}
	// Unparsable: fall back to the same crude authority scan redaction uses,
	// so a URL we cannot model is refused rather than trusted.
	return redactUnparsable(raw) != raw
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
