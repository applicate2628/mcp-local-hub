package urlredact

import (
	"net/url"
	"strings"
)

// MarketplaceURLForError returns a URL-shaped string safe for operator-facing
// validation errors. It strips userinfo from ordinary URLs. When callers pass a
// display URL, the display form is used instead of the expanded wire URL; this
// keeps a secret-expanded host from appearing in diagnostics.
func MarketplaceURLForError(raw string, displayURL ...string) string {
	if len(displayURL) > 0 && displayURL[0] != "" && displayURL[0] != raw {
		return MarketplaceURLForError(displayURL[0])
	}
	if redacted, ok := redactAuthorityUserinfo(raw); ok {
		return redacted
	}
	if u, err := url.Parse(raw); err == nil && u.User != nil {
		safe := *u
		safe.User = nil
		return safe.String()
	}
	return raw
}

func redactAuthorityUserinfo(raw string) (string, bool) {
	schemeEnd := strings.Index(raw, "://")
	prefix := ""
	rest := ""
	switch {
	case schemeEnd >= 0:
		prefix = raw[:schemeEnd+len("://")]
		rest = raw[schemeEnd+len("://"):]
	case strings.HasPrefix(raw, "//"):
		prefix = "//"
		rest = raw[len("//"):]
	default:
		return "", false
	}
	if rest == "" {
		return "", false
	}
	authority := rest
	suffix := ""
	if end := strings.IndexAny(rest, "/?#"); end >= 0 {
		authority = rest[:end]
		suffix = rest[end:]
	}
	if at := strings.LastIndex(authority, "@"); at >= 0 {
		authority = authority[at+1:]
		return prefix + authority + suffix, true
	}
	return "", false
}
