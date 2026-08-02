package pinstatus

import (
	"errors"
	"net/url"
	"path/filepath"
	"strings"
	"unicode"
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
// So this side stays a positive identifier: a matching key produces
// unknown(remote_url_credential_bearing) only when there is credential evidence.
// Other value-bearing queries are not recategorized as credentials;
// approveRemoteURL rejects them fail-closed as
// unknown(remote_url_query_unclassified) before an approved URL capability can
// reach child-process argv.
//
// Matched case-insensitively as delimited key words, so "access_token" and
// "X-Api-Key" are caught without treating a safe word such as "design" as
// credential evidence merely because it contains the letters "sig".
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

const redactedRemoteMetadata = "REDACTED"

// maxRemoteMetadataDecodePasses bounds opaque metadata decoding. A spelling
// that still changes after this many passes is refused: its eventual form is
// not established within the resource budget, so it cannot gain execution or
// emission authority.
const maxRemoteMetadataDecodePasses = 8

var errRemoteURLApprovalMissing = errors.New("remote URL approval missing")

// approvedRemoteURL is an execution capability, not a sanitized string. The
// private proof pointer makes its zero value invalid, and the struct shape
// prevents string conversion. approveRemoteURL is its sole constructor.
type approvedRemoteURL struct {
	raw   string
	proof *remoteURLApproval
}

type remoteURLApproval struct{}

var remoteURLApprovalProof = &remoteURLApproval{}

func (remote approvedRemoteURL) transportArgument() (string, bool) {
	if remote.proof != remoteURLApprovalProof || remote.raw == "" {
		return "", false
	}
	rechecked, reason := approveRemoteURL(remote.raw)
	if reason != "" || rechecked.proof != remoteURLApprovalProof || rechecked.raw != remote.raw {
		return "", false
	}
	return remote.raw, true
}

// approveRemoteURL is the single admission owner for raw remote strings.
// Credential evidence outranks an otherwise unclassified value-bearing query.
func approveRemoteURL(raw string) (approvedRemoteURL, Reason) {
	if hasEmbeddedCredential(raw) {
		return approvedRemoteURL{}, ReasonRemoteURLCredentialBearing
	}

	parsed, parseErr := url.Parse(raw)
	_, fallbackQuery, fallbackHasQuery, _ := splitURLish(raw)
	rawQuery := fallbackQuery
	hasQuery := fallbackHasQuery
	if parseErr == nil {
		rawQuery = parsed.RawQuery
		hasQuery = parsed.ForceQuery || parsed.RawQuery != ""
	}
	if hasQuery && queryCarriesValue(rawQuery) {
		return approvedRemoteURL{}, ReasonRemoteURLQueryUnclassified
	}
	if !validRemoteURLShape(raw, parsed, parseErr) {
		return approvedRemoteURL{}, ReasonPortfileUnparsable
	}
	return approvedRemoteURL{raw: raw, proof: remoteURLApprovalProof}, ""
}

func queryCarriesValue(rawQuery string) bool {
	for _, part := range strings.Split(rawQuery, "&") {
		_, value, hasValue := strings.Cut(part, "=")
		if hasValue && value != "" {
			return true
		}
	}
	return false
}

// validRemoteURLShape checks every structural component before the value gains
// transport authority. URL, SCP-like, and local-path Git remotes remain valid.
func validRemoteURLShape(raw string, parsed *url.URL, parseErr error) bool {
	if raw == "" || strings.TrimSpace(raw) != raw ||
		strings.IndexFunc(raw, func(r rune) bool { return r < 0x20 || r == 0x7f }) >= 0 {
		return false
	}
	// SCP-like Git form (git@host:owner/repo.git). Go's URL parser rejects
	// the colon in this query-free spelling, so validate it before parseErr.
	if !strings.ContainsAny(raw, "?#") {
		if filepath.IsAbs(raw) {
			return true
		}
		if at := strings.LastIndexByte(raw, '@'); at > 0 {
			if colon := strings.IndexByte(raw[at+1:], ':'); colon > 0 {
				colon += at + 1
				return colon+1 < len(raw)
			}
		}
	}
	if parseErr != nil || parsed == nil || parsed.Fragment != "" {
		return false
	}
	if parsed.Scheme != "" && strings.Contains(raw, "://") {
		if parsed.Path == "" {
			return false
		}
		if !strings.EqualFold(parsed.Scheme, "file") && parsed.Host == "" {
			return false
		}
		return true
	}
	return parsed.Scheme == "" && parsed.Path != ""
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
		redacted := redactUnparsable(raw)
		if redacted != raw || !hasEmbeddedCredential(raw) {
			return redacted
		}
		return redactedRemoteMetadata
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
		if hasEmbeddedCredential(raw) {
			return redactedRemoteMetadata
		}
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
//
// # The empty-value skip is a deliberate wire-verdict change (recorded 2026-07-27)
//
// A parameter with an EMPTY value (`?token=`) is skipped, and so is one with no
// "=" at all (`?token`). The predecessor predicate — `redactQuery(...) changed`
// — skipped only the second, so it refused the first:
//
//	https://host/repo.git?token=   OLD: refused   NEW: queried
//	https://host/re%zzpo?token=    OLD: refused   NEW: queried
//
// (measured this session by running the pre-37320feb predicate verbatim against
// the current one.) 37320feb's own message asserted "the argv verdict is unmoved
// in BOTH directions" and named `?access_token=` as a still-refused example; that
// spelling is empty-valued, so it is one of the cases that moved. The claim was
// wrong and is corrected here rather than in a commit nobody re-reads.
//
// The NEW behaviour is the correct one and is kept. This predicate's answer
// becomes the closed wire verdict unknown(remote_url_credential_bearing), whose
// contract in types.go is that the URL "embeds a credential". `?token=` embeds
// NOTHING — there is no value, so there is nothing to leak through argv, which is
// the only channel this refusal exists to close (see the file header). Refusing it
// asserted a credential about an empty string: a conclusion, not an observation,
// and the same fabricated-verdict class the rest of this file is built to prevent.
// It also cost the caller a real answer — a live ls-remote that would have
// succeeded — for no security gain.
//
// The skip is therefore the SAME rule redactQuery applies one function up, for
// the mirrored reason: emission must not invent a secret that was not there, and
// refusal must not assert one.
func queryCarriesArgvSecret(rawQuery string) bool {
	for _, part := range strings.Split(rawQuery, "&") {
		key, value, hasValue := strings.Cut(part, "=")
		if !hasValue || value == "" || !keyNamesArgvSecret(key) {
			continue
		}
		return true
	}
	return false
}

func keyNamesArgvSecret(key string) bool {
	for _, word := range credentialKeyWords(key) {
		for _, candidate := range argvSecretQueryKeys {
			if word == candidate {
				return true
			}
		}
	}
	return false
}

func credentialKeyWords(key string) []string {
	var words []string
	var word []rune
	flush := func() {
		if len(word) != 0 {
			words = append(words, strings.ToLower(string(word)))
			word = nil
		}
	}
	var previous rune
	for _, r := range key {
		if !unicode.IsLetter(r) && !unicode.IsDigit(r) {
			flush()
			previous = 0
			continue
		}
		if len(word) != 0 && unicode.IsUpper(r) && (unicode.IsLower(previous) || unicode.IsDigit(previous)) {
			flush()
		}
		word = append(word, r)
		previous = r
	}
	flush()
	return words
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
		return opaqueRemoteMetadataCarriesCredential(raw)
	}
	// Unparsable: fall back to the same crude authority+query decomposition
	// redaction uses (splitURLish), so a URL we cannot model is still examined
	// rather than trusted. The QUESTION asked of it is this file's own
	// positive credential predicate — deliberately not "did redaction change
	// anything", which would make this wire verdict a shadow of the emission
	// rule (see splitURLish).
	body, _, _, _ := splitURLish(raw)
	if redactUnparsableUserinfo(body) != body {
		return true
	}
	return opaqueRemoteMetadataCarriesCredential(raw)
}

// redactRemote returns a copy of r whose URL and source metadata are safe to
// emit. Repo normally remains its copy-pasteable owner/path spelling, but a
// malformed value is still a secret carrier and must never bypass this owner.
func redactRemote(r Remote) Remote {
	rawRepo := r.Repo
	r.Repo = redactRemoteRepo(rawRepo)
	if rawRepo != "" && r.Repo != rawRepo {
		r.URL = strings.ReplaceAll(r.URL, rawRepo, r.Repo)
	}
	r.URL = redactURL(r.URL)
	return r
}

func redactRemoteRepo(raw string) string {
	if raw == "" {
		return ""
	}
	if redacted := redactURL(raw); redacted != raw {
		return redacted
	}
	if hasEmbeddedCredential(raw) {
		return redactedRemoteMetadata
	}
	return raw
}

// remoteRepoAdmissionReason closes the execution channel for malformed source
// metadata that is not expressible as URL userinfo/query. GitHub and GitLab
// build their remote URL around REPO, so a value such as user:password@host
// would otherwise be visible in the child argv even though URL parsing sees it
// only as a path. This is positive credential evidence, not a general
// validation rule; ordinary project paths keep their established behavior.
func remoteRepoAdmissionReason(raw string) Reason {
	if hasEmbeddedCredential(raw) {
		return ReasonRemoteURLCredentialBearing
	}
	return ""
}

// opaqueRemoteMetadataCarriesCredential recognizes credential-shaped key/value
// carriers in every non-userinfo remote channel. It is deliberately positive:
// ordinary owner/path repository names contain no key/value evidence and stay
// usable. Decoding continues to a stable spelling within a fixed bound; a
// spelling that exceeds that bound fails closed rather than relying on a
// particular encoding depth.
func opaqueRemoteMetadataCarriesCredential(raw string) bool {
	spellings, complete := decodedRemoteMetadataSpellings(raw)
	if !complete {
		return true
	}
	for _, spelling := range spellings {
		if opaqueAuthorityCarriesCredential(spelling) {
			return true
		}
		for _, part := range strings.FieldsFunc(spelling, func(r rune) bool {
			return r == '&' || r == ';' || r == ',' || r == '/' || r == '?' || r == '#'
		}) {
			key, value, hasValue := strings.Cut(part, "=")
			if !hasValue {
				key, value, hasValue = strings.Cut(part, ":")
			}
			if hasValue && value != "" && keyNamesArgvSecret(key) {
				return true
			}
		}
	}
	return false
}

// opaqueAuthorityCarriesCredential catches bare-user and user:password@host
// spellings that url.Parse treats as non-URL opaque values. A colon after the
// at sign remains the SCP remote shape (git@host:path), so it stays outside
// this credential form along with host:port and IPv6.
func opaqueAuthorityCarriesCredential(raw string) bool {
	at := strings.LastIndexByte(raw, '@')
	if at <= 0 {
		return false
	}
	prefix := raw[:at]
	if scheme := strings.LastIndex(prefix, "://"); scheme >= 0 {
		prefix = prefix[scheme+3:]
	}
	colon := strings.LastIndexByte(prefix, ':')
	if colon > 0 && colon+1 < len(prefix) {
		return true
	}
	return !strings.Contains(raw[at+1:], ":")
}

func decodedRemoteMetadataSpellings(raw string) ([]string, bool) {
	spellings := []string{raw}
	for range maxRemoteMetadataDecodePasses {
		decoded, err := url.PathUnescape(spellings[len(spellings)-1])
		if err != nil || decoded == spellings[len(spellings)-1] {
			return spellings, true
		}
		spellings = append(spellings, decoded)
	}
	decoded, err := url.PathUnescape(spellings[len(spellings)-1])
	return spellings, err != nil || decoded == spellings[len(spellings)-1]
}

func redactResult(r Result) Result {
	if len(r.Ports) == 0 {
		return r
	}
	r.Ports = append([]PortResult(nil), r.Ports...)
	for i := range r.Ports {
		r.Ports[i] = redactPortResult(r.Ports[i])
	}
	return r
}

func redactPortResult(port PortResult) PortResult {
	rawRemoteURL := port.Remote.URL
	rawRepo := port.Remote.Repo
	port.Remote = redactRemote(port.Remote)
	port.Candidates = redactCandidates(port.Candidates)
	port.CompareURL = redactURL(port.CompareURL)
	if rawRepo != "" && rawRepo != port.Remote.Repo {
		port.CompareURL = strings.ReplaceAll(port.CompareURL, rawRepo, port.Remote.Repo)
	}
	if len(port.Evidence.Commands) != 0 {
		port.Evidence.Commands = append([]string(nil), port.Evidence.Commands...)
		for i, command := range port.Evidence.Commands {
			if rawRemoteURL != "" && rawRemoteURL != port.Remote.URL {
				command = strings.ReplaceAll(command, rawRemoteURL, port.Remote.URL)
			}
			if rawRepo != "" && rawRepo != port.Remote.Repo {
				command = strings.ReplaceAll(command, rawRepo, port.Remote.Repo)
			}
			port.Evidence.Commands[i] = redactEvidenceCommand(command)
		}
	}
	return port
}

// redactEvidenceCommand is the final-wire owner for each evidence command.
// Commands are independent public strings: they may come from a cache-like
// result and need not repeat Remote.URL or Remote.Repo. Credential evidence
// therefore redacts the complete command before ordinary URL redaction handles
// value-bearing query text.
func redactEvidenceCommand(command string) string {
	if hasEmbeddedCredential(command) {
		return redactedRemoteMetadata
	}
	return redactURL(command)
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
