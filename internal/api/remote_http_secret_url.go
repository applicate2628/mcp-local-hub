package api

import (
	"fmt"
	"net/netip"
	"net/url"
	"strconv"
	"strings"

	"mcp-local-hub/internal/config"
	"mcp-local-hub/internal/urlredact"
)

func expandRemoteHTTPURLSecrets(raw string, lookup SecretLookup) (string, error) {
	expanded, err := ExpandSecrets(raw, lookup)
	if err != nil {
		return "", err
	}
	if !config.RemoteHTTPURLHasSecretPlaceholderHost(raw) {
		return expanded, nil
	}
	// Validate the HOST SECRET VALUE directly, by expanding ONLY the leading
	// host placeholder. Earlier code derived the host by positional
	// comparison of the display and fully-expanded URLs, which broke when a
	// SECOND ${secret:...} elsewhere in the path/query also expanded (bot PR
	// #388 r10: remote_http_secret_url.go:75) — the suffix no longer matched
	// the unexpanded display suffix. Isolating and validating the host value
	// alone both fixes that false rejection AND keeps catching a host secret
	// that injects an authority delimiter (e.g. expands to "host/evil"),
	// because that delimiter is now inspected as part of the host value
	// rather than being silently truncated into the path.
	hostValue, err := expandRemoteHTTPPlaceholderHostValue(raw, lookup)
	if err != nil {
		return "", err
	}
	if err := validateExpandedRemoteHTTPPlaceholderHost(raw, expanded, hostValue); err != nil {
		return "", err
	}
	return expanded, nil
}

// expandRemoteHTTPPlaceholderHostValue expands ONLY the leading
// ${secret:KEY} host placeholder of a placeholder-host remote-http URL and
// returns the secret's value (the bare host the operator stored in the
// vault). Caller has already established via RemoteHTTPURLHasSecretPlaceholderHost
// that the authority is exactly one leading placeholder plus an optional
// literal :port tail, so the leading placeholder is the host.
func expandRemoteHTTPPlaceholderHostValue(displayURL string, lookup SecretLookup) (string, error) {
	const prefix = "https://"
	rest := displayURL[len(prefix):]
	authority := rest
	if end := strings.IndexAny(rest, "/?#"); end >= 0 {
		authority = rest[:end]
	}
	match := SecretPlaceholderRE.FindStringSubmatch(authority)
	if len(match) < 2 {
		return "", fmt.Errorf("placeholder host is malformed")
	}
	// Expand the single host placeholder through ExpandSecrets so it shares
	// the missing-key and CRLF-injection guards with the rest of the URL.
	hostValue, err := ExpandSecrets(match[0], lookup)
	if err != nil {
		return "", err
	}
	return hostValue, nil
}

func validateExpandedRemoteHTTPPlaceholderHost(displayURL, expandedURL, expandedHost string) error {
	if expandedHost == "" {
		return fmt.Errorf("expanded remote-http host for %s is invalid: expanded host is empty", urlredact.MarketplaceURLForError(expandedURL, displayURL))
	}
	if err := validateExpandedRemoteHTTPHostAuthority(expandedHost); err != nil {
		return fmt.Errorf("expanded remote-http host for %s is invalid: %w", urlredact.MarketplaceURLForError(expandedURL, displayURL), err)
	}
	u, err := url.Parse(expandedURL)
	if err != nil {
		return scrubbedURLErrorf(err, expandedURL, displayURL, "expanded remote-http url is invalid")
	}
	if u.Scheme != "https" {
		return fmt.Errorf("expanded remote-http url must use https:// (got scheme %q)", u.Scheme)
	}
	if u.Hostname() == "" {
		return fmt.Errorf("expanded remote-http url must include a host")
	}
	if u.User != nil {
		return fmt.Errorf("expanded remote-http url must not embed credentials")
	}
	if err := rejectMarketplaceLocalOrPrivateHost(u.Hostname()); err != nil {
		return fmt.Errorf("expanded remote-http url host rejected for %s: %s", urlredact.MarketplaceURLForError(expandedURL, displayURL), redactExpandedSecretHostReason(err.Error(), u.Hostname()))
	}
	return nil
}

