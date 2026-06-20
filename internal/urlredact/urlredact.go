package urlredact

import (
	"errors"
	"fmt"
	"net/url"
	"strings"
)

// ScrubParseError is the single owner for the recurring "a malformed URL
// reaches an error message via url.Parse" leak class. net/url's *url.Error
// embeds the verbatim input string in both its .URL field and its Error()
// text, so wrapping a url.Parse failure with %w re-prints any embedded
// credentials (e.g. user:pass@) even when the caller redacted its own "got"
// value. Every url.Parse-failure error in the marketplace + remote-http paths
// must pass through here BEFORE being wrapped, so the leak is impossible to
// reintroduce on a new error path.
//
// When err is (or wraps) a *url.Error, it returns a replacement error whose
// Error() rebuilds the message from the operation, the redacted URL
// (MarketplaceURLForError strips userinfo), and the underlying reason —
// preserving errors.Is/As traversal to the original cause. A nil err or a
// non-url.Error is returned unchanged.
func ScrubParseError(err error) error {
	if err == nil {
		return nil
	}
	var urlErr *url.Error
	if !errors.As(err, &urlErr) || urlErr.URL == "" {
		return err
	}
	return &scrubbedParseError{
		msg:   fmt.Sprintf("%s %q: %v", urlErr.Op, MarketplaceURLForError(urlErr.URL), urlErr.Err),
		cause: err,
	}
}

type scrubbedParseError struct {
	msg   string
	cause error
}

func (e *scrubbedParseError) Error() string { return e.msg }
func (e *scrubbedParseError) Unwrap() error { return e.cause }

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
