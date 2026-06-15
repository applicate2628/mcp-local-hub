// health_error_redact.go — g3 capability/health error-detail redaction.
//
// The GUI capability/health screen (internal/gui/frontend Capabilities.tsx,
// CapabilityCard.tsx, CapabilitySection.tsx) renders backend `err` strings
// verbatim into its banners and probe-error pills. When a daemon's own MCP
// error message embeds a filesystem path (e.g. `tools/list: stat
// C:\Users\<name>\secret\token.json: ...`) or a token-like value, that raw
// detail reaches the DOM and leaks via screenshots, browser dev-tools, or
// shared support sessions (NOT an XSS vector — Preact escapes HTML — an
// information-leak one).
//
// redactErrorDetail is the single choke-point applied at the API boundary
// (the owning boundary where the leak originates) so there is ONE source of
// truth on the Go side, per the bug doc's preferred fix #1. The operational
// CATEGORY portion of an error (method name, HTTP status, "timeout",
// "parse: ...") is benign and must survive so operators keep an actionable
// diagnostic; only leak-prone substrings — absolute filesystem paths and
// long token-like runs — are scrubbed. The raw, unredacted detail is logged
// server-side (see callers) for debugging.
//
// Bug: work-items/bugs/2026-05-08-g3-error-text-redaction.md.
package api

import "regexp"

// absPathRE matches absolute filesystem paths likely to carry a username or
// workspace segment: a Windows drive-letter root (`C:\...`, `D:/...`) OR a
// POSIX absolute path with at least two segments (`/home/x`, `/a/b/c`).
//
// The two-segment minimum on the POSIX branch is deliberate: it avoids eating
// benign single-slash tokens that appear in category strings such as
// `tools/list` (relative, no leading slash anyway) or a bare `/mcp` endpoint
// fragment. Windows drive roots are matched whether the separator is `\` or
// `/`. RE2 (Go's stdlib regexp) has no lookbehind, so the patterns are
// anchored on structural characters (drive-colon, leading slash) instead.
//
// Path characters intentionally EXCLUDE whitespace so the match stops at the
// token boundary and the surrounding category text (`stat `, `: no such
// file`) is preserved around the `<redacted-path>` placeholder.
var absPathRE = regexp.MustCompile(`[A-Za-z]:[\\/][^\s:][^\s]*|/[^\s/]+(?:/[^\s]+)+`)

// tokenRE matches a long contiguous alphanumeric run (24+ chars) that is
// almost certainly a credential / token / session id rather than a normal
// English word or a small numeric code (so `HTTP 500`, `EOF`, `timeout` are
// untouched). The hub-mcp credential redactor (hub_mcp_log_redact.go) keys on
// 64-hex specifically; this broader 24+ alnum net covers token-like values a
// misconfigured daemon may surface in an error context (e.g. `sk-...`-style
// keys embedded in an upstream message).
var tokenRE = regexp.MustCompile(`[A-Za-z0-9]{24,}`)

// redactErrorDetail scrubs absolute filesystem paths (→ `<redacted-path>`)
// and long token-like runs (→ `<token>`) from an error-detail string while
// leaving the operational category text intact. Paths are redacted first so a
// path that happens to contain a long alnum segment is replaced wholesale
// rather than leaving a `<token>`-peppered path fragment behind.
func redactErrorDetail(s string) string {
	if s == "" {
		return ""
	}
	s = absPathRE.ReplaceAllString(s, "<redacted-path>")
	s = tokenRE.ReplaceAllString(s, "<token>")
	return s
}