func validateExpandedRemoteHTTPHostAuthority(authority string) error {
	if strings.ContainsAny(authority, "/@#?") {
		return fmt.Errorf("contains path, query, fragment, or authority delimiter")
	}
	if r, ok := firstUnsafeMarketplaceTextRune(authority); ok {
		return fmt.Errorf("contains unsafe control or bidi rune U+%04X", r)
	}
	host, port, err := splitExpandedRemoteHTTPHostAuthority(authority)
	if err != nil {
		return err
	}
	if port != "" {
		portNumber, err := strconv.Atoi(port)
		if !isDecimalPort(port) || err != nil || portNumber > 65535 {
			return fmt.Errorf("port is invalid")
		}
	}
	hostForIP := host
	if i := strings.LastIndexByte(hostForIP, '%'); i >= 0 {
		hostForIP = hostForIP[:i]
	}
	if _, err := netip.ParseAddr(strings.TrimSuffix(hostForIP, ".")); err == nil {
		return nil
	}
	if !isValidExpandedRemoteHTTPHostname(host) {
		return fmt.Errorf("hostname contains invalid characters")
	}
	return nil
}

func splitExpandedRemoteHTTPHostAuthority(authority string) (host, port string, err error) {
	if strings.HasPrefix(authority, "[") {
		end := strings.LastIndexByte(authority, ']')
		if end < 0 {
			return "", "", fmt.Errorf("bracketed IP address is missing closing bracket")
		}
		host = authority[1:end]
		if host == "" {
			return "", "", fmt.Errorf("host is empty")
		}
		if _, err := netip.ParseAddr(host); err != nil {
			return "", "", fmt.Errorf("bracketed host is not a valid IP address")
		}
		tail := authority[end+1:]
		if tail == "" {
			return host, "", nil
		}
		if !strings.HasPrefix(tail, ":") || len(tail) == 1 {
			return "", "", fmt.Errorf("port is invalid")
		}
		return host, tail[1:], nil
	}
	if strings.ContainsAny(authority, "[]") {
		return "", "", fmt.Errorf("host contains invalid bracket")
	}
	if strings.Count(authority, ":") > 1 {
		return "", "", fmt.Errorf("IPv6 address must be bracketed")
	}
	host, port, hasPort := strings.Cut(authority, ":")
	if host == "" {
		return "", "", fmt.Errorf("host is empty")
	}
	if hasPort && port == "" {
		return "", "", fmt.Errorf("port is invalid")
	}
	return host, port, nil
}

func isDecimalPort(port string) bool {
	for _, r := range port {
		if r < '0' || r > '9' {
			return false
		}
	}
	return port != ""
}

func isValidExpandedRemoteHTTPHostname(host string) bool {
	trimmed := strings.TrimSuffix(host, ".")
	if trimmed == "" || len(trimmed) > 253 {
		return false
	}
	for _, label := range strings.Split(trimmed, ".") {
		if label == "" || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, r := range label {
			if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' {
				continue
			}
			return false
		}
	}
	return true
}

func redactExpandedSecretHostReason(reason, host string) string {
	redacted := strings.ReplaceAll(reason, host, "<redacted-host>")
	hostForIP := host
	if i := strings.LastIndexByte(hostForIP, '%'); i >= 0 {
		hostForIP = hostForIP[:i]
	}
	if addr, err := netip.ParseAddr(hostForIP); err == nil {
		redacted = strings.ReplaceAll(redacted, addr.String(), "<redacted-host>")
		redacted = strings.ReplaceAll(redacted, addr.Unmap().String(), "<redacted-host>")
	}
	return redacted
}
