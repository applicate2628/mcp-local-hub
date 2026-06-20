package urlredact

import "strings"

// MarketplaceURLForError returns a URL-shaped string safe for operator-facing
// validation errors. It strips userinfo from ordinary URLs. When callers pass a
// display URL, the display form is used instead of the expanded wire URL; this
// keeps a secret-expanded host from appearing in diagnostics.
func MarketplaceURLForError(raw string, displayURL ...string) string {
	if len(displayURL) > 0 && displayURL[0] != "" && displayURL[0] != raw {
		return MarketplaceURLForError(displayURL[0])
	}
	schemeEnd := strings.Index(raw, "://")
	if schemeEnd < 0 {
		return raw
	}
	prefix := raw[:schemeEnd+len("://")]
	rest := raw[schemeEnd+len("://"):]
	if rest == "" {
		return raw
	}
	authority := rest
	suffix := ""
	if end := strings.IndexAny(rest, "/?#"); end >= 0 {
		authority = rest[:end]
		suffix = rest[end:]
	}
	if at := strings.LastIndex(authority, "@"); at >= 0 {
		authority = authority[at+1:]
	}
	return prefix + authority + suffix
}
